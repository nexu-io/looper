package worktreecleanup

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
)

func TestPlanDryRunAppliesRetentionAndProtectsRuntimeReferences(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	old := fixture.now.Add(-10 * 24 * time.Hour)
	recent := fixture.now.Add(-24 * time.Hour)

	fixture.worktree("wt_old", "branch-old", old)
	fixture.loop("loop_old", "completed", "wt_old", "branch-old", old)
	fixture.run("run_old", "loop_old", "success", `{"worktree":{"id":"wt_old"}}`, old)

	fixture.worktree("wt_recent", "branch-recent", recent)
	fixture.loop("loop_recent", "completed", "wt_recent", "branch-recent", recent)

	fixture.worktree("wt_failed", "branch-failed", old)
	fixture.loop("loop_failed", "failed", "wt_failed", "branch-failed", old)

	fixture.worktree("wt_running", "branch-running", old)
	fixture.loop("loop_running", "completed", "wt_running", "branch-running", old)
	fixture.run("run_running", "loop_running", "running", `{"worktree":{"id":"wt_running"}}`, old)

	fixture.worktree("wt_queue", "branch-queue", old)
	fixture.loop("loop_queue", "completed", "wt_queue", "branch-queue", old)
	fixture.queue("queue_running", "loop_queue", "running", old)

	fixture.worktree("wt_orphan", "branch-orphan", old)

	result, err := fixture.service().Plan(context.Background())
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	if result.Summary.Scanned != 6 || result.Summary.Candidates != 1 || result.Summary.WouldClean != 1 || result.Summary.Orphans != 1 {
		t.Fatalf("Plan().Summary = %#v, want scanned=6 candidates=1 wouldClean=1 orphans=1", result.Summary)
	}
	assertDecision(t, result, "wt_old", ActionWouldClean, "eligible in dry-run plan")
	assertDecision(t, result, "wt_recent", ActionSkipped, "within retention window")
	assertDecision(t, result, "wt_failed", ActionSkipped, "referenced by protected loop status failed")
	assertDecision(t, result, "wt_running", ActionSkipped, "referenced by running run")
	assertDecision(t, result, "wt_queue", ActionSkipped, "referenced by active queue item")
	assertDecision(t, result, "wt_orphan", ActionSkipped, "orphan worktree and includeOrphans=false")
}

func TestPlanSkipsAffectedWorktreesWhenCheckpointCannotParse(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	old := fixture.now.Add(-10 * 24 * time.Hour)
	fixture.worktree("wt_parse", "branch-parse", old)
	fixture.loop("loop_parse", "completed", "wt_parse", "branch-parse", old)
	fixture.run("run_parse", "loop_parse", "failed", `{`, old)

	result, err := fixture.service().Plan(context.Background())
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	if result.Summary.Failed != 1 || result.Summary.WouldClean != 0 {
		t.Fatalf("Plan().Summary = %#v, want failed=1 wouldClean=0", result.Summary)
	}
	assertDecision(t, result, "wt_parse", ActionSkipped, "checkpoint parse failure")
}

func TestPlanRespectsMaxPerTick(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	old := fixture.now.Add(-10 * 24 * time.Hour)
	for _, id := range []string{"wt_a", "wt_b", "wt_c"} {
		fixture.worktree(id, "branch-"+id, old)
		fixture.loop("loop_"+id, "completed", id, "branch-"+id, old)
	}

	service := fixture.service()
	service.Config.MaxPerTick = 2
	result, err := service.Plan(context.Background())
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	if result.Summary.Candidates != 3 || result.Summary.WouldClean != 2 {
		t.Fatalf("Plan().Summary = %#v, want candidates=3 wouldClean=2", result.Summary)
	}
	assertDecision(t, result, "wt_c", ActionSkipped, "maxPerTick limit reached")
}

func TestPlanDoesNotCrossMatchSharedBranchAcrossProjects(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	old := fixture.now.Add(-10 * 24 * time.Hour)
	fixture.project("project_2", "/tmp/other")

	fixture.worktreeForProject("project_1", "wt_precise", "looper/shared", old)
	fixture.worktreeForProject("project_2", "wt_same_branch", "looper/shared", old)
	fixture.loop("loop_precise", "failed", "wt_precise", "looper/shared", old)

	fixture.worktreeForProject("project_1", "wt_branch_only", "looper/branch-only", old)
	fixture.worktreeForProject("project_2", "wt_branch_only_other", "looper/branch-only", old)
	fixture.loopWithMetadata("project_1", "loop_branch_only", "failed", `{"branch":"looper/branch-only"}`, old)

	service := fixture.service()
	service.Config.IncludeOrphans = true
	result, err := service.Plan(context.Background())
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}

	assertDecision(t, result, "wt_precise", ActionSkipped, "referenced by protected loop status failed")
	assertDecision(t, result, "wt_same_branch", ActionWouldClean, "eligible in dry-run plan")
	assertDecision(t, result, "wt_branch_only", ActionSkipped, "referenced by protected loop status failed")
	assertDecision(t, result, "wt_branch_only_other", ActionWouldClean, "eligible in dry-run plan")
}

