package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/forge"
	"github.com/nexu-io/looper/internal/projects"
	"github.com/nexu-io/looper/internal/storage"
	"github.com/nexu-io/looper/internal/webhookforward"
)

// This server models Forgejo v15/v16's API contract: reads omit secrets,
// PATCH cannot rotate secrets or repair Actions subscriptions, and delivery
// headers contain both exact Forgejo types and coarse GitHub-compatible types.
// Sources: https://codeberg.org/forgejo/forgejo/src/tag/v16.0.4/routers/api/v1/utils/hook.go
// and services/webhook/{notifier.go,shared/payloader.go} at the same tag.
type forgejoWebhookFixture struct {
	t                         *testing.T
	mu                        sync.Mutex
	hooks                     map[int64]forge.RepositoryHook
	secrets                   map[int64]string
	creates, patches, deletes int
	reads                     map[string]int
	server                    *httptest.Server
	onGetHook                 func(int64)
}

func newForgejoWebhookFixture(t *testing.T) *forgejoWebhookFixture {
	t.Helper()
	f := &forgejoWebhookFixture{t: t, hooks: map[int64]forge.RepositoryHook{}, secrets: map[int64]string{}, reads: map[string]int{}}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}

func (f *forgejoWebhookFixture) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "token webhook-contract-token" {
		f.t.Errorf("wrong hosting auth")
		http.Error(w, "unauthorized", 401)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/forge/api/v1")
	root := "/repos/acme/app/hooks"
	if strings.HasPrefix(path, root) {
		id, _ := strconv.ParseInt(strings.TrimPrefix(path, root+"/"), 10, 64)
		hook, found := f.hooks[id]
		switch r.Method {
		case http.MethodPost:
			var body struct {
				Type   string            `json:"type"`
				Active bool              `json:"active"`
				Events []string          `json:"events"`
				Config map[string]string `json:"config"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				f.t.Error(err)
				http.Error(w, "bad payload", 400)
				return
			}
			if body.Type != "forgejo" || body.Config["secret"] == "" || body.Config["content_type"] != "json" {
				f.t.Error("invalid Forgejo hook creation")
				http.Error(w, "bad hook", 422)
				return
			}
			f.creates++
			id = int64(100 + f.creates)
			hook = forge.RepositoryHook{ID: id, Type: body.Type, Active: body.Active, Events: body.Events}
			hook.Config.URL, hook.Config.ContentType = body.Config["url"], "json"
			f.hooks[id], f.secrets[id] = hook, body.Config["secret"]
			w.WriteHeader(http.StatusCreated)
		case http.MethodGet:
			if f.onGetHook != nil {
				f.onGetHook(id)
			}
			if !found {
				http.NotFound(w, r)
				return
			}
		case http.MethodPatch:
			if !found {
				http.NotFound(w, r)
				return
			}
			f.patches++
			var body struct {
				Active bool              `json:"active"`
				Config map[string]string `json:"config"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				f.t.Error(err)
				return
			}
			if body.Config["secret"] != "" {
				f.t.Error("Forgejo PATCH cannot update a secret")
			}
			hook.Active, hook.Config.URL, hook.Config.ContentType, hook.BranchFilter = body.Active, body.Config["url"], body.Config["content_type"], ""
			f.hooks[id] = hook
		case http.MethodDelete:
			f.deletes++
			delete(f.hooks, id)
			delete(f.secrets, id)
			w.WriteHeader(204)
			return
		default:
			http.Error(w, "method", 405)
			return
		}
		_ = json.NewEncoder(w).Encode(hook)
		return
	}
	if r.Method != http.MethodGet {
		f.t.Errorf("unexpected mutation %s %s", r.Method, r.URL.Path)
		http.Error(w, "unexpected mutation", 500)
		return
	}
	f.reads[path]++
	encode := func(value any) { _ = json.NewEncoder(w).Encode(value) }
	pr := func(number int64) map[string]any {
		author := "alice"
		if number == 43 {
			author = "bot"
		}
		return map[string]any{"number": number, "title": "Webhook review", "state": "open", "mergeable": true, "html_url": f.server.URL + fmt.Sprintf("/acme/app/pulls/%d", number), "user": map[string]any{"login": author}, "requested_reviewers": []any{map[string]any{"login": "bot", "id": 7}}, "head": map[string]any{"ref": "feature", "sha": "head-42"}, "base": map[string]any{"ref": "main", "sha": "base-42"}, "labels": []any{}}
	}
	switch {
	case strings.HasSuffix(r.URL.Path, "/swagger.v1.json"):
		encode(map[string]any{"paths": map[string]any{"/repos/{owner}/{repo}/pulls/{index}/requested_reviewers": map[string]any{"post": map[string]any{}}, "/repos/{owner}/{repo}/pulls/{index}/reviews": map[string]any{"get": map[string]any{}, "post": map[string]any{}}, "/repos/{owner}/{repo}/pulls/{index}/reviews/{id}/comments": map[string]any{"get": map[string]any{}}}})
	case path == "/user":
		encode(map[string]any{"id": 7, "login": "bot"})
	case strings.HasPrefix(path, "/repos/acme/app/issues") && !strings.HasSuffix(path, "/comments"):
		issue := func(number int, labels []string, assignee bool) map[string]any {
			labelObjects := []any{}
			for _, label := range labels {
				labelObjects = append(labelObjects, map[string]any{"name": label})
			}
			assignees := []any{}
			if assignee {
				assignees = append(assignees, map[string]any{"login": "bot"})
			}
			return map[string]any{"number": number, "title": "issue", "body": "implement", "state": "open", "labels": labelObjects, "assignees": assignees, "user": map[string]any{"login": "alice"}, "updated_at": "2026-09-14T00:00:00Z"}
		}
		var number int
		_, _ = fmt.Sscanf(path, "/repos/acme/app/issues/%d", &number)
		switch number {
		case 7:
			encode(issue(7, []string{"looper:plan"}, true))
		case 9:
			encode(issue(9, []string{"looper:worker-ready"}, true))
		case 10:
			encode(issue(10, []string{"looper:worker-ready", "looper:hold"}, true))
		case 11:
			encode(issue(11, []string{"looper:worker-ready"}, false))
		case 12:
			closed := issue(12, []string{"looper:plan", "looper:worker-ready"}, true)
			closed["state"] = "closed"
			encode(closed)
		case 13:
			pull := issue(13, []string{"looper:plan", "looper:worker-ready"}, true)
			pull["pull_request"] = map[string]any{"url": "pr"}
			encode(pull)
		default:
			f.t.Errorf("unexpected issue scan/number %s", path)
			http.Error(w, "unexpected issue scan", 500)
		}
	case path == "/repos/acme/app/pulls":
		encode([]any{pr(42), pr(43)})
	case path == "/repos/acme/app/pulls/42":
		encode(pr(42))
	case path == "/repos/acme/app/pulls/43":
		encode(pr(43))
	case path == "/repos/acme/app/pulls/43/reviews":
		encode([]any{map[string]any{"id": 8, "state": "REQUEST_CHANGES", "body": "fix the bug", "commit_id": "head-42", "user": map[string]any{"login": "alice"}}})
	case path == "/repos/acme/app/pulls/43/reviews/8/comments":
		encode([]any{map[string]any{"id": 31, "body": "[P1] Handle empty input", "path": "app.go", "new_position": 1, "original_commit_id": "head-42", "commit_id": "head-42", "user": map[string]any{"login": "alice"}, "created_at": "2026-09-14T00:00:00Z", "updated_at": "2026-09-14T00:00:00Z"}})
	case strings.HasSuffix(path, "/reviews"), strings.HasSuffix(path, "/comments"), strings.Contains(path, "/statuses/"):
		encode([]any{})
	case strings.HasSuffix(path, "/actions/runs"):
		encode(map[string]any{"workflow_runs": []any{}})
	case strings.HasSuffix(path, ".diff"):
		_, _ = w.Write([]byte("diff --git a/app.go b/app.go\n--- a/app.go\n+++ b/app.go\n@@ -1 +1 @@\n-old\n+new\n"))
	default:
		f.t.Errorf("unexpected GET %s", r.URL.Path)
		http.Error(w, "unexpected GET", 500)
	}
}

