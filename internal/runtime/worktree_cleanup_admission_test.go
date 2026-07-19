package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"

	gitinfra "github.com/nexu-io/looper/internal/infra/git"
)

// Contract (#580 review): when admission is already closed, do not emit durable
// cleanup events (including started) — only in-memory summary.
func TestWorktreeCleanupPassOmitsEventsWhenAdmissionClosed(t *testing.T) {
	t.Parallel()

	fixture := newWorktreeCleanupFixture(t)
	_ = fixture.seedWorktree(t, "wt_planned", "feature/planned", true)
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
	// CleanupWorktree is entered so AdmitStart can refuse under the gate; the
	// fake records the attempt, but onCleanup must not run (no mutation body).
	if len(git.cleanupCalls) != 1 {
		t.Fatalf("cleanupCalls = %#v, want one AdmitStart-refused attempt", git.cleanupCalls)
	}
}

// Contract (#592 review): Plan failure terminal events (failed/completed) are
// held under WithAllowClaim so a closed admission cannot append them after
// MarkDegraded cancels the cleanup context mid-Plan.
func TestWorktreeCleanupPassGatesPlanFailureTerminalEvents(t *testing.T) {
	t.Parallel()

	fixture := newWorktreeCleanupFixture(t)
	// Force Plan to fail after the start-event gate without canceling ctx, so
	// event appends are not masked by context.Canceled on ExecContext.
	fixture.repos.Worktrees = nil
	git := &fakeWorktreeCleanupGit{}

	// Open admission: plan failure still records terminal events under the hold.
	summary := fixture.runtime.runWorktreeCleanupPass(context.Background(), fixture.repos, git, fixture.config)
	if summary.LastStatus != "failed" || summary.Failed != 1 {
		t.Fatalf("summary = %#v, want failed status with Failed=1", summary)
	}
	if summary.LastError == "" {
		t.Fatal("summary.LastError empty, want plan failure message")
	}
	events := fixture.events(t)
	if !containsWorktreeCleanupEvent(events, "worktree.cleanup.started") {
		t.Fatalf("events = %#v, want started before plan failure", events)
	}
	if !containsWorktreeCleanupEvent(events, "worktree.cleanup.failed") || !containsWorktreeCleanupEvent(events, "worktree.cleanup.completed") {
		t.Fatalf("events = %#v, want failed+completed while admission open", events)
	}

	// Closed admission before the pass: start gate refuses; no durable events
	// from a second attempt (prior events remain from the open-admission pass).
	if err := fixture.runtime.admission.MarkDegraded("before plan failure pass"); err != nil {
		t.Fatalf("MarkDegraded() error = %v", err)
	}
	before := len(fixture.events(t))
	summary2 := fixture.runtime.runWorktreeCleanupPass(context.Background(), fixture.repos, git, fixture.config)
	if !strings.Contains(summary2.LastError, "degraded") {
		t.Fatalf("summary2.LastError = %q, want admission degraded", summary2.LastError)
	}
	after := fixture.events(t)
	if len(after) != before {
		t.Fatalf("events grew from %d to %d after closed-admission plan path; events = %#v", before, len(after), after)
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
