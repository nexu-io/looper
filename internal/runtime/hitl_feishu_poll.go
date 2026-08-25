package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/eventlog"
	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
	"github.com/nexu-io/looper/internal/worker"
)

// feishuInboxEvent is one event from the shared Cloudflare inbox (GET /events).
type feishuInboxEvent struct {
	ID           int64  `json:"id"`
	Kind         string `json:"kind"` // "message" | "card_action"
	RootID       string `json:"rootId"`
	SenderOpenID string `json:"senderOpenId"`
	Text         string `json:"text"`
	Value        struct {
		LoopSeq string `json:"loopSeq"`
		Answer  string `json:"answer"`
	} `json:"value"`
}

// feishuHITLPollDeps are the injected dependencies of the Feishu inbox poll lane.
type feishuHITLPollDeps struct {
	// loopByRoot maps a Feishu thread root message id to the loop that owns it
	// (this looper's local feishu_threads); "" when it belongs to another looper.
	loopByRoot func(ctx contextType, rootID string) string
	// loopBySeq maps a loop seq (from a card-action value) to a loop id; "" when
	// unknown to this looper.
	loopBySeq func(ctx contextType, seq int64) string
	// deliverAnswer feeds a button-click decision into the shared HITL core.
	deliverAnswer func(ctx contextType, loopID, answer string) error
	// enqueueMessage queues a free-text thread reply for the loop (conversational /
	// anytime), to be drained on the loop's next turn rather than treated as a final
	// answer.
	enqueueMessage func(ctx contextType, loopID, text string) error
	logWarn        func(msg string, fields map[string]any)
}

// pollFeishuHITLInboxOnce delivers the answers among a batch of inbox events that
// belong to this looper's awaiting loops, self-selecting by thread root (typed
// replies) or loop seq (card-action clicks). Returns the highest event id that
// was safely consumed so the caller can advance its cursor. A delivery error
// leaves that event and every later event unconsumed so the next poll retries.
// Idempotent: an event whose loop is no longer awaiting is a no-op in deliverAnswer.
func pollFeishuHITLInboxOnce(ctx contextType, events []feishuInboxEvent, deps feishuHITLPollDeps) (delivered int, maxID int64) {
	for _, e := range events {
		loopID := ""
		value := ""
		var deliver func(contextType, string, string) error
		switch strings.TrimSpace(e.Kind) {
		case "message":
			// A typed thread reply is conversational: queue it (question / new
			// instruction / an answer the agent will interpret), don't force it to
			// resolve the ask.
			text := strings.TrimSpace(e.Text)
			root := strings.TrimSpace(e.RootID)
			if text == "" || root == "" || deps.loopByRoot == nil || deps.enqueueMessage == nil {
				if e.ID > maxID {
					maxID = e.ID
				}
				continue
			}
			loopID = deps.loopByRoot(ctx, root)
			value = text
			deliver = deps.enqueueMessage
		case "card_action":
			// A button click is a clean decision → the shared answer path.
			ans := strings.TrimSpace(e.Value.Answer)
			seq, err := strconv.ParseInt(strings.TrimSpace(e.Value.LoopSeq), 10, 64)
			if ans == "" || err != nil || deps.loopBySeq == nil {
				if e.ID > maxID {
					maxID = e.ID
				}
				continue
			}
			loopID = deps.loopBySeq(ctx, seq)
			value = ans
			deliver = deps.deliverAnswer
		default:
			if e.ID > maxID {
				maxID = e.ID
			}
			continue
		}
		if strings.TrimSpace(loopID) == "" {
			if e.ID > maxID {
				maxID = e.ID
			}
			continue // belongs to another looper (or already resumed)
		}
		if err := deliver(ctx, loopID, value); err != nil {
			if deps.logWarn != nil {
				deps.logWarn("hitl feishu poll: deliver failed", map[string]any{"loopId": loopID, "kind": e.Kind, "error": err.Error()})
			}
			return delivered, maxID
		}
		delivered++
		if e.ID > maxID {
			maxID = e.ID
		}
	}
	return delivered, maxID
}

// feishuInboxCursor tracks the last inbox event id this daemon has consumed. In
// memory is sufficient: on restart it re-reads from 0 and delivery is idempotent.
var feishuInboxCursor struct {
	mu sync.Mutex
	v  int64
}

