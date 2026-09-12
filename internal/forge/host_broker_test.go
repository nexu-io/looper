package forge

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/hostingidentity"
)

type hostFixture struct {
	ctx     context.Context
	options HostBrokerOptions
	server  *httptest.Server
	token   atomic.Value
	calls   atomic.Int64
	now     atomic.Int64
}

func newHostFixture(t *testing.T, kind config.HostingIdentityKind, handler http.HandlerFunc) *hostFixture {
	t.Helper()
	f := &hostFixture{}
	f.token.Store("host-test-token-one")
	f.now.Store(time.Now().Unix())
	prefix := "/api/v1"
	provider := config.ProviderKindForgejo
	if kind == config.HostingIdentityGitHubApp {
		prefix = "/api/v3"
		provider = config.ProviderKindGitHub
	}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, prefix)
		switch path {
		case "/app":
			io.WriteString(w, `{"id":17,"slug":"looper-review"}`)
		case "/app/installations/29/access_tokens":
			json.NewEncoder(w).Encode(map[string]any{"token": f.token.Load().(string), "expires_at": time.Unix(f.now.Load(), 0).Add(time.Hour)})
		case "/user", "/users/looper-review[bot]":
			login := "looper-bot"
			if provider == config.ProviderKindGitHub {
				login = "looper-review[bot]"
			}
			json.NewEncoder(w).Encode(map[string]any{"id": 77, "login": login, "full_name": "Loop Bot", "email": "bot@example.test"})
		case "/repos/acme/looper":
			io.WriteString(w, `{"full_name":"acme/looper"}`)
		default:
			f.calls.Add(1)
			auth := "token "
			if provider == config.ProviderKindGitHub {
				auth = "Bearer "
			}
			if r.Header.Get("Authorization") != auth+f.token.Load().(string) {
				t.Error("broker did not use the selected current credential")
				w.WriteHeader(401)
				return
			}
			handler(w, r)
		}
	}))
	t.Cleanup(f.server.Close)
	definition := config.HostingIdentityConfig{Kind: kind, BaseURL: f.server.URL, TokenEnv: "HOST_TEST_TOKEN"}
	options := hostingidentity.Options{HTTPClient: f.server.Client(), LookupEnv: func(name string) (string, bool) { return f.token.Load().(string), name == "HOST_TEST_TOKEN" }}
	options.Now = func() time.Time { return time.Unix(f.now.Load(), 0) }
	if kind == config.HostingIdentityGitHubApp {
		key, err := rsa.GenerateKey(rand.Reader, 1024)
		if err != nil {
			t.Fatal(err)
		}
		keyBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
		definition.TokenEnv = ""
		definition.AppID = 17
		definition.InstallationID = 29
		definition.PrivateKeyFile = "/daemon-only/test-key.pem"
		options.ReadFile = func(string) ([]byte, error) { return keyBytes, nil }
	}
	ctx := hostingidentity.WithManager(context.Background(), hostingidentity.NewManager(options))
	var err error
	f.ctx, err = hostingidentity.BindResolved(ctx, config.ResolvedHostingIdentity{Name: "review", Definition: definition, Target: config.RepositoryIdentity{Kind: provider, BaseURL: f.server.URL, Repo: "acme/looper"}, ProjectID: "project", Role: "reviewer"})
	if err != nil {
		t.Fatal(err)
	}
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	git, _ = filepath.Abs(git)
	looper, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Normalize("")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Tools.GitPath = &git
	f.options = HostBrokerOptions{Config: cfg, RealLooper: looper, CWD: t.TempDir(), HTTPClient: f.server.Client()}
	return f
}

