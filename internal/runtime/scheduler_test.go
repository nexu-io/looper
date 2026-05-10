package runtime

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/agent"
	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/fixer"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/planner"
	"github.com/nexu-io/looper/internal/reviewer"
	"github.com/nexu-io/looper/internal/storage"
	"github.com/nexu-io/looper/internal/sweeper"
	"github.com/nexu-io/looper/internal/worker"
)

func TestWorkerAgentExecutionAdapterPropagatesParseStatus(t *testing.T) {
	t.Parallel()

	adapter := workerAgentExecutionAdapter{execution: stubAgentExecution{result: agent.Result{Status: "completed", Summary: "done", Stdout: "ok", ParseStatus: "parsed", ChangedFiles: []string{"worker.go"}, Commits: []string{"abc123"}}}}

	result, err := adapter.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if result.ParseStatus != "parsed" {
		t.Fatalf("ParseStatus = %q, want parsed", result.ParseStatus)
	}
	if result.Status != "completed" || result.Summary != "done" || result.Stdout != "ok" || len(result.ChangedFiles) != 1 || result.ChangedFiles[0] != "worker.go" || len(result.Commits) != 1 || result.Commits[0] != "abc123" {
		t.Fatalf("result = %#v, want propagated agent result fields", result)
	}
}

func TestRunDefaultSchedulerTickDiscoversStoredProjectsAndProcessesQueue(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	backupDir := t.TempDir()
	coordinator := openMigratedCoordinator(t, filepath.Join(workingDir, "scheduler.sqlite"), backupDir)
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 21, 8, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	baseBranch := "main"
	projectMetadata := `{"repo":"nexu-io/looper"}`
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "looper", Name: "Looper", RepoPath: filepath.Join(workingDir, "repo"), BaseBranch: &baseBranch, MetadataJSON: &projectMetadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	projectTarget := "project:looper"
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: "loop_worker_1", Seq: 1, ProjectID: "looper", Type: "worker", TargetType: "project", TargetID: &projectTarget, Repo: stringPtr("nexu-io/looper"), Status: "queued", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	projectID := "looper"
	loopID := "loop_worker_1"
	if err := repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: "queue_worker_1", ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: projectTarget, Repo: stringPtr("nexu-io/looper"), DedupeKey: "worker:loop_worker_1", Priority: 1, Status: "queued", AvailableAt: nowISO, MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	plannerRunner := &stubPlannerScheduler{}
	reviewerRunner := &stubReviewerScheduler{}
	fixerRunner := &stubFixerScheduler{}
	workerRunner := &stubWorkerScheduler{}
	sweeperRunner := &stubSweeperScheduler{}

	err := runDefaultSchedulerTick(context.Background(), defaultSchedulerTickInput{
		Repos:             repos,
		Now:               func() time.Time { return now },
		MaxConcurrentRuns: 1,
		Planner:           plannerRunner,
		Reviewer:          reviewerRunner,
		Fixer:             fixerRunner,
		Worker:            workerRunner,
		Sweeper:           sweeperRunner,
	})
	if err != nil {
		t.Fatalf("runDefaultSchedulerTick() error = %v", err)
	}
	if len(plannerRunner.discoverCalls) != 1 || plannerRunner.discoverCalls[0].ProjectID != "looper" || plannerRunner.discoverCalls[0].Repo != "nexu-io/looper" {
		t.Fatalf("planner discover calls = %#v, want stored project discovery", plannerRunner.discoverCalls)
	}
	if len(reviewerRunner.discoverCalls) != 1 || reviewerRunner.discoverCalls[0].Repo != "nexu-io/looper" {
		t.Fatalf("reviewer discover calls = %#v, want stored project repo", reviewerRunner.discoverCalls)
	}
	if len(fixerRunner.discoverCalls) != 1 || fixerRunner.discoverCalls[0].Repo != "nexu-io/looper" {
		t.Fatalf("fixer discover calls = %#v, want stored project repo", fixerRunner.discoverCalls)
	}
	if len(workerRunner.discoverCalls) != 1 || workerRunner.discoverCalls[0].Repo != "nexu-io/looper" {
		t.Fatalf("worker discover calls = %#v, want stored project repo", workerRunner.discoverCalls)
	}
	if len(sweeperRunner.issueDiscoverCalls) != 1 || sweeperRunner.issueDiscoverCalls[0].Repo != "nexu-io/looper" {
		t.Fatalf("sweeper issue discovery calls = %#v, want stored project repo", sweeperRunner.issueDiscoverCalls)
	}
	if len(sweeperRunner.pullRequestDiscoverCalls) != 1 || sweeperRunner.pullRequestDiscoverCalls[0].Repo != "nexu-io/looper" {
		t.Fatalf("sweeper pull request discovery calls = %#v, want stored project repo", sweeperRunner.pullRequestDiscoverCalls)
	}
	if len(sweeperRunner.reconcileDiscoverCalls) != 1 || sweeperRunner.reconcileDiscoverCalls[0].Repo != "nexu-io/looper" {
		t.Fatalf("sweeper reconcile discovery calls = %#v, want stored project repo", sweeperRunner.reconcileDiscoverCalls)
	}
	waitForSchedulerCondition(t, func() bool {
		return workerRunner.processItemCount() == 1
	})
	if workerRunner.processItemCount() != 1 {
		t.Fatalf("worker processed items = %#v, want one queued worker run", workerRunner.processedItems)
	}
}

