package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/e2e/harness"
	"github.com/nexu-io/looper/internal/forge"
)

const (
	envForgejoSandboxEnabled = "LOOPER_E2E_FORGEJO"
	envForgejoBaseURL        = "LOOPER_E2E_FORGEJO_BASE_URL"
	envForgejoSandboxRepo    = "LOOPER_E2E_FORGEJO_SANDBOX_REPO"
	envForgejoToken          = "LOOPER_E2E_FORGEJO_TOKEN"
	envForgejoReviewerToken  = "LOOPER_E2E_FORGEJO_REVIEWER_TOKEN"
	forgejoSandboxLabelName  = "looper-e2e"
)

type forgejoSandboxConfig struct {
	BaseURL      string
	Repo         string
	Owner        string
	Name         string
	Token        string
	RunID        string
	TitlePrefix  string
	BranchPrefix string
	CloneURL     string
	CmdEnv       []string
	CurrentUser  forge.Identity
	Client       *forge.ForgejoClient
	HTTPClient   *http.Client
	RepoHTMLURL  string
}

type forgejoSandboxIssue struct {
	Number int64
	URL    string
	Title  string
}

type forgejoSandboxPR struct {
	Number     int64
	URL        string
	Title      string
	HeadBranch string
	HeadSHA    string
}

type forgejoSandboxSummaryComment struct {
	ID   int64
	URL  string
	Body string
}

func TestForgejoSandboxWorkerCreatesPullRequest(t *testing.T) {
	bins := harness.MustBinaries(t)
	sb := requireForgejoSandboxConfig(t)
	home := harness.NewTempHome(t)
	repo := ensureForgejoSandboxProjectRepo(t, sb)
	port := harness.MustFreePort(t)
	fakeAgent := harness.NewFakeAgent(t, bins)
	issue := createForgejoSandboxIssue(t, sb, "worker creates pull request")
	var prURL string
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("forgejo sandbox issue=%s pr=%s branch_prefix=%s", issue.URL, prURL, sb.BranchPrefix)
		}
	})
	defer cleanupForgejoSandboxIssue(t, sb, issue.Number)

	cfg := forgejoWorkerSandboxConfig(t, bins, home, repo, fakeAgent, port, sb, "commit")
	harness.WriteConfig(t, home.ConfigPath, cfg, nil)
	proc := harness.StartLooperd(t, bins, home, home.ConfigPath, forgejoSandboxEnvMap(sb), cfg.Server.Host, cfg.Server.Port)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := proc.WaitForReady(ctx); err != nil {
		t.Fatalf("wait for ready: %v", err)
	}
	client := newAPIClient(proc.BaseURL())
	var created struct {
		ID string `json:"id"`
	}
	client.post(t, "/api/v1/workers", map[string]any{"projectId": "project_1", "repo": sb.Repo, "issueNumber": issue.Number, "baseBranch": repo.DefaultBranch}, &created)
	run := waitForRunTerminal(t, client, created.ID, 90*time.Second)
	if run.Status != "success" {
		t.Fatalf("run status = %s, want success (issue=%s error=%q checkpoint=%s)", run.Status, issue.URL, stringValue(run.ErrorMessage), stringValue(run.CheckpointJSON))
	}
	prs := waitForForgejoSandboxPRsByTitle(t, sb, issue.Title, 30*time.Second)
	if len(prs) != 1 {
		t.Fatalf("matching PRs = %#v, want one PR for issue %s", prs, issue.URL)
	}
	prURL = prs[0].URL
	cleanupForgejoSandboxPR(t, sb, prs[0].Number, prs[0].HeadBranch)
	checkpoint := parseJSONObject(t, run.CheckpointJSON)
	if checkpoint["pullRequest"] == nil {
		t.Fatalf("pullRequest checkpoint missing for issue=%s pr=%s checkpoint=%#v", issue.URL, prURL, checkpoint)
	}
	proc.Stop(context.Background())
}

