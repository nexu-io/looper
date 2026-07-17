package runtime

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/storage"
)

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