var feishuInboxHTTPClient = &http.Client{Timeout: 10 * time.Second}

type feishuHITLDeliveryDeps struct {
	sendAsk func(ctx contextType, loop storage.LoopRecord, ask loops.HITLAsk) error
	nowISO  string
	logWarn func(msg string, fields map[string]any)
}

func deliverUndeliveredFeishuBudgetAsks(ctx contextType, records []storage.LoopRecord, repos *storage.Repositories, deps feishuHITLDeliveryDeps) int {
	if repos == nil || repos.Loops == nil || deps.sendAsk == nil {
		return 0
	}
	delivered := 0
	for _, loop := range records {
		if loop.Status != "awaiting_human" {
			continue
		}
		ask, ok := loops.ReadHITLAsk(loop.MetadataJSON)
		if !ok || (!loops.IsReviewFixBudgetAsk(ask) && !loops.IsReviewScopeHumanAsk(ask)) {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(ask.Transport), "feishu") {
			continue
		}
		if strings.TrimSpace(ask.Transport) != "" {
			continue
		}
		if err := deps.sendAsk(ctx, loop, ask); err != nil {
			if deps.logWarn != nil {
				deps.logWarn("hitl feishu: budget ask delivery failed", map[string]any{"loopId": loop.ID, "error": err.Error()})
			}
			continue
		}
		ask.Transport = "feishu"
		meta, werr := loops.WriteHITLAsk(loop.MetadataJSON, ask)
		if werr != nil {
			if deps.logWarn != nil {
				deps.logWarn("hitl feishu: budget ask metadata write failed", map[string]any{"loopId": loop.ID, "error": werr.Error()})
			}
			continue
		}
		updated := loop
		updated.MetadataJSON = &meta
		if deps.nowISO != "" {
			updated.UpdatedAt = deps.nowISO
		}
		if err := repos.Loops.Upsert(ctx, updated); err != nil {
			if deps.logWarn != nil {
				deps.logWarn("hitl feishu: budget ask persist failed", map[string]any{"loopId": loop.ID, "error": err.Error()})
			}
			continue
		}
		delivered++
	}
	return delivered
}

// enqueueFeishuHITLMessage applies a typed inbox reply and, when that reply is
// an explicit budget Continue/Stop, invokes onAnswered so the live ask card is
// marked resolved. Card-action delivery already does this; typed decisions must
// too, or the cached loop-seq buttons can answer a later budget cycle.
// Typed Stop on a scope hold drains live pair agents before records terminalize,
// matching card-action deliverAnswer.
// Overlay siblings keep an ordinary agent card: Continue/Stop typed in that
// thread must resolve the pair's scope-primary card, not the sibling card.
func enqueueFeishuHITLMessage(ctx context.Context, repos *storage.Repositories, db *sql.DB, cfg *config.Config, nowISO, loopID, text string, onAnswered func(context.Context, string, string), drain func(context.Context, storage.LoopRecord) error) error {
	if err := drainScopeHoldOnStop(ctx, repos, loopID, text, drain); err != nil {
		return err
	}
	shouldResolve := false
	resolveLoopID := loopID
	caps := reviewFixBudgetLiveCaps(cfg, "")
	if repos != nil && repos.Loops != nil {
		loop, err := repos.Loops.GetByID(ctx, loopID)
		if err != nil {
			return err
		}
		if loop != nil {
			caps = reviewFixBudgetLiveCaps(cfg, loop.ProjectID)
			if loops.IsReviewFixBudgetContinue(text) || loops.IsReviewFixBudgetStop(text) {
				cardLoopID, err := feishuDecisionCardLoopID(ctx, repos, *loop)
				if err != nil {
					return err
				}
				if cardLoopID != "" {
					shouldResolve = true
					resolveLoopID = cardLoopID
				}
			}
		}
	}
	if err := enqueueHumanMessageToLoopWithCaps(ctx, repos, db, nowISO, loopID, text, caps); err != nil {
		return err
	}
	if shouldResolve && onAnswered != nil {
		onAnswered(ctx, resolveLoopID, text)
	}
	return nil
}

