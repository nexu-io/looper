package hostingidentity

import (
	"context"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/infra/shell"
	"github.com/nexu-io/looper/internal/processcontainment"
)

func testGitPath(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("git")
	if err != nil {
		t.Fatal("Git is required for the hosting transport contract")
	}
	return path
}

func plainGit(t *testing.T, cwd string, args ...string) string {
	t.Helper()
	env := map[string]string{"PATH": os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": os.DevNull, "GIT_TERMINAL_PROMPT": "0"}
	gitConfigEnv(env, [][2]string{{"core.hooksPath", os.DevNull}, {"commit.gpgSign", "false"}})
	result, err := shell.Run(context.Background(), shell.Options{Command: testGitPath(t), Args: args, CWD: cwd, Env: env})
	if err != nil {
		t.Fatalf("fixture git %q: %v", args, err)
	}
	return strings.TrimSpace(result.Stdout)
}

func initHumanRepository(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	plainGit(t, repo, "init", "--initial-branch=main")
	plainGit(t, repo, "config", "user.name", "Original Human")
	plainGit(t, repo, "config", "user.email", "human@example.com")
	writeGitFile(t, repo, "base.txt", "base\n")
	plainGit(t, repo, "add", "base.txt")
	plainGit(t, repo, "commit", "-m", "human base")
	return repo
}

func writeGitFile(t *testing.T, repo, path, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, path), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestRunGitLocalAttributionPreservesOriginalAuthorsAndReviewAnchorCommands(t *testing.T) {
	repo := initHumanRepository(t)
	base := plainGit(t, repo, "rev-parse", "HEAD")
	ctx, session := subprocessBinding(t, "https://github.com", config.HostingIdentityGitHubApp)
	for _, hook := range []string{"pre-commit", "commit-msg", "post-commit", "pre-push"} {
		if err := os.WriteFile(filepath.Join(repo, ".git", "hooks", hook), []byte("#!/bin/sh\n/usr/bin/env > hook-environment\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	botGit := func(args ...string) string {
		t.Helper()
		result, err := RunGit(ctx, shell.Options{Command: testGitPath(t), CWD: repo, Args: args, Env: map[string]string{
			"PATH": os.Getenv("PATH"), "GH_TOKEN": "ambient-token", "UNSELECTED_TOKEN": "other-token", "GIT_AUTHOR_NAME": "Ambient Author", "GIT_AUTHOR_EMAIL": "ambient@example.com", "GIT_COMMITTER_NAME": "Ambient Committer",
		}}, func(ctx context.Context, options shell.Options) (shell.Result, error) {
			for _, value := range options.Env {
				if strings.Contains(value, subprocessToken) || value == "ambient-token" || value == "other-token" || value == "Ambient Author" || value == "ambient@example.com" {
					t.Fatal("local Git received credentials or inherited author overrides")
				}
			}
			return shell.Run(ctx, options)
		})
		if err != nil {
			t.Fatalf("bot git %q: %v", args, err)
		}
		return strings.TrimSpace(result.Stdout)
	}
	assertAttribution := func(wantAuthor string) {
		t.Helper()
		got := plainGit(t, repo, "log", "-1", "--format=%an|%ae|%cn|%ce")
		want := wantAuthor + "|looper-worker[bot]|9917+looper-worker[bot]@users.noreply.github.com"
		if got != want {
			t.Fatalf("commit attribution = %q, want %q", got, want)
		}
	}
	botGit("commit", "--amend", "--no-edit")
	assertAttribution("Original Human|human@example.com")
	anchorBase := plainGit(t, repo, "rev-parse", "HEAD")
	writeGitFile(t, repo, "bot.txt", "new bot content\n")
	botGit("add", "-A")
	botGit("commit", "-m", "new bot commit")
	assertAttribution("looper-worker[bot]|9917+looper-worker[bot]@users.noreply.github.com")
	botHead := plainGit(t, repo, "rev-parse", "HEAD")
	plainGit(t, repo, "checkout", "-b", "human-source", base)
	writeGitFile(t, repo, "human-source.txt", "cherry pick me\n")
	plainGit(t, repo, "add", "-A")
	plainGit(t, repo, "commit", "-m", "human change")
	humanHead := plainGit(t, repo, "rev-parse", "HEAD")
	plainGit(t, repo, "checkout", "-b", "bot-target", botHead)
	botGit("cherry-pick", humanHead)
	assertAttribution("Original Human|human@example.com")
	if got := botGit("--literal-pathspecs", "diff", "--name-status", "-M", "--no-ext-diff", "--no-color", anchorBase+"...HEAD"); !strings.Contains(got, "bot.txt") {
		t.Fatalf("review rename-anchor command did not produce the diff: %q", got)
	}
	if got := botGit("--literal-pathspecs", "diff", "--no-ext-diff", "--no-color", anchorBase+"...HEAD", "--", "bot.txt"); !strings.Contains(got, "+new bot content") {
		t.Fatalf("review path-anchor command did not produce the diff: %q", got)
	}
	if _, err := os.Stat(filepath.Join(repo, "hook-environment")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("bot Git ran a repository hook")
	}
	// Read-only/local setup remains usable even when fresh credentials cannot
	// be acquired. It never falls through to a personal login or token.
	session.Invalidate("")
	botGit("rev-parse", "--verify", "HEAD^{commit}")
	botGit("status", "--porcelain")
}

type gitTransportTracker struct{ began, tracked, released atomic.Int64 }

func (tracker *gitTransportTracker) BeginTrack() (func(), error) {
	tracker.began.Add(1)
	return func() {}, nil
}
func (tracker *gitTransportTracker) Track(*processcontainment.Handle) func() {
	tracker.tracked.Add(1)
	return func() { tracker.released.Add(1) }
}
func (*gitTransportTracker) ReportDrainFailure(error) {}

func TestRunGitHTTPSUsesSelectedAuthAndPreservesSSHRemote(t *testing.T) {
	for _, kind := range []config.HostingIdentityKind{config.HostingIdentityGitHubApp, config.HostingIdentityForgejoToken} {
		t.Run(string(kind), func(t *testing.T) {
			gitPath := testGitPath(t)
			serverRoot := t.TempDir()
			bare := filepath.Join(serverRoot, "org", "repo.git")
			if err := os.MkdirAll(bare, 0700); err != nil {
				t.Fatal(err)
			}
			plainGit(t, bare, "init", "--bare", "--initial-branch=main")
			plainGit(t, bare, "config", "http.receivepack", "true")
			seed := initHumanRepository(t)
			plainGit(t, seed, "push", bare, "HEAD:refs/heads/main")
			backend := &cgi.Handler{Path: gitPath, Args: []string{"http-backend"}, Dir: serverRoot, Env: []string{"GIT_PROJECT_ROOT=" + serverRoot, "GIT_HTTP_EXPORT_ALL=1", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull}}
			prefix := ""
			if kind == config.HostingIdentityForgejoToken {
				prefix = "/forge"
			}
			var serveGit http.Handler = backend
			if prefix != "" {
				serveGit = http.StripPrefix(prefix, backend)
			}
			var requests atomic.Int64
			username := "x-access-token"
			if kind == config.HostingIdentityForgejoToken {
				username = "looper-worker[bot]"
			}
			server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				login, token, ok := request.BasicAuth()
				if !ok || login != username || token != subprocessToken || !strings.HasPrefix(request.URL.Path, prefix+"/org/repo.git/") {
					t.Error("Git request did not carry the selected repository-scoped Basic credential")
					response.WriteHeader(http.StatusUnauthorized)
					return
				}
				requests.Add(1)
				serveGit.ServeHTTP(response, request)
			}))
			t.Cleanup(server.Close)
			certificate := filepath.Join(t.TempDir(), "git-test-ca.pem")
			if err := os.WriteFile(certificate, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
				t.Fatal(err)
			}
			repo := t.TempDir()
			plainGit(t, repo, "init", "--initial-branch=main")
			serverURL, _ := url.Parse(server.URL)
			sshURL := "git@" + serverURL.Hostname() + ":org/repo.git"
			plainGit(t, repo, "remote", "add", "origin", sshURL)
			if err := os.WriteFile(filepath.Join(repo, ".git", "hooks", "pre-push"), []byte("#!/bin/sh\n/usr/bin/env > hook-environment\n"), 0700); err != nil {
				t.Fatal(err)
			}
			ctx, _ := subprocessBinding(t, server.URL+prefix, kind)
			personalHome := t.TempDir()
			writeGitFile(t, personalHome, ".netrc", "machine "+serverURL.Hostname()+" login personal password unselected\n")
			tracker := &gitTransportTracker{}
			var gateCalls, processCalls atomic.Int64
			runNetwork := func(args ...string) shell.Result {
				t.Helper()
				result, err := RunGit(ctx, shell.Options{
					Command: gitPath, CWD: repo, Args: args, Timeout: 20 * time.Second, Tracker: tracker,
					StartGate: func(start func() error) error { gateCalls.Add(1); return start() },
					Env:       map[string]string{"PATH": os.Getenv("PATH"), "HOME": personalHome, "GH_TOKEN": "personal", "OTHER_ROLE_TOKEN": "unselected", "SSH_AUTH_SOCK": "/personal/agent", "HTTPS_PROXY": "http://invalid.proxy", "GIT_CONFIG_PARAMETERS": "injected", "SSL_CERT_FILE": certificate, "SSL_CERT_DIR": filepath.Dir(certificate), "GIT_SSL_NO_VERIFY": "true", "GIT_SSL_KEY": "/personal/client.key"},
				}, func(ctx context.Context, options shell.Options) (shell.Result, error) {
					processCalls.Add(1)
					if options.Env["GIT_SSL_CAINFO"] != certificate || options.Env["GIT_SSL_CAPATH"] != filepath.Dir(certificate) {
						t.Fatal("Git lost the daemon's CA file or directory")
					}
					if options.Env["GIT_SSL_NO_VERIFY"] != "" || options.Env["GIT_SSL_KEY"] != "" {
						t.Fatal("Git inherited TLS verification bypass or client private key")
					}
					if options.Tracker != tracker || options.StartGate == nil || options.Timeout != 20*time.Second {
						t.Fatal("Git transport dropped process containment/start-admission options")
					}
					if options.Env["HOME"] == personalHome || options.Env["HOME"] == "" {
						t.Fatal("Git transport could read personal netrc credentials")
					}
					if strings.Contains(strings.Join(options.Args, " "), subprocessToken) || strings.Contains(strings.Join(options.Args, " "), base64.StdEncoding.EncodeToString([]byte(username+":"+subprocessToken))) {
						t.Fatal("credential was put into Git argv")
					}
					for _, value := range options.Env {
						if value == "personal" || value == "unselected" || value == "/personal/agent" || value == "http://invalid.proxy" || value == "injected" {
							t.Fatal("Git inherited unselected credentials or transport overrides")
						}
					}
					return shell.Run(ctx, options)
				})
				if err != nil {
					t.Fatalf("HTTPS bot Git %q: %v", args, err)
				}
				return result
			}
			runNetwork("fetch", "origin", "+refs/heads/main:refs/remotes/origin/main", "--no-tags", "--depth", "1")
			fetchHead, err := os.ReadFile(filepath.Join(repo, ".git", "FETCH_HEAD"))
			if err != nil {
				t.Fatal(err)
			}
			runNetwork("fetch", "--no-write-fetch-head", "--no-tags", "--refmap=", "origin", plainGit(t, bare, "rev-parse", "main"))
			if after, err := os.ReadFile(filepath.Join(repo, ".git", "FETCH_HEAD")); err != nil || string(after) != string(fetchHead) {
				t.Fatal("Forgejo commit-inspection fetch changed FETCH_HEAD")
			}
			if plainGit(t, repo, "rev-parse", "refs/remotes/origin/main") != plainGit(t, bare, "rev-parse", "main") {
				t.Fatal("fetch did not update the original named remote's tracking ref")
			}
			plainGit(t, repo, "checkout", "-b", "bot-work", "origin/main")
			writeGitFile(t, repo, "bot.txt", "bot publication\n")
			for _, args := range [][]string{{"add", "-A"}, {"commit", "-m", "bot publication"}} {
				if _, err := RunGit(ctx, shell.Options{Command: gitPath, CWD: repo, Args: args}, nil); err != nil {
					t.Fatal(err)
				}
			}
			runNetwork("push", "--porcelain", "--force-with-lease=refs/heads/bot-work:", "-u", "origin", "HEAD:refs/heads/bot-work")
			remote := runNetwork("ls-remote", "--heads", "origin", "refs/heads/bot-work")
			if !strings.Contains(remote.Stdout, plainGit(t, repo, "rev-parse", "HEAD")) {
				t.Fatal("published commit not visible through authenticated ls-remote")
			}
			if got := plainGit(t, repo, "config", "--get", "remote.origin.url"); got != sshURL {
				t.Fatalf("SSH origin was changed on disk: %q", got)
			}
			if got := plainGit(t, repo, "config", "--get", "branch.bot-work.remote"); got != "origin" {
				t.Fatalf("push -u lost named-remote tracking: %q", got)
			}
			diskConfig, err := os.ReadFile(filepath.Join(repo, ".git", "config"))
			if err != nil || strings.Contains(string(diskConfig), subprocessToken) || strings.Contains(string(diskConfig), "Authorization") || strings.Contains(string(diskConfig), server.URL) {
				t.Fatal("credential or temporary HTTPS rewrite was persisted to Git config")
			}
			if _, err := os.Stat(filepath.Join(repo, "hook-environment")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("credential-bearing Git invoked a repository pre-push hook")
			}
			if requests.Load() == 0 || processCalls.Load() != 8 || gateCalls.Load() != 8 || tracker.began.Load() != 8 || tracker.tracked.Load() != 8 || tracker.released.Load() != 8 {
				t.Fatalf("network subprocess lifecycle incomplete: requests=%d, processes=%d, gates=%d, tracked=%d, released=%d", requests.Load(), processCalls.Load(), gateCalls.Load(), tracker.tracked.Load(), tracker.released.Load())
			}
		})
	}
}

