package fixer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/storage"
	"github.com/nexu-io/looper/internal/worktreesafety"
)

func TestRunPrepareWorktreeStepAdoptsSameHeadDirtyWorktreeWithProvenance(t *testing.T) {
	t.Parallel()

	// Interrupted-repair lifecycle: rewind cleared PreparedAt but kept Path + OwnerToken.
	// Same-head adopt must run before CleanupWorktree so partial agent dirt survives.
	f := newDirtyAdoptFixture(t)
	prior := f.rewindCheckpointWithDiskOwnership(t)
	priorToken := prior.Worktree.OwnerToken
	git := &fakeGitGateway{
		createResult:   CreateWorktreeResult{WorktreePath: f.wtPath, Branch: f.branch, HeadSHA: f.headSHA},
		prepareResult:  PrepareWorktreeResult{HeadSHA: f.headSHA, Clean: false},
		inspectResults: []InspectHeadResult{{HeadSHA: f.headSHA, HasUncommittedChanges: true}},
	}
	runner := New(Options{Git: git})
	checkpoint, err := runner.runPrepareWorktreeStep(context.Background(), stepInput{
		Project:    f.project(),
		Loop:       storage.LoopRecord{ID: "loop_1", Status: "running"},
		Repo:       "acme/looper",
		PRNumber:   42,
		Checkpoint: prior,
	})
	if err != nil {
		t.Fatalf("runPrepareWorktreeStep() error = %v", err)
	}
	if checkpoint.ResumePolicy != "advance_from_checkpoint" {
		t.Fatalf("checkpoint.ResumePolicy = %q, want advance_from_checkpoint", checkpoint.ResumePolicy)
	}
	if checkpoint.Pause != nil {
		t.Fatalf("checkpoint.Pause = %#v, want nil", checkpoint.Pause)
	}
	if checkpoint.Worktree == nil {
		t.Fatal("checkpoint.Worktree = nil, want adopted worktree")
	}
	if checkpoint.Worktree.HeadSHA != f.headSHA || checkpoint.Worktree.BaseHeadSHA != f.headSHA {
		t.Fatalf("checkpoint.Worktree = %#v, want head/base %s", checkpoint.Worktree, f.headSHA)
	}
	if checkpoint.Worktree.Branch != f.branch {
		t.Fatalf("checkpoint.Worktree.Branch = %q", checkpoint.Worktree.Branch)
	}
	if checkpoint.Worktree.PreparedAt == "" {
		t.Fatal("checkpoint.Worktree.PreparedAt empty")
	}
	// Adopt must reuse the matched token (no rewrite) so a crash before
	// checkpoint persistence cannot desync disk marker vs latest checkpoint.
	if checkpoint.Worktree.OwnerToken != priorToken {
		t.Fatalf("checkpoint.Worktree.OwnerToken = %q, want stable prior token %q", checkpoint.Worktree.OwnerToken, priorToken)
	}
	got, err := worktreesafety.ReadFixerOwnerToken(f.wtPath)
	if err != nil {
		t.Fatalf("ReadFixerOwnerToken() error = %v", err)
	}
	if got != priorToken {
		t.Fatalf("disk owner token = %q, want stable prior token %q", got, priorToken)
	}
	if len(git.inspectCalls) != 1 {
		t.Fatalf("len(git.inspectCalls) = %d, want 1", len(git.inspectCalls))
	}
	if len(git.cleanupCalls) != 0 {
		t.Fatalf("len(git.cleanupCalls) = %d, want 0 (adopt before cleanup)", len(git.cleanupCalls))
	}
	if len(git.createCalls) != 0 {
		t.Fatalf("len(git.createCalls) = %d, want 0 (reuse existing path)", len(git.createCalls))
	}
	if len(git.prepareCalls) != 1 {
		t.Fatalf("len(git.prepareCalls) = %d, want 1 (existing path only)", len(git.prepareCalls))
	}
}

func TestRunPrepareWorktreeStepRejectsDirtyWithoutFixerProvenance(t *testing.T) {
	t.Parallel()

	// Shared project/PR detached path may hold reviewer dirt; without a prior
	// fixer ownership token, same-head must stay manual_intervention.
	wtPath := filepath.Join(t.TempDir(), "wt-42")
	git := &fakeGitGateway{
		createResult:   CreateWorktreeResult{WorktreePath: wtPath, Branch: "feature/fix-42", HeadSHA: "base-head"},
		prepareResult:  PrepareWorktreeResult{HeadSHA: "base-head", Clean: false},
		inspectResults: []InspectHeadResult{{HeadSHA: "base-head", HasUncommittedChanges: true}},
	}
	runner := New(Options{Git: git})
	checkpoint, err := runner.runPrepareWorktreeStep(context.Background(), stepInput{
		Project:    storage.ProjectRecord{ID: "project_1", RepoPath: t.TempDir()},
		Loop:       storage.LoopRecord{ID: "loop_1", Status: "running"},
		Repo:       "acme/looper",
		PRNumber:   42,
		Checkpoint: fixerCheckpoint{Detail: &checkpointDetail{HeadSHA: "base-head", HeadRefName: "feature/fix-42", BaseRefName: "main"}},
	})
	assertPrepareDirtyManualIntervention(t, checkpoint, err)
	if len(git.inspectCalls) != 0 {
		t.Fatalf("len(git.inspectCalls) = %d, want 0 (no provenance → no adopt inspect)", len(git.inspectCalls))
	}
	if len(git.cleanupCalls) != 0 {
		t.Fatalf("len(git.cleanupCalls) = %d, want 0", len(git.cleanupCalls))
	}
}

