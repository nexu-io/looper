package planner

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/powerformer/looper/internal/config"
	"github.com/powerformer/looper/internal/infra/specpr"
	"github.com/powerformer/looper/internal/storage"
)

func TestDiscoverIssuesEnqueuesEligibleWorkAndCreatesLoop(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{issues: []IssueSummary{{Number: 42, Title: "Plan this", Assignees: []string{"octocat"}, Labels: []string{"looper:plan"}}, {Number: 43, Title: "Skip", Assignees: []string{"someone"}, Labels: []string{"looper:plan"}}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now, AllowAutoPush: boolPtr(true)})

	result, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	if len(result.QueueItems) != 1 || len(result.CreatedLoopIDs) != 1 {
		t.Fatalf("result = %#v, want one queue item and one loop", result)
	}
	queue, err := fixture.repos.Queue.GetByID(context.Background(), result.QueueItems[0].ID)
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	if queue == nil || queue.Type != "planner" || queue.DedupeKey != "planner:project_1:"+result.CreatedLoopIDs[0]+":acme/looper:42" {
		t.Fatalf("queue = %#v, want planner queue for issue 42", queue)
	}
}

func TestDiscoverIssuesEnqueuesAcrossProjectsForSameIssue(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	nowISO := fixture.nowISO()
	baseBranch := "main"
	metadata := `{"repo":"acme/looper"}`
	if err := fixture.repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "project_2", Name: "Looper Duplicate", RepoPath: filepath.Join(t.TempDir(), "repo-2"), BaseBranch: &baseBranch, MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert(project_2) error = %v", err)
	}
	issue := IssueSummary{Number: 42, Title: "Plan this", Assignees: []string{"octocat"}, Labels: []string{"looper:plan"}}
	project1Loop, err := fixture.repos.Loops.GetByID(context.Background(), "missing")
	if err != nil || project1Loop != nil {
		t.Fatalf("Loops.GetByID(missing) = (%#v, %v), want (nil, nil)", project1Loop, err)
	}
	loopResult, err := (&Runner{repos: fixture.repos, now: fixture.now}).ensureLoopForIssue(context.Background(), storage.ProjectRecord{ID: "project_1"}, "acme/looper", issue)
	if err != nil {
		t.Fatalf("ensureLoopForIssue(project_1) error = %v", err)
	}
	project1Queue := storage.QueueItemRecord{ID: "queue_existing", ProjectID: stringPtr("project_1"), LoopID: &loopResult.record.ID, Type: "planner", TargetType: "issue", TargetID: buildIssueTargetID("acme/looper", issue.Number), Repo: stringPtr("acme/looper"), DedupeKey: buildPlannerDedupeKey("project_1", loopResult.record.ID, "acme/looper", issue.Number), Priority: storage.QueuePriorityPlanner, Status: "queued", AvailableAt: nowISO, Attempts: 0, MaxAttempts: 3, LockKey: stringPtr(buildIssueLockKey("acme/looper", issue.Number)), CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Queue.Upsert(context.Background(), project1Queue); err != nil {
		t.Fatalf("Queue.Upsert(existing) error = %v", err)
	}

	github := &fakeGitHubGateway{issues: []IssueSummary{issue}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now, AllowAutoPush: boolPtr(true)})
	result, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_2", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	if len(result.QueueItems) != 1 || len(result.CreatedLoopIDs) != 1 {
		t.Fatalf("result = %#v, want one queue item and one loop", result)
	}
	queue, err := fixture.repos.Queue.GetByID(context.Background(), result.QueueItems[0].ID)
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	if queue == nil {
		t.Fatal("Queue.GetByID() = nil, want created queue")
	}
	if queue.DedupeKey != "planner:project_2:"+result.CreatedLoopIDs[0]+":acme/looper:42" {
		t.Fatalf("queue.DedupeKey = %q, want project-scoped dedupe key", queue.DedupeKey)
	}
	allQueueItems, err := fixture.repos.Queue.List(context.Background())
	if err != nil {
		t.Fatalf("Queue.List() error = %v", err)
	}
	if len(allQueueItems) != 2 {
		t.Fatalf("len(Queue.List()) = %d, want 2", len(allQueueItems))
	}
}

