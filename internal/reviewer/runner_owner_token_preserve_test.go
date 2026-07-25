package reviewer

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
	"github.com/nexu-io/looper/internal/worktreesafety"
)

// Lifecycle: resumed reviewer finds intervening fixer marker, re-prepare CreateWorktree
// clears the marker, Prepare returns RemoteHeadChangedError before dirt inspection,
// and success/stale cleanup must not force-remove the fixer-owned path.
func TestProcessClaimedItemRemoteHeadChangedPreservesFixerDirtAndToken(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	fixture.repos.Locks.SetNow(fixture.now)
	ctx := context.Background()
	repo := "acme/looper"
	prNumber := int64(42)
	loopTarget := "pr:acme/looper:42"
	projectID := "project_1"
	loopID := "loop_resume_remote_head_preserves_fixer"
	queueID := "queue_resume_remote_head_preserves_fixer"
	nowISO := fixture.nowISO()
	lockKey := storage.PullRequestLockKey(projectID, repo, prNumber)

	wtRoot := filepath.Join(t.TempDir(), "worktrees")
	wtPath := filepath.Join(wtRoot, "wt-42")
	if err := os.MkdirAll(wtPath, 0o755); err != nil {
		t.Fatalf("MkdirAll worktree: %v", err)
	}
	dirtyFile := filepath.Join(wtPath, "partial-fix.go")
	if err := os.WriteFile(dirtyFile, []byte("package partial\n"), 0o644); err != nil {
		t.Fatalf("WriteFile partial dirt: %v", err)
	}
	const token = "fixer:loop_intervening:run_1:owns-path"
	if err := worktreesafety.WriteFixerOwnerToken(wtPath, token); err != nil {
		t.Fatalf("WriteFixerOwnerToken: %v", err)
	}
	projectMeta := fmt.Sprintf(`{"worktreeRoot":%q}`, wtRoot)
	if err := fixture.repos.Projects.Upsert(ctx, storage.ProjectRecord{
		ID: projectID, Name: "Looper", RepoPath: filepath.Join(t.TempDir(), "repo"),
		BaseBranch: stringPtr("main"), MetadataJSON: &projectMeta, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := fixture.repos.Loops.Upsert(ctx, storage.LoopRecord{
		ID: loopID, Seq: 1, ProjectID: projectID, Type: "reviewer",
		TargetType: "pull_request", TargetID: &loopTarget, Repo: &repo, PRNumber: &prNumber,
		Status: "queued", CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	checkpointJSON := mustMarshalJSON(reviewerCheckpoint{
		Detail:   &checkpointDetail{Title: "Review me", State: "OPEN", HeadSHA: "abc123", BaseRefName: "main", ReviewRequests: []string{"octocat"}},
		Snapshot: &checkpointSnapshot{HeadSHA: "abc123"},
		Worktree: &checkpointWorktree{Path: wtPath, Branch: "pr-42-head", BaseBranch: "main", PreparedAt: nowISO},
		PendingReview: &pendingReviewCheckpoint{
			HeadSHA: "abc123", IdempotencyKey: "idem", Event: reviewEventAgentNative, Summary: "No actionable findings",
		},
		ResumePolicy:   "advance_from_checkpoint",
		ClaimedLockKey: lockKey,
	})
	if err := fixture.repos.Runs.Upsert(ctx, storage.RunRecord{
		ID: "run_failed_thread_res_remote_head", LoopID: loopID, Status: "failed",
		CurrentStep: stringPtr(string(stepThreadResolution)), LastCompletedStep: stringPtr(string(stepWorktree)),
		CheckpointJSON: &checkpointJSON, StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}
	queue := storage.QueueItemRecord{
		ID: queueID, ProjectID: &projectID, LoopID: &loopID, Type: "reviewer",
		TargetType: "pull_request", TargetID: loopTarget, Repo: &repo, PRNumber: &prNumber,
		DedupeKey: "reviewer:resume-remote-head-preserves-fixer", Priority: storage.QueuePriorityReviewer,
		Status: "running", AvailableAt: nowISO, LockKey: &lockKey, MaxAttempts: 3,
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	if err := fixture.repos.Queue.Upsert(ctx, queue); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	git := &fakeGitGateway{
		worktreePath: wtPath,
		prepareErr:   &gitinfra.RemoteHeadChangedError{Branch: "refs/pull/42/head", ExpectedHeadSHA: "abc123", ActualHeadSHA: "def456"},
	}
	runner := New(Options{
		DB: fixture.coordinator.DB(), Repos: fixture.repos,
		GitHub: &fakeGitHubGateway{author: "octocat", reviewRequests: []string{"octocat"}},
		Git:    git, Logger: fixture.logger, Now: fixture.now,
		LoopConfig: testReviewerLoopConfig(),
	})
	result, err := runner.ProcessClaimedItem(ctx, queue)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "skipped" || !contains(result.Summary, "Remote head changed") {
		t.Fatalf("result = %#v, want stale remote-head skip", result)
	}
	if len(git.prepareCalls) == 0 {
		t.Fatal("expected PrepareWorktree after intervening fixer marker")
	}
	if len(git.cleanupCalls) != 0 {
		t.Fatalf("cleanupCalls = %d, want 0 (must not force-remove fixer dirt after pre-inspect failure)", len(git.cleanupCalls))
	}
	got, err := worktreesafety.ReadFixerOwnerToken(wtPath)
	if err != nil {
		t.Fatalf("ReadFixerOwnerToken() error = %v", err)
	}
	if got != token {
		t.Fatalf("ReadFixerOwnerToken() = %q, want %q restored after pre-inspect prepare failure", got, token)
	}
	gotDirt, err := os.ReadFile(dirtyFile)
	if err != nil || string(gotDirt) != "package partial\n" {
		t.Fatalf("partial dirt = %q err=%v, want preserved", gotDirt, err)
	}
}

// Lifecycle: final-attempt prepare fetch failure after CreateWorktree cleared the
// fixer marker must restore ownership and skip terminal cleanup.
func TestProcessClaimedItemTerminalPrepareErrorPreservesFixerDirtAndToken(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	fixture.repos.Locks.SetNow(fixture.now)
	ctx := context.Background()
	repo := "acme/looper"
	prNumber := int64(42)
	loopTarget := "pr:acme/looper:42"
	projectID := "project_1"
	loopID := "loop_resume_prepare_terminal_preserves_fixer"
	queueID := "queue_resume_prepare_terminal_preserves_fixer"
	nowISO := fixture.nowISO()
	lockKey := storage.PullRequestLockKey(projectID, repo, prNumber)

	wtRoot := filepath.Join(t.TempDir(), "worktrees")
	wtPath := filepath.Join(wtRoot, "wt-42")
	if err := os.MkdirAll(wtPath, 0o755); err != nil {
		t.Fatalf("MkdirAll worktree: %v", err)
	}
	dirtyFile := filepath.Join(wtPath, "partial-fix.go")
	if err := os.WriteFile(dirtyFile, []byte("package partial\n"), 0o644); err != nil {
		t.Fatalf("WriteFile partial dirt: %v", err)
	}
	const token = "fixer:loop_intervening:run_1:owns-path"
	if err := worktreesafety.WriteFixerOwnerToken(wtPath, token); err != nil {
		t.Fatalf("WriteFixerOwnerToken: %v", err)
	}
	projectMeta := fmt.Sprintf(`{"worktreeRoot":%q}`, wtRoot)
	if err := fixture.repos.Projects.Upsert(ctx, storage.ProjectRecord{
		ID: projectID, Name: "Looper", RepoPath: filepath.Join(t.TempDir(), "repo"),
		BaseBranch: stringPtr("main"), MetadataJSON: &projectMeta, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := fixture.repos.Loops.Upsert(ctx, storage.LoopRecord{
		ID: loopID, Seq: 1, ProjectID: projectID, Type: "reviewer",
		TargetType: "pull_request", TargetID: &loopTarget, Repo: &repo, PRNumber: &prNumber,
		Status: "queued", CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	checkpointJSON := mustMarshalJSON(reviewerCheckpoint{
		Detail:         &checkpointDetail{Title: "Review me", State: "OPEN", HeadSHA: "abc123", BaseRefName: "main", ReviewRequests: []string{"octocat"}},
		Snapshot:       &checkpointSnapshot{HeadSHA: "abc123"},
		Worktree:       &checkpointWorktree{Path: wtPath, Branch: "pr-42-head", BaseBranch: "main", PreparedAt: nowISO},
		ResumePolicy:   "advance_from_checkpoint",
		ClaimedLockKey: lockKey,
	})
	if err := fixture.repos.Runs.Upsert(ctx, storage.RunRecord{
		ID: "run_failed_thread_res_prepare_err", LoopID: loopID, Status: "failed",
		CurrentStep: stringPtr(string(stepThreadResolution)), LastCompletedStep: stringPtr(string(stepWorktree)),
		CheckpointJSON: &checkpointJSON, StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}
	queue := storage.QueueItemRecord{
		ID: queueID, ProjectID: &projectID, LoopID: &loopID, Type: "reviewer",
		TargetType: "pull_request", TargetID: loopTarget, Repo: &repo, PRNumber: &prNumber,
		DedupeKey: "reviewer:resume-prepare-terminal-preserves-fixer", Priority: storage.QueuePriorityReviewer,
		Status: "running", AvailableAt: nowISO, LockKey: &lockKey, MaxAttempts: 1, // final attempt → terminal
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	if err := fixture.repos.Queue.Upsert(ctx, queue); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	git := &fakeGitGateway{
		worktreePath: wtPath,
		prepareErr:   fmt.Errorf("error: cannot run ssh: No such file or directory\nfatal: unable to fork"),
	}
	runner := New(Options{
		DB: fixture.coordinator.DB(), Repos: fixture.repos,
		GitHub: &fakeGitHubGateway{author: "octocat", reviewRequests: []string{"octocat"}},
		Git:    git, Logger: fixture.logger, Now: fixture.now,
		LoopConfig: testReviewerLoopConfig(),
	})
	result, err := runner.ProcessClaimedItem(ctx, queue)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "failed" {
		t.Fatalf("result.Status = %q, want failed", result.Status)
	}
	if len(git.cleanupCalls) != 0 {
		t.Fatalf("cleanupCalls = %d, want 0 on terminal prepare failure with unprepared path", len(git.cleanupCalls))
	}
	got, err := worktreesafety.ReadFixerOwnerToken(wtPath)
	if err != nil {
		t.Fatalf("ReadFixerOwnerToken() error = %v", err)
	}
	if got != token {
		t.Fatalf("ReadFixerOwnerToken() = %q, want %q restored after prepare failure", got, token)
	}
	gotDirt, err := os.ReadFile(dirtyFile)
	if err != nil || string(gotDirt) != "package partial\n" {
		t.Fatalf("partial dirt = %q err=%v, want preserved", gotDirt, err)
	}
}

func TestCleanupReviewerWorktreeIfTerminalSkipsUnpreparedAndFixerOwned(t *testing.T) {
	t.Parallel()

	wtPath := filepath.Join(t.TempDir(), "wt-42")
	if err := os.MkdirAll(wtPath, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	git := &fakeGitGateway{}
	runner := New(Options{Git: git})
	project := storage.ProjectRecord{ID: "project_1", RepoPath: t.TempDir(), BaseBranch: stringPtr("main")}

	// Unprepared path: never cleanup (prepare may not have inspected dirt).
	checkpoint := &reviewerCheckpoint{
		Worktree: &checkpointWorktree{Path: wtPath, Branch: "pr-42-head"},
	}
	runner.cleanupReviewerWorktreeIfTerminal(context.Background(), project, checkpoint)
	if len(git.cleanupCalls) != 0 {
		t.Fatalf("cleanupCalls = %d, want 0 for unprepared worktree", len(git.cleanupCalls))
	}

	// Prepared + fixer marker: still skip (intervening fixer ownership).
	const token = "fixer:loop:run:token"
	if err := worktreesafety.WriteFixerOwnerToken(wtPath, token); err != nil {
		t.Fatalf("WriteFixerOwnerToken: %v", err)
	}
	checkpoint.Worktree.PreparedAt = "2026-04-11T12:00:00.000Z"
	runner.cleanupReviewerWorktreeIfTerminal(context.Background(), project, checkpoint)
	if len(git.cleanupCalls) != 0 {
		t.Fatalf("cleanupCalls = %d, want 0 for fixer-owned prepared worktree", len(git.cleanupCalls))
	}

	// Prepared without marker: cleanup proceeds.
	if err := worktreesafety.ClearFixerOwnerToken(wtPath); err != nil {
		t.Fatalf("ClearFixerOwnerToken: %v", err)
	}
	runner.cleanupReviewerWorktreeIfTerminal(context.Background(), project, checkpoint)
	if len(git.cleanupCalls) != 1 {
		t.Fatalf("cleanupCalls = %d, want 1 for prepared reviewer-owned worktree", len(git.cleanupCalls))
	}
	if checkpoint.Worktree.CleanedAt == "" {
		t.Fatal("CleanedAt empty after prepared cleanup")
	}
}

// Fresh reviewer (no checkpoint worktree) still hits an interrupted fixer's dirty
// checkout at the shared detached PR path. Capture the candidate marker before
// CreateWorktree clears it so dirty prepare can restore adopt authority.
func TestRunPrepareWorktreeStepFreshReviewerRestoresCandidateFixerTokenOnDirty(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	repoPath := t.TempDir()
	worktreeRoot := filepath.Join(t.TempDir(), "worktrees")
	projectID := "project_1"
	prNumber := int64(42)
	wtPath := gitinfra.DetachedPRWorktreePath(worktreeRoot, projectID, prNumber)
	if wtPath == "" {
		t.Fatal("DetachedPRWorktreePath empty")
	}
	if err := os.MkdirAll(wtPath, 0o755); err != nil {
		t.Fatalf("MkdirAll worktree: %v", err)
	}
	dirtyFile := filepath.Join(wtPath, "partial-fix.go")
	if err := os.WriteFile(dirtyFile, []byte("package partial\n"), 0o644); err != nil {
		t.Fatalf("WriteFile partial dirt: %v", err)
	}
	const token = "fixer:loop_interrupted:run_1:owns-shared-path"
	if err := worktreesafety.WriteFixerOwnerToken(wtPath, token); err != nil {
		t.Fatalf("WriteFixerOwnerToken: %v", err)
	}
	metadata := fmt.Sprintf(`{"worktreeRoot":%q}`, worktreeRoot)
	clean := false
	git := &fakeGitGateway{worktreePath: wtPath, prepareClean: &clean}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: &fakeGitHubGateway{}, Git: git, Logger: fixture.logger, Now: fixture.now})

	_, err := runner.runPrepareWorktreeStep(context.Background(), stepInput{
		Project:  storage.ProjectRecord{ID: projectID, RepoPath: repoPath, MetadataJSON: &metadata},
		Repo:     "acme/looper",
		PRNumber: prNumber,
		Checkpoint: reviewerCheckpoint{
			// No Worktree: fresh reviewer claim.
			Detail:   &checkpointDetail{HeadSHA: "abc123", BaseRefName: "main"},
			Snapshot: &checkpointSnapshot{HeadSHA: "abc123"},
		},
	})
	if err == nil {
		t.Fatal("expected dirty prepare error")
	}
	var loopErr *loopError
	if !errors.As(err, &loopErr) || loopErr.kind != FailureManualIntervention {
		t.Fatalf("error = %v, want dirty MI loopError", err)
	}
	if len(git.createCalls) != 1 {
		t.Fatalf("createCalls = %d, want 1", len(git.createCalls))
	}
	got, readErr := worktreesafety.ReadFixerOwnerToken(wtPath)
	if readErr != nil {
		t.Fatalf("ReadFixerOwnerToken() error = %v", readErr)
	}
	if got != token {
		t.Fatalf("ReadFixerOwnerToken() = %q, want %q restored after dirty prepare without checkpoint worktree", got, token)
	}
	gotDirt, err := os.ReadFile(dirtyFile)
	if err != nil || string(gotDirt) != "package partial\n" {
		t.Fatalf("partial dirt = %q err=%v, want preserved", gotDirt, err)
	}
}

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
