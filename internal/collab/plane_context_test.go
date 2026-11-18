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
	"reflect"
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
	dispatch := planestrict.Dispatch{ID: "33333333-3333-4333-8333-333333333333", Revision: 1, StateVersion: 2, State: "running", Health: "ok", RequestedMode: "collab", ActiveRole: "worker", ExecutionAttemptID: &attempt, FencingToken: 1, TaskContext: planestrict.TaskContext{Summary: "范围已确认", Context: "不改存储协议"}}
	if err = h.repos.Projects.Upsert(ctx, storage.ProjectRecord{ID: cfg.Projects[0].ID, Name: "local", RepoPath: dir, CreatedAt: "2026-11-17T00:00:00Z", UpdatedAt: "2026-11-17T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	loop, err := h.enqueue(ctx, worker.EnqueueInput{ProjectID: cfg.Projects[0].ID, StrictDispatchID: dispatch.ID, Title: "跨仓库调查", Prompt: "只读检查两个仓库"})
	if err != nil {
		t.Fatal(err)
	}
	if err = h.repos.Loops.AssignFeishuChat(ctx, loop.ID, "oc_existing", "plane:project:module:module-1"); err != nil {
		t.Fatal(err)
	}
	savedLoop, err := h.repos.Loops.GetByID(ctx, loop.ID)
	if err != nil {
		t.Fatal(err)
	}
	loop = *savedLoop
	_, state, _ := readBridgeTask(loop)
	state.RootID = "om_root"
	state.ThreadID = "omt_thread"
	previousLinks := []planestrict.TaskLink{
		{Title: "HTML 附件 v1", URL: "https://feishu.example.com/docx/fixture-v1", Type: "artifact"},
		{Title: "验收指南 v1", URL: "https://plane.example.com/fixture-v1/guide", Type: "acceptance_guide"},
		{Title: "检测报告 v1", URL: "https://plane.example.com/fixture-v1/verification", Type: "verification_report"},
		{Title: "实测截图 v1", URL: "https://plane.example.com/fixture-v1/screenshot", Type: "artifact"},
		{Title: "PR v1", URL: "https://github.com/example/repo/pull/1#fixture-v1", Type: "pull_request"},
	}
	currentLinks := make([]planestrict.TaskLink, len(previousLinks))
	for i, link := range previousLinks {
		currentLinks[i] = planestrict.TaskLink{Title: strings.ReplaceAll(link.Title, "v1", "v2"), URL: strings.ReplaceAll(link.URL, "v1", "v2"), Type: link.Type}
	}
	state.Outcome = Outcome{State: "needs_input", Summary: "v1 已交付，待验收", Links: previousLinks,
		Events: []planestrict.TaskEvent{{ID: "delivery:fixture-v1", Title: "交付 fixture-v1，待验收", Kind: "delivery", Body: "fixture-v1: actual checks passed; human acceptance pending", Links: previousLinks}}}
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
	outcome := Outcome{State: "completed", Summary: "v2 检查完成，人工验收未签署", Context: "结论：不改存储协议", Links: currentLinks,
		Events: []planestrict.TaskEvent{
			{ID: "delivery:fixture-v2", Title: "交付 fixture-v2，待验收", Kind: "delivery", Body: "fixture-v2: actual checks passed; human acceptance pending", Links: currentLinks},
			{ID: "reply-1", Title: "选择简短版", Kind: "decision", ActorName: "测试同事", ActorOpenID: "ou_fixture", SourceURL: "https://applink.feishu.cn/client/thread/open?fixture=1"},
		}}
	outcome.Links = append(outcome.Links, planestrict.TaskLink{Title: "Local file action", URL: "/tmp/report.md", Type: "artifact"})
	outcome.Events[0].Links = outcome.Links
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
			rows := []planestrict.Dispatch{}
			if (dispatch.State != "completed" && dispatch.State != "failed") || r.URL.Query().Get("context_dispatch_id") == dispatch.ID {
				rows = append(rows, dispatch)
			}
			_ = json.NewEncoder(w).Encode(planestrict.InboxResponse{Dispatches: rows})
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
			body.Context.Events = nil // Plane stores events separately from the summary.
			_ = json.NewEncoder(w).Encode(map[string]any{"context": body.Context})
		case strings.HasSuffix(r.URL.Path, "/transition/"):
			var body struct {
				State string `json:"state"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			dispatch.State, dispatch.StateVersion = body.State, dispatch.StateVersion+1
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
	snapshotPath := filepath.Join(dir, "workspace", "tasks", loop.ID+".plane.json")
	readContext := func() planestrict.TaskContext {
		t.Helper()
		data, err := os.ReadFile(snapshotPath)
		if err != nil {
			t.Fatal(err)
		}
		var snapshot struct {
			Context planestrict.TaskContext `json:"context"`
		}
		if err := json.Unmarshal(data, &snapshot); err != nil {
			t.Fatal(err)
		}
		return snapshot.Context
	}
	if local := readContext(); local.Summary != dispatch.TaskContext.Summary {
		t.Fatalf("failed publish replaced confirmed context: %+v", local)
	}
	saved, _ := h.repos.Loops.GetByID(ctx, loop.ID)
	_, state, _ = readBridgeTask(*saved)
	if saved.Status != "completed" || len(state.Outcome.Events) != 3 {
		t.Fatalf("local execution or pending facts lost: %+v", state)
	}
	// A reconstructed host uses exactly the same durable outcome.
	h = &bridgeHost{dir: dir, db: db.DB(), repos: storage.NewRepositories(db.DB()), active: map[string]bool{}}
	fail = false
	if err = h.planeDispatch(ctx, sources[0].client, "local", dispatch); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || transitions != 1 || got.Context != outcome.Context || len(got.Events) != 3 {
		t.Fatalf("retry did not publish task data: %+v", got)
	}
	if !reflect.DeepEqual(got.Links, currentLinks) {
		t.Fatalf("current entry links lost or kept an old version: %+v", got.Links)
	}
	for i, want := range []struct {
		id    string
		links []planestrict.TaskLink
	}{{"delivery:fixture-v1", previousLinks}, {"delivery:fixture-v2", currentLinks}} {
		event := got.Events[i]
		if event.ID != want.id || event.Kind != "delivery" || event.Body == "" || !reflect.DeepEqual(event.Links, want.links) {
			t.Fatalf("delivery version or full artifact links lost after failed writeback and restart: %+v", event)
		}
	}
	if got.ChatURL != "https://applink.feishu.cn/client/chat/open?openChatId=oc_existing" || got.ChatScopeKey != "plane:project:module:module-1" || got.ThreadURL == "" {
		t.Fatalf("durable group or thread destination missing: %+v", got)
	}
	if got.Events[2].ActorOpenID != "ou_fixture" || got.Events[2].SourceURL == "" {
		t.Fatal("human provenance lost")
	}
	if len(got.Links) != len(currentLinks) || len(got.Events[1].Links) != len(currentLinks) || got.Links[0].URL != outcome.Links[0].URL {
		t.Fatal("local file actions were exported as Plane web links")
	}
	saved, _ = h.repos.Loops.GetByID(ctx, loop.ID)
	_, state, _ = readBridgeTask(*saved)
	if len(state.Outcome.Events) != 1 || state.Outcome.Events[0].ID != "late-1" {
		t.Fatalf("concurrent reply lost or ack failed: %+v", state.Outcome.Events)
	}
	if local := readContext(); local.Summary != outcome.Summary || local.Context != outcome.Context || !reflect.DeepEqual(local.Links, currentLinks) || len(local.Events) != 0 {
		t.Fatalf("recovery file differs from confirmed Plane context: %+v", local)
	}

	// A later reply is durable, but the completed dispatch is no longer in the
	// ordinary inbox. Rediscover only pending context, without claiming again.
	dispatch.State, dispatch.StateVersion = "completed", dispatch.StateVersion+1
	h.sources = sources
	if err := h.pollPlane(ctx); err != nil {
		t.Fatal(err)
	}
	if calls != 3 || transitions != 1 || len(got.Events) != 1 || got.Events[0].ID != "late-1" {
		t.Fatalf("completed reply was not published independently: calls=%d transitions=%d context=%+v", calls, transitions, got)
	}
	if err := h.pollPlane(ctx); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatal("acknowledged completed task kept polling context")
	}
	if local := readContext(); local.Summary != outcome.Summary || local.Context != outcome.Context {
		t.Fatalf("completed dispatch left a stale recovery file: %+v", local)
	}

	// A native session can resume after a failed turn. Keep its original
	// dispatch/attempt instead of leaving Plane failed while it is running.
	dispatch.State, dispatch.StateVersion = "failed", dispatch.StateVersion+1
	if err := h.pollPlane(ctx); err != nil {
		t.Fatal(err)
	}
	if transitions != 1 {
		t.Fatal("failed dispatch reopened without a native continuation")
	}
	callback("started")
	if err := h.pollPlane(ctx); err != nil {
		t.Fatal(err)
	}
	if transitions != 2 || dispatch.State != "running" {
		t.Fatalf("resumed native execution did not reopen its failed dispatch: transitions=%d", transitions)
	}
}
