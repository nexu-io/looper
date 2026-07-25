package reviewer

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	gitinfra "github.com/nexu-io/looper/internal/infra/git"
)

// setupRealRepoWithPRHead creates a bare remote + clone with main, a feature branch,
// and refs/pull/<n>/head on the remote for reviewer PrepareWorktree PR-ref fetches.
func setupRealRepoWithPRHead(t *testing.T, branch string, prNumber int64) (root, remotePath, repoPath, headSHA string) {
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
	if err := os.WriteFile(filepath.Join(repoPath, "feature.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatalf("WriteFile feature.txt: %v", err)
	}
	mustRunGit(t, repoPath, "add", "feature.txt")
	mustRunGit(t, repoPath, "commit", "-m", "feature")
	mustRunGit(t, repoPath, "push", "-u", "origin", branch)
	headSHA = strings.TrimSpace(mustRunGit(t, repoPath, "rev-parse", "HEAD"))
	// Reviewer prepare fetches refs/pull/<n>/head.
	mustRunGit(t, repoPath, "push", "origin", fmt.Sprintf("HEAD:refs/pull/%d/head", prNumber))
	mustRunGit(t, repoPath, "checkout", "main")
	return root, remotePath, repoPath, headSHA
}

func mustRunGit(t *testing.T, cwd string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = cwd
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		if msg == "" {
			msg = err.Error()
		}
		t.Fatalf("git %v: %s", args, msg)
	}
	return stdout.String()
}

// countingRealGitGateway adapts infra/git.Gateway for reviewer.GitGateway and
// records create/prepare/cleanup so lifecycle invariants can be asserted.
type countingRealGitGateway struct {
	inner        *gitinfra.Gateway
	cleanupCalls int
	createCalls  int
	prepareCalls int
}

func (g *countingRealGitGateway) CreateWorktree(ctx context.Context, input CreateWorktreeInput) (CreateWorktreeResult, error) {
	g.createCalls++
	rec, err := g.inner.CreateWorktree(ctx, gitinfra.CreateWorktreeInput{
		ProjectID: input.ProjectID, RepoPath: input.RepoPath, WorktreeRoot: input.WorktreeRoot,
		Branch: input.Branch, BaseBranch: input.BaseBranch, PRNumber: input.PRNumber,
		ProtectedBranches: input.ProtectedBranches, CheckoutMode: gitinfra.CheckoutMode(input.CheckoutMode),
	})
	if err != nil {
		return CreateWorktreeResult{}, err
	}
	head := ""
	if rec.HeadSHA != nil {
		head = *rec.HeadSHA
	}
	return CreateWorktreeResult{WorktreePath: rec.WorktreePath, Branch: rec.Branch, HeadSHA: head}, nil
}

func (g *countingRealGitGateway) PrepareWorktree(ctx context.Context, input PrepareWorktreeInput) (PrepareWorktreeResult, error) {
	g.prepareCalls++
	res, err := g.inner.PrepareWorktree(ctx, gitinfra.PrepareWorktreeInput{
		RepoPath: input.RepoPath, WorktreeRoot: input.WorktreeRoot, WorktreePath: input.WorktreePath,
		Branch: input.Branch, Ref: input.Ref, ExpectedHeadSHA: input.ExpectedHeadSHA, Remote: input.Remote,
	})
	if err != nil {
		return PrepareWorktreeResult{}, err
	}
	return PrepareWorktreeResult{HeadSHA: res.HeadSHA, Clean: res.Clean}, nil
}

func (g *countingRealGitGateway) CleanupWorktree(ctx context.Context, input CleanupWorktreeInput) error {
	g.cleanupCalls++
	return g.inner.CleanupWorktree(ctx, gitinfra.CleanupWorktreeInput{
		ProjectID: input.ProjectID, RepoPath: input.RepoPath, WorktreeRoot: input.WorktreeRoot,
		WorktreePath: input.WorktreePath, Branch: input.Branch, ProtectedBranches: input.ProtectedBranches,
	})
}