func TestRunGitRejectsRepositoryTransportEscapesBeforeCredentials(t *testing.T) {
	for _, tc := range []struct{ key, value string }{
		{"url.https://evil.test/.insteadOf", "https://github.com/"},
		{"http.https://github.com/.extraHeader", "Authorization: local-secret"},
		{"credential.helper", "!echo local-secret"},
		{"remote.origin.vcs", "custom"},
		{"remote.origin.receivepack", "unsafe-command"},
		{"remote.origin.pushurl", "https://github.com/fork/repo.git"},
		{"remote.origin.promisor", "true"},
	} {
		t.Run(tc.key, func(t *testing.T) {
			repo := initHumanRepository(t)
			plainGit(t, repo, "remote", "add", "origin", "git@github.com:org/repo.git")
			plainGit(t, repo, "config", tc.key, tc.value)
			assertTransportRejectedBeforeCredentials(t, repo)
		})
	}
	t.Run("worktree-specific config is inspected", func(t *testing.T) {
		repo := initHumanRepository(t)
		plainGit(t, repo, "remote", "add", "origin", "git@github.com:org/repo.git")
		plainGit(t, repo, "config", "extensions.worktreeConfig", "true")
		plainGit(t, repo, "config", "--worktree", "url.https://evil.test/.insteadOf", "git@github.com:")
		assertTransportRejectedBeforeCredentials(t, repo)
	})
	t.Run("config parse errors hide file content", func(t *testing.T) {
		repo := initHumanRepository(t)
		writeGitFile(t, repo, ".git/config", "[invalid-local-secret\n")
		assertTransportRejectedBeforeCredentials(t, repo)
	})
}

