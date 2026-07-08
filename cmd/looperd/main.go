package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	looperdapi "github.com/nexu-io/looper/internal/api"
	"github.com/nexu-io/looper/internal/bootstrap"
	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/domain"
	"github.com/nexu-io/looper/internal/eventlog"
	"github.com/nexu-io/looper/internal/loops"
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
	Stopped           bool   `json:"stopped"`
	LoopID            string `json:"loopId"`
	RunID             string `json:"runId,omitempty"`
	ExecutionID       string `json:"executionId,omitempty"`
	Vendor            string `json:"vendor,omitempty"`
	PID               int64  `json:"pid,omitempty"`
	Outcome           string `json:"outcome,omitempty"`
	ProcessSkipReason string `json:"processSkipReason,omitempty"`
}

const (
	stopOutcomeProcessSignaled = "process_signaled"
	stopOutcomePausedOnly      = "paused_only"
	stopOutcomeAlreadyStopping = "already_stopping"
	stopOutcomeAlreadyFinished = "already_finished"

	processSkipNoRuns              = "no_running_run"
	processSkipNoExecution         = "no_execution"
	processSkipAlreadyFinished     = "execution_already_finished"
	processSkipAlreadyStopping     = "execution_already_stopping"
	processSkipNoPID               = "pid_unavailable"
	processSkipNoSignal            = "signal_unavailable"
	processSkipVerifierNotRunning  = "pid_not_running"
	processSkipVerifierRejectedPID = "pid_verification_rejected"
)

type signalProcessFunc func(int, syscall.Signal) error

type executionMatchesProcessFunc func(context.Context, storage.AgentExecutionRecord, int) (bool, bool, error)

func startRuntimeWithAPI(ctx context.Context, deps bootstrap.RuntimeDependencies) (bootstrap.Runtime, error) {
	rt := looperdruntime.New(looperdruntime.Options{
		Config:        deps.Config,
		Logger:        deps.Logger,
		DeferRecovery: true,
	})
	if err := rt.Start(ctx); err != nil {
		return nil, err
	}

	handler := looperdapi.NewHandler(looperdapi.Context{
		Config:  deps.Config,
		Runtime: rt,
		ReconcileStaleRuns: func(ctx context.Context) (looperdruntime.StaleRunReconcileSummary, error) {
			return rt.ReconcileStaleRunningRuns(ctx)
		},
		StopLoop: func(ctx context.Context, loopID, reason string) (any, error) {
			return stopLoop(ctx, rt.Services(), loopID, reason, time.Now, syscall.Kill, rt.ExecutionMatchesProcess)
		},
		CloseLoop: func(ctx context.Context, loopID, reason string) (any, error) {
			return closeLoop(ctx, rt.Services(), loopID, reason, time.Now, syscall.Kill, rt.ExecutionMatchesProcess)
		},
		StopAll: func(ctx context.Context, reason string) (any, error) {
			return stopAllLoops(ctx, rt.Services(), reason, time.Now, syscall.Kill, rt.ExecutionMatchesProcess)
		},
		TakeoverLoop: func(ctx context.Context, loopID, reason string) (looperdapi.TakeoverResult, error) {
			return takeoverLoop(ctx, rt.Services(), loopID, reason, time.Now, syscall.Kill, rt.ExecutionMatchesProcess)
		},
		TriggerSchedulerTick: func() {
			rt.TriggerSchedulerTick()
		},
	})
	server := looperdapi.NewServer(deps.Config, handler)
	if err := server.Start(); err != nil {
		if deps.Logger != nil {
			deps.Logger.Warn("looperd recovery aborted because instance did not acquire ownership", map[string]any{"error": err.Error()})
		}
		rt.Stop("api server failed to start")
		rt.WaitForShutdown()
		return nil, err
	}

	shutdownTimeout := time.Duration(deps.Config.Daemon.ShutdownTimeoutMS) * time.Millisecond
	if shutdownTimeout <= 0 {
		shutdownTimeout = time.Second
	}

	if err := rt.CompleteStartup(ctx); err != nil {
		_ = stopServerWithTimeout(server.Stop, shutdownTimeout)
		rt.Stop("runtime startup failed after api server ownership")
		rt.WaitForShutdown()
		return nil, err
	}

	return &daemonRuntime{
		runtime:         rt,
		server:          server,
		shutdownTimeout: shutdownTimeout,
	}, nil
}

