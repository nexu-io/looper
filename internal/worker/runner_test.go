package worker

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/powerformer/looper/internal/config"
	"github.com/powerformer/looper/internal/storage"
)

func TestProcessNextIgnoresOtherQueueTypes(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	claimedWorker, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "worker-0", "worker")
	if err != nil || claimedWorker == nil {
		t.Fatalf("initial ClaimNextOfType() = (%#v, %v), want claimed worker", claimedWorker, err)
	}
	nowISO := fixture.nowISO()
	projectID := "project_1"
	loopID := "loop_planner_1"
	if err := fixture.repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 2, ProjectID: projectID, Type: "planner", TargetType: "issue", Status: "queued", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	if err := fixture.repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: "queue_planner_1", ProjectID: &projectID, LoopID: &loopID, Type: "planner", TargetType: "issue", TargetID: "issue:acme/looper:42", DedupeKey: "planner:acme/looper:42", Priority: 1, Status: "queued", AvailableAt: nowISO, MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: &fakeGitHubGateway{}, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})

	result, err := runner.ProcessNext(context.Background(), "worker-1")
	if err != nil {
		t.Fatalf("ProcessNext() error = %v", err)
	}
	if result != nil {
		t.Fatalf("ProcessNext() = %#v, want nil", result)
	}
}

func TestProcessClaimedItemCompletesCreatePRFlow(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	git := &fakeGitGateway{createResult: CreateWorktreeResult{WorktreePath: filepath.Join(t.TempDir(), "wt"), Branch: "looper/feature", BaseBranch: "main", HeadSHA: "abc123", WorktreeID: "worktree_1"}}
	github := &fakeGitHubGateway{createPRResult: CreatePullRequestResult{Number: 101, URL: "https://example/pr/101"}}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "done", Stdout: "ok"}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, AllowAutoCommit: true, AllowAutoPush: true, OpenPRStrategy: config.OpenPRStrategyAllDone})

	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "worker-1", "worker")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNextOfType() = (%#v, %v), want claimed item", claim, err)
	}
	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "success" || result.PullRequestNumber != 101 {
		t.Fatalf("result = %#v, want success with PR 101", result)
	}
	if len(agent.starts) != 1 || len(git.pushCalls) != 1 || len(github.createPRCalls) != 1 {
		t.Fatalf("agent starts=%d push=%d createPR=%d, want 1/1/1", len(agent.starts), len(git.pushCalls), len(github.createPRCalls))
	}
	if !strings.Contains(agent.starts[0].Prompt, `__LOOPER_RESULT__={"summary":"<one-sentence summary>"}`) {
		t.Fatalf("prompt = %q, want canonical completion instruction", agent.starts[0].Prompt)
	}
	loop, err := fixture.repos.Loops.GetByID(context.Background(), result.LoopID)
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if loop == nil || loop.Status != "completed" || loop.MetadataJSON == nil || !strings.Contains(*loop.MetadataJSON, `"prNumber":101`) {
		t.Fatalf("loop = %#v, want completed loop with PR metadata", loop)
	}
	run, err := fixture.repos.Runs.GetByID(context.Background(), result.RunID)
	if err != nil {
		t.Fatalf("Runs.GetByID() error = %v", err)
	}
	if run == nil || run.Status != "success" || run.LastCompletedStep == nil || *run.LastCompletedStep != string(stepOpenPR) {
		t.Fatalf("run = %#v, want success through open-pr", run)
	}
	worktrees, err := fixture.repos.Worktrees.ListByProject(context.Background(), "project_1")
	if err != nil {
		t.Fatalf("Worktrees.ListByProject() error = %v", err)
	}
	if len(worktrees) != 0 {
		t.Fatalf("Worktrees.ListByProject() = %#v, want no direct runner upsert with fake gateway", worktrees)
	}
	lock, err := fixture.repos.Locks.Get(context.Background(), "worker:loop_worker_1")
	if err != nil {
		t.Fatalf("Locks.Get() error = %v", err)
	}
	if lock != nil {
		t.Fatalf("lock = %#v, want prepare-work lock released after successful run", lock)
	}
}

