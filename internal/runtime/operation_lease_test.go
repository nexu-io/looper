package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
	"github.com/nexu-io/looper/internal/worker"
)

func TestAdmitOperationRefusesWhenAdmissionClosed(t *testing.T) {
	t.Parallel()
	reg := NewActiveExecutionRegistry()
	_ = reg.BeginShutdown("test stop")
	_, err := reg.AdmitOperation(context.Background(), OperationMeta{ClaimedBy: "scheduler"})
	if !errors.Is(err, ErrOperationAdmissionClosed) {
		t.Fatalf("AdmitOperation error = %v, want ErrOperationAdmissionClosed", err)
	}
}

func TestBindClaimRefusesAfterShutdownWithoutStartingPermit(t *testing.T) {
	t.Parallel()
	reg := NewActiveExecutionRegistry()
	lease, err := reg.AdmitOperation(context.Background(), OperationMeta{ClaimedBy: "scheduler"})
	if err != nil {
		t.Fatalf("AdmitOperation: %v", err)
	}

	// Mark admission closed without BeginShutdown wait (bound finalizer drain).
	// BindClaim must return an explicit refuse and retain ownership until Release.
	reg.mu.Lock()
	reg.admissionClosed = true
	reg.shutdownReason = "drain"
	reg.mu.Unlock()

	loopID := "loop-bind-refuse"
	item := storage.QueueItemRecord{ID: "qi-bind-refuse", Type: "worker", LoopID: &loopID, Status: "running"}
	permit, err := lease.BindClaim(item)
	if !errors.Is(err, ErrOperationLeaseCancelled) {
		t.Fatalf("BindClaim error = %v, want ErrOperationLeaseCancelled", err)
	}
	if permit.Valid() {
		t.Fatal("BindClaim must not return a valid permit after cancel")
	}
	if !reg.OwnsQueueClaim(item.ID) {
		t.Fatal("cancelled bind must retain ownership until durable finalize + Release")
	}
	lease.Release()
	if reg.OwnsQueueClaim(item.ID) {
		t.Fatal("Release must drop ownership")
	}
}

func TestBindClaimRefusesWhenLoopStopping(t *testing.T) {
	t.Parallel()
	reg := NewActiveExecutionRegistry()
	lease, err := reg.AdmitOperation(context.Background(), OperationMeta{ClaimedBy: "scheduler"})
	if err != nil {
		t.Fatalf("AdmitOperation: %v", err)
	}
	release, err := reg.BeginLoopStop("loop-stop", "halt")
	if err != nil {
		t.Fatalf("BeginLoopStop: %v", err)
	}
	defer release()

	loopID := "loop-stop"
	item := storage.QueueItemRecord{ID: "qi-loop-stop", Type: "worker", LoopID: &loopID, Status: "running"}
	permit, err := lease.BindClaim(item)
	if !errors.Is(err, ErrOperationLeaseCancelled) {
		t.Fatalf("BindClaim error = %v, want ErrOperationLeaseCancelled", err)
	}
	if permit.Valid() {
		t.Fatal("permit must be invalid when loop is stopping")
	}
	if !lease.Owns(item.ID) {
		t.Fatal("lease must own claim until Release after cancel-bind")
	}
	lease.Release()
}

func TestClaimMissReleasesOperationLeaseImmediately(t *testing.T) {
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

	coordinator := openMigratedCoordinator(t, cfg.Storage.DBPath, backupDir)
	t.Cleanup(func() { _ = coordinator.Close() })
	repos := storage.NewRepositories(coordinator.DB())
	reg := NewActiveExecutionRegistry()

	claimed, err := claimAndRunScheduledQueueItems(context.Background(), 1, defaultSchedulerTickInput{
		Repos:          repos,
		Now:            func() time.Time { return now },
		OperationOwner: reg,
		AsyncRunner:    immediateSchedulerRunner{},
		Worker:         &stubWorkerScheduler{},
	})
	if err != nil {
		t.Fatalf("claimAndRunScheduledQueueItems: %v", err)
	}
	if len(claimed) != 0 {
		t.Fatalf("claimed = %#v, want empty on miss", claimed)
	}
	if reg.PendingOperationCount() != 0 || reg.BoundOperationCount() != 0 {
		t.Fatalf("pending=%d bound=%d, want both 0 after claim miss release", reg.PendingOperationCount(), reg.BoundOperationCount())
	}
}

