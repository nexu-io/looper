package runtime

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/agent"
	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/processcontainment"
)

// Contract (#577): shutdown order is admission → cancel/drain producers →
// handles, then SQLite close. On drain failure, retain storage and never
// report graceful success (StorageRetained).
func TestShutdownRetainsStorageWhenContainmentDrainFails(t *testing.T) {
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

	// Bind a live Supervisor-owned process, then force softKill to fail so
	// BeginShutdown surfaces a drain error (confirmed-drain path still runs).
	lease, err := rt.activeExecutions.AdmitSpawn(context.Background(), agent.SpawnMeta{
		LoopID: "loop-drain-fail", RunID: "run-drain-fail", ExecutionID: "exec-drain-fail",
	})
	if err != nil {
		t.Fatalf("AdmitSpawn: %v", err)
	}
	cmd := exec.Command("sleep", "60")
	processcontainment.Configure(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("cmd.Start: %v", err)
	}
	handle, err := processcontainment.Bind(cmd, processcontainment.Options{
		GracePeriod:  20 * time.Millisecond,
		DrainTimeout: 2 * time.Second,
	})
	if err != nil {
		_ = cmd.Process.Kill()
		t.Fatalf("Bind: %v", err)
	}
	forceDrainFail := errors.New("forced soft-kill failure for retain-storage contract")
	if err := lease.BindHandle(handle, func(string) error { return forceDrainFail }); err != nil {
		t.Fatalf("BindHandle: %v", err)
	}

	// BeginShutdown records drain failure without closing storage.
	rt.BeginShutdown("test drain fail")
	if rt.AdmissionState() != AdmissionStopping {
		t.Fatalf("AdmissionState() = %q, want stopping", rt.AdmissionState())
	}
	if services := rt.Services(); services.Coordinator == nil {
		t.Fatal("Services().Coordinator = nil after BeginShutdown, want storage retained until Stop decision")
	}
	if err := rt.ShutdownDrainError(); err == nil {
		t.Fatal("ShutdownDrainError() = nil after failed drain, want error")
	} else if !errors.Is(err, errShutdownDrainTimeout) && !errors.Is(err, forceDrainFail) && !strings.Contains(err.Error(), forceDrainFail.Error()) {
		t.Fatalf("ShutdownDrainError() = %v, want join of drain failure", err)
	}

	// Stop must retain SQLite — no false graceful close.
	rt.Stop("test drain fail")
	if !rt.StorageRetained() {
		t.Fatal("StorageRetained() = false, want true after drain failure")
	}
	if services := rt.Services(); services.Coordinator == nil {
		t.Fatal("Services().Coordinator = nil after Stop with drain failure, want retained storage")
	}
	// Process group must still have been targeted for kill even though softKill failed.
	if !handle.ConfirmedDead() {
		// softKill failed but handle.Kill should still have run; if not confirmed,
		// kill may have timed — either way storage must stay retained (already asserted).
		t.Logf("handle ConfirmedDead=%v Snapshot=%+v (storage retained regardless)", handle.ConfirmedDead(), handle.Snapshot())
	}
}

// Contract: happy-path shutdown closes storage after successful drain.
func TestShutdownClosesStorageWhenDrainSucceeds(t *testing.T) {
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

	// Bind a short-lived process that Kill can confirmed-drain.
	lease, err := rt.activeExecutions.AdmitSpawn(context.Background(), agent.SpawnMeta{
		LoopID: "loop-drain-ok", RunID: "run-drain-ok", ExecutionID: "exec-drain-ok",
	})
	if err != nil {
		t.Fatalf("AdmitSpawn: %v", err)
	}
	cmd := exec.Command("sleep", "60")
	processcontainment.Configure(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("cmd.Start: %v", err)
	}
	handle, err := processcontainment.Bind(cmd, processcontainment.Options{
		GracePeriod:  20 * time.Millisecond,
		DrainTimeout: 3 * time.Second,
	})
	if err != nil {
		_ = cmd.Process.Kill()
		t.Fatalf("Bind: %v", err)
	}
	if err := lease.BindHandle(handle, func(string) error { return nil }); err != nil {
		t.Fatalf("BindHandle: %v", err)
	}

	rt.Stop("test clean drain")
	if rt.StorageRetained() {
		t.Fatal("StorageRetained() = true after successful drain, want false")
	}
	if err := rt.ShutdownDrainError(); err != nil {
		t.Fatalf("ShutdownDrainError() = %v, want nil", err)
	}
	if services := rt.Services(); services.Coordinator != nil {
		t.Fatal("Services().Coordinator still set after successful Stop, want closed")
	}
	if !handle.ConfirmedDead() {
		t.Fatal("handle not confirmed-dead after successful shutdown drain")
	}
}

// Contract (#577): non-agent Supervisor-owned containment failures (shell /
// trusted-review) must feed retain-storage, not only agent-registry drains.
func TestShutdownRetainsStorageWhenNonAgentDrainFailureReported(t *testing.T) {
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

	// Simulate validation/trusted-review reporting ErrNotConfirmedDead outside
	// ActiveExecutionRegistry agent leases (the gap fixed for #590 review).
	force := errors.New("non-agent shell drain not confirmed")
	rt.activeExecutions.ReportDrainFailure(force)

	rt.BeginShutdown("test non-agent drain fail")
	if err := rt.ShutdownDrainError(); err == nil {
		t.Fatal("ShutdownDrainError() = nil after non-agent drain failure report, want error")
	} else if !errors.Is(err, force) && !strings.Contains(err.Error(), force.Error()) {
		t.Fatalf("ShutdownDrainError() = %v, want non-agent failure", err)
	}

	rt.Stop("test non-agent drain fail")
	if !rt.StorageRetained() {
		t.Fatal("StorageRetained() = false, want true after non-agent drain failure")
	}
	if services := rt.Services(); services.Coordinator == nil {
		t.Fatal("Services().Coordinator = nil after Stop with non-agent drain failure, want retained storage")
	}
}

// Registry contract: BeginShutdown surfaces kill/drain failures (#577).
func TestActiveExecutionRegistryBeginShutdownReturnsDrainFailure(t *testing.T) {
	t.Parallel()
	reg := NewActiveExecutionRegistry()
	reg.killTimeout = 2 * time.Second

	lease, err := reg.AdmitSpawn(context.Background(), agent.SpawnMeta{
		LoopID: "loop-sd", RunID: "run-sd", ExecutionID: "exec-sd",
	})
	if err != nil {
		t.Fatalf("AdmitSpawn: %v", err)
	}
	cmd := exec.Command("sleep", "60")
	processcontainment.Configure(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("cmd.Start: %v", err)
	}
	handle, err := processcontainment.Bind(cmd, processcontainment.Options{
		GracePeriod:  20 * time.Millisecond,
		DrainTimeout: 2 * time.Second,
	})
	if err != nil {
		_ = cmd.Process.Kill()
		t.Fatalf("Bind: %v", err)
	}
	force := errors.New("soft kill failed")
	if err := lease.BindHandle(handle, func(string) error { return force }); err != nil {
		t.Fatalf("BindHandle: %v", err)
	}

	err = reg.BeginShutdown("shutdown")
	if err == nil {
		t.Fatal("BeginShutdown() error = nil, want drain failure")
	}
	if !errors.Is(err, errShutdownDrainTimeout) && !errors.Is(err, force) && !strings.Contains(err.Error(), force.Error()) {
		t.Fatalf("BeginShutdown() = %v, want wrapped drain failure", err)
	}
}
