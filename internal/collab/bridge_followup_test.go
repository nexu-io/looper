package collab

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nexu-io/looper/internal/feishubridge"
	"github.com/nexu-io/looper/internal/storage"
	"github.com/nexu-io/looper/internal/worker"
)

// A successful conversational reply is not a new task conclusion. Missing
// first conclusions, malformed updates and actual execution failures still fail.
func TestBridgeFollowupWithoutTaskUpdate(t *testing.T) {
	for _, tc := range []struct{ prior, file, phase, want string }{
		{"completed", "", "finished", "completed"},
		{"needs_input", "", "finished", "awaiting_human"},
		{"monitoring_prs", "", "finished", "shepherding"},
		{"", "", "finished", "failed"},
		{"completed", "{", "finished", "failed"},
		{"completed", "", "failed", "failed"},
	} {
		t.Run(tc.prior+"/"+tc.file+"/"+tc.phase, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			db, err := storage.OpenSQLiteCoordinator(ctx, filepath.Join(dir, "state.sqlite"), storage.SQLiteCoordinatorOptions{Migrations: storage.EmbeddedMigrations})
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err = db.MigrationRunner().RunPending(ctx, storage.RunPendingOptions{}); err != nil {
				t.Fatal(err)
			}
			h := &bridgeHost{dir: dir, db: db.DB(), repos: storage.NewRepositories(db.DB()), active: map[string]bool{}}
			if err = h.repos.Projects.Upsert(ctx, storage.ProjectRecord{ID: "local", Name: "local", RepoPath: dir, CreatedAt: "2026-11-17T00:00:00Z", UpdatedAt: "2026-11-17T00:00:00Z"}); err != nil {
				t.Fatal(err)
			}
			loop, err := h.enqueue(ctx, worker.EnqueueInput{ProjectID: "local", Title: "Task with follow-up", Prompt: "Explain the current progress"})
			if err != nil {
				t.Fatal(err)
			}
			_, state, _ := readBridgeTask(loop)
			state.ThreadID, state.SessionID = "omt_original", "native-original"
			state.Outcome = Outcome{State: tc.prior, Summary: "Last confirmed conclusion", PullRequests: []string{"https://github.com/acme/repo/pull/1"}}
			if err = h.patch(ctx, loop.ID, "completed", state, state.Outcome.Summary); err != nil {
				t.Fatal(err)
			}
			event := feishubridge.TurnEvent{TurnID: "followup", ThreadID: state.ThreadID, SessionID: state.SessionID, Phase: "started"}
			if _, err = h.recordTurnEvent(ctx, event); err != nil {
				t.Fatal(err)
			}
			if tc.file != "" {
				if err = os.MkdirAll(filepath.Dir(h.resultPath(loop.ID)), 0700); err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(h.resultPath(loop.ID), []byte(tc.file), 0600); err != nil {
					t.Fatal(err)
				}
			}
			event.Phase, event.Terminal = tc.phase, "done"
			if _, err = h.recordTurnEvent(ctx, event); err != nil {
				t.Fatal(err)
			}
			saved, err := h.repos.Loops.GetByID(ctx, loop.ID)
			if err != nil {
				t.Fatal(err)
			}
			_, final, _ := readBridgeTask(*saved)
			if saved.Status != tc.want || final.Outcome.Summary != state.Outcome.Summary || final.SessionID != state.SessionID || len(h.active) != 0 {
				t.Fatalf("follow-up lost task state: status=%s state=%+v active=%v", saved.Status, final, h.active)
			}
			events, err := h.repos.Events.ListByLoop(ctx, loop.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range events {
				if e.EventType == "artifacts.reported" || e.EventType == "repository.reported" {
					t.Fatal("ordinary conversation re-reported stale delivery as new evidence")
				}
			}
		})
	}
}