func TestProcessClaimedItemManualPlannerBypassesDiscoveryChecks(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	issue := IssueSummary{Number: 42, Title: "Plan this"}
	loopResult, err := (&Runner{repos: fixture.repos, now: fixture.now}).ensureLoopForIssue(context.Background(), storage.ProjectRecord{ID: "project_1"}, "acme/looper", issue)
	if err != nil {
		t.Fatalf("ensureLoopForIssue() error = %v", err)
	}
	queueItem, err := (&Runner{repos: fixture.repos, now: fixture.now, retryMaxAttempts: 3}).enqueue(context.Background(), enqueueInput{ProjectID: "project_1", LoopID: loopResult.record.ID, Repo: "acme/looper", IssueNumber: issue.Number, Payload: map[string]any{"issueNumber": issue.Number, "manual": true}})
	if err != nil {
		t.Fatalf("enqueue() error = %v", err)
	}
	github := &fakeGitHubGateway{issueDetail: IssueDetail{Number: 42, Title: "Plan this", Assignees: []string{"someone"}, Labels: []string{"not-looper:plan"}, Body: "details", URL: "https://example/issues/42"}}
	git := &fakeGitGateway{createResult: CreateWorktreeResult{ID: "worktree_1", WorktreePath: filepath.Join(t.TempDir(), "wt"), Branch: "looper/planner/42-plan-this", BaseBranch: "main"}}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "wrote spec"}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, AllowAutoPush: boolPtr(true)})
	github.createPRResult = CreatePullRequestResult{Number: 101, URL: "https://example/pr/101"}

	claimed, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "planner-worker-1", "planner")
	if err != nil || claimed == nil {
		t.Fatalf("ClaimNextOfType() = (%#v, %v), want claimed item", claimed, err)
	}
	if claimed.ID != queueItem.ID {
		t.Fatalf("claimed.ID = %q, want %q", claimed.ID, queueItem.ID)
	}
	result, err := runner.ProcessClaimedItem(context.Background(), *claimed)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "success" {
		t.Fatalf("result = %#v, want success", result)
	}
	if result.PullRequestNumber != 101 {
		t.Fatalf("result.PullRequestNumber = %d, want 101", result.PullRequestNumber)
	}
	if len(agent.starts) != 1 {
		t.Fatalf("agent starts = %d, want 1", len(agent.starts))
	}
}

func TestProcessClaimedItemPlannerSkipsWhenNotManualAndIneligible(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	issue := IssueSummary{Number: 42, Title: "Plan this"}
	loopResult, err := (&Runner{repos: fixture.repos, now: fixture.now}).ensureLoopForIssue(context.Background(), storage.ProjectRecord{ID: "project_1"}, "acme/looper", issue)
	if err != nil {
		t.Fatalf("ensureLoopForIssue() error = %v", err)
	}
	queueItem, err := (&Runner{repos: fixture.repos, now: fixture.now, retryMaxAttempts: 3}).enqueue(context.Background(), enqueueInput{ProjectID: "project_1", LoopID: loopResult.record.ID, Repo: "acme/looper", IssueNumber: issue.Number, Payload: map[string]any{"issueNumber": issue.Number}})
	if err != nil {
		t.Fatalf("enqueue() error = %v", err)
	}
	github := &fakeGitHubGateway{issueDetail: IssueDetail{Number: 42, Title: "Plan this", Assignees: []string{"someone"}, Labels: []string{"not-looper:plan"}, Body: "details", URL: "https://example/issues/42"}}
	git := &fakeGitGateway{createResult: CreateWorktreeResult{ID: "worktree_1", WorktreePath: filepath.Join(t.TempDir(), "wt"), Branch: "looper/planner/42-plan-this", BaseBranch: "main"}}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "wrote spec"}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, AllowAutoPush: boolPtr(true)})

	claimed, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "planner-worker-1", "planner")
	if err != nil || claimed == nil {
		t.Fatalf("ClaimNextOfType() = (%#v, %v), want claimed item", claimed, err)
	}
	if claimed.ID != queueItem.ID {
		t.Fatalf("claimed.ID = %q, want %q", claimed.ID, queueItem.ID)
	}
	result, err := runner.ProcessClaimedItem(context.Background(), *claimed)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "skipped" {
		t.Fatalf("result = %#v, want skipped", result)
	}
	if result.FailureKind != "" {
		t.Fatalf("result.FailureKind = %q, want empty", result.FailureKind)
	}
	if len(agent.starts) != 0 {
		t.Fatalf("len(agent.starts) = %d, want 0", len(agent.starts))
	}
}