func forgejoWebhookTestConfig(t *testing.T, fixture *forgejoWebhookFixture) (config.Config, *storage.SQLiteCoordinator, *storage.Repositories) {
	t.Helper()
	t.Setenv("FORGEJO_WEBHOOK_CONTRACT_TOKEN", "webhook-contract-token")
	root := t.TempDir()
	cfg, err := config.DefaultConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Storage.DBPath = filepath.Join(root, "looper.sqlite")
	cfg.Webhook.Enabled, cfg.Webhook.Mode, cfg.Webhook.PublicBaseURL, cfg.Webhook.ListenPort = true, config.WebhookModeTunnel, "https://hooks.example/base", 0
	cfg.Tools.GHPath = nil
	cfg.Notifications.Osascript.Enabled = false
	vendor := config.AgentVendorCodex
	cfg.Agent.Vendor = &vendor
	cfg.Providers = []config.ProviderConfig{{ID: "fj", Kind: config.ProviderKindForgejo, BaseURL: fixture.server.URL + "/forge", TokenEnv: stringPtr("FORGEJO_WEBHOOK_CONTRACT_TOKEN")}}
	cfg.Projects = []config.ProjectRefConfig{{ID: "forgejo-project", Name: "Forgejo", Provider: "fj", Repo: "acme/app", RepoPath: root}}
	config.ApplyForgejoProjectProfile(&cfg.Projects[0])
	coordinator := openMigratedCoordinator(t, cfg.Storage.DBPath, filepath.Join(root, "backups"))
	t.Cleanup(func() { _ = coordinator.Close() })
	repos := storage.NewRepositories(coordinator.DB())
	metadata := `{"provider":"fj","repo":"acme/app"}`
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: cfg.Projects[0].ID, Name: "Forgejo", RepoPath: root, MetadataJSON: &metadata, CreatedAt: "2026-09-14T00:00:00Z", UpdatedAt: "2026-09-14T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	return cfg, coordinator, repos
}