func TestRunDefaultSchedulerTickClaimsQueuedWorkBeforeDiscovery(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	backupDir := t.TempDir()
	coordinator := openMigratedCoordinator(t, filepath.Join(workingDir, "scheduler-claim-first.sqlite"), backupDir)
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 21, 8, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	baseBranch := "main"
	projectMetadata := `{"repo":"nexu-io/looper"}`
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "looper", Name: "Looper", RepoPath: filepath.Join(workingDir, "repo"), BaseBranch: &baseBranch, MetadataJSON: &projectMetadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	projectID := "looper"
	loopTarget := "project:looper"
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: "loop_worker_claim_first", Seq: 1, ProjectID: projectID, Type: "worker", TargetType: "project", TargetID: &loopTarget, Repo: stringPtr("nexu-io/looper"), Status: "queued", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	loopID := "loop_worker_claim_first"
	if err := repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: "queue_worker_claim_first", ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: loopTarget, Repo: stringPtr("nexu-io/looper"), DedupeKey: "worker:loop_worker_claim_first", Priority: 1, Status: "queued", AvailableAt: nowISO, MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	plannerRunner := &queueStatusCheckingPlannerScheduler{t: t, repos: repos, queueItemID: "queue_worker_claim_first"}

	if err := runDefaultSchedulerTick(context.Background(), defaultSchedulerTickInput{
		Repos:             repos,
		Now:               func() time.Time { return now },
		MaxConcurrentRuns: 1,
		Planner:           plannerRunner,
		Worker:            &stubWorkerScheduler{},
	}); err != nil {
		t.Fatalf("runDefaultSchedulerTick() error = %v", err)
	}
	if !plannerRunner.checkedClaimedStatus {
		t.Fatal("planner discovery did not verify claimed queue item status")
	}
}

func TestRunScheduledQueueItemsDispatchesEachSupportedType(t *testing.T) {
	t.Parallel()

	queueItems := []storage.QueueItemRecord{{ID: "planner-item", Type: "planner"}, {ID: "reviewer-item", Type: "reviewer"}, {ID: "fixer-item", Type: "fixer"}, {ID: "worker-item", Type: "worker"}, {ID: "sweeper-warn-item", Type: "sweeper:warn"}, {ID: "sweeper-close-item", Type: "sweeper:close"}, {ID: "sweeper-reconcile-item", Type: "sweeper:reconcile"}}
	plannerRunner := &stubPlannerScheduler{}
	reviewerRunner := &stubReviewerScheduler{}
	fixerRunner := &stubFixerScheduler{}
	workerRunner := &stubWorkerScheduler{}
	sweeperRunner := &stubSweeperScheduler{}

	err := runScheduledQueueItems(context.Background(), queueItems, defaultSchedulerTickInput{
		Planner:  plannerRunner,
		Reviewer: reviewerRunner,
		Fixer:    fixerRunner,
		Worker:   workerRunner,
		Sweeper:  sweeperRunner,
	})
	if err != nil {
		t.Fatalf("runScheduledQueueItems() error = %v", err)
	}
	waitForSchedulerCondition(t, func() bool {
		return plannerRunner.processItemCount() == 1 && reviewerRunner.processItemCount() == 1 && fixerRunner.processItemCount() == 1 && workerRunner.processItemCount() == 1 && sweeperRunner.processItemCount() == 3
	})
	if plannerRunner.processItemCount() != 1 || reviewerRunner.processItemCount() != 1 || fixerRunner.processItemCount() != 1 || workerRunner.processItemCount() != 1 || sweeperRunner.processItemCount() != 3 {
		t.Fatalf("processed items = planner:%#v reviewer:%#v fixer:%#v worker:%#v sweeper:%#v, want planner/reviewer/fixer/worker once and sweeper three times", plannerRunner.processedItems, reviewerRunner.processedItems, fixerRunner.processedItems, workerRunner.processedItems, sweeperRunner.processedItems)
	}
}

func TestRunScheduledQueueItemsProcessesItemsConcurrently(t *testing.T) {
	t.Parallel()

	runner := &parallelWorkerScheduler{
		secondStarted: make(chan struct{}),
	}
	err := runScheduledQueueItems(context.Background(), []storage.QueueItemRecord{{Type: "worker"}, {Type: "worker"}}, defaultSchedulerTickInput{
		Worker: runner,
	})
	if err != nil {
		t.Fatalf("runScheduledQueueItems() error = %v", err)
	}
	waitForSchedulerCondition(t, func() bool {
		return atomic.LoadInt32(&runner.calls) == 2
	})
	if got := atomic.LoadInt32(&runner.calls); got != 2 {
		t.Fatalf("worker ProcessNext calls = %d, want 2", got)
	}
}

