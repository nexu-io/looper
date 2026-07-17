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

func TestConfigureSetsProcessGroup(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "true")
	Configure(cmd)
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		t.Fatalf("Configure() Setpgid = %#v, want true", cmd.SysProcAttr)
	}
}

func TestRuntimePlatformsSupported(t *testing.T) {
	// Acceptance: runtime tests must exercise Darwin and Linux semantics.
	// CI runs linux; developer hosts commonly run darwin. Cross-compilation
	// alone is not this test — we require an actual Unix process-group OS.
	switch runtime.GOOS {
	case "darwin", "linux":
		t.Logf("running containment runtime tests on GOOS=%s GOARCH=%s", runtime.GOOS, runtime.GOARCH)
	default:
		t.Skipf("process containment runtime tests require darwin or linux, got %s", runtime.GOOS)
	}
}

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

	// Signal seam swallows KILL so the group stays runnable; Kill must not
	// report success and must return ErrNotConfirmedDead.
	cmd := exec.Command("/bin/sh", "-c", `while true; do sleep 0.05; done`)
	realKill := syscall.Kill
	var handle *Handle
	handle, err := Start(cmd, Options{
		GracePeriod:  10 * time.Millisecond,
		DrainTimeout: 80 * time.Millisecond,
		Signal: func(pid int, sig syscall.Signal) error {
			if sig == syscall.SIGKILL {
				return nil // pretend delivery succeeded without killing
			}
			if sig == 0 {
				// Keep group "runnable" forever for this test.
				return nil
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

func TestBindRequiresStartedProcess(t *testing.T) {
	_, err := Bind(exec.Command("/bin/true"), Options{})
	if err == nil {
		t.Fatal("Bind() error = nil, want error for unstarted command")
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

func requireUnixProcessGroup(t *testing.T) {
	t.Helper()
	switch runtime.GOOS {
	case "darwin", "linux":
	default:
		t.Skipf("requires darwin/linux process groups, got %s", runtime.GOOS)
	}
}

func waitForPIDFile(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			var pid int
			if _, scanErr := parsePID(string(data), &pid); scanErr == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for pid file %s", path)
	return 0
}

func parsePID(s string, pid *int) (int, error) {
	n, err := fmtSscanf(s, pid)
	return n, err
}

// fmtSscanf avoids importing fmt only for Sscanf in hot helpers while keeping
// tests readable; small wrapper for pid files that may include trailing newlines.
func fmtSscanf(s string, pid *int) (int, error) {
	var n int
	for _, r := range s {
		if r < '0' || r > '9' {
			break
		}
		n = n*10 + int(r-'0')
	}
	if n <= 0 {
		return 0, errors.New("no pid")
	}
	*pid = n
	return 1, nil
}

func assertProcessRunning(t *testing.T, pid int) {
	t.Helper()
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("pid %d not running: %v", pid, err)
	}
}

func assertProcessDead(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("pid %d still running", pid)
}
