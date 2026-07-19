package runtime

import (
	"context"
	"testing"
	"time"

	gitinfra "github.com/nexu-io/looper/internal/infra/git"
)

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
