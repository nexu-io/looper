package cliapp

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
)

func TestParseBootstrapRemote(t *testing.T) {
	t.Parallel()
	for _, value := range []string{
		"git@code.example.com:acme/looper.git",
		"forgejo@code.example.com:acme/looper.git",
		"code.example.com:acme/looper.git",
		"ssh://git@code.example.com/acme/looper.git",
		"https://code.example.com/acme/looper.git",
	} {
		remote, err := parseBootstrapRemote(value)
		if err != nil {
			t.Fatalf("parseBootstrapRemote(%q) error = %v", value, err)
		}
		if remote.Host != "code.example.com" || remote.Repo != "acme/looper" {
			t.Fatalf("parseBootstrapRemote(%q) = %#v", value, remote)
		}
	}
}

func TestParseBootstrapRemoteAllowsConfiguredForgejoBasePath(t *testing.T) {
	t.Parallel()
	remote, err := parseBootstrapRemote("https://code.example.com/forge/acme/looper.git")
	if err != nil {
		t.Fatalf("parseBootstrapRemote() error = %v", err)
	}
	if remote.Repo != "acme/looper" || remote.Path != "forge/acme/looper" {
		t.Fatalf("remote = %#v", remote)
	}
	if !forgejoRemoteMatchesBaseURL(remote, "https://code.example.com/forge") {
		t.Fatal("remote should match its configured Forgejo base path")
	}
	if forgejoRemoteMatchesBaseURL(remote, "https://code.example.com/other") {
		t.Fatal("remote should not match a different Forgejo base path")
	}
}

func TestForgejoRemoteMatchesBaseURLPort(t *testing.T) {
	t.Parallel()
	remote, err := parseBootstrapRemote("https://ssh.code.example.com:3000/acme/looper.git")
	if err != nil {
		t.Fatalf("parseBootstrapRemote() error = %v", err)
	}
	if remote.Host != "ssh.code.example.com:3000" {
		t.Fatalf("remote host = %q, want host and port", remote.Host)
	}
	if !forgejoRemoteMatchesBaseURL(remote, "https://code.example.com:3000") {
		t.Fatal("remote should match the same host and port with the ssh host alias")
	}
	if forgejoRemoteMatchesBaseURL(remote, "https://code.example.com:8443") {
		t.Fatal("remote should not match the same host on a different port")
	}
	if forgejoRemoteMatchesBaseURL(remote, "https://code.example.com") {
		t.Fatal("remote with an explicit port should not match a provider without one")
	}
}

func TestForgejoRemoteMatchesBaseURLNormalizesDefaultPort(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		origin  string
		baseURL string
	}{
		{origin: "https://code.example.com:443/acme/looper.git", baseURL: "https://code.example.com"},
		{origin: "https://code.example.com/acme/looper.git", baseURL: "https://code.example.com:443"},
		{origin: "http://code.example.com:80/acme/looper.git", baseURL: "http://code.example.com"},
		{origin: "http://code.example.com/acme/looper.git", baseURL: "http://code.example.com:80"},
	} {
		remote, err := parseBootstrapRemote(test.origin)
		if err != nil {
			t.Fatalf("parseBootstrapRemote(%q) error = %v", test.origin, err)
		}
		if !forgejoRemoteMatchesBaseURL(remote, test.baseURL) {
			t.Errorf("remote %q should match default-port base URL %q", test.origin, test.baseURL)
		}
	}
}

func TestForgejoRemoteMatchesBaseURLIgnoresSSHPort(t *testing.T) {
	t.Parallel()
	remote, err := parseBootstrapRemote("ssh://git@code.example.com:2222/acme/looper.git")
	if err != nil {
		t.Fatalf("parseBootstrapRemote() error = %v", err)
	}
	if !forgejoRemoteMatchesBaseURL(remote, "https://code.example.com") {
		t.Fatal("remote SSH port should not be compared with the Forgejo HTTP port")
	}
	if forgejoRemoteMatchesBaseURL(remote, "https://other.example.com") {
		t.Fatal("remote should not match a different Forgejo host")
	}
}

