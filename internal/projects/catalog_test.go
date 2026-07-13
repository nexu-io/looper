package projects

import (
	"strings"
	"sync"
	"testing"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
)

var _ ConfigSource = (*Catalog)(nil)

func TestCatalogPublishesFullConfigAtomically(t *testing.T) {
	t.Parallel()

	vendor := config.AgentVendorCodex
	global := config.Config{
		Agent:     config.AgentConfig{Vendor: &vendor, Params: map[string]any{"nested": map[string]any{"value": "original"}}},
		Providers: []config.ProviderConfig{{ID: "forgejo-main", Kind: config.ProviderKindForgejo}},
		Projects:  []config.ProjectRefConfig{{ID: "import-input"}},
	}
	catalog := NewCatalog(global)
	catalog.Publish([]config.ProjectRefConfig{{ID: "database", Repo: "core/database", Roles: &config.PartialRoleConfigs{}}})

	got := catalog.Snapshot()
	if len(got.Projects) != 1 || got.Projects[0].ID != "database" {
		t.Fatalf("Snapshot().Projects = %#v, want published database project", got.Projects)
	}
	if len(got.Providers) != 1 || got.Providers[0].ID != "forgejo-main" || got.Agent.Vendor == nil || *got.Agent.Vendor != vendor {
		t.Fatalf("Snapshot() lost global config: %#v", got)
	}
}

func TestCatalogDoesNotRetainOrReturnMutableAliases(t *testing.T) {
	t.Parallel()

	roles := &config.PartialRoleConfigs{}
	projects := []config.ProjectRefConfig{{ID: "database", Repo: "core/database", Roles: roles}}
	global := config.Config{
		Agent:     config.AgentConfig{Params: map[string]any{"nested": map[string]any{"value": "original"}}, Env: map[string]string{"TOKEN": "env"}},
		Providers: []config.ProviderConfig{{ID: "forgejo-main", Kind: config.ProviderKindForgejo}},
	}
	catalog := NewCatalog(global)
	catalog.Publish(projects)

	projects[0].ID = "caller-mutated"
	projects[0].Roles = nil
	global.Providers[0].ID = "caller-mutated"
	global.Agent.Env["TOKEN"] = "caller-mutated"

	first := catalog.Snapshot()
	first.Projects[0].ID = "snapshot-mutated"
	first.Projects[0].Roles = nil
	first.Providers[0].ID = "snapshot-mutated"
	first.Agent.Env["TOKEN"] = "snapshot-mutated"
	first.Agent.Params["nested"].(map[string]any)["value"] = "snapshot-mutated"

	got := catalog.Snapshot()
	if got.Projects[0].ID != "database" || got.Projects[0].Roles == nil {
		t.Fatalf("published projects were mutated through an alias: %#v", got.Projects)
	}
	if got.Providers[0].ID != "forgejo-main" || got.Agent.Env["TOKEN"] != "env" {
		t.Fatalf("published globals were mutated through an alias: %#v", got)
	}
	nested := got.Agent.Params["nested"].(map[string]any)
	if nested["value"] != "original" {
		t.Fatalf("Snapshot().Agent.Params nested value = %v, want original", nested["value"])
	}
}

func TestCatalogConcurrentSnapshotsObserveWholePublications(t *testing.T) {
	t.Parallel()

	catalog := NewCatalog(config.Config{})
	catalog.Publish([]config.ProjectRefConfig{{ID: "a", Name: "a"}})

	const iterations = 200
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			if i%2 == 0 {
				catalog.Publish([]config.ProjectRefConfig{{ID: "a", Name: "a"}})
			} else {
				catalog.Publish([]config.ProjectRefConfig{{ID: "b", Name: "b"}, {ID: "b2", Name: "b2"}})
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			snapshot := catalog.Snapshot()
			switch len(snapshot.Projects) {
			case 1:
				if snapshot.Projects[0].ID != "a" || snapshot.Projects[0].Name != "a" {
					t.Errorf("observed torn a publication: %#v", snapshot.Projects)
					return
				}
			case 2:
				if snapshot.Projects[0].ID != "b" || snapshot.Projects[0].Name != "b" || snapshot.Projects[1].ID != "b2" || snapshot.Projects[1].Name != "b2" {
					t.Errorf("observed torn b publication: %#v", snapshot.Projects)
					return
				}
			default:
				t.Errorf("observed unexpected publication: %#v", snapshot.Projects)
				return
			}
		}
	}()
	wg.Wait()
}

