package collab

import (
	"context"
	"encoding/json"
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

func TestBridgeStopReturnsResponseAfterSidecarExits(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("Node.js unavailable")
	}
	t.Setenv("FEISHU_APP_ID", "fixture")
	t.Setenv("FEISHU_APP_SECRET", "fixture")
	t.Setenv("LOOPER_FEISHU_INBOX_URL", "")
	t.Setenv("LOOPER_FEISHU_INBOX_TOKEN", "")
	for i := 0; i < 12; i++ {
		t.Run(string(rune('a'+i)), func(t *testing.T) {
			cfg, dir := hostFixture(t, "#!/bin/sh\nexit 0\n")
			cli := filepath.Join(dir, "lark.sh")
			if err := os.WriteFile(cli, []byte("#!/bin/sh\nprintf '%s\\n' '{\"ok\":true,\"data\":{}}'\n"), 0700); err != nil {
				t.Fatal(err)
			}
			cfg.HITL = config.HITLConfig{Enabled: true, AnswerTransport: "feishu", Feishu: &config.HITLFeishuConfig{CLIPath: cli, ChatID: "oc_test", OwnerOpenID: "ou_owner"}}
			entry := filepath.Join(dir, "bridge.mjs")
			if err := os.WriteFile(entry, []byte(`import {createServer} from 'node:http';
import {createInterface} from 'node:readline';
createInterface({input:process.stdin}).once('line',line=>{
const init=JSON.parse(line);const server=createServer((req,res)=>res.end('{}'));
server.listen(init.socket);const stop=()=>server.close(()=>process.exit(0));
process.stdin.once('end',stop);process.once('SIGTERM',stop)});`), 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			done := make(chan error, 1)
			go func() {
				done <- Serve(ctx, Options{Config: cfg, StateDir: dir, NetworkStatePath: filepath.Join(dir, "network.json"), BridgeEntry: entry})
			}()
			defer func() {
				cancel()
				if err := <-done; err != nil {
					t.Error(err)
				}
			}()
			tick := time.NewTicker(10 * time.Millisecond)
			defer tick.Stop()
			for {
				data, err := Request(ctx, dir, "GET", "/status", nil)
				var status Status
				if err == nil && json.Unmarshal(data, &status) == nil && status.State == "running" {
					break
				}
				select {
				case <-ctx.Done():
					t.Fatal("host did not become ready")
				case <-tick.C:
				}
			}
			data, err := Request(ctx, dir, "POST", "/stop", nil)
			if err != nil || !strings.Contains(string(data), `"status":"stopped"`) {
				t.Fatalf("lost stop acknowledgement: %s %v", data, err)
			}
		})
	}
}

// Exercise the real service lifetime, SQLite, sidecar exit callback, and a
// second start. A service stop must not turn resumable work into a failed task.
func TestBridgeServiceRestartResumesInterruptedConversation(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("Node.js unavailable")
	}
	t.Setenv("FEISHU_APP_ID", "fixture")
	t.Setenv("FEISHU_APP_SECRET", "fixture")
	t.Setenv("LOOPER_FEISHU_INBOX_URL", "")
	t.Setenv("LOOPER_FEISHU_INBOX_TOKEN", "")
	for _, stopMethod := range []string{"control", "signal"} {
		t.Run(stopMethod, func(t *testing.T) {
			ctx, deadline := context.WithTimeout(context.Background(), 15*time.Second)
			defer deadline()
			cfg, dir := hostFixture(t, "#!/bin/sh\nexit 0\n")
			cli := filepath.Join(dir, "lark.sh")
			if err := os.WriteFile(cli, []byte("#!/bin/sh\nprintf '%s\\n' '{\"ok\":true,\"data\":{}}'\n"), 0700); err != nil {
				t.Fatal(err)
			}
			cfg.HITL = config.HITLConfig{Enabled: true, AnswerTransport: "feishu", Feishu: &config.HITLFeishuConfig{CLIPath: cli, ChatID: "oc_test", OwnerOpenID: "ou_owner"}}
			open := func() *storage.SQLiteCoordinator {
				t.Helper()
				db, err := storage.OpenSQLiteCoordinator(ctx, filepath.Join(dir, "looper.sqlite"), storage.SQLiteCoordinatorOptions{Migrations: storage.EmbeddedMigrations})
				if err != nil {
					t.Fatal(err)
				}
				if _, err = db.MigrationRunner().RunPending(ctx, storage.RunPendingOptions{}); err != nil {
					t.Fatal(err)
				}
				return db
			}
			db := open()
			h := &bridgeHost{dir: dir, db: db.DB(), repos: storage.NewRepositories(db.DB()), active: map[string]bool{}}
			if err := h.repos.Projects.Upsert(ctx, storage.ProjectRecord{ID: "local", Name: "local", RepoPath: dir, CreatedAt: "2026-11-18T00:00:00Z", UpdatedAt: "2026-11-18T00:00:00Z"}); err != nil {
				t.Fatal(err)
			}
			loop, err := h.enqueue(ctx, worker.EnqueueInput{ProjectID: "local", Prompt: "Continue pending review"})
			if err != nil {
				t.Fatal(err)
			}
			state := bridgeTask{RootID: "om_root", ThreadID: "omt_original", SessionID: "native-original", Outcome: Outcome{State: "monitoring_prs", Summary: "Review pending", Context: "Preserve human decisions", PullRequests: []string{"https://code.example/team/app/pulls/1"}}}
			// A turn was active before the first start, as after a hard process exit.
			if err = h.patch(ctx, loop.ID, "running", state, ""); err != nil {
				t.Fatal(err)
			}
			if err = h.repos.FeishuThreads.Upsert(ctx, state.RootID, loop.ID, loop.ID, "oc_test", "2026-11-18T00:00:00Z"); err != nil {
				t.Fatal(err)
			}
			db.Close()
			entry := filepath.Join(dir, "fixture.mjs")
			if err = os.WriteFile(entry, []byte(`import {createServer,request} from 'node:http';
import {writeFileSync} from 'node:fs';import {createInterface} from 'node:readline';
createInterface({input:process.stdin}).once('line',line=>{const init=JSON.parse(line);let task,stopping=false;
const report=phase=>new Promise((resolve,reject)=>{const req=request({socketPath:init.callbackSocket,path:'/bridge/events',method:'POST'},res=>{res.resume();res.on('end',()=>res.statusCode===200?resolve():reject(new Error('callback '+res.statusCode)))});req.on('error',reject);req.end(JSON.stringify({turnId:String(process.pid),phase,terminal:'interrupted',threadId:task.threadId,rootMessageId:task.rootMessageId,sessionId:'native-original'}))});
const server=createServer(async(req,res)=>{if(req.url==='/tasks'){let body='';for await(const c of req)body+=c;task=JSON.parse(body);await report('started');writeFileSync(init.cwd+'/received.json',body)}res.end('{}')});
server.listen(init.socket);const stop=async()=>{if(stopping)return;stopping=true;if(task)await report('finished');server.close(()=>process.exit(0))};process.stdin.once('end',stop);process.once('SIGTERM',stop)});`), 0600); err != nil {
				t.Fatal(err)
			}
			for generation := 0; generation < 2; generation++ {
				received := filepath.Join(dir, "workspace", "received.json")
				if err := os.Remove(received); err != nil && !os.IsNotExist(err) {
					t.Fatal(err)
				}
				serviceCtx, cancel := context.WithCancel(ctx)
				done := make(chan error, 1)
				go func() {
					done <- Serve(serviceCtx, Options{Config: cfg, StateDir: dir, NetworkStatePath: filepath.Join(dir, "network.json"), BridgeEntry: entry})
					close(done)
				}()
				wait := func() {
					t.Helper()
					tick := time.NewTicker(10 * time.Millisecond)
					defer tick.Stop()
					for {
						data, err := Request(ctx, dir, "GET", "/status", nil)
						var status Status
						_, receivedErr := os.Stat(received)
						if err == nil && json.Unmarshal(data, &status) == nil && status.State == "running" && status.ActiveExecutions == 1 && receivedErr == nil {
							return
						}
						select {
						case err := <-done:
							t.Fatalf("service exited: %v", err)
						case <-ctx.Done():
							t.Fatal("task did not resume")
						case <-tick.C:
						}
					}
				}
				// Always clean up, including on a failed assertion.
				func() {
					defer func() {
						cancel()
						if err := <-done; err != nil {
							t.Error(err)
						}
					}()
					wait()
					if stopMethod == "control" {
						if _, err := Request(ctx, dir, http.MethodPost, "/stop", nil); err != nil {
							t.Fatal(err)
						}
					} else {
						cancel()
					}
				}()
				db = open()
				h.db = db.DB()
				h.repos = storage.NewRepositories(db.DB())
				saved, err := h.repos.Loops.GetByID(ctx, loop.ID)
				if err != nil {
					t.Fatal(err)
				}
				_, got, _ := readBridgeTask(*saved)
				if saved.Status != "queued" || got.SessionID != state.SessionID || got.ThreadID != state.ThreadID || got.RootID != state.RootID || got.Outcome.Context != state.Outcome.Context || len(got.Outcome.PullRequests) != 1 || got.Outcome.PullRequests[0] != state.Outcome.PullRequests[0] {
					t.Errorf("generation %d did not retain resumable task: status=%s state=%+v", generation, saved.Status, got)
				}
				runs, err := h.repos.Runs.ListByLoop(ctx, loop.ID)
				if err != nil || len(runs) != generation+1 {
					t.Errorf("lost run audit: %+v %v", runs, err)
				}
				for _, run := range runs {
					if run.Status != "interrupted" || run.EndedAt == nil {
						t.Errorf("interruption misreported: %+v", run)
					}
				}
				raw, err := os.ReadFile(received)
				var task feishubridge.Task
				if err != nil || json.Unmarshal(raw, &task) != nil || task.ThreadID != state.ThreadID || task.RootMessageID != state.RootID || !strings.Contains(task.Prompt, "继续原任务") {
					t.Errorf("wrong restart dispatch: %s %v", raw, err)
				}
				db.Close()
				if t.Failed() {
					return
				}
			}
		})
	}
}