func TestProcessClaimedItemDiscoveryQueueIgnoresManualLoopMetadata(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	issue := IssueSummary{Number: 42, Title: "Plan this"}
	loopResult, err := (&Runner{repos: fixture.repos, now: fixture.now}).ensureLoopForIssue(context.Background(), storage.ProjectRecord{ID: "project_1"}, "acme/looper", issue)
	if err != nil {
		t.Fatalf("ensureLoopForIssue() error = %v", err)
	}
	loop := loopResult.record
	loop.MetadataJSON = stringPtr(`{"manual":true,"issueNumber":42}`)
	if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	queueItem, err := (&Runner{repos: fixture.repos, now: fixture.now, retryMaxAttempts: 3}).enqueue(context.Background(), enqueueInput{ProjectID: "project_1", LoopID: loop.ID, Repo: "acme/looper", IssueNumber: issue.Number, Payload: map[string]any{"issueNumber": issue.Number}})
	if err != nil {
		t.Fatalf("enqueue() error = %v", err)
	}
	github := &fakeGitHubGateway{issueDetail: IssueDetail{Number: 42, Title: "Plan this", Assignees: []string{"someone"}, Labels: []string{"not-looper:plan"}, Body: "details", URL: "https://example/issues/42"}}
	agent := &fakeAgentExecutor{}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, AllowAutoPush: boolPtr(true)})

	claimed, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "planner-worker-1", "planner")
	if err != nil || claimed == nil {
		t.Fatalf("ClaimNextOfType() = (%#v, %v), want claimed item", claimed, err)
	}
	if claimed.ID != queueItem.ID {
		t.Fatalf("claimed.ID = %q, want %q", claimed.ID, queueItem.ID)
	}
	result, err := runner.ProcessClaimedItem(context.Background(), *claimed)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "skipped" {
		t.Fatalf("result = %#v, want skipped", result)
	}
	if len(agent.starts) != 0 {
		t.Fatalf("len(agent.starts) = %d, want 0", len(agent.starts))
	}
}

func TestProcessClaimedItemSuccessfulPlannerPublish(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{issues: []IssueSummary{{Number: 42, Title: "Plan this", Assignees: []string{"octocat"}, Labels: []string{"looper:plan"}}}, issueDetail: IssueDetail{Number: 42, Title: "Plan this", Body: "details", URL: "https://example/issues/42", Assignees: []string{"octocat"}, Labels: []string{"looper:plan"}}, createPRResult: CreatePullRequestResult{Number: 101, URL: "https://example/pr/101"}}
	git := &fakeGitGateway{createResult: CreateWorktreeResult{ID: "worktree_1", WorktreePath: filepath.Join(t.TempDir(), "wt"), Branch: "looper/planner/42-plan-this", BaseBranch: "main"}}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "wrote spec"}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, AllowAutoPush: boolPtr(true)})

	_, _ = runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "planner-worker-1", "planner")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNextOfType() = (%#v, %v), want claimed item", claim, err)
	}
	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "success" || result.PullRequestNumber != 101 {
		t.Fatalf("result = %#v, want success with PR 101", result)
	}
	if len(agent.starts) != 1 || len(git.pushCalls) != 1 || len(github.createPRCalls) != 1 {
		t.Fatalf("agent starts=%d push=%d createPR=%d, want 1/1/1", len(agent.starts), len(git.pushCalls), len(github.createPRCalls))
	}
	if len(github.addLabelCalls) != 1 || len(github.addLabelCalls[0].Labels) != 1 || github.addLabelCalls[0].Labels[0] != specpr.ReviewingLabel {
		t.Fatalf("addLabelCalls = %#v, want spec-reviewing label", github.addLabelCalls)
	}
	if got := github.createPRCalls[0].Body; !strings.Contains(got, "\nSpec: ") {
		t.Fatalf("createPR body = %q, want Spec path line", got)
	}
	if !strings.Contains(agent.starts[0].Prompt, "When finished, print exactly one final line to stdout in this format:") {
		t.Fatalf("prompt = %q, want completion instruction", agent.starts[0].Prompt)
	}
	if !strings.Contains(agent.starts[0].Prompt, `__LOOPER_RESULT__={"summary":"<one-sentence summary>"}`) {
		t.Fatalf("prompt = %q, want canonical completion marker", agent.starts[0].Prompt)
	}
	loop, err := fixture.repos.Loops.GetByID(context.Background(), result.LoopID)
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if loop == nil || loop.Status != "completed" || loop.MetadataJSON == nil || !strings.Contains(*loop.MetadataJSON, `"prUrl":"https://example/pr/101"`) {
		t.Fatalf("loop = %#v, want completed loop with prUrl metadata", loop)
	}
	run, err := fixture.repos.Runs.GetByID(context.Background(), result.RunID)
	if err != nil {
		t.Fatalf("Runs.GetByID() error = %v", err)
	}
	if run == nil || run.CheckpointJSON == nil || !strings.Contains(*run.CheckpointJSON, `"id":"worktree_1"`) {
		t.Fatalf("run = %#v, want checkpoint with worktree id", run)
	}
}

