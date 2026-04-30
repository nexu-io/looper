package reviewer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/powerformer/looper/internal/config"
	"github.com/powerformer/looper/internal/infra/specpr"
	"github.com/powerformer/looper/internal/storage"
)

func TestDiscoverPullRequestsCreatesLoopAndQueue(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})

	result, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	if len(result.QueueItems) != 1 {
		t.Fatalf("len(QueueItems) = %d, want 1", len(result.QueueItems))
	}
	if len(result.CreatedLoopIDs) != 1 {
		t.Fatalf("len(CreatedLoopIDs) = %d, want 1", len(result.CreatedLoopIDs))
	}
	loop, err := fixture.repos.Loops.GetByID(context.Background(), result.CreatedLoopIDs[0])
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if loop == nil || loop.Status != "queued" || loop.Repo == nil || *loop.Repo != "acme/looper" {
		t.Fatalf("loop = %#v, want queued reviewer loop", loop)
	}
	queue, err := fixture.repos.Queue.GetByID(context.Background(), result.QueueItems[0].ID)
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	if queue == nil || queue.Status != "queued" || queue.DedupeKey != "reviewer:project_1:"+result.CreatedLoopIDs[0]+":acme/looper:42" {
		t.Fatalf("queue = %#v, want queued reviewer item", queue)
	}
}

func TestDiscoverPullRequestsReturnsCurrentUserLookupError(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{currentLoginErr: fmt.Errorf("gh auth failed")}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})

	result, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err == nil || !strings.Contains(err.Error(), "gh auth failed") {
		t.Fatalf("DiscoverPullRequests() error = %v, want gh auth failed", err)
	}
	if len(result.QueueItems) != 0 || len(result.CreatedLoopIDs) != 0 {
		t.Fatalf("result = %#v, want no discovery results on auth error", result)
	}
}

func TestDiscoverPullRequestsPreservesPausedLoop(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})

	nowISO := fixture.nowISO()
	repo := "acme/looper"
	prNumber := int64(42)
	loop := storage.LoopRecord{ID: "loop_paused", Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: "paused", CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	result, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: repo})
	if err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	if len(result.CreatedLoopIDs) != 0 || len(result.QueueItems) != 0 {
		t.Fatalf("result = %#v, want no created loops or queue items", result)
	}
	persisted, err := fixture.repos.Loops.GetByID(context.Background(), loop.ID)
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if persisted == nil || persisted.Status != "paused" || persisted.NextRunAt != nil {
		t.Fatalf("loop = %#v, want paused loop with nil next run", persisted)
	}
	items, err := fixture.repos.Queue.List(context.Background())
	if err != nil {
		t.Fatalf("Queue.List() error = %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("len(Queue.List()) = %d, want 0", len(items))
	}
}

func TestDiscoverPullRequestsSkipsSpecLabelWhenCurrentUserIsNotRequested(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{labels: []string{specpr.ReviewingLabel}, reviewRequests: []string{"alice"}, currentLogin: "bob"}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})

	result, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	if len(result.CreatedLoopIDs) != 0 || len(result.QueueItems) != 0 {
		t.Fatalf("result = %#v, want no created loops or queue items", result)
	}
	items, err := fixture.repos.Queue.List(context.Background())
	if err != nil {
		t.Fatalf("Queue.List() error = %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("len(Queue.List()) = %d, want 0", len(items))
	}
}

func TestDiscoverPullRequestsAllowsSpecLabelWhenCurrentUserIsRequested(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{labels: []string{specpr.ReviewingLabel}, reviewRequests: []string{"bob"}, currentLogin: "bob"}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})

	result, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	if len(result.QueueItems) != 1 {
		t.Fatalf("len(QueueItems) = %d, want 1", len(result.QueueItems))
	}
}

func TestDiscoverPullRequestsSkipsAutomaticFollowUpWhenCurrentUserIsNotRequested(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{reviewRequests: []string{"alice"}, currentLogin: "bob"}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})
	nowISO := fixture.nowISO()
	repo := "acme/looper"
	prNumber := int64(42)
	metadata := `{"followUpdates":true}`
	loop := storage.LoopRecord{ID: "loop_follow", Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: "completed", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	result, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: repo})
	if err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	if len(result.QueueItems) != 0 {
		t.Fatalf("len(QueueItems) = %d, want 0", len(result.QueueItems))
	}
}

func TestDiscoverPullRequestsAllowsAutomaticFollowUpWhenCurrentUserIsRequested(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{reviewRequests: []string{"bob"}, currentLogin: "bob"}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})
	nowISO := fixture.nowISO()
	repo := "acme/looper"
	prNumber := int64(42)
	metadata := `{"followUpdates":true}`
	loop := storage.LoopRecord{ID: "loop_follow_requested", Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: "completed", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	result, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: repo})
	if err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	if len(result.QueueItems) != 1 {
		t.Fatalf("len(QueueItems) = %d, want 1", len(result.QueueItems))
	}
}

func TestDiscoverPullRequestsAllowsManualFollowUpWithoutReviewRequest(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{reviewRequests: []string{"alice"}, currentLogin: "bob"}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})
	nowISO := fixture.nowISO()
	repo := "acme/looper"
	prNumber := int64(42)
	metadata := `{"followUpdates":true,"manual":true}`
	loop := storage.LoopRecord{ID: "loop_manual_follow", Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: "completed", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	result, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: repo})
	if err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	if len(result.QueueItems) != 1 {
		t.Fatalf("len(QueueItems) = %d, want 1", len(result.QueueItems))
	}
}

func TestDiscoverPullRequestsAllowsManualFollowUpAfterSkippedAutomaticLoopForSamePR(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{reviewRequests: []string{"alice"}, currentLogin: "bob"}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})
	nowISO := fixture.nowISO()
	repo := "acme/looper"
	prNumber := int64(42)
	automaticMetadata := `{"followUpdates":true}`
	manualMetadata := `{"followUpdates":true,"manual":true}`
	for _, loop := range []storage.LoopRecord{
		{ID: "loop_auto_follow", Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: "completed", MetadataJSON: &automaticMetadata, CreatedAt: nowISO, UpdatedAt: nowISO},
		{ID: "loop_manual_follow_after_auto", Seq: 2, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: "completed", MetadataJSON: &manualMetadata, CreatedAt: nowISO, UpdatedAt: nowISO},
	} {
		if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
			t.Fatalf("Loops.Upsert(%s) error = %v", loop.ID, err)
		}
	}

	result, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: repo})
	if err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	if len(result.QueueItems) != 1 {
		t.Fatalf("len(QueueItems) = %d, want 1", len(result.QueueItems))
	}
	if result.QueueItems[0].LoopID == nil || *result.QueueItems[0].LoopID != "loop_manual_follow_after_auto" {
		t.Fatalf("queue loopID = %#v, want manual follow-up loop", result.QueueItems[0].LoopID)
	}
}

func TestProcessClaimedItemSkipsQueuedAutomaticLoopWhenCurrentUserIsNotRequested(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{reviewRequests: []string{"alice"}, currentLogin: "bob"}
	agent := &fakeAgentExecutor{}
	git := &fakeGitGateway{}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now})
	nowISO := fixture.nowISO()
	repo := "acme/looper"
	prNumber := int64(42)
	loop := storage.LoopRecord{ID: "loop_api", Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: "queued", CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	queue, err := runner.enqueue(context.Background(), enqueueInput{ProjectID: "project_1", LoopID: loop.ID, Repo: repo, PRNumber: prNumber})
	if err != nil {
		t.Fatalf("enqueue() error = %v", err)
	}
	claimed, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claimed == nil || claimed.ID != queue.ID {
		t.Fatalf("ClaimNextOfType() = (%#v, %v), want queued item %s", claimed, err, queue.ID)
	}

	result, err := runner.ProcessClaimedItem(context.Background(), *claimed)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "skipped" || !strings.Contains(result.Summary, "not requested for review") {
		t.Fatalf("result = %#v, want skipped not requested", result)
	}
	if len(agent.starts) != 0 || len(git.createCalls) != 0 {
		t.Fatalf("agent starts=%d git creates=%d, want no review work", len(agent.starts), len(git.createCalls))
	}
}

func TestProcessClaimedItemRetriesWhenCurrentUserLookupFails(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{currentLoginErr: fmt.Errorf("gh auth failed")}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})
	nowISO := fixture.nowISO()
	repo := "acme/looper"
	prNumber := int64(42)
	loop := storage.LoopRecord{ID: "loop_lookup_error", Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: "queued", CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	queue, err := runner.enqueue(context.Background(), enqueueInput{ProjectID: "project_1", LoopID: loop.ID, Repo: repo, PRNumber: prNumber})
	if err != nil {
		t.Fatalf("enqueue() error = %v", err)
	}
	claimed, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claimed == nil || claimed.ID != queue.ID {
		t.Fatalf("ClaimNextOfType() = (%#v, %v), want queued item %s", claimed, err, queue.ID)
	}

	result, err := runner.ProcessClaimedItem(context.Background(), *claimed)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "failed" || result.FailureKind != FailureRetryableTransient {
		t.Fatalf("result = %#v, want retryable transient failure", result)
	}
	queueAfter, err := fixture.repos.Queue.GetByID(context.Background(), queue.ID)
	if err != nil || queueAfter == nil {
		t.Fatalf("Queue.GetByID() = (%#v, %v), want queue", queueAfter, err)
	}
	if queueAfter.Status != "queued" {
		t.Fatalf("queue status = %s, want queued retry", queueAfter.Status)
	}
}

func TestProcessClaimedItemAllowsManualQueuedLoopWithoutReviewRequest(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{reviewRequests: []string{"alice"}, currentLogin: "bob"}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "Manual review", Stdout: `__LOOPER_RESULT__={"summary":"posted review"}`}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now})
	nowISO := fixture.nowISO()
	repo := "acme/looper"
	prNumber := int64(42)
	metadata := `{"manual":true}`
	loop := storage.LoopRecord{ID: "loop_manual_api", Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: "queued", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	queue, err := runner.enqueue(context.Background(), enqueueInput{ProjectID: "project_1", LoopID: loop.ID, Repo: repo, PRNumber: prNumber})
	if err != nil {
		t.Fatalf("enqueue() error = %v", err)
	}
	claimed, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claimed == nil || claimed.ID != queue.ID {
		t.Fatalf("ClaimNextOfType() = (%#v, %v), want queued item %s", claimed, err, queue.ID)
	}

	result, err := runner.ProcessClaimedItem(context.Background(), *claimed)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "success" {
		t.Fatalf("result = %#v, want success", result)
	}
	if len(agent.starts) != 1 {
		t.Fatalf("agent starts=%d, want agent-native review to run", len(agent.starts))
	}
}

