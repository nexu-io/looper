package reviewer

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
)

func runGitHeadDrift(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, string(out))
	}
	return strings.TrimSpace(string(out))
}

func seedReviewerHeadDriftRepo(t *testing.T) (remoteDir, repoDir, baseSHA, oldHead, disjointHead, overlappingHead, forcePushedHead string) {
	t.Helper()
	root := t.TempDir()
	remoteDir = filepath.Join(root, "origin.git")
	repoDir = filepath.Join(root, "repo")

	runGitHeadDrift(t, root, "init", "--bare", remoteDir)
	runGitHeadDrift(t, root, "clone", remoteDir, repoDir)
	runGitHeadDrift(t, repoDir, "config", "user.name", "Test")
	runGitHeadDrift(t, repoDir, "config", "user.email", "test@example.com")
	runGitHeadDrift(t, repoDir, "checkout", "-b", "main")

	if err := os.WriteFile(filepath.Join(repoDir, "app.go"), []byte("package app\n\nfunc Hello() int { return 1 }\n"), 0o644); err != nil {
		t.Fatalf("write app.go: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "docs.md"), []byte("# Docs\n"), 0o644); err != nil {
		t.Fatalf("write docs.md: %v", err)
	}
	runGitHeadDrift(t, repoDir, "add", ".")
	runGitHeadDrift(t, repoDir, "commit", "-m", "initial")
	baseSHA = runGitHeadDrift(t, repoDir, "rev-parse", "HEAD")
	runGitHeadDrift(t, repoDir, "push", "-u", "origin", "main")

	runGitHeadDrift(t, repoDir, "checkout", "-b", "feature/review-me")
	if err := os.WriteFile(filepath.Join(repoDir, "app.go"), []byte("package app\n\nfunc Hello() int { return 2 }\n"), 0o644); err != nil {
		t.Fatalf("write app.go v2: %v", err)
	}
	runGitHeadDrift(t, repoDir, "add", "app.go")
	runGitHeadDrift(t, repoDir, "commit", "-m", "pr commit 1")
	oldHead = runGitHeadDrift(t, repoDir, "rev-parse", "HEAD")
	runGitHeadDrift(t, repoDir, "push", "-u", "origin", "feature/review-me")

	// Push disjoint additive commit touching only docs.md.
	pusher := filepath.Join(root, "pusher")
	runGitHeadDrift(t, root, "clone", remoteDir, pusher)
	runGitHeadDrift(t, pusher, "config", "user.name", "Test")
	runGitHeadDrift(t, pusher, "config", "user.email", "test@example.com")
	runGitHeadDrift(t, pusher, "checkout", "feature/review-me")
	if err := os.WriteFile(filepath.Join(pusher, "docs.md"), []byte("# Docs v2\n"), 0o644); err != nil {
		t.Fatalf("write docs.md v2: %v", err)
	}
	runGitHeadDrift(t, pusher, "add", "docs.md")
	runGitHeadDrift(t, pusher, "commit", "-m", "disjoint docs update")
	disjointHead = runGitHeadDrift(t, pusher, "rev-parse", "HEAD")
	runGitHeadDrift(t, pusher, "push", "origin", "feature/review-me")

	// Also build an overlapping commit on a branch and a force-pushed commit on another branch.
	runGitHeadDrift(t, pusher, "checkout", "-b", "feature/overlap")
	if err := os.WriteFile(filepath.Join(pusher, "app.go"), []byte("package app\n\nfunc Hello() int { return 3 }\n"), 0o644); err != nil {
		t.Fatalf("write app.go v3: %v", err)
	}
	runGitHeadDrift(t, pusher, "add", "app.go")
	runGitHeadDrift(t, pusher, "commit", "-m", "touch app.go")
	overlappingHead = runGitHeadDrift(t, pusher, "rev-parse", "HEAD")
	runGitHeadDrift(t, pusher, "push", "origin", "feature/overlap")

	runGitHeadDrift(t, pusher, "checkout", "-b", "feature/force", baseSHA)
	if err := os.WriteFile(filepath.Join(pusher, "docs.md"), []byte("# Rewritten\n"), 0o644); err != nil {
		t.Fatalf("write docs.md force: %v", err)
	}
	runGitHeadDrift(t, pusher, "add", "docs.md")
	runGitHeadDrift(t, pusher, "commit", "-m", "force push")
	forcePushedHead = runGitHeadDrift(t, pusher, "rev-parse", "HEAD")
	runGitHeadDrift(t, pusher, "push", "origin", "feature/force")

	return remoteDir, repoDir, baseSHA, oldHead, disjointHead, overlappingHead, forcePushedHead
}

type headDriftFakeGitGateway struct {
	fakeGitGateway
	worktreePath string
	headSHA      string
}

func (g *headDriftFakeGitGateway) CreateWorktree(_ context.Context, input CreateWorktreeInput) (CreateWorktreeResult, error) {
	return CreateWorktreeResult{WorktreePath: g.worktreePath, Branch: input.Branch, HeadSHA: g.headSHA}, nil
}

func (g *headDriftFakeGitGateway) PrepareWorktree(_ context.Context, _ PrepareWorktreeInput) (PrepareWorktreeResult, error) {
	return PrepareWorktreeResult{HeadSHA: g.headSHA, Clean: true}, nil
}

// syncHeadGitHubGateway wraps fakeGitHubGateway with mutex-protected live head
// transitions and an optional poll-count gate for race-free monitor tests.
type syncHeadGitHubGateway struct {
	*fakeGitHubGateway
	mu               sync.Mutex
	liveHead         string
	notifyAfterPolls int
	pollCount        int
	polledCh         chan struct{}
	polledOnce       sync.Once
}

func (g *syncHeadGitHubGateway) setHead(sha string) {
	g.mu.Lock()
	g.liveHead = sha
	g.mu.Unlock()
}

func (g *syncHeadGitHubGateway) ViewPullRequest(ctx context.Context, input ViewPullRequestInput) (PullRequestDetail, error) {
	g.mu.Lock()
	g.fakeGitHubGateway.viewHeadSHA = g.liveHead
	g.mu.Unlock()
	return g.fakeGitHubGateway.ViewPullRequest(ctx, input)
}

func (g *syncHeadGitHubGateway) CapturePullRequestSnapshot(ctx context.Context, input CapturePullRequestSnapshotInput) (storage.PullRequestSnapshotRecord, error) {
	g.mu.Lock()
	g.fakeGitHubGateway.viewHeadSHA = g.liveHead
	g.mu.Unlock()
	return g.fakeGitHubGateway.CapturePullRequestSnapshot(ctx, input)
}

func (g *syncHeadGitHubGateway) GetPullRequestHeadSHA(_ context.Context, _ ViewPullRequestInput) (string, error) {
	g.mu.Lock()
	head := g.liveHead
	g.pollCount++
	count := g.pollCount
	target := g.notifyAfterPolls
	g.mu.Unlock()
	if target > 0 && count >= target && g.polledCh != nil {
		g.polledOnce.Do(func() { close(g.polledCh) })
	}
	return head, nil
}

// killSignalingExecutor wraps fakeAgentExecutor so Wait can block until Kill
// fires instead of racing a fixed sleep under -race.
type killSignalingExecutor struct {
	inner     *fakeAgentExecutor
	killedCh  chan struct{}
	killOnce  sync.Once
	mu        sync.Mutex
	killCount int
}

type killSignalingExecution struct {
	inner  AgentExecution
	parent *killSignalingExecutor
}

func (e *killSignalingExecutor) Start(ctx context.Context, input AgentRunInput) (AgentExecution, error) {
	exec, err := e.inner.Start(ctx, input)
	if err != nil {
		return nil, err
	}
	return &killSignalingExecution{inner: exec, parent: e}, nil
}

func (e *killSignalingExecution) Wait(ctx context.Context) (AgentResult, error) {
	return e.inner.Wait(ctx)
}

func (e *killSignalingExecution) Kill(reason string) error {
	e.parent.mu.Lock()
	e.parent.killCount++
	e.parent.mu.Unlock()
	err := e.inner.Kill(reason)
	if e.parent.killedCh != nil {
		e.parent.killOnce.Do(func() { close(e.parent.killedCh) })
	}
	return err
}

func (e *killSignalingExecutor) kills() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.killCount
}

