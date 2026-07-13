package projects

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/domain"
	"github.com/nexu-io/looper/internal/infra/shell"
	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
)

func TestServiceAddProjectCreatesAPIProject(t *testing.T) {
	t.Parallel()

	coordinator := openCoordinator(t)
	ctx := context.Background()
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 17, 12, 34, 56, 0, time.UTC)
	service := &Service{DB: coordinator.DB(), Repos: repos, Now: func() time.Time { return now }}

	result, err := service.AddProject(ctx, AddInput{
		ID:         "looper",
		Name:       "Looper",
		RepoPath:   "/tmp/looper",
		BaseBranch: "main",
	})
	if err != nil {
		t.Fatalf("AddProject() error = %v", err)
	}
	if result.Project.ID != "looper" {
		t.Fatalf("AddProject().Project.ID = %q, want looper", result.Project.ID)
	}
	if result.Project.MetadataJSON == nil || *result.Project.MetadataJSON != `{"repo":null,"worktreeRoot":null,"source":"api"}` {
		t.Fatalf("AddProject().Project.MetadataJSON = %v, want api metadata", result.Project.MetadataJSON)
	}
}

func TestServiceAddForgejoProjectActivatesProviderBinding(t *testing.T) {
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
	service := &Service{
		DB:           coordinator.DB(),
		Repos:        repos,
		Config:       cfg,
		ConfigSource: catalog,
		Now:          time.Now,
		DetectRepo: func(context.Context, string) (DetectedRepo, error) {
			return DetectedRepo{Repo: "core/odcrew", Provider: "forgejo-main"}, nil
		},
		ListOpenPullRequests: func(context.Context, ListOpenPullRequestsInput) ([]PullRequestSummary, error) {
			t.Fatal("ListOpenPullRequests should not run for a Forgejo project")
			return nil, nil
		},
		PublishProjects: catalog.Publish,
	}

	result, err := service.AddProject(context.Background(), AddInput{
		ID: "odcrew", Name: "odcrew", RepoPath: "/tmp/odcrew", BaseBranch: "main", Provider: stringPointer("forgejo-main"),
	})
	if err != nil {
		t.Fatalf("AddProject() error = %v", err)
	}
	if result.Provider == nil || *result.Provider != "forgejo-main" {
		t.Fatalf("AddProject().Provider = %v, want forgejo-main", result.Provider)
	}
	metadata := parseMetadata(result.Project.MetadataJSON)
	if metadataString(metadata, "provider") != "forgejo-main" || metadataString(metadata, "repo") != "core/odcrew" {
		t.Fatalf("metadata = %v, want persisted provider binding", result.Project.MetadataJSON)
	}
	if _, ok := metadata["roles"]; !ok {
		t.Fatalf("metadata = %v, want persisted Forgejo role profile", result.Project.MetadataJSON)
	}
	snapshot := catalog.Snapshot()
	if len(snapshot.Projects) != 1 || snapshot.Projects[0].Provider != "forgejo-main" || snapshot.Projects[0].Repo != "core/odcrew" {
		t.Fatalf("catalog projects = %#v, want published Forgejo binding", snapshot.Projects)
	}
	triggers := snapshot.Projects[0].Roles.Reviewer.Discovery.Triggers
	if triggers.RequireReviewRequest == nil || *triggers.RequireReviewRequest || triggers.Labels == nil || len(*triggers.Labels) != 1 || (*triggers.Labels)[0] != "looper:review" {
		t.Fatalf("catalog reviewer triggers = %#v, want Forgejo label profile", triggers)
	}
}

func TestServiceConcurrentAddsPublishCommittedCatalog(t *testing.T) {
	t.Parallel()

	coordinator := openCoordinator(t)
	ctx := context.Background()
	repos := storage.NewRepositories(coordinator.DB())
	firstPublishStarted := make(chan struct{})
	releaseFirstPublish := make(chan struct{})
	var publishMu sync.Mutex
	var published []config.ProjectRefConfig
	publishCount := 0
	service := &Service{
		DB:    coordinator.DB(),
		Repos: repos,
		Now:   time.Now,
		PublishProjects: func(projects []config.ProjectRefConfig) {
			publishMu.Lock()
			publishCount++
			count := publishCount
			published = append([]config.ProjectRefConfig(nil), projects...)
			publishMu.Unlock()
			if count == 1 {
				close(firstPublishStarted)
				<-releaseFirstPublish
			}
		},
	}

	errorsCh := make(chan error, 2)
	go func() {
		_, err := service.AddProject(ctx, AddInput{ID: "a", Name: "A", RepoPath: "/tmp/a", BaseBranch: "main"})
		errorsCh <- err
	}()
	<-firstPublishStarted
	go func() {
		_, err := service.AddProject(ctx, AddInput{ID: "b", Name: "B", RepoPath: "/tmp/b", BaseBranch: "main"})
		errorsCh <- err
	}()

	close(releaseFirstPublish)
	for range 2 {
		if err := <-errorsCh; err != nil {
			t.Fatalf("AddProject() error = %v", err)
		}
	}

	publishMu.Lock()
	got := append([]config.ProjectRefConfig(nil), published...)
	publishMu.Unlock()
	if len(got) != 2 || got[0].ID != "a" || got[1].ID != "b" {
		t.Fatalf("last published projects = %#v, want a and b", got)
	}
	stored, err := service.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(stored) != 2 {
		t.Fatalf("len(List()) = %d, want 2", len(stored))
	}
}

func TestServiceAddProjectRejectsProjectIDWithBackslash(t *testing.T) {
	t.Parallel()

	coordinator := openCoordinator(t)
	ctx := context.Background()
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 17, 12, 34, 56, 0, time.UTC)
	service := &Service{DB: coordinator.DB(), Repos: repos, Now: func() time.Time { return now }}

	_, err := service.AddProject(ctx, AddInput{
		ID:         `foo\bar`,
		Name:       "Looper",
		RepoPath:   "/tmp/looper",
		BaseBranch: "main",
	})
	if err == nil {
		t.Fatal("AddProject() error = nil, want invalid project id")
	}
	if !strings.Contains(err.Error(), "invalid project id") {
		t.Fatalf("AddProject() error = %v, want invalid project id", err)
	}
	stored, getErr := repos.Projects.GetByID(ctx, `foo\bar`)
	if getErr != nil {
		t.Fatalf("Projects.GetByID() error = %v", getErr)
	}
	if stored != nil {
		t.Fatalf("Projects.GetByID() = %#v, want nil", stored)
	}
}

