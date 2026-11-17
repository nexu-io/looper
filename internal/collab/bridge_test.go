package collab

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/agent"
	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/feishubridge"
	"github.com/nexu-io/looper/internal/storage"
	"github.com/nexu-io/looper/internal/worker"
)

// Real task records and native-session callbacks: no model, PR or repo is
// required for a read-only task to finish, and replies reuse the task identity.
func TestBridgeTaskCrossRepositoryConversation(t *testing.T) {
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
	repos := storage.NewRepositories(db.DB())
	h := &bridgeHost{dir: dir, db: db.DB(), repos: repos, active: map[string]bool{}, cfg: config.Config{Projects: []config.ProjectRefConfig{
		{ID: "local", Repo: "acme/frontend", RepoPath: "/available/frontend"},
		{ID: "other", Repo: "acme/backend", RepoPath: "/available/backend"},
	}}}
	for _, p := range h.cfg.Projects {
		if err := repos.Projects.Upsert(ctx, storage.ProjectRecord{ID: p.ID, Name: p.ID, RepoPath: p.RepoPath, CreatedAt: "2026-11-16T00:00:00.000Z", UpdatedAt: "2026-11-16T00:00:00.000Z"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.writeWorkspace(filepath.Join(dir, "workspace")); err != nil {
		t.Fatal(err)
	}
	instructions, _ := os.ReadFile(filepath.Join(dir, "workspace", "AGENTS.md"))
	for _, want := range []string{"/available/frontend", "/available/backend", "允许跨多个仓库"} {
		if !strings.Contains(string(instructions), want) {
			t.Fatalf("missing repository context %q", want)
		}
	}
	loop, err := h.enqueue(ctx, worker.EnqueueInput{Repo: "old-client/arbitrary", Title: "Investigate both", Prompt: "Read both repositories"})
	if err != nil {
		t.Fatal(err)
	}
	input, state, ok := readBridgeTask(loop)
	if !ok || input.Repo != "" || loop.Repo != nil {
		t.Fatalf("task acquired a repository binding: %+v %+v", loop, input)
	}
	state.RootID = "om_root"
	if err := h.patch(ctx, loop.ID, "running", state, ""); err != nil {
		t.Fatal(err)
	}
	turnID := "turn-1"
	event := func(phase, root, thread string) {
		t.Helper()
		id, err := h.recordTurnEvent(ctx, feishubridge.TurnEvent{TurnID: turnID, Phase: phase, RootMessageID: root, ThreadID: thread, SessionID: "native-session", Terminal: "done"})
		if err != nil || id != loop.ID {
			t.Fatalf("%s callback: id=%s err=%v", phase, id, err)
		}
	}
	event("started", "om_root", "")
	event("bound", "om_root", "omt_topic")
	event("started", "om_root", "omt_topic") // replay must not create a second run
	saved, _ := repos.Loops.GetByID(ctx, loop.ID)
	_, bound, _ := readBridgeTask(*saved)
	if bound.ThreadID != "omt_topic" || bound.SessionID != "native-session" {
		t.Fatalf("session not durable mid-turn: %+v", bound)
	}
	writeResult := func(o Outcome) {
		t.Helper()
		data, _ := json.Marshal(o)
		if err := os.WriteFile(h.resultPath(loop.ID), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeResult(Outcome{State: "needs_input", Summary: "Choose an option", WaitingFor: []string{"ou_product"}})
	event("finished", "om_root", "omt_topic")
	saved, _ = repos.Loops.GetByID(ctx, loop.ID)
	if saved.Status != "awaiting_human" || len(h.active) != 0 {
		t.Fatalf("not waiting: %+v", saved)
	}
	event("finished", "om_root", "omt_topic") // replay must not change the first turn
	turnID = "turn-2"
	// A subsequent human reply has its own message ID; the real topic routes it.
	event("started", "om_colleague_reply", "omt_topic")
	if _, err := os.Stat(h.resultPath(loop.ID)); !os.IsNotExist(err) {
		t.Fatal("prior conclusion leaked into next turn")
	}
	writeResult(Outcome{State: "completed", Summary: "Read-only investigation completed", Repositories: agent.ParseRepositoryReports([]map[string]string{{"repo": "https://github.com/acme/frontend", "role": "consult", "validationStatus": "not_run"}, {"repo": "https://github.com/acme/backend", "role": "consult", "validationStatus": "not_run"}})})
	event("finished", "om_colleague_reply", "omt_topic")
	saved, _ = repos.Loops.GetByID(ctx, loop.ID)
	if saved.Status != "completed" || strings.Contains(*saved.MetadataJSON, `"hitl"`) || len(taskStatus(*saved).PullRequests) != 0 {
		t.Fatalf("read-only task failed to finish: %+v", saved)
	}
	runs, err := repos.Runs.ListByLoop(ctx, loop.ID)
	if err != nil || len(runs) != 2 {
		t.Fatalf("turn count: %d %v", len(runs), err)
	}
	for _, r := range runs {
		if r.EndedAt == nil || r.Status != "success" {
			t.Fatal("ended turn left active")
		}
	}
	events, err := repos.Events.ListByLoop(ctx, loop.ID)
	counts := map[string]int{}
	for _, e := range events {
		counts[e.EventType]++
		if e.CorrelationID == nil || *e.CorrelationID != loop.ID {
			t.Fatal("uncorrelated event")
		}
	}
	if err != nil || counts["task.received"] != 1 || counts["run.started"] != 2 || counts["run.ended"] != 2 || counts["human.answered"] != 1 || counts["repository.reported"] != 2 {
		t.Fatalf("timeline: %v %v", counts, err)
	}
	if id, err := h.recordTurnEvent(ctx, feishubridge.TurnEvent{Phase: "started", ThreadID: "omt_unrelated"}); err != nil || id != "" {
		t.Fatalf("unrelated topic routed to task: %s %v", id, err)
	}
	turnID = "turn-3"
	event("started", "om_followup", "omt_topic")
	if err := h.interruptTrackedTurns(ctx); err != nil {
		t.Fatal(err)
	}
	interrupted, err := repos.Runs.GetByID(ctx, bridgeRunID(loop.ID, turnID))
	if err != nil || interrupted == nil || interrupted.Status != "interrupted" || interrupted.EndedAt == nil {
		t.Fatalf("lost crash evidence: %+v %v", interrupted, err)
	}
	executions, err := repos.AgentExecutions.ListByRun(ctx, interrupted.ID)
	if err != nil || len(executions) != 1 || executions[0].Status != "failed" {
		t.Fatalf("crash execution: %+v %v", executions, err)
	}
	if err := h.saveTurn(ctx, *saved, bridgeTask{TrackingRunID: interrupted.ID}, feishubridge.TurnEvent{Phase: "finished", Terminal: "done"}, "completed"); err != nil {
		t.Fatal(err)
	}
	preserved, _ := repos.Runs.GetByID(ctx, interrupted.ID)
	if preserved.Status != "interrupted" {
		t.Fatal("late legacy callback replaced terminal evidence")
	}
}

func TestBridgeStatusPreservesAllPullRequests(t *testing.T) {
	links := []string{"https://github.com/acme/frontend/pull/1", "https://github.com/acme/backend/pull/2"}
	raw, _ := json.Marshal(map[string]any{"worker": worker.EnqueueInput{Title: "Cross-repo change"}, "bridge": bridgeTask{Outcome: Outcome{State: "monitoring_prs", PullRequests: links}}})
	meta := string(raw)
	for _, status := range []string{"running", "awaiting_human", "completed"} {
		got := taskStatus(storage.LoopRecord{Status: status, MetadataJSON: &meta})
		if len(got.PullRequests) != 2 {
			t.Fatalf("%s lost PR links: %+v", status, got)
		}
	}
	for _, link := range links {
		if _, _, err := parsePullRequestURL(link); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := parsePullRequestURL("https://github.com/acme/repo/pull/2suffix"); err == nil {
		t.Fatal("invalid PR URL accepted")
	}
}
