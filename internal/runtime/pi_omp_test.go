package runtime

import (
	"testing"

	"github.com/nexu-io/looper/internal/config"
)

func TestPiOmpNativeResumeRemainsUnsupported(t *testing.T) {
	for _, vendor := range []config.AgentVendor{config.AgentVendorPi, config.AgentVendorOmp} {
		if runtimeNativeResumeSupported(string(vendor)) {
			t.Fatalf("%s runtime native resume must remain unsupported", vendor)
		}
	}
}
