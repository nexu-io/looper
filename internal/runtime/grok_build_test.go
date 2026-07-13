package runtime

import (
	"testing"

	"github.com/nexu-io/looper/internal/config"
)

func TestGrokBuildNativeResumeRemainsUnsupported(t *testing.T) {
	if runtimeNativeResumeSupported(string(config.AgentVendorGrokBuild)) {
		t.Fatal("Grok Build runtime native resume must remain unsupported")
	}
}
