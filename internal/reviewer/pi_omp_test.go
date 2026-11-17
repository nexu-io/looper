package reviewer

import (
	"testing"

	"github.com/nexu-io/looper/internal/config"
)

func TestPiOmpReviewerNativeResumeRemainsUnsupported(t *testing.T) {
	for _, vendor := range []config.AgentVendor{config.AgentVendorPi, config.AgentVendorOmp} {
		if nativeResumeSupportedForReviewer(vendor) {
			t.Fatalf("%s reviewer native resume must remain unsupported", vendor)
		}
	}
}
