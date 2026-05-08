package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
	"sync"
	"syscall"
	"time"

	looperdapi "github.com/nexu-io/looper/internal/api"
	"github.com/nexu-io/looper/internal/bootstrap"
	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/domain"
	"github.com/nexu-io/looper/internal/eventlog"
	looperdruntime "github.com/nexu-io/looper/internal/runtime"
	"github.com/nexu-io/looper/internal/storage"
	"github.com/nexu-io/looper/internal/version"
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
		StopAll: func(ctx context.Context, reason string) (any, error) {
			return stopAllLoops(ctx, rt.Services(), reason, time.Now, syscall.Kill, rt.ExecutionMatchesProcess)
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
	if !isStoppableExecutionStatus(latestExecution.Status) {
		return result, nil
	}
	if services.ActiveExecutions != nil {
		killed, err := services.ActiveExecutions.Kill(result.LoopID, latestRun.ID, latestExecution.ID, reason)
		if err != nil {
			return nil, err
		}
		if killed {
			if err := markExecutionCancelling(ctx, services, *latestExecution, reasonCopy, now); err != nil {
				return nil, err
			}
			return result, nil
		}
	}
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
		if err := signalAgentProcessGroup(pid, signal, 5*time.Second); err != nil {
			return nil, err
		}
	}

	if err := markExecutionCancelling(ctx, services, *latestExecution, reasonCopy, now); err != nil {
		return nil, err
	}

	return result, nil
}

type stopAllResult string

const (
	stopAllResultStopped         stopAllResult = "stopped"
	stopAllResultAlreadyFinished stopAllResult = "alreadyFinished"
	stopAllResultAlreadyStopping stopAllResult = "alreadyStopping"
	stopAllResultFailed          stopAllResult = "failed"
)

type stopAllSummary struct {
	Total           int `json:"total"`
	Stopped         int `json:"stopped"`
	AlreadyFinished int `json:"alreadyFinished"`
	AlreadyStopping int `json:"alreadyStopping"`
	Failed          int `json:"failed"`
}

type stopAllItem struct {
	LoopID                  string `json:"loopId,omitempty"`
	Seq                     int64  `json:"seq,omitempty"`
	Type                    string `json:"type,omitempty"`
	RunID                   string `json:"runId,omitempty"`
	ExecutionID             string `json:"executionId,omitempty"`
	PreviousLoopStatus      string `json:"previousLoopStatus,omitempty"`
	PreviousRunStatus       string `json:"previousRunStatus,omitempty"`
	PreviousExecutionStatus string `json:"previousExecutionStatus,omitempty"`
	Result                  string `json:"result"`
	Error                   string `json:"error,omitempty"`
}

type stopAllResponse struct {
	Summary stopAllSummary `json:"summary"`
	Items   []stopAllItem  `json:"items"`
}

type stopAllCandidate struct {
	Loop        storage.LoopRecord
	Run         *storage.RunRecord
	Execution   *storage.AgentExecutionRecord
	Executions  []storage.AgentExecutionRecord
	ActiveQueue bool
}

