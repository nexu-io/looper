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

// Contract: after BindHandle succeeds the lease leaves pending; BeginLoopStop
// must still cancel that bound lease so executor native-resume fallback cannot
// start/rebind a second process after haltLoop drained the first handle.
// BeginLoopStop must also confirmed-drain the bound handle so haltLoop does not
// rely on a durable AgentExecutionRecord that may not exist yet
// (BindHandle→persistStatus window).
func TestBeginLoopStopCancelsBoundActiveLease(t *testing.T) {
	t.Parallel()
	reg := NewActiveExecutionRegistry()
	reg.killTimeout = 5 * time.Second

	lease, err := reg.AdmitSpawn(context.Background(), agent.SpawnMeta{
		LoopID: "loop-bound", RunID: "run-bound", ExecutionID: "exec-bound",
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
		GracePeriod:  50 * time.Millisecond,
		DrainTimeout: 3 * time.Second,
	})
	if err != nil {
		_ = cmd.Process.Kill()
		t.Fatalf("Bind: %v", err)
	}
	if err := lease.BindHandle(handle, func(string) error { return nil }); err != nil {
		t.Fatalf("BindHandle: %v", err)
	}
	if reg.PendingCount() != 0 {
		t.Fatalf("PendingCount = %d after bind, want 0", reg.PendingCount())
	}
	if !reg.HasLiveHandle("loop-bound", "run-bound", "exec-bound") {
		t.Fatal("expected live handle after successful bind")
	}
	if lease.Context().Err() != nil {
		t.Fatalf("lease already cancelled before stop: %v", lease.Context().Err())
	}

	release, stopErr := reg.BeginLoopStop("loop-bound", "halt")
	if stopErr != nil {
		t.Fatalf("BeginLoopStop: %v", stopErr)
	}
	defer release()

	select {
	case <-lease.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("bound lease context not cancelled by BeginLoopStop")
	}
	// Fallback guard used by executor: cancelled lease blocks second spawn.
	if lease.Context().Err() == nil {
		t.Fatal("lease.Context().Err() = nil after BeginLoopStop, want cancelled")
	}

	// Confirmed drain of the post-BindHandle process (no separate Kill by ID).
	if !handle.ConfirmedDead() {
		t.Fatal("bound handle not confirmed-drained by BeginLoopStop")
	}
}

// BeginLoopStop must drain handles for the loop even when haltLoop cannot look
// up a durable execution id (registry-bound, not yet persisted).
func TestBeginLoopStopDrainsBoundHandleWithoutKillByID(t *testing.T) {
	t.Parallel()
	reg := NewActiveExecutionRegistry()
	reg.killTimeout = 5 * time.Second

	lease, err := reg.AdmitSpawn(context.Background(), agent.SpawnMeta{
		LoopID: "loop-pre-persist", RunID: "run-pre", ExecutionID: "exec-pre",
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
		GracePeriod:  50 * time.Millisecond,
		DrainTimeout: 3 * time.Second,
	})
	if err != nil {
		_ = cmd.Process.Kill()
		t.Fatalf("Bind: %v", err)
	}
	if err := lease.BindHandle(handle, func(string) error { return nil }); err != nil {
		t.Fatalf("BindHandle: %v", err)
	}

	// Simulate haltLoop with no durable execution: only BeginLoopStop, no Kill(id).
	release, stopErr := reg.BeginLoopStop("loop-pre-persist", "looper stop")
	if stopErr != nil {
		t.Fatalf("BeginLoopStop: %v", stopErr)
	}
	defer release()

	if !handle.ConfirmedDead() {
		t.Fatal("BeginLoopStop did not confirmed-drain handle without Kill-by-id")
	}
}

// Drain failures from killOwned must surface so haltLoop cannot report stop
// success while a BindHandle→persistStatus process is unconfirmed/live.
func TestBeginLoopStopPropagatesDrainFailure(t *testing.T) {
	t.Parallel()
	reg := NewActiveExecutionRegistry()
	reg.killTimeout = 5 * time.Second

	lease, err := reg.AdmitSpawn(context.Background(), agent.SpawnMeta{
		LoopID: "loop-drain-fail", RunID: "run-df", ExecutionID: "exec-df",
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
		GracePeriod:  50 * time.Millisecond,
		DrainTimeout: 3 * time.Second,
	})
	if err != nil {
		_ = cmd.Process.Kill()
		t.Fatalf("Bind: %v", err)
	}
	softFail := errors.New("soft kill failed")
	if err := lease.BindHandle(handle, func(string) error { return softFail }); err != nil {
		t.Fatalf("BindHandle: %v", err)
	}

	release, stopErr := reg.BeginLoopStop("loop-drain-fail", "looper stop")
	if release != nil {
		defer release()
	}
	if stopErr == nil {
		t.Fatal("BeginLoopStop error = nil, want soft-kill drain failure propagated")
	}
	if !errors.Is(stopErr, softFail) {
		t.Fatalf("BeginLoopStop error = %v, want softFail", stopErr)
	}
	// Handle drain still runs; soft failure alone must not hide gate/open release.
	if !handle.ConfirmedDead() {
		t.Fatal("handle not confirmed-dead after BeginLoopStop despite soft-kill error")
	}
	if !reg.LoopStopActive("loop-drain-fail") {
		t.Fatal("LoopStopActive = false after drain-failure BeginLoopStop, want gate closed")
	}
}

// After BeginLoopStop confirmed-drains a bound handle, releaseLease may delete
// the registry entry while handle.Kill waits. haltLoop's subsequent Kill-by-id
// must still return killed=true so a persisted PID does not raise
// ErrAgentLiveHandleMissing for a process stop already killed.
func TestKillReportsDrainedAfterBeginLoopStopRelease(t *testing.T) {
	t.Parallel()
	reg := NewActiveExecutionRegistry()
	reg.killTimeout = 5 * time.Second

	const (
		loopID = "loop-kill-after-drain"
		runID  = "run-kill-after-drain"
		execID = "exec-kill-after-drain"
	)
	lease, err := reg.AdmitSpawn(context.Background(), agent.SpawnMeta{
		LoopID: loopID, RunID: runID, ExecutionID: execID,
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
		GracePeriod:  50 * time.Millisecond,
		DrainTimeout: 3 * time.Second,
	})
	if err != nil {
		_ = cmd.Process.Kill()
		t.Fatalf("Bind: %v", err)
	}
	if err := lease.BindHandle(handle, func(string) error { return nil }); err != nil {
		t.Fatalf("BindHandle: %v", err)
	}

	release, stopErr := reg.BeginLoopStop(loopID, "looper stop")
	if stopErr != nil {
		t.Fatalf("BeginLoopStop: %v", stopErr)
	}
	defer release()

	// Simulate execution.run finishing and releaseLease deleting the entry after
	// the process dies under confirmed drain (entry may already be gone).
	lease.Release()
	if reg.HasLiveHandle(loopID, runID, execID) {
		t.Fatal("HasLiveHandle = true after Release, want entry removed")
	}
	if !handle.ConfirmedDead() {
		t.Fatal("handle not confirmed-dead after BeginLoopStop")
	}

	killed, killErr := reg.Kill(loopID, runID, execID, "looper stop")
	if killErr != nil {
		t.Fatalf("Kill after BeginLoopStop+Release error = %v", killErr)
	}
	if !killed {
		t.Fatal("Kill after BeginLoopStop+Release = false, want true (carry drained result)")
	}
}
