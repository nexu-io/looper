package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/eventlog"
	"github.com/nexu-io/looper/internal/infra/shell"
	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
)

const (
	osascriptTimeout = 35 * time.Second
	webhookTimeout   = 10 * time.Second

	feishuAPIBase           = "https://open.feishu.cn"
	feishuTokenSafetyMargin = 60 * time.Second
)

type RunCommandFunc func(context.Context, shell.Options) (shell.Result, error)

// HTTPPostFunc delivers a webhook notification body to url and returns the
// HTTP status code. It is injectable so tests can avoid real network calls.
type HTTPPostFunc func(url string, body []byte) (int, error)

// FeishuAppHTTPFunc performs an HTTP request for the Feishu app-bot delivery
// (token fetch + message send) and returns the status code and response body.
// It is injectable so tests can avoid real network calls.
type FeishuAppHTTPFunc func(ctx context.Context, method, url string, headers map[string]string, body []byte) (int, []byte, error)

type Options struct {
	Config        config.NotificationConfig
	OsascriptPath string
	LogFilePath   string
	Repositories  *storage.Repositories
	Now           func() time.Time
	RunCommand    RunCommandFunc
	HTTPPost      HTTPPostFunc
	FeishuAppHTTP FeishuAppHTTPFunc
}

type SystemNotificationPayload struct {
	ID         string
	ProjectID  string
	LoopID     string
	RunID      string
	Level      string
	Title      string
	Subtitle   string
	Body       string
	Sound      string
	Group      string
	EntityType string
	EntityID   string
	DedupeKey  string
}

type Gateway struct {
	config        config.NotificationConfig
	osascriptPath string
	logFilePath   string
	repositories  *storage.Repositories
	now           func() time.Time
	runCommand    RunCommandFunc
	httpPost      HTTPPostFunc
	feishuAppHTTP FeishuAppHTTPFunc

	feishuTokenMu  sync.Mutex
	feishuToken    string
	feishuTokenExp time.Time

	// liveTails holds the most recent agent-activity snapshot per loop, kept in
	// memory (not the loop record) so frequent progress ticks never race the
	// scheduler's loop/run writes. Retained across the completion rebuild so the
	// final card still shows the last activity.
	liveMu    sync.Mutex
	liveTails map[string]liveTailEntry
	// askCards remembers each loop's live ask card (message id + the card it was
	// built from) so the card can be patched to its resolved "✅ 已选:X" state when
	// the answer arrives — keeping the full brief for review.
	askCards map[string]askCardState
	// liveFeeds remembers each loop's live-progress comment (a reply threaded under
	// the anchor). The raw tool-call feed lives HERE — inside the thread — so the
	// outer anchor card stays a human-scannable brief. Keyed by loop id → message id;
	// posted on first activity, patched in place thereafter.
	liveFeeds map[string]string
}

type askCardState struct {
	msgID string
	card  HITLAskCard
}

func NewGateway(options Options) *Gateway {
	now := options.Now
	if now == nil {
		now = time.Now
	}

	runCommand := options.RunCommand
	if runCommand == nil {
		runCommand = shell.Run
	}

	httpPost := options.HTTPPost
	if httpPost == nil {
		httpPost = defaultWebhookPost
	}

	feishuAppHTTP := options.FeishuAppHTTP
	if feishuAppHTTP == nil {
		feishuAppHTTP = defaultFeishuAppHTTP
	}

	return &Gateway{
		config:        options.Config,
		osascriptPath: options.OsascriptPath,
		logFilePath:   options.LogFilePath,
		repositories:  options.Repositories,
		now:           now,
		runCommand:    runCommand,
		httpPost:      httpPost,
		feishuAppHTTP: feishuAppHTTP,
	}
}

func (g *Gateway) Notify(ctx context.Context, payload SystemNotificationPayload) []storage.NotificationRecord {
	records := make([]storage.NotificationRecord, 0, 3)

	if record, ok := g.recordInApp(ctx, payload); ok {
		records = append(records, record)
	}

	if record, ok := g.recordOsascript(ctx, payload); ok {
		records = append(records, record)
	}

	if strings.EqualFold(strings.TrimSpace(g.config.Webhook.Mode), "app") {
		if record, ok := g.recordFeishuApp(ctx, payload); ok {
			records = append(records, record)
		}
	} else if record, ok := g.recordWebhook(ctx, payload); ok {
		records = append(records, record)
	}

	return records
}

func (g *Gateway) recordInApp(ctx context.Context, payload SystemNotificationPayload) (storage.NotificationRecord, bool) {
	nowISO := eventlog.FormatJavaScriptISOString(g.now())
	record := storage.NotificationRecord{
		ID:           firstNonEmpty(payload.ID, eventlog.NewEventID("notification")),
		ProjectID:    nilIfEmpty(payload.ProjectID),
		LoopID:       nilIfEmpty(payload.LoopID),
		RunID:        nilIfEmpty(payload.RunID),
		EntityType:   nilIfEmpty(payload.EntityType),
		EntityID:     nilIfEmpty(payload.EntityID),
		Channel:      "in_app",
		Level:        payload.Level,
		Title:        payload.Title,
		Subtitle:     nilIfEmpty(payload.Subtitle),
		Body:         payload.Body,
		Status:       ternaryString(g.config.InApp, "success", "skipped"),
		DedupeKey:    nilIfEmpty(payload.DedupeKey),
		ErrorMessage: ternaryPointer(!g.config.InApp, "disabled"),
		PayloadJSON:  stringPointer(mustMarshalPayload(payload)),
		SentAt:       ternaryTimePointer(g.config.InApp, nowISO),
		CreatedAt:    nowISO,
		UpdatedAt:    nowISO,
	}

	if err := g.persistNotification(ctx, record); err != nil {
		return storage.NotificationRecord{}, false
	}

	return record, true
}

