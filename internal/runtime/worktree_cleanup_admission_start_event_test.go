package runtime

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
)

// eventAppendGateQuerier blocks the first event_logs INSERT so tests can prove
// MarkDegraded is not stalled by admission.mu held across appendWorktreeCleanupEvent.
type eventAppendGateQuerier struct {
	db       *sql.DB
	entered  chan struct{}
	release  chan struct{}
	gateOnce sync.Once
}

func (q *eventAppendGateQuerier) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if strings.Contains(query, "INSERT INTO event_logs") {
		q.gateOnce.Do(func() {
			close(q.entered)
			select {
			case <-q.release:
			case <-ctx.Done():
			}
		})
	}
	return q.db.ExecContext(ctx, query, args...)
}

func (q *eventAppendGateQuerier) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return q.db.QueryContext(ctx, query, args...)
}

func (q *eventAppendGateQuerier) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return q.db.QueryRowContext(ctx, query, args...)
}

// Contract (#592 review): start-event append must not hold admission.mu across
// a blocking SQLite write, or MarkDegraded cannot cancel producers while the
// append waits (deadlock through busy timeout / pool wait).
func TestWorktreeCleanupStartEventDoesNotHoldAdmissionAcrossAppend(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	cfg, err := config.DefaultConfig(root)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Daemon.WorktreeCleanup.Enabled = true
	cfg.Daemon.WorktreeCleanup.DryRun = true
	cfg.Daemon.WorktreeCleanup.MaxPerTick = 10
	cfg.Daemon.WorktreeCleanup.RetentionDays = 0
	cfg.Daemon.WorktreeCleanup.IncludeOrphans = true
	worktreeRoot := filepath.Join(root, "worktrees")
	repoPath := filepath.Join(root, "repo")
	if err := os.MkdirAll(repoPath, 0o755); err != nil {
		t.Fatalf("MkdirAll(repoPath) error = %v", err)
	}
	if err := os.MkdirAll(worktreeRoot, 0o755); err != nil {
		t.Fatalf("MkdirAll(worktreeRoot) error = %v", err)
	}
	now := time.Date(2026, time.May, 20, 12, 0, 0, 0, time.UTC)
	coordinator, err := storage.OpenSQLiteCoordinator(context.Background(), filepath.Join(root, "looper.sqlite"), storage.SQLiteCoordinatorOptions{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	if _, err := coordinator.MigrationRunner().RunPending(context.Background(), storage.RunPendingOptions{}); err != nil {
		t.Fatalf("RunPending() error = %v", err)
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	gate := &eventAppendGateQuerier{db: coordinator.DB(), entered: entered, release: release}
	repos := storage.NewRepositories(gate)
	project := storage.ProjectRecord{
		ID: "project_1", Name: "Project", RepoPath: repoPath, BaseBranch: stringPtr("main"),
		MetadataJSON: stringPtr(`{"worktreeRoot":"` + worktreeRoot + `"}`),
		CreatedAt:    now.Format("2006-01-02T15:04:05.000Z"), UpdatedAt: now.Format("2006-01-02T15:04:05.000Z"),
	}
	if err := repos.Projects.Upsert(context.Background(), project); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	rt := New(Options{Config: cfg, Now: func() time.Time { return now }, WorktreeCleanupInitialDelay: -1})
	if err := rt.admission.MarkReady("start-event admission hold regression"); err != nil {
		t.Fatalf("admission.MarkReady() error = %v", err)
	}

	done := make(chan WorktreeCleanupStatus, 1)
	go func() {
		done <- rt.runWorktreeCleanupPass(context.Background(), repos, &fakeWorktreeCleanupGit{}, cfg)
	}()

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for start-event SQLite append")
	}

	degradeDone := make(chan error, 1)
	go func() {
		degradeDone <- rt.MarkDegraded("during start-event append")
	}()
	select {
	case err := <-degradeDone:
		if err != nil {
			t.Fatalf("MarkDegraded() during start-event append error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("MarkDegraded blocked while start-event append in flight (admission hold deadlock)")
	}
	if state := rt.admission.State(); state != AdmissionDegraded {
		t.Fatalf("admission.State() during start-event append = %q, want degraded", state)
	}

	close(release)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("cleanup pass did not return after start-event append released")
	}
}