func TestProcessClaimedItemRestartsAutomaticResumeFromDiscoverForFreshReviewRequests(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{reviewRequests: []string{"octocat"}}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "Resumed review", Stdout: `__LOOPER_RESULT__={"summary":"posted review"}`}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now})
	nowISO := fixture.nowISO()
	repo := "acme/looper"
	prNumber := int64(42)
	loop := storage.LoopRecord{ID: "loop_legacy_resume", Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: "queued", CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	legacyCheckpoint := reviewerCheckpoint{Detail: &checkpointDetail{Title: "Review me", State: "OPEN", HeadSHA: "abc123"}, ResumePolicy: "advance_from_checkpoint"}
	legacyRun := storage.RunRecord{ID: "run_legacy", LoopID: loop.ID, Status: "failed", CurrentStep: stringPtr(string(stepClaim)), LastCompletedStep: stringPtr(string(stepFilter)), CheckpointJSON: stringPtr(mustMarshalJSON(legacyCheckpoint)), StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Runs.Upsert(context.Background(), legacyRun); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}
	queue, err := runner.enqueue(context.Background(), enqueueInput{ProjectID: "project_1", LoopID: loop.ID, Repo: repo, PRNumber: prNumber})
	if err != nil {
		t.Fatalf("enqueue() error = %v", err)
	}
	claimed, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claimed == nil || claimed.ID != queue.ID {
		t.Fatalf("ClaimNextOfType() = (%#v, %v), want queued item %s", claimed, err, queue.ID)
	}

	result, err := runner.ProcessClaimedItem(context.Background(), *claimed)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "success" {
		t.Fatalf("result = %#v, want success after fresh discover", result)
	}
	if github.viewCalls == 0 || len(agent.starts) != 1 {
		t.Fatalf("viewCalls=%d agent starts=%d, want fresh discover and review", github.viewCalls, len(agent.starts))
	}
}

func TestEnqueueScopesReviewerDedupeKeyToLoop(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: &fakeGitHubGateway{}, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})

	project2ID := "project_2"
	loop1ID := "loop_1"
	loop2ID := "loop_2"
	nowISO := fixture.nowISO()
	baseBranch := "main"
	repoPath2 := filepath.Join(t.TempDir(), "repo-2")
	repo := "acme/looper"
	prNumber := int64(42)

	if err := fixture.repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: project2ID, Name: "Looper Two", RepoPath: repoPath2, BaseBranch: &baseBranch, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert(project_2) error = %v", err)
	}
	for _, loop := range []storage.LoopRecord{
		{ID: loop1ID, Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Status: "queued", CreatedAt: nowISO, UpdatedAt: nowISO},
		{ID: loop2ID, Seq: 2, ProjectID: project2ID, Type: "reviewer", TargetType: "pull_request", Status: "queued", CreatedAt: nowISO, UpdatedAt: nowISO},
	} {
		if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
			t.Fatalf("Loops.Upsert(%s) error = %v", loop.ID, err)
		}
	}

	first, err := runner.enqueue(context.Background(), enqueueInput{ProjectID: "project_1", LoopID: loop1ID, Repo: repo, PRNumber: prNumber})
	if err != nil {
		t.Fatalf("enqueue(first) error = %v", err)
	}
	second, err := runner.enqueue(context.Background(), enqueueInput{ProjectID: project2ID, LoopID: loop2ID, Repo: repo, PRNumber: prNumber})
	if err != nil {
		t.Fatalf("enqueue(second) error = %v", err)
	}
	if first.ID == second.ID {
		t.Fatalf("enqueue(second) reused queue item %q across loops", second.ID)
	}
	if second.LoopID == nil || *second.LoopID != loop2ID {
		t.Fatalf("second loopID = %#v, want %q", second.LoopID, loop2ID)
	}
	if second.DedupeKey != buildReviewerDedupeKey(project2ID, loop2ID, repo, prNumber) {
		t.Fatalf("second dedupe key = %q, want scoped reviewer key", second.DedupeKey)
	}
	items, err := fixture.repos.Queue.List(context.Background())
	if err != nil {
		t.Fatalf("Queue.List() error = %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("len(Queue.List()) = %d, want 2", len(items))
	}
}

func TestProcessClaimedItemCompletesAgentNativeReviewWithoutGoPublish(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "Please add tests", Stdout: `__LOOPER_RESULT__={"summary":"posted review"}`}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, AllowAutoApprove: true})

	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	firstClaim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || firstClaim == nil {
		t.Fatalf("ClaimNext() = (%#v, %v), want claimed queue item", firstClaim, err)
	}
	firstResult, err := runner.ProcessClaimedItem(context.Background(), *firstClaim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem(first) error = %v", err)
	}
	if firstResult.Status != "success" {
		t.Fatalf("first result = %#v, want success", firstResult)
	}
	if len(agent.starts) != 1 {
		t.Fatalf("len(agent.starts) = %d, want 1", len(agent.starts))
	}
	runs, err := fixture.repos.Runs.ListByLoop(context.Background(), firstResult.LoopID)
	if err != nil {
		t.Fatalf("Runs.ListByLoop() error = %v", err)
	}
	if len(runs) == 0 || runs[0].LastCompletedStep == nil || *runs[0].LastCompletedStep != string(stepPublish) {
		t.Fatalf("runs[0] = %#v, want lastCompletedStep=publish", runs)
	}
	queueAfterSuccess, err := fixture.repos.Queue.GetByID(context.Background(), firstClaim.ID)
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	if queueAfterSuccess == nil || queueAfterSuccess.Status != "completed" {
		t.Fatalf("queue after success = %#v, want completed", queueAfterSuccess)
	}
	loop, err := fixture.repos.Loops.GetByID(context.Background(), firstResult.LoopID)
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if loop == nil || loop.Status != "completed" || loop.MetadataJSON == nil || !contains(*loop.MetadataJSON, `"lastPublishedHeadSha":"abc123"`) {
		t.Fatalf("loop after success = %#v, want completed with lastPublishedHeadSha", loop)
	}
}

func TestProcessClaimedItemAgentNativeReviewCompletesWithoutGoPublish(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{reviewRequests: []string{"octocat"}}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "Please add tests", Stdout: `__LOOPER_RESULT__={"summary":"posted review"}`}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, AllowAutoApprove: true})

	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	firstClaim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || firstClaim == nil {
		t.Fatalf("ClaimNext() = (%#v, %v), want claimed queue item", firstClaim, err)
	}
	firstResult, err := runner.ProcessClaimedItem(context.Background(), *firstClaim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem(first) error = %v", err)
	}
	if firstResult.Status != "success" {
		t.Fatalf("first result = %#v, want success", firstResult)
	}
	if len(agent.starts) != 1 {
		t.Fatalf("agent starts=%d, want agent review", len(agent.starts))
	}
}

func TestProcessClaimedItemRequiresSideEffectsBeforeRecordingPublishSuccess(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{reviewRequests: []string{"octocat"}, reviewMarkerOutcome: "clean", addReactionErr: fmt.Errorf("reaction failed")}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "LGTM", Stdout: `__LOOPER_RESULT__={"summary":"posted review"}`}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, AllowAutoApprove: true})

	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNext() = (%#v, %v), want claimed queue item", claim, err)
	}
	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "failed" || result.FailureKind != FailureRetryableAfterResume || !contains(result.Summary, "Failed to add clean-review reaction") {
		t.Fatalf("result = %#v, want retryable side-effect failure", result)
	}
	loop, err := fixture.repos.Loops.GetByID(context.Background(), result.LoopID)
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if loop == nil || (loop.MetadataJSON != nil && contains(*loop.MetadataJSON, `"lastPublishedHeadSha":"abc123"`)) {
		t.Fatalf("loop after failed side effect = %#v, want no lastPublishedHeadSha", loop)
	}
}

func TestProcessClaimedItemRequiresActionableSideEffectsBeforeRecordingPublishSuccess(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{reviewRequests: []string{"octocat"}, reviewMarkerOutcome: "actionable", removeReactionErr: fmt.Errorf("remove reaction failed")}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "Please add tests", Stdout: `__LOOPER_RESULT__={"summary":"posted review"}`}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, AllowAutoApprove: true})

	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNext() = (%#v, %v), want claimed queue item", claim, err)
	}
	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "failed" || result.FailureKind != FailureRetryableAfterResume || !contains(result.Summary, "Failed to remove stale clean-review reaction") {
		t.Fatalf("result = %#v, want retryable side-effect failure", result)
	}
	loop, err := fixture.repos.Loops.GetByID(context.Background(), result.LoopID)
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if loop == nil || (loop.MetadataJSON != nil && contains(*loop.MetadataJSON, `"lastPublishedHeadSha":"abc123"`)) {
		t.Fatalf("loop after failed actionable side effect = %#v, want no lastPublishedHeadSha", loop)
	}
}

func TestProcessClaimedItemAppliesCleanSpecSideEffectsBeforePublishSuccess(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{labels: []string{specpr.ReviewingLabel}, reviewRequests: []string{"octocat"}, reviewMarkerOutcome: "clean", reviewMarkerEvent: ReviewEventApprove}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "LGTM", Stdout: `__LOOPER_RESULT__={"summary":"posted review"}`}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, AllowAutoApprove: true})

	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNext() = (%#v, %v), want claimed queue item", claim, err)
	}
	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "success" {
		t.Fatalf("result = %#v, want success", result)
	}
	if len(github.addReactionCalls) != 1 || github.addReactionCalls[0].Content != "+1" {
		t.Fatalf("addReactionCalls = %#v, want one +1 reaction", github.addReactionCalls)
	}
	if len(github.removeLabelCalls) != 1 || github.removeLabelCalls[0].Labels[0] != specpr.ReviewingLabel {
		t.Fatalf("removeLabelCalls = %#v, want spec-reviewing removal", github.removeLabelCalls)
	}
	if len(github.addLabelCalls) != 1 || github.addLabelCalls[0].Labels[0] != specpr.ReadyLabel {
		t.Fatalf("addLabelCalls = %#v, want spec-ready add", github.addLabelCalls)
	}
}