func TestMaterializeCatalogUsesRecordsAsProjectAuthority(t *testing.T) {
	t.Parallel()

	baseBranch := "main"
	metadata := `{"network":{"mode":"routed"},"provider":"forgejo-main","repo":"core/odcrew","source":"config","worktreeRoot":"/tmp/worktrees"}`
	archivedMetadata := `{"repo":"acme/removed","source":"config"}`
	imported := config.Config{Providers: []config.ProviderConfig{{ID: "forgejo-main", Kind: config.ProviderKindForgejo}}, Projects: []config.ProjectRefConfig{
		{ID: "odcrew", Name: "stale name", RepoPath: "/stale", Provider: "stale-provider", Repo: "stale/repo"},
		{ID: "config-only", Name: "must not appear", RepoPath: "/config-only"},
	}}

	got, err := MaterializeCatalog(imported, []storage.ProjectRecord{
		{ID: "odcrew", Name: "ODCrew", RepoPath: "/repos/odcrew", BaseBranch: &baseBranch, MetadataJSON: &metadata},
		{ID: "removed", Name: "Removed", RepoPath: "/repos/removed", Archived: true, MetadataJSON: &archivedMetadata},
	})
	if err != nil {
		t.Fatalf("MaterializeCatalog() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(MaterializeCatalog()) = %d, want 1", len(got))
	}
	project := got[0]
	if project.ID != "odcrew" || project.Name != "ODCrew" || project.RepoPath != "/repos/odcrew" {
		t.Fatalf("project identity = %#v, want database record", project)
	}
	if project.Provider != "forgejo-main" || project.Repo != "core/odcrew" {
		t.Fatalf("project binding = (%q, %q), want stored binding", project.Provider, project.Repo)
	}
	if project.Network.Mode != config.NetworkModeRouted {
		t.Fatalf("project policy network mode = %q, want imported routed policy", project.Network.Mode)
	}
}

func TestMaterializeCatalogDoesNotApplyConfigPolicyToAPIRecord(t *testing.T) {
	t.Parallel()

	metadata := `{"repo":"acme/api","source":"api"}`
	imported := config.Config{Projects: []config.ProjectRefConfig{{
		ID: "api", Network: config.ProjectNetworkConfig{Mode: config.NetworkModeRouted},
	}}}
	got, err := MaterializeCatalog(imported, []storage.ProjectRecord{{ID: "api", Name: "API", RepoPath: "/repos/api", MetadataJSON: &metadata}})
	if err != nil {
		t.Fatalf("MaterializeCatalog() error = %v", err)
	}
	if len(got) != 1 || got[0].Network.Mode != "" {
		t.Fatalf("MaterializeCatalog() = %#v, want record without unstored imported policy", got)
	}
}

func TestMaterializeCatalogRejectsUnknownProvider(t *testing.T) {
	t.Parallel()

	metadata := `{"provider":"removed","repo":"core/odcrew","source":"api"}`
	_, err := MaterializeCatalog(config.Config{}, []storage.ProjectRecord{{ID: "odcrew", MetadataJSON: &metadata}})
	if err == nil || !strings.Contains(err.Error(), `unknown provider "removed"`) {
		t.Fatalf("MaterializeCatalog() error = %v, want unknown provider", err)
	}
}

func TestMaterializeCatalogRejectsDuplicateActiveRepoBindings(t *testing.T) {
	t.Parallel()

	githubMetadata := `{"repo":"nexu-io/looper","source":"config"}`
	forgejoMetadata := `{"provider":"forgejo-main","repo":"NEXU-IO/LOOPER","source":"api"}`
	global := config.Config{Providers: []config.ProviderConfig{{ID: "forgejo-main", Kind: config.ProviderKindForgejo}}}

	_, err := MaterializeCatalog(global, []storage.ProjectRecord{
		{ID: "github", MetadataJSON: &githubMetadata},
		{ID: "forgejo", MetadataJSON: &forgejoMetadata},
	})
	if err == nil || !strings.Contains(err.Error(), `repo "NEXU-IO/LOOPER" duplicates active project "github"`) {
		t.Fatalf("MaterializeCatalog() error = %v, want duplicate active repo binding", err)
	}
}

