package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/coordinator"
	"github.com/nexu-io/looper/internal/e2e/harness"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/storage"
)

const (
	depGateDispatchFailureMarker = "<!-- looper:coordinator:dispatch-failure -->"
	depGateCycleMarker           = "<!-- looper:coordinator:cycle -->"
)

type depGateSandboxIssue struct {
	ID     int64  `json:"id"`
	Number int64  `json:"number"`
	URL    string `json:"html_url"`
	Title  string
}

type depGateSandboxComment struct {
	ID   int64  `json:"id"`
	Body string `json:"body"`
	URL  string `json:"html_url"`
}

type depGateSandboxIssueView struct {
	State  string `json:"state"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
}

func TestGitHubSandboxDependencyGateScenarios(t *testing.T) {
	bins := harness.MustBinaries(t)
	sb := requireSandboxConfig(t)
	t.Setenv("GH_TOKEN", sb.Token)
	t.Setenv("GITHUB_TOKEN", sb.Token)
	t.Setenv("GH_PROMPT_DISABLED", "1")
	repo := ensureSandboxProjectRepo(t, sb)
	ensureSandboxCoordinatorLabel(t, sb, "triaged", "0e8a16")
	ensureSandboxCoordinatorLabel(t, sb, "dispatch/plan", "1d76db")

	t.Run("looperd startup validation succeeds against real dependency API", func(t *testing.T) {
		home := harness.NewTempHome(t)
		port := harness.MustFreePort(t)
		cfg := coordinatorSandboxDaemonConfig(t, bins, home, repo, port)
		harness.WriteConfig(t, home.ConfigPath, cfg, nil)
		seedCoordinatorSandboxProjectMetadata(t, home, repo, sb.Repo)
		proc := harness.StartLooperd(t, bins, home, home.ConfigPath, sandboxEnvMap(sb), cfg.Server.Host, cfg.Server.Port)
		defer proc.Stop(context.Background())
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if _, err := proc.WaitForReady(ctx); err != nil {
			t.Fatalf("wait for ready: %v", err)
		}
	})

	t.Run("human gated blocked_by fails then releases after completion", func(t *testing.T) {
		home := harness.NewTempHome(t)
		blocker := createDependencyGateIssue(t, sb, "dependency blocker")
		dependent := createDependencyGateIssue(t, sb, "dependency dependent")
		defer closeSandboxIssueIfOpen(t, sb, blocker.Number, "not_planned")
		defer closeSandboxIssueIfOpen(t, sb, dependent.Number, "not_planned")
		addSandboxIssueLabels(t, sb, dependent.Number, "triaged", "dispatch/plan")
		linkSandboxBlockedBy(t, sb, dependent.Number, blocker.ID)
		postSandboxIssueComment(t, sb, dependent.Number, "/plan")

		runner := newCoordinatorSandboxRunner(t, home, repo, sb.Repo)
		if _, err := runner.DiscoverIssues(context.Background(), coordinator.DiscoveryInput{ProjectID: "project_1", Repo: sb.Repo}); err != nil {
			t.Fatalf("DiscoverIssues blocked path: %v", err)
		}

		waitForSandboxIssueComment(t, sb, dependent.Number, depGateDispatchFailureMarker)
		waitForSandboxIssueLabelAbsent(t, sb, dependent.Number, "looper:plan")

		setSandboxIssueState(t, sb, blocker.Number, "closed", "completed")
		postSandboxIssueComment(t, sb, dependent.Number, "/plan")
		if _, err := runner.DiscoverIssues(context.Background(), coordinator.DiscoveryInput{ProjectID: "project_1", Repo: sb.Repo}); err != nil {
			t.Fatalf("DiscoverIssues release path: %v", err)
		}
		waitForSandboxIssueLabel(t, sb, dependent.Number, "looper:plan")
	})

	t.Run("GitHub rejects blocked_by cycle creation", func(t *testing.T) {
		left := createDependencyGateIssue(t, sb, "cycle left")
		right := createDependencyGateIssue(t, sb, "cycle right")
		defer closeSandboxIssueIfOpen(t, sb, left.Number, "not_planned")
		defer closeSandboxIssueIfOpen(t, sb, right.Number, "not_planned")
		linkSandboxBlockedBy(t, sb, left.Number, right.ID)
		// Real GitHub rejects cycles before coordinator discovery can observe them.
		// Coordinator-side cycle re-triage remains covered by TestCoordinatorCycleHandlingWithFakeGH.
		output, err := runSandboxCommand("", sb.CmdEnv, "gh", "api", fmt.Sprintf("repos/%s/issues/%d/dependencies/blocked_by", sb.Repo, right.Number), "--method", "POST", "-H", "X-GitHub-Api-Version: 2026-03-10", "-F", fmt.Sprintf("issue_id=%d", left.ID))
		if err == nil {
			t.Fatalf("expected GitHub to reject cycle creation, but request succeeded: %s", output)
		}
		if !strings.Contains(output, "would create a cycle") {
			t.Fatalf("expected cycle validation error, got: %s", output)
		}
	})

	t.Run("not planned blocker returns dependent to retriage without cycle comment", func(t *testing.T) {
		home := harness.NewTempHome(t)
		blocker := createDependencyGateIssue(t, sb, "not planned blocker")
		dependent := createDependencyGateIssue(t, sb, "not planned dependent")
		defer closeSandboxIssueIfOpen(t, sb, blocker.Number, "not_planned")
		defer closeSandboxIssueIfOpen(t, sb, dependent.Number, "not_planned")
		addSandboxIssueLabels(t, sb, dependent.Number, "triaged", "dispatch/plan")
		linkSandboxBlockedBy(t, sb, dependent.Number, blocker.ID)
		setSandboxIssueState(t, sb, blocker.Number, "closed", "not_planned")

		runner := newCoordinatorSandboxRunner(t, home, repo, sb.Repo)
		if _, err := runner.DiscoverIssues(context.Background(), coordinator.DiscoveryInput{ProjectID: "project_1", Repo: sb.Repo}); err != nil {
			t.Fatalf("DiscoverIssues not_planned path: %v", err)
		}

		waitForSandboxIssueLabelsAbsent(t, sb, dependent.Number, "triaged", "dispatch/plan")
		comments := listSandboxIssueComments(t, sb, dependent.Number)
		for _, comment := range comments {
			if strings.Contains(comment.Body, depGateCycleMarker) {
				t.Fatalf("unexpected cycle comment on dependent %s: %s", dependent.URL, comment.URL)
			}
		}
	})
}

func coordinatorSandboxConfig(tb testing.TB, home harness.TempHome, repo harness.SeededRepo) config.Config {
	tb.Helper()
	cfg := harness.DefaultConfig(tb, home, harness.ConfigOptions{
		Projects:          writeProjectConfig(repo, home),
		DisableDisclosure: true,
	})
	cfg.Roles.Coordinator.Enabled = true
	cfg.Roles.Coordinator.PollInterval = "0s"
	cfg.Roles.Coordinator.Dependencies.Enabled = true
	cfg.Roles.Planner.AutoDiscovery = false
	cfg.Roles.Worker.AutoDiscovery = false
	cfg.Roles.Fixer.AutoDiscovery = false
	return cfg
}

func coordinatorSandboxDaemonConfig(tb testing.TB, bins harness.BuiltBinaries, home harness.TempHome, repo harness.SeededRepo, port int) config.Config {
	tb.Helper()
	cfg := harness.DefaultConfig(tb, home, harness.ConfigOptions{
		Port:              port,
		ToolPaths:         harness.TestToolPaths{Git: "git", GH: "gh", Looper: bins.LooperPath, Osascript: bins.FakeOsascriptPath},
		EnableOsascript:   true,
		Projects:          writeProjectConfig(repo, home),
		DisableDisclosure: true,
	})
	cfg.Roles.Coordinator.Enabled = true
	cfg.Roles.Coordinator.PollInterval = "2s"
	cfg.Roles.Coordinator.Dependencies.Enabled = true
	cfg.Roles.Planner.AutoDiscovery = false
	cfg.Roles.Worker.AutoDiscovery = false
	cfg.Roles.Fixer.AutoDiscovery = false
	return cfg
}

func newCoordinatorSandboxRunner(tb testing.TB, home harness.TempHome, repo harness.SeededRepo, repoSlug string) *coordinator.Runner {
	tb.Helper()
	coord := seedCoordinatorSandboxProjectMetadata(tb, home, repo, repoSlug)
	tb.Cleanup(func() { _ = coord.Close() })
	cfg := coordinatorSandboxConfig(tb, home, repo)
	gateway := githubinfra.New(githubinfra.Options{GHPath: "gh", CWD: repo.Path, Now: time.Now})
	return coordinator.New(coordinator.Options{Repos: storage.NewRepositories(coord.DB()), GitHub: gateway, Config: &cfg, Now: time.Now})
}

func seedCoordinatorSandboxProjectMetadata(tb testing.TB, home harness.TempHome, repo harness.SeededRepo, repoSlug string) *storage.SQLiteCoordinator {
	tb.Helper()
	coordinator, err := storage.OpenSQLiteCoordinator(context.Background(), home.DBPath, storage.SQLiteCoordinatorOptions{Migrations: storage.EmbeddedMigrations, BackupDir: home.BackupDir})
	if err != nil {
		tb.Fatalf("open sqlite coordinator: %v", err)
	}
	if _, err := coordinator.MigrationRunner().RunPending(context.Background()); err != nil {
		tb.Fatalf("run sqlite migrations: %v", err)
	}
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Now().UTC().Format(time.RFC3339)
	metadata := fmt.Sprintf(`{"repo":%q}`, repoSlug)
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID:           "project_1",
		Name:         "Looper E2E",
		RepoPath:     repo.Path,
		BaseBranch:   stringPtr(repo.DefaultBranch),
		Archived:     false,
		MetadataJSON: &metadata,
		CreatedAt:    now,
		UpdatedAt:    now,
	}); err != nil {
		tb.Fatalf("seed project metadata: %v", err)
	}
	return coordinator
}

func ensureSandboxCoordinatorLabel(tb testing.TB, sb sandboxConfig, name, color string) {
	tb.Helper()
	output, err := runSandboxCommand("", sb.CmdEnv, "gh", "label", "create", name, "--repo", sb.Repo, "--color", color, "--description", "Looper dependency gate sandbox label")
	if err != nil && !strings.Contains(output, "already exists") {
		tb.Fatalf("ensure label %s: %v\noutput=%s", name, err, output)
	}
}

func createDependencyGateIssue(tb testing.TB, sb sandboxConfig, scenario string) depGateSandboxIssue {
	tb.Helper()
	title := fmt.Sprintf("%s %s", sb.TitlePrefix, scenario)
	body := fmt.Sprintf("Dependency gate sandbox issue for %s (%s)", scenario, sb.RunID)
	var issue depGateSandboxIssue
	runSandboxJSON(tb, "", sb.CmdEnv, &issue, "gh", "api", "repos/"+sb.Repo+"/issues", "--method", "POST", "-f", "title="+title, "-f", "body="+body, "-f", "labels[]="+sandboxLabelName)
	issue.Title = title
	return issue
}

func addSandboxIssueLabels(tb testing.TB, sb sandboxConfig, issueNumber int64, labels ...string) {
	tb.Helper()
	args := []string{"gh", "api", fmt.Sprintf("repos/%s/issues/%d/labels", sb.Repo, issueNumber), "--method", "POST"}
	for _, label := range labels {
		args = append(args, "-f", "labels[]="+label)
	}
	runSandboxCommandMust(tb, "", sb.CmdEnv, args[0], args[1:]...)
}

func linkSandboxBlockedBy(tb testing.TB, sb sandboxConfig, issueNumber, blockerID int64) {
	tb.Helper()
	runSandboxCommandMust(tb, "", sb.CmdEnv, "gh", "api", fmt.Sprintf("repos/%s/issues/%d/dependencies/blocked_by", sb.Repo, issueNumber), "--method", "POST", "-H", "X-GitHub-Api-Version: 2026-03-10", "-F", fmt.Sprintf("issue_id=%d", blockerID))
}

func postSandboxIssueComment(tb testing.TB, sb sandboxConfig, issueNumber int64, body string) {
	tb.Helper()
	runSandboxCommandMust(tb, "", sb.CmdEnv, "gh", "api", fmt.Sprintf("repos/%s/issues/%d/comments", sb.Repo, issueNumber), "--method", "POST", "-f", "body="+body)
}

func setSandboxIssueState(tb testing.TB, sb sandboxConfig, issueNumber int64, state, reason string) {
	tb.Helper()
	args := []string{"gh", "api", fmt.Sprintf("repos/%s/issues/%d", sb.Repo, issueNumber), "--method", "PATCH", "-f", "state=" + state}
	if strings.TrimSpace(reason) != "" {
		args = append(args, "-f", "state_reason="+reason)
	}
	runSandboxCommandMust(tb, "", sb.CmdEnv, args[0], args[1:]...)
}

func closeSandboxIssueIfOpen(tb testing.TB, sb sandboxConfig, issueNumber int64, reason string) {
	tb.Helper()
	issue := getSandboxIssueView(tb, sb, issueNumber)
	if issue.State == "closed" {
		return
	}
	setSandboxIssueState(tb, sb, issueNumber, "closed", reason)
}

func waitForSandboxIssueLabel(tb testing.TB, sb sandboxConfig, issueNumber int64, want string) {
	tb.Helper()
	waitForCondition(tb, 90*time.Second, func() (bool, string) {
		issue := getSandboxIssueView(tb, sb, issueNumber)
		for _, label := range issue.Labels {
			if label.Name == want {
				return true, ""
			}
		}
		return false, fmt.Sprintf("issue #%d labels=%v missing %s", issueNumber, sandboxLabelNames(issue.Labels), want)
	})
}

func waitForSandboxIssueLabelAbsent(tb testing.TB, sb sandboxConfig, issueNumber int64, unwanted string) {
	tb.Helper()
	waitForCondition(tb, 45*time.Second, func() (bool, string) {
		issue := getSandboxIssueView(tb, sb, issueNumber)
		for _, label := range issue.Labels {
			if label.Name == unwanted {
				return false, fmt.Sprintf("issue #%d still has label %s", issueNumber, unwanted)
			}
		}
		return true, ""
	})
}

func waitForSandboxIssueLabelsAbsent(tb testing.TB, sb sandboxConfig, issueNumber int64, unwanted ...string) {
	tb.Helper()
	waitForCondition(tb, 90*time.Second, func() (bool, string) {
		issue := getSandboxIssueView(tb, sb, issueNumber)
		present := map[string]bool{}
		for _, label := range issue.Labels {
			present[label.Name] = true
		}
		for _, label := range unwanted {
			if present[label] {
				return false, fmt.Sprintf("issue #%d still has label %s; labels=%v", issueNumber, label, sandboxLabelNames(issue.Labels))
			}
		}
		return true, ""
	})
}

func waitForSandboxIssueComment(tb testing.TB, sb sandboxConfig, issueNumber int64, contains string) {
	tb.Helper()
	waitForCondition(tb, 90*time.Second, func() (bool, string) {
		comments := listSandboxIssueComments(tb, sb, issueNumber)
		for _, comment := range comments {
			if strings.Contains(comment.Body, contains) {
				return true, ""
			}
		}
		return false, fmt.Sprintf("issue #%d comments missing substring %q", issueNumber, contains)
	})
}

func listSandboxIssueComments(tb testing.TB, sb sandboxConfig, issueNumber int64) []depGateSandboxComment {
	tb.Helper()
	var comments []depGateSandboxComment
	runSandboxJSON(tb, "", sb.CmdEnv, &comments, "gh", "api", fmt.Sprintf("repos/%s/issues/%d/comments", sb.Repo, issueNumber))
	return comments
}

func getSandboxIssueView(tb testing.TB, sb sandboxConfig, issueNumber int64) depGateSandboxIssueView {
	tb.Helper()
	var issue depGateSandboxIssueView
	runSandboxJSON(tb, "", sb.CmdEnv, &issue, "gh", "api", fmt.Sprintf("repos/%s/issues/%d", sb.Repo, issueNumber))
	return issue
}

func sandboxLabelNames(labels []struct {
	Name string `json:"name"`
}) []string {
	out := make([]string, 0, len(labels))
	for _, label := range labels {
		out = append(out, label.Name)
	}
	return out
}
