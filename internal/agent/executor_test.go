package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
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

func TestResolveSpawnWithNativeResumeVendorArgs(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		vendor config.AgentVendor
		want   string
	}{
		{name: "claude", vendor: config.AgentVendorClaudeCode, want: "--resume session-123 --print hello"},
		{name: "codex", vendor: config.AgentVendorCodex, want: "exec resume session-123 hello"},
		{name: "opencode", vendor: config.AgentVendorOpenCode, want: "run --session session-123 hello"},
		{name: "cursor", vendor: config.AgentVendorCursorCLI, want: "--resume session-123 --print hello"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, args := ResolveSpawnWithNativeResume(ExecutorConfig{Vendor: tc.vendor}, "hello", "session-123", true)
			if got := strings.Join(args, " "); got != tc.want {
				t.Fatalf("args = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestResolveSpawnWithNativeResumeCodexPreservesExecOptions(t *testing.T) {
	t.Parallel()

	model := "gpt-5"
	_, args := ResolveSpawnWithNativeResume(ExecutorConfig{
		Vendor: config.AgentVendorCodex,
		Model:  &model,
		Params: map[string]any{"args": []any{"exec", "--json", "--sandbox", "workspace-write"}},
	}, "hello", "session-123", true)
	if got, want := strings.Join(args, " "), "exec --model gpt-5 --json --sandbox workspace-write resume session-123 hello"; got != want {
		t.Fatalf("args = %q, want %q", got, want)
	}
}

func TestResolveSpawnWithNativeResumeDoesNotDuplicateEqualsFlags(t *testing.T) {
	t.Parallel()

	model := "gpt-5"
	_, args := ResolveSpawnWithNativeResume(ExecutorConfig{
		Vendor: config.AgentVendorOpenCode,
		Model:  &model,
		Params: map[string]any{"args": []any{"run", "--model=gpt-4", "--session=existing", "--prompt=from-args"}},
	}, "hello", "session-123", true)
	if got, want := strings.Join(args, " "), "run --model=gpt-4 --session=existing --prompt=from-args"; got != want {
		t.Fatalf("args = %q, want %q", got, want)
	}
}

func TestRecoverableNativeResumeSourceAllowsCompletedPending(t *testing.T) {
	t.Parallel()

	pending := "pending"
	if !isRecoverableNativeResumeSource("completed", &pending) {
		t.Fatalf("isRecoverableNativeResumeSource(completed, pending) = false, want true")
	}
	notPending := "captured"
	if isRecoverableNativeResumeSource("completed", &notPending) {
		t.Fatalf("isRecoverableNativeResumeSource(completed, captured) = true, want false")
	}
}

func TestExtractNativeSessionID(t *testing.T) {
	t.Parallel()

	if got := extractNativeSessionID(`{"session_id":"session-json"}`); got != "session-json" {
		t.Fatalf("extractNativeSessionID(json) = %q, want session-json", got)
	}
	if got := extractNativeSessionID("started chatId: cursor-chat-1"); got != "cursor-chat-1" {
		t.Fatalf("extractNativeSessionID(text) = %q, want cursor-chat-1", got)
	}
	if got := extractNativeSessionID("session_id not found"); got != "" {
		t.Fatalf("extractNativeSessionID(error text) = %q, want empty session ID", got)
	}
	if got := extractNativeSessionID(`event "session_id": "session-quoted"`); got != "session-quoted" {
		t.Fatalf("extractNativeSessionID(quoted text) = %q, want session-quoted", got)
	}
}

func TestOnOutputRecomputesNativeSessionIDFromBufferedOutput(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.April, 20, 12, 0, 0, 0, time.UTC)
	x := &execution{
		executor:       New(ExecutorOptions{Now: func() time.Time { return now }}),
		maxOutputBytes: 1024,
	}
	x.onOutput("stdout", []byte("session_id: abc"))
	if got := x.nativeSessionID; got != "abc" {
		t.Fatalf("nativeSessionID after first chunk = %q, want abc", got)
	}
	x.onOutput("stdout", []byte("def\n"))
	if got := x.nativeSessionID; got != "abcdef" {
		t.Fatalf("nativeSessionID after second chunk = %q, want abcdef", got)
	}
}

func TestExecutorResumesPersistedNativeSession(t *testing.T) {
	t.Parallel()

	coordinator := openAgentCoordinator(t)
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 20, 12, 0, 0, 0, time.UTC)
	nowISO := now.Format("2006-01-02T15:04:05.000Z")
	sessionID := "codex-session-1"
	mode := "native_resume"
	status := "pending"
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "project_1", Name: "Project", RepoPath: t.TempDir(), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: "loop_1", Seq: 1, ProjectID: "project_1", Type: "worker", TargetType: "issue", Status: "running", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	if err := repos.AgentExecutions.Upsert(context.Background(), storage.AgentExecutionRecord{
		ID:                 "agent_previous",
		ProjectID:          strPtr("project_1"),
		LoopID:             strPtr("loop_1"),
		Vendor:             string(config.AgentVendorCodex),
		Status:             "killed",
		NativeSessionID:    &sessionID,
		NativeResumeMode:   &mode,
		NativeResumeStatus: &status,
		StartedAt:          nowISO,
		CreatedAt:          nowISO,
		UpdatedAt:          nowISO,
	}); err != nil {
		t.Fatalf("AgentExecutions.Upsert() error = %v", err)
	}

	scriptDir := t.TempDir()
	argsPath := filepath.Join(scriptDir, "args.txt")
	scriptPath := filepath.Join(scriptDir, "mock-codex")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" > \"$ARGS_PATH\"\nprintf '%s\\n' '{\"session_id\":\"codex-session-1\"}'\nprintf '%s\\n' '__LOOPER_RESULT__={\"summary\":\"resumed\"}'\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile(scriptPath) error = %v", err)
	}
	executor := New(ExecutorOptions{
		Config: ExecutorConfig{Vendor: config.AgentVendorCodex, Params: map[string]any{"command": scriptPath}, NativeResumeEnabled: true},
		Repos:  repos,
		Now: func() time.Time {
			now = now.Add(10 * time.Millisecond)
			return now
		},
	})

	execHandle, err := executor.Start(context.Background(), RunInput{ExecutionID: "agent_resumed", LoopID: "loop_1", WorkingDirectory: t.TempDir(), Prompt: "continue work", Timeout: 5 * time.Second, Env: map[string]string{"ARGS_PATH": argsPath}})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	result, err := execHandle.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if result.Status != "completed" || result.Summary != "resumed" {
		t.Fatalf("result = %#v, want completed resumed execution", result)
	}
	argsBytes, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("ReadFile(argsPath) error = %v", err)
	}
	if got, want := strings.TrimSpace(string(argsBytes)), "exec resume codex-session-1 continue work"; got != want {
		t.Fatalf("resume args = %q, want %q", got, want)
	}
	record, err := repos.AgentExecutions.GetByID(context.Background(), "agent_resumed")
	if err != nil {
		t.Fatalf("AgentExecutions.GetByID() error = %v", err)
	}
	if record == nil || record.NativeSessionID == nil || *record.NativeSessionID != sessionID || record.NativeResumeMode == nil || *record.NativeResumeMode != "native_resume" {
		t.Fatalf("agent execution record = %#v, want native resume session metadata", record)
	}
}

