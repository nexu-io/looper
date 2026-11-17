package projects

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
)

func projectHostingIdentityConfig(t *testing.T) config.Config {
	t.Helper()
	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Identities = map[string]config.HostingIdentityConfig{
		"worker-bot":   {Kind: config.HostingIdentityGitHubApp, AppID: 1, InstallationID: 2, PrivateKeyFile: "/missing/worker.pem"},
		"reviewer-bot": {Kind: config.HostingIdentityGitHubApp, AppID: 3, InstallationID: 4, PrivateKeyFile: "/missing/reviewer.pem"},
	}
	return cfg
}

func TestConfiguredHostingIdentitiesSurviveSQLiteCatalogAndGlobalReload(t *testing.T) {
	coordinator := openCoordinator(t)
	repos := storage.NewRepositories(coordinator.DB())
	cfg := projectHostingIdentityConfig(t)
	cfg.Projects = []config.ProjectRefConfig{{
		ID: "project", Name: "Project", RepoPath: t.TempDir(), Repo: "owner/repo", Identity: "worker-bot",
		Roles: &config.PartialRoleConfigs{Reviewer: &config.PartialReviewerRoleConfig{Identity: stringPointer("reviewer-bot")}},
	}}
	catalog := NewCatalog(cfg)
	service := &Service{DB: coordinator.DB(), Repos: repos, Config: cfg, ConfigSource: catalog, PublishProjects: catalog.Publish}
	if err := service.SyncConfigured(context.Background(), cfg, time.Now()); err != nil {
		t.Fatal(err)
	}
	record, err := repos.Projects.GetByID(context.Background(), "project")
	if err != nil || record == nil || metadataString(parseMetadata(record.MetadataJSON), "identity") != "worker-bot" {
		t.Fatalf("identity missing from SQLite: %#v, %v", record, err)
	}
	before := catalog.Snapshot()
	for role, name := range map[string]string{"worker": "worker-bot", "reviewer": "reviewer-bot"} {
		got, selected, err := config.ResolveHostingIdentity(before, "project", role)
		if err != nil || !selected || got.Name != name {
			t.Fatalf("materialized %s identity = (%#v, %v, %v)", role, got, selected, err)
		}
	}
	updated := config.CloneConfig(cfg)
	definition := updated.Identities["worker-bot"]
	definition.InstallationID = 20
	updated.Identities["worker-bot"] = definition
	updated.Projects = []config.ProjectRefConfig{{ID: "not-imported", Identity: "reviewer-bot"}}
	catalog.PublishGlobals(updated)
	after := catalog.Snapshot()
	oldBinding, _, err := config.ResolveHostingIdentity(before, "project", "worker")
	if err != nil {
		t.Fatal(err)
	}
	newBinding, _, err := config.ResolveHostingIdentity(after, "project", "worker")
	if err != nil {
		t.Fatal(err)
	}
	if oldBinding.Definition.InstallationID != 2 || newBinding.Definition.InstallationID != 20 || len(after.Projects) != 1 || after.Projects[0].ID != "project" {
		t.Fatalf("global publication changed project authority or old snapshot: old=%#v new=%#v projects=%#v", oldBinding, newBinding, after.Projects)
	}
}

