package runtime

import (
	"context"
	"errors"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/agent"
	"github.com/nexu-io/looper/internal/processcontainment"
)

// BeginLoopStop must report drain failure when a pending Start→BindHandle
// window never closes within killBudget (wedged executor after cmd.Start).
func TestBeginLoopStopReturnsErrorWhenPendingSpawnWaitTimesOut(t *testing.T) {
	t.Parallel()
	reg := NewActiveExecutionRegistry()
	reg.killTimeout = 40 * time.Millisecond

	lease, err := reg.AdmitSpawn(context.Background(), agent.SpawnMeta{
		LoopID: "loop-wait-timeout", RunID: "run-wt", ExecutionID: "exec-wt",
	})
	if err != nil {
		t.Fatalf("AdmitSpawn: %v", err)
	}
	// Leave the lease pending without BindHandle/Release so spawnDone never closes.

	_, stopErr := reg.BeginLoopStop("loop-wait-timeout", "looper stop")
	if stopErr == nil {
		t.Fatal("BeginLoopStop error = nil, want pending spawn wait timeout")
	}
	if !errors.Is(stopErr, errLoopStopWaitTimeout) {
		t.Fatalf("BeginLoopStop error = %v, want errLoopStopWaitTimeout", stopErr)
	}
	if !reg.LoopStopActive("loop-wait-timeout") {
		t.Fatal("LoopStopActive = false after timed-out BeginLoopStop, want gate closed")
	}
	// Cleanup so the lease does not outlive the test.
	lease.Release()
}

