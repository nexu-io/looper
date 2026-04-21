package agent

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/powerformer/looper/internal/config"
	"github.com/powerformer/looper/internal/storage"
)

func TestResolveSpawnVendorParity(t *testing.T) {
	t.Parallel()

	model := "gpt-5"
	command, args := ResolveSpawn(ExecutorConfig{Vendor: config.AgentVendorClaudeCode, Model: &model}, "hello")
	if command != "claude" {
		t.Fatalf("claude command = %q, want claude", command)
	}
	if len(args) < 4 || args[0] != "--model" || args[2] != "--print" {
		t.Fatalf("claude args = %#v, want --model <model> --print <prompt>", args)
	}

	command, args = ResolveSpawn(ExecutorConfig{Vendor: config.AgentVendorCodex, Model: &model}, "hello")
	if command != "codex" {
		t.Fatalf("codex command = %q, want codex", command)
	}
	if len(args) < 4 || args[0] != "exec" || args[1] != "--model" || args[len(args)-1] != "hello" {
		t.Fatalf("codex args = %#v, want exec --model <model> <prompt>", args)
	}

	command, args = ResolveSpawn(ExecutorConfig{Vendor: config.AgentVendorOpenCode, Model: &model}, "hello")
	if command != "opencode" {
		t.Fatalf("opencode command = %q, want opencode", command)
	}
	if len(args) < 4 || args[0] != "run" || args[1] != "--model" || args[len(args)-1] != "hello" {
		t.Fatalf("opencode args = %#v, want run --model <model> <prompt>", args)
	}

	command, args = ResolveSpawn(ExecutorConfig{Vendor: config.AgentVendorCursorCLI, Model: &model}, "hello")
	if command != "agent" {
		t.Fatalf("cursor command = %q, want agent", command)
	}
	if len(args) < 4 || args[0] != "--model" || args[2] != "--print" {
		t.Fatalf("cursor args = %#v, want --model <model> --print <prompt>", args)
	}
}

func TestResolveSpawnCodexDoesNotDuplicateExecSubcommand(t *testing.T) {
	t.Parallel()

	model := "gpt-5"
	command, args := ResolveSpawn(ExecutorConfig{
		Vendor: config.AgentVendorCodex,
		Model:  &model,
		Params: map[string]any{"args": []any{"--profile", "test", "exec"}},
	}, "hello")
	if command != "codex" {
		t.Fatalf("codex command = %q, want codex", command)
	}
	if strings.Join(args, " ") != "--model gpt-5 --profile test exec hello" {
		t.Fatalf("codex args = %#v, want single exec subcommand preserved", args)
	}
}

func TestResolveSpawnOpenCodeDoesNotDuplicateRunSubcommand(t *testing.T) {
	t.Parallel()

	model := "gpt-5"
	command, args := ResolveSpawn(ExecutorConfig{
		Vendor: config.AgentVendorOpenCode,
		Model:  &model,
		Params: map[string]any{"args": []any{"--profile", "test", "run"}},
	}, "hello")
	if command != "opencode" {
		t.Fatalf("opencode command = %q, want opencode", command)
	}
	if strings.Join(args, " ") != "--model gpt-5 --profile test run hello" {
		t.Fatalf("opencode args = %#v, want single run subcommand preserved", args)
	}
}

func TestExecutorSuccessfulExecutionPersistsExecutionAndEvents(t *testing.T) {
	t.Parallel()

	coordinator := openAgentCoordinator(t)
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 20, 12, 0, 0, 0, time.UTC)
	executor := New(ExecutorOptions{
		Config: ExecutorConfig{Vendor: config.AgentVendor("custom"), Params: map[string]any{"command": "/bin/sh", "args": []any{"-c", `printf 'ok\n'; printf '__LOOPER_RESULT__={"summary":"done","artifacts":["spec.md"],"changedFiles":["main.go"],"commits":["abc123"]}\n'`}}},
		Repos:  repos,
		Now: func() time.Time {
			now = now.Add(10 * time.Millisecond)
			return now
		},
	})

	execHandle, err := executor.Start(context.Background(), RunInput{ExecutionID: "agent_1", WorkingDirectory: t.TempDir(), Prompt: "ignored", Timeout: time.Second})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	result, err := execHandle.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if result.Status != "completed" {
		t.Fatalf("result.Status = %q, want completed", result.Status)
	}
	if result.ParseStatus != "parsed" || result.CompletionSignal != CompletionMarkerPrefix || result.Summary != "done" {
		t.Fatalf("result = %#v, want parsed completion marker result", result)
	}
	if len(result.Artifacts) != 1 || result.Artifacts[0] != "spec.md" || len(result.ChangedFiles) != 1 || result.ChangedFiles[0] != "main.go" || len(result.Commits) != 1 || result.Commits[0] != "abc123" {
		t.Fatalf("result parsed fields = %#v, want artifacts/changedFiles/commits", result)
	}

	record, err := repos.AgentExecutions.GetByID(context.Background(), "agent_1")
	if err != nil {
		t.Fatalf("AgentExecutions.GetByID() error = %v", err)
	}
	if record == nil || record.Status != "completed" || record.EndedAt == nil {
		t.Fatalf("agent execution record = %#v, want completed with endedAt", record)
	}
	if record.ParseStatus == nil || *record.ParseStatus != "parsed" || record.CompletionSignal == nil || *record.CompletionSignal != CompletionMarkerPrefix {
		t.Fatalf("agent execution record = %#v, want parse status + completion signal", record)
	}

	events, err := repos.Events.ListByEntity(context.Background(), "agent_execution", "agent_1")
	if err != nil {
		t.Fatalf("Events.ListByEntity() error = %v", err)
	}
	if !containsEvent(events, "agent.invoked") || !containsEvent(events, "agent.completed") {
		t.Fatalf("agent events = %#v, want invoked + completed", events)
	}
}

