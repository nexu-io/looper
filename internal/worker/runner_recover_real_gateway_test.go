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

// realWorkerGitGateway adapts infra/git.Gateway to worker.GitGateway so recovery
// tests exercise the production Restore→Create / preserve contracts.
type realWorkerGitGateway struct {
	inner        *gitinfra.Gateway
	restoreCalls int
	createCalls  int
}

func (g *realWorkerGitGateway) CreateWorktree(ctx context.Context, input CreateWorktreeInput) (CreateWorktreeResult, error) {
	g.createCalls++
	record, err := g.inner.CreateWorktree(ctx, gitinfra.CreateWorktreeInput{
		ProjectID:         input.ProjectID,
		RepoPath:          input.RepoPath,
		WorktreeRoot:      input.WorktreeRoot,
		Branch:            input.Branch,
		BaseBranch:        input.BaseBranch,
		PRNumber:          input.PRNumber,
		ProtectedBranches: append([]string{}, input.ProtectedBranches...),
		CheckoutMode:      gitinfra.CheckoutMode(input.CheckoutMode),
	})
	if err != nil {
		return CreateWorktreeResult{}, err
	}
	return CreateWorktreeResult{
		WorktreePath: record.WorktreePath,
		Branch:       record.Branch,
		BaseBranch:   strings.TrimSpace(derefString(record.BaseBranch)),
		HeadSHA:      strings.TrimSpace(derefString(record.HeadSHA)),
		WorktreeID:   record.ID,
	}, nil
}

func (g *realWorkerGitGateway) RestoreWorktree(ctx context.Context, input RestoreWorktreeInput) (*RestoreWorktreeResult, error) {
	g.restoreCalls++
	record, err := g.inner.RestoreWorktree(ctx, gitinfra.RestoreWorktreeInput{
		ProjectID:            input.ProjectID,
		RepoPath:             input.RepoPath,
		Branch:               input.Branch,
		WorktreeRoot:         input.WorktreeRoot,
		CheckoutMode:         gitinfra.CheckoutMode(input.CheckoutMode),
		ExpectedWorktreePath: input.ExpectedWorktreePath,
	})
	if err != nil || record == nil {
		return nil, err
	}
	return &RestoreWorktreeResult{
		WorktreePath: record.WorktreePath,
		Branch:       record.Branch,
		BaseBranch:   strings.TrimSpace(derefString(record.BaseBranch)),
		HeadSHA:      strings.TrimSpace(derefString(record.HeadSHA)),
		WorktreeID:   record.ID,
	}, nil
}

func (g *realWorkerGitGateway) PrepareWorktree(ctx context.Context, input PrepareWorktreeInput) (PrepareWorktreeResult, error) {
	prepared, err := g.inner.PrepareWorktree(ctx, gitinfra.PrepareWorktreeInput{
		WorktreePath:    input.WorktreePath,
		Branch:          input.Branch,
		ExpectedHeadSHA: input.ExpectedHeadSHA,
		Remote:          input.Remote,
	})
	if err != nil {
		return PrepareWorktreeResult{}, err
	}
	return PrepareWorktreeResult{HeadSHA: prepared.HeadSHA, Clean: prepared.Clean}, nil
}

func (g *realWorkerGitGateway) InspectHead(ctx context.Context, input InspectHeadInput) (InspectHeadResult, error) {
	result, err := g.inner.InspectHead(ctx, gitinfra.InspectHeadInput{WorktreePath: input.WorktreePath, BaseRef: input.BaseRef})
	if err != nil {
		return InspectHeadResult{}, err
	}
	return InspectHeadResult{
		HeadSHA:               result.HeadSHA,
		NewCommitSHAs:         result.NewCommitSHAs,
		HasUncommittedChanges: result.HasUncommittedChanges,
		ChangedFiles:          result.ChangedFiles,
	}, nil
}

func (g *realWorkerGitGateway) Commit(ctx context.Context, input CommitInput) (CommitResult, error) {
	result, err := g.inner.Commit(ctx, gitinfra.CommitInput{WorktreePath: input.WorktreePath, Message: input.Message})
	if err != nil {
		return CommitResult{}, err
	}
	return CommitResult{CommitSHA: result.CommitSHA}, nil
}

