package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	gitinfra "github.com/nexu-io/looper/internal/infra/git"
	"github.com/nexu-io/looper/internal/storage"
	"github.com/nexu-io/looper/internal/worktreecleanup"
)

func TestWorktreeCleanupPassCleansEligibleCheckout(t *testing.T) {
	t.Parallel()

	fixture := newWorktreeCleanupFixture(t)
	worktree := fixture.seedWorktree(t, "wt_clean", "feature/clean", true)
	git := &fakeWorktreeCleanupGit{
		listed: map[string][]gitinfra.WorktreeListEntry{fixture.project.RepoPath: {{Path: worktree.WorktreePath, Branch: worktree.Branch}}},
		clean:  map[string]bool{worktree.WorktreePath: true},
		onCleanup: func(input gitinfra.CleanupWorktreeInput) error {
			if input.WorktreePath != worktree.WorktreePath {
				t.Fatalf("CleanupWorktree().WorktreePath = %q, want %q", input.WorktreePath, worktree.WorktreePath)
			}
			updated := worktree
			nowISO := fixture.now.Format("2006-01-02T15:04:05.000Z")
			updated.Status = "cleaned"
			updated.CleanedAt = &nowISO
			updated.UpdatedAt = nowISO
			return fixture.repos.Worktrees.Upsert(context.Background(), updated)
		},
	}

	summary := fixture.runtime.runWorktreeCleanupPass(context.Background(), fixture.repos, git, fixture.config)

	if summary.LastStatus != "completed" || summary.Cleaned != 1 || summary.Failed != 0 {
		t.Fatalf("summary = %#v, want completed cleaned=1 failed=0", summary)
	}
	stored, err := fixture.repos.Worktrees.GetByID(context.Background(), worktree.ID)
	if err != nil {
		t.Fatalf("Worktrees.GetByID() error = %v", err)
	}
	if stored == nil || stored.Status != "cleaned" || stored.CleanedAt == nil {
		t.Fatalf("stored worktree = %#v, want cleaned with cleaned_at", stored)
	}
	events := fixture.events(t)
	if !containsWorktreeCleanupEvent(events, "worktree.cleanup.started") || !containsWorktreeCleanupEvent(events, "worktree.cleanup.cleaned") || !containsWorktreeCleanupEvent(events, "worktree.cleanup.completed") {
		t.Fatalf("events = %#v, want started/cleaned/completed", events)
	}
}

func TestWorktreeCleanupPassSkipsDirtyCheckout(t *testing.T) {
	t.Parallel()

	fixture := newWorktreeCleanupFixture(t)
	worktree := fixture.seedWorktree(t, "wt_dirty", "feature/dirty", true)
	git := &fakeWorktreeCleanupGit{
		listed: map[string][]gitinfra.WorktreeListEntry{fixture.project.RepoPath: {{Path: worktree.WorktreePath, Branch: worktree.Branch}}},
		clean:  map[string]bool{worktree.WorktreePath: false},
	}

	summary := fixture.runtime.runWorktreeCleanupPass(context.Background(), fixture.repos, git, fixture.config)

	if summary.LastStatus != "completed" || summary.Skipped != 1 || summary.Cleaned != 0 || len(git.cleanupCalls) != 0 {
		t.Fatalf("summary = %#v cleanupCalls=%#v, want skipped dirty checkout", summary, git.cleanupCalls)
	}
	stored, err := fixture.repos.Worktrees.GetByID(context.Background(), worktree.ID)
	if err != nil {
		t.Fatalf("Worktrees.GetByID() error = %v", err)
	}
	if stored == nil || stored.Status != "active" {
		t.Fatalf("stored worktree = %#v, want active", stored)
	}
	events := fixture.events(t)
	if !containsWorktreeCleanupEventPayload(events, "worktree.cleanup.skipped", "dirty_git_status") {
		t.Fatalf("events = %#v, want dirty_git_status skip", events)
	}
}