func TestHeadChangeMonitorKeepsRunningOnDisjointAdditiveCommitAndRecordsPublishedHead(t *testing.T) {
	t.Parallel()
	_, repoDir, baseSHA, oldHead, disjointHead, _, _ := seedReviewerHeadDriftRepo(t)
	fixture := newRunnerFixture(t)
	ctx := context.Background()

	baseBranch := "main"
	if err := fixture.repos.Projects.Upsert(ctx, storage.ProjectRecord{
		ID: "project_1", Name: "Looper", RepoPath: repoDir, BaseBranch: &baseBranch,
		CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO(),
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	polledTwice := make(chan struct{})
	github := &syncHeadGitHubGateway{
		fakeGitHubGateway: &fakeGitHubGateway{
			viewHeadSHA:         oldHead,
			viewBaseSHA:         baseSHA,
			reviewRequests:      []string{"octocat"},
			reviewMarkerEvent:   ReviewEventRequestChanges,
			reviewMarkerOutcome: "blocking",
		},
		liveHead:         oldHead,
		notifyAfterPolls: 2,
		polledCh:         polledTwice,
	}
	agent := &killSignalingExecutor{
		inner: &fakeAgentExecutor{
			results: []AgentResult{{
				Status:      "completed",
				Summary:     "Found blocking bug in app.go",
				Stdout:      `__LOOPER_RESULT__={"summary":"Found blocking bug in app.go","outcome":"blocking"}`,
				ParseStatus: "parsed",
			}},
			onStart: func(AgentRunInput) {
				github.setHead(disjointHead)
			},
			wait: func(waitCtx context.Context) error {
				// Wait until at least 2 monitor polls observe disjointHead, proving the
				// first poll verified disjointness and chose not to kill the agent.
				select {
				case <-polledTwice:
					return nil
				case <-waitCtx.Done():
					return waitCtx.Err()
				case <-time.After(10 * time.Second):
					return nil
				}
			},
		},
	}
	git := &headDriftFakeGitGateway{worktreePath: repoDir, headSHA: oldHead}
	runner := New(Options{
		DB:                     fixture.coordinator.DB(),
		Repos:                  fixture.repos,
		GitHub:                 github,
		Git:                    git,
		AgentExecutor:          agent,
		Logger:                 fixture.logger,
		Now:                    fixture.now,
		HeadChangePollInterval: time.Millisecond,
		ReviewEvents:           config.ReviewerReviewEventsConfig{Clean: config.ReviewerReviewEventApprove, Blocking: config.ReviewerReviewEventRequestChanges},
		LoopConfig:             testReviewerLoopConfig(),
	})

	repo := "acme/looper"
	prNumber := int64(42)
	metadata := `{"followUpdates":true,"loop":{"enabled":true}}`
	loop := storage.LoopRecord{
		ID: "loop_head_disjoint_keep_running", Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request",
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
		t.Fatalf("result = %#v, want success after disjoint additive commit", result)
	}
	if agent.kills() != 0 {
		t.Fatalf("kills = %d, want 0 for disjoint additive commit", agent.kills())
	}
	updatedLoop, err := fixture.repos.Loops.GetByID(ctx, loop.ID)
	if err != nil || updatedLoop == nil {
		t.Fatalf("Loops.GetByID() = (%#v, %v)", updatedLoop, err)
	}
	// Must record oldHead (the reviewed commit), NOT disjointHead (so follow-up reviews the new commit).
	if got, _ := stringFromAny(parseJSONObject(updatedLoop.MetadataJSON)["lastPublishedHeadSha"]); got != oldHead {
		t.Fatalf("lastPublishedHeadSha = %q, want reviewed head %q", got, oldHead)
	}
	// Must reconcile verified non-clean review side effects (remove stale +1 reaction).
	removedPlusOne := false
	for _, removed := range github.removeReactionCalls {
		if removed.Content == "+1" {
			removedPlusOne = true
			break
		}
	}
	if !removedPlusOne {
		t.Fatalf("removeReactionCalls = %#v, want stale +1 clean-review reaction removed on recovered actionable review", github.removeReactionCalls)
	}
}

func TestHeadChangeMonitorInterruptsOnOverlapOrForcePushWithoutBurningAttemptsAndDebounces(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		fetchBranch string
		targetHead  func(overlap, force string) string
	}{
		{
			name:        "overlapping_commit",
			fetchBranch: "feature/overlap",
			targetHead:  func(overlap, _ string) string { return overlap },
		},
		{
			name:        "force_push",
			fetchBranch: "feature/force",
			targetHead:  func(_, force string) string { return force },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, repoDir, baseSHA, oldHead, _, overlappingHead, forcePushedHead := seedReviewerHeadDriftRepo(t)
			// Fetch the target branch into repoDir so monitor can verify overlap/non-ancestor locally.
			runGitHeadDrift(t, repoDir, "fetch", "--no-tags", "origin", tc.fetchBranch)
			nextHead := tc.targetHead(overlappingHead, forcePushedHead)

			fixture := newRunnerFixture(t)
			ctx := context.Background()
			baseBranch := "main"
			if err := fixture.repos.Projects.Upsert(ctx, storage.ProjectRecord{
				ID: "project_1", Name: "Looper", RepoPath: repoDir, BaseBranch: &baseBranch,
				CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO(),
			}); err != nil {
				t.Fatalf("Projects.Upsert() error = %v", err)
			}

			github := &syncHeadGitHubGateway{
				fakeGitHubGateway: &fakeGitHubGateway{
					viewHeadSHA:         oldHead,
					viewBaseSHA:         baseSHA,
					reviewRequests:      []string{"octocat"},
					reviewMarkerMissing: true,
				},
				liveHead: oldHead,
			}
			killedCh := make(chan struct{})
			agent := &killSignalingExecutor{
				killedCh: killedCh,
				inner: &fakeAgentExecutor{
					results: []AgentResult{{Status: "completed", Summary: "stale", Stdout: `__LOOPER_RESULT__={"summary":"stale"}`, ParseStatus: "parsed"}},
					onStart: func(AgentRunInput) {
						github.setHead(nextHead)
					},
					wait: func(waitCtx context.Context) error {
						select {
						case <-killedCh:
							return nil
						case <-waitCtx.Done():
							return waitCtx.Err()
						case <-time.After(10 * time.Second):
							return nil
						}
					},
				},
			}
			git := &headDriftFakeGitGateway{worktreePath: repoDir, headSHA: oldHead}
			loopCfg := testReviewerLoopConfig()
			loopCfg.QuietPeriodSeconds = 60
			runner := New(Options{
				DB:                     fixture.coordinator.DB(),
				Repos:                  fixture.repos,
				GitHub:                 github,
				Git:                    git,
				AgentExecutor:          agent,
				Logger:                 fixture.logger,
				Now:                    fixture.now,
				HeadChangePollInterval: time.Millisecond,
				LoopConfig:             loopCfg,
			})

			repo := "acme/looper"
			prNumber := int64(42)
			metadata := `{"followUpdates":true,"loop":{"enabled":true}}`
			loop := storage.LoopRecord{
				ID: "loop_head_interrupt_" + tc.name, Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request",
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
			// Simulate queue item already near MaxAttempts to verify head-change interrupts never exhaust attempts.
			queue.Attempts = queue.MaxAttempts - 1
			if err := fixture.repos.Queue.MarkRetry(ctx, storage.QueueMarkRetryInput{ID: queue.ID, AvailableAt: fixture.nowISO(), Attempts: queue.Attempts, ErrorKind: string(FailureRetryableAfterResume), UpdatedAt: fixture.nowISO()}); err != nil {
				t.Fatalf("MarkRetry() error = %v", err)
			}
			claimed, err := fixture.repos.Queue.ClaimNextOfType(ctx, fixture.nowISO(), "reviewer-worker-1", "reviewer")
			if err != nil || claimed == nil || claimed.ID != queue.ID {
				t.Fatalf("ClaimNextOfType() = (%#v, %v)", claimed, err)
			}

			result, err := runner.ProcessClaimedItem(ctx, *claimed)
			if err != nil {
				t.Fatalf("ProcessClaimedItem() error = %v", err)
			}
			if result.Status != "interrupted" || !strings.Contains(result.Summary, "PR head changed while reviewer was running") {
				t.Fatalf("result = %#v, want interrupted head-change", result)
			}
			if agent.kills() != 1 {
				t.Fatalf("kills = %d, want 1", agent.kills())
			}
			requeued, err := fixture.repos.Queue.GetByID(ctx, queue.ID)
			if err != nil || requeued == nil {
				t.Fatalf("Queue.GetByID() = (%#v, %v)", requeued, err)
			}
			if requeued.Status != "queued" {
				t.Fatalf("requeued.Status = %q, want queued (must not exhaust MaxAttempts)", requeued.Status)
			}
			if requeued.Attempts != queue.Attempts {
				t.Fatalf("requeued.Attempts = %d, want preserved %d", requeued.Attempts, queue.Attempts)
			}
			avail, parseErr := time.Parse(time.RFC3339Nano, requeued.AvailableAt)
			if parseErr != nil {
				t.Fatalf("parse AvailableAt %q: %v", requeued.AvailableAt, parseErr)
			}
			wantMinAvail := fixture.now().Add(60 * time.Second)
			if avail.Before(wantMinAvail) {
				t.Fatalf("requeued.AvailableAt = %s, want >= %s (QuietPeriodSeconds debounce)", avail.Format(time.RFC3339), wantMinAvail.Format(time.RFC3339))
			}
		})
	}
}

func TestRunPublishStepStillMarksStaleOnCleanReviewWhenHeadAdvances(t *testing.T) {
	t.Parallel()
	_, repoDir, baseSHA, oldHead, disjointHead, _, _ := seedReviewerHeadDriftRepo(t)
	fixture := newRunnerFixture(t)
	ctx := context.Background()

	baseBranch := "main"
	if err := fixture.repos.Projects.Upsert(ctx, storage.ProjectRecord{
		ID: "project_1", Name: "Looper", RepoPath: repoDir, BaseBranch: &baseBranch,
		CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO(),
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	github := &syncHeadGitHubGateway{
		fakeGitHubGateway: &fakeGitHubGateway{
			viewHeadSHA:         oldHead,
			viewBaseSHA:         baseSHA,
			reviewRequests:      []string{"octocat"},
			reviewMarkerEvent:   ReviewEventApprove,
			reviewMarkerOutcome: "clean",
		},
		liveHead: oldHead,
	}
	agent := &fakeAgentExecutor{
		results: []AgentResult{{
			Status:      "completed",
			Summary:     "No actionable findings",
			Stdout:      `__LOOPER_RESULT__={"summary":"No actionable findings","outcome":"clean"}`,
			ParseStatus: "parsed",
		}},
		onStart: func(AgentRunInput) {
			github.setHead(disjointHead)
		},
	}
	git := &headDriftFakeGitGateway{worktreePath: repoDir, headSHA: oldHead}
	runner := New(Options{
		DB:                     fixture.coordinator.DB(),
		Repos:                  fixture.repos,
		GitHub:                 github,
		Git:                    git,
		AgentExecutor:          agent,
		Logger:                 fixture.logger,
		Now:                    fixture.now,
		HeadChangePollInterval: time.Hour, // disable mid-run poll to test publish step directly
		ReviewEvents:           config.ReviewerReviewEventsConfig{Clean: config.ReviewerReviewEventApprove, Blocking: config.ReviewerReviewEventRequestChanges},
		LoopConfig:             testReviewerLoopConfig(),
	})

	repo := "acme/looper"
	prNumber := int64(42)
	metadata := `{"followUpdates":true,"loop":{"enabled":true}}`
	loop := storage.LoopRecord{
		ID: "loop_clean_stale_on_head_advance", Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request",
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
	if result.Status != "skipped" || !strings.Contains(result.Summary, "PR head changed before publish") {
		t.Fatalf("result = %#v, want skipped stale on clean review when head advanced", result)
	}
}
