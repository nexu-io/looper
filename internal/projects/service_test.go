package projects

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/powerformer/looper/internal/config"
	"github.com/powerformer/looper/internal/storage"
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
