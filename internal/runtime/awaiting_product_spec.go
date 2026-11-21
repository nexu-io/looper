package runtime

import (
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