func TestStopVsBindKillsAndConfirmedDrainsBeforeStartSuccess(t *testing.T) {
	t.Parallel()
	reg := NewActiveExecutionRegistry()
	reg.killTimeout = 5 * time.Second

	lease, err := reg.AdmitSpawn(context.Background(), agent.SpawnMeta{
		LoopID: "loop-1", RunID: "run-1", ExecutionID: "exec-1",
	})
	if err != nil {
		t.Fatalf("AdmitSpawn: %v", err)
	}

	// Start a long-lived process group leader.
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

	// Race: close loop admission while BindHandle is still pending. BeginLoopStop
	// waits for the pending spawn window, so BindHandle must run concurrently.
	stopDone := make(chan error, 1)
	go func() {
		// Intentionally keep the sticky gate (do not invoke release).
		_, stopErr := reg.BeginLoopStop("loop-1", "halt")
		stopDone <- stopErr
	}()
	deadline := time.Now().Add(2 * time.Second)
	for !reg.LoopStopActive("loop-1") {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for BeginLoopStop to set gate")
		}
		time.Sleep(time.Millisecond)
	}

	err = lease.BindHandle(handle, func(string) error { return nil })
	if !errors.Is(err, agent.ErrSpawnStoppedDuringBind) {
		t.Fatalf("BindHandle error = %v, want ErrSpawnStoppedDuringBind", err)
	}
	if !handle.ConfirmedDead() {
		t.Fatal("handle must be confirmed-dead after stop-vs-bind race")
	}
	select {
	case stopErr := <-stopDone:
		if stopErr != nil {
			t.Fatalf("BeginLoopStop: %v", stopErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("BeginLoopStop did not return after BindHandle ended the pending window")
	}
	if reg.LiveCount() != 0 {
		t.Fatalf("LiveCount = %d, want 0 after rejected bind", reg.LiveCount())
	}
	if reg.PendingCount() != 0 {
		t.Fatalf("PendingCount = %d, want 0 after rejected bind", reg.PendingCount())
	}
}

// BeginLoopStop must wait for a pending Start→BindHandle window so stop cannot
// return while a just-started process is live outside the registry.
func TestBeginLoopStopWaitsForPendingSpawn(t *testing.T) {
	t.Parallel()
	reg := NewActiveExecutionRegistry()
	reg.killTimeout = 5 * time.Second

	lease, err := reg.AdmitSpawn(context.Background(), agent.SpawnMeta{
		LoopID: "loop-pending-wait", RunID: "run-pw", ExecutionID: "exec-pw",
	})
	if err != nil {
		t.Fatalf("AdmitSpawn: %v", err)
	}

	// Process started after AdmitSpawn; BindHandle not yet called — registry
	// has no containment handle for BeginLoopStop's first drain pass.
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

	stopDone := make(chan error, 1)
	go func() {
		_, err := reg.BeginLoopStop("loop-pending-wait", "looper stop")
		stopDone <- err
	}()

	// Give BeginLoopStop time to set the gate and start waiting on spawnDone.
	time.Sleep(50 * time.Millisecond)
	select {
	case err := <-stopDone:
		t.Fatalf("BeginLoopStop returned early before BindHandle: %v", err)
	default:
	}

	bindErr := lease.BindHandle(handle, func(string) error { return nil })
	if !errors.Is(bindErr, agent.ErrSpawnStoppedDuringBind) {
		t.Fatalf("BindHandle = %v, want ErrSpawnStoppedDuringBind", bindErr)
	}
	if !handle.ConfirmedDead() {
		t.Fatal("pending spawn handle not confirmed-dead after refused BindHandle")
	}

	select {
	case err := <-stopDone:
		if err != nil {
			t.Fatalf("BeginLoopStop error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("BeginLoopStop did not return after pending spawn window ended")
	}
}

// When refuse-path killUnowned fails, BeginLoopStop must join that error: the
// handle never enters r.executions, so a closed spawnDone alone must not
// report stop success with an unconfirmed live agent.
func TestBeginLoopStopPropagatesUnownedBindDrainFailure(t *testing.T) {
	t.Parallel()
	reg := NewActiveExecutionRegistry()
	reg.killTimeout = 5 * time.Second

	lease, err := reg.AdmitSpawn(context.Background(), agent.SpawnMeta{
		LoopID: "loop-unowned-drain", RunID: "run-ud", ExecutionID: "exec-ud",
	})
	if err != nil {
		t.Fatalf("AdmitSpawn: %v", err)
	}

	cmd := exec.Command("sleep", "60")
	processcontainment.Configure(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("cmd.Start: %v", err)
	}
	// Inject signal failure so killUnowned cannot confirm death; the handle is
	// never inserted into the registry on the refuse path.
	signalFail := errors.New("synthetic signal delivery failure")
	handle, err := processcontainment.Bind(cmd, processcontainment.Options{
		GracePeriod:  50 * time.Millisecond,
		DrainTimeout: 2 * time.Second,
		Signal: func(pid int, sig syscall.Signal) error {
			return signalFail
		},
	})
	if err != nil {
		_ = cmd.Process.Kill()
		t.Fatalf("Bind: %v", err)
	}

	stopDone := make(chan error, 1)
	go func() {
		_, stopErr := reg.BeginLoopStop("loop-unowned-drain", "looper stop")
		stopDone <- stopErr
	}()
	deadline := time.Now().Add(2 * time.Second)
	for !reg.LoopStopActive("loop-unowned-drain") {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for BeginLoopStop to set gate")
		}
		time.Sleep(time.Millisecond)
	}

	bindErr := lease.BindHandle(handle, func(string) error { return nil })
	if !errors.Is(bindErr, agent.ErrSpawnStoppedDuringBind) {
		t.Fatalf("BindHandle = %v, want ErrSpawnStoppedDuringBind", bindErr)
	}
	if !errors.Is(bindErr, signalFail) {
		t.Fatalf("BindHandle = %v, want joined signalFail from killUnowned", bindErr)
	}

	select {
	case stopErr := <-stopDone:
		if stopErr == nil {
			t.Fatal("BeginLoopStop error = nil, want unowned killUnowned drain failure")
		}
		if !errors.Is(stopErr, signalFail) {
			t.Fatalf("BeginLoopStop error = %v, want signalFail published from lease", stopErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("BeginLoopStop did not return after refused BindHandle")
	}

	// Real cleanup: injected Signal blocked containment Kill.
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}
}
