package collab

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/forge"
	"github.com/nexu-io/looper/internal/storage"
	"github.com/nexu-io/looper/internal/worker"
)

func TestMemoryMCPRegistrationRequiresExplicitOptIn(t *testing.T) {
	h := &bridgeHost{dir: t.TempDir(), sources: []planeSource{{memoryEnabled: false}}}
	if socket, script := h.memoryToolPaths(); socket != "" || script != "" {
		t.Fatal("disabled memory adds a Python MCP process to existing sessions")
	}
	h.sources = append(h.sources, planeSource{memoryEnabled: true})
	if socket, script := h.memoryToolPaths(); socket != SocketPath(h.dir) || script != filepath.Join(h.dir, "workspace", "memory.py") {
		t.Fatal("enabled memory tools not registered")
	}
}

func TestMemoryHostScopesToolsAndRetainsAcknowledgements(t *testing.T) {
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
	for _, p := range cfg.Projects {
		if err := h.repos.Projects.Upsert(ctx, storage.ProjectRecord{ID: p.ID, Name: p.ID, RepoPath: p.RepoPath, CreatedAt: "2026-11-17T00:00:00Z", UpdatedAt: "2026-11-17T00:00:00Z"}); err != nil {
			t.Fatal(err)
		}
	}
	var calls []map[string]any
	unavailable := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Looper-Signature") == "" {
			t.Error("unsigned memory request")
		}
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		calls = append(calls, payload)
		if unavailable {
			http.Error(w, "offline", 503)
			return
		}
		if payload["action"] == "list_for_item" {
			_, _ = w.Write([]byte(`{"indexes":[{"id":"m1","revision":2}],"total":1,"complete":true}`))
		} else {
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer server.Close()
	_, private, _ := ed25519.GenerateKey(rand.Reader)
	der, _ := x509.MarshalPKCS8PrivateKey(private)
	keyPath := filepath.Join(dir, "node.pem")
	if err = os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	env, workspace, project := "MEMORY_TEST_API_KEY", "workspace", "33333333-4444-4555-8666-777777777777"
	t.Setenv(env, "private")
	client, err := forge.NewPlaneClientFromConfig(config.ProviderConfig{ID: "plane", Kind: config.ProviderKindPlane, BaseURL: server.URL + "/api/v1", TokenEnv: &env, Workspace: &workspace, ProjectID: &project, StrictDispatch: &config.PlaneStrictDispatchConfig{Enabled: true, BaseURL: server.URL, BindingID: "77777777-7777-4777-8777-777777777777", NodeID: "node", KeyRevision: 1, PrivateKeyFile: keyPath}}, "acme/repo")
	if err != nil {
		t.Fatal(err)
	}
	h.sources = []planeSource{{project: cfg.Projects[0], client: client, memoryEnabled: true}}
	loop, err := h.enqueue(ctx, worker.EnqueueInput{ProjectID: cfg.Projects[0].ID, Title: "task", Prompt: "goal", StrictDispatchID: "11111111-1111-4111-8111-111111111111"})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := h.memoryCall(ctx, loop, map[string]any{"action": "list_for_item", "dispatch_id": "spoof"})
	if err != nil {
		t.Fatal(err)
	}
	if calls[0]["dispatch_id"] != "11111111-1111-4111-8111-111111111111" {
		t.Fatal("agent selected another dispatch")
	}
	if err = h.recordMemoryIndex(ctx, loop, raw); err != nil {
		t.Fatal(err)
	}
	unavailable = true
	h.syncMemory(ctx)
	var pending int
	_ = h.db.QueryRow(`SELECT count(*) FROM memory_operations WHERE acknowledged=0`).Scan(&pending)
	if pending != 1 {
		t.Fatal("offline observation lost")
	}
	unavailable = false
	h.syncMemory(ctx)
	_ = h.db.QueryRow(`SELECT count(*) FROM memory_operations WHERE acknowledged=0`).Scan(&pending)
	if pending != 0 {
		t.Fatal("acknowledgement lost")
	}
	if err = h.enqueueMemoryReview(ctx, loop, bridgeTask{TrackingRunID: "run-1", Outcome: Outcome{State: "completed", Summary: "done"}}); err != nil {
		t.Fatal(err)
	}
	// Stored state, not an arbitrary caller-provided run, owns the operation's correlation.
	if err = h.queueMemory(ctx, loop, map[string]any{"action": "read_completed", "call_id": "stable", "memory_id": "m1", "revision": 2}); err != nil {
		t.Fatal(err)
	}
	if err = h.queueMemory(ctx, loop, map[string]any{"action": "read_completed", "call_id": "stable", "memory_id": "m1", "revision": 2}); err != nil {
		t.Fatal(err)
	}
	_ = h.db.QueryRow(`SELECT count(*) FROM memory_operations WHERE id='stable'`).Scan(&pending)
	if pending != 1 {
		t.Fatal("replay duplicated observation")
	}
	var reviewCall string
	if err = h.db.QueryRow(`SELECT id FROM memory_operations WHERE json_extract(payload,'$.action')='review_enqueue'`).Scan(&reviewCall); err != nil {
		t.Fatal(err)
	}
	if len(reviewCall) > 128 {
		t.Fatalf("review id exceeds signed API limit: %d", len(reviewCall))
	}
}

func TestMemoryMCPStdioScopesCalls(t *testing.T) {
	ctx := context.Background()
	dir, err := os.MkdirTemp("/tmp", "looper-memory-mcp-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	script, err := memoryAssets.ReadFile("memory/tool.py")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "memory.py")
	if err = os.WriteFile(path, script, 0600); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "control.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	server := http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/memory/task-1" {
			t.Error("task scope changed")
		}
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		if payload["action"] != "get" {
			t.Error("unexpected action")
		}
		_, _ = w.Write([]byte(`{"content":"published original","revision":2}`))
	})}
	defer server.Close()
	go server.Serve(listener)
	cmd := exec.CommandContext(ctx, "python3", path, "--socket", socket, "--task", "task-1", "--mcp")
	cmd.Stdin = strings.NewReader("{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\",\"params\":{\"protocolVersion\":\"2024-11-05\"}}\n{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"tools/list\"}\n{\"jsonrpc\":\"2.0\",\"id\":3,\"method\":\"tools/call\",\"params\":{\"name\":\"get\",\"arguments\":{\"memory_id\":\"m\",\"revision\":2}}}\n{\"jsonrpc\":\"2.0\",\"id\":4,\"method\":\"tools/call\",\"params\":{\"name\":\"publish\",\"arguments\":{}}}\n")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || !strings.Contains(string(out), "published original") || !strings.Contains(string(out), "unknown memory tool") {
		t.Fatalf("unsafe or incomplete MCP response: %s", out)
	}
}