func TestRunScheduledQueueItemsReturnsBeforeClaimedRunsFinish(t *testing.T) {
	t.Parallel()

	runner := &blockingWorkerScheduler{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	returned := make(chan error, 1)
	go func() {
		returned <- runScheduledQueueItems(context.Background(), []storage.QueueItemRecord{{Type: "worker"}}, defaultSchedulerTickInput{Worker: runner})
	}()

	select {
	case <-runner.started:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("worker ProcessNext did not start")
	}

	select {
	case err := <-returned:
		if err != nil {
			t.Fatalf("runScheduledQueueItems() error = %v", err)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("runScheduledQueueItems() did not return before claimed run finished")
	}

	close(runner.release)
}

func TestClaimAndRunScheduledQueueItemsBackfillsAvailableSlots(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	backupDir := t.TempDir()
	coordinator := openMigratedCoordinator(t, filepath.Join(workingDir, "backfill.sqlite"), backupDir)
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 21, 8, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "project_sched", Name: "Looper", RepoPath: filepath.Join(workingDir, "repo"), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: "loop_active", Seq: 1, ProjectID: "project_sched", Type: "worker", TargetType: "project", Status: "running", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	loopID := "loop_active"
	for _, item := range []storage.QueueItemRecord{
		{ID: "worker_locked_1", LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: "project_sched", DedupeKey: "d_worker_locked_1", Priority: 1, Status: "queued", AvailableAt: nowISO, MaxAttempts: 3, LockKey: stringPtr("repo:acme/looper"), CreatedAt: "2026-04-21T07:40:00.000Z", UpdatedAt: nowISO},
		{ID: "worker_locked_2", LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: "project_sched", DedupeKey: "d_worker_locked_2", Priority: 2, Status: "queued", AvailableAt: nowISO, MaxAttempts: 3, LockKey: stringPtr("repo:acme/looper"), CreatedAt: "2026-04-21T07:41:00.000Z", UpdatedAt: nowISO},
		{ID: "worker_fallback", LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: "project_sched", DedupeKey: "d_worker_fallback", Priority: 3, Status: "queued", AvailableAt: nowISO, MaxAttempts: 3, CreatedAt: "2026-04-21T07:42:00.000Z", UpdatedAt: nowISO},
	} {
		if err := repos.Queue.Upsert(context.Background(), item); err != nil {
			t.Fatalf("Queue.Upsert(%s) error = %v", item.ID, err)
		}
	}

	workerRunner := &stubWorkerScheduler{}
	if err := claimAndRunScheduledQueueItems(context.Background(), 2, defaultSchedulerTickInput{
		Repos:  repos,
		Now:    func() time.Time { return now },
		Worker: workerRunner,
	}); err != nil {
		t.Fatalf("claimAndRunScheduledQueueItems() error = %v", err)
	}
	waitForSchedulerCondition(t, func() bool {
		return workerRunner.processItemCount() == 2
	})
	if workerRunner.processItemCount() != 2 {
		t.Fatalf("worker processed items = %#v, want worker_locked_1 and worker_fallback", workerRunner.processedItems)
	}
}

func TestRunScheduledQueueItemsRejectsUnsupportedType(t *testing.T) {
	t.Parallel()

	err := runScheduledQueueItems(context.Background(), []storage.QueueItemRecord{{Type: "mystery"}}, defaultSchedulerTickInput{})
	if err == nil || !strings.Contains(err.Error(), "unsupported queue item type") {
		t.Fatalf("runScheduledQueueItems() error = %v, want unsupported queue item type", err)
	}
}

func TestRunScheduledQueueItemsErrorsWhenRunnerMissing(t *testing.T) {
	t.Parallel()

	err := runScheduledQueueItems(context.Background(), []storage.QueueItemRecord{{Type: "worker"}}, defaultSchedulerTickInput{})
	if err == nil || !strings.Contains(err.Error(), "worker runner is not configured") {
		t.Fatalf("runScheduledQueueItems() error = %v, want missing worker runner error", err)
	}
}

func TestRunScheduledQueueItemsErrorsWhenSweeperRunnerMissing(t *testing.T) {
	t.Parallel()

	err := runScheduledQueueItems(context.Background(), []storage.QueueItemRecord{{Type: "sweeper:warn"}}, defaultSchedulerTickInput{})
	if err == nil || !strings.Contains(err.Error(), "sweeper runner is not configured") {
		t.Fatalf("runScheduledQueueItems() error = %v, want missing sweeper runner error", err)
	}
}

func TestProcessSnapshotQueueItemRetriesTransientCaptureFailure(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	backupDir := t.TempDir()
	coordinator := openMigratedCoordinator(t, filepath.Join(workingDir, "snapshot-retry.sqlite"), backupDir)
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 21, 8, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	projectID := "looper"
	repo := "nexu-io/looper"
	prNumber := int64(109)
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Looper", RepoPath: filepath.Join(workingDir, "repo"), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	queueItem := storage.QueueItemRecord{ID: "queue_snapshot_1", ProjectID: &projectID, Type: "snapshot", TargetType: "pull_request", TargetID: "nexu-io/looper#109", Repo: &repo, PRNumber: &prNumber, DedupeKey: "snapshot:nexu-io/looper:109", Priority: storage.QueuePriorityReviewer, Status: "running", AvailableAt: nowISO, Attempts: 0, MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := repos.Queue.Upsert(context.Background(), queueItem); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	err := processSnapshotQueueItem(context.Background(), queueItem, defaultSchedulerTickInput{
		Repos:       repos,
		Now:         func() time.Time { return now },
		Snapshotter: stubSnapshotScheduler{err: errors.New("gh timeout")},
	})
	if err != nil {
		t.Fatalf("processSnapshotQueueItem() error = %v", err)
	}
	updated, err := repos.Queue.GetByID(context.Background(), queueItem.ID)
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	if updated == nil {
		t.Fatal("Queue.GetByID() = nil, want retried queue item")
	}
	if updated.Status != "queued" || updated.Attempts != 1 || updated.FinishedAt != nil {
		t.Fatalf("queue item = %#v, want queued retry with one attempt and no finished_at", updated)
	}
	if updated.LastError == nil || *updated.LastError != "gh timeout" || updated.LastErrorKind == nil || *updated.LastErrorKind != "retryable_transient" {
		t.Fatalf("queue item error = (%v, %v), want retryable gh timeout", updated.LastError, updated.LastErrorKind)
	}
}