func (g *Gateway) recordOsascript(ctx context.Context, payload SystemNotificationPayload) (storage.NotificationRecord, bool) {
	nowISO := eventlog.FormatJavaScriptISOString(g.now())
	id := eventlog.NewEventID("notification")

	if payload.DedupeKey != "" && g.repositories != nil && g.repositories.Notifications != nil {
		dedupeRecord, err := g.repositories.Notifications.GetLatestByDedupe(ctx, "osascript", payload.DedupeKey)
		if err == nil && dedupeRecord != nil {
			createdAt, parseErr := time.Parse(time.RFC3339Nano, dedupeRecord.CreatedAt)
			if parseErr == nil {
				throttleWindow := time.Duration(g.config.Osascript.ThrottleWindowSeconds) * time.Second
				if g.now().UTC().Sub(createdAt.UTC()) < throttleWindow {
					record := storage.NotificationRecord{
						ID:           id,
						ProjectID:    nilIfEmpty(payload.ProjectID),
						LoopID:       nilIfEmpty(payload.LoopID),
						RunID:        nilIfEmpty(payload.RunID),
						EntityType:   nilIfEmpty(payload.EntityType),
						EntityID:     nilIfEmpty(payload.EntityID),
						Channel:      "osascript",
						Level:        payload.Level,
						Title:        payload.Title,
						Subtitle:     nilIfEmpty(payload.Subtitle),
						Body:         payload.Body,
						Status:       "skipped",
						DedupeKey:    nilIfEmpty(payload.DedupeKey),
						ErrorMessage: stringPointer("deduped"),
						PayloadJSON:  stringPointer(mustMarshalPayload(payload)),
						CreatedAt:    nowISO,
						UpdatedAt:    nowISO,
					}
					if err := g.persistNotification(ctx, record); err != nil {
						return storage.NotificationRecord{}, false
					}
					return record, true
				}
			}
		}
	}

	if !g.config.Osascript.Enabled || strings.TrimSpace(g.osascriptPath) == "" {
		record := storage.NotificationRecord{
			ID:           id,
			ProjectID:    nilIfEmpty(payload.ProjectID),
			LoopID:       nilIfEmpty(payload.LoopID),
			RunID:        nilIfEmpty(payload.RunID),
			EntityType:   nilIfEmpty(payload.EntityType),
			EntityID:     nilIfEmpty(payload.EntityID),
			Channel:      "osascript",
			Level:        payload.Level,
			Title:        payload.Title,
			Subtitle:     nilIfEmpty(payload.Subtitle),
			Body:         payload.Body,
			Status:       "skipped",
			DedupeKey:    nilIfEmpty(payload.DedupeKey),
			ErrorMessage: stringPointer("disabled"),
			PayloadJSON:  stringPointer(mustMarshalPayload(payload)),
			CreatedAt:    nowISO,
			UpdatedAt:    nowISO,
		}
		if err := g.persistNotification(ctx, record); err != nil {
			return storage.NotificationRecord{}, false
		}
		return record, true
	}

	_, err := g.runCommand(ctx, shell.Options{
		Command: g.osascriptPath,
		Args:    []string{"-e", buildAppleScript(payload, g.config, g.logFilePath)},
		Timeout: osascriptTimeout,
	})
	if err != nil {
		record := storage.NotificationRecord{
			ID:           id,
			ProjectID:    nilIfEmpty(payload.ProjectID),
			LoopID:       nilIfEmpty(payload.LoopID),
			RunID:        nilIfEmpty(payload.RunID),
			EntityType:   nilIfEmpty(payload.EntityType),
			EntityID:     nilIfEmpty(payload.EntityID),
			Channel:      "osascript",
			Level:        payload.Level,
			Title:        payload.Title,
			Subtitle:     nilIfEmpty(payload.Subtitle),
			Body:         payload.Body,
			Status:       "failed",
			DedupeKey:    nilIfEmpty(payload.DedupeKey),
			ErrorMessage: stringPointer(err.Error()),
			PayloadJSON:  stringPointer(mustMarshalPayload(payload)),
			CreatedAt:    nowISO,
			UpdatedAt:    nowISO,
		}
		if persistErr := g.persistNotification(ctx, record); persistErr != nil {
			return storage.NotificationRecord{}, false
		}
		return record, true
	}

	record := storage.NotificationRecord{
		ID:          id,
		ProjectID:   nilIfEmpty(payload.ProjectID),
		LoopID:      nilIfEmpty(payload.LoopID),
		RunID:       nilIfEmpty(payload.RunID),
		EntityType:  nilIfEmpty(payload.EntityType),
		EntityID:    nilIfEmpty(payload.EntityID),
		Channel:     "osascript",
		Level:       payload.Level,
		Title:       payload.Title,
		Subtitle:    nilIfEmpty(payload.Subtitle),
		Body:        payload.Body,
		Status:      "success",
		DedupeKey:   nilIfEmpty(payload.DedupeKey),
		PayloadJSON: stringPointer(mustMarshalPayload(payload)),
		SentAt:      stringPointer(nowISO),
		CreatedAt:   nowISO,
		UpdatedAt:   nowISO,
	}
	if err := g.persistNotification(ctx, record); err != nil {
		return storage.NotificationRecord{}, false
	}

	return record, true
}

func (g *Gateway) recordWebhook(ctx context.Context, payload SystemNotificationPayload) (storage.NotificationRecord, bool) {
	nowISO := eventlog.FormatJavaScriptISOString(g.now())
	id := eventlog.NewEventID("notification")

	build := func(status, errorMessage string, sentAt *string) storage.NotificationRecord {
		return storage.NotificationRecord{
			ID:           id,
			ProjectID:    nilIfEmpty(payload.ProjectID),
			LoopID:       nilIfEmpty(payload.LoopID),
			RunID:        nilIfEmpty(payload.RunID),
			EntityType:   nilIfEmpty(payload.EntityType),
			EntityID:     nilIfEmpty(payload.EntityID),
			Channel:      "webhook",
			Level:        payload.Level,
			Title:        payload.Title,
			Subtitle:     nilIfEmpty(payload.Subtitle),
			Body:         payload.Body,
			Status:       status,
			DedupeKey:    nilIfEmpty(payload.DedupeKey),
			ErrorMessage: nilIfEmpty(errorMessage),
			PayloadJSON:  stringPointer(mustMarshalPayload(payload)),
			SentAt:       sentAt,
			CreatedAt:    nowISO,
			UpdatedAt:    nowISO,
		}
	}

	persist := func(record storage.NotificationRecord) (storage.NotificationRecord, bool) {
		if err := g.persistNotification(ctx, record); err != nil {
			return storage.NotificationRecord{}, false
		}
		return record, true
	}

	if !g.config.Webhook.Enabled {
		return persist(build("skipped", "disabled", nil))
	}

	url := ""
	if envName := strings.TrimSpace(g.config.Webhook.URLEnv); envName != "" {
		url = strings.TrimSpace(os.Getenv(envName))
	}
	if url == "" {
		return persist(build("skipped", "no url", nil))
	}

	if !webhookLevelAllowed(g.config.Webhook.Levels, payload.Level) {
		return persist(build("skipped", "level filtered", nil))
	}

	if payload.DedupeKey != "" && g.repositories != nil && g.repositories.Notifications != nil {
		dedupeRecord, err := g.repositories.Notifications.GetLatestByDedupe(ctx, "webhook", payload.DedupeKey)
		if err == nil && dedupeRecord != nil {
			createdAt, parseErr := time.Parse(time.RFC3339Nano, dedupeRecord.CreatedAt)
			if parseErr == nil {
				throttleWindow := time.Duration(g.config.Webhook.ThrottleWindowSeconds) * time.Second
				if g.now().UTC().Sub(createdAt.UTC()) < throttleWindow {
					return persist(build("skipped", "deduped", nil))
				}
			}
		}
	}

	body, err := buildWebhookBody(g.config.Webhook.Format, payload)
	if err != nil {
		return persist(build("failed", err.Error(), nil))
	}

	status, err := g.httpPost(url, body)
	if err != nil {
		return persist(build("failed", err.Error(), nil))
	}
	if status < 200 || status >= 300 {
		return persist(build("failed", fmt.Sprintf("webhook responded with status %d", status), nil))
	}

	return persist(build("success", "", stringPointer(nowISO)))
}

// recordFeishuApp delivers an interactive card through a Feishu app bot's IM
// API (posts to a group chat the bot belongs to). Like the other channels it is
// best-effort and records a NotificationRecord on the "feishu_app" channel.
func (g *Gateway) recordFeishuApp(ctx context.Context, payload SystemNotificationPayload) (storage.NotificationRecord, bool) {
	nowISO := eventlog.FormatJavaScriptISOString(g.now())
	id := eventlog.NewEventID("notification")

	build := func(status, errorMessage string, sentAt *string) storage.NotificationRecord {
		return storage.NotificationRecord{
			ID:           id,
			ProjectID:    nilIfEmpty(payload.ProjectID),
			LoopID:       nilIfEmpty(payload.LoopID),
			RunID:        nilIfEmpty(payload.RunID),
			EntityType:   nilIfEmpty(payload.EntityType),
			EntityID:     nilIfEmpty(payload.EntityID),
			Channel:      "feishu_app",
			Level:        payload.Level,
			Title:        payload.Title,
			Subtitle:     nilIfEmpty(payload.Subtitle),
			Body:         payload.Body,
			Status:       status,
			DedupeKey:    nilIfEmpty(payload.DedupeKey),
			ErrorMessage: nilIfEmpty(errorMessage),
			PayloadJSON:  stringPointer(mustMarshalPayload(payload)),
			SentAt:       sentAt,
			CreatedAt:    nowISO,
			UpdatedAt:    nowISO,
		}
	}

	persist := func(record storage.NotificationRecord) (storage.NotificationRecord, bool) {
		if err := g.persistNotification(ctx, record); err != nil {
			return storage.NotificationRecord{}, false
		}
		return record, true
	}

	cfg := g.config.Webhook
	if !cfg.Enabled {
		return persist(build("skipped", "disabled", nil))
	}

	appID := strings.TrimSpace(os.Getenv(strings.TrimSpace(cfg.AppIDEnv)))
	appSecret := strings.TrimSpace(os.Getenv(strings.TrimSpace(cfg.AppSecretEnv)))
	chatID := strings.TrimSpace(cfg.ChatID)
	if strings.TrimSpace(cfg.AppIDEnv) == "" || strings.TrimSpace(cfg.AppSecretEnv) == "" || chatID == "" {
		return persist(build("skipped", "no app config", nil))
	}
	if appID == "" || appSecret == "" {
		return persist(build("skipped", "no app credentials", nil))
	}

	if !webhookLevelAllowed(cfg.Levels, payload.Level) {
		return persist(build("skipped", "level filtered", nil))
	}

	if payload.DedupeKey != "" && g.repositories != nil && g.repositories.Notifications != nil {
		dedupeRecord, err := g.repositories.Notifications.GetLatestByDedupe(ctx, "feishu_app", payload.DedupeKey)
		if err == nil && dedupeRecord != nil {
			createdAt, parseErr := time.Parse(time.RFC3339Nano, dedupeRecord.CreatedAt)
			if parseErr == nil {
				throttleWindow := time.Duration(cfg.ThrottleWindowSeconds) * time.Second
				if g.now().UTC().Sub(createdAt.UTC()) < throttleWindow {
					return persist(build("skipped", "deduped", nil))
				}
			}
		}
	}

	token, err := g.feishuTenantToken(ctx, appID, appSecret)
	if err != nil {
		return persist(build("failed", err.Error(), nil))
	}

	// Non-interactive updates (PR opened, progress, completion) are plain text —
	// only a mid-run question needs an interactive card with buttons. Thread the
	// message under the loop's root so a task's updates aggregate into one Feishu
	// thread instead of stacking as separate cards.
	rootMessageID := g.ensureFeishuThreadRoot(ctx, token, chatID, payload.LoopID)
	// Refresh the anchor card so its colour/label tracks the loop's status
	// (processing → done/failed) as lifecycle updates arrive.
	g.updateFeishuThreadHeader(ctx, token, payload.LoopID)
	content, err := json.Marshal(map[string]string{"text": feishuNotificationText(payload)})
	if err != nil {
		return persist(build("failed", err.Error(), nil))
	}
	if _, err := g.postFeishuAppMessage(ctx, token, chatID, rootMessageID, "text", string(content)); err != nil {
		return persist(build("failed", err.Error(), nil))
	}

	return persist(build("success", "", stringPointer(nowISO)))
}

