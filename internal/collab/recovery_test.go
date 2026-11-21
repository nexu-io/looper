package collab

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/eventlog"
	"github.com/nexu-io/looper/internal/storage"
	"github.com/nexu-io/looper/internal/worker"
)

// Seed the durable boundaries at which a host can disappear. Exercise actual
// startup and execution rather than testing a second recovery implementation.
func TestStartupRecoversClaimsWithoutOverlappingUncertainExecutions(t *testing.T) {
	for _, boundary := range []string{"claimed_before_run", "run_without_execution", "durably_finished_execution", "uncertain_execution"} {
		t.Run(boundary, func(t *testing.T) {
			t.Parallel()
			cfg, dir := hostFixture(t, `#!/bin/sh
printf run >> "$COLLAB_TEST_CALLS"
printf '%s\n' '{"type":"thread.started","thread_id":"recovery-session"}' '{"type":"item.completed","item":{"type":"agent_message","text":"__LOOPER_RESULT__={\"summary\":\"recovered fixture\"}"}}'
`)
			calls := filepath.Join(dir, "model-calls")
			cfg.Agent.Env = map[string]string{"COLLAB_TEST_CALLS": calls}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			db, err := storage.OpenSQLiteCoordinator(ctx, filepath.Join(dir, "looper.sqlite"), storage.SQLiteCoordinatorOptions{Migrations: storage.EmbeddedMigrations})
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err := db.MigrationRunner().RunPending(ctx, storage.RunPendingOptions{}); err != nil {
				t.Fatal(err)
			}
			repos := storage.NewRepositories(db.DB())
			now := eventlog.FormatJavaScriptISOString(time.Now().UTC())
			metadata := `{"repo":"acme/local"}`
			if err := repos.Projects.Upsert(ctx, storage.ProjectRecord{ID: "local", Name: "local", RepoPath: cfg.Projects[0].RepoPath, MetadataJSON: &metadata, CreatedAt: now, UpdatedAt: now}); err != nil {
				t.Fatal(err)
			}
			runner := worker.New(worker.Options{DB: db.DB(), Repos: repos})
			loop, err := runner.Enqueue(ctx, worker.EnqueueInput{ProjectID: "local", Repo: "acme/local", Prompt: "Verify local recovery fixture"})
			if err != nil {
				t.Fatal(err)
			}
			claimed, err := repos.Queue.ClaimNextOfType(ctx, eventlog.FormatJavaScriptISOString(time.Now().UTC()), "crashed-host", "worker")
			if err != nil || claimed == nil {
				t.Fatalf("seed claim: %v %v", claimed, err)
			}
			if boundary != "claimed_before_run" {
				loop.Status = "running"
				if err := repos.Loops.Upsert(ctx, loop); err != nil {
					t.Fatal(err)
				}
				runID := "crashed-run"
				step := "execute"
				if err := repos.Runs.Upsert(ctx, storage.RunRecord{ID: runID, LoopID: loop.ID, Status: "running", CurrentStep: &step, StartedAt: now, CreatedAt: now, UpdatedAt: now}); err != nil {
					t.Fatal(err)
				}
				if boundary != "run_without_execution" {
					execution := storage.AgentExecutionRecord{ID: "old-execution", ProjectID: &loop.ProjectID, LoopID: &loop.ID, RunID: &runID, Vendor: "codex", Status: "running", StartedAt: now, CreatedAt: now, UpdatedAt: now}
					if boundary == "durably_finished_execution" {
						execution.Status = "completed"
						execution.EndedAt = &now
					}
					if err := repos.AgentExecutions.Upsert(ctx, execution); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				done <- Serve(ctx, Options{Config: cfg, StateDir: dir, NetworkStatePath: filepath.Join(dir, "network.json")})
			}()
			defer func() {
				cancel()
				select {
				case err := <-done:
					if err != nil {
						t.Error(err)
					}
				case <-time.After(5 * time.Second):
					t.Error("host failed to drain")
				}
			}()
			waitForTask := func(id, desired string) {
				t.Helper()
				for ctx.Err() == nil {
					data, err := Request(ctx, dir, "GET", "/status", nil)
					if err == nil {
						var status Status
						if err := json.Unmarshal(data, &status); err != nil {
							t.Fatal(err)
						}
						for _, task := range status.Tasks {
							if task.ID == id && task.Status == "failed" {
								t.Fatalf("task %s failed before reaching %s", id, desired)
							}
							if task.ID == id && task.Status == desired {
								return
							}
						}
					}
					time.Sleep(15 * time.Millisecond)
				}
				t.Fatalf("task %s never reached %s", id, desired)
			}
			if boundary == "uncertain_execution" {
				waitForTask(loop.ID, "paused")
				if _, err := os.Stat(calls); !os.IsNotExist(err) {
					t.Fatal("uncertain old execution was overlapped")
				}
				data, err := Request(ctx, dir, "POST", "/run", worker.EnqueueInput{ProjectID: "local", Repo: "acme/local", Prompt: "Independent task can proceed"})
				if err != nil {
					t.Fatal(err)
				}
				var created Task
				if err := json.Unmarshal(data, &created); err != nil {
					t.Fatal(err)
				}
				waitForTask(created.ID, "completed")
				waitForTask(loop.ID, "paused")
			} else {
				waitForTask(loop.ID, "completed")
			}
			if data, err := os.ReadFile(calls); err != nil || string(data) != "run" {
				t.Fatalf("expected exactly one execution, got %q (%v)", data, err)
			}
		})
	}
}
