package reviewer

import (
	"testing"

	"github.com/nexu-io/looper/internal/config"
)

func TestGrokBuildReviewerNativeResumeRemainsUnsupported(t *testing.T) {
	if nativeResumeSupportedForReviewer(config.AgentVendorGrokBuild) {
		t.Fatal("Grok Build reviewer native resume must remain unsupported")
	}
}
