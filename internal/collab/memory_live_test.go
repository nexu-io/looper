package collab

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/feishubridge"
	"github.com/nexu-io/looper/internal/storage"
	"github.com/nexu-io/looper/internal/worker"
)

// Opt-in acceptance against the caller's isolated Plane and a real tool-using Agent.
// Never starts a Feishu consumer or sends a message.
func TestMemoryRealPlaneAgent(t *testing.T) {
	path := os.Getenv("LOOPER_TEST_MEMORY_CONFIG")
	if path == "" {
		t.Skip("explicit local Plane + model acceptance only")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg config.Config
	if err = json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dir, err := os.MkdirTemp("/tmp", "looper-memory-live-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
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
		if err = h.repos.Projects.Upsert(ctx, storage.ProjectRecord{ID: p.ID, Name: p.ID, RepoPath: p.RepoPath, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano)}); err != nil {
			t.Fatal(err)
		}
	}
	if h.sources, err = bridgePlaneSources(cfg); err != nil {
		t.Fatal(err)
	}
	h.legacySources = append([]planeSource(nil), h.sources...)
	if err = h.writeWorkspace(filepath.Join(dir, "workspace")); err != nil {
		t.Fatal(err)
	}
	loop, err := h.enqueue(ctx, worker.EnqueueInput{ProjectID: cfg.Projects[0].ID, Title: "Memory acceptance", Prompt: "Read original memory and report its purpose", StrictDispatchID: os.Getenv("LOOPER_TEST_MEMORY_DISPATCH")})
	if err != nil {
		t.Fatal(err)
	}
	_, state, _ := readBridgeTask(loop)
	state.RootID = "local-memory-acceptance"
	if err = h.patch(ctx, loop.ID, "running", state, ""); err != nil {
		t.Fatal(err)
	}
	event := feishubridge.TurnEvent{TurnID: "real-model-acceptance", RootMessageID: state.RootID, Phase: "started"}
	if _, err = h.recordTurnEvent(ctx, event); err != nil {
		t.Fatal(err)
	}
	catalog := h.memoryContext(ctx, loop.ID)
	listener, err := net.Listen("unix", SocketPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	_ = os.Chmod(SocketPath(dir), 0600)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /memory/", h.memoryTool)
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	defer server.Close()
	go server.Serve(listener)
	prompt := "这是本地记忆系统验收，不发飞书消息、不修改业务代码、不提交 PR。请从下面目录自主找到跨仓任务相关记忆，用给定 memory 工具取得原文，说明它是不是公司正式规范，再通过 feedback 报告采用用途。工具只用 get、feedback、必要的 list_for_item。完成后返回 JSON {state:completed,summary:实际结论,pullRequests:[],repositories:[]}。\n" + catalog
	out := h.resultPath(loop.ID)
	schema := filepath.Join(dir, "outcome-schema.json")
	if err = os.WriteFile(schema, []byte(`{"type":"object","properties":{"state":{"type":"string","enum":["completed"]},"summary":{"type":"string"},"pullRequests":{"type":"array","items":{"type":"string"}},"repositories":{"type":"array","items":{"type":"object","properties":{},"additionalProperties":false}}},"required":["state","summary","pullRequests","repositories"],"additionalProperties":false}`), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, "codex", "exec", "--ephemeral", "--ignore-user-config", "--skip-git-repo-check", "--sandbox", "read-only", "--disable", "plugins", "-C", filepath.Join(dir, "workspace"), "--output-schema", schema, "--output-last-message", out, "-")
	cmd.Args = append(cmd.Args[:len(cmd.Args)-1], "-c", "mcp_servers.looper_memory.command=\"python3\"", "-c", "mcp_servers.looper_memory.args="+string(mustJSON(t, []string{filepath.Join(dir, "workspace", "memory.py"), "--socket", SocketPath(dir), "--task", loop.ID, "--mcp"})), "-")
	for _, name := range []string{"list_for_item", "get", "feedback", "propose"} {
		cmd.Args = append(cmd.Args[:len(cmd.Args)-1], "-c", "mcp_servers.looper_memory.tools."+name+".approval_mode=\"approve\"", "-")
	}
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "TMPDIR=" + os.Getenv("TMPDIR")}
	cmd.Stdin = strings.NewReader(prompt)
	result, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("real Agent failed: %v (%s)", err, result)
	}
	event.Phase, event.Terminal = "finished", "done"
	if _, err = h.recordTurnEvent(ctx, event); err != nil {
		t.Fatal(err)
	}
	h.syncMemory(ctx)
	var reads, pending int
	if err = h.db.QueryRow(`SELECT count(*) FROM memory_operations WHERE json_extract(payload,'$.action')='read_completed' AND acknowledged=1`).Scan(&reads); err != nil {
		t.Fatal(err)
	}
	if reads < 1 {
		t.Fatalf("Agent did not successfully retrieve a memory original: %s", result)
	}
	_ = h.db.QueryRow(`SELECT count(*) FROM memory_operations WHERE acknowledged=0`).Scan(&pending)
	if pending != 0 {
		var action, message string
		_ = h.db.QueryRow(`SELECT json_extract(payload,'$.action'),error FROM memory_operations WHERE acknowledged=0 LIMIT 1`).Scan(&action, &message)
		t.Fatalf("observations not acknowledged (%s): %s", action, message)
	}
	// Exercise the production independent-review queue and candidate persistence too.
	h.runMemoryReview(ctx)
	h.syncMemory(ctx)
	t.Logf("Real Plane + Feishu snapshot + tool-using Agent: %d acknowledged reads; independent review attempted", reads)
	if dest := os.Getenv("LOOPER_TEST_MEMORY_EVIDENCE"); dest != "" {
		if err = os.WriteFile(dest, result, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
