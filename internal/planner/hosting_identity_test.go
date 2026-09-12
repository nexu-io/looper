package planner

import (
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
)

func TestHostingIdentityPlannerPromptReadsItsIssueBeforePRExists(t *testing.T) {
	t.Parallel()
	for _, kind := range []config.HostingIdentityKind{config.HostingIdentityGitHubApp, config.HostingIdentityForgejoToken} {
		t.Run(string(kind), func(t *testing.T) {
			prompt, _ := buildPlannerPrompt(
				storage.ProjectRecord{ID: "project", RepoPath: t.TempDir()}, config.Config{},
				&checkpointIssue{Repo: "acme/looper", IssueNumber: 42, Title: "Plan the feature", SpecPath: "docs/feature.md"},
				&checkpointWorktree{Branch: "looper/plan-42", BaseBranch: "main"},
				true, config.DefaultDisclosureConfig(), "opencode", "", kind,
			)
			for _, want := range []string{
				`"$LOOPER_HOST_CLI" host api issues/42.`,
				`"$LOOPER_HOST_CLI" host api issues/42/comments --paginate`,
				"Create or update the spec at docs/feature.md", "Commit the spec changes on the current branch",
				"Looper publishes local commits", "__LOOPER_RESULT__",
			} {
				if !strings.Contains(prompt, want) {
					t.Errorf("planner bot prompt lost %q", want)
				}
			}
			for _, unwanted := range []string{"host api pulls/", "host threads ", "host thread ", "issues/0", "seeded", "Fail on drift", "Before acting"} {
				if strings.Contains(prompt, unwanted) {
					t.Errorf("planner without a PR received unavailable PR context or seed policy %q", unwanted)
				}
			}
		})
	}
}
