package collab

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/feishubridge"
	"github.com/nexu-io/looper/internal/forge"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/infra/shell"
	"github.com/nexu-io/looper/internal/storage"
	"github.com/nexu-io/looper/internal/worker"
)

func TestBridgePRFollowupPreservesEvidenceAndNewerConclusions(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("Node.js unavailable")
	}
	for _, platform := range []string{"github", "forgejo"} {
		for _, concurrentTurn := range []bool{false, true} {
			name := "review evidence reaches original session"
			if concurrentTurn {
				name = "human turn updates PRs during observation"
			}
			t.Run(platform+"/"+name, func(t *testing.T) {
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
				if platform == "forgejo" {
					server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						switch r.URL.Path {
						case "/swagger.v1.json":
							io.WriteString(w, `{"paths":{"/repos/{owner}/{repo}/pulls/{index}/reviews":{"get":{}},"/repos/{owner}/{repo}/pulls/{index}/reviews/{id}/comments":{"get":{}}}}`)
						case "/api/v1/repos/acme/frontend/pulls/1":
							if concurrentTurn {
								state.Outcome.PullRequests = append(state.Outcome.PullRequests, "https://github.com/acme/backend/pull/2")
								state.Outcome.Summary = "Human clarified the cross-repository scope"
								if err := h.patch(ctx, loop.ID, "shepherding", state, state.Outcome.Summary); err != nil {
									t.Error(err)
								}
							}
							io.WriteString(w, `{"number":1,"state":"open","head":{"sha":"head"},"user":{"login":"author"}}`)
						case "/api/v1/repos/acme/frontend/pulls/1/reviews":
							io.WriteString(w, `[{"id":9827,"state":"COMMENT","body":"Persist theme after refresh","commit_id":"head","comments_count":1,"user":{"login":"reviewer"}}]`)
						case "/api/v1/repos/acme/frontend/pulls/1/reviews/9827/comments":
							io.WriteString(w, `[{"id":42,"body":"storage denial must not break switching","path":"account.ts","user":{"login":"reviewer"}}]`)
						case "/api/v1/repos/acme/frontend/issues/1/comments":
							io.WriteString(w, `[]`)
						case "/api/v1/repos/acme/frontend/statuses/head":
							io.WriteString(w, `[{"id":1,"context":"browser","status":"failure","target_url":"https://ci.example/log"}]`)
						case "/api/v1/repos/acme/frontend/actions/runs":
							io.WriteString(w, `{"workflow_runs":[]}`)
						default:
							t.Errorf("unexpected Forgejo request: %s", r.URL)
							w.WriteHeader(404)
						}
					}))
					defer server.Close()
					t.Setenv("COLLAB_TEST_FORGE_TOKEN", "fixture")
					env := "COLLAB_TEST_FORGE_TOKEN"
					h.cfg.Providers = []config.ProviderConfig{{ID: "forge", Kind: config.ProviderKindForgejo, BaseURL: server.URL, Auth: config.ProviderAuthTokenEnv, TokenEnv: &env}}
					state.Outcome.PullRequests = []string{server.URL + "/acme/frontend/pulls/1"}
					if err := h.patch(ctx, loop.ID, "shepherding", state, "waiting for review"); err != nil {
						t.Fatal(err)
					}
					h.prObserver = &pullRequestObserver{cfg: &h.cfg, github: h.gh, forgejoOptions: []forge.ForgejoOption{forge.WithHTTPClient(server.Client())}}
				}
				startPRFollowupFixture(t, ctx, h)
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
				for _, want := range []string{"Persist theme after refresh", "https://ci.example/log", h.resultPath(loop.ID), "chat_id：oc_private", "message_id：om_root"} {
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
}

func startPRFollowupFixture(t *testing.T, ctx context.Context, h *bridgeHost) {
	t.Helper()
	dir := h.dir
	var err error
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
	t.Cleanup(func() { h.client.Close() })
}

// The real SQLite task and bridge HTTP contract, with native Forgejo wire
// responses: no direct calls to an Agent or hand-crafted wake snapshots.
func TestForgejoPRLifecycleResumesOriginalConversation(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("Node.js unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	dir, err := os.MkdirTemp("", "collab-lifecycle-")
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
	h.cfg.HITL.Feishu = &config.HITLFeishuConfig{OwnerOpenID: "ou_owner"}
	if err = h.repos.Projects.Upsert(ctx, storage.ProjectRecord{ID: "local", Name: "local", RepoPath: dir, CreatedAt: "2026-11-18T00:00:00Z", UpdatedAt: "2026-11-18T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	loop, err := h.enqueue(ctx, worker.EnqueueInput{ProjectID: "local", Prompt: "Landing page fix"})
	if err != nil {
		t.Fatal(err)
	}
	head, base, checkState, prState := "head1", "base1", "pending", "open"
	merged, mergeable, readError := false, true, false
	reviews := []map[string]any{}
	comments := []map[string]any{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if readError {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		switch r.URL.Path {
		case "/swagger.v1.json":
			io.WriteString(w, `{"paths":{"/repos/{owner}/{repo}/pulls/{index}/reviews":{"get":{}},"/repos/{owner}/{repo}/pulls/{index}/reviews/{id}/comments":{"get":{}}}}`)
		case "/api/v1/repos/core/landing/pulls/322":
			json.NewEncoder(w).Encode(map[string]any{"number": 322, "state": prState, "merged": merged, "merged_at": nil, "mergeable": mergeable, "head": map[string]any{"sha": head}, "base": map[string]any{"sha": base}, "user": map[string]any{"login": "author"}})
		case "/api/v1/repos/core/landing/pulls/322/reviews":
			json.NewEncoder(w).Encode(reviews)
		case "/api/v1/repos/core/landing/pulls/322/reviews/9827/comments":
			io.WriteString(w, `[{"id":42,"body":"restore login on enhancer failure","path":"account.ts","user":{"login":"reviewer"}}]`)
		case "/api/v1/repos/core/landing/issues/322/comments":
			json.NewEncoder(w).Encode(comments)
		case "/api/v1/repos/core/landing/statuses/" + head:
			json.NewEncoder(w).Encode([]map[string]any{{"id": 7, "context": "browser", "status": checkState, "description": "Pricing regression", "target_url": "https://ci.example/pricing"}})
		case "/api/v1/repos/core/landing/actions/runs":
			if r.URL.Query().Get("head_sha") != head {
				t.Errorf("wrong head: %s", r.URL)
			}
			io.WriteString(w, `{"workflow_runs":[]}`)
		default:
			t.Errorf("unexpected request %s", r.URL)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	t.Setenv("COLLAB_LIFECYCLE_TOKEN", "fixture")
	env := "COLLAB_LIFECYCLE_TOKEN"
	h.cfg.Providers = []config.ProviderConfig{{ID: "forge", Kind: config.ProviderKindForgejo, BaseURL: server.URL, Auth: config.ProviderAuthTokenEnv, TokenEnv: &env}}
	h.prObserver = &pullRequestObserver{cfg: &h.cfg, forgejoOptions: []forge.ForgejoOption{forge.WithHTTPClient(server.Client())}}
	state := bridgeTask{RootID: "om_original", ThreadID: "omt_original", SessionID: "original-session", Outcome: Outcome{State: "monitoring_prs", PullRequests: []string{server.URL + "/core/landing/pulls/322", "https://github.com/acme/api/pull/2"}}}
	// A task may span both platforms. A closed GitHub PR must not complete an
	// open Forgejo PR; all evidence is delivered to the same original session.
	h.prObserver.github = githubinfra.New(githubinfra.Options{GHRun: func(_ context.Context, options shell.Options) (shell.Result, error) {
		if strings.HasPrefix(strings.Join(options.Args, " "), "pr view") {
			return shell.Result{Stdout: `{"state":"CLOSED"}`}, nil
		}
		if strings.Contains(strings.Join(options.Args, " "), "reviewThreads") {
			return shell.Result{Stdout: `{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[],"pageInfo":{"hasNextPage":false}}}}}}`}, nil
		}
		return shell.Result{Stdout: "[]"}, nil
	}})
	if err = h.patch(ctx, loop.ID, "shepherding", state, "monitoring"); err != nil {
		t.Fatal(err)
	}
	if err = h.repos.FeishuThreads.Upsert(ctx, state.RootID, loop.ID, "task", "oc_original", "2026-11-18T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	startPRFollowupFixture(t, ctx, h)
	poll := func(wantWake bool, want ...string) {
		t.Helper()
		os.Remove(filepath.Join(dir, "request.json"))
		h.pollPullRequests(ctx)
		body, err := os.ReadFile(filepath.Join(dir, "request.json"))
		if !wantWake {
			if !os.IsNotExist(err) {
				t.Fatalf("unexpected wake: %s (%v)", body, err)
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		var task feishubridge.Task
		if err = json.Unmarshal(body, &task); err != nil {
			t.Fatal(err)
		}
		if task.ThreadID != state.ThreadID || task.RootMessageID != state.RootID || task.ChatID != "oc_original" {
			t.Fatalf("wrong conversation: %+v", task)
		}
		for _, text := range want {
			if !strings.Contains(task.Prompt, text) {
				t.Errorf("missing %q in %s", text, task.Prompt)
			}
		}
		saved, err := h.repos.Loops.GetByID(ctx, loop.ID)
		if err != nil {
			t.Fatal(err)
		}
		_, state, _ = readBridgeTask(*saved)
		if state.SessionID != "original-session" || saved.Status != "queued" {
			t.Fatalf("lost original session: %+v %+v", saved, state)
		}
		// Simulate a completed Agent turn returning to monitoring, without changing
		// the host's last-observed signal or starting a second conversation.
		delete(h.active, loop.ID)
		if err = h.patch(ctx, loop.ID, "awaiting_human", state, "monitoring"); err != nil {
			t.Fatal(err)
		}
	}
	poll(true, "PENDING", "本地启动")
	poll(false)
	reviews = []map[string]any{{"id": 9827, "state": "COMMENT", "body": "P2 enhancer fallback", "commit_id": head, "comments_count": 1, "user": map[string]any{"login": "reviewer"}}}
	poll(true, "P2 enhancer fallback", "restore login on enhancer failure", "account.ts")
	poll(false)
	head = "head2"
	poll(true, "head2")
	checkState = "failure"
	poll(true, "FAILURE", "https://ci.example/pricing")
	checkState = "success"
	poll(true, "SUCCESS")
	reviews = append(reviews, map[string]any{"id": 9828, "state": "APPROVED", "commit_id": head, "comments_count": 0, "user": map[string]any{"login": "reviewer"}})
	poll(true, "APPROVED")
	base = "base2"
	mergeable = false
	poll(true, "base2", `"Mergeable":false`)
	mergeable = true
	poll(true, `"Mergeable":true`)
	comments = append(comments, map[string]any{"id": 6, "body": "Preview is ready", "user": map[string]any{"login": "deployer"}})
	poll(true, "Preview is ready")
	comments = append(comments, map[string]any{"id": 7, "body": "Fixed and requested re-review", "user": map[string]any{"login": "author"}})
	poll(false)
	readError = true
	poll(true, "读取失败")
	poll(false)
	readError = false
	poll(true, "Preview is ready")
	prState = "closed"
	merged = true
	poll(true, `"State":"MERGED"`)
	poll(false)
	merged = false
	poll(true, `"State":"CLOSED"`)
}
