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
	"github.com/nexu-io/looper/internal/dashboard"
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
	processSkipNoLiveHandle        = "live_handle_unavailable"
	processSkipNoSignal            = "signal_unavailable"
	processSkipVerifierNotRunning  = "pid_not_running"
	processSkipVerifierRejectedPID = "pid_verification_rejected"
)

type signalProcessFunc func(int, syscall.Signal) error

type executionMatchesProcessFunc func(context.Context, storage.AgentExecutionRecord, int) (bool, bool, error)

func startRuntimeWithAPI(ctx context.Context, deps bootstrap.RuntimeDependencies) (bootstrap.Runtime, error) {
	rt := looperdruntime.New(looperdruntime.Options{
		Config:        deps.Config,
		InitialConfig: deps.InitialConfig,
		ReloadConfig:  deps.ReloadConfig,
		LoadConfigAt:  deps.LoadConfigAt,
		ConfigPath:    deps.Metadata.ConfigPath,
		Logger:        deps.Logger,
		DeferRecovery: true,
	})
	if err := rt.Start(ctx); err != nil {
		return nil, err
	}

	apiHandler := looperdapi.NewHandler(looperdapi.Context{
		Config: deps.Config,
		ConfigSnapshot: func() (config.Config, looperdapi.ConfigMetadata) {
			cfg, status := rt.ConfigSnapshot()
			return cfg, runtimeConfigMetadataFromStatus(status)
		},
		PatchConfig: func(ctx context.Context, patch looperdapi.ConfigPatchRequest) error {
			return patchRuntimeConfig(ctx, rt, patch)
		},
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
	root := looperdapi.NewRootHandler(apiHandler, dashboard.Handler())
	server := looperdapi.NewServer(deps.Config, root)
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

func runtimeConfigMetadata(rt *looperdruntime.Runtime) looperdapi.ConfigMetadata {
	_, status := rt.ConfigSnapshot()
	return runtimeConfigMetadataFromStatus(status)
}

func runtimeConfigMetadataFromStatus(status looperdruntime.ConfigReloadStatus) looperdapi.ConfigMetadata {
	rejectedPaths := publicConfigRejectedPaths(status.RejectedPaths)
	lastError := sanitizeConfigDiagnostic(status.LastError, status.RejectedPaths)
	metadata := looperdapi.ConfigMetadata{
		ConfigPath:    status.ConfigPath,
		Format:        status.Format,
		FilePresent:   status.FilePresent,
		Revision:      status.Revision,
		LastAttemptAt: status.LastAttemptAt,
		LastAppliedAt: status.LastAppliedAt,
		RejectedPaths: rejectedPaths,
		Fields:        make(map[string]looperdapi.ConfigFieldMetadata, len(status.FieldSources)),
	}
	if lastError != "" {
		metadata.LastError = &lastError
	}
	for path, source := range status.FieldSources {
		if path == "projects" || strings.HasPrefix(path, "projects.") || isSensitiveConfigMetadataPath(path) || config.IsHotReloadCompatibilityPath(path) {
			continue
		}
		hot := config.IsHotEditablePath(path)
		fieldLevel := config.IsFieldLevelConfigPath(path) || path == "agent.env"
		editable := hot && fieldLevel && source != config.ValueSourceEnv && source != config.ValueSourceCLI
		applyMode := "restart"
		if hot {
			applyMode = "hot"
		}
		metadata.Fields[path] = looperdapi.ConfigFieldMetadata{
			Source:    string(source),
			Editable:  editable,
			ApplyMode: applyMode,
		}
	}
	return metadata
}

var sensitiveConfigMetadataRoots = []string{
	"server.localToken",
	"agent.params",
	"daemon.environment",
}

func isSensitiveConfigMetadataPath(path string) bool {
	for _, root := range sensitiveConfigMetadataRoots {
		if path == root || strings.HasPrefix(path, root+".") {
			return true
		}
	}
	return false
}

func publicConfigPath(path string) string {
	for _, root := range sensitiveConfigMetadataRoots {
		if path == root || strings.HasPrefix(path, root+".") {
			return root
		}
	}
	return path
}

func publicConfigRejectedPaths(paths []string) []string {
	seen := make(map[string]struct{}, len(paths))
	result := make([]string, 0, len(paths))
	for _, path := range paths {
		path = publicConfigPath(path)
		if _, exists := seen[path]; exists {
			continue
		}
		seen[path] = struct{}{}
		result = append(result, path)
	}
	sort.Strings(result)
	return result
}

func sanitizeConfigDiagnostic(message string, paths []string) string {
	if message != "" && len(paths) == 0 {
		// Field-less parser errors can embed the rejected YAML/TOML scalar. Keep
		// the API defensive even if a caller supplies status not produced by the
		// runtime's own sanitized diagnostic path.
		return "configuration reload rejected: config file could not be decoded or validated"
	}
	hasSensitive := false
	for _, path := range paths {
		if isSensitiveConfigMetadataPath(path) {
			hasSensitive = true
			break
		}
	}
	if hasSensitive {
		prefix := "configuration reload rejected"
		if strings.Contains(strings.ToLower(message), "restart") {
			prefix = "configuration changes require a daemon restart"
		}
		return prefix + "; sensitive field details are hidden"
	}
	return message
}

func patchRuntimeConfig(ctx context.Context, rt *looperdruntime.Runtime, patch looperdapi.ConfigPatchRequest) error {
	err := rt.PatchConfig(ctx, looperdruntime.ConfigPatch{Revision: patch.Revision, Set: patch.Set, Unset: patch.Unset})
	if err == nil {
		return nil
	}

	requestError := looperdapi.ConfigRequestError{
		Kind:    looperdapi.ConfigRequestErrorKindValidation,
		Message: err.Error(),
	}
	var patchError *looperdruntime.ConfigPatchError
	if errors.As(err, &patchError) {
		switch patchError.Kind {
		case "conflict":
			requestError.Kind = looperdapi.ConfigRequestErrorKindConflict
		case "unsupported":
			requestError.Kind = looperdapi.ConfigRequestErrorKindUnsupported
		case "validation":
			requestError.Kind = looperdapi.ConfigRequestErrorKindValidation
		default:
			// Filesystem and bootstrap failures are server errors, not user input
			// failures. Preserve the untyped error so the API returns 500.
			return err
		}
		for _, path := range patchError.Paths {
			requestError.Issues = append(requestError.Issues, looperdapi.ConfigPatchIssue{
				Path:    path,
				Code:    configPatchIssueCode(patchError.Kind),
				Message: patchError.Error(),
			})
		}
	}

	var validationError *config.ConfigValidationError
	if errors.As(err, &validationError) {
		requestError.Issues = requestError.Issues[:0]
		for _, issue := range validationError.Issues {
			requestError.Issues = append(requestError.Issues, looperdapi.ConfigPatchIssue{
				Path:    issue.Path,
				Code:    "invalid_value",
				Message: issue.Message,
			})
		}
	}
	if len(requestError.Issues) == 0 {
		requestError.Issues = []looperdapi.ConfigPatchIssue{{
			Code:    configPatchIssueCode(string(requestError.Kind)),
			Message: err.Error(),
		}}
	}
	return requestError
}

func configPatchIssueCode(kind string) string {
	switch kind {
	case "conflict":
		return "file_changed"
	case "unsupported":
		return "field_not_editable"
	default:
		return "invalid_value"
	}
}

func stopServerWithTimeout(stop func(context.Context) error, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return stop(ctx)
}

func (d *daemonRuntime) Stop(reason string) {
	d.stopOnce.Do(func() {
		// Admission → HTTP ingress drain → cancel/drain producers → SQLite close
		// (or retain storage on drain timeout) — ADR-0015 / #575 / #577.
		if d.runtime != nil {
			d.runtime.BeginShutdown(reason)
		}
		if d.server != nil {
			ctx, cancel := context.WithTimeout(context.Background(), d.shutdownTimeout)
			err := d.server.Stop(ctx)
			cancel()
			if err != nil {
				// Fail loud when ingress drain is incomplete; Runtime.Stop still
				// decides storage close vs retain based on producer drain.
				_, _ = fmt.Fprintf(os.Stderr, "looperd: HTTP ingress drain incomplete during shutdown: %v\n", err)
			}
		}
		if d.runtime != nil {
			d.runtime.Stop(reason)
			if d.runtime.StorageRetained() {
				// No false graceful success: undrained ownership keeps SQLite open.
				drainErr := d.runtime.ShutdownDrainError()
				if drainErr != nil {
					_, _ = fmt.Fprintf(os.Stderr, "looperd: shutdown drain incomplete; storage retained: %v\n", drainErr)
				} else {
					_, _ = fmt.Fprintf(os.Stderr, "looperd: shutdown drain incomplete; storage retained\n")
				}
			}
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

	// Durable pause must succeed before lease cancel. BeginLoopStop cancels
	// lease contexts wired into execution.run and cannot be undone; doing it
	// before Pause would half-kill agents when a transient Pause error leaves
	// the loop still running. Terminal close has no pre-kill status transition.
	if !terminal {
		paused, err := services.Loops.Pause(ctx, loopID, &reasonCopy)
		if err != nil {
			return nil, err
		}
		result.Stopped = true
		result.LoopID = paused.Loop.ID
	}

	// Close spawn admission for this loop before kill so stop-vs-spawn races
	// cannot return a live process after halt begins (#576). After a durable
	// stop (successful pause/terminate), keep the gate closed: in-flight
	// runners that already claimed work may still reach AgentExecutor.Start
	// without re-reading loop status. ClearLoopStop runs only on intentional
	// re-activation (API unpause/retry/handback).
	//
	// BeginLoopStop is irreversible for live agents (lease cancel + bound-handle
	// drain). Non-terminal already paused, so open the gate immediately after
	// Pause. Terminal close defers the gate until after abortable run/execution
	// preflight: a transient lookup error must not kill agents while the loop
	// stays open. releaseLoopStop reopens admission only and cannot undo a
	// canceled lease.
	//
	// Terminal close has no durable status transition until complete(); if a
	// later Kill/signal/terminate aborts after the gate opens, release the gate
	// so the still-running loop can AdmitSpawn again. Non-terminal already
	// paused durably, so the gate stays sticky even when later steps fail.
	var releaseLoopStop func()
	var loopStopDrainErr error
	beginLoopStop := func() {
		if releaseLoopStop != nil || services.ActiveExecutions == nil {
			return
		}
		var err error
		releaseLoopStop, err = services.ActiveExecutions.BeginLoopStop(loopID, reason)
		if err != nil {
			loopStopDrainErr = err
		}
	}
	if !terminal {
		beginLoopStop()
		if loopStopDrainErr != nil {
			return nil, loopStopDrainErr
		}
	}
	keepStopGateSticky := !terminal
	defer func() {
		if releaseLoopStop != nil && !keepStopGateSticky {
			releaseLoopStop()
		}
	}()
	finish := func() (any, error) {
		// Ensure admission is closed before durable terminate (terminal) so the
		// sticky gate applies even when there was no process to kill.
		beginLoopStop()
		if loopStopDrainErr != nil {
			return nil, loopStopDrainErr
		}
		out, err := complete()
		if err == nil {
			keepStopGateSticky = true
		}
		return out, err
	}

	if services.Repositories == nil || services.Repositories.Runs == nil {
		result.ProcessSkipReason = processSkipNoRuns
		return finish()
	}

	// Abortable preflight for terminal close: do not BeginLoopStop yet.
	latestRun, err := services.Repositories.Runs.GetLatestByLoopID(ctx, loopID)
	if err != nil {
		return nil, err
	}
	if latestRun == nil || latestRun.Status != "running" {
		result.Outcome = stopOutcomeAlreadyFinished
		result.ProcessSkipReason = processSkipNoRuns
		return finish()
	}
	result.RunID = latestRun.ID

	if services.Repositories.AgentExecutions == nil {
		result.ProcessSkipReason = processSkipNoExecution
		return finish()
	}

	latestExecution, err := services.Repositories.AgentExecutions.GetLatestByRunID(ctx, latestRun.ID)
	if err != nil {
		return nil, err
	}
	if latestExecution == nil {
		result.ProcessSkipReason = processSkipNoExecution
		return finish()
	}

	result.ExecutionID = latestExecution.ID
	result.Vendor = latestExecution.Vendor
	if !isStoppableExecutionStatus(latestExecution.Status) {
		result.Outcome = stopOutcomeAlreadyFinished
		result.ProcessSkipReason = processSkipAlreadyFinished
		return finish()
	}
	if latestExecution.Status == "cancelling" {
		result.Outcome = stopOutcomeAlreadyStopping
		result.ProcessSkipReason = processSkipAlreadyStopping
	}
	// Past abortable preflight: close admission and drain leases before kill.
	beginLoopStop()
	if loopStopDrainErr != nil {
		return nil, loopStopDrainErr
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
			return finish()
		}
		// #576: agent live PID fallback removed after full in-scope agent coverage.
		// In-scope agents are owned at the common executor boundary; do not
		// reconstruct stop/kill from SQLite PID while the daemon is live.
		// A stoppable execution with a persisted PID but no registry entry is an
		// ownership invariant violation: fail loudly so stop/close cannot report
		// success while leaving a live agent process behind.
		if latestExecution.PID != nil && *latestExecution.PID > 0 {
			result.PID = *latestExecution.PID
			return nil, looperdruntime.ErrAgentLiveHandleMissing
		}
		result.ProcessSkipReason = processSkipNoLiveHandle
		return finish()
	}
	// ActiveExecutions unavailable (misconfigured daemon): keep historical PID
	// signal path so stop does not silently lose the ability to kill.
	if latestExecution.PID == nil || *latestExecution.PID <= 0 {
		result.ProcessSkipReason = processSkipNoPID
		return finish()
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
			return finish()
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
		return finish()
	}

	if err := markExecutionCancelling(ctx, services, *latestExecution, reasonCopy, now); err != nil {
		return nil, err
	}

	return finish()
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
		// Close loop spawn admission only for the kill window so a concurrent
		// Start cannot return unowned. Always release on return: this helper
		// does not perform durable pause/terminate. When haltLoop already kept
		// a sticky gate (successful pause), BeginLoopStop is refcounted so our
		// release leaves that sticky gate in place. When stopAll falls back
		// here after a Pause failure, releasing reopens AdmitSpawn for the
		// still-running loop (ClearLoopStop would never run otherwise).
		releaseLoopStop, drainErr := services.ActiveExecutions.BeginLoopStop(candidate.Loop.ID, reason)
		defer releaseLoopStop()
		if drainErr != nil {
			return result, drainErr
		}
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
		// #576: no live SQLite-PID fallback when Supervisor registry is present.
		// Missing handle with a persisted PID is an ownership invariant violation.
		if candidate.Execution.PID != nil && *candidate.Execution.PID > 0 {
			result.PID = *candidate.Execution.PID
			return result, looperdruntime.ErrAgentLiveHandleMissing
		}
		result.ProcessSkipReason = processSkipNoLiveHandle
		return result, nil
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
	if err := services.Repositories.AgentExecutions.Upsert(ctx, updated); err != nil {
		// Terminal already won: stop path must not invent success-over-conflict.
		if errors.Is(err, storage.ErrAgentExecutionConflict) {
			return nil
		}
		return err
	}
	return nil
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