func TestProcessClaimedItemRefreshesReviewStateBeforeSpecReadyTransition(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{labels: []string{specpr.ReviewingLabel}, reviewRequests: []string{"octocat"}, reviewDecision: "CHANGES_REQUESTED", useReviewStateAfterFirstView: true, reviewMarkerOutcome: "clean", reviewMarkerEvent: ReviewEventApprove}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "LGTM", Stdout: `__LOOPER_RESULT__={"summary":"posted review"}`}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, AllowAutoApprove: true})

	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNext() = (%#v, %v), want claimed queue item", claim, err)
	}
	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "success" {
		t.Fatalf("result = %#v, want success", result)
	}
	if github.viewCalls < 2 {
		t.Fatalf("viewCalls = %d, want publish detail refresh before spec-ready transition", github.viewCalls)
	}
	if len(github.removeLabelCalls) != 1 || github.removeLabelCalls[0].Labels[0] != specpr.ReviewingLabel {
		t.Fatalf("removeLabelCalls = %#v, want spec-reviewing removal after refresh", github.removeLabelCalls)
	}
	if len(github.addLabelCalls) != 1 || github.addLabelCalls[0].Labels[0] != specpr.ReadyLabel {
		t.Fatalf("addLabelCalls = %#v, want spec-ready add after refresh", github.addLabelCalls)
	}
}

func TestProcessClaimedItemDoesNotTransitionSpecLabelsForCleanCommentReview(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{labels: []string{specpr.ReviewingLabel}, reviewRequests: []string{"octocat"}, reviewMarkerOutcome: "clean", reviewMarkerEvent: ReviewEventComment}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "LGTM", Stdout: `__LOOPER_RESULT__={"summary":"posted review"}`}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, AllowAutoApprove: true})

	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNext() = (%#v, %v), want claimed queue item", claim, err)
	}
	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "success" {
		t.Fatalf("result = %#v, want success", result)
	}
	if len(github.addReactionCalls) != 1 || github.addReactionCalls[0].Content != "+1" {
		t.Fatalf("addReactionCalls = %#v, want one +1 reaction", github.addReactionCalls)
	}
	if len(github.removeLabelCalls) != 0 {
		t.Fatalf("removeLabelCalls = %#v, want no spec-reviewing removal for COMMENT review", github.removeLabelCalls)
	}
	if len(github.addLabelCalls) != 0 {
		t.Fatalf("addLabelCalls = %#v, want no spec-ready add for COMMENT review", github.addLabelCalls)
	}
}

func TestProcessClaimedItemDoesNotTransitionSpecLabelsWhenPRReviewStateIsNotClean(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name           string
		reviewDecision string
		comments       []map[string]any
	}{
		{name: "changes requested", reviewDecision: "CHANGES_REQUESTED"},
		{name: "unresolved thread", comments: []map[string]any{{"state": "UNRESOLVED"}}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fixture := newRunnerFixture(t)
			github := &fakeGitHubGateway{labels: []string{specpr.ReviewingLabel}, reviewRequests: []string{"octocat"}, reviewDecision: tt.reviewDecision, comments: tt.comments, reviewMarkerOutcome: "clean", reviewMarkerEvent: ReviewEventApprove}
			agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "LGTM", Stdout: `__LOOPER_RESULT__={"summary":"posted review"}`}}}
			runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, AllowAutoApprove: true})

			if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
				t.Fatalf("DiscoverPullRequests() error = %v", err)
			}
			claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
			if err != nil || claim == nil {
				t.Fatalf("ClaimNext() = (%#v, %v), want claimed queue item", claim, err)
			}
			result, err := runner.ProcessClaimedItem(context.Background(), *claim)
			if err != nil {
				t.Fatalf("ProcessClaimedItem() error = %v", err)
			}
			if result.Status != "success" {
				t.Fatalf("result = %#v, want success", result)
			}
			if len(github.addReactionCalls) != 1 || github.addReactionCalls[0].Content != "+1" {
				t.Fatalf("addReactionCalls = %#v, want one +1 reaction", github.addReactionCalls)
			}
			if len(github.removeLabelCalls) != 0 {
				t.Fatalf("removeLabelCalls = %#v, want no spec-reviewing removal for unclean PR", github.removeLabelCalls)
			}
			if len(github.addLabelCalls) != 0 {
				t.Fatalf("addLabelCalls = %#v, want no spec-ready add for unclean PR", github.addLabelCalls)
			}
		})
	}
}

func TestProcessClaimedItemFailsWhenAgentMissingCompletionMarker(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{reviewRequests: []string{"octocat"}, reviewMarkerMissing: true}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "posted maybe", Stdout: "posted maybe"}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now})

	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNext() = (%#v, %v), want claimed queue item", claim, err)
	}
	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "failed" || result.FailureKind != FailureNonRetryable || !contains(result.Summary, "valid completion marker") {
		t.Fatalf("result = %#v, want non-retryable completion marker failure", result)
	}
}

func TestProcessClaimedItemRecoversMissingCompletionMarkerWhenReviewMarkerExists(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{reviewRequests: []string{"octocat"}, reviewMarkerOutcome: "actionable"}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "posted maybe", Stdout: "posted maybe"}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now})

	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNext() = (%#v, %v), want claimed queue item", claim, err)
	}
	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "success" {
		t.Fatalf("result = %#v, want success after marker recovery", result)
	}
	loop, err := fixture.repos.Loops.GetByID(context.Background(), result.LoopID)
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if loop == nil || loop.MetadataJSON == nil || !contains(*loop.MetadataJSON, `"lastPublishedHeadSha":"abc123"`) {
		t.Fatalf("loop after recovered publish = %#v, want lastPublishedHeadSha recorded", loop)
	}
	if github.reviewMarkerCalls != 2 {
		t.Fatalf("review marker calls = %d, want parse recovery lookup plus publish verification", github.reviewMarkerCalls)
	}
}

func TestProcessClaimedItemRecoversFailedAgentRunWhenReviewMarkerExists(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{reviewRequests: []string{"octocat"}, removeReviewRequestOnSecondView: true, reviewMarkerOutcome: "clean"}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "failed", Summary: "posted review, failed to add reaction"}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now})

	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNext() = (%#v, %v), want claimed queue item", claim, err)
	}
	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "success" {
		t.Fatalf("result = %#v, want success after failed-run marker recovery", result)
	}
	if github.reviewMarkerCalls != 2 {
		t.Fatalf("review marker calls = %d, want non-completed recovery lookup plus publish verification", github.reviewMarkerCalls)
	}
	if len(agent.starts) != 1 {
		t.Fatalf("len(agent.starts) = %d, want one review execution", len(agent.starts))
	}
}

func TestProcessClaimedItemRetriesWhenAgentReviewMarkerMissing(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{reviewRequests: []string{"octocat"}, reviewMarkerMissing: true}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "posted", Stdout: `__LOOPER_RESULT__={"summary":"posted review"}`}, {Status: "completed", Summary: "posted again", Stdout: `__LOOPER_RESULT__={"summary":"posted review again"}`}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now})

	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNext() = (%#v, %v), want claimed queue item", claim, err)
	}
	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "failed" || result.FailureKind != FailureRetryableAfterResume || !contains(result.Summary, "no matching GitHub review marker") {
		t.Fatalf("result = %#v, want retryable missing marker failure", result)
	}
	if len(agent.starts) != 1 {
		t.Fatalf("len(agent.starts) after first attempt = %d, want 1", len(agent.starts))
	}
	github.reviewMarkerMissing = false
	fixture.advance(time.Hour)
	claim, err = fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claim == nil {
		t.Fatalf("retry ClaimNext() = (%#v, %v), want claimed queue item", claim, err)
	}
	result, err = runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("retry ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "success" {
		t.Fatalf("retry result = %#v, want success", result)
	}
	if len(agent.starts) != 1 {
		t.Fatalf("len(agent.starts) after retry = %d, want marker recheck without review rerun", len(agent.starts))
	}
	if github.reviewMarkerCalls != 2 {
		t.Fatalf("review marker calls = %d, want initial lookup plus retry", github.reviewMarkerCalls)
	}
}

func TestProcessClaimedItemRerunsReviewAfterRepeatedAgentReviewMarkerMisses(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{reviewRequests: []string{"octocat"}, reviewMarkerMissing: true}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "posted", Stdout: `__LOOPER_RESULT__={"summary":"posted review"}`}, {Status: "completed", Summary: "posted again", Stdout: `__LOOPER_RESULT__={"summary":"posted review again"}`}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now})

	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNext() = (%#v, %v), want claimed queue item", claim, err)
	}
	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "failed" || result.FailureKind != FailureRetryableAfterResume || !contains(result.Summary, "retrying marker verification") {
		t.Fatalf("result = %#v, want retryable marker recheck failure", result)
	}
	if len(agent.starts) != 1 {
		t.Fatalf("len(agent.starts) after first attempt = %d, want 1", len(agent.starts))
	}

	fixture.advance(time.Hour)
	claim, err = fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claim == nil {
		t.Fatalf("retry ClaimNext() = (%#v, %v), want claimed queue item", claim, err)
	}
	result, err = runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("retry ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "failed" || result.FailureKind != FailureRetryableAfterResume || !contains(result.Summary, "no matching GitHub review marker") {
		t.Fatalf("retry result = %#v, want retryable missing marker failure", result)
	}
	if len(agent.starts) != 1 {
		t.Fatalf("len(agent.starts) after marker retry = %d, want no review rerun yet", len(agent.starts))
	}
	latestRun, err := fixture.repos.Runs.GetLatestByLoopID(context.Background(), result.LoopID)
	if err != nil {
		t.Fatalf("GetLatestByLoopID() error = %v", err)
	}
	if latestRun == nil {
		t.Fatal("latest run = nil, want failed run")
	}
	checkpoint := parseCheckpoint(latestRun.CheckpointJSON)
	if checkpoint.ResumePolicy != "rerun_review" || checkpoint.PendingReview != nil {
		t.Fatalf("checkpoint = %#v, want cleared pending review with rerun_review after repeated marker misses", checkpoint)
	}
}

