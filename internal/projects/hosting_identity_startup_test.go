package projects

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/hostingidentity"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/storage"
)

func TestHostingIdentityProjectProbesAreScopedOutsidePublicationBoundary(t *testing.T) {
	for _, operation := range []string{"add", "startup_sync"} {
		for _, semantic := range []bool{false, true} {
			t.Run(operation+map[bool]string{false: "/external_unavailable", true: "/semantic_rejected"}[semantic], func(t *testing.T) {
				cfg := projectHostingIdentityConfig(t)
				cfg.Roles.Reviewer.AutoMerge.Enabled = true
				cfg.Roles.Reviewer.AutoMerge.RequireBranchProtection = false
				project := config.ProjectRefConfig{ID: "project", Name: "Project", Repo: "owner/repo", RepoPath: t.TempDir(), Identity: "worker-bot"}
				if operation == "startup_sync" {
					cfg.Projects = []config.ProjectRefConfig{project}
				}
				db := openCoordinator(t)
				repos := storage.NewRepositories(db.DB())
				catalog := NewCatalog(cfg)
				boundary := &sync.RWMutex{}
				calls := 0
				service := &Service{DB: db.DB(), Repos: repos, Config: cfg, ConfigSource: catalog, ConfigBoundary: boundary, PublishProjects: catalog.Publish,
					GetRepositorySettings: func(ctx context.Context, input githubinfra.RepositorySettingsInput) (githubinfra.RepositorySettings, error) {
						calls++
						session, selected := hostingidentity.FromContext(ctx)
						if !selected || session.Name() != "worker-bot" || session.ProjectID() != "project" || session.Role() != "worker" || input.Repo != "owner/repo" {
							t.Fatalf("project probe has wrong selection: %v, %#v", session, input)
						}
						if !boundary.TryLock() {
							t.Fatal("network probe holds global configuration publication lock")
						}
						boundary.Unlock()
						if semantic {
							return githubinfra.RepositorySettings{AllowAutoMerge: true}, nil // configured squash is forbidden
						}
						return githubinfra.RepositorySettings{}, &hostingidentity.Error{Identity: session.Name(), Operation: "read settings", StatusCode: 403, Reason: "installation unavailable"}
					},
				}
				var err error
				if operation == "add" {
					var result AddResult
					result, err = service.AddProject(context.Background(), AddInput{ID: project.ID, Name: project.Name, RepoPath: project.RepoPath, Repo: &project.Repo, Identity: &project.Identity})
					if !semantic && (len(result.Warnings) == 0 || !strings.Contains(strings.Join(result.Warnings, " "), `hosting identity "worker-bot"`)) {
						t.Fatalf("missing affected identity diagnostics: %#v, %v", result, err)
					}
				} else {
					err = service.SyncConfigured(context.Background(), cfg, time.Now())
				}
				if calls != 1 {
					t.Fatalf("settings calls = %d, want one", calls)
				}
				var validation ProjectValidationError
				if semantic && !errors.As(err, &validation) {
					t.Fatalf("semantic rejection downgraded to warning: %v", err)
				}
				if !semantic && err != nil {
					t.Fatalf("external bot outage prevented project admission: %v", err)
				}
				record, readErr := repos.Projects.GetByID(context.Background(), "project")
				if readErr != nil || (record != nil) == semantic {
					t.Fatalf("project publication did not match outcome: %#v, %v", record, readErr)
				}
			})
		}
	}
}

func TestHostingIdentityForgejoSummaryModeStillRejectsAutoMerge(t *testing.T) {
	cfg := projectHostingIdentityConfig(t)
	cfg.Providers = []config.ProviderConfig{{ID: "forgejo", Kind: config.ProviderKindForgejo, BaseURL: "https://forge.example"}}
	cfg.Identities = map[string]config.HostingIdentityConfig{"bot": {Kind: config.HostingIdentityForgejoToken, BaseURL: "https://forge.example", TokenEnv: "UNSET_FORGEJO_BOT"}}
	cfg.Projects = []config.ProjectRefConfig{{ID: "project", Repo: "owner/repo", Provider: "forgejo", Identity: "bot"}}
	cfg.Roles.Reviewer.AutoMerge.Enabled = true
	cfg.Roles.Reviewer.Behavior.PublishMode = config.ReviewerPublishModeSummaryComment
	warning, err := (&Service{}).validateHostingReviewerAutoMerge(context.Background(), "project", stringPointer("owner/repo"), "main", cfg)
	var validation ProjectValidationError
	if warning != "" || !errors.As(err, &validation) || !strings.Contains(err.Error(), "summary_comment") {
		t.Fatalf("summary-comment semantics swallowed by bot diagnostics: %q, %v", warning, err)
	}
}