// deliverFeishuHITLCardAction applies a card-action click. Overlay siblings keep
// an ordinary agent card interactive; those option clicks are not pair
// Continue/Stop decisions and must not fail-close the inbox cursor ahead of the
// primary scope card.
func deliverFeishuHITLCardAction(ctx context.Context, repos *storage.Repositories, db *sql.DB, cfg *config.Config, nowISO, loopID, answer string, drain func(context.Context, storage.LoopRecord) error, onAnswered func(context.Context, string, string)) error {
	caps := reviewFixBudgetLiveCaps(cfg, "")
	if repos != nil && repos.Loops != nil {
		if loop, err := repos.Loops.GetByID(ctx, loopID); err == nil && loop != nil {
			caps = reviewFixBudgetLiveCaps(cfg, loop.ProjectID)
			if feishuOverlayResidualCardIsNotPairDecision(*loop, answer) {
				return nil
			}
		}
	}
	if err := drainScopeHoldOnStop(ctx, repos, loopID, answer, drain); err != nil {
		return err
	}
	if err := deliverHITLAnswerToLoopWithCaps(ctx, repos, db, nowISO, loopID, answer, caps); err != nil {
		return err
	}
	if onAnswered != nil {
		onAnswered(ctx, loopID, answer)
	}
	return nil
}

func feishuOverlayResidualCardIsNotPairDecision(loop storage.LoopRecord, answer string) bool {
	if !loops.IsReviewScopeHumanHold(loop) {
		return false
	}
	if loops.IsReviewFixBudgetContinue(answer) || loops.IsReviewFixBudgetStop(answer) {
		return false
	}
	ask, ok := loops.ReadHITLAsk(loop.MetadataJSON)
	return ok && githubHITLResidualOrdinaryAsk(ask)
}

// feishuDecisionCardLoopID returns the loop whose Continue/Stop Feishu card
// should be resolved for a typed decision. Overlay siblings keep an ordinary
// agent ask, so the pair's scope/budget primary card is the one that must close.
func feishuDecisionCardLoopID(ctx context.Context, repos *storage.Repositories, loop storage.LoopRecord) (string, error) {
	if ask, ok := loops.ReadHITLAsk(loop.MetadataJSON); ok && (loops.IsReviewFixBudgetAsk(ask) || loops.IsReviewScopeHumanAsk(ask)) {
		return loop.ID, nil
	}
	if !loops.IsReviewScopeHumanHold(loop) || repos == nil || repos.Loops == nil {
		return "", nil
	}
	all, err := repos.Loops.List(ctx)
	if err != nil {
		return "", err
	}
	for _, sibling := range loops.FindSiblingReviewFixLoops(all, loop) {
		ask, ok := loops.ReadHITLAsk(sibling.MetadataJSON)
		if ok && (loops.IsReviewFixBudgetAsk(ask) || loops.IsReviewScopeHumanAsk(ask)) {
			return sibling.ID, nil
		}
	}
	return "", nil
}

func sendFeishuBudgetAsk(ctx context.Context, input defaultSchedulerTickInput, loop storage.LoopRecord, ask loops.HITLAsk) error {
	if input.OnHITLAsk == nil {
		return fmt.Errorf("feishu HITL notifier is not configured")
	}
	prNumber := ask.PRNumber
	if prNumber == 0 {
		prNumber = derefLoopPRNumber(loop)
	}
	notif := worker.HITLAskNotification{
		ProjectID: loop.ProjectID,
		LoopID:    loop.ID,
		LoopSeq:   loop.Seq,
		Repo:      derefLoopRepo(loop),
		Title:     ask.Question,
		Question:  ask.Question,
		Options:   ask.Options,
	}
	if prNumber > 0 {
		notif.SourceType = "GitHub PR"
		notif.SourceRef = "#" + strconv.FormatInt(prNumber, 10)
	}
	return input.OnHITLAsk(ctx, notif)
}

