package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/eventlog"
	"github.com/nexu-io/looper/internal/lifecycle"
	"github.com/nexu-io/looper/internal/storage"
)

const (
	defaultMaxOutputBytes    = 256 * 1024
	maxPersistedLogReadBytes = 16 * 1024 * 1024
	completionMarkerEnv      = "LOOPER_COMPLETION_MARKER"
)

type ExecutorConfig struct {
	Vendor              config.AgentVendor
	Model               *string
	Params              map[string]any
	Env                 map[string]string
	NativeResumeEnabled bool
}

type ExecutorOptions struct {
	Config ExecutorConfig
	Repos  *storage.Repositories
	LogDir string
	Now    func() time.Time
}

type RunInput struct {
	ExecutionID        string
	ProjectID          string
	LoopID             string
	RunID              string
	Prompt             string
	NativeResumePrompt string
	WorkingDirectory   string
	Timeout            time.Duration
	HeartbeatTimeout   time.Duration
	GracefulShutdown   time.Duration
	MaxOutputBytes     int
	Metadata           map[string]any
	IdempotencyKey     string
	Env                map[string]string
	NativeSessionID    string
}

type Result struct {
	Status                       string
	Summary                      string
	Stdout                       string
	Stderr                       string
	ParseStatus                  string
	CompletionSignal             string
	Artifacts                    []string
	ChangedFiles                 []string
	Commits                      []string
	Lifecycle                    *lifecycle.State
	HeartbeatCount               int64
	TimeoutType                  string
	ConfiguredIdleTimeoutSeconds int64
	ConfiguredMaxRuntimeSeconds  int64
	ElapsedRuntimeSeconds        int64
	LastProgressAt               string
	PID                          int
}

type completionParse struct {
	ParseStatus      string
	CompletionSignal string
	Summary          string
	Artifacts        []string
	ChangedFiles     []string
	Commits          []string
	Lifecycle        *lifecycle.State
}

type Execution interface {
	Wait(context.Context) (Result, error)
	Kill(string) error
}

type ConfiguredExecutor struct {
	config ExecutorConfig
	repos  *storage.Repositories
	logDir string
	now    func() time.Time
}

func New(options ExecutorOptions) *ConfiguredExecutor {
	now := options.Now
	if now == nil {
		now = time.Now
	}
	return &ConfiguredExecutor{config: options.Config, repos: options.Repos, logDir: options.LogDir, now: now}
}

type nativeResumeInfo struct {
	Enabled           bool
	SessionID         string
	Mode              string
	Status            string
	SourceExecutionID string
}

func (e *ConfiguredExecutor) resolveNativeResume(ctx context.Context, input RunInput) (nativeResumeInfo, error) {
	if !e.config.NativeResumeEnabled {
		return nativeResumeInfo{Mode: "checkpoint_restart", Status: "disabled"}, nil
	}
	if sessionID := strings.TrimSpace(input.NativeSessionID); sessionID != "" {
		if nativeResumeSupported(e.config.Vendor) {
			return nativeResumeInfo{Enabled: true, SessionID: sessionID, Mode: "native_resume", Status: "started"}, nil
		}
		return nativeResumeInfo{Mode: "checkpoint_restart", Status: "unsupported"}, nil
	}
	if e.repos == nil || e.repos.AgentExecutions == nil || strings.TrimSpace(input.LoopID) == "" {
		return nativeResumeInfo{Mode: "checkpoint_restart", Status: "unavailable"}, nil
	}
	latest, err := e.repos.AgentExecutions.GetLatestByLoopID(ctx, input.LoopID)
	if err != nil {
		return nativeResumeInfo{}, fmt.Errorf("load latest agent execution for native resume: %w", err)
	}
	if latest == nil || latest.NativeSessionID == nil || strings.TrimSpace(*latest.NativeSessionID) == "" {
		return nativeResumeInfo{Mode: "checkpoint_restart", Status: "unavailable"}, nil
	}
	if latest.Vendor != string(e.config.Vendor) || !nativeResumeSupported(e.config.Vendor) || !isRecoverableNativeResumeSource(latest.Status, latest.NativeResumeStatus) {
		return nativeResumeInfo{Mode: "checkpoint_restart", Status: "unavailable"}, nil
	}
	return nativeResumeInfo{Enabled: true, SessionID: strings.TrimSpace(*latest.NativeSessionID), Mode: "native_resume", Status: "started", SourceExecutionID: latest.ID}, nil
}

func (e *ConfiguredExecutor) markNativeResumeFailed(ctx context.Context, executionID string, message string) error {
	if executionID == "" || e.repos == nil || e.repos.AgentExecutions == nil {
		return nil
	}
	record, err := e.repos.AgentExecutions.GetByID(ctx, executionID)
	if err != nil || record == nil {
		return err
	}
	nowISO := eventlog.FormatJavaScriptISOString(e.now().UTC())
	record.NativeResumeStatus = stringPtr("failed")
	record.NativeResumeError = stringPtr(message)
	record.UpdatedAt = nowISO
	return e.repos.AgentExecutions.Upsert(ctx, *record)
}

func nativeResumeSupported(vendor config.AgentVendor) bool {
	switch vendor {
	case config.AgentVendorClaudeCode, config.AgentVendorCodex, config.AgentVendorOpenCode, config.AgentVendorCursorCLI:
		return true
	default:
		return false
	}
}

func isRecoverableNativeResumeSource(status string, resumeStatus *string) bool {
	if resumeStatus == nil || *resumeStatus != "pending" {
		return false
	}
	switch status {
	case "running", "cancelling", "killed", "timeout", "failed", "completed":
		return true
	default:
		return false
	}
}

