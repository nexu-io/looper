package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/hostingidentity"
	"github.com/nexu-io/looper/internal/processcontainment"
)

func botExecutorFixture(t *testing.T, script string, vendor config.AgentVendor) (context.Context, *ConfiguredExecutor, RunInput) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/user":
			io.WriteString(w, `{"id":17,"login":"looper-bot","full_name":"Loop Bot","email":"bot@example.test"}`)
		case "/api/v1/repos/acme/looper":
			io.WriteString(w, `{"full_name":"acme/looper"}`)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(server.Close)
	worktree := t.TempDir()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	git, _ = filepath.Abs(git)
	cmd := exec.Command(git, "init", worktree)
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "agent")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nset -eu\n"+script), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Normalize("")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Tools.GitPath = &git
	cfg.Identities = map[string]config.HostingIdentityConfig{
		"worker": {Kind: config.HostingIdentityForgejoToken, BaseURL: server.URL, TokenEnv: "BOT_EXEC_TOKEN"},
		"other":  {Kind: config.HostingIdentityForgejoToken, BaseURL: server.URL, TokenEnv: "OTHER_EXEC_TOKEN"},
		"app":    {Kind: config.HostingIdentityGitHubApp, BaseURL: "https://github.com", PrivateKeyFile: "/daemon-only/app.pem"},
	}
	legacy := "LEGACY_EXEC_TOKEN"
	cfg.Providers = []config.ProviderConfig{{ID: "forgejo", Kind: config.ProviderKindForgejo, BaseURL: server.URL, TokenEnv: &legacy}}
	manager := hostingidentity.NewManager(hostingidentity.Options{HTTPClient: server.Client(), LookupEnv: func(name string) (string, bool) { return "test-bot-credential", name == "BOT_EXEC_TOKEN" }})
	ctx, err := hostingidentity.BindResolved(hostingidentity.WithManager(context.Background(), manager), config.ResolvedHostingIdentity{Name: "worker", Definition: cfg.Identities["worker"], Target: config.RepositoryIdentity{Kind: config.ProviderKindForgejo, BaseURL: server.URL, Repo: "acme/looper"}, ProjectID: "project", Role: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	looper, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	owner := vendor
	executor := New(ExecutorOptions{Config: ExecutorConfig{Vendor: vendor, Params: map[string]any{"command": path}, NativeResumeEnabled: true, Env: map[string]string{
		"GH_TOKEN": "configured-personal", "BOT_EXEC_TOKEN": "configured-bot", "OTHER_EXEC_TOKEN": "other-bot", "LEGACY_EXEC_TOKEN": "legacy-bot", "KEY_ALIAS": "/daemon-only/app.pem", "SSH_AUTH_SOCK": "/personal/ssh", "OPENAI_API_KEY": "model-value", "GH_CONFIG_DIR": "/personal/gh", "LOOPER_CONFIG": "/daemon-only/config.json",
	}}, ParamsOwnerVendor: &owner, HostingConfig: &cfg, TrustedLooperPath: looper})
	// These contracts run real shell/Git subprocesses alongside the full suite;
	// their assertions concern credentials and lifecycle, not execution speed.
	input := RunInput{ProjectID: "project", WorkingDirectory: worktree, Prompt: "complete local task", Timeout: 30 * time.Second, GracefulShutdown: 10 * time.Millisecond, Env: map[string]string{"GITHUB_TOKEN": "run-personal", "OTHER_EXEC_TOKEN": "run-other", "LOOPER_HOST_CLI": "/injected/looper", "LOOPER_TRUSTED_REVIEW_SOCK": "/injected/socket"}}
	return ctx, executor, input
}

func startBotExecution(t *testing.T, ctx context.Context, executor *ConfiguredExecutor, input RunInput) (Execution, error) {
	t.Helper()
	execution, err := executor.Start(ctx, input)
	if execution != nil {
		// Register before callers can fail an assertion, particularly while
		// waiting for readiness. Drain before fixture directories are removed.
		t.Cleanup(func() {
			if err := execution.Kill("bot fixture cleanup"); err != nil {
				t.Errorf("kill bot fixture during cleanup: %v", err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			result, err := execution.Wait(ctx)
			if err != nil {
				t.Errorf("drain bot fixture during cleanup: %v", err)
			}
			if t.Failed() {
				t.Logf("bot fixture final status=%s timeout=%s summary=%s; stdout=%s stderr=%s", result.Status, result.TimeoutType, result.Summary, result.Stdout, result.Stderr)
			}
		})
	}
	return execution, err
}

const assertBotEnvironment = `
printf 'bot contract: checking environment\n' >&2
test -z "${GH_TOKEN-}${GITHUB_TOKEN-}${BOT_EXEC_TOKEN-}${OTHER_EXEC_TOKEN-}${LEGACY_EXEC_TOKEN-}${KEY_ALIAS-}${SSH_AUTH_SOCK-}${LOOPER_CONFIG-}" || exit 40
test "$OPENAI_API_KEY" = model-value || exit 41
test "$GIT_SSH_COMMAND" = false || exit 42
test -d "$GH_CONFIG_DIR" || exit 43
test -z "$(ls -A "$GH_CONFIG_DIR")" || exit 44
test -S "$LOOPER_TRUSTED_REVIEW_SOCK" || exit 45
test "$LOOPER_HOST_CLI" != /injected/looper || exit 46
if gh auth token >/dev/null 2>&1; then exit 47; fi
if tea api user >/dev/null 2>&1; then exit 48; fi
if git push origin main >/dev/null 2>&1; then exit 49; fi
if git -c protocol.allow=always fetch origin >/dev/null 2>&1; then exit 50; fi
printf 'bot contract: environment checked\n' >&2
printf '%s|%s\n' "$GH_CONFIG_DIR" "$LOOPER_TRUSTED_REVIEW_SOCK" >> "$CAPABILITY_FILE"
`

func TestBotExecutorScrubsMergedEnvAndKeepsLocalCommitAttribution(t *testing.T) {
	capture := filepath.Join(t.TempDir(), "capability")
	ctx, executor, input := botExecutorFixture(t, assertBotEnvironment+`
printf 'bot contract: staging local change\n' >&2
printf 'one\n' > change.txt
git add change.txt
printf 'bot contract: creating attributed commit\n' >&2
git commit -m 'feat: bot commit' >/dev/null
git log -1 --format='author=%an|%ae committer=%cn|%ce'
printf 'bot contract: creating original-author commit\n' >&2
git commit --allow-empty --author='Original <original@example.test>' -m 'test: original author' >/dev/null
printf 'bot contract: amending while preserving author\n' >&2
git commit --amend --no-edit --allow-empty >/dev/null
git log -1 --format='amended=%an|%ae committer=%cn|%ce'
printf '%s\n' '__LOOPER_RESULT__={"summary":"local commits complete"}'
`, config.AgentVendor("custom"))
	input.Env["CAPABILITY_FILE"] = capture
	execution, err := startBotExecution(t, ctx, executor, input)
	if err != nil {
		t.Fatal(err)
	}
	result, err := execution.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "completed" || result.Lifecycle != nil {
		t.Fatalf("unexpected execution status/lifecycle: %s (timeout=%s, summary=%s); stdout=%s stderr=%s", result.Status, result.TimeoutType, result.Summary, result.Stdout, result.Stderr)
	}
	if !strings.Contains(result.Stdout, "author=Loop Bot|bot@example.test committer=Loop Bot|bot@example.test") || !strings.Contains(result.Stdout, "amended=Original|original@example.test committer=Loop Bot|bot@example.test") {
		t.Fatalf("commit attribution incorrect: %s", result.Stdout)
	}
	assertCapabilitiesRemoved(t, capture, 1)
}

func TestBotExecutorNativeFallbackRetainsSanitizedCapabilities(t *testing.T) {
	capture := filepath.Join(t.TempDir(), "capability")
	ctx, executor, input := botExecutorFixture(t, assertBotEnvironment+`
case "$*" in *resume*) printf 'resume failed\n' >&2; exit 2;; esac
printf '%s\n' '__LOOPER_RESULT__={"summary":"checkpoint complete"}'
`, config.AgentVendorCodex)
	input.Env["CAPABILITY_FILE"] = capture
	input.NativeSessionID = "old-session"
	input.NativeResumePrompt = "resume task"
	execution, err := startBotExecution(t, ctx, executor, input)
	if err != nil {
		t.Fatal(err)
	}
	result, err := execution.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "completed" || result.Summary != "checkpoint complete" {
		t.Fatalf("fallback result = %s (timeout=%s, summary=%s); stdout=%s stderr=%s", result.Status, result.TimeoutType, result.Summary, result.Stdout, result.Stderr)
	}
	assertCapabilitiesRemoved(t, capture, 2)
}

func TestBotExecutorKillClosesCapabilities(t *testing.T) {
	capture := filepath.Join(t.TempDir(), "capability")
	ctx, executor, input := botExecutorFixture(t, assertBotEnvironment+"sleep 60\n", config.AgentVendor("custom"))
	input.Env["CAPABILITY_FILE"] = capture
	execution, err := startBotExecution(t, ctx, executor, input)
	if err != nil {
		t.Fatal(err)
	}
	// Leave time to observe readiness before the fixture's runtime deadline.
	deadline := time.Now().Add(20 * time.Second)
	for {
		if data, _ := os.ReadFile(capture); len(data) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("agent did not finish environment checks within 20s")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := execution.Kill("contract test"); err != nil {
		t.Fatal(err)
	}
	if _, err := execution.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertCapabilitiesRemoved(t, capture, 1)
}

func TestBotExecutorStartFailureCleansResources(t *testing.T) {
	root := t.TempDir()
	t.Setenv("TMPDIR", root)
	ctx, executor, input := botExecutorFixture(t, "exit 0\n", config.AgentVendor("custom"))
	executor.config.Params["command"] = filepath.Join(root, "missing-agent")
	if _, err := startBotExecution(t, ctx, executor, input); err == nil {
		t.Fatal("missing agent started")
	}
	for _, pattern := range []string{"looper-agent-host-*", "looper-trusted-review-sock-*"} {
		matches, err := filepath.Glob(filepath.Join(root, pattern))
		if err != nil || len(matches) > 0 {
			t.Fatalf("start failure retained hosting resources (%s): %v", pattern, err)
		}
	}
}

func TestBotExecutorResolvesConfiguredToolNamesBeforeAgentEnv(t *testing.T) {
	capture := filepath.Join(t.TempDir(), "capability")
	ctx, executor, input := botExecutorFixture(t, assertBotEnvironment+"git --version\nprintf '%s\\n' '__LOOPER_RESULT__={\"summary\":\"pinned tools\"}'\n", config.AgentVendor("custom"))
	gitName := "git"
	executor.hostingConfig.Tools.GitPath = &gitName
	shadow := t.TempDir()
	marker := filepath.Join(t.TempDir(), "shadow-ran")
	if err := os.WriteFile(filepath.Join(shadow, "git"), []byte("#!/bin/sh\ntouch '"+marker+"'\nexit 23\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	input.Env["PATH"] = shadow + string(os.PathListSeparator) + os.Getenv("PATH")
	input.Env["CAPABILITY_FILE"] = capture
	execution, err := startBotExecution(t, ctx, executor, input)
	if err != nil {
		t.Fatal(err)
	}
	result, err := execution.Wait(context.Background())
	if err != nil || result.Status != "completed" {
		t.Fatalf("named tool did not run: %v %s", err, result.Stderr)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("agent PATH replaced trusted Git")
	}
	assertCapabilitiesRemoved(t, capture, 1)
}

func TestHostingValidationEnvBlocksPersonalCredentialsAndCleansUp(t *testing.T) {
	cfg, err := config.Normalize("")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Identities = map[string]config.HostingIdentityConfig{"worker": {TokenEnv: "VALIDATION_BOT_TOKEN"}}
	env, cleanup, err := PrepareHostingValidationEnv(cfg, map[string]string{"PATH": os.Getenv("PATH"), "HOME": os.Getenv("HOME"), "GH_TOKEN": "test-personal", "VALIDATION_BOT_TOKEN": "test-bot", "SSH_AUTH_SOCK": "/test/ssh"})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup(nil)
	dir := env["GH_CONFIG_DIR"]
	cmd := exec.Command("/bin/sh", "-c", `test -z "${GH_TOKEN-}${VALIDATION_BOT_TOKEN-}${SSH_AUTH_SOCK-}${LOOPER_HOST_CLI-}${LOOPER_TRUSTED_REVIEW_SOCK-}" && test -d "$GH_CONFIG_DIR" && ! gh auth token >/dev/null 2>&1 && ! tea api user >/dev/null 2>&1 && ! git push origin main >/dev/null 2>&1 && git --version >/dev/null`)
	cmd.Env = envMapToSlice(env)
	if err := cmd.Run(); err != nil {
		t.Fatal("validation environment retained personal hosting access or lost local Git")
	}
	cleanup(nil)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("validation auth directory was retained")
	}
}

func TestHostingValidationCleanupRetainsRoutingOnUnconfirmedDeath(t *testing.T) {
	cfg, err := config.Normalize("")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		runErr error
		retain bool
	}{
		{name: "success"},
		{name: "command failure", runErr: errors.New("exit status 2")},
		{name: "canceled and drained", runErr: context.Canceled},
		{name: "timed out and drained", runErr: context.DeadlineExceeded},
		{name: "unconfirmed", runErr: processcontainment.ErrNotConfirmedDead, retain: true},
		{name: "wrapped unconfirmed", runErr: fmt.Errorf("validation interrupted: %w", processcontainment.ErrNotConfirmedDead), retain: true},
		{name: "failed and unconfirmed", runErr: errors.Join(errors.New("exit status 2"), processcontainment.ErrNotConfirmedDead), retain: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			personalBin := t.TempDir()
			// A surviving descendant would find this personal CLI if cleanup
			// removed the blocking shim from the first PATH entry.
			if err := os.WriteFile(filepath.Join(personalBin, "tea"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			env, cleanup, err := PrepareHostingValidationEnv(cfg, map[string]string{"PATH": personalBin + string(os.PathListSeparator) + os.Getenv("PATH")})
			if err != nil {
				t.Fatal(err)
			}
			dir := filepath.Dir(env["GH_CONFIG_DIR"])
			t.Cleanup(func() { _ = os.RemoveAll(dir) })
			cleanup(tc.runErr)
			// Cleanup is final and idempotent, including a retained directory.
			cleanup(nil)
			if !tc.retain {
				if _, err := os.Stat(dir); !os.IsNotExist(err) {
					t.Fatalf("drained validation retained its hosting paths: %v", err)
				}
				return
			}
			for _, path := range []string{env["GH_CONFIG_DIR"], filepath.Join(dir, "bin", "git"), filepath.Join(dir, "bin", "gh"), filepath.Join(dir, "bin", "tea")} {
				if _, err := os.Stat(path); err != nil {
					t.Fatalf("unconfirmed validation lost hosting path %s: %v", path, err)
				}
			}
			cmd := exec.Command("/bin/sh", "-c", "tea api user")
			cmd.Env = envMapToSlice(env)
			if err := cmd.Run(); err == nil {
				t.Fatal("unconfirmed validation fell through to personal tea")
			}
		})
	}
}

func assertCapabilitiesRemoved(t *testing.T, path string, count int) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != count {
		t.Fatalf("capability observation count = %d, want %d", len(lines), count)
	}
	for _, line := range lines {
		parts := strings.Split(line, "|")
		if len(parts) != 2 {
			t.Fatal("invalid capability observation")
		}
		for _, resource := range parts {
			if _, err := os.Stat(resource); !os.IsNotExist(err) {
				t.Fatal(fmt.Sprintf("execution retained a hosting resource: %v", err))
			}
		}
	}
}
