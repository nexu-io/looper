package runtime

import (
	"testing"

	"github.com/nexu-io/looper/internal/config"
)

func TestAgyNativeResumeRemainsUnsupported(t *testing.T) {
	if runtimeNativeResumeSupported(string(config.AgentVendorAgy)) {
		t.Fatal("agy runtime native resume must remain unsupported")
	}
}
