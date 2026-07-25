package fixer

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gitinfra "github.com/nexu-io/looper/internal/infra/git"
	"github.com/nexu-io/looper/internal/storage"
)

// TestRunPrepareWorktreeStepSameHeadDirtyAdopt_RealGitRestorePath models the
// 3412-shaped retry path: prior dirty prepare left checkpoint.Worktree unset, so
// shouldRebuildWorktree is false (no CleanupWorktree). CreateWorktree →
// RestoreWorktree reuses the existing managed path with dirt intact; Prepare
// returns Clean:false; tryAdoptDirtyFixerWorktree adopts same-head dirt.
// This is the restore path, not the rewind CleanupWorktree path (which only runs
// when checkpoint has Worktree with empty PreparedAt).
func TestRunPrepareWorktreeStepSameHeadDirtyAdopt_RealGitRestorePath(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	fx := newDirtyAdoptGitFixture(t)
	adapter := &realFixerGitAdapter{gateway: gitinfra.New(gitinfra.Options{GitPath: "git", Now: fx.now})}

	const (
		projectID = "project_dirty_adopt"
		branch    = "feature/fix-3412"
		prNumber  = int64(3412)
	)

	created, err := adapter.CreateWorktree(ctx, CreateWorktreeInput{
		ProjectID:    projectID,
		RepoPath:     fx.repoPath,
		WorktreeRoot: fx.worktreeRoot,
		Branch:       branch,
		BaseBranch:   "main",
		PRNumber:     prNumber,
		CheckoutMode: "detached",
	})
	if err != nil {
		t.Fatalf("CreateWorktree() error = %v", err)
	}
	if created.WorktreePath == "" || created.HeadSHA == "" {
		t.Fatalf("CreateWorktree() = %#v, want path and head", created)
	}
	headSHA := created.HeadSHA

	dirtyPath := filepath.Join(created.WorktreePath, "dirty-adopt.txt")
	const dirtyContents = "uncommitted agent output\n"
	if err := os.WriteFile(dirtyPath, []byte(dirtyContents), 0o644); err != nil {
		t.Fatalf("WriteFile dirty file: %v", err)
	}

	metadata := fmt.Sprintf(`{"worktreeRoot":%q}`, fx.worktreeRoot)
	runner := New(Options{
		Git:             adapter,
		AllowAutoCommit: true,
		Now:             fx.now,
	})

	// No Worktree on checkpoint: simulates prior dirty MI that never persisted path.
	checkpoint, err := runner.runPrepareWorktreeStep(ctx, stepInput{
		Project: storage.ProjectRecord{
			ID:           projectID,
			RepoPath:     fx.repoPath,
			MetadataJSON: &metadata,
		},
		Loop:     storage.LoopRecord{ID: "loop_dirty_adopt", Status: "running"},
		Repo:     "acme/looper",
		PRNumber: prNumber,
		Checkpoint: fixerCheckpoint{
			Detail: &checkpointDetail{
				HeadSHA:     headSHA,
				HeadRefName: branch,
				BaseRefName: "main",
			},
		},
	})
	if err != nil {
		t.Fatalf("runPrepareWorktreeStep() error = %v", err)
	}
	if checkpoint.Worktree == nil {
		t.Fatal("checkpoint.Worktree = nil, want adopted worktree")
	}
	if sameWorktreePath(checkpoint.Worktree.Path, created.WorktreePath) {
		// restored same managed path (CreateWorktree → RestoreWorktree)
	} else {
		t.Fatalf("Worktree.Path = %q, want same managed path as %q", checkpoint.Worktree.Path, created.WorktreePath)
	}
	if checkpoint.Worktree.HeadSHA != headSHA || checkpoint.Worktree.BaseHeadSHA != headSHA {
		t.Fatalf("Worktree head/base = %q/%q, want %q", checkpoint.Worktree.HeadSHA, checkpoint.Worktree.BaseHeadSHA, headSHA)
	}
	if checkpoint.Worktree.PreparedAt == "" {
		t.Fatal("Worktree.PreparedAt empty after adopt")
	}
	// Dirt must still be on disk at the adopted path (not cleaned by restore/prepare).
	adoptedDirtyPath := filepath.Join(checkpoint.Worktree.Path, "dirty-adopt.txt")
	if got := readFileString(t, adoptedDirtyPath); got != dirtyContents {
		t.Fatalf("dirty file after prepare/adopt = %q, want dirt intact %q", got, dirtyContents)
	}

	reconciled, err := runner.reconcileCommits(ctx, storage.ProjectRecord{
		ID:           projectID,
		RepoPath:     fx.repoPath,
		MetadataJSON: &metadata,
	}, checkpoint, "fix: adopt dirty worktree", storage.RunRecord{})
	if err != nil {
		t.Fatalf("reconcileCommits() error = %v", err)
	}
	if reconciled.ReconcileCommits == nil {
		t.Fatal("ReconcileCommits = nil")
	}
	if !reconciled.ReconcileCommits.CommittedByLoop {
		t.Fatalf("CommittedByLoop = false, want true")
	}
	if !reconciled.ReconcileCommits.WorkingTreeClean {
		t.Fatalf("WorkingTreeClean = false, want true")
	}
	if got := readFileString(t, adoptedDirtyPath); got != dirtyContents {
		t.Fatalf("dirty file after commit = %q, want content preserved %q", got, dirtyContents)
	}
	status := strings.TrimSpace(runGitOutput(t, checkpoint.Worktree.Path, "status", "--porcelain"))
	if status != "" {
		t.Fatalf("worktree still dirty after reconcile: %q", status)
	}
}