func TestMaterializeCatalogAppliesAndValidatesForgejoRoleProfile(t *testing.T) {
	t.Parallel()

	global, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	global.Providers = []config.ProviderConfig{{ID: "forgejo-main", Kind: config.ProviderKindForgejo}}
	global.Roles.Coordinator.Enabled = true
	global.Roles.Coordinator.Dependencies.Enabled = true
	metadata := `{"provider":"forgejo-main","repo":"core/odcrew","source":"api"}`
	got, err := MaterializeCatalog(global, []storage.ProjectRecord{{ID: "odcrew", MetadataJSON: &metadata}})
	if err != nil {
		t.Fatalf("MaterializeCatalog() error = %v", err)
	}
	global.Projects = got
	triggers := config.ProjectRoleConfigs(global, "odcrew").Reviewer.Discovery.Triggers
	if triggers.RequireReviewRequest || len(triggers.Labels) != 1 || triggers.Labels[0] != "looper:review" {
		t.Fatalf("materialized reviewer triggers = %#v, want Forgejo label/comment-only profile", triggers)
	}
	coordinator := config.ProjectRoleConfigs(global, "odcrew").Coordinator
	if coordinator.Enabled || coordinator.Dependencies.Enabled {
		t.Fatalf("materialized coordinator = %#v, want Forgejo coordinator and dependency gates disabled", coordinator)
	}

	incompatibleMetadata := `{"provider":"forgejo-main","repo":"core/odcrew","roles":{"reviewer":{"discovery":{"triggers":{"requireReviewRequest":true}}}},"source":"api"}`
	_, err = MaterializeCatalog(global, []storage.ProjectRecord{{ID: "odcrew", MetadataJSON: &incompatibleMetadata}})
	if err == nil || !strings.Contains(err.Error(), "requireReviewRequest") {
		t.Fatalf("MaterializeCatalog() error = %v, want incompatible Forgejo role rejection", err)
	}
}

func TestConfiguredProjectMetadataRoundTripsRuntimePolicy(t *testing.T) {
	t.Parallel()

	project := config.ProjectRefConfig{
		ID:       "odcrew",
		Name:     "ODCrew",
		Provider: "forgejo-main",
		Repo:     "core/odcrew",
		RepoPath: "/repos/odcrew",
		Path:     "nested/path",
		Network:  config.ProjectNetworkConfig{Mode: config.NetworkModeRouted},
		Webhook:  config.ProjectWebhookConfig{Mode: config.WebhookModeTunnel},
		Roles:    &config.PartialRoleConfigs{},
	}
	repo := project.Repo
	metadata, err := buildProjectMetadataJSON(nil, project, &repo)
	if err != nil {
		t.Fatalf("buildProjectMetadataJSON() error = %v", err)
	}
	global := config.Config{Providers: []config.ProviderConfig{{ID: "forgejo-main", Kind: config.ProviderKindForgejo}}}
	got, err := MaterializeCatalog(global, []storage.ProjectRecord{{
		ID: project.ID, Name: project.Name, RepoPath: project.RepoPath, MetadataJSON: &metadata,
	}})
	if err != nil {
		t.Fatalf("MaterializeCatalog() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(MaterializeCatalog()) = %d, want 1", len(got))
	}
	materialized := got[0]
	if materialized.Provider != project.Provider || materialized.Repo != project.Repo || materialized.Path != project.Path {
		t.Fatalf("materialized binding = %#v, want %#v", materialized, project)
	}
	if materialized.Network.Mode != project.Network.Mode || materialized.Webhook.Mode != project.Webhook.Mode || materialized.Roles == nil {
		t.Fatalf("materialized policy = %#v, want persisted project policy", materialized)
	}
}
