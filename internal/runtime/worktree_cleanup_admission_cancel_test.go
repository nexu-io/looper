package runtime

import (
	"context"
	"strings"
	"testing"
	"time"

	gitinfra "github.com/nexu-io/looper/internal/infra/git"
)

// Contract (#592 review): cleanWorktreeCandidate must not start the remove
// after the cleanup context is canceled — AdmitStart rechecks ctx under the
// same WithAllowClaim hold as process Start.
func TestCleanWorktreeCandidateSkipsWhenContextCanceledAfterAllowClaim(t *testing.T) {
	t.Parallel()

	fixture := newWorktreeCleanupFixture(t)
	worktree := fixture.seedWorktree(t, "wt_ctx_skip", "feature/ctx-skip", true)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // simulate MarkDegraded cancelWorkProducers before Start

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
	// AdmitStart refuses before onCleanup; CleanupWorktree may still be entered
	// for validation, but the fake records the call only after append — the
	// important contract is no onCleanup mutation (cleanupCalls with AdmitStart
	// failure returns before onCleanup; call is still appended for observability).
	if len(git.cleanupCalls) != 1 {
		t.Fatalf("cleanupCalls = %#v, want one attempt that refused at AdmitStart", git.cleanupCalls)
	}
}

// Contract (#592 review): AdmitStart holds admission.mu across the Start gate
// so MarkDegraded cannot close admission between allow and process launch.
// The long remove body stays outside the hold (no degrade deadlock).
func TestCleanWorktreeCandidateAdmitStartHoldsAdmissionAcrossStart(t *testing.T) {
	t.Parallel()

	fixture := newWorktreeCleanupFixture(t)
	worktree := fixture.seedWorktree(t, "wt_admit_start", "feature/admit-start", true)

	enteredStart := make(chan struct{})
	releaseStart := make(chan struct{})
	bodyStarted := make(chan struct{})
	git := &fakeWorktreeCleanupGit{
		listed: map[string][]gitinfra.WorktreeListEntry{fixture.project.RepoPath: {
			{Path: worktree.WorktreePath, Branch: worktree.Branch},
		}},
		clean: map[string]bool{worktree.WorktreePath: true},
		onCleanup: func(input gitinfra.CleanupWorktreeInput) error {
			close(bodyStarted)
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
	// Override CleanupWorktree path via onCleanup alone is insufficient: inject
	// a blocking AdmitStart by wrapping through a custom git that holds during start.
	blockingGit := &admitStartHoldGit{
		fake:         git,
		enteredStart: enteredStart,
		releaseStart: releaseStart,
		wantWorktree: worktree.WorktreePath,
	}

	done := make(chan worktreeCleanupCandidateResult, 1)
	go func() {
		done <- fixture.runtime.cleanWorktreeCandidate(context.Background(), fixture.repos, blockingGit, fixture.config, fixture.project, worktree, fixture.root, "clean")
	}()

	select {
	case <-enteredStart:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for AdmitStart critical section")
	}

	// MarkDegraded must block while AdmitStart holds admission.mu.
	degradeDone := make(chan error, 1)
	go func() {
		degradeDone <- fixture.runtime.MarkDegraded("during admit start")
	}()
	select {
	case err := <-degradeDone:
		t.Fatalf("MarkDegraded completed while AdmitStart held admission: %v", err)
	case <-time.After(50 * time.Millisecond):
		// expected: still blocked
	}

	// Body must not run until Start gate releases (Start-only hold).
	select {
	case <-bodyStarted:
		t.Fatal("remove body started while AdmitStart still held")
	default:
	}

	close(releaseStart)

	select {
	case err := <-degradeDone:
		if err != nil {
			t.Fatalf("MarkDegraded() after AdmitStart released error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("MarkDegraded did not complete after AdmitStart released")
	}

	select {
	case result := <-done:
		if result.status != "cleaned" {
			t.Fatalf("result = %#v, want cleaned after admitted start", result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cleanWorktreeCandidate did not return")
	}
	if fixture.runtime.admission.State() != AdmissionDegraded {
		t.Fatalf("admission.State() = %q, want degraded", fixture.runtime.admission.State())
	}
}

// admitStartHoldGit blocks inside AdmitStart to prove the hold covers Start only.
type admitStartHoldGit struct {
	fake         *fakeWorktreeCleanupGit
	enteredStart chan struct{}
	releaseStart chan struct{}
	wantWorktree string
}

func (g *admitStartHoldGit) ListWorktrees(ctx context.Context, repoPath string) ([]gitinfra.WorktreeListEntry, error) {
	return g.fake.ListWorktrees(ctx, repoPath)
}

func (g *admitStartHoldGit) WorktreeClean(ctx context.Context, worktreePath string) (bool, error) {
	return g.fake.WorktreeClean(ctx, worktreePath)
}

func (g *admitStartHoldGit) CleanupWorktree(ctx context.Context, input gitinfra.CleanupWorktreeInput) error {
	if input.AdmitStart == nil {
		return g.fake.CleanupWorktree(ctx, input)
	}
	// Replace AdmitStart with one that blocks while still invoking the real
	// runtime gate first would double-hold; instead nest: runtime's AdmitStart
	// is already the outer hold. We need to block *inside* that hold.
	// cleanWorktreeCandidate always sets AdmitStart; wrap it so the hold body
	// blocks before calling start.
	inner := input.AdmitStart
	input.AdmitStart = func(start func() error) error {
		return inner(func() error {
			close(g.enteredStart)
			<-g.releaseStart
			return start()
		})
	}
	return g.fake.CleanupWorktree(ctx, input)
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