func TestProcessClaimedItemResumesFromOpenPRAfterRetryableFailure(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	git := &fakeGitGateway{createResult: CreateWorktreeResult{WorktreePath: filepath.Join(t.TempDir(), "wt"), Branch: "looper/feature", BaseBranch: "main", HeadSHA: "abc123", WorktreeID: "worktree_1"}}
	github := &fakeGitHubGateway{createPRResult: CreatePullRequestResult{Number: 101, URL: "https://example/pr/101"}, createPRErrors: []error{fmt.Errorf("temporary create pr failure")}}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "done", Stdout: "ok"}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, AllowAutoCommit: true, AllowAutoPush: true, OpenPRStrategy: config.OpenPRStrategyAllDone})

	claim1, _ := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "worker-1", "worker")
	first, err := runner.ProcessClaimedItem(context.Background(), *claim1)
	if err != nil {
		t.Fatalf("ProcessClaimedItem(first) error = %v", err)
	}
	if first.Status != "failed" || first.FailureKind != FailureRetryableAfterResume {
		t.Fatalf("first = %#v, want retryable_after_resume failure", first)
	}
	fixture.advance(5 * time.Second)
	claim2, _ := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "worker-1", "worker")
	second, err := runner.ProcessClaimedItem(context.Background(), *claim2)
	if err != nil {
		t.Fatalf("ProcessClaimedItem(second) error = %v", err)
	}
	if second.Status != "success" {
		t.Fatalf("second = %#v, want success", second)
	}
	if len(agent.starts) != 1 {
		t.Fatalf("len(agent.starts) = %d, want 1 (execute should not rerun)", len(agent.starts))
	}
	if len(git.createCalls) != 1 {
		t.Fatalf("len(git.createCalls) = %d, want 1 (worktree should not rerun)", len(git.createCalls))
	}
	if len(github.createPRCalls) != 2 {
		t.Fatalf("len(github.createPRCalls) = %d, want 2", len(github.createPRCalls))
	}
}

func TestProcessClaimedItemResumeReleasesClaimedLockWhenSetupFails(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	checkpointJSON := `{"claimedLockKey":"worker:project_1"}`
	if err := fixture.repos.Runs.Upsert(context.Background(), storage.RunRecord{ID: "run_failed_resume", LoopID: "loop_worker_1", Status: "failed", LastCompletedStep: stringPtr(string(stepPrepareWork)), CheckpointJSON: &checkpointJSON, StartedAt: fixture.nowISO(), CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}
	if _, err := fixture.coordinator.DB().ExecContext(context.Background(), `
		CREATE TRIGGER loops_fail_running_resume_worker
		BEFORE UPDATE ON loops
		FOR EACH ROW
		WHEN NEW.id = 'loop_worker_1' AND NEW.status = 'running'
		BEGIN
			SELECT RAISE(FAIL, 'forced loop update failure');
		END;
	`); err != nil {
		t.Fatalf("create trigger error = %v", err)
	}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: &fakeGitHubGateway{}, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})

	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "worker-1", "worker")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNextOfType() = (%#v, %v), want claimed item", claim, err)
	}
	_, err = runner.ProcessClaimedItem(context.Background(), *claim)
	if err == nil || !strings.Contains(err.Error(), "forced loop update failure") {
		t.Fatalf("ProcessClaimedItem() error = %v, want forced loop update failure", err)
	}
	lock, err := fixture.repos.Locks.Get(context.Background(), "worker:project_1")
	if err != nil {
		t.Fatalf("Locks.Get() error = %v", err)
	}
	if lock != nil {
		t.Fatalf("lock = %#v, want released claimed lock", lock)
	}
	acquired, err := fixture.repos.Locks.Acquire(context.Background(), storage.LockRecord{Key: "worker:project_1", Owner: "retry", ExpiresAt: fixture.now().Add(time.Minute).UTC().Format("2006-01-02T15:04:05.000Z"), CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()})
	if err != nil {
		t.Fatalf("Locks.Acquire() error = %v", err)
	}
	if !acquired {
		t.Fatal("Locks.Acquire() = false, want claimed lock to be immediately reacquirable")
	}
}

