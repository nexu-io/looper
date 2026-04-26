package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/powerformer/looper/internal/config"
	"github.com/powerformer/looper/internal/eventlog"
	"github.com/powerformer/looper/internal/storage"
)

const (
	defaultMaxOutputBytes = 256 * 1024
	completionMarkerEnv   = "LOOPER_COMPLETION_MARKER"
)

type ExecutorConfig struct {
	Vendor config.AgentVendor
	Model  *string
	Params map[string]any
	Env    map[string]string
}

type ExecutorOptions struct {
	Config ExecutorConfig
	Repos  *storage.Repositories
	Now    func() time.Time
}

type RunInput struct {
	ExecutionID      string
	ProjectID        string
	LoopID           string
	RunID            string
	Prompt           string
	WorkingDirectory string
	Timeout          time.Duration
	HeartbeatTimeout time.Duration
	GracefulShutdown time.Duration
	MaxOutputBytes   int
	Metadata         map[string]any
	IdempotencyKey   string
	Env              map[string]string
}

type Result struct {
	Status           string
	Summary          string
	Stdout           string
	Stderr           string
	ParseStatus      string
	CompletionSignal string
	Artifacts        []string
	ChangedFiles     []string
	Commits          []string
	HeartbeatCount   int64
	PID              int
}

type completionParse struct {
	ParseStatus      string
	CompletionSignal string
	Summary          string
	Artifacts        []string
	ChangedFiles     []string
	Commits          []string
}

type Execution interface {
	Wait(context.Context) (Result, error)
	Kill(string) error
}

type ConfiguredExecutor struct {
	config ExecutorConfig
	repos  *storage.Repositories
	now    func() time.Time
}

func New(options ExecutorOptions) *ConfiguredExecutor {
	now := options.Now
	if now == nil {
		now = time.Now
	}
	return &ConfiguredExecutor{config: options.Config, repos: options.Repos, now: now}
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
	command, args := ResolveSpawn(e.config, input.Prompt)

	cmd := exec.Command(command, args...)
	cmd.Dir = input.WorkingDirectory
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

	maxOutputBytes := input.MaxOutputBytes
	if maxOutputBytes <= 0 {
		maxOutputBytes = defaultMaxOutputBytes
	}

	x := &execution{
		executor:           e,
		input:              input,
		executionID:        executionID,
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
		killCh:             make(chan string, 1),
		doneCh:             make(chan execOutcome, 1),
	}
	cmd.Stdout = &streamCapture{onChunk: func(chunk []byte) { x.onOutput("stdout", chunk) }}
	cmd.Stderr = &streamCapture{onChunk: func(chunk []byte) { x.onOutput("stderr", chunk) }}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start agent command: %w", err)
	}

	x.persistStatus("running", nil, nil, nil)
	e.appendLifecycleEvent("agent.invoked", input, executionID, map[string]any{"command": command, "args": args, "cwd": input.WorkingDirectory}, startedAtISO)

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

	mu             sync.Mutex
	status         string
	stdout         []byte
	stderr         []byte
	heartbeatCount int64

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

