package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
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
	gatewayStateLoopLimit   = 4096
)

type RunCommandFunc func(context.Context, shell.Options) (shell.Result, error)

// HTTPPostFunc delivers a webhook notification body to url and returns the
// HTTP status code. It is injectable so tests can avoid real network calls.
type HTTPPostFunc func(url string, body []byte) (int, error)

// FeishuAppHTTPFunc performs an HTTP request for the Feishu app-bot delivery
// (token fetch + message send) and returns the status code and response body.
// It is injectable so tests can avoid real network calls.
type FeishuAppHTTPFunc func(ctx context.Context, method, url string, headers map[string]string, body []byte) (int, []byte, error)

// ResolveOwnerOpenID returns the Feishu open_id of the person who owns this
// Looper deployment for a project. The identity is attribution only: Feishu may
// render the mention grey when that person is not a member of the destination
// chat, and delivery must still proceed.
type ResolveOwnerOpenID func(projectID string) string

type Options struct {
	Config             config.NotificationConfig
	OsascriptPath      string
	LogFilePath        string
	Repositories       *storage.Repositories
	State              *GatewayState
	Now                func() time.Time
	RunCommand         RunCommandFunc
	HTTPPost           HTTPPostFunc
	FeishuAppHTTP      FeishuAppHTTPFunc
	ResolveOwnerOpenID ResolveOwnerOpenID
}

// GatewayState retains transport continuity across immutable, config-specific
// Gateway instances. Policy stays on each Gateway; only token/live-card state
// needed to update an already-created notification is shared.
type GatewayState struct {
	feishuTokenMu  sync.Mutex
	feishuToken    string
	feishuTokenExp time.Time

	liveMu sync.Mutex
	// liveTails retains the most recent agent-activity snapshot per loop so the
	// completion card can include the final live tail.
	liveTails map[string]liveTailEntry
	// askCards retains the message/card pair needed to patch an answered ask.
	askCards map[string]askCardState
	// liveFeeds retains each loop's in-thread live-progress message id.
	liveFeeds map[string]string
	// loopLastUsed bounds the union of loop-scoped transport entries without
	// persisting delivery state. All access is protected by liveMu.
	loopLastUsed  map[string]uint64
	loopUseSerial uint64
}

func NewGatewayState() *GatewayState {
	return &GatewayState{}
}

func (s *GatewayState) touchLoopLocked(loopID string) {
	loopID = strings.TrimSpace(loopID)
	if loopID == "" {
		return
	}
	if s.loopLastUsed == nil {
		s.loopLastUsed = make(map[string]uint64)
	}
	if _, exists := s.loopLastUsed[loopID]; !exists && len(s.loopLastUsed) >= gatewayStateLoopLimit {
		oldestLoop := ""
		oldestUse := ^uint64(0)
		for candidate, lastUse := range s.loopLastUsed {
			if lastUse < oldestUse {
				oldestLoop = candidate
				oldestUse = lastUse
			}
		}
		delete(s.loopLastUsed, oldestLoop)
		delete(s.liveTails, oldestLoop)
		delete(s.askCards, oldestLoop)
		delete(s.liveFeeds, oldestLoop)
	}
	s.loopUseSerial++
	s.loopLastUsed[loopID] = s.loopUseSerial
}

func (s *GatewayState) forgetLoopIfUnusedLocked(loopID string) {
	if _, exists := s.liveTails[loopID]; exists {
		return
	}
	if _, exists := s.askCards[loopID]; exists {
		return
	}
	if _, exists := s.liveFeeds[loopID]; exists {
		return
	}
	delete(s.loopLastUsed, loopID)
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
	// MentionOpenIds @-mentions these Feishu open_ids in the thread message — set only
	// for milestones a human should see (e.g. the impl PR is ready), not routine updates.
	MentionOpenIds []string
}

type Gateway struct {
	config             config.NotificationConfig
	osascriptPath      string
	logFilePath        string
	repositories       *storage.Repositories
	now                func() time.Time
	runCommand         RunCommandFunc
	httpPost           HTTPPostFunc
	feishuAppHTTP      FeishuAppHTTPFunc
	resolveOwnerOpenID ResolveOwnerOpenID
	state              *GatewayState

	rootMu    sync.Mutex
	rootLocks map[string]*sync.Mutex

	intakeMu       sync.Mutex
	intakeOutcomes map[string]anchorOutcomeStyle
}

type askCardState struct {
	msgID string
	card  HITLAskCard
}

// anchorOutcomeStyle is a header colour+label override for a retired anchor card.
type anchorOutcomeStyle struct {
	template string
	label    string
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
	state := options.State
	if state == nil {
		state = NewGatewayState()
	}

	return &Gateway{
		config:             options.Config,
		osascriptPath:      options.OsascriptPath,
		logFilePath:        options.LogFilePath,
		repositories:       options.Repositories,
		now:                now,
		runCommand:         runCommand,
		httpPost:           httpPost,
		feishuAppHTTP:      feishuAppHTTP,
		resolveOwnerOpenID: options.ResolveOwnerOpenID,
		state:              state,
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
	body := strings.Join(lines, "\n")
	// Feishu text @-mention: <at user_id="ou_..."></at> (distinct from the card form).
	mention := ""
	for _, id := range payload.MentionOpenIds {
		if id = strings.TrimSpace(id); id != "" {
			mention += `<at user_id="` + id + `"></at> `
		}
	}
	return mention + body
}

// HITLAskCard is a one-way Feishu notification for a decision that lives in
// Plane or GitHub. The only card action opens the exact source-of-truth comment.
type HITLAskCard struct {
	ProjectID      string
	LoopID         string
	LoopSeq        int64
	Repo           string
	Title          string
	Question       string
	Options        []string
	MentionOpenIds []string
	// OwnerOpenID identifies which teammate's local Looper emitted the card. It is
	// deliberately independent from MentionOpenIds: the owner need not be the
	// decision maker and need not be a member of the destination chat.
	OwnerOpenID string

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
	// AnsweredWith renders the notification as resolved after the authoritative
	// Plane/GitHub answer is accepted. Feishu itself never supplies this value.
	AnsweredWith string
}

type feishuDecisionCardIntent struct {
	LoopID         string   `json:"loopId"`
	Body           string   `json:"body"`
	ActionURL      string   `json:"actionUrl"`
	MentionOpenIDs []string `json:"mentionOpenIds,omitempty"`
	Purpose        string   `json:"purpose,omitempty"`
}

type feishuCardIntent struct {
	Kind     string                    `json:"kind"`
	HITL     *HITLAskCard              `json:"hitl,omitempty"`
	Decision *feishuDecisionCardIntent `json:"decision,omitempty"`
	Attempts int64                     `json:"attempts,omitempty"`
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
	if strings.TrimSpace(card.SourceURL) == "" {
		return fmt.Errorf("decision notification requires an exact source-of-truth URL")
	}
	return g.enqueueFeishuCard(ctx, "Decision needed", card.Question, card.LoopID, feishuCardIntent{Kind: "hitl", HITL: &card})
}

func (g *Gateway) deliverHITLAsk(ctx context.Context, card HITLAskCard) error {
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
	card.MentionOpenIds = firstFeishuOwner(card.MentionOpenIds)
	if strings.TrimSpace(card.OwnerOpenID) == "" {
		card.OwnerOpenID = g.ownerOpenID(card.ProjectID)
	}
	cardJSON, err := buildFeishuAskCard(card)
	if err != nil {
		return err
	}
	// Thread the notification under the loop's root; its sole button is an outbound
	// deep link and never sends a decision back to Looper.
	rootMessageID := g.ensureFeishuThreadRoot(ctx, token, chatID, card.LoopID)
	// The loop is awaiting_human now — turn the anchor card orange "等你定夺".
	g.updateFeishuThreadHeader(ctx, token, card.LoopID)
	msgID, err := g.postFeishuAppMessage(ctx, token, chatID, rootMessageID, "interactive", string(cardJSON))
	if err != nil {
		return err
	}
	if strings.TrimSpace(msgID) != "" && strings.TrimSpace(card.LoopID) != "" {
		g.state.liveMu.Lock()
		if g.state.askCards == nil {
			g.state.askCards = map[string]askCardState{}
		}
		g.state.touchLoopLocked(card.LoopID)
		g.state.askCards[card.LoopID] = askCardState{msgID: msgID, card: card}
		g.state.liveMu.Unlock()
	}
	return nil
}

func firstFeishuOwner(openIDs []string) []string {
	for _, id := range openIDs {
		if id = strings.TrimSpace(id); id != "" {
			return []string{id}
		}
	}
	return nil
}

// MarkAskAnswered updates the outbound Feishu notification after an answer was
// accepted from its authoritative source. Feishu itself remains notification-only.
func (g *Gateway) MarkAskAnswered(ctx context.Context, loopID, answer string) {
	loopID = strings.TrimSpace(loopID)
	answer = strings.TrimSpace(answer)
	if loopID == "" || answer == "" {
		return
	}
	g.RecordMilestone(ctx, loopID, "🧑‍⚖️ 已定夺:"+answer)
	g.state.liveMu.Lock()
	st, ok := g.state.askCards[loopID]
	if ok {
		g.state.touchLoopLocked(loopID)
	}
	g.state.liveMu.Unlock()
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
		g.state.liveMu.Lock()
		delete(g.state.askCards, loopID)
		g.state.forgetLoopIfUnusedLocked(loopID)
		g.state.liveMu.Unlock()
	}
}