func TestProcessClaimedItemValidationFailureRequeues(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	git := &fakeGitGateway{createResult: CreateWorktreeResult{WorktreePath: filepath.Join(t.TempDir(), "wt"), Branch: "looper/feature", BaseBranch: "main", HeadSHA: "abc123", WorktreeID: "worktree_1"}}
	github := &fakeGitHubGateway{}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "done", Stdout: "ok"}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, AllowAutoCommit: true, AllowAutoPush: true, OpenPRStrategy: config.OpenPRStrategyAllDone, ValidationRunner: func(context.Context, ValidationInput) (ValidationResult, error) {
		return ValidationResult{Passed: false, Summary: "Validation failed"}, nil
	}})

	claim, _ := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "worker-1", "worker")
	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "failed" || result.FailureKind != FailureManualIntervention {
		t.Fatalf("result = %#v, want manual_intervention failure", result)
	}
	queue, err := fixture.repos.Queue.GetByID(context.Background(), claim.ID)
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	if queue == nil || queue.Status != string(FailureManualIntervention) {
		t.Fatalf("queue = %#v, want terminal manual_intervention item", queue)
	}
	loop, err := fixture.repos.Loops.GetByID(context.Background(), result.LoopID)
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if loop == nil || loop.Status != "paused" {
		t.Fatalf("loop = %#v, want paused", loop)
	}
}

func TestProcessClaimedItemPreservesPausedLoopOnRetryableFailureAfterPause(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "done", Stdout: "ok"}}, wait: func(ctx context.Context) error {
		loopID := ""
		items, err := fixture.repos.Queue.List(ctx)
		if err != nil {
			return err
		}
		for _, item := range items {
			if item.Type == "worker" && item.Status == "running" && item.LoopID != nil {
				loopID = *item.LoopID
				break
			}
		}
		if loopID == "" {
			return fmt.Errorf("running worker queue item not found")
		}
		loop, err := fixture.repos.Loops.GetByID(ctx, loopID)
		if err != nil {
			return err
		}
		if loop == nil {
			return fmt.Errorf("loop not found: %s", loopID)
		}
		loop.Status = "paused"
		loop.NextRunAt = nil
		loop.UpdatedAt = fixture.nowISO()
		if err := fixture.repos.Loops.Upsert(ctx, *loop); err != nil {
			return err
		}
		reason := "loop paused"
		if _, err := fixture.repos.Queue.CancelByLoop(ctx, loopID, fixture.nowISO(), &reason); err != nil {
			return err
		}
		return context.DeadlineExceeded
	}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: &fakeGitHubGateway{}, Git: &fakeGitGateway{createResult: CreateWorktreeResult{WorktreePath: filepath.Join(t.TempDir(), "wt"), Branch: "looper/feature", BaseBranch: "main", HeadSHA: "abc123", WorktreeID: "worktree_1"}}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, AllowAutoCommit: true, AllowAutoPush: true, OpenPRStrategy: config.OpenPRStrategyAllDone})

	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "worker-1", "worker")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNextOfType() = (%#v, %v), want claimed item", claim, err)
	}
	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "failed" || result.FailureKind != FailureRetryableTransient {
		t.Fatalf("result = %#v, want retryable_transient failure", result)
	}
	queue, err := fixture.repos.Queue.GetByID(context.Background(), claim.ID)
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	if queue == nil || queue.Status != "queued" {
		t.Fatalf("queue = %#v, want queued retry", queue)
	}
	loop, err := fixture.repos.Loops.GetByID(context.Background(), result.LoopID)
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if loop == nil || loop.Status != "paused" || loop.NextRunAt != nil {
		t.Fatalf("loop = %#v, want paused loop with nil next run", loop)
	}
}

