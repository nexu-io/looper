package reviewer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	gitinfra "github.com/nexu-io/looper/internal/infra/git"
	"github.com/nexu-io/looper/internal/storage"
	"github.com/nexu-io/looper/internal/worktreesafety"
)

// Real-gateway invariant: reviewer re-prepare against a fixer-owned dirty path
// hits RemoteHeadChangedError before status inspection. CreateWorktree revokes
// the marker; runPrepare must restore provenance, leave dirt intact, and never
// call CleanupWorktree.
func TestRunPrepareWorktreeStepRealGatewayRemoteHeadChangedPreservesFixerDirt(t *testing.T) {
	t.Parallel()

	const prNumber int64 = 42
	const projectID = "project_real_reviewer_preserve"
	branch := "feature/review-42"
	root, _, repoPath, oldHead := setupRealRepoWithPRHead(t, branch, prNumber)
	worktreeRoot := filepath.Join(root, "worktrees")
	if err := os.MkdirAll(worktreeRoot, 0o755); err != nil {
		t.Fatalf("MkdirAll worktreeRoot: %v", err)
	}

	gateway := gitinfra.New(gitinfra.Options{GitPath: "git"})
	created, err := gateway.CreateWorktree(context.Background(), gitinfra.CreateWorktreeInput{
		ProjectID: projectID, RepoPath: repoPath, WorktreeRoot: worktreeRoot,
		Branch: "pr-42-head", BaseBranch: "main", PRNumber: prNumber,
		CheckoutMode: gitinfra.CheckoutModeDetached,
	})
	if err != nil {
		t.Fatalf("CreateWorktree() error = %v", err)
	}
	wtPath := created.WorktreePath

	// Advance remote PR head so prepare reports RemoteHeadChangedError before dirt check.
	mustRunGit(t, repoPath, "checkout", branch)
	if err := os.WriteFile(filepath.Join(repoPath, "advance.txt"), []byte("new head\n"), 0o644); err != nil {
		t.Fatalf("WriteFile advance: %v", err)
	}
	mustRunGit(t, repoPath, "add", "advance.txt")
	mustRunGit(t, repoPath, "commit", "-m", "advance pr head")
	newHead := mustRunGit(t, repoPath, "rev-parse", "HEAD")
	mustRunGit(t, repoPath, "push", "origin", fmt.Sprintf("HEAD:refs/pull/%d/head", prNumber))
	mustRunGit(t, repoPath, "checkout", "main")
	if newHead == oldHead {
		t.Fatal("expected advanced PR head")
	}

	// Intervening fixer dirt + ownership after CreateWorktree (re-stamped as fixer would).
	dirtyFile := filepath.Join(wtPath, "partial-agent-edit.txt")
	if err := os.WriteFile(dirtyFile, []byte("keep me\n"), 0o644); err != nil {
		t.Fatalf("WriteFile dirty: %v", err)
	}
	const token = "fixer:loop_real:run_1:owns-path"
	if err := worktreesafety.WriteFixerOwnerToken(wtPath, token); err != nil {
		t.Fatalf("WriteFixerOwnerToken: %v", err)
	}

	adapter := &countingRealGitGateway{inner: gateway}
	runner := New(Options{Git: adapter})
	metadata := fmt.Sprintf(`{"worktreeRoot":%q}`, worktreeRoot)
	checkpoint, prepErr := runner.runPrepareWorktreeStep(context.Background(), stepInput{
		Project:  storage.ProjectRecord{ID: projectID, RepoPath: repoPath, BaseBranch: stringPtr("main"), MetadataJSON: &metadata},
		Repo:     "acme/looper",
		PRNumber: prNumber,
		Checkpoint: reviewerCheckpoint{
			Detail:   &checkpointDetail{HeadSHA: oldHead, BaseRefName: "main"},
			Snapshot: &checkpointSnapshot{HeadSHA: oldHead},
			// PreparedAt set as if prior prepare succeeded; marker forces re-prepare.
			Worktree: &checkpointWorktree{Path: wtPath, Branch: "pr-42-head", BaseBranch: "main", PreparedAt: "2026-04-11T12:00:00.000Z"},
		},
	})
	if prepErr != nil {
		t.Fatalf("runPrepareWorktreeStep() error = %v, want nil with stale skip", prepErr)
	}
	if checkpoint.SkipKind != "stale" || !contains(checkpoint.SkipReason, "Remote head changed") {
		t.Fatalf("checkpoint = %#v, want stale remote-head skip", checkpoint)
	}
	if checkpoint.Worktree == nil || checkpoint.Worktree.PreparedAt != "" {
		t.Fatalf("Worktree = %#v, want path kept with empty PreparedAt", checkpoint.Worktree)
	}
	if adapter.createCalls < 1 {
		t.Fatalf("createCalls = %d, want >= 1 (reclaim after fixer marker)", adapter.createCalls)
	}
	if adapter.prepareCalls < 1 {
		t.Fatalf("prepareCalls = %d, want >= 1", adapter.prepareCalls)
	}
	if adapter.cleanupCalls != 0 {
		t.Fatalf("cleanupCalls = %d, want 0", adapter.cleanupCalls)
	}
	got, err := worktreesafety.ReadFixerOwnerToken(wtPath)
	if err != nil {
		t.Fatalf("ReadFixerOwnerToken() error = %v", err)
	}
	if got != token {
		t.Fatalf("ReadFixerOwnerToken() = %q, want %q restored after pre-inspect failure", got, token)
	}
	dirtyBytes, err := os.ReadFile(dirtyFile)
	if err != nil || string(dirtyBytes) != "keep me\n" {
		t.Fatalf("dirty marker = %q err=%v, want preserved", dirtyBytes, err)
	}
}

