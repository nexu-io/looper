package runtime

import (
	"context"
	"strings"
	"testing"
	"time"

	gitinfra "github.com/nexu-io/looper/internal/infra/git"
)

// Contract (#592 review): cleanWorktreeCandidate must not start CleanupWorktree
// after admission closes — MarkDegraded cancels producers under the same
// admission.mu as the state flip, and the candidate rechecks ctx after AllowClaim.
func TestCleanWorktreeCandidateSkipsWhenContextCanceledAfterAllowClaim(t *testing.T) {
	t.Parallel()

	fixture := newWorktreeCleanupFixture(t)
	worktree := fixture.seedWorktree(t, "wt_ctx_skip", "feature/ctx-skip", true)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // simulate MarkDegraded cancelWorkProducers after AllowClaim passed

	git := &fakeWorktreeCleanupGit{
		listed: map[string][]gitinfra.WorktreeListEntry{fixture.project.RepoPath: {
			{Path: worktree.WorktreePath, Branch: worktree.Branch},
		}},
		clean: map[string]bool{worktree.WorktreePath: true},
	}

	result := fixture.runtime.cleanWorktreeCandidate(ctx, fixture.repos, git, fixture.config, fixture.project, worktree, fixture.root, "clean")
	if result.status != "skipped" {
		t.Fatalf("result = %#v, want skipped when ctx already canceled", result)
	}
	if len(git.cleanupCalls) != 0 {
		t.Fatalf("cleanupCalls = %#v, want none when ctx canceled before remove", git.cleanupCalls)
	}
}

// Contract (#580 / #592 review): MarkDegraded mid-pass must refuse remaining
// cleanup mutations without holding admission.mu across CleanupWorktree.
// Concurrent MarkDegraded must complete while the cancellable git remove is
// in flight so cancelWorkProducers can run (no deadlock with admission).
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
		t.Fatal("timed out waiting for first CleanupWorktree")
	}

	// Must not block on admission.mu while CleanupWorktree is in flight.
	degradeDone := make(chan error, 1)
	go func() {
		degradeDone <- fixture.runtime.MarkDegraded("mid-pass degrade")
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
	// per-candidate skip may only increment Skipped.
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
