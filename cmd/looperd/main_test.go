package main

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"regexp"
	"syscall"
	"testing"
	"time"

	"github.com/powerformer/looper/internal/bootstrap"
	"github.com/powerformer/looper/internal/config"
	"github.com/powerformer/looper/internal/loops"
	looperdruntime "github.com/powerformer/looper/internal/runtime"
	"github.com/powerformer/looper/internal/storage"
	"github.com/powerformer/looper/internal/version"
)

func TestRunPrintsVersionWithoutBootstrappingCommandHandling(t *testing.T) {
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	bootstrapCalled := false

	exitCode := runWithDeps([]string{"--version"}, stdout, stderr, runDeps{
		bootstrapImpl: func(context.Context, bootstrap.Options) (bootstrap.Result, error) {
			bootstrapCalled = true
			return bootstrap.Result{}, errors.New("bootstrap should not be called")
		},
	})

	if exitCode != 0 {
		t.Fatalf("run([--version]) exit code = %d, want 0", exitCode)
	}

	if got, want := stdout.String(), version.Value+"\n"; got != want {
		t.Fatalf("run([--version]) stdout = %q, want %q", got, want)
	}

	if !regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?\n$`).MatchString(stdout.String()) {
		t.Fatalf("run([--version]) stdout = %q, want only a semantic version followed by newline", stdout.String())
	}

	if bootstrapCalled {
		t.Fatal("bootstrapImpl was called for --version")
	}

	if got := stderr.String(); got != "" {
		t.Fatalf("run([--version]) stderr = %q, want empty string", got)
	}
}

func TestRunPrefersVersionFlagOverOtherArguments(t *testing.T) {
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	bootstrapCalled := false

	exitCode := runWithDeps([]string{"serve", "--version"}, stdout, stderr, runDeps{
		bootstrapImpl: func(context.Context, bootstrap.Options) (bootstrap.Result, error) {
			bootstrapCalled = true
			return bootstrap.Result{}, errors.New("bootstrap should not be called")
		},
	})

	if exitCode != 0 {
		t.Fatalf("run([serve --version]) exit code = %d, want 0", exitCode)
	}

	if got, want := stdout.String(), version.Value+"\n"; got != want {
		t.Fatalf("run([serve --version]) stdout = %q, want %q", got, want)
	}

	if got := stderr.String(); got != "" {
		t.Fatalf("run([serve --version]) stderr = %q, want empty string", got)
	}

	if bootstrapCalled {
		t.Fatal("bootstrapImpl was called for serve --version")
	}
}

func TestRunBootstrapsLooperdByDefault(t *testing.T) {
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	called := false

	exitCode := runWithDeps([]string{}, stdout, stderr, runDeps{
		bootstrapImpl: func(_ context.Context, options bootstrap.Options) (bootstrap.Result, error) {
			called = true
			if len(options.Args) != 0 {
				t.Fatalf("bootstrap args = %#v, want empty slice", options.Args)
			}
			if !options.WaitForShutdown {
				t.Fatal("bootstrap WaitForShutdown = false, want true")
			}
			return bootstrap.Result{}, nil
		},
	})

	if exitCode != 0 {
		t.Fatalf("runWithDeps([]) exit code = %d, want 0", exitCode)
	}
	if !called {
		t.Fatalf("bootstrapImpl was not called")
	}
	if got := stdout.String(); got != "" {
		t.Fatalf("runWithDeps([]) stdout = %q, want empty string", got)
	}
	if got := stderr.String(); got != "" {
		t.Fatalf("runWithDeps([]) stderr = %q, want empty string", got)
	}
}

func TestRunPrintsHelpWhenHelpFlagAppearsAfterOtherArgs(t *testing.T) {
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	bootstrapCalled := false

	exitCode := runWithDeps([]string{"--config", "/tmp/config.json", "--help"}, stdout, stderr, runDeps{
		bootstrapImpl: func(context.Context, bootstrap.Options) (bootstrap.Result, error) {
			bootstrapCalled = true
			return bootstrap.Result{}, errors.New("bootstrap should not be called")
		},
	})

	if exitCode != 0 {
		t.Fatalf("run([--config /tmp/config.json --help]) exit code = %d, want 0", exitCode)
	}
	if got := stderr.String(); got != "" {
		t.Fatalf("run([--config /tmp/config.json --help]) stderr = %q, want empty string", got)
	}
	if !bytes.Contains(stdout.Bytes(), []byte("Usage:")) {
		t.Fatalf("run([--config /tmp/config.json --help]) stdout = %q, want usage text", stdout.String())
	}
	if bootstrapCalled {
		t.Fatal("bootstrapImpl was called for --help")
	}
}

func TestRunFormatsConfigValidationErrors(t *testing.T) {
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}

	exitCode := runWithDeps([]string{}, stdout, stderr, runDeps{
		bootstrapImpl: func(context.Context, bootstrap.Options) (bootstrap.Result, error) {
			return bootstrap.Result{}, &config.ConfigValidationError{Issues: []config.ValidationIssue{
				{Path: "server.port", Message: "must be an integer between 1 and 65535"},
				{Path: "daemon.logDir", Message: "must be a non-empty path"},
			}}
		},
	})

	if exitCode != 1 {
		t.Fatalf("runWithDeps([]) exit code = %d, want 1", exitCode)
	}
	const wantStderr = "looperd failed to start due to invalid configuration:\n- server.port: must be an integer between 1 and 65535\n- daemon.logDir: must be a non-empty path\n"
	if got := stderr.String(); got != wantStderr {
		t.Fatalf("runWithDeps([]) stderr = %q, want %q", got, wantStderr)
	}
	if got := stdout.String(); got != "" {
		t.Fatalf("runWithDeps([]) stdout = %q, want empty string", got)
	}
}

func TestRunPrintsBootstrapErrors(t *testing.T) {
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	wantErr := errors.New("runtime assembly has not been ported yet")

	exitCode := runWithDeps([]string{}, stdout, stderr, runDeps{
		bootstrapImpl: func(context.Context, bootstrap.Options) (bootstrap.Result, error) {
			return bootstrap.Result{}, wantErr
		},
	})

	if exitCode != 1 {
		t.Fatalf("runWithDeps([]) exit code = %d, want 1", exitCode)
	}
	if got, want := stderr.String(), "looperd: runtime assembly has not been ported yet\n"; got != want {
		t.Fatalf("runWithDeps([]) stderr = %q, want %q", got, want)
	}
	if got := stdout.String(); got != "" {
		t.Fatalf("runWithDeps([]) stdout = %q, want empty string", got)
	}
}

func TestStopLoopPausesLoopAndSignalsActiveExecution(t *testing.T) {
	ctx := context.Background()
	coordinator, err := storage.OpenSQLiteCoordinator(ctx, filepath.Join(t.TempDir(), "looper.sqlite"), storage.SQLiteCoordinatorOptions{Migrations: storage.EmbeddedMigrations})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	if _, err := coordinator.MigrationRunner().RunPending(ctx); err != nil {
		t.Fatalf("MigrationRunner().RunPending() error = %v", err)
	}
	t.Cleanup(func() {
		_ = coordinator.Close()
	})

	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 21, 12, 0, 0, 0, time.UTC)
	nowISO := "2026-04-21T12:00:00.000Z"
	project := storage.ProjectRecord{ID: "project_1", Name: "Looper", RepoPath: t.TempDir(), CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := repos.Projects.Upsert(ctx, project); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	loop := storage.LoopRecord{ID: "loop_1", Seq: 30, ProjectID: project.ID, Type: "worker", TargetType: "project", Status: "running", CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := repos.Loops.Upsert(ctx, loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	run := storage.RunRecord{ID: "run_1", LoopID: loop.ID, Status: "running", StartedAt: nowISO, LastHeartbeatAt: &nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := repos.Runs.Upsert(ctx, run); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}
	pid := int64(4321)
	agentExecution := storage.AgentExecutionRecord{ID: "agentexec_1", ProjectID: &project.ID, LoopID: &loop.ID, RunID: &run.ID, Vendor: "codex", Status: "running", PID: &pid, StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := repos.AgentExecutions.Upsert(ctx, agentExecution); err != nil {
		t.Fatalf("AgentExecutions.Upsert() error = %v", err)
	}

	services := looperdruntime.Services{
		Coordinator:  coordinator,
		Repositories: repos,
		Loops:        &loops.Service{DB: coordinator.DB(), Repos: repos, Now: func() time.Time { return now }},
	}

	called := false
	gotSignalPID := 0
	verified := false
	gotResult, err := stopLoop(ctx, services, loop.ID, "Stopped by test", func() time.Time { return now }, func(pid int, sig syscall.Signal) error {
		called = true
		gotSignalPID = pid
		if sig != syscall.SIGTERM {
			t.Fatalf("signal = %v, want %v", sig, syscall.SIGTERM)
		}
		return nil
	}, func(_ context.Context, execution storage.AgentExecutionRecord, gotPID int) (bool, bool, error) {
		verified = true
		if execution.ID != agentExecution.ID || gotPID != int(pid) {
			t.Fatalf("execution verifier received execution=%q pid=%d, want %q pid=%d", execution.ID, gotPID, agentExecution.ID, pid)
		}
		return true, true, nil
	})
	if err != nil {
		t.Fatalf("stopLoop() error = %v", err)
	}

	result, ok := gotResult.(stopLoopResult)
	if !ok {
		t.Fatalf("stopLoop() result type = %T, want stopLoopResult", gotResult)
	}
	if !result.Stopped || result.LoopID != loop.ID || result.RunID != run.ID || result.ExecutionID != agentExecution.ID || result.Vendor != "codex" || result.PID != pid {
		t.Fatalf("stopLoop() result = %#v", result)
	}
	if !called || gotSignalPID != int(pid) {
		t.Fatalf("signal invoked = %v pid=%d, want true pid=%d", called, gotSignalPID, pid)
	}
	if !verified {
		t.Fatal("execution verifier was not called")
	}

	storedLoop, err := repos.Loops.GetByID(ctx, loop.ID)
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if storedLoop == nil || storedLoop.Status != "paused" {
		t.Fatalf("Loops.GetByID() = %#v, want paused loop", storedLoop)
	}
	storedExecution, err := repos.AgentExecutions.GetByID(ctx, agentExecution.ID)
	if err != nil {
		t.Fatalf("AgentExecutions.GetByID() error = %v", err)
	}
	if storedExecution == nil || storedExecution.Status != "cancelling" {
		t.Fatalf("AgentExecutions.GetByID() = %#v, want cancelling execution", storedExecution)
	}
}

func TestStopLoopSkipsSignalWhenExecutionVerifierRejectsPID(t *testing.T) {
	ctx := context.Background()
	coordinator, err := storage.OpenSQLiteCoordinator(ctx, filepath.Join(t.TempDir(), "looper.sqlite"), storage.SQLiteCoordinatorOptions{Migrations: storage.EmbeddedMigrations})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	if _, err := coordinator.MigrationRunner().RunPending(ctx); err != nil {
		t.Fatalf("MigrationRunner().RunPending() error = %v", err)
	}
	t.Cleanup(func() {
		_ = coordinator.Close()
	})

	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 21, 12, 0, 0, 0, time.UTC)
	nowISO := "2026-04-21T12:00:00.000Z"
	project := storage.ProjectRecord{ID: "project_1", Name: "Looper", RepoPath: t.TempDir(), CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := repos.Projects.Upsert(ctx, project); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	loop := storage.LoopRecord{ID: "loop_1", Seq: 30, ProjectID: project.ID, Type: "worker", TargetType: "project", Status: "running", CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := repos.Loops.Upsert(ctx, loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	run := storage.RunRecord{ID: "run_1", LoopID: loop.ID, Status: "running", StartedAt: nowISO, LastHeartbeatAt: &nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := repos.Runs.Upsert(ctx, run); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}
	pid := int64(4321)
	agentExecution := storage.AgentExecutionRecord{ID: "agentexec_1", ProjectID: &project.ID, LoopID: &loop.ID, RunID: &run.ID, Vendor: "codex", Status: "running", PID: &pid, StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := repos.AgentExecutions.Upsert(ctx, agentExecution); err != nil {
		t.Fatalf("AgentExecutions.Upsert() error = %v", err)
	}

	services := looperdruntime.Services{
		Coordinator:  coordinator,
		Repositories: repos,
		Loops:        &loops.Service{DB: coordinator.DB(), Repos: repos, Now: func() time.Time { return now }},
	}

	signaled := false
	gotResult, err := stopLoop(ctx, services, loop.ID, "Stopped by test", func() time.Time { return now }, func(int, syscall.Signal) error {
		signaled = true
		return nil
	}, func(context.Context, storage.AgentExecutionRecord, int) (bool, bool, error) {
		return false, true, nil
	})
	if err != nil {
		t.Fatalf("stopLoop() error = %v", err)
	}

	result, ok := gotResult.(stopLoopResult)
	if !ok {
		t.Fatalf("stopLoop() result type = %T, want stopLoopResult", gotResult)
	}
	if !result.Stopped || result.LoopID != loop.ID || result.RunID != run.ID || result.ExecutionID != agentExecution.ID || result.Vendor != "codex" || result.PID != 0 {
		t.Fatalf("stopLoop() result = %#v", result)
	}
	if signaled {
		t.Fatal("signal invoked, want verifier-rejected PID to be skipped")
	}

	storedExecution, err := repos.AgentExecutions.GetByID(ctx, agentExecution.ID)
	if err != nil {
		t.Fatalf("AgentExecutions.GetByID() error = %v", err)
	}
	if storedExecution == nil || storedExecution.Status != "running" {
		t.Fatalf("AgentExecutions.GetByID() = %#v, want running execution", storedExecution)
	}
}
