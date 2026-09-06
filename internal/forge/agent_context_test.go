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
