package main

import (
	"context"
	"errors"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/loops"
	looperdruntime "github.com/nexu-io/looper/internal/runtime"
	"github.com/nexu-io/looper/internal/storage"
)

// When the Supervisor registry is present but has no entry for a stoppable
// execution that still has a persisted PID, stop/close must fail loudly rather
// than report success while leaving a live agent process behind (#576 / PR #586).
func TestCloseLoopFailsWhenLiveHandleMissingWithPersistedPID(t *testing.T) {
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
	pid := int64(9876)
	agentExecution := storage.AgentExecutionRecord{ID: "agentexec_1", ProjectID: &project.ID, LoopID: &loop.ID, RunID: &run.ID, Vendor: "codex", Status: "running", PID: &pid, StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := repos.AgentExecutions.Upsert(ctx, agentExecution); err != nil {
		t.Fatalf("AgentExecutions.Upsert() error = %v", err)
	}

	// Registry present but empty: no live handle for the running execution.
	registry := looperdruntime.NewActiveExecutionRegistry()
	services := looperdruntime.Services{
		Coordinator:      coordinator,
		Repositories:     repos,
		Loops:            &loops.Service{DB: coordinator.DB(), Repos: repos, Now: func() time.Time { return now }},
		ActiveExecutions: registry,
	}

	signaled := false
	if _, err := closeLoop(ctx, services, loop.ID, "Closed by test", func() time.Time { return now }, func(int, syscall.Signal) error {
		signaled = true
		return nil
	}, nil); !errors.Is(err, looperdruntime.ErrAgentLiveHandleMissing) {
		t.Fatalf("closeLoop() error = %v, want %v", err, looperdruntime.ErrAgentLiveHandleMissing)
	}
	if signaled {
		t.Fatal("PID signal path invoked while Supervisor registry was present")
	}
	storedLoop, err := repos.Loops.GetByID(ctx, loop.ID)
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if storedLoop == nil || storedLoop.Status != "running" {
		t.Fatalf("Loops.GetByID() = %#v, want running loop after missing-handle failure", storedLoop)
	}
	if registry.LoopStopActive(loop.ID) {
		t.Fatal("LoopStopActive = true after aborted closeLoop, want gate released")
	}
}

func TestStopLoopFailsWhenLiveHandleMissingWithPersistedPID(t *testing.T) {
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
	pid := int64(9876)
	agentExecution := storage.AgentExecutionRecord{ID: "agentexec_1", ProjectID: &project.ID, LoopID: &loop.ID, RunID: &run.ID, Vendor: "codex", Status: "running", PID: &pid, StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := repos.AgentExecutions.Upsert(ctx, agentExecution); err != nil {
		t.Fatalf("AgentExecutions.Upsert() error = %v", err)
	}

	registry := looperdruntime.NewActiveExecutionRegistry()
	services := looperdruntime.Services{
		Coordinator:      coordinator,
		Repositories:     repos,
		Loops:            &loops.Service{DB: coordinator.DB(), Repos: repos, Now: func() time.Time { return now }},
		ActiveExecutions: registry,
	}

	if _, err := stopLoop(ctx, services, loop.ID, "Stopped by test", func() time.Time { return now }, func(int, syscall.Signal) error {
		t.Fatal("PID signal path invoked while Supervisor registry was present")
		return nil
	}, nil); !errors.Is(err, looperdruntime.ErrAgentLiveHandleMissing) {
		t.Fatalf("stopLoop() error = %v, want %v", err, looperdruntime.ErrAgentLiveHandleMissing)
	}
}

// stopAll fallback after Pause failure must not leave a sticky loop stop gate:
// stopCandidateExecution closes admission only for the kill window, then releases.

// stopAll fallback after Pause failure must not leave a sticky loop stop gate:
// stopCandidateExecution closes admission only for the kill window, then releases.