func TestProcessNextSetupFailureMarksQueueFailed(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	claimedWorker, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "worker-0", "worker")
	if err != nil || claimedWorker == nil {
		t.Fatalf("initial ClaimNextOfType() = (%#v, %v), want claimed worker", claimedWorker, err)
	}
	projectID := "project_1"
	nowISO := fixture.nowISO()
	payload := `{"repo":"acme/looper","baseBranch":"main","prompt":"test"}`
	if err := fixture.repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: "queue_missing_worker", ProjectID: &projectID, Type: "worker", TargetType: "issue", TargetID: "issue:acme/looper:99", Repo: stringPtr("acme/looper"), DedupeKey: "worker:acme/looper:99", Priority: 1, Status: "queued", AvailableAt: nowISO, MaxAttempts: 3, PayloadJSON: &payload, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: &fakeGitHubGateway{}, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})

	result, err := runner.ProcessNext(context.Background(), "worker-1")
	if err != nil {
		t.Fatalf("ProcessNext() error = %v", err)
	}
	if result == nil || result.Status != "failed" || result.FailureKind != FailureNonRetryable || !strings.Contains(result.Summary, "requires loopId") {
		t.Fatalf("result = %#v, want non-retryable missing-loopId failure", result)
	}
	queue, err := fixture.repos.Queue.GetByID(context.Background(), "queue_missing_worker")
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	if queue == nil || queue.Status != "failed" {
		t.Fatalf("queue = %#v, want failed", queue)
	}
}

func TestRecoverClaimedItemReconcilesRunningLoopState(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	nowISO := fixture.nowISO()
	loopTarget := "project:project_1"
	if err := fixture.repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: "loop_worker_running", Seq: 2, ProjectID: "project_1", Type: "worker", TargetType: "project", TargetID: &loopTarget, Repo: stringPtr("acme/looper"), Status: "running", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	projectID := "project_1"
	loopID := "loop_worker_running"
	payload := `{"title":"Recover worker loop","prompt":"Do the thing","repo":"acme/looper","baseBranch":"main"}`
	if err := fixture.repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: "queue_worker_running", ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: loopTarget, Repo: stringPtr("acme/looper"), DedupeKey: "worker:loop_worker_running", Priority: 1, Status: "running", AvailableAt: nowISO, Attempts: 0, MaxAttempts: 1, PayloadJSON: &payload, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: &fakeGitHubGateway{}, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})

	result, err := runner.recoverClaimedItem(context.Background(), storage.QueueItemRecord{ID: "queue_worker_running", ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: loopTarget, Repo: stringPtr("acme/looper"), DedupeKey: "worker:loop_worker_running", Priority: 1, Status: "running", AvailableAt: nowISO, Attempts: 0, MaxAttempts: 1, PayloadJSON: &payload, CreatedAt: nowISO, UpdatedAt: nowISO}, fmt.Errorf("persist step failed"))
	if err != nil {
		t.Fatalf("recoverClaimedItem() error = %v", err)
	}
	if result == nil || result.Status != "failed" || result.FailureKind != FailureNonRetryable {
		t.Fatalf("result = %#v, want failed non-retryable recovery", result)
	}
	queue, err := fixture.repos.Queue.GetByID(context.Background(), "queue_worker_running")
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	if queue == nil || queue.Status != "failed" {
		t.Fatalf("queue = %#v, want failed", queue)
	}
	loop, err := fixture.repos.Loops.GetByID(context.Background(), loopID)
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if loop == nil || loop.Status != "failed" || loop.NextRunAt != nil {
		t.Fatalf("loop = %#v, want failed terminal loop", loop)
	}
}

func TestProcessClaimedItemPullRequestLoopRequiresSpecPath(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	nowISO := fixture.nowISO()
	prNumber := int64(42)
	loopTarget := "pr:acme/looper:42"
	loopMeta := `{"worker":{"repo":"acme/looper","baseBranch":"main"}}`
	if err := fixture.repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: "loop_worker_pr", Seq: 2, ProjectID: "project_1", Type: "worker", TargetType: "pull_request", TargetID: &loopTarget, Repo: stringPtr("acme/looper"), PRNumber: &prNumber, Status: "queued", MetadataJSON: &loopMeta, NextRunAt: &nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	projectID := "project_1"
	loopID := "loop_worker_pr"
	payload := `{"repo":"acme/looper","baseBranch":"main","prompt":"do not use prompt for PR loops"}`
	if err := fixture.repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: "queue_worker_pr", ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "pull_request", TargetID: loopTarget, Repo: stringPtr("acme/looper"), PRNumber: &prNumber, DedupeKey: "worker:acme/looper:42", Priority: 1, Status: "queued", AvailableAt: nowISO, MaxAttempts: 3, PayloadJSON: &payload, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}
	github := &fakeGitHubGateway{prDetail: PullRequestDetail{Number: 42, Title: "Existing PR", BaseRefName: "main", HeadRefName: "feature/pr-42", HeadSHA: "abc123"}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now, AllowAutoCommit: true, AllowAutoPush: true, OpenPRStrategy: config.OpenPRStrategyAllDone})

	claim, _ := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "worker-1", "worker")
	claim, _ = fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "worker-2", "worker")
	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.FailureKind != FailureManualIntervention || !strings.Contains(result.Summary, "No explicit spec path found") {
		t.Fatalf("result = %#v, want manual_intervention spec-path failure", result)
	}
}