func TestForgejoRemoteMatchesBaseURLAllowsSSHWithHTTPBasePath(t *testing.T) {
	t.Parallel()
	for _, origin := range []string{
		"git@code.example.com:acme/looper.git",
		"ssh://git@code.example.com/acme/looper.git",
	} {
		remote, err := parseBootstrapRemote(origin)
		if err != nil {
			t.Fatalf("parseBootstrapRemote(%q) error = %v", origin, err)
		}
		if !forgejoRemoteMatchesBaseURL(remote, "https://code.example.com/forge") {
			t.Errorf("remote %q should match Forgejo served under an HTTP base path", origin)
		}
	}
}

func TestPrepareProjectAddProviderAcceptsExplicitForgejoID(t *testing.T) {
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
	runtime := newCommandRuntime(New(Deps{LookPath: configLookPathForTests()}), []string{"--config", configPath})
	cmd := newCommand(commandSpec{
		use: "test",
		localFlags: []flagSpec{
			stringFlag("provider", "id", ""),
			stringFlag("repo", "owner/name", ""),
			stringFlag("forgejo-url", "url", ""),
		},
	})
	if err := cmd.Flags().Set("provider", "forgejo"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("repo", "acme/looper"); err != nil {
		t.Fatal(err)
	}

	provider, repo, err := runtime.prepareProjectAddProvider(cmd, "")
	if err != nil {
		t.Fatalf("prepareProjectAddProvider() error = %v", err)
	}
	if provider != "forgejo" || repo != "acme/looper" {
		t.Fatalf("prepareProjectAddProvider() = (%q, %q), want (%q, %q)", provider, repo, "forgejo", "acme/looper")
	}
}

func TestPrepareProjectAddProviderRejectsExplicitForgejoProviderWithMismatchedRemote(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "config.json")
	cfg, err := config.DefaultConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Providers = append(cfg.Providers, config.ProviderConfig{ID: "forgejo-main", Kind: config.ProviderKindForgejo, BaseURL: "https://code.example.com:8443", TokenEnv: stringPtr("FORGEJO_TOKEN")})
	raw, err := config.MarshalConfigFile(configPath, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	runtime := newCommandRuntime(New(Deps{
		LookPath: func(string) (string, error) { return "/usr/bin/git", nil },
		RunCommand: func(context.Context, string, []string, time.Duration) (commandExecutionResult, error) {
			return commandExecutionResult{ExitCode: 0, Stdout: "https://code.example.com:3000/acme/looper.git"}, nil
		},
	}), []string{"--config", configPath})
	cmd := newCommand(commandSpec{
		use: "test",
		localFlags: []flagSpec{
			stringFlag("provider", "id", ""),
			stringFlag("repo", "owner/name", ""),
			stringFlag("forgejo-url", "url", ""),
		},
	})
	if err := cmd.Flags().Set("provider", "forgejo-main"); err != nil {
		t.Fatal(err)
	}

	_, _, err = runtime.prepareProjectAddProvider(cmd, root)
	if err == nil || !strings.Contains(err.Error(), `does not match provider "forgejo-main" base URL "https://code.example.com:8443"`) {
		t.Fatalf("prepareProjectAddProvider() error = %v", err)
	}
}

func TestPrepareProjectAddProviderReusesProviderWithEquivalentBaseURL(t *testing.T) {
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
	cfg.Providers = append(cfg.Providers, config.ProviderConfig{ID: "forgejo-main", Kind: config.ProviderKindForgejo, BaseURL: "http://code.example.com", TokenEnv: stringPtr("FORGEJO_TOKEN")})
	raw, err := config.MarshalConfigFile(configPath, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	equivalentURL := "http://CODE.example.com:80"
	runtime := newCommandRuntime(New(Deps{
		HTTPClient: httpClient,
		LookPath:   func(string) (string, error) { return "/usr/bin/git", nil },
		RunCommand: func(context.Context, string, []string, time.Duration) (commandExecutionResult, error) {
			return commandExecutionResult{ExitCode: 0, Stdout: equivalentURL + "/acme/looper.git"}, nil
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
		"provider":          "forgejo-main",
		"forgejo-url":       equivalentURL,
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
	if provider != "forgejo-main" || repo != "acme/looper" {
		t.Fatalf("prepareProjectAddProvider() = (%q, %q), want (%q, %q)", provider, repo, "forgejo-main", "acme/looper")
	}
}