func sendForgejoWebhook(t *testing.T, rt *webhookRuntime, cfg config.Config, key, event, coarse, delivery, body, signatureSecret string) *httptest.ResponseRecorder {
	t.Helper()
	target := webhookTunnelManagedURL(cfg, key)
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	for _, prefix := range []string{"X-Forgejo-", "X-Gitea-", "X-GitHub-"} {
		req.Header.Set(prefix+"Event", coarse)
		req.Header.Set(prefix+"Event-Type", event)
		req.Header.Set(prefix+"Delivery", delivery)
	}
	req.Header.Set("X-Forgejo-Signature", strings.TrimPrefix(testGitHubSignature(signatureSecret, []byte(body)), "sha256="))
	req.Header.Set("X-Hub-Signature-256", testGitHubSignature(signatureSecret, []byte(body)))
	response := httptest.NewRecorder()
	(&webhookTunnelServer{runtime: rt}).ServeHTTP(response, req)
	return response
}

func TestForgejoWebhookIngressCreatesRoleQueuesWithoutPollingOrGH(t *testing.T) {
	fixture := newForgejoWebhookFixture(t)
	cfg, coordinator, repos := forgejoWebhookTestConfig(t, fixture)
	var wakes atomic.Int64
	handlers := buildDefaultSchedulerHandlers(cfg, &testLogger{}, coordinator, repos, nil, nil, nil, nil, func() { wakes.Add(1) }, time.Now, nil)
	t.Cleanup(handlers.webhook.Close)
	rt := newWebhookRuntime(cfg, &testLogger{}, time.Now)
	t.Cleanup(rt.Stop)
	rt.forwarder = func() WebhookForwarder { return handlers.webhook }
	if err := rt.Reconcile(repos); err != nil {
		t.Fatal(err)
	}
	if status := rt.Status(); status.Degraded || len(status.TunnelHooks) != 1 || len(status.Forwarders) != 0 {
		t.Fatalf("Forgejo-only runtime = %#v", status)
	}
	key := config.WebhookRepositoryKey(cfg, cfg.Projects[0])
	record, _, _ := repos.WebhookTunnelHooks.Get(context.Background(), key)
	secret, err := readWebhookTunnelSecret(cfg.Storage.DBPath, record.SecretRef)
	if err != nil {
		t.Fatal(err)
	}
	for index, tc := range []struct{ event, coarse, body string }{
		{"issue_label", "issues", `{"action":"label_updated","issue":{"number":7},"repository":{"full_name":"acme/app"}}`},
		{"issue_assign", "issues", `{"action":"assigned","issue":{"number":9},"repository":{"full_name":"acme/app"}}`},
		{"issue_label", "issues", `{"action":"label_updated","issue":{"number":10},"repository":{"full_name":"acme/app"}}`},
		{"issue_assign", "issues", `{"action":"unassigned","issue":{"number":11},"repository":{"full_name":"acme/app"}}`},
		{"issues", "issues", `{"action":"closed","issue":{"number":12},"repository":{"full_name":"acme/app"}}`},
		{"issues", "issues", `{"action":"opened","issue":{"number":13},"repository":{"full_name":"acme/app"}}`},
		{"pull_request_review_request", "pull_request", `{"action":"review_requested","pull_request":{"number":42},"repository":{"full_name":"acme/app"}}`},
		{"pull_request_review_rejected", "pull_request_rejected", `{"action":"reviewed","pull_request":{"number":43},"review":{"type":"pull_request_review_rejected","content":"fix the bug"},"repository":{"full_name":"acme/app"}}`},
	} {
		response := sendForgejoWebhook(t, rt, cfg, key, tc.event, tc.coarse, strconv.Itoa(index), tc.body, secret)
		if response.Code != 202 {
			t.Fatalf("delivery %s = %d %s", tc.event, response.Code, response.Body.String())
		}
	}
	waitForgejoRuntimeForwarder(t, handlers.webhook, 8)
	loops, err := repos.Loops.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	types := []string{}
	for _, loop := range loops {
		types = append(types, loop.Type)
	}
	sort.Strings(types)
	if !reflect.DeepEqual(types, []string{"fixer", "planner", "reviewer", "worker"}) {
		t.Fatalf("webhook-created loops = %#v (types %v)", loops, types)
	}
	queue, err := repos.Queue.ListQueued(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(queue) != 4 || wakes.Load() < 4 {
		t.Fatalf("queue=%d wakes=%d, want each role enqueued and claimer woken", len(queue), wakes.Load())
	}
	// An Actions completion has a nested repository and reuses the real PR
	// discovery path; it must not duplicate existing loops or queue entries.
	response := sendForgejoWebhook(t, rt, cfg, key, "action_run_success", "action_run_success", "actions", `{"action":"success","run":{"repository":{"full_name":"acme/app"},"commit_sha":"head-42","prettyref":"#42"}}`, secret)
	if response.Code != 202 {
		t.Fatalf("Actions = %d %s", response.Code, response.Body.String())
	}
	waitForgejoRuntimeForwarder(t, handlers.webhook, 9)
	response = sendForgejoWebhook(t, rt, cfg, key, "push", "push", "base-update", `{"ref":"refs/heads/main","after":"new-base","repository":{"full_name":"acme/app"}}`, secret)
	if response.Code != 202 {
		t.Fatalf("base push = %d %s", response.Code, response.Body.String())
	}
	waitForgejoRuntimeForwarder(t, handlers.webhook, 10)
	response = sendForgejoWebhook(t, rt, cfg, key, "action_run_failure", "action_run_failure", "pr-actions", `{"run":{"repository":{"full_name":"acme/app"},"event_payload":"{\"pull_request\":{\"number\":43}}"}}`, secret)
	if response.Code != 202 {
		t.Fatalf("PR Actions = %d %s", response.Code, response.Body.String())
	}
	waitForgejoRuntimeForwarder(t, handlers.webhook, 11)
	loops, _ = repos.Loops.List(context.Background())
	if len(loops) != 4 {
		t.Fatalf("duplicate loops after Actions: %d", len(loops))
	}
}

func waitForgejoRuntimeForwarder(t *testing.T, f WebhookForwarder, want int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		stats := f.Stats()
		if stats.ExecutionsFailed > 0 {
			t.Fatalf("webhook discovery failed: %#v", stats.RecentOutcomes)
		}
		if stats.ExecutionsSucceeded >= want && stats.InFlight == 0 && stats.Queued == 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("webhook discovery timed out: %#v", f.Stats())
}

func TestForgejoWebhookLifecycleAndSignedIngress(t *testing.T) {
	fixture := newForgejoWebhookFixture(t)
	cfg, _, repos := forgejoWebhookTestConfig(t, fixture)
	rt := newWebhookRuntime(cfg, &testLogger{}, time.Now)
	t.Cleanup(rt.Stop)
	forwarder := &testTunnelForwarder{result: webhookforward.ForwardResult{Status: "accepted", WorkItems: 1}}
	rt.forwarder = func() WebhookForwarder { return forwarder }
	ctx := context.Background()
	key := config.WebhookRepositoryKey(cfg, cfg.Projects[0])
	if err := rt.Reconcile(repos); err != nil {
		t.Fatal(err)
	}
	record, _, _ := repos.WebhookTunnelHooks.Get(ctx, key)
	secret, _ := readWebhookTunnelSecret(cfg.Storage.DBPath, record.SecretRef)
	if fixture.creates != 1 || record.HookID == 0 {
		t.Fatalf("initial registration = %#v", record)
	}
	if err := rt.Reconcile(repos); err != nil {
		t.Fatal(err)
	}
	if fixture.creates != 1 || fixture.patches != 0 {
		t.Fatalf("expanded events/hidden secret caused churn: creates=%d patches=%d", fixture.creates, fixture.patches)
	}
	body := `{"action":"reviewed","pull_request":{"number":42},"repository":{"full_name":"acme/app"}}`
	for _, tc := range []struct {
		name, body, secret string
		want               int
	}{
		{"valid", body, secret, 202}, {"wrong secret", body, "wrong", 401}, {"wrong repo", strings.Replace(body, "acme/app", "acme/other", 1), secret, 400}, {"missing repo", `{"pull_request":{"number":42}}`, secret, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			forwarder.reset()
			response := sendForgejoWebhook(t, rt, cfg, key, "pull_request_review_comment", "pull_request_comment", tc.name, tc.body, tc.secret)
			if response.Code != tc.want {
				t.Fatalf("response=%d %s", response.Code, response.Body.String())
			}
			if tc.want != 202 && forwarder.calls != 0 {
				t.Fatal("rejected request reached discovery")
			}
			if tc.want == 202 && (forwarder.lastRequest.EventType != "pull_request_review_comment" || forwarder.lastRequest.Repository == nil || forwarder.lastRequest.Repository.BaseURL != cfg.Providers[0].BaseURL) {
				t.Fatalf("source binding lost: %#v", forwarder.lastRequest)
			}
		})
	}
	// Re-enable by ID, preserving a secret which GET cannot disclose.
	fixture.mu.Lock()
	hook := fixture.hooks[record.HookID]
	hook.Active = false
	fixture.hooks[record.HookID] = hook
	fixture.mu.Unlock()
	if err := rt.Reconcile(repos); err != nil {
		t.Fatal(err)
	}
	if fixture.patches != 1 || fixture.secrets[record.HookID] != secret {
		t.Fatal("reactivation changed signing secret")
	}
	// Missing Actions subscriptions need replacement: stable Forgejo PATCH
	// ignores those flags. The new hook keeps the local signing secret.
	fixture.mu.Lock()
	hook = fixture.hooks[record.HookID]
	hook.Events = []string{"push"}
	fixture.hooks[record.HookID] = hook
	fixture.mu.Unlock()
	if err := rt.Reconcile(repos); err != nil {
		t.Fatal(err)
	}
	replaced, _, _ := repos.WebhookTunnelHooks.Get(ctx, key)
	if replaced.HookID == record.HookID || fixture.secrets[replaced.HookID] != secret || fixture.deletes != 1 {
		t.Fatalf("subscription repair = %#v", replaced)
	}
	rt.Stop()
	restarted := newWebhookRuntime(cfg, &testLogger{}, time.Now)
	t.Cleanup(restarted.Stop)
	restarted.forwarder = func() WebhookForwarder { return forwarder }
	if err := restarted.Reconcile(repos); err != nil {
		t.Fatal(err)
	}
	if fixture.creates != 2 {
		t.Fatal("restart duplicated remote webhook")
	}
	if response := sendForgejoWebhook(t, restarted, cfg, key, "pull_request_sync", "pull_request", "restart", body, secret); response.Code != 202 {
		t.Fatalf("restart delivery=%d", response.Code)
	}
	removed := config.CloneConfig(cfg)
	removed.Projects = nil
	restarted.updateConfig(removed)
	if err := restarted.Reconcile(repos); err != nil {
		t.Fatal(err)
	}
	orphan, _, _ := repos.WebhookTunnelHooks.Get(ctx, key)
	if !orphan.Orphaned || fixture.deletes != 1 {
		t.Fatal("project removal must retain remote hook and local orphan")
	}
	request := httptest.NewRequest(http.MethodPost, webhookTunnelManagedURL(cfg, key), bytes.NewBufferString(body))
	response := httptest.NewRecorder()
	(&webhookTunnelServer{runtime: restarted}).ServeHTTP(response, request)
	if response.Code != 404 {
		t.Fatalf("removed project ingress=%d", response.Code)
	}
}