// feishuNotificationText renders a system notification as a plain-text body for
// the app bot. Feishu auto-links any URLs it contains (e.g. the PR link).
func feishuNotificationText(payload SystemNotificationPayload) string {
	lines := make([]string, 0, 3)
	for _, part := range []string{payload.Title, payload.Subtitle, payload.Body} {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			lines = append(lines, trimmed)
		}
	}
	if len(lines) == 0 {
		return "Looper update"
	}
	return strings.Join(lines, "\n")
}

// HITLAskCard is a mid-run human-in-the-loop question rendered as an interactive
// Feishu card with one button per option. Each button's value carries the loop
// seq + the chosen answer so a card-action callback identifies the loop + reply.
type HITLAskCard struct {
	ProjectID      string
	LoopID         string
	LoopSeq        int64
	Repo           string
	Title          string
	Question       string
	Options        []string
	MentionOpenIds []string

	// Source identifies where the task came from so the human knows what they are
	// deciding about. SourceType is a human label ("GitHub Issue", "GitHub PR",
	// "Plane"), SourceRef the short id ("#132"), SourceURL the clickable link.
	SourceType string
	SourceRef  string
	SourceURL  string
	// TriggerLogin is who created/assigned the task (GitHub login / Plane account),
	// rendered as attribution so the human knows whose work this is.
	TriggerLogin string

	// The following are the agent's decision brief — populated when the agent did
	// its homework before asking. All optional: the card renders gracefully without
	// them, but a good ask should carry them.
	//
	// Recommendation is a short "here's what I found + what I'd do" summary.
	Recommendation string
	// RecommendedOption, when it matches one of Options, marks that button ⭐.
	RecommendedOption string
	// Consequences maps an option to a one-line "what happens if you pick this".
	Consequences map[string]string
	// Confidence is the agent's self-assessed certainty ("high"/"medium"/"low").
	Confidence string

	// AnsweredWith, when set, renders the card in its resolved state: the option
	// buttons are replaced by a "✅ 已选:<answer>" line while the question, research
	// and consequences stay intact for later review.
	AnsweredWith string
}

// feishuMentionMarkup renders Feishu open_ids as card @-mention tags, e.g.
// "<at id=ou_x></at> <at id=ou_y></at>". Returns "" when there is nothing to ping.
func feishuMentionMarkup(openIDs []string) string {
	tags := make([]string, 0, len(openIDs))
	for _, id := range openIDs {
		if id = strings.TrimSpace(id); id != "" {
			tags = append(tags, "<at id="+id+"></at>")
		}
	}
	return strings.Join(tags, " ")
}

// SendHITLAsk delivers an ask-card to the Feishu app-bot target chat. It reuses
// the app-bot credentials in notifications.webhook (appIdEnv/appSecretEnv/chatId)
// and is a no-op error when they are not configured. Best-effort; caller logs.
func (g *Gateway) SendHITLAsk(ctx context.Context, card HITLAskCard) error {
	cfg := g.config.Webhook
	appID := strings.TrimSpace(os.Getenv(strings.TrimSpace(cfg.AppIDEnv)))
	appSecret := strings.TrimSpace(os.Getenv(strings.TrimSpace(cfg.AppSecretEnv)))
	chatID := strings.TrimSpace(cfg.ChatID)
	if appID == "" || appSecret == "" || chatID == "" {
		return fmt.Errorf("feishu app-bot is not configured for HITL asks (need appIdEnv/appSecretEnv/chatId)")
	}
	token, err := g.feishuTenantToken(ctx, appID, appSecret)
	if err != nil {
		return err
	}
	// The @-mention targets come from config (deployment-specific), not the caller.
	if len(card.MentionOpenIds) == 0 {
		card.MentionOpenIds = cfg.MentionOpenIds
	}
	cardJSON, err := buildFeishuAskCard(card)
	if err != nil {
		return err
	}
	// Thread the ask card under the loop's root so the question lands in the same
	// thread as the task's other updates. Card buttons still work inside a thread.
	rootMessageID := g.ensureFeishuThreadRoot(ctx, token, chatID, card.LoopID)
	// The loop is awaiting_human now — turn the anchor card orange "等你定夺".
	g.updateFeishuThreadHeader(ctx, token, card.LoopID)
	msgID, err := g.postFeishuAppMessage(ctx, token, chatID, rootMessageID, "interactive", string(cardJSON))
	if err != nil {
		return err
	}
	// Remember the card so MarkAskAnswered can patch it in place on delivery.
	if strings.TrimSpace(msgID) != "" && strings.TrimSpace(card.LoopID) != "" {
		g.liveMu.Lock()
		if g.askCards == nil {
			g.askCards = map[string]askCardState{}
		}
		g.askCards[card.LoopID] = askCardState{msgID: msgID, card: card}
		g.liveMu.Unlock()
	}
	return nil
}

// MarkAskAnswered patches a loop's ask card to its resolved "✅ 已选:X" state,
// keeping the question + research + consequences intact. Best-effort; a no-op if
// no ask card is remembered for the loop or the app-bot isn't configured.
func (g *Gateway) MarkAskAnswered(ctx context.Context, loopID, answer string) {
	loopID = strings.TrimSpace(loopID)
	answer = strings.TrimSpace(answer)
	if loopID == "" || answer == "" {
		return
	}
	// Always log the decision to the loop's story (+ refresh the anchor), even if
	// this process no longer holds the ask card to patch.
	g.RecordMilestone(ctx, loopID, "🧑‍⚖️ 已定夺:"+answer)
	g.liveMu.Lock()
	st, ok := g.askCards[loopID]
	g.liveMu.Unlock()
	if !ok || strings.TrimSpace(st.msgID) == "" {
		return
	}
	st.card.AnsweredWith = answer
	cardJSON, err := buildFeishuAskCard(st.card)
	if err != nil {
		return
	}
	cfg := g.config.Webhook
	appID := strings.TrimSpace(os.Getenv(strings.TrimSpace(cfg.AppIDEnv)))
	appSecret := strings.TrimSpace(os.Getenv(strings.TrimSpace(cfg.AppSecretEnv)))
	if appID == "" || appSecret == "" {
		return
	}
	token, err := g.feishuTenantToken(ctx, appID, appSecret)
	if err != nil {
		return
	}
	if err := g.patchFeishuAppCard(ctx, token, st.msgID, string(cardJSON)); err == nil {
		g.liveMu.Lock()
		delete(g.askCards, loopID)
		g.liveMu.Unlock()
	}
}

