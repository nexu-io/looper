package e2e

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/e2e/harness"
)

const (
	envSandboxEnabled  = "LOOPER_E2E_GITHUB"
	envSandboxRepo     = "LOOPER_E2E_SANDBOX_REPO"
	envSandboxToken    = "LOOPER_E2E_GITHUB_TOKEN"
	sandboxLabelName   = "looper-e2e"
	sandboxMaxAttempts = 4
)

type sandboxConfig struct {
	Repo         string
	Owner        string
	Name         string
	Token        string
	RunID        string
	TitlePrefix  string
	BranchPrefix string
	CmdEnv       []string
}

type sandboxIssue struct {
	Number int64
	URL    string
	Title  string
}

type sandboxPR struct {
	Number     int64
	URL        string
	Title      string
	HeadBranch string
	HeadSHA    string
	ThreadID   string
	ThreadURL  string
}

func TestGitHubSandboxWorkerCreatesPullRequest(t *testing.T) {
	bins := harness.MustBinaries(t)
	sb := requireSandboxConfig(t)
	home := harness.NewTempHome(t)
	repo := ensureSandboxProjectRepo(t, sb)
	port := harness.MustFreePort(t)
	fakeAgent := harness.NewFakeAgent(t, bins)
	issue := createSandboxIssue(t, sb, "worker creates pull request")
	var prURL string
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("sandbox issue=%s pr=%s branch_prefix=%s", issue.URL, prURL, sb.BranchPrefix)
		}
	})

	cfg := workerSandboxConfig(t, bins, home, repo, fakeAgent, port, "commit")
	harness.WriteConfig(t, home.ConfigPath, cfg, nil)
	proc := harness.StartLooperd(t, bins, home, home.ConfigPath, sandboxEnvMap(sb), cfg.Server.Host, cfg.Server.Port)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := proc.WaitForReady(ctx); err != nil {
		t.Fatalf("wait for ready: %v", err)
	}
	client := newAPIClient(proc.BaseURL())
	var created struct {
		ID string `json:"id"`
	}
	client.post(t, "/api/v1/workers", map[string]any{
		"projectId":   "project_1",
		"repo":        sb.Repo,
		"issueNumber": issue.Number,
		"baseBranch":  repo.DefaultBranch,
	}, &created)
	run := waitForRunTerminal(t, client, created.ID, 90*time.Second)
	if run.Status != "success" {
		t.Fatalf("run status = %s, want success (issue=%s error=%q checkpoint=%s)", run.Status, issue.URL, stringValue(run.ErrorMessage), stringValue(run.CheckpointJSON))
	}
	prs := waitForSandboxPRsByTitle(t, sb, issue.Title, 30*time.Second)
	if len(prs) != 1 {
		t.Fatalf("matching PRs = %#v, want one PR for issue %s", prs, issue.URL)
	}
	prURL = prs[0].URL
	cleanupSandboxPR(t, sb, prs[0].Number, prs[0].HeadBranch)
	cleanupSandboxIssue(t, sb, issue.Number)
	checkpoint := parseJSONObject(t, run.CheckpointJSON)
	if checkpoint["pullRequest"] == nil {
		t.Fatalf("pullRequest checkpoint missing for issue=%s pr=%s checkpoint=%#v", issue.URL, prURL, checkpoint)
	}
	proc.Stop(context.Background())
}

