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

// Contract (#580 review): when admission is already closed, do not emit durable
// cleanup events (including started) — only in-memory summary.
func TestWorktreeCleanupPassOmitsEventsWhenAdmissionClosed(t *testing.T) {
	t.Parallel()

	fixture := newWorktreeCleanupFixture(t)
	_ = fixture.seedWorktree(t, "wt_planned", "feature/planned", true)
	// Gate start-event emission under WithAllowClaim so a closed admission
	// cannot persist worktree.cleanup.started (or later cleanup events).
	if err := fixture.runtime.admission.MarkDegraded("before start event"); err != nil {
		t.Fatalf("MarkDegraded() error = %v", err)
	}

	git := &fakeWorktreeCleanupGit{
		listed: map[string][]gitinfra.WorktreeListEntry{fixture.project.RepoPath: {}},
		clean:  map[string]bool{},
	}
	summary := fixture.runtime.runWorktreeCleanupPass(context.Background(), fixture.repos, git, fixture.config)

	if !strings.Contains(summary.LastError, "degraded") {
		t.Fatalf("summary.LastError = %q, want admission degraded", summary.LastError)
	}
	if summary.Cleaned != 0 || summary.Skipped != 0 || summary.Failed != 0 {
		t.Fatalf("summary = %#v, want no candidate mutations after admission closed", summary)
	}
	if len(git.cleanupCalls) != 0 {
		t.Fatalf("cleanupCalls = %#v, want none while degraded", git.cleanupCalls)
	}
	events := fixture.events(t)
	if containsWorktreeCleanupEvent(events, "worktree.cleanup.started") {
		t.Fatalf("events = %#v, want no started event when admission closed at emission", events)
	}
	if containsWorktreeCleanupEvent(events, "worktree.cleanup.completed") {
		t.Fatalf("events = %#v, want no completed event after admission closed", events)
	}
	if containsWorktreeCleanupEvent(events, "worktree.cleanup.skipped") || containsWorktreeCleanupEvent(events, "worktree.cleanup.cleaned") {
		t.Fatalf("events = %#v, want no skip/cleaned events after admission closed", events)
	}
}