// RecordMilestone appends one human-scannable milestone to a loop's story
// (persisted in loop metadata) and refreshes the anchor so the outer card reads
// as a timestamped narrative — who decided what, phases, the PR — rather than a
// single current-status line. Best-effort; a no-op when the loop can't be loaded.
func (g *Gateway) RecordMilestone(ctx context.Context, loopID, text string) {
	loopID = strings.TrimSpace(loopID)
	text = strings.TrimSpace(text)
	if loopID == "" || text == "" || g.repositories == nil || g.repositories.Loops == nil {
		return
	}
	loop, err := g.repositories.Loops.GetByID(ctx, loopID)
	if err != nil || loop == nil {
		return
	}
	nowISO := eventlog.FormatJavaScriptISOString(g.now().UTC())
	meta, err := loops.AppendMilestone(loop.MetadataJSON, loops.Milestone{At: nowISO, Text: text})
	if err != nil {
		return
	}
	loop.MetadataJSON = &meta
	loop.UpdatedAt = nowISO
	if err := g.repositories.Loops.Upsert(ctx, *loop); err != nil {
		return
	}
	cfg := g.config.Webhook
	appID := strings.TrimSpace(os.Getenv(strings.TrimSpace(cfg.AppIDEnv)))
	appSecret := strings.TrimSpace(os.Getenv(strings.TrimSpace(cfg.AppSecretEnv)))
	if appID == "" || appSecret == "" {
		return
	}
	if token, err := g.feishuTenantToken(ctx, appID, appSecret); err == nil {
		g.updateFeishuThreadHeader(ctx, token, loopID)
	}
}

// feishuMilestoneList renders a loop's milestone log as a compact, timestamped
// narrative for the anchor: "HH:MM · <text>", most recent last, capped.
func feishuMilestoneList(milestones []loops.Milestone) string {
	const show = 6
	start := 0
	if len(milestones) > show {
		start = len(milestones) - show
	}
	lines := make([]string, 0, len(milestones)-start+1)
	lines = append(lines, "📋 进展")
	for _, m := range milestones[start:] {
		if strings.TrimSpace(m.Text) == "" {
			continue
		}
		lines = append(lines, "· "+feishuShortTime(m.At)+" "+m.Text)
	}
	return strings.Join(lines, "\n")
}

// feishuShortTime turns an ISO timestamp into a local "HH:MM"; empty on parse
// failure so the caller can omit it.
func feishuShortTime(iso string) string {
	iso = strings.TrimSpace(iso)
	if iso == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339Nano, iso)
	if err != nil {
		if t, err = time.Parse("2006-01-02T15:04:05.000Z", iso); err != nil {
			return ""
		}
	}
	return t.Local().Format("15:04")
}

func buildFeishuAskCard(card HITLAskCard) ([]byte, error) {
	title := strings.TrimSpace(card.Title)
	if title == "" {
		title = "Looper needs a decision"
	}
	body := strings.TrimSpace(card.Question)
	if body == "" {
		body = title
	}
	seq := strconv.FormatInt(card.LoopSeq, 10)
	recommended := strings.TrimSpace(card.RecommendedOption)

	// Option buttons — the recommended one is marked ⭐ and stays "primary"; the
	// rest drop to "default" so the recommendation reads at a glance.
	actions := make([]any, 0, len(card.Options))
	for _, option := range card.Options {
		option = strings.TrimSpace(option)
		if option == "" {
			continue
		}
		label := option
		btnType := "primary"
		if recommended != "" {
			if strings.EqualFold(option, recommended) {
				label = "⭐ " + option + " · 推荐"
			} else {
				btnType = "default"
			}
		}
		actions = append(actions, map[string]any{
			"tag":   "button",
			"type":  btnType,
			"text":  map[string]any{"tag": "plain_text", "content": label},
			"value": map[string]any{"loopSeq": seq, "answer": option},
		})
	}

	elements := make([]any, 0, 8)
	// @-mention the humans who need to act, so an ask isn't missed in a busy group.
	if mention := feishuMentionMarkup(card.MentionOpenIds); mention != "" {
		elements = append(elements, larkDiv(mention+" 需要你定夺 👇"))
	}
	// Source line: what this is (Issue/PR + link) · repo · who triggered it.
	if src := feishuSourceLine(card); src != "" {
		elements = append(elements, larkDiv(src))
		elements = append(elements, map[string]any{"tag": "hr"})
	}
	// The question itself.
	elements = append(elements, larkDiv("**"+title+"**\n"+body))
	// The agent's decision brief — research + recommendation.
	if rec := strings.TrimSpace(card.Recommendation); rec != "" {
		elements = append(elements, larkDiv("🔎 "+rec))
	}
	// Options — or, once answered, the resolved selection. Buttons are removed (so
	// it can't be re-clicked) but the question, research and consequences stay for
	// later review.
	if answered := strings.TrimSpace(card.AnsweredWith); answered != "" {
		elements = append(elements, larkDiv("✅ **已选:"+answered+"** · Looper 继续处理中 →"))
	} else if len(actions) > 0 {
		elements = append(elements, map[string]any{"tag": "action", "actions": actions})
	}
	// Per-option consequences: pick this → that happens.
	if conseq := feishuConsequences(card); conseq != "" {
		elements = append(elements, map[string]any{"tag": "note", "elements": []any{map[string]any{"tag": "lark_md", "content": conseq}}})
	}
	// Footer note: confidence · blocking · loop.
	noteParts := make([]string, 0, 4)
	if c := feishuConfidenceLabel(card.Confidence); c != "" {
		noteParts = append(noteParts, c)
	}
	noteParts = append(noteParts, "loop "+seq, "点选项或直接回文字")
	elements = append(elements, map[string]any{"tag": "note", "elements": []any{map[string]any{"tag": "lark_md", "content": strings.Join(noteParts, " · ")}}})

	header := map[string]any{"template": "orange", "title": map[string]any{"tag": "plain_text", "content": "Looper needs a decision"}}
	if strings.TrimSpace(card.AnsweredWith) != "" {
		header = map[string]any{"template": "green", "title": map[string]any{"tag": "plain_text", "content": "✅ 已定夺"}}
	}
	cardObj := map[string]any{
		"config":   map[string]any{"wide_screen_mode": true},
		"header":   header,
		"elements": elements,
	}
	return json.Marshal(cardObj)
}

// larkDiv is a text block that renders lark markdown (bold, links, @-mentions).
func larkDiv(content string) map[string]any {
	return map[string]any{"tag": "div", "text": map[string]any{"tag": "lark_md", "content": content}}
}

// feishuSourceLine renders "📋 [GitHub Issue #132](url) · repo · 由 @who 提出".
// Returns "" when there is nothing worth showing.
func feishuSourceLine(card HITLAskCard) string {
	parts := make([]string, 0, 3)
	label := strings.TrimSpace(strings.TrimSpace(card.SourceType) + " " + strings.TrimSpace(card.SourceRef))
	if label != "" {
		if url := strings.TrimSpace(card.SourceURL); url != "" {
			parts = append(parts, "📋 ["+label+"]("+url+")")
		} else {
			parts = append(parts, "📋 "+label)
		}
	}
	if repo := strings.TrimSpace(card.Repo); repo != "" {
		parts = append(parts, repo)
	}
	if who := strings.TrimSpace(card.TriggerLogin); who != "" {
		parts = append(parts, "由 @"+strings.TrimPrefix(who, "@")+" 提出")
	}
	return strings.Join(parts, " · ")
}

// feishuConsequences renders "中文 → …\n英文 → …" for options that have one.
func feishuConsequences(card HITLAskCard) string {
	if len(card.Consequences) == 0 {
		return ""
	}
	lines := make([]string, 0, len(card.Options))
	for _, option := range card.Options {
		option = strings.TrimSpace(option)
		if v := strings.TrimSpace(card.Consequences[option]); v != "" {
			lines = append(lines, option+" → "+v)
		}
	}
	return strings.Join(lines, "\n")
}