func TestExecutorFallsBackAfterFailedNativeResumeAttempt(t *testing.T) {
	t.Parallel()

	coordinator := openAgentCoordinator(t)
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 20, 12, 0, 0, 0, time.UTC)
	nowISO := now.Format("2006-01-02T15:04:05.000Z")
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "project_1", Name: "Project", RepoPath: t.TempDir(), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: "loop_1", Seq: 1, ProjectID: "project_1", Type: "worker", TargetType: "issue", Status: "running", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	sessionID := "codex-session-1"
	mode := "native_resume"
	status := "pending"
	if err := repos.AgentExecutions.Upsert(context.Background(), storage.AgentExecutionRecord{
		ID:                 "agent_previous",
		ProjectID:          strPtr("project_1"),
		LoopID:             strPtr("loop_1"),
		Vendor:             string(config.AgentVendorCodex),
		Status:             "killed",
		NativeSessionID:    &sessionID,
		NativeResumeMode:   &mode,
		NativeResumeStatus: &status,
		StartedAt:          nowISO,
		CreatedAt:          nowISO,
		UpdatedAt:          nowISO,
	}); err != nil {
		t.Fatalf("AgentExecutions.Upsert() error = %v", err)
	}

	scriptDir := t.TempDir()
	argsPath := filepath.Join(scriptDir, "args.txt")
	scriptPath := filepath.Join(scriptDir, "mock-codex")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$ARGS_PATH\"\ncase \"$*\" in *resume*) printf '%s\\n' 'resume failed' >&2; exit 2;; esac\nprintf '%s\\n' '__LOOPER_RESULT__={\"summary\":\"checkpoint\"}'\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile(scriptPath) error = %v", err)
	}
	executor := New(ExecutorOptions{
		Config: ExecutorConfig{Vendor: config.AgentVendorCodex, Params: map[string]any{"command": scriptPath}, NativeResumeEnabled: true},
		Repos:  repos,
		Now: func() time.Time {
			now = now.Add(10 * time.Millisecond)
			return now
		},
	})

	failedExec, err := executor.Start(context.Background(), RunInput{ExecutionID: "agent_resume_failed", LoopID: "loop_1", WorkingDirectory: t.TempDir(), Prompt: "full checkpoint prompt", NativeResumePrompt: "continue work", Timeout: 5 * time.Second, Env: map[string]string{"ARGS_PATH": argsPath}})
	if err != nil {
		t.Fatalf("Start(first) error = %v", err)
	}
	result, err := failedExec.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait(first) error = %v", err)
	}
	if result.Status != "completed" || result.Summary != "checkpoint" {
		t.Fatalf("result = %#v, want immediate checkpoint fallback completion", result)
	}
	record, err := repos.AgentExecutions.GetByID(context.Background(), "agent_resume_failed")
	if err != nil {
		t.Fatalf("AgentExecutions.GetByID() error = %v", err)
	}
	if record.NativeResumeMode == nil || *record.NativeResumeMode != "checkpoint_restart" || record.NativeResumeStatus == nil || *record.NativeResumeStatus != "fallback_completed" {
		t.Fatalf("native resume metadata = mode:%#v status:%#v, want checkpoint fallback completed", record.NativeResumeMode, record.NativeResumeStatus)
	}
	argsBytes, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("ReadFile(argsPath) error = %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(argsBytes)), "\n")
	if len(lines) != 2 || lines[0] != "exec resume codex-session-1 continue work" || lines[1] != "exec full checkpoint prompt" {
		t.Fatalf("spawned args = %#v, want native resume then immediate checkpoint restart", lines)
	}
}