func TestProcessSnapshotQueueItemRetriesTransientPersistenceFailure(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	backupDir := t.TempDir()
	coordinator := openMigratedCoordinator(t, filepath.Join(workingDir, "snapshot-upsert-retry.sqlite"), backupDir)
	repos := storage.NewRepositories(&failingSnapshotUpsertQuerier{db: coordinator.DB(), err: errors.New("database is locked")})
	now := time.Date(2026, time.April, 21, 8, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	projectID := "looper"
	repo := "nexu-io/looper"
	prNumber := int64(109)
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Looper", RepoPath: filepath.Join(workingDir, "repo"), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	queueItem := storage.QueueItemRecord{ID: "queue_snapshot_upsert_1", ProjectID: &projectID, Type: "snapshot", TargetType: "pull_request", TargetID: "nexu-io/looper#109", Repo: &repo, PRNumber: &prNumber, DedupeKey: "snapshot:nexu-io/looper:109", Priority: storage.QueuePriorityReviewer, Status: "running", AvailableAt: nowISO, Attempts: 0, MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := repos.Queue.Upsert(context.Background(), queueItem); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	err := processSnapshotQueueItem(context.Background(), queueItem, defaultSchedulerTickInput{
		Repos:       repos,
		Now:         func() time.Time { return now },
		Snapshotter: stubSnapshotScheduler{},
	})
	if err != nil {
		t.Fatalf("processSnapshotQueueItem() error = %v", err)
	}
	updated, err := repos.Queue.GetByID(context.Background(), queueItem.ID)
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	if updated == nil {
		t.Fatal("Queue.GetByID() = nil, want retried queue item")
	}
	if updated.Status != "queued" || updated.Attempts != 1 || updated.FinishedAt != nil {
		t.Fatalf("queue item = %#v, want queued retry with one attempt and no finished_at", updated)
	}
	if updated.LastError == nil || !strings.Contains(*updated.LastError, "database is locked") || updated.LastErrorKind == nil || *updated.LastErrorKind != "retryable_transient" {
		t.Fatalf("queue item error = (%v, %v), want retryable database is locked", updated.LastError, updated.LastErrorKind)
	}
}

func TestProcessSnapshotQueueItemRetriesTransientProjectLookupFailure(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	backupDir := t.TempDir()
	coordinator := openMigratedCoordinator(t, filepath.Join(workingDir, "snapshot-project-lookup-retry.sqlite"), backupDir)
	setupRepos := storage.NewRepositories(coordinator.DB())
	repos := storage.NewRepositories(&failingProjectLookupQuerier{db: coordinator.DB()})
	now := time.Date(2026, time.April, 21, 8, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	projectID := "looper"
	repo := "nexu-io/looper"
	prNumber := int64(109)
	if err := setupRepos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Looper", RepoPath: filepath.Join(workingDir, "repo"), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	queueItem := storage.QueueItemRecord{ID: "queue_snapshot_project_lookup_1", ProjectID: &projectID, Type: "snapshot", TargetType: "pull_request", TargetID: "nexu-io/looper#109", Repo: &repo, PRNumber: &prNumber, DedupeKey: "snapshot:nexu-io/looper:109", Priority: storage.QueuePriorityReviewer, Status: "running", AvailableAt: nowISO, Attempts: 0, MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := setupRepos.Queue.Upsert(context.Background(), queueItem); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	err := processSnapshotQueueItem(context.Background(), queueItem, defaultSchedulerTickInput{
		Repos:       repos,
		Now:         func() time.Time { return now },
		Snapshotter: stubSnapshotScheduler{},
	})
	if err != nil {
		t.Fatalf("processSnapshotQueueItem() error = %v", err)
	}
	updated, err := repos.Queue.GetByID(context.Background(), queueItem.ID)
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	if updated == nil {
		t.Fatal("Queue.GetByID() = nil, want retried queue item")
	}
	if updated.Status != "queued" || updated.Attempts != 1 || updated.FinishedAt != nil {
		t.Fatalf("queue item = %#v, want queued retry with one attempt and no finished_at", updated)
	}
	if updated.LastError == nil || !strings.Contains(*updated.LastError, "get project by id") || updated.LastErrorKind == nil || *updated.LastErrorKind != "retryable_transient" {
		t.Fatalf("queue item error = (%v, %v), want retryable project lookup failure", updated.LastError, updated.LastErrorKind)
	}
}