func TestGitHubSandboxFixerResolvesReviewThread(t *testing.T) {
	bins := harness.MustBinaries(t)
	sb := requireSandboxConfig(t)
	home := harness.NewTempHome(t)
	repo := ensureSandboxProjectRepo(t, sb)
	port := harness.MustFreePort(t)
	fakeAgent := harness.NewFakeAgent(t, bins)
	pr := createSandboxPRWithReviewThread(t, sb, repo, "fixer resolves thread")
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("sandbox pr=%s thread=%s branch=%s", pr.URL, pr.ThreadURL, pr.HeadBranch)
		}
	})
	defer cleanupSandboxPR(t, sb, pr.Number, pr.HeadBranch)

	cfg := fixerSandboxConfig(t, bins, home, repo, fakeAgent, port, "commit")
	harness.WriteConfig(t, home.ConfigPath, cfg, nil)
	proc := harness.StartLooperd(t, bins, home, home.ConfigPath, sandboxEnvMap(sb), cfg.Server.Host, cfg.Server.Port)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := proc.WaitForReady(ctx); err != nil {
		t.Fatalf("wait for ready: %v", err)
	}
	client := newAPIClient(proc.BaseURL())
	var created struct {
		ID string `json:"id"`
	}
	client.post(t, "/api/v1/loops", map[string]any{"projectId": "project_1", "type": "fixer", "targetType": "pull_request", "repo": sb.Repo, "prNumber": pr.Number}, &created)
	run := waitForRunTerminal(t, client, created.ID, 90*time.Second)
	if run.Status != "success" {
		t.Fatalf("run status = %s, want success (pr=%s thread=%s error=%q checkpoint=%s)", run.Status, pr.URL, pr.ThreadURL, stringValue(run.ErrorMessage), stringValue(run.CheckpointJSON))
	}
	thread := loadSandboxThread(t, sb, pr.Number, pr.ThreadID)
	if !thread.IsResolved {
		t.Fatalf("thread = %#v, want resolved (pr=%s thread=%s)", thread, pr.URL, pr.ThreadURL)
	}
	proc.Stop(context.Background())
}

func TestGitHubSandboxNoDiffPathsDoNotOpenOrResolve(t *testing.T) {
	bins := harness.MustBinaries(t)
	sb := requireSandboxConfig(t)

	t.Run("worker-no-diff-no-pr", func(t *testing.T) {
		home := harness.NewTempHome(t)
		repo := ensureSandboxProjectRepo(t, sb)
		port := harness.MustFreePort(t)
		fakeAgent := harness.NewFakeAgent(t, bins)
		issue := createSandboxIssue(t, sb, "worker no diff")
		t.Cleanup(func() {
			if t.Failed() {
				t.Logf("sandbox issue=%s branch_prefix=%s", issue.URL, sb.BranchPrefix)
			}
		})
		defer cleanupSandboxIssue(t, sb, issue.Number)
		cfg := workerSandboxConfig(t, bins, home, repo, fakeAgent, port, "success-no-diff")
		harness.WriteConfig(t, home.ConfigPath, cfg, nil)
		proc := harness.StartLooperd(t, bins, home, home.ConfigPath, sandboxEnvMap(sb), cfg.Server.Host, cfg.Server.Port)
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
		if len(findSandboxPRsByTitle(t, sb, issue.Title)) != 0 {
			t.Fatalf("unexpected PR created for no-diff issue %s", issue.URL)
		}
		proc.Stop(context.Background())
	})

	t.Run("fixer-no-new-commit-keeps-thread-unresolved", func(t *testing.T) {
		home := harness.NewTempHome(t)
		repo := ensureSandboxProjectRepo(t, sb)
		port := harness.MustFreePort(t)
		fakeAgent := harness.NewFakeAgent(t, bins)
		pr := createSandboxPRWithReviewThread(t, sb, repo, "fixer no new commit")
		t.Cleanup(func() {
			if t.Failed() {
				t.Logf("sandbox pr=%s thread=%s branch=%s", pr.URL, pr.ThreadURL, pr.HeadBranch)
			}
		})
		defer cleanupSandboxPR(t, sb, pr.Number, pr.HeadBranch)
		cfg := fixerSandboxConfig(t, bins, home, repo, fakeAgent, port, "success-no-diff")
		harness.WriteConfig(t, home.ConfigPath, cfg, nil)
		proc := harness.StartLooperd(t, bins, home, home.ConfigPath, sandboxEnvMap(sb), cfg.Server.Host, cfg.Server.Port)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if _, err := proc.WaitForReady(ctx); err != nil {
			t.Fatalf("wait for ready: %v", err)
		}
		client := newAPIClient(proc.BaseURL())
		var created struct {
			ID string `json:"id"`
		}
		client.post(t, "/api/v1/loops", map[string]any{"projectId": "project_1", "type": "fixer", "targetType": "pull_request", "repo": sb.Repo, "prNumber": pr.Number}, &created)
		run := waitForRunTerminal(t, client, created.ID, 60*time.Second)
		if run.Status != "failed" {
			t.Fatalf("run status = %s, want failed no-new-commit path (pr=%s thread=%s error=%q checkpoint=%s)", run.Status, pr.URL, pr.ThreadURL, stringValue(run.ErrorMessage), stringValue(run.CheckpointJSON))
		}
		checkpoint := parseJSONObject(t, run.CheckpointJSON)
		push, _ := checkpoint["push"].(map[string]any)
		if push == nil || push["pushed"] != false {
			t.Fatalf("push = %#v, want pushed=false (pr=%s)", push, pr.URL)
		}
		if reason, _ := push["skippedReason"].(string); !strings.Contains(reason, "No new commits") {
			t.Fatalf("push.skippedReason = %q, want no-new-commits skip (pr=%s)", reason, pr.URL)
		}
		thread := loadSandboxThread(t, sb, pr.Number, pr.ThreadID)
		if thread.IsResolved {
			t.Fatalf("thread = %#v, want unresolved after no-new-commit path (pr=%s thread=%s)", thread, pr.URL, pr.ThreadURL)
		}
		proc.Stop(context.Background())
	})
}

