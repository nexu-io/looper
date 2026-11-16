package collab

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/feishubridge"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/infra/shell"
	"github.com/nexu-io/looper/internal/storage"
	"github.com/nexu-io/looper/internal/worker"
)

func TestBridgePRFollowupPreservesEvidenceAndNewerConclusions(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("Node.js unavailable")
	}
	for _, concurrentTurn := range []bool{false, true} {
		name := "review evidence reaches original session"
		if concurrentTurn {
			name = "human turn updates PRs during observation"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			dir, err := os.MkdirTemp("", "collab-pr-")
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
			h := &bridgeHost{dir: dir, db: db.DB(), repos: storage.NewRepositories(db.DB()), active: map[string]bool{}}
			h.cfg.HITL.Feishu = &config.HITLFeishuConfig{OwnerOpenID: "ou_owner"}
			if err := h.repos.Projects.Upsert(ctx, storage.ProjectRecord{ID: "local", Name: "local", RepoPath: dir, CreatedAt: "2026-11-16T00:00:00Z", UpdatedAt: "2026-11-16T00:00:00Z"}); err != nil {
				t.Fatal(err)
			}
			loop, err := h.enqueue(ctx, worker.EnqueueInput{ProjectID: "local", Prompt: "Keep the user's theme after refresh"})
			if err != nil {
				t.Fatal(err)
			}
			state := bridgeTask{RootID: "om_root", ThreadID: "omt_original", SessionID: "native-original", Outcome: Outcome{State: "monitoring_prs", PullRequests: []string{"https://github.com/acme/frontend/pull/1"}}}
			if err := h.patch(ctx, loop.ID, "shepherding", state, "waiting for review"); err != nil {
				t.Fatal(err)
			}
			if err := h.repos.FeishuThreads.Upsert(ctx, state.RootID, loop.ID, "task", "oc_private", "2026-11-16T00:00:00Z"); err != nil {
				t.Fatal(err)
			}
			h.gh = githubinfra.New(githubinfra.Options{GHRun: func(_ context.Context, options shell.Options) (shell.Result, error) {
				args := strings.Join(options.Args, " ")
				switch {
				case strings.HasPrefix(args, "pr view"):
					if concurrentTurn {
						state.Outcome.PullRequests = append(state.Outcome.PullRequests, "https://github.com/acme/backend/pull/2")
						state.Outcome.Summary = "Human clarified the cross-repository scope"
						if err := h.patch(ctx, loop.ID, "shepherding", state, state.Outcome.Summary); err != nil {
							t.Fatal(err)
						}
					}
					return shell.Result{Stdout: `{"state":"OPEN","headRefOid":"head","reviews":[{"state":"COMMENTED","commit":{"oid":"head"},"body":"Persist theme after refresh; storage denial must not break switching"}],"statusCheckRollup":[{"name":"browser","conclusion":"FAILURE","detailsUrl":"https://ci.example/log"}]}`}, nil
				case strings.Contains(args, "reviewThreads"):
					return shell.Result{Stdout: `{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[],"pageInfo":{"hasNextPage":false}}}}}}`}, nil
				default:
					return shell.Result{Stdout: "[]"}, nil
				}
			}})
			entry := filepath.Join(dir, "fixture.mjs")
			if err := os.WriteFile(entry, []byte(`import {createServer} from 'node:http';
import {writeFileSync} from 'node:fs';
import {createInterface} from 'node:readline';
const input=createInterface({input:process.stdin});
input.once('line',line=>{const init=JSON.parse(line);
const server=createServer(async(req,res)=>{if(req.url==='/tasks'){let body='';for await(const chunk of req)body+=chunk;writeFileSync(init.cwd+'/request.json',body)}res.end('{}')});
server.listen(init.socket);const stop=()=>server.close(()=>process.exit(0));process.stdin.once('end',stop);process.once('SIGTERM',stop)});`), 0600); err != nil {
				t.Fatal(err)
			}
			h.client, err = feishubridge.Start(ctx, entry, dir, feishubridge.Init{Socket: filepath.Join(dir, "bridge.sock"), CWD: dir, AppID: "fixture", AppSecret: "fixture"}, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			defer h.client.Close()
			h.pollPullRequests(ctx)
			if concurrentTurn {
				if _, err := os.Stat(filepath.Join(dir, "request.json")); !os.IsNotExist(err) {
					t.Fatal("obsolete PR snapshot woke the agent")
				}
				saved, _ := h.repos.Loops.GetByID(ctx, loop.ID)
				_, got, _ := readBridgeTask(*saved)
				if len(got.Outcome.PullRequests) != 2 || got.Outcome.Summary != state.Outcome.Summary {
					t.Fatalf("observer overwrote newer conclusion: %+v", got)
				}
				return
			}
			body, err := os.ReadFile(filepath.Join(dir, "request.json"))
			if err != nil {
				t.Fatal(err)
			}
			var task feishubridge.Task
			if err := json.Unmarshal(body, &task); err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"Persist theme after refresh", "https://ci.example/log", h.resultPath(loop.ID)} {
				if !strings.Contains(task.Prompt, want) {
					t.Errorf("follow-up omitted actual evidence %q: %s", want, task.Prompt)
				}
			}
			if task.ThreadID != state.ThreadID || task.RootMessageID != state.RootID {
				t.Fatalf("follow-up left the original conversation: %+v", task)
			}
		})
	}
}
