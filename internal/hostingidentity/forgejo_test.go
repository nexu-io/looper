package hostingidentity

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
)

const (
	workerToken        = "worker-token/+secret=1"
	rotatedWorkerToken = "worker-token/+secret=2"
	reviewerToken      = "reviewer-token-secret"
	impostorToken      = "different-account-token"
)

type forgejoFixture struct {
	t            *testing.T
	clock        *testClock
	server       *httptest.Server
	env          atomic.Value
	wrongRepo    atomic.Bool
	missingEmail atomic.Bool
	mu           sync.Mutex
	revoked      map[string]bool
	userCalls    map[string]int
	envNames     []string
}

func newForgejoFixture(t *testing.T) *forgejoFixture {
	t.Helper()
	fixture := &forgejoFixture{t: t, clock: newTestClock(), revoked: make(map[string]bool), userCalls: make(map[string]int)}
	fixture.env.Store(map[string]string{"WORKER_TOKEN": workerToken, "REVIEWER_TOKEN": reviewerToken, "GH_TOKEN": "personal-gh-secret", "GITHUB_TOKEN": "personal-github-secret", "TEA_TOKEN": "personal-tea-secret"})
	fixture.server = httptest.NewServer(http.HandlerFunc(fixture.handle))
	t.Cleanup(fixture.server.Close)
	return fixture
}

func (fixture *forgejoFixture) handle(response http.ResponseWriter, request *http.Request) {
	authorization := request.Header.Get("Authorization")
	if !strings.HasPrefix(authorization, "token ") {
		fixture.t.Error("Forgejo must use a dedicated token")
	}
	token := strings.TrimPrefix(authorization, "token ")
	fixture.mu.Lock()
	revoked := fixture.revoked[token]
	if request.URL.Path == "/forge/api/v1/user" {
		fixture.userCalls[token]++
	}
	fixture.mu.Unlock()
	if revoked {
		response.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(response, "secret-server-body "+token)
		return
	}
	login, id := "worker-bot", int64(41)
	switch token {
	case workerToken, rotatedWorkerToken:
	case reviewerToken:
		login, id = "reviewer-bot", 42
	case impostorToken:
		login, id = "other-account", 43
	default:
		fixture.t.Error("request used an unexpected token")
	}
	switch request.URL.Path {
	case "/forge/api/v1/user":
		email := login + "@noreply.forge.local"
		if fixture.missingEmail.Load() {
			email = ""
		}
		writeJSON(fixture.t, response, map[string]any{"id": id, "login": login, "full_name": "", "email": email})
	case "/forge/api/v1/repos/org/repo":
		repo := "org/repo"
		if fixture.wrongRepo.Load() {
			repo = "org/unrelated"
		}
		writeJSON(fixture.t, response, map[string]any{"full_name": repo})
	default:
		fixture.t.Errorf("unexpected Forgejo operation: %s", request.URL.Path)
		response.WriteHeader(http.StatusNotFound)
	}
}

func (fixture *forgejoFixture) options() Options {
	return Options{HTTPClient: fixture.server.Client(), Now: fixture.clock.now, LookupEnv: func(name string) (string, bool) {
		fixture.mu.Lock()
		fixture.envNames = append(fixture.envNames, name)
		fixture.mu.Unlock()
		value, ok := fixture.env.Load().(map[string]string)[name]
		return value, ok
	}, ReadFile: func(string) ([]byte, error) {
		fixture.t.Error("Forgejo identity must not read private keys or tea credentials")
		return nil, errors.New("unexpected read")
	}}
}

func (fixture *forgejoFixture) resolved(name, tokenEnv string) config.ResolvedHostingIdentity {
	base := fixture.server.URL + "/forge"
	return config.ResolvedHostingIdentity{Name: name, ProjectID: "project", Role: "worker", Definition: config.HostingIdentityConfig{Kind: config.HostingIdentityForgejoToken, BaseURL: base, TokenEnv: tokenEnv}, Target: config.RepositoryIdentity{ProviderID: "forge", Kind: config.ProviderKindForgejo, BaseURL: base, Repo: "org/repo"}}
}

