package sweeper

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
)

func TestDiscoverIssuesSkipsWhenAutoDiscoveryDisabledForProject(t *testing.T) {
	t.Parallel()

	repos := newTestRepositories(t)
	now := time.Date(2026, time.May, 9, 12, 0, 0, 0, time.UTC)
	nowISO := now.Format(javaScriptISOStringUTC)
	projectID := "demo"
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Demo", RepoPath: filepath.Join(t.TempDir(), "repo"), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	defaultConfig, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	runner := New(Options{Repos: repos, Now: func() time.Time { return now }, Config: &defaultConfig})
	result, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: projectID, Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	if result.Skipped != 1 || len(result.QueueItems) != 0 {
		t.Fatalf("DiscoverIssues() = %#v, want one skipped result with no queue items", result)
	}
}

func TestProcessClaimedQueueItemCompletesSupportedTypesAsSkipped(t *testing.T) {
	t.Parallel()

	repos := newTestRepositories(t)
	now := time.Date(2026, time.May, 9, 12, 0, 0, 0, time.UTC)
	nowISO := now.Format(javaScriptISOStringUTC)
	queueID := "queue_sweeper_warn_1"
	if err := repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: queueID, Type: QueueTypeWarn, TargetType: "issue", TargetID: "acme/looper#42", DedupeKey: "sweeper:warn:acme/looper#42", Priority: 1, Status: "running", AvailableAt: nowISO, MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	runner := New(Options{Repos: repos, Now: func() time.Time { return now }})
	result, err := runner.ProcessClaimedQueueItem(context.Background(), storage.QueueItemRecord{ID: queueID, Type: QueueTypeWarn})
	if err != nil {
		t.Fatalf("ProcessClaimedQueueItem() error = %v", err)
	}
	if result == nil || result.Status != "skipped" || result.QueueItemID != queueID {
		t.Fatalf("ProcessClaimedQueueItem() = %#v, want skipped result for %s", result, queueID)
	}
	stored, err := repos.Queue.GetByID(context.Background(), queueID)
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	if stored == nil || stored.Status != "completed" {
		t.Fatalf("stored queue item = %#v, want completed status", stored)
	}
}

func TestProcessClaimedQueueItemRejectsUnsupportedQueueType(t *testing.T) {
	t.Parallel()

	runner := New(Options{})
	result, err := runner.ProcessClaimedQueueItem(context.Background(), storage.QueueItemRecord{ID: "queue_1", Type: "worker"})
	if err == nil {
		t.Fatal("ProcessClaimedQueueItem() error = nil, want unsupported type error")
	}
	if result != nil {
		t.Fatalf("ProcessClaimedQueueItem() result = %#v, want nil on unsupported type", result)
	}
}

func newTestRepositories(t *testing.T) *storage.Repositories {
	t.Helper()
	coordinator, err := storage.OpenSQLiteCoordinator(context.Background(), filepath.Join(t.TempDir(), "looper.sqlite"), storage.SQLiteCoordinatorOptions{Migrations: storage.EmbeddedMigrations})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	t.Cleanup(func() {
		if err := coordinator.Close(); err != nil {
			t.Fatalf("coordinator.Close() error = %v", err)
		}
	})
	if _, err := coordinator.MigrationRunner().RunPending(context.Background()); err != nil {
		t.Fatalf("MigrationRunner().RunPending() error = %v", err)
	}
	return storage.NewRepositories(coordinator.DB())
}
