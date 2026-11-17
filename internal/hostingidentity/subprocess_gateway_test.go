package hostingidentity_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/hostingidentity"
	"github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/infra/shell"
)

func TestGitHubGatewayPublicationPreservesLiteralContentAndExplicitTargets(t *testing.T) {
	ctx := githubSubprocessContext(t)
	const repo, cwd = "org/repo", "/caller/checkout"
	for _, body := range []string{"--repo=owner/name documents a CLI flag", "-Regression details", "--web"} {
		// Preserve the existing review location policy while exercising literal
		// flag-like text at the start of the published review body.
		reviewBody := body + "\n\nfunction RunGH should preserve literal publication text."
		for _, tc := range []struct {
			name string
			args []string
			run  func(*github.Gateway) error
		}{
			{"comment", []string{"pr", "comment", "42", "--repo", repo, "--body", body}, func(g *github.Gateway) error {
				return g.AddPullRequestComment(ctx, github.PullRequestCommentInput{Repo: repo, PRNumber: 42, CWD: cwd, Body: body})
			}},
			{"review", []string{"pr", "review", "42", "--repo", repo, "--comment", "--body", reviewBody}, func(g *github.Gateway) error {
				return g.SubmitReview(ctx, github.SubmitReviewInput{Repo: repo, PRNumber: 42, CWD: cwd, Event: "COMMENT", Body: reviewBody})
			}},
			{"title", []string{"pr", "edit", "42", "--repo", repo, "--title", body}, func(g *github.Gateway) error {
				return g.UpdatePullRequestTitle(ctx, github.UpdatePullRequestTitleInput{Repo: repo, PRNumber: 42, CWD: cwd, Title: body})
			}},
			{"body", []string{"pr", "edit", "42", "--repo", repo, "--body", body}, func(g *github.Gateway) error {
				return g.UpdatePullRequestBody(ctx, github.UpdatePullRequestBodyInput{Repo: repo, PRNumber: 42, CWD: cwd, Body: body})
			}},
			{"create", []string{"pr", "create", "--repo", repo, "--head", "topic", "--base", "main", "--title", body, "--body", body, "--draft"}, func(g *github.Gateway) error {
				result, err := g.CreatePullRequest(ctx, github.CreatePullRequestInput{Repo: repo, CWD: cwd, HeadBranch: "topic", BaseBranch: "main", Title: body, Body: body, Draft: true})
				if err == nil && result.Number != 42 {
					t.Errorf("created PR number = %d, want 42", result.Number)
				}
				return err
			}},
		} {
			t.Run(tc.name+"/"+body, func(t *testing.T) {
				called := false
				gateway := github.New(github.Options{GHRun: func(_ context.Context, options shell.Options) (shell.Result, error) {
					called = true
					assertGitHubSubprocessScope(t, options, cwd)
					if !reflect.DeepEqual(options.Args, tc.args) {
						t.Fatalf("publication content or explicit target changed: got %q, want %q", options.Args, tc.args)
					}
					return shell.Result{Stdout: "https://github.com/org/repo/pull/42"}, nil
				}})
				if err := tc.run(gateway); err != nil || !called {
					t.Fatalf("gateway publication did not reach selected CLI: called=%v, err=%v", called, err)
				}
			})
		}
	}
	// Repository detection historically used the checkout. Its selected target
	// must now come from GH_REPO while the CLI remains outside that checkout.
	gateway := github.New(github.Options{GHRun: func(_ context.Context, options shell.Options) (shell.Result, error) {
		assertGitHubSubprocessScope(t, options, cwd)
		if !reflect.DeepEqual(options.Args, []string{"repo", "view", "--json", "nameWithOwner,url"}) {
			t.Fatalf("unexpected repository detection command: %q", options.Args)
		}
		return shell.Result{Stdout: `{"nameWithOwner":"org/repo","url":"https://github.com/org/repo"}`}, nil
	}})
	if got, err := gateway.DetectCurrentRepository(ctx, cwd); err != nil || got != repo {
		t.Fatalf("selected repository detection = %q, %v", got, err)
	}
}

func assertGitHubSubprocessScope(t *testing.T, options shell.Options, callerCWD string) {
	t.Helper()
	if options.Env["GH_TOKEN"] != "gateway-test-installation-token" || options.Env["GH_REPO"] != "github.com/org/repo" || options.Env["GH_HOST"] != "github.com" {
		t.Fatal("gateway did not preserve the selected identity and repository")
	}
	if options.CWD == callerCWD || options.CWD != options.Env["HOME"] || options.CWD != options.Env["GH_CONFIG_DIR"] {
		t.Fatal("gateway did not isolate the credential-bearing CLI from the checkout")
	}
	entries, err := os.ReadDir(options.CWD)
	if err != nil || len(entries) != 0 {
		t.Fatalf("CLI working directory is not private and empty: %v", err)
	}
}

func githubSubprocessContext(t *testing.T) context.Context {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	manager := hostingidentity.NewManager(hostingidentity.Options{
		ReadFile: func(string) ([]byte, error) { return keyPEM, nil },
		HTTPClient: &http.Client{Transport: githubSubprocessTransport(func(request *http.Request) (*http.Response, error) {
			if request.URL.Host != "api.github.com" {
				return nil, fmt.Errorf("unexpected fixture host %s", request.URL.Host)
			}
			var value any
			switch request.URL.Path {
			case "/app":
				value = map[string]any{"id": 137, "slug": "looper-worker"}
			case "/app/installations/246/access_tokens":
				value = map[string]any{"token": "gateway-test-installation-token", "expires_at": time.Now().Add(time.Hour)}
			case "/repos/org/repo":
				value = map[string]any{"full_name": "org/repo"}
			case "/users/looper-worker[bot]":
				value = map[string]any{"id": 9917, "login": "looper-worker[bot]"}
			default:
				return nil, fmt.Errorf("unexpected fixture path %s", request.URL.Path)
			}
			body, err := json.Marshal(value)
			if err != nil {
				return nil, err
			}
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(body))), Request: request}, nil
		})},
	})
	ctx, err := hostingidentity.BindResolved(hostingidentity.WithManager(context.Background(), manager), config.ResolvedHostingIdentity{
		Name: "worker-bot", ProjectID: "project", Role: "worker",
		Definition: config.HostingIdentityConfig{Kind: config.HostingIdentityGitHubApp, BaseURL: "https://github.com", AppID: 137, InstallationID: 246, PrivateKeyFile: "/test/app.pem"},
		Target:     config.RepositoryIdentity{Kind: config.ProviderKindGitHub, BaseURL: "https://github.com", Repo: "org/repo"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}

type githubSubprocessTransport func(*http.Request) (*http.Response, error)

func (transport githubSubprocessTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}
