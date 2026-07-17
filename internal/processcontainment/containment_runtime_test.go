package processcontainment

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"
)

func TestKillTERMResistantChildReaped(t *testing.T) {
	requireUnixProcessGroup(t)

	workDir := t.TempDir()
	childPIDPath := filepath.Join(workDir, "child.pid")
	// Leader shells out a TERM-resistant child in the same process group, then
	// waits. Kill must escalate past TERM and reap the resistant child.
	script := `
set -e
(trap '' TERM; while true; do sleep 0.05; done) &
echo $! > "$CHILD_PID_FILE"
wait
`
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.Dir = workDir
	cmd.Env = append(os.Environ(), "CHILD_PID_FILE="+childPIDPath)

	handle, err := Start(cmd, Options{
		GracePeriod:  30 * time.Millisecond,
		DrainTimeout: 3 * time.Second,
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	childPID := waitForPIDFile(t, childPIDPath)
	assertProcessRunning(t, childPID)

	if err := handle.Kill(context.Background()); err != nil {
		t.Fatalf("Kill() error = %v", err)
	}
	if !handle.ConfirmedDead() {
		t.Fatalf("ConfirmedDead() = false after successful Kill")
	}
	assertProcessDead(t, childPID)
	assertProcessDead(t, handle.PID())
}

func TestNormalExitReapsBackgroundChildInGroup(t *testing.T) {
	requireUnixProcessGroup(t)

	workDir := t.TempDir()
	childPIDPath := filepath.Join(workDir, "child.pid")
	// Leader starts a background child and exits without waiting. Drain must
	// still force the group-owned descendant down and report confirmed-dead.
	script := `
set -e
(sleep 60) &
echo $! > "$CHILD_PID_FILE"
exit 0
`
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.Dir = workDir
	cmd.Env = append(os.Environ(), "CHILD_PID_FILE="+childPIDPath)

	handle, err := Start(cmd, Options{
		GracePeriod:  20 * time.Millisecond,
		DrainTimeout: 3 * time.Second,
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	childPID := waitForPIDFile(t, childPIDPath)
	if err := handle.Wait(context.Background()); err != nil {
		t.Fatalf("Wait() error = %v (leader should exit 0)", err)
	}
	assertProcessRunning(t, childPID)
	if handle.ConfirmedDead() {
		t.Fatal("ConfirmedDead() true after leader-only Wait; descendants still live")
	}

	if err := handle.Drain(context.Background()); err != nil {
		t.Fatalf("Drain() error = %v", err)
	}
	if !handle.ConfirmedDead() {
		t.Fatal("ConfirmedDead() = false after Drain cleaned background child")
	}
	assertProcessDead(t, childPID)
}

func TestSignalOnlyIsNeverSuccess(t *testing.T) {
	requireUnixProcessGroup(t)

	cmd := exec.Command("/bin/sh", "-c", `trap '' TERM; while true; do sleep 0.05; done`)
	handle, err := Start(cmd, Options{
		GracePeriod:  50 * time.Millisecond,
		DrainTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() {
		_ = handle.Kill(context.Background())
	})

	if err := handle.SignalGroup(syscall.SIGTERM); err != nil {
		t.Fatalf("SignalGroup(SIGTERM) error = %v", err)
	}
	snap := handle.Snapshot()
	if !snap.TermDelivered {
		t.Fatal("TermDelivered = false after SignalGroup(SIGTERM)")
	}
	if snap.ConfirmedDead || handle.ConfirmedDead() {
		t.Fatal("signal delivery alone reported ConfirmedDead success")
	}
	assertProcessRunning(t, handle.PID())
}

func TestKillTimeoutFailsLoud(t *testing.T) {
	requireUnixProcessGroup(t)

	// Leader ignores TERM so only KILL would stop it. The signal seam swallows
	// SIGKILL delivery while leaving the process actually running so Linux
	// /proc non-zombie probes still report groupRunnable (signal-0 alone is
	// insufficient on Linux once the real group is empty). Kill must not
	// report success and must return ErrNotConfirmedDead when drain times out.
	// Ready-file handshake avoids racing SIGTERM before trap is installed.
	//
	// Do not use set -e: group SIGTERM still kills sleep(1) children. With set -e
	// that non-zero exit ends the leader, the group becomes zombie-only, Linux
	// /proc probes report not-runnable, and Kill returns success instead of
	// timing out. Keep the shell alive and restart sleep after TERM.
	workDir := t.TempDir()
	readyPath := filepath.Join(workDir, "ready")
	script := `
trap '' TERM
echo ready > "$READY_FILE"
while true; do sleep 0.05 || true; done
`
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.Dir = workDir
	cmd.Env = append(os.Environ(), "READY_FILE="+readyPath)
	realKill := syscall.Kill
	var handle *Handle
	handle, err := Start(cmd, Options{
		GracePeriod:  10 * time.Millisecond,
		DrainTimeout: 80 * time.Millisecond,
		Signal: func(pid int, sig syscall.Signal) error {
			if sig == syscall.SIGKILL {
				return nil // pretend delivery succeeded without killing
			}
			return realKill(pid, sig)
		},
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() {
		// Force real cleanup of the test process group.
		_ = realKill(-handle.PGID(), syscall.SIGKILL)
		_ = realKill(handle.PID(), syscall.SIGKILL)
		_, _ = handle.cmd.Process.Wait()
	})

	waitForReadyFile(t, readyPath)
	// Process must still be live so confirmed-dead cannot succeed via /proc.
	assertProcessRunning(t, handle.PID())

	err = handle.Kill(context.Background())
	if err == nil {
		t.Fatal("Kill() error = nil, want explicit failure when not confirmed dead")
	}
	if !errors.Is(err, ErrNotConfirmedDead) {
		t.Fatalf("Kill() error = %v, want ErrNotConfirmedDead", err)
	}
	if handle.ConfirmedDead() {
		t.Fatal("ConfirmedDead() true after failed Kill")
	}
	assertProcessRunning(t, handle.PID())
}

func TestKillAfterNormalLifecycleConfirmsDead(t *testing.T) {
	requireUnixProcessGroup(t)

	cmd := exec.Command("/bin/sh", "-c", `while true; do sleep 0.05; done`)
	handle, err := Start(cmd, Options{
		GracePeriod:  20 * time.Millisecond,
		DrainTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if err := handle.Kill(context.Background()); err != nil {
		t.Fatalf("Kill() error = %v", err)
	}
	snap := handle.Snapshot()
	if !snap.ConfirmedDead || !snap.LeaderReaped {
		t.Fatalf("snapshot = %#v, want confirmed dead + reaped leader", snap)
	}
}

func TestKillIdempotentAfterConfirmedDead(t *testing.T) {
	requireUnixProcessGroup(t)

	cmd := exec.Command("/bin/sh", "-c", `while true; do sleep 0.05; done`)
	handle, err := Start(cmd, Options{
		GracePeriod:  20 * time.Millisecond,
		DrainTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if err := handle.Kill(context.Background()); err != nil {
		t.Fatalf("Kill() error = %v", err)
	}
	if !handle.ConfirmedDead() {
		t.Fatal("ConfirmedDead() = false after successful Kill")
	}
	// Second Kill must not re-signal a potentially reused pgid.
	if err := handle.Kill(context.Background()); err != nil {
		t.Fatalf("second Kill() error = %v, want nil after confirmed dead", err)
	}
}

func TestDarwinAndLinuxGroupSignalSemantics(t *testing.T) {
	requireUnixProcessGroup(t)

	// Runtime assertion shared by Darwin and Linux: negative pid addresses the
	// process group, signal 0 probes liveness, and ESRCH means non-runnable.
	cmd := exec.Command("/bin/sh", "-c", `while true; do sleep 0.05; done`)
	Configure(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	pid := cmd.Process.Pid
	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		t.Fatalf("Getpgid() error = %v", err)
	}
	if pgid != pid {
		t.Fatalf("pgid = %d, want leader pid %d after Setpgid", pgid, pid)
	}
	if err := syscall.Kill(-pgid, 0); err != nil {
		t.Fatalf("Kill(-pgid, 0) error = %v on %s, want group live", err, runtime.GOOS)
	}
	if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil {
		t.Fatalf("Kill(-pgid, SIGKILL) error = %v", err)
	}
	_, _ = cmd.Process.Wait()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(-pgid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("group %d still runnable after SIGKILL on %s", pgid, runtime.GOOS)
}
