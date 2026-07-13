package projects

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
)

func TestServiceSyncConfiguredIgnoresForgejoDetectionForGitHubDefault(t *testing.T) {
	t.Parallel()

	coordinator := openCoordinator(t)
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	repoPath := "/tmp/odcrew"
	baseBranch := "main"
	service := &Service{
		Repos: repos,
		Now:   func() time.Time { return now },
		DetectRepo: func(context.Context, string) (DetectedRepo, error) {
			return DetectedRepo{Repo: "core/odcrew", Provider: "forgejo-main"}, nil
		},
	}
	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Projects = []config.ProjectRefConfig{{ID: "odcrew", Name: "ODCrew", RepoPath: repoPath, BaseBranch: &baseBranch}}

	if err := service.SyncConfigured(context.Background(), cfg, now); err != nil {
		t.Fatalf("SyncConfigured() error = %v", err)
	}
	project, err := repos.Projects.GetByID(context.Background(), "odcrew")
	if err != nil {
		t.Fatalf("Projects.GetByID() error = %v", err)
	}
	if project == nil || metadataString(parseMetadata(project.MetadataJSON), "repo") != "" {
		t.Fatalf("stored project = %#v, want no GitHub repo inferred from Forgejo origin", project)
	}
}

func TestServiceAddProjectAllowsExplicitGitHubRepoOnDetectedForgejoOrigin(t *testing.T) {
	t.Parallel()

	coordinator := openCoordinator(t)
	repos := storage.NewRepositories(coordinator.DB())
	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	tokenEnv := "LOOPER_FORGEJO_TOKEN"
	cfg.Providers = []config.ProviderConfig{{ID: "forgejo-main", Kind: config.ProviderKindForgejo, BaseURL: "https://code.example.com", TokenEnv: &tokenEnv}}
	service := &Service{
		DB:     coordinator.DB(),
		Repos:  repos,
		Config: cfg,
		Now:    time.Now,
		DetectRepo: func(context.Context, string) (DetectedRepo, error) {
			return DetectedRepo{Repo: "forgejo/checkout", Provider: "forgejo-main"}, nil
		},
	}
	repo := "github-org/repo"

	result, err := service.AddProject(context.Background(), AddInput{
		ID: "project", Name: "Project", RepoPath: "/tmp/project", Repo: &repo,
	})
	if err != nil {
		t.Fatalf("AddProject() error = %v", err)
	}
	metadata := parseMetadata(result.Project.MetadataJSON)
	if got := metadataString(metadata, "repo"); got != repo {
		t.Fatalf("stored repo = %q, want explicit repo %q", got, repo)
	}
	if got := metadataString(metadata, "provider"); got != "" {
		t.Fatalf("stored provider = %q, want GitHub default", got)
	}
}

func TestServiceAddProjectRejectsDetectedRepoFromMismatchedProvider(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		detectedProvider string
		wantMessage      string
	}{
		{name: "GitHub origin", detectedProvider: "", wantMessage: "belongs to the GitHub default"},
		{name: "different Forgejo provider", detectedProvider: "forgejo-other", wantMessage: "belongs to forgejo-other"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			coordinator := openCoordinator(t)
			repos := storage.NewRepositories(coordinator.DB())
			cfg, err := config.DefaultConfig(t.TempDir())
			if err != nil {
				t.Fatalf("DefaultConfig() error = %v", err)
			}
			tokenEnv := "LOOPER_FORGEJO_TOKEN"
			cfg.Providers = []config.ProviderConfig{
				{ID: "forgejo-main", Kind: config.ProviderKindForgejo, BaseURL: "https://code.example.com", TokenEnv: &tokenEnv},
				{ID: "forgejo-other", Kind: config.ProviderKindForgejo, BaseURL: "https://other.example.com", TokenEnv: &tokenEnv},
			}
			published := false
			service := &Service{
				DB:     coordinator.DB(),
				Repos:  repos,
				Config: cfg,
				Now:    time.Now,
				DetectRepo: func(context.Context, string) (DetectedRepo, error) {
					return DetectedRepo{Repo: "owner/repo", Provider: tt.detectedProvider}, nil
				},
				PublishProjects: func([]config.ProjectRefConfig) { published = true },
			}
			provider := "forgejo-main"

			_, err = service.AddProject(context.Background(), AddInput{
				ID: "project", Name: "Project", RepoPath: "/tmp/project", Provider: &provider,
			})
			if err == nil || !strings.Contains(err.Error(), tt.wantMessage) {
				t.Fatalf("AddProject() error = %v, want %q", err, tt.wantMessage)
			}
			stored, getErr := repos.Projects.GetByID(context.Background(), "project")
			if getErr != nil {
				t.Fatalf("GetByID() error = %v", getErr)
			}
			if stored != nil || published {
				t.Fatalf("stored = %#v, published = %v; want rejection before persistence", stored, published)
			}
		})
	}
}