func (e *ConfiguredExecutor) Start(ctx context.Context, input RunInput) (Execution, error) {
	if strings.TrimSpace(input.Prompt) == "" {
		return nil, fmt.Errorf("agent prompt is required")
	}
	if strings.TrimSpace(input.WorkingDirectory) == "" {
		return nil, fmt.Errorf("working directory is required")
	}

	executionID := input.ExecutionID
	if executionID == "" {
		executionID = eventlog.NewEventID("agentexec")
	}
	startedAt := e.now().UTC()
	startedAtISO := eventlog.FormatJavaScriptISOString(startedAt)
	resume, err := e.resolveNativeResume(ctx, input)
	if err != nil {
		return nil, err
	}
	spawnPrompt := input.Prompt
	if resume.Enabled && strings.TrimSpace(input.NativeResumePrompt) != "" {
		spawnPrompt = input.NativeResumePrompt
	}
	command, args := ResolveSpawnWithNativeResume(e.config, spawnPrompt, resume.SessionID, resume.Enabled)

	cmd := exec.Command(command, args...)
	cmd.Dir = input.WorkingDirectory
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = os.Environ()
	for key, value := range e.config.Env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	for key, value := range input.Env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	cmd.Env = append(cmd.Env,
		"LOOPER_PROMPT="+spawnPrompt,
		completionMarkerEnv+"="+CompletionMarkerPrefix,
	)

	maxOutputBytes := input.MaxOutputBytes
	if maxOutputBytes <= 0 {
		maxOutputBytes = defaultMaxOutputBytes
	}

	x := &execution{
		executor:           e,
		input:              input,
		executionID:        executionID,
		startedAt:          startedAt,
		command:            command,
		args:               args,
		startedAtISO:       startedAtISO,
		process:            cmd,
		timeout:            input.Timeout,
		heartbeatTimeout:   input.HeartbeatTimeout,
		gracefulShutdown:   input.GracefulShutdown,
		maxOutputBytes:     maxOutputBytes,
		lastHeartbeatAtISO: startedAtISO,
		lastOutputAt:       startedAt,
		status:             "running",
		nativeSessionID:    resume.SessionID,
		nativeResumeMode:   resume.Mode,
		nativeResumeStatus: resume.Status,
		killCh:             make(chan string, 1),
		doneCh:             make(chan execOutcome, 1),
	}
	x.stdoutLogPath, x.stderrLogPath = e.executionLogPaths(input, executionID)
	x.initializePersistedLogs()
	cmd.Stdout = &streamCapture{onChunk: func(chunk []byte) { x.onOutput("stdout", chunk) }}
	cmd.Stderr = &streamCapture{onChunk: func(chunk []byte) { x.onOutput("stderr", chunk) }}
	if err := cmd.Start(); err != nil {
		if resume.Enabled {
			if markErr := e.markNativeResumeFailed(ctx, resume.SourceExecutionID, err.Error()); markErr == nil && e.logDir != "" {
				// best-effort marker only; command fallback is the important recovery behavior
			}
			command, args = ResolveSpawn(e.config, input.Prompt)
			cmd = exec.Command(command, args...)
			cmd.Dir = input.WorkingDirectory
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			cmd.Env = os.Environ()
			for key, value := range e.config.Env {
				cmd.Env = append(cmd.Env, key+"="+value)
			}
			for key, value := range input.Env {
				cmd.Env = append(cmd.Env, key+"="+value)
			}
			cmd.Env = append(cmd.Env,
				"LOOPER_PROMPT="+input.Prompt,
				completionMarkerEnv+"="+CompletionMarkerPrefix,
			)
			cmd.Stdout = &streamCapture{onChunk: func(chunk []byte) { x.onOutput("stdout", chunk) }}
			cmd.Stderr = &streamCapture{onChunk: func(chunk []byte) { x.onOutput("stderr", chunk) }}
			x.mu.Lock()
			x.command = command
			x.args = args
			x.process = cmd
			x.nativeSessionID = ""
			x.nativeResumeMode = "checkpoint_restart"
			x.nativeResumeStatus = "fallback_started"
			x.nativeResumeError = err.Error()
			x.mu.Unlock()
			if startErr := cmd.Start(); startErr != nil {
				return nil, fmt.Errorf("start agent command: %w (native resume fallback after: %v)", startErr, err)
			}
		} else {
			return nil, fmt.Errorf("start agent command: %w", err)
		}
	}

	resumeSessionID, resumeMode, resumeStatus, _ := x.nativeResumeSnapshot()
	x.persistStatus("running", nil, nil, nil)
	e.appendLifecycleEvent("agent.invoked", input, executionID, map[string]any{"command": command, "args": args, "cwd": input.WorkingDirectory, "nativeResumeMode": resumeMode, "nativeResumeStatus": resumeStatus, "nativeSessionId": resumeSessionID}, startedAtISO)

	go x.run(ctx)
	return x, nil
}

type execOutcome struct {
	result Result
	err    error
}

type execution struct {
	executor           *ConfiguredExecutor
	input              RunInput
	executionID        string
	startedAt          time.Time
	command            string
	args               []string
	startedAtISO       string
	process            *exec.Cmd
	timeout            time.Duration
	heartbeatTimeout   time.Duration
	gracefulShutdown   time.Duration
	maxOutputBytes     int
	lastHeartbeatAtISO string
	lastOutputAt       time.Time

	mu                      sync.Mutex
	status                  string
	stdout                  []byte
	stderr                  []byte
	stdoutLogPath           string
	stderrLogPath           string
	persistedLogWriteFailed bool
	heartbeatCount          int64
	nativeSessionID         string
	nativeResumeMode        string
	nativeResumeStatus      string
	nativeResumeError       string

	killCh chan string
	doneCh chan execOutcome
}

func (x *execution) Wait(ctx context.Context) (Result, error) {
	select {
	case <-ctx.Done():
		return Result{}, ctx.Err()
	case out := <-x.doneCh:
		x.doneCh <- out
		return out.result, out.err
	}
}

func (x *execution) Kill(reason string) error {
	select {
	case x.killCh <- reason:
	default:
	}
	return nil
}

func (x *execution) signalProcessGroup(signal syscall.Signal) error {
	if x.process.Process == nil {
		return os.ErrProcessDone
	}
	pid := x.process.Process.Pid
	if pid <= 0 {
		return x.process.Process.Signal(signal)
	}
	if err := syscall.Kill(-pid, signal); err != nil {
		if err == syscall.ESRCH {
			return os.ErrProcessDone
		}
		return err
	}
	return nil
}

func (x *execution) killProcessGroup() error {
	if x.process.Process == nil {
		return os.ErrProcessDone
	}
	pid := x.process.Process.Pid
	if pid > 0 {
		if err := syscall.Kill(-pid, syscall.SIGKILL); err == nil || err == syscall.ESRCH {
			return nil
		}
	}
	return x.process.Process.Kill()
}