// Contract (#580 / review): MarkDegraded mid-pass must refuse remaining cleanup
// mutations. Concurrent MarkDegraded blocks on WithAllowClaim during the first
// delete (cannot close admission mid-remove), then closes after the hold so the
// second candidate never reaches CleanupWorktree.
func TestWorktreeCleanupPassCancelsWhenAdmissionClosesMidPass(t *testing.T) {
	t.Parallel()

	fixture := newWorktreeCleanupFixture(t)
	// Stagger UpdatedAt so plan order is stable (older first).
	first := fixture.seedWorktreeAt(t, "wt_first", "feature/first", true, fixture.now.Add(-2*time.Hour))
	second := fixture.seedWorktreeAt(t, "wt_second", "feature/second", true, fixture.now.Add(-time.Hour))
	enteredFirst := make(chan struct{})
	releaseFirst := make(chan struct{})
	var cleaned []string
	git := &fakeWorktreeCleanupGit{
		listed: map[string][]gitinfra.WorktreeListEntry{fixture.project.RepoPath: {
			{Path: first.WorktreePath, Branch: first.Branch},
			{Path: second.WorktreePath, Branch: second.Branch},
		}},
		clean: map[string]bool{first.WorktreePath: true, second.WorktreePath: true},
		onCleanup: func(input gitinfra.CleanupWorktreeInput) error {
			cleaned = append(cleaned, input.WorktreePath)
			if input.WorktreePath != first.WorktreePath {
				t.Fatalf("CleanupWorktree called for %q after admission should be closed", input.WorktreePath)
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
		done <- fixture.runtime.runWorktreeCleanupPass(context.Background(), fixture.repos, git, fixture.config)
	}()

	select {
	case <-enteredFirst:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first CleanupWorktree under admission hold")
	}

	degradeDone := make(chan error, 1)
	go func() {
		degradeDone <- fixture.runtime.MarkDegraded("mid-pass degrade")
	}()
	// MarkDegraded must block while WithAllowClaim holds admission during delete.
	select {
	case err := <-degradeDone:
		t.Fatalf("MarkDegraded completed while CleanupWorktree held admission: %v", err)
	case <-time.After(30 * time.Millisecond):
	}

	close(releaseFirst)

	select {
	case err := <-degradeDone:
		if err != nil {
			t.Fatalf("MarkDegraded() after hold release error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("MarkDegraded did not complete after CleanupWorktree released admission")
	}

	var summary WorktreeCleanupStatus
	select {
	case summary = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("cleanup pass did not return after mid-pass degrade")
	}

	if len(cleaned) != 1 {
		t.Fatalf("cleaned paths = %#v, want exactly one candidate before admission closed", cleaned)
	}
	if summary.Cleaned != 1 {
		t.Fatalf("summary.Cleaned = %d, want 1", summary.Cleaned)
	}
	if fixture.runtime.admission.State() != AdmissionDegraded {
		t.Fatalf("admission.State() = %q, want degraded after mid-pass MarkDegraded", fixture.runtime.admission.State())
	}
	// LastError is set when the loop-level AllowClaim refuses; a later
	// per-candidate WithAllowClaim skip may only increment Skipped.
	if summary.LastError != "" && !strings.Contains(summary.LastError, "degraded") {
		t.Fatalf("summary.LastError = %q, want empty or admission degraded", summary.LastError)
	}
	if summary.LastError == "" && summary.Skipped < 1 {
		t.Fatalf("summary = %#v, want LastError degraded or at least one skipped candidate", summary)
	}
	stored, err := fixture.repos.Worktrees.GetByID(context.Background(), second.ID)
	if err != nil {
		t.Fatalf("Worktrees.GetByID(remaining) error = %v", err)
	}
	if stored == nil || stored.Status != "active" {
		t.Fatalf("remaining worktree = %#v, want still active after mid-pass admission close", stored)
	}
}

// Contract (#580 review): CleanupWorktree runs under WithAllowClaim so
// MarkDegraded cannot close admission mid-remove. The in-flight delete may
// finish under open admission (closer blocked on the same mutex); after the
// hold releases, MarkDegraded proceeds and cancelWorkProducers runs too late
// to retroactively forbid a remove that already completed under the hold.
func TestWorktreeCleanupInFlightHoldsAdmissionAcrossCleanup(t *testing.T) {
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
		t.Fatal("timed out waiting for in-flight CleanupWorktree under admission hold")
	}

	degradeDone := make(chan error, 1)
	go func() {
		degradeDone <- fixture.runtime.MarkDegraded("mid cleanup pass")
	}()
	// Do not call admission.State() here: it takes the same mutex WithAllowClaim
	// holds for CleanupWorktree and would deadlock the test.
	select {
	case err := <-degradeDone:
		t.Fatalf("MarkDegraded completed while CleanupWorktree held admission: %v", err)
	case <-time.After(30 * time.Millisecond):
	}

	close(release)

	select {
	case err := <-degradeDone:
		if err != nil {
			t.Fatalf("MarkDegraded() after hold release error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("MarkDegraded did not complete after CleanupWorktree released admission")
	}

	var summary WorktreeCleanupStatus
	select {
	case summary = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("cleanup pass did not return after held cleanup finished")
	}

	// Delete completed under open admission (closer was blocked); cleaned=1 is correct.
	if summary.Cleaned != 1 {
		t.Fatalf("summary.Cleaned = %d, want 1 (delete finished under held open admission)", summary.Cleaned)
	}
	if state := fixture.runtime.admission.State(); state != AdmissionDegraded {
		t.Fatalf("admission.State() after pass = %q, want degraded", state)
	}
	stored, err := fixture.repos.Worktrees.GetByID(context.Background(), worktree.ID)
	if err != nil {
		t.Fatalf("Worktrees.GetByID() error = %v", err)
	}
	if stored == nil || stored.Status != "cleaned" {
		t.Fatalf("stored worktree = %#v, want cleaned after held cleanup completed", stored)
	}
	events := fixture.events(t)
	if !containsWorktreeCleanupEvent(events, "worktree.cleanup.cleaned") {
		t.Fatalf("events = %#v, want cleaned event under held admission", events)
	}
}

// Contract: destructive CleanupWorktree is refused when admission closes after
// eligibility checks but before the delete.
func TestCleanWorktreeCandidateRefusesWhenAdmissionClosed(t *testing.T) {
	t.Parallel()

	fixture := newWorktreeCleanupFixture(t)
	worktree := fixture.seedWorktree(t, "wt_closed", "feature/closed", true)
	if err := fixture.runtime.admission.MarkDegraded("before delete"); err != nil {
		t.Fatalf("MarkDegraded() error = %v", err)
	}
	git := &fakeWorktreeCleanupGit{
		listed: map[string][]gitinfra.WorktreeListEntry{fixture.project.RepoPath: {{Path: worktree.WorktreePath, Branch: worktree.Branch}}},
		clean:  map[string]bool{worktree.WorktreePath: true},
	}

	result := fixture.runtime.cleanWorktreeCandidate(context.Background(), fixture.repos, git, fixture.config, fixture.project, worktree, fixture.root, "clean")
	if result.status != "skipped" || !strings.Contains(result.message, "degraded") {
		t.Fatalf("result = %#v, want skipped admission degraded", result)
	}
	if len(git.cleanupCalls) != 0 {
		t.Fatalf("cleanupCalls = %#v, want none while degraded", git.cleanupCalls)
	}
}

// Contract (#580 review): plan-skip event append is under WithAllowClaim so a
// closed admission cannot persist worktree.cleanup.skipped after close.
func TestWorktreeCleanupPlanSkipHoldsAdmission(t *testing.T) {
	t.Parallel()

	fixture := newWorktreeCleanupFixture(t)
	worktree := fixture.seedWorktree(t, "wt_plan_skip", "feature/plan-skip", true)
	if err := fixture.runtime.admission.MarkDegraded("before plan skip"); err != nil {
		t.Fatalf("MarkDegraded() error = %v", err)
	}

	err := fixture.runtime.recordWorktreeCleanupPlanSkip(context.Background(), fixture.repos, worktree, "below_min_age")
	if !errors.Is(err, ErrAdmissionDegraded) {
		t.Fatalf("recordWorktreeCleanupPlanSkip() = %v, want ErrAdmissionDegraded", err)
	}
	events := fixture.events(t)
	if containsWorktreeCleanupEvent(events, "worktree.cleanup.skipped") {
		t.Fatalf("events = %#v, want no skip event when admission closed at write boundary", events)
	}
}

// Contract (#580 review): candidate skip/failure record helpers hold admission
// across worktrees touch + event append so degradation after eligibility checks
// cannot commit durable cleanup mutations after close.
func TestWorktreeCleanupRecordHelpersHoldAdmission(t *testing.T) {
	t.Parallel()

	fixture := newWorktreeCleanupFixture(t)
	worktree := fixture.seedWorktree(t, "wt_record_gate", "feature/record-gate", true)
	if err := fixture.runtime.admission.MarkDegraded("before record write"); err != nil {
		t.Fatalf("MarkDegraded() error = %v", err)
	}

	skip := fixture.runtime.recordWorktreeCleanupSkip(context.Background(), fixture.repos, worktree, "dirty_git_status")
	if skip.status != "skipped" || !strings.Contains(skip.message, "degraded") {
		t.Fatalf("recordWorktreeCleanupSkip() = %#v, want skipped admission degraded", skip)
	}
	failure := fixture.runtime.recordWorktreeCleanupFailure(context.Background(), fixture.repos, worktree, errors.New("git list failed"))
	if failure.status != "skipped" || !strings.Contains(failure.message, "degraded") {
		t.Fatalf("recordWorktreeCleanupFailure() = %#v, want skipped admission degraded", failure)
	}

	stored, err := fixture.repos.Worktrees.GetByID(context.Background(), worktree.ID)
	if err != nil {
		t.Fatalf("Worktrees.GetByID() error = %v", err)
	}
	if stored == nil || stored.UpdatedAt != worktree.UpdatedAt {
		t.Fatalf("stored worktree = %#v, want UpdatedAt unchanged after admission closed (no touch)", stored)
	}
	events := fixture.events(t)
	if containsWorktreeCleanupEvent(events, "worktree.cleanup.skipped") || containsWorktreeCleanupEvent(events, "worktree.cleanup.failed") {
		t.Fatalf("events = %#v, want no skip/failed events when admission closed at write boundary", events)
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
