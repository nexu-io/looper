package fixer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/nexu-io/looper/internal/storage"
)

func TestCleanupFixerWorktreeIfTerminalSkipsUnpreparedWorktree(t *testing.T) {
	t.Parallel()

	wtPath := filepath.Join(t.TempDir(), "wt-42")
	if err := os.MkdirAll(wtPath, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	git := &fakeGitGateway{}
	runner := New(Options{Git: git})
	checkpoint := &fixerCheckpoint{
		Worktree: &checkpointWorktree{Path: wtPath, Branch: "feature/fix-42"}, // PreparedAt empty
	}
	runner.cleanupFixerWorktreeIfTerminal(context.Background(), storage.ProjectRecord{
		ID: "project_1", RepoPath: t.TempDir(), BaseBranch: stringPtr("main"),
	}, checkpoint)
	if len(git.cleanupCalls) != 0 {
		t.Fatalf("len(git.cleanupCalls) = %d, want 0 for unprepared worktree", len(git.cleanupCalls))
	}
	if checkpoint.Worktree.CleanedAt != "" {
		t.Fatalf("CleanedAt = %q, want empty", checkpoint.Worktree.CleanedAt)
	}

	// Prepared worktrees still clean up on the terminal path.
	checkpoint.Worktree.PreparedAt = "2026-04-11T12:00:00.000Z"
	runner.cleanupFixerWorktreeIfTerminal(context.Background(), storage.ProjectRecord{
		ID: "project_1", RepoPath: t.TempDir(), BaseBranch: stringPtr("main"),
	}, checkpoint)
	if len(git.cleanupCalls) != 1 {
		t.Fatalf("len(git.cleanupCalls) = %d, want 1 for prepared worktree", len(git.cleanupCalls))
	}
	if checkpoint.Worktree.CleanedAt == "" {
		t.Fatal("CleanedAt empty after prepared cleanup")
	}
}

// Full queue lifecycle: interrupted-repair rewind → prepare probe fails on final
// attempt → terminal parking must not force-remove the dirty worktree.
func TestProcessClaimedItemTerminalPrepareErrorPreservesDirtyWorktree(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	f := newDirtyAdoptFixture(t)
	project, err := fixture.repos.Projects.GetByID(context.Background(), "project_1")
	if err != nil || project == nil {
		t.Fatalf("Projects.GetByID() = (%#v, %v)", project, err)
	}
	project.MetadataJSON = &f.metadata
	project.RepoPath = f.repoPath
	if err := fixture.repos.Projects.Upsert(context.Background(), *project); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	repo := "acme/looper"
	prNumber := int64(42)
	loopTarget := "pr:acme/looper:42"
	nowISO := fixture.nowISO()
	if err := fixture.repos.Loops.Upsert(context.Background(), storage.LoopRecord{
		ID:         "loop_fixer_prepare_terminal_preserve",
		Seq:        1,
		ProjectID:  "project_1",
		Type:       "fixer",
		TargetType: "pull_request",
		TargetID:   &loopTarget,
		Repo:       &repo,
		PRNumber:   &prNumber,
		Status:     "queued",
		CreatedAt:  nowISO,
		UpdatedAt:  nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	// Prior repair failure with a prepared worktree so resume rewinds to prepare-worktree.
	checkpointJSON := mustMarshalJSON(fixerCheckpoint{
		ClaimedLockKey: "pr:acme/looper:42",
		Detail:         &checkpointDetail{HeadSHA: f.headSHA, HeadRefName: f.branch, BaseRefName: "main", State: "OPEN"},
		FixItems:       []FixItem{{Type: "comment", ID: "c1", ThreadID: "t1", Summary: "please fix"}},
		Worktree: &checkpointWorktree{
			Path:        f.wtPath,
			Branch:      f.branch,
			HeadSHA:     f.headSHA,
			BaseHeadSHA: f.headSHA,
			PreparedAt:  nowISO,
		},
		Repair: &checkpointRepair{Summary: "interrupted", ParseStatus: "parsed", CompletedAt: nowISO},
	})
	if err := fixture.repos.Runs.Upsert(context.Background(), storage.RunRecord{
		ID:                "run_failed_before_prepare_retry",
		LoopID:            "loop_fixer_prepare_terminal_preserve",
		Status:            "failed",
		CurrentStep:       stringPtr(string(stepRepair)),
		LastCompletedStep: stringPtr(string(stepPrepareWorktree)),
		CheckpointJSON:    &checkpointJSON,
		StartedAt:         nowISO,
		CreatedAt:         nowISO,
		UpdatedAt:         nowISO,
	}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}

	projectID := "project_1"
	loopID := "loop_fixer_prepare_terminal_preserve"
	lockKey := "pr:acme/looper:42"
	if err := fixture.repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{
		ID:          "queue_fixer_prepare_terminal_preserve",
		ProjectID:   &projectID,
		LoopID:      &loopID,
		Type:        "fixer",
		TargetType:  "pull_request",
		TargetID:    loopTarget,
		Repo:        &repo,
		PRNumber:    &prNumber,
		DedupeKey:   "fixer:acme/looper:42:prepare-terminal-preserve",
		Priority:    1,
		Status:      "queued",
		AvailableAt: nowISO,
		MaxAttempts: 1, // final allowed attempt → terminal parking
		LockKey:     &lockKey,
		CreatedAt:   nowISO,
		UpdatedAt:   nowISO,
	}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	github := &fakeGitHubGateway{
		currentUser: "looper",
		viewResponses: []PullRequestDetail{{
			Number: 42, State: "OPEN", HeadSHA: f.headSHA, HeadRefName: f.branch,
			BaseRefName: "main", BaseSHA: "base-1", Author: "looper",
			Comments: []map[string]any{{"id": "c1", "threadId": "t1", "body": "please fix"}},
		}},
	}
	git := &fakeGitGateway{
		// Use the external ssh-style wording that previously triggered force cleanup.
		prepareErr:    fmt.Errorf("error: cannot run ssh: No such file or directory\nfatal: unable to fork"),
		createResult:  CreateWorktreeResult{WorktreePath: f.wtPath, Branch: f.branch, HeadSHA: f.headSHA},
		prepareResult: PrepareWorktreeResult{HeadSHA: f.headSHA, Clean: false},
	}
	runner := New(Options{
		DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git,
		Logger: fixture.logger, Now: fixture.now,
	})

	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "fixer-worker-1", "fixer")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNextOfType() = (%#v, %v), want claimed item", claim, err)
	}

	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "failed" {
		t.Fatalf("result = %#v, want failed status after terminal prepare error", result)
	}
	if len(git.cleanupCalls) != 0 {
		t.Fatalf("len(git.cleanupCalls) = %d, want 0 (terminal prepare failure must not force-remove dirty worktree)", len(git.cleanupCalls))
	}
	if len(git.createCalls) != 0 {
		t.Fatalf("len(git.createCalls) = %d, want 0 (existing dirty path must not recreate)", len(git.createCalls))
	}
	f.assertDirtyPreserved(t)

	queue, err := fixture.repos.Queue.GetByID(context.Background(), claim.ID)
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	if queue == nil || queue.Status != "manual_intervention" || queue.FinishedAt == nil {
		t.Fatalf("queue = %#v, want terminal manual_intervention", queue)
	}

	// Failed run checkpoint must still point at the preserved worktree (unprepared).
	run, err := fixture.repos.Runs.GetByID(context.Background(), result.RunID)
	if err != nil || run == nil {
		t.Fatalf("Runs.GetByID() = (%#v, %v)", run, err)
	}
	persisted := parseCheckpoint(run.CheckpointJSON)
	if persisted.Worktree == nil || persisted.Worktree.Path != f.wtPath {
		t.Fatalf("persisted worktree = %#v, want path %q retained", persisted.Worktree, f.wtPath)
	}
	if persisted.Worktree.PreparedAt != "" {
		t.Fatalf("persisted PreparedAt = %q, want empty (rewind / failed prepare)", persisted.Worktree.PreparedAt)
	}
	if persisted.Worktree.CleanedAt != "" {
		t.Fatalf("persisted CleanedAt = %q, want empty", persisted.Worktree.CleanedAt)
	}
}