func (x *execution) run(ctx context.Context) {
	waitCh := make(chan error, 1)
	go func() { waitCh <- x.process.Wait() }()

	var (
		waitErr         error
		timedOut        bool
		timeoutType     string
		killed          bool
		killReason      string
		graceKillTimer  <-chan time.Time
		timeoutTimer    <-chan time.Time
		inactivityTimer *time.Ticker
		termDelivered   bool
		terminateOnce   sync.Once
		terminateSignal = func() {
			terminateOnce.Do(func() {
				if x.process.Process == nil {
					return
				}
				if err := x.signalProcessGroup(syscall.SIGTERM); err != nil {
					if err != os.ErrProcessDone {
						_ = x.killProcessGroup()
					}
					return
				}
				termDelivered = true
				grace := x.gracefulShutdown
				if grace <= 0 {
					grace = 5 * time.Second
				}
				graceKillTimer = time.After(grace)
			})
		}
	)

	if x.timeout > 0 {
		timeoutTimer = time.After(x.timeout)
	}
	if x.heartbeatTimeout > 0 {
		interval := x.heartbeatTimeout
		if interval > time.Second {
			interval = time.Second
		}
		inactivityTimer = time.NewTicker(interval)
		defer inactivityTimer.Stop()
	}

	waiting := true
	for waiting {
		select {
		case waitErr = <-waitCh:
			waiting = false
		case <-timeoutTimer:
			timeoutTimer = nil
			if !timedOut {
				timedOut = true
				timeoutType = "max_runtime"
			}
			if killReason == "" {
				killReason = fmt.Sprintf("agent max runtime timed out after %s", x.timeout)
			}
			x.setStatus("timeout")
			terminateSignal()
		case <-tickerChan(inactivityTimer):
			if timedOut || killed {
				continue
			}
			select {
			case waitErr = <-waitCh:
				waiting = false
				continue
			default:
			}
			if x.timeSinceLastOutput() < x.heartbeatTimeout {
				continue
			}
			timedOut = true
			timeoutType = "idle"
			if killReason == "" {
				killReason = fmt.Sprintf("agent idle timed out after %s without observable progress", x.heartbeatTimeout)
			}
			x.setStatus("timeout")
			terminateSignal()
		case reason := <-x.killCh:
			killed = true
			killReason = reason
			x.setStatus("killed")
			terminateSignal()
		case <-ctx.Done():
			killed = true
			if killReason == "" {
				killReason = ctx.Err().Error()
			}
			x.setStatus("killed")
			terminateSignal()
		case <-graceKillTimer:
			graceKillTimer = nil
			_ = x.killProcessGroup()
		}
	}
	if termDelivered && (killed || timedOut) {
		_ = x.killProcessGroup()
	}

	stdout, stderr := x.resolveOutputLogs()
	status := x.finalStatus(timedOut, killed)
	if waitErr != nil && status == "failed" && strings.TrimSpace(stderr) == "" {
		stderr = waitErr.Error()
		if x.appendPersistedLog(x.stderrLogPath, []byte(stderr)) {
			x.markPersistedLogWriteFailed()
		}
	}
	errorMessage := ""
	if status == "failed" || status == "timeout" || status == "killed" {
		errorMessage = strings.TrimSpace(stderr)
		if errorMessage == "" {
			errorMessage = killReason
		}
	}
	completion := parseCompletion(stdout, stderr)
	if status != "completed" {
		completion = completionParse{ParseStatus: "missing"}
	}
	if completion.Summary == "" {
		completion.Summary = errorMessage
		if completion.Summary == "" {
			completion.Summary = summarizeLogs(stdout, stderr)
		}
	}
	endedAtISO := eventlog.FormatJavaScriptISOString(x.executor.now().UTC())
	lastProgressAt := x.lastProgressAtISO()
	result := Result{
		Status:                       status,
		Summary:                      completion.Summary,
		Stdout:                       stdout,
		Stderr:                       stderr,
		ParseStatus:                  completion.ParseStatus,
		CompletionSignal:             completion.CompletionSignal,
		Artifacts:                    append([]string(nil), completion.Artifacts...),
		ChangedFiles:                 append([]string(nil), completion.ChangedFiles...),
		Commits:                      append([]string(nil), completion.Commits...),
		Lifecycle:                    completion.Lifecycle,
		HeartbeatCount:               x.heartbeatCountValue(),
		TimeoutType:                  timeoutType,
		ConfiguredIdleTimeoutSeconds: durationSeconds(x.heartbeatTimeout),
		ConfiguredMaxRuntimeSeconds:  durationSeconds(x.timeout),
		ElapsedRuntimeSeconds:        durationSeconds(x.executor.now().UTC().Sub(x.startedAt)),
		LastProgressAt:               lastProgressAt,
		PID:                          pidOrZero(x.process.Process),
	}
	if x.shouldFallbackNativeResume(status, stdout, stderr) {
		if fallbackResult, fallbackErrorMessage, ok := x.runCheckpointFallback(ctx, errorMessage); ok {
			result = fallbackResult
			status = fallbackResult.Status
			timeoutType = fallbackResult.TimeoutType
			errorMessage = fallbackErrorMessage
			endedAtISO = eventlog.FormatJavaScriptISOString(x.executor.now().UTC())
		}
	}

	x.persistFinal(status, result, errorMessage, endedAtISO)
	eventType := "agent.completed"
	if status == "timeout" {
		switch timeoutType {
		case "idle":
			eventType = "agent.idle_timeout"
		case "max_runtime":
			eventType = "agent.max_runtime_timeout"
		default:
			eventType = "agent.timed_out"
		}
	} else if status == "killed" {
		eventType = "agent.killed"
	}
	x.executor.appendLifecycleEvent(eventType, x.input, x.executionID, map[string]any{
		"status":                       status,
		"timeoutType":                  timeoutType,
		"configuredIdleTimeoutSeconds": result.ConfiguredIdleTimeoutSeconds,
		"configuredMaxRuntimeSeconds":  result.ConfiguredMaxRuntimeSeconds,
		"elapsedRuntimeSeconds":        result.ElapsedRuntimeSeconds,
		"lastProgressAt":               result.LastProgressAt,
		"parseStatus":                  result.ParseStatus,
		"heartbeatCount":               result.HeartbeatCount,
		"summary":                      result.Summary,
	}, endedAtISO)

	x.doneCh <- execOutcome{result: result, err: nil}
}

func (x *execution) shouldFallbackNativeResume(status string, stdout string, stderr string) bool {
	_, mode, resumeStatus, _ := x.nativeResumeSnapshot()
	return mode == "native_resume" && resumeStatus == "started" && status == "failed" && isNativeResumeAttachFailure(stdout, stderr)
}

func isNativeResumeAttachFailure(stdout string, stderr string) bool {
	if strings.TrimSpace(stdout) != "" {
		return false
	}
	message := strings.TrimSpace(stderr)
	if message == "" {
		return false
	}
	for _, line := range strings.Split(message, "\n") {
		line = normalizeNativeResumeErrorLine(line)
		switch {
		case line == "resume failed" || strings.HasPrefix(line, "resume failed:"):
			return true
		case strings.HasPrefix(line, "failed to resume session") || strings.HasPrefix(line, "could not resume session") || strings.HasPrefix(line, "cannot resume session"):
			return true
		case strings.HasPrefix(line, "failed to resume conversation") || strings.HasPrefix(line, "could not resume conversation") || strings.HasPrefix(line, "cannot resume conversation"):
			return true
		}
	}
	return false
}

func normalizeNativeResumeErrorLine(line string) string {
	line = strings.ToLower(strings.TrimSpace(line))
	line = strings.TrimPrefix(line, "error:")
	line = strings.TrimPrefix(strings.TrimSpace(line), "fatal:")
	return strings.TrimSpace(line)
}