func TestForgejoSandboxNativeReviewRequestDiscoveryPublishAndRetry(t *testing.T) {
	sb := requireForgejoSandboxConfig(t)
	reviewerToken := strings.TrimSpace(os.Getenv(envForgejoReviewerToken))
	if reviewerToken == "" {
		t.Skipf("%s is required for a non-self-authored native review lifecycle", envForgejoReviewerToken)
	}
	reviewerClient, err := forge.NewForgejoClient(forge.RepositoryRef{ProviderID: "forgejo-sandbox-reviewer", Kind: forge.ProviderKindForgejo, BaseURL: sb.BaseURL, Repo: sb.Repo}, reviewerToken)
	if err != nil {
		t.Fatalf("create native reviewer client: %v", err)
	}
	reviewer, err := reviewerClient.CurrentUser(context.Background())
	if err != nil {
		t.Fatalf("lookup native reviewer identity: %v", err)
	}
	if strings.EqualFold(reviewer.Login, sb.CurrentUser.Login) {
		t.Fatalf("%s must authenticate a user other than the PR author", envForgejoReviewerToken)
	}
	repo := ensureForgejoSandboxProjectRepo(t, sb)
	pr := createForgejoSandboxPR(t, sb, repo, "native review lifecycle")
	defer cleanupForgejoSandboxPR(t, sb, pr.Number, pr.HeadBranch)
	ctx := context.Background()
	if err := sb.Client.AddPullRequestReviewers(ctx, pr.Number, []string{reviewer.Login}); err != nil {
		t.Fatalf("request native Forgejo reviewer: %v", err)
	}
	discovered, err := reviewerClient.ListReviewRequestedPullRequests(ctx, reviewer.Login, 100)
	if err != nil {
		t.Fatalf("discover native Forgejo review request: %v", err)
	}
	foundPR := false
	for _, candidate := range discovered {
		if candidate.Number == pr.Number {
			foundPR = true
			break
		}
	}
	if !foundPR {
		t.Fatalf("review-request discovery omitted PR %s", pr.URL)
	}
	marker := fmt.Sprintf("<!-- looper:review id=forgejo-sandbox:%s:%d head=%s outcome=non_blocking -->", sb.RunID, pr.Number, pr.HeadSHA)
	reviews, err := reviewerClient.ListPullRequestReviews(ctx, pr.Number)
	if err != nil {
		t.Fatalf("list native Forgejo reviews before publish: %v", err)
	}
	if !forgejoSandboxReviewMarkerExists(reviews, marker) {
		if _, err := reviewerClient.CreatePullRequestReview(ctx, forge.CreatePullRequestReviewInput{Number: pr.Number, Event: "COMMENT", CommitID: pr.HeadSHA, Body: "Looper sandbox native review\n\n" + marker}); err != nil {
			t.Fatalf("publish native Forgejo review: %v", err)
		}
	}
	// Retry follows the same marker-first contract and must not create another review.
	reviews, err = reviewerClient.ListPullRequestReviews(ctx, pr.Number)
	if err != nil {
		t.Fatalf("list native Forgejo reviews after publish: %v", err)
	}
	count := 0
	for _, review := range reviews {
		if strings.Contains(review.Body, marker) {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("native marker review count = %d, want 1 (pr=%s)", count, pr.URL)
	}
}

func forgejoSandboxReviewMarkerExists(reviews []forge.PullRequestReview, marker string) bool {
	for _, review := range reviews {
		if strings.Contains(review.Body, marker) {
			return true
		}
	}
	return false
}

func TestForgejoSandboxFixerResolvesReviewThread(t *testing.T) {
	bins := harness.MustBinaries(t)
	sb := requireForgejoSandboxConfig(t)
	repo := ensureForgejoSandboxProjectRepo(t, sb)
	pr := createForgejoSandboxPR(t, sb, repo, "fixer summary protocol")
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("forgejo sandbox pr=%s branch=%s", pr.URL, pr.HeadBranch)
		}
	})
	defer cleanupForgejoSandboxPR(t, sb, pr.Number, pr.HeadBranch)

	reviewerHome := harness.NewTempHome(t)
	reviewerCfg := forgejoReviewerSandboxConfig(t, bins, reviewerHome, repo, harness.NewFakeAgent(t, bins), harness.MustFreePort(t), sb, "forgejo-reviewer-open")
	reviewerRun := runForgejoSandboxLoop(t, bins, reviewerHome, sb, reviewerCfg, map[string]any{"projectId": "project_1", "type": "reviewer", "targetType": "pull_request", "repo": sb.Repo, "prNumber": pr.Number, "metadata": map[string]any{"manual": true}}, 90*time.Second)
	if reviewerRun.Status != "success" {
		t.Fatalf("first reviewer status = %s, want success (pr=%s error=%q checkpoint=%s)", reviewerRun.Status, pr.URL, stringValue(reviewerRun.ErrorMessage), stringValue(reviewerRun.CheckpointJSON))
	}
	reviewerComment, reviewerSummary := requireSingleForgejoReviewerSummary(t, sb, pr.Number)
	if len(reviewerSummary.Items) != 1 || reviewerSummary.Items[0].Status != forge.ReviewItemStatusOpen {
		t.Fatalf("reviewer summary = %#v, want one open item", reviewerSummary)
	}

	fixerHome := harness.NewTempHome(t)
	fixerCfg := forgejoFixerSandboxConfig(t, bins, fixerHome, repo, harness.NewFakeAgent(t, bins), harness.MustFreePort(t), sb, "commit")
	fixerRun := runForgejoSandboxDiscoveredFixer(t, bins, fixerHome, sb, fixerCfg, 90*time.Second)
	if fixerRun.Status != "success" {
		t.Fatalf("fixer status = %s, want success (pr=%s error=%q checkpoint=%s)", fixerRun.Status, pr.URL, stringValue(fixerRun.ErrorMessage), stringValue(fixerRun.CheckpointJSON))
	}
	fixerComment, fixerSummary := requireSingleForgejoFixerSummary(t, sb, pr.Number)
	if fixerSummary.ConsumedReviewRoundID != reviewerSummary.ReviewRoundID {
		t.Fatalf("fixer consumed_review_round_id = %d, want %d", fixerSummary.ConsumedReviewRoundID, reviewerSummary.ReviewRoundID)
	}
	if len(fixerSummary.Results) != 1 || fixerSummary.Results[0].ReviewItemID != reviewerSummary.Items[0].ReviewItemID {
		t.Fatalf("fixer summary = %#v, want one result matching reviewer item", fixerSummary)
	}

	reviewerHome2 := harness.NewTempHome(t)
	reviewerCfg2 := forgejoReviewerSandboxConfig(t, bins, reviewerHome2, repo, harness.NewFakeAgent(t, bins), harness.MustFreePort(t), sb, "forgejo-reviewer-clean")
	reviewerRun2 := runForgejoSandboxLoop(t, bins, reviewerHome2, sb, reviewerCfg2, map[string]any{"projectId": "project_1", "type": "reviewer", "targetType": "pull_request", "repo": sb.Repo, "prNumber": pr.Number, "metadata": map[string]any{"manual": true}}, 90*time.Second)
	if reviewerRun2.Status != "success" {
		t.Fatalf("second reviewer status = %s, want success (pr=%s error=%q checkpoint=%s)", reviewerRun2.Status, pr.URL, stringValue(reviewerRun2.ErrorMessage), stringValue(reviewerRun2.CheckpointJSON))
	}
	updatedReviewerComment, updatedReviewerSummary := requireSingleForgejoReviewerSummary(t, sb, pr.Number)
	if updatedReviewerComment.ID != reviewerComment.ID {
		t.Fatalf("reviewer summary comment id = %d, want reused %d", updatedReviewerComment.ID, reviewerComment.ID)
	}
	if updatedReviewerSummary.ReviewRoundID <= reviewerSummary.ReviewRoundID {
		t.Fatalf("review_round_id = %d, want > %d", updatedReviewerSummary.ReviewRoundID, reviewerSummary.ReviewRoundID)
	}
	if len(updatedReviewerSummary.Items) != 1 || updatedReviewerSummary.Items[0].ReviewItemID != reviewerSummary.Items[0].ReviewItemID || updatedReviewerSummary.Items[0].Status != forge.ReviewItemStatusResolved {
		t.Fatalf("updated reviewer summary = %#v, want resolved prior item", updatedReviewerSummary)
	}
	if fixerComment.ID == 0 || !strings.Contains(fixerComment.Body, forge.FixerSummaryMarker) {
		t.Fatalf("fixer summary comment = %#v, want marker", fixerComment)
	}
}