func (g *Gateway) enqueueFeishuCard(ctx context.Context, title, body, loopID string, intent feishuCardIntent) error {
	if g.repositories == nil || g.repositories.Notifications == nil || g.repositories.Events == nil {
		return fmt.Errorf("feishu card outbox repositories are not configured")
	}
	nowISO := eventlog.FormatJavaScriptISOString(g.now().UTC())
	payload, err := json.Marshal(intent)
	if err != nil {
		return err
	}
	record := storage.NotificationRecord{
		ID:          eventlog.NewEventID("notification"),
		LoopID:      g.existingLoopID(ctx, loopID),
		EntityType:  stringPointer("notification"),
		Channel:     "feishu_card",
		Level:       "action",
		Title:       strings.TrimSpace(title),
		Body:        strings.TrimSpace(body),
		Status:      "pending",
		PayloadJSON: stringPointer(string(payload)),
		CreatedAt:   nowISO,
		UpdatedAt:   nowISO,
	}
	if err := g.persistNotification(ctx, record); err != nil {
		return err
	}
	return g.attemptFeishuCard(ctx, record)
}

func (g *Gateway) existingLoopID(ctx context.Context, loopID string) *string {
	loopID = strings.TrimSpace(loopID)
	if loopID == "" || g.repositories == nil || g.repositories.Loops == nil {
		return nil
	}
	loop, err := g.repositories.Loops.GetByID(ctx, loopID)
	if err != nil || loop == nil {
		return nil
	}
	return &loopID
}

// RetryPendingCards redelivers due action-card intents. The intent is persisted
// before the first network call, so a daemon crash or transient Feishu outage cannot
// make an actionable request disappear silently.
func (g *Gateway) RetryPendingCards(ctx context.Context, limit int64) (int, error) {
	if g.repositories == nil || g.repositories.Notifications == nil {
		return 0, nil
	}
	due, err := g.repositories.Notifications.ListPendingOutbox(ctx, limit)
	if err != nil {
		return 0, err
	}
	retried := 0
	for _, record := range due {
		if !g.feishuCardRetryDue(record) {
			continue
		}
		_ = g.attemptFeishuCard(ctx, record)
		retried++
	}
	return retried, nil
}

func (g *Gateway) feishuCardRetryDue(record storage.NotificationRecord) bool {
	var intent feishuCardIntent
	if record.PayloadJSON == nil || json.Unmarshal([]byte(*record.PayloadJSON), &intent) != nil || intent.Attempts <= 0 {
		return true
	}
	updatedAt, err := time.Parse(time.RFC3339Nano, record.UpdatedAt)
	if err != nil {
		return true
	}
	exponent := intent.Attempts - 1
	if exponent > 5 {
		exponent = 5
	}
	delay := time.Duration(15*(int64(1)<<exponent)) * time.Second
	return !g.now().UTC().Before(updatedAt.Add(delay))
}

func (g *Gateway) attemptFeishuCard(ctx context.Context, record storage.NotificationRecord) error {
	var intent feishuCardIntent
	if record.PayloadJSON == nil || json.Unmarshal([]byte(*record.PayloadJSON), &intent) != nil {
		intent.Attempts = 5
		return g.finishFeishuCardAttempt(ctx, record, intent, fmt.Errorf("invalid Feishu card outbox payload"))
	}
	var deliveryErr error
	switch intent.Kind {
	case "hitl":
		if intent.HITL == nil {
			deliveryErr = fmt.Errorf("missing HITL card payload")
		} else {
			deliveryErr = g.deliverHITLAsk(ctx, *intent.HITL)
		}
	case "decision":
		if intent.Decision == nil {
			deliveryErr = fmt.Errorf("missing decision card payload")
		} else {
			deliveryErr = g.deliverThreadDecisionCard(ctx, *intent.Decision)
		}
	default:
		deliveryErr = fmt.Errorf("unsupported Feishu card intent %q", intent.Kind)
	}
	return g.finishFeishuCardAttempt(ctx, record, intent, deliveryErr)
}