func sameWorktreePath(a, b string) bool {
	if a == b {
		return true
	}
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	if errA != nil || errB != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return ra == rb
}

// realFixerGitAdapter maps fixer.GitGateway onto gitinfra.Gateway the same way
// internal/runtime/scheduler.go fixerGitAdapter does (without disclosure stamping).
type realFixerGitAdapter struct {
	gateway *gitinfra.Gateway
}

func (a *realFixerGitAdapter) CreateWorktree(ctx context.Context, input CreateWorktreeInput) (CreateWorktreeResult, error) {
	worktree, err := a.gateway.CreateWorktree(ctx, gitinfra.CreateWorktreeInput{
		ProjectID:         input.ProjectID,
		RepoPath:          input.RepoPath,
		WorktreeRoot:      input.WorktreeRoot,
		Branch:            input.Branch,
		BaseBranch:        input.BaseBranch,
		PRNumber:          input.PRNumber,
		ProtectedBranches: input.ProtectedBranches,
		CheckoutMode:      gitinfra.CheckoutMode(input.CheckoutMode),
	})
	if err != nil {
		return CreateWorktreeResult{}, err
	}
	head := ""
	if worktree.HeadSHA != nil {
		head = *worktree.HeadSHA
	}
	return CreateWorktreeResult{WorktreePath: worktree.WorktreePath, Branch: worktree.Branch, HeadSHA: head}, nil
}

func (a *realFixerGitAdapter) PrepareWorktree(ctx context.Context, input PrepareWorktreeInput) (PrepareWorktreeResult, error) {
	result, err := a.gateway.PrepareWorktree(ctx, gitinfra.PrepareWorktreeInput{
		RepoPath:        input.RepoPath,
		WorktreeRoot:    input.WorktreeRoot,
		WorktreePath:    input.WorktreePath,
		Branch:          input.Branch,
		ExpectedHeadSHA: input.ExpectedHeadSHA,
		Remote:          input.Remote,
	})
	if err != nil {
		return PrepareWorktreeResult{}, err
	}
	return PrepareWorktreeResult{HeadSHA: result.HeadSHA, Clean: result.Clean}, nil
}

func (a *realFixerGitAdapter) InspectHead(ctx context.Context, input InspectHeadInput) (InspectHeadResult, error) {
	result, err := a.gateway.InspectHead(ctx, gitinfra.InspectHeadInput{
		RepoPath:     input.RepoPath,
		WorktreeRoot: input.WorktreeRoot,
		WorktreePath: input.WorktreePath,
		BaseRef:      input.BaseRef,
	})
	if err != nil {
		return InspectHeadResult{}, err
	}
	return InspectHeadResult{
		HeadSHA:               result.HeadSHA,
		NewCommitSHAs:         result.NewCommitSHAs,
		HasUncommittedChanges: result.HasUncommittedChanges,
		ChangedFiles:          result.ChangedFiles,
	}, nil
}

func (a *realFixerGitAdapter) Commit(ctx context.Context, input CommitInput) (CommitResult, error) {
	result, err := a.gateway.Commit(ctx, gitinfra.CommitInput{
		RepoPath:     input.RepoPath,
		WorktreeRoot: input.WorktreeRoot,
		WorktreePath: input.WorktreePath,
		Message:      input.Message,
	})
	if err != nil {
		return CommitResult{}, err
	}
	return CommitResult{CommitSHA: result.CommitSHA}, nil
}

