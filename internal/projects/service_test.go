package projects

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/infra/shell"
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
		DetectRepo: func(context.Context, string) (string, error) { return "nexu-io/looper", nil },
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
		DetectRepo: func(context.Context, string) (string, error) {
			return "nexu-io/looper", nil
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
		DetectRepo: func(context.Context, string) (string, error) {
			return "", nil
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
		DetectRepo: func(context.Context, string) (string, error) {
			return "", nil
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
		DetectRepo: func(context.Context, string) (string, error) {
			return "", errors.New("git unavailable")
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