func TestPublishResumeDoesNotRerunPriorSteps(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{issues: []IssueSummary{{Number: 42, Title: "Plan this", Assignees: []string{"octocat"}, Labels: []string{"looper:plan"}}}, issueDetail: IssueDetail{Number: 42, Title: "Plan this", URL: "https://example/issues/42", Assignees: []string{"octocat"}, Labels: []string{"looper:plan"}}, createPRResult: CreatePullRequestResult{Number: 101, URL: "https://example/pr/101"}, createPRErrors: []error{fmt.Errorf("temporary create pr failure")}}
	git := &fakeGitGateway{createResult: CreateWorktreeResult{ID: "worktree_1", WorktreePath: filepath.Join(t.TempDir(), "wt"), Branch: "looper/planner/42-plan-this", BaseBranch: "main"}}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "wrote spec"}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, AllowAutoPush: boolPtr(true)})

	_, _ = runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	firstClaim, _ := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "planner-worker-1", "planner")
	first, err := runner.ProcessClaimedItem(context.Background(), *firstClaim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem(first) error = %v", err)
	}
	if first.Status != "failed" || first.FailureKind != FailureRetryableAfterResume {
		t.Fatalf("first result = %#v, want retryable_after_resume failure", first)
	}
	fixture.advance(5 * time.Second)
	retryClaim, _ := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "planner-worker-1", "planner")
	second, err := runner.ProcessClaimedItem(context.Background(), *retryClaim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem(retry) error = %v", err)
	}
	if second.Status != "success" {
		t.Fatalf("retry result = %#v, want success", second)
	}
	if len(agent.starts) != 1 {
		t.Fatalf("len(agent.starts) = %d, want 1 (write-spec not rerun)", len(agent.starts))
	}
	if len(git.pushCalls) != 1 {
		t.Fatalf("len(git.pushCalls) = %d, want 1 (push not rerun)", len(git.pushCalls))
	}
}

func TestProcessClaimedItemResumeReleasesClaimedLockWhenSetupFails(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	issue := IssueSummary{Number: 42, Title: "Plan this", Assignees: []string{"octocat"}, Labels: []string{"looper:plan"}}
	loopResult, err := (&Runner{repos: fixture.repos, now: fixture.now}).ensureLoopForIssue(context.Background(), storage.ProjectRecord{ID: "project_1"}, "acme/looper", issue)
	if err != nil {
		t.Fatalf("ensureLoopForIssue() error = %v", err)
	}
	queueItem, err := (&Runner{repos: fixture.repos, now: fixture.now, retryMaxAttempts: 3}).enqueue(context.Background(), enqueueInput{ProjectID: "project_1", LoopID: loopResult.record.ID, Repo: "acme/looper", IssueNumber: issue.Number, Payload: map[string]any{"issueNumber": issue.Number}})
	if err != nil {
		t.Fatalf("enqueue() error = %v", err)
	}
	checkpointJSON := `{"claimedLockKey":"` + buildIssueLockKey("acme/looper", issue.Number) + `"}`
	if err := fixture.repos.Runs.Upsert(context.Background(), storage.RunRecord{ID: "run_failed_resume", LoopID: loopResult.record.ID, Status: "failed", LastCompletedStep: stringPtr(string(stepDiscoverIssues)), CheckpointJSON: &checkpointJSON, StartedAt: fixture.nowISO(), CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}
	if _, err := fixture.coordinator.DB().ExecContext(context.Background(), `
		CREATE TRIGGER loops_fail_running_resume_planner
		BEFORE UPDATE ON loops
		FOR EACH ROW
		WHEN NEW.id = '`+loopResult.record.ID+`' AND NEW.status = 'running'
		BEGIN
			SELECT RAISE(FAIL, 'forced loop update failure');
		END;
	`); err != nil {
		t.Fatalf("create trigger error = %v", err)
	}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: &fakeGitHubGateway{}, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "wrote spec"}}}, Logger: fixture.logger, Now: fixture.now, AllowAutoPush: boolPtr(true)})

	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "planner-worker-1", "planner")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNextOfType() = (%#v, %v), want claimed item", claim, err)
	}
	if claim.ID != queueItem.ID {
		t.Fatalf("claim.ID = %q, want %q", claim.ID, queueItem.ID)
	}
	_, err = runner.ProcessClaimedItem(context.Background(), *claim)
	if err == nil || !strings.Contains(err.Error(), "forced loop update failure") {
		t.Fatalf("ProcessClaimedItem() error = %v, want forced loop update failure", err)
	}
	lockKey := buildIssueLockKey("acme/looper", issue.Number)
	lock, err := fixture.repos.Locks.Get(context.Background(), lockKey)
	if err != nil {
		t.Fatalf("Locks.Get() error = %v", err)
	}
	if lock != nil {
		t.Fatalf("lock = %#v, want released claimed lock", lock)
	}
	acquired, err := fixture.repos.Locks.Acquire(context.Background(), storage.LockRecord{Key: lockKey, Owner: "retry", ExpiresAt: fixture.now().Add(time.Minute).UTC().Format("2006-01-02T15:04:05.000Z"), CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()})
	if err != nil {
		t.Fatalf("Locks.Acquire() error = %v", err)
	}
	if !acquired {
		t.Fatal("Locks.Acquire() = false, want claimed lock to be immediately reacquirable")
	}
}

