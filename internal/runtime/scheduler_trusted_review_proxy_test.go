package runtime

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/forge"
)

func TestMintTrustedReviewProxyResolvesConfiguredLooperCommandFromPATH(t *testing.T) {
	binDir := t.TempDir()
	looperPath := filepath.Join(binDir, "looper")
	if err := os.WriteFile(looperPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(looperPath) error = %v", err)
	}
	t.Setenv("PATH", binDir)

	sock, cleanup, err := mintTrustedReviewProxyForPR(
		"looper",
		nil,
		"acme/looper#42",
		t.TempDir(),
		config.Config{},
		forge.TrustedReviewProxyPolicy{
			Clean:            "COMMENT",
			Blocking:         "REQUEST_CHANGES",
			ExpectedCommitID: "head-42",
		},
		nil,
	)
	if err != nil {
		t.Fatalf("mintTrustedReviewProxyForPR() error = %v", err)
	}
	defer cleanup()
	if !filepath.IsAbs(sock) {
		t.Fatalf("socket path = %q, want absolute path", sock)
	}
}
