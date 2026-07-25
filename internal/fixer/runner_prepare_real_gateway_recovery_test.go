package fixer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	metadata := fmt.Sprintf(`{"worktreeRoot":%q}`, worktreeRoot)
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
	metadata := fmt.Sprintf(`{"worktreeRoot":%q}`, worktreeRoot)
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

// Real gateway lifecycle: .git points at an existing but empty/corrupt private
// gitdir. Real git reports "not a git repository"; prepare must classify the
// checkout as unusable and recover (cleanup + clear/recreate) rather than
// preserving the broken path and returning the same retryable prepare error forever.
func TestRunPrepareWorktreeStepRealGatewayRecreatesCorruptLinkedGitdir(t *testing.T) {
	t.Parallel()

	root, _, repoPath, headSHA := setupRealRepoWithBranch(t, "feature/fix-42")
	worktreeRoot := filepath.Join(root, "worktrees")
	branch := "feature/fix-42"
	projectID := "project_real_corrupt_gitdir"
	if err := os.MkdirAll(worktreeRoot, 0o755); err != nil {
		t.Fatalf("MkdirAll worktreeRoot: %v", err)
	}
	gateway := gitinfra.New(gitinfra.Options{GitPath: "git"})
	created, err := gateway.CreateWorktree(context.Background(), gitinfra.CreateWorktreeInput{
		ProjectID:    projectID,
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

	// Resolve linked private gitdir and strip required metadata (HEAD).
	gitMeta, err := os.ReadFile(filepath.Join(wtPath, ".git"))
	if err != nil {
		t.Fatalf("ReadFile .git: %v", err)
	}
	line := strings.TrimSpace(string(gitMeta))
	const prefix = "gitdir:"
	if !strings.HasPrefix(strings.ToLower(line), prefix) {
		t.Fatalf(".git content = %q, want gitdir: pointer", line)
	}
	gitdir := strings.TrimSpace(line[len(prefix):])
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(wtPath, gitdir)
	}
	if err := os.Remove(filepath.Join(gitdir, "HEAD")); err != nil {
		t.Fatalf("Remove private HEAD: %v", err)
	}
	// Drop worktree content so recovery can clear-and-recreate (only unusable
	// .git metadata remains). Populated agent dirt would correctly go MI.
	entries, err := os.ReadDir(wtPath)
	if err != nil {
		t.Fatalf("ReadDir worktree: %v", err)
	}
	for _, e := range entries {
		if e.Name() == ".git" {
			continue
		}
		if err := os.RemoveAll(filepath.Join(wtPath, e.Name())); err != nil {
			t.Fatalf("RemoveAll %s: %v", e.Name(), err)
		}
	}
	// Probe must already report unusable with integrity-looking prepare text.
	probeErr := errors.New("fatal: not a git repository (or any of the parent directories): .git")
	if !isMissingOrUnusableFixerWorktree(wtPath, probeErr) {
		t.Fatal("isMissingOrUnusableFixerWorktree = false for corrupt linked gitdir, want true")
	}

	adapter := &countingRealGitGateway{inner: gateway}
	runner := New(Options{Git: adapter})
	metadata := fmt.Sprintf(`{"worktreeRoot":%q}`, worktreeRoot)
	checkpoint, prepErr := runner.runPrepareWorktreeStep(context.Background(), stepInput{
		Project:  storage.ProjectRecord{ID: projectID, RepoPath: repoPath, MetadataJSON: &metadata},
		Loop:     storage.LoopRecord{ID: "loop_1", Status: "running"},
		Repo:     "acme/looper",
		PRNumber: 42,
		Checkpoint: fixerCheckpoint{
			Detail:   &checkpointDetail{HeadSHA: headSHA, HeadRefName: branch, BaseRefName: "main"},
			Worktree: &checkpointWorktree{Path: wtPath, Branch: branch},
		},
	})
	if prepErr != nil {
		t.Fatalf("runPrepareWorktreeStep() error = %v, want recreated prepared worktree", prepErr)
	}
	if checkpoint.Worktree == nil || checkpoint.Worktree.PreparedAt == "" || checkpoint.Worktree.OwnerToken == "" {
		t.Fatalf("checkpoint.Worktree = %#v, want prepared with owner token", checkpoint.Worktree)
	}
	if adapter.cleanupCalls < 1 {
		t.Fatalf("cleanupCalls = %d, want >= 1 (unusable path recovery)", adapter.cleanupCalls)
	}
	if adapter.createCalls < 1 {
		t.Fatalf("createCalls = %d, want >= 1", adapter.createCalls)
	}
	if _, err := os.Stat(filepath.Join(checkpoint.Worktree.Path, "README.md")); err != nil {
		t.Fatalf("recreated worktree missing README: %v", err)
	}
	if !localFixerWorktreeCheckoutUsable(checkpoint.Worktree.Path) {
		t.Fatalf("recreated path %s still unusable after prepare", checkpoint.Worktree.Path)
	}
}

// Real gateway lifecycle: retained checkpoint path holds only a malformed .git
// file (not a gitdir: pointer). Real git reports "invalid gitfile format";
// prepare must classify as unusable and clear/recreate rather than retry forever.
func TestRunPrepareWorktreeStepRealGatewayRecreatesMalformedGitfile(t *testing.T) {
	t.Parallel()

	root, _, repoPath, headSHA := setupRealRepoWithBranch(t, "feature/fix-42")
	worktreeRoot := filepath.Join(root, "worktrees")
	branch := "feature/fix-42"
	projectID := "project_real_malformed_gitfile"
	// Deterministic managed path used by CreateWorktree for detached PR worktrees.
	stalePath := filepath.Join(worktreeRoot, fmt.Sprintf("looper-fix-%s-pr-%d-detached", projectID, 42))
	if err := os.MkdirAll(stalePath, 0o755); err != nil {
		t.Fatalf("MkdirAll stale: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stalePath, ".git"), []byte("not-a-valid-gitfile\n"), 0o644); err != nil {
		t.Fatalf("WriteFile malformed .git: %v", err)
	}

	// Probe must report unusable with Git's distinct gitfile error.
	probeErr := errors.New("fatal: invalid gitfile format: .git")
	if !isMissingOrUnusableFixerWorktree(stalePath, probeErr) {
		t.Fatal("isMissingOrUnusableFixerWorktree = false for malformed gitfile, want true")
	}
	if localFixerWorktreeCheckoutUsable(stalePath) {
		t.Fatal("localFixerWorktreeCheckoutUsable = true for malformed gitfile, want false")
	}

	adapter := &countingRealGitGateway{inner: gitinfra.New(gitinfra.Options{GitPath: "git"})}
	runner := New(Options{Git: adapter})
	metadata := fmt.Sprintf(`{"worktreeRoot":%q}`, worktreeRoot)
	checkpoint, prepErr := runner.runPrepareWorktreeStep(context.Background(), stepInput{
		Project:  storage.ProjectRecord{ID: projectID, RepoPath: repoPath, MetadataJSON: &metadata},
		Loop:     storage.LoopRecord{ID: "loop_1", Status: "running"},
		Repo:     "acme/looper",
		PRNumber: 42,
		Checkpoint: fixerCheckpoint{
			Detail:   &checkpointDetail{HeadSHA: headSHA, HeadRefName: branch, BaseRefName: "main"},
			Worktree: &checkpointWorktree{Path: stalePath, Branch: branch},
		},
	})
	if prepErr != nil {
		t.Fatalf("runPrepareWorktreeStep() error = %v, want recreated prepared worktree", prepErr)
	}
	if checkpoint.Worktree == nil || checkpoint.Worktree.PreparedAt == "" || checkpoint.Worktree.OwnerToken == "" {
		t.Fatalf("checkpoint.Worktree = %#v, want prepared with owner token", checkpoint.Worktree)
	}
	if adapter.cleanupCalls < 1 {
		t.Fatalf("cleanupCalls = %d, want >= 1 (unusable path recovery)", adapter.cleanupCalls)
	}
	if adapter.createCalls < 1 {
		t.Fatalf("createCalls = %d, want >= 1", adapter.createCalls)
	}
	if _, err := os.Stat(filepath.Join(checkpoint.Worktree.Path, "README.md")); err != nil {
		t.Fatalf("recreated worktree missing README: %v", err)
	}
	if !localFixerWorktreeCheckoutUsable(checkpoint.Worktree.Path) {
		t.Fatalf("recreated path %s still unusable after prepare", checkpoint.Worktree.Path)
	}
}