func requireSandboxConfig(tb testing.TB) sandboxConfig {
	tb.Helper()
	if strings.TrimSpace(os.Getenv(envSandboxEnabled)) != "1" {
		tb.Skipf("set %s=1 to run real GitHub sandbox E2E", envSandboxEnabled)
	}
	repo := strings.TrimSpace(os.Getenv(envSandboxRepo))
	if repo == "" {
		tb.Fatalf("%s=1 requires %s to be set", envSandboxEnabled, envSandboxRepo)
	}
	token := strings.TrimSpace(os.Getenv(envSandboxToken))
	if token == "" {
		tb.Fatalf("%s=1 requires %s to be set; configure a sandbox-scoped GitHub App or fine-grained PAT", envSandboxEnabled, envSandboxToken)
	}
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" {
		tb.Fatalf("invalid sandbox repo %q, want owner/repo", repo)
	}
	runID := strconv.FormatInt(time.Now().UTC().UnixNano(), 36)
	titlePrefix := "looper-e2e:" + runID
	branchPrefix := "looper-e2e-" + runID
	cmdEnv := append(os.Environ(), "GH_TOKEN="+token, "GITHUB_TOKEN="+token, "GH_PROMPT_DISABLED=1")
	if _, err := runSandboxCommand("", cmdEnv, "gh", "auth", "status"); err != nil {
		tb.Fatalf("sandbox GitHub auth unavailable with %s: %v", envSandboxToken, err)
	}
	return sandboxConfig{Repo: repo, Owner: owner, Name: name, Token: token, RunID: runID, TitlePrefix: titlePrefix, BranchPrefix: branchPrefix, CmdEnv: cmdEnv}
}

func sandboxEnvMap(sb sandboxConfig) map[string]string {
	return map[string]string{
		"GH_TOKEN":           sb.Token,
		"GITHUB_TOKEN":       sb.Token,
		"GH_PROMPT_DISABLED": "1",
	}
}