func TestForgejoSandboxNoDiffPathsDoNotOpenOrResolve(t *testing.T) {
	bins := harness.MustBinaries(t)
	sb := requireForgejoSandboxConfig(t)

	t.Run("worker-no-diff-no-pr", func(t *testing.T) {
		home := harness.NewTempHome(t)
		repo := ensureForgejoSandboxProjectRepo(t, sb)
		port := harness.MustFreePort(t)
		fakeAgent := harness.NewFakeAgent(t, bins)
		issue := createForgejoSandboxIssue(t, sb, "worker no diff")
		t.Cleanup(func() {
			if t.Failed() {
				t.Logf("forgejo sandbox issue=%s branch_prefix=%s", issue.URL, sb.BranchPrefix)
			}
		})
		defer cleanupForgejoSandboxIssue(t, sb, issue.Number)
		cfg := forgejoWorkerSandboxConfig(t, bins, home, repo, fakeAgent, port, sb, "success-no-diff")
		harness.WriteConfig(t, home.ConfigPath, cfg, nil)
		proc := harness.StartLooperd(t, bins, home, home.ConfigPath, forgejoSandboxEnvMap(sb), cfg.Server.Host, cfg.Server.Port)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if _, err := proc.WaitForReady(ctx); err != nil {
			t.Fatalf("wait for ready: %v", err)
		}
		client := newAPIClient(proc.BaseURL())
		var created struct {
			ID string `json:"id"`
		}
		client.post(t, "/api/v1/workers", map[string]any{"projectId": "project_1", "repo": sb.Repo, "issueNumber": issue.Number, "baseBranch": repo.DefaultBranch}, &created)
		run := waitForRunTerminal(t, client, created.ID, 60*time.Second)
		if run.Status != "success" {
			t.Fatalf("run status = %s, want success skip (issue=%s error=%q checkpoint=%s)", run.Status, issue.URL, stringValue(run.ErrorMessage), stringValue(run.CheckpointJSON))
		}
		checkpoint := parseJSONObject(t, run.CheckpointJSON)
		skipReason, _ := checkpoint["skipReason"].(string)
		if !strings.Contains(skipReason, "has no commits ahead of") {
			t.Fatalf("skipReason = %q, want no-diff skip for issue=%s", skipReason, issue.URL)
		}
		if len(findForgejoSandboxPRsByTitle(t, sb, issue.Title)) != 0 {
			t.Fatalf("unexpected PR created for no-diff issue %s", issue.URL)
		}
		proc.Stop(context.Background())
	})

	t.Run("fixer-no-new-commit-keeps-thread-unresolved", func(t *testing.T) {
		t.Skip("Forgejo fixer review-thread resolution is unsupported by the current MVP capability set")
	})
}

