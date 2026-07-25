package fixer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	gitinfra "github.com/nexu-io/looper/internal/infra/git"
	"github.com/nexu-io/looper/internal/storage"
)

// Real gateway lifecycle: .git points at an existing but empty/corrupt private
// gitdir. Real git reports "not a git repository"; prepare must classify the
// checkout as unusable and recover (cleanup + clear/recreate) rather than
// preserving the broken path and returning the same retryable prepare error forever.
func TestRunPrepareWorktreeStepRealGatewayRecreatesCorruptLinkedGitdir(t *testing.T) {
	t.Parallel()

	f := setupRealLinkedWorktree(t, "project_real_corrupt_gitdir", "feature/fix-42")
	if err := os.Remove(filepath.Join(f.gitdir, "HEAD")); err != nil {
		t.Fatalf("Remove private HEAD: %v", err)
	}
	stripWorktreeExceptGit(t, f.wtPath)

	probeErr := errors.New("fatal: not a git repository (or any of the parent directories): .git")
	if !isMissingOrUnusableFixerWorktree(f.wtPath, probeErr) {
		t.Fatal("isMissingOrUnusableFixerWorktree = false for corrupt linked gitdir, want true")
	}
	assertRealGatewayRecreatesUnusableWorktree(t, f)
}

// Real gateway lifecycle: linked private gitdir still has HEAD but lost
// commondir. Real git reports "not a git repository"; prepare must classify as
// unusable and recover rather than preserve the broken checkout forever.
func TestRunPrepareWorktreeStepRealGatewayRecreatesMissingCommondir(t *testing.T) {
	t.Parallel()

	f := setupRealLinkedWorktree(t, "project_real_missing_commondir", "feature/fix-42")
	if _, err := os.Stat(filepath.Join(f.gitdir, "HEAD")); err != nil {
		t.Fatalf("private HEAD missing before corruption: %v", err)
	}
	if err := os.Remove(filepath.Join(f.gitdir, "commondir")); err != nil {
		t.Fatalf("Remove private commondir: %v", err)
	}
	stripWorktreeExceptGit(t, f.wtPath)

	probeErr := errors.New("fatal: not a git repository (or any of the parent directories): .git")
	if localFixerWorktreeCheckoutUsable(f.wtPath) {
		t.Fatal("localFixerWorktreeCheckoutUsable = true for missing commondir, want false")
	}
	if !isMissingOrUnusableFixerWorktree(f.wtPath, probeErr) {
		t.Fatal("isMissingOrUnusableFixerWorktree = false for missing commondir, want true")
	}
	assertRealGatewayRecreatesUnusableWorktree(t, f)
}