func (g *realWorkerGitGateway) Push(ctx context.Context, input PushInput) error {
	return g.inner.Push(ctx, gitinfra.PushInput{
		WorktreePath:      input.WorktreePath,
		Branch:            input.Branch,
		Remote:            input.Remote,
		ProtectedBranches: append([]string{}, input.ProtectedBranches...),
	})
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

// Real gateway contract: hollow (empty/metadata-only) checkpoint path is cleared
// by Restore (nil, nil) and recoverWorkerWorktree recreates via CreateWorktree.
func TestRunExecuteStepRealGatewayRecreatesHollowCheckpointWorktree(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	branch := "looper/worker-hollow"
	repoPath, headSHA := setupRealWorkerRepoWithBranch(t, branch)
	worktreeRoot := filepath.Join(t.TempDir(), "worktrees")
	if err := os.MkdirAll(worktreeRoot, 0o755); err != nil {
		t.Fatalf("MkdirAll worktreeRoot: %v", err)
	}
	// Managed path name matches CreateWorktree for branch-mode (sanitize branch).
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

	adapter := &realWorkerGitGateway{inner: gitinfra.New(gitinfra.Options{GitPath: "git"})}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "done", ParseStatus: "parsed"}}}
	runner := New(Options{
		DB: fixture.coordinator.DB(), Repos: fixture.repos, Git: adapter, AgentExecutor: agent,
		Logger: fixture.logger, Now: fixture.now, AllowAutoCommit: true,
	})
	run := storage.RunRecord{ID: "run_real_hollow_recreate", LoopID: "loop_worker_1", Status: "running", CurrentStep: stringPtr(string(stepExecute)), StartedAt: fixture.nowISO(), CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}
	if err := fixture.repos.Runs.Upsert(context.Background(), run); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}
	loop, err := fixture.repos.Loops.GetByID(context.Background(), "loop_worker_1")
	if err != nil || loop == nil {
		t.Fatalf("Loops.GetByID() = (%#v, %v), want loop", loop, err)
	}
	metadata := fmt.Sprintf(`{"worktreeRoot":%q}`, worktreeRoot)

	checkpoint, err := runner.runExecuteStep(context.Background(), stepInput{
		Project: storage.ProjectRecord{ID: "project_1", RepoPath: repoPath, MetadataJSON: &metadata},
		Loop:    *loop,
		Run:     run,
		Checkpoint: workerCheckpoint{
			Work:     &workerInput{Title: "Implement worker loop", Repo: "acme/looper", IssueNumber: 27, BaseBranch: "main", Branch: branch, ExecutionMode: "create-pr"},
			Worktree: &checkpointWorktree{ID: "worktree_old", Path: hollowPath, Branch: branch, BaseBranch: "main", HeadSHA: headSHA},
			Plan:     &checkpointPlan{Summary: "Implement worker loop", Items: []string{"Do it"}},
		},
	})
	if err != nil {
		t.Fatalf("runExecuteStep() error = %v", err)
	}
	if adapter.restoreCalls < 1 {
		t.Fatalf("restoreCalls = %d, want >= 1", adapter.restoreCalls)
	}
	if adapter.createCalls < 1 {
		t.Fatalf("createCalls = %d, want >= 1 (CreateWorktree fallback after restore nil)", adapter.createCalls)
	}
	if checkpoint.Worktree == nil || strings.TrimSpace(checkpoint.Worktree.Path) == "" {
		t.Fatalf("checkpoint.Worktree = %#v, want recreated path", checkpoint.Worktree)
	}
	if !worktreesafety.LocalCheckoutUsable(checkpoint.Worktree.Path) {
		t.Fatalf("recreated path %q is not a usable git worktree", checkpoint.Worktree.Path)
	}
	if _, err := os.Stat(filepath.Join(checkpoint.Worktree.Path, "README.md")); err != nil {
		t.Fatalf("recreated worktree missing README: %v", err)
	}
	if len(agent.starts) != 1 || agent.starts[0].WorkingDirectory != checkpoint.Worktree.Path {
		t.Fatalf("agent starts = %#v, want recreated working directory", agent.starts)
	}
}

