package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/agent"
	"github.com/nexu-io/looper/internal/loops"
	looperdruntime "github.com/nexu-io/looper/internal/runtime"
	"github.com/nexu-io/looper/internal/storage"
)

// Terminal close must not BeginLoopStop (lease cancel is irreversible for
// execution.run) before abortable run/execution preflight. A transient lookup
// error leaves the loop running, so active agents must not be half-killed.
func TestCloseLoopDoesNotCancelLeasesWhenRunLookupFails(t *testing.T) {
	ctx := context.Background()
	coordinator, err := storage.OpenSQLiteCoordinator(ctx, filepath.Join(t.TempDir(), "looper.sqlite"), storage.SQLiteCoordinatorOptions{Migrations: storage.EmbeddedMigrations})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	if _, err := coordinator.MigrationRunner().RunPending(ctx); err != nil {
		t.Fatalf("MigrationRunner().RunPending() error = %v", err)
	}

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
	lease, err := registry.AdmitSpawn(ctx, agent.SpawnMeta{
		LoopID: loop.ID, RunID: "run_live", ExecutionID: "exec_live",
	})
	if err != nil {
		t.Fatalf("AdmitSpawn() error = %v", err)
	}
	if lease.Context().Err() != nil {
		t.Fatalf("lease already cancelled before close: %v", lease.Context().Err())
	}

	services := looperdruntime.Services{
		Coordinator:      coordinator,
		Repositories:     repos,
		Loops:            &loops.Service{DB: coordinator.DB(), Repos: repos, Now: func() time.Time { return now }},
		ActiveExecutions: registry,
	}

	// Force Runs.GetLatestByLoopID to fail after preflight would otherwise open
	// the stop gate: close the SQLite coordinator under the services.
	if err := coordinator.Close(); err != nil {
		t.Fatalf("coordinator.Close() error = %v", err)
	}

	if _, err := closeLoop(ctx, services, loop.ID, "Closed by test", func() time.Time { return now }, nil, nil); err == nil {
		t.Fatal("closeLoop() error = nil, want run lookup failure")
	}
	if err := lease.Context().Err(); err != nil {
		t.Fatalf("lease cancelled after failed close preflight: %v (BeginLoopStop must run only after abortable lookups)", err)
	}
	if registry.LoopStopActive(loop.ID) {
		t.Fatal("LoopStopActive = true after aborted close preflight, want gate never opened")
	}
	if _, err := registry.AdmitSpawn(ctx, agent.SpawnMeta{
		LoopID: loop.ID, RunID: "run_after", ExecutionID: "exec_after",
	}); err != nil {
		t.Fatalf("AdmitSpawn after failed close preflight error = %v, want success", err)
	}
}

// Failed Pause must not cancel spawn leases: BeginLoopStop is irreversible for
// lease contexts wired into execution.run, so it runs only after Pause succeeds.

// Failed Pause must not cancel spawn leases: BeginLoopStop is irreversible for
// lease contexts wired into execution.run, so it runs only after Pause succeeds.

// Failed Pause must not cancel spawn leases: BeginLoopStop is irreversible for
// lease contexts wired into execution.run, so it runs only after Pause succeeds.
func TestStopLoopDoesNotCancelLeasesWhenPauseFails(t *testing.T) {
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
	// Terminal status cannot transition to paused → Pause fails while loop stays terminal.
	loop := storage.LoopRecord{ID: "loop_1", Seq: 30, ProjectID: project.ID, Type: "worker", TargetType: "project", Status: "terminated", CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := repos.Loops.Upsert(ctx, loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	registry := looperdruntime.NewActiveExecutionRegistry()
	lease, err := registry.AdmitSpawn(ctx, agent.SpawnMeta{
		LoopID: loop.ID, RunID: "run_pending", ExecutionID: "exec_pending",
	})
	if err != nil {
		t.Fatalf("AdmitSpawn() error = %v", err)
	}
	if lease.Context().Err() != nil {
		t.Fatalf("lease already cancelled before stop: %v", lease.Context().Err())
	}

	services := looperdruntime.Services{
		Coordinator:      coordinator,
		Repositories:     repos,
		Loops:            &loops.Service{DB: coordinator.DB(), Repos: repos, Now: func() time.Time { return now }},
		ActiveExecutions: registry,
	}

	if _, err := stopLoop(ctx, services, loop.ID, "Stopped by test", func() time.Time { return now }, nil, nil); err == nil {
		t.Fatal("stopLoop() error = nil, want Pause transition failure")
	}
	if err := lease.Context().Err(); err != nil {
		t.Fatalf("lease cancelled after failed Pause: %v (BeginLoopStop must run only after successful pause)", err)
	}
	// New spawns for the loop must still be admitted after the failed stop.
	if _, err := registry.AdmitSpawn(ctx, agent.SpawnMeta{
		LoopID: loop.ID, RunID: "run_after", ExecutionID: "exec_after",
	}); err != nil {
		t.Fatalf("AdmitSpawn after failed stop error = %v, want success (loop stop admission released)", err)
	}
}

// When the Supervisor registry is present but has no entry for a stoppable
// execution that still has a persisted PID, stop/close must fail loudly rather
// than report success while leaving a live agent process behind (#576 / PR #586).
