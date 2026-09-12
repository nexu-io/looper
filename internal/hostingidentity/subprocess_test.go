package hostingidentity

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/infra/shell"
)

const subprocessToken = "test-installation-token-must-remain-in-daemon"

func subprocessBinding(t *testing.T, baseURL string, kind config.HostingIdentityKind) (context.Context, *Session) {
	t.Helper()
	resolved := githubResolved()
	resolved.Definition.BaseURL, resolved.Target.BaseURL = baseURL, baseURL
	if kind == config.HostingIdentityForgejoToken {
		resolved.Definition = config.HostingIdentityConfig{Kind: kind, BaseURL: baseURL, TokenEnv: "SELECTED_FORGEJO_TOKEN"}
		resolved.Target.Kind = config.ProviderKindForgejo
	}
	manager := NewManager(Options{
		LookupEnv: func(name string) (string, bool) { return subprocessToken, name == "SELECTED_FORGEJO_TOKEN" },
		ReadFile:  func(string) ([]byte, error) { return nil, errors.New("test does not have an App key") },
	})
	session := sessionFor(t, manager, resolved)
	session.entry.credential = Credential{Token: subprocessToken, Login: "looper-worker[bot]", NumericID: 9917, Name: "looper-worker[bot]", Email: "9917+looper-worker[bot]@users.noreply.github.com", ExpiresAt: time.Now().Add(time.Hour)}
	session.entry.refreshAt = time.Now().Add(time.Hour)
	return bindingContext(session), session
}

func bindingContext(session *Session) context.Context {
	return context.WithValue(context.Background(), sessionContextKey{}, runBinding{projectID: session.ProjectID(), role: session.Role(), session: session})
}

func TestSanitizeAgentEnvRemovesEveryHostingSourceAfterMerge(t *testing.T) {
	providerToken := "LEGACY_CUSTOM_TOKEN"
	cfg := config.Config{
		Identities: map[string]config.HostingIdentityConfig{
			"selected": {TokenEnv: "MY_SELECTED_TOKEN"},
			"other":    {TokenEnv: "OTHER_ROLE_TOKEN", PrivateKeyFile: "/trusted/app.pem"},
		},
		Providers: []config.ProviderConfig{{TokenEnv: &providerToken}},
	}
	env := map[string]string{
		"PATH": "/bin", "OPENAI_API_KEY": "keep-model", "ANTHROPIC_API_KEY": "keep-other-model",
		"MY_SELECTED_TOKEN": "selected", "OTHER_ROLE_TOKEN": "unselected", "LEGACY_CUSTOM_TOKEN": "legacy",
		"GH_TOKEN": "personal", "GITHUB_TOKEN": "ci", "GH_ENTERPRISE_TOKEN": "enterprise", "TEA_CONFIG": "/personal/tea.yml",
		"FORGEJO_TOKEN": "personal", "SSH_AUTH_SOCK": "/ssh/agent", "GIT_ASKPASS": "/trusted/helper",
		"GIT_CONFIG_COUNT": "1", "GIT_CONFIG_KEY_0": "http.extraHeader", "GIT_CONFIG_VALUE_0": "Authorization: secret",
		"GIT_AUTHOR_NAME": "Human", "GIT_AUTHOR_EMAIL": "human@example.com", "GIT_CONFIG_PARAMETERS": "injected",
		"LOOPER_CONFIG": "/trusted/config.json", "LOOPER_TRUSTED_REVIEW_SOCKET": "/prior/run.sock", "MY_PRIVATE_KEY": "secret",
		"ARBITRARY_KEY_PATH": "/trusted/app.pem", "GIT_SSH_COMMAND": "/trusted/ssh", "WORKTREE": "/workspace",
	}
	original := make(map[string]string, len(env))
	for key, value := range env {
		original[key] = value
	}
	clean := SanitizeAgentEnv(cfg, env)
	for key, value := range env {
		switch key {
		case "PATH", "OPENAI_API_KEY", "ANTHROPIC_API_KEY", "WORKTREE":
			if clean[key] != value {
				t.Errorf("required agent environment %s was lost", key)
			}
		case "GIT_SSH_COMMAND":
			if clean[key] != "false" {
				t.Error("SSH transport was not disabled")
			}
		default:
			if _, exists := clean[key]; exists {
				t.Errorf("hosting capability %s survived the final scrub", key)
			}
		}
	}
	if !reflect.DeepEqual(env, original) {
		t.Fatal("sanitizer mutated the caller's environment")
	}
}

