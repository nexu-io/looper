package storage

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestRepositoriesRoundTripForProjectsLoopsRunsAndRuntimeMetadata(t *testing.T) {
	t.Parallel()

	coordinator := openMigratedCoordinatorForRepositories(t)
	ctx := context.Background()
	repos := NewRepositories(coordinator.DB())

	now := "2026-04-11T12:00:00.000Z"
	mainBranch := "main"
	baseBranch := "main"
	projectMeta := `{"tier":"mvp"}`
	if err := repos.Projects.Upsert(ctx, ProjectRecord{
		ID:           "project_1",
		Name:         "Looper",
		RepoPath:     "/tmp/looper",
		BaseBranch:   &baseBranch,
		Archived:     false,
		MetadataJSON: &projectMeta,
		CreatedAt:    now,
		UpdatedAt:    now,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	targetID := "pr:42"
	repo := "acme/looper"
	prNumber := int64(42)
	config := `{"priority":"normal"}`
	if err := repos.Loops.Upsert(ctx, LoopRecord{
		ID:         "loop_1",
		Seq:        1,
		ProjectID:  "project_1",
		Type:       "reviewer",
		TargetType: "pull_request",
		TargetID:   &targetID,
		Repo:       &repo,
		PRNumber:   &prNumber,
		Status:     "idle",
		ConfigJSON: &config,
		CreatedAt:  now,
		UpdatedAt:  now,
	}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	running := "running"
	if err := repos.Runs.Upsert(ctx, RunRecord{
		ID:              "run_1",
		LoopID:          "loop_1",
		Status:          running,
		StartedAt:       now,
		LastHeartbeatAt: &now,
		CreatedAt:       now,
		UpdatedAt:       now,
	}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}

	headSHA := "abc123"
	if err := repos.PullRequestSnapshots.Upsert(ctx, PullRequestSnapshotRecord{
		ID:         "snapshot_1",
		ProjectID:  "project_1",
		Repo:       repo,
		PRNumber:   prNumber,
		HeadSHA:    headSHA,
		CapturedAt: now,
		CreatedAt:  now,
	}); err != nil {
		t.Fatalf("PullRequestSnapshots.Upsert() error = %v", err)
	}

	lockReason := "reviewer"
	acquired, err := repos.Locks.Acquire(ctx, LockRecord{
		Key:       "pr:acme/looper:42",
		Owner:     "reviewer-loop",
		Reason:    &lockReason,
		ExpiresAt: "2026-04-11T12:05:00.000Z",
		CreatedAt: now,
		UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("Locks.Acquire() error = %v", err)
	}
	if !acquired {
		t.Fatal("Locks.Acquire() = false, want true")
	}

	worktreePath := "/tmp/looper-worktrees/feature-loop-1"
	headWorktreeSHA := "def456"
	if err := repos.Worktrees.Upsert(ctx, WorktreeRecord{
		ID:           "wt_1",
		ProjectID:    "project_1",
		RepoPath:     "/tmp/looper",
		WorktreePath: worktreePath,
		Branch:       "feature/loop-1",
		BaseBranch:   &mainBranch,
		Status:       "active",
		HeadSHA:      &headWorktreeSHA,
		CreatedAt:    now,
		UpdatedAt:    now,
	}); err != nil {
		t.Fatalf("Worktrees.Upsert() error = %v", err)
	}

	loopBySeq, err := repos.Loops.GetBySeq(ctx, 1)
	if err != nil {
		t.Fatalf("Loops.GetBySeq() error = %v", err)
	}
	if loopBySeq == nil || loopBySeq.ID != "loop_1" {
		t.Fatalf("Loops.GetBySeq() = %#v, want loop_1", loopBySeq)
	}

	latestRun, err := repos.Runs.GetLatestByLoopID(ctx, "loop_1")
	if err != nil {
		t.Fatalf("Runs.GetLatestByLoopID() error = %v", err)
	}
	if latestRun == nil || latestRun.ID != "run_1" {
		t.Fatalf("Runs.GetLatestByLoopID() = %#v, want run_1", latestRun)
	}

	snapshot, err := repos.PullRequestSnapshots.GetLatest(ctx, repo, prNumber)
	if err != nil {
		t.Fatalf("PullRequestSnapshots.GetLatest() error = %v", err)
	}
	if snapshot == nil || snapshot.HeadSHA != headSHA {
		t.Fatalf("PullRequestSnapshots.GetLatest() = %#v, want headSha %q", snapshot, headSHA)
	}

	lock, err := repos.Locks.Get(ctx, "pr:acme/looper:42")
	if err != nil {
		t.Fatalf("Locks.Get() error = %v", err)
	}
	if lock == nil || lock.Owner != "reviewer-loop" {
		t.Fatalf("Locks.Get() = %#v, want owner reviewer-loop", lock)
	}

	worktree, err := repos.Worktrees.GetByBranch(ctx, "project_1", "feature/loop-1")
	if err != nil {
		t.Fatalf("Worktrees.GetByBranch() error = %v", err)
	}
	if worktree == nil || worktree.WorktreePath != worktreePath {
		t.Fatalf("Worktrees.GetByBranch() = %#v, want %q", worktree, worktreePath)
	}
	if worktree.BaseBranch == nil || *worktree.BaseBranch != mainBranch {
		t.Fatalf("Worktrees.GetByBranch().BaseBranch = %#v, want %q", worktree.BaseBranch, mainBranch)
	}
}

func TestLoopsAllocateSeqSeedsFromMaxWhenCounterMissing(t *testing.T) {
	t.Parallel()

	coordinator := openMigratedCoordinatorForRepositories(t)
	ctx := context.Background()
	repos := NewRepositories(coordinator.DB())

	now := "2026-04-11T12:00:00.000Z"
	if err := repos.Projects.Upsert(ctx, ProjectRecord{
		ID:        "project_1",
		Name:      "Looper",
		RepoPath:  "/tmp/looper",
		Archived:  false,
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	if err := repos.Loops.Upsert(ctx, LoopRecord{ID: "loop_3", Seq: 3, ProjectID: "project_1", Type: "worker", TargetType: "project", Status: "running", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Loops.Upsert(loop_3) error = %v", err)
	}
	if err := repos.Loops.Upsert(ctx, LoopRecord{ID: "loop_7", Seq: 7, ProjectID: "project_1", Type: "worker", TargetType: "project", Status: "running", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Loops.Upsert(loop_7) error = %v", err)
	}

	if _, err := coordinator.DB().ExecContext(ctx, `DELETE FROM counters WHERE name = 'loop_seq'`); err != nil {
		t.Fatalf("DELETE counters error = %v", err)
	}

	seq1, err := repos.Loops.AllocateSeq(ctx)
	if err != nil {
		t.Fatalf("Loops.AllocateSeq() first error = %v", err)
	}
	if seq1 != 8 {
		t.Fatalf("Loops.AllocateSeq() first = %d, want 8", seq1)
	}

	seq2, err := repos.Loops.AllocateSeq(ctx)
	if err != nil {
		t.Fatalf("Loops.AllocateSeq() second error = %v", err)
	}
	if seq2 != 9 {
		t.Fatalf("Loops.AllocateSeq() second = %d, want 9", seq2)
	}
}

func TestRepositoriesRollbackTransactionalWrites(t *testing.T) {
	t.Parallel()

	coordinator := openMigratedCoordinatorForRepositories(t)
	ctx := context.Background()
	repos := NewRepositories(coordinator.DB())

	now := "2026-04-11T12:00:00.000Z"
	if err := repos.Projects.Upsert(ctx, ProjectRecord{ID: "project_1", Name: "Looper", RepoPath: "/tmp/looper", Archived: false, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	wantErr := errors.New("abort transaction")
	err := coordinator.WithTransaction(ctx, func(tx *sql.Tx) error {
		txRepos := NewRepositories(tx)
		if upsertErr := txRepos.Loops.Upsert(ctx, LoopRecord{ID: "loop_rollback", Seq: 1, ProjectID: "project_1", Type: "worker", TargetType: "project", Status: "queued", CreatedAt: now, UpdatedAt: now}); upsertErr != nil {
			return upsertErr
		}

		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("WithTransaction() error = %v, want %v", err, wantErr)
	}

	got, err := repos.Loops.GetByID(ctx, "loop_rollback")
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if got != nil {
		t.Fatalf("Loops.GetByID(loop_rollback) = %#v, want nil", got)
	}
}

func TestLocksAcquireRequiresExpiryBeforeReplacement(t *testing.T) {
	t.Parallel()

	coordinator := openMigratedCoordinatorForRepositories(t)
	ctx := context.Background()
	repos := NewRepositories(coordinator.DB())
	repos.Locks.SetNow(func() time.Time {
		return time.Date(2026, time.April, 11, 12, 0, 0, 0, time.UTC)
	})

	now := "2026-04-11T12:00:00.000Z"
	reason := "initial"
	acquired, err := repos.Locks.Acquire(ctx, LockRecord{
		Key:       "task:123",
		Owner:     "worker-a",
		Reason:    &reason,
		ExpiresAt: "2026-04-11T12:05:00.000Z",
		CreatedAt: now,
		UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("Locks.Acquire(initial) error = %v", err)
	}
	if !acquired {
		t.Fatal("Locks.Acquire(initial) = false, want true")
	}

	replacementReason := "takeover"
	acquired, err = repos.Locks.Acquire(ctx, LockRecord{
		Key:       "task:123",
		Owner:     "worker-b",
		Reason:    &replacementReason,
		ExpiresAt: "2026-04-11T12:20:00.000Z",
		CreatedAt: "2026-04-11T12:01:00.000Z",
		UpdatedAt: "2026-04-11T12:01:00.000Z",
	})
	if err != nil {
		t.Fatalf("Locks.Acquire(replacement blocked) error = %v", err)
	}
	if acquired {
		t.Fatal("Locks.Acquire(replacement blocked) = true, want false")
	}

	repos.Locks.SetNow(func() time.Time {
		return time.Date(2026, time.April, 11, 12, 10, 0, 0, time.UTC)
	})
	acquired, err = repos.Locks.Acquire(ctx, LockRecord{
		Key:       "task:123",
		Owner:     "worker-b",
		Reason:    &replacementReason,
		ExpiresAt: "2026-04-11T12:20:00.000Z",
		CreatedAt: "2026-04-11T12:10:00.000Z",
		UpdatedAt: "2026-04-11T12:10:00.000Z",
	})
	if err != nil {
		t.Fatalf("Locks.Acquire(replacement after expiry) error = %v", err)
	}
	if !acquired {
		t.Fatal("Locks.Acquire(replacement after expiry) = false, want true")
	}

	lock, err := repos.Locks.Get(ctx, "task:123")
	if err != nil {
		t.Fatalf("Locks.Get() error = %v", err)
	}
	if lock == nil || lock.Owner != "worker-b" {
		t.Fatalf("Locks.Get() = %#v, want owner worker-b", lock)
	}
}

func TestRunsListByStatusOrdersByStartedAtThenIDDesc(t *testing.T) {
	t.Parallel()

	coordinator := openMigratedCoordinatorForRepositories(t)
	ctx := context.Background()
	repos := NewRepositories(coordinator.DB())

	now := "2026-04-11T12:00:00.000Z"
	if err := repos.Projects.Upsert(ctx, ProjectRecord{ID: "project_1", Name: "Looper", RepoPath: "/tmp/looper", Archived: false, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := repos.Loops.Upsert(ctx, LoopRecord{ID: "loop_1", Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "project", Status: "running", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	for _, run := range []RunRecord{
		{ID: "run_1", LoopID: "loop_1", Status: "running", StartedAt: "2026-04-11T12:00:00.000Z", CreatedAt: now, UpdatedAt: now},
		{ID: "run_3", LoopID: "loop_1", Status: "running", StartedAt: "2026-04-11T12:00:00.000Z", CreatedAt: now, UpdatedAt: now},
		{ID: "run_2", LoopID: "loop_1", Status: "running", StartedAt: "2026-04-11T12:01:00.000Z", CreatedAt: now, UpdatedAt: now},
	} {
		if err := repos.Runs.Upsert(ctx, run); err != nil {
			t.Fatalf("Runs.Upsert(%s) error = %v", run.ID, err)
		}
	}

	running, err := repos.Runs.ListByStatus(ctx, "running")
	if err != nil {
		t.Fatalf("Runs.ListByStatus() error = %v", err)
	}
	if len(running) != 3 {
		t.Fatalf("len(Runs.ListByStatus()) = %d, want 3", len(running))
	}

	got := []string{running[0].ID, running[1].ID, running[2].ID}
	want := []string{"run_2", "run_3", "run_1"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Runs.ListByStatus() order = %v, want %v", got, want)
		}
	}
}

func openMigratedCoordinatorForRepositories(t *testing.T) *SQLiteCoordinator {
	t.Helper()

	root := t.TempDir()
	coordinator, err := OpenSQLiteCoordinator(context.Background(), filepath.Join(root, "looper.sqlite"), SQLiteCoordinatorOptions{
		Migrations: EmbeddedMigrations,
		BackupDir:  filepath.Join(root, "backups"),
	})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	t.Cleanup(func() {
		if closeErr := coordinator.Close(); closeErr != nil {
			t.Fatalf("coordinator.Close() error = %v", closeErr)
		}
	})

	if _, err := coordinator.MigrationRunner().RunPending(context.Background()); err != nil {
		t.Fatalf("MigrationRunner.RunPending() error = %v", err)
	}

	return coordinator
}
