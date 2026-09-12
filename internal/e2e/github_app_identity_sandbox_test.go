package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/e2e/harness"
	"github.com/nexu-io/looper/internal/hostingidentity"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/infra/shell"
)

const (
	envSandboxAppEnabled        = "LOOPER_E2E_GITHUB_APP"
	envSandboxAppID             = "LOOPER_E2E_GITHUB_APP_ID"
	envSandboxInstallationID    = "LOOPER_E2E_GITHUB_INSTALLATION_ID"
	envSandboxAppPrivateKeyFile = "LOOPER_E2E_GITHUB_APP_PRIVATE_KEY_FILE"
	appSandboxIdentity          = "sandbox-app"
	appSandboxUnselectedToken   = "LOOPER_E2E_OTHER_BOT_TOKEN"
)

// This test deliberately does not use LOOPER_E2E_GITHUB_TOKEN, an authenticated
// origin URL, or the legacy sandbox agent's token environment. The daemon must
// exchange its own App JWT and provide the agent only with host capabilities.
func TestGitHubSandboxAppIdentity(t *testing.T) {
	if os.Getenv(envSandboxAppEnabled) != "1" {
		t.Skipf("set %s=1 with App credential references to run", envSandboxAppEnabled)
	}
	definition, err := parseSandboxAppIdentity(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	repoSlug := resolveGitHubSandboxRepoEnv(t, os.Getenv)
	if owner, name, ok := strings.Cut(repoSlug, "/"); !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		t.Fatalf("%s requires an owner/repo sandbox target", envSandboxAppEnabled)
	}
	manager := hostingidentity.NewManager(hostingidentity.Options{})
	ctx, cancel := context.WithTimeout(hostingidentity.WithManager(context.Background(), manager), 5*time.Minute)
	defer cancel()
	bindingConfig := config.Config{
		Identities: map[string]config.HostingIdentityConfig{appSandboxIdentity: definition},
		Providers:  []config.ProviderConfig{{ID: "github", Kind: config.ProviderKindGitHub, BaseURL: "https://github.com"}},
		Projects:   []config.ProjectRefConfig{{ID: "project_1", Provider: "github", Repo: repoSlug, Identity: appSandboxIdentity}},
	}
	ctx, err = hostingidentity.Bind(ctx, bindingConfig, "project_1", "worker")
	if err != nil {
		t.Fatal(err)
	}
	session, _ := hostingidentity.FromContext(ctx)
	actor, err := session.Credentials(ctx)
	if err != nil {
		t.Fatalf("App authentication: %v", err)
	}
	if actor.NumericID <= 0 || !strings.HasSuffix(actor.Login, "[bot]") {
		t.Fatalf("App resolved an invalid bot principal: %v", actor)
	}
	var installation struct {
		TotalCount   int `json:"total_count"`
		Repositories []struct {
			FullName string `json:"full_name"`
		} `json:"repositories"`
	}
	appSandboxAPI(t, ctx, session, http.MethodGet, "/installation/repositories", nil, &installation)
	if installation.TotalCount != 1 || len(installation.Repositories) != 1 || !strings.EqualFold(installation.Repositories[0].FullName, repoSlug) {
		t.Fatal("daemon-minted installation token must be scoped to exactly the configured sandbox repository")
	}

	bins := harness.MustBinaries(t)
	home := harness.NewTempHome(t)
	var repository struct {
		DefaultBranch string `json:"default_branch"`
	}
	appSandboxAPI(t, ctx, session, http.MethodGet, "/repos/"+repoSlug, nil, &repository)
	if repository.DefaultBranch == "" {
		t.Fatal("App sandbox repository must have an initialized default branch")
	}
	repo := harness.SeededRepo{Path: filepath.Join(t.TempDir(), "repo"), DefaultBranch: repository.DefaultBranch}
	if err := os.MkdirAll(repo.Path, 0o755); err != nil {
		t.Fatal(err)
	}
	gitEnv := map[string]string{"PATH": os.Getenv("PATH"), "HOME": home.HomeDir, "GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": os.DevNull, "GIT_TERMINAL_PROMPT": "0"}
	localGit := func(args ...string) string {
		t.Helper()
		result, err := shell.Run(ctx, shell.Options{Command: "git", Args: args, CWD: repo.Path, Env: gitEnv, Timeout: 30 * time.Second})
		if err != nil {
			t.Fatalf("sandbox local Git failed: %s", session.Redact(err.Error()))
		}
		return strings.TrimSpace(result.Stdout)
	}
	localGit("init", "-b", repo.DefaultBranch)
	sshOrigin := "git@github.com:" + repoSlug + ".git"
	localGit("remote", "add", "origin", sshOrigin)
	if _, err := hostingidentity.RunGit(ctx, shell.Options{Command: "git", CWD: repo.Path, Args: []string{"fetch", "origin", "+refs/heads/" + repo.DefaultBranch + ":refs/remotes/origin/" + repo.DefaultBranch}, Timeout: time.Minute}, nil); err != nil {
		t.Fatalf("bot HTTPS fetch from SSH-configured repository: %s", session.Redact(err.Error()))
	}
	localGit("checkout", "-B", repo.DefaultBranch, "refs/remotes/origin/"+repo.DefaultBranch)
	repo.InitialCommit = localGit("rev-parse", "HEAD")

	title := "looper-e2e:" + strconv.FormatInt(time.Now().UnixNano(), 36) + " App identity"
	var issue struct {
		Number int64 `json:"number"`
	}
	appSandboxAPI(t, ctx, session, http.MethodPost, "/repos/"+repoSlug+"/issues", map[string]any{"title": title, "body": "Exercise the configured App bot: add a small file, commit locally, and let the daemon publish the PR."}, &issue)
	var prNumber int64
	var headBranch string
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
		defer cleanupCancel()
		cleanup := func(method, path string, payload any) {
			if err := appSandboxRequest(cleanupCtx, session, method, path, payload, nil); err != nil {
				t.Errorf("sandbox cleanup: %v", err)
			}
		}
		if prNumber > 0 {
			cleanup(http.MethodPatch, fmt.Sprintf("/repos/%s/pulls/%d", repoSlug, prNumber), map[string]any{"state": "closed"})
		}
		if headBranch != "" {
			cleanup(http.MethodDelete, "/repos/"+repoSlug+"/git/refs/heads/"+headBranch, nil)
		}
		cleanup(http.MethodPatch, fmt.Sprintf("/repos/%s/issues/%d", repoSlug, issue.Number), map[string]any{"state": "closed"})
	})

	fakeAgent := harness.NewFakeAgent(t, bins)
	cfg := workerSandboxConfig(t, bins, home, repo, fakeAgent, harness.MustFreePort(t), "bot-commit")
	cfg.Providers = bindingConfig.Providers
	cfg.Identities = bindingConfig.Identities
	cfg.Projects[0].Provider, cfg.Projects[0].Repo, cfg.Projects[0].Identity = "github", repoSlug, appSandboxIdentity
	// Poison explicit agent overrides too: sanitation must happen after every
	// source has merged, including unused named bot credential references.
	cfg.Identities["unused-forgejo"] = config.HostingIdentityConfig{Kind: config.HostingIdentityForgejoToken, BaseURL: "https://unused.example", TokenEnv: appSandboxUnselectedToken}
	cfg.Agent.Env["GH_TOKEN"] = "sandbox-agent-token-must-be-removed"
	cfg.Agent.Env[appSandboxUnselectedToken] = "sandbox-unselected-token-must-be-removed"
	harness.WriteConfig(t, home.ConfigPath, cfg, nil)
	proc := harness.StartLooperd(t, bins, home, home.ConfigPath, map[string]string{
		"GH_TOKEN": "sandbox-personal-fallback-must-not-work", "GITHUB_TOKEN": "", "GH_ENTERPRISE_TOKEN": "", "GITHUB_ENTERPRISE_TOKEN": "",
		appSandboxUnselectedToken: "sandbox-daemon-unselected-token",
	}, cfg.Server.Host, cfg.Server.Port)
	readyCtx, readyCancel := context.WithTimeout(ctx, 45*time.Second)
	defer readyCancel()
	if _, err := proc.WaitForReady(readyCtx); err != nil {
		t.Fatalf("App daemon readiness: %v", err)
	}
	client := newAPIClient(proc.BaseURL())
	var created struct {
		ID string `json:"id"`
	}
	client.post(t, "/api/v1/workers", map[string]any{"projectId": "project_1", "repo": repoSlug, "issueNumber": issue.Number, "baseBranch": repo.DefaultBranch}, &created)
	run := waitForRunTerminal(t, client, created.ID, 120*time.Second)
	if run.Status != "success" {
		t.Fatalf("App worker status = %s: %s", run.Status, session.Redact(stringValue(run.ErrorMessage)))
	}
	var capabilityEvidence struct {
		Login, Repo          string
		CredentialKeysAbsent bool
		RepositoryRead       bool
	}
	evidenceJSON, err := os.ReadFile(filepath.Join(fakeAgent.ArtifactDir, "bot-capabilities.json"))
	if err != nil || json.Unmarshal(evidenceJSON, &capabilityEvidence) != nil || capabilityEvidence.Login != actor.Login || capabilityEvidence.Repo != repoSlug || !capabilityEvidence.CredentialKeysAbsent || !capabilityEvidence.RepositoryRead {
		t.Fatal("agent did not verify the selected bot's credential-free repository capabilities")
	}
	checkpoint := parseJSONObject(t, run.CheckpointJSON)
	pr, ok := checkpoint["pullRequest"].(map[string]any)
	if !ok {
		t.Fatal("App worker did not persist its daemon-published PR")
	}
	if number, ok := pr["number"].(float64); ok {
		prNumber = int64(number)
	}
	if prNumber <= 0 {
		t.Fatal("App worker PR checkpoint has no number")
	}
	var published struct {
		User struct{ Login string }    `json:"user"`
		Head struct{ Ref, SHA string } `json:"head"`
	}
	appSandboxAPI(t, ctx, session, http.MethodGet, fmt.Sprintf("/repos/%s/pulls/%d", repoSlug, prNumber), nil, &published)
	headBranch = published.Head.Ref
	if published.User.Login != actor.Login {
		t.Fatalf("PR author = %q, want App %q", published.User.Login, actor.Login)
	}
	if got := localGit("remote", "get-url", "origin"); got != sshOrigin {
		t.Fatalf("origin changed to %q, want original SSH configuration", got)
	}
	var commit struct {
		Commit struct {
			Author, Committer struct{ Name, Email string }
		} `json:"commit"`
	}
	appSandboxAPI(t, ctx, session, http.MethodGet, "/repos/"+repoSlug+"/commits/"+published.Head.SHA, nil, &commit)
	if commit.Commit.Author.Name != actor.Name || commit.Commit.Author.Email != actor.Email || commit.Commit.Committer.Name != actor.Name || commit.Commit.Committer.Email != actor.Email {
		t.Fatalf("published commit attribution = %+v, want App %s <%s>", commit.Commit, actor.Name, actor.Email)
	}
	proc.Stop(context.Background())

	// Refresh the same captured installation, then exercise actual publication
	// through the shared gateway. COMMENT is permitted on the bot's own PR;
	// this test does not change self-approval/review-request policy.
	session.Invalidate(actor.Token)
	refreshed, err := session.Credentials(ctx)
	if err != nil || refreshed.Login != actor.Login || refreshed.NumericID != actor.NumericID {
		t.Fatalf("same-installation refresh did not preserve bot identity: %v", err)
	}
	gateway := githubinfra.New(githubinfra.Options{GHPath: "gh", GitPath: "git", CWD: repo.Path})
	comment := "App identity sandbox comment " + title
	if err := gateway.AddPullRequestComment(ctx, githubinfra.PullRequestCommentInput{Repo: repoSlug, PRNumber: prNumber, Body: comment, CWD: repo.Path}); err != nil {
		t.Fatal(err)
	}
	review := "App identity sandbox review " + title
	if err := gateway.SubmitReview(ctx, githubinfra.SubmitReviewInput{Repo: repoSlug, PRNumber: prNumber, Event: "COMMENT", Body: review, CommitID: published.Head.SHA, CWD: repo.Path}); err != nil {
		t.Fatal(err)
	}
	for _, publication := range []struct{ path, body string }{{fmt.Sprintf("issues/%d/comments", prNumber), comment}, {fmt.Sprintf("pulls/%d/reviews", prNumber), review}} {
		var entries []struct {
			Body string
			User struct{ Login string }
		}
		appSandboxAPI(t, ctx, session, http.MethodGet, "/repos/"+repoSlug+"/"+publication.path, nil, &entries)
		found := false
		for _, entry := range entries {
			if strings.Contains(entry.Body, publication.body) && entry.User.Login == actor.Login {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s missing publication attributed to %s", publication.path, actor.Login)
		}
	}
}

