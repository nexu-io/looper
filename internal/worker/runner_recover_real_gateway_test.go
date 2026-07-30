package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	gitinfra "github.com/nexu-io/looper/internal/infra/git"
	"github.com/nexu-io/looper/internal/storage"
	"github.com/nexu-io/looper/internal/worktreesafety"
)

// realWorkerGitGateway adapts infra/git.Gateway for worker recovery contracts.
// Unused interface methods panic: these tests only exercise Restore→Create / preserve.
type realWorkerGitGateway struct {
	inner        *gitinfra.Gateway
	restoreCalls int
	createCalls  int
}

func (g *realWorkerGitGateway) CreateWorktree(ctx context.Context, input CreateWorktreeInput) (CreateWorktreeResult, error) {
	g.createCalls++
	record, err := g.inner.CreateWorktree(ctx, gitinfra.CreateWorktreeInput{
		ProjectID: input.ProjectID, RepoPath: input.RepoPath, WorktreeRoot: input.WorktreeRoot,
		Branch: input.Branch, BaseBranch: input.BaseBranch, PRNumber: input.PRNumber,
		ProtectedBranches: append([]string{}, input.ProtectedBranches...),
		CheckoutMode:      gitinfra.CheckoutMode(input.CheckoutMode),
	})
	if err != nil {
		return CreateWorktreeResult{}, err
	}
	return CreateWorktreeResult{
		WorktreePath: record.WorktreePath, Branch: record.Branch,
		BaseBranch: strings.TrimSpace(derefString(record.BaseBranch)),
		HeadSHA:    strings.TrimSpace(derefString(record.HeadSHA)), WorktreeID: record.ID,
	}, nil
}

func (g *realWorkerGitGateway) RestoreWorktree(ctx context.Context, input RestoreWorktreeInput) (*RestoreWorktreeResult, error) {
	g.restoreCalls++
	record, err := g.inner.RestoreWorktree(ctx, gitinfra.RestoreWorktreeInput{
		ProjectID: input.ProjectID, RepoPath: input.RepoPath, Branch: input.Branch,
		WorktreeRoot: input.WorktreeRoot, CheckoutMode: gitinfra.CheckoutMode(input.CheckoutMode),
		ExpectedWorktreePath: input.ExpectedWorktreePath,
	})
	if err != nil || record == nil {
		return nil, err
	}
	return &RestoreWorktreeResult{
		WorktreePath: record.WorktreePath, Branch: record.Branch,
		BaseBranch: strings.TrimSpace(derefString(record.BaseBranch)),
		HeadSHA:    strings.TrimSpace(derefString(record.HeadSHA)), WorktreeID: record.ID,
	}, nil
}

func (g *realWorkerGitGateway) PrepareWorktree(context.Context, PrepareWorktreeInput) (PrepareWorktreeResult, error) {
	panic("PrepareWorktree not used by recoverWorkerWorktree contract tests")
}
func (g *realWorkerGitGateway) InspectHead(context.Context, InspectHeadInput) (InspectHeadResult, error) {
	panic("InspectHead not used by recoverWorkerWorktree contract tests")
}
func (g *realWorkerGitGateway) Commit(context.Context, CommitInput) (CommitResult, error) {
	panic("Commit not used by recoverWorkerWorktree contract tests")
}
func (g *realWorkerGitGateway) Push(context.Context, PushInput) error {
	panic("Push not used by recoverWorkerWorktree contract tests")
}

