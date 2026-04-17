package runtime

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"github.com/powerformer/looper/internal/config"
	"github.com/powerformer/looper/internal/storage"
)

func TestRuntimeStartOpensSQLiteAndSyncsConfiguredProjects(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}

	backupDir := t.TempDir()
	dbPath := workingDir + "/runtime.sqlite"
	worktreeRoot := workingDir + "/worktrees"
	startedAt := time.Date(2026, time.April, 17, 12, 34, 56, 0, time.UTC)

	cfg.Storage.DBPath = dbPath
	cfg.Storage.BackupDir = &backupDir
	cfg.Projects = []config.ProjectRefConfig{{
		ID:           "project_1",
		Name:         "Looper",
		RepoPath:     workingDir + "/repo",
		BaseBranch:   nil,
		WorktreeRoot: &worktreeRoot,
	}}

	rt := New(Options{
		Config: cfg,
		Logger: &testLogger{},
		Now: func() time.Time {
			return startedAt
		},
	})

	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() {
		rt.Stop("test cleanup")
	})

	services := rt.Services()
	if services.Coordinator == nil {
		t.Fatal("Services().Coordinator = nil, want initialized coordinator")
	}
	if services.Repositories == nil || services.Repositories.Projects == nil {
		t.Fatal("Services().Repositories.Projects = nil, want initialized repository set")
	}

	project, err := services.Repositories.Projects.GetByID(context.Background(), "project_1")
	if err != nil {
		t.Fatalf("Projects.GetByID() error = %v", err)
	}
	if project == nil {
		t.Fatal("Projects.GetByID() = nil, want synced project")
	}
	if project.BaseBranch == nil || *project.BaseBranch != cfg.Defaults.BaseBranch {
		t.Fatalf("project.BaseBranch = %v, want %q", project.BaseBranch, cfg.Defaults.BaseBranch)
	}
	wantMetadata := `{"repo":null,"worktreeRoot":"` + worktreeRoot + `","source":"config"}`
	if project.MetadataJSON == nil || *project.MetadataJSON != wantMetadata {
		t.Fatalf("project.MetadataJSON = %v, want %q", project.MetadataJSON, wantMetadata)
	}
	if project.CreatedAt != "2026-04-17T12:34:56.000Z" {
		t.Fatalf("project.CreatedAt = %q, want startup timestamp", project.CreatedAt)
	}
	if project.UpdatedAt != "2026-04-17T12:34:56.000Z" {
		t.Fatalf("project.UpdatedAt = %q, want startup timestamp", project.UpdatedAt)
	}
	if got, ok := rt.StartedAt(); !ok || !got.Equal(startedAt) {
		t.Fatalf("StartedAt() = (%v, %t), want (%v, true)", got, ok, startedAt)
	}
}

func TestRuntimeStartIsIdempotent(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}

	cfg.Storage.DBPath = workingDir + "/runtime.sqlite"

	openCalls := 0
	rt := New(Options{
		Config: cfg,
		Logger: &testLogger{},
		OpenSQLiteCoordinator: func(ctx context.Context, dbPath string, options storage.SQLiteCoordinatorOptions) (*storage.SQLiteCoordinator, error) {
			openCalls++
			return storage.OpenSQLiteCoordinator(ctx, dbPath, options)
		},
	})

	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("first Start() error = %v", err)
	}
	defer rt.Stop("test")

	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("second Start() error = %v", err)
	}

	if openCalls != 1 {
		t.Fatalf("openSQLiteCoordinator call count = %d, want 1", openCalls)
	}
	if _, ok := rt.StartedAt(); !ok {
		t.Fatal("StartedAt() ok = false, want true")
	}
}

func TestRuntimeStopClosesCoordinatorAndUnblocksWaitForShutdown(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}

	cfg.Storage.DBPath = workingDir + "/runtime.sqlite"

	rt := New(Options{Config: cfg, Logger: &testLogger{}})
	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		rt.WaitForShutdown()
	}()

	rt.Stop("SIGTERM")
	rt.Stop("SIGTERM")

	select {
	case <-shutdownDone:
	case <-time.After(2 * time.Second):
		t.Fatal("WaitForShutdown() did not return after Stop()")
	}

	services := rt.Services()
	if services.Coordinator != nil {
		t.Fatal("Services().Coordinator != nil after Stop(), want nil")
	}
	if services.Repositories != nil {
		t.Fatal("Services().Repositories != nil after Stop(), want nil")
	}
	db, err := sql.Open(storage.DriverName, cfg.Storage.DBPath)
	if err != nil {
		t.Fatalf("sql.Open() after Stop() error = %v", err)
	}
	defer db.Close()
}