type cleanupFixture struct {
	t     *testing.T
	repos *storage.Repositories
	now   time.Time
	seq   int64
}

func newFixture(t *testing.T) cleanupFixture {
	t.Helper()
	coordinator, err := storage.OpenSQLiteCoordinator(context.Background(), filepath.Join(t.TempDir(), "cleanup.sqlite"), storage.SQLiteCoordinatorOptions{BackupDir: t.TempDir()})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	if _, err := coordinator.MigrationRunner().RunPending(context.Background()); err != nil {
		t.Fatalf("RunPending() error = %v", err)
	}
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.May, 20, 12, 0, 0, 0, time.UTC)
	nowISO := iso(now)
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "project_1", Name: "Looper", RepoPath: "/tmp/looper", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	return cleanupFixture{t: t, repos: repos, now: now}
}

func (f *cleanupFixture) project(id, repoPath string) {
	f.t.Helper()
	nowISO := iso(f.now)
	if err := f.repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: id, Name: id, RepoPath: repoPath, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		f.t.Fatalf("Projects.Upsert() error = %v", err)
	}
}

func (f *cleanupFixture) service() *Service {
	return &Service{
		Repos: f.repos,
		Config: config.WorktreeCleanupConfig{
			Enabled:        true,
			Interval:       "24h",
			RetentionDays:  7,
			MaxPerTick:     10,
			IncludeOrphans: false,
			DryRun:         true,
		},
		Now: func() time.Time { return f.now },
	}
}

func (f *cleanupFixture) worktree(id, branch string, updatedAt time.Time) {
	f.t.Helper()
	f.worktreeForProject("project_1", id, branch, updatedAt)
}

func (f *cleanupFixture) worktreeForProject(projectID, id, branch string, updatedAt time.Time) {
	f.t.Helper()
	if err := f.repos.Worktrees.Upsert(context.Background(), storage.WorktreeRecord{ID: id, ProjectID: projectID, RepoPath: "/tmp/looper", WorktreePath: "/tmp/worktrees/" + id, Branch: branch, Status: "active", CreatedAt: iso(updatedAt), UpdatedAt: iso(updatedAt)}); err != nil {
		f.t.Fatalf("Worktrees.Upsert() error = %v", err)
	}
}

func (f *cleanupFixture) loop(id, status, worktreeID, branch string, updatedAt time.Time) {
	f.t.Helper()
	metadata := `{"worktreeId":"` + worktreeID + `","branch":"` + branch + `"}`
	f.loopWithMetadata("project_1", id, status, metadata, updatedAt)
}

func (f *cleanupFixture) loopWithMetadata(projectID, id, status, metadata string, updatedAt time.Time) {
	f.t.Helper()
	f.seq++
	if err := f.repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: id, Seq: f.seq, ProjectID: projectID, Type: "worker", TargetType: "project", Status: status, MetadataJSON: &metadata, CreatedAt: iso(updatedAt), UpdatedAt: iso(updatedAt)}); err != nil {
		f.t.Fatalf("Loops.Upsert() error = %v", err)
	}
}

func (f *cleanupFixture) run(id, loopID, status, checkpoint string, updatedAt time.Time) {
	f.t.Helper()
	if err := f.repos.Runs.Upsert(context.Background(), storage.RunRecord{ID: id, LoopID: loopID, Status: status, CheckpointJSON: &checkpoint, StartedAt: iso(updatedAt), CreatedAt: iso(updatedAt), UpdatedAt: iso(updatedAt)}); err != nil {
		f.t.Fatalf("Runs.Upsert() error = %v", err)
	}
}

func (f *cleanupFixture) queue(id, loopID, status string, updatedAt time.Time) {
	f.t.Helper()
	if err := f.repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: id, ProjectID: strPtr("project_1"), LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: "project_1", DedupeKey: id, Priority: 1, Status: status, AvailableAt: iso(updatedAt), Attempts: 0, MaxAttempts: 1, CreatedAt: iso(updatedAt), UpdatedAt: iso(updatedAt)}); err != nil {
		f.t.Fatalf("Queue.Upsert() error = %v", err)
	}
}

func assertDecision(t *testing.T, result PlanResult, worktreeID, action, reason string) {
	t.Helper()
	for _, decision := range result.Decisions {
		if decision.Worktree.ID == worktreeID {
			if decision.Action != action || decision.Reason != reason {
				t.Fatalf("decision for %s = %#v, want action=%q reason=%q", worktreeID, decision, action, reason)
			}
			return
		}
	}
	t.Fatalf("missing decision for %s: %#v", worktreeID, result.Decisions)
}

func iso(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}

func strPtr(value string) *string {
	return &value
}