func TestWorktreeCleanupPassDoesNotStarveNewerCandidateAfterSkip(t *testing.T) {
	t.Parallel()

	fixture := newWorktreeCleanupFixture(t)
	fixture.config.Daemon.WorktreeCleanup.MaxPerTick = 1
	oldDirty := fixture.seedWorktreeAt(t, "wt_old_dirty", "feature/old-dirty", true, fixture.now.Add(-2*time.Hour))
	newClean := fixture.seedWorktreeAt(t, "wt_new_clean", "feature/new-clean", true, fixture.now.Add(-time.Hour))
	git := &fakeWorktreeCleanupGit{
		listed: map[string][]gitinfra.WorktreeListEntry{fixture.project.RepoPath: {
			{Path: oldDirty.WorktreePath, Branch: oldDirty.Branch},
			{Path: newClean.WorktreePath, Branch: newClean.Branch},
		}},
		clean: map[string]bool{oldDirty.WorktreePath: false, newClean.WorktreePath: true},
		onCleanup: func(input gitinfra.CleanupWorktreeInput) error {
			if input.WorktreePath != newClean.WorktreePath {
				t.Fatalf("CleanupWorktree().WorktreePath = %q, want %q", input.WorktreePath, newClean.WorktreePath)
			}
			updated := newClean
			nowISO := formatJavaScriptISOString(fixture.now)
			updated.Status = "cleaned"
			updated.CleanedAt = &nowISO
			updated.UpdatedAt = nowISO
			return fixture.repos.Worktrees.Upsert(context.Background(), updated)
		},
	}

	first := fixture.runtime.runWorktreeCleanupPass(context.Background(), fixture.repos, git, fixture.config)
	if first.LastStatus != "completed" || first.Skipped != 2 || first.Cleaned != 0 {
		t.Fatalf("first summary = %#v, want dirty worktree plus maxPerTick skip", first)
	}

	second := fixture.runtime.runWorktreeCleanupPass(context.Background(), fixture.repos, git, fixture.config)
	if second.LastStatus != "completed" || second.Cleaned != 1 || second.Skipped != 1 {
		t.Fatalf("second summary = %#v, want newer clean worktree cleaned and remaining dirty skipped by maxPerTick", second)
	}
	if len(git.cleanupCalls) != 1 {
		t.Fatalf("cleanupCalls = %#v, want one clean candidate cleanup", git.cleanupCalls)
	}
	storedDirty, err := fixture.repos.Worktrees.GetByID(context.Background(), oldDirty.ID)
	if err != nil {
		t.Fatalf("Worktrees.GetByID(old dirty) error = %v", err)
	}
	if got, want := storedDirty.UpdatedAt, formatJavaScriptISOString(fixture.now); got != want {
		t.Fatalf("old dirty UpdatedAt = %q, want cleanup attempt timestamp %q", got, want)
	}
	storedClean, err := fixture.repos.Worktrees.GetByID(context.Background(), newClean.ID)
	if err != nil {
		t.Fatalf("Worktrees.GetByID(new clean) error = %v", err)
	}
	if storedClean == nil || storedClean.Status != "cleaned" {
		t.Fatalf("stored clean worktree = %#v, want cleaned", storedClean)
	}
}

