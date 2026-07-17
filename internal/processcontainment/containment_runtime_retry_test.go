package processcontainment

import (
	"context"
	"errors"
	"os/exec"
	"runtime"
	"syscall"
	"testing"
	"time"
)

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

func TestDrainIdempotentAfterConfirmedDead(t *testing.T) {
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
	// Second Drain must no-op: re-probing a reaped pgid risks signaling a reused id.
	if err := handle.Drain(context.Background()); err != nil {
		t.Fatalf("second Drain() error = %v, want nil after confirmed dead", err)
	}
	if !handle.ConfirmedDead() {
		t.Fatal("ConfirmedDead() cleared by Drain after confirmation")
	}
}

// TestStartStdoutPipeSurvivesShortLivedLeader ensures Start does not arm Wait
// before returning, so StdoutPipe readers can drain short-lived producers.
func TestStartStdoutPipeSurvivesShortLivedLeader(t *testing.T) {
	requireUnixProcessGroup(t)

	cmd := exec.Command("/bin/sh", "-c", `printf 'hello-from-pipe'`)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe() error = %v", err)
	}
	handle, err := Start(cmd, Options{
		DrainTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	// Read after Start returns; Wait must not have closed the pipe yet.
	buf := make([]byte, 64)
	n, readErr := stdout.Read(buf)
	if readErr != nil && n == 0 {
		t.Fatalf("stdout.Read() error = %v (pipe closed before reader started?)", readErr)
	}
	got := string(buf[:n])
	if got != "hello-from-pipe" {
		t.Fatalf("stdout = %q, want %q", got, "hello-from-pipe")
	}
	if err := handle.Wait(context.Background()); err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if err := handle.Drain(context.Background()); err != nil {
		t.Fatalf("Drain() error = %v", err)
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
		// Match package confirmed-dead semantics: on Linux, kill(-pgid, 0) can
		// still succeed for zombie-only groups, which are not runnable.
		if !groupStillRunnable(pgid) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("group %d still runnable after SIGKILL on %s", pgid, runtime.GOOS)
}

// groupStillRunnable mirrors Handle.groupRunnable for tests outside a Handle:
// signal-0 liveness plus the Linux non-zombie /proc filter.
func groupStillRunnable(pgid int) bool {
	err := syscall.Kill(-pgid, 0)
	if err != nil {
		// ESRCH (or equivalent) => no addressable group members.
		return !errors.Is(err, syscall.ESRCH)
	}
	if live, ok := groupHasNonZombieMember(pgid); ok {
		return live
	}
	return true
}