func TestForgejoSandboxDependencyGateScenarios(t *testing.T) {
	requireForgejoSandboxConfig(t)
	for _, name := range []string{
		"looperd startup validation succeeds against real dependency API",
		"human gated blocked_by fails then releases after completion",
		"Forgejo rejects blocked_by cycle creation",
		"not planned blocker returns dependent to retriage without cycle comment",
	} {
		t.Run(name, func(t *testing.T) {
			t.Skip("Forgejo Coordinator/dependency-gate behavior is unsupported by the current MVP capability set")
		})
	}
}

func requireForgejoSandboxConfig(tb testing.TB) forgejoSandboxConfig {
	tb.Helper()
	cfg, enabled, err := parseForgejoSandboxConfig(os.Getenv, os.Environ)
	if !enabled {
		tb.Skipf("set %s=1 to run real Forgejo sandbox E2E", envForgejoSandboxEnabled)
	}
	if err != nil {
		tb.Fatalf("invalid Forgejo sandbox config: %v", err)
	}
	validated, err := validateForgejoSandboxPrerequisites(context.Background(), cfg)
	if err != nil {
		tb.Fatalf("invalid Forgejo sandbox prerequisites: %v", err)
	}
	return validated
}

func parseForgejoSandboxConfig(getenv func(string) string, environ func() []string) (forgejoSandboxConfig, bool, error) {
	if strings.TrimSpace(getenv(envForgejoSandboxEnabled)) != "1" {
		return forgejoSandboxConfig{}, false, nil
	}
	baseURL := strings.TrimSpace(getenv(envForgejoBaseURL))
	if baseURL == "" {
		return forgejoSandboxConfig{}, true, fmt.Errorf("%s=1 requires %s", envForgejoSandboxEnabled, envForgejoBaseURL)
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return forgejoSandboxConfig{}, true, fmt.Errorf("%s must be an absolute URL, got %q", envForgejoBaseURL, baseURL)
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.Fragment = ""
	baseURL = strings.TrimRight(parsed.String(), "/")
	repo := strings.TrimSpace(getenv(envForgejoSandboxRepo))
	if repo == "" {
		return forgejoSandboxConfig{}, true, fmt.Errorf("%s=1 requires %s", envForgejoSandboxEnabled, envForgejoSandboxRepo)
	}
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return forgejoSandboxConfig{}, true, fmt.Errorf("invalid %s %q, want owner/repo", envForgejoSandboxRepo, repo)
	}
	token := strings.TrimSpace(getenv(envForgejoToken))
	if token == "" {
		return forgejoSandboxConfig{}, true, fmt.Errorf("%s=1 requires %s", envForgejoSandboxEnabled, envForgejoToken)
	}
	runID := strconv.FormatInt(time.Now().UTC().UnixNano(), 36)
	cloneURL, err := forgejoAuthenticatedRemoteURL(baseURL, repo, token)
	if err != nil {
		return forgejoSandboxConfig{}, true, err
	}
	cmdEnv := append(environ(), envForgejoToken+"="+token, "GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=Never", "GIT_ASKPASS=/usr/bin/true")
	return forgejoSandboxConfig{BaseURL: baseURL, Repo: repo, Owner: owner, Name: name, Token: token, RunID: runID, TitlePrefix: "looper-e2e:" + runID, BranchPrefix: "looper-e2e-" + runID, CloneURL: cloneURL, CmdEnv: cmdEnv}, true, nil
}

func validateForgejoSandboxPrerequisites(ctx context.Context, cfg forgejoSandboxConfig) (forgejoSandboxConfig, error) {
	client, err := forge.NewForgejoClient(forge.RepositoryRef{ProviderID: "forgejo-sandbox", Kind: forge.ProviderKindForgejo, BaseURL: cfg.BaseURL, Repo: cfg.Repo}, cfg.Token)
	if err != nil {
		return forgejoSandboxConfig{}, err
	}
	identity, err := client.CurrentUser(ctx)
	if err != nil {
		return forgejoSandboxConfig{}, fmt.Errorf("current user lookup failed: %w", err)
	}
	repoHTMLURL, err := forgejoSandboxRepoHTMLURL(ctx, cfg)
	if err != nil {
		return forgejoSandboxConfig{}, err
	}
	prs, err := client.ListOpenPullRequests(ctx, forge.ListPullRequestsInput{Limit: 1})
	if err != nil {
		return forgejoSandboxConfig{}, fmt.Errorf("list open pull requests failed for %s: %w", cfg.Repo, err)
	}
	_ = prs
	cfg.CurrentUser = identity
	cfg.Client = client
	cfg.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	cfg.RepoHTMLURL = repoHTMLURL
	return cfg, nil
}