func feishuConfidenceLabel(confidence string) string {
	switch strings.ToLower(strings.TrimSpace(confidence)) {
	case "high":
		return "置信度 高"
	case "medium", "med":
		return "置信度 中"
	case "low":
		return "置信度 低"
	}
	return ""
}

// feishuTenantToken returns a cached tenant_access_token when still valid, else
// fetches a fresh one from Feishu. Access is serialized so concurrent
// notifications share one token.
func (g *Gateway) feishuTenantToken(ctx context.Context, appID, appSecret string) (string, error) {
	g.feishuTokenMu.Lock()
	defer g.feishuTokenMu.Unlock()

	if g.feishuToken != "" && g.now().UTC().Before(g.feishuTokenExp) {
		return g.feishuToken, nil
	}

	body, err := json.Marshal(map[string]string{"app_id": appID, "app_secret": appSecret})
	if err != nil {
		return "", err
	}
	status, respBody, err := g.feishuAppHTTP(ctx, http.MethodPost, feishuAPIBase+"/open-apis/auth/v3/tenant_access_token/internal", map[string]string{
		"Content-Type": "application/json; charset=utf-8",
	}, body)
	if err != nil {
		return "", err
	}
	if status < 200 || status >= 300 {
		return "", fmt.Errorf("feishu token responded with status %d", status)
	}

	var parsed struct {
		Code              int    `json:"code"`
		Msg               string `json:"msg"`
		TenantAccessToken string `json:"tenant_access_token"`
		Expire            int    `json:"expire"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", fmt.Errorf("decode feishu token response: %w", err)
	}
	if parsed.Code != 0 || strings.TrimSpace(parsed.TenantAccessToken) == "" {
		return "", fmt.Errorf("feishu token error code %d: %s", parsed.Code, parsed.Msg)
	}

	ttl := time.Duration(parsed.Expire) * time.Second
	if ttl <= 0 {
		ttl = 2 * time.Hour
	}
	g.feishuToken = parsed.TenantAccessToken
	g.feishuTokenExp = g.now().UTC().Add(ttl - feishuTokenSafetyMargin)
	return g.feishuToken, nil
}

func feishuResponseCode(body []byte) (int, string) {
	var parsed struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return 0, ""
	}
	return parsed.Code, parsed.Msg
}

// feishuMessageID extracts data.message_id from a Feishu send/reply response.
func feishuMessageID(body []byte) string {
	var parsed struct {
		Data struct {
			MessageID string `json:"message_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return ""
	}
	return strings.TrimSpace(parsed.Data.MessageID)
}

// postFeishuAppMessage sends one message through the app bot. When rootMessageID
// is non-empty the message is threaded as a reply under that root
// (reply_in_thread), so all of a loop's messages collapse into one thread;
// otherwise it is posted top-level to chatID. Returns the new message id.
func (g *Gateway) postFeishuAppMessage(ctx context.Context, token, chatID, rootMessageID, msgType, content string) (string, error) {
	var apiURL string
	var payload map[string]any
	if strings.TrimSpace(rootMessageID) != "" {
		apiURL = feishuAPIBase + "/open-apis/im/v1/messages/" + strings.TrimSpace(rootMessageID) + "/reply"
		payload = map[string]any{"msg_type": msgType, "content": content, "reply_in_thread": true}
	} else {
		apiURL = feishuAPIBase + "/open-apis/im/v1/messages?receive_id_type=chat_id"
		payload = map[string]any{"receive_id": chatID, "msg_type": msgType, "content": content}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	status, respBody, err := g.feishuAppHTTP(ctx, http.MethodPost, apiURL, map[string]string{
		"Authorization": "Bearer " + token,
		"Content-Type":  "application/json; charset=utf-8",
	}, body)
	if err != nil {
		return "", err
	}
	if status < 200 || status >= 300 {
		return "", fmt.Errorf("feishu im responded with status %d", status)
	}
	if code, msg := feishuResponseCode(respBody); code != 0 {
		return "", fmt.Errorf("feishu im error code %d: %s", code, msg)
	}
	return feishuMessageID(respBody), nil
}

// ensureFeishuThreadRoot returns the Feishu root message id a loop's notifications
// thread under, creating it (a plain-text "开始处理" header) on first use. Returns
// "" when loopID is empty or the root can't be created — callers then post
// top-level, so threading degrades gracefully rather than dropping messages.
func (g *Gateway) ensureFeishuThreadRoot(ctx context.Context, token, chatID, loopID string) string {
	loopID = strings.TrimSpace(loopID)
	if loopID == "" || g.repositories == nil || g.repositories.FeishuThreads == nil {
		return ""
	}
	if root, err := g.repositories.FeishuThreads.RootByLoop(ctx, loopID); err == nil && root != "" {
		return root
	}
	// The thread anchor is the first thing a human sees, so render it as a compact
	// task-header card (linked source · repo · trigger · title); fall back to plain
	// text if the loop can't be loaded into a card.
	msgType, content := "text", ""
	if cardJSON, ok := g.feishuThreadHeaderCard(ctx, loopID); ok {
		msgType, content = "interactive", cardJSON
	} else {
		raw, err := json.Marshal(map[string]string{"text": g.feishuThreadHeaderText(ctx, loopID)})
		if err != nil {
			return ""
		}
		content = string(raw)
	}
	msgID, err := g.postFeishuAppMessage(ctx, token, chatID, "", msgType, content)
	if err != nil || msgID == "" {
		return ""
	}
	// Persisted (not just in-memory) so the inbound callback can reverse-map a
	// thread reply back to this loop, and so it survives a daemon restart.
	_ = g.repositories.FeishuThreads.Upsert(ctx, msgID, loopID, chatID, eventlog.FormatJavaScriptISOString(g.now()))
	return msgID
}

// feishuThreadHeaderText builds a loop thread's plain-text root header, e.g.
// "🔧 开始处理 #360 · owner/repo\n<title>". Best-effort; falls back to the loop id.
func (g *Gateway) feishuThreadHeaderText(ctx context.Context, loopID string) string {
	var loop *storage.LoopRecord
	if g.repositories != nil && g.repositories.Loops != nil {
		if got, err := g.repositories.Loops.GetByID(ctx, loopID); err == nil {
			loop = got
		}
	}
	if loop == nil {
		return "🔧 开始处理任务 " + loopID
	}
	head := "🔧 开始处理"
	target := ""
	if loop.TargetID != nil {
		target = *loop.TargetID
	}
	if label := humanizeLoopTarget(target); label != "" {
		head += " " + label
	}
	if loop.Repo != nil && strings.TrimSpace(*loop.Repo) != "" {
		head += " · " + strings.TrimSpace(*loop.Repo)
	}
	if title := loopTitleFromMetadata(loop.MetadataJSON); title != "" {
		head += "\n" + title
	}
	return head
}

// feishuThreadHeaderCard renders the thread anchor as a compact task-header card:
// a linked source (Issue #N → url) · repo · who triggered it, plus the title.
// Returns ("", false) when the loop can't be loaded, so the caller falls back to
// the plain-text header.
func (g *Gateway) feishuThreadHeaderCard(ctx context.Context, loopID string) (string, bool) {
	if g.repositories == nil || g.repositories.Loops == nil {
		return "", false
	}
	loop, err := g.repositories.Loops.GetByID(ctx, loopID)
	if err != nil || loop == nil {
		return "", false
	}
	target := ""
	if loop.TargetID != nil {
		target = *loop.TargetID
	}
	// Source = where the task CAME FROM (the originating issue), kept stable even
	// after the loop target flips to the PR it opens — so the anchor never relabels
	// its own source line as "PR" (which mismatched the issue link before).
	issueURL := loopWorkerString(loop.MetadataJSON, "issueUrl")
	sourceLabel, sourceURL := "", ""
	if issueURL != "" {
		sourceLabel = "Issue"
		if n := urlTrailingNumber(issueURL); n != "" {
			sourceLabel = "Issue #" + n
		}
		sourceURL = issueURL
	} else {
		sourceLabel = humanizeLoopTarget(target) // PR-triggered loop: no originating issue
	}
	prURL := loopWorkerString(loop.MetadataJSON, "prUrl")
	trigger := loopWorkerString(loop.MetadataJSON, "triggerLogin")
	repo := ""
	if loop.Repo != nil {
		repo = strings.TrimSpace(*loop.Repo)
	}
	title := loopTitleFromMetadata(loop.MetadataJSON)

	parts := make([]string, 0, 3)
	if sourceLabel != "" {
		if sourceURL != "" {
			parts = append(parts, "📋 ["+sourceLabel+"]("+sourceURL+")")
		} else {
			parts = append(parts, "📋 "+sourceLabel)
		}
	}
	if repo != "" {
		parts = append(parts, repo)
	}
	if trigger != "" {
		parts = append(parts, "由 @"+strings.TrimPrefix(trigger, "@")+" 提出")
	}
	if len(parts) == 0 && title == "" {
		return "", false
	}
	elements := make([]any, 0, 6)
	if len(parts) > 0 {
		elements = append(elements, larkDiv(strings.Join(parts, " · ")))
	}
	if title != "" {
		elements = append(elements, larkDiv("**"+title+"**"))
	}
	// Key milestone, surfaced on the OUTER card: the PR this task opened, linked —
	// so a human sees the deliverable without opening the thread and scrolling to
	// the bottom for the "Opened a PR" reply.
	if prURL != "" {
		prLabel := "PR"
		if n := urlTrailingNumber(prURL); n != "" {
			prLabel = "PR #" + n
		}
		elements = append(elements, larkDiv("🔀 已开 ["+prLabel+"]("+prURL+") →"))
	}
	// The narrative — NOT the raw tool feed (that lives inside the thread, see
	// updateLiveFeedComment). A timestamped milestone log (decisions · phases · PR)
	// when we have one, else a single current-phase brief while nothing notable has
	// landed yet.
	tail, elapsed := g.liveTailFor(loopID)
	if milestones := loops.ReadMilestones(loop.MetadataJSON); len(milestones) > 0 {
		elements = append(elements, map[string]any{"tag": "hr"})
		elements = append(elements, larkDiv(feishuMilestoneList(milestones)))
	} else if brief := feishuAnchorBrief(loop, tail); brief != "" {
		elements = append(elements, map[string]any{"tag": "hr"})
		elements = append(elements, larkDiv(brief))
	}
	noteParts := make([]string, 0, 2)
	if elapsed > 0 {
		noteParts = append(noteParts, "⏱ "+humanizeElapsedSeconds(elapsed))
	}
	noteParts = append(noteParts, "展开话题看实时进度 →")
	elements = append(elements, map[string]any{"tag": "note", "elements": []any{map[string]any{"tag": "lark_md", "content": strings.Join(noteParts, " · ")}}})
	// One-command local takeover of the agent session (runs on the daemon host).
	// Offered only while the loop is live; dropped once it reaches a terminal state.
	if !feishuLoopStatusTerminal(loop.Status) {
		elements = append(elements, map[string]any{"tag": "note", "elements": []any{map[string]any{"tag": "lark_md", "content": "💻 本地接管:`looper resume " + strconv.FormatInt(loop.Seq, 10) + "`"}}})
	}
	template, label := feishuLoopStatusStyle(loop.Status)
	cardObj := map[string]any{
		"config":   map[string]any{"wide_screen_mode": true},
		"header":   map[string]any{"template": template, "title": map[string]any{"tag": "plain_text", "content": label}},
		"elements": elements,
	}
	raw, err := json.Marshal(cardObj)
	if err != nil {
		return "", false
	}
	return string(raw), true
}

// feishuLoopStatusTerminal reports whether a loop has reached an end state — no
// further automated work will run, so surfaces like the takeover hint drop off.
func feishuLoopStatusTerminal(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "completed", "done", "merged", "failed", "abandoned", "error", "terminated", "stopped":
		return true
	default:
		return false
	}
}