func (x *execution) runCheckpointFallback(ctx context.Context, nativeError string) (Result, string, bool) {
	command, args := ResolveSpawn(x.executor.config, x.input.Prompt)
	cmd := exec.Command(command, args...)
	cmd.Dir = x.input.WorkingDirectory
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = os.Environ()
	for key, value := range x.executor.config.Env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	for key, value := range x.input.Env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	cmd.Env = append(cmd.Env,
		"LOOPER_PROMPT="+x.input.Prompt,
		completionMarkerEnv+"="+CompletionMarkerPrefix,
	)
	cmd.Stdout = &streamCapture{onChunk: func(chunk []byte) { x.onOutput("stdout", chunk) }}
	cmd.Stderr = &streamCapture{onChunk: func(chunk []byte) { x.onOutput("stderr", chunk) }}

	now := x.executor.now().UTC()
	nowISO := eventlog.FormatJavaScriptISOString(now)
	x.mu.Lock()
	x.command = command
	x.args = args
	x.process = cmd
	x.status = "running"
	x.stdout = nil
	x.stderr = nil
	x.nativeSessionID = ""
	x.nativeResumeMode = "checkpoint_restart"
	x.nativeResumeStatus = "fallback_started"
	x.nativeResumeError = nativeError
	x.lastHeartbeatAtISO = nowISO
	x.lastOutputAt = now
	x.mu.Unlock()
	x.persistStatus("running", nil, nil, nil)
	x.executor.appendLifecycleEvent("agent.native_resume_fallback_started", x.input, x.executionID, map[string]any{"command": command, "args": args, "nativeResumeError": nativeError}, nowISO)

	if err := cmd.Start(); err != nil {
		x.mu.Lock()
		x.status = "failed"
		x.nativeResumeStatus = "fallback_failed"
		x.nativeResumeError = firstNonEmpty(err.Error(), nativeError)
		x.mu.Unlock()
		return Result{}, "", false
	}

	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	var (
		waitErr        error
		timedOut       bool
		killed         bool
		timeoutType    string
		killReason     string
		timeoutTimer   <-chan time.Time
		graceKillTimer <-chan time.Time
		idleTicker     *time.Ticker
		termDelivered  bool
	)
	if x.timeout > 0 {
		timeoutTimer = time.After(x.timeout)
	}
	if x.heartbeatTimeout > 0 {
		interval := x.heartbeatTimeout
		if interval > time.Second {
			interval = time.Second
		}
		idleTicker = time.NewTicker(interval)
		defer idleTicker.Stop()
	}
	terminate := func() {
		if cmd.Process == nil {
			return
		}
		if err := x.signalProcessGroup(syscall.SIGTERM); err != nil {
			if err != os.ErrProcessDone {
				_ = x.killProcessGroup()
			}
			return
		}
		termDelivered = true
		grace := x.gracefulShutdown
		if grace <= 0 {
			grace = 5 * time.Second
		}
		graceKillTimer = time.After(grace)
	}
	waiting := true
	for waiting {
		select {
		case waitErr = <-waitCh:
			waiting = false
		case <-timeoutTimer:
			timeoutTimer = nil
			if !timedOut {
				timedOut = true
				timeoutType = "max_runtime"
				killReason = fmt.Sprintf("agent max runtime timed out after %s", x.timeout)
			}
			terminate()
		case <-tickerChan(idleTicker):
			if timedOut || killed || x.timeSinceLastOutput() < x.heartbeatTimeout {
				continue
			}
			timedOut = true
			timeoutType = "idle"
			killReason = fmt.Sprintf("agent idle timed out after %s without observable progress", x.heartbeatTimeout)
			terminate()
		case reason := <-x.killCh:
			killed = true
			killReason = reason
			terminate()
		case <-ctx.Done():
			killed = true
			killReason = ctx.Err().Error()
			terminate()
		case <-graceKillTimer:
			graceKillTimer = nil
			_ = x.killProcessGroup()
		}
	}
	if termDelivered && (killed || timedOut) {
		_ = x.killProcessGroup()
	}
	stdout := x.stdoutString()
	stderr := x.stderrString()
	now = x.executor.now().UTC()
	nowISO = eventlog.FormatJavaScriptISOString(now)
	x.mu.Lock()
	x.stdout = []byte(stdout)
	x.stderr = []byte(stderr)
	x.lastHeartbeatAtISO = nowISO
	x.lastOutputAt = now
	x.heartbeatCount++
	x.mu.Unlock()
	status := "completed"
	if timedOut {
		status = "timeout"
	} else if killed {
		status = "killed"
	} else if waitErr != nil || (cmd.ProcessState != nil && cmd.ProcessState.ExitCode() != 0) {
		status = "failed"
	}
	errorMessage := ""
	if status != "completed" {
		errorMessage = strings.TrimSpace(stderr)
		if errorMessage == "" && waitErr != nil {
			errorMessage = waitErr.Error()
		}
		if errorMessage == "" {
			errorMessage = killReason
		}
	}
	completion := parseCompletion(stdout, stderr)
	if status != "completed" {
		completion = completionParse{ParseStatus: "missing"}
	}
	if completion.Summary == "" {
		completion.Summary = firstNonEmpty(errorMessage, summarizeLogs(stdout, stderr))
	}
	x.mu.Lock()
	x.status = status
	x.nativeResumeStatus = "fallback_completed"
	if status != "completed" {
		x.nativeResumeStatus = "fallback_failed"
	}
	x.mu.Unlock()
	return Result{
		Status:                       status,
		Summary:                      completion.Summary,
		Stdout:                       stdout,
		Stderr:                       stderr,
		ParseStatus:                  completion.ParseStatus,
		CompletionSignal:             completion.CompletionSignal,
		Artifacts:                    append([]string(nil), completion.Artifacts...),
		ChangedFiles:                 append([]string(nil), completion.ChangedFiles...),
		Commits:                      append([]string(nil), completion.Commits...),
		Lifecycle:                    completion.Lifecycle,
		HeartbeatCount:               x.heartbeatCountValue(),
		TimeoutType:                  timeoutType,
		ConfiguredIdleTimeoutSeconds: durationSeconds(x.heartbeatTimeout),
		ConfiguredMaxRuntimeSeconds:  durationSeconds(x.timeout),
		ElapsedRuntimeSeconds:        durationSeconds(x.executor.now().UTC().Sub(x.startedAt)),
		LastProgressAt:               x.lastProgressAtISO(),
		PID:                          pidOrZero(cmd.Process),
	}, errorMessage, true
}