func TestWorktreeCleanupPassRespectsRetentionAndOrphanPolicy(t *testing.T) {
	t.Parallel()

	fixture := newWorktreeCleanupFixture(t)
	fixture.config.Daemon.WorktreeCleanup.RetentionDays = 7
	fixture.config.Daemon.WorktreeCleanup.IncludeOrphans = false
	old := fixture.now.Add(-10 * 24 * time.Hour)
	recent := fixture.seedWorktreeAt(t, "wt_recent", "feature/recent", true, fixture.now)
	oldOrphan := fixture.seedWorktreeAt(t, "wt_old_orphan", "feature/old-orphan", true, old)
	oldReferenced := fixture.seedWorktreeAt(t, "wt_old_referenced", "feature/old-referenced", true, old)
	fixture.seedLoopForWorktree(t, "loop_recent", recent, "completed", fixture.now)
	fixture.seedLoopForWorktree(t, "loop_old_referenced", oldReferenced, "completed", old)
	git := &fakeWorktreeCleanupGit{
		listed: map[string][]gitinfra.WorktreeListEntry{fixture.project.RepoPath: {
			{Path: recent.WorktreePath, Branch: recent.Branch},
			{Path: oldOrphan.WorktreePath, Branch: oldOrphan.Branch},
			{Path: oldReferenced.WorktreePath, Branch: oldReferenced.Branch},
		}},
		clean: map[string]bool{
			recent.WorktreePath:        true,
			oldOrphan.WorktreePath:     true,
			oldReferenced.WorktreePath: true,
		},
		onCleanup: func(input gitinfra.CleanupWorktreeInput) error {
			updated, err := fixture.repos.Worktrees.GetByID(context.Background(), worktreeIDForPath(map[string]storage.WorktreeRecord{
				recent.WorktreePath:        recent,
				oldOrphan.WorktreePath:     oldOrphan,
				oldReferenced.WorktreePath: oldReferenced,
			}, input.WorktreePath))
			if err != nil {
				return err
			}
			nowISO := formatJavaScriptISOString(fixture.now)
			updated.Status = "cleaned"
			updated.CleanedAt = &nowISO
			updated.UpdatedAt = nowISO
			return fixture.repos.Worktrees.Upsert(context.Background(), *updated)
		},
	}

	first := fixture.runtime.runWorktreeCleanupPass(context.Background(), fixture.repos, git, fixture.config)
	if first.LastStatus != "completed" || first.Cleaned != 1 || first.Skipped != 2 || len(git.cleanupCalls) != 1 {
		t.Fatalf("first summary = %#v cleanupCalls=%#v, want old referenced cleaned and policy skips", first, git.cleanupCalls)
	}
	if git.cleanupCalls[0].WorktreePath != oldReferenced.WorktreePath {
		t.Fatalf("first cleanup path = %q, want old referenced path %q", git.cleanupCalls[0].WorktreePath, oldReferenced.WorktreePath)
	}
	events := fixture.events(t)
	if !containsWorktreeCleanupEventPayload(events, "worktree.cleanup.skipped", "within retention window") {
		t.Fatalf("events = %#v, want retention skip", events)
	}
	if !containsWorktreeCleanupEventPayload(events, "worktree.cleanup.skipped", "orphan worktree and includeOrphans=false") {
		t.Fatalf("events = %#v, want orphan policy skip", events)
	}

	fixture.config.Daemon.WorktreeCleanup.RetentionDays = 0
	fixture.config.Daemon.WorktreeCleanup.IncludeOrphans = true
	second := fixture.runtime.runWorktreeCleanupPass(context.Background(), fixture.repos, git, fixture.config)
	if second.LastStatus != "completed" || second.Cleaned != 2 || second.Skipped != 0 {
		t.Fatalf("second summary = %#v, want recent and orphan cleaned once config allows them", second)
	}
	if len(git.cleanupCalls) != 3 {
		t.Fatalf("cleanupCalls = %#v, want all allowed worktrees cleaned", git.cleanupCalls)
	}
}

func TestCleanupWorktreeCandidateSkipsQueueItemInsertedAfterPlanning(t *testing.T) {
	t.Parallel()

	fixture := newWorktreeCleanupFixture(t)
	worktree := fixture.seedWorktree(t, "wt_queue_after_plan", "feature/queue-after-plan", true)
	plan, err := (&worktreecleanup.Service{
		Repos:  fixture.repos,
		Config: fixture.config.Daemon.WorktreeCleanup,
		Now:    func() time.Time { return fixture.now },
	}).Plan(context.Background())
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	if len(plan.Decisions) != 1 || plan.Decisions[0].Action != worktreecleanup.ActionWouldClean {
		t.Fatalf("plan.Decisions = %#v, want worktree selected before queue item exists", plan.Decisions)
	}

	fixture.seedQueueForWorktree(t, "queue_after_plan", worktree, "queued")
	git := &fakeWorktreeCleanupGit{
		listed: map[string][]gitinfra.WorktreeListEntry{fixture.project.RepoPath: {{Path: worktree.WorktreePath, Branch: worktree.Branch}}},
		clean:  map[string]bool{worktree.WorktreePath: true},
		onCleanup: func(input gitinfra.CleanupWorktreeInput) error {
			t.Fatalf("CleanupWorktree() called for %q, want queued worktree skipped", input.WorktreePath)
			return nil
		},
	}

	result := fixture.runtime.cleanupWorktreeCandidate(context.Background(), fixture.repos, git, fixture.config, worktree)

	if result.status != "skipped" || result.message != "active_queue_item_references_worktree" {
		t.Fatalf("result = %#v, want active queue skip", result)
	}
	if len(git.cleanupCalls) != 0 {
		t.Fatalf("cleanupCalls = %#v, want no cleanup", git.cleanupCalls)
	}
	stored, err := fixture.repos.Worktrees.GetByID(context.Background(), worktree.ID)
	if err != nil {
		t.Fatalf("Worktrees.GetByID() error = %v", err)
	}
	if stored == nil || stored.Status != "active" {
		t.Fatalf("stored worktree = %#v, want active", stored)
	}
	events := fixture.events(t)
	if !containsWorktreeCleanupEventPayload(events, "worktree.cleanup.skipped", "active_queue_item_references_worktree") {
		t.Fatalf("events = %#v, want active queue skip", events)
	}
}

