package fixer

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/nexu-io/looper/internal/domain"
	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
)

func TestRunPrepareWorktreeStepAdoptsSameHeadDirtyWorktreeWithProvenance(t *testing.T) {
	t.Parallel()

	// Interrupted-repair lifecycle: rewind cleared PreparedAt but kept Path + OwnerToken.
	// Same-head adopt must run before CleanupWorktree so partial agent dirt survives.
	f := newDirtyAdoptFixture(t)
	git := &fakeGitGateway{
		createResult:   CreateWorktreeResult{WorktreePath: f.wtPath, Branch: f.branch, HeadSHA: f.headSHA},
		prepareResult:  PrepareWorktreeResult{HeadSHA: f.headSHA, Clean: false},
		inspectResults: []InspectHeadResult{{HeadSHA: f.headSHA, HasUncommittedChanges: true}},
	}
	runner := New(Options{Git: git})
	checkpoint, err := runner.runPrepareWorktreeStep(context.Background(), f.stepWithOwnership(t, storage.LoopRecord{ID: "loop_1", Status: "running"}, git))
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
	if checkpoint.Worktree.OwnerToken == "" {
		t.Fatal("checkpoint.Worktree.OwnerToken empty after adopt")
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

func TestRunPrepareWorktreeStepDirtyAdoptEarlyExitGatesSkipInspectHead(t *testing.T) {
	t.Parallel()

	takeoverMeta, err := loops.WriteTakeoverResume(nil, loops.TakeoverResume{SessionID: "sess-1"})
	if err != nil {
		t.Fatalf("WriteTakeoverResume() error = %v", err)
	}

	// Early-exit gates that must not call InspectHead, plus local mismatch which does.
	// All cases carry fixer provenance so the gates under test are reached.
	cases := []struct {
		name        string
		loop        storage.LoopRecord
		detailHead  string
		prepareHead string
		inspectHead string // only used when inspectCalls want > 0
		wantInspect int
	}{
		{
			name:        "human_takeover",
			loop:        storage.LoopRecord{ID: "loop_1", Status: string(domain.LoopStatusHumanTakeover)},
			detailHead:  "base-head",
			prepareHead: "base-head",
			wantInspect: 0,
		},
		{
			name:        "awaiting_human",
			loop:        storage.LoopRecord{ID: "loop_1", Status: string(domain.LoopStatusAwaitingHuman)},
			detailHead:  "base-head",
			prepareHead: "base-head",
			wantInspect: 0,
		},
		{
			name:        "takeoverResume",
			loop:        storage.LoopRecord{ID: "loop_1", Status: "running", MetadataJSON: &takeoverMeta},
			detailHead:  "base-head",
			prepareHead: "base-head",
			wantInspect: 0,
		},
		{
			name:        "empty_expected_head",
			loop:        storage.LoopRecord{ID: "loop_1", Status: "running"},
			detailHead:  "",
			prepareHead: "base-head",
			wantInspect: 0,
		},
		{
			name:        "remote_mismatch",
			loop:        storage.LoopRecord{ID: "loop_1", Status: "running"},
			detailHead:  "base-head",
			prepareHead: "remote-head",
			wantInspect: 0,
		},
		{
			name:        "local_mismatch",
			loop:        storage.LoopRecord{ID: "loop_1", Status: "running"},
			detailHead:  "base-head",
			prepareHead: "base-head",
			inspectHead: "other-head",
			wantInspect: 1,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newDirtyAdoptFixture(t)
			inspectResults := []InspectHeadResult{{HeadSHA: "base-head", HasUncommittedChanges: true}}
			if tc.inspectHead != "" {
				inspectResults = []InspectHeadResult{{HeadSHA: tc.inspectHead, HasUncommittedChanges: true}}
			}
			git := &fakeGitGateway{
				createResult:   CreateWorktreeResult{WorktreePath: f.wtPath, Branch: f.branch, HeadSHA: f.headSHA},
				prepareResult:  PrepareWorktreeResult{HeadSHA: tc.prepareHead, Clean: false},
				inspectResults: inspectResults,
			}
			runner := New(Options{Git: git})
			detail := &checkpointDetail{HeadRefName: f.branch, BaseRefName: "main"}
			if tc.detailHead != "" {
				detail.HeadSHA = tc.detailHead
			}
			ownerToken := "fixer:loop_1:run_prior:prepared"
			f.seedOwnerToken(t, ownerToken)
			checkpoint, err := runner.runPrepareWorktreeStep(context.Background(), stepInput{
				Project:  f.project(),
				Loop:     tc.loop,
				Repo:     "acme/looper",
				PRNumber: 42,
				Checkpoint: fixerCheckpoint{
					Detail: detail,
					Worktree: &checkpointWorktree{
						Path:       f.wtPath,
						Branch:     f.branch,
						OwnerToken: ownerToken,
					},
				},
			})
			assertPrepareDirtyManualIntervention(t, checkpoint, err)
			if len(git.inspectCalls) != tc.wantInspect {
				t.Fatalf("len(git.inspectCalls) = %d, want %d", len(git.inspectCalls), tc.wantInspect)
			}
			// Dirty non-adopt on rewind must not CleanupWorktree (preserve evidence).
			if len(git.cleanupCalls) != 0 {
				t.Fatalf("len(git.cleanupCalls) = %d, want 0 after rejected adopt", len(git.cleanupCalls))
			}
			if len(git.createCalls) != 0 {
				t.Fatalf("len(git.createCalls) = %d, want 0 (no recreate after dirty MI)", len(git.createCalls))
			}
		})
	}
}