func (x *execution) onOutput(stream string, chunk []byte) {
	now := x.executor.now().UTC()
	nowISO := eventlog.FormatJavaScriptISOString(now)
	x.mu.Lock()
	x.heartbeatCount++
	x.lastHeartbeatAtISO = nowISO
	x.lastOutputAt = now
	if stream == "stdout" {
		if x.appendPersistedLog(x.stdoutLogPath, chunk) {
			x.persistedLogWriteFailed = true
		}
		x.stdout = appendTailBounded(x.stdout, chunk, x.maxOutputBytes)
	} else {
		if x.appendPersistedLog(x.stderrLogPath, chunk) {
			x.persistedLogWriteFailed = true
		}
		x.stderr = appendTailBounded(x.stderr, chunk, x.maxOutputBytes)
	}
	heartbeatCount := x.heartbeatCount
	stdout := string(x.stdout)
	stderr := string(x.stderr)
	if nativeSessionID := extractNativeSessionID(stdout, stderr); nativeSessionID != "" {
		x.nativeSessionID = nativeSessionID
	}
	x.mu.Unlock()

	outputJSON := x.outputJSON(stdout, stderr)
	x.persistStatus(x.currentStatus(), &heartbeatCount, &nowISO, &outputJSON)
	x.bumpRunHeartbeat(nowISO)
}

func (x *execution) bumpRunHeartbeat(nowISO string) {
	if x.input.RunID == "" || x.executor.repos == nil || x.executor.repos.Runs == nil {
		return
	}
	ctx := context.Background()
	run, err := x.executor.repos.Runs.GetByID(ctx, x.input.RunID)
	if err != nil || run == nil {
		return
	}
	updated := *run
	updated.LastHeartbeatAt = &nowISO
	updated.UpdatedAt = nowISO
	_ = x.executor.repos.Runs.Upsert(ctx, updated)
}

func (x *execution) persistStatus(status string, heartbeatCount *int64, heartbeatAt *string, outputJSON *string) {
	if x.executor.repos == nil || x.executor.repos.AgentExecutions == nil {
		return
	}
	nativeSessionID, nativeResumeMode, nativeResumeStatus, nativeResumeError := x.nativeResumeSnapshot()
	metadata := mustJSON(x.executionMetadata(""))
	commandJSON := mustJSON(map[string]any{"command": x.command, "args": x.args})
	pid := int64(pidOrZero(x.process.Process))
	record := storage.AgentExecutionRecord{
		ID:                 x.executionID,
		ProjectID:          emptyToNil(x.input.ProjectID),
		LoopID:             emptyToNil(x.input.LoopID),
		RunID:              emptyToNil(x.input.RunID),
		Vendor:             string(x.executor.config.Vendor),
		Status:             status,
		PID:                int64PtrIfPositive(pid),
		CommandJSON:        &commandJSON,
		CWD:                &x.input.WorkingDirectory,
		HeartbeatCount:     0,
		LastHeartbeatAt:    &x.startedAtISO,
		NativeSessionID:    emptyToNil(nativeSessionID),
		NativeResumeMode:   emptyToNil(nativeResumeMode),
		NativeResumeStatus: emptyToNil(nativeResumeStatus),
		NativeResumeError:  emptyToNil(nativeResumeError),
		StartedAt:          x.startedAtISO,
		MetadataJSON:       &metadata,
		CreatedAt:          x.startedAtISO,
		UpdatedAt:          x.startedAtISO,
	}
	if heartbeatCount != nil {
		record.HeartbeatCount = *heartbeatCount
		record.UpdatedAt = *heartbeatAt
		record.LastHeartbeatAt = heartbeatAt
	}
	if outputJSON != nil {
		record.OutputJSON = outputJSON
	}
	_ = x.executor.repos.AgentExecutions.Upsert(context.Background(), record)
}

func (x *execution) persistFinal(status string, result Result, errorMessage, endedAtISO string) {
	if x.executor.repos == nil || x.executor.repos.AgentExecutions == nil {
		return
	}
	nativeSessionID, nativeResumeMode, nativeResumeStatus, nativeResumeError := x.nativeResumeSnapshot()
	commandJSON := mustJSON(map[string]any{"command": x.command, "args": x.args})
	metadata := mustJSON(x.executionMetadata(result.TimeoutType))
	embeddedStdout := x.stdoutString()
	embeddedStderr := x.stderrString()
	if embeddedStderr == "" && result.Stderr != "" {
		embeddedStderr = result.Stderr
	}
	output := x.outputPayload(embeddedStdout, embeddedStderr)
	output["gitPrLifecycle"] = result.Lifecycle
	outputJSON := mustJSON(output)
	pid := int64(pidOrZero(x.process.Process))
	parseStatus := result.ParseStatus
	completionSignal := emptyToNil(result.CompletionSignal)
	if extractedNativeSessionID := extractNativeSessionID(embeddedStdout, embeddedStderr); extractedNativeSessionID != "" {
		nativeSessionID = extractedNativeSessionID
	}
	if nativeResumeMode == "native_resume" && status == "failed" {
		nativeResumeStatus = "failed"
		nativeResumeError = firstNonEmpty(nativeResumeError, errorMessage, strings.TrimSpace(result.Stderr))
	}
	if nativeSessionID != "" && (nativeResumeStatus == "" || nativeResumeStatus == "unavailable") {
		nativeResumeStatus = "captured"
	}
	record := storage.AgentExecutionRecord{
		ID:                 x.executionID,
		ProjectID:          emptyToNil(x.input.ProjectID),
		LoopID:             emptyToNil(x.input.LoopID),
		RunID:              emptyToNil(x.input.RunID),
		Vendor:             string(x.executor.config.Vendor),
		Status:             status,
		PID:                int64PtrIfPositive(pid),
		CommandJSON:        &commandJSON,
		CWD:                &x.input.WorkingDirectory,
		Summary:            emptyToNil(result.Summary),
		ParseStatus:        &parseStatus,
		CompletionSignal:   completionSignal,
		HeartbeatCount:     result.HeartbeatCount,
		LastHeartbeatAt:    &x.lastHeartbeatAtISO,
		OutputJSON:         &outputJSON,
		ErrorMessage:       emptyToNil(errorMessage),
		NativeSessionID:    emptyToNil(nativeSessionID),
		NativeResumeMode:   emptyToNil(nativeResumeMode),
		NativeResumeStatus: emptyToNil(nativeResumeStatus),
		NativeResumeError:  emptyToNil(nativeResumeError),
		StartedAt:          x.startedAtISO,
		EndedAt:            &endedAtISO,
		MetadataJSON:       &metadata,
		CreatedAt:          x.startedAtISO,
		UpdatedAt:          endedAtISO,
	}
	_ = x.executor.repos.AgentExecutions.Upsert(context.Background(), record)
}

func (x *execution) currentStatus() string {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.status
}

func (x *execution) setStatus(status string) {
	x.mu.Lock()
	x.status = status
	x.mu.Unlock()
}