func assertTransportRejectedBeforeCredentials(t *testing.T, repo string, args ...string) {
	t.Helper()
	ctx, _ := subprocessBinding(t, "https://github.com", config.HostingIdentityGitHubApp)
	if len(args) == 0 {
		args = []string{"fetch", "origin", "main"}
	}
	calls := 0
	_, err := RunGit(ctx, shell.Options{Command: testGitPath(t), CWD: repo, Args: args}, func(ctx context.Context, options shell.Options) (shell.Result, error) {
		calls++
		if len(options.Args) == 0 || options.Args[0] != "config" {
			t.Fatal("unsafe transport reached the credential-bearing runner")
		}
		for _, value := range options.Env {
			if strings.Contains(value, subprocessToken) {
				t.Fatal("config probe received a credential")
			}
		}
		return shell.Run(ctx, options)
	})
	if err == nil || calls != 1 || strings.Contains(err.Error(), "local-secret") {
		t.Fatalf("unsafe repository config was not rejected safely: calls=%d, err=%v", calls, err)
	}
}

func TestRunGitRejectsUnboundedArgumentsAndNeverFallsBack(t *testing.T) {
	ctx, session := subprocessBinding(t, "https://github.com", config.HostingIdentityGitHubApp)
	for _, args := range [][]string{
		{"-c", "credential.helper=unsafe", "fetch", "origin", "main"}, {"-C", "/other", "fetch", "origin", "main"},
		{"--git-dir=/other", "fetch", "origin", "main"}, {"custom-alias"}, {"fetch", "--all"}, {"fetch", "origin"},
		{"fetch", "--upload-pack=unsafe", "origin", "main"}, {"push", "origin", "main", "--receive-pack=unsafe"}, {"fetch", "origin", "main", "--depth=bad"},
	} {
		if _, err := RunGit(ctx, shell.Options{Args: args}, func(context.Context, shell.Options) (shell.Result, error) {
			t.Fatal("unsupported argument reached a subprocess")
			return shell.Result{}, nil
		}); err == nil {
			t.Errorf("unsafe Git command accepted: %q", args)
		}
	}
	repo := initHumanRepository(t)
	plainGit(t, repo, "remote", "add", "origin", "git@github.com:org/repo.git")
	for _, remote := range []string{"https://github.com/fork/repo.git", "https://evil.test/org/repo.git", "ssh://git@evil.test/org/repo.git", "ext::unsafe-command", "https://secret@github.com/org/repo.git"} {
		assertTransportRejectedBeforeCredentials(t, repo, "fetch", remote, "main")
	}
	session.Invalidate("")
	calls := 0
	_, err := RunGit(ctx, shell.Options{Command: testGitPath(t), CWD: repo, Args: []string{"fetch", "origin", "main"}}, func(ctx context.Context, options shell.Options) (shell.Result, error) {
		calls++
		if options.Args[0] != "config" {
			t.Fatal("credential failure fell back to an ambient Git transport")
		}
		return shell.Run(ctx, options)
	})
	if err == nil || calls != 1 {
		t.Fatalf("expired unavailable credential did not fail closed: %v, calls=%d", err, calls)
	}
	if _, err := RunGH(ctx, shell.Options{Args: []string{"api", "repos/org/repo"}}, func(context.Context, shell.Options) (shell.Result, error) {
		t.Fatal("credential failure fell back to an ambient GitHub CLI session")
		return shell.Result{}, nil
	}); err == nil {
		t.Fatal("unavailable selected App did not fail closed")
	}
}

func TestRunGitHTTPSDoesNotFollowRedirectsOutsideRepository(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		if strings.HasPrefix(request.URL.Path, "/outside") {
			t.Error("credential-bearing Git followed a redirect outside its selected repository")
		}
		http.Redirect(response, request, "/outside", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(server.Close)
	certificate := filepath.Join(t.TempDir(), "git-test-ca.pem")
	if err := os.WriteFile(certificate, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	repo := initHumanRepository(t)
	plainGit(t, repo, "remote", "add", "origin", server.URL+"/org/repo.git")
	ctx, _ := subprocessBinding(t, server.URL, config.HostingIdentityGitHubApp)
	_, err := RunGit(ctx, shell.Options{Command: testGitPath(t), CWD: repo, Args: []string{"ls-remote", "--heads", "origin", "main"}}, func(ctx context.Context, options shell.Options) (shell.Result, error) {
		options.Env["GIT_SSL_CAINFO"] = certificate
		return shell.Run(ctx, options)
	})
	if err == nil || requests.Load() != 1 {
		t.Fatalf("redirect was not rejected at the original request: requests=%d, error=%v", requests.Load(), err)
	}
}