// runFeishuHITLPoll polls the shared Cloudflare inbox once and delivers any
// answers for this looper's awaiting loops. Gated by the feishu transport +
// cf-inbox inbound; a no-op otherwise.
func runFeishuHITLPoll(ctx context.Context, input defaultSchedulerTickInput) {
	if input.Config == nil || !input.Config.HITL.Enabled || input.Repos == nil {
		return
	}
	if !strings.EqualFold(strings.TrimSpace(input.Config.HITL.AnswerTransport), "feishu") {
		return
	}
	fs := input.Config.HITL.Feishu
	if fs == nil || !strings.EqualFold(strings.TrimSpace(fs.Inbound), "cf-inbox") {
		return
	}
	inboxURL := strings.TrimSpace(os.Getenv(strings.TrimSpace(fs.EventInboxURLEnv)))
	token := strings.TrimSpace(os.Getenv(strings.TrimSpace(fs.EventInboxTokenEnv)))
	if inboxURL == "" || token == "" {
		return
	}

	nowISO := eventlog.FormatJavaScriptISOString(input.Now().UTC())
	if allLoops, err := input.Repos.Loops.List(ctx); err == nil {
		deliveryDeps := feishuHITLDeliveryDeps{
			sendAsk: func(ctx contextType, loop storage.LoopRecord, ask loops.HITLAsk) error {
				return sendFeishuBudgetAsk(ctx, input, loop, ask)
			},
			nowISO: nowISO,
		}
		if input.Logger != nil {
			deliveryDeps.logWarn = func(msg string, fields map[string]any) { input.Logger.Warn(msg, fields) }
		}
		_ = deliverUndeliveredFeishuBudgetAsks(ctx, allLoops, input.Repos, deliveryDeps)
	}

	feishuInboxCursor.mu.Lock()
	since := feishuInboxCursor.v
	feishuInboxCursor.mu.Unlock()

	events, err := fetchFeishuInboxEvents(ctx, inboxURL, token, since)
	if err != nil {
		if input.Logger != nil {
			input.Logger.Warn("hitl feishu poll: fetch inbox failed", map[string]any{"error": err.Error()})
		}
		return
	}
	if len(events) == 0 {
		return
	}

	deps := feishuHITLPollDeps{
		loopByRoot: func(ctx contextType, rootID string) string {
			if input.Repos.FeishuThreads == nil {
				return ""
			}
			loopID, _ := input.Repos.FeishuThreads.LoopByRoot(ctx, rootID)
			return loopID
		},
		loopBySeq: func(ctx contextType, seq int64) string {
			loop, err := input.Repos.Loops.GetBySeq(ctx, seq)
			if err != nil || loop == nil {
				return ""
			}
			return loop.ID
		},
		deliverAnswer: func(ctx contextType, loopID, answer string) error {
			return deliverFeishuHITLCardAction(ctx, input.Repos, input.DB, input.Config, nowISO, loopID, answer, input.DrainHITLPair, input.OnHITLAnswerDelivered)
		},
		enqueueMessage: func(ctx contextType, loopID, text string) error {
			return enqueueFeishuHITLMessage(ctx, input.Repos, input.DB, input.Config, nowISO, loopID, text, input.OnHITLAnswerDelivered, input.DrainHITLPair)
		},
	}
	if input.Logger != nil {
		deps.logWarn = func(msg string, fields map[string]any) { input.Logger.Warn(msg, fields) }
	}

	delivered, maxID := pollFeishuHITLInboxOnce(ctx, events, deps)
	if maxID > 0 {
		feishuInboxCursor.mu.Lock()
		if maxID > feishuInboxCursor.v {
			feishuInboxCursor.v = maxID
		}
		feishuInboxCursor.mu.Unlock()
	}
	if delivered > 0 && input.Logger != nil {
		input.Logger.Info("hitl feishu: delivered human answers", map[string]any{"count": delivered})
	}
}

func fetchFeishuInboxEvents(ctx context.Context, inboxURL, token string, since int64) ([]feishuInboxEvent, error) {
	u, err := url.Parse(inboxURL)
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("since", strconv.FormatInt(since, 10))
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := feishuInboxHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("inbox responded with status %d", resp.StatusCode)
	}
	var parsed struct {
		OK     bool               `json:"ok"`
		Events []feishuInboxEvent `json:"events"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, err
	}
	return parsed.Events, nil
}

// storageReposForFeishuPoll is a compile-time assertion that the repos we rely on
// exist (keeps this file honest if the storage API changes).
var _ = func(r *storage.Repositories) {
	_ = r.FeishuThreads
	_ = r.Loops
}