func stopAllLoops(ctx context.Context, services looperdruntime.Services, reason string, now func() time.Time, signal signalProcessFunc, executionMatchesProcess executionMatchesProcessFunc) (stopAllResponse, error) {
	if services.Loops == nil {
		return stopAllResponse{}, fmt.Errorf("loops service is not configured")
	}
	if services.Repositories == nil || services.Repositories.Loops == nil || services.Repositories.Runs == nil || services.Repositories.Queue == nil || services.Repositories.AgentExecutions == nil {
		return stopAllResponse{}, fmt.Errorf("storage is not configured")
	}

	candidates, err := collectStopAllCandidates(ctx, services.Repositories)
	if err != nil {
		return stopAllResponse{}, err
	}

	response := stopAllResponse{Items: make([]stopAllItem, 0, len(candidates))}
	for _, candidate := range candidates {
		item := stopAllItem{
			LoopID:             candidate.Loop.ID,
			Seq:                candidate.Loop.Seq,
			Type:               candidate.Loop.Type,
			PreviousLoopStatus: candidate.Loop.Status,
		}
		if candidate.Run != nil {
			item.RunID = candidate.Run.ID
			item.PreviousRunStatus = candidate.Run.Status
		}
		if candidate.Execution != nil {
			item.ExecutionID = candidate.Execution.ID
			item.PreviousExecutionStatus = candidate.Execution.Status
		}

		item.Result = string(classifyStopAllResult(candidate))
		if item.Result == string(stopAllResultAlreadyFinished) || item.Result == string(stopAllResultAlreadyStopping) {
			response.Items = append(response.Items, item)
			continue
		}

		_, err := stopLoop(ctx, services, candidate.Loop.ID, reason, now, signal, executionMatchesProcess)
		if err != nil {
			stopErr := err
			if candidate.Execution != nil && candidate.Execution.Status == "running" {
				if fallbackErr := stopCandidateExecution(ctx, services, candidate, reason, now, signal, executionMatchesProcess); fallbackErr != nil {
					err = errors.Join(stopErr, fallbackErr)
				} else {
					err = stopErr
				}
			}
			if refreshed, refreshErr := refreshStopAllCandidate(ctx, services.Repositories, candidate.Loop.ID); refreshErr == nil {
				refreshedResult := classifyStopAllResult(refreshed)
				if refreshedResult == stopAllResultAlreadyFinished || refreshedResult == stopAllResultAlreadyStopping {
					item.Result = string(refreshedResult)
				} else {
					item.Result = string(stopAllResultFailed)
					item.Error = err.Error()
				}
			} else {
				item.Result = string(stopAllResultFailed)
				item.Error = err.Error()
			}
		} else {
			for _, execution := range candidate.Executions {
				if execution.Status != "running" {
					continue
				}
				execCandidate := candidate
				execCandidate.Execution = &execution
				if execErr := stopCandidateExecution(ctx, services, execCandidate, reason, now, signal, executionMatchesProcess); execErr != nil && item.Error == "" {
					item.Result = string(stopAllResultFailed)
					item.Error = execErr.Error()
				}
			}
		}
		if item.Result == string(stopAllResultFailed) {
			// Keep the per-item failure while still processing remaining candidates.
		} else if refreshed, refreshErr := refreshStopAllCandidate(ctx, services.Repositories, candidate.Loop.ID); refreshErr != nil {
			item.Result = string(stopAllResultFailed)
			item.Error = refreshErr.Error()
		} else if candidate.Loop.Status != string(domain.LoopStatusWaiting) && classifyStopAllResult(refreshed) == stopAllResultAlreadyFinished {
			item.Result = string(stopAllResultAlreadyFinished)
		}
		response.Items = append(response.Items, item)
	}

	for _, item := range response.Items {
		response.Summary.Total++
		switch stopAllResult(item.Result) {
		case stopAllResultStopped:
			response.Summary.Stopped++
		case stopAllResultAlreadyFinished:
			response.Summary.AlreadyFinished++
		case stopAllResultAlreadyStopping:
			response.Summary.AlreadyStopping++
		case stopAllResultFailed:
			response.Summary.Failed++
		}
	}

	return response, nil
}