func TestHostingSubprocessLegacyIsExactPassthrough(t *testing.T) {
	for _, run := range []struct {
		name string
		fn   func(context.Context, shell.Options, func(context.Context, shell.Options) (shell.Result, error)) (shell.Result, error)
	}{{"git", RunGit}, {"gh", RunGH}} {
		t.Run(run.name, func(t *testing.T) {
			options := shell.Options{Command: "custom", Args: []string{"extension", "custom"}, CWD: "/legacy", Env: map[string]string{"GH_TOKEN": "legacy"}, Stdin: "body", Timeout: time.Second}
			want := errors.New("legacy error")
			result, err := run.fn(context.Background(), options, func(_ context.Context, got shell.Options) (shell.Result, error) {
				if !reflect.DeepEqual(got, options) {
					t.Fatalf("legacy options changed: %+v", got)
				}
				return shell.Result{Stdout: "legacy result"}, want
			})
			if result.Stdout != "legacy result" || err != want {
				t.Fatal("legacy result or error changed")
			}
		})
	}
}

func TestRunGHSelectsHostAndPrivateConfig(t *testing.T) {
	for _, tc := range []struct{ host, tokenVar string }{
		{"https://github.com", "GH_TOKEN"},
		{"https://company.ghe.com", "GH_TOKEN"},
		{"https://github.enterprise.test", "GH_ENTERPRISE_TOKEN"},
	} {
		t.Run(tc.host, func(t *testing.T) {
			ctx, session := subprocessBinding(t, tc.host, config.HostingIdentityGitHubApp)
			var configDir string
			options := shell.Options{Command: "gh", Args: []string{"label", "list", "--repo", "org/repo"}, CWD: "/workspace", Stdin: "body", Env: map[string]string{
				"PATH": "/bin", "GH_TOKEN": "personal", "GH_ENTERPRISE_TOKEN": "personal-enterprise", "UNSELECTED_TOKEN": "other", "GIT_ASKPASS": "/helper", "SSH_AUTH_SOCK": "/agent", "HTTPS_PROXY": "http://proxy", "OPENAI_API_KEY": "model", "GH_CONFIG_DIR": "/personal/config",
			}}
			_, err := RunGH(ctx, options, func(_ context.Context, got shell.Options) (shell.Result, error) {
				if got.Env[tc.tokenVar] != subprocessToken || got.Env["GH_HOST"] != strings.TrimPrefix(tc.host, "https://") || got.Env["GH_REPO"] != strings.TrimPrefix(tc.host, "https://")+"/org/repo" {
					t.Fatal("CLI did not receive exactly the selected host/repository/token")
				}
				for _, value := range got.Env {
					if value == "personal" || value == "personal-enterprise" || value == "other" || value == "/helper" || value == "/agent" || value == "http://proxy" || value == "model" {
						t.Fatal("privileged CLI inherited unrelated credentials or transport settings")
					}
				}
				if got.CWD != got.Env["HOME"] || got.CWD != got.Env["GH_CONFIG_DIR"] || got.CWD == options.CWD {
					t.Fatal("credential-bearing CLI still runs inside the caller's checkout")
				}
				if got.Stdin != options.Stdin || !reflect.DeepEqual(got.Args, options.Args) {
					t.Fatal("CLI invocation data changed")
				}
				configDir = got.Env["GH_CONFIG_DIR"]
				info, statErr := os.Stat(configDir)
				if statErr != nil || info.Mode().Perm() != 0700 {
					t.Fatalf("CLI configuration is not private: %v", statErr)
				}
				return shell.Result{Stdout: session.Target().Repo}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(configDir); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("temporary CLI configuration was not removed")
			}
		})
	}
}

func TestRunGHRefreshesPerOperationAndRedactsExecutionErrors(t *testing.T) {
	fixture := newGitHubFixture(t, false)
	session := sessionFor(t, NewManager(fixture.options()), githubResolved())
	ctx := bindingContext(session)
	for call := 1; call <= 2; call++ {
		wantToken := fmt.Sprintf("installation-token-%d", call)
		result, err := RunGH(ctx, shell.Options{Command: "gh", Args: []string{"api", "repos/org/repo/pulls"}}, func(_ context.Context, options shell.Options) (shell.Result, error) {
			if options.Env["GH_TOKEN"] != wantToken {
				t.Fatal("operation used a stale or unselected token")
			}
			leak := wantToken + " " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+wantToken)) + " installation-token-1"
			result := shell.Result{Stdout: leak, Stderr: leak, ExitCode: 7, StdoutTruncated: true, Duration: time.Second}
			return result, &shell.CommandExecutionError{Message: "failed: " + leak, Result: result}
		})
		var execution *shell.CommandExecutionError
		if !errors.As(err, &execution) || execution.Result.ExitCode != 7 || !result.StdoutTruncated || result.Duration != time.Second {
			t.Fatalf("execution error contract was lost: %v", err)
		}
		for _, text := range []string{result.Stdout, result.Stderr, err.Error(), execution.Result.Stdout, execution.Result.Stderr} {
			if strings.Contains(text, "installation-token-") || strings.Contains(text, base64.StdEncoding.EncodeToString([]byte("x-access-token:"+wantToken))) {
				t.Fatal("selected credential survived result/error redaction")
			}
		}
		fixture.clock.advance(time.Hour)
	}
	if fixture.mints.Load() != 2 {
		t.Fatal("the second operation did not refresh the expired credential")
	}
}

