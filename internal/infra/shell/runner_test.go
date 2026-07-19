package shell

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRunCapturesStdoutAndStderr(t *testing.T) {
	t.Parallel()
	result, err := Run(context.Background(), Options{
		Command: "/bin/sh",
		Args:    []string{"-c", `printf 'hello'; printf 'oops' >&2`},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0", result.ExitCode)
	}
	if result.Stdout != "hello" {
		t.Fatalf("Stdout = %q, want hello", result.Stdout)
	}
	if result.Stderr != "oops" {
		t.Fatalf("Stderr = %q, want oops", result.Stderr)
	}
}

// Contract (#592 review): StartGate wraps cmd.Start so callers can hold an
// external critical section across process launch only (not Wait).
func TestRunStartGateWrapsProcessStart(t *testing.T) {
	t.Parallel()

	var gateCalls, startCalls int
	result, err := Run(context.Background(), Options{
		Command: "/bin/sh",
		Args:    []string{"-c", `printf gated`},
		StartGate: func(start func() error) error {
			gateCalls++
			startCalls++
			return start()
		},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if gateCalls != 1 || startCalls != 1 {
		t.Fatalf("gateCalls=%d startCalls=%d, want 1 each", gateCalls, startCalls)
	}
	if result.Stdout != "gated" {
		t.Fatalf("Stdout = %q, want gated", result.Stdout)
	}
}

func TestRunStartGateRefusalDoesNotStartProcess(t *testing.T) {
	t.Parallel()

	refuse := errors.New("admission closed")
	_, err := Run(context.Background(), Options{
		Command: "/bin/sh",
		Args:    []string{"-c", `printf should-not-run`},
		StartGate: func(start func() error) error {
			return refuse
		},
	})
	if !errors.Is(err, refuse) {
		t.Fatalf("Run() error = %v, want refuse", err)
	}
}

func TestRunReturnsCommandExecutionErrorOnNonZeroExit(t *testing.T) {
	t.Parallel()
	result, err := Run(context.Background(), Options{
		Command: "/bin/sh",
		Args:    []string{"-c", `printf 'bad' >&2; exit 7`},
	})
	var commandErr *CommandExecutionError
	if !errors.As(err, &commandErr) {
		t.Fatalf("error = %v, want CommandExecutionError", err)
	}
	if commandErr.Result.ExitCode != 7 {
		t.Fatalf("ExitCode = %d, want 7", commandErr.Result.ExitCode)
	}
	if result.ExitCode != 7 {
		t.Fatalf("result.ExitCode = %d, want 7", result.ExitCode)
	}
	if commandErr.Result.Stderr != "bad" {
		t.Fatalf("Stderr = %q, want bad", commandErr.Result.Stderr)
	}
	if !strings.Contains(commandErr.Error(), "bad") {
		t.Fatalf("error = %q, want stderr detail", commandErr.Error())
	}
}

func TestRunIncludesStdoutWhenNonZeroExitHasNoStderr(t *testing.T) {
	t.Parallel()
	_, err := Run(context.Background(), Options{
		Command: "/bin/sh",
		Args:    []string{"-c", `printf 'useful output'; exit 2`},
	})
	var commandErr *CommandExecutionError
	if !errors.As(err, &commandErr) {
		t.Fatalf("error = %v, want CommandExecutionError", err)
	}
	if !strings.Contains(commandErr.Error(), "Command exited with code 2: useful output") {
		t.Fatalf("error = %q, want stdout detail", commandErr.Error())
	}
}

func TestRunTimesOutAndPreservesCapturedOutput(t *testing.T) {
	t.Parallel()
	result, err := Run(context.Background(), Options{
		Command:          "/bin/sh",
		Args:             []string{"-c", `printf 'start'; sleep 1`},
		Timeout:          500 * time.Millisecond,
		GracefulShutdown: 50 * time.Millisecond,
	})
	var commandErr *CommandExecutionError
	if !errors.As(err, &commandErr) {
		t.Fatalf("error = %v, want CommandExecutionError", err)
	}
	if commandErr.Message != "Command timed out" {
		t.Fatalf("Message = %q, want timeout", commandErr.Message)
	}
	if !strings.Contains(commandErr.Result.Stdout, "start") {
		t.Fatalf("Stdout = %q, want captured prefix", commandErr.Result.Stdout)
	}
	if result.Stdout != commandErr.Result.Stdout {
		t.Fatalf("result.Stdout = %q, want %q", result.Stdout, commandErr.Result.Stdout)
	}
}

func TestRunBoundsCapturedOutput(t *testing.T) {
	t.Parallel()
	result, err := Run(context.Background(), Options{
		Command:          "/bin/sh",
		Args:             []string{"-c", `printf 'abcdefghi'`},
		MaxCapturedBytes: 4,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Stdout != "abcd" {
		t.Fatalf("Stdout = %q, want abcd", result.Stdout)
	}
	if !result.StdoutTruncated {
		t.Fatal("StdoutTruncated = false, want true")
	}
}

func TestRunRespectsContextCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	_, err := Run(ctx, Options{
		Command:          "/bin/sh",
		Args:             []string{"-c", `sleep 1`},
		GracefulShutdown: 10 * time.Millisecond,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestIsTextFileBusy(t *testing.T) {
	t.Parallel()
	if !isTextFileBusy(syscall.ETXTBSY) {
		t.Fatal("isTextFileBusy(syscall.ETXTBSY) = false, want true")
	}
	if isTextFileBusy(os.ErrNotExist) {
		t.Fatal("isTextFileBusy(os.ErrNotExist) = true, want false")
	}
	if isTextFileBusy(nil) {
		t.Fatal("isTextFileBusy(nil) = true, want false")
	}
}

func TestRunRetriesStartOnTextFileBusy(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := filepath.Join(dir, "tool")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf ok\n"), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	// Keep an exclusive write fd open so the first Start hits ETXTBSY on Linux.
	// Release it shortly after Run begins so a later retry succeeds.
	holder, err := os.OpenFile(script, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open script for write hold: %v", err)
	}
	// Confirm this platform produces ETXTBSY under a write hold; skip otherwise
	// (some filesystems / kernels do not surface it for shell scripts).
	if probe, probeErr := startContainedCommand(context.Background(), Options{Command: script}, defaultGracefulStop, newBoundedBuffer(64), newBoundedBuffer(64)); probeErr == nil {
		_ = holder.Close()
		if waitErr := probe.Wait(context.Background()); waitErr != nil {
			t.Fatalf("probe Wait() error = %v", waitErr)
		}
		t.Skip("filesystem does not return ETXTBSY while script is open for write")
	} else if !isTextFileBusy(probeErr) {
		_ = holder.Close()
		t.Skipf("probe start error = %v, want ETXTBSY to exercise retry", probeErr)
	}

	release := make(chan struct{})
	go func() {
		<-release
		time.Sleep(15 * time.Millisecond)
		_ = holder.Close()
	}()

	close(release)
	result, err := Run(context.Background(), Options{Command: script})
	if err != nil {
		t.Fatalf("Run() error = %v, want retry past ETXTBSY", err)
	}
	if result.Stdout != "ok" {
		t.Fatalf("Stdout = %q, want ok", result.Stdout)
	}
}

func TestRunDrainsBackgroundChildAfterLeaderExitsWithPipes(t *testing.T) {
	t.Parallel()
	// Writer-based capture (bounded stdout/stderr) makes cmd.Wait block on copy
	// EOF when a same-group background child inherits the pipes after the
	// leader exits. Run must Drain that child instead of hanging in Wait.
	workDir := t.TempDir()
	childPIDPath := filepath.Join(workDir, "child.pid")
	script := `
set -e
(sleep 60) &
echo $! > child.pid
exit 0
`
	done := make(chan struct {
		result Result
		err    error
	}, 1)
	go func() {
		result, err := Run(context.Background(), Options{
			Command:          "/bin/sh",
			Args:             []string{"-c", script},
			CWD:              workDir,
			GracefulShutdown: 50 * time.Millisecond,
		})
		done <- struct {
			result Result
			err    error
		}{result, err}
	}()

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("Run() error = %v, want nil", got.err)
		}
		if got.result.ExitCode != 0 {
			t.Fatalf("ExitCode = %d, want 0", got.result.ExitCode)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run() hung after leader exit with background pipe-holding child")
	}

	// Child must not remain runnable after confirmed drain.
	data, err := os.ReadFile(childPIDPath)
	if err != nil {
		t.Fatalf("read child pid: %v", err)
	}
	var childPID int
	if _, scanErr := fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &childPID); scanErr != nil || childPID <= 0 {
		t.Fatalf("child pid file = %q, want positive pid", data)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if processIsNonRunnable(childPID) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("background child pid %d still runnable after shell normal-exit drain", childPID)
}

func TestRunCancelConfirmedDrainsBackgroundChild(t *testing.T) {
	t.Parallel()
	// Contract at the shell spawn boundary (#577): cancel must confirmed-drain
	// process-group descendants, not treat SIGTERM delivery alone as success.
	workDir := t.TempDir()
	childPIDPath := filepath.Join(workDir, "child.pid")
	script := `
set -e
(sleep 60) &
echo $! > child.pid
sleep 60
`
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := Run(ctx, Options{
			Command:          "/bin/sh",
			Args:             []string{"-c", script},
			CWD:              workDir,
			GracefulShutdown: 50 * time.Millisecond,
		})
		done <- err
	}()

	// Wait for background child PID file.
	var childPID int
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(childPIDPath)
		if err == nil {
			if _, scanErr := fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &childPID); scanErr == nil && childPID > 0 {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if childPID <= 0 {
		cancel()
		t.Fatal("child pid file not written")
	}
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run() error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not return after cancel")
	}

	// Child must not remain runnable after confirmed drain.
	// Use zombie-aware liveness: on Linux kill(pid, 0) still succeeds for
	// zombies, but containment treats zombie-only groups as non-runnable.
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if processIsNonRunnable(childPID) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("background child pid %d still runnable after shell cancel drain", childPID)
}

// processIsNonRunnable matches processcontainment confirmed-dead semantics:
// ESRCH, or a Linux zombie that kill(0) still addresses.
func processIsNonRunnable(pid int) bool {
	err := syscall.Kill(pid, 0)
	if errors.Is(err, syscall.ESRCH) {
		return true
	}
	if err != nil {
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
