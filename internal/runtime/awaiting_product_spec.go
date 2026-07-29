package runtime

import (
	"encoding/json"
	"strings"
)

// shouldResumeForProductSpec decides whether a paused planner loop that was waiting
// for a product spec (flowchart node E2) should now be resumed: it is a planner loop,
// currently paused, has NOT yet opened a spec PR (so it's still at the product-spec
// gate, not paused for a later reason), and its work item now has a product spec.
func shouldResumeForProductSpec(loopType, status string, hasSpecPR, hasProductSpec bool) bool {
	return strings.EqualFold(strings.TrimSpace(loopType), "planner") &&
		strings.EqualFold(strings.TrimSpace(status), "paused") &&
		!hasSpecPR &&
		hasProductSpec
}

// loopIssueURLAndSpecPR reads a planner loop's originating issue URL + whether it has
// already opened a spec PR, from the loop metadata.
func loopIssueURLAndSpecPR(metadataJSON *string) (issueURL string, hasSpecPR bool) {
	if metadataJSON == nil || strings.TrimSpace(*metadataJSON) == "" {
		return "", false
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(*metadataJSON), &meta); err != nil {
		return "", false
	}
	if s, ok := meta["issueUrl"].(string); ok {
		issueURL = strings.TrimSpace(s)
	} else if s, ok := meta["issueURL"].(string); ok {
		issueURL = strings.TrimSpace(s)
	}
	// Worker loops carry only prUrl at the top level and nest the originating
	// work-item URL under worker.issueUrl. Planner loops record it top-level (above).
	// Without this fallback, setPlaneWorkItemState reads "" for every worker loop,
	// so the Plane state never advances to In Review / Done for a worker-opened PR.
	if issueURL == "" {
		if worker, ok := meta["worker"].(map[string]any); ok {
			if value, ok := worker["issueUrl"].(string); ok {
				issueURL = strings.TrimSpace(value)
			} else if value, ok := worker["issueURL"].(string); ok {
				issueURL = strings.TrimSpace(value)
			}
		}
	}
	if value, ok := meta["prUrl"].(string); ok && strings.TrimSpace(value) != "" {
		hasSpecPR = true
	}
	return issueURL, hasSpecPR
}