// feishuLoopStatusStyle maps a loop status to the thread-header card's colour +
// label, so the anchor card visibly reflects where the task is: processing (blue)
// → awaiting a human (orange) → done (green) → needs attention (red).
func feishuLoopStatusStyle(status string) (template, label string) {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "awaiting_human":
		return "orange", "⏸ Looper 等你定夺"
	case "completed", "done", "merged":
		return "green", "✅ Looper 已完成"
	case "failed", "abandoned", "error":
		return "red", "⚠️ Looper 需要处理"
	default:
		return "blue", "🔧 Looper 处理中"
	}
}

// liveTailEntry is the most recent agent-activity snapshot for one loop.
type liveTailEntry struct {
	lines      []string
	elapsedSec int64
}

// RefreshThreadHeader records a loop's latest live activity (last few agent
// output lines + elapsed) in memory and re-renders its Feishu anchor card. Called
// by the progress ticker while an agent runs. Best-effort + a no-op when the
// app-bot isn't configured. In-memory storage means this never touches the loop
// record, so it can't race the scheduler's loop/run writes.
func (g *Gateway) RefreshThreadHeader(ctx context.Context, loopID string, tail []string, elapsedSec int64) {
	loopID = strings.TrimSpace(loopID)
	if loopID == "" {
		return
	}
	if len(tail) > 0 || elapsedSec > 0 {
		g.liveMu.Lock()
		if g.liveTails == nil {
			g.liveTails = map[string]liveTailEntry{}
		}
		g.liveTails[loopID] = liveTailEntry{lines: tail, elapsedSec: elapsedSec}
		g.liveMu.Unlock()
	}
	cfg := g.config.Webhook
	appID := strings.TrimSpace(os.Getenv(strings.TrimSpace(cfg.AppIDEnv)))
	appSecret := strings.TrimSpace(os.Getenv(strings.TrimSpace(cfg.AppSecretEnv)))
	if appID == "" || appSecret == "" {
		return
	}
	token, err := g.feishuTenantToken(ctx, appID, appSecret)
	if err != nil {
		return
	}
	// Ensure the anchor exists NOW so live progress is visible DURING the run: the
	// lifecycle Notify path that would otherwise create it is level-filtered (info
	// updates are dropped) and only fires the anchor at completion. Idempotent.
	if chatID := strings.TrimSpace(cfg.ChatID); chatID != "" {
		g.ensureFeishuThreadRoot(ctx, token, chatID, loopID)
	}
	// Anchor (topic root) → human brief; live tool feed → its own reply in-thread.
	g.updateFeishuThreadHeader(ctx, token, loopID)
	g.updateLiveFeedComment(ctx, token, loopID, tail, elapsedSec)
}

// updateLiveFeedComment posts (first time) or patches (thereafter) the raw
// tool-call feed as a reply threaded under the loop's anchor — the in-thread
// real-time status sync surface. A no-op until the anchor root exists or when
// there is no activity yet (so a terminal refresh with an empty tail leaves the
// last feed in place rather than blanking it).
func (g *Gateway) updateLiveFeedComment(ctx context.Context, token, loopID string, tail []string, elapsedSec int64) {
	loopID = strings.TrimSpace(loopID)
	if loopID == "" || len(tail) == 0 || g.repositories == nil || g.repositories.FeishuThreads == nil {
		return
	}
	root, err := g.repositories.FeishuThreads.RootByLoop(ctx, loopID)
	if err != nil || strings.TrimSpace(root) == "" {
		return // anchor not posted yet — nothing to thread under
	}
	cardJSON, ok := feishuLiveFeedCard(tail, elapsedSec)
	if !ok {
		return
	}
	g.liveMu.Lock()
	msgID := ""
	if g.liveFeeds != nil {
		msgID = g.liveFeeds[loopID]
	}
	g.liveMu.Unlock()
	if msgID != "" {
		_ = g.patchFeishuAppCard(ctx, token, msgID, cardJSON)
		return
	}
	chatID := strings.TrimSpace(g.config.Webhook.ChatID)
	if chatID == "" {
		return
	}
	newID, err := g.postFeishuAppMessage(ctx, token, chatID, root, "interactive", cardJSON)
	if err != nil || strings.TrimSpace(newID) == "" {
		return
	}
	g.liveMu.Lock()
	if g.liveFeeds == nil {
		g.liveFeeds = map[string]string{}
	}
	g.liveFeeds[loopID] = newID
	g.liveMu.Unlock()
}

