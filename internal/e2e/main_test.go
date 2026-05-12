package e2e

import (
	"os"
	"testing"

	"github.com/nexu-io/looper/internal/e2e/harness"
)

func TestMain(m *testing.M) {
	os.Exit(harness.RunTestMain(m))
}
