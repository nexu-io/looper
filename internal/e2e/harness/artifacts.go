package harness

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

const envE2EArtifactRoot = "LOOPER_E2E_ARTIFACT_ROOT"

var artifactNameSanitizer = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func artifactTempDir(tb testing.TB, prefix string) string {
	tb.Helper()
	base := artifactBaseDir(tb)
	if base == "" {
		return tb.TempDir()
	}
	if err := os.MkdirAll(base, 0o755); err != nil {
		tb.Fatalf("mkdir artifact base: %v", err)
	}
	dir, err := os.MkdirTemp(base, prefix+"-")
	if err != nil {
		tb.Fatalf("mkdir artifact temp dir: %v", err)
	}
	return dir
}

func artifactBaseDir(tb testing.TB) string {
	tb.Helper()
	root := os.Getenv(envE2EArtifactRoot)
	if root == "" {
		return ""
	}
	name := artifactNameSanitizer.ReplaceAllString(tb.Name(), "-")
	if name == "" {
		name = "test"
	}
	return filepath.Join(root, name)
}
