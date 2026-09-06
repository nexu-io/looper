package projects

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/storage"
)

func TestServiceSyncConfiguredForgejoAutoMergeUsesProviderPolicy(t *testing.T) {
	t.Setenv("FORGEJO_PROJECT_MERGE_TOKEN", "test")
	for _, tc := range []struct {
		name                      string
		protected, checks, squash bool
		want                      string
	}{
		{"opt in", true, true, true, ""},
		{"missing protection", false, true, true, "default branch protection"},
		{"status checks disabled", true, false, true, "default branch protection"},
		{"strategy disabled", true, true, false, "strategy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				switch r.URL.Path {
				case "/api/v1/repos/core/looper":
					_ = json.NewEncoder(w).Encode(map[string]any{"allow_squash_merge": tc.squash})
				case "/api/v1/repos/core/looper/branches/main":
					_ = json.NewEncoder(w).Encode(map[string]any{"protected": tc.protected, "enable_status_check": tc.checks, "status_check_contexts": []string{"unit"}})
				default:
					t.Errorf("unexpected provider request %s %s", r.Method, r.URL)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			cfg, err := config.DefaultConfig(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			tokenEnv, enabled := "FORGEJO_PROJECT_MERGE_TOKEN", true
			cfg.Providers = []config.ProviderConfig{{ID: "fj", Kind: config.ProviderKindForgejo, BaseURL: server.URL, TokenEnv: &tokenEnv}}
			cfg.Projects = []config.ProjectRefConfig{{ID: "looper", Name: "Looper", Provider: "fj", Repo: "core/looper", RepoPath: t.TempDir(), Roles: &config.PartialRoleConfigs{Reviewer: &config.PartialReviewerRoleConfig{AutoMerge: &config.PartialReviewerAutoMergeConfig{Enabled: &enabled}}}}}
			config.ApplyForgejoProjectProfile(&cfg.Projects[0])
			coordinator := openCoordinator(t)
			repos := storage.NewRepositories(coordinator.DB())
			service := &Service{DB: coordinator.DB(), Repos: repos}
			err = service.SyncConfigured(context.Background(), cfg, time.Now())
			if tc.want != "" {
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("SyncConfigured = %v, want %s", err, tc.want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if requests != 2 {
				t.Fatalf("provider reads = %d, want repo + branch", requests)
			}
			project, err := repos.Projects.GetByID(context.Background(), "looper")
			if err != nil || project == nil {
				t.Fatalf("configured project = %#v, %v", project, err)
			}
		})
	}
}

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

func TestValidateReviewerAutoMergeRejectsForgejoSummaryComment(t *testing.T) {
	t.Parallel()

	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	tokenEnv := "FORGEJO_TOKEN"
	cfg.Roles.Reviewer.AutoMerge.Enabled = true
	cfg.Roles.Reviewer.Behavior.PublishMode = config.ReviewerPublishModeSummaryComment
	cfg.Providers = []config.ProviderConfig{{ID: "fj", Kind: config.ProviderKindForgejo, BaseURL: "https://forgejo.example.test", TokenEnv: &tokenEnv}}
	cfg.Projects = []config.ProjectRefConfig{{ID: "project_1", Name: "Looper", Provider: "fj", Repo: "acme/looper", RepoPath: t.TempDir()}}
	repo := "acme/looper"
	err = (&Service{}).validateReviewerAutoMergeForProject(context.Background(), "project_1", &repo, "main", cfg)
	if err == nil || !strings.Contains(err.Error(), `publishMode "summary_comment"`) {
		t.Fatalf("validateReviewerAutoMergeForProject() error = %v, want summary_comment rejection", err)
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
