package reviewer

import (
	"testing"

	"github.com/nexu-io/looper/internal/config"
)

func TestAgyReviewerNativeResumeRemainsUnsupported(t *testing.T) {
	if nativeResumeSupportedForReviewer(config.AgentVendorAgy) {
		t.Fatal("agy reviewer native resume must remain unsupported")
	}
}