func (f *hostFixture) start(t *testing.T) string {
	t.Helper()
	socket, cleanup, err := StartHostBroker(f.ctx, f.options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	t.Setenv(HostSockEnv, socket)
	return socket
}

func TestHostBrokerReadContractAndRotation(t *testing.T) {
	f := newHostFixture(t, config.HostingIdentityForgejoToken, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/repos/acme/looper/pulls/42":
			io.WriteString(w, `{"number":42,"head":{"sha":"head"}}`)
		case "/api/v1/repos/acme/looper/pulls/42.diff":
			io.WriteString(w, "diff --git a/one b/one\n")
		case "/api/v1/repos/acme/looper/issues/42/comments":
			if r.URL.Query().Get("page") == "1" {
				io.WriteString(w, `[{"id":1}]`)
			} else {
				io.WriteString(w, `[]`)
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	f.start(t)
	for _, request := range []HostRequest{{Op: "identity"}, {Op: "api.read", Path: "pulls/42"}, {Op: "api.read", Path: "pulls/42", Diff: true}, {Op: "api.read", Path: "issues/42/comments?limit=1", Paginate: true}} {
		out, err := ProxyHost(context.Background(), request)
		if err != nil || out == "" {
			t.Fatalf("read %s failed: %v", request.Op, err)
		}
		if strings.Contains(out, "host-test-token") || strings.Contains(out, "test-key.pem") {
			t.Fatal("hosting response exposed credential material")
		}
	}
	f.token.Store("host-test-token-two")
	if _, err := ProxyHost(context.Background(), HostRequest{Op: "api.read", Path: "pulls/42"}); err != nil {
		t.Fatal(err)
	}
	if f.calls.Load() != 5 {
		t.Fatalf("read request count = %d", f.calls.Load())
	}
}

func TestHostBrokerRefreshesAppTokenBetweenReads(t *testing.T) {
	f := newHostFixture(t, config.HostingIdentityGitHubApp, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"number":42}`) })
	f.start(t)
	request := HostRequest{Op: "api.read", Path: "pulls/42"}
	if _, err := ProxyHost(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	f.token.Store("host-test-token-refreshed")
	f.now.Add(3600)
	if _, err := ProxyHost(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if f.calls.Load() != 2 {
		t.Fatal("App read did not use a refreshed credential")
	}
}

func TestHostBrokerRejectsEscapesAndEveryWrite(t *testing.T) {
	f := newHostFixture(t, config.HostingIdentityForgejoToken, func(w http.ResponseWriter, r *http.Request) {
		t.Error("invalid capability reached provider")
		io.WriteString(w, `[]`)
	})
	socket := f.start(t)
	requests := []HostRequest{
		{Op: "api.read", Path: "https://other.invalid/repos/acme/looper/pulls/1"},
		{Op: "api.read", Path: "/repos/acme/looper/pulls/1"},
		{Op: "api.read", Path: "repos/other/repo/pulls/1"},
		{Op: "api.read", Path: "pulls/../issues/1"},
		{Op: "api.read", Path: "pulls/%252e%252e/issues/1"},
		{Op: "api.read", Path: "pulls//1"},
		{Op: "api.read", Path: "pulls/1#fragment"},
		{Op: "api.read", Path: "pulls/1/reviews", Method: "POST"},
		{Op: "api.read", Path: "pulls/1", Method: "DELETE"},
		{Op: "api.read", Path: "pulls/1?token=untrusted"},
		{Op: "api.read", Path: "pulls/1", Cwd: "/tmp/other"},
		{Op: "api.read", Path: "pulls/1", Argv: []string{"gh", "auth", "token"}},
		{Op: "api.read", Path: "pulls/1", Stdin: []byte("{}")},
		{Op: "api.read", Path: "graphql"},
		{Op: "api.read", Path: "user"},
		{Op: "api.read", Path: "issues/1/comments?page=2", Paginate: true},
		{Op: "identity", Ref: "main"},
		{Op: "git.push", Ref: "main"},
		{Op: "git.fetch", Ref: "--upload-pack=anything"},
		{Op: "git.fetch", Ref: "https://other.invalid/repo"},
		{Op: "git.fetch", Ref: "refs/heads/../main"},
		{Op: "review.submit", Argv: []string{"review", "submit", "acme/looper#1"}},
	}
	for _, request := range requests {
		if _, err := ProxyHost(context.Background(), request); err == nil {
			t.Errorf("accepted unsupported request %#v", request)
		}
	}
	conn, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(conn, `{"op":"identity","environment":{"GH_TOKEN":"fake"}}`)
	var response trustedReviewProxyResponse
	if err := json.NewDecoder(conn).Decode(&response); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if response.Error == "" {
		t.Fatal("accepted unknown request fields")
	}
	if f.calls.Load() != 0 {
		t.Fatal("rejected capability performed a provider request")
	}
}

func TestHostBrokerPaginationResponseLimitsAndErrors(t *testing.T) {
	for _, mode := range []string{"foreign-page", "scope-page", "oversized", "malformed", "denied"} {
		t.Run(mode, func(t *testing.T) {
			f := newHostFixture(t, config.HostingIdentityForgejoToken, func(w http.ResponseWriter, r *http.Request) {
				switch mode {
				case "foreign-page":
					w.Header().Set("Link", `<https://other.invalid/api/v1/repos/acme/looper/issues/1/comments?page=2&limit=50>; rel="next"`)
					io.WriteString(w, `[]`)
				case "scope-page":
					w.Header().Set("Link", `</api/v1/repos/other/repo/issues/1/comments?page=2&limit=50>; rel="next"`)
					io.WriteString(w, `[]`)
				case "oversized":
					io.WriteString(w, strings.Repeat(" ", maxHostResponseBytes+1))
				case "malformed":
					io.WriteString(w, `{"wrong":true}`)
				case "denied":
					w.WriteHeader(403)
					io.WriteString(w, "host-test-token-one")
				}
			})
			f.start(t)
			_, err := ProxyHost(context.Background(), HostRequest{Op: "api.read", Path: "issues/1/comments", Paginate: true})
			if err == nil {
				t.Fatal("invalid provider response was accepted")
			}
			if strings.Contains(err.Error(), "host-test-token") {
				t.Fatal("provider error exposed credential")
			}
		})
	}
}

