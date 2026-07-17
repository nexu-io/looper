package processcontainment

import (
	"bytes"
	"errors"
	"os"
	"runtime"
	"strconv"
	"syscall"
	"testing"
	"time"
)

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
