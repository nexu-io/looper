package fixer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	gitinfra "github.com/nexu-io/looper/internal/infra/git"
	"github.com/nexu-io/looper/internal/storage"
)

// Real gateway lifecycle: unregistered empty dir at the managed path survives
// CleanupWorktree (git leaves it), but prepare must remove it and recreate.
func TestRunPrepareWorktreeStepRealGatewayRecreatesEmptyUnregisteredPath(t *testing.T) {
	t.Parallel()

	_, _, repoPath, headSHA := setupRealRepoWithBranch(t, "feature/fix-42")
	worktreeRoot := filepath.Join(t.TempDir(), "worktrees")
	branch := "feature/fix-42"
	projectID := "project_real_unreg_empty"
	// Deterministic managed path used by CreateWorktree for detached PR worktrees.
	stalePath := filepath.Join(worktreeRoot, fmt.Sprintf("looper-fix-%s-pr-%d-detached", projectID, 42))
	if err := os.MkdirAll(stalePath, 0o755); err != nil {
		t.Fatalf("MkdirAll stale: %v", err)
	}

	adapter := &countingRealGitGateway{inner: gitinfra.New(gitinfra.Options{GitPath: "git"})}
	runner := New(Options{Git: adapter})
	metadata := worktreeRootMetadataJSON(worktreeRoot)
	checkpoint, err := runner.runPrepareWorktreeStep(context.Background(), stepInput{
		Project:  storage.ProjectRecord{ID: projectID, RepoPath: repoPath, MetadataJSON: &metadata},
		Loop:     storage.LoopRecord{ID: "loop_1", Status: "running"},
		Repo:     "acme/looper",
		PRNumber: 42,
		Checkpoint: fixerCheckpoint{
			Detail:   &checkpointDetail{HeadSHA: headSHA, HeadRefName: branch, BaseRefName: "main"},
			Worktree: &checkpointWorktree{Path: stalePath, Branch: branch},
		},
	})
	if err != nil {
		t.Fatalf("runPrepareWorktreeStep() error = %v", err)
	}
	if checkpoint.Worktree == nil || checkpoint.Worktree.PreparedAt == "" {
		t.Fatalf("checkpoint.Worktree = %#v, want prepared recreated worktree", checkpoint.Worktree)
	}
	if adapter.cleanupCalls != 1 {
		t.Fatalf("cleanupCalls = %d, want 1", adapter.cleanupCalls)
	}
	if adapter.createCalls != 1 {
		t.Fatalf("createCalls = %d, want 1", adapter.createCalls)
	}
	// Path should be a healthy worktree again (same managed path).
	if checkpoint.Worktree.Path != stalePath {
		t.Fatalf("Worktree.Path = %q, want managed path %q", checkpoint.Worktree.Path, stalePath)
	}
	if _, err := os.Stat(filepath.Join(checkpoint.Worktree.Path, "README.md")); err != nil {
		t.Fatalf("recreated worktree missing README: %v", err)
	}
}

// Real gateway lifecycle: populated unregistered leftover is preserved for MI
// because CleanupWorktree does not delete unregistered directories.
func TestRunPrepareWorktreeStepRealGatewayPreservesPopulatedUnregisteredPath(t *testing.T) {
	t.Parallel()

	_, _, repoPath, headSHA := setupRealRepoWithBranch(t, "feature/fix-42")
	worktreeRoot := filepath.Join(t.TempDir(), "worktrees")
	branch := "feature/fix-42"
	projectID := "project_real_unreg_pop"
	stalePath := filepath.Join(worktreeRoot, fmt.Sprintf("looper-fix-%s-pr-%d-detached", projectID, 42))
	if err := os.MkdirAll(stalePath, 0o755); err != nil {
		t.Fatalf("MkdirAll stale: %v", err)
	}
	marker := filepath.Join(stalePath, "partial-agent-edit.txt")
	if err := os.WriteFile(marker, []byte("keep me\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	adapter := &countingRealGitGateway{inner: gitinfra.New(gitinfra.Options{GitPath: "git"})}
	runner := New(Options{Git: adapter})
	metadata := worktreeRootMetadataJSON(worktreeRoot)
	checkpoint, err := runner.runPrepareWorktreeStep(context.Background(), stepInput{
		Project:  storage.ProjectRecord{ID: projectID, RepoPath: repoPath, MetadataJSON: &metadata},
		Loop:     storage.LoopRecord{ID: "loop_1", Status: "running"},
		Repo:     "acme/looper",
		PRNumber: 42,
		Checkpoint: fixerCheckpoint{
			Detail:   &checkpointDetail{HeadSHA: headSHA, HeadRefName: branch, BaseRefName: "main"},
			Worktree: &checkpointWorktree{Path: stalePath, Branch: branch},
		},
	})
	assertPrepareDirtyManualIntervention(t, checkpoint, err)
	if adapter.cleanupCalls != 1 {
		t.Fatalf("cleanupCalls = %d, want 1", adapter.cleanupCalls)
	}
	if adapter.createCalls != 0 {
		t.Fatalf("createCalls = %d, want 0", adapter.createCalls)
	}
	got, readErr := os.ReadFile(marker)
	if readErr != nil || string(got) != "keep me\n" {
		t.Fatalf("marker = %q err=%v, want preserved", got, readErr)
	}
}
