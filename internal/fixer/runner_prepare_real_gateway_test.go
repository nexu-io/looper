package fixer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gitinfra "github.com/nexu-io/looper/internal/infra/git"
	"github.com/nexu-io/looper/internal/storage"
)

// Contract/invariant integration: real git.Gateway prepare fails on a broken
// remote while the managed worktree still holds interrupted dirt. runPrepare
// must return the prepare error and leave the checkout (and dirt) intact —
// never force CleanupWorktree because error text mentions "No such file".
func TestRunPrepareWorktreeStepRealGatewayExternalFetchErrorPreservesDirtyWorktree(t *testing.T) {
	t.Parallel()

	root, _, repoPath, headSHA := setupRealRepoWithBranch(t, "feature/fix-42")
	worktreeRoot := filepath.Join(root, "worktrees")
	branch := "feature/fix-42"
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

	// Break origin so PrepareWorktree's fetch fails with external dependency text.
	brokenRemote := filepath.Join(root, "missing-remote-does-not-exist.git")
	mustRunGit(t, repoPath, "remote", "set-url", "origin", brokenRemote)
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

// Contract/invariant: an SSH/remote helper that prints the classic local
// integrity message must not force CleanupWorktree when the managed checkout
// still has valid local .git metadata and interrupted dirt.
func TestRunPrepareWorktreeStepRealGatewayRemoteHelperNotAGitRepoPreservesDirtyWorktree(t *testing.T) {
	t.Parallel()

	root, _, repoPath, headSHA := setupRealRepoWithBranch(t, "feature/fix-42")
	worktreeRoot := filepath.Join(root, "worktrees")
	branch := "feature/fix-42"
	if err := os.MkdirAll(worktreeRoot, 0o755); err != nil {
		t.Fatalf("MkdirAll worktreeRoot: %v", err)
	}
	gateway := gitinfra.New(gitinfra.Options{GitPath: "git"})
	created, err := gateway.CreateWorktree(context.Background(), gitinfra.CreateWorktreeInput{
		ProjectID:    "project_real_ssh_helper",
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

	// Force fetch through a fake SSH helper that emits the exact local-integrity
	// phrase reviewers observed from real remote helpers, while the local
	// worktree remains a valid checkout.
	helperScript := filepath.Join(root, "fake-ssh-helper.sh")
	script := "#!/bin/sh\nprintf '%s\\n' 'fatal: not a git repository (or any of the parent directories): .git' >&2\nexit 1\n"
	if err := os.WriteFile(helperScript, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile helper: %v", err)
	}
	// SCP-style SSH URL avoids ssh.variant=simple "port not supported" failures
	// seen with ssh://host:port/... while still routing through core.sshCommand.
	mustRunGit(t, repoPath, "remote", "set-url", "origin", "git@127.0.0.1:nonexistent/looper.git")
	// core.sshCommand is shared via the main repo common dir for linked worktrees.
	mustRunGit(t, repoPath, "config", "core.sshCommand", helperScript)

	adapter := &countingRealGitGateway{inner: gateway}
	runner := New(Options{Git: adapter})
	metadata := fmt.Sprintf(`{"worktreeRoot":%q}`, worktreeRoot)
	checkpoint, prepErr := runner.runPrepareWorktreeStep(context.Background(), stepInput{
		Project:  storage.ProjectRecord{ID: "project_real_ssh_helper", RepoPath: repoPath, MetadataJSON: &metadata},
		Loop:     storage.LoopRecord{ID: "loop_1", Status: "running"},
		Repo:     "acme/looper",
		PRNumber: 42,
		Checkpoint: fixerCheckpoint{
			Detail:   &checkpointDetail{HeadSHA: headSHA, HeadRefName: branch, BaseRefName: "main"},
			Worktree: &checkpointWorktree{Path: wtPath, Branch: branch}, // PreparedAt cleared by rewind
		},
	})
	if prepErr == nil {
		t.Fatal("runPrepareWorktreeStep() error = nil, want real prepare/fetch failure from ssh helper")
	}
	if !strings.Contains(strings.ToLower(prepErr.Error()), "not a git repository") {
		t.Fatalf("prepare error = %v, want remote-helper integrity wording", prepErr)
	}
	if isMissingOrUnusableFixerWorktree(wtPath, prepErr) {
		t.Fatalf("isMissingOrUnusableFixerWorktree classified remote-helper text as unusable: %v", prepErr)
	}
	if checkpoint.Worktree == nil || checkpoint.Worktree.Path != wtPath {
		t.Fatalf("checkpoint.Worktree = %#v, want path preserved", checkpoint.Worktree)
	}
	if adapter.cleanupCalls != 0 {
		t.Fatalf("cleanupCalls = %d, want 0 (must not force-remove valid local checkout)", adapter.cleanupCalls)
	}
	if adapter.createCalls != 0 {
		t.Fatalf("createCalls = %d, want 0", adapter.createCalls)
	}
	if _, err := os.Stat(wtPath); err != nil {
		t.Fatalf("worktree path missing after prepare error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wtPath, ".git")); err != nil {
		t.Fatalf("local .git missing after prepare error: %v", err)
	}
	got, err := os.ReadFile(dirtyFile)
	if err != nil || string(got) != "keep me\n" {
		t.Fatalf("dirty marker after remote-helper prepare error = %q err=%v, want preserved", got, err)
	}
}