func TestWriteSpecFailureMarksRunQueueLoop(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{issues: []IssueSummary{{Number: 42, Title: "Plan this", Assignees: []string{"octocat"}, Labels: []string{"looper:plan"}}}, issueDetail: IssueDetail{Number: 42, Title: "Plan this", Assignees: []string{"octocat"}, Labels: []string{"looper:plan"}}}
	git := &fakeGitGateway{createResult: CreateWorktreeResult{ID: "worktree_1", WorktreePath: filepath.Join(t.TempDir(), "wt"), Branch: "looper/planner/42-plan-this", BaseBranch: "main"}}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "failed", Summary: "agent failed"}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, AllowAutoPush: boolPtr(true)})

	_, _ = runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	claim, _ := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "planner-worker-1", "planner")
	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "failed" || result.FailureKind != FailureRetryableTransient {
		t.Fatalf("result = %#v, want retryable_transient failure", result)
	}
	run, err := fixture.repos.Runs.GetByID(context.Background(), result.RunID)
	if err != nil {
		t.Fatalf("Runs.GetByID() error = %v", err)
	}
	if run == nil || run.Status != "failed" || run.CurrentStep == nil || *run.CurrentStep != string(stepWriteSpec) {
		t.Fatalf("run = %#v, want failed run on write-spec", run)
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
	if loop == nil || loop.Status != "queued" {
		t.Fatalf("loop = %#v, want queued for retry", loop)
	}
}

func TestProcessClaimedItemPreservesPausedLoopOnRetryableFailureAfterPause(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{issues: []IssueSummary{{Number: 42, Title: "Plan this", Assignees: []string{"octocat"}, Labels: []string{"looper:plan"}}}, issueDetail: IssueDetail{Number: 42, Title: "Plan this", Body: "details", URL: "https://example/issues/42", Assignees: []string{"octocat"}, Labels: []string{"looper:plan"}}}
	git := &fakeGitGateway{createResult: CreateWorktreeResult{ID: "worktree_1", WorktreePath: filepath.Join(t.TempDir(), "wt"), Branch: "looper/planner/42-plan-this", BaseBranch: "main"}}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "wrote spec"}}, wait: func(ctx context.Context) error {
		items, err := fixture.repos.Queue.List(ctx)
		if err != nil {
			return err
		}
		loopID := ""
		for _, item := range items {
			if item.Type == "planner" && item.Status == "running" && item.LoopID != nil {
				loopID = *item.LoopID
				break
			}
		}
		if loopID == "" {
			return fmt.Errorf("running planner queue item not found")
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
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, AllowAutoPush: boolPtr(true)})

	if _, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "planner-worker-1", "planner")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNextOfType() = (%#v, %v), want claimed item", claim, err)
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

func TestProcessNextSetupFailureMarksQueueFailed(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	projectID := "project_1"
	nowISO := fixture.nowISO()
	if err := fixture.repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: "queue_missing_planner", ProjectID: &projectID, Type: "planner", TargetType: "issue", TargetID: "issue:acme/looper:99", DedupeKey: "planner:acme/looper:99", Priority: 1, Status: "queued", AvailableAt: nowISO, MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: &fakeGitHubGateway{}, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})

	result, err := runner.ProcessNext(context.Background(), "planner-worker-1")
	if err != nil {
		t.Fatalf("ProcessNext() error = %v", err)
	}
	if result == nil || result.Status != "failed" || result.FailureKind != FailureNonRetryable || !strings.Contains(result.Summary, "requires loopId") {
		t.Fatalf("result = %#v, want non-retryable missing-loopId failure", result)
	}
	queue, err := fixture.repos.Queue.GetByID(context.Background(), "queue_missing_planner")
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	if queue == nil || queue.Status != "failed" {
		t.Fatalf("queue = %#v, want failed", queue)
	}
}

