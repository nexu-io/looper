package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"sync"
	"time"

	looperdapi "github.com/powerformer/looper/internal/api"
	"github.com/powerformer/looper/internal/bootstrap"
	"github.com/powerformer/looper/internal/config"
	looperdruntime "github.com/powerformer/looper/internal/runtime"
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
			options.StartRuntime = startRuntimeWithAPI
			return bootstrap.Bootstrap(ctx, options)
		}
	}

	_, err := bootstrapImpl(context.Background(), bootstrap.Options{
		Args:            args,
		Env:             deps.env,
		Stdout:          stdout,
		Stderr:          stderr,
		WaitForShutdown: true,
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

type daemonRuntime struct {
	runtime         *looperdruntime.Runtime
	server          *looperdapi.Server
	shutdownTimeout time.Duration
	stopOnce        sync.Once
}

func startRuntimeWithAPI(ctx context.Context, deps bootstrap.RuntimeDependencies) (bootstrap.Runtime, error) {
	runtimeValue, err := looperdruntime.Start(ctx, deps)
	if err != nil {
		return nil, err
	}

	rt, ok := runtimeValue.(*looperdruntime.Runtime)
	if !ok {
		return nil, fmt.Errorf("unexpected runtime type %T", runtimeValue)
	}

	handler := looperdapi.NewHandler(looperdapi.Context{
		Config:  deps.Config,
		Runtime: rt,
		TriggerSchedulerTick: func() {
			rt.TriggerSchedulerTick()
		},
	})
	server := looperdapi.NewServer(deps.Config, handler)
	if err := server.Start(); err != nil {
		rt.Stop("api server failed to start")
		rt.WaitForShutdown()
		return nil, err
	}

	shutdownTimeout := time.Duration(deps.Config.Daemon.ShutdownTimeoutMS) * time.Millisecond
	if shutdownTimeout <= 0 {
		shutdownTimeout = time.Second
	}

	return &daemonRuntime{
		runtime:         rt,
		server:          server,
		shutdownTimeout: shutdownTimeout,
	}, nil
}

func (d *daemonRuntime) Stop(reason string) {
	d.stopOnce.Do(func() {
		if d.server != nil {
			ctx, cancel := context.WithTimeout(context.Background(), d.shutdownTimeout)
			_ = d.server.Stop(ctx)
			cancel()
		}
		if d.runtime != nil {
			d.runtime.Stop(reason)
		}
	})
}

func (d *daemonRuntime) WaitForShutdown() {
	if d.runtime != nil {
		d.runtime.WaitForShutdown()
	}
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
	Bootstrap flow, signal handling, and runtime assembly are ported. Daemon command parity remains in progress.
`)
}