func TestServiceAddProjectDiscoversPullRequestsAndWorktrees(t *testing.T) {
	t.Parallel()

	coordinator := openCoordinator(t)
	ctx := context.Background()
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 17, 12, 34, 56, 0, time.UTC)
	service := &Service{
		DB:         coordinator.DB(),
		Repos:      repos,
		Now:        func() time.Time { return now },
		DetectRepo: func(context.Context, string) (DetectedRepo, error) { return DetectedRepo{Repo: "nexu-io/looper"}, nil },
		ListWorktrees: func(context.Context, string) ([]WorktreeListEntry, error) {
			return []WorktreeListEntry{{Path: "/tmp/looper", Branch: "main", HeadSHA: "abc123"}, {Path: "/tmp/looper-pr-1", Branch: "pr-1", HeadSHA: "def456"}}, nil
		},
		ListOpenPullRequests: func(context.Context, ListOpenPullRequestsInput) ([]PullRequestSummary, error) {
			return []PullRequestSummary{{Number: 1, State: "OPEN", IsDraft: false}, {Number: 2, State: "OPEN", IsDraft: true}}, nil
		},
		CapturePullRequestSnapshot: func(context.Context, CapturePullRequestSnapshotInput) (storage.PullRequestSnapshotRecord, error) {
			capturedAt := now.UTC().Format(time.RFC3339Nano)
			return storage.PullRequestSnapshotRecord{ID: "snapshot_1", ProjectID: "looper", Repo: "nexu-io/looper", PRNumber: 1, HeadSHA: "abc123", Title: stringPointer("PR 1"), CapturedAt: capturedAt, CreatedAt: capturedAt}, nil
		},
	}

	result, err := service.AddProject(ctx, AddInput{ID: "looper", Name: "Looper", RepoPath: "/tmp/looper", BaseBranch: "main", SnapshotMode: SnapshotModeFull})
	if err != nil {
		t.Fatalf("AddProject() error = %v", err)
	}
	if result.Repo == nil || *result.Repo != "nexu-io/looper" {
		t.Fatalf("AddProject().Repo = %v, want nexu-io/looper", result.Repo)
	}
	if result.DiscoveredWorktrees != 2 {
		t.Fatalf("AddProject().DiscoveredWorktrees = %d, want 2", result.DiscoveredWorktrees)
	}
	if result.DiscoveredPullRequests != 1 {
		t.Fatalf("AddProject().DiscoveredPullRequests = %d, want 1", result.DiscoveredPullRequests)
	}
	if len(result.Warnings) != 0 {
		t.Fatalf("AddProject().Warnings = %#v, want none", result.Warnings)
	}

	worktrees, err := repos.Worktrees.ListByProject(ctx, "looper")
	if err != nil {
		t.Fatalf("Worktrees.ListByProject() error = %v", err)
	}
	if len(worktrees) != 2 {
		t.Fatalf("len(worktrees) = %d, want 2", len(worktrees))
	}
	snapshot, err := repos.PullRequestSnapshots.GetLatest(ctx, "nexu-io/looper", 1)
	if err != nil {
		t.Fatalf("PullRequestSnapshots.GetLatest() error = %v", err)
	}
	if snapshot == nil || snapshot.Title == nil || *snapshot.Title != "PR 1" {
		t.Fatalf("snapshot = %#v, want PR 1 snapshot", snapshot)
	}
}

func TestServiceAddProjectDefaultAsyncEnqueuesSnapshotsWithoutCapturing(t *testing.T) {
	t.Parallel()

	coordinator := openCoordinator(t)
	ctx := context.Background()
	repos := storage.NewRepositories(coordinator.DB())
	captured := false
	repo := "nexu-io/looper"
	service := &Service{
		DB:    coordinator.DB(),
		Repos: repos,
		Now:   func() time.Time { return time.Date(2026, time.April, 17, 12, 34, 56, 0, time.UTC) },
		ListOpenPullRequests: func(context.Context, ListOpenPullRequestsInput) ([]PullRequestSummary, error) {
			return []PullRequestSummary{{Number: 1, State: "OPEN", IsDraft: false}, {Number: 2, State: "OPEN", IsDraft: true}}, nil
		},
		CapturePullRequestSnapshot: func(context.Context, CapturePullRequestSnapshotInput) (storage.PullRequestSnapshotRecord, error) {
			captured = true
			return storage.PullRequestSnapshotRecord{}, nil
		},
	}

	result, err := service.AddProject(ctx, AddInput{ID: "looper", Name: "Looper", RepoPath: "/tmp/looper", BaseBranch: "main", Repo: &repo})
	if err != nil {
		t.Fatalf("AddProject() error = %v", err)
	}
	if captured {
		t.Fatal("CapturePullRequestSnapshot called in default async mode")
	}
	if result.DiscoveredPullRequests != 1 || result.PendingSnapshots != 1 || result.CapturedSnapshots != 0 {
		t.Fatalf("AddProject() counts = discovered %d pending %d captured %d, want 1/1/0", result.DiscoveredPullRequests, result.PendingSnapshots, result.CapturedSnapshots)
	}
	items, err := repos.Queue.List(ctx)
	if err != nil {
		t.Fatalf("Queue.List() error = %v", err)
	}
	if len(items) != 1 || items[0].Type != "snapshot" || items[0].PRNumber == nil || *items[0].PRNumber != 1 {
		t.Fatalf("queue items = %#v, want one snapshot for PR 1", items)
	}
	if items[0].Priority != storage.QueuePrioritySnapshot {
		t.Fatalf("snapshot priority = %d, want %d", items[0].Priority, storage.QueuePrioritySnapshot)
	}
	if items[0].MaxAttempts != -1 {
		t.Fatalf("snapshot max attempts = %d, want -1", items[0].MaxAttempts)
	}
}

func TestServiceAddProjectAsyncFallsBackToFullWhenQueueDisabled(t *testing.T) {
	t.Parallel()

	coordinator := openCoordinator(t)
	ctx := context.Background()
	repos := storage.NewRepositories(coordinator.DB())
	repo := "nexu-io/looper"
	now := time.Date(2026, time.April, 17, 12, 34, 56, 0, time.UTC)
	service := &Service{
		DB:    coordinator.DB(),
		Repos: repos,
		Now:   func() time.Time { return now },
		ListOpenPullRequests: func(context.Context, ListOpenPullRequestsInput) ([]PullRequestSummary, error) {
			return []PullRequestSummary{{Number: 1, State: "OPEN", IsDraft: false}}, nil
		},
		CapturePullRequestSnapshot: func(context.Context, CapturePullRequestSnapshotInput) (storage.PullRequestSnapshotRecord, error) {
			capturedAt := now.UTC().Format(time.RFC3339Nano)
			return storage.PullRequestSnapshotRecord{ID: "snapshot_1", ProjectID: "looper", Repo: repo, PRNumber: 1, HeadSHA: "abc123", Title: stringPointer("PR 1"), CapturedAt: capturedAt, CreatedAt: capturedAt}, nil
		},
		AsyncSnapshotQueueEnabled: func() bool { return false },
	}

	result, err := service.AddProject(ctx, AddInput{ID: "looper", Name: "Looper", RepoPath: "/tmp/looper", BaseBranch: "main", Repo: &repo, SnapshotMode: SnapshotModeAsync})
	if err != nil {
		t.Fatalf("AddProject() error = %v", err)
	}
	if result.DiscoveredPullRequests != 1 || result.PendingSnapshots != 0 || result.CapturedSnapshots != 1 {
		t.Fatalf("AddProject() counts = discovered %d pending %d captured %d, want 1/0/1", result.DiscoveredPullRequests, result.PendingSnapshots, result.CapturedSnapshots)
	}
	if len(result.Warnings) != 1 || result.Warnings[0] != "Async snapshot mode requires the scheduler; capturing snapshots synchronously instead." {
		t.Fatalf("AddProject().Warnings = %#v, want async fallback warning", result.Warnings)
	}
	items, err := repos.Queue.List(ctx)
	if err != nil {
		t.Fatalf("Queue.List() error = %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("queue items = %#v, want none", items)
	}
	snapshot, err := repos.PullRequestSnapshots.GetLatest(ctx, repo, 1)
	if err != nil {
		t.Fatalf("PullRequestSnapshots.GetLatest() error = %v", err)
	}
	if snapshot == nil {
		t.Fatal("snapshot = nil, want captured snapshot")
	}
}

func TestServiceAddProjectSnapshotModeOffSkipsPullRequestDiscovery(t *testing.T) {
	t.Parallel()

	coordinator := openCoordinator(t)
	ctx := context.Background()
	repos := storage.NewRepositories(coordinator.DB())
	listed := false
	repo := "nexu-io/looper"
	service := &Service{DB: coordinator.DB(), Repos: repos, Now: time.Now, ListOpenPullRequests: func(context.Context, ListOpenPullRequestsInput) ([]PullRequestSummary, error) {
		listed = true
		return nil, nil
	}}

	result, err := service.AddProject(ctx, AddInput{ID: "looper", Name: "Looper", RepoPath: "/tmp/looper", BaseBranch: "main", Repo: &repo, SnapshotMode: SnapshotModeOff})
	if err != nil {
		t.Fatalf("AddProject() error = %v", err)
	}
	if listed || result.DiscoveredPullRequests != 0 || result.PendingSnapshots != 0 {
		t.Fatalf("off mode listed=%v counts=%#v, want no discovery", listed, result)
	}
}

func TestServiceAddProjectReturnsDiscoveryWarnings(t *testing.T) {
	t.Parallel()

	coordinator := openCoordinator(t)
	ctx := context.Background()
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 17, 12, 34, 56, 0, time.UTC)
	service := &Service{
		DB:    coordinator.DB(),
		Repos: repos,
		Now:   func() time.Time { return now },
		ListWorktrees: func(context.Context, string) ([]WorktreeListEntry, error) {
			return nil, errors.New("git worktree failed")
		},
		ListOpenPullRequests: func(context.Context, ListOpenPullRequestsInput) ([]PullRequestSummary, error) {
			return nil, errors.New("gh pr list failed")
		},
		CapturePullRequestSnapshot: func(context.Context, CapturePullRequestSnapshotInput) (storage.PullRequestSnapshotRecord, error) {
			return storage.PullRequestSnapshotRecord{}, nil
		},
	}
	repo := "nexu-io/looper"

	result, err := service.AddProject(ctx, AddInput{ID: "looper", Name: "Looper", RepoPath: "/tmp/looper", BaseBranch: "main", Repo: &repo, SnapshotMode: SnapshotModeFull})
	if err != nil {
		t.Fatalf("AddProject() error = %v", err)
	}
	if result.DiscoveredWorktrees != 0 {
		t.Fatalf("AddProject().DiscoveredWorktrees = %d, want 0", result.DiscoveredWorktrees)
	}
	if result.DiscoveredPullRequests != 0 {
		t.Fatalf("AddProject().DiscoveredPullRequests = %d, want 0", result.DiscoveredPullRequests)
	}
	if len(result.Warnings) != 2 {
		t.Fatalf("len(AddProject().Warnings) = %d, want 2", len(result.Warnings))
	}
	if result.Warnings[0] != "Could not discover worktrees: git worktree failed" {
		t.Fatalf("Warnings[0] = %q, want worktree warning", result.Warnings[0])
	}
	if result.Warnings[1] != "Could not discover pull requests: gh pr list failed" {
		t.Fatalf("Warnings[1] = %q, want pull request warning", result.Warnings[1])
	}
}