func (fixture *forgejoFixture) setToken(name, token string) {
	updated := make(map[string]string)
	for key, value := range fixture.env.Load().(map[string]string) {
		updated[key] = value
	}
	updated[name] = token
	fixture.env.Store(updated)
}

func TestForgejoAccountRotationScopeAndIdentityIsolation(t *testing.T) {
	fixture := newForgejoFixture(t)
	manager := NewManager(fixture.options())
	worker := sessionFor(t, manager, fixture.resolved("worker", "WORKER_TOKEN"))
	reviewer := sessionFor(t, manager, fixture.resolved("reviewer", "REVIEWER_TOKEN"))
	for _, credential := range concurrentCredentials(t, worker) {
		if credential.Login != "worker-bot" || credential.NumericID != 41 || credential.Name != "worker-bot" || credential.Email != "worker-bot@noreply.forge.local" || credential.Token != workerToken {
			t.Error("wrong Forgejo worker identity")
		}
	}
	credential, err := reviewer.Credentials(context.Background())
	if err != nil || credential.Login != "reviewer-bot" || credential.Token != reviewerToken {
		t.Fatalf("reviewer identity = %s, %v", credential, err)
	}
	fixture.mu.Lock()
	if fixture.userCalls[workerToken] != 1 || fixture.userCalls[reviewerToken] != 1 {
		t.Error("identity caches were not independent")
	}
	fixture.mu.Unlock()
	fixture.setToken("WORKER_TOKEN", rotatedWorkerToken)
	credential, err = worker.Credentials(context.Background())
	if err != nil || credential.Token != rotatedWorkerToken || credential.Login != "worker-bot" {
		t.Fatalf("same-account rotation failed: %v", err)
	}
	fixture.setToken("WORKER_TOKEN", impostorToken)
	credential, err = worker.Credentials(context.Background())
	if err == nil || credential.Token != "" || !strings.Contains(err.Error(), "account changed") {
		t.Fatalf("rotation changed active bot account: %v", err)
	}
	newRun := sessionFor(t, manager, fixture.resolved("worker", "WORKER_TOKEN"))
	if _, err := newRun.Credentials(context.Background()); err == nil {
		t.Fatal("unchanged definition silently switched its known bot account on new run")
	}
	newIdentity := sessionFor(t, manager, fixture.resolved("new-worker", "WORKER_TOKEN"))
	credential, err = newIdentity.Credentials(context.Background())
	if err != nil || credential.Login != "other-account" {
		t.Fatalf("explicit named identity change failed: %v", err)
	}
	credential, err = reviewer.Credentials(context.Background())
	if err != nil || credential.Token != reviewerToken {
		t.Fatalf("broken worker affected healthy reviewer: %v", err)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	for _, name := range fixture.envNames {
		if name != "WORKER_TOKEN" && name != "REVIEWER_TOKEN" {
			t.Errorf("legacy credential environment consulted: %s", name)
		}
	}
}

func TestForgejoMissingRevokedAndWrongRepositoryNeverFallback(t *testing.T) {
	for _, scenario := range []string{"unset", "empty", "invalid", "revoked", "repository", "email"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := newForgejoFixture(t)
			manager := NewManager(fixture.options())
			resolved := fixture.resolved("worker", "WORKER_TOKEN")
			switch scenario {
			case "unset":
				resolved.Definition.TokenEnv = "MISSING_TOKEN"
			case "empty":
				fixture.setToken("WORKER_TOKEN", "  ")
			case "invalid":
				fixture.setToken("WORKER_TOKEN", "token-with\nnewline-secret")
			case "revoked":
				fixture.revoked[workerToken] = true
			case "repository":
				fixture.wrongRepo.Store(true)
			case "email":
				fixture.missingEmail.Store(true)
			}
			session := sessionFor(t, manager, resolved)
			credential, err := session.Credentials(context.Background())
			if err == nil || credential.Token != "" {
				t.Fatal("broken explicit identity returned credentials")
			}
			if !strings.Contains(err.Error(), `hosting identity "worker"`) || strings.Contains(err.Error(), workerToken) || strings.Contains(err.Error(), "secret-server-body") || strings.Contains(err.Error(), "newline-secret") {
				t.Errorf("unsafe/unlabeled identity error: %v", err)
			}
			if scenario == "revoked" && !strings.Contains(err.Error(), "read:user") {
				t.Errorf("missing actionable Forgejo scope hint: %v", err)
			}
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			for _, name := range fixture.envNames {
				if name != resolved.Definition.TokenEnv {
					t.Errorf("fallback read environment %q", name)
				}
			}
		})
	}
}

