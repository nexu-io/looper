package collab

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/feishubridge"
	"github.com/nexu-io/looper/internal/storage"
	"github.com/nexu-io/looper/internal/worker"
)

func inboxTestHost(t *testing.T, name string) *bridgeHost {
	t.Helper()
	dir, err := os.MkdirTemp("", "inbox-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	ctx := context.Background()
	db, err := storage.OpenSQLiteCoordinator(ctx, filepath.Join(dir, "looper.sqlite"), storage.SQLiteCoordinatorOptions{Migrations: storage.EmbeddedMigrations})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.MigrationRunner().RunPending(ctx, storage.RunPendingOptions{}); err != nil {
		t.Fatal(err)
	}
	h := &bridgeHost{dir: dir, db: db.DB(), repos: storage.NewRepositories(db.DB()), active: map[string]bool{}}
	h.cfg.Projects = []config.ProjectRefConfig{{ID: "local", RepoPath: dir}}
	if err := h.repos.Projects.Upsert(ctx, storage.ProjectRecord{ID: "local", Name: "local", RepoPath: dir, CreatedAt: "2026-11-17T00:00:00Z", UpdatedAt: "2026-11-17T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	loop, err := h.enqueue(ctx, worker.EnqueueInput{ProjectID: "local", Title: name, Prompt: "read-only fixture"})
	if err != nil {
		t.Fatal(err)
	}
	state := bridgeTask{RootID: "om_" + name, ThreadID: "omt_" + name, SessionID: "native_" + name}
	if err := h.patch(ctx, loop.ID, "awaiting_human", state, ""); err != nil {
		t.Fatal(err)
	}
	if err := h.repos.FeishuThreads.Upsert(ctx, state.RootID, loop.ID, "", "oc_shared", "2026-11-17T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	// A real child process and private Unix HTTP boundary, without Feishu/LLM calls.
	source := `import {createServer} from 'node:http';
import {appendFileSync,existsSync,unlinkSync} from 'node:fs';
import {createInterface} from 'node:readline';
createInterface({input:process.stdin}).once('line',line=>{
 const init=JSON.parse(line);
 if(!init.externalEvents)throw new Error('competing WebSocket mode');
 if(Object.values(process.env).includes('poll-secret-fixture'))throw new Error('poll token leaked');
 const stop=()=>server.close(()=>{try{unlinkSync(init.socket)}catch{};process.exit(0)});
 const server=createServer(async(req,res)=>{
  if(req.url==='/inbound'){
   if(existsSync(init.cwd+'/fail-once')){unlinkSync(init.cwd+'/fail-once');res.statusCode=503;res.end('{}');return;}
   let body='';for await(const chunk of req)body+=chunk;
   appendFileSync(init.cwd+'/received.jsonl',body+'\n');
  }
  res.end('{}');if(req.url==='/shutdown')setImmediate(stop);
 });server.listen(init.socket);process.stdin.once('end',stop);process.once('SIGTERM',stop);
});`
	entry := filepath.Join(dir, "fixture.mjs")
	if err := os.WriteFile(entry, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	client, err := feishubridge.Start(ctx, entry, dir, feishubridge.Init{Socket: filepath.Join(dir, "bridge.sock"), CWD: dir, AppID: "app", AppSecret: "secret", ExternalEvents: true}, io.Discard, "poll-secret-fixture")
	if err != nil {
		t.Fatal(err)
	}
	h.client = client
	t.Cleanup(func() { client.Close() })
	return h
}

func TestSharedInboxTwoHostsRetryAndDurableCursor(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("Node.js unavailable")
	}
	t.Setenv("CUSTOM_POLL_TOKEN", "poll-secret-fixture")
	a, b := inboxTestHost(t, "alice"), inboxTestHost(t, "bob")
	if err := os.WriteFile(filepath.Join(a.dir, "fail-once"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	var events []feishubridge.InboxEvent
	for i, name := range []string{"alice", "bob", "unknown"} {
		raw, _ := json.Marshal(map[string]any{"header": map[string]any{"app_id": "app", "event_type": "im.message.receive_v1"}, "event": map[string]any{"message": map[string]any{"message_id": "om_reply_" + name, "thread_id": "omt_" + name, "root_id": "om_" + name, "mentions": []string{"bot"}, "image_key": "retained-image"}}})
		events = append(events, feishubridge.InboxEvent{ID: int64(i + 1), Kind: "message", ChatID: "oc_shared", RootID: "om_" + name, ThreadID: "omt_" + name, Raw: raw})
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer poll-secret-fixture" {
			w.WriteHeader(401)
			return
		}
		var since int64
		fmt.Sscan(r.URL.Query().Get("since"), &since)
		page := []feishubridge.InboxEvent{}
		for _, e := range events {
			if e.ID > since {
				page = append(page, e)
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "events": page})
	}))
	defer srv.Close()
	key := "feishu_inbox_cursor:app:" + srv.URL
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for _, h := range []*bridgeHost{a, b} {
		wg.Add(1)
		go func(h *bridgeHost) {
			defer wg.Done()
			h.maintainFeishuInbox(ctx, srv.URL, "poll-secret-fixture", "app")
		}(h)
	}
	defer func() { cancel(); wg.Wait() }()
	deadline := time.NewTimer(8 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		ca, _ := a.repos.Counters.Get(ctx, key)
		cb, _ := b.repos.Counters.Get(ctx, key)
		if ca == 3 && cb == 3 {
			break
		}
		select {
		case <-deadline.C:
			t.Fatal("hosts did not independently drain inbox after retry")
		case <-tick.C:
		}
	}
	cancel()
	wg.Wait()
	for _, h := range []*bridgeHost{a, b} {
		data, err := os.ReadFile(filepath.Join(h.dir, "received.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(strings.TrimSpace(string(data)), "\n")
		if len(lines) != 1 {
			t.Fatalf("duplicate or cross-host delivery: %s", data)
		}
		want := events[0].Raw
		if h == b {
			want = events[1].Raw
		}
		if string(want) != lines[0] {
			t.Fatalf("SDK fields changed or delivered to wrong host: %s", data)
		}
		// Re-open the persisted counter through a fresh repository after poller restart.
		repos := storage.NewRepositories(h.db)
		since, err := repos.Counters.Get(context.Background(), key)
		if err != nil || since != 3 {
			t.Fatalf("cursor not durable: %d %v", since, err)
		}
		next, err := feishubridge.PollInbox(context.Background(), srv.URL, "poll-secret-fixture", since, func(feishubridge.InboxEvent) error { t.Fatal("replayed old reply"); return nil })
		if err != nil || next != since {
			t.Fatalf("restart: %d %v", next, err)
		}
	}
	// A stopped task and another app's event never reach the sidecar.
	if _, err := a.db.Exec(`UPDATE loops SET status='terminated'`); err != nil {
		t.Fatal(err)
	}
	if err := a.deliverInboxEvent(context.Background(), "app", events[0]); err != nil {
		t.Fatal(err)
	}
	other := events[1]
	other.Raw = json.RawMessage(`{"header":{"app_id":"other"}}`)
	if err := b.deliverInboxEvent(context.Background(), "app", other); err != nil {
		t.Fatal(err)
	}
	for _, h := range []*bridgeHost{a, b} {
		data, _ := os.ReadFile(filepath.Join(h.dir, "received.jsonl"))
		if strings.Count(string(data), "\n") != 1 {
			t.Fatal("late/foreign event delivered")
		}
	}
}

func TestSharedInboxConfiguration(t *testing.T) {
	t.Setenv("LOOPER_FEISHU_INBOX_URL", "")
	t.Setenv("LOOPER_FEISHU_INBOX_TOKEN", "")
	if endpoint, _, err := sharedFeishuInbox(config.Config{}); err != nil || endpoint != "" {
		t.Fatalf("legacy WebSocket mode: %s %v", endpoint, err)
	}
	t.Setenv("LOOPER_FEISHU_INBOX_URL", "https://inbox.test/events")
	if _, _, err := sharedFeishuInbox(config.Config{}); err == nil {
		t.Fatal("missing secret silently fell back to competing WebSocket")
	}
	t.Setenv("LOOPER_FEISHU_INBOX_TOKEN", "private")
	if _, _, err := sharedFeishuInbox(config.Config{}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOOPER_FEISHU_INBOX_URL", "http://public.test/events")
	if _, _, err := sharedFeishuInbox(config.Config{}); err == nil {
		t.Fatal("poll secret sent over public plaintext HTTP")
	}
}