func TestExecutorNativeResumeFailureAfterAttachDoesNotFallback(t *testing.T) {
	t.Parallel()

	coordinator := openAgentCoordinator(t)
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 20, 12, 0, 0, 0, time.UTC)
	nowISO := now.Format("2006-01-02T15:04:05.000Z")
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "project_1", Name: "Project", RepoPath: t.TempDir(), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: "loop_1", Seq: 1, ProjectID: "project_1", Type: "worker", TargetType: "issue", Status: "running", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	sessionID := "codex-session-1"
	mode := "native_resume"
	status := "pending"
	if err := repos.AgentExecutions.Upsert(context.Background(), storage.AgentExecutionRecord{
		ID:                 "agent_previous",
		ProjectID:          strPtr("project_1"),
		LoopID:             strPtr("loop_1"),
		Vendor:             string(config.AgentVendorCodex),
		Status:             "killed",
		NativeSessionID:    &sessionID,
		NativeResumeMode:   &mode,
		NativeResumeStatus: &status,
		StartedAt:          nowISO,
		CreatedAt:          nowISO,
		UpdatedAt:          nowISO,
	}); err != nil {
		t.Fatalf("AgentExecutions.Upsert() error = %v", err)
	}

	scriptDir := t.TempDir()
	argsPath := filepath.Join(scriptDir, "args.txt")
	scriptPath := filepath.Join(scriptDir, "mock-codex")
	script := "#!/bin/sh\nprintf '%s\n' \"$*\" >> \"$ARGS_PATH\"\ncase \"$*\" in *resume*) printf '%s\n' 'failed to resume work: missing session token' >&2; exit 2;; esac\nprintf '%s\n' '__LOOPER_RESULT__={\"summary\":\"checkpoint\"}'\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile(scriptPath) error = %v", err)
	}
	executor := New(ExecutorOptions{
		Config: ExecutorConfig{Vendor: config.AgentVendorCodex, Params: map[string]any{"command": scriptPath}, NativeResumeEnabled: true},
		Repos:  repos,
		Now: func() time.Time {
			now = now.Add(10 * time.Millisecond)
			return now
		},
	})

	execHandle, err := executor.Start(context.Background(), RunInput{ExecutionID: "agent_resume_attached_failed", LoopID: "loop_1", WorkingDirectory: t.TempDir(), Prompt: "continue work", Timeout: 5 * time.Second, Env: map[string]string{"ARGS_PATH": argsPath}})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	result, err := execHandle.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if result.Status != "failed" || result.Summary != "failed to resume work: missing session token" {
		t.Fatalf("result = %#v, want native resume failure without checkpoint fallback", result)
	}
	record, err := repos.AgentExecutions.GetByID(context.Background(), "agent_resume_attached_failed")
	if err != nil {
		t.Fatalf("AgentExecutions.GetByID() error = %v", err)
	}
	if record.NativeResumeMode == nil || *record.NativeResumeMode != "native_resume" || record.NativeResumeStatus == nil || *record.NativeResumeStatus != "failed" {
		t.Fatalf("native resume metadata = mode:%#v status:%#v, want native resume failed", record.NativeResumeMode, record.NativeResumeStatus)
	}
	argsBytes, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("ReadFile(argsPath) error = %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(argsBytes)), "\n")
	if len(lines) != 1 || lines[0] != "exec resume codex-session-1 continue work" {
		t.Fatalf("spawned args = %#v, want only native resume invocation", lines)
	}
}

