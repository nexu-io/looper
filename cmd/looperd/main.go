package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"

	"github.com/powerformer/looper/internal/bootstrap"
	"github.com/powerformer/looper/internal/config"
	"github.com/powerformer/looper/internal/version"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

type bootstrapFunc func(context.Context, bootstrap.Options) (bootstrap.Result, error)

type runDeps struct {
	bootstrapImpl bootstrapFunc
	env           map[string]string
}

func run(args []string, stdout, stderr io.Writer) int {
	return runWithDeps(args, stdout, stderr, runDeps{})
}

func runWithDeps(args []string, stdout, stderr io.Writer, deps runDeps) int {
	if hasVersionArg(args) {
		_, _ = fmt.Fprintln(stdout, version.Value)
		return 0
	}

	if len(args) > 0 && (isHelpArg(args[0]) || args[0] == "help") {
		writeUsage(stdout)
		return 0
	}

	bootstrapImpl := deps.bootstrapImpl
	if bootstrapImpl == nil {
		bootstrapImpl = func(ctx context.Context, options bootstrap.Options) (bootstrap.Result, error) {
			options.StartRuntime = func(context.Context, bootstrap.RuntimeDependencies) (bootstrap.Runtime, error) {
				return nil, fmt.Errorf("runtime assembly has not been ported yet")
			}
			return bootstrap.Bootstrap(ctx, options)
		}
	}

	_, err := bootstrapImpl(context.Background(), bootstrap.Options{
		Args:   args,
		Env:    deps.env,
		Stdout: stdout,
		Stderr: stderr,
	})
	if err == nil {
		return 0
	}

	var validationErr *config.ConfigValidationError
	if errors.As(err, &validationErr) {
		_, _ = fmt.Fprintln(stderr, "looperd failed to start due to invalid configuration:")
		for _, issue := range validationErr.Issues {
			_, _ = fmt.Fprintf(stderr, "- %s: %s\n", issue.Path, issue.Message)
		}
		return 1
	}

	_, _ = fmt.Fprintf(stderr, "looperd: %v\n", err)
	return 1
}

func hasVersionArg(args []string) bool {
	return slices.Contains(args, "--version")
}

func isHelpArg(arg string) bool {
	return arg == "-h" || arg == "--help"
}

func writeUsage(w io.Writer) {
	_, _ = fmt.Fprint(w, `looperd (Go port)

Usage:
	looperd [flags]
	looperd help

Status:
	Bootstrap flow is ported. Signal handling, runtime assembly, and daemon command parity remain in progress.
`)
}