func TestProcessSnapshotQueueItemRetriesTransientCompletionFailure(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	backupDir := t.TempDir()
	coordinator := openMigratedCoordinator(t, filepath.Join(workingDir, "snapshot-complete-retry.sqlite"), backupDir)
	setupRepos := storage.NewRepositories(coordinator.DB())
	repos := storage.NewRepositories(&failingQueueCompleteQuerier{db: coordinator.DB(), err: errors.New("database is locked")})
	now := time.Date(2026, time.April, 21, 8, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	projectID := "looper"
	repo := "nexu-io/looper"
	prNumber := int64(109)
	if err := setupRepos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Looper", RepoPath: filepath.Join(workingDir, "repo"), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	queueItem := storage.QueueItemRecord{ID: "queue_snapshot_complete_1", ProjectID: &projectID, Type: "snapshot", TargetType: "pull_request", TargetID: "nexu-io/looper#109", Repo: &repo, PRNumber: &prNumber, DedupeKey: "snapshot:nexu-io/looper:109", Priority: storage.QueuePriorityReviewer, Status: "running", AvailableAt: nowISO, Attempts: 0, MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := setupRepos.Queue.Upsert(context.Background(), queueItem); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	err := processSnapshotQueueItem(context.Background(), queueItem, defaultSchedulerTickInput{
		Repos:       repos,
		Now:         func() time.Time { return now },
		Snapshotter: stubSnapshotScheduler{},
	})
	if err != nil {
		t.Fatalf("processSnapshotQueueItem() error = %v", err)
	}
	updated, err := repos.Queue.GetByID(context.Background(), queueItem.ID)
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	if updated == nil {
		t.Fatal("Queue.GetByID() = nil, want retried queue item")
	}
	if updated.Status != "queued" || updated.Attempts != 1 || updated.FinishedAt != nil {
		t.Fatalf("queue item = %#v, want queued retry with one attempt and no finished_at", updated)
	}
	assertQueueRetryError(t, updated, "database is locked")
}

func TestSchedulerAvailableSlotsAccountsForRunningQueueItems(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	backupDir := t.TempDir()
	coordinator := openMigratedCoordinator(t, filepath.Join(workingDir, "slots.sqlite"), backupDir)
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 21, 8, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	baseBranch := "main"
	projectID := "looper"
	loopID := "loop_worker_running"
	projectMetadata := `{"repo":"nexu-io/looper"}`
	projectTarget := "project:looper"
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Looper", RepoPath: filepath.Join(workingDir, "repo"), BaseBranch: &baseBranch, MetadataJSON: &projectMetadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 1, ProjectID: projectID, Type: "worker", TargetType: "project", TargetID: &projectTarget, Repo: stringPtr("nexu-io/looper"), Status: "running", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	if err := repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: "queue_running", ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: "project:looper", DedupeKey: "worker:running", Priority: 1, Status: "running", AvailableAt: nowISO, MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Queue.Upsert(running) error = %v", err)
	}

	available, err := schedulerAvailableSlots(context.Background(), repos, 1)
	if err != nil {
		t.Fatalf("schedulerAvailableSlots() error = %v", err)
	}
	if available != 0 {
		t.Fatalf("schedulerAvailableSlots() = %d, want 0", available)
	}
}

func TestRunDefaultSchedulerTickContinuesAfterDiscoveryError(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	backupDir := t.TempDir()
	coordinator := openMigratedCoordinator(t, filepath.Join(workingDir, "errors.sqlite"), backupDir)
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 21, 8, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	baseBranch := "main"
	projectMetadata := `{"repo":"nexu-io/looper"}`
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "looper", Name: "Looper", RepoPath: filepath.Join(workingDir, "repo"), BaseBranch: &baseBranch, MetadataJSON: &projectMetadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	projectTarget := "project:looper"
	projectID := "looper"
	loopID := "loop_worker_1"
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 1, ProjectID: projectID, Type: "worker", TargetType: "project", TargetID: &projectTarget, Repo: stringPtr("nexu-io/looper"), Status: "queued", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	if err := repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: "queue_worker_1", ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: projectTarget, Repo: stringPtr("nexu-io/looper"), DedupeKey: "worker:loop_worker_1", Priority: 1, Status: "queued", AvailableAt: nowISO, MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	plannerRunner := &stubPlannerScheduler{discoverErr: errors.New("planner boom")}
	workerRunner := &stubWorkerScheduler{}
	err := runDefaultSchedulerTick(context.Background(), defaultSchedulerTickInput{
		Repos:             repos,
		Now:               func() time.Time { return now },
		MaxConcurrentRuns: 1,
		Planner:           plannerRunner,
		Worker:            workerRunner,
	})
	if err == nil || !strings.Contains(err.Error(), "planner discovery failed") {
		t.Fatalf("runDefaultSchedulerTick() error = %v, want joined discovery error", err)
	}
	waitForSchedulerCondition(t, func() bool {
		return workerRunner.processItemCount() == 1
	})
	if workerRunner.processItemCount() != 1 {
		t.Fatalf("worker processed items = %#v, want queue processing to continue", workerRunner.processedItems)
	}
}