func (x *execution) finalStatus(timedOut, killed bool) string {
	x.mu.Lock()
	defer x.mu.Unlock()
	if timedOut {
		x.status = "timeout"
		return x.status
	}
	if killed {
		x.status = "killed"
		return x.status
	}
	if x.process.ProcessState != nil && x.process.ProcessState.ExitCode() == 0 {
		x.status = "completed"
		return x.status
	}
	x.status = "failed"
	return x.status
}

func (x *execution) stdoutString() string {
	x.mu.Lock()
	defer x.mu.Unlock()
	return string(x.stdout)
}

func (x *execution) stderrString() string {
	x.mu.Lock()
	defer x.mu.Unlock()
	return string(x.stderr)
}

func (x *execution) heartbeatCountValue() int64 {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.heartbeatCount
}

func (x *execution) lastProgressAtISO() string {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.lastHeartbeatAtISO
}

func (x *execution) nativeResumeSnapshot() (sessionID string, mode string, status string, resumeError string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.nativeSessionID, x.nativeResumeMode, x.nativeResumeStatus, x.nativeResumeError
}

func (x *execution) executionMetadata(timeoutType string) map[string]any {
	metadata := map[string]any{
		"idempotencyKey": emptyToNil(x.input.IdempotencyKey),
		"metadata":       x.input.Metadata,
		"timeoutPolicy": map[string]any{
			"idleTimeoutSeconds": durationSeconds(x.heartbeatTimeout),
			"maxRuntimeSeconds":  durationSeconds(x.timeout),
		},
	}
	if timeoutType != "" {
		metadata["timeout"] = map[string]any{
			"type":                         timeoutType,
			"configuredIdleTimeoutSeconds": durationSeconds(x.heartbeatTimeout),
			"configuredMaxRuntimeSeconds":  durationSeconds(x.timeout),
			"elapsedRuntimeSeconds":        durationSeconds(x.executor.now().UTC().Sub(x.startedAt)),
			"lastProgressAt":               x.lastProgressAtISO(),
		}
	}
	return metadata
}

func durationSeconds(duration time.Duration) int64 {
	if duration <= 0 {
		return 0
	}
	seconds := int64(duration / time.Second)
	if seconds == 0 {
		return 1
	}
	return seconds
}

func (x *execution) resolveOutputLogs() (string, string) {
	stdout := x.stdoutString()
	stderr := x.stderrString()
	if x.hasPersistedLogWriteFailure() {
		return stdout, stderr
	}
	if persisted, ok := readPersistedExecutionLog(x.stdoutLogPath); ok {
		stdout = persisted
	}
	if persisted, ok := readPersistedExecutionLog(x.stderrLogPath); ok {
		stderr = persisted
	}
	return stdout, stderr
}

func (x *execution) hasPersistedLogWriteFailure() bool {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.persistedLogWriteFailed
}

func (x *execution) markPersistedLogWriteFailed() {
	x.mu.Lock()
	x.persistedLogWriteFailed = true
	x.mu.Unlock()
}

func (x *execution) timeSinceLastOutput() time.Duration {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.lastOutputAt.IsZero() {
		return 0
	}
	return x.executor.now().UTC().Sub(x.lastOutputAt)
}

type streamCapture struct {
	onChunk func([]byte)
}

func (w *streamCapture) Write(p []byte) (int, error) {
	if len(p) > 0 && w.onChunk != nil {
		chunk := make([]byte, len(p))
		copy(chunk, p)
		w.onChunk(chunk)
	}
	return len(p), nil
}

func ResolveSpawn(cfg ExecutorConfig, prompt string) (string, []string) {
	command := resolveCommand(cfg)
	args := resolveArgs(cfg, prompt)
	return command, args
}

func ResolveSpawnWithNativeResume(cfg ExecutorConfig, prompt string, sessionID string, enabled bool) (string, []string) {
	if !enabled || strings.TrimSpace(sessionID) == "" || !nativeResumeSupported(cfg.Vendor) {
		return ResolveSpawn(cfg, prompt)
	}
	command := resolveCommand(cfg)
	args := resolveNativeResumeArgs(cfg, stringArgs(cfg.Params["args"]), strings.TrimSpace(sessionID), prompt)
	return command, args
}

func resolveCommand(cfg ExecutorConfig) string {
	if override, ok := cfg.Params["command"].(string); ok && strings.TrimSpace(override) != "" {
		return override
	}
	switch cfg.Vendor {
	case config.AgentVendorClaudeCode:
		return "claude"
	case config.AgentVendorCursorCLI:
		return "agent"
	default:
		return string(cfg.Vendor)
	}
}

func resolveArgs(cfg ExecutorConfig, prompt string) []string {
	resolvedArgs := stringArgs(cfg.Params["args"])
	switch cfg.Vendor {
	case config.AgentVendorClaudeCode:
		return resolveClaudeArgs(cfg, resolvedArgs, prompt)
	case config.AgentVendorCodex:
		return resolveCodexArgs(cfg, resolvedArgs, prompt)
	case config.AgentVendorOpenCode:
		return resolveOpenCodeArgs(cfg, resolvedArgs, prompt)
	case config.AgentVendorCursorCLI:
		return resolveCursorArgs(cfg, resolvedArgs, prompt)
	default:
		return append([]string{}, resolvedArgs...)
	}
}

func resolveClaudeArgs(cfg ExecutorConfig, args []string, prompt string) []string {
	resolved := prependModelFlag(args, cfg.Model, "--model", []string{"--model"})
	if !hasAnyFlag(resolved, []string{"-p", "--print"}) {
		resolved = append(resolved, "--print", prompt)
	}
	if !hasAnyFlag(resolved, []string{"--dangerously-skip-permissions"}) {
		resolved = append(resolved, "--dangerously-skip-permissions")
	}
	return resolved
}

func resolveCodexArgs(cfg ExecutorConfig, args []string, prompt string) []string {
	resolved := append([]string{}, args...)
	if !containsArg(resolved, "exec") {
		resolved = append([]string{"exec"}, resolved...)
	}
	withModel := prependModelFlag(resolved, cfg.Model, "--model", []string{"--model", "-m"})
	if hasAnyFlag(withModel, []string{"-"}) {
		return withModel
	}
	return append(withModel, prompt)
}

func resolveOpenCodeArgs(cfg ExecutorConfig, args []string, prompt string) []string {
	resolved := append([]string{}, args...)
	if !containsArg(resolved, "run") {
		resolved = append([]string{"run"}, resolved...)
	}
	withModel := prependModelFlag(resolved, cfg.Model, "--model", []string{"--model", "-m"})
	if hasAnyFlag(withModel, []string{"-p", "--prompt", "-f", "--file"}) {
		return withModel
	}
	return append(withModel, prompt)
}

