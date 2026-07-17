package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/nexu-io/looper/internal/config"
)

// Contract: mutations and claims are gated until admission is ready; there is
// no dual ready-flag Authority that can disagree with admission (#575).
func TestSafetyFloorMutationsAndClaimsGatedUntilReady(t *testing.T) {
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

	if got := rt.AdmissionState(); got != AdmissionStarting {
		t.Fatalf("AdmissionState() after Start+DeferRecovery = %q, want starting", got)
	}
	if err := rt.AllowMutations(); !errors.Is(err, ErrAdmissionNotReady) {
		t.Fatalf("AllowMutations() while starting = %v, want ErrAdmissionNotReady", err)
	}
	if err := rt.AllowClaim(); !errors.Is(err, ErrAdmissionNotReady) {
		t.Fatalf("AllowClaim() while starting = %v, want ErrAdmissionNotReady", err)
	}
	// ownershipAcquired must not open mutations — only admission does.
	if rt.ownershipAcquired {
		t.Fatal("ownershipAcquired = true before CompleteStartup")
	}

	var claimCalls atomic.Int64
	rt.mu.Lock()
	rt.defaultSchedulerClaim = func(context.Context, Services) error {
		claimCalls.Add(1)
		return nil
	}
	rt.mu.Unlock()
	rt.executeSchedulerClaimPass(context.Background())
	if claimCalls.Load() != 0 {
		t.Fatalf("claim pump ran while starting, calls=%d", claimCalls.Load())
	}

	if err := rt.CompleteStartup(context.Background()); err != nil {
		t.Fatalf("CompleteStartup() error = %v", err)
	}
	if got := rt.AdmissionState(); got != AdmissionReady {
		t.Fatalf("AdmissionState() after CompleteStartup = %q, want ready", got)
	}
	if !rt.ownershipAcquired {
		t.Fatal("ownershipAcquired = false after CompleteStartup")
	}
	if err := rt.AllowMutations(); err != nil {
		t.Fatalf("AllowMutations() after ready = %v", err)
	}
	if err := rt.AllowClaim(); err != nil {
		t.Fatalf("AllowClaim() after ready = %v", err)
	}

	beforeReadyClaims := claimCalls.Load()
	rt.executeSchedulerClaimPass(context.Background())
	if claimCalls.Load() <= beforeReadyClaims {
		t.Fatalf("claim pump calls after ready = %d, want > %d", claimCalls.Load(), beforeReadyClaims)
	}
	claimsWhenReady := claimCalls.Load()

	// Dual-flag invariant: forcing ownershipAcquired alone must not admit work
	// when admission is degraded.
	if err := rt.MarkDegraded("test degrade"); err != nil {
		t.Fatalf("MarkDegraded() error = %v", err)
	}
	rt.ownershipAcquired = true
	if err := rt.AllowMutations(); !errors.Is(err, ErrAdmissionDegraded) {
		t.Fatalf("AllowMutations() while degraded with ownershipAcquired = %v, want degraded", err)
	}
	if err := rt.AllowClaim(); !errors.Is(err, ErrAdmissionDegraded) {
		t.Fatalf("AllowClaim() while degraded with ownershipAcquired = %v, want degraded", err)
	}
	rt.executeSchedulerClaimPass(context.Background())
	if claimCalls.Load() != claimsWhenReady {
		t.Fatalf("claim pump advanced while degraded, calls=%d want %d", claimCalls.Load(), claimsWhenReady)
	}
}

func TestSafetyFloorBeginShutdownClosesAdmissionBeforeStorage(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	backupDir := filepath.Join(workingDir, "backups")
	cfg.Storage.BackupDir = &backupDir

	rt := New(Options{Config: cfg, Logger: &testLogger{}})
	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if rt.AdmissionState() != AdmissionReady {
		t.Fatalf("AdmissionState() = %q, want ready", rt.AdmissionState())
	}

	rt.BeginShutdown("test stop")
	if rt.AdmissionState() != AdmissionStopping {
		t.Fatalf("AdmissionState() after BeginShutdown = %q, want stopping", rt.AdmissionState())
	}
	if err := rt.AllowMutations(); !errors.Is(err, ErrAdmissionStopping) {
		t.Fatalf("AllowMutations after BeginShutdown = %v", err)
	}
	// Storage still available until Stop closes it.
	if services := rt.Services(); services.Coordinator == nil {
		t.Fatal("Services().Coordinator = nil after BeginShutdown, want storage retained until Stop")
	}
	rt.Stop("test stop")
}
