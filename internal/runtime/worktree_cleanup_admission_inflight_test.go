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
	gitinfra "github.com/nexu-io/looper/internal/infra/git"
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

// Contract (#592 review): CleanupWorktree does not hold admission.mu, so
// MarkDegraded can close admission and cancel producers while git remove is
// in flight. The in-flight remove may still finish; durable cleaned is gated
// under a short post-command WithAllowClaim hold.
func TestWorktreeCleanupInFlightDoesNotHoldAdmissionAcrossCleanup(t *testing.T) {
	t.Parallel()

	fixture := newWorktreeCleanupFixture(t)
	worktree := fixture.seedWorktree(t, "wt_inflight", "feature/inflight", true)

	entered := make(chan struct{})
	release := make(chan struct{})
	git := &fakeWorktreeCleanupGit{
		listed: map[string][]gitinfra.WorktreeListEntry{fixture.project.RepoPath: {
			{Path: worktree.WorktreePath, Branch: worktree.Branch},
		}},
		clean: map[string]bool{worktree.WorktreePath: true},
		onCleanup: func(input gitinfra.CleanupWorktreeInput) error {
			close(entered)
			<-release
			updated, err := fixture.repos.Worktrees.GetByID(context.Background(), worktree.ID)
			if err != nil {
				return err
			}
			nowISO := fixture.now.Format("2006-01-02T15:04:05.000Z")
			updated.Status = "cleaned"
			updated.CleanedAt = &nowISO
			updated.UpdatedAt = nowISO
			return fixture.repos.Worktrees.Upsert(context.Background(), *updated)
		},
	}

	done := make(chan WorktreeCleanupStatus, 1)
	go func() {
		done <- fixture.runtime.runWorktreeCleanupPass(context.Background(), fixture.repos, git, fixture.config)
	}()

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for in-flight CleanupWorktree")
	}

	degradeDone := make(chan error, 1)
	go func() {
		degradeDone <- fixture.runtime.MarkDegraded("mid cleanup pass")
	}()
	select {
	case err := <-degradeDone:
		if err != nil {
			t.Fatalf("MarkDegraded() during in-flight CleanupWorktree error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("MarkDegraded blocked while CleanupWorktree in flight (admission hold deadlock)")
	}
	if state := fixture.runtime.admission.State(); state != AdmissionDegraded {
		t.Fatalf("admission.State() during in-flight cleanup = %q, want degraded", state)
	}

	close(release)

	var summary WorktreeCleanupStatus
	select {
	case summary = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("cleanup pass did not return after in-flight cleanup finished")
	}

	// Filesystem remove finished; cleaned count reflects the mutation even when
	// durable cleaned event is refused after admission closed.
	if summary.Cleaned != 1 {
		t.Fatalf("summary.Cleaned = %d, want 1 (remove finished after admission closed)", summary.Cleaned)
	}
	stored, err := fixture.repos.Worktrees.GetByID(context.Background(), worktree.ID)
	if err != nil {
		t.Fatalf("Worktrees.GetByID() error = %v", err)
	}
	if stored == nil || stored.Status != "cleaned" {
		t.Fatalf("stored worktree = %#v, want cleaned after in-flight cleanup completed", stored)
	}
	// Durable cleaned must not append after admission closed (post-command gate).
	events := fixture.events(t)
	if containsWorktreeCleanupEvent(events, "worktree.cleanup.cleaned") {
		t.Fatalf("events = %#v, want no cleaned event after admission closed mid-remove", events)
	}
}

// Contract (#592 review): when MarkDegraded cancels the pass context mid-pass,
// the ctx.Err() path must not append worktree.cleanup.completed after admission
// has already closed (cancelWorkProducers runs after the degraded transition).
func TestWorktreeCleanupPassOmitsCompletedWhenContextCanceledAfterDegrade(t *testing.T) {
	t.Parallel()

	fixture := newWorktreeCleanupFixture(t)
	first := fixture.seedWorktreeAt(t, "wt_ctx_first", "feature/ctx-first", true, fixture.now.Add(-2*time.Hour))
	second := fixture.seedWorktreeAt(t, "wt_ctx_second", "feature/ctx-second", true, fixture.now.Add(-time.Hour))

	ctx, cancel := context.WithCancel(context.Background())
	// Production MarkDegraded cancels worktreeCleanupCancel; wire it so the
	// in-flight pass observes ctx.Err() after admission closes.
	fixture.runtime.worktreeCleanupCancel = cancel

	enteredFirst := make(chan struct{})
	releaseFirst := make(chan struct{})
	git := &fakeWorktreeCleanupGit{
		listed: map[string][]gitinfra.WorktreeListEntry{fixture.project.RepoPath: {
			{Path: first.WorktreePath, Branch: first.Branch},
			{Path: second.WorktreePath, Branch: second.Branch},
		}},
		clean: map[string]bool{first.WorktreePath: true, second.WorktreePath: true},
		onCleanup: func(input gitinfra.CleanupWorktreeInput) error {
			if input.WorktreePath != first.WorktreePath {
				t.Fatalf("CleanupWorktree called for %q; second candidate must not mutate after cancel", input.WorktreePath)
			}
			close(enteredFirst)
			<-releaseFirst
			updated, err := fixture.repos.Worktrees.GetByID(context.Background(), first.ID)
			if err != nil {
				return err
			}
			nowISO := fixture.now.Format("2006-01-02T15:04:05.000Z")
			updated.Status = "cleaned"
			updated.CleanedAt = &nowISO
			updated.UpdatedAt = nowISO
			return fixture.repos.Worktrees.Upsert(context.Background(), *updated)
		},
	}

	done := make(chan WorktreeCleanupStatus, 1)
	go func() {
		done <- fixture.runtime.runWorktreeCleanupPass(ctx, fixture.repos, git, fixture.config)
	}()

	select {
	case <-enteredFirst:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first CleanupWorktree")
	}

	degradeDone := make(chan error, 1)
	go func() {
		degradeDone <- fixture.runtime.MarkDegraded("cancel cleanup mid-pass")
	}()
	select {
	case err := <-degradeDone:
		if err != nil {
			t.Fatalf("MarkDegraded() during in-flight CleanupWorktree error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("MarkDegraded blocked while CleanupWorktree in flight (admission hold deadlock)")
	}

	close(releaseFirst)

	var summary WorktreeCleanupStatus
	select {
	case summary = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("cleanup pass did not return after mid-pass cancel")
	}

	if summary.Cleaned != 1 {
		t.Fatalf("summary.Cleaned = %d, want 1 (first candidate finished after degrade)", summary.Cleaned)
	}
	if fixture.runtime.admission.State() != AdmissionDegraded {
		t.Fatalf("admission.State() = %q, want degraded", fixture.runtime.admission.State())
	}
	events := fixture.events(t)
	if !containsWorktreeCleanupEvent(events, "worktree.cleanup.started") {
		t.Fatalf("events = %#v, want started", events)
	}
	// Durable cleaned/completed must not append after admission closed.
	if containsWorktreeCleanupEvent(events, "worktree.cleanup.cleaned") {
		t.Fatalf("events = %#v, want no cleaned after admission closed mid-remove", events)
	}
	if containsWorktreeCleanupEvent(events, "worktree.cleanup.completed") {
		t.Fatalf("events = %#v, want no completed after canceled pass under closed admission", events)
	}
	if containsWorktreeCleanupEvent(events, "worktree.cleanup.failed") {
		t.Fatalf("events = %#v, want no pass-level failed after cancel under closed admission", events)
	}
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
