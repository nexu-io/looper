package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/domain"
	"github.com/nexu-io/looper/internal/projects"
	"github.com/nexu-io/looper/internal/storage"
)

func TestConfigGetExposesHostingCredentialReferencesOnly(t *testing.T) {
	cfg := testConfigRouteConfig(t)
	cfg.Identities = map[string]config.HostingIdentityConfig{
		"bot": {Kind: config.HostingIdentityForgejoToken, BaseURL: "https://code.example", TokenEnv: "HOSTING_BOT_TOKEN"},
		"app": {Kind: config.HostingIdentityGitHubApp, AppID: 1, InstallationID: 2, PrivateKeyFile: "/private/app.pem"},
	}
	cfg.Agent.Env = map[string]string{"HOSTING_BOT_TOKEN": "secret-agent-token"}
	cfg.Daemon.Environment = map[string]string{"HOSTING_BOT_TOKEN": "secret-daemon-token"}
	cfg.Roles.Reviewer.Identity = "app"
	response := httptest.NewRecorder()
	NewHandler(Context{Config: cfg}).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/config", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	for _, secret := range []string{"secret-agent-token", "secret-daemon-token"} {
		if strings.Contains(body, secret) {
			t.Fatalf("config response leaked credential value: %s", body)
		}
	}
	data := parseJSONMap(t, response.Body.Bytes())["data"].(map[string]any)
	identities := data["identities"].(map[string]any)
	assertEqual(t, identities["bot"].(map[string]any)["tokenEnv"], "HOSTING_BOT_TOKEN")
	assertEqual(t, identities["app"].(map[string]any)["privateKeyFile"], "/private/app.pem")
	assertEqual(t, data["roles"].(map[string]any)["reviewer"].(map[string]any)["identity"], "app")
}

func TestManualCreateUsesSelectedBotBeforeCheckingHolds(t *testing.T) {
	fixture := newTestFixture(t)
	repoPath := t.TempDir()
	metadata := `{"repo":"acme/looper"}`
	if err := fixture.runtime.Services().Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "project_1", Name: "Bot", RepoPath: repoPath, MetadataJSON: &metadata, CreatedAt: fixture.now.Format(javaScriptISOString), UpdatedAt: fixture.now.Format(javaScriptISOString)}); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "personal-gh-called")
	ghPath := filepath.Join(t.TempDir(), "gh")
	script := "#!/bin/sh\n: > '" + strings.ReplaceAll(marker, "'", "'\"'\"'") + "'\nexit 3\n"
	if err := os.WriteFile(ghPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GH_TOKEN", "personal-token-must-not-be-used")
	cfg := fixture.config
	cfg.Tools.GHPath = &ghPath
	cfg.Projects = []config.ProjectRefConfig{{ID: "project_1", RepoPath: repoPath, Repo: "acme/looper", Identity: "default-bot"}}
	cfg.Identities = map[string]config.HostingIdentityConfig{
		"default-bot": {Kind: config.HostingIdentityGitHubApp, AppID: 1, InstallationID: 2, PrivateKeyFile: filepath.Join(t.TempDir(), "missing.pem")},
		"review-bot":  {Kind: config.HostingIdentityGitHubApp, AppID: 3, InstallationID: 4, PrivateKeyFile: filepath.Join(t.TempDir(), "missing.pem")},
	}
	cfg.Roles.Reviewer.Identity = "review-bot"
	handler := NewHandler(Context{Config: cfg, Runtime: runtimeWithConfig(fixture.runtime, cfg)})
	for _, tc := range []struct{ role, path, body, identity string }{
		{"planner", "/api/v1/planners", `{"projectId":"project_1","issueNumber":77}`, "default-bot"},
		{"worker", "/api/v1/workers", `{"projectId":"project_1","repo":"acme/looper","issueNumber":77,"baseBranch":"main"}`, "default-bot"},
		{"reviewer", "/api/v1/loops", `{"projectId":"project_1","type":"reviewer","targetType":"pull_request","repo":"acme/looper","prNumber":42}`, "review-bot"},
		{"fixer", "/api/v1/loops", `{"projectId":"project_1","type":"fixer","targetType":"pull_request","repo":"acme/looper","prNumber":42}`, "default-bot"},
	} {
		t.Run(tc.role, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body)))
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), tc.identity) {
				t.Fatalf("manual create status = %d, body = %s; want selected identity failure", response.Code, response.Body.String())
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatal("manual bot hold check invoked personal gh")
			}
		})
	}
	queue, err := fixture.runtime.Services().Repositories.Queue.List(context.Background())
	if err != nil || len(queue) != 0 {
		t.Fatalf("manual authentication failures enqueued work: count=%d, error=%v", len(queue), err)
	}
}