func TestDefaultSyncConfiguredProjectsPreservesRepoMetadataWhenRepoPathIsUnchanged(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}

	cfg.Storage.DBPath = workingDir + "/runtime.sqlite"
	coordinator, err := storage.OpenSQLiteCoordinator(context.Background(), cfg.Storage.DBPath, storage.SQLiteCoordinatorOptions{})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	defer coordinator.Close()

	if _, err := coordinator.MigrationRunner().RunPending(context.Background()); err != nil {
		t.Fatalf("RunPending() error = %v", err)
	}

	repositories := storage.NewRepositories(coordinator.DB())
	repoPath := workingDir + "/repo"
	existingMetadata := `{"repo":"powerformer/looper","worktreeRoot":"/tmp/old","source":"config"}`
	baseBranch := "main"
	if err := repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID:           "project_1",
		Name:         "Looper",
		RepoPath:     repoPath,
		BaseBranch:   &baseBranch,
		Archived:     false,
		MetadataJSON: &existingMetadata,
		CreatedAt:    "2026-04-11T12:00:00.000Z",
		UpdatedAt:    "2026-04-11T12:00:00.000Z",
	}); err != nil {
		t.Fatalf("Projects.Upsert() seed error = %v", err)
	}

	cfg.Projects = []config.ProjectRefConfig{{
		ID:       "project_1",
		Name:     "Looper",
		RepoPath: repoPath,
	}}

	now := time.Date(2026, time.April, 17, 12, 0, 0, 0, time.UTC)
	if err := defaultSyncConfiguredProjects(context.Background(), repositories, cfg, now); err != nil {
		t.Fatalf("defaultSyncConfiguredProjects() error = %v", err)
	}

	project, err := repositories.Projects.GetByID(context.Background(), "project_1")
	if err != nil {
		t.Fatalf("Projects.GetByID() error = %v", err)
	}
	if project == nil || project.MetadataJSON == nil {
		t.Fatal("project metadata missing after sync")
	}
	const want = `{"repo":"powerformer/looper","worktreeRoot":null,"source":"config"}`
	if *project.MetadataJSON != want {
		t.Fatalf("project.MetadataJSON = %q, want %q", *project.MetadataJSON, want)
	}
	if project.CreatedAt != "2026-04-11T12:00:00.000Z" {
		t.Fatalf("project.CreatedAt = %q, want preserved timestamp", project.CreatedAt)
	}
	if project.UpdatedAt != "2026-04-17T12:00:00.000Z" {
		t.Fatalf("project.UpdatedAt = %q, want sync timestamp", project.UpdatedAt)
	}
}

func TestDefaultSyncConfiguredProjectsPreservesUnknownMetadataFields(t *testing.T) {
	t.Parallel()

	existingMetadata := `{"extra":"value","repo":"powerformer/looper","worktreeRoot":"/tmp/old","source":"config"}`
	repoPath := "/tmp/repo"
	project := config.ProjectRefConfig{ID: "project_1", Name: "Looper", RepoPath: repoPath}
	existing := &storage.ProjectRecord{RepoPath: repoPath, MetadataJSON: &existingMetadata}

	got, err := buildProjectMetadataJSON(existing, project)
	if err != nil {
		t.Fatalf("buildProjectMetadataJSON() error = %v", err)
	}

	const want = `{"extra":"value","repo":"powerformer/looper","worktreeRoot":null,"source":"config"}`
	if got != want {
		t.Fatalf("buildProjectMetadataJSON() = %q, want %q", got, want)
	}
}

func TestRuntimeStartReturnsErrorAfterStop(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}

	cfg.Storage.DBPath = workingDir + "/runtime.sqlite"

	rt := New(Options{Config: cfg, Logger: &testLogger{}})
	rt.Stop("test")

	err = rt.Start(context.Background())
	if err == nil || err.Error() != "runtime already stopped" {
		t.Fatalf("Start() after Stop() error = %v, want runtime already stopped", err)
	}
}

func TestFormatJavaScriptISOStringPreservesMilliseconds(t *testing.T) {
	t.Parallel()

	value := time.Date(2026, time.April, 17, 12, 34, 56, 789_123_000, time.UTC)
	if got, want := formatJavaScriptISOString(value), "2026-04-17T12:34:56.789Z"; got != want {
		t.Fatalf("formatJavaScriptISOString() = %q, want %q", got, want)
	}
}

type testLogger struct {
	mu      sync.Mutex
	entries []string
}

func (l *testLogger) Debug(message string, context map[string]any) { l.append(message) }
func (l *testLogger) Info(message string, context map[string]any)  { l.append(message) }
func (l *testLogger) Warn(message string, context map[string]any)  { l.append(message) }
func (l *testLogger) Error(message string, context map[string]any) { l.append(message) }

func (l *testLogger) append(message string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, message)
}