func TestForgejoRevalidatesAndRetainsRedactionAfterRevocation(t *testing.T) {
	fixture := newForgejoFixture(t)
	session := sessionFor(t, NewManager(fixture.options()), fixture.resolved("worker", "WORKER_TOKEN"))
	if _, err := session.Credentials(context.Background()); err != nil {
		t.Fatal(err)
	}
	fixture.clock.advance(5 * time.Minute)
	if _, err := session.Credentials(context.Background()); err != nil {
		t.Fatal(err)
	}
	fixture.mu.Lock()
	if fixture.userCalls[workerToken] != 2 {
		t.Error("Forgejo token was not periodically revalidated")
	}
	fixture.revoked[workerToken] = true
	fixture.mu.Unlock()
	session.Invalidate(workerToken)
	if err := session.Check(context.Background()); err == nil {
		t.Fatal("revoked token probe succeeded")
	}
	for _, secret := range []string{workerToken, url.QueryEscape(workerToken), url.PathEscape(workerToken), base64.StdEncoding.EncodeToString([]byte(workerToken)), base64.StdEncoding.EncodeToString([]byte("x-access-token:" + workerToken)), base64.StdEncoding.EncodeToString([]byte("worker-bot:" + workerToken))} {
		if text := session.Redact("rejected " + secret); strings.Contains(text, secret) {
			t.Fatal("revoked token was not redacted")
		}
	}
	fixture.setToken("WORKER_TOKEN", "")
	if credential, err := session.Credentials(context.Background()); err == nil || credential.Token != "" {
		t.Fatal("missing environment reused a previously cached token")
	}
}

func TestForgejoCommitOverrideAllowsPrivateAccountEmail(t *testing.T) {
	fixture := newForgejoFixture(t)
	fixture.missingEmail.Store(true)
	resolved := fixture.resolved("worker", "WORKER_TOKEN")
	resolved.Definition.Commit.Email = "worker@configured.example"
	session := sessionFor(t, NewManager(fixture.options()), resolved)
	identity, err := session.CommitIdentity(context.Background())
	if err != nil || identity.Name != "worker-bot" || identity.Email != resolved.Definition.Commit.Email {
		t.Fatalf("private email override: %+v, %v", identity, err)
	}
}

func TestCredentialHTTPNeverFollowsRedirectsEvenWithInjectedPolicy(t *testing.T) {
	for _, sameOrigin := range []bool{false, true} {
		t.Run(fmt.Sprintf("same_origin_%t", sameOrigin), func(t *testing.T) {
			var received, redirectPolicyCalls atomic.Int64
			destination := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				received.Add(1)
				response.WriteHeader(http.StatusOK)
			}))
			defer destination.Close()
			source := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				if request.URL.Path != "/api/v1/user" {
					received.Add(1)
					response.WriteHeader(http.StatusOK)
					return
				}
				location := destination.URL + "/credentials"
				if sameOrigin {
					location = "/credentials"
				}
				http.Redirect(response, request, location, http.StatusFound)
			}))
			defer source.Close()
			client := source.Client()
			client.CheckRedirect = func(*http.Request, []*http.Request) error { redirectPolicyCalls.Add(1); return nil }
			manager := NewManager(Options{HTTPClient: client, LookupEnv: func(string) (string, bool) { return workerToken, true }})
			resolved := config.ResolvedHostingIdentity{Name: "worker", Definition: config.HostingIdentityConfig{Kind: config.HostingIdentityForgejoToken, BaseURL: source.URL, TokenEnv: "WORKER_TOKEN"}, Target: config.RepositoryIdentity{Kind: config.ProviderKindForgejo, BaseURL: source.URL, Repo: "org/repo"}}
			session := sessionFor(t, manager, resolved)
			if _, err := session.Credentials(context.Background()); err == nil || !strings.Contains(err.Error(), "redirects") {
				t.Fatalf("redirect = %v", err)
			}
			if received.Load() != 0 || redirectPolicyCalls.Load() != 0 {
				t.Fatal("credential request followed a redirect")
			}
			if client.CheckRedirect == nil {
				t.Fatal("manager mutated the caller's HTTP client")
			}
		})
	}
}

