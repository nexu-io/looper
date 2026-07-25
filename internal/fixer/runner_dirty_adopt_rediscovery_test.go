package fixer

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/nexu-io/looper/internal/storage"
)

func TestPreservedWorktreeOwnershipForRediscoveryCarriesUnpreparedOwnerToken(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "wt-42")
	token := "fixer:loop_1:run_1:t"
	got := preservedWorktreeOwnershipForRediscovery(fixerCheckpoint{
		Worktree: &checkpointWorktree{Path: path, Branch: "feature/fix-42", OwnerToken: token},
	}, stepPrepareWorktree)
	if got == nil || got.Path != path || got.Branch != "feature/fix-42" || got.OwnerToken != token || got.PreparedAt != "" {
		t.Fatalf("preserved = %#v, want unprepared path+branch+token", got)
	}

	if preservedWorktreeOwnershipForRediscovery(fixerCheckpoint{
		Worktree: &checkpointWorktree{Path: path, Branch: "feature/fix-42", OwnerToken: token, PreparedAt: "ready"},
	}, stepPrepareWorktree) != nil {
		t.Fatal("prepared worktree must not be carried for rediscovery ownership")
	}
	if preservedWorktreeOwnershipForRediscovery(fixerCheckpoint{
		Worktree: &checkpointWorktree{Path: path, Branch: "feature/fix-42", OwnerToken: token},
	}, stepRepair) != nil {
		t.Fatal("non-prepare failures must not carry rediscovery ownership")
	}
	if preservedWorktreeOwnershipForRediscovery(fixerCheckpoint{
		Worktree: &checkpointWorktree{Path: path, Branch: "feature/fix-42"},
	}, stepPrepareWorktree) != nil {
		t.Fatal("markerless path must not be carried as ownership")
	}
}

func TestCreateRunContextPreservesOwnerTokenAcrossPrepareProbeRediscovery(t *testing.T) {
	t.Parallel()

	// Multi-attempt contract: failed prepare-worktree restarts discover, but the
	// unprepared ownership slice must survive so the next prepare can same-head adopt.
	fixture := newRunnerFixture(t)
	f := newDirtyAdoptFixture(t)
	repo := "acme/looper"
	prNumber := int64(42)
	loopTarget := "pr:acme/looper:42"
	nowISO := fixture.nowISO()
	ownerToken := "fixer:loop_prepare_rediscover:run_1:" + nowISO
	if err := fixture.repos.Loops.Upsert(context.Background(), storage.LoopRecord{
		ID:         "loop_fixer_prepare_rediscover_owner",
		Seq:        1,
		ProjectID:  "project_1",
		Type:       "fixer",
		TargetType: "pull_request",
		TargetID:   &loopTarget,
		Repo:       &repo,
		PRNumber:   &prNumber,
		Status:     "queued",
		CreatedAt:  nowISO,
		UpdatedAt:  nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	checkpointJSON := mustMarshalJSON(fixerCheckpoint{
		Detail:   &checkpointDetail{HeadSHA: f.headSHA, HeadRefName: f.branch, BaseRefName: "main", State: "OPEN"},
		FixItems: []FixItem{{Type: "comment", ID: "c1", ThreadID: "t1", Summary: "please fix"}},
		// Prepare probe failed after rewind: path + ownership kept, PreparedAt empty.
		Worktree: &checkpointWorktree{Path: f.wtPath, Branch: f.branch, OwnerToken: ownerToken},
	})
	if err := fixture.repos.Runs.Upsert(context.Background(), storage.RunRecord{
		ID:             "run_failed_prepare_probe",
		LoopID:         "loop_fixer_prepare_rediscover_owner",
		Status:         "failed",
		CurrentStep:    stringPtr(string(stepPrepareWorktree)),
		CheckpointJSON: &checkpointJSON,
		Summary:        stringPtr("error: cannot run ssh: No such file or directory"),
		ErrorMessage:   stringPtr("error: cannot run ssh: No such file or directory"),
		StartedAt:      nowISO,
		CreatedAt:      nowISO,
		UpdatedAt:      nowISO,
	}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}

	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, Logger: fixture.logger, Now: fixture.now})
	loop, err := fixture.repos.Loops.GetByID(context.Background(), "loop_fixer_prepare_rediscover_owner")
	if err != nil || loop == nil {
		t.Fatalf("Loops.GetByID() = (%#v, %v)", loop, err)
	}
	created, err := runner.createRunContext(context.Background(), *loop)
	if err != nil {
		t.Fatalf("createRunContext() error = %v", err)
	}
	if created.StartStep != stepDiscoverPR {
		t.Fatalf("StartStep = %q, want discover-pr", created.StartStep)
	}
	if created.Resumed {
		t.Fatal("Resumed = true, want false for discover restart")
	}
	if created.Checkpoint.Worktree == nil {
		t.Fatal("Worktree = nil, want ownership preserved across prepare rediscovery")
	}
	if created.Checkpoint.Worktree.Path != f.wtPath || created.Checkpoint.Worktree.OwnerToken != ownerToken {
		t.Fatalf("Worktree = %#v, want path %q token %q", created.Checkpoint.Worktree, f.wtPath, ownerToken)
	}
	if created.Checkpoint.Worktree.PreparedAt != "" {
		t.Fatalf("PreparedAt = %q, want empty", created.Checkpoint.Worktree.PreparedAt)
	}
	// Second prepare attempt with carried ownership must still same-head adopt.
	f.seedOwnerToken(t, ownerToken)
	git := &fakeGitGateway{
		createResult:   CreateWorktreeResult{WorktreePath: f.wtPath, Branch: f.branch, HeadSHA: f.headSHA},
		prepareResult:  PrepareWorktreeResult{HeadSHA: f.headSHA, Clean: false},
		inspectResults: []InspectHeadResult{{HeadSHA: f.headSHA, HasUncommittedChanges: true}},
	}
	runner = New(Options{Git: git})
	retryCheckpoint := created.Checkpoint
	retryCheckpoint.Detail = &checkpointDetail{HeadSHA: f.headSHA, HeadRefName: f.branch, BaseRefName: "main", State: "OPEN"}
	prepared, err := runner.runPrepareWorktreeStep(context.Background(), stepInput{
		Project:    f.project(),
		Loop:       storage.LoopRecord{ID: loop.ID, Status: "running"},
		Run:        storage.RunRecord{ID: "run_retry"},
		Repo:       repo,
		PRNumber:   prNumber,
		Checkpoint: retryCheckpoint,
	})
	if err != nil {
		t.Fatalf("runPrepareWorktreeStep() after rediscovery error = %v", err)
	}
	if prepared.Worktree == nil || prepared.Worktree.PreparedAt == "" || prepared.Worktree.OwnerToken == "" {
		t.Fatalf("prepared = %#v, want adopted worktree with new ownership stamp", prepared.Worktree)
	}
	if len(git.createCalls) != 0 {
		t.Fatalf("len(git.createCalls) = %d, want 0 (reuse preserved path)", len(git.createCalls))
	}
}
