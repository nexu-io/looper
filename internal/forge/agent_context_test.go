package forge

import (
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/config"
)

func TestForgejoAgentContextUsesConfiguredTransportWithoutCredentials(t *testing.T) {
	t.Setenv("FORGEJO_TEST_SECRET", "do-not-copy-this-token")
	for _, tea := range []bool{false, true} {
		provider := config.ProviderConfig{ID: "forge", Kind: config.ProviderKindForgejo, BaseURL: "https://code.example/forge", Auth: config.ProviderAuthTokenEnv, TokenEnv: stringPtr("FORGEJO_TEST_SECRET")}
		if tea {
			provider.Auth = config.ProviderAuthTea
			provider.TeaLogin = stringPtr("team-main")
			provider.TeaPath = stringPtr("/opt/Tea Tools/tea")
		}
		cfg := config.Config{Providers: []config.ProviderConfig{provider}, Projects: []config.ProjectRefConfig{{ID: "project", Provider: "forge", Repo: "core/looper"}}}
		got := ForgejoAgentContext(cfg, "project", "core/looper", 42)
		for _, required := range []string{"https://code.example/forge/core/looper/pulls/42", "/repos/core/looper/pulls/42/reviews/<review_id>/comments", "run.id", "bare array", "/actions/jobs/<job_id>/logs", "page=1&limit=50"} {
			if !strings.Contains(got, required) {
				t.Errorf("context missing %q: %s", required, got)
			}
		}
		if strings.Contains(got, "do-not-copy-this-token") || strings.Contains(got, "gh pr") {
			t.Errorf("context leaks credentials or GitHub command: %s", got)
		}
		if tea && !strings.Contains(got, "'/opt/Tea Tools/tea' api --login 'team-main' -i") {
			t.Errorf("configured tea command missing: %s", got)
		}
		if !tea && !strings.Contains(got, "FORGEJO_TEST_SECRET") {
			t.Errorf("token variable reference missing: %s", got)
		}
		if got := ForgejoAgentContext(cfg, "unknown", "core/looper", 42); got != "" {
			t.Errorf("unknown project context = %q", got)
		}
	}
}

func TestHostingAgentContextUsesAvailableRoleTarget(t *testing.T) {
	for _, kind := range []config.HostingIdentityKind{config.HostingIdentityGitHubApp, config.HostingIdentityForgejoToken} {
		t.Run(string(kind), func(t *testing.T) {
			for _, tc := range []struct {
				name         string
				role         string
				targetNumber int64
				wantPR       bool
			}{
				{name: "planner issue", role: "planner", targetNumber: 42},
				{name: "planner without target", role: "planner"},
				{name: "worker before PR creation", role: "worker"},
				{name: "worker existing PR", role: "worker", targetNumber: 42, wantPR: true},
				{name: "reviewer existing PR", role: "reviewer", targetNumber: 42, wantPR: true},
				{name: "fixer existing PR", role: "fixer", targetNumber: 42, wantPR: true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					got := HostingAgentContext(kind, tc.role, "core/looper", tc.targetNumber)
					for _, required := range []string{
						"role=" + tc.role + " repository=core/looper",
						`"$LOOPER_HOST_CLI" host whoami`, "Authentication is daemon-owned", "repository-relative GET paths",
						"all supported pages", "A failed read or response-limit error is not an empty or clean result",
						`"$LOOPER_HOST_CLI" host git fetch <ref>`, "Looper publishes local commits", "__LOOPER_RESULT__",
					} {
						if !strings.Contains(got, required) {
							t.Errorf("shared context missing %q: %s", required, got)
						}
					}
					for _, unwanted := range []string{"seeded", "Fail on drift", "Before acting", "pulls/<number>", "pulls/0", "issues/0"} {
						if strings.Contains(got, unwanted) {
							t.Errorf("shared context includes unavailable target or role policy %q: %s", unwanted, got)
						}
					}
					if tc.wantPR {
						for _, required := range []string{"host api pulls/42.", "host api pulls/42 --diff", "host api issues/42/comments --paginate", "host api pulls/42/reviews --paginate"} {
							if !strings.Contains(got, required) {
								t.Errorf("PR context missing %q: %s", required, got)
							}
						}
						if kind == config.HostingIdentityForgejoToken && !strings.Contains(got, "host api pulls/42/reviews/<review_id>/comments --paginate") {
							t.Errorf("Forgejo native reads missing: %s", got)
						}
						if kind == config.HostingIdentityGitHubApp && (!strings.Contains(got, "host api pulls/42/comments --paginate") || !strings.Contains(got, "host threads 42")) {
							t.Errorf("GitHub inline/thread reads missing: %s", got)
						}
					} else {
						for _, unwanted := range []string{"host api pulls/", "host threads ", "host thread "} {
							if strings.Contains(got, unwanted) {
								t.Errorf("context without PR includes %q: %s", unwanted, got)
							}
						}
					}
					if tc.role == "planner" && tc.targetNumber > 0 {
						for _, required := range []string{"host api issues/42.", "host api issues/42/comments --paginate"} {
							if !strings.Contains(got, required) {
								t.Errorf("planner issue context missing %q: %s", required, got)
							}
						}
					}
					wantThreadFingerprint := kind == config.HostingIdentityGitHubApp && tc.role == "fixer" && tc.wantPR
					if strings.Contains(got, "threadCommentsObserved") != wantThreadFingerprint {
						t.Errorf("thread fingerprint policy does not match role: %s", got)
					}
				})
			}
		})
	}
}
