package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"slices"

	"github.com/powerformer/looper/internal/cliapp"
	"github.com/powerformer/looper/internal/version"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

type appRunner interface {
	Run(context.Context, []string) int
}

type runDeps struct {
	ctx    context.Context
	newApp func(cliapp.Deps) appRunner
}

func run(args []string, stdout, stderr io.Writer) int {
	return runWithDeps(args, stdout, stderr, runDeps{})
}

func runWithDeps(args []string, stdout, stderr io.Writer, deps runDeps) int {
	if slices.Contains(args, "--version") {
		_, _ = fmt.Fprintln(stdout, version.Value)
		return 0
	}

	ctx := deps.ctx
	if ctx == nil {
		ctx = context.Background()
	}

	newApp := deps.newApp
	if newApp == nil {
		newApp = func(appDeps cliapp.Deps) appRunner {
			return cliapp.New(appDeps)
		}
	}

	app := newApp(cliapp.Deps{Stdout: stdout, Stderr: stderr})
	return app.Run(ctx, args)
}
