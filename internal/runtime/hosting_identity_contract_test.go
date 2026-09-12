package runtime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/fixer"
	"github.com/nexu-io/looper/internal/hostingidentity"
	"github.com/nexu-io/looper/internal/planner"
	"github.com/nexu-io/looper/internal/reviewer"
	"github.com/nexu-io/looper/internal/storage"
	"github.com/nexu-io/looper/internal/worker"
)

// Exercise the public discovery entrypoints, actor filters, real scheduler
// adapters and native HTTP transport together. A personal tea login is
// deliberately unusable so any fallback fails the contract.
func TestHostingIdentityRoleDiscoveryUsesSelectedForgejoAccount(t *testing.T) {
	t.Parallel()
	var denyPlanner atomic.Bool
	var mu sync.Mutex
	var filters []string
	var pullActors []string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actor := map[string]string{"token planner-token": "planner-bot", "token worker-token": "worker-bot", "token reviewer-token": "reviewer-bot", "token fixer-token": "fixer-bot"}[r.Header.Get("Authorization")]
		if actor == "" {
			t.Errorf("unexpected authentication on %s", r.URL.Path)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/v1/user":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": len(actor), "login": actor, "email": actor + "@example.com"})
		case "/api/v1/repos/acme/looper":
			_ = json.NewEncoder(w).Encode(map[string]any{"full_name": "acme/looper"})
		case "/swagger.v1.json":
			_, _ = w.Write([]byte(`{"paths":{"/repos/{owner}/{repo}/pulls/{index}/requested_reviewers":{"post":{}}}}`))
		case "/api/v1/repos/acme/looper/pulls":
			mu.Lock()
			pullActors = append(pullActors, actor)
			mu.Unlock()
			_, _ = w.Write([]byte(`[]`))
		case "/api/v1/repos/acme/looper/pulls/42":
			mu.Lock()
			pullActors = append(pullActors, "targeted:"+actor)
			mu.Unlock()
			http.Error(w, "scoped pull permission denied", http.StatusForbidden)
		case "/api/v1/repos/acme/looper/issues":
			mu.Lock()
			filters = append(filters, actor+":"+r.URL.Query().Get("assignee"))
			mu.Unlock()
			if denyPlanner.Load() && actor == "planner-bot" {
				http.Error(w, "denied planner-token", http.StatusForbidden)
				return
			}
			_ = json.NewEncoder(w).Encode([]any{map[string]any{"number": 27, "title": "Implement selected identity", "state": "open", "assignees": []any{map[string]any{"login": actor}}, "labels": []any{map[string]any{"name": "ready"}}}})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	cfg.Providers = []config.ProviderConfig{{ID: "forgejo", Kind: config.ProviderKindForgejo, BaseURL: server.URL, Auth: config.ProviderAuthTea, TeaLogin: stringPtr("personal"), TeaPath: stringPtr("/missing/tea")}}
	cfg.Identities = map[string]config.HostingIdentityConfig{
		"planner":  {Kind: config.HostingIdentityForgejoToken, BaseURL: server.URL, TokenEnv: "PLANNER_IDENTITY"},
		"worker":   {Kind: config.HostingIdentityForgejoToken, BaseURL: server.URL, TokenEnv: "WORKER_IDENTITY"},
		"reviewer": {Kind: config.HostingIdentityForgejoToken, BaseURL: server.URL, TokenEnv: "REVIEWER_IDENTITY"},
		"fixer":    {Kind: config.HostingIdentityForgejoToken, BaseURL: server.URL, TokenEnv: "FIXER_IDENTITY"},
	}
	cfg.Projects = []config.ProjectRefConfig{{ID: "project", Provider: "forgejo", Repo: "acme/looper", RepoPath: root, Identity: "planner"}}
	cfg.Roles.Planner.AutoDiscovery, cfg.Roles.Worker.AutoDiscovery = true, true
	cfg.Roles.Worker.Identity = "worker"
	cfg.Roles.Reviewer.Identity = "reviewer"
	cfg.Roles.Fixer.Identity = "fixer"
	cfg.Roles.Reviewer.Discovery.AutoDiscovery = true
	cfg.Roles.Reviewer.Discovery.SpecReview.IncludeReviewingLabel = false
	cfg.Roles.Reviewer.Discovery.Triggers.RequireReviewRequest = true
	cfg.Roles.Fixer.AutoDiscovery = true
	cfg.Roles.Fixer.Triggers.AuthorFilter = config.FixerAuthorFilterCurrentUser
	cfg.Roles.Planner.Triggers = config.IssueRoleTriggersConfig{Labels: []string{"ready"}, LabelMode: config.LabelModeAll, RequireAssigneeCurrentUser: true}
	cfg.Roles.Worker.Triggers = cfg.Roles.Planner.Triggers
	manager := hostingidentity.NewManager(hostingidentity.Options{HTTPClient: server.Client(), LookupEnv: func(name string) (string, bool) {
		value := map[string]string{"PLANNER_IDENTITY": "planner-token", "WORKER_IDENTITY": "worker-token", "REVIEWER_IDENTITY": "reviewer-token", "FIXER_IDENTITY": "fixer-token"}[name]
		return value, value != ""
	}})
	ctx := hostingidentity.WithManager(context.Background(), manager)
	db := openMigratedCoordinator(t, filepath.Join(t.TempDir(), "identities.sqlite"), t.TempDir())
	repos := storage.NewRepositories(db.DB())
	now := time.Now().UTC().Format(time.RFC3339)
	if err := repos.Projects.Upsert(ctx, storage.ProjectRecord{ID: "project", Name: "Project", RepoPath: root, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	plannerAdapter := plannerGitHubAdapter{config: &cfg}
	workerAdapter := workerGitHubAdapter{config: &cfg}
	planning := planner.New(planner.Options{DB: db.DB(), Repos: repos, GitHub: plannerAdapter, CustomInstructions: &cfg})
	working := worker.New(worker.Options{DB: db.DB(), Repos: repos, GitHub: workerAdapter, CustomInstructions: &cfg})
	plan, err := planning.DiscoverIssues(ctx, planner.DiscoveryInput{ProjectID: "project", Repo: "acme/looper"})
	if err != nil || len(plan.QueueItems) != 1 {
		t.Fatalf("planner discovery = %#v, %v", plan, err)
	}
	work, err := working.DiscoverIssues(ctx, worker.DiscoveryInput{ProjectID: "project", Repo: "acme/looper"})
	if err != nil || len(work.QueueItems) != 1 {
		t.Fatalf("worker discovery = %#v, %v", work, err)
	}
	mu.Lock()
	gotFilters := strings.Join(filters, ",")
	mu.Unlock()
	if gotFilters != "planner-bot:planner-bot,worker-bot:worker-bot" {
		t.Fatalf("role discovery actor filters = %q", gotFilters)
	}
	reviewing := reviewer.New(reviewer.Options{DB: db.DB(), Repos: repos, GitHub: reviewerGitHubAdapter{config: &cfg}, CustomInstructions: &cfg})
	fixing := fixer.New(fixer.Options{DB: db.DB(), Repos: repos, GitHub: fixerGitHubAdapter{config: &cfg}, CustomInstructions: &cfg})
	if _, err := reviewing.DiscoverPullRequests(ctx, reviewer.DiscoveryInput{ProjectID: "project", Repo: "acme/looper"}); err != nil {
		t.Fatalf("reviewer discovery: %v", err)
	}
	if _, err := fixing.DiscoverPullRequests(ctx, fixer.DiscoveryInput{ProjectID: "project", Repo: "acme/looper"}); err != nil {
		t.Fatalf("fixer discovery: %v", err)
	}
	if _, err := fixing.DiscoverPullRequestsForBaseBranchUpdate(ctx, fixer.BaseBranchDiscoveryInput{ProjectID: "project", Repo: "acme/looper", BaseRefName: "main"}); err != nil {
		t.Fatalf("fixer base-update discovery: %v", err)
	}
	if _, err := reviewing.DiscoverPullRequest(ctx, reviewer.TargetedDiscoveryInput{ProjectID: "project", Repo: "acme/looper", PRNumber: 42}); err == nil || !strings.Contains(err.Error(), `hosting identity "reviewer"`) {
		t.Fatalf("targeted reviewer failure lost selected identity: %v", err)
	}
	if _, err := fixing.DiscoverPullRequest(ctx, fixer.TargetedDiscoveryInput{ProjectID: "project", Repo: "acme/looper", PRNumber: 42}); err == nil || !strings.Contains(err.Error(), `hosting identity "fixer"`) {
		t.Fatalf("targeted fixer failure lost selected identity: %v", err)
	}
	mu.Lock()
	gotPullActors := strings.Join(pullActors, ",")
	mu.Unlock()
	if gotPullActors != "reviewer-bot,fixer-bot,fixer-bot,targeted:reviewer-bot,targeted:fixer-bot" {
		t.Fatalf("reviewer/fixer native requests escaped role selection: %s", gotPullActors)
	}

	// An already-bound run retains its definition even when the adapter's
	// live config now selects another identity. A new run observes the change.
	frozen, err := hostingidentity.Bind(ctx, cfg, "project", "worker")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Roles.Worker.Identity = "planner"
	login, err := workerAdapter.GetCurrentUserLogin(frozen, root)
	if err != nil || login != "worker-bot" {
		t.Fatalf("in-flight selection changed: %q, %v", login, err)
	}
	denyPlanner.Store(true)
	newRun := worker.New(worker.Options{DB: db.DB(), Repos: repos, GitHub: workerAdapter, CustomInstructions: &cfg})
	if _, err := newRun.DiscoverIssues(ctx, worker.DiscoveryInput{ProjectID: "project", Repo: "acme/looper"}); err == nil || !strings.Contains(err.Error(), `hosting identity "planner"`) || strings.Contains(err.Error(), "planner-token") {
		t.Fatalf("new run did not report isolated safe auth failure: %v", err)
	}
	if _, err := workerAdapter.ListOpenIssues(frozen, worker.ListOpenIssuesInput{Repo: "acme/looper", CWD: root, Assignee: "worker-bot"}); err != nil {
		t.Fatalf("other identity was affected by planner denial: %v", err)
	}
}