// liveTailFor returns the retained live activity snapshot for a loop.
func (g *Gateway) liveTailFor(loopID string) ([]string, int64) {
	g.liveMu.Lock()
	defer g.liveMu.Unlock()
	if g.liveTails == nil {
		return nil, 0
	}
	e := g.liveTails[strings.TrimSpace(loopID)]
	return e.lines, e.elapsedSec
}

// updateFeishuThreadHeader re-renders a loop's thread-anchor card to reflect its
// current status (colour + label). Best-effort: a loop with no root card yet, or
// a failed PATCH, is a no-op.
func (g *Gateway) updateFeishuThreadHeader(ctx context.Context, token, loopID string) {
	if g.repositories == nil || g.repositories.FeishuThreads == nil {
		return
	}
	root, err := g.repositories.FeishuThreads.RootByLoop(ctx, strings.TrimSpace(loopID))
	if err != nil || strings.TrimSpace(root) == "" {
		return
	}
	cardJSON, ok := g.feishuThreadHeaderCard(ctx, loopID)
	if !ok {
		return
	}
	_ = g.patchFeishuAppCard(ctx, token, root, cardJSON)
}

// patchFeishuAppCard updates an already-sent interactive card in place.
func (g *Gateway) patchFeishuAppCard(ctx context.Context, token, messageID, content string) error {
	apiURL := feishuAPIBase + "/open-apis/im/v1/messages/" + strings.TrimSpace(messageID)
	body, err := json.Marshal(map[string]any{"content": content})
	if err != nil {
		return err
	}
	status, respBody, err := g.feishuAppHTTP(ctx, http.MethodPatch, apiURL, map[string]string{
		"Authorization": "Bearer " + token,
		"Content-Type":  "application/json; charset=utf-8",
	}, body)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("feishu update card responded with status %d", status)
	}
	if code, msg := feishuResponseCode(respBody); code != 0 {
		return fmt.Errorf("feishu update card error code %d: %s", code, msg)
	}
	return nil
}

// loopWorkerField extracts worker.<key> (any type) from a loop's metadata JSON.
func loopWorkerField(metadataJSON *string, key string) any {
	if metadataJSON == nil || strings.TrimSpace(*metadataJSON) == "" {
		return nil
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(*metadataJSON), &meta); err != nil {
		return nil
	}
	if w, ok := meta["worker"].(map[string]any); ok {
		return w[key]
	}
	return nil
}

// loopWorkerString extracts worker.<key> (a string) from a loop's metadata JSON.
func loopWorkerString(metadataJSON *string, key string) string {
	if v, ok := loopWorkerField(metadataJSON, key).(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

// feishuActivityTail renders the last few agent output lines as a compact log.
func feishuActivityTail(lines []string) string {
	rendered := make([]string, 0, len(lines)+1)
	rendered = append(rendered, "🔧 实时进度")
	for _, line := range lines {
		if len(line) > 90 {
			line = line[:90] + "…"
		}
		rendered = append(rendered, "· "+line)
	}
	return strings.Join(rendered, "\n")
}

// feishuAnchorBrief returns the one-line, human-scannable status for the anchor
// card — NOT the raw tool feed. It prefers the agent's own summary (genuinely
// AI-written, present once the loop reaches a terminal/awaiting state) and falls
// back to a natural-language phase inferred from the latest activity while the
// agent is still running. Returns "" when there is nothing meaningful to say yet.
func feishuAnchorBrief(loop *storage.LoopRecord, tail []string) string {
	if loop != nil {
		for _, key := range []string{"summary", "changeSummary", "resultSummary", "outcome"} {
			if s := loopWorkerString(loop.MetadataJSON, key); s != "" {
				if len(s) > 160 {
					s = s[:160] + "…"
				}
				return "📝 " + s
			}
		}
		// A terminal loop is no longer "正在…"; without a summary the green/orange/red
		// header + title already say enough — don't contradict it with a stale phase.
		if loop.Status != "" {
			switch strings.ToLower(strings.TrimSpace(loop.Status)) {
			case "completed", "done", "merged", "failed", "abandoned", "error", "awaiting_human", "terminated", "stopped":
				return ""
			}
		}
	}
	if phase := feishuPhaseFromTail(tail); phase != "" {
		return "🔧 " + phase
	}
	return ""
}

// feishuPhaseFromTail maps the most recent tool command to a short natural-language
// phase ("正在开 PR", "正在跑测试", …) so a human scanning the group sees WHAT the
// agent is doing rather than a raw shell line. Falls back to the trimmed command.
func feishuPhaseFromTail(tail []string) string {
	if len(tail) == 0 {
		return ""
	}
	line := tail[len(tail)-1]
	// Strip the leading status icon ("✅ " / "❌ " / "⏳ ") the tool feed prefixes.
	if i := strings.IndexByte(line, ' '); i >= 0 && len([]rune(line[:i])) <= 2 {
		line = strings.TrimSpace(line[i+1:])
	}
	lower := strings.ToLower(line)
	switch {
	case strings.Contains(lower, "gh pr create") || strings.Contains(lower, "pull request"):
		return "正在开 PR"
	case strings.Contains(lower, "gh pr ") || strings.Contains(lower, "gh api"):
		return "正在处理 PR"
	case strings.Contains(lower, "git push"):
		return "正在推送分支"
	case strings.Contains(lower, "git commit"):
		return "正在提交改动"
	case strings.Contains(lower, "git clone") || strings.Contains(lower, "git checkout") || strings.Contains(lower, "git fetch") || strings.Contains(lower, "git worktree"):
		return "正在准备代码"
	case strings.Contains(lower, "test") || strings.Contains(lower, "vitest") || strings.Contains(lower, "jest") || strings.Contains(lower, "pytest") || strings.Contains(lower, "lint") || strings.Contains(lower, "build"):
		return "正在跑测试/构建"
	case strings.Contains(lower, "apply_patch") || strings.Contains(lower, "sed -i") || strings.Contains(lower, "tee ") || strings.HasPrefix(lower, "cat >") || strings.Contains(lower, " > "):
		return "正在改动代码"
	case strings.HasPrefix(lower, "rg ") || strings.HasPrefix(lower, "grep ") || strings.HasPrefix(lower, "ls ") || strings.HasPrefix(lower, "cat ") || strings.HasPrefix(lower, "find ") || strings.Contains(lower, "git status") || strings.Contains(lower, "git log") || strings.Contains(lower, "git diff"):
		return "正在阅读代码"
	default:
		if len(line) > 60 {
			line = line[:60] + "…"
		}
		return line
	}
}

// feishuLiveFeedCard renders the raw tool-call feed as a standalone card, posted
// as the first reply INSIDE the thread — the real-time status sync surface, kept
// separate from the human-scannable anchor.
func feishuLiveFeedCard(tail []string, elapsedSec int64) (string, bool) {
	if len(tail) == 0 {
		return "", false
	}
	elements := []any{larkDiv(feishuActivityTail(tail))}
	note := "实时更新中"
	if elapsedSec > 0 {
		note = "⏱ " + humanizeElapsedSeconds(elapsedSec) + " · 实时更新中"
	}
	elements = append(elements, map[string]any{"tag": "note", "elements": []any{map[string]any{"tag": "lark_md", "content": note}}})
	cardObj := map[string]any{
		"config":   map[string]any{"wide_screen_mode": true},
		"header":   map[string]any{"template": "wathet", "title": map[string]any{"tag": "plain_text", "content": "⚙️ 实时进度同步"}},
		"elements": elements,
	}
	raw, err := json.Marshal(cardObj)
	if err != nil {
		return "", false
	}
	return string(raw), true
}

// humanizeElapsedSeconds turns 134 into "2m14s", 45 into "45s".
func humanizeElapsedSeconds(sec int64) string {
	if sec < 0 {
		sec = 0
	}
	if sec < 60 {
		return strconv.FormatInt(sec, 10) + "s"
	}
	return strconv.FormatInt(sec/60, 10) + "m" + strconv.FormatInt(sec%60, 10) + "s"
}

// urlTrailingNumber extracts the trailing numeric path segment from a URL, e.g.
// ".../issues/153" → "153", ".../pull/154" → "154". Returns "" when the last
// segment is not a plain number (tolerant of trailing slashes / query strings).
func urlTrailingNumber(u string) string {
	u = strings.TrimSpace(u)
	if i := strings.IndexAny(u, "?#"); i >= 0 {
		u = u[:i]
	}
	u = strings.TrimRight(u, "/")
	if i := strings.LastIndexByte(u, '/'); i >= 0 {
		u = u[i+1:]
	}
	if u == "" {
		return ""
	}
	if _, err := strconv.Atoi(u); err != nil {
		return ""
	}
	return u
}

// humanizeLoopTarget turns a loop target id ("issue:owner/repo:360",
// "pr:owner/repo:8") into a short "#360" / "PR #8" label. Returns "" when the
// trailing segment is not a number (e.g. project-scoped loops).
func humanizeLoopTarget(targetID string) string {
	targetID = strings.TrimSpace(targetID)
	if targetID == "" {
		return ""
	}
	parts := strings.Split(targetID, ":")
	num := strings.TrimSpace(parts[len(parts)-1])
	if _, err := strconv.Atoi(num); err != nil {
		return ""
	}
	if strings.HasPrefix(targetID, "pr:") || strings.HasPrefix(targetID, "pull_request:") {
		return "PR #" + num
	}
	return "#" + num
}

// loopTitleFromMetadata best-effort reads a human task title from loop metadata,
// tolerating both a top-level "title" and a nested "worker.title".
func loopTitleFromMetadata(metadataJSON *string) string {
	if metadataJSON == nil || strings.TrimSpace(*metadataJSON) == "" {
		return ""
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(*metadataJSON), &meta); err != nil {
		return ""
	}
	if t, ok := meta["title"].(string); ok && strings.TrimSpace(t) != "" {
		return strings.TrimSpace(t)
	}
	if w, ok := meta["worker"].(map[string]any); ok {
		if t, ok := w["title"].(string); ok && strings.TrimSpace(t) != "" {
			return strings.TrimSpace(t)
		}
	}
	return ""
}

func defaultFeishuAppHTTP(ctx context.Context, method, url string, headers map[string]string, body []byte) (int, []byte, error) {
	client := &http.Client{Timeout: webhookTimeout}

	request, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}

	response, err := client.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()

	responseBody, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return response.StatusCode, nil, err
	}
	return response.StatusCode, responseBody, nil
}