func collectStopAllCandidates(ctx context.Context, repos *storage.Repositories) ([]stopAllCandidate, error) {
	loopsList, err := repos.Loops.List(ctx)
	if err != nil {
		return nil, err
	}
	runsList, err := repos.Runs.List(ctx)
	if err != nil {
		return nil, err
	}
	executions, err := repos.AgentExecutions.ListActive(ctx)
	if err != nil {
		return nil, err
	}
	queueItems, err := repos.Queue.List(ctx)
	if err != nil {
		return nil, err
	}

	loopsByID := make(map[string]storage.LoopRecord, len(loopsList))
	for _, loop := range loopsList {
		loopsByID[loop.ID] = loop
	}
	bestRunByLoopID := make(map[string]storage.RunRecord)
	runByID := make(map[string]storage.RunRecord, len(runsList))
	for _, run := range runsList {
		runByID[run.ID] = run
		current, ok := bestRunByLoopID[run.LoopID]
		if !ok || (current.Status != "running" && run.Status == "running") {
			bestRunByLoopID[run.LoopID] = run
		}
	}
	activeExecutionsByLoopID := make(map[string][]storage.AgentExecutionRecord)
	for _, execution := range executions {
		loopID := ""
		if execution.LoopID != nil {
			loopID = *execution.LoopID
		}
		if loopID == "" && execution.RunID != nil {
			if run, ok := runByID[*execution.RunID]; ok {
				loopID = run.LoopID
			}
		}
		if loopID == "" {
			continue
		}
		activeExecutionsByLoopID[loopID] = append(activeExecutionsByLoopID[loopID], execution)
	}

	activeLoopIDs := make(map[string]struct{})
	for _, run := range runsList {
		if run.Status == "running" {
			activeLoopIDs[run.LoopID] = struct{}{}
		}
	}
	for _, loop := range loopsList {
		if isStopAllLoopStatus(loop.Status) {
			activeLoopIDs[loop.ID] = struct{}{}
		}
	}
	for _, item := range queueItems {
		if item.LoopID != nil && (item.Status == "queued" || item.Status == "running") {
			activeLoopIDs[*item.LoopID] = struct{}{}
		}
	}
	for loopID := range activeExecutionsByLoopID {
		activeLoopIDs[loopID] = struct{}{}
	}
	activeQueueByLoopID := make(map[string]bool)
	for _, item := range queueItems {
		if item.LoopID != nil && (item.Status == "queued" || item.Status == "running") {
			activeQueueByLoopID[*item.LoopID] = true
		}
	}

	candidates := make([]stopAllCandidate, 0, len(activeLoopIDs))
	for loopID := range activeLoopIDs {
		loop, ok := loopsByID[loopID]
		if !ok {
			continue
		}
		candidate := stopAllCandidate{Loop: loop}
		if run, ok := bestRunByLoopID[loopID]; ok {
			candidate.Run = &run
		}
		if executions, ok := activeExecutionsByLoopID[loopID]; ok && len(executions) > 0 {
			candidate.Executions = executions
			candidate.Execution = &executions[0]
		}
		candidate.ActiveQueue = activeQueueByLoopID[loopID]
		candidates = append(candidates, candidate)
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Loop.Seq != candidates[j].Loop.Seq {
			return candidates[i].Loop.Seq < candidates[j].Loop.Seq
		}
		return candidates[i].Loop.ID < candidates[j].Loop.ID
	})
	return candidates, nil
}

func isStopAllLoopStatus(status string) bool {
	switch domain.LoopStatus(status) {
	case domain.LoopStatusQueued, domain.LoopStatusRunning, domain.LoopStatusWaiting:
		return true
	default:
		return false
	}
}

func refreshStopAllCandidate(ctx context.Context, repos *storage.Repositories, loopID string) (stopAllCandidate, error) {
	loop, err := repos.Loops.GetByID(ctx, loopID)
	if err != nil {
		return stopAllCandidate{}, err
	}
	if loop == nil {
		return stopAllCandidate{}, nil
	}
	candidate := stopAllCandidate{Loop: *loop}
	if queueItem, err := repos.Queue.FindActiveByLoopID(ctx, loopID); err != nil {
		return stopAllCandidate{}, err
	} else if queueItem != nil {
		candidate.ActiveQueue = true
	}
	runsList, err := repos.Runs.List(ctx)
	if err != nil {
		return stopAllCandidate{}, err
	}
	runByID := make(map[string]storage.RunRecord, len(runsList))
	for _, run := range runsList {
		runByID[run.ID] = run
	}
	if run, err := repos.Runs.GetLatestByLoopID(ctx, loopID); err != nil {
		return stopAllCandidate{}, err
	} else if run != nil {
		candidate.Run = run
	}
	executions, err := repos.AgentExecutions.ListActive(ctx)
	if err != nil {
		return stopAllCandidate{}, err
	}
	for _, execution := range executions {
		executionLoopID := ""
		if execution.LoopID != nil {
			executionLoopID = *execution.LoopID
		}
		if executionLoopID == "" && execution.RunID != nil {
			if run, ok := runByID[*execution.RunID]; ok {
				executionLoopID = run.LoopID
			}
		}
		if executionLoopID != loopID {
			continue
		}
		candidate.Executions = append(candidate.Executions, execution)
	}
	if len(candidate.Executions) > 0 {
		candidate.Execution = &candidate.Executions[0]
	}
	return candidate, nil
}