func setupRealWorkerRepoWithBranch(t *testing.T, branch string) (repoPath, headSHA string) {
	t.Helper()
	root := t.TempDir()
	remotePath := filepath.Join(root, "remote.git")
	repoPath = filepath.Join(root, "repo")
	mustRunWorkerGit(t, root, "init", "--bare", remotePath)
	mustRunWorkerGit(t, root, "clone", remotePath, repoPath)
	mustRunWorkerGit(t, repoPath, "config", "user.email", "test@example.com")
	mustRunWorkerGit(t, repoPath, "config", "user.name", "Looper Test")
	if err := os.WriteFile(filepath.Join(repoPath, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("WriteFile README: %v", err)
	}
	mustRunWorkerGit(t, repoPath, "add", "README.md")
	mustRunWorkerGit(t, repoPath, "commit", "-m", "init")
	mustRunWorkerGit(t, repoPath, "branch", "-M", "main")
	mustRunWorkerGit(t, repoPath, "push", "-u", "origin", "main")
	mustRunWorkerGit(t, repoPath, "checkout", "-b", branch)
	if err := os.WriteFile(filepath.Join(repoPath, "feature.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatalf("WriteFile feature.txt: %v", err)
	}
	mustRunWorkerGit(t, repoPath, "add", "feature.txt")
	mustRunWorkerGit(t, repoPath, "commit", "-m", "feature")
	mustRunWorkerGit(t, repoPath, "push", "-u", "origin", branch)
	headSHA = strings.TrimSpace(mustRunWorkerGit(t, repoPath, "rev-parse", "HEAD"))
	mustRunWorkerGit(t, repoPath, "checkout", "main")
	return repoPath, headSHA
}

func mustRunWorkerGit(t *testing.T, cwd string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = cwd
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s (cwd=%s): %v\n%s", strings.Join(args, " "), cwd, err, out)
	}
	return string(out)
}

func realRecoverFixture(t *testing.T, branch string) (runner *Runner, adapter *realWorkerGitGateway, project storage.ProjectRecord, worktreeRoot, repoPath, headSHA string) {
	t.Helper()
	fixture := newRunnerFixture(t)
	repoPath, headSHA = setupRealWorkerRepoWithBranch(t, branch)
	worktreeRoot = filepath.Join(t.TempDir(), "worktrees")
	if err := os.MkdirAll(worktreeRoot, 0o755); err != nil {
		t.Fatalf("MkdirAll worktreeRoot: %v", err)
	}
	adapter = &realWorkerGitGateway{inner: gitinfra.New(gitinfra.Options{GitPath: "git"})}
	runner = New(Options{
		DB: fixture.coordinator.DB(), Repos: fixture.repos, Git: adapter,
		Logger: fixture.logger, Now: fixture.now, AllowAutoCommit: true,
	})
	metadata := fmt.Sprintf(`{"worktreeRoot":%q}`, worktreeRoot)
	project = storage.ProjectRecord{ID: "project_1", RepoPath: repoPath, MetadataJSON: &metadata}
	return runner, adapter, project, worktreeRoot, repoPath, headSHA
}

// Real gateway: hollow (metadata-only) path → Restore clears (nil,nil) → Create.
func TestRecoverWorkerWorktreeRealGatewayRecreatesHollow(t *testing.T) {
	t.Parallel()
	branch := "looper/worker-hollow"
	runner, adapter, project, worktreeRoot, _, headSHA := realRecoverFixture(t, branch)
	hollowPath := filepath.Join(worktreeRoot, "looper-worker-hollow")
	if err := os.MkdirAll(hollowPath, 0o755); err != nil {
		t.Fatalf("MkdirAll hollow: %v", err)
	}
	if err := os.WriteFile(filepath.Join(hollowPath, ".git"), []byte("gitdir: /does/not/exist\n"), 0o644); err != nil {
		t.Fatalf("WriteFile hollow .git: %v", err)
	}
	if worktreesafety.LocalCheckoutUsable(hollowPath) {
		t.Fatal("LocalCheckoutUsable(hollowPath) = true, want false")
	}
	work := workerInput{Title: "Implement worker loop", Repo: "acme/looper", IssueNumber: 27, BaseBranch: "main", Branch: branch, ExecutionMode: "create-pr"}
	wt := checkpointWorktree{ID: "worktree_old", Path: hollowPath, Branch: branch, BaseBranch: "main", HeadSHA: headSHA}
	checkpoint := workerCheckpoint{Work: &work, Worktree: &wt, Plan: &checkpointPlan{Summary: "plan", Items: []string{"Do it"}}}

	got, err := runner.recoverWorkerWorktree(context.Background(), stepInput{Project: project}, &checkpoint, work, wt, "path is not a usable git worktree")
	if err != nil {
		t.Fatalf("recoverWorkerWorktree() error = %v", err)
	}
	if adapter.restoreCalls < 1 || adapter.createCalls < 1 {
		t.Fatalf("restoreCalls=%d createCalls=%d, want both >= 1", adapter.restoreCalls, adapter.createCalls)
	}
	if strings.TrimSpace(got.Path) == "" || !worktreesafety.LocalCheckoutUsable(got.Path) {
		t.Fatalf("recovered path %q is not a usable git worktree", got.Path)
	}
	if _, err := os.Stat(filepath.Join(got.Path, "README.md")); err != nil {
		t.Fatalf("recreated worktree missing README: %v", err)
	}
}

// Real gateway: populated unusable path → preserve + FailureManualIntervention.
func TestRecoverWorkerWorktreeRealGatewayParksPreserved(t *testing.T) {
	t.Parallel()
	branch := "looper/worker-populated"
	runner, adapter, project, worktreeRoot, repoPath, headSHA := realRecoverFixture(t, branch)
	created, err := adapter.inner.CreateWorktree(context.Background(), gitinfra.CreateWorktreeInput{
		ProjectID: "project_1", RepoPath: repoPath, WorktreeRoot: worktreeRoot, Branch: branch, BaseBranch: "main",
	})
	if err != nil {
		t.Fatalf("CreateWorktree() error = %v", err)
	}
	populatedPath := created.WorktreePath
	_ = os.RemoveAll(filepath.Join(populatedPath, ".git"))
	_ = os.Remove(filepath.Join(populatedPath, ".git"))
	if err := os.WriteFile(filepath.Join(populatedPath, ".git"), []byte("gitdir: /does/not/exist\n"), 0o644); err != nil {
		t.Fatalf("WriteFile corrupt .git: %v", err)
	}
	marker := filepath.Join(populatedPath, "agent-partial-edit.txt")
	if err := os.WriteFile(marker, []byte("keep me\n"), 0o644); err != nil {
		t.Fatalf("WriteFile marker: %v", err)
	}
	if worktreesafety.LocalCheckoutUsable(populatedPath) {
		t.Fatal("LocalCheckoutUsable(populatedPath) = true, want false")
	}
	work := workerInput{Title: "Implement worker loop", Repo: "acme/looper", IssueNumber: 27, BaseBranch: "main", Branch: branch, ExecutionMode: "create-pr"}
	wt := checkpointWorktree{ID: "worktree_old", Path: populatedPath, Branch: branch, BaseBranch: "main", HeadSHA: headSHA}
	checkpoint := workerCheckpoint{Work: &work, Worktree: &wt, Plan: &checkpointPlan{Summary: "plan", Items: []string{"Do it"}}}

	_, err = runner.recoverWorkerWorktree(context.Background(), stepInput{Project: project}, &checkpoint, work, wt, "path is not a usable git worktree")
	var loopErr *loopError
	if !errors.As(err, &loopErr) || loopErr.kind != FailureManualIntervention {
		t.Fatalf("error = %v, want FailureManualIntervention", err)
	}
	if !strings.Contains(loopErr.message, "manual intervention required") {
		t.Fatalf("error = %q, want preserved MI message", loopErr.message)
	}
	if adapter.restoreCalls < 1 {
		t.Fatalf("restoreCalls = %d, want >= 1", adapter.restoreCalls)
	}
	if shouldRetryQueueFailure(loopErr.kind, 242, -1) {
		t.Fatal("preserved populated worktree must not requeue under unlimited maxAttempts")
	}
	got, readErr := os.ReadFile(marker)
	if readErr != nil || string(got) != "keep me\n" {
		t.Fatalf("marker = %q err=%v, want preserved agent output", got, readErr)
	}
}
