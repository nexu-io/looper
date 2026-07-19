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

// Contract: after BindClaim returns a valid permit, BeginLoopStop may cancel
// the lease before the processor goroutine starts. The permit remains Valid(),
// but runOwnedQueueClaims must requeue under retained ownership and must not
// invoke the processor with the detached dispatch context.
func TestPostBindStopNeverStartsProcessor(t *testing.T) {
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
	projectID := "project_post_bind_stop"
	loopID := "loop_post_bind_stop"
	queueID := "queue_post_bind_stop"
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "PostBindStop", RepoPath: filepath.Join(workingDir, "repo"), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert: %v", err)
	}
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 3, ProjectID: projectID, Type: "worker", TargetType: "project", Status: "queued", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert: %v", err)
	}
	if err := repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{
		ID: queueID, ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: projectID,
		DedupeKey: "worker:post_bind_stop", Priority: storage.QueuePriorityWorker, Status: "queued",
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
	permit, bindErr := lease.BindClaim(*item)
	if bindErr != nil || !permit.Valid() {
		t.Fatalf("BindClaim = (%#v, %v), want valid permit", permit, bindErr)
	}

	// Post-bind stop: lease is cancelled while permit remains Valid().
	release, err := reg.BeginLoopStop(loopID, "stop after bind before processor")
	if err != nil {
		t.Fatalf("BeginLoopStop: %v", err)
	}
	defer release()
	if lease.Context().Err() == nil {
		t.Fatal("lease context must be cancelled after BeginLoopStop")
	}
	if !permit.Valid() {
		t.Fatal("permit remains Valid() after post-bind stop; that is the race under test")
	}

	var started atomic.Bool
	worker := &ownershipProbeWorker{
		onProcess: func(storage.QueueItemRecord) {
			started.Store(true)
		},
	}
	// Detached dispatch ctx matches production: scheduler cancel is stripped so
	// finalize can still write; the gate must be the operation lease instead.
	dispatchCtx := context.WithoutCancel(context.Background())
	if err := runOwnedQueueClaims(dispatchCtx, []ownedQueueClaim{{
		item:   *item,
		lease:  lease,
		permit: permit,
	}}, defaultSchedulerTickInput{
		Repos:          repos,
		Now:            func() time.Time { return now },
		OperationOwner: reg,
		AsyncRunner:    immediateSchedulerRunner{},
		Worker:         worker,
	}); err != nil {
		t.Fatalf("runOwnedQueueClaims: %v", err)
	}
	if started.Load() {
		t.Fatal("processor must not start after post-bind lease cancel")
	}
	got, err := repos.Queue.GetByID(context.Background(), queueID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got == nil || got.Status != "queued" {
		t.Fatalf("after post-bind stop = %#v, want requeued", got)
	}
	if reg.OwnsQueueClaim(queueID) {
		t.Fatal("ownership must drop after durable requeue + Release")
	}
}

// Contract: AsyncRunner may delay start until after BeginLoopStop cancels the
// bound lease. The launch-time permit check saw a live lease; the deferred run
// must revalidate lease.Context() and requeue without invoking the processor.
func TestPostBindStopDuringAsyncLaunchNeverStartsProcessor(t *testing.T) {
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
	projectID := "project_async_post_bind"
	loopID := "loop_async_post_bind"
	queueID := "queue_async_post_bind"
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "AsyncPostBind", RepoPath: filepath.Join(workingDir, "repo"), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert: %v", err)
	}
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 4, ProjectID: projectID, Type: "worker", TargetType: "project", Status: "queued", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert: %v", err)
	}
	if err := repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{
		ID: queueID, ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: projectID,
		DedupeKey: "worker:async_post_bind", Priority: storage.QueuePriorityWorker, Status: "queued",
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
	permit, bindErr := lease.BindClaim(*item)
	if bindErr != nil || !permit.Valid() {
		t.Fatalf("BindClaim = (%#v, %v), want valid permit", permit, bindErr)
	}

	var stopRelease func()
	t.Cleanup(func() {
		if stopRelease != nil {
			stopRelease()
		}
	})
	async := callbackSchedulerRunner{before: func() {
		release, stopErr := reg.BeginLoopStop(loopID, "stop during async launch")
		if stopErr != nil {
			t.Errorf("BeginLoopStop during async: %v", stopErr)
			return
		}
		stopRelease = release
	}}

	var started atomic.Bool
	worker := &ownershipProbeWorker{
		onProcess: func(storage.QueueItemRecord) {
			started.Store(true)
		},
	}
	if err := runOwnedQueueClaims(context.Background(), []ownedQueueClaim{{
		item:   *item,
		lease:  lease,
		permit: permit,
	}}, defaultSchedulerTickInput{
		Repos:          repos,
		Now:            func() time.Time { return now },
		OperationOwner: reg,
		AsyncRunner:    async,
		Worker:         worker,
	}); err != nil {
		t.Fatalf("runOwnedQueueClaims: %v", err)
	}
	if started.Load() {
		t.Fatal("processor must not start when lease is cancelled between schedule and run")
	}
	got, err := repos.Queue.GetByID(context.Background(), queueID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got == nil || got.Status != "queued" {
		t.Fatalf("after async post-bind stop = %#v, want requeued", got)
	}
	if reg.OwnsQueueClaim(queueID) {
		t.Fatal("ownership must drop after durable requeue + Release")
	}
}
