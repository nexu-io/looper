package fixer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/domain"
	gitinfra "github.com/nexu-io/looper/internal/infra/git"
	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
)

// Shared fixtures for same-head dirty adopt / prepare-error recovery.
// Kept out of runner_test.go so this recovery state machine does not expand
// that single subsystem test file past the AGENTS.md growth threshold.

type dirtyAdoptFixture struct {
	repoPath     string
	worktreeRoot string
	wtPath       string
	dirtyFile    string
	metadata     string
	branch       string
	headSHA      string
}

func newDirtyAdoptFixture(t *testing.T) dirtyAdoptFixture {
	t.Helper()
	repoPath := t.TempDir()
	worktreeRoot := filepath.Join(t.TempDir(), "worktrees")
	wtPath := filepath.Join(worktreeRoot, "wt-42")
	if err := os.MkdirAll(wtPath, 0o755); err != nil {
		t.Fatalf("MkdirAll worktree: %v", err)
	}
	dirtyFile := filepath.Join(wtPath, "partial-agent-edit.txt")
	if err := os.WriteFile(dirtyFile, []byte("keep me\n"), 0o644); err != nil {
		t.Fatalf("WriteFile dirty marker: %v", err)
	}
	return dirtyAdoptFixture{
		repoPath:     repoPath,
		worktreeRoot: worktreeRoot,
		wtPath:       wtPath,
		dirtyFile:    dirtyFile,
		metadata:     fmt.Sprintf(`{"worktreeRoot":%q}`, worktreeRoot),
		branch:       "feature/fix-42",
		headSHA:      "base-head",
	}
}

func (f dirtyAdoptFixture) project() storage.ProjectRecord {
	return storage.ProjectRecord{ID: "project_1", RepoPath: f.repoPath, MetadataJSON: &f.metadata}
}

func (f dirtyAdoptFixture) rewindCheckpoint() fixerCheckpoint {
	return fixerCheckpoint{
		Detail:   &checkpointDetail{HeadSHA: f.headSHA, HeadRefName: f.branch, BaseRefName: "main"},
		Worktree: &checkpointWorktree{Path: f.wtPath, Branch: f.branch}, // PreparedAt cleared by rewind
	}
}

func (f dirtyAdoptFixture) step(loop storage.LoopRecord, git GitGateway) stepInput {
	return stepInput{
		Project:    f.project(),
		Loop:       loop,
		Repo:       "acme/looper",
		PRNumber:   42,
		Checkpoint: f.rewindCheckpoint(),
	}
}

func (f dirtyAdoptFixture) assertDirtyPreserved(t *testing.T) {
	t.Helper()
	got, err := os.ReadFile(f.dirtyFile)
	if err != nil || string(got) != "keep me\n" {
		t.Fatalf("dirty marker = %q err=%v, want preserved", got, err)
	}
}

func assertPrepareDirtyManualIntervention(t *testing.T, checkpoint fixerCheckpoint, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("runPrepareWorktreeStep() error = nil, want manual intervention")
	}
	var loopErr *loopError
	if !errors.As(err, &loopErr) {
		t.Fatalf("error = %T, want *loopError", err)
	}
	if loopErr.kind != FailureManualIntervention {
		t.Fatalf("loopErr.kind = %v, want %v", loopErr.kind, FailureManualIntervention)
	}
	if checkpoint.ResumePolicy != "manual_intervention" {
		t.Fatalf("checkpoint.ResumePolicy = %q, want manual_intervention", checkpoint.ResumePolicy)
	}
}