func TestGithubCLIAutoPROpeningAvailableRechecksAuthenticatedCLIWithoutConfiguredPath(t *testing.T) {
	t.Parallel()

	rootDir := t.TempDir()
	authenticatedPath := filepath.Join(rootDir, "gh-authenticated")
	writeExecutable(t, authenticatedPath, `#!/bin/sh
case "$*" in
  "auth status --hostname github.com")
    exit 0
    ;;
  *)
    printf '{}'
    ;;
esac
`)
	unauthenticatedPath := filepath.Join(rootDir, "gh-unauthenticated")
	writeExecutable(t, unauthenticatedPath, `#!/bin/sh
case "$*" in
  "auth status --hostname github.com")
    exit 1
    ;;
  *)
    printf '{}'
    ;;
esac
`)

	logger := &testLogger{}
	authenticatedGateway := githubinfra.New(githubinfra.Options{GHPath: authenticatedPath, CWD: rootDir})
	unauthenticatedGateway := githubinfra.New(githubinfra.Options{GHPath: unauthenticatedPath, CWD: rootDir})

	if !githubCLIAutoPROpeningAvailable(context.Background(), config.Config{Tools: config.ToolPathsConfig{GHPath: &authenticatedPath}}, authenticatedGateway, logger, "nexu-io/looper", rootDir) {
		t.Fatal("githubCLIAutoPROpeningAvailable() = false, want true for authenticated gh cli")
	}
	if githubCLIAutoPROpeningAvailable(context.Background(), config.Config{Tools: config.ToolPathsConfig{GHPath: &unauthenticatedPath}}, unauthenticatedGateway, logger, "nexu-io/looper", rootDir) {
		t.Fatal("githubCLIAutoPROpeningAvailable() = true, want false for unauthenticated gh cli")
	}
	if !githubCLIAutoPROpeningAvailable(context.Background(), config.Config{}, authenticatedGateway, logger, "nexu-io/looper", rootDir) {
		t.Fatal("githubCLIAutoPROpeningAvailable() = false, want true when gateway can recheck authenticated gh cli without configured path")
	}
}

func TestGithubCLIAutoPROpeningAvailableUsesRepoHostname(t *testing.T) {
	t.Parallel()

	rootDir := t.TempDir()
	logPath := filepath.Join(rootDir, "gh.log")
	scriptPath := filepath.Join(rootDir, "gh")
	writeExecutable(t, scriptPath, fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >> %q
case "$*" in
  "auth status --hostname github.example.com")
    exit 0
    ;;
  *)
    exit 1
    ;;
