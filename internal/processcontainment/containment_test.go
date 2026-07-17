package processcontainment

import (
	"context"
	"errors"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// TestWaitPrefersCompletedReapOverCanceledContext ensures Wait does not return
// ctx.Err() when the leader is already reaped (both select cases ready).
func TestWaitPrefersCompletedReapOverCanceledContext(t *testing.T) {
	h := &Handle{
		waitCh:  make(chan struct{}),
		waitErr: nil,
	}
	close(h.waitCh)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := h.Wait(ctx); err != nil {
		t.Fatalf("Wait() error = %v, want nil when leader already reaped", err)
	}
	if !h.waitConsumed {
		t.Fatal("waitConsumed = false after Wait on closed waitCh, want true")
	}
}

// TestFailNotConfirmedPreservesConfirmedDead ensures overlapping cleanup
// failures never clear a confirmed-dead latch once set (reusable PGID guard).
func TestFailNotConfirmedPreservesConfirmedDead(t *testing.T) {
	h := &Handle{confirmedDead: true}
	err := h.failNotConfirmed(context.DeadlineExceeded)
	if !errors.Is(err, ErrNotConfirmedDead) {
		t.Fatalf("failNotConfirmed() error = %v, want ErrNotConfirmedDead", err)
	}
	if !h.ConfirmedDead() {
		t.Fatal("ConfirmedDead() = false after failNotConfirmed, want true (monotonic)")
	}
}

// TestSignalGroupNoOpAfterConfirmedDead ensures the exported low-level path
// does not signal -pgid after Kill/Drain confirmed death (reusable PGID guard).
func TestSignalGroupNoOpAfterConfirmedDead(t *testing.T) {
	signaled := 0
	h := &Handle{
		pgid:          12345,
		confirmedDead: true,
		signalFn: func(pid int, sig syscall.Signal) error {
			signaled++
			return nil
		},
	}
	if err := h.SignalGroup(syscall.SIGKILL); err != nil {
		t.Fatalf("SignalGroup(SIGKILL) error = %v, want nil after confirmed dead", err)
	}
	if err := h.SignalGroup(syscall.SIGTERM); err != nil {
		t.Fatalf("SignalGroup(SIGTERM) error = %v, want nil after confirmed dead", err)
	}
	if signaled != 0 {
		t.Fatalf("signalFn called %d times, want 0 after confirmed dead", signaled)
	}
	if h.termDelivered || h.killEscalated {
		t.Fatal("termDelivered/killEscalated set after no-op SignalGroup, want unchanged")
	}
}

// TestSignalGroupAfterWaitDoesNotSignalReusedPGID ensures Wait-then-SignalGroup
// cleanup does not deliver non-zero signals when kill(-pgid,0) succeeds only
// because a new process group reused the numeric PGID (confirmedDead still false).
func TestSignalGroupAfterWaitDoesNotSignalReusedPGID(t *testing.T) {
	var nonZeroSignals []syscall.Signal
	h := &Handle{
		pgid:   12345,
		waitCh: make(chan struct{}),
		signalFn: func(pid int, sig syscall.Signal) error {
			if sig == 0 {
				// Reused group: -pgid and +pgid both appear live.
				return nil
			}
			nonZeroSignals = append(nonZeroSignals, sig)
			return nil
		},
	}
	close(h.waitCh)
	if err := h.Wait(context.Background()); err != nil {
		t.Fatalf("Wait() error = %v, want nil", err)
	}

	if err := h.SignalGroup(syscall.SIGTERM); err != nil {
		t.Fatalf("SignalGroup(SIGTERM) error = %v, want nil on reused PGID", err)
	}
	if err := h.SignalGroup(syscall.SIGKILL); err != nil {
		t.Fatalf("SignalGroup(SIGKILL) error = %v, want nil on reused PGID", err)
	}
	if len(nonZeroSignals) != 0 {
		t.Fatalf("SignalGroup delivered %v, want none when PGID was reused after Wait", nonZeroSignals)
	}
	if h.termDelivered || h.killEscalated {
		t.Fatalf("termDelivered=%v killEscalated=%v, want both false when reuse blocked signaling", h.termDelivered, h.killEscalated)
	}
}

// TestSignalGroupAfterWaitDoesNotSignalEmptyGroup ensures Wait-then-SignalGroup
// is a no-op when the group is already empty (ESRCH on probe) before confirmedDead.
func TestSignalGroupAfterWaitDoesNotSignalEmptyGroup(t *testing.T) {
	var nonZeroSignals []syscall.Signal
	h := &Handle{
		pgid:   12345,
		waitCh: make(chan struct{}),
		signalFn: func(pid int, sig syscall.Signal) error {
			if sig == 0 {
				return syscall.ESRCH
			}
			nonZeroSignals = append(nonZeroSignals, sig)
			return nil
		},
	}
	close(h.waitCh)
	if err := h.Wait(context.Background()); err != nil {
		t.Fatalf("Wait() error = %v, want nil", err)
	}

	if err := h.SignalGroup(syscall.SIGKILL); err != nil {
		t.Fatalf("SignalGroup(SIGKILL) error = %v, want nil on empty group", err)
	}
	if len(nonZeroSignals) != 0 {
		t.Fatalf("SignalGroup delivered %v, want none on empty post-reap group", nonZeroSignals)
	}
	if h.killEscalated {
		t.Fatal("killEscalated = true after no-op SignalGroup, want false")
	}
}

// TestKillAfterWaitDrainsWithoutSignalingEmptyGroup ensures Wait-then-Kill
// cleanup does not send SIGTERM to -pgid when the leader is already reaped and
// the group is empty (reusable PGID guard).
func TestKillAfterWaitDrainsWithoutSignalingEmptyGroup(t *testing.T) {
	var nonZeroSignals []syscall.Signal
	h := &Handle{
		pgid:         12345,
		waitCh:       make(chan struct{}),
		gracePeriod:  -1,
		drainTimeout: time.Second,
		now:          time.Now,
		signalFn: func(pid int, sig syscall.Signal) error {
			if sig == 0 {
				// Empty group: no addressable members.
				return syscall.ESRCH
			}
			nonZeroSignals = append(nonZeroSignals, sig)
			return nil
		},
	}
	close(h.waitCh)
	// Simulate a prior Wait that already consumed the reaped leader.
	if err := h.Wait(context.Background()); err != nil {
		t.Fatalf("Wait() error = %v, want nil", err)
	}

	if err := h.Kill(context.Background()); err != nil {
		t.Fatalf("Kill() after Wait error = %v, want nil", err)
	}
	if len(nonZeroSignals) != 0 {
		t.Fatalf("Kill() delivered signals %v to -pgid, want none after Wait on empty group", nonZeroSignals)
	}
	if !h.ConfirmedDead() {
		t.Fatal("ConfirmedDead() = false after Kill on already-reaped empty group, want true")
	}
	if h.termDelivered || h.killEscalated {
		t.Fatalf("termDelivered=%v killEscalated=%v, want both false when no signal delivered", h.termDelivered, h.killEscalated)
	}
}

// TestKillAfterWaitDoesNotSignalReusedPGID ensures delayed cleanup after a
// reaped leader does not TERM/KILL when kill(-pgid,0) succeeds only because a
// new process group reused the numeric PGID (live process at pid==pgid).
func TestKillAfterWaitDoesNotSignalReusedPGID(t *testing.T) {
	var nonZeroSignals []syscall.Signal
	h := &Handle{
		pgid:         12345,
		waitCh:       make(chan struct{}),
		gracePeriod:  -1,
		drainTimeout: time.Second,
		now:          time.Now,
		signalFn: func(pid int, sig syscall.Signal) error {
			if sig == 0 {
				// Reused group: -pgid and +pgid both appear live.
				return nil
			}
			nonZeroSignals = append(nonZeroSignals, sig)
			return nil
		},
	}
	close(h.waitCh)
	if err := h.Wait(context.Background()); err != nil {
		t.Fatalf("Wait() error = %v, want nil", err)
	}

	if err := h.Kill(context.Background()); err != nil {
		t.Fatalf("Kill() after Wait error = %v, want nil on reused PGID", err)
	}
	if len(nonZeroSignals) != 0 {
		t.Fatalf("Kill() delivered signals %v, want none when PGID was reused after reap", nonZeroSignals)
	}
	if !h.ConfirmedDead() {
		t.Fatal("ConfirmedDead() = false after Kill on reused PGID, want true")
	}
	if h.termDelivered || h.killEscalated {
		t.Fatalf("termDelivered=%v killEscalated=%v, want both false when reuse blocked signaling", h.termDelivered, h.killEscalated)
	}
}

// TestGroupRunnableAfterReapWithDescendants still treats orphaned members as
// live when the leader PID is gone but -pgid remains addressable.
func TestGroupRunnableAfterReapWithDescendants(t *testing.T) {
	h := &Handle{
		pgid:   12345,
		waitCh: make(chan struct{}),
		signalFn: func(pid int, sig syscall.Signal) error {
			if sig != 0 {
				t.Fatalf("unexpected non-zero signal %v in groupRunnable probe", sig)
			}
			if pid == -12345 {
				return nil // descendants still in group
			}
			if pid == 12345 {
				return syscall.ESRCH // leader reaped, PID not recycled
			}
			t.Fatalf("unexpected pid %d in signalFn", pid)
			return nil
		},
		// Force the signal-0 path: a synthetic PGID is not visible in /proc on
		// Linux CI, which would otherwise report no live members and hide the
		// orphaned-descendant branch under test.
		groupLive: func(int) (bool, bool) { return false, false },
	}
	close(h.waitCh)
	if !h.groupRunnable() {
		t.Fatal("groupRunnable() = false with orphaned descendants, want true")
	}
}

func TestConfigureSetsProcessGroup(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "true")
	Configure(cmd)
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		t.Fatalf("Configure() Setpgid = %#v, want true", cmd.SysProcAttr)
	}
	if cmd.SysProcAttr.Pgid != 0 {
		t.Fatalf("Configure() Pgid = %d, want 0 (new group leader)", cmd.SysProcAttr.Pgid)
	}
	if cmd.SysProcAttr.Setsid {
		t.Fatal("Configure() Setsid = true, want false (conflicts with Setpgid)")
	}
}

func TestConfigureNormalizesInheritedSysProcAttr(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "true")
	// Caller left fields that would join an existing group or break Start.
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: false,
		Pgid:    99999,
		Setsid:  true,
	}
	Configure(cmd)
	if !cmd.SysProcAttr.Setpgid {
		t.Fatal("Configure() Setpgid = false, want true")
	}
	if cmd.SysProcAttr.Pgid != 0 {
		t.Fatalf("Configure() Pgid = %d, want 0", cmd.SysProcAttr.Pgid)
	}
	if cmd.SysProcAttr.Setsid {
		t.Fatal("Configure() Setsid = true, want false")
	}
}