func (g *Gateway) finishFeishuCardAttempt(ctx context.Context, record storage.NotificationRecord, intent feishuCardIntent, deliveryErr error) error {
	intent.Attempts++
	now := g.now().UTC()
	nowISO := eventlog.FormatJavaScriptISOString(now)
	record.UpdatedAt = nowISO
	if deliveryErr == nil {
		record.Status = "delivered"
		record.ErrorMessage = nil
		record.SentAt = &nowISO
	} else {
		record.ErrorMessage = stringPointer(deliveryErr.Error())
		if intent.Attempts >= 5 {
			record.Status = "failed"
		} else {
			record.Status = "pending"
		}
	}
	if payload, err := json.Marshal(intent); err == nil {
		record.PayloadJSON = stringPointer(string(payload))
	}
	if err := g.persistNotification(ctx, record); err != nil {
		return err
	}
	return deliveryErr
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
	options := make([]string, 0, len(card.Options))
	for _, option := range card.Options {
		option = strings.TrimSpace(option)
		if option == "" {
			continue
		}
		if strings.EqualFold(option, strings.TrimSpace(card.RecommendedOption)) {
			option = "⭐ " + option + "（推荐）"
		}
		options = append(options, "- "+option)
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
	if len(options) > 0 {
		elements = append(elements, larkDiv("**可选项**\n"+strings.Join(options, "\n")))
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
	noteParts = append(noteParts, "loop "+seq, "请在来源系统回答；飞书回复不会被读取")
	elements = append(elements, map[string]any{"tag": "note", "elements": []any{map[string]any{"tag": "lark_md", "content": strings.Join(noteParts, " · ")}}})
	elements = append(elements, feishuLooperOwnerNote(card.OwnerOpenID))
	if answered := strings.TrimSpace(card.AnsweredWith); answered != "" {
		elements = append(elements, larkDiv("✅ **已定夺:"+answered+"** · Looper 继续处理中 →"))
	} else {
		elements = append(elements, map[string]any{"tag": "action", "actions": []any{map[string]any{
			"tag":  "button",
			"type": "primary",
			"text": map[string]any{"tag": "plain_text", "content": "前往回答"},
			"url":  strings.TrimSpace(card.SourceURL),
		}}})
	}

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

func (g *Gateway) ownerOpenID(projectID string) string {
	if g == nil || g.resolveOwnerOpenID == nil {
		return ""
	}
	return strings.TrimSpace(g.resolveOwnerOpenID(strings.TrimSpace(projectID)))
}

// feishuLooperOwnerNote makes cards from multiple teammates' local Loopers
// distinguishable in one shared group. An out-of-chat mention is intentionally
// retained; Feishu renders it grey but still shows the owner's identity.
func feishuLooperOwnerNote(openID string) map[string]any {
	identity := "未配置 owner"
	if openID = strings.TrimSpace(openID); openID != "" {
		identity = "<at id=" + openID + "></at>"
	}
	return map[string]any{"tag": "note", "elements": []any{map[string]any{"tag": "lark_md", "content": "来自 " + identity + " 的 Looper"}}}
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
	g.state.feishuTokenMu.Lock()
	defer g.state.feishuTokenMu.Unlock()

	if g.state.feishuToken != "" && g.now().UTC().Before(g.state.feishuTokenExp) {
		return g.state.feishuToken, nil
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
	g.state.feishuToken = parsed.TenantAccessToken
	g.state.feishuTokenExp = g.now().UTC().Add(ttl - feishuTokenSafetyMargin)
	return g.state.feishuToken, nil
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
	return g.postFeishuAppMessageWithUUID(ctx, token, chatID, rootMessageID, msgType, content, "")
}

func (g *Gateway) postFeishuAppMessageWithUUID(ctx context.Context, token, chatID, rootMessageID, msgType, content, uuid string) (string, error) {
	var apiURL string
	var payload map[string]any
	if strings.TrimSpace(rootMessageID) != "" {
		apiURL = feishuAPIBase + "/open-apis/im/v1/messages/" + strings.TrimSpace(rootMessageID) + "/reply"
		payload = map[string]any{"msg_type": msgType, "content": content, "reply_in_thread": true}
	} else {
		apiURL = feishuAPIBase + "/open-apis/im/v1/messages?receive_id_type=chat_id"
		payload = map[string]any{"receive_id": chatID, "msg_type": msgType, "content": content}
	}
	if strings.TrimSpace(uuid) != "" {
		payload["uuid"] = strings.TrimSpace(uuid)
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

// PostThreadImage uploads a PNG and posts it once under the loop's existing Feishu
// thread. Feishu's uuid is the visible-message idempotency authority; a crash may
// repeat the invisible upload, but cannot duplicate the visible image message.
func (g *Gateway) PostThreadImage(ctx context.Context, loopID, pngPath, dedupeUUID string) (string, error) {
	loopID = strings.TrimSpace(loopID)
	if loopID == "" || strings.TrimSpace(pngPath) == "" || !strings.EqualFold(strings.TrimSpace(g.config.Webhook.Mode), "app") {
		return "", nil
	}
	cfg := g.config.Webhook
	appID := strings.TrimSpace(os.Getenv(strings.TrimSpace(cfg.AppIDEnv)))
	appSecret := strings.TrimSpace(os.Getenv(strings.TrimSpace(cfg.AppSecretEnv)))
	if appID == "" || appSecret == "" {
		return "", nil
	}
	root := g.threadRootForLoop(ctx, loopID)
	if root == "" || strings.TrimSpace(cfg.ChatID) == "" {
		return "", nil
	}
	token, err := g.feishuTenantToken(ctx, appID, appSecret)
	if err != nil {
		return "", err
	}
	imageKey, err := g.uploadFeishuImage(ctx, token, pngPath)
	if err != nil {
		return "", err
	}
	content, err := json.Marshal(map[string]string{"image_key": imageKey})
	if err != nil {
		return "", err
	}
	return g.postFeishuAppMessageWithUUID(ctx, token, cfg.ChatID, root, "image", string(content), dedupeUUID)
}

func (g *Gateway) uploadFeishuImage(ctx context.Context, token, pngPath string) (string, error) {
	file, err := os.Open(pngPath)
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 10<<20 {
		return "", fmt.Errorf("feishu image must be a regular file between 1 byte and 10 MiB")
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("image_type", "message"); err != nil {
		return "", err
	}
	part, err := writer.CreateFormFile("image", filepath.Base(pngPath))
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(part, file); err != nil {
		return "", err
	}
	if err := writer.Close(); err != nil {
		return "", err
	}
	status, response, err := g.feishuAppHTTP(ctx, http.MethodPost, feishuAPIBase+"/open-apis/im/v1/images", map[string]string{
		"Authorization": "Bearer " + token,
		"Content-Type":  writer.FormDataContentType(),
	}, body.Bytes())
	if err != nil {
		return "", err
	}
	if status < 200 || status >= 300 {
		return "", fmt.Errorf("feishu image upload responded with status %d", status)
	}
	var parsed struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			ImageKey string `json:"image_key"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response, &parsed); err != nil || parsed.Code != 0 || strings.TrimSpace(parsed.Data.ImageKey) == "" {
		return "", fmt.Errorf("feishu image upload failed: code=%d msg=%s", parsed.Code, parsed.Msg)
	}
	return strings.TrimSpace(parsed.Data.ImageKey), nil
}

// ensureFeishuThreadRoot returns the Feishu root message id a loop's notifications
// thread under, creating it (a plain-text "开始处理" header) on first use. Returns
// "" when loopID is empty or the root can't be created — callers then post
// top-level, so threading degrades gracefully rather than dropping messages.
// lockRoot returns an unlock func for a per-loop mutex, so ensureFeishuThreadRoot
// runs its check-then-create atomically for a given loop (see rootLocks).
func (g *Gateway) lockRoot(loopID string) func() {
	g.rootMu.Lock()
	if g.rootLocks == nil {
		g.rootLocks = make(map[string]*sync.Mutex)
	}
	lk := g.rootLocks[loopID]
	if lk == nil {
		lk = &sync.Mutex{}
		g.rootLocks[loopID] = lk
	}
	g.rootMu.Unlock()
	lk.Lock()
	return lk.Unlock
}

func (g *Gateway) ensureFeishuThreadRoot(ctx context.Context, token, chatID, loopID string) string {
	loopID = strings.TrimSpace(loopID)
	if loopID == "" || g.repositories == nil || g.repositories.FeishuThreads == nil {
		return ""
	}
	// Task identity: the planner/worker/... loops spawned for one work item share
	// ONE anchor card, keyed by their originating issue (issue:repo:N). Loops with
	// no source issue (PR-triggered, project-level, issue-less bug) fall back to
	// per-loop keying, exactly as before.
	taskKey := g.loopTaskKey(ctx, loopID)
	// Claim before post: serialise anchor creation for this TASK (or loop when
	// task-less) so a concurrent caller can't POST a second card between our lookup
	// and Upsert.
	lockKey := taskKey
	if lockKey == "" {
		lockKey = loopID
	}
	defer g.lockRoot(lockKey)()
	if root := g.resolveThreadRoot(ctx, loopID, taskKey); root != "" {
		// A new loop joining an existing task's card: re-point reply-routing (and
		// re-key a pre-upgrade per-loop card) at the currently-active loop.
		if taskKey != "" {
			_ = g.repositories.FeishuThreads.Upsert(ctx, root, loopID, taskKey, chatID, eventlog.FormatJavaScriptISOString(g.now()))
		}
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
	// later outbound updates back to this loop, and so it survives a daemon restart.
	_ = g.repositories.FeishuThreads.Upsert(ctx, msgID, loopID, taskKey, chatID, eventlog.FormatJavaScriptISOString(g.now()))
	return msgID
}

// loopTaskKey derives a loop's task identity (issue:repo:N) from its metadata, so
// sibling loops (planner spec, worker impl, fixer, ...) for one work item resolve
// to the same anchor card. Returns "" when the loop has no source issue.
func (g *Gateway) loopTaskKey(ctx context.Context, loopID string) string {
	if g.repositories == nil || g.repositories.Loops == nil {
		return ""
	}
	loop, err := g.repositories.Loops.GetByID(ctx, strings.TrimSpace(loopID))
	if err != nil || loop == nil {
		return ""
	}
	return loopTaskKeyFromRecord(loop)
}

// loopTaskKeyFromRecord builds issue:repo:N from a loop, or "" when there's no
// originating issue (or no repo) to anchor the task on.
func loopTaskKeyFromRecord(loop *storage.LoopRecord) string {
	if loop == nil || loop.Repo == nil {
		return ""
	}
	repo := strings.TrimSpace(*loop.Repo)
	if repo == "" {
		return ""
	}
	n := loopIssueNumber(loop.MetadataJSON)
	if n <= 0 {
		return ""
	}
	return fmt.Sprintf("issue:%s:%d", repo, n)
}

// loopIssueNumber reads the originating issue number from either metadata shape:
// planner loops store issueNumber at the top level; worker loops nest it under
// $.worker.issueNumber.
func loopIssueNumber(metadataJSON *string) int64 {
	if metadataJSON == nil || strings.TrimSpace(*metadataJSON) == "" {
		return 0
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(*metadataJSON), &meta); err != nil {
		return 0
	}
	if n := jsonNumberToInt64(meta["issueNumber"]); n > 0 {
		return n
	}
	if w, ok := meta["worker"].(map[string]any); ok {
		if n := jsonNumberToInt64(w["issueNumber"]); n > 0 {
			return n
		}
	}
	return 0
}

// jsonNumberToInt64 coerces a value decoded from JSON (float64, json.Number, or a
// numeric string) to int64; 0 when it isn't a positive-representable number.
func jsonNumberToInt64(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	case json.Number:
		i, _ := n.Int64()
		return i
	case string:
		i, _ := strconv.ParseInt(strings.TrimSpace(n), 10, 64)
		return i
	}
	return 0
}

// resolveThreadRoot finds the anchor card root for a loop: by task identity when
// the loop has a source issue (so sibling loops share one card), else by loop id.
// The loop-id fallback also lets a pre-upgrade per-loop card be adopted (and then
// re-keyed) by the first task-aware loop.
func (g *Gateway) resolveThreadRoot(ctx context.Context, loopID, taskKey string) string {
	if g.repositories == nil || g.repositories.FeishuThreads == nil {
		return ""
	}
	if taskKey != "" {
		if root, err := g.repositories.FeishuThreads.RootByTask(ctx, taskKey); err == nil && root != "" {
			return root
		}
	}
	if root, err := g.repositories.FeishuThreads.RootByLoop(ctx, strings.TrimSpace(loopID)); err == nil {
		return root
	}
	return ""
}

// threadRootForLoop resolves a loop's anchor card root (task-keyed when possible),
// for the header/live-feed refreshers that already hold only a loop id.
func (g *Gateway) threadRootForLoop(ctx context.Context, loopID string) string {
	return g.resolveThreadRoot(ctx, loopID, g.loopTaskKey(ctx, loopID))
}

// feishuThreadHeaderText builds a loop thread's plain-text root header, e.g.
// "🔧 开始处理 #360 · owner/repo\n<title>". Best-effort; never exposes an
// internal loop id when the record cannot be resolved.
func (g *Gateway) feishuThreadHeaderText(ctx context.Context, loopID string) string {
	var loop *storage.LoopRecord
	if g.repositories != nil && g.repositories.Loops != nil {
		if got, err := g.repositories.Loops.GetByID(ctx, loopID); err == nil {
			loop = got
		}
	}
	if loop == nil {
		return "🔧 Looper 任务更新"
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
	issueURL := g.loopIssueURL(ctx, loop)
	sourceLabel, sourceURL := "", ""
	if issueURL != "" {
		sourceLabel = "Issue"
		if n := loopIssueNumber(loop.MetadataJSON); n > 0 {
			sourceLabel = fmt.Sprintf("Issue #%d", n)
		} else if n := urlTrailingNumber(issueURL); n != "" {
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
	// Fallback: a shepherded (or restart-resumed) worker records its PR in
	// loop.PRNumber + target pr:repo:N even when $.worker.prUrl wasn't persisted to the
	// metadata. Derive the link so a human sees the PR to review straight from the card
	// while it sits at 👀 评审中 — otherwise the card names the task but hides the
	// deliverable under review.
	if prURL == "" && loop.PRNumber != nil && *loop.PRNumber > 0 && repo != "" {
		prURL = fmt.Sprintf("https://github.com/%s/pull/%d", repo, *loop.PRNumber)
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
	if label := loopTriggerLabelHint(loop.Type); label != "" {
		parts = append(parts, "🏷 "+label)
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
	elements = append(elements, feishuLooperOwnerNote(g.ownerOpenID(loop.ProjectID)))
	// One-command local takeover of the agent session (runs on the daemon host).
	// Offered only while the loop is live; dropped once it reaches a terminal state.
	if !feishuLoopStatusTerminal(loop.Status) {
		elements = append(elements, map[string]any{"tag": "note", "elements": []any{map[string]any{"tag": "lark_md", "content": "💻 本地接管:`looper resume " + strconv.FormatInt(loop.Seq, 10) + "`"}}})
	}
	// hasPR drives the "已交付 · 待合并" vs "已完成" wording. Derive it from a
	// reliable signal: a completed worker loop's target flips to its PR
	// (pr:repo:N), and/or the metadata carries a prUrl. (loopWorkerString only
	// reads $.worker.*, but prUrl is stored at the top level, so it alone is not
	// reliable here.)
	hasPR := prURL != "" || strings.HasPrefix(strings.TrimSpace(target), "pr:")
	prNum := prNumberFromTargetOrURL(target, prURL)
	awaitingProductSpec := loopAwaitingProductSpec(loop.MetadataJSON)
	template, label := feishuLoopFlowchartStyle(loop.Type, loop.Status, hasPR, awaitingProductSpec, loopNodeHPhase(loop.MetadataJSON), loopShepherdPhase(loop.MetadataJSON), prNum)
	// §A: once the WORKER delivers its impl PR, mirror that PR's real review-cycle
	// state (👀 待 review / 🔄 CI 检查中 / ✋ 待修改 / ❌ CI 失败 / ✅ 待合并 / 🎉 已合并)
	// from its latest snapshot so the header tracks the PR through to merge (node Z).
	// Planner tech specs live on Plane pages (no spec PR) — their 方案评审中 comes from
	// the role label above, not a PR snapshot. Merged/failed terminals are untouched.
	// Shepherding loops are EXCLUDED: the shepherd's own phase (computed from the live
	// PR, and the only one that knows the 🧪 待验收 QA gate) is authoritative there —
	// the coarser snapshot must not override 待验收 back to 待合并.
	if hasPR && feishuLoopAwaitingMerge(loop.Status) && !strings.EqualFold(strings.TrimSpace(loop.Type), "planner") && !strings.EqualFold(strings.TrimSpace(loop.Status), "shepherding") {
		if t, l, ok := g.prCardStyleFromSnapshot(ctx, repo, target, prURL); ok {
			template, label = t, l
		}
	}
	// A retired looper:auto anchor (parked or routed) wins over the generic
	// role/status label, so the shared task card retires to its real outcome
	// instead of freezing on "🧭 分类中". Set only for the coordinator loop id.
	if style, ok := g.anchorOutcomeOverride(loopID); ok {
		template, label = style.template, style.label
	}
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

// loopIssueURL resolves the originating issue URL for an anchor card. Current
// loops persist it in metadata, but older or checkpoint-resumed planners may only
// have it in their latest run checkpoint. Falling back keeps the source reference
// clickable while those loops converge onto the current metadata shape.
func (g *Gateway) loopIssueURL(ctx context.Context, loop *storage.LoopRecord) string {
	if loop == nil {
		return ""
	}
	if issueURL := loopWorkerString(loop.MetadataJSON, "issueUrl"); issueURL != "" {
		return issueURL
	}
	if g.repositories == nil || g.repositories.Runs == nil {
		return ""
	}
	run, err := g.repositories.Runs.GetLatestByLoopID(ctx, loop.ID)
	if err != nil || run == nil || run.CheckpointJSON == nil {
		return ""
	}
	var checkpoint struct {
		Issue *struct {
			URL string `json:"url"`
		} `json:"issue"`
	}
	if json.Unmarshal([]byte(*run.CheckpointJSON), &checkpoint) != nil || checkpoint.Issue == nil {
		return ""
	}
	return strings.TrimSpace(checkpoint.Issue.URL)
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

// loopTriggerLabelHint names the label that started a loop, by role — so the card
// shows WHY this task is running: planner=looper:plan, worker=looper:worker-ready,
// coordinator=looper:auto. Reviewer/fixer are PR-driven (no source label) → "".
func loopTriggerLabelHint(loopType string) string {
	switch strings.ToLower(strings.TrimSpace(loopType)) {
	case "planner":
		return "looper:plan"
	case "worker":
		return "looper:worker-ready"
	case "coordinator":
		return "looper:auto"
	default:
		return ""
	}
}

// loopAwaitingProductSpec reports whether a held loop is waiting specifically for a
// product spec (flowchart node E) — the planner sets this metadata flag when its
// product-spec gate holds, so the card can say "⏸ 等待产品方案" instead of the generic
// "⏸ 等你定夺". Any other hold (ambiguity ask, bug HITL) leaves it false.
func loopAwaitingProductSpec(metadataJSON *string) bool {
	if metadataJSON == nil || strings.TrimSpace(*metadataJSON) == "" {
		return false
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(*metadataJSON), &meta); err != nil {
		return false
	}
	b, _ := meta["awaitingProductSpec"].(bool)
	return b
}

// loopNodeHPhase reads the planner's node H spec-pipeline phase from metadata
// (authoring / grilling / reviewing / awaiting_human_review), so the card header can
// name the exact sub-phase. Empty when unset (pre-node-H or non-Plane).
func loopNodeHPhase(metadataJSON *string) string {
	if metadataJSON == nil || strings.TrimSpace(*metadataJSON) == "" {
		return ""
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(*metadataJSON), &meta); err != nil {
		return ""
	}
	s, _ := meta["nodeHPhase"].(string)
	return strings.TrimSpace(s)
}

// loopShepherdPhase reads $.shepherd.phase (reviewing|fixing|awaiting_merge) for a
// worker loop driving its impl PR to merge, so the card header animates through
// the PR-shepherding lane.
func loopShepherdPhase(metadataJSON *string) string {
	if metadataJSON == nil || strings.TrimSpace(*metadataJSON) == "" {
		return ""
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(*metadataJSON), &meta); err != nil {
		return ""
	}
	shepherd, ok := meta["shepherd"].(map[string]any)
	if !ok {
		return ""
	}
	s, _ := shepherd["phase"].(string)
	return strings.TrimSpace(s)
}

// feishuLoopFlowchartStyle maps a loop's ROLE + status to the anchor card's colour +
// title, so one glance at the header says WHERE in the flow the task sits — not a
// generic "处理中". The roles are the flowchart lanes: coordinator=分诊,
// planner=写技术方案(spec on a Plane page, reviewed there — no spec PR),
// reviewer=评审, worker=实现. Terminals win first; a held loop shows the human-wait
// (product-spec vs generic); otherwise the running label is the role's lane. A
// planner that COMPLETED has written its tech spec to Plane and now awaits node H
// review (👀 方案评审中), never a merge.
func feishuLoopFlowchartStyle(loopType, status string, hasPR, awaitingProductSpec bool, nodeHPhase, shepherdPhase string, prNumber int64) (template, label string) {
	role := strings.ToLower(strings.TrimSpace(loopType))
	st := strings.ToLower(strings.TrimSpace(status))
	// Terminals win first.
	switch st {
	case "merged":
		return "green", "🎉 已合并"
	case "failed", "abandoned", "error":
		return "red", "⚠️ Looper 需要处理"
	}
	// Worker shepherding its own impl PR to merge (looper:auto): the sub-phase
	// animates the header through the PR-driving lane. The bot never merges — a
	// human does — so the ready state reads "待合并" (waiting for a human), and
	// 🎉已合并 comes only from the terminal "merged" outcome above.
	// A shepherd loop shows its phase both while PARKED (status shepherding) AND while
	// actively running a fix pass (status running) — otherwise the card drops back to
	// the generic "🔨 实现中" mid-fix. shepherdPhase is only set once shepherding began,
	// so a plain worker run (no shepherd marker) still falls through to 实现中 below.
	if sp := strings.ToLower(strings.TrimSpace(shepherdPhase)); st == "shepherding" || (st == "running" && sp != "") {
		switch sp {
		case "fixing":
			return "blue", "🔧 修复中"
		case "awaiting_validation":
			return "orange", "🧪 待验收(QA)"
		case "awaiting_merge":
			return "turquoise", "✅ 待合并(等人合并)"
		default:
			return "blue", "👀 评审中"
		}
	}
	// node E hold: product-spec wait (before AUTHOR).
	if awaitingProductSpec {
		return "orange", "⏸ 等待产品方案"
	}
	// node H sub-phases (planner spec pipeline): author → grill → review → 需要人类审核.
	// The phase marker names exactly where in the spec pipeline the task sits, so the
	// header stops resting on a generic "编写技术方案中" through grill + review.
	if role == "planner" {
		switch strings.ToLower(strings.TrimSpace(nodeHPhase)) {
		case "awaiting_product_spec":
			return "orange", "📄 等待产品 Spec"
		case "awaiting_product":
			return "orange", "🙋 等待产品拍板"
		case "awaiting_downstream":
			return "orange", "🎨 等待设计/研发拍板"
		case "grilling":
			return "blue", "🔬 方案拷问中"
		case "reviewing":
			return "blue", "👀 spec 评审中"
		case "awaiting_product_answer":
			// node H product-decision hold: the spec surfaced a product question and
			// @-mentioned the product owner; it waits for their Plane answer, not the
			// owner's approval. Distinct from 需要人类审核 spec so the card is honest.
			return "orange", "🙋 等待产品拍板"
		case "awaiting_human_review":
			return "orange", "🙋 需要人类审核 spec"
		}
	}
	// Generic HITL ask (e.g. grill uncertainty asking a human).
	if st == "awaiting_human" {
		return "orange", "⏸ Looper 等你定夺"
	}
	if st == "completed" || st == "done" {
		if role == "planner" {
			// AUTHOR+GRILL+REVIEW converged, spec on Plane → awaiting a human's approve.
			return "orange", "🙋 需要人类审核 spec"
		}
		// worker / fixer / generic: the impl PR is OPEN — it is NOT delivered. "已交付"
		// is reserved for a MERGED PR (🎉 已合并, driven by the snapshot terminal in §A);
		// an unmerged PR must never claim delivery. Default a just-opened PR to 待 review;
		// its real review-cycle state (approved / 待 QA / merged …) is layered on top from
		// the PR snapshot by the §A override at the call site.
		if hasPR {
			return "blue", "👀 待 review" + prNumberSuffix(prNumber)
		}
		return "green", "✅ Looper 已完成"
	}
	// running / queued — the label is the role's flowchart lane.
	switch role {
	case "coordinator":
		return "blue", "🧭 分诊中"
	case "planner":
		return "blue", "📝 编写技术方案中"
	case "worker":
		return "blue", "🔨 实现中"
	case "reviewer":
		return "blue", "👀 评审中"
	case "fixer":
		return "blue", "🔧 修复中"
	default:
		return "blue", "🔧 Looper 处理中"
	}
}

// feishuLoopAwaitingMerge reports whether a loop has delivered a PR and is now
// waiting on the review/merge cycle — the window where the anchor card should
// mirror the PR's real state (§A) instead of the generic "已交付" wording.
func feishuLoopAwaitingMerge(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "completed", "done", "shepherding":
		return true
	default:
		return false
	}
}

// prCardState is the review-cycle state a delivered task's PR is in, derived from
// its latest snapshot so the anchor card mirrors the PR's real status (plan §A).
type prCardState string

const (
	prCardStateChecksFailed       prCardState = "checks_failed"
	prCardStateChangesRequested   prCardState = "changes_requested"
	prCardStateChecksRunning      prCardState = "checks_running"
	prCardStateAwaitingValidation prCardState = "awaiting_validation"
	prCardStateValidated          prCardState = "validated"
	prCardStateApproved           prCardState = "approved"
	prCardStateReviewPending      prCardState = "review_pending"
)

type checkRollup int

const (
	checkNone checkRollup = iota
	checkRunning
	checkFailed
	checkPassed
)

// classifyChecks rolls a snapshot's ChecksSummary (a comma-joined list of CI
// conclusions/states) into a single verdict: any failure wins, then anything
// still running, else all-passed.
func classifyChecks(summary string) checkRollup {
	summary = strings.ToLower(strings.TrimSpace(summary))
	if summary == "" {
		return checkNone
	}
	anyFailed, anyRunning, any := false, false, false
	for _, state := range strings.Split(summary, ",") {
		state = strings.TrimSpace(state)
		if state == "" {
			continue
		}
		any = true
		switch {
		case containsAnySubstring(state, "fail", "error", "cancel", "timed_out", "timedout", "action_required", "stale", "startup_failure"):
			anyFailed = true
		case containsAnySubstring(state, "pending", "progress", "queued", "waiting", "expected", "requested", "running"):
			anyRunning = true
		}
	}
	switch {
	case !any:
		return checkNone
	case anyFailed:
		return checkFailed
	case anyRunning:
		return checkRunning
	default:
		return checkPassed
	}
}

func containsAnySubstring(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// prCardStateFromSnapshot derives the review-cycle state from a snapshot's
// first-class fields. Precedence mirrors what blocks a merge: failing CI first,
// then requested changes, then CI still running, then approved. Once APPROVED, the
// QA validation-gate labels split the "ready" state: a PR carrying `needs-validation`
// (and not yet `validated`) is awaiting QA (🧪 待 QA 验收); a `validated` PR (or one
// whose needs-validation was cleared) is 验收通过 · 待合并; an APPROVED PR that never
// needed validation is plain 待合并. Returns ("", false) when there's nothing to show.
func prCardStateFromSnapshot(reviewState, checksSummary string, labels []string, unresolved int64) (prCardState, bool) {
	review := strings.ToUpper(strings.TrimSpace(reviewState))
	checks := classifyChecks(checksSummary)
	switch {
	case checks == checkFailed:
		return prCardStateChecksFailed, true
	case review == "CHANGES_REQUESTED":
		return prCardStateChangesRequested, true
	case checks == checkRunning:
		return prCardStateChecksRunning, true
	case review == "APPROVED":
		// validated wins over needs-validation (QA signed off even if the maintainer's
		// needs-validation label lingers); needs-validation alone means still awaiting QA.
		switch {
		case labelsContain(labels, "validated"):
			return prCardStateValidated, true
		case labelsContain(labels, "needs-validation"):
			return prCardStateAwaitingValidation, true
		default:
			return prCardStateApproved, true
		}
	default:
		return prCardStateReviewPending, true
	}
}

// labelsContain reports whether labels holds target (case-insensitive, trimmed).
func labelsContain(labels []string, target string) bool {
	target = strings.ToLower(strings.TrimSpace(target))
	for _, l := range labels {
		if strings.ToLower(strings.TrimSpace(l)) == target {
			return true
		}
	}
	return false
}

// prCardStateStyle maps a PR review-cycle state to the card header's colour +
// label (plan §A's title table). "已合并" is intentionally not produced here — it
// is a terminal driven by merge detection, not a review snapshot.
func prCardStateStyle(state prCardState, prNumber string) (template, label string) {
	suffix := ""
	if strings.TrimSpace(prNumber) != "" {
		suffix = " · PR #" + strings.TrimSpace(prNumber)
	}
	switch state {
	case prCardStateChecksFailed:
		return "red", "❌ CI 失败" + suffix
	case prCardStateChangesRequested:
		return "orange", "✋ 待修改" + suffix
	case prCardStateChecksRunning:
		return "blue", "🔄 CI 检查中" + suffix
	case prCardStateAwaitingValidation:
		return "orange", "🧪 待 QA 验收" + suffix
	case prCardStateValidated:
		return "turquoise", "✅ 验收通过 · 待合并" + suffix
	case prCardStateApproved:
		return "turquoise", "✅ 待合并" + suffix
	default:
		return "blue", "👀 待 review" + suffix
	}
}

// prNumberSuffix renders the " · PR #N" tail a card label carries when a PR number
// is known, or "" when it isn't.
func prNumberSuffix(prNumber int64) string {
	if prNumber > 0 {
		return " · PR #" + strconv.FormatInt(prNumber, 10)
	}
	return ""
}

// prNumberFromTargetOrURL extracts a PR number from a loop target (pr:owner/repo:N)
// or, failing that, the trailing number of a PR URL (.../pull/N).
func prNumberFromTargetOrURL(target, prURL string) int64 {
	if t := strings.TrimSpace(target); strings.HasPrefix(t, "pr:") {
		if i := strings.LastIndex(t, ":"); i >= 0 {
			if n := jsonNumberToInt64(t[i+1:]); n > 0 {
				return n
			}
		}
	}
	if n := urlTrailingNumber(prURL); n != "" {
		return jsonNumberToInt64(n)
	}
	return 0
}

// prCardStyleFromSnapshot resolves the PR-state card style for a delivered loop by
// reading the latest snapshot of the PR it opened. Best-effort: ("", "", false)
// when there's no PR number, no snapshot, or the repos aren't wired.
func (g *Gateway) prCardStyleFromSnapshot(ctx context.Context, repo, target, prURL string) (string, string, bool) {
	if g.repositories == nil || g.repositories.PullRequestSnapshots == nil {
		return "", "", false
	}
	repo = strings.TrimSpace(repo)
	prNum := prNumberFromTargetOrURL(target, prURL)
	if repo == "" || prNum <= 0 {
		return "", "", false
	}
	snap, err := g.repositories.PullRequestSnapshots.GetLatest(ctx, repo, prNum)
	if err != nil || snap == nil {
		return "", "", false
	}
	review, checks, unresolved := "", "", int64(0)
	if snap.ReviewState != nil {
		review = *snap.ReviewState
	}
	if snap.ChecksSummary != nil {
		checks = *snap.ChecksSummary
	}
	if snap.UnresolvedThreadCount != nil {
		unresolved = *snap.UnresolvedThreadCount
	}
	labels := prLabelsFromSnapshot(snap.PayloadJSON)
	// Node Z terminal: if the PR has merged (or closed) — read from the snapshot's
	// captured PR detail — the card reaches its final state instead of resting at
	// "待合并". 🎉 已合并 is the only green terminal.
	prNumStr := strconv.FormatInt(prNum, 10)
	switch prMergeStateFromSnapshot(snap.PayloadJSON) {
	case "MERGED":
		return "green", "🎉 已合并 · PR #" + prNumStr, true
	case "CLOSED":
		return "red", "🚫 已关闭 · PR #" + prNumStr, true
	}
	state, ok := prCardStateFromSnapshot(review, checks, labels, unresolved)
	if !ok {
		return "", "", false
	}
	t, l := prCardStateStyle(state, prNumStr)
	return t, l, true
}

// prLabelsFromSnapshot extracts the PR's labels from a snapshot's captured detail
// payload ({"detail": {"Labels": [...]}}), or nil when unavailable. The QA gate
// (needs-validation / validated) lives on these labels — see prCardStateFromSnapshot.
func prLabelsFromSnapshot(payloadJSON *string) []string {
	if payloadJSON == nil || strings.TrimSpace(*payloadJSON) == "" {
		return nil
	}
	var payload struct {
		Detail struct {
			Labels []string `json:"Labels"`
		} `json:"detail"`
	}
	if err := json.Unmarshal([]byte(*payloadJSON), &payload); err != nil {
		return nil
	}
	return payload.Detail.Labels
}

// prMergeStateFromSnapshot extracts the PR's lifecycle state ("MERGED" / "CLOSED" /
// "OPEN") from a snapshot's captured detail payload, or "" when unavailable. A merged
// PR reports state OPEN with mergedAt set on some providers, so both are checked.
func prMergeStateFromSnapshot(payloadJSON *string) string {
	if payloadJSON == nil || strings.TrimSpace(*payloadJSON) == "" {
		return ""
	}
	var payload struct {
		Detail struct {
			State    string `json:"State"`
			MergedAt string `json:"MergedAt"`
			ClosedAt string `json:"ClosedAt"`
		} `json:"detail"`
	}
	if err := json.Unmarshal([]byte(*payloadJSON), &payload); err != nil {
		return ""
	}
	state := strings.ToUpper(strings.TrimSpace(payload.Detail.State))
	if state == "MERGED" || strings.TrimSpace(payload.Detail.MergedAt) != "" {
		return "MERGED"
	}
	if state == "CLOSED" || (state != "OPEN" && strings.TrimSpace(payload.Detail.ClosedAt) != "") {
		return "CLOSED"
	}
	return state
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
		g.state.liveMu.Lock()
		if g.state.liveTails == nil {
			g.state.liveTails = map[string]liveTailEntry{}
		}
		g.state.touchLoopLocked(loopID)
		g.state.liveTails[loopID] = liveTailEntry{lines: tail, elapsedSec: elapsedSec}
		g.state.liveMu.Unlock()
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

// FinalizeIntakeAnchor retires a looper:auto classification's shared task anchor to
// a header that reflects the routing OUTCOME, then re-renders it. Without this the
// coordinator anchor freezes on "🧭 分类中": completeIntakeAnchor never refreshes it
// and, for a parked item (needs-human / out-of-scope / awaiting-product), no
// downstream loop ever runs to take the anchor over. For a routed item the label is
// a bridge the planner/worker overwrites with its own phase once it renders. outcome
// is one of routed_plan | routed_worker | hold_product | needs_human | out_of_scope;
// an unknown outcome just refreshes to the loop's real status. App-mode + best-effort.
func (g *Gateway) FinalizeIntakeAnchor(ctx context.Context, loopID, outcome string) {
	loopID = strings.TrimSpace(loopID)
	if loopID == "" {
		return
	}
	if style, ok := intakeOutcomeStyle(outcome); ok {
		g.intakeMu.Lock()
		if g.intakeOutcomes == nil {
			g.intakeOutcomes = map[string]anchorOutcomeStyle{}
		}
		g.intakeOutcomes[loopID] = style
		g.intakeMu.Unlock()
	}
	g.RefreshThreadHeader(ctx, loopID, nil, 0)
}

// intakeOutcomeStyle maps a routing outcome to the anchor header colour + label.
func intakeOutcomeStyle(outcome string) (anchorOutcomeStyle, bool) {
	switch strings.ToLower(strings.TrimSpace(outcome)) {
	case "routed_plan":
		return anchorOutcomeStyle{"blue", "➡️ 已派发 · 写技术方案"}, true
	case "routed_worker":
		return anchorOutcomeStyle{"blue", "➡️ 已派发 · 待实现"}, true
	case "hold_product":
		return anchorOutcomeStyle{"orange", "⏸ 等待产品方案"}, true
	case "needs_human":
		return anchorOutcomeStyle{"orange", "🙋 转人工确认"}, true
	case "out_of_scope":
		return anchorOutcomeStyle{"grey", "🚫 超出范围(已标记)"}, true
	default:
		return anchorOutcomeStyle{}, false
	}
}

// anchorOutcomeOverride returns the retired-anchor header override for a loop, if
// FinalizeIntakeAnchor recorded one.
func (g *Gateway) anchorOutcomeOverride(loopID string) (anchorOutcomeStyle, bool) {
	g.intakeMu.Lock()
	defer g.intakeMu.Unlock()
	if g.intakeOutcomes == nil {
		return anchorOutcomeStyle{}, false
	}
	style, ok := g.intakeOutcomes[strings.TrimSpace(loopID)]
	return style, ok
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
	root := g.threadRootForLoop(ctx, loopID)
	if strings.TrimSpace(root) == "" {
		return // anchor not posted yet — nothing to thread under
	}
	ownerOpenID := ""
	if g.repositories.Loops != nil {
		if loop, err := g.repositories.Loops.GetByID(ctx, loopID); err == nil && loop != nil {
			ownerOpenID = g.ownerOpenID(loop.ProjectID)
		}
	}
	cardJSON, ok := feishuLiveFeedCardWithOwner(tail, elapsedSec, ownerOpenID)
	if !ok {
		return
	}
	g.state.liveMu.Lock()
	msgID := ""
	if g.state.liveFeeds != nil {
		msgID = g.state.liveFeeds[loopID]
	}
	if msgID != "" {
		g.state.touchLoopLocked(loopID)
	}
	g.state.liveMu.Unlock()
	// Fall back to the persisted id so a daemon restart patches the same feed card
	// in place instead of orphaning it and posting a fresh one.
	if msgID == "" && g.repositories.FeishuLiveFeeds != nil {
		if persisted, err := g.repositories.FeishuLiveFeeds.MessageByLoop(ctx, loopID); err == nil && persisted != "" {
			msgID = persisted
			g.state.liveMu.Lock()
			if g.state.liveFeeds == nil {
				g.state.liveFeeds = map[string]string{}
			}
			g.state.touchLoopLocked(loopID)
			g.state.liveFeeds[loopID] = persisted
			g.state.liveMu.Unlock()
		}
	}
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
	g.state.liveMu.Lock()
	if g.state.liveFeeds == nil {
		g.state.liveFeeds = map[string]string{}
	}
	g.state.touchLoopLocked(loopID)
	g.state.liveFeeds[loopID] = newID
	g.state.liveMu.Unlock()
	if g.repositories.FeishuLiveFeeds != nil {
		_ = g.repositories.FeishuLiveFeeds.Set(ctx, loopID, newID, eventlog.FormatJavaScriptISOString(g.now()))
	}
}

// liveTailFor returns the retained live activity snapshot for a loop.
func (g *Gateway) liveTailFor(loopID string) ([]string, int64) {
	g.state.liveMu.Lock()
	defer g.state.liveMu.Unlock()
	if g.state.liveTails == nil {
		return nil, 0
	}
	e := g.state.liveTails[strings.TrimSpace(loopID)]
	if len(e.lines) > 0 || e.elapsedSec > 0 {
		g.state.touchLoopLocked(loopID)
	}
	return e.lines, e.elapsedSec
}

// updateFeishuThreadHeader re-renders a loop's thread-anchor card to reflect its
// current status (colour + label). Best-effort: a loop with no root card yet, or
// a failed PATCH, is a no-op.
func (g *Gateway) updateFeishuThreadHeader(ctx context.Context, token, loopID string) {
	if g.repositories == nil || g.repositories.FeishuThreads == nil {
		return
	}
	root := g.threadRootForLoop(ctx, loopID)
	if strings.TrimSpace(root) == "" {
		return
	}
	cardJSON, ok := g.feishuThreadHeaderCard(ctx, loopID)
	if !ok {
		return
	}
	_ = g.patchFeishuAppCard(ctx, token, root, cardJSON)
}

// PostThreadNote posts a plain-text reply into a loop's Feishu thread, optionally
// @-mentioning open_ids — node H's thread touchpoints: the spec-draft FYI after AUTHOR
// and the grill transcript after GRILL. No-op unless the app transport is configured
// or the loop has no thread root yet.
func (g *Gateway) PostThreadNote(ctx context.Context, loopID, text string, mentionOpenIDs []string) error {
	return g.postThreadNote(ctx, loopID, text, mentionOpenIDs, "")
}

// PostThreadNoteWithUUID is the idempotent form used by revisioned decision
// notifications. Repeating the same UUID returns the same Feishu message.
func (g *Gateway) PostThreadNoteWithUUID(ctx context.Context, loopID, text string, mentionOpenIDs []string, uuid string) error {
	return g.postThreadNote(ctx, loopID, text, mentionOpenIDs, uuid)
}

func (g *Gateway) postThreadNote(ctx context.Context, loopID, text string, mentionOpenIDs []string, uuid string) error {
	loopID = strings.TrimSpace(loopID)
	if loopID == "" || strings.TrimSpace(text) == "" || !strings.EqualFold(strings.TrimSpace(g.config.Webhook.Mode), "app") {
		return nil
	}
	cfg := g.config.Webhook
	appID := strings.TrimSpace(os.Getenv(strings.TrimSpace(cfg.AppIDEnv)))
	appSecret := strings.TrimSpace(os.Getenv(strings.TrimSpace(cfg.AppSecretEnv)))
	if appID == "" || appSecret == "" {
		return nil
	}
	token, err := g.feishuTenantToken(ctx, appID, appSecret)
	if err != nil {
		return err
	}
	root := g.threadRootForLoop(ctx, loopID)
	chatID := strings.TrimSpace(cfg.ChatID)
	if strings.TrimSpace(root) == "" || chatID == "" {
		return nil
	}
	// Feishu text messages @-mention with <at user_id="ou_..."></at> (double-quoted,
	// distinct from the card lark_md form).
	mention := ""
	for _, id := range mentionOpenIDs {
		if id = strings.TrimSpace(id); id != "" {
			mention += `<at user_id="` + id + `"></at> `
		}
	}
	raw, err := json.Marshal(map[string]string{"text": mention + strings.TrimSpace(text)})
	if err != nil {
		return err
	}
	if _, err := g.postFeishuAppMessageWithUUID(ctx, token, chatID, root, "text", string(raw), uuid); err != nil {
		return err
	}
	// Refresh the anchor card so its phase title tracks the loop — a node H post lands
	// right after a phase change (e.g. →需要人类审核 spec), so the header shouldn't lag.
	g.updateFeishuThreadHeader(ctx, token, loopID)
	return nil
}

// PostThreadDecisionCard posts a node-H product-decision ask into a loop's Feishu
// thread as a header-LESS interactive card, so the product-language body keeps its
// structure — bold sub-headers, blank lines between sections, one option per line —
// instead of collapsing into a flat wall of text (a plain "text" message renders no
// markdown). The card has no title bar on purpose: it should read as a threaded note,
// not a banner. @-mentions the product owner via the card <at id=..> form. No-op unless
// the app transport is configured and the loop already has a thread root.
func (g *Gateway) PostThreadDecisionCard(ctx context.Context, loopID, body, actionURL string, mentionOpenIDs []string) error {
	return g.postThreadActionCard(ctx, loopID, body, actionURL, mentionOpenIDs, "decision")
}

// PostThreadApprovalCard posts the owner-facing node-H tech-spec approval card.
// It deliberately uses distinct copy and a distinct message id from product asks.
func (g *Gateway) PostThreadApprovalCard(ctx context.Context, loopID, body, actionURL string, mentionOpenIDs []string) error {
	return g.postThreadActionCard(ctx, loopID, body, actionURL, mentionOpenIDs, "approval")
}

// PostThreadProductSpecCard points product to the work-item comment where they can
// create or update the authoritative product spec before planning continues.
func (g *Gateway) PostThreadProductSpecCard(ctx context.Context, loopID, body, actionURL string, mentionOpenIDs []string) error {
	return g.postThreadActionCard(ctx, loopID, body, actionURL, mentionOpenIDs, "product_spec")
}

func (g *Gateway) postThreadActionCard(ctx context.Context, loopID, body, actionURL string, mentionOpenIDs []string, purpose string) error {
	loopID = strings.TrimSpace(loopID)
	actionURL = strings.TrimSpace(actionURL)
	if loopID == "" || strings.TrimSpace(body) == "" || actionURL == "" || !strings.EqualFold(strings.TrimSpace(g.config.Webhook.Mode), "app") {
		return nil
	}
	intent := feishuDecisionCardIntent{LoopID: loopID, Body: strings.TrimSpace(body), ActionURL: actionURL, MentionOpenIDs: firstFeishuOwner(mentionOpenIDs), Purpose: purpose}
	return g.enqueueFeishuCard(ctx, "Plane action needed", body, loopID, feishuCardIntent{Kind: "decision", Decision: &intent})
}

func (g *Gateway) deliverThreadDecisionCard(ctx context.Context, intent feishuDecisionCardIntent) error {
	loopID := strings.TrimSpace(intent.LoopID)
	body := strings.TrimSpace(intent.Body)
	actionURL := strings.TrimSpace(intent.ActionURL)
	cfg := g.config.Webhook
	appID := strings.TrimSpace(os.Getenv(strings.TrimSpace(cfg.AppIDEnv)))
	appSecret := strings.TrimSpace(os.Getenv(strings.TrimSpace(cfg.AppSecretEnv)))
	if appID == "" || appSecret == "" {
		return fmt.Errorf("feishu app credentials are not configured")
	}
	token, err := g.feishuTenantToken(ctx, appID, appSecret)
	if err != nil {
		return err
	}
	chatID := strings.TrimSpace(cfg.ChatID)
	if chatID == "" {
		return fmt.Errorf("feishu chat id is not configured")
	}
	// This also creates a real anchor for run-less coordinator loops. Previously the
	// decision delivery required an already-existing root and silently no-op'd.
	root := g.ensureFeishuThreadRoot(ctx, token, chatID, loopID)
	approval := strings.EqualFold(strings.TrimSpace(intent.Purpose), "approval")
	productSpec := strings.EqualFold(strings.TrimSpace(intent.Purpose), "product_spec")
	lead := "🙋 这个需求有个地方需要你来拍板 —— 请前往 Plane 的具体评论回答。飞书回复不会被读取。"
	buttonText := "前往 Plane 回答"
	messageIDKey := decisionCardMsgIDKey
	if approval {
		lead = "👀 技术方案已完成 GRILL + REVIEW，请前往 Plane 审核。飞书回复不会被读取。"
		buttonText = "前往 Plane 审核"
		messageIDKey = approvalCardMsgIDKey
	} else if productSpec {
		lead = "📝 请先补充产品 spec，再让 Looper 开始技术梳理。请前往 Plane work item 处理；飞书回复不会被读取。"
		buttonText = "前往 Plane 补 spec"
	}
	if mention := feishuMentionMarkup(firstFeishuOwner(intent.MentionOpenIDs)); mention != "" {
		lead = mention + " " + lead
	}
	card := map[string]any{
		"config": map[string]any{"wide_screen_mode": true},
		"elements": []any{
			larkDiv(lead),
			map[string]any{"tag": "hr"},
			larkDiv(body),
			map[string]any{"tag": "action", "actions": []any{map[string]any{
				"tag": "button", "type": "primary",
				"text": map[string]any{"tag": "plain_text", "content": buttonText},
				"url":  actionURL,
			}}},
		},
	}
	cardJSON, err := json.Marshal(card)
	if err != nil {
		return err
	}
	// A follow-up (线程永远可追问) revises the ask — UPDATE the existing 拍板 card in place
	// so the thread shows ONE evolving card, not a fresh duplicate on every question. Fall
	// back to a new post when there's no prior card or the patch fails (e.g. it aged out).
	if prior := g.loopMetaString(ctx, loopID, messageIDKey); prior != "" {
		if err := g.patchFeishuAppCard(ctx, token, prior, string(cardJSON)); err == nil {
			g.updateFeishuThreadHeader(ctx, token, loopID)
			return nil
		}
	}
	msgID, err := g.postFeishuAppMessage(ctx, token, chatID, root, "interactive", string(cardJSON))
	if err != nil {
		return err
	}
	if strings.TrimSpace(msgID) != "" {
		g.setLoopMetaString(ctx, loopID, messageIDKey, msgID)
	}
	g.updateFeishuThreadHeader(ctx, token, loopID)
	return nil
}

// decisionCardMsgIDKey stores the product-action card's message id in loop metadata
// so a follow-up revision patches that card in place instead of posting a duplicate.
const decisionCardMsgIDKey = "productAskCardMsgId"

const approvalCardMsgIDKey = "specApprovalCardMsgId"

// loopMetaString reads a top-level string field from a loop's metadata; "" when absent
// or unreadable.
func (g *Gateway) loopMetaString(ctx context.Context, loopID, key string) string {
	if g.repositories == nil || g.repositories.Loops == nil {
		return ""
	}
	loop, err := g.repositories.Loops.GetByID(ctx, loopID)
	if err != nil || loop == nil || loop.MetadataJSON == nil {
		return ""
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(*loop.MetadataJSON), &meta); err != nil {
		return ""
	}
	s, _ := meta[key].(string)
	return strings.TrimSpace(s)
}

// setLoopMetaString stores a top-level string field in a loop's metadata, preserving
// other keys. Best-effort, re-reading fresh to minimize clobbering concurrent writers.
func (g *Gateway) setLoopMetaString(ctx context.Context, loopID, key, value string) {
	if g.repositories == nil || g.repositories.Loops == nil {
		return
	}
	loop, err := g.repositories.Loops.GetByID(ctx, loopID)
	if err != nil || loop == nil {
		return
	}
	meta := map[string]any{}
	if loop.MetadataJSON != nil && strings.TrimSpace(*loop.MetadataJSON) != "" {
		_ = json.Unmarshal([]byte(*loop.MetadataJSON), &meta)
	}
	meta[key] = value
	encoded, err := json.Marshal(meta)
	if err != nil {
		return
	}
	s := string(encoded)
	loop.MetadataJSON = &s
	loop.UpdatedAt = eventlog.FormatJavaScriptISOString(g.now().UTC())
	_ = g.repositories.Loops.Upsert(ctx, *loop)
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
		// Never surface a raw shell command on the human-scannable anchor — an
		// unrecognised command becomes a generic phase; the raw feed lives inside
		// the thread (feishuLiveFeedCard), not here.
		return "正在处理…"
	}
}

// feishuLiveFeedCard renders the raw tool-call feed as a standalone card, posted
// as the first reply INSIDE the thread — the real-time status sync surface, kept
// separate from the human-scannable anchor.
func feishuLiveFeedCard(tail []string, elapsedSec int64) (string, bool) {
	return feishuLiveFeedCardWithOwner(tail, elapsedSec, "")
}

func feishuLiveFeedCardWithOwner(tail []string, elapsedSec int64, ownerOpenID string) (string, bool) {
	if len(tail) == 0 {
		return "", false
	}
	elements := []any{larkDiv(feishuActivityTail(tail))}
	note := "实时更新中"
	if elapsedSec > 0 {
		note = "⏱ " + humanizeElapsedSeconds(elapsedSec) + " · 实时更新中"
	}
	elements = append(elements, map[string]any{"tag": "note", "elements": []any{map[string]any{"tag": "lark_md", "content": note}}})
	elements = append(elements, feishuLooperOwnerNote(ownerOpenID))
	// No header — the body already leads with "🔧 实时进度"; a separate "实时进度同步"
	// title just repeats it. Feishu renders a header-less card fine.
	cardObj := map[string]any{
		"config":   map[string]any{"wide_screen_mode": true},
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

	eventType := "notification.sent"
	switch record.Status {
	case "pending":
		eventType = "notification.pending"
	case "delivered", "success":
		eventType = "notification.delivered"
	case "failed":
		eventType = "notification.failed"
	}
	if record.ErrorMessage != nil && record.Status == "pending" {
		eventType = "notification.delivery_failed"
	}
	return eventlog.Append(ctx, g.repositories, eventlog.AppendInput{
		ID:         eventlog.NewEventID("event"),
		EventType:  eventType,
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
			"error":     record.ErrorMessage,
		},
		CreatedAt: mustParseJSISOString(record.UpdatedAt),
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