func TestProcessClaimedItemRecordsReviewWhenRequestRemovedAfterMarkerAppears(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{reviewRequests: []string{"octocat"}, reviewMarkerMissing: true}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "posted", Stdout: `__LOOPER_RESULT__={"summary":"posted review"}`}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now})

	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNext() = (%#v, %v), want claimed queue item", claim, err)
	}
	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "failed" || result.FailureKind != FailureRetryableAfterResume || !contains(result.Summary, "no matching GitHub review marker") {
		t.Fatalf("result = %#v, want retryable missing marker failure", result)
	}
	github.reviewMarkerMissing = false
	github.reviewRequests = []string{"someoneelse"}
	fixture.advance(time.Hour)
	claim, err = fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claim == nil {
		t.Fatalf("retry ClaimNext() = (%#v, %v), want claimed queue item", claim, err)
	}
	result, err = runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("retry ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "success" {
		t.Fatalf("retry result = %#v, want publish success after marker appears", result)
	}
	if len(agent.starts) != 1 {
		t.Fatalf("len(agent.starts) after retry = %d, want no second review", len(agent.starts))
	}
	loop, err := fixture.repos.Loops.GetByID(context.Background(), result.LoopID)
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if loop == nil || loop.MetadataJSON == nil || !contains(*loop.MetadataJSON, `"lastPublishedHeadSha":"abc123"`) {
		t.Fatalf("loop after marker recovery = %#v, want lastPublishedHeadSha recorded", loop)
	}
}

func TestProcessClaimedItemSkipsRerunReviewWhenRequestRemovedAndMarkerMissing(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{reviewRequests: []string{"octocat"}, reviewMarkerMissing: true}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "posted", Stdout: `__LOOPER_RESULT__={"summary":"posted review"}`}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now})

	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNext() = (%#v, %v), want claimed queue item", claim, err)
	}
	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "failed" || result.FailureKind != FailureRetryableAfterResume || !contains(result.Summary, "no matching GitHub review marker") {
		t.Fatalf("result = %#v, want retryable missing marker failure", result)
	}
	github.reviewRequests = []string{"someoneelse"}
	fixture.advance(time.Hour)
	claim, err = fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claim == nil {
		t.Fatalf("retry ClaimNext() = (%#v, %v), want claimed queue item", claim, err)
	}
	result, err = runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("retry ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "skipped" || !contains(result.Summary, "current user is not requested for review") {
		t.Fatalf("retry result = %#v, want eligibility skip when marker is still missing", result)
	}
	if len(agent.starts) != 1 {
		t.Fatalf("len(agent.starts) after retry = %d, want no second review", len(agent.starts))
	}
}

func TestProcessClaimedItemRejectsUnverifiableLegacyPendingReview(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{reviewRequests: []string{"octocat"}, reviewMarkerMissing: true}
	agent := &fakeAgentExecutor{}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now})
	ctx := context.Background()

	prNumber := int64(42)
	repo := "acme/looper"
	loopTarget := "pr:42"
	loop := storage.LoopRecord{ID: "loop_legacy", Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", TargetID: &loopTarget, Repo: &repo, PRNumber: &prNumber, Status: "queued", CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}
	if err := fixture.repos.Loops.Upsert(ctx, loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	if _, err := runner.enqueue(ctx, enqueueInput{ProjectID: loop.ProjectID, LoopID: loop.ID, Repo: repo, PRNumber: prNumber}); err != nil {
		t.Fatalf("enqueue() error = %v", err)
	}
	claim, err := fixture.repos.Queue.ClaimNextOfType(ctx, fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNextOfType() = (%#v, %v), want claimed queue item", claim, err)
	}

	legacyCheckpoint := reviewerCheckpoint{
		Detail:        &checkpointDetail{Title: "Review me", State: "OPEN", HeadSHA: "abc123", ReviewRequests: []string{"octocat"}},
		Snapshot:      &checkpointSnapshot{HeadSHA: "abc123"},
		PendingReview: &pendingReviewCheckpoint{HeadSHA: "abc123", Event: ReviewEventComment, Summary: "legacy review already posted"},
		ResumePolicy:  "advance_from_checkpoint",
	}
	checkpointJSON := mustMarshalJSON(legacyCheckpoint)
	run := storage.RunRecord{ID: "run_legacy", LoopID: loop.ID, Status: "failed", CurrentStep: stringPtr(string(stepPublish)), LastCompletedStep: stringPtr(string(stepReview)), CheckpointJSON: &checkpointJSON, StartedAt: fixture.nowISO(), CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}
	if err := fixture.repos.Runs.Upsert(ctx, run); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}

	result, err := runner.ProcessClaimedItem(ctx, *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "failed" || result.FailureKind != FailureRetryableAfterResume || !contains(result.Summary, "Legacy pending review checkpoint cannot be verified") {
		t.Fatalf("result = %#v, want retryable legacy verification failure", result)
	}
	if len(agent.starts) != 0 {
		t.Fatalf("len(agent.starts) = %d, want no review rerun in failed publish attempt", len(agent.starts))
	}
	if github.reviewMarkerCalls != 0 {
		t.Fatalf("reviewMarkerCalls = %d, want agent-native marker lookup skipped for legacy pending review", github.reviewMarkerCalls)
	}
	updatedLoop, err := fixture.repos.Loops.GetByID(ctx, loop.ID)
	if err != nil || updatedLoop == nil {
		t.Fatalf("Loops.GetByID() = (%#v, %v), want loop", updatedLoop, err)
	}
	if updatedLoop.MetadataJSON != nil && contains(*updatedLoop.MetadataJSON, `"lastPublishedHeadSha":"abc123"`) {
		t.Fatalf("loop metadata = %v, want no legacy publish progress", updatedLoop.MetadataJSON)
	}
}

func TestProcessClaimedItemRetriesWhenAgentNativeReviewApprovesWithoutPermission(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{reviewRequests: []string{"octocat"}, reviewMarkerEvent: ReviewEventApprove}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "Looks good", Stdout: `__LOOPER_RESULT__={"summary":"posted review"}`}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, AllowAutoApprove: false})

	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNext() = (%#v, %v), want claimed queue item", claim, err)
	}
	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "failed" || result.FailureKind != FailureRetryableAfterResume || !contains(result.Summary, "no matching GitHub review marker") {
		t.Fatalf("result = %#v, want retryable disallowed approval marker failure", result)
	}
	loop, err := fixture.repos.Loops.GetByID(context.Background(), result.LoopID)
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if loop == nil || (loop.MetadataJSON != nil && contains(*loop.MetadataJSON, `"lastPublishedHeadSha":"abc123"`)) {
		t.Fatalf("loop after failed publish = %#v, want no lastPublishedHeadSha", loop)
	}
}

func TestProcessClaimedItemRecordsAgentNativePublishWhenReviewRequestRemovedAfterPosting(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{reviewRequests: []string{"octocat"}, removeReviewRequestOnSecondView: true}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "Please add tests", Stdout: `__LOOPER_RESULT__={"summary":"posted review"}`}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, AllowAutoApprove: true})

	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNext() = (%#v, %v), want claimed queue item", claim, err)
	}
	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "success" {
		t.Fatalf("result = %#v, want publish success after marker verification", result)
	}
	loop, err := fixture.repos.Loops.GetByID(context.Background(), result.LoopID)
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if loop == nil || loop.MetadataJSON == nil || !contains(*loop.MetadataJSON, `"lastPublishedHeadSha":"abc123"`) {
		t.Fatalf("loop after verified publish = %#v, want lastPublishedHeadSha recorded", loop)
	}
}

func TestProcessClaimedItemRecordsPublishedHeadForAgentNativeReview(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{reviewRequests: []string{"octocat"}}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "Please add tests", Stdout: `__LOOPER_RESULT__={"summary":"posted review"}`}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, AllowAutoApprove: true})

	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	firstClaim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || firstClaim == nil {
		t.Fatalf("ClaimNext() = (%#v, %v), want claimed queue item", firstClaim, err)
	}
	firstResult, err := runner.ProcessClaimedItem(context.Background(), *firstClaim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem(first) error = %v", err)
	}
	if firstResult.Status != "success" {
		t.Fatalf("first result = %#v, want success", firstResult)
	}
	if len(agent.starts) != 1 {
		t.Fatalf("agent starts=%d, want agent-native review", len(agent.starts))
	}
	loop, err := fixture.repos.Loops.GetByID(context.Background(), firstResult.LoopID)
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if loop == nil || loop.MetadataJSON == nil || !contains(*loop.MetadataJSON, `"lastPublishedHeadSha":"abc123"`) {
		t.Fatalf("loop after skipped retry = %#v, want lastPublishedHeadSha recorded", loop)
	}
}

func TestProcessClaimedItemAgentNativeReviewCompletesWithoutPublishRetry(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	fixture.repos.Locks.SetNow(fixture.now)
	github := &fakeGitHubGateway{}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "Please add tests", Stdout: `__LOOPER_RESULT__={"summary":"posted review"}`}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, AllowAutoApprove: true})

	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	claim1, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claim1 == nil {
		t.Fatalf("first ClaimNextOfType() = (%#v, %v), want claimed item", claim1, err)
	}
	first, err := runner.ProcessClaimedItem(context.Background(), *claim1)
	if err != nil {
		t.Fatalf("ProcessClaimedItem(first) error = %v", err)
	}
	if first.Status != "success" {
		t.Fatalf("first = %#v, want success", first)
	}
	queue, err := fixture.repos.Queue.GetByID(context.Background(), claim1.ID)
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	if queue == nil || queue.Status != "completed" {
		t.Fatalf("queue = %#v, want completed item", queue)
	}
}