// Real gateway contract: populated unusable managed path is preserved and parks
// as FailureManualIntervention (no Create, no agent start, no infinite retry).
func TestRunExecuteStepRealGatewayParksPreservedPopulatedWorktree(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	branch := "looper/worker-populated"
	repoPath, headSHA := setupRealWorkerRepoWithBranch(t, branch)
	worktreeRoot := filepath.Join(t.TempDir(), "worktrees")
	if err := os.MkdirAll(worktreeRoot, 0o755); err != nil {
		t.Fatalf("MkdirAll worktreeRoot: %v", err)
	}
	// Create a real worktree, then hollow the checkout while leaving agent output.
	gateway := gitinfra.New(gitinfra.Options{GitPath: "git"})
	created, err := gateway.CreateWorktree(context.Background(), gitinfra.CreateWorktreeInput{
		ProjectID:    "project_1",
		RepoPath:     repoPath,
		WorktreeRoot: worktreeRoot,
		Branch:       branch,
		BaseBranch:   "main",
	})
	if err != nil {
		t.Fatalf("CreateWorktree() error = %v", err)
	}
	populatedPath := created.WorktreePath
	// Corrupt .git metadata while keeping agent dirt that must be preserved.
	if err := os.RemoveAll(filepath.Join(populatedPath, ".git")); err != nil {
		// Linked worktrees use a .git *file*; remove that and rewrite unusable.
		_ = os.Remove(filepath.Join(populatedPath, ".git"))
	}
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

	adapter := &realWorkerGitGateway{inner: gateway}
	agent := &fakeAgentExecutor{}
	runner := New(Options{
		DB: fixture.coordinator.DB(), Repos: fixture.repos, Git: adapter, AgentExecutor: agent,
		Logger: fixture.logger, Now: fixture.now, AllowAutoCommit: true,
	})
	run := storage.RunRecord{ID: "run_real_populated_mi", LoopID: "loop_worker_1", Status: "running", CurrentStep: stringPtr(string(stepExecute)), StartedAt: fixture.nowISO(), CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}
	if err := fixture.repos.Runs.Upsert(context.Background(), run); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}
	loop, err := fixture.repos.Loops.GetByID(context.Background(), "loop_worker_1")
	if err != nil || loop == nil {
		t.Fatalf("Loops.GetByID() = (%#v, %v), want loop", loop, err)
	}
	metadata := fmt.Sprintf(`{"worktreeRoot":%q}`, worktreeRoot)

	_, err = runner.runExecuteStep(context.Background(), stepInput{
		Project: storage.ProjectRecord{ID: "project_1", RepoPath: repoPath, MetadataJSON: &metadata},
		Loop:    *loop,
		Run:     run,
		Checkpoint: workerCheckpoint{
			Work:     &workerInput{Title: "Implement worker loop", Repo: "acme/looper", IssueNumber: 27, BaseBranch: "main", Branch: branch, ExecutionMode: "create-pr"},
			Worktree: &checkpointWorktree{ID: "worktree_old", Path: populatedPath, Branch: branch, BaseBranch: "main", HeadSHA: headSHA},
			Plan:     &checkpointPlan{Summary: "Implement worker loop", Items: []string{"Do it"}},
		},
	})
	var loopErr *loopError
	if !errors.As(err, &loopErr) {
		t.Fatalf("runExecuteStep() error = %v, want *loopError", err)
	}
	if loopErr.kind != FailureManualIntervention {
		t.Fatalf("loopErr.kind = %v, want %v (err=%v)", loopErr.kind, FailureManualIntervention, err)
	}
	if !strings.Contains(loopErr.message, "manual intervention required") {
		t.Fatalf("error = %q, want preserved MI message", loopErr.message)
	}
	if adapter.restoreCalls < 1 {
		t.Fatalf("restoreCalls = %d, want >= 1", adapter.restoreCalls)
	}
	// Create may not run when restore already returned the preserve sentinel.
	// If restore returned nil for an unregistered path, create parks instead.
	if len(agent.starts) != 0 {
		t.Fatalf("agent starts = %#v, want none", agent.starts)
	}
	if shouldRetryQueueFailure(loopErr.kind, 242, -1) {
		t.Fatal("preserved populated worktree must not requeue under unlimited maxAttempts")
	}
	got, readErr := os.ReadFile(marker)
	if readErr != nil || string(got) != "keep me\n" {
		t.Fatalf("marker = %q err=%v, want preserved agent output", got, readErr)
	}
}