func TestExecutorFallbackTimeoutPropagatesTimeoutTypeToLifecycle(t *testing.T) {
	t.Parallel()

	coordinator := openAgentCoordinator(t)
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 20, 12, 0, 0, 0, time.UTC)
	nowISO := now.Format("2006-01-02T15:04:05.000Z")
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "project_1", Name: "Project", RepoPath: t.TempDir(), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: "loop_1", Seq: 1, ProjectID: "project_1", Type: "worker", TargetType: "issue", Status: "running", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	sessionID := "codex-session-1"
	mode := "native_resume"
	status := "pending"
	if err := repos.AgentExecutions.Upsert(context.Background(), storage.AgentExecutionRecord{
		ID:                 "agent_previous",
		ProjectID:          strPtr("project_1"),
		LoopID:             strPtr("loop_1"),
		Vendor:             string(config.AgentVendorCodex),
		Status:             "killed",
		NativeSessionID:    &sessionID,
		NativeResumeMode:   &mode,
		NativeResumeStatus: &status,
		StartedAt:          nowISO,
		CreatedAt:          nowISO,
		UpdatedAt:          nowISO,
	}); err != nil {
		t.Fatalf("AgentExecutions.Upsert() error = %v", err)
	}

	scriptPath := filepath.Join(t.TempDir(), "mock-codex")
	script := "#!/bin/sh\ncase \"$*\" in *resume*) printf '%s\\n' 'resume failed' >&2; exit 2;; esac\nprintf '%s\\n' 'fallback progress'\nsleep 1\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile(scriptPath) error = %v", err)
	}
	executor := New(ExecutorOptions{
		Config: ExecutorConfig{Vendor: config.AgentVendorCodex, Params: map[string]any{"command": scriptPath}, NativeResumeEnabled: true},
		Repos:  repos,
		Now: func() time.Time {
			now = now.Add(10 * time.Millisecond)
			return now
		},
	})

	execHandle, err := executor.Start(context.Background(), RunInput{ExecutionID: "agent_fallback_timeout", LoopID: "loop_1", WorkingDirectory: t.TempDir(), Prompt: "continue work", Timeout: time.Second, HeartbeatTimeout: 50 * time.Millisecond, GracefulShutdown: 10 * time.Millisecond})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	result, err := execHandle.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if result.Status != "timeout" || result.TimeoutType != "idle" {
		t.Fatalf("result = %#v, want fallback idle timeout", result)
	}
	events, err := repos.Events.ListByEntity(context.Background(), "agent_execution", "agent_fallback_timeout")
	if err != nil {
		t.Fatalf("Events.ListByEntity() error = %v", err)
	}
	if !containsEvent(events, "agent.idle_timeout") || containsEvent(events, "agent.timed_out") {
		t.Fatalf("agent events = %#v, want idle timeout event without generic timeout", events)
	}
}

