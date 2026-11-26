package reviewer

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
)

func runGitBaseDrift(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, string(out))
	}
	return strings.TrimSpace(string(out))
}

func seedReviewerBaseDriftRepo(t *testing.T) (remoteDir, repoDir, oldBase, unrelatedNewBase, shiftedNewBase, headSHA string) {
	t.Helper()
	root := t.TempDir()
	remoteDir = filepath.Join(root, "origin.git")
	repoDir = filepath.Join(root, "repo")

	runGitBaseDrift(t, root, "init", "--bare", remoteDir)
	runGitBaseDrift(t, root, "clone", remoteDir, repoDir)
	runGitBaseDrift(t, repoDir, "config", "user.name", "Test")
	runGitBaseDrift(t, repoDir, "config", "user.email", "test@example.com")
	runGitBaseDrift(t, repoDir, "checkout", "-b", "main")

	if err := os.WriteFile(filepath.Join(repoDir, "app.go"), []byte("package app\n\nfunc Hello() int { return 1 }\n"), 0o644); err != nil {
		t.Fatalf("write app.go: %v", err)
	}
	runGitBaseDrift(t, repoDir, "add", ".")
	runGitBaseDrift(t, repoDir, "commit", "-m", "initial")
	oldBase = runGitBaseDrift(t, repoDir, "rev-parse", "HEAD")
	runGitBaseDrift(t, repoDir, "push", "-u", "origin", "main")

	runGitBaseDrift(t, repoDir, "checkout", "-b", "feature/pr")
	if err := os.WriteFile(filepath.Join(repoDir, "app.go"), []byte("package app\n\nfunc Hello() int { return 2 }\n"), 0o644); err != nil {
		t.Fatalf("write app.go v2: %v", err)
	}
	runGitBaseDrift(t, repoDir, "add", "app.go")
	runGitBaseDrift(t, repoDir, "commit", "-m", "pr commit 1")
	c1 := runGitBaseDrift(t, repoDir, "rev-parse", "HEAD")

	if err := os.WriteFile(filepath.Join(repoDir, "extra.go"), []byte("package app\n\nfunc Extra() {}\n"), 0o644); err != nil {
		t.Fatalf("write extra.go: %v", err)
	}
	runGitBaseDrift(t, repoDir, "add", "extra.go")
	runGitBaseDrift(t, repoDir, "commit", "-m", "pr commit 2")
	headSHA = runGitBaseDrift(t, repoDir, "rev-parse", "HEAD")
	runGitBaseDrift(t, repoDir, "push", "-u", "origin", "feature/pr")

	// Advance origin/main remotely with an unrelated commit (not fetched in repoDir yet).
	pusher := filepath.Join(root, "pusher")
	runGitBaseDrift(t, root, "clone", remoteDir, pusher)
	runGitBaseDrift(t, pusher, "config", "user.name", "Test")
	runGitBaseDrift(t, pusher, "config", "user.email", "test@example.com")
	runGitBaseDrift(t, pusher, "checkout", "main")
	if err := os.WriteFile(filepath.Join(pusher, "other.go"), []byte("package app\n\nfunc Other() {}\n"), 0o644); err != nil {
		t.Fatalf("write other.go: %v", err)
	}
	runGitBaseDrift(t, pusher, "add", "other.go")
	runGitBaseDrift(t, pusher, "commit", "-m", "unrelated main commit")
	unrelatedNewBase = runGitBaseDrift(t, pusher, "rev-parse", "HEAD")
	runGitBaseDrift(t, pusher, "push", "origin", "main")

	// Also create a commit on a separate branch merging c1 so merge-base shifts.
	runGitBaseDrift(t, pusher, "fetch", "origin", "feature/pr")
	runGitBaseDrift(t, pusher, "checkout", "-b", "main-shifted")
	runGitBaseDrift(t, pusher, "merge", "--no-ff", "-m", "merge c1", c1)
	shiftedNewBase = runGitBaseDrift(t, pusher, "rev-parse", "HEAD")
	runGitBaseDrift(t, pusher, "push", "origin", "main-shifted")

	return remoteDir, repoDir, oldBase, unrelatedNewBase, shiftedNewBase, headSHA
}

type baseDriftFakeGitGateway struct {
	fakeGitGateway
	worktreePath string
	headSHA      string
}

func (g *baseDriftFakeGitGateway) CreateWorktree(_ context.Context, input CreateWorktreeInput) (CreateWorktreeResult, error) {
	return CreateWorktreeResult{WorktreePath: g.worktreePath, Branch: input.Branch, HeadSHA: g.headSHA}, nil
}

