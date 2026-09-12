package reviewer

import (
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/config"
)

func TestHostingIdentityReviewerPromptUsesBrokerForEveryRead(t *testing.T) {
	prompt, _ := buildReviewPromptWithInstructions("project", config.Config{}, "acme/looper", 42, reviewerCheckpoint{Snapshot: &checkpointSnapshot{HeadSHA: "abc123"}}, "run", "reviewer:head", config.ReviewerReviewEventsConfig{Clean: config.ReviewerReviewEventComment, Blocking: config.ReviewerReviewEventComment}, false, true, "", config.ReviewerScopeFullPR, config.DefaultDisclosureConfig(), "opencode", "", "/opt/looper/bin/looper", false, false, "", config.HostingIdentityGitHubApp)
	for _, want := range []string{`"$LOOPER_HOST_CLI" host api pulls/42`, `host api pulls/42/reviews --paginate`, `host api pulls/42 --diff`, `host threads 42`, `review submit`, `findings`, `scopeEvidence`} {
		if !strings.Contains(prompt, want) {
			t.Errorf("bot reviewer prompt lost %q", want)
		}
	}
	for _, unwanted := range []string{"use `gh api`", "use `gh` to confirm", "fetched with `gh pr diff`", "fetched through `gh`", "run `gh pr view"} {
		if strings.Contains(prompt, unwanted) {
			t.Errorf("bot reviewer prompt instructs ambient authentication: %q", unwanted)
		}
	}
}

func TestHostingIdentityReviewerPromptRetainsSeedDriftChecks(t *testing.T) {
	t.Parallel()
	for _, kind := range []config.HostingIdentityKind{config.HostingIdentityGitHubApp, config.HostingIdentityForgejoToken} {
		t.Run(string(kind), func(t *testing.T) {
			cfg := config.Config{}
			if kind == config.HostingIdentityForgejoToken {
				cfg.Providers = []config.ProviderConfig{{ID: "forgejo", Kind: config.ProviderKindForgejo, BaseURL: "https://forge.example"}}
				cfg.Projects = []config.ProjectRefConfig{{ID: "project", Provider: "forgejo", Repo: "acme/looper"}}
			}
			checkpoint := reviewerCheckpoint{
				Detail:   &checkpointDetail{State: "OPEN", BaseRefName: "main", HeadRefName: "looper/feature", IsDraft: false},
				Snapshot: &checkpointSnapshot{HeadSHA: "abc123"},
			}
			prompt, _ := buildReviewPromptWithInstructions("project", cfg, "acme/looper", 42, checkpoint, "run", "reviewer:head", config.ReviewerReviewEventsConfig{Clean: config.ReviewerReviewEventComment, Blocking: config.ReviewerReviewEventComment}, false, true, "", config.ReviewerScopeFullPR, config.DefaultDisclosureConfig(), "opencode", "", "/opt/looper/bin/looper", false, false, "", kind)
			for _, want := range []string{
				"Minimal PR seed (authoritative handoff fields",
				`"head_sha": "abc123"`, `"base_ref": "main"`, `"head_ref": "looper/feature"`,
				`"expected_state": "OPEN"`, `"expected_draft": false`,
				"host api pulls/42.", "host api pulls/42 --diff",
				"Before reviewing and again before conclusions or publication, read live PR metadata",
				"verify seeded head/base/state/draft and stop on drift or access failures",
				"Fetch the diff and read all PR conversation and reviews before reviewing",
				"review submit", "__LOOPER_RESULT__",
			} {
				if !strings.Contains(prompt, want) {
					t.Errorf("bot reviewer prompt lost seeded review authority or required live read %q", want)
				}
			}
		})
	}
}