func TestForgejoWebhookIngressRetryAndGiteaHeaders(t *testing.T) {
	fixture := newForgejoWebhookFixture(t)
	cfg, _, repos := forgejoWebhookTestConfig(t, fixture)
	initial := config.CloneConfig(cfg)
	initial.Providers[0].BaseURL = "https://old.example"
	rt := newWebhookRuntime(initial, &testLogger{}, time.Now)
	t.Cleanup(rt.Stop)
	// Catalog publication must include newly configured provider credentials
	// and URLs, not only the project slice.
	rt.updateConfig(cfg)
	forwarder := &testTunnelForwarder{result: webhookforward.ForwardResult{Status: "accepted", WorkItems: 1}}
	rt.forwarder = func() WebhookForwarder { return forwarder }
	if err := rt.Reconcile(repos); err != nil {
		t.Fatal(err)
	}
	key := config.WebhookRepositoryKey(cfg, cfg.Projects[0])
	record, found, _ := repos.WebhookTunnelHooks.Get(context.Background(), key)
	if !found {
		t.Fatalf("new catalog provider was not reconciled: %#v", rt.Status())
	}
	secret, _ := readWebhookTunnelSecret(cfg.Storage.DBPath, record.SecretRef)
	body := `{"action":"label_updated","issue":{"number":7},"repository":{"full_name":"acme/app"}}`
	for _, refusal := range []error{webhookforward.ErrQueueFull, webhookforward.ErrAdmissionRefused} {
		forwarder.err = refusal
		response := sendForgejoWebhook(t, rt, cfg, key, "issue_label", "issues", "retry", body, secret)
		if response.Code != http.StatusServiceUnavailable {
			t.Fatalf("%v response=%d", refusal, response.Code)
		}
	}
	forwarder.reset()
	req := httptest.NewRequest(http.MethodPost, record.ManagedURL, strings.NewReader(body))
	req.Header.Set("X-Gitea-Signature", strings.TrimPrefix(testGitHubSignature(secret, []byte(body)), "sha256="))
	req.Header.Set("X-Gitea-Event-Type", "issue_label")
	req.Header.Set("X-Gitea-Event", "issues")
	req.Header.Set("X-Gitea-Delivery", "gitea-compatible")
	response := httptest.NewRecorder()
	(&webhookTunnelServer{runtime: rt}).ServeHTTP(response, req)
	if response.Code != 202 || forwarder.lastRequest.EventType != "issue_label" || forwarder.lastRequest.Repository == nil {
		t.Fatalf("Gitea-compatible headers lost target: response=%d request=%#v", response.Code, forwarder.lastRequest)
	}
	forwarder.reset()
	rt.allowForward = func() error { return errors.New("daemon stopping") }
	response = sendForgejoWebhook(t, rt, cfg, key, "issue_label", "issues", "shutdown", body, secret)
	if response.Code != 503 || forwarder.calls != 0 {
		t.Fatal("shutdown must refuse ingress before discovery")
	}
}