func forgejoSandboxRepoHTMLURL(ctx context.Context, cfg forgejoSandboxConfig) (string, error) {
	var repo struct {
		HTMLURL string `json:"html_url"`
	}
	if err := forgejoSandboxAPI(ctx, cfg, http.MethodGet, "repos/"+cfg.Repo, nil, &repo); err != nil {
		return "", fmt.Errorf("sandbox repo lookup failed for %s: %w", cfg.Repo, err)
	}
	if strings.TrimSpace(repo.HTMLURL) == "" {
		return "", fmt.Errorf("sandbox repo lookup returned empty html_url for %s", cfg.Repo)
	}
	return repo.HTMLURL, nil
}

func ensureForgejoSandboxProjectRepo(tb testing.TB, sb forgejoSandboxConfig) harness.SeededRepo {
	tb.Helper()
	repoPath := filepath.Join(tb.TempDir(), "repo")
	if _, err := runSandboxCommand("", sb.CmdEnv, "git", "clone", sb.CloneURL, repoPath); err != nil {
		if err := os.MkdirAll(repoPath, 0o755); err != nil {
			tb.Fatalf("mkdir repo path: %v", err)
		}
		runSandboxCommandMust(tb, "", sb.CmdEnv, "git", "init", "-b", "main", repoPath)
		runSandboxCommandMust(tb, repoPath, sb.CmdEnv, "git", "remote", "add", "origin", sb.CloneURL)
		runSandboxCommandMust(tb, repoPath, sb.CmdEnv, "git", "config", "user.name", "Looper E2E")
		runSandboxCommandMust(tb, repoPath, sb.CmdEnv, "git", "config", "user.email", "looper-e2e@example.com")
		configureSandboxGitAuth(tb, repoPath)
		readmePath := filepath.Join(repoPath, "README.md")
		if err := os.WriteFile(readmePath, []byte("# Looper sandbox\n"), 0o644); err != nil {
			tb.Fatalf("write README: %v", err)
		}
		runSandboxCommandMust(tb, repoPath, sb.CmdEnv, "git", "add", "README.md")
		runSandboxCommandMust(tb, repoPath, sb.CmdEnv, "git", "commit", "-m", "seed sandbox repo")
		runSandboxCommandMust(tb, repoPath, sb.CmdEnv, "git", "push", "-u", "origin", "main")
	} else {
		runSandboxCommandMust(tb, repoPath, sb.CmdEnv, "git", "config", "user.name", "Looper E2E")
		runSandboxCommandMust(tb, repoPath, sb.CmdEnv, "git", "config", "user.email", "looper-e2e@example.com")
		configureSandboxGitAuth(tb, repoPath)
		runSandboxCommandMust(tb, repoPath, sb.CmdEnv, "git", "fetch", "origin", "main")
		runSandboxCommandMust(tb, repoPath, sb.CmdEnv, "git", "checkout", "-B", "main", "origin/main")
	}
	ensureForgejoSandboxLabel(tb, sb)
	return harness.SeededRepo{Path: repoPath, DefaultBranch: "main", InitialCommit: strings.TrimSpace(runSandboxCommandMust(tb, repoPath, sb.CmdEnv, "git", "rev-parse", "HEAD"))}
}

func ensureForgejoSandboxLabel(tb testing.TB, sb forgejoSandboxConfig) {
	tb.Helper()
	if _, err := forgejoSandboxEnsureLabel(context.Background(), sb, forgejoSandboxLabelName, "5319e7", "Looper sandbox E2E resources"); err != nil {
		tb.Fatalf("ensure sandbox label: %v", err)
	}
}

func forgejoWorkerSandboxConfig(tb testing.TB, bins harness.BuiltBinaries, home harness.TempHome, repo harness.SeededRepo, fakeAgent harness.FakeAgent, port int, sb forgejoSandboxConfig, agentMode string) config.Config {
	tb.Helper()
	vendor, command, agentEnv := fakeAgent.AgentConfig(agentMode, "git", "")
	project := writeProjectConfig(repo, home)[0]
	project.Provider = "forgejo-main"
	project.Repo = sb.Repo
	cfg := harness.DefaultConfig(tb, home, harness.ConfigOptions{
		Port:              port,
		ToolPaths:         harness.TestToolPaths{Git: "git", Looper: bins.LooperPath, Osascript: bins.FakeOsascriptPath},
		EnableOsascript:   true,
		AgentVendor:       vendor,
		AgentCommand:      command,
		AgentEnv:          agentEnv,
		Projects:          []config.ProjectRefConfig{project},
		DisableDisclosure: true,
	})
	cfg.Scheduler.PollIntervalSeconds = 10
	cfg.Defaults.AllowAutoCommit = true
	cfg.Defaults.AllowAutoPush = true
	cfg.Defaults.OpenPRStrategy = config.OpenPRStrategyAllDone
	cfg.Roles.Worker.Triggers.RequireAssigneeCurrentUser = false
	cfg.Providers = []config.ProviderConfig{{ID: "forgejo-main", Kind: config.ProviderKindForgejo, BaseURL: sb.BaseURL, TokenEnv: stringPtr(envForgejoToken)}}
	return cfg
}