func TestProcessClaimedItemRestartsFromDiscoverWhenHeadChangesBeforePublish(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{changeHeadOnSecondView: true}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "Review old head", Stdout: `__LOOPER_RESULT__={"summary":"posted review"}`}, {Status: "completed", Summary: "Review new head", Stdout: `__LOOPER_RESULT__={"summary":"posted review"}`}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now})

	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	firstClaim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || firstClaim == nil {
		t.Fatalf("ClaimNext() = (%#v, %v), want claimed queue item", firstClaim, err)
	}
	firstResult, err := runner.ProcessClaimedItem(context.Background(), *firstClaim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem(first) error = %v", err)
	}
	if firstResult.Status != "failed" || firstResult.FailureKind != FailureRetryableAfterResume || !contains(firstResult.Summary, "PR head changed before publish") {
		t.Fatalf("first result = %#v, want retryable head-change failure", firstResult)
	}
	if len(agent.starts) != 1 {
		t.Fatalf("agent starts=%d, want 1", len(agent.starts))
	}
}

func TestProcessClaimedItemNotifiesWhenReviewAgentStarts(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "Looks good", Stdout: `__LOOPER_RESULT__={"summary":"posted review"}`}}}
	notifications := make([]AgentExecutionStartedInput, 0, 1)
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, OnAgentExecutionStarted: func(_ context.Context, input AgentExecutionStartedInput) error {
		notifications = append(notifications, input)
		return nil
	}})

	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	claimed, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claimed == nil {
		t.Fatalf("ClaimNext() = (%#v, %v), want claimed queue item", claimed, err)
	}
	result, err := runner.ProcessClaimedItem(context.Background(), *claimed)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "success" {
		t.Fatalf("result = %#v, want success", result)
	}
	if len(notifications) != 1 {
		t.Fatalf("len(notifications) = %d, want 1", len(notifications))
	}
	if notifications[0].Subtitle != "acme/looper#42" || notifications[0].Body != "Review started" {
		t.Fatalf("notifications[0] = %#v, want review-start payload", notifications[0])
	}
}

func TestProcessClaimedItemRunsReviewerInDedicatedWorktree(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{}
	git := &fakeGitGateway{}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "Looks good", Stdout: `__LOOPER_RESULT__={"summary":"posted review"}`}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now})

	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	claimed, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claimed == nil {
		t.Fatalf("ClaimNext() = (%#v, %v), want claimed queue item", claimed, err)
	}
	result, err := runner.ProcessClaimedItem(context.Background(), *claimed)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "success" {
		t.Fatalf("result = %#v, want success", result)
	}
	if len(git.createCalls) != 1 {
		t.Fatalf("len(git.createCalls) = %d, want 1", len(git.createCalls))
	}
	if git.createCalls[0].Branch != "pr-42-head" {
		t.Fatalf("create branch = %q, want PR-scoped branch", git.createCalls[0].Branch)
	}
	if git.createCalls[0].PRNumber != 42 {
		t.Fatalf("create PR number = %d, want 42", git.createCalls[0].PRNumber)
	}
	if len(git.prepareCalls) != 1 {
		t.Fatalf("len(git.prepareCalls) = %d, want 1", len(git.prepareCalls))
	}
	if git.prepareCalls[0].Branch != "pr-42-head" {
		t.Fatalf("prepare branch = %q, want PR-scoped branch", git.prepareCalls[0].Branch)
	}
	if git.prepareCalls[0].Ref != "refs/pull/42/head" {
		t.Fatalf("prepare ref = %q, want PR head ref", git.prepareCalls[0].Ref)
	}
	if len(agent.starts) != 1 {
		t.Fatalf("len(agent.starts) = %d, want 1", len(agent.starts))
	}
	if len(git.cleanupCalls) != 1 {
		t.Fatalf("len(git.cleanupCalls) = %d, want 1", len(git.cleanupCalls))
	}
	if agent.starts[0].WorkingDirectory != git.worktreePath {
		t.Fatalf("agent working dir = %q, want %q", agent.starts[0].WorkingDirectory, git.worktreePath)
	}
	project, err := fixture.repos.Projects.GetByID(context.Background(), "project_1")
	if err != nil || project == nil {
		t.Fatalf("Projects.GetByID() = (%#v, %v), want project", project, err)
	}
	if agent.starts[0].WorkingDirectory == project.RepoPath {
		t.Fatalf("agent working dir = repo path %q, want dedicated worktree", project.RepoPath)
	}
}

func TestRunPrepareWorktreeStepFallsBackWhenCheckpointLacksHeadRef(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	git := &fakeGitGateway{}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: &fakeGitHubGateway{}, Git: git, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})

	project, err := fixture.repos.Projects.GetByID(context.Background(), "project_1")
	if err != nil || project == nil {
		t.Fatalf("Projects.GetByID() = (%#v, %v), want project", project, err)
	}

	checkpoint, err := runner.runPrepareWorktreeStep(context.Background(), stepInput{
		Project:  *project,
		Repo:     "acme/looper",
		PRNumber: 42,
		Checkpoint: reviewerCheckpoint{
			Detail:   &checkpointDetail{HeadSHA: "abc123", BaseRefName: "main"},
			Snapshot: &checkpointSnapshot{HeadSHA: "abc123"},
		},
	})
	if err != nil {
		t.Fatalf("runPrepareWorktreeStep() error = %v", err)
	}
	if len(git.createCalls) != 1 {
		t.Fatalf("len(git.createCalls) = %d, want 1", len(git.createCalls))
	}
	if git.createCalls[0].Branch != "pr-42-head" {
		t.Fatalf("create branch = %q, want fallback branch", git.createCalls[0].Branch)
	}
	if git.createCalls[0].PRNumber != 42 {
		t.Fatalf("create PR number = %d, want 42", git.createCalls[0].PRNumber)
	}
	if len(git.prepareCalls) != 1 {
		t.Fatalf("len(git.prepareCalls) = %d, want 1", len(git.prepareCalls))
	}
	if git.prepareCalls[0].Ref != "refs/pull/42/head" {
		t.Fatalf("prepare ref = %q, want PR head ref", git.prepareCalls[0].Ref)
	}
	if checkpoint.Worktree == nil || checkpoint.Worktree.Branch != "pr-42-head" {
		t.Fatalf("checkpoint worktree = %#v, want fallback branch", checkpoint.Worktree)
	}
}

func TestReviewerWorktreeBranchIgnoresHeadRefName(t *testing.T) {
	t.Parallel()

	branch := reviewerWorktreeBranch(42, reviewerCheckpoint{
		Detail:   &checkpointDetail{HeadRefName: "patch-1"},
		Worktree: &checkpointWorktree{Branch: "pr-42-head"},
	})
	if branch != "pr-42-head" {
		t.Fatalf("reviewerWorktreeBranch() = %q, want existing PR-scoped branch", branch)
	}

	branch = reviewerWorktreeBranch(42, reviewerCheckpoint{
		Detail: &checkpointDetail{HeadRefName: "main"},
	})
	if branch != "pr-42-head" {
		t.Fatalf("reviewerWorktreeBranch() = %q, want PR-scoped fallback", branch)
	}
}

func TestRunReviewStepRepreparesMissingReviewerWorktree(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	git := &fakeGitGateway{worktreePath: filepath.Join(t.TempDir(), "reviewer-worktree")}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "Looks good", Stdout: `__LOOPER_RESULT__={"summary":"posted review"}`}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: &fakeGitHubGateway{}, Git: git, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now})

	project, err := fixture.repos.Projects.GetByID(context.Background(), "project_1")
	if err != nil || project == nil {
		t.Fatalf("Projects.GetByID() = (%#v, %v), want project", project, err)
	}

	checkpoint, err := runner.runReviewStep(context.Background(), stepInput{
		Project:  *project,
		Loop:     storage.LoopRecord{ID: "loop_1"},
		Run:      storage.RunRecord{ID: "run_1"},
		Repo:     "acme/looper",
		PRNumber: 42,
		Checkpoint: reviewerCheckpoint{
			Detail:   &checkpointDetail{HeadRefName: "feature/review-me", BaseRefName: "main"},
			Snapshot: &checkpointSnapshot{HeadSHA: "abc123"},
			Worktree: &checkpointWorktree{Path: filepath.Join(t.TempDir(), "deleted-worktree"), Branch: "feature/review-me", PreparedAt: fixture.nowISO()},
		},
	})
	if err != nil {
		t.Fatalf("runReviewStep() error = %v", err)
	}
	if len(git.createCalls) != 1 || len(git.prepareCalls) != 1 {
		t.Fatalf("createCalls=%d prepareCalls=%d, want 1 each", len(git.createCalls), len(git.prepareCalls))
	}
	if len(agent.starts) != 1 {
		t.Fatalf("len(agent.starts) = %d, want 1", len(agent.starts))
	}
	if agent.starts[0].WorkingDirectory != git.worktreePath {
		t.Fatalf("agent working dir = %q, want %q", agent.starts[0].WorkingDirectory, git.worktreePath)
	}
	if checkpoint.Worktree == nil || checkpoint.Worktree.Path != git.worktreePath {
		t.Fatalf("checkpoint worktree = %#v, want recreated worktree path", checkpoint.Worktree)
	}
}

func TestRunReviewStepPersistsRepreparedWorktreeBeforeAgentStart(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	git := &fakeGitGateway{worktreePath: filepath.Join(t.TempDir(), "reviewer-worktree")}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: &fakeGitHubGateway{}, Git: git, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})

	project, err := fixture.repos.Projects.GetByID(context.Background(), "project_1")
	if err != nil || project == nil {
		t.Fatalf("Projects.GetByID() = (%#v, %v), want project", project, err)
	}
	prNumber := int64(42)
	loopTarget := "pr:42"
	loop := storage.LoopRecord{ID: "loop_1", Seq: 1, ProjectID: project.ID, Type: "reviewer", TargetType: "pull_request", TargetID: &loopTarget, Repo: stringPtr("acme/looper"), PRNumber: &prNumber, Status: "running", CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}
	if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	initialCheckpoint := reviewerCheckpoint{
		Detail:   &checkpointDetail{HeadRefName: "feature/review-me", BaseRefName: "main"},
		Snapshot: &checkpointSnapshot{HeadSHA: "abc123"},
		Worktree: &checkpointWorktree{Path: filepath.Join(t.TempDir(), "deleted-worktree"), Branch: "feature/review-me", PreparedAt: fixture.nowISO()},
	}
	checkpointJSON := mustMarshalJSON(initialCheckpoint)
	run := storage.RunRecord{ID: "run_1", LoopID: loop.ID, Status: "running", CurrentStep: stringPtr(string(stepReview)), CheckpointJSON: &checkpointJSON, StartedAt: fixture.nowISO(), CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}
	if err := fixture.repos.Runs.Upsert(context.Background(), run); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}

	checkpoint, err := runner.runReviewStep(context.Background(), stepInput{
		Project:    *project,
		Loop:       loop,
		Run:        run,
		Repo:       "acme/looper",
		PRNumber:   prNumber,
		Checkpoint: initialCheckpoint,
	})
	if err == nil || !contains(err.Error(), "no queued agent result") {
		t.Fatalf("runReviewStep() error = %v, want no queued agent result", err)
	}
	if checkpoint.Worktree == nil || checkpoint.Worktree.Path != git.worktreePath {
		t.Fatalf("checkpoint worktree = %#v, want recreated worktree path", checkpoint.Worktree)
	}
	persistedRun, err := fixture.repos.Runs.GetByID(context.Background(), run.ID)
	if err != nil || persistedRun == nil {
		t.Fatalf("Runs.GetByID() = (%#v, %v), want run", persistedRun, err)
	}
	persistedCheckpoint := parseCheckpoint(persistedRun.CheckpointJSON)
	if persistedCheckpoint.Worktree == nil || persistedCheckpoint.Worktree.Path != git.worktreePath {
		t.Fatalf("persisted checkpoint worktree = %#v, want recreated worktree path", persistedCheckpoint.Worktree)
	}
}

