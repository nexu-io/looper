package collab

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/feishubridge"
	"github.com/nexu-io/looper/internal/infra/notify"
	"github.com/nexu-io/looper/internal/storage"
	"github.com/nexu-io/looper/internal/worker"
)

func TestBridgeQueueContinuesAfterTaskDispatchFailure(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("Node.js unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	dir, err := os.MkdirTemp("", "collab-queue-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	db, err := storage.OpenSQLiteCoordinator(ctx, filepath.Join(dir, "state.sqlite"), storage.SQLiteCoordinatorOptions{Migrations: storage.EmbeddedMigrations})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.MigrationRunner().RunPending(ctx, storage.RunPendingOptions{}); err != nil {
		t.Fatal(err)
	}
	h := &bridgeHost{dir: dir, db: db.DB(), repos: storage.NewRepositories(db.DB()), active: map[string]bool{}, notify: notify.NewGateway(notify.Options{})}
	h.cfg.HITL.Feishu = &config.HITLFeishuConfig{OwnerOpenID: "ou_owner"}
	if err := h.repos.Projects.Upsert(ctx, storage.ProjectRecord{ID: "local", Name: "local", RepoPath: dir, CreatedAt: "2026-11-16T00:00:00Z", UpdatedAt: "2026-11-16T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{"om_rejected", "om_accepted"} {
		loop, err := h.enqueue(ctx, worker.EnqueueInput{ProjectID: "local", Title: root, Prompt: "Investigate"})
		if err != nil {
			t.Fatal(err)
		}
		if err := h.repos.FeishuThreads.Upsert(ctx, root, loop.ID, "task", "oc_private", "2026-11-16T00:00:00Z"); err != nil {
			t.Fatal(err)
		}
		// Ensure the rejected task is first, independent of generated IDs.
		updated := "2026-11-17T00:00:00Z"
		if root == "om_accepted" {
			updated = "2026-11-16T00:00:00Z"
		}
		if _, err := h.db.ExecContext(ctx, "UPDATE loops SET updated_at=? WHERE id=?", updated, loop.ID); err != nil {
			t.Fatal(err)
		}
	}
	entry := filepath.Join(dir, "fixture.mjs")
	if err := os.WriteFile(entry, []byte(`import {createServer} from 'node:http';
import {writeFileSync} from 'node:fs';
import {createInterface} from 'node:readline';
createInterface({input:process.stdin}).once('line',line=>{const init=JSON.parse(line);
const server=createServer(async(req,res)=>{if(req.url==='/tasks'){let body='';for await(const chunk of req)body+=chunk;if(JSON.parse(body).rootMessageId==='om_rejected'){res.writeHead(503);res.end('temporary failure');return}writeFileSync(init.cwd+'/accepted.json',body)}res.end('{}')});
server.listen(init.socket);const stop=()=>server.close(()=>process.exit(0));process.stdin.once('end',stop);process.once('SIGTERM',stop)});`), 0600); err != nil {
		t.Fatal(err)
	}
	h.client, err = feishubridge.Start(ctx, entry, dir, feishubridge.Init{Socket: filepath.Join(dir, "bridge.sock"), CWD: dir, AppID: "fixture", AppSecret: "fixture"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer h.client.Close()
	if err := h.dispatchQueued(ctx); err == nil {
		t.Fatal("dispatch failure was hidden")
	}
	if body, err := os.ReadFile(filepath.Join(dir, "accepted.json")); err != nil || !strings.Contains(string(body), "om_accepted") {
		t.Fatalf("another task was blocked: %s, %v", body, err)
	}
	items, err := h.repos.Loops.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, loop := range items {
		input, _, _ := readBridgeTask(loop)
		want := "running"
		if input.Title == "om_rejected" {
			want = "queued"
		}
		if loop.Status != want || h.active[loop.ID] != (want == "running") {
			t.Errorf("task %s: status=%s active=%v", input.Title, loop.Status, h.active[loop.ID])
		}
	}
}
