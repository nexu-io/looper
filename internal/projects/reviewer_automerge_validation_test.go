package projects

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/storage"
)

func TestValidateReviewerAutoMergeForProjectFailureModes(t *testing.T) {
	t.Parallel()

	baseConfig, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	baseConfig.Roles.Reviewer.AutoMerge.Enabled = true
	baseConfig.Defaults.BaseBranch = "main"
	service := &Service{
		GetRepositorySettings: func(context.Context, githubinfra.RepositorySettingsInput) (githubinfra.RepositorySettings, error) {
			return githubinfra.RepositorySettings{AllowSquashMerge: true, AllowMergeCommit: true, AllowRebaseMerge: true, AllowAutoMerge: true}, nil
		},
		GetBranchProtection: func(context.Context, githubinfra.BranchProtectionInput) (githubinfra.BranchProtection, error) {
			return githubinfra.BranchProtection{Enabled: true, HasRequiredChecks: true}, nil
		},
	}
	repo := "acme/looper"

	tests := []struct {
		name    string
		mutate  func(*config.Config)
		service *Service
		want    string
	}{
		{name: "unsupported scope", mutate: func(cfg *config.Config) { cfg.Roles.Reviewer.AutoMerge.Scope = "future-scope" }, service: service, want: `scope "future-scope" is unsupported in v1`},
		{name: "strategy disallowed", mutate: func(cfg *config.Config) {
			cfg.Roles.Reviewer.AutoMerge.Strategy = config.ReviewerAutoMergeStrategyRebase
		}, service: &Service{GetRepositorySettings: func(context.Context, githubinfra.RepositorySettingsInput) (githubinfra.RepositorySettings, error) {
			return githubinfra.RepositorySettings{AllowSquashMerge: true, AllowMergeCommit: true, AllowRebaseMerge: false, AllowAutoMerge: true}, nil
		}, GetBranchProtection: service.GetBranchProtection}, want: `configured strategy "rebase" is not allowed by repo settings`},
		{name: "auto merge disabled", mutate: func(cfg *config.Config) {}, service: &Service{GetRepositorySettings: func(context.Context, githubinfra.RepositorySettingsInput) (githubinfra.RepositorySettings, error) {
			return githubinfra.RepositorySettings{AllowSquashMerge: true, AllowMergeCommit: true, AllowRebaseMerge: true, AllowAutoMerge: false}, nil
		}, GetBranchProtection: service.GetBranchProtection}, want: "GitHub repo auto-merge is disabled"},
		{name: "missing branch protection", mutate: func(cfg *config.Config) {}, service: &Service{GetRepositorySettings: service.GetRepositorySettings, GetBranchProtection: func(context.Context, githubinfra.BranchProtectionInput) (githubinfra.BranchProtection, error) {
			return githubinfra.BranchProtection{}, nil
		}}, want: "default branch protection is missing or has no required checks"},
		{name: "missing branch protection gateway only matters when protection required", mutate: func(cfg *config.Config) {
			cfg.Roles.Reviewer.AutoMerge.RequireBranchProtection = false
		}, service: &Service{GetRepositorySettings: service.GetRepositorySettings}, want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := baseConfig
			tt.mutate(&cfg)
			err := tt.service.validateReviewerAutoMergeForProject(context.Background(), "project_1", &repo, "main", cfg)
			if tt.want == "" {
				if err != nil {
					t.Fatalf("validateReviewerAutoMergeForProject() error = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("validateReviewerAutoMergeForProject() error = %v, want substring %q", err, tt.want)
			}
		})
	}
}

func TestServiceAddProjectValidatesReviewerAutoMerge(t *testing.T) {
	t.Parallel()

	coordinator := openCoordinator(t)
	repos := storage.NewRepositories(coordinator.DB())
	repo := "acme/looper"
	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Roles.Reviewer.AutoMerge.Enabled = true

	service := &Service{
		DB:     coordinator.DB(),
		Repos:  repos,
		Config: cfg,
		Now:    func() time.Time { return time.Date(2026, time.May, 1, 0, 0, 0, 0, time.UTC) },
		GetRepositorySettings: func(context.Context, githubinfra.RepositorySettingsInput) (githubinfra.RepositorySettings, error) {
			return githubinfra.RepositorySettings{AllowSquashMerge: true, AllowMergeCommit: true, AllowRebaseMerge: true, AllowAutoMerge: true}, nil
		},
		GetBranchProtection: func(context.Context, githubinfra.BranchProtectionInput) (githubinfra.BranchProtection, error) {
			return githubinfra.BranchProtection{}, nil
		},
	}

	_, err = service.AddProject(context.Background(), AddInput{ID: "looper", Name: "Looper", RepoPath: "/tmp/looper", BaseBranch: "main", Repo: &repo})
	if err == nil || !strings.Contains(err.Error(), "default branch protection is missing or has no required checks") {
		t.Fatalf("AddProject() error = %v, want branch protection validation failure", err)
	}
}

func TestServiceAddProjectAllowsUnknownBaseBranchWithoutProtectionRequirement(t *testing.T) {
	t.Parallel()

	coordinator := openCoordinator(t)
	repos := storage.NewRepositories(coordinator.DB())
	repo := "acme/looper"
	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Roles.Reviewer.AutoMerge.Enabled = true
	cfg.Roles.Reviewer.AutoMerge.RequireBranchProtection = false
	cfg.Defaults.BaseBranch = ""

	service := &Service{
		DB:     coordinator.DB(),
		Repos:  repos,
		Config: cfg,
		Now:    func() time.Time { return time.Date(2026, time.May, 1, 0, 0, 0, 0, time.UTC) },
		GetRepositorySettings: func(context.Context, githubinfra.RepositorySettingsInput) (githubinfra.RepositorySettings, error) {
			return githubinfra.RepositorySettings{AllowSquashMerge: true, AllowMergeCommit: true, AllowRebaseMerge: true, AllowAutoMerge: true}, nil
		},
	}

	result, err := service.AddProject(context.Background(), AddInput{ID: "looper", Name: "Looper", RepoPath: "/tmp/looper", Repo: &repo})
	if err != nil {
		t.Fatalf("AddProject() error = %v, want nil", err)
	}
	if result.Project.BaseBranch != nil && *result.Project.BaseBranch != "" {
		t.Fatalf("AddProject() base branch = %q, want empty", *result.Project.BaseBranch)
	}
}

func TestServiceSyncConfiguredValidatesReviewerAutoMerge(t *testing.T) {
	t.Parallel()

	coordinator := openCoordinator(t)
	repos := storage.NewRepositories(coordinator.DB())
	service := &Service{
		DB:    coordinator.DB(),
		Repos: repos,
		Now:   func() time.Time { return time.Date(2026, time.May, 1, 0, 0, 0, 0, time.UTC) },
		GetRepositorySettings: func(context.Context, githubinfra.RepositorySettingsInput) (githubinfra.RepositorySettings, error) {
			return githubinfra.RepositorySettings{AllowSquashMerge: true, AllowMergeCommit: true, AllowRebaseMerge: true, AllowAutoMerge: true}, nil
		},
		GetBranchProtection: func(context.Context, githubinfra.BranchProtectionInput) (githubinfra.BranchProtection, error) {
			return githubinfra.BranchProtection{}, nil
		},
	}
	repoName := "acme/looper"
	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Roles.Reviewer.AutoMerge.Enabled = true
	baseBranch := "main"
	cfg.Projects = []config.ProjectRefConfig{{ID: "looper", Name: "Looper", RepoPath: "/tmp/looper", BaseBranch: &baseBranch}}

	service.DetectRepo = func(context.Context, string) (DetectedRepo, error) { return DetectedRepo{Repo: repoName}, nil }
	err = service.SyncConfigured(context.Background(), cfg, time.Date(2026, time.May, 1, 0, 0, 0, 0, time.UTC))
	if err == nil || !strings.Contains(err.Error(), "default branch protection is missing or has no required checks") {
		t.Fatalf("SyncConfigured() error = %v, want branch protection validation failure", err)
	}
}

func TestServiceSyncConfiguredAllowsUnknownBaseBranchWithoutProtectionRequirement(t *testing.T) {
	t.Parallel()

	coordinator := openCoordinator(t)
	repos := storage.NewRepositories(coordinator.DB())
	service := &Service{
		DB:    coordinator.DB(),
		Repos: repos,
		Now:   func() time.Time { return time.Date(2026, time.May, 1, 0, 0, 0, 0, time.UTC) },
		GetRepositorySettings: func(context.Context, githubinfra.RepositorySettingsInput) (githubinfra.RepositorySettings, error) {
			return githubinfra.RepositorySettings{AllowSquashMerge: true, AllowMergeCommit: true, AllowRebaseMerge: true, AllowAutoMerge: true}, nil
		},
	}
	repoName := "acme/looper"
	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Roles.Reviewer.AutoMerge.Enabled = true
	cfg.Roles.Reviewer.AutoMerge.RequireBranchProtection = false
	cfg.Defaults.BaseBranch = ""
	cfg.Projects = []config.ProjectRefConfig{{ID: "looper", Name: "Looper", RepoPath: "/tmp/looper"}}

	service.DetectRepo = func(context.Context, string) (DetectedRepo, error) { return DetectedRepo{Repo: repoName}, nil }
	err = service.SyncConfigured(context.Background(), cfg, time.Date(2026, time.May, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("SyncConfigured() error = %v, want nil", err)
	}
	project, err := repos.Projects.GetByID(context.Background(), "looper")
	if err != nil {
		t.Fatalf("Projects.GetByID() error = %v", err)
	}
	if project == nil {
		t.Fatalf("Projects.GetByID() = nil, want stored project")
	}
	if project.BaseBranch != nil && *project.BaseBranch != "" {
		t.Fatalf("SyncConfigured() base branch = %q, want empty", *project.BaseBranch)
	}
}