func TestExecutorSuccessfulExecutionPersistsExecutionAndEvents(t *testing.T) {
	t.Parallel()

	coordinator := openAgentCoordinator(t)
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 20, 12, 0, 0, 0, time.UTC)
	executor := New(ExecutorOptions{
		Config: ExecutorConfig{Vendor: config.AgentVendor("custom"), Params: map[string]any{"command": "/bin/sh", "args": []any{"-c", `printf 'ok\n'; printf '__LOOPER_RESULT__={"summary":"done","artifacts":["spec.md"],"changedFiles":["main.go"],"commits":["abc123"],"git_pr_lifecycle":{"branch":"looper/test","base_branch":"main","commit_shas":["abc123"],"pushed":true,"pr_number":84,"pr_url":"https://github.com/nexu-io/looper/pull/84","actions":{"commit":"agent","push":"agent","pr":"agent"}}}\n'`}}},
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
	if result.Lifecycle == nil || result.Lifecycle.Branch != "looper/test" || result.Lifecycle.PRNumber != 84 || result.Lifecycle.Actions.Commit != "agent" {
		t.Fatalf("result lifecycle = %#v, want parsed git_pr_lifecycle", result.Lifecycle)
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

func TestExecutorMalformedLifecycleDoesNotInvalidateCompletion(t *testing.T) {
	t.Parallel()

	executor := New(ExecutorOptions{Config: ExecutorConfig{Vendor: config.AgentVendor("custom"), Params: map[string]any{"command": "/bin/sh", "args": []any{"-c", `printf '__LOOPER_RESULT__={"summary":"done","commits":["abc123"],"git_pr_lifecycle":{"branch":"looper/test","pr_number":"84"}}\n'`}}}})
	execHandle, err := executor.Start(context.Background(), RunInput{ExecutionID: "agent_bad_lifecycle", WorkingDirectory: t.TempDir(), Prompt: "ignored", Timeout: time.Second})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	result, err := execHandle.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if result.ParseStatus != "parsed" || result.Summary != "done" || len(result.Commits) != 1 || result.Commits[0] != "abc123" || result.Lifecycle != nil {
		t.Fatalf("result = %#v, want parsed completion with lifecycle unavailable", result)
	}
}

func TestExecutorFailedCommandIgnoresEchoedTemplateCompletion(t *testing.T) {
	t.Parallel()

	realError := "The 'gpt-5.5' model requires a newer version of Codex. Please upgrade to the latest app or CLI and try again."
	executor := New(ExecutorOptions{Config: ExecutorConfig{Vendor: config.AgentVendor("custom"), Params: map[string]any{"command": "/bin/sh", "args": []any{"-c", `printf '__LOOPER_RESULT__={"summary":"<one-sentence summary>"}\n'; printf "$REAL_ERROR\n" >&2; exit 1`}}}})
	execHandle, err := executor.Start(context.Background(), RunInput{ExecutionID: "agent_failed_template", WorkingDirectory: t.TempDir(), Prompt: "ignored", Timeout: time.Second, Env: map[string]string{"REAL_ERROR": realError}})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	result, err := execHandle.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if result.Status != "failed" {
		t.Fatalf("result.Status = %q, want failed", result.Status)
	}
	if result.ParseStatus != "missing" || result.CompletionSignal != "" {
		t.Fatalf("result = %#v, want failed command to ignore echoed completion marker", result)
	}
	if !strings.Contains(result.Summary, realError) || strings.Contains(result.Summary, "<one-sentence summary>") {
		t.Fatalf("result.Summary = %q, want real stderr without template placeholder", result.Summary)
	}
}

func TestParseCompletionIgnoresTemplatePlaceholder(t *testing.T) {
	t.Parallel()

	parsed := parseCompletion(CompletionMarkerPrefix+`{"summary":"<one-sentence summary>"}`+"\nreal work\n", "")
	if parsed.ParseStatus != "missing" || parsed.Summary != "" {
		t.Fatalf("parseCompletion() = %#v, want template placeholder ignored", parsed)
	}
}

func TestReadPersistedExecutionLogReadsTailToPreserveCompletionMarker(t *testing.T) {
	t.Parallel()

	logPath := filepath.Join(t.TempDir(), "stdout.log")
	completionLine := CompletionMarkerPrefix + `{"summary":"done from tail"}` + "\n"
	headSize := maxPersistedLogReadBytes - len(completionLine) + 1023
	fullLog := strings.Repeat("x", headSize) + "\n" + completionLine
	if err := os.WriteFile(logPath, []byte(fullLog), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	persisted, ok := readPersistedExecutionLog(logPath)
	if !ok {
		t.Fatal("readPersistedExecutionLog() = ok false, want true")
	}
	if len(persisted) != maxPersistedLogReadBytes {
		t.Fatalf("len(persisted) = %d, want %d", len(persisted), maxPersistedLogReadBytes)
	}
	if !strings.HasSuffix(persisted, completionLine) {
		t.Fatalf("persisted log missing completion tail suffix")
	}

	parsed := parseCompletion(persisted, "")
	if parsed.ParseStatus != "parsed" || parsed.Summary != "done from tail" || parsed.CompletionSignal != CompletionMarkerPrefix {
		t.Fatalf("parseCompletion() = %#v, want parsed tail completion marker", parsed)
	}
}

func TestExecutionResolveOutputLogsSkipsPersistedReplacementAfterWriteFailure(t *testing.T) {
	t.Parallel()

	logPath := filepath.Join(t.TempDir(), "stdout.log")
	fullPersisted := strings.Repeat("persisted-", 4)
	if err := os.WriteFile(logPath, []byte(fullPersisted), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	x := &execution{
		stdout:                  []byte("tail-only"),
		stdoutLogPath:           logPath,
		persistedLogWriteFailed: true,
	}

	stdout, stderr := x.resolveOutputLogs()
	if stdout != "tail-only" {
		t.Fatalf("stdout = %q, want in-memory tail when persisted write failed", stdout)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q, want empty stderr", stderr)
	}
}

func TestAppendPersistedLogMarksWriteFailure(t *testing.T) {
	t.Parallel()

	dirPath := filepath.Join(t.TempDir(), "logs")
	if err := os.MkdirAll(dirPath, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	x := &execution{}
	if !x.appendPersistedLog(dirPath, []byte("chunk")) {
		t.Fatal("appendPersistedLog() = false, want true for unwritable path")
	}
	x.markPersistedLogWriteFailed()

	if !x.hasPersistedLogWriteFailure() {
		t.Fatal("hasPersistedLogWriteFailure() = false, want true")
	}
}

func TestIsAgentSetupFailureMessageDetectsCodexModelCompatibility(t *testing.T) {
	t.Parallel()

	message := "The 'gpt-5.5' model requires a newer version of Codex. Please upgrade to the latest app or CLI and try again."
	if !IsAgentSetupFailureMessage(message) {
		t.Fatalf("IsAgentSetupFailureMessage(%q) = false, want true", message)
	}
}

func TestIsAgentSetupFailureMessageIgnoresIncidentalModelText(t *testing.T) {
	t.Parallel()

	messages := []string{
		"custom tool failed: invalid model response from downstream service",
		"retryable operation returned unknown model while parsing payload",
		"unsupported model value in project data; try again later",
	}
	for _, message := range messages {
		if IsAgentSetupFailureMessage(message) {
			t.Fatalf("IsAgentSetupFailureMessage(%q) = true, want false", message)
		}
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

func TestExecutorPersistsHistoricalLogsBeyondMaxOutputBytes(t *testing.T) {
	t.Parallel()

	coordinator := openAgentCoordinator(t)
	repos := storage.NewRepositories(coordinator.DB())
	logDir := t.TempDir()
	fullStdout := strings.Repeat("out-", 16)
	fullStderr := strings.Repeat("err-", 16)
	executor := New(ExecutorOptions{
		Config: ExecutorConfig{Vendor: config.AgentVendor("custom"), Params: map[string]any{"command": "/bin/sh", "args": []any{"-c", `printf "$FULL_STDOUT"; printf "$FULL_STDERR" >&2`}}},
		Repos:  repos,
		LogDir: logDir,
	})

	execHandle, err := executor.Start(context.Background(), RunInput{ExecutionID: "agent_persisted_logs", WorkingDirectory: t.TempDir(), Prompt: "ignored", Timeout: time.Second, MaxOutputBytes: 8, Env: map[string]string{"FULL_STDOUT": fullStdout, "FULL_STDERR": fullStderr}})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	result, err := execHandle.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if result.Stdout != fullStdout {
		t.Fatalf("result.Stdout = %q, want full persisted stdout %q", result.Stdout, fullStdout)
	}
	if result.Stderr != fullStderr {
		t.Fatalf("result.Stderr = %q, want full persisted stderr %q", result.Stderr, fullStderr)
	}

	record, err := repos.AgentExecutions.GetByID(context.Background(), "agent_persisted_logs")
	if err != nil {
		t.Fatalf("AgentExecutions.GetByID() error = %v", err)
	}
	if record == nil || record.OutputJSON == nil {
		t.Fatalf("agent execution record = %#v, want output_json", record)
	}

	var output struct {
		Stdout        string `json:"stdout"`
		Stderr        string `json:"stderr"`
		StdoutLogPath string `json:"stdoutLogPath"`
		StderrLogPath string `json:"stderrLogPath"`
	}
	if err := json.Unmarshal([]byte(*record.OutputJSON), &output); err != nil {
		t.Fatalf("json.Unmarshal(output_json) error = %v", err)
	}
	if output.Stdout != fullStdout[len(fullStdout)-8:] {
		t.Fatalf("output stdout = %q, want truncated tail %q", output.Stdout, fullStdout[len(fullStdout)-8:])
	}
	if output.Stderr != fullStderr[len(fullStderr)-8:] {
		t.Fatalf("output stderr = %q, want truncated tail %q", output.Stderr, fullStderr[len(fullStderr)-8:])
	}
	if output.StdoutLogPath == "" || output.StderrLogPath == "" {
		t.Fatalf("output log paths = %#v, want stdout/stderr log paths", output)
	}
	stdoutLog, err := os.ReadFile(output.StdoutLogPath)
	if err != nil {
		t.Fatalf("ReadFile(stdout log) error = %v", err)
	}
	stderrLog, err := os.ReadFile(output.StderrLogPath)
	if err != nil {
		t.Fatalf("ReadFile(stderr log) error = %v", err)
	}
	if string(stdoutLog) != fullStdout {
		t.Fatalf("stdout log = %q, want %q", string(stdoutLog), fullStdout)
	}
	if string(stderrLog) != fullStderr {
		t.Fatalf("stderr log = %q, want %q", string(stderrLog), fullStderr)
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

func TestExecutorKillTerminatesChildProcessGroup(t *testing.T) {
	workDir := t.TempDir()
	childPIDPath := filepath.Join(workDir, "child.pid")
	executor := New(ExecutorOptions{Config: ExecutorConfig{Vendor: config.AgentVendor("custom"), Params: map[string]any{"command": "/bin/sh", "args": []any{"-c", `(trap '' TERM; while true; do sleep 1; done) & echo $! > "$CHILD_PID_FILE"; wait`}}}})

	execHandle, err := executor.Start(context.Background(), RunInput{ExecutionID: "agent_kill_group", WorkingDirectory: workDir, Prompt: "ignored", Timeout: 5 * time.Second, GracefulShutdown: 20 * time.Millisecond, Env: map[string]string{"CHILD_PID_FILE": childPIDPath}})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	childPID := waitForPIDFile(t, childPIDPath)
	if err := execHandle.Kill("stopped by test"); err != nil {
		t.Fatalf("Kill() error = %v", err)
	}
	result, err := execHandle.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if result.Status != "killed" {
		t.Fatalf("result.Status = %q, want killed", result.Status)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(childPID, 0); err == syscall.ESRCH {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("child process %d is still running after execution Kill", childPID)
}

func TestExecutorHeartbeatTimeoutMarksTimeout(t *testing.T) {
	t.Parallel()

	coordinator := openAgentCoordinator(t)
	repos := storage.NewRepositories(coordinator.DB())
	executor := New(ExecutorOptions{Config: ExecutorConfig{Vendor: config.AgentVendor("custom"), Params: map[string]any{"command": "/bin/sh", "args": []any{"-c", "printf 'beat\n'; sleep 1"}}}, Repos: repos})

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
	if result.TimeoutType != "idle" || result.ConfiguredIdleTimeoutSeconds != 1 || result.ConfiguredMaxRuntimeSeconds != 1 || result.LastProgressAt == "" {
		t.Fatalf("timeout diagnostics = %#v, want idle metadata", result)
	}
	events, err := repos.Events.ListByEntity(context.Background(), "agent_execution", "agent_heartbeat_timeout")
	if err != nil {
		t.Fatalf("Events.ListByEntity() error = %v", err)
	}
	if !containsEvent(events, "agent.idle_timeout") {
		t.Fatalf("agent events = %#v, want idle timeout event", events)
	}
}

func TestExecutorHeartbeatTimeoutPreservesOriginalTimeoutTypeDuringGracefulShutdown(t *testing.T) {
	t.Parallel()

	executor := New(ExecutorOptions{Config: ExecutorConfig{Vendor: config.AgentVendor("custom"), Params: map[string]any{"command": "/bin/sh", "args": []any{"-c", `printf 'beat\n'; trap '' TERM; while true; do sleep 0.05; done`}}}})

	execHandle, err := executor.Start(context.Background(), RunInput{ExecutionID: "agent_heartbeat_timeout_grace", WorkingDirectory: t.TempDir(), Prompt: "ignored", Timeout: 120 * time.Millisecond, HeartbeatTimeout: 50 * time.Millisecond, GracefulShutdown: 200 * time.Millisecond})
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
	if result.TimeoutType != "idle" {
		t.Fatalf("result.TimeoutType = %q, want idle preserved after max runtime fires", result.TimeoutType)
	}
}

func TestExecutorMaxRuntimeTimeoutIgnoresProgressResets(t *testing.T) {
	t.Parallel()

	coordinator := openAgentCoordinator(t)
	repos := storage.NewRepositories(coordinator.DB())
	executor := New(ExecutorOptions{Config: ExecutorConfig{Vendor: config.AgentVendor("custom"), Params: map[string]any{"command": "/bin/sh", "args": []any{"-c", "while true; do printf 'beat\n'; sleep 0.02; done"}}}, Repos: repos})

	execHandle, err := executor.Start(context.Background(), RunInput{ExecutionID: "agent_max_runtime_timeout", WorkingDirectory: t.TempDir(), Prompt: "ignored", Timeout: 80 * time.Millisecond, HeartbeatTimeout: time.Second, GracefulShutdown: 10 * time.Millisecond})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	result, err := execHandle.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if result.Status != "timeout" || result.TimeoutType != "max_runtime" {
		t.Fatalf("result = %#v, want max runtime timeout", result)
	}
	if result.HeartbeatCount < 2 {
		t.Fatalf("HeartbeatCount = %d, want progress before max runtime timeout", result.HeartbeatCount)
	}
	events, err := repos.Events.ListByEntity(context.Background(), "agent_execution", "agent_max_runtime_timeout")
	if err != nil {
		t.Fatalf("Events.ListByEntity() error = %v", err)
	}
	if !containsEvent(events, "agent.max_runtime_timeout") {
		t.Fatalf("agent events = %#v, want max runtime timeout event", events)
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

func strPtr(value string) *string {
	return &value
}

func waitForPIDFile(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
			if err == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for pid file %s", path)
	return 0
}