func stopServerWithTimeout(stop func(context.Context) error, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return stop(ctx)
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
	return haltLoop(ctx, services, loopID, reason, now, signal, executionMatchesProcess, false)
}

func closeLoop(ctx context.Context, services looperdruntime.Services, loopID, reason string, now func() time.Time, signal signalProcessFunc, executionMatchesProcess executionMatchesProcessFunc) (any, error) {
	return haltLoop(ctx, services, loopID, reason, now, signal, executionMatchesProcess, true)
}

// takeoverLoop parks a loop for interactive human takeover: it captures the loop's
// latest agent session id + worktree + vendor, stops the daemon's in-flight run
// (reusing stopLoop — pause + kill + cancel queue, so the scheduler leaves it
// alone), then transitions the loop to human_takeover. The session id lives on
// disk, so a human resumes the exact session and a later handback (retry) lets the
// daemon native-resume it and see the human's turns.
func takeoverLoop(ctx context.Context, services looperdruntime.Services, loopID, reason string, now func() time.Time, signal signalProcessFunc, executionMatchesProcess executionMatchesProcessFunc) (looperdapi.TakeoverResult, error) {
	result := looperdapi.TakeoverResult{LoopID: loopID}
	if services.Loops == nil {
		return result, fmt.Errorf("loops service is not configured")
	}
	if services.Repositories != nil && services.Repositories.AgentExecutions != nil {
		if execution, err := services.Repositories.AgentExecutions.GetLatestByLoopID(ctx, loopID); err == nil && execution != nil {
			result.Vendor = execution.Vendor
			if execution.NativeSessionID != nil {
				result.SessionID = strings.TrimSpace(*execution.NativeSessionID)
			}
			if execution.CWD != nil {
				result.WorktreePath = strings.TrimSpace(*execution.CWD)
			}
		}
	}
	if _, err := stopLoop(ctx, services, loopID, reason, now, signal, executionMatchesProcess); err != nil {
		return result, err
	}
	if _, err := services.Loops.TransitionStatus(ctx, loopID, loops.TransitionInput{Status: domain.LoopStatusHumanTakeover}); err != nil {
		return result, err
	}
	return result, nil
}

func haltLoop(ctx context.Context, services looperdruntime.Services, loopID, reason string, now func() time.Time, signal signalProcessFunc, executionMatchesProcess executionMatchesProcessFunc, terminal bool) (any, error) {
	result := stopLoopResult{Stopped: false, LoopID: loopID, Outcome: stopOutcomePausedOnly}
	if services.Loops == nil {
		return nil, fmt.Errorf("loops service is not configured")
	}

	reasonCopy := reason
	complete := func() (any, error) {
		if !terminal {
			return result, nil
		}
		terminated, err := services.Loops.Terminate(ctx, loopID, &reasonCopy)
		if err != nil {
			return nil, err
		}
		result.Stopped = true
		result.LoopID = terminated.Loop.ID
		return result, nil
	}

	if !terminal {
		paused, err := services.Loops.Pause(ctx, loopID, &reasonCopy)
		if err != nil {
			return nil, err
		}
		result.Stopped = true
		result.LoopID = paused.Loop.ID
	}

	if services.Repositories == nil || services.Repositories.Runs == nil {
		result.ProcessSkipReason = processSkipNoRuns
		return complete()
	}

	latestRun, err := services.Repositories.Runs.GetLatestByLoopID(ctx, loopID)
	if err != nil {
		return nil, err
	}
	if latestRun == nil || latestRun.Status != "running" {
		result.Outcome = stopOutcomeAlreadyFinished
		result.ProcessSkipReason = processSkipNoRuns
		return complete()
	}
	result.RunID = latestRun.ID

	if services.Repositories.AgentExecutions == nil {
		result.ProcessSkipReason = processSkipNoExecution
		return complete()
	}

	latestExecution, err := services.Repositories.AgentExecutions.GetLatestByRunID(ctx, latestRun.ID)
	if err != nil {
		return nil, err
	}
	if latestExecution == nil {
		result.ProcessSkipReason = processSkipNoExecution
		return complete()
	}

	result.ExecutionID = latestExecution.ID
	result.Vendor = latestExecution.Vendor
	if !isStoppableExecutionStatus(latestExecution.Status) {
		result.Outcome = stopOutcomeAlreadyFinished
		result.ProcessSkipReason = processSkipAlreadyFinished
		return complete()
	}
	if latestExecution.Status == "cancelling" {
		result.Outcome = stopOutcomeAlreadyStopping
		result.ProcessSkipReason = processSkipAlreadyStopping
	}
	if services.ActiveExecutions != nil {
		killed, err := services.ActiveExecutions.Kill(result.LoopID, latestRun.ID, latestExecution.ID, reason)
		if err != nil {
			return nil, err
		}
		if killed {
			result.Outcome = stopOutcomeProcessSignaled
			result.ProcessSkipReason = ""
			if err := markExecutionCancelling(ctx, services, *latestExecution, reasonCopy, now); err != nil {
				return nil, err
			}
			return complete()
		}
	}
	if latestExecution.PID == nil || *latestExecution.PID <= 0 {
		result.ProcessSkipReason = processSkipNoPID
		return complete()
	}

	pid := int(*latestExecution.PID)
	if executionMatchesProcess != nil {
		matches, running, err := executionMatchesProcess(ctx, *latestExecution, pid)
		if err != nil {
			return nil, err
		}
		if !running || !matches {
			if !running {
				result.Outcome = stopOutcomeAlreadyFinished
				result.ProcessSkipReason = processSkipVerifierNotRunning
			} else {
				result.ProcessSkipReason = processSkipVerifierRejectedPID
			}
			return complete()
		}
	}
	result.PID = *latestExecution.PID
	if signal != nil {
		if err := signalAgentProcessGroup(pid, signal, 5*time.Second); err != nil {
			return nil, err
		}
		result.Outcome = stopOutcomeProcessSignaled
		result.ProcessSkipReason = ""
	} else {
		result.ProcessSkipReason = processSkipNoSignal
		return complete()
	}

	if err := markExecutionCancelling(ctx, services, *latestExecution, reasonCopy, now); err != nil {
		return nil, err
	}

	return complete()
}

