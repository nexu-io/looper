package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type CWDEvidence struct {
	CWD       string            `json:"cwd"`
	Args      []string          `json:"args"`
	Env       map[string]string `json:"env"`
	Timestamp string            `json:"timestamp"`
	Mode      string            `json:"mode"`
	PID       int               `json:"pid"`
}

func LoadCWDEvidence(tb testing.TB, path string) CWDEvidence {
	tb.Helper()
	payload, err := os.ReadFile(path)
	if err != nil {
		tb.Fatalf("read cwd evidence %s: %v", path, err)
	}
	var evidence CWDEvidence
	if err := json.Unmarshal(payload, &evidence); err != nil {
		tb.Fatalf("decode cwd evidence %s: %v", path, err)
	}
	return evidence
}

func AssertRepoUnchanged(tb testing.TB, before RepoSnapshot, after RepoSnapshot) {
	tb.Helper()
	if before.Head != after.Head {
		tb.Fatalf("repo head changed: before=%s after=%s", before.Head, after.Head)
	}
	if before.StatusPorcelain != after.StatusPorcelain {
		tb.Fatalf("repo status changed:\nbefore:\n%s\nafter:\n%s", before.StatusPorcelain, after.StatusPorcelain)
	}
	if before.IndexTree != after.IndexTree {
		tb.Fatalf("repo index tree changed: before=%s after=%s", before.IndexTree, after.IndexTree)
	}
}

func AssertCWDInsideWorktree(tb testing.TB, cwd string, worktreeRoot string) {
	tb.Helper()
	resolvedCWD := mustEvalPath(tb, cwd)
	resolvedRoot := mustEvalPath(tb, worktreeRoot)
	if resolvedCWD != resolvedRoot && !strings.HasPrefix(resolvedCWD, resolvedRoot+string(os.PathSeparator)) {
		tb.Fatalf("cwd %s is not inside worktree root %s", resolvedCWD, resolvedRoot)
	}
}

func AssertCWDNotRepoPath(tb testing.TB, cwd string, repoPath string) {
	tb.Helper()
	resolvedCWD := mustEvalPath(tb, cwd)
	resolvedRepo := mustEvalPath(tb, repoPath)
	if resolvedCWD == resolvedRepo {
		tb.Fatalf("cwd %s unexpectedly equals repo path %s", resolvedCWD, resolvedRepo)
	}
}

func mustEvalPath(tb testing.TB, path string) string {
	tb.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil && resolved != "" {
		return resolved
	}
	absolute, absErr := filepath.Abs(path)
	if absErr != nil {
		tb.Fatalf("resolve path %s: %v", path, absErr)
	}
	return absolute
}