func (a *realFixerGitAdapter) Push(ctx context.Context, input PushInput) error {
	return a.gateway.Push(ctx, gitinfra.PushInput{
		RepoPath:              input.RepoPath,
		WorktreeRoot:          input.WorktreeRoot,
		WorktreePath:          input.WorktreePath,
		Branch:                input.Branch,
		Remote:                input.Remote,
		ExpectedRemoteHeadSHA: input.ExpectedRemoteHeadSHA,
		ProtectedBranches:     input.ProtectedBranches,
	})
}

func (a *realFixerGitAdapter) FetchBranch(ctx context.Context, repoPath, remote, branch string) error {
	return a.gateway.FetchBranch(ctx, repoPath, remote, branch)
}

func (a *realFixerGitAdapter) MergeBaseIntoWorktree(ctx context.Context, input MergeBaseInput) (MergeBaseResult, error) {
	res, err := a.gateway.MergeBaseIntoWorktree(ctx, gitinfra.MergeBaseInput{
		WorktreePath: input.WorktreePath,
		Remote:       input.Remote,
		BaseBranch:   input.BaseBranch,
	})
	if err != nil {
		return MergeBaseResult{}, err
	}
	return MergeBaseResult{AlreadyUpToDate: res.AlreadyUpToDate, Conflicted: res.Conflicted}, nil
}

func (a *realFixerGitAdapter) IsAncestor(ctx context.Context, repoPath, ancestor, descendant string) (bool, error) {
	return a.gateway.IsAncestor(ctx, repoPath, ancestor, descendant)
}

func (a *realFixerGitAdapter) CleanupWorktree(ctx context.Context, input CleanupWorktreeInput) error {
	return a.gateway.CleanupWorktree(ctx, gitinfra.CleanupWorktreeInput{
		ProjectID:         input.ProjectID,
		RepoPath:          input.RepoPath,
		WorktreeRoot:      input.WorktreeRoot,
		WorktreePath:      input.WorktreePath,
		Branch:            input.Branch,
		ProtectedBranches: input.ProtectedBranches,
	})
}

type dirtyAdoptGitFixture struct {
	rootDir      string
	repoPath     string
	remotePath   string
	worktreeRoot string
	now          func() time.Time
}

func newDirtyAdoptGitFixture(t *testing.T) *dirtyAdoptGitFixture {
	t.Helper()
	rootDir := t.TempDir()
	fx := &dirtyAdoptGitFixture{
		rootDir:      rootDir,
		repoPath:     filepath.Join(rootDir, "repo"),
		remotePath:   filepath.Join(rootDir, "remote.git"),
		worktreeRoot: filepath.Join(rootDir, "worktrees"),
		now:          func() time.Time { return time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC) },
	}
	if err := os.MkdirAll(fx.repoPath, 0o755); err != nil {
		t.Fatalf("MkdirAll repo: %v", err)
	}
	if err := os.MkdirAll(fx.remotePath, 0o755); err != nil {
		t.Fatalf("MkdirAll remote: %v", err)
	}
	if err := os.MkdirAll(fx.worktreeRoot, 0o755); err != nil {
		t.Fatalf("MkdirAll worktree root: %v", err)
	}

	runGitOutput(t, fx.repoPath, "init", "-b", "main")
	runGitOutput(t, fx.remotePath, "init", "--bare")
	runGitOutput(t, fx.repoPath, "config", "user.email", "test@example.com")
	runGitOutput(t, fx.repoPath, "config", "user.name", "Looper Test")
	runGitOutput(t, fx.repoPath, "remote", "add", "origin", fx.remotePath)
	if err := os.WriteFile(filepath.Join(fx.repoPath, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("WriteFile README: %v", err)
	}
	runGitOutput(t, fx.repoPath, "add", "README.md")
	runGitOutput(t, fx.repoPath, "commit", "-m", "init")
	runGitOutput(t, fx.repoPath, "push", "-u", "origin", "main")

	const branch = "feature/fix-3412"
	runGitOutput(t, fx.repoPath, "checkout", "-b", branch)
	if err := os.WriteFile(filepath.Join(fx.repoPath, "fix.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatalf("WriteFile fix.txt: %v", err)
	}
	runGitOutput(t, fx.repoPath, "add", "fix.txt")
	runGitOutput(t, fx.repoPath, "commit", "-m", "feature")
	runGitOutput(t, fx.repoPath, "push", "-u", "origin", branch)
	runGitOutput(t, fx.repoPath, "checkout", "main")
	return fx
}

func runGitOutput(t *testing.T, cwd string, args ...string) string {
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

func readFileString(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", path, err)
	}
	return string(b)
}
