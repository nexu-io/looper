package collab

import (
	"context"
	"encoding/json"
	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/feishubridge"
	"github.com/nexu-io/looper/internal/storage"
	"github.com/nexu-io/looper/internal/worker"
	"path/filepath"
	"testing"
)

func TestWaitReferenceSurvivesNextRunAndReplayOnlyForPlaneCollab(t *testing.T) {
	for _, strict := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary_native", true: "plane_collab"}[strict], func(t *testing.T) {
			ctx := context.Background()
			db, err := storage.OpenSQLiteCoordinator(ctx, filepath.Join(t.TempDir(), "state.sqlite"), storage.SQLiteCoordinatorOptions{Migrations: storage.EmbeddedMigrations})
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err = db.MigrationRunner().RunPending(ctx, storage.RunPendingOptions{}); err != nil {
				t.Fatal(err)
			}
			repos := storage.NewRepositories(db.DB())
			h := &bridgeHost{db: db.DB(), repos: repos, cfg: config.Config{Projects: []config.ProjectRefConfig{{ID: "local"}}}}
			if err = repos.Projects.Upsert(ctx, storage.ProjectRecord{ID: "local", Name: "local", CreatedAt: "2026-11-18T00:00:00Z", UpdatedAt: "2026-11-18T00:00:00Z"}); err != nil {
				t.Fatal(err)
			}
			input := worker.EnqueueInput{Prompt: "test"}
			if strict {
				input.StrictDispatchID = "dispatch"
			}
			loop, err := h.enqueue(ctx, input)
			if err != nil {
				t.Fatal(err)
			}
			state := bridgeTask{TrackingRunID: "first", Outcome: Outcome{WaitKind: "review", WaitRole: "qa"}}
			terminal := feishubridge.TurnEvent{Phase: "finished", Terminal: "done"}
			for i := 0; i < 2; i++ {
				if err = h.saveTurn(ctx, loop, state, terminal, "awaiting_human"); err != nil {
					t.Fatal(err)
				}
			}
			if err = h.patch(ctx, loop.ID, "awaiting_human", state, ""); err != nil {
				t.Fatal(err)
			}
			saved, _ := repos.Loops.GetByID(ctx, loop.ID)
			next := state
			next.TrackingRunID = "second"
			if err = h.saveTurn(ctx, *saved, next, feishubridge.TurnEvent{Phase: "started"}, "running"); err != nil {
				t.Fatal(err)
			}
			events, err := repos.Events.ListByLoop(ctx, loop.ID)
			if err != nil {
				t.Fatal(err)
			}
			count := 0
			for _, event := range events {
				if event.EventType != "human.requested" && event.EventType != "human.answered" {
					continue
				}
				count++
				var attrs map[string]any
				if err = json.Unmarshal([]byte(event.PayloadJSON), &attrs); err != nil {
					t.Fatal(err)
				}
				if strict {
					if attrs["reference"] != "first" || attrs["waitKind"] != "review" || attrs["role"] != "qa" {
						t.Fatalf("lost wait identity: %v", attrs)
					}
					if event.EventType == "human.answered" && attrs["source"] != "resume_started" {
						t.Fatal(attrs)
					}
				} else if _, ok := attrs["reference"]; ok {
					t.Fatalf("new fields leaked: %v", attrs)
				}
			}
			if count != 2 {
				t.Fatalf("replay duplicated wait: %d", count)
			}
		})
	}
	attrs := waitAttributes(Outcome{WaitKind: "made-up", WaitRole: "CEO"}, "id")
	if attrs["waitKind"] != "unknown" || attrs["role"] != "unknown" {
		t.Fatal(attrs)
	}
}