func ensureSandboxProjectRepo(tb testing.TB, sb sandboxConfig) harness.SeededRepo {
	tb.Helper()
	repoPath := filepath.Join(tb.TempDir(), "repo")
	cloneURL := authenticatedRemoteURL(sb)
	if _, err := runSandboxCommand("", sb.CmdEnv, "git", sandboxGitArgs(sb, "clone", cloneURL, repoPath)...); err != nil {
		if err := os.MkdirAll(repoPath, 0o755); err != nil {
			tb.Fatalf("mkdir repo path: %v", err)
		}
		runSandboxCommandMust(tb, "", sb.CmdEnv, "git", "init", "-b", "main", repoPath)
		runSandboxCommandMust(tb, repoPath, sb.CmdEnv, "git", "remote", "add", "origin", cloneURL)
		runSandboxCommandMust(tb, repoPath, sb.CmdEnv, "git", "config", "user.name", "Looper E2E")
		runSandboxCommandMust(tb, repoPath, sb.CmdEnv, "git", "config", "user.email", "looper-e2e@example.com")
		configureSandboxGitAuth(tb, repoPath)
		readmePath := filepath.Join(repoPath, "README.md")
		if err := os.WriteFile(readmePath, []byte("# Looper sandbox\n"), 0o644); err != nil {
			tb.Fatalf("write README: %v", err)
		}
		runSandboxCommandMust(tb, repoPath, sb.CmdEnv, "git", "add", "README.md")
		runSandboxCommandMust(tb, repoPath, sb.CmdEnv, "git", "commit", "-m", "seed sandbox repo")
		runSandboxCommandMust(tb, repoPath, sb.CmdEnv, "git", sandboxGitArgs(sb, "push", "-u", "origin", "main")...)
	} else {
		runSandboxCommandMust(tb, repoPath, sb.CmdEnv, "git", "config", "user.name", "Looper E2E")
		runSandboxCommandMust(tb, repoPath, sb.CmdEnv, "git", "config", "user.email", "looper-e2e@example.com")
		configureSandboxGitAuth(tb, repoPath)
		runSandboxCommandMust(tb, repoPath, sb.CmdEnv, "git", "checkout", "-B", "main", "origin/main")
	}
	ensureSandboxLabel(tb, sb)
	return harness.SeededRepo{Path: repoPath, DefaultBranch: "main", InitialCommit: strings.TrimSpace(runSandboxCommandMust(tb, repoPath, sb.CmdEnv, "git", "rev-parse", "HEAD"))}
}

func ensureSandboxLabel(tb testing.TB, sb sandboxConfig) {
	tb.Helper()
	output, err := runSandboxCommand("", sb.CmdEnv, "gh", "label", "create", sandboxLabelName, "--repo", sb.Repo, "--color", "5319e7", "--description", "Looper sandbox E2E resources")
	if err != nil && !strings.Contains(output, "already exists") {
		tb.Fatalf("ensure sandbox label: %v\noutput=%s", err, output)
	}
}

func workerSandboxConfig(tb testing.TB, bins harness.BuiltBinaries, home harness.TempHome, repo harness.SeededRepo, fakeAgent harness.FakeAgent, port int, agentMode string) config.Config {
	tb.Helper()
	vendor, command, agentEnv := fakeAgent.AgentConfig(agentMode, "git")
	cfg := harness.DefaultConfig(tb, home, harness.ConfigOptions{
		Port:              port,
		ToolPaths:         harness.TestToolPaths{Git: "git", GH: "gh", Looper: bins.LooperPath, Osascript: bins.FakeOsascriptPath},
		EnableOsascript:   true,
		AgentVendor:       vendor,
		AgentCommand:      command,
		AgentEnv:          agentEnv,
		Projects:          writeProjectConfig(repo, home),
		DisableDisclosure: true,
	})
	cfg.Scheduler.PollIntervalSeconds = 10
	cfg.Defaults.AllowAutoCommit = true
	cfg.Defaults.AllowAutoPush = true
	cfg.Defaults.OpenPRStrategy = config.OpenPRStrategyAllDone
	cfg.Roles.Worker.Triggers.RequireAssigneeCurrentUser = false
	return cfg
}

func fixerSandboxConfig(tb testing.TB, bins harness.BuiltBinaries, home harness.TempHome, repo harness.SeededRepo, fakeAgent harness.FakeAgent, port int, agentMode string) config.Config {
	tb.Helper()
	vendor, command, agentEnv := fakeAgent.AgentConfig(agentMode, "git")
	cfg := harness.DefaultConfig(tb, home, harness.ConfigOptions{
		Port:              port,
		ToolPaths:         harness.TestToolPaths{Git: "git", GH: "gh", Looper: bins.LooperPath, Osascript: bins.FakeOsascriptPath},
		EnableOsascript:   true,
		AgentVendor:       vendor,
		AgentCommand:      command,
		AgentEnv:          agentEnv,
		Projects:          writeProjectConfig(repo, home),
		DisableDisclosure: true,
	})
	cfg.Scheduler.PollIntervalSeconds = 10
	cfg.Defaults.AllowAutoCommit = true
	cfg.Defaults.AllowAutoPush = true
	cfg.Defaults.AllowRiskyFixes = true
	cfg.Defaults.OpenPRStrategy = config.OpenPRStrategyManual
	cfg.Roles.Fixer.Triggers.AuthorFilter = config.FixerAuthorFilterAny
	return cfg
}

