package runtime

import (
	"context"
	"testing"
	"time"

	gitinfra "github.com/nexu-io/looper/internal/infra/git"
)

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