func TestServiceAddProjectWarnsWhenPullRequestSnapshotFails(t *testing.T) {
	t.Parallel()

	coordinator := openCoordinator(t)
	ctx := context.Background()
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 17, 12, 34, 56, 0, time.UTC)
	service := &Service{
		DB:    coordinator.DB(),
		Repos: repos,
		Now:   func() time.Time { return now },
		ListOpenPullRequests: func(context.Context, ListOpenPullRequestsInput) ([]PullRequestSummary, error) {
			return []PullRequestSummary{{Number: 73, State: "OPEN", IsDraft: false}}, nil
		},
		CapturePullRequestSnapshot: func(context.Context, CapturePullRequestSnapshotInput) (storage.PullRequestSnapshotRecord, error) {
			return storage.PullRequestSnapshotRecord{}, errors.New("could not find pull request diff: HTTP 406: Sorry, the diff exceeded the maximum number of lines (20000)")
		},
	}
	repo := "nexu-io/looper"

	result, err := service.AddProject(ctx, AddInput{ID: "looper", Name: "Looper", RepoPath: "/tmp/looper", BaseBranch: "main", Repo: &repo, SnapshotMode: SnapshotModeFull})
	if err != nil {
		t.Fatalf("AddProject() error = %v", err)
	}
	if result.DiscoveredPullRequests != 1 {
		t.Fatalf("AddProject().DiscoveredPullRequests = %d, want 1", result.DiscoveredPullRequests)
	}
	if len(result.Warnings) != 1 {
		t.Fatalf("len(AddProject().Warnings) = %d, want 1", len(result.Warnings))
	}
	wantWarning := "Could not snapshot pull request #73: could not find pull request diff: HTTP 406: Sorry, the diff exceeded the maximum number of lines (20000)"
	if result.Warnings[0] != wantWarning {
		t.Fatalf("Warnings[0] = %q, want %q", result.Warnings[0], wantWarning)
	}
	stored, getErr := repos.Projects.GetByID(ctx, "looper")
	if getErr != nil {
		t.Fatalf("Projects.GetByID() error = %v", getErr)
	}
	if stored == nil {
		t.Fatal("Projects.GetByID() = nil, want stored project")
	}
	snapshot, getErr := repos.PullRequestSnapshots.GetLatest(ctx, repo, 73)
	if getErr != nil {
		t.Fatalf("PullRequestSnapshots.GetLatest() error = %v", getErr)
	}
	if snapshot != nil {
		t.Fatalf("PullRequestSnapshots.GetLatest() = %#v, want nil", snapshot)
	}
}

func TestServiceAddProjectPropagatesPullRequestSnapshotCancellation(t *testing.T) {
	t.Parallel()

	coordinator := openCoordinator(t)
	ctx := context.Background()
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 17, 12, 34, 56, 0, time.UTC)
	service := &Service{
		DB:    coordinator.DB(),
		Repos: repos,
		Now:   func() time.Time { return now },
		ListOpenPullRequests: func(context.Context, ListOpenPullRequestsInput) ([]PullRequestSummary, error) {
			return []PullRequestSummary{{Number: 73, State: "OPEN", IsDraft: false}}, nil
		},
		CapturePullRequestSnapshot: func(context.Context, CapturePullRequestSnapshotInput) (storage.PullRequestSnapshotRecord, error) {
			return storage.PullRequestSnapshotRecord{}, context.Canceled
		},
	}
	repo := "nexu-io/looper"

	_, err := service.AddProject(ctx, AddInput{ID: "looper", Name: "Looper", RepoPath: "/tmp/looper", BaseBranch: "main", Repo: &repo, SnapshotMode: SnapshotModeFull})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("AddProject() error = %v, want context.Canceled", err)
	}
}

func TestServiceAddProjectPropagatesSnapshotCommandErrorCancellation(t *testing.T) {
	t.Parallel()

	coordinator := openCoordinator(t)
	ctx, cancel := context.WithCancel(context.Background())
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 17, 12, 34, 56, 0, time.UTC)
	service := &Service{
		DB:    coordinator.DB(),
		Repos: repos,
		Now:   func() time.Time { return now },
		ListOpenPullRequests: func(context.Context, ListOpenPullRequestsInput) ([]PullRequestSummary, error) {
			return []PullRequestSummary{{Number: 73, State: "OPEN", IsDraft: false}}, nil
		},
		CapturePullRequestSnapshot: func(context.Context, CapturePullRequestSnapshotInput) (storage.PullRequestSnapshotRecord, error) {
			cancel()
			return storage.PullRequestSnapshotRecord{}, &shell.CommandExecutionError{Message: "Command exited with code 1", Result: shell.Result{ExitCode: 1}}
		},
	}
	repo := "nexu-io/looper"

	_, err := service.AddProject(ctx, AddInput{ID: "looper", Name: "Looper", RepoPath: "/tmp/looper", BaseBranch: "main", Repo: &repo, SnapshotMode: SnapshotModeFull})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("AddProject() error = %v, want context.Canceled", err)
	}
}

func TestServiceAddProjectReturnsConflictForExplicitExistingID(t *testing.T) {
	t.Parallel()

	coordinator := openCoordinator(t)
	ctx := context.Background()
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 17, 12, 34, 56, 0, time.UTC)
	service := &Service{DB: coordinator.DB(), Repos: repos, Now: func() time.Time { return now }}

	_, err := service.AddProject(ctx, AddInput{
		ID:         "looper",
		Name:       "Looper",
		RepoPath:   "/tmp/looper",
		BaseBranch: "main",
		IDSource:   "explicit",
	})
	if err != nil {
		t.Fatalf("initial AddProject() error = %v", err)
	}

	_, err = service.AddProject(ctx, AddInput{
		ID:         "looper",
		Name:       "Looper Again",
		RepoPath:   "/tmp/looper-again",
		BaseBranch: "main",
		IDSource:   "explicit",
	})
	if err == nil {
		t.Fatal("duplicate AddProject() error = nil, want ProjectIDCollisionError")
	}

	var conflict ProjectIDCollisionError
	if !errors.As(err, &conflict) {
		t.Fatalf("duplicate AddProject() error = %T, want ProjectIDCollisionError", err)
	}
	if conflict.ProjectID != "looper" {
		t.Fatalf("conflict.ProjectID = %q, want looper", conflict.ProjectID)
	}
}