func createSandboxIssue(tb testing.TB, sb sandboxConfig, scenario string) sandboxIssue {
	tb.Helper()
	title := sb.TitlePrefix + " " + scenario
	body := fmt.Sprintf("Sandbox E2E issue for %s (%s)", scenario, sb.RunID)
	var issue struct {
		Number  int64  `json:"number"`
		HTMLURL string `json:"html_url"`
	}
	runSandboxJSON(tb, "", sb.CmdEnv, &issue, "gh", "api", "repos/"+sb.Repo+"/issues", "--method", "POST", "-f", "title="+title, "-f", "body="+body, "-f", "labels[]="+sandboxLabelName)
	return sandboxIssue{Number: issue.Number, URL: issue.HTMLURL, Title: title}
}

func createSandboxPRWithReviewThread(tb testing.TB, sb sandboxConfig, repo harness.SeededRepo, scenario string) sandboxPR {
	tb.Helper()
	branch := sb.BranchPrefix + "-" + strings.ReplaceAll(strings.ToLower(scenario), " ", "-")
	filePath := filepath.Join(repo.Path, "sandbox", branch+".txt")
	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		tb.Fatalf("mkdir sandbox file dir: %v", err)
	}
	runSandboxCommandMust(tb, repo.Path, sb.CmdEnv, "git", "checkout", "main")
	runSandboxCommandMust(tb, repo.Path, sb.CmdEnv, "git", "pull", "--ff-only", "origin", "main")
	runSandboxCommandMust(tb, repo.Path, sb.CmdEnv, "git", "checkout", "-B", branch)
	content := fmt.Sprintf("sandbox review target %s\n", sb.RunID)
	if err := os.WriteFile(filePath, []byte(content), 0o644); err != nil {
		tb.Fatalf("write sandbox file: %v", err)
	}
	runSandboxCommandMust(tb, repo.Path, sb.CmdEnv, "git", "add", filepath.ToSlash(strings.TrimPrefix(filePath, repo.Path+string(os.PathSeparator))))
	runSandboxCommandMust(tb, repo.Path, sb.CmdEnv, "git", "commit", "-m", sb.TitlePrefix+" "+scenario)
	headSHA := strings.TrimSpace(runSandboxCommandMust(tb, repo.Path, sb.CmdEnv, "git", "rev-parse", "HEAD"))
	runSandboxCommandMust(tb, repo.Path, sb.CmdEnv, "git", sandboxGitArgs(sb, "push", "-u", "origin", branch)...)
	title := sb.TitlePrefix + " " + scenario
	body := fmt.Sprintf("Sandbox PR for %s (%s)", scenario, sb.RunID)
	prURL := strings.TrimSpace(runSandboxCommandMust(tb, repo.Path, sb.CmdEnv, "gh", "pr", "create", "--repo", sb.Repo, "--head", branch, "--base", repo.DefaultBranch, "--title", title, "--body", body))
	var prView struct {
		Number int64 `json:"number"`
	}
	runSandboxJSON(tb, repo.Path, sb.CmdEnv, &prView, "gh", "pr", "view", prURL, "--repo", sb.Repo, "--json", "number")
	commentBody := sb.TitlePrefix + " please fix this"
	var reviewComment struct {
		HTMLURL string `json:"html_url"`
	}
	relPath := filepath.ToSlash(filepath.Join("sandbox", branch+".txt"))
	runSandboxJSON(tb, repo.Path, sb.CmdEnv, &reviewComment, "gh", "api", "repos/"+sb.Repo+"/pulls/"+strconv.FormatInt(prView.Number, 10)+"/comments", "--method", "POST", "-f", "body="+commentBody, "-f", "commit_id="+headSHA, "-f", "path="+relPath, "-F", "line=1", "-f", "side=RIGHT")
	thread := findSandboxThreadByComment(tb, sb, prView.Number, commentBody)
	return sandboxPR{Number: prView.Number, URL: prURL, Title: title, HeadBranch: branch, HeadSHA: headSHA, ThreadID: thread.ID, ThreadURL: reviewComment.HTMLURL}
}

type sandboxThread struct {
	ID         string
	IsResolved bool
	Comments   []sandboxThreadComment
}

