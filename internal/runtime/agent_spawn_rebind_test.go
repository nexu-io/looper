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

func TestNativeResumeFallbackCancelledDoesNotSpawnSecondProcess(t *testing.T) {
	t.Parallel()
	reg := NewActiveExecutionRegistry()
	// Short budget: this test intentionally leaves the pending lease open so a
	// post-stop BindHandle can exercise refuse+kill. Production Start always
	// Release/BindHandle on cancel and closes spawnDone; here BeginLoopStop's
	// wait times out and must surface that as a drain error (not silent success).
	reg.killTimeout = 40 * time.Millisecond

	// Lease that is already cancelled simulates stop during attach-fail path.
	lease, err := reg.AdmitSpawn(context.Background(), agent.SpawnMeta{
		LoopID: "loop-fb", RunID: "run-fb", ExecutionID: "exec-fb",
	})
	if err != nil {
		t.Fatalf("AdmitSpawn: %v", err)
	}
	if _, err := reg.BeginLoopStop("loop-fb", "halt"); err != nil && !errors.Is(err, errLoopStopWaitTimeout) {
		t.Fatalf("BeginLoopStop: %v", err)
	}
	// Wait until lease context is cancelled.
	select {
	case <-lease.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("lease context not cancelled")
	}

	// BindHandle of a live process during stop must kill, not leave unowned.
	cmd := exec.Command("sleep", "30")
	processcontainment.Configure(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	handle, err := processcontainment.Bind(cmd, processcontainment.Options{
		GracePeriod:  20 * time.Millisecond,
		DrainTimeout: 2 * time.Second,
	})
	if err != nil {
		_ = cmd.Process.Kill()
		t.Fatalf("Bind: %v", err)
	}
	err = lease.BindHandle(handle, nil)
	if !errors.Is(err, agent.ErrSpawnStoppedDuringBind) {
		t.Fatalf("BindHandle = %v, want ErrSpawnStoppedDuringBind", err)
	}
	if !handle.ConfirmedDead() {
		t.Fatal("expected confirmed drain after cancelled bind")
	}
}

// BeginRebind must refuse after BeginLoopStop so fallback cannot Start a second
// process that stop already finished draining.
func TestBeginRebindRefusesWhenLoopStopping(t *testing.T) {
	t.Parallel()
	reg := NewActiveExecutionRegistry()
	reg.killTimeout = 5 * time.Second
	lease, err := reg.AdmitSpawn(context.Background(), agent.SpawnMeta{
		LoopID: "loop-rebind-refuse", RunID: "run-rr", ExecutionID: "exec-rr",
	})
	if err != nil {
		t.Fatalf("AdmitSpawn: %v", err)
	}
	sl, ok := lease.(*spawnLease)
	if !ok {
		t.Fatalf("lease type %T, want *spawnLease", lease)
	}
	// Bind so the lease leaves pending; BeginLoopStop then cancels the active lease.
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
	if _, err := reg.BeginLoopStop("loop-rebind-refuse", "looper stop"); err != nil {
		t.Fatalf("BeginLoopStop: %v", err)
	}
	if err := sl.BeginRebind(); !errors.Is(err, agent.ErrSpawnLoopStopping) {
		t.Fatalf("BeginRebind error = %v, want ErrSpawnLoopStopping", err)
	}
}

// BeginLoopStop must wait for an in-flight BeginRebind window so stop cannot
// return while fallback has started a process not yet refused/killed by RebindHandle.
// The refuse path must keep rebindDone open until killUnowned confirms the
// fallback handle is dead (it is never inserted into the registry).
func TestBeginLoopStopWaitsForInFlightRebind(t *testing.T) {
	t.Parallel()
	reg := NewActiveExecutionRegistry()
	reg.killTimeout = 5 * time.Second

	lease, err := reg.AdmitSpawn(context.Background(), agent.SpawnMeta{
		LoopID: "loop-rebind-wait", RunID: "run-rw", ExecutionID: "exec-rw",
	})
	if err != nil {
		t.Fatalf("AdmitSpawn: %v", err)
	}
	sl, ok := lease.(*spawnLease)
	if !ok {
		t.Fatalf("lease type %T, want *spawnLease", lease)
	}

	// Bind an initial handle so the lease is active (production path).
	cmd1 := exec.Command("sleep", "60")
	processcontainment.Configure(cmd1)
	if err := cmd1.Start(); err != nil {
		t.Fatalf("cmd1.Start: %v", err)
	}
	handle1, err := processcontainment.Bind(cmd1, processcontainment.Options{
		GracePeriod:  50 * time.Millisecond,
		DrainTimeout: 3 * time.Second,
	})
	if err != nil {
		_ = cmd1.Process.Kill()
		t.Fatalf("Bind1: %v", err)
	}
	if err := lease.BindHandle(handle1, func(string) error { return nil }); err != nil {
		t.Fatalf("BindHandle: %v", err)
	}

	if err := sl.BeginRebind(); err != nil {
		t.Fatalf("BeginRebind: %v", err)
	}

	// Fallback ignores SIGTERM so Kill spends grace before SIGKILL — widens the
	// window where a premature rebindDone close would let stop return early.
	cmd2 := exec.Command("sh", "-c", "trap '' TERM; sleep 60")
	processcontainment.Configure(cmd2)
	if err := cmd2.Start(); err != nil {
		t.Fatalf("cmd2.Start: %v", err)
	}
	const rebindGrace = 200 * time.Millisecond
	handle2, err := processcontainment.Bind(cmd2, processcontainment.Options{
		GracePeriod:  rebindGrace,
		DrainTimeout: 3 * time.Second,
	})
	if err != nil {
		_ = cmd2.Process.Kill()
		t.Fatalf("Bind2: %v", err)
	}

	stopDone := make(chan error, 1)
	go func() {
		_, err := reg.BeginLoopStop("loop-rebind-wait", "looper stop")
		stopDone <- err
	}()

	// Give BeginLoopStop time to set the gate and start waiting on rebindDone.
	time.Sleep(50 * time.Millisecond)
	select {
	case err := <-stopDone:
		t.Fatalf("BeginLoopStop returned early before RebindHandle: %v", err)
	default:
	}

	rebindDone := make(chan error, 1)
	go func() {
		rebindDone <- sl.RebindHandle(handle2, func(string) error { return nil })
	}()

	// While refuse-kill is still in its TERM/grace window, stop must not return
	// with the fallback handle still live (rebindDone closed too early).
	select {
	case err := <-stopDone:
		if !handle2.ConfirmedDead() {
			t.Fatalf("BeginLoopStop returned while refused fallback still alive: %v", err)
		}
		// Stop finished only after confirmed drain; still collect rebind result.
		select {
		case rebindErr := <-rebindDone:
			if !errors.Is(rebindErr, agent.ErrSpawnStoppedDuringBind) {
				t.Fatalf("RebindHandle = %v, want ErrSpawnStoppedDuringBind", rebindErr)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("RebindHandle did not return after BeginLoopStop")
		}
	case rebindErr := <-rebindDone:
		if !errors.Is(rebindErr, agent.ErrSpawnStoppedDuringBind) {
			t.Fatalf("RebindHandle = %v, want ErrSpawnStoppedDuringBind", rebindErr)
		}
		if !handle2.ConfirmedDead() {
			t.Fatal("fallback handle not confirmed-dead after refused RebindHandle")
		}
		select {
		case err := <-stopDone:
			if err != nil {
				t.Fatalf("BeginLoopStop error = %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("BeginLoopStop did not return after rebind window ended")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for RebindHandle / BeginLoopStop")
	}
	if !handle1.ConfirmedDead() {
		t.Fatal("original handle not drained by BeginLoopStop")
	}
	if !handle2.ConfirmedDead() {
		t.Fatal("fallback handle not confirmed-dead after stop/rebind")
	}
}
