package main

import (
	"context"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/nexu-io/looper/internal/cliapp"
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
	if len(args) == 1 && args[0] == "--version" {
		args = []string{"version"}
	}

	ctx := deps.ctx
	if ctx == nil {
		var stop context.CancelFunc
		ctx, stop = signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
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
