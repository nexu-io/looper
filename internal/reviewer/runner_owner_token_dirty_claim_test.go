package reviewer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	gitinfra "github.com/nexu-io/looper/internal/infra/git"
	"github.com/nexu-io/looper/internal/storage"
	"github.com/nexu-io/looper/internal/worktreesafety"
)

// Fresh reviewer (no checkpoint worktree) still hits an interrupted fixer's dirty
// checkout at the shared detached PR path. Capture the candidate marker before
// CreateWorktree clears it so dirty prepare can restore adopt authority.
func TestRunPrepareWorktreeStepFreshReviewerRestoresCandidateFixerTokenOnDirty(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	repoPath := t.TempDir()
	worktreeRoot := filepath.Join(t.TempDir(), "worktrees")
	projectID := "project_1"
	prNumber := int64(42)
	wtPath := gitinfra.DetachedPRWorktreePath(worktreeRoot, projectID, prNumber)
	if wtPath == "" {
		t.Fatal("DetachedPRWorktreePath empty")
	}
	if err := os.MkdirAll(wtPath, 0o755); err != nil {
		t.Fatalf("MkdirAll worktree: %v", err)
	}
	dirtyFile := filepath.Join(wtPath, "partial-fix.go")
	if err := os.WriteFile(dirtyFile, []byte("package partial\n"), 0o644); err != nil {
		t.Fatalf("WriteFile partial dirt: %v", err)
	}
	const token = "fixer:loop_interrupted:run_1:owns-shared-path"
	if err := worktreesafety.WriteFixerOwnerToken(wtPath, token); err != nil {
		t.Fatalf("WriteFixerOwnerToken: %v", err)
	}
	metadata := fmt.Sprintf(`{"worktreeRoot":%q}`, worktreeRoot)
	clean := false
	git := &fakeGitGateway{worktreePath: wtPath, prepareClean: &clean}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: &fakeGitHubGateway{}, Git: git, Logger: fixture.logger, Now: fixture.now})

	_, err := runner.runPrepareWorktreeStep(context.Background(), stepInput{
		Project:  storage.ProjectRecord{ID: projectID, RepoPath: repoPath, MetadataJSON: &metadata},
		Repo:     "acme/looper",
		PRNumber: prNumber,
		Checkpoint: reviewerCheckpoint{
			// No Worktree: fresh reviewer claim.
			Detail:   &checkpointDetail{HeadSHA: "abc123", BaseRefName: "main"},
			Snapshot: &checkpointSnapshot{HeadSHA: "abc123"},
		},
	})
	if err == nil {
		t.Fatal("expected dirty prepare error")
	}
	var loopErr *loopError
	if !errors.As(err, &loopErr) || loopErr.kind != FailureManualIntervention {
		t.Fatalf("error = %v, want dirty MI loopError", err)
	}
	if len(git.createCalls) != 1 {
		t.Fatalf("createCalls = %d, want 1", len(git.createCalls))
	}
	got, readErr := worktreesafety.ReadFixerOwnerToken(wtPath)
	if readErr != nil {
		t.Fatalf("ReadFixerOwnerToken() error = %v", readErr)
	}
	if got != token {
		t.Fatalf("ReadFixerOwnerToken() = %q, want %q restored after dirty prepare without checkpoint worktree", got, token)
	}
	gotDirt, err := os.ReadFile(dirtyFile)
	if err != nil || string(gotDirt) != "package partial\n" {
		t.Fatalf("partial dirt = %q err=%v, want preserved", gotDirt, err)
	}
}

