package fixer

import (
	"context"
	"testing"

	"github.com/nexu-io/looper/internal/domain"
	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
)

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
