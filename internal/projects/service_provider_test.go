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
