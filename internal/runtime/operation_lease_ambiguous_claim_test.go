package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
)

func TestAmbiguousClaimCancelRecoversBeforeRelease(t *testing.T) {
	// Serial: uses package-level testAfterClaimHook.
	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig: %v", err)
	}
	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	backupDir := filepath.Join(workingDir, "backups")
	cfg.Storage.BackupDir = &backupDir
	now := time.Date(2026, time.July, 19, 12, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)

	coordinator := openMigratedCoordinator(t, cfg.Storage.DBPath, backupDir)
	t.Cleanup(func() { _ = coordinator.Close() })
	repos := storage.NewRepositories(coordinator.DB())
	projectID := "project_ambig_claim"
	loopID := "loop_ambig_claim"
	queueID := "queue_ambig_claim"
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "AmbigClaim", RepoPath: filepath.Join(workingDir, "repo"), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert: %v", err)
	}
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 1, ProjectID: projectID, Type: "worker", TargetType: "project", Status: "queued", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert: %v", err)
	}
	if err := repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{
		ID: queueID, ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: projectID,
		DedupeKey: "worker:ambig_claim", Priority: storage.QueuePriorityWorker, Status: "queued",
		AvailableAt: nowISO, Attempts: 0, MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Queue.Upsert: %v", err)
	}

	// Simulate ClaimNext UPDATE committing then scan observing context cancel:
	// hide the returned item and surface context.Canceled so recovery must adopt.
	testAfterClaimHook = func(item *storage.QueueItemRecord, err error) (*storage.QueueItemRecord, error) {
		if err != nil || item == nil {
			return item, err
		}
		return nil, context.Canceled
	}
	t.Cleanup(func() { testAfterClaimHook = nil })

	reg := NewActiveExecutionRegistry()
	// ownershipProbeWorker Completes so ensureClaimFinalized can Release cleanly.
	worker := &ownershipProbeWorker{
		onProcess: func(item storage.QueueItemRecord) {
			if !reg.OwnsQueueClaim(item.ID) {
				t.Errorf("recovered claim %s must be owned during process", item.ID)
			}
			if err := repos.Queue.Complete(context.Background(), item.ID, nowISO); err != nil {
				t.Errorf("Complete: %v", err)
			}
		},
	}

	claimed, err := claimAndRunScheduledQueueItems(context.Background(), 1, defaultSchedulerTickInput{
		Repos:          repos,
		Now:            func() time.Time { return now },
		OperationOwner: reg,
		AsyncRunner:    immediateSchedulerRunner{},
		Worker:         worker,
	})
	if err != nil {
		t.Fatalf("claimAndRunScheduledQueueItems: %v", err)
	}
	if len(claimed) != 1 || claimed[0].ID != queueID {
		t.Fatalf("claimed = %#v, want recovered %s", claimed, queueID)
	}
	got, getErr := repos.Queue.GetByID(context.Background(), queueID)
	if getErr != nil {
		t.Fatalf("GetByID: %v", getErr)
	}
	if got == nil || got.Status == "running" {
		t.Fatalf("queue after ambiguous cancel recover = %#v, want finalized (not unowned running)", got)
	}
	if reg.PendingOperationCount() != 0 || reg.BoundOperationCount() != 0 {
		t.Fatalf("pending=%d bound=%d, want both 0 after recover+bind+finalize+Release", reg.PendingOperationCount(), reg.BoundOperationCount())
	}
	if reg.OwnsQueueClaim(queueID) {
		t.Fatal("ownership must drop after durable finalize + Release")
	}
}

func TestAmbiguousClaimCancelRetainsLeaseWhenRecoveryFails(t *testing.T) {
	// Serial: uses package-level testAfterClaimHook.
	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig: %v", err)
	}
	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	backupDir := filepath.Join(workingDir, "backups")
	cfg.Storage.BackupDir = &backupDir
	now := time.Date(2026, time.July, 19, 12, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)

	coordinator := openMigratedCoordinator(t, cfg.Storage.DBPath, backupDir)
	t.Cleanup(func() { _ = coordinator.Close() })
	repos := storage.NewRepositories(coordinator.DB())
	// Close DB so recovery ListRunningClaimedBy fails after simulated cancel.
	if err := coordinator.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	testAfterClaimHook = func(*storage.QueueItemRecord, error) (*storage.QueueItemRecord, error) {
		return nil, context.Canceled
	}
	t.Cleanup(func() { testAfterClaimHook = nil })

	reg := NewActiveExecutionRegistry()
	var degraded atomic.Bool
	reg.SetOnHardPersistFailure(func(error) { degraded.Store(true) })

	_, err = claimAndRunScheduledQueueItems(context.Background(), 1, defaultSchedulerTickInput{
		Repos:          repos,
		Now:            func() time.Time { return now },
		OperationOwner: reg,
		AsyncRunner:    immediateSchedulerRunner{},
		Worker:         &stubWorkerScheduler{},
	})
	if err == nil || !errors.Is(err, ErrOperationFinalizeFailed) {
		t.Fatalf("error = %v, want ErrOperationFinalizeFailed", err)
	}
	if !degraded.Load() {
		t.Fatal("recovery failure must degrade via ReportHardPersistFailure")
	}
	if reg.PendingOperationCount() != 1 {
		t.Fatalf("pending=%d, want 1 retained lease (not released)", reg.PendingOperationCount())
	}
	_ = nowISO
}