func webhookLevelAllowed(levels []config.NotificationSoundLevel, level string) bool {
	allowed := levels
	if len(allowed) == 0 {
		allowed = []config.NotificationSoundLevel{
			config.NotificationSoundLevelActionRequired,
			config.NotificationSoundLevelFailure,
		}
	}

	for _, candidate := range allowed {
		if string(candidate) == level {
			return true
		}
	}

	return false
}

type webhookGenericBody struct {
	Level      string `json:"level"`
	Title      string `json:"title"`
	Subtitle   string `json:"subtitle,omitempty"`
	Body       string `json:"body"`
	ProjectID  string `json:"projectId,omitempty"`
	LoopID     string `json:"loopId,omitempty"`
	RunID      string `json:"runId,omitempty"`
	EntityType string `json:"entityType,omitempty"`
	EntityID   string `json:"entityId,omitempty"`
	DedupeKey  string `json:"dedupeKey,omitempty"`
}

func buildWebhookBody(format string, payload SystemNotificationPayload) ([]byte, error) {
	if format == "feishu" {
		text := payload.Title
		if strings.TrimSpace(payload.Subtitle) != "" {
			text += "\n" + payload.Subtitle
		}
		if strings.TrimSpace(payload.Body) != "" {
			text += "\n" + payload.Body
		}

		return json.Marshal(map[string]any{
			"msg_type": "text",
			"content": map[string]any{
				"text": text,
			},
		})
	}

	return json.Marshal(webhookGenericBody{
		Level:      payload.Level,
		Title:      payload.Title,
		Subtitle:   payload.Subtitle,
		Body:       payload.Body,
		ProjectID:  payload.ProjectID,
		LoopID:     payload.LoopID,
		RunID:      payload.RunID,
		EntityType: payload.EntityType,
		EntityID:   payload.EntityID,
		DedupeKey:  payload.DedupeKey,
	})
}

func defaultWebhookPost(url string, body []byte) (int, error) {
	client := &http.Client{Timeout: webhookTimeout}

	request, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	request.Header.Set("Content-Type", "application/json")

	response, err := client.Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()

	_, _ = io.Copy(io.Discard, response.Body)

	return response.StatusCode, nil
}

func (g *Gateway) persistNotification(ctx context.Context, record storage.NotificationRecord) error {
	if g.repositories == nil || g.repositories.Notifications == nil || g.repositories.Events == nil {
		return fmt.Errorf("notification repositories are not configured")
	}

	if err := g.repositories.Notifications.Upsert(ctx, record); err != nil {
		return err
	}

	return eventlog.Append(ctx, g.repositories, eventlog.AppendInput{
		ID:         eventlog.NewEventID("event"),
		EventType:  "notification.sent",
		ProjectID:  record.ProjectID,
		LoopID:     record.LoopID,
		RunID:      record.RunID,
		EntityType: firstPointer(record.EntityType, stringPointer("notification")),
		EntityID:   firstPointer(record.EntityID, &record.ID),
		Payload: map[string]any{
			"channel":   record.Channel,
			"level":     record.Level,
			"status":    record.Status,
			"dedupeKey": record.DedupeKey,
			"title":     record.Title,
		},
		CreatedAt: mustParseJSISOString(record.CreatedAt),
	})
}

func buildAppleScript(payload SystemNotificationPayload, cfg config.NotificationConfig, logFilePath string) string {
	body := escapeAppleScriptString(payload.Body)
	title := escapeAppleScriptString(payload.Title)

	if payload.Level == "failure" && strings.TrimSpace(logFilePath) != "" {
		openLogPath := escapeAppleScriptString(logFilePath)
		return fmt.Sprintf(`set dialogResult to display dialog %q with title %q buttons {"Open Log", "Dismiss"} default button "Dismiss" cancel button "Dismiss" giving up after 30
if gave up of dialogResult is false and button returned of dialogResult is "Open Log" then
  do shell script "open " & quoted form of %q
end if`, body, title, openLogPath)
	}

	subtitle := ""
	if payload.Subtitle != "" {
		subtitle = fmt.Sprintf(` subtitle %q`, escapeAppleScriptString(payload.Subtitle))
	}

	sound := ""
	if payload.Sound != "" && isSoundEnabledForLevel(cfg, payload.Level) {
		sound = fmt.Sprintf(` sound name %q`, escapeAppleScriptString(payload.Sound))
	}

	return fmt.Sprintf(`display notification %q with title %q%s%s`, body, title, subtitle, sound)
}

func escapeAppleScriptString(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	value = strings.ReplaceAll(value, "\n", " ")
	return value
}

func isSoundEnabledForLevel(cfg config.NotificationConfig, level string) bool {
	for _, candidate := range cfg.Osascript.SoundForLevels {
		if string(candidate) == level {
			return true
		}
	}

	return false
}

func mustMarshalPayload(payload SystemNotificationPayload) string {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "{}"
	}

	return string(encoded)
}

func mustParseJSISOString(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Now().UTC()
	}

	return parsed
}

func nilIfEmpty(value string) *string {
	if strings.TrimSpace(value) == "" {
		return nil
	}

	return &value
}

func stringPointer(value string) *string {
	return &value
}

func firstPointer(values ...*string) *string {
	for _, value := range values {
		if value != nil {
			return value
		}
	}

	return nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}

	return ""
}

func ternaryString(condition bool, whenTrue, whenFalse string) string {
	if condition {
		return whenTrue
	}

	return whenFalse
}

func ternaryPointer(condition bool, value string) *string {
	if !condition {
		return nil
	}

	return &value
}

func ternaryTimePointer(condition bool, value string) *string {
	if !condition {
		return nil
	}

	return &value
}
