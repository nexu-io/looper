package runtime

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/planner"
	"github.com/nexu-io/looper/internal/storage"
)

// Contract: admission closed must skip the entire work-producing tick
// (discovery / HITL / claims / stale-reconcile), not only ClaimNext*.
func TestSafetyFloorTickSkipsDiscoveryAndReconcileWhenAdmissionClosed(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	backupDir := t.TempDir()
	coordinator := openMigratedCoordinator(t, filepath.Join(workingDir, "scheduler.sqlite"), backupDir)
	t.Cleanup(func() { _ = coordinator.Close() })
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.July, 17, 12, 0, 0, 0, time.UTC)
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
			return ErrAdmissionStopping
		},
	})
	if err != nil {
		t.Fatalf("runDefaultSchedulerTick() error = %v", err)
	}
	if allowCalls.Load() == 0 {
		t.Fatal("AllowClaim was not consulted at tick start")
	}
	if len(plannerRunner.discoverCalls) != 0 {
		t.Fatalf("planner discover calls = %#v, want none when admission closed", plannerRunner.discoverCalls)
	}
	if reconcileCalls.Load() != 0 {
		t.Fatalf("ReconcileStaleRuns calls = %d, want 0 when admission closed", reconcileCalls.Load())
	}
}

// Contract: AllowClaim is rechecked immediately before each durable ClaimNext*
// so a pump-level pass cannot race with BeginShutdown and still claim work.
func TestSafetyFloorClaimRechecksAdmissionBeforeClaimNext(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	backupDir := filepath.Join(workingDir, "backups")
	cfg.Storage.BackupDir = &backupDir
	now := time.Date(2026, time.July, 17, 12, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)

	coordinator := openMigratedCoordinator(t, cfg.Storage.DBPath, backupDir)
	t.Cleanup(func() { _ = coordinator.Close() })
	repos := storage.NewRepositories(coordinator.DB())
	projectID := "project_claim_gate"
	loopID := "loop_claim_gate"
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Claim Gate", RepoPath: filepath.Join(workingDir, "repo"), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 7, ProjectID: projectID, Type: "worker", TargetType: "project", Status: "queued", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	if err := repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{
		ID: "queue_claim_gate", ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: projectID,
		DedupeKey: "worker:project_claim_gate:loop_claim_gate", Priority: storage.QueuePriorityWorker, Status: "queued",
		AvailableAt: nowISO, Attempts: 0, MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	var allowCalls atomic.Int64
	claimed, err := claimAndRunScheduledQueueItems(context.Background(), 1, defaultSchedulerTickInput{
		Repos: repos,
		Now:   func() time.Time { return now },
		AllowClaim: func() error {
			allowCalls.Add(1)
			return ErrAdmissionStopping
		},
	})
	if err != nil {
		t.Fatalf("claimAndRunScheduledQueueItems() error = %v", err)
	}
	if len(claimed) != 0 {
		t.Fatalf("claimed = %#v, want empty when admission refuses at claim point", claimed)
	}
	if allowCalls.Load() == 0 {
		t.Fatal("AllowClaim was not consulted at claim point")
	}
	item, err := repos.Queue.GetByID(context.Background(), "queue_claim_gate")
	if err != nil {
		t.Fatalf("Queue.GetByID error = %v", err)
	}
	if item == nil || item.Status != "queued" {
		t.Fatalf("queue item = %#v, want still queued (no durable claim after admission refuse)", item)
	}
}

// Contract: when availableSlots is already 0, admission must be rechecked
// immediately before ReconcileStaleRuns so BeginShutdown cannot race past the
// claim-phase entry gate and still mutate runs/queue during the drain window.
func TestSafetyFloorClaimPhaseRechecksAdmissionBeforeStaleReconcile(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	backupDir := t.TempDir()
	coordinator := openMigratedCoordinator(t, filepath.Join(workingDir, "scheduler.sqlite"), backupDir)
	t.Cleanup(func() { _ = coordinator.Close() })
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.July, 17, 12, 0, 0, 0, time.UTC)

	var allowCalls atomic.Int64
	var reconcileCalls atomic.Int64
	claimed, available, err := executeClaimPhase(context.Background(), "test", defaultSchedulerTickInput{
		Repos:             repos,
		Now:               func() time.Time { return now },
		MaxConcurrentRuns: 0, // forces availableSlots == 0 so reconcile path is taken
		ReconcileStaleRuns: func(context.Context) (StaleRunReconcileSummary, error) {
			reconcileCalls.Add(1)
			return StaleRunReconcileSummary{}, nil
		},
		AllowClaim: func() error {
			// First call: claim-phase entry recheck passes.
			// Second call: must gate ReconcileStaleRuns after slots return 0.
			if allowCalls.Add(1) >= 2 {
				return ErrAdmissionStopping
			}
			return nil
		},
	}, nil, false)
	if err != nil {
		t.Fatalf("executeClaimPhase() error = %v", err)
	}
	if claimed != 0 || available != 0 {
		t.Fatalf("claimed=%d available=%d, want 0/0", claimed, available)
	}
	if allowCalls.Load() < 2 {
		t.Fatalf("AllowClaim calls = %d, want >= 2 (entry + pre-reconcile)", allowCalls.Load())
	}
	if reconcileCalls.Load() != 0 {
		t.Fatalf("ReconcileStaleRuns calls = %d, want 0 after pre-reconcile admission refuse", reconcileCalls.Load())
	}
}

// Contract: if admission closes mid-tick after the entry gate passed, later
// discovery lanes must not enqueue (BeginShutdown during HTTP drain).
func TestSafetyFloorMidTickAdmissionCloseStopsLaterDiscovery(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	backupDir := t.TempDir()
	coordinator := openMigratedCoordinator(t, filepath.Join(workingDir, "scheduler.sqlite"), backupDir)
	t.Cleanup(func() { _ = coordinator.Close() })
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.July, 17, 12, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	baseBranch := "main"
	for _, projectID := range []string{"project_a", "project_b"} {
		meta := `{"repo":"nexu-io/` + projectID + `"}`
		if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{
			ID: projectID, Name: projectID, RepoPath: filepath.Join(workingDir, projectID),
			BaseBranch: &baseBranch, MetadataJSON: &meta, CreatedAt: nowISO, UpdatedAt: nowISO,
		}); err != nil {
			t.Fatalf("Projects.Upsert(%s) error = %v", projectID, err)
		}
	}

	var refuse atomic.Bool
	plannerRunner := &midTickClosingPlanner{refuse: &refuse}
	err := runDefaultSchedulerTick(context.Background(), defaultSchedulerTickInput{
		Repos:             repos,
		Now:               func() time.Time { return now },
		MaxConcurrentRuns: 1,
		Planner:           plannerRunner,
		AllowClaim: func() error {
			if refuse.Load() {
				return ErrAdmissionStopping
			}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("runDefaultSchedulerTick() error = %v", err)
	}
	if got := len(plannerRunner.discoverCalls); got != 1 {
		t.Fatalf("planner discover calls = %d (%#v), want exactly 1 (second project blocked after mid-tick close)", got, plannerRunner.discoverCalls)
	}
}

// midTickClosingPlanner flips admission closed after the first discovery so the
// next project's lane recheck must refuse further enqueue work.
type midTickClosingPlanner struct {
	stubPlannerScheduler
	refuse *atomic.Bool
}

func (p *midTickClosingPlanner) DiscoverIssues(ctx context.Context, input planner.DiscoveryInput) (planner.DiscoveryResult, error) {
	defer p.refuse.Store(true)
	return p.stubPlannerScheduler.DiscoverIssues(ctx, input)
}