type sandboxThreadComment struct {
	ID   string
	Body string
	Path string
	Line int
	URL  string
}

func findSandboxThreadByComment(tb testing.TB, sb sandboxConfig, prNumber int64, bodySubstring string) sandboxThread {
	tb.Helper()
	threads := listSandboxThreads(tb, sb, prNumber)
	for _, thread := range threads {
		for _, comment := range thread.Comments {
			if strings.Contains(comment.Body, bodySubstring) {
				return thread
			}
		}
	}
	tb.Fatalf("sandbox thread containing %q not found for %s#%d", bodySubstring, sb.Repo, prNumber)
	return sandboxThread{}
}

func loadSandboxThread(tb testing.TB, sb sandboxConfig, prNumber int64, threadID string) sandboxThread {
	tb.Helper()
	threads := listSandboxThreads(tb, sb, prNumber)
	for _, thread := range threads {
		if thread.ID == threadID {
			return thread
		}
	}
	tb.Fatalf("sandbox thread %s not found for %s#%d", threadID, sb.Repo, prNumber)
	return sandboxThread{}
}

func listSandboxThreads(tb testing.TB, sb sandboxConfig, prNumber int64) []sandboxThread {
	tb.Helper()
	var response struct {
		Data struct {
			Repository struct {
				PullRequest struct {
					ReviewThreads struct {
						Nodes []struct {
							ID         string `json:"id"`
							IsResolved bool   `json:"isResolved"`
							Comments   struct {
								Nodes []struct {
									ID   string `json:"id"`
									Body string `json:"body"`
									Path string `json:"path"`
									Line int    `json:"line"`
									URL  string `json:"url"`
								} `json:"nodes"`
							} `json:"comments"`
						} `json:"nodes"`
					} `json:"reviewThreads"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
	}
	query := `query($owner:String!, $name:String!, $number:Int!) { repository(owner:$owner, name:$name) { pullRequest(number:$number) { reviewThreads(first:100) { nodes { id isResolved comments(first:20) { nodes { id body path line url } } } } } } }`
	runSandboxJSON(tb, "", sb.CmdEnv, &response, "gh", "api", "graphql", "-F", "owner="+sb.Owner, "-F", "name="+sb.Name, "-F", "number="+strconv.FormatInt(prNumber, 10), "-f", "query="+query)
	out := make([]sandboxThread, 0, len(response.Data.Repository.PullRequest.ReviewThreads.Nodes))
	for _, node := range response.Data.Repository.PullRequest.ReviewThreads.Nodes {
		thread := sandboxThread{ID: node.ID, IsResolved: node.IsResolved, Comments: make([]sandboxThreadComment, 0, len(node.Comments.Nodes))}
		for _, comment := range node.Comments.Nodes {
			thread.Comments = append(thread.Comments, sandboxThreadComment{ID: comment.ID, Body: comment.Body, Path: comment.Path, Line: comment.Line, URL: comment.URL})
		}
		out = append(out, thread)
	}
	return out
}

func findSandboxPRsByTitle(tb testing.TB, sb sandboxConfig, title string) []sandboxPR {
	tb.Helper()
	var prs []struct {
		Number      int64  `json:"number"`
		URL         string `json:"url"`
		Title       string `json:"title"`
		HeadRefName string `json:"headRefName"`
	}
	runSandboxJSON(tb, "", sb.CmdEnv, &prs, "gh", "pr", "list", "--repo", sb.Repo, "--state", "all", "--search", title, "--json", "number,url,title,headRefName")
	out := make([]sandboxPR, 0, len(prs))
	for _, pr := range prs {
		if pr.Title != title {
			continue
		}
		out = append(out, sandboxPR{Number: pr.Number, URL: pr.URL, Title: pr.Title, HeadBranch: pr.HeadRefName})
	}
	return out
}

func waitForSandboxPRsByTitle(tb testing.TB, sb sandboxConfig, title string, timeout time.Duration) []sandboxPR {
	tb.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		prs := findSandboxPRsByTitle(tb, sb, title)
		if len(prs) > 0 {
			return prs
		}
		time.Sleep(2 * time.Second)
	}
	return findSandboxPRsByTitle(tb, sb, title)
}

func cleanupSandboxIssue(tb testing.TB, sb sandboxConfig, issueNumber int64) {
	tb.Helper()
	_, _ = runSandboxCommand("", sb.CmdEnv, "gh", "issue", "close", strconv.FormatInt(issueNumber, 10), "--repo", sb.Repo)
}

func cleanupSandboxPR(tb testing.TB, sb sandboxConfig, prNumber int64, branch string) {
	tb.Helper()
	if prNumber > 0 {
		_, _ = runSandboxCommand("", sb.CmdEnv, "gh", "pr", "close", strconv.FormatInt(prNumber, 10), "--repo", sb.Repo, "--delete-branch")
	}
	if strings.TrimSpace(branch) != "" {
		_, _ = runSandboxCommand("", sb.CmdEnv, "git", sandboxGitArgs(sb, "push", authenticatedRemoteURL(sb), "--delete", branch)...)
	}
}

func authenticatedRemoteURL(sb sandboxConfig) string {
	u := &url.URL{Scheme: "https", Host: "github.com", Path: "/" + sb.Repo + ".git"}
	u.User = url.UserPassword("x-access-token", sb.Token)
	return u.String()
}

func configureSandboxGitAuth(tb testing.TB, repoPath string) {
	tb.Helper()
	runSandboxCommandMust(tb, repoPath, nil, "git", "config", "credential.helper", "")
	runSandboxCommandMust(tb, repoPath, nil, "git", "config", "credential.interactive", "never")
	runSandboxCommandMust(tb, repoPath, nil, "git", "config", "core.askPass", "/usr/bin/true")
}

func sandboxCommandEnv(env []string) []string {
	merged := append([]string(nil), os.Environ()...)
	merged = append(merged,
		"GH_PROMPT_DISABLED=1",
		"GIT_TERMINAL_PROMPT=0",
		"GCM_INTERACTIVE=Never",
		"GIT_ASKPASS=/usr/bin/true",
	)
	merged = append(merged, env...)
	return merged
}

func sandboxGitArgs(sb sandboxConfig, args ...string) []string {
	auth := "AUTHORIZATION: basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+sb.Token))
	prefix := []string{
		"-c", "credential.helper=",
		"-c", "credential.interactive=never",
		"-c", "core.askPass=/usr/bin/true",
		"-c", "http.https://github.com/.extraheader=" + auth,
	}
	return append(prefix, args...)
}

func runSandboxJSON(tb testing.TB, cwd string, env []string, target any, name string, args ...string) {
	tb.Helper()
	output := runSandboxCommandMust(tb, cwd, env, name, args...)
	if err := json.Unmarshal([]byte(output), target); err != nil {
		tb.Fatalf("decode %s %v: %v\noutput=%s", name, args, err, output)
	}
}

func runSandboxCommandMust(tb testing.TB, cwd string, env []string, name string, args ...string) string {
	tb.Helper()
	output, err := runSandboxCommand(cwd, env, name, args...)
	if err != nil {
		tb.Fatalf("%s %s failed: %v\noutput=%s", name, strings.Join(args, " "), err, output)
	}
	return output
}

func runSandboxCommand(cwd string, env []string, name string, args ...string) (string, error) {
	var output string
	var err error
	for attempt := 1; attempt <= sandboxMaxAttempts; attempt++ {
		cmd := exec.Command(name, args...)
		if cwd != "" {
			cmd.Dir = cwd
		}
		cmd.Env = sandboxCommandEnv(env)
		payload, runErr := cmd.CombinedOutput()
		output, err = string(payload), runErr
		if err == nil || !shouldRetrySandboxCommand(output, err) || attempt == sandboxMaxAttempts {
			return output, err
		}
		time.Sleep(time.Duration(attempt) * 2 * time.Second)
	}
	return output, err
}

func shouldRetrySandboxCommand(output string, err error) bool {
	if err == nil {
		return false
	}
	lower := strings.ToLower(output)
	return strings.Contains(lower, "secondary rate limit") ||
		strings.Contains(lower, "rate limit exceeded") ||
		strings.Contains(lower, "abuse detection") ||
		strings.Contains(lower, "http 502") ||
		strings.Contains(lower, "http 503") ||
		strings.Contains(lower, "http 504")
}