func TestManualForgejoHoldCheckUsesRoleBotInsteadOfTea(t *testing.T) {
	fixture := newTestFixture(t)
	var reads atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "token selected-review-token" {
			t.Error("manual hold request used the wrong Forgejo identity")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/v1/user":
			fmt.Fprint(w, `{"id":42,"login":"review-bot","full_name":"Review Bot","email":"review@example.com"}`)
		case "/api/v1/repos/acme/looper":
			fmt.Fprint(w, `{"id":7,"full_name":"acme/looper"}`)
		case "/api/v1/repos/acme/looper/pulls/42":
			reads.Add(1)
			fmt.Fprint(w, `{"number":42,"state":"open","labels":[{"name":"looper:hold"}]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	t.Setenv("API_ROLE_BOT_TOKEN", "selected-review-token")
	cfg := fixture.config
	cfg.Providers = []config.ProviderConfig{{ID: "forgejo", Kind: config.ProviderKindForgejo, BaseURL: server.URL, Auth: config.ProviderAuthTea, TeaLogin: stringPtr("must-not-use-personal-tea")}}
	cfg.Projects = []config.ProjectRefConfig{{ID: "project_1", RepoPath: t.TempDir(), Provider: "forgejo", Repo: "acme/looper", Identity: "project-bot"}}
	cfg.Identities = map[string]config.HostingIdentityConfig{
		"role-bot":    {Kind: config.HostingIdentityForgejoToken, BaseURL: server.URL, TokenEnv: "API_ROLE_BOT_TOKEN"},
		"project-bot": {Kind: config.HostingIdentityForgejoToken, BaseURL: server.URL, TokenEnv: "UNUSED_API_PROJECT_TOKEN"},
	}
	cfg.Roles.Reviewer.Identity = "role-bot"
	metadata := `{"repo":"acme/looper","provider":"forgejo"}`
	if err := fixture.runtime.Services().Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "project_1", Name: "Bot", RepoPath: cfg.Projects[0].RepoPath, MetadataJSON: &metadata, CreatedAt: fixture.now.Format(javaScriptISOString), UpdatedAt: fixture.now.Format(javaScriptISOString)}); err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(Context{Config: cfg, Runtime: runtimeWithConfig(fixture.runtime, cfg)})
	err := handler.validateManualHoldBypassForLoopTarget(context.Background(), "project_1", domain.LoopTypeReviewer, domain.LoopTarget{TargetType: domain.LoopTargetTypePullRequest, Repo: "acme/looper", PRNumber: 42}, false)
	if err == nil || !strings.Contains(err.Error(), "--force") || reads.Load() != 1 {
		t.Fatalf("selected Forgejo hold check: reads=%d, error=%v", reads.Load(), err)
	}
}

func TestProjectCreatePassesHostingIdentitySelectionIncludingExplicitClearing(t *testing.T) {
	cfg := testConfigRouteConfig(t)
	h := NewHandler(Context{Config: cfg})
	for _, name := range []string{"bot", ""} {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/projects", strings.NewReader(`{"repoPath":"/tmp/project","identity":"`+name+`"}`))
		called := false
		_, err := h.buildCreateProjectResponse(req, fakeProjectService{addProject: func(_ context.Context, input projects.AddInput) (projects.AddResult, error) {
			called = true
			if input.Identity == nil || *input.Identity != name {
				t.Fatalf("lost hosting selection: %#v", input)
			}
			return projects.AddResult{}, nil
		}})
		if err != nil || !called {
			t.Fatalf("create request = %v, called=%v", err, called)
		}
	}
}
