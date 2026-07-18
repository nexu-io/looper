package runtime

import (
	"context"
	"errors"
	"os/exec"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/agent"
	"github.com/nexu-io/looper/internal/processcontainment"
)

// Contract: durable stop must leave the per-loop gate closed after halt returns so
// in-flight runners that reach AgentExecutor.Start late cannot AdmitSpawn.
func TestBeginLoopStopStickyWithoutReleaseBlocksLateAdmitSpawn(t *testing.T) {
	t.Parallel()
	reg := NewActiveExecutionRegistry()
	// Simulate haltLoop: BeginLoopStop without invoking release.
	if _, err := reg.BeginLoopStop("loop-1", "looper stop"); err != nil {
		t.Fatalf("BeginLoopStop: %v", err)
	}
	if !reg.LoopStopActive("loop-1") {
		t.Fatal("LoopStopActive = false after BeginLoopStop without release")
	}
	_, err := reg.AdmitSpawn(context.Background(), agent.SpawnMeta{LoopID: "loop-1", RunID: "run-late", ExecutionID: "exec-late"})
	if !errors.Is(err, agent.ErrSpawnLoopStopping) {
		t.Fatalf("AdmitSpawn after sticky stop error = %v, want ErrSpawnLoopStopping", err)
	}
	// Intentional re-activation reopens admission.
	if was := reg.ClearLoopStop("loop-1"); !was {
		t.Fatal("ClearLoopStop wasActive = false, want true for sticky gate")
	}
	if reg.LoopStopActive("loop-1") {
		t.Fatal("LoopStopActive = true after ClearLoopStop")
	}
	if _, err := reg.AdmitSpawn(context.Background(), agent.SpawnMeta{LoopID: "loop-1", RunID: "run-resume", ExecutionID: "exec-resume"}); err != nil {
		t.Fatalf("AdmitSpawn after ClearLoopStop error = %v, want success", err)
	}
}

// ClearLoopStop must report the gate state it removed under the same lock so
// abort restore cannot miss a concurrent BeginLoopStop that raced a prior
// LoopStopActive sample.
func TestClearLoopStopReportsGateItRemoved(t *testing.T) {
	t.Parallel()
	reg := NewActiveExecutionRegistry()
	if was := reg.ClearLoopStop("loop-clear-report"); was {
		t.Fatal("ClearLoopStop on inactive gate wasActive = true, want false")
	}
	if _, err := reg.BeginLoopStop("loop-clear-report", "looper stop"); err != nil {
		t.Fatalf("BeginLoopStop: %v", err)
	}
	if !reg.LoopStopActive("loop-clear-report") {
		t.Fatal("LoopStopActive = false after BeginLoopStop")
	}
	if was := reg.ClearLoopStop("loop-clear-report"); !was {
		t.Fatal("ClearLoopStop wasActive = false after BeginLoopStop, want true")
	}
	if reg.LoopStopActive("loop-clear-report") {
		t.Fatal("LoopStopActive = true after ClearLoopStop, want open")
	}
	// Second clear of an already-open gate reports false.
	if was := reg.ClearLoopStop("loop-clear-report"); was {
		t.Fatal("ClearLoopStop second call wasActive = true, want false")
	}
}