func TestRecoverClaimedItemReconcilesRunningLoopState(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	nowISO := fixture.nowISO()
	loopTarget := "issue:acme/looper:42"
	if err := fixture.repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: "loop_planner_running", Seq: 1, ProjectID: "project_1", Type: "planner", TargetType: "issue", TargetID: &loopTarget, Repo: stringPtr("acme/looper"), Status: "running", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	projectID := "project_1"
	loopID := "loop_planner_running"
	payload := `{"issueNumber":42}`
	if err := fixture.repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: "queue_planner_running", ProjectID: &projectID, LoopID: &loopID, Type: "planner", TargetType: "issue", TargetID: loopTarget, Repo: stringPtr("acme/looper"), DedupeKey: "planner:acme/looper:42", Priority: 1, Status: "running", AvailableAt: nowISO, Attempts: 0, MaxAttempts: 1, PayloadJSON: &payload, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: &fakeGitHubGateway{}, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})

	result, err := runner.recoverClaimedItem(context.Background(), storage.QueueItemRecord{ID: "queue_planner_running", ProjectID: &projectID, LoopID: &loopID, Type: "planner", TargetType: "issue", TargetID: loopTarget, Repo: stringPtr("acme/looper"), DedupeKey: "planner:acme/looper:42", Priority: 1, Status: "running", AvailableAt: nowISO, Attempts: 0, MaxAttempts: 1, PayloadJSON: &payload, CreatedAt: nowISO, UpdatedAt: nowISO}, fmt.Errorf("persist step failed"))
	if err != nil {
		t.Fatalf("recoverClaimedItem() error = %v", err)
	}
	if result == nil || result.Status != "failed" || result.FailureKind != FailureNonRetryable {
		t.Fatalf("result = %#v, want failed non-retryable recovery", result)
	}
	queue, err := fixture.repos.Queue.GetByID(context.Background(), "queue_planner_running")
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	if queue == nil || queue.Status != "failed" {
		t.Fatalf("queue = %#v, want failed", queue)
	}
	loop, err := fixture.repos.Loops.GetByID(context.Background(), loopID)
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if loop == nil || loop.Status != "failed" || loop.NextRunAt != nil {
		t.Fatalf("loop = %#v, want failed terminal loop", loop)
	}
}