func TestHostBrokerDisconnectCancelsProviderRead(t *testing.T) {
	started, cancelled := make(chan struct{}), make(chan struct{})
	f := newHostFixture(t, config.HostingIdentityForgejoToken, func(w http.ResponseWriter, r *http.Request) { close(started); <-r.Context().Done(); close(cancelled) })
	socket := f.start(t)
	conn, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	json.NewEncoder(conn).Encode(HostRequest{Op: "api.read", Path: "pulls/1"})
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("read never started")
	}
	conn.Close()
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("disconnect did not cancel provider request")
	}
}

func TestHostBrokerJobLogRedirectOmitsAPICredentials(t *testing.T) {
	logs := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Error("job log download forwarded API authentication")
		}
		io.WriteString(w, "compiler failed on line 3\n")
	}))
	defer logs.Close()
	f := newHostFixture(t, config.HostingIdentityForgejoToken, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", logs.URL+"/job?signature=fixture-signed-location")
		w.WriteHeader(http.StatusFound)
	})
	f.options.HTTPClient = logs.Client()
	f.start(t)
	output, err := ProxyHost(context.Background(), HostRequest{Op: "api.read", Path: "actions/jobs/9/logs"})
	if err != nil || output != "compiler failed on line 3\n" {
		t.Fatalf("job log download failed: %v", err)
	}
	if strings.Contains(output, "signature=") {
		t.Fatal("signed download URL reached agent")
	}
}