// RestoreLoopStop must cancel/drain leases admitted while the gate was cleared
// for a failed reactivation (retry/start/worker-reuse TX abort).
func TestRestoreLoopStopDrainsLeasesAdmittedDuringClear(t *testing.T) {
	t.Parallel()
	reg := NewActiveExecutionRegistry()
	reg.killTimeout = 5 * time.Second

	if _, err := reg.BeginLoopStop("loop-restore", "looper stop"); err != nil {
		t.Fatalf("BeginLoopStop: %v", err)
	}
	// Intentional reactivation opens the gate before the TX; a stale runner can
	// AdmitSpawn+BindHandle in this window if the TX later aborts.
	if was := reg.ClearLoopStop("loop-restore"); !was {
		t.Fatal("ClearLoopStop wasActive = false before restore window, want true")
	}

	lease, err := reg.AdmitSpawn(context.Background(), agent.SpawnMeta{
		LoopID: "loop-restore", RunID: "run-stale", ExecutionID: "exec-stale",
	})
	if err != nil {
		t.Fatalf("AdmitSpawn during clear window: %v", err)
	}
	cmd := exec.Command("sleep", "60")
	processcontainment.Configure(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("cmd.Start: %v", err)
	}
	handle, err := processcontainment.Bind(cmd, processcontainment.Options{
		GracePeriod:  50 * time.Millisecond,
		DrainTimeout: 3 * time.Second,
	})
	if err != nil {
		_ = cmd.Process.Kill()
		t.Fatalf("Bind: %v", err)
	}
	if err := lease.BindHandle(handle, func(string) error { return nil }); err != nil {
		t.Fatalf("BindHandle during clear window: %v", err)
	}

	// TX failed: restore sticky gate and drain anything admitted in the window.
	if err := reg.RestoreLoopStop("loop-restore"); err != nil {
		t.Fatalf("RestoreLoopStop: %v", err)
	}
	if !reg.LoopStopActive("loop-restore") {
		t.Fatal("LoopStopActive = false after RestoreLoopStop")
	}
	if !handle.ConfirmedDead() {
		t.Fatal("bound handle not confirmed-dead after RestoreLoopStop")
	}
	if lease.Context().Err() == nil {
		t.Fatal("lease context not cancelled by RestoreLoopStop")
	}
	_, admitErr := reg.AdmitSpawn(context.Background(), agent.SpawnMeta{
		LoopID: "loop-restore", RunID: "run-late", ExecutionID: "exec-late",
	})
	if !errors.Is(admitErr, agent.ErrSpawnLoopStopping) {
		t.Fatalf("AdmitSpawn after RestoreLoopStop error = %v, want ErrSpawnLoopStopping", admitErr)
	}
}

// RestoreLoopStop must add its own sticky ref even when a temporary
// BeginLoopStop already holds a count, so the temporary release cannot reopen
// AdmitSpawn after a failed reactivation.
func TestRestoreLoopStopSurvivesTemporaryBeginLoopStopRelease(t *testing.T) {
	t.Parallel()
	reg := NewActiveExecutionRegistry()

	// Durable sticky stop (haltLoop style: BeginLoopStop without release).
	if _, err := reg.BeginLoopStop("loop-restore-sticky", "looper stop"); err != nil {
		t.Fatalf("BeginLoopStop sticky: %v", err)
	}
	if was := reg.ClearLoopStop("loop-restore-sticky"); !was {
		t.Fatal("ClearLoopStop wasActive = false, want true")
	}

	// Concurrent temporary stop window (e.g. stopCandidateExecution kill path).
	tempRelease, err := reg.BeginLoopStop("loop-restore-sticky", "candidate kill window")
	if err != nil {
		t.Fatalf("BeginLoopStop temporary: %v", err)
	}

	// Failed reactivation restores the sticky gate while temp is still active.
	if err := reg.RestoreLoopStop("loop-restore-sticky"); err != nil {
		t.Fatalf("RestoreLoopStop: %v", err)
	}
	if !reg.LoopStopActive("loop-restore-sticky") {
		t.Fatal("LoopStopActive = false after RestoreLoopStop, want closed")
	}

	// Temporary release must not clear the restored sticky reference.
	tempRelease()
	if !reg.LoopStopActive("loop-restore-sticky") {
		t.Fatal("LoopStopActive = false after temporary release, want sticky restore to remain")
	}
	_, admitErr := reg.AdmitSpawn(context.Background(), agent.SpawnMeta{
		LoopID: "loop-restore-sticky", RunID: "run-late", ExecutionID: "exec-late",
	})
	if !errors.Is(admitErr, agent.ErrSpawnLoopStopping) {
		t.Fatalf("AdmitSpawn after temp release error = %v, want ErrSpawnLoopStopping", admitErr)
	}
}