func TestExecutorMissingCompletionFallsBackToLastLogLine(t *testing.T) {
	t.Parallel()

	executor := New(ExecutorOptions{Config: ExecutorConfig{Vendor: config.AgentVendor("custom"), Params: map[string]any{"command": "/bin/sh", "args": []any{"-c", "printf 'first\nsecond\n'"}}}})
	execHandle, err := executor.Start(context.Background(), RunInput{ExecutionID: "agent_missing", WorkingDirectory: t.TempDir(), Prompt: "ignored", Timeout: time.Second})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	result, err := execHandle.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if result.ParseStatus != "missing" || result.CompletionSignal != "" || result.Summary != "second" {
		t.Fatalf("result = %#v, want missing parse status and fallback summary", result)
	}
}

func TestExecutorInvalidJSONCompletionPreservesSignalAndFallsBackToLogs(t *testing.T) {
	t.Parallel()

	executor := New(ExecutorOptions{Config: ExecutorConfig{Vendor: config.AgentVendor("custom"), Params: map[string]any{"command": "/bin/sh", "args": []any{"-c", `printf 'before\n'; printf '__LOOPER_RESULT__={bad json}\n'`}}}})
	execHandle, err := executor.Start(context.Background(), RunInput{ExecutionID: "agent_invalid", WorkingDirectory: t.TempDir(), Prompt: "ignored", Timeout: time.Second})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	result, err := execHandle.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if result.ParseStatus != "invalid_json" || result.CompletionSignal != CompletionMarkerPrefix || result.Summary != "__LOOPER_RESULT__={bad json}" {
		t.Fatalf("result = %#v, want invalid_json with fallback summary and signal", result)
	}
}

