package shell

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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
	if probe, probeErr := startCommand(context.Background(), Options{Command: script}, newBoundedBuffer(64), newBoundedBuffer(64)); probeErr == nil {
		_ = holder.Close()
		if waitErr := probe.Wait(); waitErr != nil {
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