func TestProcessClaimedItemFindsExistingPRAfterPush(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	branch := buildWorkerBranchName(workerInput{Title: "Implement worker loop", Repo: "acme/looper", BaseBranch: "main", ExecutionMode: "create-pr"}, "loop_worker_1")
	git := &fakeGitGateway{createResult: CreateWorktreeResult{WorktreePath: filepath.Join(t.TempDir(), "wt"), Branch: branch, BaseBranch: "main", HeadSHA: "abc123", WorktreeID: "worktree_1"}}
	github := &fakeGitHubGateway{openPRResponses: [][]PullRequestSummary{{}, {{Number: 201, URL: "https://example/pr/201", State: "OPEN", HeadRefName: branch, BaseRefName: "main"}}}}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "done", Stdout: "ok"}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, AllowAutoCommit: true, AllowAutoPush: true, OpenPRStrategy: config.OpenPRStrategyAllDone})

	claim, _ := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "worker-1", "worker")
	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "success" || result.PullRequestNumber != 201 {
		t.Fatalf("result = %#v, want success with reused PR 201", result)
	}
	if len(github.createPRCalls) != 0 {
		t.Fatalf("len(github.createPRCalls) = %d, want 0", len(github.createPRCalls))
	}
}

func TestProcessClaimedItemFailsWhenCreatedPRNumberIsMissing(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	git := &fakeGitGateway{createResult: CreateWorktreeResult{WorktreePath: filepath.Join(t.TempDir(), "wt"), Branch: "looper/feature", BaseBranch: "main", HeadSHA: "abc123", WorktreeID: "worktree_1"}}
	github := &fakeGitHubGateway{createPRResult: CreatePullRequestResult{Number: 0, URL: "https://example/pr/unparsed"}}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "done", Stdout: "ok"}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, AllowAutoCommit: true, AllowAutoPush: true, OpenPRStrategy: config.OpenPRStrategyAllDone})

	claim, _ := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "worker-1", "worker")
	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "failed" || result.FailureKind != FailureRetryableAfterResume || !strings.Contains(result.Summary, "requires a pull request number") {
		t.Fatalf("result = %#v, want retryable_after_resume missing-pr-number failure", result)
	}
	if len(github.reviewerCalls) != 0 {
		t.Fatalf("len(github.reviewerCalls) = %d, want 0", len(github.reviewerCalls))
	}
	loop, err := fixture.repos.Loops.GetByID(context.Background(), result.LoopID)
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if loop == nil || (loop.MetadataJSON != nil && strings.Contains(*loop.MetadataJSON, `"prNumber":0`)) {
		t.Fatalf("loop = %#v, want no persisted PR number 0", loop)
	}
}

func TestRunValidationUsesShellCommandsByDefault(t *testing.T) {
	t.Parallel()
	runner := New(Options{})
	result, err := runner.runValidation(context.Background(), ValidationInput{
		CWD:      t.TempDir(),
		Commands: []string{"printf 'hello'", "printf 'warn' >&2"},
	})
	if err != nil {
		t.Fatalf("runValidation() error = %v", err)
	}
	if !result.Passed {
		t.Fatalf("result = %#v, want passed", result)
	}
	if result.Summary != "Validation passed" {
		t.Fatalf("Summary = %q, want Validation passed", result.Summary)
	}
	if result.Output != "hello\nwarn" {
		t.Fatalf("Output = %q, want joined shell output", result.Output)
	}
}