func TestProjectAPIHostingIdentityRoundTripAndFailedUpdateIsAtomic(t *testing.T) {
	coordinator := openCoordinator(t)
	repos := storage.NewRepositories(coordinator.DB())
	cfg := projectHostingIdentityConfig(t)
	catalog := NewCatalog(cfg)
	service := &Service{DB: coordinator.DB(), Repos: repos, ConfigSource: catalog, PublishProjects: catalog.Publish}
	input := AddInput{
		ID: "project", IDSource: "derived", Name: "Project", RepoPath: t.TempDir(), Repo: stringPointer("owner/repo"),
		Identity: stringPointer("worker-bot"),
	}
	if _, err := service.AddProject(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	input.Identity = nil
	if _, err := service.AddProject(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	for role, want := range map[string]string{"worker": "worker-bot", "reviewer": "worker-bot"} {
		got, selected, err := config.ResolveHostingIdentity(catalog.Snapshot(), "project", role)
		if err != nil || !selected || got.Name != want {
			t.Fatalf("re-add lost %s identity: (%#v, %v, %v)", role, got, selected, err)
		}
	}
	input.Identity = stringPointer("not-defined")
	if _, err := service.AddProject(context.Background(), input); err == nil {
		t.Fatal("invalid identity update succeeded")
	} else {
		var validation ProjectValidationError
		if !errors.As(err, &validation) {
			t.Fatalf("error = %v, want project validation error", err)
		}
	}
	record, err := repos.Projects.GetByID(context.Background(), "project")
	if err != nil || record == nil || metadataString(parseMetadata(record.MetadataJSON), "identity") != "worker-bot" || catalog.Snapshot().Projects[0].Identity != "worker-bot" {
		t.Fatalf("failed update mutated SQLite or catalog: %#v, %v", record, err)
	}
	input.Identity = stringPointer("")
	if _, err := service.AddProject(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"worker", "reviewer"} {
		if _, selected, err := config.ResolveHostingIdentity(catalog.Snapshot(), "project", role); err != nil || selected {
			t.Fatalf("explicit clearing did not restore inheritance: selected=%v, err=%v", selected, err)
		}
	}
}

func TestMaterializeCatalogRejectsIdentityRemovedFromLiveGlobals(t *testing.T) {
	cfg := projectHostingIdentityConfig(t)
	metadata := `{"repo":"owner/repo","identity":"worker-bot","roles":{"reviewer":{"identity":"reviewer-bot"}}}`
	records := []storage.ProjectRecord{{ID: "project", MetadataJSON: &metadata}}
	if _, err := MaterializeCatalog(cfg, records); err != nil {
		t.Fatal(err)
	}
	delete(cfg.Identities, "reviewer-bot")
	if _, err := MaterializeCatalog(cfg, records); err == nil {
		t.Fatal("materialization accepted missing role identity")
	}
	metadata = `{"repo":"owner/repo","identity":123}`
	if _, err := MaterializeCatalog(cfg, records); err == nil {
		t.Fatal("malformed stored identity silently restored legacy authentication")
	}
}

func TestProjectReaddPreservesStoredRoleIdentityOverrides(t *testing.T) {
	coordinator := openCoordinator(t)
	repos := storage.NewRepositories(coordinator.DB())
	cfg := projectHostingIdentityConfig(t)
	metadata := `{"source":"api","repo":"owner/repo","identity":"worker-bot","roles":{"planner":{"identity":"reviewer-bot"},"reviewer":{"identity":""},"worker":{"identity":"reviewer-bot"},"fixer":{"identity":"reviewer-bot"},"coordinator":{"identity":"reviewer-bot"}}}`
	record := storage.ProjectRecord{ID: "project", Name: "Project", RepoPath: t.TempDir(), MetadataJSON: &metadata, CreatedAt: time.Now().UTC().Format(time.RFC3339), UpdatedAt: time.Now().UTC().Format(time.RFC3339)}
	if err := repos.Projects.Upsert(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	catalog := NewCatalog(cfg)
	service := &Service{DB: coordinator.DB(), Repos: repos, ConfigSource: catalog, PublishProjects: catalog.Publish}
	if _, err := service.AddProject(context.Background(), AddInput{ID: "project", IDSource: "derived", Name: "Project", RepoPath: record.RepoPath, Repo: stringPointer("owner/repo")}); err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"planner", "reviewer", "worker", "fixer", "coordinator"} {
		want := "reviewer-bot"
		if role == "reviewer" {
			want = "worker-bot"
		}
		got, selected, err := config.ResolveHostingIdentity(catalog.Snapshot(), "project", role)
		if err != nil || !selected || got.Name != want {
			t.Fatalf("re-add lost stored %s identity: (%#v, %v, %v)", role, got, selected, err)
		}
	}
}

func TestSQLiteOnlyForgejoHostingIdentityRestartAndAtomicCoverageValidation(t *testing.T) {
	cwd := t.TempDir()
	configPath := filepath.Join(cwd, "config.json")
	raw := `{"notifications":{"osascript":{"enabled":false}},"providers":[{"id":"forgejo","kind":"forgejo","baseUrl":"https://code.example"}],"identities":{"bot":{"kind":"forgejo-token","baseUrl":"https://code.example","tokenEnv":"UNSET_FORGEJO_BOT_TOKEN"}},"projects":[]}`
	if err := os.WriteFile(configPath, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	load := func() config.Config {
		t.Helper()
		loaded, err := config.LoadFile(config.LoadFileOptions{
			CWD: cwd, ConfigPath: configPath,
			LookupEnv: func(string) (string, bool) { return "", false },
			LookPath:  func(name string) (string, error) { return "/detected/" + name, nil },
		})
		if err != nil {
			t.Fatalf("LoadFile rejected provider for SQLite-only bot project: %v", err)
		}
		if len(loaded.Config.Projects) != 0 || loaded.Config.Providers[0].TokenEnv != nil || loaded.Config.Providers[0].TeaLogin != nil {
			t.Fatalf("load invented project or legacy auth: %#v", loaded.Config)
		}
		return loaded.Config
	}
	cfg := load()
	coordinator := openCoordinator(t)
	repos := storage.NewRepositories(coordinator.DB())
	catalog := NewCatalog(cfg)
	service := &Service{DB: coordinator.DB(), Repos: repos, ConfigSource: catalog, PublishProjects: catalog.Publish}
	input := AddInput{ID: "project", IDSource: "derived", Name: "Project", RepoPath: cwd, Repo: stringPointer("owner/project"), Provider: stringPointer("forgejo"), Identity: stringPointer("bot")}
	if _, err := service.AddProject(context.Background(), input); err != nil {
		t.Fatalf("create bot-only project: %v", err)
	}

	// Restart reloads a file with no projects and materializes the persisted bot
	// selection before execution, without reading token values or tea state.
	restarted := load()
	records, err := repos.Projects.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	materialized, err := MaterializeCatalog(restarted, records)
	if err != nil {
		t.Fatalf("restart lost SQLite bot auth coverage: %v", err)
	}
	catalog = NewCatalog(restarted)
	catalog.Publish(materialized)
	for _, role := range []string{"planner", "reviewer", "worker", "fixer"} {
		binding, selected, err := config.ResolveHostingIdentity(catalog.Snapshot(), "project", role)
		if err != nil || !selected || binding.Name != "bot" {
			t.Fatalf("restart %s binding = (%#v, %v, %v)", role, binding, selected, err)
		}
	}
	service.ConfigSource = catalog
	publishes := 0
	service.PublishProjects = func(projects []config.ProjectRefConfig) {
		publishes++
		catalog.Publish(projects)
	}
	uncovered := AddInput{ID: "uncovered", Name: "Uncovered", RepoPath: cwd, Repo: stringPointer("owner/uncovered"), Provider: stringPointer("forgejo")}
	if _, err := service.AddProject(context.Background(), uncovered); err == nil || !strings.Contains(err.Error(), "providers[0].auth") {
		t.Fatalf("provider without legacy credentials accepted an uncovered role: %v", err)
	}
	if record, err := repos.Projects.GetByID(context.Background(), "uncovered"); err != nil || record != nil {
		t.Fatalf("failed add mutated SQLite: %#v, %v", record, err)
	}
	input.Identity = stringPointer("")
	if _, err := service.AddProject(context.Background(), input); err == nil || !strings.Contains(err.Error(), "providers[0].auth") {
		t.Fatalf("clearing final bot selection succeeded without legacy credentials: %v", err)
	}
	record, err := repos.Projects.GetByID(context.Background(), "project")
	if err != nil || record == nil || record.MetadataJSON == nil || *record.MetadataJSON != *records[0].MetadataJSON {
		t.Fatalf("failed clearing mutated SQLite: %#v, %v", record, err)
	}
	if publishes != 0 || len(catalog.Snapshot().Projects) != 1 || catalog.Snapshot().Projects[0].Identity != "bot" {
		t.Fatalf("failed identity changes published an invalid catalog: publishes=%d, projects=%#v", publishes, catalog.Snapshot().Projects)
	}
}

func TestForgejoLegacyAuthStillAllowsProjectsWithoutHostingIdentity(t *testing.T) {
	for _, provider := range []config.ProviderConfig{
		{ID: "forgejo", Kind: config.ProviderKindForgejo, BaseURL: "https://code.example", TokenEnv: stringPointer("UNSET_LEGACY_TOKEN")},
		{ID: "forgejo", Kind: config.ProviderKindForgejo, BaseURL: "https://code.example", Auth: config.ProviderAuthTea, TeaLogin: stringPointer("legacy-login"), TeaPath: stringPointer("/missing/tea")},
	} {
		t.Run(string(config.EffectiveProviderAuth(provider)), func(t *testing.T) {
			cfg := projectHostingIdentityConfig(t)
			cfg.Identities = nil
			cfg.Providers = []config.ProviderConfig{provider}
			coordinator := openCoordinator(t)
			repos := storage.NewRepositories(coordinator.DB())
			catalog := NewCatalog(cfg)
			service := &Service{DB: coordinator.DB(), Repos: repos, ConfigSource: catalog, PublishProjects: catalog.Publish}
			if _, err := service.AddProject(context.Background(), AddInput{ID: "legacy", Name: "Legacy", RepoPath: t.TempDir(), Repo: stringPointer("owner/legacy"), Provider: stringPointer("forgejo")}); err != nil {
				t.Fatalf("legacy provider reference was probed or rejected: %v", err)
			}
			if _, selected, err := config.ResolveHostingIdentity(catalog.Snapshot(), "legacy", "worker"); err != nil || selected {
				t.Fatalf("legacy authentication changed: selected=%v, err=%v", selected, err)
			}
		})
	}
}
