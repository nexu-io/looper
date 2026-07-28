package reviewer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gitinfra "github.com/nexu-io/looper/internal/infra/git"
	"github.com/nexu-io/looper/internal/storage"
	"github.com/nexu-io/looper/internal/worktreesafety"
)

// When marker restore fails after CreateWorktree cleared ownership, surface the
// restore error instead of returning only the original prepare/dirty failure
// with desynchronized authority.
func TestRunPrepareWorktreeStepPropagatesOwnerTokenRestoreFailureOnDirty(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	repoPath := t.TempDir()
	worktreeRoot := filepath.Join(t.TempDir(), "worktrees")
	projectID := "project_1"
	prNumber := int64(42)
	wtPath := gitinfra.DetachedPRWorktreePath(worktreeRoot, projectID, prNumber)
	if err := os.MkdirAll(wtPath, 0o755); err != nil {
		t.Fatalf("MkdirAll worktree: %v", err)
	}
	const token = "fixer:loop_restore_fail:run_1:token"
	if err := worktreesafety.WriteFixerOwnerToken(wtPath, token); err != nil {
		t.Fatalf("WriteFixerOwnerToken: %v", err)
	}
	metadata := fmt.Sprintf(`{"worktreeRoot":%q}`, worktreeRoot)
	git := &restoreFailAfterClearGit{worktreePath: wtPath}
	t.Cleanup(func() {
		_ = os.Chmod(filepath.Join(wtPath, ".git"), 0o755)
	})
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: &fakeGitHubGateway{}, Git: git, Logger: fixture.logger, Now: fixture.now})

	_, err := runner.runPrepareWorktreeStep(context.Background(), stepInput{
		Project:  storage.ProjectRecord{ID: projectID, RepoPath: repoPath, MetadataJSON: &metadata},
		Repo:     "acme/looper",
		PRNumber: prNumber,
		Checkpoint: reviewerCheckpoint{
			Detail:   &checkpointDetail{HeadSHA: "abc123", BaseRefName: "main"},
			Snapshot: &checkpointSnapshot{HeadSHA: "abc123"},
		},
	})
	if err == nil {
		t.Fatal("expected restore failure after dirty prepare")
	}
	if !strings.Contains(err.Error(), "restore fixer owner token after dirty prepare") {
		t.Fatalf("error = %v, want restore failure surface", err)
	}
	// Marker remains absent: restore could not rewrite, and we must not pretend
	// MI with orphaned dirt is safe authority.
	got, readErr := worktreesafety.ReadFixerOwnerToken(wtPath)
	if readErr != nil {
		t.Fatalf("ReadFixerOwnerToken() error = %v", readErr)
	}
	if got != "" {
		t.Fatalf("ReadFixerOwnerToken() = %q, want empty when restore failed", got)
	}
}

// restoreFailAfterClearGit matches CreateWorktree revoke, then makes the private
// git dir unwritable so restoreFixerOwnerToken cannot rewrite the marker.
type restoreFailAfterClearGit struct {
	worktreePath string
	createCalls  int
	prepareCalls int
}

func (f *restoreFailAfterClearGit) CreateWorktree(_ context.Context, input CreateWorktreeInput) (CreateWorktreeResult, error) {
	f.createCalls++
	path := f.worktreePath
	if path == "" {
		path = filepath.Join(input.WorktreeRoot, "reviewer-worktree")
		f.worktreePath = path
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		return CreateWorktreeResult{}, err
	}
	if err := worktreesafety.ClearFixerOwnerToken(path); err != nil {
		return CreateWorktreeResult{}, err
	}
	gitDir := filepath.Join(path, ".git")
	if err := os.Chmod(gitDir, 0o555); err != nil {
		return CreateWorktreeResult{}, err
	}
	return CreateWorktreeResult{WorktreePath: path, Branch: input.Branch, HeadSHA: "abc123"}, nil
}

func (f *restoreFailAfterClearGit) PrepareWorktree(_ context.Context, input PrepareWorktreeInput) (PrepareWorktreeResult, error) {
	f.prepareCalls++
	return PrepareWorktreeResult{HeadSHA: input.ExpectedHeadSHA, Clean: false}, nil
}

func (f *restoreFailAfterClearGit) CleanupWorktree(_ context.Context, _ CleanupWorktreeInput) error {
	return nil
}

func (f *restoreFailAfterClearGit) ScrubReservedReviewerScratch(_ context.Context, _ string) error {
	return nil
}