func TestHostBrokerThreadsKeepNodeIDsAndVerifyMembership(t *testing.T) {
	f := newHostFixture(t, config.HostingIdentityGitHubApp, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/graphql" || r.Method != "POST" {
			t.Error("incorrect GHES GraphQL path/method")
			w.WriteHeader(404)
			return
		}
		var payload struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
			return
		}
		if payload.Query == hostThreadsQuery {
			if payload.Variables["owner"] != "acme" || payload.Variables["repo"] != "looper" || payload.Variables["number"] != float64(42) {
				t.Error("thread query was not repository-bound")
			}
			io.WriteString(w, `{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[{"id":"THREAD_ONE","isResolved":false,"comments":{"nodes":[{"id":"COMMENT_ONE","updatedAt":"2026-09-12T01:00:00Z","body":"one","author":{"login":"reviewer"}}],"pageInfo":{"hasNextPage":true,"endCursor":"after-one"}}}],"pageInfo":{"hasNextPage":false}}}}}}`)
		} else if payload.Query == hostThreadCommentsQuery {
			if payload.Variables["id"] != "THREAD_ONE" {
				t.Error("unverified thread reached node query")
			}
			io.WriteString(w, `{"data":{"node":{"id":"THREAD_ONE","comments":{"nodes":[{"id":"COMMENT_TWO","updatedAt":"2026-09-12T02:00:00Z","body":"two","author":{"login":"reviewer"}}],"pageInfo":{"hasNextPage":false}}}}}`)
		} else {
			t.Error("broker forwarded an unknown GraphQL query")
		}
	})
	f.start(t)
	out, err := ProxyHost(context.Background(), HostRequest{Op: "threads.list", PRNumber: 42})
	if err != nil {
		t.Fatal(err)
	}
	var threads []HostThread
	if err := json.Unmarshal([]byte(out), &threads); err != nil || len(threads) != 1 || len(threads[0].Comments) != 2 {
		t.Fatalf("incomplete thread snapshot: %v", err)
	}
	if threads[0].Comments[1].ID != "COMMENT_TWO" || threads[0].Comments[1].UpdatedAt != "2026-09-12T02:00:00Z" {
		t.Fatal("thread node identity or update time changed")
	}
	if _, err := ProxyHost(context.Background(), HostRequest{Op: "thread.read", PRNumber: 42, ThreadID: "OTHER_REPOSITORY_THREAD"}); err == nil {
		t.Fatal("foreign thread was accepted")
	}
}

func TestHostBrokerReviewRetainsDaemonPolicyAndAllowlistedChildEnv(t *testing.T) {
	f := newHostFixture(t, config.HostingIdentityForgejoToken, func(http.ResponseWriter, *http.Request) {})
	child := filepath.Join(t.TempDir(), "review-child")
	// The stub checks keys only; credentials must never enter test output.
	script := trustedReviewProxyStubScript(`
test -n "$HOST_TEST_TOKEN" || exit 40
test -z "$UNRELATED_BOT_TOKEN" || exit 41
test -z "$GITHUB_TOKEN" || exit 42
test -z "$OPENAI_API_KEY" || exit 43
printf '%s\n' "$@"
`)
	if err := os.WriteFile(child, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("UNRELATED_BOT_TOKEN", "other-test-value")
	t.Setenv("GITHUB_TOKEN", "personal-test-value")
	t.Setenv("OPENAI_API_KEY", "model-test-value")
	f.options.RealLooper = child
	f.options.Review = &TrustedReviewAuthority{PRRef: "acme/looper#42", Policy: TrustedReviewProxyPolicy{Clean: "COMMENT", Blocking: "REQUEST_CHANGES", ExpectedCommitID: "bound-head", ReviewerRunID: "bound-run"}}
	f.start(t)
	out, err := ProxyHost(context.Background(), HostRequest{Op: "review.submit", Argv: []string{"review", "submit", "acme/looper#42", "--commit-id", "agent-head", "--clean-review-event", "APPROVE", "--reviewer-manual", "--reviewer-run-id", "agent-run"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "agent-head") || strings.Contains(out, "agent-run") || strings.Contains(out, "--reviewer-manual") || !strings.Contains(out, "bound-head") || !strings.Contains(out, "bound-run") {
		t.Fatal("review authority was not overwritten by bound policy")
	}
	if _, err := ProxyHost(context.Background(), HostRequest{Op: "review.submit", Argv: []string{"review", "submit", "acme/other#42"}}); err == nil {
		t.Fatal("review was retargeted")
	}
}