func TestWorktreeCleanupPassRecordsFailureAndContinues(t *testing.T) {
	t.Parallel()

	fixture := newWorktreeCleanupFixture(t)
	failing := fixture.seedWorktree(t, "wt_fail", "feature/fail", true)
	clean := fixture.seedWorktree(t, "wt_after_fail", "feature/after-fail", true)
	cleanupErr := errors.New("git worktree remove failed")
	git := &fakeWorktreeCleanupGit{
		listed: map[string][]gitinfra.WorktreeListEntry{fixture.project.RepoPath: {
			{Path: failing.WorktreePath, Branch: failing.Branch},
			{Path: clean.WorktreePath, Branch: clean.Branch},
		}},
		clean: map[string]bool{failing.WorktreePath: true, clean.WorktreePath: true},
		onCleanup: func(input gitinfra.CleanupWorktreeInput) error {
			if input.WorktreePath == failing.WorktreePath {
				return cleanupErr
			}
			updated, err := fixture.repos.Worktrees.GetByID(context.Background(), "wt_after_fail")
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

	summary := fixture.runtime.runWorktreeCleanupPass(context.Background(), fixture.repos, git, fixture.config)

	if summary.LastStatus != "failed" || summary.Failed != 1 || summary.Cleaned != 1 || !strings.Contains(summary.LastError, cleanupErr.Error()) {
		t.Fatalf("summary = %#v, want failed=1 cleaned=1 last error", summary)
	}
	if len(git.cleanupCalls) != 2 {
		t.Fatalf("cleanupCalls = %#v, want both candidates attempted", git.cleanupCalls)
	}
}

type worktreeCleanupFixture struct {
	runtime *Runtime
	config  config.Config
	repos   *storage.Repositories
	project storage.ProjectRecord
	root    string
	now     time.Time
	seq     int64
}

func newWorktreeCleanupFixture(t *testing.T) worktreeCleanupFixture {
	t.Helper()
	root := t.TempDir()
	cfg, err := config.DefaultConfig(root)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Daemon.WorktreeCleanup.Enabled = true
	cfg.Daemon.WorktreeCleanup.DryRun = false
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
	if _, err := coordinator.MigrationRunner().RunPending(context.Background(), storage.RunPendingOptions{}); err != nil {
		t.Fatalf("RunPending() error = %v", err)
	}
	repos := storage.NewRepositories(coordinator.DB())
	project := storage.ProjectRecord{ID: "project_1", Name: "Project", RepoPath: repoPath, BaseBranch: stringPtr("main"), MetadataJSON: stringPtr(`{"worktreeRoot":"` + worktreeRoot + `"}`), CreatedAt: now.Format("2006-01-02T15:04:05.000Z"), UpdatedAt: now.Format("2006-01-02T15:04:05.000Z")}
	if err := repos.Projects.Upsert(context.Background(), project); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	rt := New(Options{Config: cfg, Now: func() time.Time { return now }, WorktreeCleanupInitialDelay: -1})
	// Mid-pass AllowClaim rechecks require ready admission; unit tests exercise
	// candidate mutations, not the starting-state no-op path.
	if err := rt.admission.MarkReady("worktree cleanup fixture"); err != nil {
		t.Fatalf("admission.MarkReady() error = %v", err)
	}
	return worktreeCleanupFixture{
		runtime: rt,
		config:  cfg,
		repos:   repos,
		project: project,
		root:    worktreeRoot,
		now:     now,
	}
}

func (f worktreeCleanupFixture) seedWorktree(t *testing.T, id, branch string, createDir bool) storage.WorktreeRecord {
	t.Helper()
	return f.seedWorktreeAt(t, id, branch, createDir, f.now)
}

func (f worktreeCleanupFixture) seedWorktreeAt(t *testing.T, id, branch string, createDir bool, updatedAt time.Time) storage.WorktreeRecord {
	t.Helper()
	path := filepath.Join(f.root, id)
	if createDir {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatalf("MkdirAll(worktree) error = %v", err)
		}
	}
	nowISO := formatJavaScriptISOString(updatedAt)
	record := storage.WorktreeRecord{ID: id, ProjectID: f.project.ID, RepoPath: f.project.RepoPath, WorktreePath: path, Branch: branch, BaseBranch: stringPtr("main"), Status: "active", CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := f.repos.Worktrees.Upsert(context.Background(), record); err != nil {
		t.Fatalf("Worktrees.Upsert() error = %v", err)
	}
	return record
}

func (f *worktreeCleanupFixture) seedLoopForWorktree(t *testing.T, id string, worktree storage.WorktreeRecord, status string, updatedAt time.Time) {
	t.Helper()
	f.seq++
	metadata := `{"worktreeId":"` + worktree.ID + `","branch":"` + worktree.Branch + `","worktreePath":"` + worktree.WorktreePath + `"}`
	record := storage.LoopRecord{ID: id, Seq: f.seq, ProjectID: worktree.ProjectID, Type: "worker", TargetType: "project", Status: status, MetadataJSON: &metadata, CreatedAt: formatJavaScriptISOString(updatedAt), UpdatedAt: formatJavaScriptISOString(updatedAt)}
	if err := f.repos.Loops.Upsert(context.Background(), record); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
}

func (f worktreeCleanupFixture) seedQueueForWorktree(t *testing.T, id string, worktree storage.WorktreeRecord, status string) {
	t.Helper()
	payload := `{"worktreeId":"` + worktree.ID + `","branch":"` + worktree.Branch + `","worktreePath":"` + worktree.WorktreePath + `"}`
	if err := f.repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{
		ID:          id,
		ProjectID:   &worktree.ProjectID,
		Type:        "worker",
		TargetType:  "project",
		TargetID:    worktree.ProjectID,
		DedupeKey:   "worker:" + id,
		Priority:    storage.QueuePriorityWorker,
		Status:      status,
		AvailableAt: formatJavaScriptISOString(f.now),
		Attempts:    0,
		MaxAttempts: 3,
		PayloadJSON: &payload,
		CreatedAt:   formatJavaScriptISOString(f.now),
		UpdatedAt:   formatJavaScriptISOString(f.now),
	}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}
}

func (f worktreeCleanupFixture) events(t *testing.T) []storage.EventLogRecord {
	t.Helper()
	events, err := f.repos.Events.List(context.Background(), 20)
	if err != nil {
		t.Fatalf("Events.List() error = %v", err)
	}
	return events
}

type fakeWorktreeCleanupGit struct {
	listed       map[string][]gitinfra.WorktreeListEntry
	clean        map[string]bool
	cleanupCalls []gitinfra.CleanupWorktreeInput
	onCleanup    func(gitinfra.CleanupWorktreeInput) error
}

func (f *fakeWorktreeCleanupGit) ListWorktrees(_ context.Context, repoPath string) ([]gitinfra.WorktreeListEntry, error) {
	return append([]gitinfra.WorktreeListEntry{}, f.listed[repoPath]...), nil
}

func (f *fakeWorktreeCleanupGit) WorktreeClean(_ context.Context, worktreePath string) (bool, error) {
	return f.clean[worktreePath], nil
}

func (f *fakeWorktreeCleanupGit) CleanupWorktree(_ context.Context, input gitinfra.CleanupWorktreeInput) error {
	f.cleanupCalls = append(f.cleanupCalls, input)
	// Mirror production: AdmitStart gates process Start only; the long remove
	// body (onCleanup) runs outside the admission hold so MarkDegraded can
	// cancel while remove is in flight.
	if input.AdmitStart != nil {
		if err := input.AdmitStart(func() error { return nil }); err != nil {
			return err
		}
	}
	if f.onCleanup != nil {
		return f.onCleanup(input)
	}
	return nil
}

func containsWorktreeCleanupEvent(events []storage.EventLogRecord, eventType string) bool {
	for _, event := range events {
		if event.EventType == eventType {
			return true
		}
	}
	return false
}

func containsWorktreeCleanupEventPayload(events []storage.EventLogRecord, eventType, needle string) bool {
	for _, event := range events {
		if event.EventType == eventType && strings.Contains(event.PayloadJSON, needle) {
			return true
		}
	}
	return false
}

func worktreeIDForPath(worktrees map[string]storage.WorktreeRecord, path string) string {
	if worktree, ok := worktrees[path]; ok {
		return worktree.ID
	}
	return ""
}