// Real gateway lifecycle: private gitdir still has HEAD + commondir, but the
// common repository only has HEAD (objects/ missing). Real git reports "not a
// git repository"; prepare must classify as unusable and recover. Point
// commondir at a separate broken common so the main repoPath stays healthy for
// CreateWorktree recreation (shared common corruption would also destroy the
// source repository used to rebuild the managed path).
func TestRunPrepareWorktreeStepRealGatewayRecreatesCorruptCommonRepo(t *testing.T) {
	t.Parallel()

	f := setupRealLinkedWorktree(t, "project_real_corrupt_common", "feature/fix-42")
	if !localGitRepositoryMetadataUsable(resolveLinkedCommonDir(t, f.gitdir)) {
		t.Fatal("common repo not usable before corruption")
	}
	// HEAD-only common: missing objects/ (and refs/) — Git rejects as not a repo.
	brokenCommon := filepath.Join(t.TempDir(), "broken-common")
	if err := os.MkdirAll(brokenCommon, 0o755); err != nil {
		t.Fatalf("MkdirAll brokenCommon: %v", err)
	}
	if err := os.WriteFile(filepath.Join(brokenCommon, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatalf("WriteFile brokenCommon HEAD: %v", err)
	}
	if err := os.WriteFile(filepath.Join(f.gitdir, "commondir"), []byte(brokenCommon+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile broken commondir: %v", err)
	}
	// Confirm real git already fails on the corrupted common metadata.
	if err := tryRunGit(f.wtPath, "rev-parse", "--git-dir"); err == nil {
		t.Fatal("git rev-parse succeeded with HEAD-only common, want not a git repository")
	}
	stripWorktreeExceptGit(t, f.wtPath)

	probeErr := errors.New("fatal: not a git repository (or any of the parent directories): .git")
	if localFixerWorktreeCheckoutUsable(f.wtPath) {
		t.Fatal("localFixerWorktreeCheckoutUsable = true for corrupt common objects, want false")
	}
	if !isMissingOrUnusableFixerWorktree(f.wtPath, probeErr) {
		t.Fatal("isMissingOrUnusableFixerWorktree = false for corrupt common objects, want true")
	}
	assertRealGatewayRecreatesUnusableWorktree(t, f)
}

// Real gateway lifecycle: retained checkpoint path holds only a malformed .git
// file (not a gitdir: pointer). Real git reports "invalid gitfile format";
// prepare must classify as unusable and clear/recreate rather than retry forever.
func TestRunPrepareWorktreeStepRealGatewayRecreatesMalformedGitfile(t *testing.T) {
	t.Parallel()

	_, _, repoPath, headSHA := setupRealRepoWithBranch(t, "feature/fix-42")
	worktreeRoot := filepath.Join(t.TempDir(), "worktrees")
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

	probeErr := errors.New("fatal: invalid gitfile format: .git")
	if !isMissingOrUnusableFixerWorktree(stalePath, probeErr) {
		t.Fatal("isMissingOrUnusableFixerWorktree = false for malformed gitfile, want true")
	}
	if localFixerWorktreeCheckoutUsable(stalePath) {
		t.Fatal("localFixerWorktreeCheckoutUsable = true for malformed gitfile, want false")
	}

	adapter := &countingRealGitGateway{inner: gitinfra.New(gitinfra.Options{GitPath: "git"})}
	runner := New(Options{Git: adapter})
	metadata := worktreeRootMetadataJSON(worktreeRoot)
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
	assertPreparedRecreatedWorktree(t, checkpoint, prepErr, adapter, stalePath)
}

func assertRealGatewayRecreatesUnusableWorktree(t *testing.T, f realLinkedWorktreeFixture) {
	t.Helper()
	adapter := &countingRealGitGateway{inner: f.gateway}
	runner := New(Options{Git: adapter})
	metadata := worktreeRootMetadataJSON(f.worktreeRoot)
	checkpoint, prepErr := runner.runPrepareWorktreeStep(context.Background(), stepInput{
		Project:  storage.ProjectRecord{ID: f.projectID, RepoPath: f.repoPath, MetadataJSON: &metadata},
		Loop:     storage.LoopRecord{ID: "loop_1", Status: "running"},
		Repo:     "acme/looper",
		PRNumber: 42,
		Checkpoint: fixerCheckpoint{
			Detail:   &checkpointDetail{HeadSHA: f.headSHA, HeadRefName: f.branch, BaseRefName: "main"},
			Worktree: &checkpointWorktree{Path: f.wtPath, Branch: f.branch},
		},
	})
	assertPreparedRecreatedWorktree(t, checkpoint, prepErr, adapter, f.wtPath)
}

func assertPreparedRecreatedWorktree(t *testing.T, checkpoint fixerCheckpoint, prepErr error, adapter *countingRealGitGateway, wantPath string) {
	t.Helper()
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
	if wantPath != "" && checkpoint.Worktree.Path != wantPath {
		t.Fatalf("Worktree.Path = %q, want managed path %q", checkpoint.Worktree.Path, wantPath)
	}
	if _, err := os.Stat(filepath.Join(checkpoint.Worktree.Path, "README.md")); err != nil {
		t.Fatalf("recreated worktree missing README: %v", err)
	}
	if !localFixerWorktreeCheckoutUsable(checkpoint.Worktree.Path) {
		t.Fatalf("recreated path %s still unusable after prepare", checkpoint.Worktree.Path)
	}
}
