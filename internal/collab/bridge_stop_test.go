package collab

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/feishubridge"
	"github.com/nexu-io/looper/internal/planestrict"
	"github.com/nexu-io/looper/internal/storage"
	"github.com/nexu-io/looper/internal/worker"
)

// Plane can confirm an idle task's stop without a running OS process. Its
// retained native session must still be stopped locally and reject later turns.
func TestPlaneConfirmedStopDisablesWaitingSession(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("Node.js unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	dir, err := os.MkdirTemp("", "collab-stop-")
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
	h := &bridgeHost{dir: dir, db: db.DB(), repos: storage.NewRepositories(db.DB()), active: map[string]bool{}}
	if err = h.writeWorkspace(filepath.Join(dir, "workspace")); err != nil {
		t.Fatal(err)
	}
	if err = h.repos.Projects.Upsert(ctx, storage.ProjectRecord{ID: "local", Name: "local", RepoPath: dir, CreatedAt: "2026-11-17T00:00:00Z", UpdatedAt: "2026-11-17T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	dispatch := planestrict.Dispatch{ID: "33333333-3333-4333-8333-333333333333", State: "confirmed_stopped"}
	loop, err := h.enqueue(ctx, worker.EnqueueInput{ProjectID: "local", StrictDispatchID: dispatch.ID, Prompt: "Investigate"})
	if err != nil {
		t.Fatal(err)
	}
	state := bridgeTask{RootID: "om_root", ThreadID: "omt_waiting", SessionID: "retained-native"}
	if err = h.patch(ctx, loop.ID, "awaiting_human", state, "Waiting for owner"); err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(dir, "fixture.mjs")
	if err = os.WriteFile(entry, []byte(`import {createServer} from 'node:http';
import {writeFileSync} from 'node:fs';
import {createInterface} from 'node:readline';
createInterface({input:process.stdin}).once('line',line=>{const init=JSON.parse(line);
const server=createServer(async(req,res)=>{if(req.url==='/stop-task'){let body='';for await(const chunk of req)body+=chunk;writeFileSync(init.cwd+'/stopped.json',body)}res.end('{}')});
server.listen(init.socket);const stop=()=>server.close(()=>process.exit(0));process.stdin.once('end',stop);process.once('SIGTERM',stop)});`), 0600); err != nil {
		t.Fatal(err)
	}
	h.client, err = feishubridge.Start(ctx, entry, dir, feishubridge.Init{Socket: filepath.Join(dir, "bridge.sock"), CWD: dir, AppID: "fixture", AppSecret: "fixture"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer h.client.Close()
	// An in-flight turn has to end before the local loop becomes terminated.
	h.active[loop.ID] = true
	if err = h.planeDispatch(ctx, nil, "local", dispatch); err != nil {
		t.Fatal(err)
	}
	saved, _ := h.repos.Loops.GetByID(ctx, loop.ID)
	if saved.Status == "terminated" {
		t.Fatal("stop request was treated as proof of exit")
	}
	delete(h.active, loop.ID)
	if err = h.planeDispatch(ctx, nil, "local", dispatch); err != nil {
		t.Fatal(err)
	}
	saved, _ = h.repos.Loops.GetByID(ctx, loop.ID)
	if saved.Status != "terminated" {
		t.Fatalf("waiting session remained resumable: %s", saved.Status)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "stopped.json"))
	if err != nil {
		t.Fatal(err)
	}
	var stopped map[string]string
	if err = json.Unmarshal(raw, &stopped); err != nil || stopped["threadId"] != state.ThreadID {
		t.Fatalf("wrong session stopped: %s %v", raw, err)
	}
	// The same awaited callback precedes runStreamed in both transports. This
	// protects WebSocket replies too, not only shared inbox ownership filtering.
	for _, phase := range []string{"started", "finished"} {
		body, _ := json.Marshal(feishubridge.TurnEvent{Phase: phase, ThreadID: state.ThreadID, RootMessageID: state.RootID})
		response := httptest.NewRecorder()
		h.turnEvent(response, httptest.NewRequest("POST", "/bridge/events", bytes.NewReader(body)))
		expected := 200
		if phase == "started" {
			expected = 409
		}
		if response.Code != expected {
			t.Fatalf("%s callback status=%d", phase, response.Code)
		}
	}
	saved, _ = h.repos.Loops.GetByID(ctx, loop.ID)
	if saved.Status != "terminated" || h.active[loop.ID] {
		t.Fatal("later callbacks resurrected stopped task")
	}
	// Completed tasks retain a native session too. Revocation and removal from
	// a project must disable those conversations, without mistaking an outage
	// for a permission change.
	personalProject := "personal-plane-44444444-4444-4444-8444-444444444444"
	if err = h.repos.Projects.Upsert(ctx, storage.ProjectRecord{ID: personalProject, Name: "personal", RepoPath: dir, CreatedAt: "2026-11-17T00:00:00Z", UpdatedAt: "2026-11-17T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	deviceState, unavailable := "active", true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Looper-Signature") == "" {
			t.Error("unsigned device directory request")
		}
		if unavailable {
			http.Error(w, "temporary outage", 503)
			return
		}
		_ = json.NewEncoder(w).Encode(planestrict.DeviceProjects{State: deviceState})
	}))
	defer server.Close()
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	h.personalClient, err = planestrict.NewClient(server.URL, "fixture", "44444444-4444-4444-8444-444444444444", planestrict.Credentials{PrivateKey: key, KeyRevision: 1, NodeID: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{"awaiting_human", "completed"} {
		personal, err := h.enqueue(ctx, worker.EnqueueInput{ProjectID: personalProject, Prompt: "Personal task"})
		if err != nil {
			t.Fatal(err)
		}
		if err = h.patch(ctx, personal.ID, status, state, "Retained session"); err != nil {
			t.Fatal(err)
		}
		if unavailable {
			if err = h.refreshPersonalPlane(ctx); err == nil {
				t.Fatal("directory outage was hidden")
			}
			saved, _ = h.repos.Loops.GetByID(ctx, personal.ID)
			if saved.Status != status {
				t.Fatal("network outage revoked a task")
			}
			unavailable = false
		}
		if err = h.refreshPersonalPlane(ctx); err != nil {
			t.Fatal(err)
		}
		saved, _ = h.repos.Loops.GetByID(ctx, personal.ID)
		if saved.Status != "terminated" {
			t.Fatalf("%s task remained resumable after %s directory response", status, deviceState)
		}
		deviceState = "revoked"
	}
}