func TestProcessClaimedItemRetryAfterReviewFailureRepreparesWorktree(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	git := &fakeGitGateway{worktreePath: filepath.Join(t.TempDir(), "reviewer-worktree")}
	github := &fakeGitHubGateway{reviewMarkerMissing: true}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "failed", Summary: "agent failed"}, {Status: "completed", Summary: "Looks good", Stdout: `__LOOPER_RESULT__={"summary":"posted review"}`}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now})

	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	firstClaim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || firstClaim == nil {
		t.Fatalf("first ClaimNextOfType() = (%#v, %v), want claimed item", firstClaim, err)
	}
	firstResult, err := runner.ProcessClaimedItem(context.Background(), *firstClaim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem(first) error = %v", err)
	}
	if firstResult.Status != "failed" || firstResult.FailureKind != FailureRetryableTransient {
		t.Fatalf("first result = %#v, want retryable_transient failure", firstResult)
	}

	fixture.advance(5 * time.Second)
	github.reviewMarkerMissing = false
	retryClaim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || retryClaim == nil {
		t.Fatalf("retry ClaimNextOfType() = (%#v, %v), want claimed item", retryClaim, err)
	}
	retryResult, err := runner.ProcessClaimedItem(context.Background(), *retryClaim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem(retry) error = %v", err)
	}
	if retryResult.Status != "success" {
		t.Fatalf("retry result = %#v, want success", retryResult)
	}
	if len(git.createCalls) != 2 || len(git.prepareCalls) != 2 {
		t.Fatalf("createCalls=%d prepareCalls=%d, want 2 each", len(git.createCalls), len(git.prepareCalls))
	}
	if len(agent.starts) != 2 {
		t.Fatalf("len(agent.starts) = %d, want 2", len(agent.starts))
	}
}

func TestRunPrepareWorktreeStepPersistsCreatedWorktreeBeforeManualIntervention(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	clean := false
	git := &fakeGitGateway{worktreePath: filepath.Join(t.TempDir(), "reviewer-worktree"), prepareClean: &clean}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: &fakeGitHubGateway{}, Git: git, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})

	project, err := fixture.repos.Projects.GetByID(context.Background(), "project_1")
	if err != nil || project == nil {
		t.Fatalf("Projects.GetByID() = (%#v, %v), want project", project, err)
	}
	prNumber := int64(42)
	loopTarget := "pr:42"
	loop := storage.LoopRecord{ID: "loop_1", Seq: 1, ProjectID: project.ID, Type: "reviewer", TargetType: "pull_request", TargetID: &loopTarget, Repo: stringPtr("acme/looper"), PRNumber: &prNumber, Status: "running", CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}
	if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	run := storage.RunRecord{ID: "run_1", LoopID: "loop_1", Status: "running", CurrentStep: stringPtr(string(stepWorktree)), CheckpointJSON: stringPtr(mustMarshalJSON(reviewerCheckpoint{})), StartedAt: fixture.nowISO(), CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}
	if err := fixture.repos.Runs.Upsert(context.Background(), run); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}

	_, err = runner.runPrepareWorktreeStep(context.Background(), stepInput{
		Project:  *project,
		Run:      run,
		Repo:     "acme/looper",
		PRNumber: 42,
		Checkpoint: reviewerCheckpoint{
			Detail:   &checkpointDetail{HeadSHA: "abc123", BaseRefName: "main"},
			Snapshot: &checkpointSnapshot{HeadSHA: "abc123"},
		},
	})
	if err == nil || !contains(err.Error(), "manual intervention required") {
		t.Fatalf("runPrepareWorktreeStep() error = %v, want manual intervention required", err)
	}
	persistedRun, err := fixture.repos.Runs.GetByID(context.Background(), run.ID)
	if err != nil || persistedRun == nil {
		t.Fatalf("Runs.GetByID() = (%#v, %v), want run", persistedRun, err)
	}
	persistedCheckpoint := parseCheckpoint(persistedRun.CheckpointJSON)
	if persistedCheckpoint.Worktree == nil || persistedCheckpoint.Worktree.Path != git.worktreePath {
		t.Fatalf("persisted checkpoint worktree = %#v, want created worktree", persistedCheckpoint.Worktree)
	}
	if persistedCheckpoint.Worktree.PreparedAt != "" {
		t.Fatalf("persisted checkpoint preparedAt = %q, want empty before failed prepare", persistedCheckpoint.Worktree.PreparedAt)
	}
}

func TestProcessNextFinalizesClaimedQueueItemOnSetupFailure(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	if _, err := fixture.coordinator.DB().ExecContext(context.Background(), `
		CREATE TRIGGER reviewer_runs_fail_start
		BEFORE INSERT ON runs
		WHEN NEW.status = 'running'
		BEGIN
			SELECT RAISE(FAIL, 'start run blocked');
		END;
	`); err != nil {
		t.Fatalf("create trigger error = %v", err)
	}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: &fakeGitHubGateway{}, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})
	discovery, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}

	result, err := runner.ProcessNext(context.Background(), "reviewer-worker-1")
	if err == nil || !contains(err.Error(), "start run blocked") {
		t.Fatalf("ProcessNext() error = %v, want start run blocked", err)
	}
	if result != nil {
		t.Fatalf("ProcessNext() = %#v, want nil result", result)
	}
	queue, getErr := fixture.repos.Queue.GetByID(context.Background(), discovery.QueueItems[0].ID)
	if getErr != nil {
		t.Fatalf("Queue.GetByID() error = %v", getErr)
	}
	if queue == nil || queue.Status != "failed" || queue.FinishedAt == nil || queue.LastErrorKind == nil || *queue.LastErrorKind != string(FailureNonRetryable) {
		t.Fatalf("queue = %#v, want failed queue item with non_retryable error kind", queue)
	}
	if queue.LastError == nil || !contains(*queue.LastError, "start run blocked") {
		t.Fatalf("queue.LastError = %#v, want start run blocked", queue.LastError)
	}
}

func TestProcessClaimedItemReturnsWhenCompleteRunFails(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "Please add tests", Stdout: `__LOOPER_RESULT__={"summary":"posted review"}`}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, AllowAutoApprove: true})

	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNext() = (%#v, %v), want claimed queue item", claim, err)
	}
	if _, err := fixture.coordinator.DB().ExecContext(context.Background(), `
		CREATE TRIGGER reviewer_runs_fail_complete_insert
		BEFORE INSERT ON runs
		WHEN NEW.status != 'running'
		BEGIN
			SELECT RAISE(FAIL, 'complete run blocked');
		END;
	`); err != nil {
		t.Fatalf("create insert trigger error = %v", err)
	}
	if _, err := fixture.coordinator.DB().ExecContext(context.Background(), `
		CREATE TRIGGER reviewer_runs_fail_complete_update
		BEFORE UPDATE ON runs
		WHEN NEW.status != 'running'
		BEGIN
			SELECT RAISE(FAIL, 'complete run blocked');
		END;
	`); err != nil {
		t.Fatalf("create update trigger error = %v", err)
	}

	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err == nil || !contains(err.Error(), "complete run blocked") {
		t.Fatalf("ProcessClaimedItem() error = %v, want complete run blocked", err)
	}
	if result != (ProcessResult{}) {
		t.Fatalf("ProcessClaimedItem() = %#v, want zero result on completeRun failure", result)
	}
	queue, getErr := fixture.repos.Queue.GetByID(context.Background(), claim.ID)
	if getErr != nil {
		t.Fatalf("Queue.GetByID() error = %v", getErr)
	}
	if queue == nil || queue.Status != "running" || queue.FinishedAt != nil {
		t.Fatalf("queue = %#v, want still-running claimed item", queue)
	}
	loop, getErr := fixture.repos.Loops.GetByID(context.Background(), *claim.LoopID)
	if getErr != nil {
		t.Fatalf("Loops.GetByID() error = %v", getErr)
	}
	if loop == nil || loop.Status != "running" {
		t.Fatalf("loop = %#v, want still-running loop", loop)
	}
	runs, getErr := fixture.repos.Runs.ListByLoop(context.Background(), *claim.LoopID)
	if getErr != nil {
		t.Fatalf("Runs.ListByLoop() error = %v", getErr)
	}
	if len(runs) != 1 || runs[0].Status != "running" {
		t.Fatalf("runs = %#v, want single running run", runs)
	}
}

