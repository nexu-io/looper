package main

import (
	"bytes"
	"context"
	"testing"

	"github.com/nexu-io/looper/internal/cliapp"
	"github.com/nexu-io/looper/internal/version"
)

type contextKey struct{}

type fakeApp struct {
	run func(context.Context, []string) int
}

func (f fakeApp) Run(ctx context.Context, args []string) int {
	return f.run(ctx, args)
}

func TestRunWithDepsBuildsAppWithInjectedStreams(t *testing.T) {
	t.Parallel()

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	ctx := context.WithValue(context.Background(), contextKey{}, "sentinel")
	called := false

	exitCode := runWithDeps([]string{"status", "--json"}, stdout, stderr, runDeps{
		ctx: ctx,
		newApp: func(deps cliapp.Deps) appRunner {
			called = true
			if _, err := deps.Stdout.Write([]byte("stdout-ready\n")); err != nil {
				t.Fatalf("write stdout sentinel: %v", err)
			}
			if _, err := deps.Stderr.Write([]byte("stderr-ready\n")); err != nil {
				t.Fatalf("write stderr sentinel: %v", err)
			}

			return fakeApp{run: func(runCtx context.Context, args []string) int {
				if runCtx != ctx {
					t.Fatalf("run context = %v, want injected context", runCtx)
				}
				if got, want := len(args), 2; got != want {
					t.Fatalf("len(args) = %d, want %d", got, want)
				}
				if got, want := args[0], "status"; got != want {
					t.Fatalf("args[0] = %q, want %q", got, want)
				}
				if got, want := args[1], "--json"; got != want {
					t.Fatalf("args[1] = %q, want %q", got, want)
				}
				return 23
			}}
		},
	})

	if !called {
		t.Fatal("newApp was not called")
	}
	if got, want := exitCode, 23; got != want {
		t.Fatalf("runWithDeps(...) exit code = %d, want %d", got, want)
	}
	if got, want := stdout.String(), "stdout-ready\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if got, want := stderr.String(), "stderr-ready\n"; got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}
}

func TestRunWithDepsUsesBackgroundContextByDefault(t *testing.T) {
	t.Parallel()

	exitCode := runWithDeps(nil, &bytes.Buffer{}, &bytes.Buffer{}, runDeps{
		newApp: func(cliapp.Deps) appRunner {
			return fakeApp{run: func(ctx context.Context, _ []string) int {
				if ctx == nil {
					t.Fatal("context = nil, want background context")
				}
				if deadline, ok := ctx.Deadline(); ok {
					t.Fatalf("context deadline = %v, want none", deadline)
				}
				select {
				case <-ctx.Done():
					t.Fatalf("context done unexpectedly: %v", ctx.Err())
				default:
				}
				return 0
			}}
		},
	})

	if exitCode != 0 {
		t.Fatalf("runWithDeps(nil, ...) exit code = %d, want 0", exitCode)
	}
}

func TestRunUsesDefaultCLIAppFactory(t *testing.T) {
	t.Parallel()

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}

	exitCode := run([]string{"--help"}, stdout, stderr)

	if exitCode != 0 {
		t.Fatalf("run([--help]) exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if got := stderr.String(); got != "" {
		t.Fatalf("run([--help]) stderr = %q, want empty string", got)
	}
	if got := stdout.String(); got == "" {
		t.Fatal("run([--help]) stdout = empty string, want help output")
	}
	for _, want := range []string{"Usage:", "Subcommands:", "Flags:"} {
		if !bytes.Contains(stdout.Bytes(), []byte(want)) {
			t.Fatalf("run([--help]) stdout = %q, want to contain %q", stdout.String(), want)
		}
	}
}

func TestRunWithDepsVersionShortCircuitsBeforeAppConstruction(t *testing.T) {
	t.Parallel()

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	called := false

	exitCode := runWithDeps([]string{"--version"}, stdout, stderr, runDeps{
		newApp: func(cliapp.Deps) appRunner {
			called = true
			return fakeApp{run: func(context.Context, []string) int { return 99 }}
		},
	})

	if exitCode != 0 {
		t.Fatalf("runWithDeps([--version]) exit code = %d, want 0", exitCode)
	}
	if called {
		t.Fatal("newApp was called for --version")
	}
	if got, want := stdout.String(), version.Value+"\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if got := stderr.String(); got != "" {
		t.Fatalf("stderr = %q, want empty string", got)
	}
}

func TestRunUsesDefaultCLIAppFactoryForVersionCommand(t *testing.T) {
	t.Parallel()

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}

	exitCode := run([]string{"version"}, stdout, stderr)

	if exitCode != 0 {
		t.Fatalf("run([version]) exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if got := stderr.String(); got != "" {
		t.Fatalf("run([version]) stderr = %q, want empty string", got)
	}
	if got, want := stdout.String(), version.Value+"\n"; got != want {
		t.Fatalf("run([version]) stdout = %q, want %q", got, want)
	}
}