func TestClaimOwnershipSpanAndFinalizeBeforeRelease(t *testing.T) {
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
	projectID := "project_own_span"
	loopID := "loop_own_span"
	queueID := "queue_own_span"
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Own", RepoPath: filepath.Join(workingDir, "repo"), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert: %v", err)
	}
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 1, ProjectID: projectID, Type: "worker", TargetType: "project", Status: "queued", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert: %v", err)
	}
	if err := repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{
		ID: queueID, ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: projectID,
		DedupeKey: "worker:own_span", Priority: storage.QueuePriorityWorker, Status: "queued",
		AvailableAt: nowISO, Attempts: 0, MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Queue.Upsert: %v", err)
	}

	reg := NewActiveExecutionRegistry()
	var sawOwnedDuringProcess atomic.Bool
	worker := &ownershipProbeWorker{
		onProcess: func(item storage.QueueItemRecord) {
			if reg.OwnsQueueClaim(item.ID) {
				sawOwnedDuringProcess.Store(true)
			}
			if err := repos.Queue.Complete(context.Background(), item.ID, nowISO); err != nil {
				t.Errorf("Complete during process: %v", err)
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
		t.Fatalf("claimed = %#v, want %s", claimed, queueID)
	}
	if !sawOwnedDuringProcess.Load() {
		t.Fatal("while daemon-live running claim must be owned by operation lease during processor")
	}
	if reg.BoundOperationCount() != 0 {
		t.Fatalf("bound ops = %d, want 0 after durable finalize + Release", reg.BoundOperationCount())
	}
	got, err := repos.Queue.GetByID(context.Background(), queueID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got == nil || got.Status != "completed" {
		t.Fatalf("queue status = %#v, want completed", got)
	}
}

func TestReleaseAndBoundCancelShareLockOrder(t *testing.T) {
	t.Parallel()
	// Concurrent Release (finalize) with BeginLoopStop bound-op scan must not
	// deadlock under the registry-then-lease lock order.
	reg := NewActiveExecutionRegistry()
	const n = 32
	leases := make([]OperationLease, 0, n)
	loopID := "loop-lock-order"
	for i := 0; i < n; i++ {
		lease, err := reg.AdmitOperation(context.Background(), OperationMeta{ClaimedBy: "scheduler"})
		if err != nil {
			t.Fatalf("AdmitOperation: %v", err)
		}
		item := storage.QueueItemRecord{
			ID:     "qi-lock-" + strconv.Itoa(i),
			Type:   "worker",
			LoopID: &loopID,
			Status: "running",
		}
		permit, err := lease.BindClaim(item)
		if err != nil || !permit.Valid() {
			t.Fatalf("BindClaim: %v permit=%v", err, permit.Valid())
		}
		leases = append(leases, lease)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for _, lease := range leases {
			lease.Release()
		}
	}()
	// Interleave stop scans that take r.mu then l.mu.
	for i := 0; i < n; i++ {
		release, err := reg.BeginLoopStop("loop-lock-order", "halt")
		if err != nil {
			t.Fatalf("BeginLoopStop: %v", err)
		}
		release()
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Release goroutine deadlocked with BeginLoopStop bound-op scan")
	}
	if reg.BoundOperationCount() != 0 {
		t.Fatalf("bound ops = %d, want 0 after all Release", reg.BoundOperationCount())
	}
}

// callbackSchedulerRunner invokes before (if set) then runs fn synchronously.
// Used to inject BeginLoopStop between schedule and processor start.
type callbackSchedulerRunner struct {
	before func()
}

func (r callbackSchedulerRunner) Go(fn func()) {
	if r.before != nil {
		r.before()
	}
	fn()
}

// ownershipProbeWorker implements workerScheduler for ownership-span contract tests.
type ownershipProbeWorker struct {
	onProcess func(storage.QueueItemRecord)
}

func (s *ownershipProbeWorker) ProcessNext(context.Context, string) (*worker.ProcessResult, error) {
	return nil, nil
}

func (s *ownershipProbeWorker) ProcessClaimedQueueItem(_ context.Context, item storage.QueueItemRecord) (*worker.ProcessResult, error) {
	if s.onProcess != nil {
		s.onProcess(item)
	}
	return &worker.ProcessResult{}, nil
}
