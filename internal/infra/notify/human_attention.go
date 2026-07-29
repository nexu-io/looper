package notify

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/domain"
	"github.com/nexu-io/looper/internal/storage"
)

// HumanAttentionReason identifies which durable human-attention condition was entered.
type HumanAttentionReason string

const (
	// HumanAttentionAwaitingHuman is a mid-run HITL park (loop status awaiting_human).
	HumanAttentionAwaitingHuman HumanAttentionReason = "awaiting_human"
	// HumanAttentionManualIntervention is a durable manual_intervention hard hold.
	HumanAttentionManualIntervention HumanAttentionReason = "manual_intervention"
)

// HumanAttentionInput describes one durable entry into a state that requires an operator.
// EntryKey must uniquely identify this entry so daemon restart / unchanged park do not
// resend, while leaving and re-entering can notify again with a new key.
type HumanAttentionInput struct {
	ProjectID  string
	LoopID     string
	LoopSeq    int64
	RunID      string
	LoopType   string
	Reason     HumanAttentionReason
	EntryKey   string
	Subtitle   string
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
	if reason != HumanAttentionAwaitingHuman && reason != HumanAttentionManualIntervention {
		return nil
	}
	entryKey := strings.TrimSpace(input.EntryKey)
	if entryKey == "" {
		return nil
	}
	dedupeKey := HumanAttentionDedupeKey(reason, entryKey)
	if g.alreadyNotifiedHumanAttention(ctx, dedupeKey) {
		return nil
	}

	loopLabel := humanAttentionLoopLabel(input.LoopSeq, input.LoopID)
	body := fmt.Sprintf("%s requires operator attention (%s).", loopLabel, humanAttentionReasonLabel(reason))
	if loopType := strings.TrimSpace(input.LoopType); loopType != "" {
		body = fmt.Sprintf("%s (%s) requires operator attention (%s).", loopLabel, loopType, humanAttentionReasonLabel(reason))
	}

	openURL := ""
	if input.LoopSeq > 0 {
		if u, err := g.DashboardLoopDetailURL(input.LoopSeq); err == nil {
			openURL = u
		}
	}

	entityType := strings.TrimSpace(input.EntityType)
	entityID := strings.TrimSpace(input.EntityID)
	if entityType == "" {
		entityType = "loop"
	}
	if entityID == "" {
		entityID = firstNonEmpty(input.LoopID, entryKey)
	}

	return g.Notify(ctx, SystemNotificationPayload{
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
		OpenURL:    openURL,
	})
}

// HumanAttentionDedupeKey builds the durable dedupe key for one human-attention entry.
func HumanAttentionDedupeKey(reason HumanAttentionReason, entryKey string) string {
	return fmt.Sprintf("human_attention:%s:%s", strings.TrimSpace(string(reason)), strings.TrimSpace(entryKey))
}

// DashboardLoopDetailURL builds a local Dashboard Loop Detail URL from the gateway's
// configured base. The URL contains only the loop seq path segment — no auth token,
// answer text, or failure detail.
func (g *Gateway) DashboardLoopDetailURL(loopSeq int64) (string, error) {
	if g == nil {
		return "", fmt.Errorf("notification gateway is not configured")
	}
	if loopSeq <= 0 {
		return "", fmt.Errorf("loop seq is required for dashboard deep link")
	}
	base := strings.TrimRight(strings.TrimSpace(g.dashboardBaseURL), "/")
	if base == "" {
		return "", fmt.Errorf("dashboard base URL is not configured")
	}
	return base + "/dashboard/loops/" + strconv.FormatInt(loopSeq, 10), nil
}

// ResolveDashboardBaseURL derives a browser-openable local origin for Dashboard
// deep links from server config. Matches CLI dashboard host mapping for wildcards.
// Never includes path prefixes, tokens, or query parameters.
func ResolveDashboardBaseURL(server config.ServerConfig) string {
	if server.BaseURL != nil {
		base := strings.TrimRight(strings.TrimSpace(*server.BaseURL), "/")
		if base != "" {
			parsed, err := url.Parse(base)
			if err == nil && parsed.Scheme != "" && parsed.Host != "" && strings.Trim(parsed.Path, "/") == "" {
				return base
			}
		}
	}
	host := strings.TrimSpace(server.Host)
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	port := server.Port
	if port <= 0 {
		port = 17310
	}
	return "http://" + net.JoinHostPort(host, strconv.Itoa(port))
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

func (g *Gateway) alreadyNotifiedHumanAttention(ctx context.Context, dedupeKey string) bool {
	if g.repositories == nil || g.repositories.Notifications == nil || strings.TrimSpace(dedupeKey) == "" {
		return false
	}
	// Permanent entry dedupe across restarts: any prior in_app record with this
	// key means we already emitted for this durable entry.
	existing, err := g.repositories.Notifications.GetLatestByDedupe(ctx, "in_app", dedupeKey)
	if err != nil || existing == nil {
		return false
	}
	return true
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
	default:
		return string(reason)
	}
}
