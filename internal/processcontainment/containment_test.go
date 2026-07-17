package processcontainment

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// TestWaitPrefersCompletedReapOverCanceledContext ensures Wait does not return
// ctx.Err() when the leader is already reaped (both select cases ready).
// Returning cancel after reap makes Drain clear confirmedDead incorrectly.
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

func TestBindRequiresStartedProcess(t *testing.T) {
	_, err := Bind(exec.Command("/bin/true"), Options{})
	if err == nil {
		t.Fatal("Bind() error = nil, want error for unstarted command")
	}
}

func TestBindRejectsNonGroupLeader(t *testing.T) {
	requireUnixProcessGroup(t)

	// Started without Configure: child shares the caller's ambient process group.
	cmd := exec.Command("/bin/sh", "-c", "sleep 60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	_, err := Bind(cmd, Options{})
	if err == nil {
		t.Fatal("Bind() error = nil, want error when command is not process group leader")
	}
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

func waitForReadyFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for ready file %s", path)
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
	// Linux kill(0) also succeeds for zombies; require a non-zombie when possible.
	if runtime.GOOS == "linux" {
		if zombie, ok := linuxPIDIsZombie(pid); ok && zombie {
			t.Fatalf("pid %d is a zombie, want a runnable process", pid)
		}
	}
}

func assertProcessDead(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if processIsNonRunnable(pid) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("pid %d still running", pid)
}

// processIsNonRunnable matches package confirmed-dead semantics: ESRCH, or a
// Linux zombie that kill(0) still addresses. Zombie-only descendants must not
// fail tests after a successful Kill/Drain.
func processIsNonRunnable(pid int) bool {
	err := syscall.Kill(pid, 0)
	if errors.Is(err, syscall.ESRCH) {
		return true
	}
	if err != nil {
		// Unexpected probe error — do not treat as dead.
		return false
	}
	if runtime.GOOS == "linux" {
		if zombie, ok := linuxPIDIsZombie(pid); ok {
			return zombie
		}
	}
	return false
}

// linuxPIDIsZombie reports whether /proc/pid is a zombie (state Z).
// ok is false when the stat file cannot be read/parsed.
func linuxPIDIsZombie(pid int) (zombie bool, ok bool) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		// Process may have vanished between kill(0) and open.
		if errors.Is(err, os.ErrNotExist) {
			return true, true
		}
		return false, false
	}
	// Format: pid (comm) state ... — state is the first field after the final ") ".
	i := bytes.LastIndexByte(data, ')')
	if i < 0 || i+2 >= len(data) {
		return false, false
	}
	state := data[i+2]
	return state == 'Z' || state == 'X' || state == 'x', true
}
