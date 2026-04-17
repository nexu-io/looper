package main

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/powerformer/looper/internal/bootstrap"
	"github.com/powerformer/looper/internal/config"
	"github.com/powerformer/looper/internal/version"
)

func TestRunPrintsVersionWithoutBootstrappingCommandHandling(t *testing.T) {
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}

	exitCode := run([]string{"--version"}, stdout, stderr)

	if exitCode != 0 {
		t.Fatalf("run([--version]) exit code = %d, want 0", exitCode)
	}

	if got, want := stdout.String(), version.Value+"\n"; got != want {
		t.Fatalf("run([--version]) stdout = %q, want %q", got, want)
	}

	if got := stderr.String(); got != "" {
		t.Fatalf("run([--version]) stderr = %q, want empty string", got)
	}
}

func TestRunPrefersVersionFlagOverOtherArguments(t *testing.T) {
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}

	exitCode := run([]string{"serve", "--version"}, stdout, stderr)

	if exitCode != 0 {
		t.Fatalf("run([serve --version]) exit code = %d, want 0", exitCode)
	}

	if got, want := stdout.String(), version.Value+"\n"; got != want {
		t.Fatalf("run([serve --version]) stdout = %q, want %q", got, want)
	}

	if got := stderr.String(); got != "" {
		t.Fatalf("run([serve --version]) stderr = %q, want empty string", got)
	}
}

func TestRunBootstrapsLooperdByDefault(t *testing.T) {
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	called := false

	exitCode := runWithDeps([]string{}, stdout, stderr, runDeps{
		bootstrapImpl: func(_ context.Context, options bootstrap.Options) (bootstrap.Result, error) {
			called = true
			if len(options.Args) != 0 {
				t.Fatalf("bootstrap args = %#v, want empty slice", options.Args)
			}
			if !options.WaitForShutdown {
				t.Fatal("bootstrap WaitForShutdown = false, want true")
			}
			return bootstrap.Result{}, nil
		},
	})

	if exitCode != 0 {
		t.Fatalf("runWithDeps([]) exit code = %d, want 0", exitCode)
	}
	if !called {
		t.Fatalf("bootstrapImpl was not called")
	}
	if got := stdout.String(); got != "" {
		t.Fatalf("runWithDeps([]) stdout = %q, want empty string", got)
	}
	if got := stderr.String(); got != "" {
		t.Fatalf("runWithDeps([]) stderr = %q, want empty string", got)
	}
}

func TestRunFormatsConfigValidationErrors(t *testing.T) {
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}

	exitCode := runWithDeps([]string{}, stdout, stderr, runDeps{
		bootstrapImpl: func(context.Context, bootstrap.Options) (bootstrap.Result, error) {
			return bootstrap.Result{}, &config.ConfigValidationError{Issues: []config.ValidationIssue{
				{Path: "server.port", Message: "must be an integer between 1 and 65535"},
				{Path: "daemon.logDir", Message: "must be a non-empty path"},
			}}
		},
	})

	if exitCode != 1 {
		t.Fatalf("runWithDeps([]) exit code = %d, want 1", exitCode)
	}
	const wantStderr = "looperd failed to start due to invalid configuration:\n- server.port: must be an integer between 1 and 65535\n- daemon.logDir: must be a non-empty path\n"
	if got := stderr.String(); got != wantStderr {
		t.Fatalf("runWithDeps([]) stderr = %q, want %q", got, wantStderr)
	}
	if got := stdout.String(); got != "" {
		t.Fatalf("runWithDeps([]) stdout = %q, want empty string", got)
	}
}

func TestRunPrintsBootstrapErrors(t *testing.T) {
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	wantErr := errors.New("runtime assembly has not been ported yet")

	exitCode := runWithDeps([]string{}, stdout, stderr, runDeps{
		bootstrapImpl: func(context.Context, bootstrap.Options) (bootstrap.Result, error) {
			return bootstrap.Result{}, wantErr
		},
	})

	if exitCode != 1 {
		t.Fatalf("runWithDeps([]) exit code = %d, want 1", exitCode)
	}
	if got, want := stderr.String(), "looperd: runtime assembly has not been ported yet\n"; got != want {
		t.Fatalf("runWithDeps([]) stderr = %q, want %q", got, want)
	}
	if got := stdout.String(); got != "" {
		t.Fatalf("runWithDeps([]) stdout = %q, want empty string", got)
	}
}