func parseSandboxAppIdentity(getenv func(string) string) (config.HostingIdentityConfig, error) {
	definition := config.HostingIdentityConfig{Kind: config.HostingIdentityGitHubApp, BaseURL: "https://github.com"}
	for _, field := range []struct {
		name string
		dest *int64
	}{{envSandboxAppID, &definition.AppID}, {envSandboxInstallationID, &definition.InstallationID}} {
		value, err := strconv.ParseInt(strings.TrimSpace(getenv(field.name)), 10, 64)
		if err != nil || value <= 0 {
			return definition, fmt.Errorf("%s requires a positive numeric ID", field.name)
		}
		*field.dest = value
	}
	definition.PrivateKeyFile = strings.TrimSpace(getenv(envSandboxAppPrivateKeyFile))
	if !filepath.IsAbs(definition.PrivateKeyFile) {
		return definition, fmt.Errorf("%s requires an absolute private-key file reference", envSandboxAppPrivateKeyFile)
	}
	return definition, nil
}

func appSandboxAPI(tb testing.TB, ctx context.Context, session *hostingidentity.Session, method, path string, payload, out any) {
	tb.Helper()
	if err := appSandboxRequest(ctx, session, method, path, payload, out); err != nil {
		tb.Fatal(err)
	}
}

func appSandboxRequest(ctx context.Context, session *hostingidentity.Session, method, path string, payload, out any) error {
	credential, err := session.Credentials(ctx)
	if err != nil {
		return fmt.Errorf("sandbox App credential: %w", err)
	}
	var data []byte
	if payload != nil {
		data, err = json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("encode sandbox API request")
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, session.APIURL()+path, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("construct sandbox API request")
	}
	req.Header.Set("Authorization", "Bearer "+credential.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("sandbox API %s %s: transport failed", method, path)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("sandbox API %s %s: HTTP %d", method, path, response.StatusCode)
	}
	if out != nil {
		data, err := io.ReadAll(io.LimitReader(response.Body, (2<<20)+1))
		if err != nil || len(data) > 2<<20 || json.Unmarshal(data, out) != nil {
			return fmt.Errorf("sandbox API %s %s: invalid or oversized JSON response", method, path)
		}
	}
	return nil
}