func TestRunPrepareWorktreeStepRejectsDirtyWhenOwnerTokenClearedByOtherRunner(t *testing.T) {
	t.Parallel()

	// Fixer rewind still has path + OwnerToken in checkpoint, but another runner
	// Create/Restore cleared the on-disk marker (shared detached directory).
	f := newDirtyAdoptFixture(t)
	cp := f.rewindCheckpoint()
	// Intentionally do not seed disk ownership — simulates ClearFixerOwnerToken.
	git := &fakeGitGateway{
		createResult:   CreateWorktreeResult{WorktreePath: f.wtPath, Branch: f.branch, HeadSHA: f.headSHA},
		prepareResult:  PrepareWorktreeResult{HeadSHA: f.headSHA, Clean: false},
		inspectResults: []InspectHeadResult{{HeadSHA: f.headSHA, HasUncommittedChanges: true}},
	}
	runner := New(Options{Git: git})
	checkpoint, err := runner.runPrepareWorktreeStep(context.Background(), stepInput{
		Project:    f.project(),
		Loop:       storage.LoopRecord{ID: "loop_1", Status: "running"},
		Repo:       "acme/looper",
		PRNumber:   42,
		Checkpoint: cp,
	})
	assertPrepareDirtyManualIntervention(t, checkpoint, err)
	if len(git.inspectCalls) != 0 {
		t.Fatalf("len(git.inspectCalls) = %d, want 0 (stale ownership → no adopt)", len(git.inspectCalls))
	}
	if len(git.cleanupCalls) != 0 {
		t.Fatalf("len(git.cleanupCalls) = %d, want 0", len(git.cleanupCalls))
	}
}

func TestRunPrepareWorktreeStepRejectsDirtyWhenPathMatchesButOwnerTokenMissing(t *testing.T) {
	t.Parallel()

	// Path equality without OwnerToken must not authorize adopt (legacy / path-only provenance).
	f := newDirtyAdoptFixture(t)
	git := &fakeGitGateway{
		createResult:   CreateWorktreeResult{WorktreePath: f.wtPath, Branch: f.branch, HeadSHA: f.headSHA},
		prepareResult:  PrepareWorktreeResult{HeadSHA: f.headSHA, Clean: false},
		inspectResults: []InspectHeadResult{{HeadSHA: f.headSHA, HasUncommittedChanges: true}},
	}
	runner := New(Options{Git: git})
	checkpoint, err := runner.runPrepareWorktreeStep(context.Background(), stepInput{
		Project:  f.project(),
		Loop:     storage.LoopRecord{ID: "loop_1", Status: "running"},
		Repo:     "acme/looper",
		PRNumber: 42,
		Checkpoint: fixerCheckpoint{
			Detail:   &checkpointDetail{HeadSHA: f.headSHA, HeadRefName: f.branch, BaseRefName: "main"},
			Worktree: &checkpointWorktree{Path: f.wtPath, Branch: f.branch}, // path only
		},
	})
	assertPrepareDirtyManualIntervention(t, checkpoint, err)
	if len(git.inspectCalls) != 0 {
		t.Fatalf("len(git.inspectCalls) = %d, want 0", len(git.inspectCalls))
	}
}

// Clean prepare must not mark PreparedAt with an empty OwnerToken when the
// ownership stamp cannot be written — that desync makes later dirty adopt
// permanently fail hasFixerWorktreeProvenance and forces MI after interrupt.
func TestRunPrepareWorktreeStepFailsWhenOwnerStampUnwritable(t *testing.T) {
	t.Parallel()

	f := newDirtyAdoptFixture(t)
	gitDir := filepath.Join(f.wtPath, ".git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatalf("MkdirAll .git: %v", err)
	}
	// Make the private git dir unwritable so WriteFixerOwnerToken fails.
	if err := os.Chmod(gitDir, 0o555); err != nil {
		t.Fatalf("Chmod .git: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(gitDir, 0o755) })

	git := &fakeGitGateway{
		createResult:  CreateWorktreeResult{WorktreePath: f.wtPath, Branch: f.branch, HeadSHA: f.headSHA},
		prepareResult: PrepareWorktreeResult{HeadSHA: f.headSHA, Clean: true},
	}
	runner := New(Options{Git: git})
	checkpoint, err := runner.runPrepareWorktreeStep(context.Background(), stepInput{
		Project:  f.project(),
		Loop:     storage.LoopRecord{ID: "loop_1", Status: "running"},
		Run:      storage.RunRecord{ID: "run_stamp"},
		Repo:     "acme/looper",
		PRNumber: 42,
		Checkpoint: fixerCheckpoint{
			Detail: &checkpointDetail{HeadSHA: f.headSHA, HeadRefName: f.branch, BaseRefName: "main"},
		},
	})
	if err == nil {
		t.Fatal("runPrepareWorktreeStep() error = nil, want stamp fixer ownership failure")
	}
	if !strings.Contains(err.Error(), "stamp fixer ownership") {
		t.Fatalf("error = %v, want stamp fixer ownership", err)
	}
	// Must not claim prepared ownership with an empty token.
	if checkpoint.Worktree != nil && checkpoint.Worktree.PreparedAt != "" {
		t.Fatalf("checkpoint.Worktree = %#v, want unprepared (no PreparedAt after stamp failure)", checkpoint.Worktree)
	}
	if checkpoint.Worktree != nil && checkpoint.Worktree.OwnerToken != "" {
		t.Fatalf("OwnerToken = %q, want empty after stamp failure", checkpoint.Worktree.OwnerToken)
	}
}