func (x *execution) run(ctx context.Context) {
	waitCh := make(chan error, 1)
	go func() { waitCh <- x.process.Wait() }()

	var (
		waitErr         error
		timedOut        bool
		killed          bool
		killReason      string
		graceKillTimer  <-chan time.Time
		timeoutTimer    <-chan time.Time
		inactivityTimer *time.Ticker
		terminateOnce   sync.Once
		terminateSignal = func() {
			terminateOnce.Do(func() {
				if x.process.Process == nil {
					return
				}
				if err := x.process.Process.Signal(syscall.SIGTERM); err != nil && err != os.ErrProcessDone {
					_ = x.process.Process.Kill()
					return
				}
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
			timedOut = true
			if killReason == "" {
				killReason = "agent timed out"
			}
			x.setStatus("timeout")
			terminateSignal()
		case <-tickerChan(inactivityTimer):
			if timedOut || killed || x.process.ProcessState != nil {
				continue
			}
			if x.timeSinceLastOutput() < x.heartbeatTimeout {
				continue
			}
			timedOut = true
			if killReason == "" {
				killReason = fmt.Sprintf("agent heartbeat timed out after %s", x.heartbeatTimeout)
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
			if x.process.Process != nil {
				_ = x.process.Process.Kill()
			}
		}
	}

	stdout := x.stdoutString()
	stderr := x.stderrString()
	status := x.finalStatus(timedOut, killed)
	if waitErr != nil && status == "failed" && strings.TrimSpace(stderr) == "" {
		stderr = waitErr.Error()
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
	result := Result{
		Status:           status,
		Summary:          completion.Summary,
		Stdout:           stdout,
		Stderr:           stderr,
		ParseStatus:      completion.ParseStatus,
		CompletionSignal: completion.CompletionSignal,
		Artifacts:        append([]string(nil), completion.Artifacts...),
		ChangedFiles:     append([]string(nil), completion.ChangedFiles...),
		Commits:          append([]string(nil), completion.Commits...),
		HeartbeatCount:   x.heartbeatCountValue(),
		PID:              pidOrZero(x.process.Process),
	}

	x.persistFinal(status, result, errorMessage, endedAtISO)
	eventType := "agent.completed"
	if status == "timeout" {
		eventType = "agent.timed_out"
	} else if status == "killed" {
		eventType = "agent.killed"
	}
	x.executor.appendLifecycleEvent(eventType, x.input, x.executionID, map[string]any{
		"status":         status,
		"parseStatus":    result.ParseStatus,
		"heartbeatCount": result.HeartbeatCount,
		"summary":        result.Summary,
	}, endedAtISO)

	x.doneCh <- execOutcome{result: result, err: nil}
}

func (x *execution) onOutput(stream string, chunk []byte) {
	now := x.executor.now().UTC()
	nowISO := eventlog.FormatJavaScriptISOString(now)
	x.mu.Lock()
	x.heartbeatCount++
	x.lastHeartbeatAtISO = nowISO
	x.lastOutputAt = now
	if stream == "stdout" {
		x.stdout = appendTailBounded(x.stdout, chunk, x.maxOutputBytes)
	} else {
		x.stderr = appendTailBounded(x.stderr, chunk, x.maxOutputBytes)
	}
	heartbeatCount := x.heartbeatCount
	stdout := string(x.stdout)
	stderr := string(x.stderr)
	x.mu.Unlock()

	outputJSON := mustJSON(map[string]string{"stdout": stdout, "stderr": stderr})
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
	metadata := mustJSON(map[string]any{"idempotencyKey": emptyToNil(x.input.IdempotencyKey), "metadata": x.input.Metadata})
	commandJSON := mustJSON(map[string]any{"command": x.command, "args": x.args})
	pid := int64(pidOrZero(x.process.Process))
	record := storage.AgentExecutionRecord{
		ID:              x.executionID,
		ProjectID:       emptyToNil(x.input.ProjectID),
		LoopID:          emptyToNil(x.input.LoopID),
		RunID:           emptyToNil(x.input.RunID),
		Vendor:          string(x.executor.config.Vendor),
		Status:          status,
		PID:             int64PtrIfPositive(pid),
		CommandJSON:     &commandJSON,
		CWD:             &x.input.WorkingDirectory,
		HeartbeatCount:  0,
		LastHeartbeatAt: &x.startedAtISO,
		StartedAt:       x.startedAtISO,
		MetadataJSON:    &metadata,
		CreatedAt:       x.startedAtISO,
		UpdatedAt:       x.startedAtISO,
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
	commandJSON := mustJSON(map[string]any{"command": x.command, "args": x.args})
	metadata := mustJSON(map[string]any{"idempotencyKey": emptyToNil(x.input.IdempotencyKey), "metadata": x.input.Metadata})
	outputJSON := mustJSON(map[string]string{"stdout": result.Stdout, "stderr": result.Stderr})
	pid := int64(pidOrZero(x.process.Process))
	parseStatus := result.ParseStatus
	completionSignal := emptyToNil(result.CompletionSignal)
	record := storage.AgentExecutionRecord{
		ID:               x.executionID,
		ProjectID:        emptyToNil(x.input.ProjectID),
		LoopID:           emptyToNil(x.input.LoopID),
		RunID:            emptyToNil(x.input.RunID),
		Vendor:           string(x.executor.config.Vendor),
		Status:           status,
		PID:              int64PtrIfPositive(pid),
		CommandJSON:      &commandJSON,
		CWD:              &x.input.WorkingDirectory,
		Summary:          emptyToNil(result.Summary),
		ParseStatus:      &parseStatus,
		CompletionSignal: completionSignal,
		HeartbeatCount:   result.HeartbeatCount,
		LastHeartbeatAt:  &x.lastHeartbeatAtISO,
		OutputJSON:       &outputJSON,
		ErrorMessage:     emptyToNil(errorMessage),
		StartedAt:        x.startedAtISO,
		EndedAt:          &endedAtISO,
		MetadataJSON:     &metadata,
		CreatedAt:        x.startedAtISO,
		UpdatedAt:        endedAtISO,
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
	if hasAnyFlag(resolved, []string{"-p", "--print"}) {
		return resolved
	}
	return append(resolved, "--print", prompt)
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
			if arg == flag {
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