func (g *baseDriftFakeGitGateway) PrepareWorktree(_ context.Context, _ PrepareWorktreeInput) (PrepareWorktreeResult, error) {
	return PrepareWorktreeResult{HeadSHA: g.headSHA, Clean: true}, nil
}

func TestReviewerPublishesWhenBaseAdvancesWithUnchangedMergeBase(t *testing.T) {
	t.Parallel()
	_, repoDir, oldBase, unrelatedNewBase, _, headSHA := seedReviewerBaseDriftRepo(t)
	fixture := newRunnerFixture(t)
	ctx := context.Background()

	baseBranch := "main"
	if err := fixture.repos.Projects.Upsert(ctx, storage.ProjectRecord{
		ID: "project_1", Name: "Looper", RepoPath: repoDir, BaseBranch: &baseBranch,
		CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO(),
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	github := &fakeGitHubGateway{
		viewHeadSHA:    headSHA,
		viewBaseSHA:    oldBase,
		reviewRequests: []string{"octocat"},
	}
	agent := &fakeAgentExecutor{
		results: []AgentResult{{Status: "completed", Summary: "No actionable findings", Stdout: `__LOOPER_RESULT__={"summary":"No actionable findings"}`, ParseStatus: "parsed"}},
		onStart: func(AgentRunInput) {
			github.viewBaseSHA = unrelatedNewBase
		},
	}
	git := &baseDriftFakeGitGateway{worktreePath: repoDir, headSHA: headSHA}
	runner := New(Options{
		DB:            fixture.coordinator.DB(),
		Repos:         fixture.repos,
		GitHub:        github,
		Git:           git,
		AgentExecutor: agent,
		Logger:        fixture.logger,
		Now:           fixture.now,
		ReviewEvents:  config.ReviewerReviewEventsConfig{Clean: config.ReviewerReviewEventComment, Blocking: config.ReviewerReviewEventComment},
		LoopConfig:    testReviewerLoopConfig(),
	})

	repo := "acme/looper"
	prNumber := int64(42)
	metadata := `{"followUpdates":true,"loop":{"enabled":true}}`
	loop := storage.LoopRecord{
		ID: "loop_base_advance_publish", Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request",
		Repo: &repo, PRNumber: &prNumber, Status: "queued", MetadataJSON: &metadata,
		CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO(),
	}
	if err := fixture.repos.Loops.Upsert(ctx, loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	queue, err := runner.enqueue(ctx, enqueueInput{ProjectID: "project_1", LoopID: loop.ID, Repo: repo, PRNumber: prNumber})
	if err != nil {
		t.Fatalf("enqueue() error = %v", err)
	}
	claimed, err := fixture.repos.Queue.ClaimNextOfType(ctx, fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claimed == nil || claimed.ID != queue.ID {
		t.Fatalf("ClaimNextOfType() = (%#v, %v)", claimed, err)
	}

	result, err := runner.ProcessClaimedItem(ctx, *claimed)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "success" {
		t.Fatalf("result = %#v, want success when base advanced with unchanged merge-base", result)
	}
	updatedLoop, err := fixture.repos.Loops.GetByID(ctx, loop.ID)
	if err != nil || updatedLoop == nil {
		t.Fatalf("Loops.GetByID() = (%#v, %v)", updatedLoop, err)
	}
	if got, _ := stringFromAny(parseJSONObject(updatedLoop.MetadataJSON)["lastPublishedHeadSha"]); got != headSHA {
		t.Fatalf("lastPublishedHeadSha = %q, want %q", got, headSHA)
	}
}

func TestReviewerMarksStaleWhenBaseAdvanceShiftsMergeBaseOrConflicts(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		setup     func(t *testing.T, repoDir, unrelatedNewBase, shiftedNewBase string, github *fakeGitHubGateway)
		wantInMsg string
	}{
		{
			name: "shifted_merge_base",
			setup: func(t *testing.T, repoDir, _, shiftedNewBase string, github *fakeGitHubGateway) {
				runGitBaseDrift(t, repoDir, "fetch", "--no-tags", "origin", "main-shifted")
				github.viewBaseSHA = shiftedNewBase
			},
			wantInMsg: "PR base changed before publish",
		},
		{
			name: "merge_conflicts_after_base_advance",
			setup: func(_ *testing.T, _, unrelatedNewBase, _ string, github *fakeGitHubGateway) {
				github.viewBaseSHA = unrelatedNewBase
				github.hasConflicts = true
			},
			wantInMsg: "PR has merge conflicts",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, repoDir, oldBase, unrelatedNewBase, shiftedNewBase, headSHA := seedReviewerBaseDriftRepo(t)
			fixture := newRunnerFixture(t)
			ctx := context.Background()

			baseBranch := "main"
			if err := fixture.repos.Projects.Upsert(ctx, storage.ProjectRecord{
				ID: "project_1", Name: "Looper", RepoPath: repoDir, BaseBranch: &baseBranch,
				CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO(),
			}); err != nil {
				t.Fatalf("Projects.Upsert() error = %v", err)
			}

			github := &fakeGitHubGateway{
				viewHeadSHA:    headSHA,
				viewBaseSHA:    oldBase,
				reviewRequests: []string{"octocat"},
			}
			agent := &fakeAgentExecutor{
				results: []AgentResult{{Status: "completed", Summary: "No actionable findings", Stdout: `__LOOPER_RESULT__={"summary":"No actionable findings"}`, ParseStatus: "parsed"}},
				onStart: func(AgentRunInput) {
					tc.setup(t, repoDir, unrelatedNewBase, shiftedNewBase, github)
				},
			}
			git := &baseDriftFakeGitGateway{worktreePath: repoDir, headSHA: headSHA}
			runner := New(Options{
				DB:            fixture.coordinator.DB(),
				Repos:         fixture.repos,
				GitHub:        github,
				Git:           git,
				AgentExecutor: agent,
				Logger:        fixture.logger,
				Now:           fixture.now,
				ReviewEvents:  config.ReviewerReviewEventsConfig{Clean: config.ReviewerReviewEventComment, Blocking: config.ReviewerReviewEventComment},
				LoopConfig:    testReviewerLoopConfig(),
			})

			repo := "acme/looper"
			prNumber := int64(42)
			metadata := `{"followUpdates":true,"loop":{"enabled":true}}`
			loop := storage.LoopRecord{
				ID: "loop_base_drift_" + tc.name, Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request",
				Repo: &repo, PRNumber: &prNumber, Status: "queued", MetadataJSON: &metadata,
				CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO(),
			}
			if err := fixture.repos.Loops.Upsert(ctx, loop); err != nil {
				t.Fatalf("Loops.Upsert() error = %v", err)
			}
			queue, err := runner.enqueue(ctx, enqueueInput{ProjectID: "project_1", LoopID: loop.ID, Repo: repo, PRNumber: prNumber})
			if err != nil {
				t.Fatalf("enqueue() error = %v", err)
			}
			claimed, err := fixture.repos.Queue.ClaimNextOfType(ctx, fixture.nowISO(), "reviewer-worker-1", "reviewer")
			if err != nil || claimed == nil || claimed.ID != queue.ID {
				t.Fatalf("ClaimNextOfType() = (%#v, %v)", claimed, err)
			}

			result, err := runner.ProcessClaimedItem(ctx, *claimed)
			if err != nil {
				t.Fatalf("ProcessClaimedItem() error = %v", err)
			}
			if result.Status != "skipped" {
				t.Fatalf("result = %#v, want skipped stale run", result)
			}
			run, err := fixture.repos.Runs.GetByID(ctx, result.RunID)
			if err != nil || run == nil {
				t.Fatalf("Runs.GetByID() = (%#v, %v)", run, err)
			}
			checkpoint := parseCheckpoint(run.CheckpointJSON)
			if checkpoint.SkipKind != "stale" || !strings.Contains(checkpoint.SkipReason, tc.wantInMsg) {
				t.Fatalf("checkpoint = %#v, want stale containing %q", checkpoint, tc.wantInMsg)
			}
		})
	}
}

func TestGroupedReviewContinuesOnUnchangedMergeBaseAdvance(t *testing.T) {
	t.Parallel()
	runner, input, github, agent := groupedReviewLifecycleFixture(t)
	worktree := input.Checkpoint.Worktree.Path
	oldBase := input.Checkpoint.Snapshot.BaseSHA
	head := input.Checkpoint.Snapshot.HeadSHA

	// Create an unrelated commit off oldBase directly in the local repo so merge-base(newBase, head) == oldBase.
	unrelatedBase := runGitBaseDrift(t, worktree, "commit-tree", oldBase+"^{tree}", "-p", oldBase, "-m", "unrelated base advance")
	agent.onStart = func(AgentRunInput) {
		if len(agent.starts) == 1 {
			github.viewBaseSHA = unrelatedBase
		}
	}

	checkpoint, err := runner.executeStep(context.Background(), stepReview, input)
	if err != nil {
		t.Fatalf("executeStep(stepReview) error = %v, want nil when merge-base is unchanged", err)
	}
	if len(agent.starts) != 3 {
		t.Fatalf("agent starts = %d, want 2 group subtasks + 1 final reviewer", len(agent.starts))
	}
	if checkpoint.PendingReview == nil || checkpoint.PendingReview.HeadSHA != head {
		t.Fatalf("checkpoint.PendingReview = %#v, want pending review for head %s", checkpoint.PendingReview, head)
	}
}