func TestIsMissingOrUnusableFixerWorktree(t *testing.T) {
	t.Parallel()

	existing := t.TempDir()
	missing := filepath.Join(t.TempDir(), "gone")

	cases := []struct {
		name    string
		path    string
		prepErr error
		want    bool
	}{
		{name: "empty_path", path: "", want: true},
		{name: "missing_path", path: missing, want: true},
		{name: "existing_no_err", path: existing, want: false},
		{name: "not_a_working_tree", path: existing, prepErr: fmt.Errorf("fatal: %s is not a working tree", existing), want: true},
		{name: "not_a_git_repository", path: existing, prepErr: errors.New("fatal: not a git repository (or any of the parent directories): .git"), want: true},
		// Regression: external ssh/fetch text must not classify a live checkout as unusable.
		{name: "ssh_no_such_file", path: existing, prepErr: errors.New("error: cannot run ssh: No such file or directory\nfatal: unable to fork"), want: false},
		{name: "fetch_transport", path: existing, prepErr: errors.New("git fetch origin feature/fix-42: fatal: unable to access remote"), want: false},
		{name: "remote_head_changed", path: existing, prepErr: errors.New("remote head for feature/fix-42 changed: expected a, got b"), want: false},
		// Generic existence phrases without a missing checkout must not force cleanup.
		{name: "does_not_exist_remote_ref", path: existing, prepErr: errors.New("fatal: couldn't find remote ref does not exist"), want: false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isMissingOrUnusableFixerWorktree(tc.path, tc.prepErr); got != tc.want {
				t.Fatalf("isMissingOrUnusableFixerWorktree(%q, %v) = %v, want %v", tc.path, tc.prepErr, got, tc.want)
			}
		})
	}
}