func resolveCursorArgs(cfg ExecutorConfig, args []string, prompt string) []string {
	resolved := prependModelFlag(args, cfg.Model, "--model", []string{"--model"})
	if hasAnyFlag(resolved, []string{"-p", "--print"}) {
		return resolved
	}
	return append(resolved, "--print", prompt)
}

func resolveNativeResumeArgs(cfg ExecutorConfig, args []string, sessionID string, prompt string) []string {
	switch cfg.Vendor {
	case config.AgentVendorClaudeCode:
		resolved := prependModelFlag(args, cfg.Model, "--model", []string{"--model"})
		if !hasAnyFlag(resolved, []string{"--continue", "--resume"}) {
			resolved = append(resolved, "--resume", sessionID)
		}
		if !hasAnyFlag(resolved, []string{"-p", "--print"}) {
			resolved = append(resolved, "--print", prompt)
		}
		if !hasAnyFlag(resolved, []string{"--dangerously-skip-permissions"}) {
			resolved = append(resolved, "--dangerously-skip-permissions")
		}
		return resolved
	case config.AgentVendorCodex:
		resolved := removeFirstArg(args, "exec")
		resolved = removeFirstArg(resolved, "resume")
		withModel := prependModelFlag(append([]string{"exec"}, resolved...), cfg.Model, "--model", []string{"--model", "-m"})
		base := append(withModel, "resume")
		base = append(base, sessionID)
		if containsArg(withModel, "-") {
			return base
		}
		return append(base, prompt)
	case config.AgentVendorOpenCode:
		resolved := prependModelFlag(args, cfg.Model, "--model", []string{"--model", "-m"})
		if !hasAnyFlag(resolved, []string{"--session", "--continue"}) {
			resolved = append(resolved, "--session", sessionID)
		}
		if !containsArg(resolved, "run") {
			resolved = append([]string{"run"}, resolved...)
		}
		if !hasAnyFlag(resolved, []string{"-p", "--prompt", "-f", "--file"}) {
			resolved = append(resolved, prompt)
		}
		return resolved
	case config.AgentVendorCursorCLI:
		resolved := prependModelFlag(args, cfg.Model, "--model", []string{"--model"})
		if !hasAnyFlag(resolved, []string{"--continue", "--resume"}) {
			resolved = append(resolved, "--resume", sessionID)
		}
		if !hasAnyFlag(resolved, []string{"-p", "--print"}) {
			resolved = append(resolved, "--print", prompt)
		}
		return resolved
	default:
		return append([]string{}, args...)
	}
}

func prependModelFlag(args []string, model *string, flag string, recognizedFlags []string) []string {
	if model == nil || *model == "" || hasAnyFlag(args, recognizedFlags) {
		return append([]string{}, args...)
	}
	if len(args) > 0 && (args[0] == "exec" || args[0] == "run") {
		return append([]string{args[0], flag, *model}, args[1:]...)
	}
	return append([]string{flag, *model}, args...)
}

func hasAnyFlag(args []string, flags []string) bool {
	for _, flag := range flags {
		for _, arg := range args {
			if arg == flag || (strings.HasPrefix(flag, "--") && strings.HasPrefix(arg, flag+"=")) {
				return true
			}
		}
	}
	return false
}

func containsArg(args []string, target string) bool {
	for _, arg := range args {
		if arg == target {
			return true
		}
	}
	return false
}

func removeFirstArg(args []string, target string) []string {
	resolved := make([]string, 0, len(args))
	removed := false
	for _, arg := range args {
		if !removed && arg == target {
			removed = true
			continue
		}
		resolved = append(resolved, arg)
	}
	return resolved
}

func stringArgs(value any) []string {
	items, ok := value.([]any)
	if !ok {
		if stringsValue, okStrings := value.([]string); okStrings {
			return append([]string{}, stringsValue...)
		}
		return []string{}
	}
	result := make([]string, 0, len(items))
	for _, item := range items {
		if text, ok := item.(string); ok {
			result = append(result, text)
		}
	}
	return result
}

func extractNativeSessionID(outputs ...string) string {
	keys := []string{"nativeSessionId", "native_session_id", "sessionId", "session_id", "chatId", "chat_id"}
	for _, output := range outputs {
		for _, line := range strings.Split(output, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var payload map[string]any
			if err := json.Unmarshal([]byte(line), &payload); err == nil {
				for _, key := range keys {
					if value, ok := payload[key].(string); ok && strings.TrimSpace(value) != "" {
						return strings.TrimSpace(value)
					}
				}
			}
			for _, key := range keys {
				if value := extractKeyValue(line, key); value != "" {
					return value
				}
			}
		}
	}
	return ""
}

func extractKeyValue(line string, key string) string {
	lowerLine := strings.ToLower(line)
	lowerKey := strings.ToLower(key)
	idx := strings.Index(lowerLine, lowerKey)
	if idx < 0 {
		return ""
	}
	if idx > 0 {
		prev := lowerLine[idx-1]
		if isSessionKeyChar(prev) {
			return ""
		}
	}
	after := idx + len(key)
	if after < len(lowerLine) && isSessionKeyChar(lowerLine[after]) {
		return ""
	}
	rest := strings.TrimLeft(line[after:], " \t")
	if strings.HasPrefix(rest, "\"") || strings.HasPrefix(rest, "'") {
		rest = strings.TrimLeft(rest[1:], " \t")
	}
	if rest == "" || (rest[0] != ':' && rest[0] != '=') {
		return ""
	}
	rest = strings.TrimLeft(rest[1:], " \t\"'")
	if rest == "" {
		return ""
	}
	end := len(rest)
	for i, r := range rest {
		if r == ' ' || r == '\t' || r == '\'' || r == '"' || r == ',' || r == '}' {
			end = i
			break
		}
	}
	return strings.Trim(strings.TrimSpace(rest[:end]), "'\"")
}