func TestProcessClaimedItemUsesDefaultProjectWorktreeRootWhenProjectMetadataOmitsIt(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	project, err := fixture.repos.Projects.GetByID(context.Background(), "project_1")
	if err != nil {
		t.Fatalf("Projects.GetByID() error = %v", err)
	}
	if project == nil {
		t.Fatal("project missing")
	}
	metadata := `{"repo":"acme/looper"}`
	project.MetadataJSON = &metadata
	project.UpdatedAt = fixture.nowISO()
	if err := fixture.repos.Projects.Upsert(context.Background(), *project); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	github := &fakeGitHubGateway{issues: []IssueSummary{{Number: 42, Title: "Plan this", Assignees: []string{"octocat"}, Labels: []string{"looper:plan"}}}, issueDetail: IssueDetail{Number: 42, Title: "Plan this", Body: "details", URL: "https://example/issues/42", Assignees: []string{"octocat"}, Labels: []string{"looper:plan"}}}
	git := &fakeGitGateway{createResult: CreateWorktreeResult{ID: "worktree_1", WorktreePath: filepath.Join(t.TempDir(), "wt"), Branch: "looper/planner/42-plan-this", BaseBranch: "main"}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git, AgentExecutor: &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "wrote spec"}}}, Logger: fixture.logger, Now: fixture.now, AllowAutoPush: boolPtr(true)})

	_, _ = runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "planner-worker-1", "planner")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNextOfType() = (%#v, %v), want claimed item", claim, err)
	}
	if _, err := runner.ProcessClaimedItem(context.Background(), *claim); err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	wantRoot, err := config.DefaultProjectWorktreeRoot(project.ID, project.RepoPath)
	if err != nil {
		t.Fatalf("DefaultProjectWorktreeRoot() error = %v", err)
	}
	if len(git.createCalls) == 0 || git.createCalls[0].WorktreeRoot != wantRoot {
		t.Fatalf("CreateWorktree().WorktreeRoot = %#v, want %q", git.createCalls, wantRoot)
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
	coordinator, err := storage.OpenSQLiteCoordinator(context.Background(), filepath.Join(t.TempDir(), "planner.sqlite"), storage.SQLiteCoordinatorOptions{BackupDir: t.TempDir()})
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
	metadata := `{"repo":"acme/looper"}`
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "project_1", Name: "Looper", RepoPath: filepath.Join(t.TempDir(), "repo"), BaseBranch: &baseBranch, MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
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
	issues           []IssueSummary
	issueDetail      IssueDetail
	createPRResult   CreatePullRequestResult
	createPRErrors   []error
	createPRIndex    int
	createPRCalls    []CreatePullRequestInput
	addLabelCalls    []PullRequestLabelsInput
	addReviewerCalls []PullRequestReviewersInput
}

func (f *fakeGitHubGateway) ListOpenIssues(context.Context, ListOpenIssuesInput) ([]IssueSummary, error) {
	return append([]IssueSummary(nil), f.issues...), nil
}

func (f *fakeGitHubGateway) ViewIssue(_ context.Context, input ViewIssueInput) (IssueDetail, error) {
	detail := f.issueDetail
	if detail.Number == 0 {
		detail.Number = input.IssueNumber
	}
	if detail.Title == "" {
		detail.Title = "Issue"
	}
	return detail, nil
}

func (*fakeGitHubGateway) GetCurrentUserLogin(context.Context, string) (string, error) {
	return "octocat", nil
}

func (f *fakeGitHubGateway) CreatePullRequest(_ context.Context, input CreatePullRequestInput) (CreatePullRequestResult, error) {
	f.createPRCalls = append(f.createPRCalls, input)
	if f.createPRIndex < len(f.createPRErrors) && f.createPRErrors[f.createPRIndex] != nil {
		err := f.createPRErrors[f.createPRIndex]
		f.createPRIndex++
		return CreatePullRequestResult{}, err
	}
	f.createPRIndex++
	return f.createPRResult, nil
}

func (f *fakeGitHubGateway) AddPullRequestLabels(_ context.Context, input PullRequestLabelsInput) error {
	f.addLabelCalls = append(f.addLabelCalls, input)
	return nil
}

func (f *fakeGitHubGateway) AddPullRequestReviewers(_ context.Context, input PullRequestReviewersInput) error {
	f.addReviewerCalls = append(f.addReviewerCalls, input)
	return nil
}

type fakeGitGateway struct {
	createResult CreateWorktreeResult
	createCalls  []CreateWorktreeInput
	pushCalls    []PushInput
}

func (f *fakeGitGateway) CreateWorktree(_ context.Context, input CreateWorktreeInput) (CreateWorktreeResult, error) {
	f.createCalls = append(f.createCalls, input)
	result := f.createResult
	if result.WorktreePath == "" {
		result.WorktreePath = filepath.Join(input.WorktreeRoot, "wt")
	}
	if result.Branch == "" {
		result.Branch = input.Branch
	}
	if result.BaseBranch == "" {
		result.BaseBranch = input.BaseBranch
	}
	return result, nil
}

func (f *fakeGitGateway) Push(_ context.Context, input PushInput) error {
	f.pushCalls = append(f.pushCalls, input)
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
	return f.result, nil
}

type testLogger struct{}

func (*testLogger) Debug(string, map[string]any) {}
func (*testLogger) Info(string, map[string]any)  {}
func (*testLogger) Warn(string, map[string]any)  {}
func (*testLogger) Error(string, map[string]any) {}

func boolPtr(value bool) *bool { return &value }
