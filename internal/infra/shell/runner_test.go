package shell

import (
	"context"
	"errors"
	"strings"
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
}

func TestRunTimesOutAndPreservesCapturedOutput(t *testing.T) {
	t.Parallel()
	result, err := Run(context.Background(), Options{
		Command:          "/bin/sh",
		Args:             []string{"-c", `printf 'start'; sleep 1`},
		Timeout:          50 * time.Millisecond,
		GracefulShutdown: 10 * time.Millisecond,
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