func TestRunGHRejectsForeignTargetsAndCredentialLaunchingCommands(t *testing.T) {
	ctx, _ := subprocessBinding(t, "https://github.com", config.HostingIdentityGitHubApp)
	for _, args := range [][]string{
		{"api", "repos/other/repo/issues"}, {"api", "https://evil.test/repos/org/repo"}, {"api", "repos/org/repo/%2e%2e/other"},
		{"api", "repos/org/repo", "--hostname", "evil.test"}, {"pr", "view", "7", "--repo=other/repo"}, {"pr", "view", "https://github.com/other/repo/pull/7"},
		{"pr", "view", "7", "-Rother/repo"}, {"repo", "view", "other/repo"}, {"repo", "clone", "org/repo"}, {"pr", "checkout", "7"}, {"auth", "status"}, {"extension", "exec", "unsafe"},
		{"pr", "comment", "42", "--body", "--web", "--repo", "other/repo"},
		{"pr", "view", "--repo", "org/repo", "https://github.com/other/repo/pull/7"},
		{"repo", "view", "--json", "nameWithOwner", "other/repo"},
		{"pr", "comment", "42", "--body", "body", "--web"}, {"pr", "create", "--editor"}, {"pr", "close", "42", "--delete-branch"},
		{"pr", "view", "42", "-w"}, {"pr", "create", "-e"}, {"pr", "view", "42", "--unknown"},
		{"--repo", "org/repo", "pr", "view", "42"}, {"-C", "/another/repo", "pr", "view", "42"},
		{"pr", "comment", "42", "--body"}, {"pr", "view", "42", "--repo"},
		{"api", "--method", "GET", "repos/org/repo", "repos/other/repo"},
		{"api", "--method", "GET", "repos/org/repo", "--hostname=other.test"},
	} {
		_, err := RunGH(ctx, shell.Options{Args: args}, func(context.Context, shell.Options) (shell.Result, error) {
			t.Fatal("rejected command reached a credential-bearing runner")
			return shell.Result{}, nil
		})
		if err == nil {
			t.Errorf("command was accepted: %q", args)
		}
	}
	for _, args := range [][]string{
		{"api", "--paginate", "--slurp", "repos/org/repo/issues"},
		{"api", "-X", "GET", "repos/org/repo/branches/feature%2Fwork/protection"},
		{"api", "https://api.github.com/repos/org/repo"},
		{"label", "create", "spec:reviewing", "--repo", "org/repo", "--color", "ffffff"},
		{"pr", "comment", "42", "--body=--web", "--repo=org/repo"},
		{"pr", "view", "-Rorg/repo", "42", "--json", "headRefOid"},
		{"pr", "view", "-R=org/repo", "42"},
		{"repo", "view", "--json", "nameWithOwner,url"},
	} {
		if _, err := RunGH(ctx, shell.Options{Args: args}, func(context.Context, shell.Options) (shell.Result, error) { return shell.Result{}, nil }); err != nil {
			t.Errorf("daemon API command rejected: %q: %v", args, err)
		}
	}
}

func TestRunGHPublicationValuesAreNotCommandOptions(t *testing.T) {
	ctx, _ := subprocessBinding(t, "https://github.com", config.HostingIdentityGitHubApp)
	for _, body := range []string{"--repo=owner/name documents a CLI flag", "-Regression details", "--web", "--hostname=other.test", "--editor", "--delete-branch"} {
		for _, args := range [][]string{
			{"pr", "comment", "42", "--repo", "org/repo", "--body", body},
			{"pr", "review", "42", "--repo", "org/repo", "--comment", "--body", body},
			{"pr", "edit", "42", "--title", body, "--repo", "org/repo"},
			{"pr", "create", "--repo", "org/repo", "--head", "topic", "--base", "main", "--title", body, "--body", body},
			{"api", "-f", "body=" + body, "repos/org/repo/issues/42/comments", "--method", "POST"},
		} {
			called := false
			_, err := RunGH(ctx, shell.Options{Args: args}, func(_ context.Context, got shell.Options) (shell.Result, error) {
				called = true
				if !reflect.DeepEqual(got.Args, args) {
					t.Fatal("publication arguments changed")
				}
				return shell.Result{}, nil
			})
			if err != nil || !called {
				t.Errorf("literal publication value rejected for %q: %v", args, err)
			}
		}
	}
}

