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

// Contract: BeginShutdown cancels the scheduler context so in-flight ticks can
// observe cancellation during the HTTP drain window before Runtime.Stop.
func TestSafetyFloorBeginShutdownCancelsSchedulerContext(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	backupDir := filepath.Join(workingDir, "backups")
	cfg.Storage.BackupDir = &backupDir
	cfg.Scheduler.PollIntervalSeconds = 3600

	ctxSeen := make(chan context.Context, 1)
	block := make(chan struct{})
	rt := New(Options{
		Config:        cfg,
		Logger:        &testLogger{},
		DeferRecovery: true,
	})
	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() {
		close(block)
		rt.Stop("test cleanup")
	})

	rt.mu.Lock()
	rt.runSchedulerTick = func(ctx context.Context, _ Services) error {
		select {
		case ctxSeen <- ctx:
		default:
		}
		<-block
		return ctx.Err()
	}
	rt.services = Services{Repositories: &storage.Repositories{}}
	rt.mu.Unlock()

	rt.startSchedulerLoop()
	var tickCtx context.Context
	select {
	case tickCtx = <-ctxSeen:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for scheduler tick context")
	}
	if err := tickCtx.Err(); err != nil {
		t.Fatalf("tick context already done before BeginShutdown: %v", err)
	}

	rt.BeginShutdown("test drain")
	select {
	case <-tickCtx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("scheduler context was not canceled by BeginShutdown")
	}
	if rt.AdmissionState() != AdmissionStopping {
		t.Fatalf("AdmissionState() = %q, want stopping", rt.AdmissionState())
	}
}

// Contract (#580 review): MarkDegraded cancels the scheduler context so an
// in-flight tick that already passed AllowClaim cannot keep discovering after
// sticky degrade (same cancel path as BeginShutdown for producers).
func TestSafetyFloorMarkDegradedCancelsSchedulerContext(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	backupDir := filepath.Join(workingDir, "backups")
	cfg.Storage.BackupDir = &backupDir
	cfg.Scheduler.PollIntervalSeconds = 3600

	ctxSeen := make(chan context.Context, 1)
	block := make(chan struct{})
	rt := New(Options{
		Config:        cfg,
		Logger:        &testLogger{},
		DeferRecovery: true,
	})
	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() {
		close(block)
		rt.Stop("test cleanup")
	})

	// DeferRecovery leaves admission starting and does not arm the scheduler;
	// install a blocking tick then start the loop so we can observe cancel.
	rt.mu.Lock()
	rt.runSchedulerTick = func(ctx context.Context, _ Services) error {
		select {
		case ctxSeen <- ctx:
		default:
		}
		<-block
		return ctx.Err()
	}
	rt.services = Services{Repositories: &storage.Repositories{}}
	rt.mu.Unlock()

	rt.startSchedulerLoop()
	var tickCtx context.Context
	select {
	case tickCtx = <-ctxSeen:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for scheduler tick context")
	}
	if err := tickCtx.Err(); err != nil {
		t.Fatalf("tick context already done before MarkDegraded: %v", err)
	}

	// starting → degraded is legal; same cancel path as ready → degraded.
	if err := rt.MarkDegraded("test hard persist failure"); err != nil {
		t.Fatalf("MarkDegraded() error = %v", err)
	}
	select {
	case <-tickCtx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("scheduler context was not canceled by MarkDegraded")
	}
	if rt.AdmissionState() != AdmissionDegraded {
		t.Fatalf("AdmissionState() = %q, want degraded", rt.AdmissionState())
	}
}

// Contract (#580 review): MarkDegraded cancels the worktree cleanup context so
// an in-flight CleanupWorktree that already passed AllowClaim observes cancel
// and cannot finish git remove plus cleaned-record/event writes after close.
func TestSafetyFloorMarkDegradedCancelsWorktreeCleanupContext(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	backupDir := filepath.Join(workingDir, "backups")
	cfg.Storage.BackupDir = &backupDir

	rt := New(Options{
		Config:        cfg,
		Logger:        &testLogger{},
		DeferRecovery: true,
	})
	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { rt.Stop("test cleanup") })
	if err := rt.CompleteStartup(context.Background()); err != nil {
		t.Fatalf("CompleteStartup() error = %v", err)
	}

	cleanupCtx, cleanupCancel := context.WithCancel(context.Background())
	rt.mu.Lock()
	rt.worktreeCleanupCancel = cleanupCancel
	rt.mu.Unlock()

	if err := cleanupCtx.Err(); err != nil {
		t.Fatalf("cleanup context already done before MarkDegraded: %v", err)
	}

	if err := rt.MarkDegraded("test hard persist failure"); err != nil {
		t.Fatalf("MarkDegraded() error = %v", err)
	}
	select {
	case <-cleanupCtx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("worktree cleanup context was not canceled by MarkDegraded")
	}
	if rt.AdmissionState() != AdmissionDegraded {
		t.Fatalf("AdmissionState() = %q, want degraded", rt.AdmissionState())
	}
}