func TestRunValidationReturnsCommandFailureOutput(t *testing.T) {
	t.Parallel()
	runner := New(Options{})
	result, err := runner.runValidation(context.Background(), ValidationInput{
		CWD:      t.TempDir(),
		Commands: []string{"printf 'bad' >&2; exit 9"},
	})
	if err != nil {
		t.Fatalf("runValidation() error = %v", err)
	}
	if result.Passed {
		t.Fatalf("result = %#v, want failed validation", result)
	}
	if result.Summary != "Validation failed: printf 'bad' >&2; exit 9" {
		t.Fatalf("Summary = %q, want command-specific failure", result.Summary)
	}
	if result.Output != "bad" {
		t.Fatalf("Output = %q, want stderr output", result.Output)
	}
}

type runnerFixture struct {
	coordinator *storage.SQLiteCoordinator
	repos       *storage.Repositories
	logger      *testLogger
	current     time.Time
	now         func() time.Time
}

func newRunnerFixture(t *testing.T) *runnerFixture {
	t.Helper()
	coordinator, err := storage.OpenSQLiteCoordinator(context.Background(), filepath.Join(t.TempDir(), "worker.sqlite"), storage.SQLiteCoordinatorOptions{BackupDir: t.TempDir()})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	if _, err := coordinator.MigrationRunner().RunPending(context.Background()); err != nil {
		t.Fatalf("RunPending() error = %v", err)
	}
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 11, 12, 0, 0, 0, time.UTC)
	nowISO := fmt.Sprintf("%s.000Z", now.Format("2006-01-02T15:04:05"))
	baseBranch := "main"
	metadata := `{"repo":"acme/looper","worktreeRoot":"` + filepath.Join(t.TempDir(), "worktrees") + `"}`
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "project_1", Name: "Looper", RepoPath: filepath.Join(t.TempDir(), "repo"), BaseBranch: &baseBranch, MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	loopTarget := "project:project_1"
	loopMetadata := `{"worker":{"title":"Implement worker loop","prompt":"Do the thing","repo":"acme/looper","baseBranch":"main"}}`
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: "loop_worker_1", Seq: 1, ProjectID: "project_1", Type: "worker", TargetType: "project", TargetID: &loopTarget, Repo: stringPtr("acme/looper"), Status: "queued", MetadataJSON: &loopMetadata, NextRunAt: &nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	projectID := "project_1"
	loopID := "loop_worker_1"
	payload := `{"title":"Implement worker loop","prompt":"Do the thing","repo":"acme/looper","baseBranch":"main"}`
	if err := repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: "queue_worker_1", ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: "project:project_1", Repo: stringPtr("acme/looper"), DedupeKey: "worker:loop_worker_1", Priority: 1, Status: "queued", AvailableAt: nowISO, Attempts: 0, MaxAttempts: 3, PayloadJSON: &payload, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}
	fixture := &runnerFixture{coordinator: coordinator, repos: repos, logger: &testLogger{}, current: now}
	fixture.now = func() time.Time { return fixture.current }
	return fixture
}

func (f *runnerFixture) nowISO() string {
	return fmt.Sprintf("%s.000Z", f.current.UTC().Format("2006-01-02T15:04:05"))
}

func (f *runnerFixture) advance(delta time.Duration) { f.current = f.current.Add(delta) }

type fakeGitHubGateway struct {
	openPRs         []PullRequestSummary
	openPRResponses [][]PullRequestSummary
	openPRIndex     int
	prDetail        PullRequestDetail
	issueDetail     IssueDetail
	createPRResult  CreatePullRequestResult
	createPRErrors  []error
	createPRCalls   []CreatePullRequestInput
	removeLabels    []PullRequestLabelsInput
	reviewerCalls   []PullRequestReviewersInput
	createPRIndex   int
}

func (f *fakeGitHubGateway) ListOpenPullRequests(context.Context, ListOpenPullRequestsInput) ([]PullRequestSummary, error) {
	if f.openPRIndex < len(f.openPRResponses) {
		result := append([]PullRequestSummary(nil), f.openPRResponses[f.openPRIndex]...)
		f.openPRIndex++
		return result, nil
	}
	return append([]PullRequestSummary(nil), f.openPRs...), nil
}

