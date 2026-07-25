package reviewer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/nexu-io/looper/internal/storage"
	"github.com/nexu-io/looper/internal/worktreesafety"
)

// Lifecycle: reviewer fails during thread_resolution; intervening fixer stamps the
// shared detached PR path; reviewer retry must re-enter worktree preparation instead
// of clearing the marker while trusting PreparedAt (which createRunContext only
// clears for StartStep == stepReview).
func TestProcessClaimedItemThreadResolutionResumeRePreparesWhenFixerMarkerPresent(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	fixture.repos.Locks.SetNow(fixture.now)
	ctx := context.Background()
	repo := "acme/looper"
	prNumber := int64(42)
	loopTarget := "pr:acme/looper:42"
	projectID := "project_1"
	loopID := "loop_resume_thread_res_fixer_marker"
	queueID := "queue_resume_thread_res_fixer_marker"
	nowISO := fixture.nowISO()
	lockKey := storage.PullRequestLockKey(projectID, repo, prNumber)

	wtRoot := filepath.Join(t.TempDir(), "worktrees")
	wtPath := filepath.Join(wtRoot, "wt-42")
	if err := os.MkdirAll(wtPath, 0o755); err != nil {
		t.Fatalf("MkdirAll worktree: %v", err)
	}
	// Simulate fixer partial dirt under the shared path.
	if err := os.WriteFile(filepath.Join(wtPath, "partial-fix.go"), []byte("package partial\n"), 0o644); err != nil {
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
	// Failed during thread_resolution after a successful prepare. PreparedAt remains
	// set; createRunContext resumes at stepThreadResolution without clearing it.
	checkpointJSON := mustMarshalJSON(reviewerCheckpoint{
		Detail:   &checkpointDetail{Title: "Review me", State: "OPEN", HeadSHA: "abc123", BaseRefName: "main", ReviewRequests: []string{"octocat"}},
		Snapshot: &checkpointSnapshot{HeadSHA: "abc123"},
		Worktree: &checkpointWorktree{Path: wtPath, Branch: "pr-42-head", BaseBranch: "main", PreparedAt: nowISO},
		// PendingReview lets the run skip the review agent after re-prepare so the
		// assertion focuses on worktree reclaim rather than agent setup.
		PendingReview: &pendingReviewCheckpoint{
			HeadSHA: "abc123", IdempotencyKey: "idem", Event: reviewEventAgentNative, Summary: "No actionable findings",
		},
		ResumePolicy:   "advance_from_checkpoint",
		ClaimedLockKey: lockKey,
	})
	if err := fixture.repos.Runs.Upsert(ctx, storage.RunRecord{
		ID: "run_failed_thread_resolution", LoopID: loopID, Status: "failed",
		CurrentStep: stringPtr(string(stepThreadResolution)), LastCompletedStep: stringPtr(string(stepWorktree)),
		CheckpointJSON: &checkpointJSON, StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}
	queue := storage.QueueItemRecord{
		ID: queueID, ProjectID: &projectID, LoopID: &loopID, Type: "reviewer",
		TargetType: "pull_request", TargetID: loopTarget, Repo: &repo, PRNumber: &prNumber,
		DedupeKey: "reviewer:resume-thread-res-fixer-marker", Priority: storage.QueuePriorityReviewer,
		Status: "running", AvailableAt: nowISO, LockKey: &lockKey, MaxAttempts: 3,
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	if err := fixture.repos.Queue.Upsert(ctx, queue); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	git := &fakeGitGateway{worktreePath: wtPath}
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
	if result.Status == "" {
		t.Fatalf("ProcessClaimedItem() result = %#v, want non-empty status", result)
	}
	if len(git.createCalls) == 0 && len(git.prepareCalls) == 0 {
		t.Fatal("expected re-enter preparation for fixer-owned path; create/prepare not called")
	}
	if len(git.prepareCalls) == 0 {
		t.Fatal("expected PrepareWorktree after intervening fixer marker; prepareCalls empty")
	}
	got, err := worktreesafety.ReadFixerOwnerToken(wtPath)
	if err != nil {
		t.Fatalf("ReadFixerOwnerToken() error = %v", err)
	}
	if got != "" {
		t.Fatalf("ReadFixerOwnerToken() = %q, want empty after re-prepare reclaim", got)
	}
}

func TestReviewerWorktreePreparedRejectsFixerOwnedPath(t *testing.T) {
	t.Parallel()

	wtPath := filepath.Join(t.TempDir(), "wt-42")
	if err := os.MkdirAll(wtPath, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	checkpoint := reviewerCheckpoint{
		Worktree: &checkpointWorktree{Path: wtPath, Branch: "pr-42-head", PreparedAt: "2026-04-11T12:00:00.000Z"},
	}
	if !reviewerWorktreePrepared(checkpoint) {
		t.Fatal("reviewerWorktreePrepared() = false, want true without fixer marker")
	}
	if err := worktreesafety.WriteFixerOwnerToken(wtPath, "fixer:loop:run:token"); err != nil {
		t.Fatalf("WriteFixerOwnerToken: %v", err)
	}
	if reviewerWorktreePrepared(checkpoint) {
		t.Fatal("reviewerWorktreePrepared() = true, want false when fixer marker present")
	}
}
