package reviewer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/eventlog"
	"github.com/nexu-io/looper/internal/storage"
	"github.com/nexu-io/looper/internal/worktreesafety"
)

// Lifecycle: resumed reviewer past stepWorktree must not revoke fixer ownership
// when PR lock reacquisition fails. A concurrent fixer still owns the checkout;
// premature ClearFixerOwnerToken would force its dirty retry into MI.
func TestProcessClaimedItemFailedLockResumePreservesFixerOwnerToken(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	fixture.repos.Locks.SetNow(fixture.now)
	ctx := context.Background()
	repo := "acme/looper"
	prNumber := int64(42)
	loopTarget := "pr:acme/looper:42"
	projectID := "project_1"
	loopID := "loop_resume_lock_preserves_owner"
	queueID := "queue_resume_lock_preserves_owner"
	nowISO := fixture.nowISO()
	lockKey := storage.PullRequestLockKey(projectID, repo, prNumber)

	wtRoot := filepath.Join(t.TempDir(), "worktrees")
	wtPath := filepath.Join(wtRoot, "wt-42")
	if err := os.MkdirAll(wtPath, 0o755); err != nil {
		t.Fatalf("MkdirAll worktree: %v", err)
	}
	const token = "fixer:loop_active:run_1:still-owns"
	if err := worktreesafety.WriteFixerOwnerToken(wtPath, token); err != nil {
		t.Fatalf("WriteFixerOwnerToken: %v", err)
	}

	if err := fixture.repos.Loops.Upsert(ctx, storage.LoopRecord{
		ID: loopID, Seq: 1, ProjectID: projectID, Type: "reviewer",
		TargetType: "pull_request", TargetID: &loopTarget, Repo: &repo, PRNumber: &prNumber,
		Status: "queued", CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	// Resume past worktree into publish with PendingReview so eligibility
	// rediscovery does not force restart_from_discover.
	checkpointJSON := mustMarshalJSON(reviewerCheckpoint{
		Detail:   &checkpointDetail{Title: "Review me", State: "OPEN", HeadSHA: "abc123", ReviewRequests: []string{"octocat"}},
		Snapshot: &checkpointSnapshot{HeadSHA: "abc123"},
		Worktree: &checkpointWorktree{Path: wtPath, Branch: "pr-42-head", BaseBranch: "main", PreparedAt: nowISO},
		PendingReview: &pendingReviewCheckpoint{
			HeadSHA: "abc123", IdempotencyKey: "idem", Event: reviewEventAgentNative, Summary: "No actionable findings",
		},
		ResumePolicy:   "advance_from_checkpoint",
		ClaimedLockKey: lockKey,
	})
	if err := fixture.repos.Runs.Upsert(ctx, storage.RunRecord{
		ID: "run_failed_past_worktree", LoopID: loopID, Status: "failed",
		CurrentStep: stringPtr(string(stepPublish)), LastCompletedStep: stringPtr(string(stepReview)),
		CheckpointJSON: &checkpointJSON, StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}
	queue := storage.QueueItemRecord{
		ID: queueID, ProjectID: &projectID, LoopID: &loopID, Type: "reviewer",
		TargetType: "pull_request", TargetID: loopTarget, Repo: &repo, PRNumber: &prNumber,
		DedupeKey: "reviewer:resume-lock-preserves-owner", Priority: storage.QueuePriorityReviewer,
		Status: "running", AvailableAt: nowISO, LockKey: &lockKey, MaxAttempts: 3,
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	if err := fixture.repos.Queue.Upsert(ctx, queue); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	// Another runner holds the PR lock (e.g. active fixer).
	reason := "fixer-active"
	expires := eventlog.FormatJavaScriptISOString(fixture.now().Add(time.Hour))
	acquired, err := fixture.repos.Locks.Acquire(ctx, storage.LockRecord{
		Key: lockKey, Owner: "queue_other_holder", Reason: &reason,
		ExpiresAt: expires, CreatedAt: nowISO, UpdatedAt: nowISO,
	})
	if err != nil || !acquired {
		t.Fatalf("Locks.Acquire(holder) = (%v, %v), want true", acquired, err)
	}

	runner := New(Options{
		DB: fixture.coordinator.DB(), Repos: fixture.repos,
		GitHub: &fakeGitHubGateway{author: "octocat", reviewRequests: []string{"octocat"}},
		Git:    &fakeGitGateway{}, Logger: fixture.logger, Now: fixture.now,
		LoopConfig: testReviewerLoopConfig(),
	})
	_, err = runner.ProcessClaimedItem(ctx, queue)
	if err == nil {
		t.Fatal("ProcessClaimedItem() error = nil, want lock-held failure")
	}
	var loopErr *loopError
	if !errors.As(err, &loopErr) || loopErr.kind != FailureRetryableTransient {
		t.Fatalf("ProcessClaimedItem() error = %v, want retryable transient loopError", err)
	}
	if !contains(loopErr.message, "Pull request lock is already held") {
		t.Fatalf("error message = %q, want lock-held", loopErr.message)
	}
	got, err := worktreesafety.ReadFixerOwnerToken(wtPath)
	if err != nil {
		t.Fatalf("ReadFixerOwnerToken() error = %v", err)
	}
	if got != token {
		t.Fatalf("ReadFixerOwnerToken() = %q, want %q (must preserve on failed lock reacquisition)", got, token)
	}
}

// Lifecycle: when resume reacquires the PR lock and an intervening fixer marker is
// present, ownership is revoked during re-prepare reclaim (CreateWorktree), not by
// blindly clearing the marker while trusting PreparedAt.
func TestProcessClaimedItemSuccessfulLockResumeRevokesFixerOwnerToken(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	fixture.repos.Locks.SetNow(fixture.now)
	ctx := context.Background()
	repo := "acme/looper"
	prNumber := int64(42)
	loopTarget := "pr:acme/looper:42"
	projectID := "project_1"
	loopID := "loop_resume_lock_revokes_owner"
	queueID := "queue_resume_lock_revokes_owner"
	nowISO := fixture.nowISO()
	lockKey := storage.PullRequestLockKey(projectID, repo, prNumber)

	wtRoot := filepath.Join(t.TempDir(), "worktrees")
	wtPath := filepath.Join(wtRoot, "wt-42")
	if err := os.MkdirAll(wtPath, 0o755); err != nil {
		t.Fatalf("MkdirAll worktree: %v", err)
	}
	const token = "fixer:loop_stale:run_1:must-revoke"
	if err := worktreesafety.WriteFixerOwnerToken(wtPath, token); err != nil {
		t.Fatalf("WriteFixerOwnerToken: %v", err)
	}
	// Keep checkpoint path inside project worktree root so re-prepare reclaims it.
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
	// Invalid approval body forces publish failure after lock + re-prepare reclaim.
	checkpointJSON := mustMarshalJSON(reviewerCheckpoint{
		Detail:   &checkpointDetail{Title: "Review me", State: "OPEN", HeadSHA: "abc123", ReviewRequests: []string{"octocat"}},
		Snapshot: &checkpointSnapshot{HeadSHA: "abc123"},
		Worktree: &checkpointWorktree{Path: wtPath, Branch: "pr-42-head", BaseBranch: "main", PreparedAt: nowISO},
		PendingReview: &pendingReviewCheckpoint{
			HeadSHA: "abc123", IdempotencyKey: "idem", Event: ReviewEventApprove, Summary: "No actionable findings",
		},
		ResumePolicy:   "advance_from_checkpoint",
		ClaimedLockKey: lockKey,
	})
	if err := fixture.repos.Runs.Upsert(ctx, storage.RunRecord{
		ID: "run_failed_past_worktree_revoke", LoopID: loopID, Status: "failed",
		CurrentStep: stringPtr(string(stepPublish)), LastCompletedStep: stringPtr(string(stepReview)),
		CheckpointJSON: &checkpointJSON, StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}
	queue := storage.QueueItemRecord{
		ID: queueID, ProjectID: &projectID, LoopID: &loopID, Type: "reviewer",
		TargetType: "pull_request", TargetID: loopTarget, Repo: &repo, PRNumber: &prNumber,
		DedupeKey: "reviewer:resume-lock-revokes-owner", Priority: storage.QueuePriorityReviewer,
		Status: "running", AvailableAt: nowISO, LockKey: &lockKey, MaxAttempts: 3,
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	if err := fixture.repos.Queue.Upsert(ctx, queue); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	github := &fakeGitHubGateway{
		author: "octocat", reviewRequests: []string{"octocat"},
		reviewMarkerOutcome: "clean", reviewMarkerEvent: ReviewEventApprove,
		// Body intentionally fails short-human-summary gate for APPROVE.
		reviewMarkerBody: "@octocat <!-- hidden filler words should not count toward this approval body --> <!-- looper:review outcome=clean -->",
	}
	git := &fakeGitGateway{worktreePath: wtPath}
	runner := New(Options{
		DB: fixture.coordinator.DB(), Repos: fixture.repos,
		GitHub: github, Git: git, Logger: fixture.logger, Now: fixture.now,
		ReviewEvents: config.ReviewerReviewEventsConfig{Clean: config.ReviewerReviewEventApprove},
		LoopConfig:   testReviewerLoopConfig(),
	})
	// ProcessClaimedItem should reacquire lock, re-enter worktree prepare (marker
	// present invalidates PreparedAt), revoke owner token via CreateWorktree, then fail publish.
	result, err := runner.ProcessClaimedItem(ctx, queue)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status == "" {
		t.Fatalf("ProcessClaimedItem() result = %#v, want non-empty status", result)
	}
	if len(git.prepareCalls) == 0 {
		t.Fatal("expected re-prepare after intervening fixer marker; prepareCalls empty")
	}
	got, err := worktreesafety.ReadFixerOwnerToken(wtPath)
	if err != nil {
		t.Fatalf("ReadFixerOwnerToken() error = %v", err)
	}
	if got != "" {
		t.Fatalf("ReadFixerOwnerToken() = %q, want empty after re-prepare reclaim", got)
	}
}