esac
`, logPath))

	logger := &testLogger{}
	gateway := githubinfra.New(githubinfra.Options{GHPath: scriptPath, CWD: rootDir})
	if !githubCLIAutoPROpeningAvailable(context.Background(), config.Config{Tools: config.ToolPathsConfig{GHPath: &scriptPath}}, gateway, logger, "github.example.com/nexu-io/looper", rootDir) {
		t.Fatal("githubCLIAutoPROpeningAvailable() = false, want true for repo host auth")
	}
	logBytes, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if log := string(logBytes); !strings.Contains(log, "auth status --hostname github.example.com") {
		t.Fatalf("gh log = %q, want repo hostname auth status", log)
	}
}

type stubPlannerScheduler struct {
	mu             sync.Mutex
	discoverCalls  []planner.DiscoveryInput
	processClaims  []string
	processedItems []string
	discoverErr    error
	processErr     error
}

type queueStatusCheckingPlannerScheduler struct {
	t                    *testing.T
	repos                *storage.Repositories
	queueItemID          string
	checkedClaimedStatus bool
}

func (s *queueStatusCheckingPlannerScheduler) DiscoverIssues(ctx context.Context, _ planner.DiscoveryInput) (planner.DiscoveryResult, error) {
	s.t.Helper()
	item, err := s.repos.Queue.GetByID(ctx, s.queueItemID)
	if err != nil {
		s.t.Fatalf("Queue.GetByID() error = %v", err)
	}
	if item == nil {
		s.t.Fatal("Queue.GetByID() = nil, want claimed queue item")
	}
	if item.Status != "running" {
		s.t.Fatalf("queue item status during discovery = %q, want running", item.Status)
	}
	if item.ClaimedBy == nil || *item.ClaimedBy != "scheduler" {
		s.t.Fatalf("queue item claimed_by during discovery = %v, want scheduler", item.ClaimedBy)
	}
	s.checkedClaimedStatus = true
	return planner.DiscoveryResult{}, nil
}

func (s *queueStatusCheckingPlannerScheduler) ProcessNext(context.Context, string) (*planner.ProcessResult, error) {
	return nil, nil
}

func (s *queueStatusCheckingPlannerScheduler) ProcessClaimedQueueItem(context.Context, storage.QueueItemRecord) (*planner.ProcessResult, error) {
	return nil, nil

}

func (s *stubPlannerScheduler) DiscoverIssues(_ context.Context, input planner.DiscoveryInput) (planner.DiscoveryResult, error) {
	s.mu.Lock()
	s.discoverCalls = append(s.discoverCalls, input)
	s.mu.Unlock()
	return planner.DiscoveryResult{}, s.discoverErr
}

func (s *stubPlannerScheduler) ProcessNext(_ context.Context, claimedBy string) (*planner.ProcessResult, error) {
	s.mu.Lock()
	s.processClaims = append(s.processClaims, claimedBy)
	s.mu.Unlock()
	return nil, s.processErr
}

func (s *stubPlannerScheduler) ProcessClaimedQueueItem(_ context.Context, queueItem storage.QueueItemRecord) (*planner.ProcessResult, error) {
	s.mu.Lock()
	s.processedItems = append(s.processedItems, queueItem.ID)
	s.mu.Unlock()
	return &planner.ProcessResult{}, s.processErr
}

func (s *stubPlannerScheduler) processClaimCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.processClaims)
}

func (s *stubPlannerScheduler) processItemCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.processedItems)
}

type stubReviewerScheduler struct {
	mu             sync.Mutex
	discoverCalls  []reviewer.DiscoveryInput
	processClaims  []string
	processedItems []string
	discoverErr    error
	processErr     error
}

func (s *stubReviewerScheduler) DiscoverPullRequests(_ context.Context, input reviewer.DiscoveryInput) (reviewer.DiscoveryResult, error) {
	s.mu.Lock()
	s.discoverCalls = append(s.discoverCalls, input)
	s.mu.Unlock()
	return reviewer.DiscoveryResult{}, s.discoverErr
}

func (s *stubReviewerScheduler) ProcessNext(_ context.Context, claimedBy string) (*reviewer.ProcessResult, error) {
	s.mu.Lock()
	s.processClaims = append(s.processClaims, claimedBy)
	s.mu.Unlock()
	return nil, s.processErr
}

func (s *stubReviewerScheduler) ProcessClaimedQueueItem(_ context.Context, queueItem storage.QueueItemRecord) (*reviewer.ProcessResult, error) {
	s.mu.Lock()
	s.processedItems = append(s.processedItems, queueItem.ID)
	s.mu.Unlock()
	return &reviewer.ProcessResult{}, s.processErr
}

func (s *stubReviewerScheduler) processClaimCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.processClaims)
}

func (s *stubReviewerScheduler) processItemCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.processedItems)
}

type stubFixerScheduler struct {
	mu             sync.Mutex
	discoverCalls  []fixer.DiscoveryInput
	processClaims  []string
	processedItems []string
	discoverErr    error
	processErr     error
}

func (s *stubFixerScheduler) DiscoverPullRequests(_ context.Context, input fixer.DiscoveryInput) (fixer.DiscoveryResult, error) {
	s.mu.Lock()
	s.discoverCalls = append(s.discoverCalls, input)
	s.mu.Unlock()
	return fixer.DiscoveryResult{}, s.discoverErr
}

func (s *stubFixerScheduler) ProcessNext(_ context.Context, claimedBy string) (*fixer.ProcessResult, error) {
	s.mu.Lock()
	s.processClaims = append(s.processClaims, claimedBy)
	s.mu.Unlock()
	return nil, s.processErr
}

func (s *stubFixerScheduler) ProcessClaimedQueueItem(_ context.Context, queueItem storage.QueueItemRecord) (*fixer.ProcessResult, error) {
	s.mu.Lock()
	s.processedItems = append(s.processedItems, queueItem.ID)
	s.mu.Unlock()
	return &fixer.ProcessResult{}, s.processErr
}

func (s *stubFixerScheduler) processClaimCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.processClaims)
}

func (s *stubFixerScheduler) processItemCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.processedItems)
}

type stubWorkerScheduler struct {
	mu             sync.Mutex
	discoverCalls  []worker.DiscoveryInput
	discoverErr    error
	processClaims  []string
	processedItems []string
	processErr     error
}

func (s *stubWorkerScheduler) DiscoverIssues(_ context.Context, input worker.DiscoveryInput) (worker.DiscoveryResult, error) {
	s.mu.Lock()
	s.discoverCalls = append(s.discoverCalls, input)
	s.mu.Unlock()
	return worker.DiscoveryResult{}, s.discoverErr
}

func (s *stubWorkerScheduler) ProcessNext(_ context.Context, claimedBy string) (*worker.ProcessResult, error) {
	s.mu.Lock()
	s.processClaims = append(s.processClaims, claimedBy)
	s.mu.Unlock()
	return nil, s.processErr
}

func (s *stubWorkerScheduler) ProcessClaimedQueueItem(_ context.Context, queueItem storage.QueueItemRecord) (*worker.ProcessResult, error) {
	s.mu.Lock()
	s.processedItems = append(s.processedItems, queueItem.ID)
	s.mu.Unlock()
	return &worker.ProcessResult{}, s.processErr
}

func (s *stubWorkerScheduler) processClaimCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.processClaims)
}

func (s *stubWorkerScheduler) processItemCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.processedItems)
}

type stubSweeperDiscoveryCall struct {
	ProjectID string
	Repo      string
}

type stubSweeperScheduler struct {
	mu                       sync.Mutex
	issueDiscoverCalls       []stubSweeperDiscoveryCall
	pullRequestDiscoverCalls []stubSweeperDiscoveryCall
	reconcileDiscoverCalls   []stubSweeperDiscoveryCall
	processedItems           []string
	discoverErr              error
	processErr               error
}

func (s *stubSweeperScheduler) DiscoverIssues(_ context.Context, input sweeper.DiscoveryInput) (sweeper.DiscoveryResult, error) {
	s.mu.Lock()
	s.issueDiscoverCalls = append(s.issueDiscoverCalls, stubSweeperDiscoveryCall{ProjectID: input.ProjectID, Repo: input.Repo})
	s.mu.Unlock()
	return sweeper.DiscoveryResult{}, s.discoverErr
}

func (s *stubSweeperScheduler) DiscoverPullRequests(_ context.Context, input sweeper.DiscoveryInput) (sweeper.DiscoveryResult, error) {
	s.mu.Lock()
	s.pullRequestDiscoverCalls = append(s.pullRequestDiscoverCalls, stubSweeperDiscoveryCall{ProjectID: input.ProjectID, Repo: input.Repo})
	s.mu.Unlock()
	return sweeper.DiscoveryResult{}, s.discoverErr
}

func (s *stubSweeperScheduler) DiscoverReconcile(_ context.Context, input sweeper.DiscoveryInput) (sweeper.DiscoveryResult, error) {
	s.mu.Lock()
	s.reconcileDiscoverCalls = append(s.reconcileDiscoverCalls, stubSweeperDiscoveryCall{ProjectID: input.ProjectID, Repo: input.Repo})
	s.mu.Unlock()
	return sweeper.DiscoveryResult{}, s.discoverErr
}

func (s *stubSweeperScheduler) ProcessClaimedQueueItem(_ context.Context, queueItem storage.QueueItemRecord) (*sweeper.ProcessResult, error) {
	s.mu.Lock()
	s.processedItems = append(s.processedItems, queueItem.ID)
	s.mu.Unlock()
	return &sweeper.ProcessResult{QueueItemID: queueItem.ID, Status: "skipped"}, s.processErr
}

func (s *stubSweeperScheduler) processItemCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.processedItems)
}

type parallelWorkerScheduler struct {
	calls         int32
	secondStarted chan struct{}
}

func (s *parallelWorkerScheduler) ProcessNext(_ context.Context, _ string) (*worker.ProcessResult, error) {
	switch atomic.AddInt32(&s.calls, 1) {
	case 1:
		select {
		case <-s.secondStarted:
			return nil, nil
		case <-time.After(250 * time.Millisecond):
			return nil, errors.New("second worker item did not start concurrently")
		}
	case 2:
		close(s.secondStarted)
	}
	return nil, nil
}

func (s *parallelWorkerScheduler) ProcessClaimedQueueItem(ctx context.Context, _ storage.QueueItemRecord) (*worker.ProcessResult, error) {
	return s.ProcessNext(ctx, "")
}

type blockingWorkerScheduler struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *blockingWorkerScheduler) ProcessNext(_ context.Context, _ string) (*worker.ProcessResult, error) {
	s.once.Do(func() {
		close(s.started)
	})
	<-s.release
	return nil, nil
}

func (s *blockingWorkerScheduler) ProcessClaimedQueueItem(ctx context.Context, _ storage.QueueItemRecord) (*worker.ProcessResult, error) {
	return s.ProcessNext(ctx, "")
}

type stubSnapshotScheduler struct {
	err error
}

func (s stubSnapshotScheduler) CapturePullRequestSnapshot(_ context.Context, input githubinfra.CapturePullRequestSnapshotInput) (storage.PullRequestSnapshotRecord, error) {
	return storage.PullRequestSnapshotRecord{ID: "snapshot_1", ProjectID: input.ProjectID, Repo: input.Repo, PRNumber: input.PRNumber, HeadSHA: "head", CapturedAt: input.CapturedAt, CreatedAt: input.CapturedAt}, s.err
}

type failingSnapshotUpsertQuerier struct {
	db  *sql.DB
	err error
}

func (q *failingSnapshotUpsertQuerier) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if strings.Contains(query, "INSERT INTO pull_request_snapshots") {
		return nil, q.err
	}
	return q.db.ExecContext(ctx, query, args...)
}

func (q *failingSnapshotUpsertQuerier) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return q.db.QueryContext(ctx, query, args...)
}

func (q *failingSnapshotUpsertQuerier) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return q.db.QueryRowContext(ctx, query, args...)
}

type failingProjectLookupQuerier struct {
	db *sql.DB
}

func (q *failingProjectLookupQuerier) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return q.db.ExecContext(ctx, query, args...)
}

func (q *failingProjectLookupQuerier) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return q.db.QueryContext(ctx, query, args...)
}

func (q *failingProjectLookupQuerier) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	if strings.Contains(query, "FROM projects WHERE id") {
		return q.db.QueryRowContext(ctx, `SELECT * FROM missing_projects_table`)
	}
	return q.db.QueryRowContext(ctx, query, args...)
}

type failingQueueCompleteQuerier struct {
	db  *sql.DB
	err error
}

func (q *failingQueueCompleteQuerier) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if strings.Contains(query, "SET status = 'completed'") {
		return nil, q.err
	}
	return q.db.ExecContext(ctx, query, args...)
}

func (q *failingQueueCompleteQuerier) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return q.db.QueryContext(ctx, query, args...)
}

func (q *failingQueueCompleteQuerier) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return q.db.QueryRowContext(ctx, query, args...)
}

func assertQueueRetryError(t *testing.T, item *storage.QueueItemRecord, message string) {
	t.Helper()
	lastError := "<nil>"
	if item.LastError != nil {
		lastError = *item.LastError
	}
	lastErrorKind := "<nil>"
	if item.LastErrorKind != nil {
		lastErrorKind = *item.LastErrorKind
	}
	if item.LastError == nil || !strings.Contains(*item.LastError, message) || item.LastErrorKind == nil || *item.LastErrorKind != "retryable_transient" {
		t.Fatalf("queue item error = (%s, %s), want retryable %s", lastError, lastErrorKind, message)
	}
}

func waitForSchedulerCondition(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(250 * time.Millisecond)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !condition() {
		t.Fatal("condition not satisfied before timeout")
	}
}

func writeExecutable(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o755); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", path, err)
	}
}

type stubAgentExecution struct {
	result agent.Result
	err    error
}

func (s stubAgentExecution) Wait(context.Context) (agent.Result, error) {
	return s.result, s.err
}

func (s stubAgentExecution) Kill(string) error {
	return nil
}