func TestServiceSyncConfiguredPreservesMetadataLayout(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	backupDir := t.TempDir()
	coordinator, err := storage.OpenSQLiteCoordinator(context.Background(), filepath.Join(workingDir, "projects.sqlite"), storage.SQLiteCoordinatorOptions{BackupDir: backupDir})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	if _, err := coordinator.MigrationRunner().RunPending(context.Background()); err != nil {
		t.Fatalf("RunPending() error = %v", err)
	}

	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 17, 12, 34, 56, 0, time.UTC)
	service := &Service{DB: coordinator.DB(), Repos: repos, Now: func() time.Time { return now }}
	worktreeRoot := filepath.Join(workingDir, "worktrees")
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Projects = []config.ProjectRefConfig{{
		ID:           "project_1",
		Name:         "Looper",
		RepoPath:     filepath.Join(workingDir, "repo"),
		WorktreeRoot: &worktreeRoot,
	}}

	if err := service.SyncConfigured(context.Background(), cfg, now); err != nil {
		t.Fatalf("SyncConfigured() error = %v", err)
	}
	project, err := repos.Projects.GetByID(context.Background(), "project_1")
	if err != nil {
		t.Fatalf("Projects.GetByID() error = %v", err)
	}
	if project == nil || project.MetadataJSON == nil || *project.MetadataJSON != `{"repo":null,"worktreeRoot":"`+worktreeRoot+`","source":"config"}` {
		t.Fatalf("project.MetadataJSON = %#v, want ordered config metadata", project)
	}
}

func TestServiceSyncConfiguredRefreshesTransferredRepoMetadata(t *testing.T) {
	t.Parallel()

	coordinator := openCoordinator(t)
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.May, 8, 12, 0, 0, 0, time.UTC)
	nowISO := now.UTC().Format(time.RFC3339Nano)
	repoPath := "/tmp/looper"
	baseBranch := "main"
	metadata := `{"repo":"powerformer/looper","worktreeRoot":null,"source":"config"}`
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "looper", Name: "Looper", RepoPath: repoPath, BaseBranch: &baseBranch, MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	service := &Service{
		Repos: repos,
		Now:   func() time.Time { return now },
		DetectRepo: func(context.Context, string) (DetectedRepo, error) {
			return DetectedRepo{Repo: "nexu-io/looper"}, nil
		},
	}
	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Projects = []config.ProjectRefConfig{{ID: "looper", Name: "Looper", RepoPath: repoPath, BaseBranch: &baseBranch}}

	if err := service.SyncConfigured(context.Background(), cfg, now); err != nil {
		t.Fatalf("SyncConfigured() error = %v", err)
	}
	project, err := repos.Projects.GetByID(context.Background(), "looper")
	if err != nil {
		t.Fatalf("Projects.GetByID() error = %v", err)
	}
	if project == nil || project.MetadataJSON == nil || *project.MetadataJSON != `{"repo":"nexu-io/looper","worktreeRoot":null,"source":"config"}` {
		t.Fatalf("project.MetadataJSON = %#v, want refreshed transferred repo metadata", project)
	}
}

func TestServiceSyncConfiguredPreservesRepoMetadataWhenDetectionReturnsEmpty(t *testing.T) {
	t.Parallel()

	coordinator := openCoordinator(t)
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.May, 8, 12, 0, 0, 0, time.UTC)
	nowISO := now.UTC().Format(time.RFC3339Nano)
	repoPath := "/tmp/looper"
	baseBranch := "main"
	metadata := `{"repo":"powerformer/looper","worktreeRoot":null,"source":"config"}`
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "looper", Name: "Looper", RepoPath: repoPath, BaseBranch: &baseBranch, MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	service := &Service{
		Repos: repos,
		Now:   func() time.Time { return now },
		DetectRepo: func(context.Context, string) (DetectedRepo, error) {
			return DetectedRepo{}, nil
		},
	}
	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Projects = []config.ProjectRefConfig{{ID: "looper", Name: "Looper", RepoPath: repoPath, BaseBranch: &baseBranch}}

	if err := service.SyncConfigured(context.Background(), cfg, now); err != nil {
		t.Fatalf("SyncConfigured() error = %v", err)
	}
	project, err := repos.Projects.GetByID(context.Background(), "looper")
	if err != nil {
		t.Fatalf("Projects.GetByID() error = %v", err)
	}
	if project == nil || project.MetadataJSON == nil || *project.MetadataJSON != metadata {
		t.Fatalf("project.MetadataJSON = %#v, want preserved repo metadata", project)
	}
}

func TestServiceSyncConfiguredLeavesRepoMetadataNilWhenDetectionReturnsEmptyWithoutExistingRepo(t *testing.T) {
	t.Parallel()

	coordinator := openCoordinator(t)
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.May, 8, 12, 0, 0, 0, time.UTC)
	nowISO := now.UTC().Format(time.RFC3339Nano)
	repoPath := "/tmp/looper"
	baseBranch := "main"
	metadata := `{"repo":null,"worktreeRoot":null,"source":"config"}`
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "looper", Name: "Looper", RepoPath: repoPath, BaseBranch: &baseBranch, MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	service := &Service{
		Repos: repos,
		Now:   func() time.Time { return now },
		DetectRepo: func(context.Context, string) (DetectedRepo, error) {
			return DetectedRepo{}, nil
		},
	}
	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Projects = []config.ProjectRefConfig{{ID: "looper", Name: "Looper", RepoPath: repoPath, BaseBranch: &baseBranch}}

	if err := service.SyncConfigured(context.Background(), cfg, now); err != nil {
		t.Fatalf("SyncConfigured() error = %v", err)
	}
	project, err := repos.Projects.GetByID(context.Background(), "looper")
	if err != nil {
		t.Fatalf("Projects.GetByID() error = %v", err)
	}
	if project == nil || project.MetadataJSON == nil || *project.MetadataJSON != metadata {
		t.Fatalf("project.MetadataJSON = %#v, want nil repo metadata", project)
	}
}

func TestServiceSyncConfiguredPreservesRepoMetadataWhenDetectionFails(t *testing.T) {
	t.Parallel()

	coordinator := openCoordinator(t)
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.May, 8, 12, 0, 0, 0, time.UTC)
	nowISO := now.UTC().Format(time.RFC3339Nano)
	repoPath := "/tmp/looper"
	baseBranch := "main"
	metadata := `{"repo":"powerformer/looper","worktreeRoot":null,"source":"config"}`
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "looper", Name: "Looper", RepoPath: repoPath, BaseBranch: &baseBranch, MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	service := &Service{
		Repos: repos,
		Now:   func() time.Time { return now },
		DetectRepo: func(context.Context, string) (DetectedRepo, error) {
			return DetectedRepo{}, errors.New("git unavailable")
		},
	}
	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Projects = []config.ProjectRefConfig{{ID: "looper", Name: "Looper", RepoPath: repoPath, BaseBranch: &baseBranch}}

	if err := service.SyncConfigured(context.Background(), cfg, now); err != nil {
		t.Fatalf("SyncConfigured() error = %v", err)
	}
	project, err := repos.Projects.GetByID(context.Background(), "looper")
	if err != nil {
		t.Fatalf("Projects.GetByID() error = %v", err)
	}
	if project == nil || project.MetadataJSON == nil || *project.MetadataJSON != metadata {
		t.Fatalf("project.MetadataJSON = %#v, want preserved repo metadata after detection failure", project)
	}
}

func TestServiceSyncConfiguredDoesNotDeleteUnlistedProjects(t *testing.T) {
	t.Parallel()

	coordinator := openCoordinator(t)
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 21, 7, 46, 20, 0, time.UTC)
	nowISO := now.UTC().Format(time.RFC3339Nano)
	baseBranch := "main"
	metadata := `{"repo":"nexu-io/looper","source":"api"}`
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "looper", Name: "Looper", RepoPath: "/tmp/looper", BaseBranch: &baseBranch, MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	service := &Service{DB: coordinator.DB(), Repos: repos, Now: func() time.Time { return now }}
	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Projects = []config.ProjectRefConfig{{ID: "other", Name: "Other", RepoPath: "/tmp/other"}}

	if err := service.SyncConfigured(context.Background(), cfg, now); err != nil {
		t.Fatalf("SyncConfigured() error = %v", err)
	}
	project, err := repos.Projects.GetByID(context.Background(), "looper")
	if err != nil {
		t.Fatalf("Projects.GetByID() error = %v", err)
	}
	if project == nil || project.MetadataJSON == nil || *project.MetadataJSON != metadata {
		t.Fatalf("project = %#v, want existing project preserved", project)
	}
	other, err := repos.Projects.GetByID(context.Background(), "other")
	if err != nil {
		t.Fatalf("Projects.GetByID(other) error = %v", err)
	}
	if other == nil || other.Name != "Other" {
		t.Fatalf("other = %#v, want configured project upserted", other)
	}
}

