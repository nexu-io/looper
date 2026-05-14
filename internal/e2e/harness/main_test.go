package harness

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	os.Exit(RunTestMain(m))
}
