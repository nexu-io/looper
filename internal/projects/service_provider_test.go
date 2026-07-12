package projects

import (
	"context"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
)

func TestServiceAddProjectResolvesForgejoProviderTypeAndRepo(t *testing.T) {
	t.Parallel()

	coordinator := openCoordinator(t)
	ctx := context.Background()
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 17, 12, 34, 56, 0, time.UTC)
	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	tokenEnv := "LOOPER_FORGEJO_TOKEN"
	cfg.Providers = []config.ProviderConfig{{
		ID:       "forgejo-main",
		Kind:     config.ProviderKindForgejo,
		BaseURL:  "https://code.powerformer.net",
		TokenEnv: &tokenEnv,
	}}

	var registered *ProjectBinding
	service := &Service{
		DB:     coordinator.DB(),
		Repos:  repos,
		Config: cfg,
		Now:    func() time.Time { return now },
		DetectRepo: func(context.Context, string) (DetectedRepo, error) {
			// Non-GitHub host must not auto-bind provider; kind is explicit.
			return DetectedRepo{Repo: "core/odcrew", Host: "ssh.code.powerformer.net"}, nil
		},
		RegisterBinding: func(binding ProjectBinding) {
			registered = &binding
		},
		ListOpenPullRequests: func(context.Context, ListOpenPullRequestsInput) ([]PullRequestSummary, error) {
			t.Fatal("ListOpenPullRequests should not run for forgejo projects")
			return nil, nil
		},
	}

	providerType := "forgejo"
	result, err := service.AddProject(ctx, AddInput{
		ID:         "odcrew",
		Name:       "odcrew",
		RepoPath:   "/tmp/odcrew",
		BaseBranch: "main",
		Provider:   &providerType,
	})
	if err != nil {
		t.Fatalf("AddProject() error = %v", err)
	}
	if result.Repo == nil || *result.Repo != "core/odcrew" {
		t.Fatalf("AddProject().Repo = %v, want core/odcrew", result.Repo)
	}
	if result.Provider == nil || *result.Provider != "forgejo-main" {
		t.Fatalf("AddProject().Provider = %v, want forgejo-main", result.Provider)
	}
	if result.Project.MetadataJSON == nil || *result.Project.MetadataJSON != `{"provider":"forgejo-main","repo":"core/odcrew","worktreeRoot":null,"source":"api"}` {
		t.Fatalf("AddProject().Project.MetadataJSON = %v, want forgejo api metadata", result.Project.MetadataJSON)
	}
	if result.DiscoveredPullRequests != 0 {
		t.Fatalf("AddProject().DiscoveredPullRequests = %d, want 0 for forgejo", result.DiscoveredPullRequests)
	}
	if registered == nil || registered.Provider != "forgejo-main" || registered.Repo != "core/odcrew" {
		t.Fatalf("RegisterBinding() = %#v, want forgejo binding", registered)
	}
}
