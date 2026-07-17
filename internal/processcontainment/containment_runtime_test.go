package processcontainment

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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

// TestDrainWriterCaptureReapsBackgroundDescendant covers writer-based stdout
// capture (cmd.Stdout = io.Writer): after the leader exits, a background child
// that inherited the pipe keeps cmd.Wait blocked on copy-EOF. Drain must still
// TERM/KILL the group so Wait can finish and confirmed-dead succeeds.
func TestDrainWriterCaptureReapsBackgroundDescendant(t *testing.T) {
	requireUnixProcessGroup(t)

	workDir := t.TempDir()
	childPIDPath := filepath.Join(workDir, "child.pid")
	script := `
set -e
(sleep 60) &
echo $! > "$CHILD_PID_FILE"
exit 0
`
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.Dir = workDir
	cmd.Env = append(os.Environ(), "CHILD_PID_FILE="+childPIDPath)
	// Same pattern as agent stream capture: non-file Writers force pipe+copy
	// goroutines that only finish after every inherited write end closes.
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard

	handle, err := Start(cmd, Options{
		GracePeriod:  20 * time.Millisecond,
		DrainTimeout: 3 * time.Second,
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	childPID := waitForPIDFile(t, childPIDPath)
	// Do not call Wait first: that is the stuck path when descendants hold pipes.
	assertProcessRunning(t, childPID)

	if err := handle.Drain(context.Background()); err != nil {
		t.Fatalf("Drain() error = %v", err)
	}
	if !handle.ConfirmedDead() {
		t.Fatal("ConfirmedDead() = false after Drain with writer capture")
	}
	assertProcessDead(t, childPID)
	assertProcessDead(t, handle.PID())
}

func TestSignalOnlyIsNeverSuccess(t *testing.T) {
	requireUnixProcessGroup(t)

	// Ready-file handshake: Start only guarantees the shell is exec'd, not that
	// trap '' TERM is installed. Signaling early can kill the leader and leave
	// a zombie that assertProcessRunning rejects on Linux.
	workDir := t.TempDir()
	readyPath := filepath.Join(workDir, "ready")
	script := `
trap '' TERM
echo ready > "$READY_FILE"
while true; do sleep 0.05; done
`
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.Dir = workDir
	cmd.Env = append(os.Environ(), "READY_FILE="+readyPath)
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

	waitForReadyFile(t, readyPath)
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