func TestMalformedAndSensitiveTransportFailuresAreSanitized(t *testing.T) {
	for _, scenario := range []string{"transport", "malformed JSON", "oversized JSON"} {
		t.Run(scenario, func(t *testing.T) {
			manager := NewManager(Options{LookupEnv: func(string) (string, bool) { return workerToken, true }, HTTPClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				if scenario == "transport" {
					return nil, errors.New("private-transport-secret " + request.Header.Get("Authorization"))
				}
				body := "invalid-json-secret " + workerToken
				if scenario == "oversized JSON" {
					body = strings.Repeat("x", maxResponseBytes+1) + workerToken
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: request}, nil
			})}})
			resolved := config.ResolvedHostingIdentity{Name: "worker", Definition: config.HostingIdentityConfig{Kind: config.HostingIdentityForgejoToken, BaseURL: "https://forge.example", TokenEnv: "WORKER_TOKEN"}, Target: config.RepositoryIdentity{Kind: config.ProviderKindForgejo, BaseURL: "https://forge.example", Repo: "org/repo"}}
			session := sessionFor(t, manager, resolved)
			_, err := session.Credentials(context.Background())
			if err == nil || strings.Contains(err.Error(), workerToken) || strings.Contains(err.Error(), "private-transport-secret") || strings.Contains(err.Error(), "invalid-json-secret") {
				t.Fatalf("unsafe credential error: %v", err)
			}
		})
	}
}

func TestAPIScopePreservesForgePrefixAndRejectsEscapes(t *testing.T) {
	fixture := newForgejoFixture(t)
	session := sessionFor(t, NewManager(fixture.options()), fixture.resolved("worker", "WORKER_TOKEN"))
	base := fixture.server.URL
	for _, good := range []string{base + "/forge/api/v1/repos/org/repo", base + "/forge/api/v1/repos/org/repo/issues?state=open&page=2"} {
		if err := session.CheckAPIURL(good); err != nil {
			t.Errorf("safe URL rejected: %v", err)
		}
	}
	for _, bad := range []string{"https://unrelated.example/forge/api/v1/user", base + "/api/v1/user", base + "/forge/api/v10/user", base + "/forge/api/v1/../outside", base + "/forge/api/v1/%2e%2e/outside", base + "/forge/api/v1/user#fragment", strings.Replace(base, "://", "://user:password@", 1) + "/forge/api/v1/user"} {
		if err := session.CheckAPIURL(bad); err == nil {
			t.Errorf("unsafe URL accepted: %s", bad)
		}
	}
	if err := session.CheckRepository("Org/Repo"); err != nil {
		t.Fatal(err)
	}
	if err := session.CheckRepository("other/repo"); err == nil {
		t.Fatal("unrelated repository accepted")
	}
	github := githubResolved()
	github.Definition.BaseURL, github.Target.BaseURL = "https://enterprise.example", "https://enterprise.example"
	if actual := sessionFor(t, NewManager(Options{}), github).APIURL(); actual != "https://enterprise.example/api/v3" {
		t.Errorf("GHES API URL = %s", actual)
	}
}