type stopAllResult string

const (
	stopAllResultStopped         stopAllResult = "stopped"
	stopAllResultPausedOnly      stopAllResult = "pausedOnly"
	stopAllResultAlreadyFinished stopAllResult = "alreadyFinished"
	stopAllResultAlreadyStopping stopAllResult = "alreadyStopping"
	stopAllResultFailed          stopAllResult = "failed"
)

type stopAllSummary struct {
	Total           int `json:"total"`
	Stopped         int `json:"stopped"`
	PausedOnly      int `json:"pausedOnly"`
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
	Outcome                 string `json:"outcome,omitempty"`
	ProcessSkipReason       string `json:"processSkipReason,omitempty"`
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

		stopResultValue, err := stopLoop(ctx, services, candidate.Loop.ID, reason, now, signal, executionMatchesProcess)
		if err != nil {
			stopErr := err
			if candidate.Execution != nil && candidate.Execution.Status == "running" {
				if _, fallbackErr := stopCandidateExecution(ctx, services, candidate, reason, now, signal, executionMatchesProcess); fallbackErr != nil {
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
			if stopResult, ok := stopResultValue.(stopLoopResult); ok {
				item.Outcome = stopResult.Outcome
				item.ProcessSkipReason = stopResult.ProcessSkipReason
				item.Result = classifyStopAllItemResult(stopResult)
			}
			for _, execution := range candidate.Executions {
				if execution.Status != "running" {
					continue
				}
				execCandidate := candidate
				execCandidate.Execution = &execution
				execResult, execErr := stopCandidateExecution(ctx, services, execCandidate, reason, now, signal, executionMatchesProcess)
				if execErr != nil && item.Error == "" {
					item.Result = string(stopAllResultFailed)
					item.Error = execErr.Error()
					continue
				}
				item = mergeStopAllItemExecutionOutcome(item, execResult)
				if item.Result == string(stopAllResultPausedOnly) && item.Outcome == stopOutcomeProcessSignaled {
					item.Outcome = stopOutcomePausedOnly
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
		case stopAllResultPausedOnly:
			response.Summary.PausedOnly++
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

func classifyStopAllItemResult(result stopLoopResult) string {
	switch result.Outcome {
	case stopOutcomeProcessSignaled:
		return string(stopAllResultStopped)
	case stopOutcomePausedOnly:
		return string(stopAllResultPausedOnly)
	case stopOutcomeAlreadyStopping:
		return string(stopAllResultAlreadyStopping)
	case stopOutcomeAlreadyFinished:
		return string(stopAllResultAlreadyFinished)
	default:
		return string(stopAllResultStopped)
	}
}

func mergeStopAllItemExecutionOutcome(item stopAllItem, result stopLoopResult) stopAllItem {
	nextResult := classifyStopAllItemResult(result)
	nextWins := stopAllResultRank(nextResult) > stopAllResultRank(item.Result)
	mergedResult, mergedOutcome := mergeStopOutcomes(item.Result, item.Outcome, nextResult, result.Outcome)
	item.Result = mergedResult
	item.Outcome = mergedOutcome
	if nextWins {
		item.ProcessSkipReason = result.ProcessSkipReason
	} else if item.ProcessSkipReason == "" && result.ProcessSkipReason != "" {
		item.ProcessSkipReason = result.ProcessSkipReason
	}
	return item
}

func mergeStopOutcomes(currentResult, currentOutcome, nextResult, nextOutcome string) (string, string) {
	if stopAllResultRank(nextResult) > stopAllResultRank(currentResult) {
		return nextResult, nextOutcome
	}
	return currentResult, currentOutcome
}

func stopAllResultRank(result string) int {
	switch stopAllResult(result) {
	case stopAllResultFailed:
		return 4
	case stopAllResultPausedOnly:
		return 3
	case stopAllResultStopped:
		return 2
	case stopAllResultAlreadyStopping:
		return 1
	case stopAllResultAlreadyFinished:
		return 0
	default:
		return 0
	}
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

func stopCandidateExecution(ctx context.Context, services looperdruntime.Services, candidate stopAllCandidate, reason string, now func() time.Time, signal signalProcessFunc, executionMatchesProcess executionMatchesProcessFunc) (stopLoopResult, error) {
	result := stopLoopResult{LoopID: candidate.Loop.ID, Outcome: stopOutcomePausedOnly}
	if candidate.Execution == nil {
		result.Outcome = stopOutcomeAlreadyFinished
		result.ProcessSkipReason = processSkipNoExecution
		return result, nil
	}
	result.ExecutionID = candidate.Execution.ID
	result.Vendor = candidate.Execution.Vendor
	if candidate.Execution.RunID != nil {
		result.RunID = *candidate.Execution.RunID
	}
	runID := ""
	if candidate.Execution.RunID != nil {
		runID = *candidate.Execution.RunID
	}
	if runID == "" && candidate.Run != nil {
		runID = candidate.Run.ID
		result.RunID = candidate.Run.ID
	}
	if services.ActiveExecutions != nil && runID != "" {
		killed, err := services.ActiveExecutions.Kill(candidate.Loop.ID, runID, candidate.Execution.ID, reason)
		if err != nil {
			return result, err
		}
		if killed {
			result.Outcome = stopOutcomeProcessSignaled
			if err := markExecutionCancelling(ctx, services, *candidate.Execution, reason, now); err != nil {
				return result, err
			}
			return result, nil
		}
	}
	if candidate.Execution.PID == nil || *candidate.Execution.PID <= 0 {
		result.ProcessSkipReason = processSkipNoPID
		return result, nil
	}
	result.PID = *candidate.Execution.PID
	pid := int(*candidate.Execution.PID)
	if executionMatchesProcess != nil {
		matches, running, err := executionMatchesProcess(ctx, *candidate.Execution, pid)
		if err != nil {
			return result, err
		}
		if !running || !matches {
			if !running {
				result.Outcome = stopOutcomeAlreadyFinished
				result.ProcessSkipReason = processSkipVerifierNotRunning
			} else {
				result.ProcessSkipReason = processSkipVerifierRejectedPID
			}
			return result, nil
		}
	}
	if signal == nil {
		result.ProcessSkipReason = processSkipNoSignal
		return result, nil
	}
	if err := signalAgentProcessGroup(pid, signal, 5*time.Second); err != nil {
		return result, err
	}
	result.Outcome = stopOutcomeProcessSignaled
	result.ProcessSkipReason = ""
	if err := markExecutionCancelling(ctx, services, *candidate.Execution, reason, now); err != nil {
		return result, err
	}
	return result, nil
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