func TestForgejoWebhookSubscriptionRepairRespectsDisableLatch(t *testing.T) {
	fixture := newForgejoWebhookFixture(t)
	cfg, _, repos := forgejoWebhookTestConfig(t, fixture)
	rt := newWebhookRuntime(cfg, &testLogger{}, time.Now)
	t.Cleanup(rt.Stop)
	if err := rt.Reconcile(repos); err != nil {
		t.Fatal(err)
	}
	key := config.WebhookRepositoryKey(cfg, cfg.Projects[0])
	record, _, _ := repos.WebhookTunnelHooks.Get(context.Background(), key)
	record.ConsecutiveDisables = webhookTunnelDisableLatchThreshold - 1
	now := time.Now().UnixNano()
	record.LastDisableAt = &now
	if err := repos.WebhookTunnelHooks.Upsert(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	fixture.mu.Lock()
	hook := fixture.hooks[record.HookID]
	hook.Active, hook.Events = false, []string{"push"}
	fixture.hooks[record.HookID] = hook
	fixture.mu.Unlock()
	if err := rt.Reconcile(repos); err != nil {
		t.Fatal(err)
	}
	current, _, _ := repos.WebhookTunnelHooks.Get(context.Background(), key)
	if fixture.creates != 1 || fixture.patches != 0 || current.ConsecutiveDisables != webhookTunnelDisableLatchThreshold || !rt.Status().Degraded {
		t.Fatalf("subscription drift bypassed disable latch: %#v", rt.Status())
	}
}

func TestForgejoWebhookCatalogDoesNotRetargetQueuedDelivery(t *testing.T) {
	fixture := newForgejoWebhookFixture(t)
	cfg, coordinator, repos := forgejoWebhookTestConfig(t, fixture)
	identity, _ := config.ProjectRepositoryIdentity(cfg, cfg.Projects[0])
	source := projects.NewCatalog(cfg)
	handlers := buildCatalogSchedulerHandlers(source, nil, "", &testLogger{}, coordinator, repos, nil, nil, nil, nil, nil, time.Now, nil, nil, nil, nil)
	t.Cleanup(handlers.webhook.Close)
	// Rebind the project after the event was accepted. Both instances use
	// acme/app; only a full identity comparison can distinguish this change.
	rebound := config.CloneConfig(cfg)
	rebound.Providers[0].BaseURL = "https://replacement.example"
	source.PublishGlobals(rebound)
	input := handlers.snapshot().input(Services{Repositories: repos, Coordinator: coordinator})
	for _, object := range []string{"issues", "pull_request", "repository", "base_branch"} {
		err := discoverWebhookProject(context.Background(), input, webhookforward.ProjectDiscovery{ProjectID: cfg.Projects[0].ID, Repo: identity.Repo, RepositoryKey: identity.Key(), ObjectType: object, Number: 7, Branch: "main", Lanes: []webhookforward.Lane{webhookforward.LanePlanner, webhookforward.LaneWorker, webhookforward.LaneReviewer, webhookforward.LaneFixer}})
		if err != nil {
			t.Fatal(err)
		}
	}
	queue, _ := repos.Queue.ListQueued(context.Background(), 100)
	if len(queue) != 0 || len(fixture.reads) != 0 {
		t.Fatal("stale delivery was discovered after project rebind")
	}
}

func TestForgejoWebhookReconcileCannotOverwriteConcurrentRotation(t *testing.T) {
	for _, drift := range []string{"inactive", "subscription", "url"} {
		t.Run(drift, func(t *testing.T) {
			fixture := newForgejoWebhookFixture(t)
			cfg, _, repos := forgejoWebhookTestConfig(t, fixture)
			rt := newWebhookRuntime(cfg, &testLogger{}, time.Now)
			t.Cleanup(rt.Stop)
			if err := rt.Reconcile(repos); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			key := config.WebhookRepositoryKey(cfg, cfg.Projects[0])
			original, _, _ := repos.WebhookTunnelHooks.Get(ctx, key)
			fixture.mu.Lock()
			hook := fixture.hooks[original.HookID]
			switch drift {
			case "inactive":
				hook.Active = false
			case "subscription":
				hook.Events = []string{"push"}
			case "url":
				hook.Config.URL = "https://unexpected.example/hook"
			}
			fixture.hooks[original.HookID] = hook
			fixture.onGetHook = func(id int64) {
				fixture.onGetHook = nil
				rotated := original
				rotated.HookID = 999
				rotated.SecretRef = "rotated-secret.key"
				if err := repos.WebhookTunnelHooks.SaveIfCurrentHookID(ctx, rotated, id); err != nil {
					t.Error(err)
				}
			}
			fixture.mu.Unlock()
			if err := rt.Reconcile(repos); err != nil {
				t.Fatal(err)
			}
			current, _, _ := repos.WebhookTunnelHooks.Get(ctx, key)
			if current.HookID != 999 || current.SecretRef != "rotated-secret.key" || current.Orphaned {
				t.Fatalf("rotation overwritten: %#v", current)
			}
			if !rt.Status().Degraded {
				t.Fatal("concurrent change must be surfaced for the next reconcile")
			}
		})
	}
}

func TestForgejoWebhookReconcileRepairsEmptyRecordedID(t *testing.T) {
	fixture := newForgejoWebhookFixture(t)
	cfg, _, repos := forgejoWebhookTestConfig(t, fixture)
	key := config.WebhookRepositoryKey(cfg, cfg.Projects[0])
	if err := repos.WebhookTunnelHooks.Upsert(context.Background(), storage.WebhookTunnelHookRecord{Repo: key, HookID: 0, CreatedAt: 1, UpdatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	rt := newWebhookRuntime(cfg, &testLogger{}, time.Now)
	t.Cleanup(rt.Stop)
	if err := rt.Reconcile(repos); err != nil {
		t.Fatal(err)
	}
	record, _, _ := repos.WebhookTunnelHooks.Get(context.Background(), key)
	if record.HookID == 0 || fixture.creates != 1 || fixture.deletes != 0 || rt.Status().Degraded {
		t.Fatalf("empty hook ID was not repaired: %#v", rt.Status())
	}
}