func (f *fakeGitHubGateway) ViewPullRequest(_ context.Context, input ViewPullRequestInput) (PullRequestDetail, error) {
	detail := f.prDetail
	if detail.Number == 0 {
		detail.Number = input.PRNumber
	}
	if detail.BaseRefName == "" {
		detail.BaseRefName = "main"
	}
	if detail.HeadRefName == "" {
		detail.HeadRefName = fmt.Sprintf("pr-%d", input.PRNumber)
	}
	if detail.Title == "" {
		detail.Title = "Existing PR"
	}
	if detail.URL == "" {
		detail.URL = fmt.Sprintf("https://example/pr/%d", input.PRNumber)
	}
	return detail, nil
}

func (f *fakeGitHubGateway) ViewIssue(_ context.Context, input ViewIssueInput) (IssueDetail, error) {
	detail := f.issueDetail
	if detail.Number == 0 {
		detail.Number = input.IssueNumber
	}
	if detail.Title == "" {
		detail.Title = "Issue"
	}
	return detail, nil
}

func (f *fakeGitHubGateway) CreatePullRequest(_ context.Context, input CreatePullRequestInput) (CreatePullRequestResult, error) {
	f.createPRCalls = append(f.createPRCalls, input)
	if f.createPRIndex < len(f.createPRErrors) && f.createPRErrors[f.createPRIndex] != nil {
		err := f.createPRErrors[f.createPRIndex]
		f.createPRIndex++
		return CreatePullRequestResult{}, err
	}
	f.createPRIndex++
	return f.createPRResult, nil
}

func (f *fakeGitHubGateway) RemovePullRequestLabels(_ context.Context, input PullRequestLabelsInput) error {
	f.removeLabels = append(f.removeLabels, input)
	return nil
}

func (f *fakeGitHubGateway) AddPullRequestReviewers(_ context.Context, input PullRequestReviewersInput) error {
	f.reviewerCalls = append(f.reviewerCalls, input)
	return nil
}

type fakeGitGateway struct {
	createResult  CreateWorktreeResult
	prepareResult PrepareWorktreeResult
	createCalls   []CreateWorktreeInput
	pushCalls     []PushInput
	prepareCalls  []PrepareWorktreeInput
	pushErrors    []error
	pushIndex     int
}

func (f *fakeGitGateway) CreateWorktree(_ context.Context, input CreateWorktreeInput) (CreateWorktreeResult, error) {
	f.createCalls = append(f.createCalls, input)
	return f.createResult, nil
}

func (f *fakeGitGateway) PrepareWorktree(_ context.Context, input PrepareWorktreeInput) (PrepareWorktreeResult, error) {
	f.prepareCalls = append(f.prepareCalls, input)
	if f.prepareResult.HeadSHA == "" {
		return PrepareWorktreeResult{HeadSHA: "abc123", Clean: true}, nil
	}
	return f.prepareResult, nil
}

func (f *fakeGitGateway) Push(_ context.Context, input PushInput) error {
	f.pushCalls = append(f.pushCalls, input)
	if f.pushIndex < len(f.pushErrors) && f.pushErrors[f.pushIndex] != nil {
		err := f.pushErrors[f.pushIndex]
		f.pushIndex++
		return err
	}
	f.pushIndex++
	return nil
}

type fakeAgentExecutor struct {
	starts  []AgentRunInput
	results []AgentResult
	index   int
	waitErr error
	wait    func(context.Context) error
}

func (f *fakeAgentExecutor) Start(_ context.Context, input AgentRunInput) (AgentExecution, error) {
	f.starts = append(f.starts, input)
	result := AgentResult{Status: "completed", Summary: "done"}
	if f.index < len(f.results) {
		result = f.results[f.index]
	}
	f.index++
	return fakeAgentExecution{result: result, waitErr: f.waitErr, wait: f.wait}, nil
}

type fakeAgentExecution struct {
	result  AgentResult
	waitErr error
	wait    func(context.Context) error
}

func (f fakeAgentExecution) Wait(ctx context.Context) (AgentResult, error) {
	if f.wait != nil {
		if err := f.wait(ctx); err != nil {
			return AgentResult{}, err
		}
	}
	if f.waitErr != nil {
		return AgentResult{}, f.waitErr
	}
	return f.result, nil
}

type testLogger struct{}

func (*testLogger) Debug(string, map[string]any) {}
func (*testLogger) Info(string, map[string]any)  {}
func (*testLogger) Warn(string, map[string]any)  {}
func (*testLogger) Error(string, map[string]any) {}
