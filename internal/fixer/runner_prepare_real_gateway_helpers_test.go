package fixer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gitinfra "github.com/nexu-io/looper/internal/infra/git"
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

// realLinkedWorktreeFixture is a real detached PR worktree used by recovery tests.
type realLinkedWorktreeFixture struct {
	repoPath     string
	worktreeRoot string
	wtPath       string
	gitdir       string
	headSHA      string
	branch       string
	projectID    string
	gateway      *gitinfra.Gateway
}

// setupRealLinkedWorktree creates a real linked worktree at the managed path.
func setupRealLinkedWorktree(t *testing.T, projectID, branch string) realLinkedWorktreeFixture {
	t.Helper()
	root, _, repoPath, headSHA := setupRealRepoWithBranch(t, branch)
	worktreeRoot := filepath.Join(root, "worktrees")
	if err := os.MkdirAll(worktreeRoot, 0o755); err != nil {
		t.Fatalf("MkdirAll worktreeRoot: %v", err)
	}
	gateway := gitinfra.New(gitinfra.Options{GitPath: "git"})
	created, err := gateway.CreateWorktree(context.Background(), gitinfra.CreateWorktreeInput{
		ProjectID:    projectID,
		RepoPath:     repoPath,
		WorktreeRoot: worktreeRoot,
		Branch:       branch,
		BaseBranch:   "main",
		PRNumber:     42,
		CheckoutMode: gitinfra.CheckoutModeDetached,
	})
	if err != nil {
		t.Fatalf("CreateWorktree() error = %v", err)
	}
	wtPath := created.WorktreePath
	gitdir := resolveLinkedPrivateGitdir(t, wtPath)
	return realLinkedWorktreeFixture{
		repoPath:     repoPath,
		worktreeRoot: worktreeRoot,
		wtPath:       wtPath,
		gitdir:       gitdir,
		headSHA:      headSHA,
		branch:       branch,
		projectID:    projectID,
		gateway:      gateway,
	}
}

func resolveLinkedPrivateGitdir(t *testing.T, wtPath string) string {
	t.Helper()
	gitMeta, err := os.ReadFile(filepath.Join(wtPath, ".git"))
	if err != nil {
		t.Fatalf("ReadFile .git: %v", err)
	}
	line := strings.TrimSpace(string(gitMeta))
	const prefix = "gitdir:"
	if !strings.HasPrefix(strings.ToLower(line), prefix) {
		t.Fatalf(".git content = %q, want gitdir: pointer", line)
	}
	gitdir := strings.TrimSpace(line[len(prefix):])
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(wtPath, gitdir)
	}
	return gitdir
}

func resolveLinkedCommonDir(t *testing.T, gitdir string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(gitdir, "commondir"))
	if err != nil {
		t.Fatalf("ReadFile commondir: %v", err)
	}
	common := strings.TrimSpace(string(data))
	if common == "" {
		t.Fatal("commondir is empty")
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(gitdir, common)
	}
	return filepath.Clean(common)
}

// stripWorktreeExceptGit removes all worktree entries except .git so recovery
// can clear-and-recreate (only unusable .git metadata remains).
func stripWorktreeExceptGit(t *testing.T, wtPath string) {
	t.Helper()
	entries, err := os.ReadDir(wtPath)
	if err != nil {
		t.Fatalf("ReadDir worktree: %v", err)
	}
	for _, e := range entries {
		if e.Name() == ".git" {
			continue
		}
		if err := os.RemoveAll(filepath.Join(wtPath, e.Name())); err != nil {
			t.Fatalf("RemoveAll %s: %v", e.Name(), err)
		}
	}
}

func worktreeRootMetadataJSON(worktreeRoot string) string {
	return fmt.Sprintf(`{"worktreeRoot":%q}`, worktreeRoot)
}