func TestRunGHChildGitCannotLoadRepositoryHelpers(t *testing.T) {
	repo := initHumanRepository(t)
	helperDir := t.TempDir()
	helper := filepath.Join(helperDir, "fsmonitor")
	marker := filepath.Join(helperDir, "saw-selected-token")
	// Record only whether the fake selected token was present, never its value.
	script := "#!/bin/sh\nif [ -n \"$GH_TOKEN\" ]; then printf visible > \"${0%/*}/saw-selected-token\"; fi\nprintf '\\000'\n"
	if err := os.WriteFile(helper, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	plainGit(t, repo, "config", "core.fsmonitor", helper)
	status := shell.Options{Command: testGitPath(t), Args: []string{"status", "--porcelain"}, CWD: repo, Env: map[string]string{"PATH": os.Getenv("PATH"), "GH_TOKEN": subprocessToken}}
	if _, err := shell.Run(context.Background(), status); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("positive control did not run the repository's fsmonitor with the fake token")
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	// Temporary roots can themselves be in a repository. Include a relative
	// symlink into a root containing Git's Unix path-list delimiter so the
	// contract does not depend on a naively encoded discovery ceiling.
	colonRoot := filepath.Join(repo, "temporary:root")
	if err := os.Mkdir(colonRoot, 0700); err != nil {
		t.Fatal(err)
	}
	linkedRoot := filepath.Join(t.TempDir(), "linked-temporary-root")
	if err := os.Symlink(colonRoot, linkedRoot); err != nil {
		t.Fatal(err)
	}
	currentDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	relativeRoot, err := filepath.Rel(currentDir, linkedRoot)
	if err != nil {
		t.Fatal(err)
	}
	ctx, _ := subprocessBinding(t, "https://github.com", config.HostingIdentityGitHubApp)
	for _, tc := range []struct{ name, tmpdir string }{
		{"outside repository", t.TempDir()},
		{"repository root", repo},
		{"relative symlink into colon path", relativeRoot},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TMPDIR", tc.tmpdir)
			t.Cleanup(func() { _ = os.Remove(marker) })
			_, err := RunGH(ctx, shell.Options{CWD: repo, Args: []string{"pr", "create", "--repo", "org/repo", "--head", "topic", "--base", "main", "--title", "title", "--body", "body"}}, func(ctx context.Context, options shell.Options) (shell.Result, error) {
				// gh v2.65.0 inspects git status even when pr create has an
				// explicit head. Exercise that real child command with RunGH's
				// exact CWD/env, including repository discovery above TMPDIR.
				options.Command, options.Args = status.Command, status.Args
				result, err := shell.Run(ctx, options)
				if err == nil || result.ExitCode != 128 {
					t.Errorf("child Git did not fail outside a repository: exit=%d, err=%v", result.ExitCode, err)
				}
				return shell.Result{}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("repository fsmonitor received the selected GH token")
			}
		})
	}
}

func TestCredentialCommandTimeoutFitsTokenAndPreservesShorterCallerLimit(t *testing.T) {
	for _, tc := range []struct {
		name             string
		remaining, input time.Duration
		want             time.Duration
		wantError        bool
	}{
		{name: "unbounded command", remaining: time.Hour, want: 4 * time.Minute},
		{name: "shorter caller timeout", remaining: time.Hour, input: 20 * time.Second, want: 20 * time.Second},
		{name: "short server lifetime", remaining: 30 * time.Second, input: 3 * time.Minute, want: 25 * time.Second},
		{name: "no useful lifetime", remaining: 4 * time.Second, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, session := subprocessBinding(t, "https://github.com", config.HostingIdentityGitHubApp)
			now := time.Now()
			session.manager.now = func() time.Time { return now }
			session.entry.credential.ExpiresAt = now.Add(tc.remaining)
			called := false
			_, err := RunGH(ctx, shell.Options{Args: []string{"api", "repos/org/repo"}, Timeout: tc.input}, func(_ context.Context, options shell.Options) (shell.Result, error) {
				called = true
				if options.Timeout != tc.want {
					t.Fatalf("timeout = %s, want %s", options.Timeout, tc.want)
				}
				return shell.Result{}, nil
			})
			if (err != nil) != tc.wantError || called == tc.wantError {
				t.Fatalf("expiry contract: error=%v, called=%v", err, called)
			}
		})
	}
}
