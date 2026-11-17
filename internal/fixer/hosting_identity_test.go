package fixer

import (
	"context"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/hostingidentity"
)

func TestHostingIdentityFixerValidationUsesPrivateLocalEnvironment(t *testing.T) {
	t.Setenv("GH_TOKEN", "personal")
	t.Setenv("OTHER_BOT_TOKEN", "other-bot")
	t.Setenv("LOOPER_TRUSTED_REVIEW_SOCK", "/privileged.sock")
	cfg := config.Config{Identities: map[string]config.HostingIdentityConfig{"other": {TokenEnv: "OTHER_BOT_TOKEN"}}}
	ctx, err := hostingidentity.BindResolved(context.Background(), config.ResolvedHostingIdentity{Name: "fixer", Definition: config.HostingIdentityConfig{Kind: config.HostingIdentityGitHubApp, BaseURL: "https://github.com", AppID: 1, InstallationID: 2, PrivateKeyFile: "/missing.pem"}, Target: config.RepositoryIdentity{Kind: config.ProviderKindGitHub, BaseURL: "https://github.com", Repo: "acme/looper"}, ProjectID: "project", Role: "fixer"})
	if err != nil {
		t.Fatal(err)
	}
	runner := &Runner{customInstructions: cfg}
	command := `test -z "$GH_TOKEN$OTHER_BOT_TOKEN$LOOPER_TRUSTED_REVIEW_SOCK" && test -d "$GH_CONFIG_DIR" && test ! -f "$GH_CONFIG_DIR/hosts.yml" && test "$(git config protocol.allow)" = never && ! gh auth status && ! tea login list && ! git fetch origin`
	for i := 0; i < 2; i++ {
		result, err := runner.runValidation(ctx, ValidationInput{CWD: t.TempDir(), Commands: []string{command, command}})
		if err != nil || !result.Passed {
			t.Fatalf("validation/retry inherited daemon hosting access: %#v, %v", result, err)
		}
	}
}

func TestHostingIdentityFixerPromptPreservesStructuredReplies(t *testing.T) {
	items := []FixItem{{Type: "comment", ID: "comment-1", ThreadID: "thread-1", Summary: "Fix the regression"}}
	prompt, _ := buildFixerPrompt("project", config.Config{}, "acme/looper", 42, &checkpointDetail{State: "OPEN", HeadSHA: "abc123", BaseRefName: "main"}, items, true, config.DefaultDisclosureConfig(), "opencode", "", config.HostingIdentityGitHubApp)
	for _, want := range []string{`"$LOOPER_HOST_CLI" host api pulls/42`, "review_thread_replies", "OUT_OF_SCOPE", "observed", "__LOOPER_RESULT__"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("bot fixer prompt lost %q", want)
		}
	}
	for _, unwanted := range []string{"run `gh pr view", "gh pr diff", "gh api repos/"} {
		if strings.Contains(prompt, unwanted) {
			t.Errorf("bot fixer prompt instructs ambient authentication: %q", unwanted)
		}
	}
	native, _ := buildFixerPrompt("project", config.Config{}, "acme/looper", 42, &checkpointDetail{State: "OPEN", HeadSHA: "abc123"}, []FixItem{{Type: "comment", Source: NativeReviewCommentSource, ProviderCommentID: 17, ObservedFingerprint: "observed-v1"}}, true, config.DefaultDisclosureConfig(), "opencode", "", config.HostingIdentityForgejoToken)
	for _, want := range []string{"repair_results", "providerCommentId", "observedFingerprint", "fixed", "declined", "deferred"} {
		if !strings.Contains(native, want) {
			t.Errorf("Forgejo bot fixer lost structured repair field %q", want)
		}
	}
}

func TestHostingIdentityFixerPromptRetainsSeedDriftChecks(t *testing.T) {
	t.Parallel()
	for _, kind := range []config.HostingIdentityKind{config.HostingIdentityGitHubApp, config.HostingIdentityForgejoToken} {
		t.Run(string(kind), func(t *testing.T) {
			cfg := config.Config{}
			if kind == config.HostingIdentityForgejoToken {
				cfg.Providers = []config.ProviderConfig{{ID: "forgejo", Kind: config.ProviderKindForgejo, BaseURL: "https://forge.example"}}
				cfg.Projects = []config.ProjectRefConfig{{ID: "project", Provider: "forgejo", Repo: "acme/looper"}}
			}
			detail := &checkpointDetail{State: "OPEN", BaseRefName: "main", HeadRefName: "looper/feature", HeadSHA: "abc123", IsDraft: false}
			prompt, _ := buildFixerPrompt("project", cfg, "acme/looper", 42, detail, nil, true, config.DefaultDisclosureConfig(), "opencode", "", kind)
			for _, want := range []string{
				"Minimal PR seed (authoritative handoff fields",
				`"head_sha": "abc123"`, `"base_ref": "main"`, `"head_ref": "looper/feature"`,
				`"expected_state": "OPEN"`, `"expected_draft": false`,
				"host api pulls/42.", "host api pulls/42 --diff",
				"Before editing and again before final conclusions, fetch live PR metadata",
				"verify head.sha, base.ref and open/draft status against the seeded handoff",
				"Fail with structured auth/network/rate_limit/pr_drift error when access fails or the target changes",
				"Fetch the diff and read all PR conversation and reviews before editing",
				"__LOOPER_RESULT__",
			} {
				if !strings.Contains(prompt, want) {
					t.Errorf("bot fixer prompt lost seeded repair authority or required live read %q", want)
				}
			}
		})
	}
}
