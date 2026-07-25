package fixer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setupRealRepoWithBranch creates a bare remote + clone with main and a feature branch.
// Shared by real-gateway prepare lifecycle tests (preserve vs recovery scenarios).
func setupRealRepoWithBranch(t *testing.T, branch string) (root, remotePath, repoPath, headSHA string) {
	t.Helper()
	root = t.TempDir()
	remotePath = filepath.Join(root, "remote.git")
	repoPath = filepath.Join(root, "repo")

	mustRunGit(t, root, "init", "--bare", remotePath)
	mustRunGit(t, root, "clone", remotePath, repoPath)
	mustRunGit(t, repoPath, "config", "user.email", "test@example.com")
	mustRunGit(t, repoPath, "config", "user.name", "Looper Test")
	if err := os.WriteFile(filepath.Join(repoPath, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("WriteFile README: %v", err)
	}
	mustRunGit(t, repoPath, "add", "README.md")
	mustRunGit(t, repoPath, "commit", "-m", "init")
	mustRunGit(t, repoPath, "branch", "-M", "main")
	mustRunGit(t, repoPath, "push", "-u", "origin", "main")
	mustRunGit(t, repoPath, "checkout", "-b", branch)
	if err := os.WriteFile(filepath.Join(repoPath, "fix.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatalf("WriteFile fix.txt: %v", err)
	}
	mustRunGit(t, repoPath, "add", "fix.txt")
	mustRunGit(t, repoPath, "commit", "-m", "feature")
	mustRunGit(t, repoPath, "push", "-u", "origin", branch)
	headSHA = strings.TrimSpace(mustRunGit(t, repoPath, "rev-parse", "HEAD"))
	mustRunGit(t, repoPath, "checkout", "main")
	return root, remotePath, repoPath, headSHA
}
