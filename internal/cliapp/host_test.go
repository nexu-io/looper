package cliapp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/forge"
	"github.com/nexu-io/looper/internal/hostingidentity"
)

func hostCLIContext(t *testing.T, server *httptest.Server) context.Context {
	t.Helper()
	manager := hostingidentity.NewManager(hostingidentity.Options{HTTPClient: server.Client(), LookupEnv: func(name string) (string, bool) { return "test-cli-bot-token", name == "HOST_CLI_TOKEN" }})
	ctx, err := hostingidentity.BindResolved(hostingidentity.WithManager(context.Background(), manager), config.ResolvedHostingIdentity{Name: "review", ProjectID: "project", Role: "reviewer", Definition: config.HostingIdentityConfig{Kind: config.HostingIdentityForgejoToken, BaseURL: server.URL, TokenEnv: "HOST_CLI_TOKEN"}, Target: config.RepositoryIdentity{ProviderID: "forgejo", Kind: config.ProviderKindForgejo, BaseURL: server.URL, Repo: "acme/looper"}})
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}

func TestHostCLIUsesSocketWithoutLoadingLocalConfig(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "token test-cli-bot-token" {
			t.Error("wrong CLI broker credential")
			w.WriteHeader(401)
			return
		}
		switch r.URL.Path {
		case "/api/v1/user":
			io.WriteString(w, `{"id":9,"login":"looper-bot","email":"bot@example.test"}`)
		case "/api/v1/repos/acme/looper":
			io.WriteString(w, `{"full_name":"acme/looper"}`)
		case "/api/v1/repos/acme/looper/pulls/42":
			io.WriteString(w, `{"number":42}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	ctx := hostCLIContext(t, server)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	socket, cleanup, err := forge.StartHostBroker(ctx, forge.HostBrokerOptions{Config: config.Config{}, RealLooper: executable, CWD: t.TempDir(), HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	t.Setenv(forge.HostSockEnv, socket)
	broken := filepath.Join(t.TempDir(), "invalid-config.json")
	if err := os.WriteFile(broken, []byte("not JSON"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOOPER_CONFIG", broken)
	for _, argv := range [][]string{{"host", "whoami"}, {"host", "api", "pulls/42"}} {
		stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
		code := New(Deps{Stdout: stdout, Stderr: stderr}).Run(context.Background(), argv)
		if code != 0 || !json.Valid(stdout.Bytes()) {
			t.Fatalf("host CLI failed: code=%d stderr=%s", code, stderr.String())
		}
	}
	for _, argv := range [][]string{{"host", "api", "pulls/42", "--method", "POST"}, {"host", "api", "https://other.invalid/repos/acme/looper/pulls/42"}, {"host", "git", "push", "main"}, {"--config", broken, "host", "whoami"}} {
		if code := New(Deps{Stdout: io.Discard, Stderr: io.Discard}).Run(context.Background(), argv); code == 0 {
			t.Fatalf("host CLI accepted unsupported invocation: %v", argv)
		}
	}
}

func TestTrustedBotReviewRealCLIRebindsSnapshotAndChecksHead(t *testing.T) {
	// This invokes the supported CLI binary through the real socket and config
	// descriptor, proving auth remains in the trusted child while head checks
	// still reject publication. No public hosting service is involved.
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	looper := filepath.Join(t.TempDir(), "looper")
	build := exec.Command("go", "build", "-o", looper, "./cmd/looper")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, out)
	}
	var views, publications atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "token test-cli-bot-token" {
			t.Error("trusted CLI used another identity")
			w.WriteHeader(401)
			return
		}
		if r.Method != "GET" {
			publications.Add(1)
			w.WriteHeader(405)
			return
		}
		switch r.URL.Path {
		case "/api/v1/user":
			io.WriteString(w, `{"id":9,"login":"looper-bot","email":"bot@example.test"}`)
		case "/api/v1/repos/acme/looper":
			io.WriteString(w, `{"full_name":"acme/looper"}`)
		case "/api/v1/repos/acme/looper/pulls/42":
			views.Add(1)
			io.WriteString(w, `{"number":42,"state":"open","head":{"sha":"actual-head","ref":"topic"},"base":{"sha":"base-head","ref":"main"}}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	ctx := hostCLIContext(t, server)
	cfg, err := config.Normalize("")
	if err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()
	cfg.Daemon.WorkingDirectory = cwd
	cfg.Storage.DBPath = filepath.Join(t.TempDir(), "unused.sqlite")
	legacy := "MISSING_LEGACY_TOKEN"
	cfg.Providers = []config.ProviderConfig{{ID: "forgejo", Kind: config.ProviderKindForgejo, BaseURL: server.URL, TokenEnv: &legacy}}
	cfg.Projects = []config.ProjectRefConfig{{ID: "project", Name: "project", Repo: "acme/looper", RepoPath: cwd, Provider: "forgejo"}}
	t.Setenv("MISSING_LEGACY_TOKEN", "")
	t.Setenv("GH_TOKEN", "personal-token-that-must-not-be-used")
	socket, cleanup, err := forge.StartHostBroker(ctx, forge.HostBrokerOptions{Config: cfg, RealLooper: looper, CWD: cwd, Review: &forge.TrustedReviewAuthority{PRRef: "acme/looper#42", Policy: forge.TrustedReviewProxyPolicy{Clean: "COMMENT", Blocking: "COMMENT", ExpectedCommitID: "expected-head"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	// Agent argv tries to match actual-head, but the broker must replace it.
	command := exec.Command(looper, "review", "submit", "acme/looper#42", "--event", "COMMENT", "--commit-id", "actual-head")
	command.Env = []string{"PATH=" + os.Getenv("PATH"), forge.HostSockEnv + "=" + socket}
	command.Stdin = strings.NewReader(`{"body":"test review","comments":[]}`)
	out, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "expected-head") || !strings.Contains(string(out), "actual-head") {
		t.Fatalf("trusted review failed before expected-head validation: %v\n%s", err, out)
	}
	if views.Load() != 1 || publications.Load() != 0 {
		t.Fatalf("views=%d publications=%d", views.Load(), publications.Load())
	}
}