func TestProcessClaimedItemPreservesPausedLoopOnRetryableFailureAfterPause(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "reviewed", Stdout: `__LOOPER_RESULT__={"summary":"posted review"}`}}, wait: func(ctx context.Context) error {
		items, err := fixture.repos.Queue.List(ctx)
		if err != nil {
			return err
		}
		loopID := ""
		for _, item := range items {
			if item.Type == "reviewer" && item.Status == "running" && item.LoopID != nil {
				loopID = *item.LoopID
				break
			}
		}
		if loopID == "" {
			return fmt.Errorf("running reviewer queue item not found")
		}
		loop, err := fixture.repos.Loops.GetByID(ctx, loopID)
		if err != nil {
			return err
		}
		if loop == nil {
			return fmt.Errorf("loop not found: %s", loopID)
		}
		loop.Status = "paused"
		loop.NextRunAt = nil
		loop.UpdatedAt = fixture.nowISO()
		if err := fixture.repos.Loops.Upsert(ctx, *loop); err != nil {
			return err
		}
		reason := "loop paused"
		if _, err := fixture.repos.Queue.CancelByLoop(ctx, loopID, fixture.nowISO(), &reason); err != nil {
			return err
		}
		return context.DeadlineExceeded
	}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: &fakeGitHubGateway{}, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now})

	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNext() = (%#v, %v), want claimed queue item", claim, err)
	}
	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "failed" || result.FailureKind != FailureRetryableTransient {
		t.Fatalf("result = %#v, want retryable_transient failure", result)
	}
	queue, err := fixture.repos.Queue.GetByID(context.Background(), claim.ID)
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	if queue == nil || queue.Status != "queued" {
		t.Fatalf("queue = %#v, want queued retry", queue)
	}
	loop, err := fixture.repos.Loops.GetByID(context.Background(), result.LoopID)
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if loop == nil || loop.Status != "paused" || loop.NextRunAt != nil {
		t.Fatalf("loop = %#v, want paused loop with nil next run", loop)
	}
}

func TestBuildReviewPromptIncludesActionableQualityContract(t *testing.T) {
	t.Parallel()

	prompt := buildReviewPrompt("acme/looper", 42, reviewerCheckpoint{Detail: &checkpointDetail{Labels: []string{specpr.ReviewingLabel}}, Snapshot: &checkpointSnapshot{Title: "Spec PR", HeadSHA: "abc123"}}, "run_1", "reviewer:loop:abc123", true, false, config.DefaultDisclosureConfig(), "opencode", "")
	for _, want := range []string{
		"Every comment MUST include",
		"Bad comment example",
		"Good spec/docs comment example",
		"Spec/docs review rubric",
		"suggestedChange",
		"warm, specific LGTM review body",
		"gh api repos/acme/looper/pulls/42/reviews",
		"looper:review id=reviewer:loop:abc123 head=abc123 outcome=clean|actionable",
		"before posting anything",
		"existing PR reviews",
		"COMMENTED or APPROVED PR review",
		"ensure +1 reaction and spec-ready label transition",
		"review request removed before publish",
		"PR head changed before publish",
		"looper:spec-reviewing",
		"remove any existing +1 reaction",
		"Review body style contract",
		"Never post terminal/tool output",
		"ANSI escape sequences",
		"file-read traces",
		"<!-- looper:stamp v=1 -->",
		"<sub>Generated by looper 0.0.0-dev · runner=reviewer · agent=opencode</sub>",
		"Inline review comments must use only the hidden `<!-- looper:stamp v=1 -->` marker",
		"Do not write the footer as plain paragraph text",
		"retry once with corrected inline anchors",
		"exit non-zero instead of moving them into the review body",
		"Resolvable inline review comments are required",
		"PR review `comments` array",
		"not as a separate issue/PR conversation comment",
		"create resolvable GitHub review threads",
		"`path`, `line`, `side`",
		"`start_line` and `start_side`",
		"the detailed findings must live in inline `comments`",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "<!-- looper:review id=reviewer:loop:abc123 head=abc123 run=") {
		t.Fatalf("prompt includes run-scoped idempotency marker:\n%s", prompt)
	}
	if strings.Contains(prompt, "PR conversation comments") {
		t.Fatalf("prompt idempotency source diverges from review marker verification:\n%s", prompt)
	}
	if strings.Contains(prompt, "moving the same actionable feedback into the review body") {
		t.Fatalf("prompt allows weakening resolvable inline comment contract:\n%s", prompt)
	}
}

func TestBuildReviewPromptRestrictsExistingMarkerSkipWhenApprovalsDisallowed(t *testing.T) {
	t.Parallel()

	prompt := buildReviewPrompt("acme/looper", 42, reviewerCheckpoint{Snapshot: &checkpointSnapshot{HeadSHA: "abc123"}}, "run_1", "reviewer:loop:abc123", false, false, config.DefaultDisclosureConfig(), "opencode", "")
	for _, want := range []string{
		"Only treat an existing marker as satisfying idempotency when that marker is on a COMMENTED PR review",
		"Ignore matching markers on APPROVED reviews and post a new COMMENT review instead",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestShouldRestartFromDiscoverForAgentNativePreflightFailures(t *testing.T) {
	t.Parallel()

	for _, summary := range []string{
		"PR head changed before publish",
		"review request removed before publish",
	} {
		if !shouldRestartFromDiscover("failed", stepReview, summary) {
			t.Fatalf("shouldRestartFromDiscover(review, %q) = false, want true", summary)
		}
	}
}

func TestProcessClaimedItemRestartsFromDiscoverOnHeadChangeSignal(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{changeHeadOnSecondView: true}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "failed", Summary: "agent reported a generic shell failure"}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now})

	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNext() = (%#v, %v), want claimed queue item", claim, err)
	}
	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "failed" || result.FailureKind != FailureRetryableAfterResume || !contains(result.Summary, "PR head changed before publish") {
		t.Fatalf("result = %#v, want structured head-change retry", result)
	}
	latestRun, err := fixture.repos.Runs.GetLatestByLoopID(context.Background(), result.LoopID)
	if err != nil || latestRun == nil {
		t.Fatalf("GetLatestByLoopID() = (%#v, %v), want failed run", latestRun, err)
	}
	checkpoint := parseCheckpoint(latestRun.CheckpointJSON)
	if checkpoint.ResumePolicy != "restart_from_discover" {
		t.Fatalf("ResumePolicy = %q, want restart_from_discover", checkpoint.ResumePolicy)
	}
	loop, err := fixture.repos.Loops.GetByID(context.Background(), result.LoopID)
	if err != nil || loop == nil {
		t.Fatalf("Loops.GetByID() = (%#v, %v), want loop", loop, err)
	}
	resumed, err := runner.createRunContext(context.Background(), *loop)
	if err != nil {
		t.Fatalf("createRunContext() error = %v", err)
	}
	if resumed.StartStep != stepDiscover {
		t.Fatalf("StartStep = %q, want discover", resumed.StartStep)
	}
}

func TestProcessClaimedItemRestartsFromDiscoverOnReviewRequestSignal(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{removeReviewRequestOnSecondView: true, reviewMarkerMissing: true}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "failed", Summary: "agent reported a generic shell failure"}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now})

	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNext() = (%#v, %v), want claimed queue item", claim, err)
	}
	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "failed" || result.FailureKind != FailureRetryableAfterResume || !contains(result.Summary, "review request removed before publish") {
		t.Fatalf("result = %#v, want structured review-request retry", result)
	}
	latestRun, err := fixture.repos.Runs.GetLatestByLoopID(context.Background(), result.LoopID)
	if err != nil || latestRun == nil {
		t.Fatalf("GetLatestByLoopID() = (%#v, %v), want failed run", latestRun, err)
	}
	checkpoint := parseCheckpoint(latestRun.CheckpointJSON)
	if checkpoint.ResumePolicy != "restart_from_discover" {
		t.Fatalf("ResumePolicy = %q, want restart_from_discover", checkpoint.ResumePolicy)
	}
}

func TestBuildReviewPromptUsesConfiguredDisclosure(t *testing.T) {
	t.Parallel()

	cfg := config.DefaultDisclosureConfig()
	model := "openai/gpt-5.5"
	prompt := buildReviewPrompt("acme/looper", 42, reviewerCheckpoint{Snapshot: &checkpointSnapshot{HeadSHA: "abc123"}}, "run_1", "reviewer:loop:abc123", false, false, cfg, "claude-code", model)
	if !strings.Contains(prompt, "agent=claude-code · model=openai/gpt-5.5") {
		t.Fatalf("prompt missing configured agent/model disclosure:\n%s", prompt)
	}
	if strings.Contains(prompt, "agent=opencode") {
		t.Fatalf("prompt retained hardcoded opencode disclosure:\n%s", prompt)
	}

	cfg.Enabled = false
	disabledPrompt := buildReviewPrompt("acme/looper", 42, reviewerCheckpoint{Snapshot: &checkpointSnapshot{HeadSHA: "abc123"}}, "run_1", "reviewer:loop:abc123", false, false, cfg, "claude-code", model)
	if !strings.Contains(disabledPrompt, "disclosure stamping is disabled") {
		t.Fatalf("prompt missing disabled disclosure instruction:\n%s", disabledPrompt)
	}
	if strings.Contains(disabledPrompt, "Generated by looper") {
		t.Fatalf("prompt included disclosure footer while disabled:\n%s", disabledPrompt)
	}
}

func TestBuildReviewPromptDoesNotTransitionSpecLabelsWithoutApprove(t *testing.T) {
	t.Parallel()

	prompt := buildReviewPrompt("acme/looper", 42, reviewerCheckpoint{Detail: &checkpointDetail{Labels: []string{specpr.ReviewingLabel}}, Snapshot: &checkpointSnapshot{Title: "Spec PR", HeadSHA: "abc123"}}, "run_1", "reviewer:loop:abc123", false, false, config.DefaultDisclosureConfig(), "opencode", "")
	if !strings.Contains(prompt, "Do not transition spec-review labels") {
		t.Fatalf("prompt missing no-transition instruction:\n%s", prompt)
	}
	if strings.Contains(prompt, "add `looper:spec-ready`") {
		t.Fatalf("prompt allows spec-ready transition when approve is disabled:\n%s", prompt)
	}
}

type runnerFixture struct {
	coordinator *storage.SQLiteCoordinator
	repos       *storage.Repositories
	logger      *testLogger
	current     time.Time
	now         func() time.Time
}

