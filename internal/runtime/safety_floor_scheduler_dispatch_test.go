package runtime

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
)

// Contract: when admission closes mid-batch after ClaimNext* already claimed
// earlier slots (maxConcurrentRuns > 1 during shutdown), already-claimed items
// must still be dispatched — not returned and stranded as running/claimed.
func TestSafetyFloorAdmissionCloseMidBatchProcessesClaimedItems(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	backupDir := filepath.Join(workingDir, "backups")
	cfg.Storage.BackupDir = &backupDir
	now := time.Date(2026, time.July, 17, 12, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)

	coordinator := openMigratedCoordinator(t, cfg.Storage.DBPath, backupDir)
	t.Cleanup(func() { _ = coordinator.Close() })
	repos := storage.NewRepositories(coordinator.DB())
	projectID := "project_mid_batch"
	loopID := "loop_mid_batch"
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Mid Batch", RepoPath: filepath.Join(workingDir, "repo"), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 8, ProjectID: projectID, Type: "worker", TargetType: "project", Status: "queued", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	for i, id := range []string{"queue_mid_batch_1", "queue_mid_batch_2"} {
		createdAt := formatJavaScriptISOString(now.Add(time.Duration(i) * time.Second))
		if err := repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{
			ID: id, ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: projectID,
			DedupeKey: "worker:project_mid_batch:" + id, Priority: storage.QueuePriorityWorker, Status: "queued",
			AvailableAt: nowISO, Attempts: 0, MaxAttempts: 3, CreatedAt: createdAt, UpdatedAt: nowISO,
		}); err != nil {
			t.Fatalf("Queue.Upsert(%s) error = %v", id, err)
		}
	}

	var allowCalls atomic.Int64
	worker := &stubWorkerScheduler{}
	claimed, err := claimAndRunScheduledQueueItems(context.Background(), 2, defaultSchedulerTickInput{
		Repos:       repos,
		Now:         func() time.Time { return now },
		Worker:      worker,
		AsyncRunner: immediateSchedulerRunner{},
		AllowClaim: func() error {
			// First call: allow claim of slot 0. Second call: admission stopping
			// before slot 1 — mid-batch close with one durable claim already held.
			if allowCalls.Add(1) == 1 {
				return nil
			}
			return ErrAdmissionStopping
		},
	})
	if err != nil {
		t.Fatalf("claimAndRunScheduledQueueItems() error = %v", err)
	}
	if len(claimed) != 1 || claimed[0].ID != "queue_mid_batch_1" {
		t.Fatalf("claimed = %#v, want only first item after mid-batch admission stop", claimed)
	}
	if worker.processItemCount() != 1 || worker.processedItems[0] != "queue_mid_batch_1" {
		t.Fatalf("processed = %#v, want first claimed item dispatched (not stranded)", worker.processedItems)
	}
	second, err := repos.Queue.GetByID(context.Background(), "queue_mid_batch_2")
	if err != nil {
		t.Fatalf("Queue.GetByID(second) error = %v", err)
	}
	if second == nil || second.Status != "queued" {
		t.Fatalf("second queue item = %#v, want still queued after admission refuse on later slot", second)
	}
}

// Contract: BeginShutdown cancels the scheduler context mid-batch after
// ClaimNext* already persisted a claim (maxConcurrentRuns > 1). Already-claimed
// items must still be dispatched — not returned with ctx.Err and stranded as
// running/claimed_by=scheduler with no processor launched.
func TestSafetyFloorSchedulerContextCancelMidBatchProcessesClaimedItems(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	backupDir := filepath.Join(workingDir, "backups")
	cfg.Storage.BackupDir = &backupDir
	now := time.Date(2026, time.July, 17, 12, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)

	coordinator := openMigratedCoordinator(t, cfg.Storage.DBPath, backupDir)
	t.Cleanup(func() { _ = coordinator.Close() })
	repos := storage.NewRepositories(coordinator.DB())
	projectID := "project_ctx_cancel_batch"
	loopID := "loop_ctx_cancel_batch"
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Ctx Cancel Batch", RepoPath: filepath.Join(workingDir, "repo"), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 9, ProjectID: projectID, Type: "worker", TargetType: "project", Status: "queued", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	for i, id := range []string{"queue_ctx_cancel_1", "queue_ctx_cancel_2"} {
		createdAt := formatJavaScriptISOString(now.Add(time.Duration(i) * time.Second))
		if err := repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{
			ID: id, ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: projectID,
			DedupeKey: "worker:project_ctx_cancel_batch:" + id, Priority: storage.QueuePriorityWorker, Status: "queued",
			AvailableAt: nowISO, Attempts: 0, MaxAttempts: 3, CreatedAt: createdAt, UpdatedAt: nowISO,
		}); err != nil {
			t.Fatalf("Queue.Upsert(%s) error = %v", id, err)
		}
	}

	// Cancel after the first durable claim so the next slot's ctx.Err() path
	// would previously return the claimed slice without runScheduledQueueItems.
	claimCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	var claimCalls atomic.Int64
	worker := &stubWorkerScheduler{}
	claimed, err := claimAndRunScheduledQueueItems(claimCtx, 2, defaultSchedulerTickInput{
		Repos:       repos,
		Now:         func() time.Time { return now },
		Worker:      worker,
		AsyncRunner: immediateSchedulerRunner{},
		AllowClaim: func() error {
			n := claimCalls.Add(1)
			if n == 1 {
				return nil
			}
			// Simulate BeginShutdown canceling the scheduler context after the
			// first ClaimNext* already persisted running/claimed.
			cancel()
			return nil
		},
	})
	if err != nil {
		t.Fatalf("claimAndRunScheduledQueueItems() error = %v", err)
	}
	if len(claimed) != 1 || claimed[0].ID != "queue_ctx_cancel_1" {
		t.Fatalf("claimed = %#v, want only first item after mid-batch scheduler cancel", claimed)
	}
	if worker.processItemCount() != 1 || worker.processedItems[0] != "queue_ctx_cancel_1" {
		t.Fatalf("processed = %#v, want first claimed item dispatched (not stranded by ctx cancel)", worker.processedItems)
	}
	second, err := repos.Queue.GetByID(context.Background(), "queue_ctx_cancel_2")
	if err != nil {
		t.Fatalf("Queue.GetByID(second) error = %v", err)
	}
	if second == nil || second.Status != "queued" {
		t.Fatalf("second queue item = %#v, want still queued after cancel before second claim", second)
	}
}