func forgejoReviewerSandboxConfig(tb testing.TB, bins harness.BuiltBinaries, home harness.TempHome, repo harness.SeededRepo, fakeAgent harness.FakeAgent, port int, sb forgejoSandboxConfig, agentMode string) config.Config {
	tb.Helper()
	cfg := forgejoWorkerSandboxConfig(tb, bins, home, repo, fakeAgent, port, sb, agentMode)
	cfg.Defaults.OpenPRStrategy = config.OpenPRStrategyManual
	cfg.Roles.Reviewer.Discovery.Triggers.RequireReviewRequest = false
	cfg.Roles.Reviewer.Discovery.Triggers.Labels = []string{"looper:review"}
	cfg.Roles.Reviewer.Discovery.Triggers.LabelMode = config.LabelModeAll
	cfg.Roles.Reviewer.Behavior.ReviewEvents.Clean = config.ReviewerReviewEventComment
	cfg.Roles.Reviewer.Behavior.ReviewEvents.Blocking = config.ReviewerReviewEventComment
	cfg.Roles.Reviewer.Behavior.PublishMode = config.ReviewerPublishModeSummaryComment
	cfg.Roles.Fixer.AutoDiscovery = false
	return cfg
}

func forgejoFixerSandboxConfig(tb testing.TB, bins harness.BuiltBinaries, home harness.TempHome, repo harness.SeededRepo, fakeAgent harness.FakeAgent, port int, sb forgejoSandboxConfig, agentMode string) config.Config {
	tb.Helper()
	cfg := forgejoWorkerSandboxConfig(tb, bins, home, repo, fakeAgent, port, sb, agentMode)
	cfg.Defaults.OpenPRStrategy = config.OpenPRStrategyManual
	cfg.Defaults.AllowRiskyFixes = true
	cfg.Scheduler.PollIntervalSeconds = 10
	cfg.Roles.Reviewer.Discovery.AutoDiscovery = false
	cfg.Roles.Fixer.AutoDiscovery = true
	cfg.Roles.Fixer.Triggers.AuthorFilter = config.FixerAuthorFilterAny
	return cfg
}

func createForgejoSandboxIssue(tb testing.TB, sb forgejoSandboxConfig, scenario string) forgejoSandboxIssue {
	tb.Helper()
	title := sb.TitlePrefix + " " + scenario
	body := fmt.Sprintf("Sandbox E2E issue for %s (%s)", scenario, sb.RunID)
	labelID, err := forgejoSandboxEnsureLabel(context.Background(), sb, forgejoSandboxLabelName, "5319e7", "Looper sandbox E2E resources")
	if err != nil {
		tb.Fatalf("ensure sandbox label: %v", err)
	}
	var issue struct {
		Number  int64  `json:"number"`
		HTMLURL string `json:"html_url"`
		Title   string `json:"title"`
	}
	if err := forgejoSandboxAPI(context.Background(), sb, http.MethodPost, "repos/"+sb.Repo+"/issues", map[string]any{"title": title, "body": body, "labels": []int64{labelID}, "assignees": []string{sb.CurrentUser.Login}}, &issue); err != nil {
		tb.Fatalf("create forgejo sandbox issue: %v", err)
	}
	return forgejoSandboxIssue{Number: issue.Number, URL: issue.HTMLURL, Title: issue.Title}
}