func isSessionKeyChar(ch byte) bool {
	return (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '_' || ch == '-'
}

func (e *ConfiguredExecutor) executionLogPaths(input RunInput, executionID string) (string, string) {
	if strings.TrimSpace(e.logDir) == "" {
		return "", ""
	}
	runID := safeLogPathSegment(firstNonEmpty(input.RunID, "runless"))
	loopID := safeLogPathSegment(firstNonEmpty(input.LoopID, "loopless"))
	execID := safeLogPathSegment(firstNonEmpty(executionID, "execution"))
	dir := filepath.Join(e.logDir, "loops", loopID, runID)
	return filepath.Join(dir, execID+".stdout.log"), filepath.Join(dir, execID+".stderr.log")
}

func safeLogPathSegment(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || trimmed == "." || trimmed == ".." {
		return "unknown"
	}
	var builder strings.Builder
	for _, r := range trimmed {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			builder.WriteRune(r)
		default:
			builder.WriteRune('_')
		}
	}
	segment := builder.String()
	if segment == "." || segment == ".." {
		return "unknown"
	}
	return segment
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func (x *execution) appendPersistedLog(path string, chunk []byte) bool {
	if path == "" || len(chunk) == 0 {
		return false
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return true
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return true
	}
	n, err := file.Write(chunk)
	writeFailed := err != nil || n != len(chunk)
	if err := file.Close(); err != nil {
		writeFailed = true
	}
	return writeFailed
}

func (x *execution) initializePersistedLogs() {
	for _, path := range []string{x.stdoutLogPath, x.stderrLogPath} {
		if path == "" {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			continue
		}
		file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
		if err != nil {
			continue
		}
		_ = file.Close()
	}
}

func readPersistedExecutionLog(path string) (string, bool) {
	if path == "" {
		return "", false
	}
	file, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", false
	}
	if info.Size() > maxPersistedLogReadBytes {
		if _, err := file.Seek(info.Size()-maxPersistedLogReadBytes, io.SeekStart); err != nil {
			return "", false
		}
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxPersistedLogReadBytes))
	if err != nil {
		return "", false
	}
	return string(raw), true
}

func (x *execution) outputJSON(stdout, stderr string) string {
	return mustJSON(x.outputPayload(stdout, stderr))
}

func (x *execution) outputPayload(stdout, stderr string) map[string]any {
	payload := map[string]any{"stdout": stdout, "stderr": stderr}
	if x.stdoutLogPath != "" {
		payload["stdoutLogPath"] = x.stdoutLogPath
	}
	if x.stderrLogPath != "" {
		payload["stderrLogPath"] = x.stderrLogPath
	}
	return payload
}

func appendTailBounded(existing []byte, chunk []byte, maxBytes int) []byte {
	combined := append(existing, chunk...)
	if len(combined) <= maxBytes {
		return combined
	}
	return append([]byte{}, combined[len(combined)-maxBytes:]...)
}

func tickerChan(ticker *time.Ticker) <-chan time.Time {
	if ticker == nil {
		return nil
	}
	return ticker.C
}

func parseCompletion(stdout, stderr string) completionParse {
	raw := stdout + "\n" + stderr
	lines := strings.Split(raw, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if strings.HasPrefix(line, CompletionMarkerPrefix) {
			payload := strings.TrimPrefix(line, CompletionMarkerPrefix)
			var parsed map[string]any
			if err := json.Unmarshal([]byte(payload), &parsed); err != nil {
				return completionParse{ParseStatus: "invalid_json", CompletionSignal: CompletionMarkerPrefix}
			}
			result := completionParse{
				ParseStatus:      "parsed",
				CompletionSignal: CompletionMarkerPrefix,
				Artifacts:        asStringSlice(parsed["artifacts"]),
				ChangedFiles:     asStringSlice(parsed["changedFiles"]),
				Commits:          asStringSlice(parsed["commits"]),
			}
			if state, err := lifecycle.FromMap(parsed["git_pr_lifecycle"]); err == nil {
				result.Lifecycle = state
			}
			if summary, ok := parsed["summary"].(string); ok {
				result.Summary = summary
			}
			if isTemplateCompletion(result, parsed) {
				continue
			}
			return result
		}
	}
	return completionParse{ParseStatus: "missing"}
}

func isTemplateCompletion(result completionParse, parsed map[string]any) bool {
	if strings.TrimSpace(result.Summary) != "<one-sentence summary>" {
		return false
	}
	if len(parsed) != 1 {
		return false
	}
	_, ok := parsed["summary"]
	return ok
}

func IsAgentSetupFailureMessage(message string) bool {
	normalized := strings.ToLower(strings.TrimSpace(message))
	if normalized == "" {
		return false
	}
	for _, line := range strings.Split(normalized, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if isCodexVersionSetupFailure(line) || isAgentModelSetupFailure(line) {
			return true
		}
	}
	return false
}

func isCodexVersionSetupFailure(line string) bool {
	return strings.Contains(line, " model requires a newer version of codex") &&
		strings.Contains(line, "please upgrade to the latest app or cli")
}

func isAgentModelSetupFailure(line string) bool {
	modelFailurePhrases := []string{
		"unsupported model",
		"unknown model",
		"invalid model",
		"model is not supported",
		"unrecognized model",
	}
	if !containsAny(line, modelFailurePhrases) {
		return false
	}
	return containsAny(line, []string{"agent setup", "agent configuration", "configured model", "model configuration", "--model", "model:"}) &&
		containsAny(line, []string{"codex", "claude", "opencode", "cursor"})
}

func containsAny(value string, patterns []string) bool {
	for _, pattern := range patterns {
		if strings.Contains(value, pattern) {
			return true
		}
	}
	return false
}

func asStringSlice(value any) []string {
	items, ok := value.([]any)
	if !ok {
		if stringsValue, okStrings := value.([]string); okStrings {
			return append([]string(nil), stringsValue...)
		}
		return []string{}
	}
	result := make([]string, 0, len(items))
	for _, item := range items {
		if text, ok := item.(string); ok {
			result = append(result, text)
		}
	}
	return result
}

func summarizeLogs(stdout, stderr string) string {
	combined := strings.TrimSpace(stdout + "\n" + stderr)
	if combined == "" {
		return ""
	}
	parts := strings.Split(combined, "\n")
	for i := len(parts) - 1; i >= 0; i-- {
		line := strings.TrimSpace(parts[i])
		if line != "" {
			return line
		}
	}
	return ""
}

func (e *ConfiguredExecutor) appendLifecycleEvent(eventType string, input RunInput, executionID string, payload any, createdAt string) {
	if e.repos == nil || e.repos.Events == nil {
		return
	}
	_ = e.repos.Events.Append(context.Background(), storage.EventLogRecord{
		ID:               eventlog.NewEventID("event"),
		EventType:        eventType,
		ProjectID:        emptyToNil(input.ProjectID),
		LoopID:           emptyToNil(input.LoopID),
		RunID:            emptyToNil(input.RunID),
		EntityType:       stringPtr("agent_execution"),
		EntityID:         &executionID,
		ActorType:        stringPtr("agent"),
		ActorID:          stringPtr(string(e.config.Vendor)),
		ActorDisplayName: stringPtr(string(e.config.Vendor)),
		PayloadJSON:      mustJSON(payload),
		CreatedAt:        createdAt,
	})
}

func mustJSON(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "{}"
	}
	return string(encoded)
}

func emptyToNil(value string) *string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	v := value
	return &v
}

func int64PtrIfPositive(value int64) *int64 {
	if value <= 0 {
		return nil
	}
	v := value
	return &v
}

func stringPtr(value string) *string {
	return &value
}

func pidOrZero(process *os.Process) int {
	if process == nil {
		return 0
	}
	return process.Pid
}