func TestServiceAddProjectRejectsDuplicateActiveRepoBeforeUpsert(t *testing.T) {
	t.Parallel()

	coordinator := openCoordinator(t)
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	nowISO := now.UTC().Format(time.RFC3339Nano)
	metadata := `{"repo":"nexu-io/looper","source":"config"}`
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID: "github", Name: "GitHub", RepoPath: "/tmp/github", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	tokenEnv := "LOOPER_FORGEJO_TOKEN"
	cfg.Providers = []config.ProviderConfig{{ID: "forgejo-main", Kind: config.ProviderKindForgejo, BaseURL: "https://code.example.com", TokenEnv: &tokenEnv}}
	published := false
	service := &Service{
		Repos:  repos,
		Config: cfg,
		Now:    func() time.Time { return now },
		PublishProjects: func([]config.ProjectRefConfig) {
			published = true
		},
	}
	repo := "NEXU-IO/LOOPER"
	provider := "forgejo-main"
	_, err = service.AddProject(context.Background(), AddInput{
		ID: "forgejo", Name: "Forgejo", RepoPath: "/tmp/forgejo", Repo: &repo, Provider: &provider,
	})
	if err == nil || !strings.Contains(err.Error(), `duplicates active project "github"`) {
		t.Fatalf("AddProject() error = %v, want duplicate active repo binding", err)
	}
	stored, getErr := repos.Projects.GetByID(context.Background(), "forgejo")
	if getErr != nil {
		t.Fatalf("Projects.GetByID() error = %v", getErr)
	}
	if stored != nil || published {
		t.Fatalf("stored = %#v, published = %v; want rejection before upsert and publish", stored, published)
	}
}

func TestServiceAddProjectClearsForgejoRolesWhenProviderIsRemoved(t *testing.T) {
	t.Parallel()

	coordinator := openCoordinator(t)
	repos := storage.NewRepositories(coordinator.DB())
	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	tokenEnv := "LOOPER_FORGEJO_TOKEN"
	cfg.Providers = []config.ProviderConfig{{ID: "forgejo-main", Kind: config.ProviderKindForgejo, BaseURL: "https://code.example.com", TokenEnv: &tokenEnv}}
	catalog := NewCatalog(cfg)
	service := &Service{DB: coordinator.DB(), Repos: repos, Config: cfg, ConfigSource: catalog, Now: time.Now, PublishProjects: catalog.Publish}
	repo := "core/odcrew"
	provider := "forgejo-main"
	input := AddInput{ID: "odcrew", IDSource: "derived", Name: "ODCrew", RepoPath: "/tmp/odcrew", Repo: &repo, Provider: &provider}
	if _, err := service.AddProject(context.Background(), input); err != nil {
		t.Fatalf("AddProject(Forgejo) error = %v", err)
	}

	input.Provider = nil
	if _, err := service.AddProject(context.Background(), input); err != nil {
		t.Fatalf("AddProject(GitHub) error = %v", err)
	}
	stored, err := repos.Projects.GetByID(context.Background(), "odcrew")
	if err != nil {
		t.Fatalf("GetByID() error = %v", err)
	}
	metadata := parseMetadata(stored.MetadataJSON)
	if _, ok := metadata["roles"]; ok {
		t.Fatalf("stored metadata = %v, want provider-owned roles removed", metadata)
	}
	snapshot := catalog.Snapshot()
	if len(snapshot.Projects) != 1 || snapshot.Projects[0].Provider != "" {
		t.Fatalf("catalog projects = %#v, want GitHub-default binding", snapshot.Projects)
	}
	if !config.ProjectRoleConfigs(snapshot, "odcrew").Reviewer.Discovery.Triggers.RequireReviewRequest {
		t.Fatal("GitHub reviewer requireReviewRequest = false, want global GitHub default")
	}
}
