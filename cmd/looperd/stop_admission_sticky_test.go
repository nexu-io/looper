package main

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/agent"
	"github.com/nexu-io/looper/internal/loops"
	looperdruntime "github.com/nexu-io/looper/internal/runtime"
	"github.com/nexu-io/looper/internal/storage"
)

// After a successful terminal close, per-loop spawn admission stays closed so
// in-flight runners cannot AdmitSpawn past the durable terminate.
func TestCloseLoopKeepsSpawnAdmissionClosedAfterSuccess(t *testing.T) {
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
	loop := storage.LoopRecord{ID: "loop_close_sticky", Seq: 32, ProjectID: project.ID, Type: "worker", TargetType: "project", Status: "running", CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := repos.Loops.Upsert(ctx, loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	registry := looperdruntime.NewActiveExecutionRegistry()
	services := looperdruntime.Services{
		Coordinator:      coordinator,
		Repositories:     repos,
		Loops:            &loops.Service{DB: coordinator.DB(), Repos: repos, Now: func() time.Time { return now }},
		ActiveExecutions: registry,
	}

	if _, err := closeLoop(ctx, services, loop.ID, "Closed by test", func() time.Time { return now }, nil, nil); err != nil {
		t.Fatalf("closeLoop() error = %v", err)
	}
	if !registry.LoopStopActive(loop.ID) {
		t.Fatal("LoopStopActive = false after successful closeLoop, want sticky closed gate")
	}
	_, err = registry.AdmitSpawn(ctx, agent.SpawnMeta{
		LoopID: loop.ID, RunID: "run_late", ExecutionID: "exec_late",
	})
	if !errors.Is(err, agent.ErrSpawnLoopStopping) {
		t.Fatalf("AdmitSpawn after closeLoop error = %v, want ErrSpawnLoopStopping", err)
	}
}

// After a successful stop, per-loop spawn admission stays closed so an in-flight
// runner that reaches AgentExecutor.Start after halt returns cannot AdmitSpawn.

// After a successful stop, per-loop spawn admission stays closed so an in-flight
// runner that reaches AgentExecutor.Start after halt returns cannot AdmitSpawn.

// After a successful stop, per-loop spawn admission stays closed so an in-flight
// runner that reaches AgentExecutor.Start after halt returns cannot AdmitSpawn.
func TestStopLoopKeepsSpawnAdmissionClosedAfterReturn(t *testing.T) {
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

	registry := looperdruntime.NewActiveExecutionRegistry()
	services := looperdruntime.Services{
		Coordinator:      coordinator,
		Repositories:     repos,
		Loops:            &loops.Service{DB: coordinator.DB(), Repos: repos, Now: func() time.Time { return now }},
		ActiveExecutions: registry,
	}

	if _, err := stopLoop(ctx, services, loop.ID, "Stopped by test", func() time.Time { return now }, nil, nil); err != nil {
		t.Fatalf("stopLoop() error = %v", err)
	}
	if !registry.LoopStopActive(loop.ID) {
		t.Fatal("LoopStopActive = false after successful stopLoop, want sticky closed gate")
	}
	_, err = registry.AdmitSpawn(ctx, agent.SpawnMeta{
		LoopID: loop.ID, RunID: "run_late", ExecutionID: "exec_late",
	})
	if !errors.Is(err, agent.ErrSpawnLoopStopping) {
		t.Fatalf("AdmitSpawn after stopLoop error = %v, want ErrSpawnLoopStopping", err)
	}

	// Intentional re-activation reopens the gate (unpause/retry/fresh claim path).
	registry.ClearLoopStop(loop.ID)
	if _, err := registry.AdmitSpawn(ctx, agent.SpawnMeta{
		LoopID: loop.ID, RunID: "run_resume", ExecutionID: "exec_resume",
	}); err != nil {
		t.Fatalf("AdmitSpawn after ClearLoopStop error = %v, want success", err)
	}
}

// Terminal close must not BeginLoopStop (lease cancel is irreversible for
// execution.run) before abortable run/execution preflight. A transient lookup
// error leaves the loop running, so active agents must not be half-killed.

// Terminal close must not BeginLoopStop (lease cancel is irreversible for
// execution.run) before abortable run/execution preflight. A transient lookup
// error leaves the loop running, so active agents must not be half-killed.
