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

func TestFinalizePersistFailureRetainsOwnershipAndDegrades(t *testing.T) {
	t.Parallel()

	reg := NewActiveExecutionRegistry()
	var degraded atomic.Bool
	reg.SetOnHardPersistFailure(func(error) { degraded.Store(true) })

	lease, err := reg.AdmitOperation(context.Background(), OperationMeta{ClaimedBy: "scheduler"})
	if err != nil {
		t.Fatalf("AdmitOperation: %v", err)
	}
	loopID := "loop-fin-fail"
	item := storage.QueueItemRecord{ID: "qi-fin-fail", Type: "worker", LoopID: &loopID, Status: "running"}
	permit, err := lease.BindClaim(item)
	if err != nil || !permit.Valid() {
		t.Fatalf("BindClaim = (%#v, %v)", permit, err)
	}
	if !reg.OwnsQueueClaim(item.ID) {
		t.Fatal("expected ownership after bind")
	}

	reg.ReportHardPersistFailure(errors.New("sqlite disk full"))
	if !degraded.Load() {
		t.Fatal("hard finalize failure must invoke degrade hook")
	}
	if !reg.OwnsQueueClaim(item.ID) {
		t.Fatal("ownership must be retained after finalize failure")
	}
	if reg.BoundOperationCount() != 1 {
		t.Fatalf("bound ops = %d, want 1 retained", reg.BoundOperationCount())
	}
}

func TestRunnerErrorStillFinalizesThenReleases(t *testing.T) {
	t.Parallel()

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
	projectID := "project_runner_err"
	loopID := "loop_runner_err"
	queueID := "queue_runner_err"
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "RunnerErr", RepoPath: filepath.Join(workingDir, "repo"), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert: %v", err)
	}
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 3, ProjectID: projectID, Type: "worker", TargetType: "project", Status: "queued", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert: %v", err)
	}
	if err := repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{
		ID: queueID, ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: projectID,
		DedupeKey: "worker:runner_err", Priority: storage.QueuePriorityWorker, Status: "queued",
		AvailableAt: nowISO, Attempts: 0, MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Queue.Upsert: %v", err)
	}

	reg := NewActiveExecutionRegistry()
	workerStub := &stubWorkerScheduler{processErr: errors.New("runner boom")}
	_, err = claimAndRunScheduledQueueItems(context.Background(), 1, defaultSchedulerTickInput{
		Repos:          repos,
		Now:            func() time.Time { return now },
		OperationOwner: reg,
		AsyncRunner:    immediateSchedulerRunner{},
		Worker:         workerStub,
	})
	if err != nil {
		t.Fatalf("claimAndRunScheduledQueueItems: %v", err)
	}
	got, err := repos.Queue.GetByID(context.Background(), queueID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got == nil || got.Status == "running" {
		t.Fatalf("after runner error = %#v, want durable finalize (not running)", got)
	}
	if reg.BoundOperationCount() != 0 {
		t.Fatalf("bound ops = %d, want 0 after typed finalize + Release", reg.BoundOperationCount())
	}
}
