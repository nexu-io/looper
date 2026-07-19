package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
)

func TestStopBindRaceNeverStartsProcessor(t *testing.T) {
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
	projectID := "project_stop_bind"
	loopID := "loop_stop_bind"
	queueID := "queue_stop_bind"
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "StopBind", RepoPath: filepath.Join(workingDir, "repo"), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert: %v", err)
	}
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 2, ProjectID: projectID, Type: "worker", TargetType: "project", Status: "queued", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert: %v", err)
	}
	if err := repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{
		ID: queueID, ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: projectID,
		DedupeKey: "worker:stop_bind", Priority: storage.QueuePriorityWorker, Status: "queued",
		AvailableAt: nowISO, Attempts: 0, MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Queue.Upsert: %v", err)
	}

	reg := NewActiveExecutionRegistry()
	lease, err := reg.AdmitOperation(context.Background(), OperationMeta{ClaimedBy: "scheduler"})
	if err != nil {
		t.Fatalf("AdmitOperation: %v", err)
	}
	item, err := repos.Queue.ClaimNextOfType(context.Background(), nowISO, "scheduler", "worker")
	if err != nil || item == nil {
		t.Fatalf("ClaimNextOfType = (%#v, %v)", item, err)
	}
	release, err := reg.BeginLoopStop(loopID, "stop before bind")
	if err != nil {
		t.Fatalf("BeginLoopStop: %v", err)
	}
	defer release()

	permit, bindErr := lease.BindClaim(*item)
	if !errors.Is(bindErr, ErrOperationLeaseCancelled) {
		t.Fatalf("BindClaim = %v, want ErrOperationLeaseCancelled", bindErr)
	}
	if permit.Valid() {
		t.Fatal("processor must not receive a valid permit after cancelled lease")
	}

	if err := finalizeCancelledClaim(context.Background(), *item, defaultSchedulerTickInput{
		Repos: repos,
		Now:   func() time.Time { return now },
	}, func() time.Time { return now }); err != nil {
		t.Fatalf("finalizeCancelledClaim: %v", err)
	}
	lease.Release()

	got, err := repos.Queue.GetByID(context.Background(), queueID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got == nil || got.Status != "queued" {
		t.Fatalf("after cancelled bind = %#v, want requeued", got)
	}
	if reg.OwnsQueueClaim(queueID) {
		t.Fatal("ownership must drop after durable requeue + Release")
	}
}
