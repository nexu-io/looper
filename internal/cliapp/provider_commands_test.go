package cliapp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
)

func TestParseBootstrapRemote(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"git@code.example.com:acme/looper.git", "ssh://git@code.example.com/acme/looper.git", "https://code.example.com/acme/looper.git"} {
		remote, err := parseBootstrapRemote(value)
		if err != nil {
			t.Fatalf("parseBootstrapRemote(%q) error = %v", value, err)
		}
		if remote.Host != "code.example.com" || remote.Repo != "acme/looper" {
			t.Fatalf("parseBootstrapRemote(%q) = %#v", value, remote)
		}
	}
}

func TestResolveForgejoBootstrapPlanValidatesIdentityAndRepo(t *testing.T) {
	t.Setenv("FORGEJO_TOKEN", "test-token")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Authorization") != "token test-token" {
			t.Fatalf("Authorization header was not set")
		}
		switch req.URL.Path {
		case "/api/v1/user":
			_, _ = w.Write([]byte(`{"login":"alice","id":7}`))
		case "/api/v1/repos/acme/looper":
			_, _ = w.Write([]byte(`{"full_name":"acme/looper"}`))
		default:
			t.Fatalf("unexpected path %q", req.URL.Path)
		}
	}))
	defer server.Close()

	projectPath := t.TempDir()
	runtime := newCommandRuntime(New(Deps{
		HTTPClient: server.Client(),
		LookPath:   func(string) (string, error) { return "/usr/bin/git", nil },
		RunCommand: func(context.Context, string, []string, time.Duration) (commandExecutionResult, error) {
			return commandExecutionResult{ExitCode: 0, Stdout: strings.Replace(server.URL, "http://", "ssh://git@", 1) + "/acme/looper.git"}, nil
		},
	}), nil)
	plan := bootstrapConfigPlan{ProjectPath: projectPath, Provider: bootstrapProviderForgejo}
	notes, err := runtime.resolveForgejoBootstrapPlan(context.Background(), &plan, bootstrapOptions{ForgejoURL: server.URL, ForgejoTokenEnv: "FORGEJO_TOKEN"})
	if err != nil {
		t.Fatalf("resolveForgejoBootstrapPlan() error = %v", err)
	}
	if plan.Repo != "acme/looper" || plan.Identity != "alice" || plan.ForgejoProviderID != "forgejo" {
		t.Fatalf("plan = %#v", plan)
	}
	if len(notes) != 1 || strings.Contains(strings.Join(notes, " "), "test-token") {
		t.Fatalf("notes = %#v", notes)
	}
}

func TestProviderAddPersistsOnlyTokenEnvironmentName(t *testing.T) {
	t.Setenv("FORGEJO_TOKEN", "super-secret-token")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/api/v1/user" {
			t.Fatalf("unexpected path %q", req.URL.Path)
		}
		_, _ = w.Write([]byte(`{"login":"alice","id":7}`))
	}))
	defer server.Close()

	configPath := filepath.Join(t.TempDir(), "config.json")
	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := config.MarshalConfigFile(configPath, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	app := New(Deps{Stdout: stdout, Stderr: stderr, HTTPClient: server.Client(), LookPath: configLookPathForTests()})
	exitCode := app.Run(context.Background(), []string{"provider", "add", "--id", "forgejo-main", "--forgejo-url", server.URL, "--forgejo-token-env", "FORGEJO_TOKEN", "--json", "--config", configPath})
	if exitCode != 0 {
		t.Fatalf("provider add exit = %d stderr=%q", exitCode, stderr.String())
	}
	var result providerOutput
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Identity != "alice" || result.TokenEnv != "FORGEJO_TOKEN" || !result.RestartRequired {
		t.Fatalf("result = %#v", result)
	}
	written, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(written), "super-secret-token") || !strings.Contains(string(written), "FORGEJO_TOKEN") {
		t.Fatalf("written config contains unexpected credential data: %s", fmt.Sprintf("secret=%t env=%t", strings.Contains(string(written), "super-secret-token"), strings.Contains(string(written), "FORGEJO_TOKEN")))
	}
}

func TestEnsureBootstrapConfigCreatesForgejoBinding(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	projectPath := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatal(err)
	}
	runtime := newCommandRuntime(New(Deps{}), nil)
	created, projectAdded, err := runtime.ensureBootstrapConfig(configPath, t.TempDir(), bootstrapConfigPlan{
		Provider: bootstrapProviderForgejo, ProjectPath: projectPath, Repo: "acme/looper",
		ForgejoProviderID: "forgejo", ForgejoURL: "https://code.example.com", ForgejoTokenEnv: "FORGEJO_TOKEN",
	})
	if err != nil {
		t.Fatalf("ensureBootstrapConfig() error = %v", err)
	}
	if !created || !projectAdded {
		t.Fatalf("created=%v projectAdded=%v", created, projectAdded)
	}
	partial, present, err := config.ReadPartialConfigFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !present {
		t.Fatal("bootstrap config was not written")
	}
	loaded, err := config.Normalize(t.TempDir(), partial)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Providers) != 1 || loaded.Providers[0].ID != "forgejo" {
		t.Fatalf("providers = %#v", loaded.Providers)
	}
	if len(loaded.Projects) != 1 || loaded.Projects[0].Provider != "forgejo" || loaded.Projects[0].Repo != "acme/looper" {
		t.Fatalf("projects = %#v", loaded.Projects)
	}
}

func TestForgejoBootstrapPreflightDoesNotRequireGitHubCLI(t *testing.T) {
	runtime := newCommandRuntime(New(Deps{
		Platform: "darwin", Arch: "arm64",
		LookPath: func(file string) (string, error) {
			if file == "git" {
				return "/usr/bin/git", nil
			}
			return "", fmt.Errorf("not found")
		},
	}), nil)
	plan := bootstrapConfigPlan{Provider: bootstrapProviderForgejo}
	if _, err := runtime.bootstrapPreflight(context.Background(), filepath.Join(t.TempDir(), "config.json"), &plan); err != nil {
		t.Fatalf("bootstrapPreflight() error = %v", err)
	}
}
