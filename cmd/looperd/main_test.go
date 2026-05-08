package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/bootstrap"
	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/loops"
	looperdruntime "github.com/nexu-io/looper/internal/runtime"
	"github.com/nexu-io/looper/internal/storage"
	"github.com/nexu-io/looper/internal/version"
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
		if sig == syscall.SIGKILL {
			return nil
		}
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
	if !called || gotSignalPID != -int(pid) {
		t.Fatalf("signal invoked = %v pid=%d, want true pid=%d", called, gotSignalPID, -pid)
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

func TestSignalAgentProcessGroupDoesNotEscalateAfterESRCH(t *testing.T) {
	pid := 4321
	grace := 10 * time.Millisecond
	killCalled := make(chan struct{}, 1)

	err := signalAgentProcessGroup(pid, func(gotPID int, sig syscall.Signal) error {
		if sig == syscall.SIGKILL {
			killCalled <- struct{}{}
			return nil
		}
		if sig != syscall.SIGTERM {
			t.Fatalf("signal = %v, want SIGTERM", sig)
		}
		return syscall.ESRCH
	}, grace)
	if err != nil {
		t.Fatalf("signalAgentProcessGroup() error = %v", err)
	}

	select {
	case <-killCalled:
		t.Fatal("SIGKILL escalation was armed after SIGTERM returned ESRCH")
	case <-time.After(3 * grace):
	}
}

func TestStopLoopKillsActiveInMemoryExecution(t *testing.T) {
	ctx := context.Background()
	coordinator, err := storage.OpenSQLiteCoordinator(ctx, filepath.Join(t.TempDir(), "looper.sqlite"), storage.SQLiteCoordinatorOptions{Migrations: storage.EmbeddedMigrations})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	if _, err := coordinator.MigrationRunner().RunPending(ctx); err != nil {
		t.Fatalf("MigrationRunner().RunPending() error = %v", err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })

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
	agentExecution := storage.AgentExecutionRecord{ID: "agentexec_1", ProjectID: &project.ID, LoopID: &loop.ID, RunID: &run.ID, Vendor: "codex", Status: "running", StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := repos.AgentExecutions.Upsert(ctx, agentExecution); err != nil {
		t.Fatalf("AgentExecutions.Upsert() error = %v", err)
	}

	registry := looperdruntime.NewActiveExecutionRegistry()
	active := &fakeActiveExecution{}
	unregister := registry.Register(loop.ID, run.ID, agentExecution.ID, active)
	defer unregister()
	services := looperdruntime.Services{
		Coordinator:      coordinator,
		Repositories:     repos,
		Loops:            &loops.Service{DB: coordinator.DB(), Repos: repos, Now: func() time.Time { return now }},
		ActiveExecutions: registry,
	}

	signaled := false
	gotResult, err := stopLoop(ctx, services, loop.ID, "Stopped by test", func() time.Time { return now }, func(int, syscall.Signal) error {
		signaled = true
		return nil
	}, nil)
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
		t.Fatal("signal invoked, want active execution kill path")
	}
	if active.reason != "Stopped by test" {
		t.Fatalf("Kill reason = %q, want stop reason", active.reason)
	}
	storedExecution, err := repos.AgentExecutions.GetByID(ctx, agentExecution.ID)
	if err != nil {
		t.Fatalf("AgentExecutions.GetByID() error = %v", err)
	}
	if storedExecution == nil || storedExecution.Status != "cancelling" {
		t.Fatalf("AgentExecutions.GetByID() = %#v, want cancelling execution", storedExecution)
	}
}

func TestStopLoopRetriesActiveInMemoryExecutionAlreadyCancelling(t *testing.T) {
	ctx := context.Background()
	coordinator, err := storage.OpenSQLiteCoordinator(ctx, filepath.Join(t.TempDir(), "looper.sqlite"), storage.SQLiteCoordinatorOptions{Migrations: storage.EmbeddedMigrations})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	if _, err := coordinator.MigrationRunner().RunPending(ctx); err != nil {
		t.Fatalf("MigrationRunner().RunPending() error = %v", err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })

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
	agentExecution := storage.AgentExecutionRecord{ID: "agentexec_1", ProjectID: &project.ID, LoopID: &loop.ID, RunID: &run.ID, Vendor: "codex", Status: "cancelling", StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := repos.AgentExecutions.Upsert(ctx, agentExecution); err != nil {
		t.Fatalf("AgentExecutions.Upsert() error = %v", err)
	}

	registry := looperdruntime.NewActiveExecutionRegistry()
	active := &fakeActiveExecution{}
	unregister := registry.Register(loop.ID, run.ID, agentExecution.ID, active)
	defer unregister()
	services := looperdruntime.Services{
		Coordinator:      coordinator,
		Repositories:     repos,
		Loops:            &loops.Service{DB: coordinator.DB(), Repos: repos, Now: func() time.Time { return now }},
		ActiveExecutions: registry,
	}

	signaled := false
	gotResult, err := stopLoop(ctx, services, loop.ID, "Stopped by test", func() time.Time { return now }, func(int, syscall.Signal) error {
		signaled = true
		return nil
	}, nil)
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
	if !active.killed {
		t.Fatal("active execution Kill was not invoked")
	}
	if active.reason != "Stopped by test" {
		t.Fatalf("Kill reason = %q, want stop reason", active.reason)
	}
	if signaled {
		t.Fatal("signal invoked, want active execution kill path")
	}
}

func TestStopLoopSignalsExecutionAlreadyCancelling(t *testing.T) {
	ctx := context.Background()
	coordinator, err := storage.OpenSQLiteCoordinator(ctx, filepath.Join(t.TempDir(), "looper.sqlite"), storage.SQLiteCoordinatorOptions{Migrations: storage.EmbeddedMigrations})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	if _, err := coordinator.MigrationRunner().RunPending(ctx); err != nil {
		t.Fatalf("MigrationRunner().RunPending() error = %v", err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })

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
	agentExecution := storage.AgentExecutionRecord{ID: "agentexec_1", ProjectID: &project.ID, LoopID: &loop.ID, RunID: &run.ID, Vendor: "codex", Status: "cancelling", PID: &pid, StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := repos.AgentExecutions.Upsert(ctx, agentExecution); err != nil {
		t.Fatalf("AgentExecutions.Upsert() error = %v", err)
	}

	services := looperdruntime.Services{
		Coordinator:  coordinator,
		Repositories: repos,
		Loops:        &loops.Service{DB: coordinator.DB(), Repos: repos, Now: func() time.Time { return now }},
	}

	var signalCalls []int
	gotResult, err := stopLoop(ctx, services, loop.ID, "Stopped by test", func() time.Time { return now }, func(gotPID int, _ syscall.Signal) error {
		signalCalls = append(signalCalls, gotPID)
		return syscall.ESRCH
	}, func(context.Context, storage.AgentExecutionRecord, int) (bool, bool, error) {
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
	if len(signalCalls) != 2 || signalCalls[0] != -int(pid) || signalCalls[1] != int(pid) {
		t.Fatalf("signal calls = %#v, want process group then process", signalCalls)
	}
}

func TestStopLoopSkipsStaleActiveExecutionWhenLatestExecutionCompleted(t *testing.T) {
	ctx := context.Background()
	coordinator, err := storage.OpenSQLiteCoordinator(ctx, filepath.Join(t.TempDir(), "looper.sqlite"), storage.SQLiteCoordinatorOptions{Migrations: storage.EmbeddedMigrations})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	if _, err := coordinator.MigrationRunner().RunPending(ctx); err != nil {
		t.Fatalf("MigrationRunner().RunPending() error = %v", err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })

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
	agentExecution := storage.AgentExecutionRecord{ID: "agentexec_1", ProjectID: &project.ID, LoopID: &loop.ID, RunID: &run.ID, Vendor: "codex", Status: "completed", PID: &pid, StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := repos.AgentExecutions.Upsert(ctx, agentExecution); err != nil {
		t.Fatalf("AgentExecutions.Upsert() error = %v", err)
	}

	registry := looperdruntime.NewActiveExecutionRegistry()
	active := &fakeActiveExecution{}
	unregister := registry.Register(loop.ID, run.ID, agentExecution.ID, active)
	defer unregister()
	services := looperdruntime.Services{
		Coordinator:      coordinator,
		Repositories:     repos,
		Loops:            &loops.Service{DB: coordinator.DB(), Repos: repos, Now: func() time.Time { return now }},
		ActiveExecutions: registry,
	}

	signaled := false
	gotResult, err := stopLoop(ctx, services, loop.ID, "Stopped by test", func() time.Time { return now }, func(int, syscall.Signal) error {
		signaled = true
		return nil
	}, nil)
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
	if active.killed {
		t.Fatal("active execution Kill invoked, want stale registry entry skipped")
	}
	if signaled {
		t.Fatal("signal invoked, want completed execution to be skipped")
	}

	storedExecution, err := repos.AgentExecutions.GetByID(ctx, agentExecution.ID)
	if err != nil {
		t.Fatalf("AgentExecutions.GetByID() error = %v", err)
	}
	if storedExecution == nil || storedExecution.Status != "completed" {
		t.Fatalf("AgentExecutions.GetByID() = %#v, want completed execution", storedExecution)
	}
}

func TestStopLoopDoesNotOverwriteCompletedExecutionAfterActiveKill(t *testing.T) {
	ctx := context.Background()
	coordinator, err := storage.OpenSQLiteCoordinator(ctx, filepath.Join(t.TempDir(), "looper.sqlite"), storage.SQLiteCoordinatorOptions{Migrations: storage.EmbeddedMigrations})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	if _, err := coordinator.MigrationRunner().RunPending(ctx); err != nil {
		t.Fatalf("MigrationRunner().RunPending() error = %v", err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })

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
	agentExecution := storage.AgentExecutionRecord{ID: "agentexec_1", ProjectID: &project.ID, LoopID: &loop.ID, RunID: &run.ID, Vendor: "codex", Status: "running", StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := repos.AgentExecutions.Upsert(ctx, agentExecution); err != nil {
		t.Fatalf("AgentExecutions.Upsert() error = %v", err)
	}

	registry := looperdruntime.NewActiveExecutionRegistry()
	active := &fakeActiveExecution{onKill: func() error {
		completed := agentExecution
		completed.Status = "completed"
		return repos.AgentExecutions.Upsert(ctx, completed)
	}}
	unregister := registry.Register(loop.ID, run.ID, agentExecution.ID, active)
	defer unregister()
	services := looperdruntime.Services{
		Coordinator:      coordinator,
		Repositories:     repos,
		Loops:            &loops.Service{DB: coordinator.DB(), Repos: repos, Now: func() time.Time { return now }},
		ActiveExecutions: registry,
	}

	gotResult, err := stopLoop(ctx, services, loop.ID, "Stopped by test", func() time.Time { return now }, nil, nil)
	if err != nil {
		t.Fatalf("stopLoop() error = %v", err)
	}
	result, ok := gotResult.(stopLoopResult)
	if !ok {
		t.Fatalf("stopLoop() result type = %T, want stopLoopResult", gotResult)
	}
	if !result.Stopped || result.LoopID != loop.ID || result.RunID != run.ID || result.ExecutionID != agentExecution.ID || result.Vendor != "codex" {
		t.Fatalf("stopLoop() result = %#v", result)
	}
	if !active.killed {
		t.Fatal("active execution Kill was not invoked")
	}

	storedExecution, err := repos.AgentExecutions.GetByID(ctx, agentExecution.ID)
	if err != nil {
		t.Fatalf("AgentExecutions.GetByID() error = %v", err)
	}
	if storedExecution == nil || storedExecution.Status != "completed" {
		t.Fatalf("AgentExecutions.GetByID() = %#v, want completed execution", storedExecution)
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

func TestStopAllLoopsHandlesMixedTypesPartialFailureAndRepeatedCalls(t *testing.T) {
	ctx := context.Background()
	services, repos, now := newStopAllTestServices(t)

	insertStopAllTestLoop(t, ctx, repos, now, stopAllLoopFixture{loopID: "loop_planner", seq: 1, loopType: "planner", loopStatus: "running", runID: "run_planner", runStatus: "running", executionID: "exec_planner", executionStatus: "running", pid: 4101})
	insertStopAllTestLoop(t, ctx, repos, now, stopAllLoopFixture{loopID: "loop_reviewer", seq: 2, loopType: "reviewer", loopStatus: "running", runID: "run_reviewer", runStatus: "running", executionID: "exec_reviewer", executionStatus: "running", pid: 4102})
	insertStopAllTestLoop(t, ctx, repos, now, stopAllLoopFixture{loopID: "loop_worker", seq: 3, loopType: "worker", loopStatus: "running", runID: "run_worker", runStatus: "running", executionID: "exec_worker", executionStatus: "running", pid: 4103})
	insertStopAllTestLoop(t, ctx, repos, now, stopAllLoopFixture{loopID: "loop_fixer", seq: 4, loopType: "fixer", loopStatus: "running", runID: "run_fixer", runStatus: "running", executionID: "exec_fixer", executionStatus: "cancelling", pid: 4104})
	insertStopAllTestLoop(t, ctx, repos, now, stopAllLoopFixture{loopID: "loop_future", seq: 5, loopType: "auditor", loopStatus: "running", runID: "run_future", runStatus: "running", executionID: "exec_future", executionStatus: "running", pid: 4105})
	insertStopAllTestLoop(t, ctx, repos, now, stopAllLoopFixture{loopID: "loop_queued", seq: 6, loopType: "worker", loopStatus: "queued", queueStatus: "running"})
	insertStopAllTestLoop(t, ctx, repos, now, stopAllLoopFixture{loopID: "loop_waiting", seq: 7, loopType: "reviewer", loopStatus: "waiting", runID: "run_waiting", runStatus: "success"})

	var signalPIDs []int
	response, err := stopAllLoops(ctx, services, "Stopped by test", func() time.Time { return now }, func(pid int, sig syscall.Signal) error {
		if sig != syscall.SIGTERM {
			return nil
		}
		signalPIDs = append(signalPIDs, pid)
		if pid == -4102 || pid == 4102 {
			return fmt.Errorf("signal failed")
		}
		return syscall.ESRCH
	}, func(_ context.Context, execution storage.AgentExecutionRecord, pid int) (bool, bool, error) {
		return true, true, nil
	})
	if err != nil {
		t.Fatalf("stopAllLoops() error = %v", err)
	}

	if got, want := response.Summary, (stopAllSummary{Total: 7, Stopped: 6, Failed: 1}); got != want {
		t.Fatalf("stopAllLoops() summary = %#v, want %#v", got, want)
	}
	if got := stopAllItemTypes(response.Items); !slices.Equal(got, []string{"planner", "reviewer", "worker", "fixer", "auditor", "worker", "reviewer"}) {
		t.Fatalf("item types = %#v, want mixed known and future types", got)
	}
	assertStopAllItemResult(t, response.Items, "loop_reviewer", string(stopAllResultFailed))
	assertStopAllItemResult(t, response.Items, "loop_fixer", string(stopAllResultStopped))
	assertStopAllItemResult(t, response.Items, "loop_future", string(stopAllResultStopped))
	assertStopAllItemResult(t, response.Items, "loop_queued", string(stopAllResultStopped))
	assertStopAllItemResult(t, response.Items, "loop_waiting", string(stopAllResultStopped))
	if !slices.Contains(signalPIDs, -4101) || !slices.Contains(signalPIDs, -4103) || !slices.Contains(signalPIDs, -4105) {
		t.Fatalf("signal pids = %#v, want other loops processed after failure", signalPIDs)
	}

	repeated, err := stopAllLoops(ctx, services, "Stopped by test again", func() time.Time { return now.Add(time.Minute) }, func(pid int, sig syscall.Signal) error {
		if sig != syscall.SIGTERM {
			return nil
		}
		return syscall.ESRCH
	}, func(_ context.Context, execution storage.AgentExecutionRecord, pid int) (bool, bool, error) {
		return true, true, nil
	})
	if err != nil {
		t.Fatalf("second stopAllLoops() error = %v", err)
	}
	if repeated.Summary.AlreadyStopping < 4 {
		t.Fatalf("second stopAllLoops() summary = %#v, want repeated call to report alreadyStopping", repeated.Summary)
	}
	assertStopAllItemResult(t, repeated.Items, "loop_future", string(stopAllResultAlreadyStopping))
}

func TestClassifyStopAllResultChecksAllActiveExecutionsBeforeAlreadyStopping(t *testing.T) {
	runID := "run_mixed"
	cancelling := storage.AgentExecutionRecord{ID: "exec_cancelling", RunID: &runID, Status: "cancelling"}
	running := storage.AgentExecutionRecord{ID: "exec_running", RunID: &runID, Status: "running"}
	candidate := stopAllCandidate{
		Loop:      storage.LoopRecord{ID: "loop_mixed", Status: "paused"},
		Run:       &storage.RunRecord{ID: runID, Status: "running"},
		Execution: &cancelling,
		Executions: []storage.AgentExecutionRecord{
			cancelling,
			running,
		},
	}

	if got := classifyStopAllResult(candidate); got != stopAllResultStopped {
		t.Fatalf("classifyStopAllResult() = %q, want %q while any active execution is still running", got, stopAllResultStopped)
	}
}

func TestRefreshStopAllCandidateKeepsOtherActiveExecutionsForClassification(t *testing.T) {
	services, repos, now := newStopAllTestServices(t)
	ctx := context.Background()

	insertStopAllTestLoop(t, ctx, repos, now, stopAllLoopFixture{loopID: "loop_mixed", seq: 1, loopType: "worker", loopStatus: "paused", runID: "run_newest", runStatus: "finished", executionID: "exec_newest", executionStatus: "cancelling"})
	otherRunID := "run_other"
	olderISO := now.Add(-time.Minute).Format("2006-01-02T15:04:05.000Z")
	if err := repos.Runs.Upsert(ctx, storage.RunRecord{ID: otherRunID, LoopID: "loop_mixed", Status: "running", StartedAt: olderISO, LastHeartbeatAt: &olderISO, CreatedAt: olderISO, UpdatedAt: olderISO}); err != nil {
		t.Fatalf("Runs.Upsert(%s) error = %v", otherRunID, err)
	}
	if err := repos.AgentExecutions.Upsert(ctx, storage.AgentExecutionRecord{ID: "exec_other", ProjectID: stringPtr("project_1"), RunID: &otherRunID, Vendor: "codex", Status: "running", StartedAt: olderISO, CreatedAt: olderISO, UpdatedAt: olderISO}); err != nil {
		t.Fatalf("AgentExecutions.Upsert(exec_other) error = %v", err)
	}

	refreshed, err := refreshStopAllCandidate(ctx, services.Repositories, "loop_mixed")
	if err != nil {
		t.Fatalf("refreshStopAllCandidate() error = %v", err)
	}
	if len(refreshed.Executions) != 2 {
		t.Fatalf("len(refreshed.Executions) = %d, want 2", len(refreshed.Executions))
	}
	if refreshed.Execution == nil || refreshed.Execution.ID != "exec_newest" {
		t.Fatalf("refreshed.Execution = %#v, want newest cancelling execution", refreshed.Execution)
	}
	if got := classifyStopAllResult(refreshed); got != stopAllResultStopped {
		t.Fatalf("classifyStopAllResult(refresh) = %q, want %q while another execution is still running", got, stopAllResultStopped)
	}
}

type fakeActiveExecution struct {
	killed bool
	reason string
	onKill func() error
}

func (f *fakeActiveExecution) Kill(reason string) error {
	f.killed = true
	f.reason = reason
	if f.onKill != nil {
		return f.onKill()
	}
	return nil
}

type stopAllLoopFixture struct {
	loopID          string
	seq             int64
	loopType        string
	loopStatus      string
	runID           string
	runStatus       string
	executionID     string
	executionStatus string
	pid             int64
	queueStatus     string
}

func newStopAllTestServices(t *testing.T) (looperdruntime.Services, *storage.Repositories, time.Time) {
	t.Helper()
	ctx := context.Background()
	coordinator, err := storage.OpenSQLiteCoordinator(ctx, filepath.Join(t.TempDir(), "looper.sqlite"), storage.SQLiteCoordinatorOptions{Migrations: storage.EmbeddedMigrations})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	if _, err := coordinator.MigrationRunner().RunPending(ctx); err != nil {
		t.Fatalf("MigrationRunner().RunPending() error = %v", err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })

	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 21, 12, 0, 0, 0, time.UTC)
	nowISO := now.Format("2006-01-02T15:04:05.000Z")
	project := storage.ProjectRecord{ID: "project_1", Name: "Looper", RepoPath: t.TempDir(), CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := repos.Projects.Upsert(ctx, project); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	services := looperdruntime.Services{Coordinator: coordinator, Repositories: repos, Loops: &loops.Service{DB: coordinator.DB(), Repos: repos, Now: func() time.Time { return now }}}
	return services, repos, now
}

func insertStopAllTestLoop(t *testing.T, ctx context.Context, repos *storage.Repositories, now time.Time, fixture stopAllLoopFixture) {
	t.Helper()
	nowISO := now.Format("2006-01-02T15:04:05.000Z")
	if err := repos.Loops.Upsert(ctx, storage.LoopRecord{ID: fixture.loopID, Seq: fixture.seq, ProjectID: "project_1", Type: fixture.loopType, TargetType: "project", Status: fixture.loopStatus, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert(%s) error = %v", fixture.loopID, err)
	}
	if fixture.runID != "" {
		if err := repos.Runs.Upsert(ctx, storage.RunRecord{ID: fixture.runID, LoopID: fixture.loopID, Status: fixture.runStatus, StartedAt: nowISO, LastHeartbeatAt: &nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
			t.Fatalf("Runs.Upsert(%s) error = %v", fixture.runID, err)
		}
	}
	if fixture.executionID != "" {
		pid := fixture.pid
		exec := storage.AgentExecutionRecord{ID: fixture.executionID, ProjectID: stringPtr("project_1"), LoopID: &fixture.loopID, RunID: &fixture.runID, Vendor: "codex", Status: fixture.executionStatus, StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}
		if pid > 0 {
			exec.PID = &pid
		}
		if err := repos.AgentExecutions.Upsert(ctx, exec); err != nil {
			t.Fatalf("AgentExecutions.Upsert(%s) error = %v", fixture.executionID, err)
		}
	}
	if fixture.queueStatus != "" {
		if err := repos.Queue.Upsert(ctx, storage.QueueItemRecord{ID: "queue_" + fixture.loopID, ProjectID: stringPtr("project_1"), LoopID: &fixture.loopID, Type: fixture.loopType, TargetType: "project", TargetID: "project_1", DedupeKey: "dedupe:" + fixture.loopID, Priority: 1, Status: fixture.queueStatus, AvailableAt: nowISO, Attempts: 0, MaxAttempts: 1, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
			t.Fatalf("Queue.Upsert(%s) error = %v", fixture.loopID, err)
		}
	}
}

func assertStopAllItemResult(t *testing.T, items []stopAllItem, loopID, want string) {
	t.Helper()
	for _, item := range items {
		if item.LoopID == loopID {
			if item.Result != want {
				t.Fatalf("item %s result = %q, want %q", loopID, item.Result, want)
			}
			return
		}
	}
	t.Fatalf("loop %s not found in stop-all items: %#v", loopID, items)
}

func stopAllItemTypes(items []stopAllItem) []string {
	types := make([]string, 0, len(items))
	for _, item := range items {
		types = append(types, item.Type)
	}
	return types
}

func stringPtr(value string) *string { return &value }