// Real-gateway contract: leftover untracked reserved reviewer scratch must not
// force manual_intervention. Ordinary untracked dirt still does.
func TestRunPrepareWorktreeStepRealGatewayScrubsReservedReviewerScratch(t *testing.T) {
	t.Parallel()

	const prNumber int64 = 77
	const projectID = "project_real_reviewer_scratch"
	branch := "feature/review-77"
	root, _, repoPath, headSHA := setupRealRepoWithPRHead(t, branch, prNumber)
	worktreeRoot := filepath.Join(root, "worktrees")
	if err := os.MkdirAll(worktreeRoot, 0o755); err != nil {
		t.Fatalf("MkdirAll worktreeRoot: %v", err)
	}

	gateway := gitinfra.New(gitinfra.Options{GitPath: "git"})
	created, err := gateway.CreateWorktree(context.Background(), gitinfra.CreateWorktreeInput{
		ProjectID: projectID, RepoPath: repoPath, WorktreeRoot: worktreeRoot,
		Branch: "pr-77-head", BaseBranch: "main", PRNumber: prNumber,
		CheckoutMode: gitinfra.CheckoutModeDetached,
	})
	if err != nil {
		t.Fatalf("CreateWorktree() error = %v", err)
	}
	wtPath := created.WorktreePath

	scratch := filepath.Join(wtPath, ".looper-review-3503.json")
	if err := os.WriteFile(scratch, []byte(`{"leftover":true}`+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile scratch: %v", err)
	}

	adapter := &countingRealGitGateway{inner: gateway}
	runner := New(Options{Git: adapter})
	metadata := fmt.Sprintf(`{"worktreeRoot":%q}`, worktreeRoot)
	checkpoint, prepErr := runner.runPrepareWorktreeStep(context.Background(), stepInput{
		Project:  storage.ProjectRecord{ID: projectID, RepoPath: repoPath, BaseBranch: stringPtr("main"), MetadataJSON: &metadata},
		Repo:     "acme/looper",
		PRNumber: prNumber,
		Checkpoint: reviewerCheckpoint{
			Detail:   &checkpointDetail{HeadSHA: headSHA, BaseRefName: "main"},
			Snapshot: &checkpointSnapshot{HeadSHA: headSHA},
			Worktree: &checkpointWorktree{Path: wtPath, Branch: "pr-77-head", BaseBranch: "main"},
		},
	})
	if prepErr != nil {
		t.Fatalf("runPrepareWorktreeStep() error = %v, want clean prepare after scrub", prepErr)
	}
	if checkpoint.Worktree == nil || checkpoint.Worktree.PreparedAt == "" {
		t.Fatalf("Worktree = %#v, want prepared path", checkpoint.Worktree)
	}
	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Fatalf("reserved scratch still present after prepare: err=%v", err)
	}
}

func TestRunPrepareWorktreeStepRealGatewayKeepsOrdinaryUntrackedDirt(t *testing.T) {
	t.Parallel()

	const prNumber int64 = 78
	const projectID = "project_real_reviewer_ordinary_dirt"
	branch := "feature/review-78"
	root, _, repoPath, headSHA := setupRealRepoWithPRHead(t, branch, prNumber)
	worktreeRoot := filepath.Join(root, "worktrees")
	if err := os.MkdirAll(worktreeRoot, 0o755); err != nil {
		t.Fatalf("MkdirAll worktreeRoot: %v", err)
	}

	gateway := gitinfra.New(gitinfra.Options{GitPath: "git"})
	created, err := gateway.CreateWorktree(context.Background(), gitinfra.CreateWorktreeInput{
		ProjectID: projectID, RepoPath: repoPath, WorktreeRoot: worktreeRoot,
		Branch: "pr-78-head", BaseBranch: "main", PRNumber: prNumber,
		CheckoutMode: gitinfra.CheckoutModeDetached,
	})
	if err != nil {
		t.Fatalf("CreateWorktree() error = %v", err)
	}
	wtPath := created.WorktreePath

	ordinary := filepath.Join(wtPath, "agent-notes.txt")
	if err := os.WriteFile(ordinary, []byte("real dirt\n"), 0o644); err != nil {
		t.Fatalf("WriteFile ordinary: %v", err)
	}

	adapter := &countingRealGitGateway{inner: gateway}
	runner := New(Options{Git: adapter})
	metadata := fmt.Sprintf(`{"worktreeRoot":%q}`, worktreeRoot)
	_, prepErr := runner.runPrepareWorktreeStep(context.Background(), stepInput{
		Project:  storage.ProjectRecord{ID: projectID, RepoPath: repoPath, BaseBranch: stringPtr("main"), MetadataJSON: &metadata},
		Repo:     "acme/looper",
		PRNumber: prNumber,
		Checkpoint: reviewerCheckpoint{
			Detail:   &checkpointDetail{HeadSHA: headSHA, BaseRefName: "main"},
			Snapshot: &checkpointSnapshot{HeadSHA: headSHA},
			Worktree: &checkpointWorktree{Path: wtPath, Branch: "pr-78-head", BaseBranch: "main"},
		},
	})
	if prepErr == nil {
		t.Fatal("runPrepareWorktreeStep() error = nil, want manual_intervention for ordinary dirt")
	}
	if !contains(prepErr.Error(), "worktree is dirty") && !contains(prepErr.Error(), "manual intervention") {
		t.Fatalf("error = %v, want dirty/manual intervention", prepErr)
	}
	if got, err := os.ReadFile(ordinary); err != nil || string(got) != "real dirt\n" {
		t.Fatalf("ordinary dirt = %q err=%v, want preserved", got, err)
	}
}
