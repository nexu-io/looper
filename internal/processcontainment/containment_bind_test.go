package processcontainment

import (
	"context"
	"errors"
	"os/exec"
	"runtime"
	"testing"
)

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

func TestBindAllowsFastExitAfterConfigure(t *testing.T) {
	requireUnixProcessGroup(t)

	// Short-lived commands can exit before Bind's getpgid. Configure guarantees
	// the process was group leader while live, so Bind must still attach.
	for i := 0; i < 20; i++ {
		cmd := exec.Command("/bin/sh", "-c", "exit 7")
		Configure(cmd)
		if err := cmd.Start(); err != nil {
			t.Fatalf("Start() error = %v", err)
		}
		handle, err := Bind(cmd, Options{})
		if err != nil {
			t.Fatalf("Bind() after fast exit error = %v", err)
		}
		if err := handle.Wait(context.Background()); err != nil {
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				t.Fatalf("Wait() error = %v, want ExitError", err)
			}
		}
		if state := handle.ProcessState(); state == nil || state.ExitCode() != 7 {
			code := -1
			if state != nil {
				code = state.ExitCode()
			}
			t.Fatalf("ExitCode = %d, want 7", code)
		}
		if err := handle.Drain(context.Background()); err != nil {
			t.Fatalf("Drain() error = %v", err)
		}
		if !handle.ConfirmedDead() {
			t.Fatal("ConfirmedDead() = false after Drain of fast-exit leader")
		}
	}
}