func TestRunPrepareWorktreeStepAdoptsSameHeadDirtyWorktreeWithProvenance(t *testing.T) {
	t.Parallel()

	// Interrupted-repair lifecycle: rewind cleared PreparedAt but kept Path.
	// Same-head adopt must run before CleanupWorktree so partial agent dirt survives.
	f := newDirtyAdoptFixture(t)
	git := &fakeGitGateway{
		createResult:   CreateWorktreeResult{WorktreePath: f.wtPath, Branch: f.branch, HeadSHA: f.headSHA},
		prepareResult:  PrepareWorktreeResult{HeadSHA: f.headSHA, Clean: false},
		inspectResults: []InspectHeadResult{{HeadSHA: f.headSHA, HasUncommittedChanges: true}},
	}
	runner := New(Options{Git: git})
	checkpoint, err := runner.runPrepareWorktreeStep(context.Background(), f.step(storage.LoopRecord{ID: "loop_1", Status: "running"}, git))
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
	// fixer checkpoint path, same-head must stay manual_intervention.
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
			checkpoint, err := runner.runPrepareWorktreeStep(context.Background(), stepInput{
				Project:  f.project(),
				Loop:     tc.loop,
				Repo:     "acme/looper",
				PRNumber: 42,
				Checkpoint: fixerCheckpoint{
					Detail:   detail,
					Worktree: &checkpointWorktree{Path: f.wtPath, Branch: f.branch},
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

// Prepare probe failures on rewind (fetch/transport/remote-head/ssh) never read
// worktree status, so CleanupWorktree must not run — that would destroy
// interrupted-repair dirt that the dirty-adopt path never got to evaluate.
func TestRunPrepareWorktreeStepPrepareErrorPreservesExistingWorktree(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
	}{
		{name: "fetch_transport", err: fmt.Errorf("git fetch origin feature/fix-42: fatal: unable to access remote")},
		{name: "remote_head_changed", err: fmt.Errorf("remote head for feature/fix-42 changed: expected base-head, got advanced-head")},
		// External dependency wording that previously matched the broad classifier.
		{name: "ssh_no_such_file", err: fmt.Errorf("error: cannot run ssh: No such file or directory\nfatal: unable to fork")},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newDirtyAdoptFixture(t)
			git := &fakeGitGateway{
				prepareErr:     tc.err,
				createResult:   CreateWorktreeResult{WorktreePath: f.wtPath, Branch: f.branch, HeadSHA: f.headSHA},
				prepareResult:  PrepareWorktreeResult{HeadSHA: f.headSHA, Clean: false},
				inspectResults: []InspectHeadResult{{HeadSHA: f.headSHA, HasUncommittedChanges: true}},
			}
			runner := New(Options{Git: git})
			checkpoint, err := runner.runPrepareWorktreeStep(context.Background(), f.step(storage.LoopRecord{ID: "loop_1", Status: "running"}, git))
			if err == nil {
				t.Fatal("runPrepareWorktreeStep() error = nil, want prepare error")
			}
			if !errors.Is(err, tc.err) && err.Error() != tc.err.Error() {
				t.Fatalf("error = %v, want %v", err, tc.err)
			}
			if checkpoint.Worktree == nil || checkpoint.Worktree.Path != f.wtPath {
				t.Fatalf("checkpoint.Worktree = %#v, want path preserved at %q", checkpoint.Worktree, f.wtPath)
			}
			if len(git.cleanupCalls) != 0 {
				t.Fatalf("len(git.cleanupCalls) = %d, want 0 (prepare error must not force-remove)", len(git.cleanupCalls))
			}
			if len(git.createCalls) != 0 {
				t.Fatalf("len(git.createCalls) = %d, want 0 (no recreate after prepare error)", len(git.createCalls))
			}
			if len(git.prepareCalls) != 1 {
				t.Fatalf("len(git.prepareCalls) = %d, want 1", len(git.prepareCalls))
			}
			if len(git.inspectCalls) != 0 {
				t.Fatalf("len(git.inspectCalls) = %d, want 0 (never reached adopt path)", len(git.inspectCalls))
			}
			f.assertDirtyPreserved(t)
		})
	}
}

// Missing checkpoint worktree directories must fall through to CreateWorktree instead of
// returning a prepare/cwd error that burns a retry (and can become terminal).
func TestRunPrepareWorktreeStepRecreatesMissingCheckpointWorktree(t *testing.T) {
	t.Parallel()

	repoPath := t.TempDir()
	worktreeRoot := filepath.Join(t.TempDir(), "worktrees")
	missingPath := filepath.Join(worktreeRoot, "wt-missing")
	recreatedPath := filepath.Join(worktreeRoot, "wt-recreated")
	// Intentionally do not create missingPath.
	metadata := fmt.Sprintf(`{"worktreeRoot":%q}`, worktreeRoot)
	git := &fakeGitGateway{
		createResult:  CreateWorktreeResult{WorktreePath: recreatedPath, Branch: "feature/fix-42", HeadSHA: "base-head"},
		prepareResult: PrepareWorktreeResult{HeadSHA: "base-head", Clean: true},
	}
	runner := New(Options{Git: git})
	checkpoint, err := runner.runPrepareWorktreeStep(context.Background(), stepInput{
		Project:  storage.ProjectRecord{ID: "project_1", RepoPath: repoPath, MetadataJSON: &metadata},
		Loop:     storage.LoopRecord{ID: "loop_1", Status: "running"},
		Repo:     "acme/looper",
		PRNumber: 42,
		Checkpoint: fixerCheckpoint{
			Detail:   &checkpointDetail{HeadSHA: "base-head", HeadRefName: "feature/fix-42", BaseRefName: "main"},
			Worktree: &checkpointWorktree{Path: missingPath, Branch: "feature/fix-42"}, // PreparedAt cleared; path gone
		},
	})
	if err != nil {
		t.Fatalf("runPrepareWorktreeStep() error = %v", err)
	}
	if checkpoint.Worktree == nil || checkpoint.Worktree.Path != recreatedPath {
		t.Fatalf("checkpoint.Worktree = %#v, want recreated path %q", checkpoint.Worktree, recreatedPath)
	}
	if checkpoint.Worktree.PreparedAt == "" {
		t.Fatal("checkpoint.Worktree.PreparedAt empty after recreate")
	}
	if len(git.cleanupCalls) != 1 {
		t.Fatalf("len(git.cleanupCalls) = %d, want 1 (stale missing path cleanup)", len(git.cleanupCalls))
	}
	if len(git.createCalls) != 1 {
		t.Fatalf("len(git.createCalls) = %d, want 1", len(git.createCalls))
	}
	// Prepare is only invoked on the recreated path, not the missing checkpoint path.
	if len(git.prepareCalls) != 1 || git.prepareCalls[0].WorktreePath != recreatedPath {
		t.Fatalf("prepareCalls = %#v, want one call on recreated path", git.prepareCalls)
	}
}

// Empty / unregistered directories that still pass path-safety checks must recreate
// when prepare reports "not a working tree", rather than preserving a dead path.
func TestRunPrepareWorktreeStepRecreatesUnusableCheckpointWorktree(t *testing.T) {
	t.Parallel()

	repoPath := t.TempDir()
	worktreeRoot := filepath.Join(t.TempDir(), "worktrees")
	stalePath := filepath.Join(worktreeRoot, "wt-stale")
	if err := os.MkdirAll(stalePath, 0o755); err != nil {
		t.Fatalf("MkdirAll stale worktree: %v", err)
	}
	recreatedPath := filepath.Join(worktreeRoot, "wt-recreated")
	metadata := fmt.Sprintf(`{"worktreeRoot":%q}`, worktreeRoot)
	git := &fakeGitGateway{
		// First prepare (stale path) fails; second prepare (recreated) succeeds.
		prepareErrors: []error{fmt.Errorf("fatal: %s is not a working tree", stalePath), nil},
		createResult:  CreateWorktreeResult{WorktreePath: recreatedPath, Branch: "feature/fix-42", HeadSHA: "base-head"},
		prepareResult: PrepareWorktreeResult{HeadSHA: "base-head", Clean: true},
	}
	runner := New(Options{Git: git})
	checkpoint, err := runner.runPrepareWorktreeStep(context.Background(), stepInput{
		Project:  storage.ProjectRecord{ID: "project_1", RepoPath: repoPath, MetadataJSON: &metadata},
		Loop:     storage.LoopRecord{ID: "loop_1", Status: "running"},
		Repo:     "acme/looper",
		PRNumber: 42,
		Checkpoint: fixerCheckpoint{
			Detail:   &checkpointDetail{HeadSHA: "base-head", HeadRefName: "feature/fix-42", BaseRefName: "main"},
			Worktree: &checkpointWorktree{Path: stalePath, Branch: "feature/fix-42"},
		},
	})
	if err != nil {
		t.Fatalf("runPrepareWorktreeStep() error = %v", err)
	}
	if checkpoint.Worktree == nil || checkpoint.Worktree.Path != recreatedPath {
		t.Fatalf("checkpoint.Worktree = %#v, want recreated path %q", checkpoint.Worktree, recreatedPath)
	}
	if len(git.cleanupCalls) != 1 {
		t.Fatalf("len(git.cleanupCalls) = %d, want 1", len(git.cleanupCalls))
	}
	if len(git.createCalls) != 1 {
		t.Fatalf("len(git.createCalls) = %d, want 1", len(git.createCalls))
	}
	// First prepare on stale path fails; second prepare is on the recreated path.
	if len(git.prepareCalls) != 2 {
		t.Fatalf("len(git.prepareCalls) = %d, want 2 (stale then recreated)", len(git.prepareCalls))
	}
	if git.prepareCalls[0].WorktreePath != stalePath || git.prepareCalls[1].WorktreePath != recreatedPath {
		t.Fatalf("prepareCalls paths = %q, %q, want stale then recreated", git.prepareCalls[0].WorktreePath, git.prepareCalls[1].WorktreePath)
	}
}

// Full queue lifecycle: interrupted-repair rewind → prepare probe fails on final
// attempt → terminal parking must not force-remove the dirty worktree.
func TestProcessClaimedItemTerminalPrepareErrorPreservesDirtyWorktree(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	f := newDirtyAdoptFixture(t)
	project, err := fixture.repos.Projects.GetByID(context.Background(), "project_1")
	if err != nil || project == nil {
		t.Fatalf("Projects.GetByID() = (%#v, %v)", project, err)
	}
	project.MetadataJSON = &f.metadata
	project.RepoPath = f.repoPath
	if err := fixture.repos.Projects.Upsert(context.Background(), *project); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	repo := "acme/looper"
	prNumber := int64(42)
	loopTarget := "pr:acme/looper:42"
	nowISO := fixture.nowISO()
	if err := fixture.repos.Loops.Upsert(context.Background(), storage.LoopRecord{
		ID:         "loop_fixer_prepare_terminal_preserve",
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

	// Prior repair failure with a prepared worktree so resume rewinds to prepare-worktree.
	checkpointJSON := mustMarshalJSON(fixerCheckpoint{
		ClaimedLockKey: "pr:acme/looper:42",
		Detail:         &checkpointDetail{HeadSHA: f.headSHA, HeadRefName: f.branch, BaseRefName: "main", State: "OPEN"},
		FixItems:       []FixItem{{Type: "comment", ID: "c1", ThreadID: "t1", Summary: "please fix"}},
		Worktree: &checkpointWorktree{
			Path:        f.wtPath,
			Branch:      f.branch,
			HeadSHA:     f.headSHA,
			BaseHeadSHA: f.headSHA,
			PreparedAt:  nowISO,
		},
		Repair: &checkpointRepair{Summary: "interrupted", ParseStatus: "parsed", CompletedAt: nowISO},
	})
	if err := fixture.repos.Runs.Upsert(context.Background(), storage.RunRecord{
		ID:                "run_failed_before_prepare_retry",
		LoopID:            "loop_fixer_prepare_terminal_preserve",
		Status:            "failed",
		CurrentStep:       stringPtr(string(stepRepair)),
		LastCompletedStep: stringPtr(string(stepPrepareWorktree)),
		CheckpointJSON:    &checkpointJSON,
		StartedAt:         nowISO,
		CreatedAt:         nowISO,
		UpdatedAt:         nowISO,
	}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}

	projectID := "project_1"
	loopID := "loop_fixer_prepare_terminal_preserve"
	lockKey := "pr:acme/looper:42"
	if err := fixture.repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{
		ID:          "queue_fixer_prepare_terminal_preserve",
		ProjectID:   &projectID,
		LoopID:      &loopID,
		Type:        "fixer",
		TargetType:  "pull_request",
		TargetID:    loopTarget,
		Repo:        &repo,
		PRNumber:    &prNumber,
		DedupeKey:   "fixer:acme/looper:42:prepare-terminal-preserve",
		Priority:    1,
		Status:      "queued",
		AvailableAt: nowISO,
		MaxAttempts: 1, // final allowed attempt → terminal parking
		LockKey:     &lockKey,
		CreatedAt:   nowISO,
		UpdatedAt:   nowISO,
	}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	github := &fakeGitHubGateway{
		currentUser: "looper",
		viewResponses: []PullRequestDetail{{
			Number: 42, State: "OPEN", HeadSHA: f.headSHA, HeadRefName: f.branch,
			BaseRefName: "main", BaseSHA: "base-1", Author: "looper",
			Comments: []map[string]any{{"id": "c1", "threadId": "t1", "body": "please fix"}},
		}},
	}
	git := &fakeGitGateway{
		// Use the external ssh-style wording that previously triggered force cleanup.
		prepareErr:    fmt.Errorf("error: cannot run ssh: No such file or directory\nfatal: unable to fork"),
		createResult:  CreateWorktreeResult{WorktreePath: f.wtPath, Branch: f.branch, HeadSHA: f.headSHA},
		prepareResult: PrepareWorktreeResult{HeadSHA: f.headSHA, Clean: false},
	}
	runner := New(Options{
		DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git,
		Logger: fixture.logger, Now: fixture.now,
	})

	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "fixer-worker-1", "fixer")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNextOfType() = (%#v, %v), want claimed item", claim, err)
	}

	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "failed" {
		t.Fatalf("result = %#v, want failed status after terminal prepare error", result)
	}
	if len(git.cleanupCalls) != 0 {
		t.Fatalf("len(git.cleanupCalls) = %d, want 0 (terminal prepare failure must not force-remove dirty worktree)", len(git.cleanupCalls))
	}
	if len(git.createCalls) != 0 {
		t.Fatalf("len(git.createCalls) = %d, want 0 (existing dirty path must not recreate)", len(git.createCalls))
	}
	f.assertDirtyPreserved(t)

	queue, err := fixture.repos.Queue.GetByID(context.Background(), claim.ID)
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	if queue == nil || queue.Status != "manual_intervention" || queue.FinishedAt == nil {
		t.Fatalf("queue = %#v, want terminal manual_intervention", queue)
	}

	// Failed run checkpoint must still point at the preserved worktree (unprepared).
	run, err := fixture.repos.Runs.GetByID(context.Background(), result.RunID)
	if err != nil || run == nil {
		t.Fatalf("Runs.GetByID() = (%#v, %v)", run, err)
	}
	persisted := parseCheckpoint(run.CheckpointJSON)
	if persisted.Worktree == nil || persisted.Worktree.Path != f.wtPath {
		t.Fatalf("persisted worktree = %#v, want path %q retained", persisted.Worktree, f.wtPath)
	}
	if persisted.Worktree.PreparedAt != "" {
		t.Fatalf("persisted PreparedAt = %q, want empty (rewind / failed prepare)", persisted.Worktree.PreparedAt)
	}
	if persisted.Worktree.CleanedAt != "" {
		t.Fatalf("persisted CleanedAt = %q, want empty", persisted.Worktree.CleanedAt)
	}
}

func TestCleanupFixerWorktreeIfTerminalSkipsUnpreparedWorktree(t *testing.T) {
	t.Parallel()

	wtPath := filepath.Join(t.TempDir(), "wt-42")
	if err := os.MkdirAll(wtPath, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	git := &fakeGitGateway{}
	runner := New(Options{Git: git})
	checkpoint := &fixerCheckpoint{
		Worktree: &checkpointWorktree{Path: wtPath, Branch: "feature/fix-42"}, // PreparedAt empty
	}
	runner.cleanupFixerWorktreeIfTerminal(context.Background(), storage.ProjectRecord{
		ID: "project_1", RepoPath: t.TempDir(), BaseBranch: stringPtr("main"),
	}, checkpoint)
	if len(git.cleanupCalls) != 0 {
		t.Fatalf("len(git.cleanupCalls) = %d, want 0 for unprepared worktree", len(git.cleanupCalls))
	}
	if checkpoint.Worktree.CleanedAt != "" {
		t.Fatalf("CleanedAt = %q, want empty", checkpoint.Worktree.CleanedAt)
	}

	// Prepared worktrees still clean up on the terminal path.
	checkpoint.Worktree.PreparedAt = "2026-04-11T12:00:00.000Z"
	runner.cleanupFixerWorktreeIfTerminal(context.Background(), storage.ProjectRecord{
		ID: "project_1", RepoPath: t.TempDir(), BaseBranch: stringPtr("main"),
	}, checkpoint)
	if len(git.cleanupCalls) != 1 {
		t.Fatalf("len(git.cleanupCalls) = %d, want 1 for prepared worktree", len(git.cleanupCalls))
	}
	if checkpoint.Worktree.CleanedAt == "" {
		t.Fatal("CleanedAt empty after prepared cleanup")
	}
}

// Contract/invariant integration: real git.Gateway prepare fails on a broken
// remote while the managed worktree still holds interrupted dirt. runPrepare
// must return the prepare error and leave the checkout (and dirt) intact —
// never force CleanupWorktree because error text mentions "No such file".
func TestRunPrepareWorktreeStepRealGatewayExternalFetchErrorPreservesDirtyWorktree(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	remotePath := filepath.Join(root, "remote.git")
	repoPath := filepath.Join(root, "repo")
	worktreeRoot := filepath.Join(root, "worktrees")
	branch := "feature/fix-42"

	mustRunGit(t, root, "init", "--bare", remotePath)
	mustRunGit(t, root, "clone", remotePath, repoPath)
	mustRunGit(t, repoPath, "config", "user.email", "test@example.com")
	mustRunGit(t, repoPath, "config", "user.name", "Looper Test")
	if err := os.WriteFile(filepath.Join(repoPath, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("WriteFile README: %v", err)
	}
	mustRunGit(t, repoPath, "add", "README.md")
	mustRunGit(t, repoPath, "commit", "-m", "init")
	mustRunGit(t, repoPath, "branch", "-M", "main")
	mustRunGit(t, repoPath, "push", "-u", "origin", "main")
	mustRunGit(t, repoPath, "checkout", "-b", branch)
	if err := os.WriteFile(filepath.Join(repoPath, "fix.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatalf("WriteFile fix.txt: %v", err)
	}
	mustRunGit(t, repoPath, "add", "fix.txt")
	mustRunGit(t, repoPath, "commit", "-m", "feature")
	mustRunGit(t, repoPath, "push", "-u", "origin", branch)
	headSHA := strings.TrimSpace(mustRunGit(t, repoPath, "rev-parse", "HEAD"))
	mustRunGit(t, repoPath, "checkout", "main")

	if err := os.MkdirAll(worktreeRoot, 0o755); err != nil {
		t.Fatalf("MkdirAll worktreeRoot: %v", err)
	}
	gateway := gitinfra.New(gitinfra.Options{GitPath: "git"})
	created, err := gateway.CreateWorktree(context.Background(), gitinfra.CreateWorktreeInput{
		ProjectID:    "project_real_prepare",
		RepoPath:     repoPath,
		WorktreeRoot: worktreeRoot,
		Branch:       branch,
		BaseBranch:   "main",
		PRNumber:     42,
		CheckoutMode: gitinfra.CheckoutModeDetached,
	})
	if err != nil {
		t.Fatalf("CreateWorktree() error = %v", err)
	}
	wtPath := created.WorktreePath
	dirtyFile := filepath.Join(wtPath, "partial-agent-edit.txt")
	if err := os.WriteFile(dirtyFile, []byte("keep me\n"), 0o644); err != nil {
		t.Fatalf("WriteFile dirty marker: %v", err)
	}

	// Break origin so PrepareWorktree's fetch fails with external dependency text
	// that historically matched broad "no such file" classifiers.
	brokenRemote := filepath.Join(root, "missing-remote-does-not-exist.git")
	mustRunGit(t, repoPath, "remote", "set-url", "origin", brokenRemote)
	// Also break ssh path via env for adapter-level fetch that shells out to git;
	// real PrepareWorktree uses the remote URL above (local path missing).
	adapter := &countingRealGitGateway{inner: gateway}
	runner := New(Options{Git: adapter})
	metadata := fmt.Sprintf(`{"worktreeRoot":%q}`, worktreeRoot)
	checkpoint, prepErr := runner.runPrepareWorktreeStep(context.Background(), stepInput{
		Project:  storage.ProjectRecord{ID: "project_real_prepare", RepoPath: repoPath, MetadataJSON: &metadata},
		Loop:     storage.LoopRecord{ID: "loop_1", Status: "running"},
		Repo:     "acme/looper",
		PRNumber: 42,
		Checkpoint: fixerCheckpoint{
			Detail:   &checkpointDetail{HeadSHA: headSHA, HeadRefName: branch, BaseRefName: "main"},
			Worktree: &checkpointWorktree{Path: wtPath, Branch: branch}, // PreparedAt cleared by rewind
		},
	})
	if prepErr == nil {
		t.Fatal("runPrepareWorktreeStep() error = nil, want real prepare/fetch failure")
	}
	// Invariant: external fetch failure is not treated as unusable checkout.
	if isMissingOrUnusableFixerWorktree(wtPath, prepErr) {
		t.Fatalf("isMissingOrUnusableFixerWorktree classified real fetch error as unusable: %v", prepErr)
	}
	if checkpoint.Worktree == nil || checkpoint.Worktree.Path != wtPath {
		t.Fatalf("checkpoint.Worktree = %#v, want path preserved", checkpoint.Worktree)
	}
	if adapter.cleanupCalls != 0 {
		t.Fatalf("cleanupCalls = %d, want 0 (must not force-remove on external fetch error)", adapter.cleanupCalls)
	}
	if adapter.createCalls != 0 {
		t.Fatalf("createCalls = %d, want 0", adapter.createCalls)
	}
	if _, err := os.Stat(wtPath); err != nil {
		t.Fatalf("worktree path missing after prepare error: %v", err)
	}
	got, err := os.ReadFile(dirtyFile)
	if err != nil || string(got) != "keep me\n" {
		t.Fatalf("dirty marker after real prepare error = %q err=%v, want preserved", got, err)
	}
}

// countingRealGitGateway adapts infra/git.Gateway for fixer.GitGateway and
// records cleanup/create so lifecycle invariants can be asserted.
type countingRealGitGateway struct {
	inner        *gitinfra.Gateway
	cleanupCalls int
	createCalls  int
	prepareCalls int
}

func (g *countingRealGitGateway) CreateWorktree(ctx context.Context, input CreateWorktreeInput) (CreateWorktreeResult, error) {
	g.createCalls++
	rec, err := g.inner.CreateWorktree(ctx, gitinfra.CreateWorktreeInput{
		ProjectID: input.ProjectID, RepoPath: input.RepoPath, WorktreeRoot: input.WorktreeRoot,
		Branch: input.Branch, BaseBranch: input.BaseBranch, PRNumber: input.PRNumber,
		ProtectedBranches: input.ProtectedBranches, CheckoutMode: gitinfra.CheckoutMode(input.CheckoutMode),
	})
	if err != nil {
		return CreateWorktreeResult{}, err
	}
	head := ""
	if rec.HeadSHA != nil {
		head = *rec.HeadSHA
	}
	return CreateWorktreeResult{WorktreePath: rec.WorktreePath, Branch: rec.Branch, HeadSHA: head}, nil
}

func (g *countingRealGitGateway) PrepareWorktree(ctx context.Context, input PrepareWorktreeInput) (PrepareWorktreeResult, error) {
	g.prepareCalls++
	res, err := g.inner.PrepareWorktree(ctx, gitinfra.PrepareWorktreeInput{
		RepoPath: input.RepoPath, WorktreeRoot: input.WorktreeRoot, WorktreePath: input.WorktreePath,
		Branch: input.Branch, ExpectedHeadSHA: input.ExpectedHeadSHA, Remote: input.Remote,
	})
	if err != nil {
		return PrepareWorktreeResult{}, err
	}
	return PrepareWorktreeResult{HeadSHA: res.HeadSHA, Clean: res.Clean}, nil
}

func (g *countingRealGitGateway) InspectHead(ctx context.Context, input InspectHeadInput) (InspectHeadResult, error) {
	res, err := g.inner.InspectHead(ctx, gitinfra.InspectHeadInput{
		RepoPath: input.RepoPath, WorktreeRoot: input.WorktreeRoot, WorktreePath: input.WorktreePath, BaseRef: input.BaseRef,
	})
	if err != nil {
		return InspectHeadResult{}, err
	}
	return InspectHeadResult{
		HeadSHA: res.HeadSHA, NewCommitSHAs: res.NewCommitSHAs,
		HasUncommittedChanges: res.HasUncommittedChanges, ChangedFiles: res.ChangedFiles,
	}, nil
}

func (g *countingRealGitGateway) Commit(ctx context.Context, input CommitInput) (CommitResult, error) {
	res, err := g.inner.Commit(ctx, gitinfra.CommitInput{
		RepoPath: input.RepoPath, WorktreeRoot: input.WorktreeRoot, WorktreePath: input.WorktreePath, Message: input.Message,
	})
	if err != nil {
		return CommitResult{}, err
	}
	return CommitResult{CommitSHA: res.CommitSHA}, nil
}

func (g *countingRealGitGateway) Push(ctx context.Context, input PushInput) error {
	return g.inner.Push(ctx, gitinfra.PushInput{
		RepoPath: input.RepoPath, WorktreeRoot: input.WorktreeRoot, WorktreePath: input.WorktreePath,
		Branch: input.Branch, Remote: input.Remote, ExpectedRemoteHeadSHA: input.ExpectedRemoteHeadSHA,
		ProtectedBranches: input.ProtectedBranches,
	})
}

func (g *countingRealGitGateway) FetchBranch(ctx context.Context, repoPath, remote, branch string) error {
	return g.inner.FetchBranch(ctx, repoPath, remote, branch)
}

func (g *countingRealGitGateway) IsAncestor(ctx context.Context, repoPath, ancestor, descendant string) (bool, error) {
	return g.inner.IsAncestor(ctx, repoPath, ancestor, descendant)
}

func (g *countingRealGitGateway) MergeBaseIntoWorktree(ctx context.Context, input MergeBaseInput) (MergeBaseResult, error) {
	res, err := g.inner.MergeBaseIntoWorktree(ctx, gitinfra.MergeBaseInput{
		WorktreePath: input.WorktreePath, Remote: input.Remote, BaseBranch: input.BaseBranch,
	})
	if err != nil {
		return MergeBaseResult{}, err
	}
	return MergeBaseResult{AlreadyUpToDate: res.AlreadyUpToDate, Conflicted: res.Conflicted}, nil
}

func (g *countingRealGitGateway) CleanupWorktree(ctx context.Context, input CleanupWorktreeInput) error {
	g.cleanupCalls++
	return g.inner.CleanupWorktree(ctx, gitinfra.CleanupWorktreeInput{
		ProjectID: input.ProjectID, RepoPath: input.RepoPath, WorktreeRoot: input.WorktreeRoot,
		WorktreePath: input.WorktreePath, Branch: input.Branch, ProtectedBranches: input.ProtectedBranches,
	})
}

func mustRunGit(t *testing.T, cwd string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = cwd
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		if msg == "" {
			msg = err.Error()
		}
		t.Fatalf("git %v: %s", args, msg)
	}
	return stdout.String()
}