func classifyStopAllResult(candidate stopAllCandidate) stopAllResult {
	hasRunningExecution := false
	hasCancellingExecution := false
	for _, execution := range candidate.Executions {
		switch execution.Status {
		case "running":
			hasRunningExecution = true
		case "cancelling":
			hasCancellingExecution = true
		}
	}
	if candidate.Execution != nil {
		switch candidate.Execution.Status {
		case "running":
			hasRunningExecution = true
		case "cancelling":
			hasCancellingExecution = true
		}
	}
	if hasRunningExecution {
		return stopAllResultStopped
	}
	if hasCancellingExecution {
		if !isStopAllLoopStatus(candidate.Loop.Status) && !candidate.ActiveQueue {
			return stopAllResultAlreadyStopping
		}
	}
	if candidate.Run != nil && candidate.Run.Status != "" && candidate.Run.Status != "running" && !isStopAllLoopStatus(candidate.Loop.Status) && !candidate.ActiveQueue {
		return stopAllResultAlreadyFinished
	}
	return stopAllResultStopped
}

func stopCandidateExecution(ctx context.Context, services looperdruntime.Services, candidate stopAllCandidate, reason string, now func() time.Time, signal signalProcessFunc, executionMatchesProcess executionMatchesProcessFunc) error {
	if candidate.Execution == nil {
		return nil
	}
	runID := ""
	if candidate.Execution.RunID != nil {
		runID = *candidate.Execution.RunID
	}
	if runID == "" && candidate.Run != nil {
		runID = candidate.Run.ID
	}
	if services.ActiveExecutions != nil && runID != "" {
		killed, err := services.ActiveExecutions.Kill(candidate.Loop.ID, runID, candidate.Execution.ID, reason)
		if err != nil {
			return err
		}
		if killed {
			return markExecutionCancelling(ctx, services, *candidate.Execution, reason, now)
		}
	}
	if candidate.Execution.PID == nil || *candidate.Execution.PID <= 0 {
		return nil
	}
	pid := int(*candidate.Execution.PID)
	if executionMatchesProcess != nil {
		matches, running, err := executionMatchesProcess(ctx, *candidate.Execution, pid)
		if err != nil {
			return err
		}
		if !running || !matches {
			return nil
		}
	}
	if signal != nil {
		if err := signalAgentProcessGroup(pid, signal, 5*time.Second); err != nil {
			return err
		}
	}
	return markExecutionCancelling(ctx, services, *candidate.Execution, reason, now)
}

func isStoppableExecutionStatus(status string) bool {
	return status == "running" || status == "cancelling"
}

func signalAgentProcessGroup(pid int, signalProcess signalProcessFunc, grace time.Duration) error {
	termSignaled := false
	if err := signalProcess(-pid, syscall.SIGTERM); err != nil {
		if !errors.Is(err, syscall.ESRCH) {
			return err
		}
		if err := signalProcess(pid, syscall.SIGTERM); err != nil {
			if errors.Is(err, syscall.ESRCH) {
				return nil
			}
			return err
		}
		termSignaled = true
	} else {
		termSignaled = true
	}
	if grace > 0 && termSignaled {
		go func() {
			timer := time.NewTimer(grace)
			defer timer.Stop()
			<-timer.C
			if err := signalProcess(-pid, syscall.SIGKILL); errors.Is(err, syscall.ESRCH) {
				_ = signalProcess(pid, syscall.SIGKILL)
			}
		}()
	}
	return nil
}

func markExecutionCancelling(ctx context.Context, services looperdruntime.Services, execution storage.AgentExecutionRecord, reason string, now func() time.Time) error {
	if services.Repositories == nil || services.Repositories.AgentExecutions == nil {
		return nil
	}
	current, err := services.Repositories.AgentExecutions.GetByID(ctx, execution.ID)
	if err != nil {
		return err
	}
	if current == nil || current.Status != "running" {
		return nil
	}
	updated := *current
	updated.Status = "cancelling"
	updated.UpdatedAt = eventlog.FormatJavaScriptISOString(now().UTC())
	if updated.ErrorMessage == nil {
		updated.ErrorMessage = &reason
	}
	return services.Repositories.AgentExecutions.Upsert(ctx, updated)
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
