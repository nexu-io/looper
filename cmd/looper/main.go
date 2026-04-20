package main

import (
	"context"
	"io"
	"os"

	"github.com/powerformer/looper/internal/cliapp"
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
