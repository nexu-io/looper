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

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/feishubridge"
	"github.com/nexu-io/looper/internal/planestrict"
	"github.com/nexu-io/looper/internal/storage"
	"github.com/nexu-io/looper/internal/worker"
)

// SQLite + actual signed HTTP client; a failed writeback must remain retryable,
// even if the next native turn completes before Plane comes back.
func TestPlaneContextRetriesBeforeTerminalAndPreservesConcurrentReply(t *testing.T) {
	ctx := context.Background()
	cfg, dir := hostFixture(t, "#!/bin/sh\nexit 1\n")
	db, err := storage.OpenSQLiteCoordinator(ctx, filepath.Join(dir, "state.sqlite"), storage.SQLiteCoordinatorOptions{Migrations: storage.EmbeddedMigrations})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.MigrationRunner().RunPending(ctx, storage.RunPendingOptions{}); err != nil {
		t.Fatal(err)
	}
	h := &bridgeHost{dir: dir, db: db.DB(), repos: storage.NewRepositories(db.DB()), active: map[string]bool{}, cfg: cfg}
	if err = h.writeWorkspace(filepath.Join(dir, "workspace")); err != nil {
		t.Fatal(err)
	}
	attempt := "66666666-6666-4666-8666-666666666666"
	dispatch := planestrict.Dispatch{ID: "33333333-3333-4333-8333-333333333333", Revision: 1, StateVersion: 2, State: "running", Health: "ok", ExecutionAttemptID: &attempt, FencingToken: 1, TaskContext: planestrict.TaskContext{Summary: "范围已确认", Context: "不改存储协议"}}
	if err = h.repos.Projects.Upsert(ctx, storage.ProjectRecord{ID: cfg.Projects[0].ID, Name: "local", RepoPath: dir, CreatedAt: "2026-11-17T00:00:00Z", UpdatedAt: "2026-11-17T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	loop, err := h.enqueue(ctx, worker.EnqueueInput{ProjectID: cfg.Projects[0].ID, StrictDispatchID: dispatch.ID, Title: "跨仓库调查", Prompt: "只读检查两个仓库"})
	if err != nil {
		t.Fatal(err)
	}
	_, state, _ := readBridgeTask(loop)
	state.RootID = "om_root"
	state.ThreadID = "omt_thread"
	state.Outcome = Outcome{State: "needs_input", Summary: "已完成影响分析", Events: []planestrict.TaskEvent{{ID: "finding-1", Title: "影响范围已检查"}}}
	if err = h.patch(ctx, loop.ID, "awaiting_human", state, state.Outcome.Summary); err != nil {
		t.Fatal(err)
	}
	callback := func(phase string) {
		t.Helper()
		if _, err := h.recordTurnEvent(ctx, feishubridge.TurnEvent{Phase: phase, ThreadID: state.ThreadID, Terminal: "done"}); err != nil {
			t.Fatal(err)
		}
	}
	callback("started")
	outcome := Outcome{State: "completed", Summary: "调查已完成", Context: "结论：不改存储协议", Events: []planestrict.TaskEvent{{ID: "reply-1", Title: "选择简短版", Kind: "decision", ActorName: "测试同事", ActorOpenID: "ou_fixture", SourceURL: "https://applink.feishu.cn/client/thread/open?fixture=1"}}}
	data, _ := json.Marshal(outcome)
	if err = os.WriteFile(h.resultPath(loop.ID), data, 0600); err != nil {
		t.Fatal(err)
	}
	callback("finished")
	fail := true
	transitions := 0
	calls := 0
	var got planestrict.TaskContext
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Looper-Signature") == "" {
			t.Error("unsigned context write")
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/nodes/session/"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			_ = json.NewEncoder(w).Encode(map[string]any{"session_id": body["session_id"], "state": "active"})
		case strings.HasSuffix(r.URL.Path, "/dispatch/inbox/"):
			_ = json.NewEncoder(w).Encode(planestrict.InboxResponse{Dispatches: []planestrict.Dispatch{dispatch}})
		case strings.HasSuffix(r.URL.Path, "/context/"):
			calls++
			var body struct {
				Context planestrict.TaskContext `json:"context"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			got = body.Context
			if fail {
				http.Error(w, "temporary failure", 503)
				return
			}
			// A new fact arriving while the request is in flight must survive its ack.
			current, _ := h.repos.Loops.GetByID(ctx, loop.ID)
			_, s, _ := readBridgeTask(*current)
			s.Outcome.Events = append(s.Outcome.Events, planestrict.TaskEvent{ID: "late-1", Title: "补充发现"})
			if err := h.patch(ctx, loop.ID, current.Status, s, outcome.Summary); err != nil {
				t.Error(err)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`))
		case strings.HasSuffix(r.URL.Path, "/transition/"):
			transitions++
			_ = json.NewEncoder(w).Encode(planestrict.MutationResponse{Dispatch: dispatch})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	keyPath := filepath.Join(dir, "device.pem")
	if err = os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	workspace, project, token := "fixture", "44444444-4444-4444-8444-444444444444", "CONTEXT_TEST_TOKEN"
	t.Setenv(token, "fixture")
	cfg.Providers = []config.ProviderConfig{{ID: "plane", Kind: config.ProviderKindPlane, BaseURL: server.URL + "/api/v1", Workspace: &workspace, ProjectID: &project, TokenEnv: &token, StrictDispatch: &config.PlaneStrictDispatchConfig{Enabled: true, BaseURL: server.URL, NodeID: "node", BindingID: "55555555-5555-4555-8555-555555555555", KeyRevision: 1, PrivateKeyFile: keyPath}}}
	cfg.Projects[0].Provider = "plane"
	sources, err := planeSources(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = h.planeDispatch(ctx, sources[0].client, "local", dispatch); err == nil {
		t.Fatal("writeback failure was swallowed")
	}
	if transitions != 0 {
		t.Fatal("terminal state was published before context")
	}
	saved, _ := h.repos.Loops.GetByID(ctx, loop.ID)
	_, state, _ = readBridgeTask(*saved)
	if saved.Status != "completed" || len(state.Outcome.Events) != 2 {
		t.Fatalf("local execution or pending facts lost: %+v", state)
	}
	// A reconstructed host uses exactly the same durable outcome.
	h = &bridgeHost{dir: dir, db: db.DB(), repos: storage.NewRepositories(db.DB()), active: map[string]bool{}}
	fail = false
	if err = h.planeDispatch(ctx, sources[0].client, "local", dispatch); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || transitions != 1 || got.Context != outcome.Context || len(got.Events) < 2 {
		t.Fatalf("retry did not publish task data: %+v", got)
	}
	if got.Events[1].ActorOpenID != "ou_fixture" || got.Events[1].SourceURL == "" {
		t.Fatal("human provenance lost")
	}
	saved, _ = h.repos.Loops.GetByID(ctx, loop.ID)
	_, state, _ = readBridgeTask(*saved)
	if len(state.Outcome.Events) != 1 || state.Outcome.Events[0].ID != "late-1" {
		t.Fatalf("concurrent reply lost or ack failed: %+v", state.Outcome.Events)
	}
	snapshot, err := os.ReadFile(filepath.Join(dir, "workspace", "tasks", loop.ID+".plane.json"))
	if err != nil || !strings.Contains(string(snapshot), dispatch.TaskContext.Context) {
		t.Fatalf("recovery context missing: %s %v", snapshot, err)
	}
}
