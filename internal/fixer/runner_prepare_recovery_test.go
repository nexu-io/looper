package fixer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/nexu-io/looper/internal/storage"
)

// Prepare probe failures on rewind (fetch/transport/remote-head/ssh) never read
// worktree status, so CleanupWorktree must not run — that would destroy
// interrupted-repair dirt that the dirty-adopt path never got to evaluate.
func TestRunPrepareWorktreeStepPrepareErrorPreservesExistingWorktree(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		err         error
		withGitMeta bool // integrity-looking remote text needs local .git to refuse cleanup
	}{
		{name: "fetch_transport", err: fmt.Errorf("git fetch origin feature/fix-42: fatal: unable to access remote")},
		{name: "remote_head_changed", err: fmt.Errorf("remote head for feature/fix-42 changed: expected base-head, got advanced-head")},
		// External dependency wording that previously matched the broad classifier.
		{name: "ssh_no_such_file", err: fmt.Errorf("error: cannot run ssh: No such file or directory\nfatal: unable to fork")},
		// SSH/remote helper can emit local-integrity wording; local .git must win.
		{
			name:        "remote_helper_not_a_git_repository",
			err:         fmt.Errorf("fatal: not a git repository (or any of the parent directories): .git"),
			withGitMeta: true,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newDirtyAdoptFixture(t)
			if tc.withGitMeta {
				// Linked-worktree-style local metadata with required private
				// HEAD + resolvable common repo integrity (HEAD + objects/ +
				// refs/) so the probe treats the checkout as usable despite
				// integrity-looking prepare stderr.
				gitdir := filepath.Join(t.TempDir(), "gitdir")
				common := filepath.Join(t.TempDir(), "common")
				if err := os.MkdirAll(gitdir, 0o755); err != nil {
					t.Fatalf("MkdirAll gitdir: %v", err)
				}
				if err := os.WriteFile(filepath.Join(gitdir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
					t.Fatalf("WriteFile gitdir HEAD: %v", err)
				}
				writeMinimalGitRepoMetadata(t, common)
				if err := os.WriteFile(filepath.Join(gitdir, "commondir"), []byte(common+"\n"), 0o644); err != nil {
					t.Fatalf("WriteFile commondir: %v", err)
				}
				if err := os.WriteFile(filepath.Join(f.wtPath, ".git"), []byte("gitdir: "+gitdir+"\n"), 0o644); err != nil {
					t.Fatalf("WriteFile .git: %v", err)
				}
			}
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

// Empty unregistered directories left after CleanupWorktree must be removed so
// CreateWorktree can reuse the managed path. Fake cleanup alone is not enough:
// the real gateway swallows "is not a working tree" without deleting the dir.
func TestRunPrepareWorktreeStepRecreatesEmptyUnusableCheckpointWorktree(t *testing.T) {
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
	if _, err := os.Stat(stalePath); !os.IsNotExist(err) {
		t.Fatalf("empty stale path still exists after clear, err=%v", err)
	}
	if len(git.prepareCalls) != 2 {
		t.Fatalf("len(git.prepareCalls) = %d, want 2 (stale then recreated)", len(git.prepareCalls))
	}
}

// Populated unregistered leftovers must not be recreated over — preserve for MI.
func TestRunPrepareWorktreeStepPreservesPopulatedUnusableCheckpointWorktree(t *testing.T) {
	t.Parallel()

	repoPath := t.TempDir()
	worktreeRoot := filepath.Join(t.TempDir(), "worktrees")
	stalePath := filepath.Join(worktreeRoot, "wt-stale")
	if err := os.MkdirAll(stalePath, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	marker := filepath.Join(stalePath, "partial-agent-edit.txt")
	if err := os.WriteFile(marker, []byte("keep me\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	metadata := fmt.Sprintf(`{"worktreeRoot":%q}`, worktreeRoot)
	git := &fakeGitGateway{
		prepareErr:    fmt.Errorf("fatal: %s is not a working tree", stalePath),
		createResult:  CreateWorktreeResult{WorktreePath: stalePath, Branch: "feature/fix-42", HeadSHA: "base-head"},
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
	assertPrepareDirtyManualIntervention(t, checkpoint, err)
	if len(git.cleanupCalls) != 1 {
		t.Fatalf("len(git.cleanupCalls) = %d, want 1", len(git.cleanupCalls))
	}
	if len(git.createCalls) != 0 {
		t.Fatalf("len(git.createCalls) = %d, want 0 (must not recreate over populated leftover)", len(git.createCalls))
	}
	got, readErr := os.ReadFile(marker)
	if readErr != nil || string(got) != "keep me\n" {
		t.Fatalf("marker = %q err=%v, want preserved", got, readErr)
	}
}
