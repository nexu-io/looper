package cliapp

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
)

func TestEnsureBootstrapConfigRejectsExistingProjectWithoutForgejoBinding(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "config.json")
	projectPath := filepath.Join(root, "repo")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.DefaultConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Projects = append(cfg.Projects, buildBootstrapProject(projectPath, "main"))
	raw, err := config.MarshalConfigFile(configPath, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	runtime := newCommandRuntime(New(Deps{}), nil)
	_, _, err = runtime.ensureBootstrapConfig(configPath, root, bootstrapConfigPlan{
		Provider: bootstrapProviderForgejo, ProjectPath: projectPath, Repo: "acme/looper",
		ForgejoProviderID: "forgejo", ForgejoURL: "https://code.example.com", ForgejoTokenEnv: "FORGEJO_TOKEN",
	})
	if err == nil || !strings.Contains(err.Error(), `is not bound to Forgejo provider "forgejo" repository "acme/looper"`) {
		t.Fatalf("ensureBootstrapConfig() error = %v", err)
	}
}

func TestPrepareProjectAddProviderPreservesExplicitForgejoIDWithURL(t *testing.T) {
	t.Setenv("FORGEJO_TOKEN", "test-token")
	httpClient := newTestHTTPClient(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/api/v1/user":
			return jsonResponse(t, http.StatusOK, `{"login":"alice","id":7}`), nil
		case "/api/v1/repos/acme/looper":
			return jsonResponse(t, http.StatusOK, `{"full_name":"acme/looper"}`), nil
		default:
			t.Fatalf("unexpected path %q", req.URL.Path)
			return nil, nil
		}
	})

	root := t.TempDir()
	configPath := filepath.Join(root, "config.json")
	cfg, err := config.DefaultConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Providers = append(cfg.Providers, config.ProviderConfig{ID: "forgejo", Kind: config.ProviderKindForgejo, BaseURL: "https://code.example.com", TokenEnv: stringPtr("FORGEJO_TOKEN")})
	raw, err := config.MarshalConfigFile(configPath, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	runtime := newCommandRuntime(New(Deps{
		HTTPClient: httpClient,
		LookPath:   func(string) (string, error) { return "/usr/bin/git", nil },
		RunCommand: func(context.Context, string, []string, time.Duration) (commandExecutionResult, error) {
			return commandExecutionResult{ExitCode: 0, Stdout: "https://code.example.com/acme/looper.git"}, nil
		},
	}), []string{"--config", configPath})
	cmd := newCommand(commandSpec{
		use: "test",
		localFlags: []flagSpec{
			stringFlag("provider", "id", ""),
			stringFlag("repo", "owner/name", ""),
			stringFlag("forgejo-url", "url", ""),
			stringFlag("forgejo-token-env", "name", ""),
		},
	})
	cmd.SetContext(context.Background())
	for name, value := range map[string]string{
		"provider":          "forgejo",
		"forgejo-url":       "https://code.example.com",
		"forgejo-token-env": "FORGEJO_TOKEN",
	} {
		if err := cmd.Flags().Set(name, value); err != nil {
			t.Fatal(err)
		}
	}

	provider, repo, err := runtime.prepareProjectAddProvider(cmd, root)
	if err != nil {
		t.Fatalf("prepareProjectAddProvider() error = %v", err)
	}
	if provider != "forgejo" || repo != "acme/looper" {
		t.Fatalf("prepareProjectAddProvider() = (%q, %q), want (%q, %q)", provider, repo, "forgejo", "acme/looper")
	}
}

func TestProviderTestUsesAPIManagedProjectBinding(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
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

	root := t.TempDir()
	configPath := filepath.Join(root, "config.json")
	dbPath := filepath.Join(root, "looper.sqlite")
	cfg, err := config.DefaultConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Storage.DBPath = dbPath
	cfg.Providers = append(cfg.Providers, config.ProviderConfig{ID: "forgejo-main", Kind: config.ProviderKindForgejo, BaseURL: server.URL, TokenEnv: stringPtr("FORGEJO_TOKEN")})
	raw, err := config.MarshalConfigFile(configPath, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	coordinator, err := storage.OpenSQLiteCoordinator(context.Background(), dbPath, storage.SQLiteCoordinatorOptions{Migrations: storage.EmbeddedMigrations, BackupDir: filepath.Join(root, "backups")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.MigrationRunner().RunPending(context.Background()); err != nil {
		t.Fatal(err)
	}
	metadata := `{"provider":"forgejo-main","repo":"acme/looper","source":"api"}`
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if err := storage.NewRepositories(coordinator.DB()).Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "api-project", Name: "API Project", RepoPath: filepath.Join(root, "repo"), MetadataJSON: &metadata, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Close(); err != nil {
		t.Fatal(err)
	}

	t.Setenv("FORGEJO_TOKEN", "test-token")
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	app := New(Deps{Stdout: stdout, Stderr: stderr, HTTPClient: server.Client(), LookPath: configLookPathForTests()})
	exitCode := app.Run(context.Background(), []string{"provider", "test", "forgejo-main", "--config", configPath})
	if exitCode != 0 {
		t.Fatalf("provider test exit=%d stderr=%q", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "acme/looper") {
		t.Fatalf("provider test output = %q", stdout.String())
	}
}

func TestProviderRemoveRejectsAPIManagedProjectBinding(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "config.json")
	dbPath := filepath.Join(root, "looper.sqlite")
	cfg, err := config.DefaultConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Storage.DBPath = dbPath
	cfg.Providers = append(cfg.Providers, config.ProviderConfig{ID: "forgejo-main", Kind: config.ProviderKindForgejo, BaseURL: "https://code.example.com", TokenEnv: stringPtr("FORGEJO_TOKEN")})
	raw, err := config.MarshalConfigFile(configPath, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	coordinator, err := storage.OpenSQLiteCoordinator(context.Background(), dbPath, storage.SQLiteCoordinatorOptions{Migrations: storage.EmbeddedMigrations, BackupDir: filepath.Join(root, "backups")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.MigrationRunner().RunPending(context.Background()); err != nil {
		t.Fatal(err)
	}
	metadata := `{"provider":"forgejo-main","repo":"acme/looper","source":"api"}`
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if err := storage.NewRepositories(coordinator.DB()).Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "api-project", Name: "API Project", RepoPath: filepath.Join(root, "repo"), MetadataJSON: &metadata, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Close(); err != nil {
		t.Fatal(err)
	}

	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	app := New(Deps{Stdout: stdout, Stderr: stderr, LookPath: configLookPathForTests()})
	exitCode := app.Run(context.Background(), []string{"provider", "remove", "forgejo-main", "--force", "--config", configPath})
	if exitCode == 0 || !strings.Contains(stderr.String(), `provider "forgejo-main" is bound to project "api-project"`) {
		t.Fatalf("provider remove exit=%d stderr=%q", exitCode, stderr.String())
	}
	loaded, err := config.LoadFile(config.LoadFileOptions{CWD: root, Args: []string{"--config", configPath}})
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Config.Providers) != 1 || loaded.Config.Providers[0].ID != "forgejo-main" {
		t.Fatalf("providers = %#v", loaded.Config.Providers)
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
