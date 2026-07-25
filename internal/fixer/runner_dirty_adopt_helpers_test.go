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

	gitinfra "github.com/nexu-io/looper/internal/infra/git"
	"github.com/nexu-io/looper/internal/storage"
	"github.com/nexu-io/looper/internal/worktreesafety"
)

// Shared fixtures for same-head dirty adopt / prepare-error recovery.
// Kept out of runner_test.go and split across recovery test files so no single
// runner *_test.go in this PR exceeds the AGENTS.md growth threshold.

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

func (f dirtyAdoptFixture) seedOwnerToken(t *testing.T, token string) {
	t.Helper()
	if err := worktreesafety.WriteFixerOwnerToken(f.wtPath, token); err != nil {
		t.Fatalf("WriteFixerOwnerToken: %v", err)
	}
}

func (f dirtyAdoptFixture) rewindCheckpoint() fixerCheckpoint {
	// Prior prepare stamped ownership; rewind clears PreparedAt but keeps Path + OwnerToken.
	const token = "fixer:loop_1:run_prior:prepared"
	return fixerCheckpoint{
		Detail: &checkpointDetail{HeadSHA: f.headSHA, HeadRefName: f.branch, BaseRefName: "main"},
		Worktree: &checkpointWorktree{
			Path:       f.wtPath,
			Branch:     f.branch,
			OwnerToken: token, // PreparedAt cleared by rewind; ownership retained
		},
	}
}

func (f dirtyAdoptFixture) rewindCheckpointWithDiskOwnership(t *testing.T) fixerCheckpoint {
	t.Helper()
	cp := f.rewindCheckpoint()
	f.seedOwnerToken(t, cp.Worktree.OwnerToken)
	return cp
}

func (f dirtyAdoptFixture) step(loop storage.LoopRecord, git GitGateway) stepInput {
	// Tests that only need path preservation (not adopt) may omit disk ownership.
	return stepInput{
		Project:    f.project(),
		Loop:       loop,
		Repo:       "acme/looper",
		PRNumber:   42,
		Checkpoint: f.rewindCheckpoint(),
	}
}

func (f dirtyAdoptFixture) stepWithOwnership(t *testing.T, loop storage.LoopRecord, git GitGateway) stepInput {
	t.Helper()
	return stepInput{
		Project:    f.project(),
		Loop:       loop,
		Repo:       "acme/looper",
		PRNumber:   42,
		Checkpoint: f.rewindCheckpointWithDiskOwnership(t),
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

func tryRunGit(cwd string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = cwd
	return cmd.Run()
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