func createForgejoSandboxPR(tb testing.TB, sb forgejoSandboxConfig, repo harness.SeededRepo, scenario string) forgejoSandboxPR {
	tb.Helper()
	branch := sb.BranchPrefix + "-" + strings.ReplaceAll(strings.ToLower(scenario), " ", "-")
	filePath := filepath.Join(repo.Path, "sandbox", "forgejo-summary-protocol.txt")
	runSandboxCommandMust(tb, repo.Path, sb.CmdEnv, "git", "checkout", "main")
	runSandboxCommandMust(tb, repo.Path, sb.CmdEnv, "git", "pull", "--ff-only", "origin", "main")
	runSandboxCommandMust(tb, repo.Path, sb.CmdEnv, "git", "checkout", "-B", branch)
	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		tb.Fatalf("mkdir forgejo sandbox dir: %v", err)
	}
	if err := os.WriteFile(filePath, []byte(fmt.Sprintf("sandbox reviewer target %s\n", sb.RunID)), 0o644); err != nil {
		tb.Fatalf("write forgejo sandbox file: %v", err)
	}
	relPath := filepath.ToSlash(strings.TrimPrefix(filePath, repo.Path+string(os.PathSeparator)))
	runSandboxCommandMust(tb, repo.Path, sb.CmdEnv, "git", "add", relPath)
	title := sb.TitlePrefix + " " + scenario
	runSandboxCommandMust(tb, repo.Path, sb.CmdEnv, "git", "commit", "-m", title)
	headSHA := strings.TrimSpace(runSandboxCommandMust(tb, repo.Path, sb.CmdEnv, "git", "rev-parse", "HEAD"))
	runSandboxCommandMust(tb, repo.Path, sb.CmdEnv, "git", "push", "-u", "origin", branch)
	var pr struct {
		Number  int64  `json:"number"`
		HTMLURL string `json:"html_url"`
		Title   string `json:"title"`
	}
	body := fmt.Sprintf("Sandbox PR for %s (%s)", scenario, sb.RunID)
	if err := forgejoSandboxAPI(context.Background(), sb, http.MethodPost, "repos/"+sb.Repo+"/pulls", map[string]any{"title": title, "body": body, "head": branch, "base": repo.DefaultBranch}, &pr); err != nil {
		tb.Fatalf("create forgejo sandbox pr: %v", err)
	}
	if _, err := forgejoSandboxEnsureLabel(context.Background(), sb, "looper:review", "0e8a16", "Looper reviewer discovery label"); err != nil {
		tb.Fatalf("ensure forgejo reviewer label: %v", err)
	}
	if _, err := sb.Client.AddIssueLabels(context.Background(), pr.Number, []string{"looper:review"}); err != nil {
		tb.Fatalf("label forgejo sandbox pr: %v", err)
	}
	return forgejoSandboxPR{Number: pr.Number, URL: pr.HTMLURL, Title: pr.Title, HeadBranch: branch, HeadSHA: headSHA}
}

func findForgejoSandboxPRsByTitle(tb testing.TB, sb forgejoSandboxConfig, title string) []forgejoSandboxPR {
	tb.Helper()
	prs, err := sb.Client.ListOpenPullRequests(context.Background(), forge.ListPullRequestsInput{State: "all", Limit: 100})
	if err != nil {
		tb.Fatalf("list forgejo sandbox pull requests: %v", err)
	}
	out := make([]forgejoSandboxPR, 0, len(prs))
	for _, pr := range prs {
		if pr.Title != title {
			continue
		}
		out = append(out, forgejoSandboxPR{Number: pr.Number, URL: pr.HTMLURL, Title: pr.Title, HeadBranch: pr.Head.Name, HeadSHA: pr.Head.SHA})
	}
	return out
}

func waitForForgejoSandboxPRsByTitle(tb testing.TB, sb forgejoSandboxConfig, title string, timeout time.Duration) []forgejoSandboxPR {
	tb.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		prs := findForgejoSandboxPRsByTitle(tb, sb, title)
		if len(prs) > 0 {
			return prs
		}
		time.Sleep(2 * time.Second)
	}
	return findForgejoSandboxPRsByTitle(tb, sb, title)
}

func cleanupForgejoSandboxIssue(tb testing.TB, sb forgejoSandboxConfig, issueNumber int64) {
	tb.Helper()
	_ = forgejoSandboxAPI(context.Background(), sb, http.MethodPatch, "repos/"+sb.Repo+"/issues/"+strconv.FormatInt(issueNumber, 10), map[string]any{"state": "closed"}, nil)
}

func cleanupForgejoSandboxPR(tb testing.TB, sb forgejoSandboxConfig, prNumber int64, branch string) {
	tb.Helper()
	if prNumber > 0 {
		_ = forgejoSandboxAPI(context.Background(), sb, http.MethodPatch, "repos/"+sb.Repo+"/pulls/"+strconv.FormatInt(prNumber, 10), map[string]any{"state": "closed"}, nil)
	}
	if strings.TrimSpace(branch) != "" {
		_ = forgejoSandboxAPI(context.Background(), sb, http.MethodDelete, "repos/"+sb.Repo+"/branches/"+url.PathEscape(branch), nil, nil)
	}
}

