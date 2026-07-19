package runtime

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
)

// Contract (#580): starting and degraded refuse the full work-producing tick
// (discovery + claims + stale-reconcile), not only ClaimNext*.
func TestNonMutatingCoverageTickPausesUnderStartingAndDegraded(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "starting", err: ErrAdmissionNotReady},
		{name: "degraded", err: ErrAdmissionDegraded},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			workingDir := t.TempDir()
			backupDir := t.TempDir()
			coordinator := openMigratedCoordinator(t, filepath.Join(workingDir, "scheduler.sqlite"), backupDir)
			t.Cleanup(func() { _ = coordinator.Close() })
			repos := storage.NewRepositories(coordinator.DB())
			now := time.Date(2026, time.July, 19, 12, 0, 0, 0, time.UTC)
			nowISO := formatJavaScriptISOString(now)
			baseBranch := "main"
			projectMetadata := `{"repo":"nexu-io/looper"}`
			if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{
				ID: "looper", Name: "Looper", RepoPath: filepath.Join(workingDir, "repo"),
				BaseBranch: &baseBranch, MetadataJSON: &projectMetadata, CreatedAt: nowISO, UpdatedAt: nowISO,
			}); err != nil {
				t.Fatalf("Projects.Upsert() error = %v", err)
			}

			plannerRunner := &stubPlannerScheduler{}
			var reconcileCalls atomic.Int64
			var allowCalls atomic.Int64
			err := runDefaultSchedulerTick(context.Background(), defaultSchedulerTickInput{
				Repos:             repos,
				Now:               func() time.Time { return now },
				MaxConcurrentRuns: 1,
				Planner:           plannerRunner,
				ReconcileStaleRuns: func(context.Context) (StaleRunReconcileSummary, error) {
					reconcileCalls.Add(1)
					return StaleRunReconcileSummary{}, nil
				},
				AllowClaim: func() error {
					allowCalls.Add(1)
					return tc.err
				},
			})
			if err != nil {
				t.Fatalf("runDefaultSchedulerTick() error = %v", err)
			}
			if allowCalls.Load() == 0 {
				t.Fatal("AllowClaim was not consulted at tick start")
			}
			if len(plannerRunner.discoverCalls) != 0 {
				t.Fatalf("planner discover calls = %#v, want none under %s", plannerRunner.discoverCalls, tc.name)
			}
			if reconcileCalls.Load() != 0 {
				t.Fatalf("ReconcileStaleRuns calls = %d, want 0 under %s", reconcileCalls.Load(), tc.name)
			}
		})
	}
}

// Contract (#580): worktree cleanup is a mutation surface and must not run
// while admission is starting, degraded, or stopping.
func TestNonMutatingCoverageWorktreeCleanupPausedWhenAdmissionClosed(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	backupDir := filepath.Join(workingDir, "backups")
	cfg.Storage.BackupDir = &backupDir
	cfg.Daemon.WorktreeCleanup.Enabled = true

	rt := New(Options{Config: cfg, Logger: &testLogger{}, DeferRecovery: true})
	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { rt.Stop("test cleanup") })

	// Starting: cleanup must no-op.
	rt.executeWorktreeCleanupPass(context.Background())
	if status := rt.WorktreeCleanupStatus(); status.LastStatus == "running" || status.Scanned > 0 || status.Cleaned > 0 {
		t.Fatalf("cleanup while starting status=%#v, want idle no-op", status)
	}

	if err := rt.CompleteStartup(context.Background()); err != nil {
		t.Fatalf("CompleteStartup() error = %v", err)
	}
	if err := rt.MarkDegraded("test degrade"); err != nil {
		t.Fatalf("MarkDegraded() error = %v", err)
	}
	rt.executeWorktreeCleanupPass(context.Background())
	if status := rt.WorktreeCleanupStatus(); status.Scanned > 0 || status.Cleaned > 0 || status.LastStatus == "running" {
		t.Fatalf("cleanup while degraded status=%#v, want no-op", status)
	}
}

// Contract (#580): claim pump and full-tick projection share the single
// admission Authority under degraded — no dual ready flag.
func TestNonMutatingCoverageDegradedPausesClaimPump(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	backupDir := filepath.Join(workingDir, "backups")
	cfg.Storage.BackupDir = &backupDir

	rt := New(Options{Config: cfg, Logger: &testLogger{}, DeferRecovery: true})
	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { rt.Stop("test cleanup") })
	if err := rt.CompleteStartup(context.Background()); err != nil {
		t.Fatalf("CompleteStartup() error = %v", err)
	}

	var claimCalls atomic.Int64
	rt.mu.Lock()
	rt.defaultSchedulerClaim = func(context.Context, Services) error {
		claimCalls.Add(1)
		return nil
	}
	rt.mu.Unlock()

	rt.executeSchedulerClaimPass(context.Background())
	if claimCalls.Load() != 1 {
		t.Fatalf("claim calls while ready = %d, want 1", claimCalls.Load())
	}
	if err := rt.MarkDegraded("persist failure"); err != nil {
		t.Fatalf("MarkDegraded() error = %v", err)
	}
	rt.executeSchedulerClaimPass(context.Background())
	if claimCalls.Load() != 1 {
		t.Fatalf("claim calls after degraded = %d, want still 1", claimCalls.Load())
	}
}
