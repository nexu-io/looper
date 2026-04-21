package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"sync"
	"syscall"
	"time"

	looperdapi "github.com/powerformer/looper/internal/api"
	"github.com/powerformer/looper/internal/bootstrap"
	"github.com/powerformer/looper/internal/config"
	"github.com/powerformer/looper/internal/eventlog"
	looperdruntime "github.com/powerformer/looper/internal/runtime"
	"github.com/powerformer/looper/internal/storage"
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

	if hasHelpArg(args) || (len(args) > 0 && args[0] == "help") {
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

type stopLoopResult struct {
	Stopped     bool   `json:"stopped"`
	LoopID      string `json:"loopId"`
	RunID       string `json:"runId,omitempty"`
	ExecutionID string `json:"executionId,omitempty"`
	Vendor      string `json:"vendor,omitempty"`
	PID         int64  `json:"pid,omitempty"`
}

type signalProcessFunc func(int, syscall.Signal) error

type executionMatchesProcessFunc func(context.Context, storage.AgentExecutionRecord, int) (bool, bool, error)

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
		StopLoop: func(ctx context.Context, loopID, reason string) (any, error) {
			return stopLoop(ctx, rt.Services(), loopID, reason, time.Now, syscall.Kill, rt.ExecutionMatchesProcess)
		},
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

func stopLoop(ctx context.Context, services looperdruntime.Services, loopID, reason string, now func() time.Time, signal signalProcessFunc, executionMatchesProcess executionMatchesProcessFunc) (any, error) {
	result := stopLoopResult{Stopped: false, LoopID: loopID}
	if services.Loops == nil {
		return nil, fmt.Errorf("loops service is not configured")
	}

	reasonCopy := reason
	paused, err := services.Loops.Pause(ctx, loopID, &reasonCopy)
	if err != nil {
		return nil, err
	}
	result.Stopped = true
	result.LoopID = paused.Loop.ID

	if services.Repositories == nil || services.Repositories.Runs == nil {
		return result, nil
	}

	latestRun, err := services.Repositories.Runs.GetLatestByLoopID(ctx, loopID)
	if err != nil || latestRun == nil || latestRun.Status != "running" {
		return result, err
	}
	result.RunID = latestRun.ID

	if services.Repositories.AgentExecutions == nil {
		return result, nil
	}

	latestExecution, err := services.Repositories.AgentExecutions.GetLatestByRunID(ctx, latestRun.ID)
	if err != nil || latestExecution == nil {
		return result, err
	}

	result.ExecutionID = latestExecution.ID
	result.Vendor = latestExecution.Vendor
	if latestExecution.PID == nil || *latestExecution.PID <= 0 {
		return result, nil
	}

	pid := int(*latestExecution.PID)
	if executionMatchesProcess != nil {
		matches, running, err := executionMatchesProcess(ctx, *latestExecution, pid)
		if err != nil {
			return nil, err
		}
		if !running || !matches {
			return result, nil
		}
	}
	result.PID = *latestExecution.PID
	if signal != nil {
		if err := signal(pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
			return nil, err
		}
	}

	updated := *latestExecution
	updated.Status = "cancelling"
	updated.UpdatedAt = eventlog.FormatJavaScriptISOString(now().UTC())
	if updated.ErrorMessage == nil {
		updated.ErrorMessage = &reasonCopy
	}
	if err := services.Repositories.AgentExecutions.Upsert(ctx, updated); err != nil {
		return nil, err
	}

	return result, nil
}

func hasVersionArg(args []string) bool {
	return slices.Contains(args, "--version")
}

func hasHelpArg(args []string) bool {
	return slices.ContainsFunc(args, isHelpArg)
}

func isHelpArg(arg string) bool {
	return arg == "-h" || arg == "--help"
}

func writeUsage(w io.Writer) {
	_, _ = fmt.Fprint(w, `looperd

Usage:
	looperd [flags]
	looperd help

Daemon and HTTP API server for Looper.
`)
}
