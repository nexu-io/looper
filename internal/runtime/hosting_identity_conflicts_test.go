package runtime

import (
	"context"
	"testing"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/hostingidentity"
)

func TestHostingIdentityForgejoConflictInspectionUsesLocalGit(t *testing.T) {
	t.Parallel()
	cwd, base, conflict, clean := forgejoConflictRepo(t)
	manager := hostingidentity.NewManager(hostingidentity.Options{LookupEnv: func(string) (string, bool) {
		t.Error("local merge inspection attempted to acquire a hosting credential")
		return "", false
	}})
	ctx, err := hostingidentity.BindResolved(hostingidentity.WithManager(context.Background(), manager), config.ResolvedHostingIdentity{
		Name: "conflict-bot", ProjectID: "project", Role: "fixer",
		Definition: config.HostingIdentityConfig{Kind: config.HostingIdentityForgejoToken, BaseURL: "https://forge.example", TokenEnv: "UNAVAILABLE_CONFLICT_BOT_TOKEN"},
		Target:     config.RepositoryIdentity{Kind: config.ProviderKindForgejo, BaseURL: "https://forge.example", Repo: "acme/looper"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, head string
		want       bool
	}{{"conflicted", conflict, true}, {"clean", clean, false}} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := forgejoHasMergeConflicts(ctx, "git", cwd, base, tc.head)
			if err != nil || got != tc.want {
				t.Fatalf("bot-bound merge inspection = %v, %v; want %v, nil", got, err, tc.want)
			}
		})
	}
}