func newRunnerFixture(t *testing.T) *runnerFixture {
	t.Helper()
	coordinator, err := storage.OpenSQLiteCoordinator(context.Background(), filepath.Join(t.TempDir(), "reviewer.sqlite"), storage.SQLiteCoordinatorOptions{BackupDir: t.TempDir()})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	if _, err := coordinator.MigrationRunner().RunPending(context.Background()); err != nil {
		t.Fatalf("RunPending() error = %v", err)
	}
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 11, 12, 0, 0, 0, time.UTC)
	nowISO := fmt.Sprintf("%s.000Z", now.Format("2006-01-02T15:04:05"))
	baseBranch := "main"
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "project_1", Name: "Looper", RepoPath: filepath.Join(t.TempDir(), "repo"), BaseBranch: &baseBranch, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	fixture := &runnerFixture{coordinator: coordinator, repos: repos, logger: &testLogger{}, current: now}
	fixture.now = func() time.Time { return fixture.current }
	return fixture
}

func (f *runnerFixture) advance(delta time.Duration) { f.current = f.current.Add(delta) }

func (f *runnerFixture) nowISO() string {
	return fmt.Sprintf("%s.000Z", f.current.UTC().Format("2006-01-02T15:04:05"))
}

type fakeGitHubGateway struct {
	changeHeadOnSecondView          bool
	removeReviewRequestOnSecondView bool
	viewCalls                       int
	labels                          []string
	reviewDecision                  string
	comments                        []map[string]any
	useReviewStateAfterFirstView    bool
	reviewDecisionAfterFirstView    string
	commentsAfterFirstView          []map[string]any
	reviewRequests                  []string
	currentLogin                    string
	currentLoginErr                 error
	reviewMarkerMissing             bool
	reviewMarkerErr                 error
	reviewMarkerEvent               ReviewEvent
	reviewMarkerOutcome             string
	reviewMarkerCalls               int
	addReactionErr                  error
	removeReactionErr               error
	addLabelErr                     error
	removeLabelErr                  error
	addReactionCalls                []PullRequestReactionInput
	removeReactionCalls             []PullRequestReactionInput
	addLabelCalls                   []PullRequestLabelsInput
	removeLabelCalls                []PullRequestLabelsInput
}

func (g *fakeGitHubGateway) ListOpenPullRequests(context.Context, ListOpenPullRequestsInput) ([]PullRequestSummary, error) {
	reviewRequests := g.effectiveReviewRequests()
	return []PullRequestSummary{{Number: 42, Title: "Review me", State: "OPEN", ReviewDecision: g.reviewDecision, Labels: append([]string(nil), g.labels...), HeadSHA: "abc123", ReviewRequests: reviewRequests}, {Number: 99, Title: "Draft", State: "OPEN", IsDraft: true, HeadSHA: "draft123", ReviewRequests: reviewRequests}}, nil
}

func (g *fakeGitHubGateway) GetCurrentUserLogin(context.Context, string) (string, error) {
	if g.currentLoginErr != nil {
		return "", g.currentLoginErr
	}
	if strings.TrimSpace(g.currentLogin) != "" {
		return g.currentLogin, nil
	}
	return "octocat", nil
}

func (g *fakeGitHubGateway) ViewPullRequest(context.Context, ViewPullRequestInput) (PullRequestDetail, error) {
	g.viewCalls++
	headSHA := "abc123"
	if g.changeHeadOnSecondView && g.viewCalls >= 2 {
		headSHA = "new-head"
	}
	reviewRequests := g.effectiveReviewRequests()
	if g.removeReviewRequestOnSecondView && g.viewCalls >= 2 {
		reviewRequests = nil
	}
	reviewDecision := g.reviewDecision
	comments := g.comments
	if g.useReviewStateAfterFirstView && g.viewCalls > 1 {
		reviewDecision = g.reviewDecisionAfterFirstView
		comments = g.commentsAfterFirstView
	}
	return PullRequestDetail{Number: 42, Title: "Review me", Body: "PR body", State: "OPEN", ReviewDecision: reviewDecision, Labels: append([]string(nil), g.labels...), HeadSHA: headSHA, BaseSHA: "base123", HeadRefName: "feature/review-me", BaseRefName: "main", Author: "octocat", ReviewRequests: reviewRequests, ChecksSummary: "SUCCESS", Diff: "diff --git a/a.ts b/a.ts", Comments: cloneCommentMaps(comments)}, nil
}

func cloneCommentMaps(comments []map[string]any) []map[string]any {
	if comments == nil {
		return nil
	}
	cloned := make([]map[string]any, 0, len(comments))
	for _, comment := range comments {
		clonedComment := make(map[string]any, len(comment))
		for key, value := range comment {
			clonedComment[key] = value
		}
		cloned = append(cloned, clonedComment)
	}
	return cloned
}

func (g *fakeGitHubGateway) effectiveReviewRequests() []string {
	if g.reviewRequests != nil {
		return append([]string(nil), g.reviewRequests...)
	}
	return []string{"octocat"}
}

func (g *fakeGitHubGateway) CapturePullRequestSnapshot(_ context.Context, input CapturePullRequestSnapshotInput) (storage.PullRequestSnapshotRecord, error) {
	headSHA := "abc123"
	if g.changeHeadOnSecondView && g.viewCalls >= 2 {
		headSHA = "new-head"
	}
	return storage.PullRequestSnapshotRecord{ID: fmt.Sprintf("snapshot:%d:%s", input.PRNumber, input.CapturedAt), ProjectID: input.ProjectID, Repo: input.Repo, PRNumber: input.PRNumber, HeadSHA: headSHA, BaseSHA: stringPtr("base123"), Title: stringPtr("Review me"), Body: stringPtr("PR body"), Author: stringPtr("octocat"), ChecksSummary: stringPtr("SUCCESS"), PayloadJSON: stringPtr(`{"diff":"diff --git a/a.ts b/a.ts"}`), CapturedAt: input.CapturedAt, CreatedAt: input.CapturedAt}, nil
}

func (g *fakeGitHubGateway) FindReviewMarker(_ context.Context, input VerifyReviewMarkerInput) (ReviewMarkerResult, error) {
	g.reviewMarkerCalls++
	if g.reviewMarkerErr != nil {
		return ReviewMarkerResult{}, g.reviewMarkerErr
	}
	if g.reviewMarkerEvent != "" && !reviewEventIn(input.AllowedReviewEvents, g.reviewMarkerEvent) {
		return ReviewMarkerResult{}, nil
	}
	if g.reviewMarkerMissing {
		return ReviewMarkerResult{}, nil
	}
	outcome := g.reviewMarkerOutcome
	if outcome == "" {
		outcome = "actionable"
	}
	return ReviewMarkerResult{Found: true, Outcome: outcome, Event: g.reviewMarkerEvent}, nil
}

func (g *fakeGitHubGateway) AddPullRequestReaction(_ context.Context, input PullRequestReactionInput) error {
	g.addReactionCalls = append(g.addReactionCalls, input)
	return g.addReactionErr
}

func (g *fakeGitHubGateway) RemovePullRequestReaction(_ context.Context, input PullRequestReactionInput) error {
	g.removeReactionCalls = append(g.removeReactionCalls, input)
	return g.removeReactionErr
}

func (g *fakeGitHubGateway) AddPullRequestLabels(_ context.Context, input PullRequestLabelsInput) error {
	g.addLabelCalls = append(g.addLabelCalls, input)
	return g.addLabelErr
}

func (g *fakeGitHubGateway) RemovePullRequestLabels(_ context.Context, input PullRequestLabelsInput) error {
	g.removeLabelCalls = append(g.removeLabelCalls, input)
	return g.removeLabelErr
}

func reviewEventIn(events []ReviewEvent, want ReviewEvent) bool {
	for _, event := range events {
		if event == want {
			return true
		}
	}
	return false
}

type fakeGitGateway struct {
	worktreePath string
	createCalls  []CreateWorktreeInput
	prepareCalls []PrepareWorktreeInput
	cleanupCalls []CleanupWorktreeInput
	prepareClean *bool
}

func (f *fakeGitGateway) CreateWorktree(_ context.Context, input CreateWorktreeInput) (CreateWorktreeResult, error) {
	f.createCalls = append(f.createCalls, input)
	path := f.worktreePath
	if path == "" {
		path = filepath.Join("/tmp", "reviewer-worktree")
		f.worktreePath = path
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		return CreateWorktreeResult{}, err
	}
	return CreateWorktreeResult{WorktreePath: path, Branch: input.Branch, HeadSHA: "abc123"}, nil
}

func (f *fakeGitGateway) PrepareWorktree(_ context.Context, input PrepareWorktreeInput) (PrepareWorktreeResult, error) {
	f.prepareCalls = append(f.prepareCalls, input)
	clean := true
	if f.prepareClean != nil {
		clean = *f.prepareClean
	}
	return PrepareWorktreeResult{HeadSHA: input.ExpectedHeadSHA, Clean: clean}, nil
}

func (f *fakeGitGateway) CleanupWorktree(_ context.Context, input CleanupWorktreeInput) error {
	f.cleanupCalls = append(f.cleanupCalls, input)
	return nil
}

type fakeAgentExecutor struct {
	results []AgentResult
	starts  []AgentRunInput
	waitErr error
	wait    func(context.Context) error
}

func (f *fakeAgentExecutor) Start(_ context.Context, input AgentRunInput) (AgentExecution, error) {
	f.starts = append(f.starts, input)
	if len(f.results) == 0 {
		return nil, fmt.Errorf("no queued agent result")
	}
	result := f.results[0]
	f.results = f.results[1:]
	return fakeAgentExecution{result: result, waitErr: f.waitErr, wait: f.wait}, nil
}

type fakeAgentExecution struct {
	result  AgentResult
	waitErr error
	wait    func(context.Context) error
}

func (f fakeAgentExecution) Wait(ctx context.Context) (AgentResult, error) {
	if f.wait != nil {
		if err := f.wait(ctx); err != nil {
			return AgentResult{}, err
		}
	}
	if f.waitErr != nil {
		return AgentResult{}, f.waitErr
	}
	if f.result.Status == "completed" && f.result.ParseStatus == "" && strings.Contains(f.result.Stdout, "__LOOPER_RESULT__=") {
		f.result.ParseStatus = "parsed"
	}
	return f.result, nil
}

type testLogger struct{}

func (*testLogger) Debug(string, map[string]any) {}
func (*testLogger) Info(string, map[string]any)  {}
func (*testLogger) Warn(string, map[string]any)  {}
func (*testLogger) Error(string, map[string]any) {}

func contains(haystack, needle string) bool { return strings.Contains(haystack, needle) }