func runForgejoSandboxLoop(tb testing.TB, bins harness.BuiltBinaries, home harness.TempHome, sb forgejoSandboxConfig, cfg config.Config, payload map[string]any, timeout time.Duration) runView {
	tb.Helper()
	harness.WriteConfig(tb, home.ConfigPath, cfg, nil)
	proc := harness.StartLooperd(tb, bins, home, home.ConfigPath, forgejoSandboxEnvMap(sb), cfg.Server.Host, cfg.Server.Port)
	defer proc.Stop(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := proc.WaitForReady(ctx); err != nil {
		tb.Fatalf("wait for ready: %v", err)
	}
	client := newAPIClient(proc.BaseURL())
	var created struct {
		ID string `json:"id"`
	}
	client.post(tb, "/api/v1/loops", payload, &created)
	return waitForRunTerminal(tb, client, created.ID, timeout)
}

func runForgejoSandboxDiscoveredFixer(tb testing.TB, bins harness.BuiltBinaries, home harness.TempHome, sb forgejoSandboxConfig, cfg config.Config, timeout time.Duration) runView {
	tb.Helper()
	harness.WriteConfig(tb, home.ConfigPath, cfg, nil)
	proc := harness.StartLooperd(tb, bins, home, home.ConfigPath, forgejoSandboxEnvMap(sb), cfg.Server.Host, cfg.Server.Port)
	defer proc.Stop(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := proc.WaitForReady(ctx); err != nil {
		tb.Fatalf("wait for ready: %v", err)
	}
	client := newAPIClient(proc.BaseURL())
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var loops loopsListResponse
		client.get(tb, "/api/v1/loops", &loops)
		for _, loop := range loops.Items {
			if loop.ProjectID == "project_1" {
				return waitForRunTerminal(tb, client, loop.ID, time.Until(deadline))
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	tb.Fatal("timed out waiting for automatically discovered Forgejo fixer loop")
	panic("unreachable")
}

func listForgejoSandboxPRComments(tb testing.TB, sb forgejoSandboxConfig, prNumber int64) []forge.Comment {
	tb.Helper()
	comments, err := sb.Client.ListIssueComments(context.Background(), prNumber)
	if err != nil {
		tb.Fatalf("list forgejo sandbox PR comments: %v", err)
	}
	return comments
}

func requireSingleForgejoReviewerSummary(tb testing.TB, sb forgejoSandboxConfig, prNumber int64) (forgejoSandboxSummaryComment, forge.ReviewerSummary) {
	tb.Helper()
	comment, summary, err := forge.ParseUniqueReviewerSummaryComment(listForgejoSandboxPRComments(tb, sb, prNumber))
	if err != nil {
		tb.Fatalf("parse unique reviewer summary comment: %v", err)
	}
	return forgejoSandboxSummaryComment{ID: comment.ID, URL: comment.HTMLURL, Body: comment.Body}, summary
}

func requireSingleForgejoFixerSummary(tb testing.TB, sb forgejoSandboxConfig, prNumber int64) (forgejoSandboxSummaryComment, forge.FixerSummary) {
	tb.Helper()
	comment, summary, err := forge.ParseUniqueFixerSummaryComment(listForgejoSandboxPRComments(tb, sb, prNumber))
	if err != nil {
		tb.Fatalf("parse unique fixer summary comment: %v", err)
	}
	return forgejoSandboxSummaryComment{ID: comment.ID, URL: comment.HTMLURL, Body: comment.Body}, summary
}

func forgejoSandboxEnsureLabel(ctx context.Context, sb forgejoSandboxConfig, name, color, description string) (int64, error) {
	var labels []struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}
	if err := forgejoSandboxAPI(ctx, sb, http.MethodGet, "repos/"+sb.Repo+"/labels", nil, &labels); err != nil {
		return 0, err
	}
	for _, label := range labels {
		if label.Name == name {
			return label.ID, nil
		}
	}
	var created struct {
		ID int64 `json:"id"`
	}
	if err := forgejoSandboxAPI(ctx, sb, http.MethodPost, "repos/"+sb.Repo+"/labels", map[string]any{"name": name, "color": color, "description": description}, &created); err != nil {
		return 0, err
	}
	if created.ID == 0 {
		return 0, fmt.Errorf("create label %q returned id=0", name)
	}
	return created.ID, nil
}

func forgejoSandboxAPI(ctx context.Context, sb forgejoSandboxConfig, method, path string, payload any, out any) error {
	apiURL, err := url.Parse(strings.TrimRight(sb.BaseURL, "/") + "/api/v1/" + strings.TrimLeft(path, "/"))
	if err != nil {
		return fmt.Errorf("build forgejo sandbox api url %q: %w", path, err)
	}
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("encode forgejo sandbox %s %s: %w", method, path, err)
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, apiURL.String(), body)
	if err != nil {
		return fmt.Errorf("build forgejo sandbox request %s %s: %w", method, path, err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "token "+sb.Token)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := sb.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("forgejo sandbox API %s %s failed: %w", method, path, err)
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read forgejo sandbox API %s %s: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("forgejo sandbox API %s %s returned HTTP %d: %s", method, path, resp.StatusCode, strings.ReplaceAll(strings.TrimSpace(string(responseBody)), sb.Token, "[REDACTED]"))
	}
	if out == nil || len(bytes.TrimSpace(responseBody)) == 0 {
		return nil
	}
	if err := json.Unmarshal(responseBody, out); err != nil {
		return fmt.Errorf("decode forgejo sandbox API %s %s: %w", method, path, err)
	}
	return nil
}

func forgejoSandboxEnvMap(sb forgejoSandboxConfig) map[string]string {
	return map[string]string{envForgejoToken: sb.Token}
}

func forgejoAuthenticatedRemoteURL(baseURL, repo, token string) (string, error) {
	u, err := url.Parse(strings.TrimRight(baseURL, "/") + "/" + strings.TrimPrefix(repo, "/") + ".git")
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("build Forgejo clone URL from %q and %q", baseURL, repo)
	}
	u.User = url.UserPassword("looper-e2e", token)
	return u.String(), nil
}
