package collab

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/eventlog"
	"github.com/nexu-io/looper/internal/executionhost"
	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/planestrict"
	"github.com/nexu-io/looper/internal/storage"
	"github.com/nexu-io/looper/internal/worker"
)

// Real signed client, SQLite and queue; only the remote Plane API is a fixture.
// Keep wrong-repo and broken dispatches ahead of the good one to detect both
// accidental claims and starvation after a single failed item.
func TestPlaneIngestionFiltersBeforeClaimAndReplaysOriginalTask(t *testing.T) {
	ctx := context.Background()
	wrong := planestrict.Dispatch{ID: "11111111-1111-4111-8111-111111111111", RequestedMode: "collab", ActiveRole: "worker", CodeRepo: "acme/other", State: "queued", Health: "ok"}
	broken := wrong
	broken.ID, broken.IssueID, broken.CodeRepo = "22222222-2222-4222-8222-222222222222", "broken", "acme/local"
	good := broken
	good.CollaborationScope = &planestrict.CollaborationScope{Key: "plane:project:module:motion", Name: "Motion"}
	good.ID, good.IssueID, good.Revision, good.StateVersion = "33333333-3333-4333-8333-333333333333", "good", 1, 1
	claims := 0
	var transitions []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/looper/") && r.Header.Get("Looper-Signature") == "" {
			t.Error("unsigned dispatch request")
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/nodes/session/"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			_ = json.NewEncoder(w).Encode(map[string]any{"session_id": body["session_id"], "state": "active"})
		case strings.HasSuffix(r.URL.Path, "/dispatch/inbox/"):
			_ = json.NewEncoder(w).Encode(planestrict.InboxResponse{Dispatches: []planestrict.Dispatch{wrong, broken, good}})
		case strings.HasSuffix(r.URL.Path, "/work-items/broken/"):
			http.Error(w, "unavailable item", 503)
		case strings.HasSuffix(r.URL.Path, "/work-items/good/"):
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "good", "name": "Fix local fixture", "sequence_id": 99, "description_html": "<p>Inspect impact before changing code</p>"})
		case r.URL.Path == "/api/v1/workspaces/fixture/projects/44444444-4444-4444-8444-444444444444/":
			_ = json.NewEncoder(w).Encode(map[string]any{"identifier": "FIX"})
		case strings.HasSuffix(r.URL.Path, "/labels/"):
			_ = json.NewEncoder(w).Encode(map[string]any{"results": []any{}})
		case strings.HasSuffix(r.URL.Path, "/"+good.ID+"/claim/"):
			claims++
			good.State = "claimed"
			_ = json.NewEncoder(w).Encode(planestrict.MutationResponse{Dispatch: good, Claimed: true})
		case strings.HasSuffix(r.URL.Path, "/"+good.ID+"/transition/"):
			var body struct {
				State string `json:"state"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			good.State = body.State
			transitions = append(transitions, body.State)
			_ = json.NewEncoder(w).Encode(planestrict.MutationResponse{Dispatch: good})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	cfg, dir := hostFixture(t, "#!/bin/sh\nexit 1\n")
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, "device.pem")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	workspace, project, tokenEnv := "fixture", "44444444-4444-4444-8444-444444444444", "COLLAB_PLANE_TEST_TOKEN"
	t.Setenv(tokenEnv, "fixture-only")
	cfg.Projects[0].Provider = "plane"
	cfg.Providers = []config.ProviderConfig{{ID: "plane", Kind: config.ProviderKindPlane, BaseURL: server.URL + "/api/v1", Workspace: &workspace, ProjectID: &project, TokenEnv: &tokenEnv, StrictDispatch: &config.PlaneStrictDispatchConfig{Enabled: true, BaseURL: server.URL, NodeID: "node", BindingID: "55555555-5555-4555-8555-555555555555", KeyRevision: 1, PrivateKeyFile: keyPath}}}
	sources, err := planeSources(cfg)
	if err != nil {
		t.Fatal(err)
	}
	db, err := storage.OpenSQLiteCoordinator(ctx, filepath.Join(dir, "looper.sqlite"), storage.SQLiteCoordinatorOptions{Migrations: storage.EmbeddedMigrations})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.MigrationRunner().RunPending(ctx, storage.RunPendingOptions{}); err != nil {
		t.Fatal(err)
	}
	repos := storage.NewRepositories(db.DB())
	now := eventlog.FormatJavaScriptISOString(time.Now())
	metadata := `{"repo":"acme/local"}`
	if err := repos.Projects.Upsert(ctx, storage.ProjectRecord{ID: "local", Name: "local", RepoPath: cfg.Projects[0].RepoPath, MetadataJSON: &metadata, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	runner := worker.New(worker.Options{DB: db.DB(), Repos: repos})
	service := loops.Service{DB: db.DB(), Repos: repos}
	owner := executionhost.NewActiveExecutionRegistry()
	for pass := 0; pass < 3; pass++ {
		if err := sources[0].poll(ctx, runner, &service, repos, owner); err == nil || !strings.Contains(err.Error(), broken.ID) {
			t.Fatalf("broken dispatch should be reported without starving other work: %v", err)
		}
		items, err := repos.Loops.List(ctx)
		if err != nil || len(items) != 1 {
			t.Fatalf("tasks: %+v %v", items, err)
		}
		if items[0].ID != "loop_plane_"+good.ID {
			t.Fatalf("wrong dispatch ingested: %+v", items[0])
		}
	}
	queue, err := repos.Queue.List(ctx)
	if err != nil || len(queue) != 1 || claims != 1 {
		t.Fatalf("replay created another claim or queue item: %d %+v %v", claims, queue, err)
	}
	if queue[0].PayloadJSON == nil || !strings.Contains(*queue[0].PayloadJSON, good.CollaborationScope.Key) {
		t.Fatal("Plane collaboration scope was lost before enqueue")
	}
	if queue[0].PayloadJSON == nil || !strings.Contains(*queue[0].PayloadJSON, good.ID) {
		t.Fatal("dispatch identity missing from original queue")
	}
	loop, err := repos.Loops.GetByID(ctx, "loop_plane_"+good.ID)
	if err != nil || loop == nil {
		t.Fatalf("original task: %v %v", loop, err)
	}
	attempt := "66666666-6666-4666-8666-666666666666"
	good.ExecutionAttemptID, good.FencingToken, good.State = &attempt, 1, "running"
	loop.Status = "running"
	target := "acme/local#409"
	loop.TargetType, loop.TargetID = "pull_request", &target
	metadata, err = loops.WriteShepherd(loop.MetadataJSON, loops.Shepherd{Active: true, Phase: "awaiting_merge"})
	if err != nil {
		t.Fatal(err)
	}
	loop.MetadataJSON = &metadata
	if err := repos.Loops.Upsert(ctx, *loop); err != nil {
		t.Fatal(err)
	}
	if err := sources[0].syncTask(ctx, repos, owner, *loop, good); err != nil {
		t.Fatal(err)
	}
	if len(transitions) != 0 {
		t.Fatal("PR waiting for merge was completed early")
	}

	loop.Status = "awaiting_human"
	metadata, err = loops.WriteHITLAsk(loop.MetadataJSON, loops.HITLAsk{Question: "Approve fixture?", Transport: "feishu", Status: "awaiting", QuestionMessageID: "question", SessionID: "original-session"})
	if err != nil {
		t.Fatal(err)
	}
	loop.MetadataJSON = &metadata
	if err := repos.Loops.Upsert(ctx, *loop); err != nil {
		t.Fatal(err)
	}
	queue[0].Status = "cancelled"
	if err := repos.Queue.Upsert(ctx, queue[0]); err != nil {
		t.Fatal(err)
	}
	if err := sources[0].syncTask(ctx, repos, owner, *loop, good); err != nil {
		t.Fatal(err)
	}
	if good.State != "awaiting_human" {
		t.Fatalf("HITL not synced: %s", good.State)
	}
	if err := loops.DeliverHITLAnswerForQuestion(ctx, repos, now, loop.ID, "1", "question"); err != nil {
		t.Fatal(err)
	}
	resumed, err := repos.Queue.GetByID(ctx, queue[0].ID)
	if err != nil || resumed.Status != "queued" || resumed.PayloadJSON == nil || !strings.Contains(*resumed.PayloadJSON, good.ID) {
		t.Fatalf("HITL lost dispatch or claim: %+v %v", resumed, err)
	}
	loop, err = repos.Loops.GetByID(ctx, loop.ID)
	if err != nil {
		t.Fatal(err)
	}
	ask, ok := loops.ReadHITLAsk(loop.MetadataJSON)
	if !ok || ask.SessionID != "original-session" || ask.Answer != "1" {
		t.Fatalf("HITL replaced the original task conversation: %+v", ask)
	}

	loop.Status = "completed"
	metadata, err = loops.WriteShepherd(loop.MetadataJSON, loops.Shepherd{Outcome: "merged"})
	if err != nil {
		t.Fatal(err)
	}
	loop.MetadataJSON = &metadata
	if err := repos.Loops.Upsert(ctx, *loop); err != nil {
		t.Fatal(err)
	}
	replay, err := runner.Enqueue(ctx, worker.EnqueueInput{ProjectID: "local", Repo: "acme/local", IssueNumber: 99, StrictDispatchID: good.ID})
	if err != nil || replay.Status != "completed" || replay.TargetType != "pull_request" {
		t.Fatalf("terminal replay reran or retargeted original task: %+v %v", replay, err)
	}
	if err := sources[0].syncTask(ctx, repos, owner, *loop, good); err != nil {
		t.Fatal(err)
	}
	if good.State != "completed" {
		t.Fatalf("merged task not completed: %s", good.State)
	}
	metadata, err = loops.WriteShepherd(loop.MetadataJSON, loops.Shepherd{Outcome: "abandoned"})
	if err != nil {
		t.Fatal(err)
	}
	loop.MetadataJSON, good.State = &metadata, "running"
	if err := sources[0].syncTask(ctx, repos, owner, *loop, good); err != nil {
		t.Fatal(err)
	}
	if good.State != "failed" {
		t.Fatal("closed unmerged PR was reported as successful")
	}
	if err := repos.AgentExecutions.Upsert(ctx, storage.AgentExecutionRecord{ID: "uncertain", LoopID: &loop.ID, Vendor: "codex", Status: "running", StartedAt: now, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	good.State = "running"
	if err := sources[0].syncTask(ctx, repos, owner, *loop, good); err != nil {
		t.Fatal(err)
	}
	if good.State != "running" {
		t.Fatal("terminal sync ignored uncertain execution evidence")
	}
	if err := stopPlaneTask(ctx, &service, repos, owner, *loop); err == nil {
		t.Fatal("terminal task with uncertain evidence acknowledged a stop")
	}
	queue, err = repos.Queue.List(ctx)
	if err != nil || len(queue) != 1 {
		t.Fatalf("replay created another execution: %+v %v", queue, err)
	}

	// A late reply cannot reactivate a task stopped from Plane.
	stopped, err := runner.Enqueue(ctx, worker.EnqueueInput{ProjectID: "local", Repo: "acme/local", IssueNumber: 100})
	if err != nil {
		t.Fatal(err)
	}
	stopped.Status = "awaiting_human"
	metadata, err = loops.WriteHITLAsk(stopped.MetadataJSON, loops.HITLAsk{Question: "Choose?", Transport: "feishu", Status: "awaiting", QuestionMessageID: "late-question"})
	if err != nil {
		t.Fatal(err)
	}
	stopped.MetadataJSON = &metadata
	if err := repos.Loops.Upsert(ctx, stopped); err != nil {
		t.Fatal(err)
	}
	if err := stopPlaneTask(ctx, &service, repos, owner, stopped); err != nil {
		t.Fatal(err)
	}
	if err := loops.DeliverHITLAnswerForQuestion(ctx, repos, now, stopped.ID, "1", "late-question"); err != nil {
		t.Fatal(err)
	}
	final, err := repos.Loops.GetByID(ctx, stopped.ID)
	if err != nil || final.Status != "terminated" || !owner.LoopStopActive(stopped.ID) {
		t.Fatalf("late answer reopened a stopped task: %+v %v", final, err)
	}
	stoppedQueue, err := repos.Queue.GetLatestByLoopID(ctx, stopped.ID)
	if err != nil || stoppedQueue.Status != "cancelled" {
		t.Fatalf("late answer requeued a stopped task: %+v %v", stoppedQueue, err)
	}

}
