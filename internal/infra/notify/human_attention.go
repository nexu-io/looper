package notify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/nexu-io/looper/internal/domain"
	"github.com/nexu-io/looper/internal/eventlog"
	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
)

// HumanAttentionReason identifies which durable human-attention condition was entered.
type HumanAttentionReason string

const (
	// HumanAttentionAwaitingHuman is a mid-run HITL park (loop status awaiting_human).
	HumanAttentionAwaitingHuman HumanAttentionReason = "awaiting_human"
	// HumanAttentionManualIntervention is a durable manual_intervention hard hold.
	HumanAttentionManualIntervention HumanAttentionReason = "manual_intervention"
	// HumanAttentionReviewFixBudget is a no-HITL review-fix budget exhausted hold
	// (paused + review_fix_budget_exhausted). Sibling-only pause does not notify.
	HumanAttentionReviewFixBudget HumanAttentionReason = "review_fix_budget"
	// HumanAttentionReviewScopeHuman is a no-HITL needs_human scope hold
	// (paused + review_scope_human_required). Sibling-only pause does not notify.
	HumanAttentionReviewScopeHuman HumanAttentionReason = "review_scope_human"
)

// HumanAttentionInput describes one durable entry into a state that requires an operator.
//
// EntryKey permanently dedupes human-attention notifications for one durable entry.
//
// Failure it prevents: daemon restart, recovery re-observation, and unchanged
// parked re-polls would otherwise resend the same operator alert for a hold the
// operator already saw.
//
// Costs of this concept:
//   - Failed delivery is permanent for that entry: if the first attempt records an
//     in_app row (audit) but osascript/webhook fails, re-observe will not retry
//     that entry; the operator must notice via dashboard/other channels until a
//     leave+re-enter produces a new EntryKey.
//   - Key collision edge cases: two distinct parks that produce the same EntryKey
//     (e.g. same run id reused incorrectly, or FinishedAt missing so only queue id
//     is used) suppress a legitimate second alert.
//   - Storage coupling: dedupe authority is the notifications table (GetLatestByDedupe
//     on in_app), so notification persistence is required for cross-restart silence;
//     wiping notifications re-enables alerts for still-parked entries.
//   - Extra paths: callers must mint stable EntryKeys (run:/queue:/asked:/loop:) and
//     NotifyHumanAttention short-circuits on lookup before Notify.
//
// Why simpler alternatives are insufficient:
//   - Transition-only delivery without a durable key cannot survive restart: the
//     runtime re-observes parked state after recovery and would re-notify.
//   - Existing osascript throttle is time-windowed per channel and does not span
//     daemon restarts or distinguish leave+re-enter from unchanged park.
//   - Trusting "notify once in process memory" loses silence across restarts, which
//     is the dominant unattended-operator case for local macOS alerts.
//
// Authority for "already notified" is the durable notifications row for this
// dedupe key, not agent structured output.
type HumanAttentionInput struct {
	ProjectID  string
	LoopID     string
	LoopSeq    int64
	RunID      string
	LoopType   string
	Reason     HumanAttentionReason
	EntryKey   string
	Subtitle   string
	Question   string
	Evidence   string
	EntityType string
	EntityID   string
}

// NotifyHumanAttention emits one action_required notification for a durable
// human-attention entry. Best-effort: delivery/audit failures are recorded when
// possible and never returned as errors that could affect loop/queue/run behavior.
func (g *Gateway) NotifyHumanAttention(ctx context.Context, input HumanAttentionInput) []storage.NotificationRecord {
	if g == nil {
		return nil
	}
	reason := HumanAttentionReason(strings.TrimSpace(string(input.Reason)))
	if reason != HumanAttentionAwaitingHuman && reason != HumanAttentionManualIntervention && reason != HumanAttentionReviewFixBudget && reason != HumanAttentionReviewScopeHuman {
		return nil
	}
	entryKey := strings.TrimSpace(input.EntryKey)
	if entryKey == "" {
		return nil
	}
	dedupeKey := HumanAttentionDedupeKey(reason, entryKey)

	loopLabel := humanAttentionLoopLabel(input.LoopSeq, input.LoopID)
	body := fmt.Sprintf("%s requires operator attention (%s).", loopLabel, humanAttentionReasonLabel(reason))
	if loopType := strings.TrimSpace(input.LoopType); loopType != "" {
		body = fmt.Sprintf("%s (%s) requires operator attention (%s).", loopLabel, loopType, humanAttentionReasonLabel(reason))
	}
	if q := strings.TrimSpace(input.Question); q != "" {
		body += " " + q
	}
	if e := strings.TrimSpace(input.Evidence); e != "" {
		body += " " + e
	}

	entityType := strings.TrimSpace(input.EntityType)
	entityID := strings.TrimSpace(input.EntityID)
	if entityType == "" {
		entityType = "loop"
	}
	if entityID == "" {
		entityID = firstNonEmpty(input.LoopID, entryKey)
	}

	// Suppress remote channels only when a Feishu HITL ask card was actually
	// delivered for this park (live card in gateway state, or durable
	// hitl.transport=feishu). Unconditional awaiting_human LocalOnly would drop
	// the only remote alert for fixer parks and github/respond transports that
	// never send a Feishu card. manual_intervention never has that duplicate.
	localOnly := reason == HumanAttentionAwaitingHuman && g.feishuAskDeliveredForLoop(ctx, input.LoopID)

	payload := SystemNotificationPayload{
		ID:         humanAttentionReservationID(dedupeKey),
		ProjectID:  input.ProjectID,
		LoopID:     input.LoopID,
		RunID:      input.RunID,
		Level:      "action_required",
		Title:      "Looper Needs Attention",
		Subtitle:   strings.TrimSpace(input.Subtitle),
		Body:       body,
		Sound:      "Funk",
		EntityType: entityType,
		EntityID:   entityID,
		DedupeKey:  dedupeKey,
		LocalOnly:  localOnly,
	}

	// Atomic permanent entry claim before any channel delivery so concurrent
	// recovery rescan + post-claim observers cannot both emit.
	inApp, claimed := g.claimHumanAttentionEntry(ctx, payload)
	if !claimed {
		return nil
	}

	records := make([]storage.NotificationRecord, 0, 3)
	if inApp.ID != "" {
		records = append(records, inApp)
	}
	if record, ok := g.recordOsascript(ctx, payload); ok {
		records = append(records, record)
	}
	if !payload.LocalOnly {
		if strings.EqualFold(strings.TrimSpace(g.config.Webhook.Mode), "app") {
			if record, ok := g.recordFeishuApp(ctx, payload); ok {
				records = append(records, record)
			}
		} else if record, ok := g.recordWebhook(ctx, payload); ok {
			records = append(records, record)
		}
	}
	return records
}

