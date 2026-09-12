package coordinator

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/agent"
	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/hostingidentity"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/infra/shell"
)

func TestHostingIdentityCoordinatorDiscoveryBindsBeforeGitHubTransport(t *testing.T) {
	t.Parallel()
	_, repos, cfg, repoPath, now := coordinatorFakeGHFixture(t)
	manager, baseURL := coordinatorHostingIdentityManager(t)
	cfg.Providers = []config.ProviderConfig{{ID: "github-enterprise", Kind: config.ProviderKindGitHub, BaseURL: baseURL}}
	cfg.Projects = []config.ProjectRefConfig{{ID: "demo", Repo: "acme/looper", RepoPath: repoPath, Provider: "github-enterprise"}}
	cfg.Identities = map[string]config.HostingIdentityConfig{"coordinator-bot": {Kind: config.HostingIdentityGitHubApp, BaseURL: baseURL, AppID: 1, InstallationID: 2, PrivateKeyFile: "/captured.pem"}}
	cfg.Roles.Coordinator.Enabled = true
	cfg.Roles.Coordinator.Identity = "coordinator-bot"
	issueCalls := 0
	gateway := githubinfra.New(githubinfra.Options{GHRun: func(ctx context.Context, options shell.Options) (shell.Result, error) {
		session, selected := hostingidentity.FromContext(ctx)
		if !selected || session.Name() != "coordinator-bot" || session.Role() != "coordinator" || session.ProjectID() != "demo" || options.Env["GH_ENTERPRISE_TOKEN"] != "coordinator-installation" {
			t.Fatal("coordinator reached GitHub before capturing its selected identity")
		}
		args := strings.Join(options.Args, " ")
		if strings.HasPrefix(args, "issue list ") {
			issueCalls++
			return shell.Result{Stdout: `[]`}, nil
		}
		if strings.HasPrefix(args, "pr list ") {
			return shell.Result{Stdout: `[]`}, nil
		}
		return shell.Result{}, fmt.Errorf("unexpected coordinator command %s", args)
	}})
	runner := New(Options{Repos: repos, GitHub: gateway, Config: &cfg, Now: func() time.Time { return now }})
	result, err := runner.DiscoverIssues(hostingidentity.WithManager(context.Background(), manager), DiscoveryInput{ProjectID: "demo", Repo: "acme/looper"})
	if err != nil || !result.Ticked || issueCalls != 1 {
		t.Fatalf("coordinator bot discovery = %#v, %v; issue calls %d", result, err, issueCalls)
	}
}

func coordinatorHostingIdentityManager(t *testing.T) (*hostingidentity.Manager, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v3/app":
			_, _ = w.Write([]byte(`{"id":1,"slug":"coordinator-app"}`))
		case "/api/v3/app/installations/2/access_tokens":
			_, _ = fmt.Fprintf(w, `{"token":"coordinator-installation","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
		case "/api/v3/repos/acme/looper":
			_, _ = w.Write([]byte(`{"full_name":"acme/looper"}`))
		case "/api/v3/users/coordinator-app[bot]":
			_, _ = w.Write([]byte(`{"id":42,"login":"coordinator-app[bot]","email":"coordinator@example.com"}`))
		default:
			t.Errorf("unexpected credential request %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	manager := hostingidentity.NewManager(hostingidentity.Options{ReadFile: func(string) ([]byte, error) { return keyPEM, nil }, HTTPClient: server.Client()})
	return manager, server.URL
}

func TestHostingIdentityCoordinatorDiscoveryExecutesTriage(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"legacy", "bot"} {
		t.Run(mode, func(t *testing.T) {
			fixture := newCoordinatorFixture(t)
			cfg := fixture.cfg
			cfg.Roles.Coordinator.Enabled = true
			cfg.Projects[0].Repo = "acme/looper"
			fixture.github.issues = []githubinfra.IssueSummary{{Number: 42}}
			fixture.github.details[42] = githubinfra.IssueDetail{Number: 42, Title: "Triage executor regression", Author: "octo", CreatedAt: fixture.now.Format(time.RFC3339)}
			ctx := context.Background()
			if mode == "bot" {
				manager, baseURL := coordinatorHostingIdentityManager(t)
				ctx = hostingidentity.WithManager(ctx, manager)
				cfg.Providers = []config.ProviderConfig{{ID: "github-enterprise", Kind: config.ProviderKindGitHub, BaseURL: baseURL}}
				cfg.Projects[0].Provider = "github-enterprise"
				cfg.Identities = map[string]config.HostingIdentityConfig{"coordinator-bot": {Kind: config.HostingIdentityGitHubApp, BaseURL: baseURL, AppID: 1, InstallationID: 2, PrivateKeyFile: "/captured.pem"}}
				cfg.Roles.Coordinator.Identity = "coordinator-bot"
			}
			looper, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			vendor := config.AgentVendor("custom")
			command := `set -e
case "$LOOPER_PROMPT" in *"Triage executor regression"*) ;; *) exit 40 ;; esac
if [ "$EXPECTED_MODE" = bot ]; then
  test -S "$LOOPER_TRUSTED_REVIEW_SOCK"
  test -n "$LOOPER_HOST_CLI"
  test -z "$GH_TOKEN$GH_ENTERPRISE_TOKEN"
else
  test -z "$LOOPER_TRUSTED_REVIEW_SOCK$LOOPER_HOST_CLI"
fi
printf '%s\n' '{"disposition":"valid","comment":"Triage subprocess completed.","labels":{"kind":["kind/bug"],"area":["area/coordinator"],"complexity":["complexity/m"],"dispatch":["dispatch/plan"]}}'
`
			executor := agent.New(agent.ExecutorOptions{
				Config:            agent.ExecutorConfig{Vendor: vendor, Params: map[string]any{"command": "/bin/sh", "args": []any{"-c", command}}, Env: map[string]string{"EXPECTED_MODE": mode}},
				ParamsOwnerVendor: &vendor, Repos: fixture.runner.repos, HostingConfig: cfg, TrustedLooperPath: looper,
			})
			fixture.runner.triageLLM = NewAgentLLM(executor, time.Now, 10*time.Second, 10*time.Second)
			result, err := fixture.runner.DiscoverIssues(ctx, DiscoveryInput{ProjectID: fixture.projectID, Repo: "acme/looper"})
			if err != nil || !result.Ticked {
				t.Fatalf("discovery through triage executor = %#v, %v", result, err)
			}
			if len(fixture.github.createdBodies) != 1 || !strings.Contains(fixture.github.createdBodies[0], "Triage subprocess completed.") || countOperations(fixture.github.ops, "add:triaged") != 1 {
				t.Fatalf("real triage execution did not reach publication: ops=%v comments=%v", fixture.github.ops, fixture.github.createdBodies)
			}
			executions, err := fixture.runner.repos.AgentExecutions.List(ctx)
			if err != nil || len(executions) != 1 || executions[0].ProjectID == nil || *executions[0].ProjectID != fixture.projectID {
				t.Fatalf("triage executor project = %#v, %v", executions, err)
			}
		})
	}
}
