package reviewer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	gitinfra "github.com/nexu-io/looper/internal/infra/git"
	"github.com/nexu-io/looper/internal/storage"
	"github.com/nexu-io/looper/internal/worktreesafety"
)

// Lifecycle: resumed reviewer finds intervening fixer marker, re-prepare CreateWorktree
// clears the marker, Prepare returns RemoteHeadChangedError before dirt inspection,
// and success/stale cleanup must not force-remove the fixer-owned path.
func TestProcessClaimedItemRemoteHeadChangedPreservesFixerDirtAndToken(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	fixture.repos.Locks.SetNow(fixture.now)
	ctx := context.Background()
	repo := "acme/looper"
	prNumber := int64(42)
	loopTarget := "pr:acme/looper:42"
	projectID := "project_1"
	loopID := "loop_resume_remote_head_preserves_fixer"
	queueID := "queue_resume_remote_head_preserves_fixer"
	nowISO := fixture.nowISO()
	lockKey := storage.PullRequestLockKey(projectID, repo, prNumber)

	wtRoot := filepath.Join(t.TempDir(), "worktrees")
	wtPath := filepath.Join(wtRoot, "wt-42")
	if err := os.MkdirAll(wtPath, 0o755); err != nil {
		t.Fatalf("MkdirAll worktree: %v", err)
	}
	dirtyFile := filepath.Join(wtPath, "partial-fix.go")
	if err := os.WriteFile(dirtyFile, []byte("package partial\n"), 0o644); err != nil {
		t.Fatalf("WriteFile partial dirt: %v", err)
	}
	const token = "fixer:loop_intervening:run_1:owns-path"
	if err := worktreesafety.WriteFixerOwnerToken(wtPath, token); err != nil {
		t.Fatalf("WriteFixerOwnerToken: %v", err)
	}
	projectMeta := fmt.Sprintf(`{"worktreeRoot":%q}`, wtRoot)
	if err := fixture.repos.Projects.Upsert(ctx, storage.ProjectRecord{
		ID: projectID, Name: "Looper", RepoPath: filepath.Join(t.TempDir(), "repo"),
		BaseBranch: stringPtr("main"), MetadataJSON: &projectMeta, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := fixture.repos.Loops.Upsert(ctx, storage.LoopRecord{
		ID: loopID, Seq: 1, ProjectID: projectID, Type: "reviewer",
		TargetType: "pull_request", TargetID: &loopTarget, Repo: &repo, PRNumber: &prNumber,
		Status: "queued", CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	checkpointJSON := mustMarshalJSON(reviewerCheckpoint{
		Detail:   &checkpointDetail{Title: "Review me", State: "OPEN", HeadSHA: "abc123", BaseRefName: "main", ReviewRequests: []string{"octocat"}},
		Snapshot: &checkpointSnapshot{HeadSHA: "abc123"},
		Worktree: &checkpointWorktree{Path: wtPath, Branch: "pr-42-head", BaseBranch: "main", PreparedAt: nowISO},
		PendingReview: &pendingReviewCheckpoint{
			HeadSHA: "abc123", IdempotencyKey: "idem", Event: reviewEventAgentNative, Summary: "No actionable findings",
		},
		ResumePolicy:   "advance_from_checkpoint",
		ClaimedLockKey: lockKey,
	})
	if err := fixture.repos.Runs.Upsert(ctx, storage.RunRecord{
		ID: "run_failed_thread_res_remote_head", LoopID: loopID, Status: "failed",
		CurrentStep: stringPtr(string(stepThreadResolution)), LastCompletedStep: stringPtr(string(stepWorktree)),
		CheckpointJSON: &checkpointJSON, StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}
	queue := storage.QueueItemRecord{
		ID: queueID, ProjectID: &projectID, LoopID: &loopID, Type: "reviewer",
		TargetType: "pull_request", TargetID: loopTarget, Repo: &repo, PRNumber: &prNumber,
		DedupeKey: "reviewer:resume-remote-head-preserves-fixer", Priority: storage.QueuePriorityReviewer,
		Status: "running", AvailableAt: nowISO, LockKey: &lockKey, MaxAttempts: 3,
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	if err := fixture.repos.Queue.Upsert(ctx, queue); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	git := &fakeGitGateway{
		worktreePath: wtPath,
		prepareErr:   &gitinfra.RemoteHeadChangedError{Branch: "refs/pull/42/head", ExpectedHeadSHA: "abc123", ActualHeadSHA: "def456"},
	}
	runner := New(Options{
		DB: fixture.coordinator.DB(), Repos: fixture.repos,
		GitHub: &fakeGitHubGateway{author: "octocat", reviewRequests: []string{"octocat"}},
		Git:    git, Logger: fixture.logger, Now: fixture.now,
		LoopConfig: testReviewerLoopConfig(),
	})
	result, err := runner.ProcessClaimedItem(ctx, queue)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "skipped" || !contains(result.Summary, "Remote head changed") {
		t.Fatalf("result = %#v, want stale remote-head skip", result)
	}
	if len(git.prepareCalls) == 0 {
		t.Fatal("expected PrepareWorktree after intervening fixer marker")
	}
	if len(git.cleanupCalls) != 0 {
		t.Fatalf("cleanupCalls = %d, want 0 (must not force-remove fixer dirt after pre-inspect failure)", len(git.cleanupCalls))
	}
	got, err := worktreesafety.ReadFixerOwnerToken(wtPath)
	if err != nil {
		t.Fatalf("ReadFixerOwnerToken() error = %v", err)
	}
	if got != token {
		t.Fatalf("ReadFixerOwnerToken() = %q, want %q restored after pre-inspect prepare failure", got, token)
	}
	gotDirt, err := os.ReadFile(dirtyFile)
	if err != nil || string(gotDirt) != "package partial\n" {
		t.Fatalf("partial dirt = %q err=%v, want preserved", gotDirt, err)
	}
}

// Lifecycle: final-attempt prepare fetch failure after CreateWorktree cleared the
// fixer marker must restore ownership and skip terminal cleanup.
func TestProcessClaimedItemTerminalPrepareErrorPreservesFixerDirtAndToken(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	fixture.repos.Locks.SetNow(fixture.now)
	ctx := context.Background()
	repo := "acme/looper"
	prNumber := int64(42)
	loopTarget := "pr:acme/looper:42"
	projectID := "project_1"
	loopID := "loop_resume_prepare_terminal_preserves_fixer"
	queueID := "queue_resume_prepare_terminal_preserves_fixer"
	nowISO := fixture.nowISO()
	lockKey := storage.PullRequestLockKey(projectID, repo, prNumber)

	wtRoot := filepath.Join(t.TempDir(), "worktrees")
	wtPath := filepath.Join(wtRoot, "wt-42")
	if err := os.MkdirAll(wtPath, 0o755); err != nil {
		t.Fatalf("MkdirAll worktree: %v", err)
	}
	dirtyFile := filepath.Join(wtPath, "partial-fix.go")
	if err := os.WriteFile(dirtyFile, []byte("package partial\n"), 0o644); err != nil {
		t.Fatalf("WriteFile partial dirt: %v", err)
	}
	const token = "fixer:loop_intervening:run_1:owns-path"
	if err := worktreesafety.WriteFixerOwnerToken(wtPath, token); err != nil {
		t.Fatalf("WriteFixerOwnerToken: %v", err)
	}
	projectMeta := fmt.Sprintf(`{"worktreeRoot":%q}`, wtRoot)
	if err := fixture.repos.Projects.Upsert(ctx, storage.ProjectRecord{
		ID: projectID, Name: "Looper", RepoPath: filepath.Join(t.TempDir(), "repo"),
		BaseBranch: stringPtr("main"), MetadataJSON: &projectMeta, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := fixture.repos.Loops.Upsert(ctx, storage.LoopRecord{
		ID: loopID, Seq: 1, ProjectID: projectID, Type: "reviewer",
		TargetType: "pull_request", TargetID: &loopTarget, Repo: &repo, PRNumber: &prNumber,
		Status: "queued", CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	checkpointJSON := mustMarshalJSON(reviewerCheckpoint{
		Detail:         &checkpointDetail{Title: "Review me", State: "OPEN", HeadSHA: "abc123", BaseRefName: "main", ReviewRequests: []string{"octocat"}},
		Snapshot:       &checkpointSnapshot{HeadSHA: "abc123"},
		Worktree:       &checkpointWorktree{Path: wtPath, Branch: "pr-42-head", BaseBranch: "main", PreparedAt: nowISO},
		ResumePolicy:   "advance_from_checkpoint",
		ClaimedLockKey: lockKey,
	})
	if err := fixture.repos.Runs.Upsert(ctx, storage.RunRecord{
		ID: "run_failed_thread_res_prepare_err", LoopID: loopID, Status: "failed",
		CurrentStep: stringPtr(string(stepThreadResolution)), LastCompletedStep: stringPtr(string(stepWorktree)),
		CheckpointJSON: &checkpointJSON, StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}
	queue := storage.QueueItemRecord{
		ID: queueID, ProjectID: &projectID, LoopID: &loopID, Type: "reviewer",
		TargetType: "pull_request", TargetID: loopTarget, Repo: &repo, PRNumber: &prNumber,
		DedupeKey: "reviewer:resume-prepare-terminal-preserves-fixer", Priority: storage.QueuePriorityReviewer,
		Status: "running", AvailableAt: nowISO, LockKey: &lockKey, MaxAttempts: 1, // final attempt → terminal
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	if err := fixture.repos.Queue.Upsert(ctx, queue); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	git := &fakeGitGateway{
		worktreePath: wtPath,
		prepareErr:   fmt.Errorf("error: cannot run ssh: No such file or directory\nfatal: unable to fork"),
	}
	runner := New(Options{
		DB: fixture.coordinator.DB(), Repos: fixture.repos,
		GitHub: &fakeGitHubGateway{author: "octocat", reviewRequests: []string{"octocat"}},
		Git:    git, Logger: fixture.logger, Now: fixture.now,
		LoopConfig: testReviewerLoopConfig(),
	})
	result, err := runner.ProcessClaimedItem(ctx, queue)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "failed" {
		t.Fatalf("result.Status = %q, want failed", result.Status)
	}
	if len(git.cleanupCalls) != 0 {
		t.Fatalf("cleanupCalls = %d, want 0 on terminal prepare failure with unprepared path", len(git.cleanupCalls))
	}
	got, err := worktreesafety.ReadFixerOwnerToken(wtPath)
	if err != nil {
		t.Fatalf("ReadFixerOwnerToken() error = %v", err)
	}
	if got != token {
		t.Fatalf("ReadFixerOwnerToken() = %q, want %q restored after prepare failure", got, token)
	}
	gotDirt, err := os.ReadFile(dirtyFile)
	if err != nil || string(gotDirt) != "package partial\n" {
		t.Fatalf("partial dirt = %q err=%v, want preserved", gotDirt, err)
	}
}

func TestCleanupReviewerWorktreeIfTerminalSkipsUnpreparedAndFixerOwned(t *testing.T) {
	t.Parallel()

	wtPath := filepath.Join(t.TempDir(), "wt-42")
	if err := os.MkdirAll(wtPath, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	git := &fakeGitGateway{}
	runner := New(Options{Git: git})
	project := storage.ProjectRecord{ID: "project_1", RepoPath: t.TempDir(), BaseBranch: stringPtr("main")}

	// Unprepared path: never cleanup (prepare may not have inspected dirt).
	checkpoint := &reviewerCheckpoint{
		Worktree: &checkpointWorktree{Path: wtPath, Branch: "pr-42-head"},
	}
	runner.cleanupReviewerWorktreeIfTerminal(context.Background(), project, checkpoint)
	if len(git.cleanupCalls) != 0 {
		t.Fatalf("cleanupCalls = %d, want 0 for unprepared worktree", len(git.cleanupCalls))
	}

	// Prepared + fixer marker: still skip (intervening fixer ownership).
	const token = "fixer:loop:run:token"
	if err := worktreesafety.WriteFixerOwnerToken(wtPath, token); err != nil {
		t.Fatalf("WriteFixerOwnerToken: %v", err)
	}
	checkpoint.Worktree.PreparedAt = "2026-04-11T12:00:00.000Z"
	runner.cleanupReviewerWorktreeIfTerminal(context.Background(), project, checkpoint)
	if len(git.cleanupCalls) != 0 {
		t.Fatalf("cleanupCalls = %d, want 0 for fixer-owned prepared worktree", len(git.cleanupCalls))
	}

	// Prepared without marker: cleanup proceeds.
	if err := worktreesafety.ClearFixerOwnerToken(wtPath); err != nil {
		t.Fatalf("ClearFixerOwnerToken: %v", err)
	}
	runner.cleanupReviewerWorktreeIfTerminal(context.Background(), project, checkpoint)
	if len(git.cleanupCalls) != 1 {
		t.Fatalf("cleanupCalls = %d, want 1 for prepared reviewer-owned worktree", len(git.cleanupCalls))
	}
	if checkpoint.Worktree.CleanedAt == "" {
		t.Fatal("CleanedAt empty after prepared cleanup")
	}
}