func TestExecutorHeartbeatUpdatesWhileOutputArrives(t *testing.T) {
	t.Parallel()

	coordinator := openAgentCoordinator(t)
	repos := storage.NewRepositories(coordinator.DB())
	executor := New(ExecutorOptions{Config: ExecutorConfig{Vendor: config.AgentVendor("custom"), Params: map[string]any{"command": "/bin/sh", "args": []any{"-c", "for i in 1 2 3; do printf \"beat$i\\n\"; sleep 0.05; done"}}}, Repos: repos})

	execHandle, err := executor.Start(context.Background(), RunInput{ExecutionID: "agent_hb", WorkingDirectory: t.TempDir(), Prompt: "ignored", Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if _, err := execHandle.Wait(context.Background()); err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	record, err := repos.AgentExecutions.GetByID(context.Background(), "agent_hb")
	if err != nil {
		t.Fatalf("AgentExecutions.GetByID() error = %v", err)
	}
	if record == nil || record.HeartbeatCount < 3 || record.LastHeartbeatAt == nil {
		t.Fatalf("heartbeat record = %#v, want >=3 heartbeats with timestamp", record)
	}
}

func TestExecutorCapturesConcurrentStdoutAndStderr(t *testing.T) {
	t.Parallel()

	executor := New(ExecutorOptions{Config: ExecutorConfig{Vendor: config.AgentVendor("custom"), Params: map[string]any{"command": "/bin/sh", "args": []any{"-c", `for i in 1 2 3; do printf "out$i\n"; printf "err$i\n" >&2; sleep 0.02; done`}}}})
	execHandle, err := executor.Start(context.Background(), RunInput{ExecutionID: "agent_streams", WorkingDirectory: t.TempDir(), Prompt: "ignored", Timeout: time.Second})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	result, err := execHandle.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if result.Status != "completed" {
		t.Fatalf("result.Status = %q, want completed", result.Status)
	}
	if result.Stdout == "" || result.Stderr == "" {
		t.Fatalf("result = %#v, want both stdout and stderr captured", result)
	}
	for _, want := range []string{"out1", "out2", "out3"} {
		if !containsText(result.Stdout, want) {
			t.Fatalf("stdout = %q, want %q", result.Stdout, want)
		}
	}
	for _, want := range []string{"err1", "err2", "err3"} {
		if !containsText(result.Stderr, want) {
			t.Fatalf("stderr = %q, want %q", result.Stderr, want)
		}
	}
}

func TestExecutorBoundsCapturedOutputToTail(t *testing.T) {
	t.Parallel()

	executor := New(ExecutorOptions{Config: ExecutorConfig{Vendor: config.AgentVendor("custom"), Params: map[string]any{"command": "/bin/sh", "args": []any{"-c", `printf 'abcdefgh'; printf '12345678' >&2`}}}})
	execHandle, err := executor.Start(context.Background(), RunInput{ExecutionID: "agent_bounded", WorkingDirectory: t.TempDir(), Prompt: "ignored", Timeout: time.Second, MaxOutputBytes: 4})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	result, err := execHandle.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if result.Stdout != "efgh" {
		t.Fatalf("stdout = %q, want efgh", result.Stdout)
	}
	if result.Stderr != "5678" {
		t.Fatalf("stderr = %q, want 5678", result.Stderr)
	}
}

func TestExecutorTimeoutMarksTimeout(t *testing.T) {
	t.Parallel()

	coordinator := openAgentCoordinator(t)
	repos := storage.NewRepositories(coordinator.DB())
	executor := New(ExecutorOptions{Config: ExecutorConfig{Vendor: config.AgentVendor("custom"), Params: map[string]any{"command": "/bin/sh", "args": []any{"-c", "sleep 1"}}}, Repos: repos})

	execHandle, err := executor.Start(context.Background(), RunInput{ExecutionID: "agent_timeout", WorkingDirectory: t.TempDir(), Prompt: "ignored", Timeout: 100 * time.Millisecond, GracefulShutdown: 10 * time.Millisecond})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	result, err := execHandle.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if result.Status != "timeout" {
		t.Fatalf("timeout status = %q, want timeout", result.Status)
	}
}

func TestExecutorHeartbeatTimeoutMarksTimeout(t *testing.T) {
	t.Parallel()

	executor := New(ExecutorOptions{Config: ExecutorConfig{Vendor: config.AgentVendor("custom"), Params: map[string]any{"command": "/bin/sh", "args": []any{"-c", "printf 'beat\n'; sleep 1"}}}})

	execHandle, err := executor.Start(context.Background(), RunInput{ExecutionID: "agent_heartbeat_timeout", WorkingDirectory: t.TempDir(), Prompt: "ignored", Timeout: time.Second, HeartbeatTimeout: 50 * time.Millisecond, GracefulShutdown: 10 * time.Millisecond})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	result, err := execHandle.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if result.Status != "timeout" {
		t.Fatalf("result.Status = %q, want timeout", result.Status)
	}
}

func TestExecutorKillEscalationForcesExitAfterGracePeriod(t *testing.T) {
	t.Parallel()

	executor := New(ExecutorOptions{Config: ExecutorConfig{Vendor: config.AgentVendor("custom"), Params: map[string]any{"command": "/bin/sh", "args": []any{"-c", `trap '' TERM; while true; do sleep 0.05; done`}}}})
	startedAt := time.Now()
	execHandle, err := executor.Start(context.Background(), RunInput{ExecutionID: "agent_kill_escalation", WorkingDirectory: t.TempDir(), Prompt: "ignored", Timeout: 50 * time.Millisecond, GracefulShutdown: 20 * time.Millisecond})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	result, err := execHandle.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if result.Status != "timeout" {
		t.Fatalf("result.Status = %q, want timeout", result.Status)
	}
	if elapsed := time.Since(startedAt); elapsed > 500*time.Millisecond {
		t.Fatalf("Wait() elapsed = %s, want kill escalation to finish promptly", elapsed)
	}
}

func TestExecutorExplicitKillMarksKilled(t *testing.T) {
	t.Parallel()

	coordinator := openAgentCoordinator(t)
	repos := storage.NewRepositories(coordinator.DB())
	executor := New(ExecutorOptions{Config: ExecutorConfig{Vendor: config.AgentVendor("custom"), Params: map[string]any{"command": "/bin/sh", "args": []any{"-c", "sleep 2"}}}, Repos: repos})

	execHandle, err := executor.Start(context.Background(), RunInput{ExecutionID: "agent_kill", WorkingDirectory: t.TempDir(), Prompt: "ignored", Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if err := execHandle.Kill("test kill"); err != nil {
		t.Fatalf("Kill() error = %v", err)
	}

	result, err := execHandle.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if result.Status != "killed" {
		t.Fatalf("result status = %q, want killed", result.Status)
	}
}

func openAgentCoordinator(t *testing.T) *storage.SQLiteCoordinator {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "agent.sqlite")
	backupDir := filepath.Join(t.TempDir(), "backup")
	coordinator, err := storage.OpenSQLiteCoordinator(context.Background(), dbPath, storage.SQLiteCoordinatorOptions{BackupDir: backupDir})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	if _, err := coordinator.MigrationRunner().RunPending(context.Background(), storage.RunPendingOptions{}); err != nil {
		t.Fatalf("RunPending() error = %v", err)
	}
	t.Cleanup(func() {
		_ = coordinator.Close()
	})
	return coordinator
}

func containsEvent(events []storage.EventLogRecord, eventType string) bool {
	for _, event := range events {
		if event.EventType == eventType {
			return true
		}
	}
	return false
}

func containsText(haystack, needle string) bool {
	return strings.Contains(haystack, needle)
}