func TestServiceSyncConfiguredArchivesConfigProjectsRemovedFromConfig(t *testing.T) {
	t.Parallel()

	coordinator := openCoordinator(t)
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.July, 12, 10, 0, 0, 0, time.UTC)
	createdAt := now.Add(-time.Hour).Format(time.RFC3339Nano)
	configMetadata := `{"repo":"nexu-io/removed","source":"config"}`
	apiMetadata := `{"repo":"nexu-io/api","source":"api"}`
	baseBranch := "main"
	for _, project := range []storage.ProjectRecord{
		{ID: "removed", Name: "Removed", RepoPath: "/tmp/removed", BaseBranch: &baseBranch, MetadataJSON: &configMetadata, CreatedAt: createdAt, UpdatedAt: createdAt},
		{ID: "api-project", Name: "API", RepoPath: "/tmp/api", BaseBranch: &baseBranch, MetadataJSON: &apiMetadata, CreatedAt: createdAt, UpdatedAt: createdAt},
	} {
		if err := repos.Projects.Upsert(context.Background(), project); err != nil {
			t.Fatalf("Projects.Upsert(%s) error = %v", project.ID, err)
		}
	}
	targetID := "pr:nexu-io/removed:531"
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: "loop_removed", Seq: 1, ProjectID: "removed", Type: string(domain.LoopTypeReviewer), TargetType: string(domain.LoopTargetTypePullRequest), TargetID: &targetID, Status: string(domain.LoopStatusQueued), CreatedAt: createdAt, UpdatedAt: createdAt}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	if err := repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: "queue_removed", ProjectID: stringPointer("removed"), LoopID: stringPointer("loop_removed"), Type: "reviewer", TargetType: "pull_request", TargetID: targetID, DedupeKey: "reviewer:removed:loop_removed", Priority: storage.QueuePriorityReviewer, Status: "queued", AvailableAt: createdAt, MaxAttempts: 3, CreatedAt: createdAt, UpdatedAt: createdAt}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	service := &Service{DB: coordinator.DB(), Repos: repos, Now: func() time.Time { return now }}
	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Projects = nil

	if err := service.SyncConfigured(context.Background(), cfg, now); err != nil {
		t.Fatalf("SyncConfigured() error = %v", err)
	}
	removed, err := repos.Projects.GetByID(context.Background(), "removed")
	if err != nil {
		t.Fatalf("Projects.GetByID(removed) error = %v", err)
	}
	if removed == nil || !removed.Archived || removed.UpdatedAt != currentISO(func() time.Time { return now }) {
		t.Fatalf("removed = %#v, want archived config project at import time", removed)
	}
	removedLoop, err := repos.Loops.GetByID(context.Background(), "loop_removed")
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if removedLoop == nil || removedLoop.Status != string(domain.LoopStatusTerminated) {
		t.Fatalf("removed loop = %#v, want terminated", removedLoop)
	}
	removedQueue, err := repos.Queue.GetByID(context.Background(), "queue_removed")
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	if removedQueue == nil || removedQueue.Status != "cancelled" || removedQueue.LastError == nil || *removedQueue.LastError != "project archived" {
		t.Fatalf("removed queue = %#v, want cancelled with archive reason", removedQueue)
	}
	apiProject, err := repos.Projects.GetByID(context.Background(), "api-project")
	if err != nil {
		t.Fatalf("Projects.GetByID(api-project) error = %v", err)
	}
	if apiProject == nil || apiProject.Archived || apiProject.UpdatedAt != createdAt {
		t.Fatalf("api project = %#v, want untouched active API project", apiProject)
	}
}

func TestServiceSyncConfiguredRejectsAPIManagedIDCollision(t *testing.T) {
	t.Parallel()

	coordinator := openCoordinator(t)
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.July, 12, 10, 0, 0, 0, time.UTC)
	createdAt := now.Add(-time.Hour).Format(time.RFC3339Nano)
	baseBranch := "main"
	metadata := `{"repo":"nexu-io/api","source":"api"}`
	existing := storage.ProjectRecord{ID: "shared", Name: "API Project", RepoPath: "/tmp/api", BaseBranch: &baseBranch, MetadataJSON: &metadata, CreatedAt: createdAt, UpdatedAt: createdAt}
	if err := repos.Projects.Upsert(context.Background(), existing); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	service := &Service{DB: coordinator.DB(), Repos: repos, Now: func() time.Time { return now }}
	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Projects = []config.ProjectRefConfig{{ID: "shared", Name: "Configured Project", RepoPath: "/tmp/config"}}

	err = service.SyncConfigured(context.Background(), cfg, now)
	var validationErr ProjectValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("SyncConfigured() error = %T %v, want ProjectValidationError", err, err)
	}
	if !strings.Contains(err.Error(), "conflicts with an API-managed project") {
		t.Fatalf("SyncConfigured() error = %q, want API ownership conflict", err)
	}
	stored, getErr := repos.Projects.GetByID(context.Background(), "shared")
	if getErr != nil {
		t.Fatalf("Projects.GetByID() error = %v", getErr)
	}
	if stored == nil || stored.Name != existing.Name || stored.RepoPath != existing.RepoPath || stored.UpdatedAt != existing.UpdatedAt || stored.MetadataJSON == nil || *stored.MetadataJSON != metadata {
		t.Fatalf("stored = %#v, want API project unchanged", stored)
	}
}