// Intermediate persistCheckpoint after CreateWorktree revokes the fixer marker.
// If that write fails, restore prior ownership so the next fixer retry can still
// prove same-head adopt authority over preserved dirt (reviewer/fixer boundary).
func TestRunPrepareWorktreeStepRestoresFixerTokenWhenCheckpointPersistFails(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	ctx := context.Background()
	repoPath := t.TempDir()
	worktreeRoot := filepath.Join(t.TempDir(), "worktrees")
	projectID := "project_1"
	prNumber := int64(42)
	wtPath := gitinfra.DetachedPRWorktreePath(worktreeRoot, projectID, prNumber)
	if wtPath == "" {
		t.Fatal("DetachedPRWorktreePath empty")
	}
	if err := os.MkdirAll(wtPath, 0o755); err != nil {
		t.Fatalf("MkdirAll worktree: %v", err)
	}
	dirtyFile := filepath.Join(wtPath, "partial-fix.go")
	if err := os.WriteFile(dirtyFile, []byte("package partial\n"), 0o644); err != nil {
		t.Fatalf("WriteFile partial dirt: %v", err)
	}
	const token = "fixer:loop_interrupted:run_1:persist-fail-restore"
	if err := worktreesafety.WriteFixerOwnerToken(wtPath, token); err != nil {
		t.Fatalf("WriteFixerOwnerToken: %v", err)
	}
	metadata := fmt.Sprintf(`{"worktreeRoot":%q}`, worktreeRoot)
	nowISO := fixture.nowISO()
	loopTarget := "pr:acme/looper:42"
	if err := fixture.repos.Loops.Upsert(ctx, storage.LoopRecord{
		ID: "loop_persist_fail_restore", Seq: 1, ProjectID: projectID, Type: "reviewer",
		TargetType: "pull_request", TargetID: &loopTarget, Repo: stringPtr("acme/looper"), PRNumber: &prNumber,
		Status: "running", CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	run := storage.RunRecord{
		ID: "run_persist_fail_restore", LoopID: "loop_persist_fail_restore", Status: "running",
		CurrentStep: stringPtr(string(stepWorktree)), StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	if err := fixture.repos.Runs.Upsert(ctx, run); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}

	git := &fakeGitGateway{worktreePath: wtPath}
	runner := New(Options{
		DB: fixture.coordinator.DB(), Repos: fixture.repos,
		GitHub: &fakeGitHubGateway{}, Git: git, Logger: fixture.logger, Now: fixture.now,
	})
	// Close storage after CreateWorktree path is ready so intermediate
	// persistCheckpoint fails without reaching PrepareWorktree.
	if err := fixture.coordinator.Close(); err != nil {
		t.Fatalf("coordinator.Close() error = %v", err)
	}

	_, err := runner.runPrepareWorktreeStep(ctx, stepInput{
		Project:  storage.ProjectRecord{ID: projectID, RepoPath: repoPath, MetadataJSON: &metadata},
		Run:      run,
		Repo:     "acme/looper",
		PRNumber: prNumber,
		Checkpoint: reviewerCheckpoint{
			Detail:   &checkpointDetail{HeadSHA: "abc123", BaseRefName: "main"},
			Snapshot: &checkpointSnapshot{HeadSHA: "abc123"},
		},
	})
	if err == nil {
		t.Fatal("expected checkpoint persist failure")
	}
	if len(git.createCalls) != 1 {
		t.Fatalf("createCalls = %d, want 1 (CreateWorktree must clear marker before persist)", len(git.createCalls))
	}
	if len(git.prepareCalls) != 0 {
		t.Fatalf("prepareCalls = %d, want 0 (must not prepare after persist failure)", len(git.prepareCalls))
	}
	got, readErr := worktreesafety.ReadFixerOwnerToken(wtPath)
	if readErr != nil {
		t.Fatalf("ReadFixerOwnerToken() error = %v", readErr)
	}
	if got != token {
		t.Fatalf("ReadFixerOwnerToken() = %q, want %q restored after checkpoint persist failure", got, token)
	}
	gotDirt, err := os.ReadFile(dirtyFile)
	if err != nil || string(gotDirt) != "package partial\n" {
		t.Fatalf("partial dirt = %q err=%v, want preserved", gotDirt, err)
	}
}