// Contract (#592 review): sticky MarkDegraded must not CancelExecute webhook
// workers. Accepted/202 deliveries still need CreateOrGetActiveByDedupe under a
// live daemon; GitHub will not retry. New accepts are refused at Forward.
func TestSafetyFloorMarkDegradedDoesNotCancelWebhookExecute(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	backupDir := filepath.Join(workingDir, "backups")
	cfg.Storage.BackupDir = &backupDir

	rt := New(Options{
		Config:        cfg,
		Logger:        &testLogger{},
		DeferRecovery: true,
	})
	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { rt.Stop("test cleanup") })
	if err := rt.CompleteStartup(context.Background()); err != nil {
		t.Fatalf("CompleteStartup() error = %v", err)
	}

	var cancelCalls atomic.Int64
	forwarder := &countingCancelForwarder{onCancel: func() { cancelCalls.Add(1) }}
	rt.mu.Lock()
	rt.webhookForwarder = forwarder
	rt.mu.Unlock()

	if err := rt.MarkDegraded("test hard persist failure"); err != nil {
		t.Fatalf("MarkDegraded() error = %v", err)
	}
	if cancelCalls.Load() != 0 {
		t.Fatalf("CancelExecute calls = %d, want 0 from MarkDegraded", cancelCalls.Load())
	}
	if rt.AdmissionState() != AdmissionDegraded {
		t.Fatalf("AdmissionState() = %q, want degraded", rt.AdmissionState())
	}
}

// Contract: direct Runtime.Stop cancels in-flight webhook discovery before
// producer waits, matching BeginShutdown / daemonRuntime.Stop cancel timing.
func TestSafetyFloorRuntimeStopCancelsWebhookExecute(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	backupDir := filepath.Join(workingDir, "backups")
	cfg.Storage.BackupDir = &backupDir

	rt := New(Options{
		Config:        cfg,
		Logger:        &testLogger{},
		DeferRecovery: true,
	})
	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if err := rt.CompleteStartup(context.Background()); err != nil {
		t.Fatalf("CompleteStartup() error = %v", err)
	}

	var cancelCalls atomic.Int64
	forwarder := &countingCancelForwarder{onCancel: func() { cancelCalls.Add(1) }}
	rt.mu.Lock()
	rt.webhookForwarder = forwarder
	rt.mu.Unlock()

	rt.Stop("test direct stop")
	if cancelCalls.Load() < 1 {
		t.Fatalf("CancelExecute calls = %d, want >= 1 from direct Runtime.Stop", cancelCalls.Load())
	}
	if rt.AdmissionState() != AdmissionStopping {
		t.Fatalf("AdmissionState() = %q, want stopping", rt.AdmissionState())
	}
}

// Contract: after MarkReady, CompleteStartup wakes the full scheduler so the
// initial startSchedulerLoop tick (while admission was starting) is not the
// only chance at immediate discovery.
func TestSafetyFloorCompleteStartupWakesSchedulerAfterMarkReady(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	backupDir := filepath.Join(workingDir, "backups")
	cfg.Storage.BackupDir = &backupDir
	cfg.Scheduler.PollIntervalSeconds = 3600

	var tickCount atomic.Int64
	rt := New(Options{
		Config:        cfg,
		Logger:        &testLogger{},
		DeferRecovery: true,
	})
	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { rt.Stop("test cleanup") })

	rt.mu.Lock()
	rt.runSchedulerTick = func(context.Context, Services) error {
		tickCount.Add(1)
		return nil
	}
	rt.defaultSchedulerClaim = func(context.Context, Services) error { return nil }
	rt.mu.Unlock()

	if err := rt.CompleteStartup(context.Background()); err != nil {
		t.Fatalf("CompleteStartup() error = %v", err)
	}
	if rt.AdmissionState() != AdmissionReady {
		t.Fatalf("AdmissionState() = %q, want ready", rt.AdmissionState())
	}

	// startSchedulerLoop runs one immediate tick, then MarkReady + TriggerSchedulerTick
	// must produce a second tick without waiting for the long poll interval.
	waitForCondition(t, 2*time.Second, func() bool {
		return tickCount.Load() >= 2
	})
}

// countingCancelForwarder records CancelExecute for direct-Stop coverage.
type countingCancelForwarder struct {
	stubRuntimeWebhookForwarder
	onCancel func()
}

func (f *countingCancelForwarder) CancelExecute() {
	if f.onCancel != nil {
		f.onCancel()
	}
}