func TestServiceRemoveProjectArchivesProjectAndPreservesHistory(t *testing.T) {
	t.Parallel()

	coordinator := openCoordinator(t)
	ctx := context.Background()
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.June, 11, 12, 0, 0, 0, time.UTC)
	nowISO := now.UTC().Format(time.RFC3339Nano)
	baseBranch := "main"
	metadata := `{"repo":"nexu-io/looper","source":"api"}`
	if err := repos.Projects.Upsert(ctx, storage.ProjectRecord{ID: "looper", Name: "Looper", RepoPath: "/tmp/looper", BaseBranch: &baseBranch, MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := repos.Loops.Upsert(ctx, storage.LoopRecord{ID: "loop_1", Seq: 1, ProjectID: "looper", Type: "reviewer", TargetType: "pull_request", Status: "idle", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	service := &Service{DB: coordinator.DB(), Repos: repos, Now: func() time.Time { return now.Add(time.Minute) }}

	removed, err := service.RemoveProject(ctx, "looper")
	if err != nil {
		t.Fatalf("RemoveProject() error = %v", err)
	}
	if !removed.Archived {
		t.Fatalf("RemoveProject().Archived = %v, want true", removed.Archived)
	}

	stored, err := repos.Projects.GetByID(ctx, "looper")
	if err != nil {
		t.Fatalf("Projects.GetByID() error = %v", err)
	}
	if stored == nil || !stored.Archived {
		t.Fatalf("stored project = %#v, want archived project", stored)
	}
	wantUpdatedAt := currentISO(func() time.Time { return now.Add(time.Minute) })
	if stored.UpdatedAt != wantUpdatedAt {
		t.Fatalf("stored.UpdatedAt = %q, want %q", stored.UpdatedAt, wantUpdatedAt)
	}
	loop, err := repos.Loops.GetByID(ctx, "loop_1")
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if loop == nil || loop.ProjectID != "looper" {
		t.Fatalf("loop = %#v, want preserved loop history", loop)
	}
}

func TestServiceRemoveProjectCancelsActiveProjectQueueItems(t *testing.T) {
	t.Parallel()

	coordinator := openCoordinator(t)
	ctx := context.Background()
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.June, 11, 12, 0, 0, 0, time.UTC)
	nowISO := now.UTC().Format(time.RFC3339Nano)
	repoName := "acme/looper"
	prNumber := int64(42)
	runningPRNumber := int64(43)
	dedupeKey := "worker:acme/looper:42"
	runningDedupeKey := "worker:acme/looper:43"
	if err := repos.Projects.Upsert(ctx, storage.ProjectRecord{ID: "looper", Name: "Looper", RepoPath: "/tmp/looper", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert(looper) error = %v", err)
	}
	if err := repos.Projects.Upsert(ctx, storage.ProjectRecord{ID: "other", Name: "Other", RepoPath: "/tmp/other", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert(other) error = %v", err)
	}
	if err := repos.Queue.Upsert(ctx, storage.QueueItemRecord{ID: "queue_looper", ProjectID: stringPointer("looper"), Type: "worker", TargetType: "pull_request", TargetID: "pr:42", Repo: &repoName, PRNumber: &prNumber, DedupeKey: dedupeKey, Priority: storage.QueuePriorityWorker, Status: "queued", AvailableAt: nowISO, Attempts: 0, MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Queue.Upsert(queue_looper) error = %v", err)
	}
	if err := repos.Queue.Upsert(ctx, storage.QueueItemRecord{ID: "queue_running", ProjectID: stringPointer("looper"), Type: "worker", TargetType: "pull_request", TargetID: "pr:43", Repo: &repoName, PRNumber: &runningPRNumber, DedupeKey: runningDedupeKey, Priority: storage.QueuePriorityWorker, Status: "running", AvailableAt: nowISO, Attempts: 1, MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Queue.Upsert(queue_running) error = %v", err)
	}

	service := &Service{DB: coordinator.DB(), Repos: repos, Now: func() time.Time { return now.Add(time.Minute) }}

	removed, err := service.RemoveProject(ctx, "looper")
	if err != nil {
		t.Fatalf("RemoveProject() error = %v", err)
	}
	if !removed.Archived {
		t.Fatalf("RemoveProject().Archived = %v, want true", removed.Archived)
	}

	for _, id := range []string{"queue_looper", "queue_running"} {
		item, err := repos.Queue.GetByID(ctx, id)
		if err != nil {
			t.Fatalf("Queue.GetByID(%s) error = %v", id, err)
		}
		if item == nil || item.Status != "cancelled" || item.FinishedAt == nil || item.LastError == nil || *item.LastError != "project archived" {
			t.Fatalf("Queue.GetByID(%s) = %#v, want cancelled item with archive reason", id, item)
		}
	}

	if err := repos.Queue.MarkRetry(ctx, storage.QueueMarkRetryInput{ID: "queue_running", AvailableAt: nowISO, Attempts: 2, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Queue.MarkRetry(queue_running) error = %v", err)
	}
	retried, err := repos.Queue.GetByID(ctx, "queue_running")
	if err != nil {
		t.Fatalf("Queue.GetByID(queue_running after retry) error = %v", err)
	}
	if retried == nil || retried.Status != "cancelled" {
		t.Fatalf("Queue.GetByID(queue_running after retry) = %#v, want cancelled", retried)
	}

	if active, err := repos.Queue.FindActiveByDedupe(ctx, dedupeKey); err != nil {
		t.Fatalf("Queue.FindActiveByDedupe() error = %v", err)
	} else if active != nil {
		t.Fatalf("Queue.FindActiveByDedupe() = %#v, want nil after archive", active)
	}
	if active, err := repos.Queue.FindActiveByDedupe(ctx, runningDedupeKey); err != nil {
		t.Fatalf("Queue.FindActiveByDedupe(running) error = %v", err)
	} else if active != nil {
		t.Fatalf("Queue.FindActiveByDedupe(running) = %#v, want nil after archive", active)
	}

	created, didCreate, err := repos.Queue.CreateOrGetActiveByDedupe(ctx, storage.QueueItemRecord{ID: "queue_other", ProjectID: stringPointer("other"), Type: "worker", TargetType: "pull_request", TargetID: "pr:42", Repo: &repoName, PRNumber: &prNumber, DedupeKey: dedupeKey, Priority: storage.QueuePriorityWorker, Status: "queued", AvailableAt: nowISO, Attempts: 0, MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO})
	if err != nil {
		t.Fatalf("Queue.CreateOrGetActiveByDedupe() error = %v", err)
	}
	if !didCreate || created.ID != "queue_other" {
		t.Fatalf("Queue.CreateOrGetActiveByDedupe() = (%#v, %v), want created queue_other", created, didCreate)
	}
}

func TestServiceRemoveProjectTerminatesActiveLoopsBeforeReactivation(t *testing.T) {
	t.Parallel()

	coordinator := openCoordinator(t)
	ctx := context.Background()
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.June, 11, 12, 0, 0, 0, time.UTC)
	nowISO := now.UTC().Format(time.RFC3339Nano)
	if err := repos.Projects.Upsert(ctx, storage.ProjectRecord{ID: "looper", Name: "Looper", RepoPath: "/tmp/looper", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	prNumber := int64(42)
	repoName := "acme/looper"
	if err := repos.Loops.Upsert(ctx, storage.LoopRecord{ID: "loop_1", Seq: 1, ProjectID: "looper", Type: string(domain.LoopTypeReviewer), TargetType: string(domain.LoopTargetTypePullRequest), Repo: &repoName, PRNumber: &prNumber, Status: string(domain.LoopStatusRunning), CreatedAt: nowISO, UpdatedAt: nowISO, NextRunAt: stringPointer(nowISO)}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	service := &Service{DB: coordinator.DB(), Repos: repos, Now: func() time.Time { return now.Add(time.Minute) }}
	removed, err := service.RemoveProject(ctx, "looper")
	if err != nil {
		t.Fatalf("RemoveProject() error = %v", err)
	}
	if !removed.Archived {
		t.Fatalf("RemoveProject().Archived = %v, want true", removed.Archived)
	}

	loop, err := repos.Loops.GetByID(ctx, "loop_1")
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if loop == nil || loop.Status != string(domain.LoopStatusTerminated) || loop.NextRunAt != nil {
		t.Fatalf("archived loop = %#v, want terminated with cleared next_run_at", loop)
	}

	if _, err := service.AddProject(ctx, AddInput{ID: "looper", Name: "Looper", RepoPath: "/tmp/looper", BaseBranch: "main"}); err != nil {
		t.Fatalf("AddProject() error = %v", err)
	}

	loopService := &loops.Service{DB: coordinator.DB(), Repos: repos, Now: func() time.Time { return now.Add(2 * time.Minute) }}
	created, err := loopService.Create(ctx, loops.CreateInput{
		ProjectID: "looper",
		Type:      domain.LoopTypeReviewer,
		Target:    domain.LoopTarget{TargetType: domain.LoopTargetTypePullRequest, Repo: repoName, PRNumber: prNumber},
		Status:    domain.LoopStatusQueued,
	})
	if err != nil {
		t.Fatalf("loops.Create() error = %v", err)
	}
	if created.ProjectID != "looper" || created.Status != string(domain.LoopStatusQueued) {
		t.Fatalf("loops.Create() = %#v, want queued loop for reactivated project", created)
	}
}

func TestServiceRemoveProjectTerminatesFailedAndInterruptedLoopsAndCancelsRecoverableQueueItems(t *testing.T) {
	t.Parallel()

	coordinator := openCoordinator(t)
	ctx := context.Background()
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.June, 11, 12, 0, 0, 0, time.UTC)
	nowISO := now.UTC().Format(time.RFC3339Nano)
	if err := repos.Projects.Upsert(ctx, storage.ProjectRecord{ID: "looper", Name: "Looper", RepoPath: "/tmp/looper", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	repoName := "acme/looper"
	prNumber := int64(42)
	failedTargetID := "pr:acme/looper:42"
	if err := repos.Loops.Upsert(ctx, storage.LoopRecord{ID: "loop_failed", Seq: 1, ProjectID: "looper", Type: string(domain.LoopTypeReviewer), TargetType: string(domain.LoopTargetTypePullRequest), TargetID: &failedTargetID, Repo: &repoName, PRNumber: &prNumber, Status: string(domain.LoopStatusFailed), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert(loop_failed) error = %v", err)
	}
	interruptedTargetID := "pr:acme/looper:44"
	interruptedPRNumber := int64(44)
	if err := repos.Loops.Upsert(ctx, storage.LoopRecord{ID: "loop_interrupted", Seq: 3, ProjectID: "looper", Type: string(domain.LoopTypeReviewer), TargetType: string(domain.LoopTargetTypePullRequest), TargetID: &interruptedTargetID, Repo: &repoName, PRNumber: &interruptedPRNumber, Status: string(domain.LoopStatusInterrupted), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert(loop_interrupted) error = %v", err)
	}
	manualTargetID := "pr:acme/looper:43"
	manualPRNumber := int64(43)
	if err := repos.Loops.Upsert(ctx, storage.LoopRecord{ID: "loop_manual", Seq: 2, ProjectID: "looper", Type: string(domain.LoopTypeReviewer), TargetType: string(domain.LoopTargetTypePullRequest), TargetID: &manualTargetID, Repo: &repoName, PRNumber: &manualPRNumber, Status: string(domain.LoopStatusPaused), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert(loop_manual) error = %v", err)
	}
	errorMessage := "review failed"
	errorKind := "retryable_transient"
	if err := repos.Queue.Upsert(ctx, storage.QueueItemRecord{ID: "queue_failed", ProjectID: stringPointer("looper"), LoopID: stringPointer("loop_failed"), Type: "reviewer", TargetType: "pull_request", TargetID: failedTargetID, Repo: &repoName, PRNumber: &prNumber, DedupeKey: "reviewer:project_1:loop_failed:acme/looper:42", Priority: storage.QueuePriorityReviewer, Status: "failed", AvailableAt: nowISO, Attempts: 1, MaxAttempts: 5, LastError: &errorMessage, LastErrorKind: &errorKind, FinishedAt: stringPointer(nowISO), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Queue.Upsert(queue_failed) error = %v", err)
	}
	manualErrorKind := "manual_intervention"
	manualError := "needs manual follow-up"
	if err := repos.Queue.Upsert(ctx, storage.QueueItemRecord{ID: "queue_manual", ProjectID: stringPointer("looper"), LoopID: stringPointer("loop_manual"), Type: "reviewer", TargetType: "pull_request", TargetID: manualTargetID, Repo: &repoName, PRNumber: &manualPRNumber, DedupeKey: "reviewer:project_1:loop_manual:acme/looper:43", Priority: storage.QueuePriorityReviewer, Status: "manual_intervention", AvailableAt: nowISO, Attempts: 1, MaxAttempts: 5, LastError: &manualError, LastErrorKind: &manualErrorKind, FinishedAt: stringPointer(nowISO), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Queue.Upsert(queue_manual) error = %v", err)
	}

	service := &Service{DB: coordinator.DB(), Repos: repos, Now: func() time.Time { return now.Add(time.Minute) }}
	removed, err := service.RemoveProject(ctx, "looper")
	if err != nil {
		t.Fatalf("RemoveProject() error = %v", err)
	}
	if !removed.Archived {
		t.Fatalf("RemoveProject().Archived = %v, want true", removed.Archived)
	}

	failedLoop, err := repos.Loops.GetByID(ctx, "loop_failed")
	if err != nil {
		t.Fatalf("Loops.GetByID(loop_failed) error = %v", err)
	}
	if failedLoop == nil || failedLoop.Status != string(domain.LoopStatusTerminated) {
		t.Fatalf("failed loop = %#v, want terminated", failedLoop)
	}
	interruptedLoop, err := repos.Loops.GetByID(ctx, "loop_interrupted")
	if err != nil {
		t.Fatalf("Loops.GetByID(loop_interrupted) error = %v", err)
	}
	if interruptedLoop == nil || interruptedLoop.Status != string(domain.LoopStatusTerminated) {
		t.Fatalf("interrupted loop = %#v, want terminated", interruptedLoop)
	}
	manualLoop, err := repos.Loops.GetByID(ctx, "loop_manual")
	if err != nil {
		t.Fatalf("Loops.GetByID(loop_manual) error = %v", err)
	}
	if manualLoop == nil || manualLoop.Status != string(domain.LoopStatusTerminated) {
		t.Fatalf("manual loop = %#v, want terminated", manualLoop)
	}

	failedQueue, err := repos.Queue.GetByID(ctx, "queue_failed")
	if err != nil {
		t.Fatalf("Queue.GetByID(queue_failed) error = %v", err)
	}
	if failedQueue == nil || failedQueue.Status != "cancelled" || failedQueue.FinishedAt == nil || failedQueue.LastError == nil || *failedQueue.LastError != "project archived" {
		t.Fatalf("failed queue = %#v, want cancelled archived queue item", failedQueue)
	}
	manualQueue, err := repos.Queue.GetByID(ctx, "queue_manual")
	if err != nil {
		t.Fatalf("Queue.GetByID(queue_manual) error = %v", err)
	}
	if manualQueue == nil || manualQueue.Status != "cancelled" || manualQueue.FinishedAt == nil || manualQueue.LastError == nil || *manualQueue.LastError != "project archived" {
		t.Fatalf("manual queue = %#v, want cancelled archived queue item", manualQueue)
	}
}

func TestServiceListSkipsArchivedProjects(t *testing.T) {
	t.Parallel()

	coordinator := openCoordinator(t)
	ctx := context.Background()
	repos := storage.NewRepositories(coordinator.DB())
	nowISO := time.Date(2026, time.June, 11, 12, 0, 0, 0, time.UTC).UTC().Format(time.RFC3339Nano)
	if err := repos.Projects.Upsert(ctx, storage.ProjectRecord{ID: "active", Name: "Active", RepoPath: "/tmp/active", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert(active) error = %v", err)
	}
	if err := repos.Projects.Upsert(ctx, storage.ProjectRecord{ID: "archived", Name: "Archived", RepoPath: "/tmp/archived", Archived: true, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert(archived) error = %v", err)
	}

	service := &Service{DB: coordinator.DB(), Repos: repos}
	items, err := service.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(items) != 1 || items[0].ID != "active" {
		t.Fatalf("List() = %#v, want only active project", items)
	}
}

func TestServiceRemoveProjectTreatsArchivedProjectAsNotFound(t *testing.T) {
	t.Parallel()

	coordinator := openCoordinator(t)
	ctx := context.Background()
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.June, 11, 12, 0, 0, 0, time.UTC)
	nowISO := now.UTC().Format(time.RFC3339Nano)
	if err := repos.Projects.Upsert(ctx, storage.ProjectRecord{ID: "looper", Name: "Looper", RepoPath: "/tmp/looper", Archived: true, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	service := &Service{DB: coordinator.DB(), Repos: repos, Now: func() time.Time { return now.Add(time.Minute) }}
	_, err := service.RemoveProject(ctx, "looper")
	if err == nil {
		t.Fatal("RemoveProject(id) error = nil, want not found")
	}
	var notFound ProjectNotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("RemoveProject(id) error = %T, want ProjectNotFoundError", err)
	}
	_, err = service.RemoveProject(ctx, "Looper")
	if err == nil {
		t.Fatal("RemoveProject(name) error = nil, want not found")
	}
	if !errors.As(err, &notFound) {
		t.Fatalf("RemoveProject(name) error = %T, want ProjectNotFoundError", err)
	}
}

func TestServiceRemoveProjectDoesNotFallbackToNameAfterArchivedIDMatch(t *testing.T) {
	t.Parallel()

	coordinator := openCoordinator(t)
	ctx := context.Background()
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.June, 11, 12, 0, 0, 0, time.UTC)
	nowISO := now.UTC().Format(time.RFC3339Nano)
	if err := repos.Projects.Upsert(ctx, storage.ProjectRecord{ID: "foo", Name: "Archived", RepoPath: "/tmp/archived", Archived: true, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert(foo archived) error = %v", err)
	}
	if err := repos.Projects.Upsert(ctx, storage.ProjectRecord{ID: "active", Name: "foo", RepoPath: "/tmp/active", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert(active) error = %v", err)
	}

	service := &Service{DB: coordinator.DB(), Repos: repos, Now: func() time.Time { return now.Add(time.Minute) }}
	_, err := service.RemoveProject(ctx, "foo")
	if err == nil {
		t.Fatal("RemoveProject() error = nil, want not found")
	}
	var notFound ProjectNotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("RemoveProject() error = %T, want ProjectNotFoundError", err)
	}

	active, err := repos.Projects.GetByID(ctx, "active")
	if err != nil {
		t.Fatalf("Projects.GetByID(active) error = %v", err)
	}
	if active == nil || active.Archived {
		t.Fatalf("active project = %#v, want still active", active)
	}
	archived, err := repos.Projects.GetByID(ctx, "foo")
	if err != nil {
		t.Fatalf("Projects.GetByID(foo) error = %v", err)
	}
	if archived == nil || !archived.Archived {
		t.Fatalf("archived project = %#v, want unchanged archived project", archived)
	}
}

func TestServiceAddProjectReactivatesArchivedExplicitID(t *testing.T) {
	t.Parallel()

	coordinator := openCoordinator(t)
	ctx := context.Background()
	repos := storage.NewRepositories(coordinator.DB())
	nowISO := time.Date(2026, time.June, 11, 12, 0, 0, 0, time.UTC).UTC().Format(time.RFC3339Nano)
	if err := repos.Projects.Upsert(ctx, storage.ProjectRecord{ID: "looper", Name: "Old", RepoPath: "/tmp/old", Archived: true, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	service := &Service{DB: coordinator.DB(), Repos: repos}
	result, err := service.AddProject(ctx, AddInput{ID: "looper", Name: "Looper", RepoPath: "/tmp/looper", BaseBranch: "main"})
	if err != nil {
		t.Fatalf("AddProject() error = %v", err)
	}
	if result.Project.Archived {
		t.Fatalf("AddProject().Project.Archived = %v, want false", result.Project.Archived)
	}
}

func TestServiceRemoveProjectRejectsConfigManagedProject(t *testing.T) {
	t.Parallel()

	coordinator := openCoordinator(t)
	ctx := context.Background()
	repos := storage.NewRepositories(coordinator.DB())
	nowISO := time.Date(2026, time.June, 11, 12, 0, 0, 0, time.UTC).UTC().Format(time.RFC3339Nano)
	metadata := `{"repo":null,"worktreeRoot":null,"source":"config"}`
	if err := repos.Projects.Upsert(ctx, storage.ProjectRecord{ID: "looper", Name: "Looper", RepoPath: "/tmp/looper", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	service := &Service{DB: coordinator.DB(), Repos: repos}
	_, err := service.RemoveProject(ctx, "looper")
	if err == nil {
		t.Fatal("RemoveProject() error = nil, want validation error")
	}
	var validationErr ProjectValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("RemoveProject() error = %T, want ProjectValidationError", err)
	}

	stored, err := repos.Projects.GetByID(ctx, "looper")
	if err != nil {
		t.Fatalf("Projects.GetByID() error = %v", err)
	}
	if stored == nil || stored.Archived {
		t.Fatalf("stored project = %#v, want non-archived project", stored)
	}
}

func TestServiceAddProjectRejectsConfigManagedProject(t *testing.T) {
	t.Parallel()

	coordinator := openCoordinator(t)
	repos := storage.NewRepositories(coordinator.DB())
	metadata := `{"repo":"acme/configured","source":"config"}`
	nowISO := "2026-07-12T10:00:00.000Z"
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID: "configured", Name: "Configured", RepoPath: "/repos/configured", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	service := &Service{DB: coordinator.DB(), Repos: repos}
	_, err := service.AddProject(context.Background(), AddInput{ID: "configured", IDSource: "derived", Name: "Changed", RepoPath: "/repos/changed"})
	if err == nil || !strings.Contains(err.Error(), "managed by config") {
		t.Fatalf("AddProject() error = %v, want config authority rejection", err)
	}
	stored, getErr := repos.Projects.GetByID(context.Background(), "configured")
	if getErr != nil {
		t.Fatalf("GetByID() error = %v", getErr)
	}
	if stored == nil || stored.Name != "Configured" || stored.RepoPath != "/repos/configured" {
		t.Fatalf("stored project = %#v, want unchanged config project", stored)
	}
}

func TestProjectsRepositoryArchiveMarksArchivedWithoutDeletingRow(t *testing.T) {
	t.Parallel()

	coordinator := openCoordinator(t)
	ctx := context.Background()
	repos := storage.NewRepositories(coordinator.DB())
	nowISO := time.Date(2026, time.June, 11, 12, 0, 0, 0, time.UTC).UTC().Format(time.RFC3339Nano)
	if err := repos.Projects.Upsert(ctx, storage.ProjectRecord{ID: "looper", Name: "Looper", RepoPath: "/tmp/looper", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	archivedAt := time.Date(2026, time.June, 11, 12, 5, 0, 0, time.UTC).UTC().Format(time.RFC3339Nano)

	ok, err := repos.Projects.Archive(ctx, "looper", archivedAt)
	if err != nil {
		t.Fatalf("Projects.Archive() error = %v", err)
	}
	if !ok {
		t.Fatal("Projects.Archive() = false, want true")
	}

	var archived int
	var updatedAt string
	row := coordinator.DB().QueryRowContext(ctx, `SELECT archived, updated_at FROM projects WHERE id = ?`, "looper")
	if err := row.Scan(&archived, &updatedAt); err != nil {
		t.Fatalf("scan archived project row: %v", err)
	}
	if archived != 1 || updatedAt != archivedAt {
		t.Fatalf("project row = archived:%d updated_at:%q, want 1/%q", archived, updatedAt, archivedAt)
	}
	var count int
	if err := coordinator.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM projects WHERE id = ?`, "looper").Scan(&count); err != nil {
		t.Fatalf("count archived project row: %v", err)
	}
	if count != 1 {
		t.Fatalf("project row count = %d, want 1", count)
	}
	if err := coordinator.DB().QueryRowContext(ctx, `SELECT archived FROM projects WHERE id = ?`, "missing").Scan(&archived); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing project query error = %v, want sql.ErrNoRows", err)
	}
}

func TestProjectsRepositoryArchiveSkipsArchivedProjectUpdates(t *testing.T) {
	t.Parallel()

	coordinator := openCoordinator(t)
	ctx := context.Background()
	repos := storage.NewRepositories(coordinator.DB())
	nowISO := time.Date(2026, time.June, 11, 12, 0, 0, 0, time.UTC).UTC().Format(time.RFC3339Nano)
	if err := repos.Projects.Upsert(ctx, storage.ProjectRecord{ID: "looper", Name: "Looper", RepoPath: "/tmp/looper", Archived: true, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	archivedAt := time.Date(2026, time.June, 11, 12, 5, 0, 0, time.UTC).UTC().Format(time.RFC3339Nano)

	ok, err := repos.Projects.Archive(ctx, "looper", archivedAt)
	if err != nil {
		t.Fatalf("Projects.Archive() error = %v", err)
	}
	if ok {
		t.Fatal("Projects.Archive() = true, want false for already archived project")
	}
	stored, err := repos.Projects.GetByID(ctx, "looper")
	if err != nil {
		t.Fatalf("Projects.GetByID() error = %v", err)
	}
	if stored == nil || stored.UpdatedAt != nowISO {
		t.Fatalf("stored = %#v, want unchanged updatedAt %q", stored, nowISO)
	}
}

func openCoordinator(t *testing.T) *storage.SQLiteCoordinator {
	t.Helper()
	coordinator, err := storage.OpenSQLiteCoordinator(context.Background(), filepath.Join(t.TempDir(), "service.sqlite"), storage.SQLiteCoordinatorOptions{BackupDir: t.TempDir()})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	if _, err := coordinator.MigrationRunner().RunPending(context.Background()); err != nil {
		t.Fatalf("RunPending() error = %v", err)
	}
	return coordinator
}
