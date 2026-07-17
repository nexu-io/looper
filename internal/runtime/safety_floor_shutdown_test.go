package runtime

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
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

// Contract: after MarkReady, if BeginShutdown runs before recoveryCancel is
// registered, startDeferredReviewerRecovery must not arm a live recovery that
// can requeue while admission is already stopping.
func TestSafetyFloorDeferredRecoveryDoesNotStartAfterShutdown(t *testing.T) {
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

	if err := rt.admission.MarkReady("test mark ready"); err != nil {
		t.Fatalf("MarkReady() error = %v", err)
	}
	rt.mu.Lock()
	rt.services = Services{Repositories: &storage.Repositories{}}
	rt.mu.Unlock()

	// Shutdown closes admission before CompleteStartup can register recoveryCancel.
	rt.BeginShutdown("test drain before deferred recovery arm")
	if rt.AdmissionState() != AdmissionStopping {
		t.Fatalf("AdmissionState() = %q, want stopping", rt.AdmissionState())
	}

	rt.startDeferredReviewerRecovery(&githubinfra.Gateway{})

	rt.mu.Lock()
	cancel := rt.recoveryCancel
	done := rt.recoveryDone
	rt.mu.Unlock()
	if cancel != nil || done != nil {
		// Publish-then-recheck may still register then cancel; require done
		// closed so Stop cannot hang on an orphaned recovery goroutine.
		if cancel == nil || done == nil {
			t.Fatal("partial deferred recovery registration after shutdown")
		}
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("deferred recovery did not exit after post-shutdown start")
		}
	}
	if !rt.admissionRefusesDeferredRequeue() {
		t.Fatal("admissionRefusesDeferredRequeue() = false while stopping, want true")
	}
	if err := rt.AllowClaim(); err == nil {
		t.Fatal("AllowClaim() = nil after shutdown, want refusal so recovery cannot requeue")
	}
}

// Contract: BeginShutdown cancels deferred reviewer recovery at admission close
// so requeueFailedReviewerWithSharedGuards cannot persist queued work while
// admission is already stopping (HTTP drain window before Runtime.Stop).
func TestSafetyFloorBeginShutdownCancelsDeferredReviewerRecovery(t *testing.T) {
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

	// Simulate a post-ready deferred recovery goroutine still in flight during
	// the HTTP drain window (recoveryCancel is only waited on in Runtime.Stop).
	recoveryCtx, recoveryCancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	rt.mu.Lock()
	rt.recoveryCancel = recoveryCancel
	rt.recoveryDone = done
	rt.mu.Unlock()
	t.Cleanup(func() {
		select {
		case <-done:
		default:
			close(done)
		}
	})

	if err := recoveryCtx.Err(); err != nil {
		t.Fatalf("recovery context already done before BeginShutdown: %v", err)
	}

	rt.BeginShutdown("test drain")
	select {
	case <-recoveryCtx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("deferred recovery context was not canceled by BeginShutdown")
	}
	if rt.AdmissionState() != AdmissionStopping {
		t.Fatalf("AdmissionState() = %q, want stopping", rt.AdmissionState())
	}
	// recoveryCancel must remain set so Runtime.Stop can still wait on done.
	rt.mu.Lock()
	stillSet := rt.recoveryCancel != nil
	rt.mu.Unlock()
	if !stillSet {
		t.Fatal("recoveryCancel was cleared by BeginShutdown; Stop must retain it for wait")
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
