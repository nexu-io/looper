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
	if versionFlagIndex := indexOf(args, "--version"); versionFlagIndex >= 0 {
		args = append([]string{"version"}, append(args[:versionFlagIndex], args[versionFlagIndex+1:]...)...)
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

func indexOf(args []string, target string) int {
	for i, arg := range args {
		if arg == target {
			return i
		}
	}
	return -1
}