// humanAttentionReservationID is a stable primary key for the permanent in_app
// claim row for one human-attention entry. Concurrent InsertIfAbsent callers
// race on this id; only the winner proceeds to osascript/webhook delivery.
func humanAttentionReservationID(dedupeKey string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(dedupeKey)))
	return "notification_ha_" + hex.EncodeToString(sum[:16])
}

// claimHumanAttentionEntry reserves the permanent in_app audit row for one
// entry. Returns claimed=false when another caller already reserved it.
// Without a notifications repository the claim cannot be durable; delivery is
// still allowed (same as the previous best-effort path without storage).
func (g *Gateway) claimHumanAttentionEntry(ctx context.Context, payload SystemNotificationPayload) (storage.NotificationRecord, bool) {
	nowISO := eventlog.FormatJavaScriptISOString(g.now())
	record := storage.NotificationRecord{
		ID:           firstNonEmpty(payload.ID, humanAttentionReservationID(payload.DedupeKey)),
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
	if g.repositories == nil || g.repositories.Notifications == nil {
		return record, true
	}
	inserted, err := g.repositories.Notifications.InsertIfAbsent(ctx, record)
	if err != nil {
		// Fail open on storage errors so a transient DB fault cannot permanently
		// silence a park; concurrent double-emit is preferred over total silence.
		return record, true
	}
	if !inserted {
		return storage.NotificationRecord{}, false
	}
	if g.repositories.Events != nil {
		_ = eventlog.Append(ctx, g.repositories, eventlog.AppendInput{
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
	return record, true
}

// feishuAskDeliveredForLoop reports whether a Feishu HITL ask card is the active
// remote signal for this loop: either this process still holds the live card, or
// durable loop metadata records hitl.transport=feishu after a successful send.
func (g *Gateway) feishuAskDeliveredForLoop(ctx context.Context, loopID string) bool {
	loopID = strings.TrimSpace(loopID)
	if loopID != "" && g.hasOpenFeishuAskCard(loopID) {
		return true
	}
	if loopID == "" || g.repositories == nil || g.repositories.Loops == nil {
		return false
	}
	loop, err := g.repositories.Loops.GetByID(ctx, loopID)
	if err != nil || loop == nil {
		return false
	}
	ask, ok := loops.ReadHITLAsk(loop.MetadataJSON)
	if !ok {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(ask.Transport), "feishu")
}

// hasOpenFeishuAskCard reports whether this gateway's shared state still holds a
// live Feishu ask card for loopID (set by SendHITLAsk, cleared by MarkAskAnswered).
func (g *Gateway) hasOpenFeishuAskCard(loopID string) bool {
	if g == nil || g.state == nil {
		return false
	}
	loopID = strings.TrimSpace(loopID)
	if loopID == "" {
		return false
	}
	g.state.liveMu.Lock()
	defer g.state.liveMu.Unlock()
	st, ok := g.state.askCards[loopID]
	return ok && strings.TrimSpace(st.msgID) != ""
}

// HumanAttentionDedupeKey builds the durable dedupe key for one human-attention entry.
func HumanAttentionDedupeKey(reason HumanAttentionReason, entryKey string) string {
	return fmt.Sprintf("human_attention:%s:%s", strings.TrimSpace(string(reason)), strings.TrimSpace(entryKey))
}

// IsHumanAttentionLoopStatus reports whether the loop status itself is a
// human-attention park (awaiting_human).
func IsHumanAttentionLoopStatus(status string) bool {
	return strings.TrimSpace(status) == string(domain.LoopStatusAwaitingHuman)
}

// IsManualInterventionCondition reports whether the durable queue/run hold is
// the hard manual_intervention condition (not ordinary retry/backoff/exhaustion).
func IsManualInterventionCondition(lastErrorKind string, resumePolicy string) bool {
	if strings.TrimSpace(lastErrorKind) == string(HumanAttentionManualIntervention) {
		return true
	}
	return strings.TrimSpace(resumePolicy) == string(HumanAttentionManualIntervention)
}

func humanAttentionLoopLabel(seq int64, loopID string) string {
	if seq > 0 {
		return fmt.Sprintf("Loop #%d", seq)
	}
	if id := strings.TrimSpace(loopID); id != "" {
		return "Loop " + id
	}
	return "A loop"
}

func humanAttentionReasonLabel(reason HumanAttentionReason) string {
	switch reason {
	case HumanAttentionAwaitingHuman:
		return "awaiting human decision"
	case HumanAttentionManualIntervention:
		return "manual intervention"
	case HumanAttentionReviewFixBudget:
		return "review-fix budget exhausted"
	case HumanAttentionReviewScopeHuman:
		return "review scope requires human judgment"
	default:
		return string(reason)
	}
}
