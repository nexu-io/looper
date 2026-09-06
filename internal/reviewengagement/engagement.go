// Package reviewengagement defines the shared follow-up rule for the reviewer
// daemon and trusted submit. Publication metadata remains the only stored head.
package reviewengagement

import (
	"encoding/json"
	"github.com/nexu-io/looper/internal/config"
	"strings"
)

func metadata(raw *string) map[string]any {
	var result map[string]any
	if raw != nil {
		_ = json.Unmarshal([]byte(*raw), &result)
	}
	return result
}

func Enabled(raw *string) bool {
	meta := metadata(raw)
	enabled, _ := meta["followUpdates"].(bool)
	manual, _ := meta["manual"].(bool)
	if !enabled || manual {
		return false
	}
	loop, _ := meta["loop"].(map[string]any)
	if enabled, ok := loop["enabled"].(bool); ok && !enabled {
		return false
	}
	return true
}

func PublishedHead(raw *string) string {
	head, _ := metadata(raw)["lastPublishedHeadSha"].(string)
	return strings.TrimSpace(head)
}

func HasNewHead(raw *string, head string) bool {
	previous := PublishedHead(raw)
	return Enabled(raw) && strings.TrimSpace(head) != "" && previous != "" && previous != strings.TrimSpace(head)
}

// Resolve prefers existing publication bookkeeping. Only an enabled loop with
// missing bookkeeping asks the caller for authenticated historical engagement.
// Callers supply provider I/O; this rule is identical across lifecycle entries.
func Resolve(raw *string, head string, recoverHead func() (string, error)) (string, error) {
	if !Enabled(raw) || strings.TrimSpace(head) == "" {
		return "", nil
	}
	if previous := PublishedHead(raw); previous != "" {
		return previous, nil
	}
	return recoverHead()
}

// Policy applies the loop's trusted policy snapshot over the project defaults.
func Policy(raw *string, fallback config.ReviewerReviewEventsConfig) config.ReviewerReviewEventsConfig {
	events, _ := metadata(raw)["reviewEvents"].(map[string]any)
	clean, _ := events["clean"].(string)
	blocking, _ := events["blocking"].(string)
	switch value := config.ReviewerReviewEvent(strings.ToUpper(strings.TrimSpace(clean))); value {
	case config.ReviewerReviewEventComment, config.ReviewerReviewEventApprove:
		fallback.Clean = value
	}
	switch value := config.ReviewerReviewEvent(strings.ToUpper(strings.TrimSpace(blocking))); value {
	case config.ReviewerReviewEventComment, config.ReviewerReviewEventRequestChanges:
		fallback.Blocking = value
	}
	return fallback
}

func AllowedEvents(policy config.ReviewerReviewEventsConfig) []string {
	events := []string{"COMMENT"}
	if policy.Clean == config.ReviewerReviewEventApprove {
		events = append(events, "APPROVE")
	}
	if policy.Blocking == config.ReviewerReviewEventRequestChanges {
		events = append(events, "REQUEST_CHANGES")
	}
	return events
}
