package worker

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/powerformer/looper/internal/config"
	"github.com/powerformer/looper/internal/storage"
)

func TestProcessNextIgnoresOtherQueueTypes(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	claimedWorker, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "worker-0", "worker")
	if err != nil || claimedWorker == nil {
		t.Fatalf("initial ClaimNextOfType() = (%#v, %v), want claimed worker", claimedWorker, err)
	}
	nowISO := fixture.nowISO()
	projectID := "project_1"
	loopID := "loop_planner_1"
	if err := fixture.repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 2, ProjectID: projectID, Type: "planner", TargetType: "issue", Status: "queued", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	if err := fixture.repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: "queue_planner_1", ProjectID: &projectID, LoopID: &loopID, Type: "planner", TargetType: "issue", TargetID: "issue:acme/looper:42", DedupeKey: "planner:acme/looper:42", Priority: 1, Status: "queued", AvailableAt: nowISO, MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: &fakeGitHubGateway{}, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})

	result, err := runner.ProcessNext(context.Background(), "worker-1")
	if err != nil {
		t.Fatalf("ProcessNext() error = %v", err)
	}
	if result != nil {
		t.Fatalf("ProcessNext() = %#v, want nil", result)
	}
}

func TestProcessClaimedItemCompletesCreatePRFlow(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	git := &fakeGitGateway{createResult: CreateWorktreeResult{WorktreePath: filepath.Join(t.TempDir(), "wt"), Branch: "looper/feature", BaseBranch: "main", HeadSHA: "abc123", WorktreeID: "worktree_1"}}
	github := &fakeGitHubGateway{createPRResult: CreatePullRequestResult{Number: 101, URL: "https://example/pr/101"}}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "done", Stdout: "ok", ParseStatus: "parsed"}}}
	completed := make([]RunCompletedInput, 0, 1)
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, AllowAutoCommit: true, AllowAutoPush: true, OpenPRStrategy: config.OpenPRStrategyAllDone, OnRunCompleted: func(_ context.Context, input RunCompletedInput) error {
		queue, err := fixture.repos.Queue.GetByID(context.Background(), "queue_worker_1")
		if err != nil {
			t.Fatalf("Queue.GetByID() in notification callback error = %v", err)
		}
		if queue == nil || queue.Status != "completed" {
			t.Fatalf("queue during notification = %#v, want completed queue item", queue)
		}
		loop, err := fixture.repos.Loops.GetByID(context.Background(), "loop_worker_1")
		if err != nil {
			t.Fatalf("Loops.GetByID() in notification callback error = %v", err)
		}
		if loop == nil || loop.Status != "completed" || loop.NextRunAt != nil {
			t.Fatalf("loop during notification = %#v, want completed loop", loop)
		}
		completed = append(completed, input)
		return nil
	}})

	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "worker-1", "worker")
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
	if len(completed) != 1 {
		t.Fatalf("len(completed) = %d, want 1", len(completed))
	}
	if completed[0].Status != "success" || completed[0].PullRequestNumber != 101 || completed[0].PullRequestURL != "https://example/pr/101" {
		t.Fatalf("completed[0] = %#v, want success with PR details", completed[0])
	}
	if !strings.Contains(agent.starts[0].Prompt, `__LOOPER_RESULT__={"summary":"<one-sentence summary>"}`) {
		t.Fatalf("prompt = %q, want canonical completion instruction", agent.starts[0].Prompt)
	}
	loop, err := fixture.repos.Loops.GetByID(context.Background(), result.LoopID)
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if loop == nil || loop.Status != "completed" || loop.TargetType != "pull_request" || loop.TargetID == nil || *loop.TargetID != "pr:acme/looper:101" || loop.PRNumber == nil || *loop.PRNumber != 101 || loop.MetadataJSON == nil || !strings.Contains(*loop.MetadataJSON, `"prNumber":101`) {
		t.Fatalf("loop = %#v, want completed loop with PR metadata", loop)
	}
	queue, err := fixture.repos.Queue.GetByID(context.Background(), "queue_worker_1")
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	if queue == nil || queue.TargetType != "pull_request" || queue.TargetID != "pr:acme/looper:101" || queue.LockKey == nil || *queue.LockKey != "pr:acme/looper:101" || queue.PRNumber == nil || *queue.PRNumber != 101 {
		t.Fatalf("queue = %#v, want retargeted queue item", queue)
	}
	run, err := fixture.repos.Runs.GetByID(context.Background(), result.RunID)
	if err != nil {
		t.Fatalf("Runs.GetByID() error = %v", err)
	}
	if run == nil || run.Status != "success" || run.LastCompletedStep == nil || *run.LastCompletedStep != string(stepOpenPR) {
		t.Fatalf("run = %#v, want success through open-pr", run)
	}
	worktrees, err := fixture.repos.Worktrees.ListByProject(context.Background(), "project_1")
	if err != nil {
		t.Fatalf("Worktrees.ListByProject() error = %v", err)
	}
	if len(worktrees) != 0 {
		t.Fatalf("Worktrees.ListByProject() = %#v, want no direct runner upsert with fake gateway", worktrees)
	}
	lock, err := fixture.repos.Locks.Get(context.Background(), "worker:loop_worker_1")
	if err != nil {
		t.Fatalf("Locks.Get() error = %v", err)
	}
	if lock != nil {
		t.Fatalf("lock = %#v, want prepare-work lock released after successful run", lock)
	}
}

func TestBuildWorkerPromptDisablesRemoteLifecycleWhenAgentPRCreationDisabled(t *testing.T) {
	t.Parallel()

	prompt, err := buildWorkerPrompt("", workerInput{Repo: "acme/looper", Title: "fix bug", Branch: "looper/fix", BaseBranch: "main"}, nil, false)
	if err != nil {
		t.Fatalf("buildWorkerPrompt() error = %v", err)
	}
	for _, unwanted := range []string{
		"adopt the existing pull request",
		"reuse it and preserve human-edited title/body",
		"adding only missing labels, reviewers, or closing references",
	} {
		if strings.Contains(prompt, unwanted) {
			t.Fatalf("prompt contains remote PR instruction %q:\n%s", unwanted, prompt)
		}
	}
	for _, want := range []string{
		"remote actions disabled by Looper configuration",
		"do not push branches, create pull requests, update pull request metadata, or otherwise change remote review state",
		`expectPush=false expectPR=false`,
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestProcessClaimedItemFailsWhenAgentCompletionResultMissing(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	git := &fakeGitGateway{createResult: CreateWorktreeResult{WorktreePath: filepath.Join(t.TempDir(), "wt"), Branch: "looper/feature", BaseBranch: "main", HeadSHA: "abc123", WorktreeID: "worktree_1"}}
	github := &fakeGitHubGateway{}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "upstream server_error", Stdout: "server_error", ParseStatus: "missing"}}}
	validationCalls := 0
	completed := make([]RunCompletedInput, 0, 1)
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, AllowAutoCommit: true, AllowAutoPush: true, OpenPRStrategy: config.OpenPRStrategyAllDone, ValidationRunner: func(context.Context, ValidationInput) (ValidationResult, error) {
		validationCalls++
		return ValidationResult{Passed: true, Summary: "ok"}, nil
	}, OnRunCompleted: func(_ context.Context, input RunCompletedInput) error {
		completed = append(completed, input)
		return nil
	}})

	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "worker-1", "worker")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNextOfType() = (%#v, %v), want claimed item", claim, err)
	}
	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "failed" || result.FailureKind != FailureRetryableTransient || !strings.Contains(result.Summary, "server_error") {
		t.Fatalf("result = %#v, want retryable failed result with upstream error", result)
	}
	if validationCalls != 0 {
		t.Fatalf("validationCalls = %d, want execute failure to stop before validation", validationCalls)
	}
	if len(git.pushCalls) != 0 || len(github.createPRCalls) != 0 {
		t.Fatalf("push calls=%d createPR calls=%d, want 0/0 after invalid agent completion", len(git.pushCalls), len(github.createPRCalls))
	}
	if len(completed) != 0 {
		t.Fatalf("len(completed) = %d, want no completion notification for retryable failure", len(completed))
	}
	loop, err := fixture.repos.Loops.GetByID(context.Background(), "loop_worker_1")
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if loop == nil || loop.Status != "queued" {
		t.Fatalf("loop = %#v, want queued loop for retryable failure", loop)
	}
	run, err := fixture.repos.Runs.GetByID(context.Background(), result.RunID)
	if err != nil {
		t.Fatalf("Runs.GetByID() error = %v", err)
	}
	if run == nil || run.Status != "failed" || run.CurrentStep == nil || *run.CurrentStep != string(stepExecute) {
		t.Fatalf("run = %#v, want failed run at execute step", run)
	}
	if run.LastCompletedStep != nil && *run.LastCompletedStep == string(stepOpenPR) {
		t.Fatalf("run = %#v, want open-pr to remain incomplete", run)
	}
}

func TestRunExecuteStepFailsResumedCompletedCheckpointWithoutParsedResult(t *testing.T) {
	t.Parallel()

	runner := New(Options{})
	checkpoint, err := runner.runExecuteStep(context.Background(), stepInput{
		Checkpoint: workerCheckpoint{
			Execution: &checkpointExecution{
				Status:      "completed",
				Summary:     "upstream server_error",
				ParseStatus: "",
			},
		},
	})
	if err == nil {
		t.Fatalf("runExecuteStep() error = nil, want parse-status failure")
	}
	if checkpoint.Execution == nil || checkpoint.Execution.Status != "completed" {
		t.Fatalf("checkpoint.Execution = %#v, want completed checkpoint preserved", checkpoint.Execution)
	}
	var loopErr *loopError
	if !errors.As(err, &loopErr) {
		t.Fatalf("error = %T, want *loopError", err)
	}
	if loopErr.kind != FailureRetryableTransient {
		t.Fatalf("loopErr.kind = %v, want %v", loopErr.kind, FailureRetryableTransient)
	}
	if !strings.Contains(err.Error(), "server_error") {
		t.Fatalf("error = %q, want upstream summary", err.Error())
	}
}

func TestCreateRunContextReplaysExecuteWhenResumeCheckpointParseStatusIsInvalid(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, Logger: fixture.logger, Now: fixture.now})
	checkpointJSON := mustMarshalJSON(workerCheckpoint{
		Work:           &workerInput{Title: "Worker task"},
		ClaimedLockKey: "worker:loop_worker_1",
		Worktree:       &checkpointWorktree{ID: "wt_1", Path: filepath.Join(t.TempDir(), "wt"), Branch: "feature/test"},
		Plan:           &checkpointPlan{Summary: "plan"},
		Execution:      &checkpointExecution{Status: "completed", Summary: "upstream server_error"},
		Validation:     &ValidationResult{Passed: true, Summary: "stale"},
		PullRequest:    &checkpointPullPR{Number: 101, URL: "https://example/pr/101"},
		SkipReason:     "stale skip reason",
	})
	if err := fixture.repos.Runs.Upsert(context.Background(), storage.RunRecord{
		ID:                "run_failed_after_validate",
		LoopID:            "loop_worker_1",
		Status:            "failed",
		CurrentStep:       stringPtr(string(stepValidate)),
		LastCompletedStep: stringPtr(string(stepExecute)),
		CheckpointJSON:    &checkpointJSON,
		StartedAt:         fixture.nowISO(),
		CreatedAt:         fixture.nowISO(),
		UpdatedAt:         fixture.nowISO(),
	}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}
	loop, err := fixture.repos.Loops.GetByID(context.Background(), "loop_worker_1")
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if loop == nil {
		t.Fatal("loop = nil, want worker loop")
	}

	resumed, err := runner.createRunContext(context.Background(), *loop)
	if err != nil {
		t.Fatalf("createRunContext() error = %v", err)
	}
	if !resumed.Resumed || resumed.StartStep != stepExecute {
		t.Fatalf("resumed = %#v, want resumed execute replay", resumed)
	}
	if resumed.Checkpoint.Execution != nil {
		t.Fatalf("Execution = %#v, want cleared execution checkpoint", resumed.Checkpoint.Execution)
	}
	if resumed.Checkpoint.Validation != nil {
		t.Fatalf("Validation = %#v, want cleared validation checkpoint", resumed.Checkpoint.Validation)
	}
	if resumed.Checkpoint.PullRequest != nil {
		t.Fatalf("PullRequest = %#v, want cleared pull request checkpoint", resumed.Checkpoint.PullRequest)
	}
	if resumed.Checkpoint.SkipReason != "" {
		t.Fatalf("SkipReason = %q, want cleared skip reason", resumed.Checkpoint.SkipReason)
	}
	if resumed.Checkpoint.Worktree == nil || resumed.Checkpoint.Plan == nil {
		t.Fatalf("checkpoint = %#v, want preserved worktree and plan", resumed.Checkpoint)
	}
	if resumed.Run.LastCompletedStep == nil || *resumed.Run.LastCompletedStep != string(stepPlan) {
		t.Fatalf("run.LastCompletedStep = %#v, want plan", resumed.Run.LastCompletedStep)
	}
}

func TestProcessClaimedQueueItemResumeValidationFailureUpdatesLoopState(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, Logger: fixture.logger, Now: fixture.now})
	checkpointJSON := mustMarshalJSON(workerCheckpoint{
		Execution: &checkpointExecution{
			Status:      "completed",
			Summary:     "upstream server_error",
			ParseStatus: "",
		},
	})
	if err := fixture.repos.Runs.Upsert(context.Background(), storage.RunRecord{
		ID:                "run_failed_after_execute",
		LoopID:            "loop_worker_1",
		Status:            "failed",
		LastCompletedStep: stringPtr(string(stepExecute)),
		CheckpointJSON:    &checkpointJSON,
		StartedAt:         fixture.nowISO(),
		CreatedAt:         fixture.nowISO(),
		UpdatedAt:         fixture.nowISO(),
	}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}
	queue, err := fixture.repos.Queue.GetByID(context.Background(), "queue_worker_1")
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	if queue == nil {
		t.Fatal("queue = nil, want queue record")
	}
	queue.MaxAttempts = 1
	if err := fixture.repos.Queue.Upsert(context.Background(), *queue); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "worker-1", "worker")
	if err != nil {
		t.Fatalf("ClaimNextOfType() error = %v", err)
	}
	if claim == nil {
		t.Fatal("claim = nil, want claimed queue item")
	}

	result, err := runner.ProcessClaimedQueueItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedQueueItem() error = %v", err)
	}
	if result == nil || result.Status != "failed" || result.FailureKind != FailureRetryableTransient {
		t.Fatalf("result = %#v, want failed retryable_transient result", result)
	}

	queue, err = fixture.repos.Queue.GetByID(context.Background(), "queue_worker_1")
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	if queue == nil || queue.Status != "failed" {
		t.Fatalf("queue = %#v, want failed terminal queue item", queue)
	}

	loop, err := fixture.repos.Loops.GetByID(context.Background(), "loop_worker_1")
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if loop == nil || loop.Status != "failed" || loop.NextRunAt != nil {
		t.Fatalf("loop = %#v, want failed terminal loop", loop)
	}
}

func TestBuildPullRequestBodyUsesCrossRepoClosingReference(t *testing.T) {
	t.Parallel()
	body := buildPullRequestBody(workerInput{Repo: "acme/looper", IssueRepo: "powerformer/looper", IssueNumber: 27, IssueURL: "https://github.com/powerformer/looper/issues/27"}, &checkpointPlan{Items: []string{"Add linked issue auto-close support"}}, &checkpointExecution{Summary: "done"})
	if !strings.Contains(body, "Issue: powerformer/looper#27") {
		t.Fatalf("body = %q, want issue repo reference", body)
	}
	if !strings.Contains(body, "Closes powerformer/looper#27") {
		t.Fatalf("body = %q, want cross-repo closing reference", body)
	}
	if strings.Contains(body, "Closes #27") {
		t.Fatalf("body = %q, want fully qualified cross-repo closing reference only", body)
	}
}

func TestBuildPullRequestBodyUsesBareClosingReferenceForSameRepo(t *testing.T) {
	t.Parallel()
	body := buildPullRequestBody(workerInput{Repo: "powerformer/looper", IssueRepo: "powerformer/looper", IssueNumber: 27}, nil, nil)
	if !strings.Contains(body, "Closes #27") {
		t.Fatalf("body = %q, want same-repo closing reference", body)
	}
	if strings.Contains(body, "Closes powerformer/looper#27") {
		t.Fatalf("body = %q, want unqualified same-repo closing reference", body)
	}
}

func TestHydrateWorkerInputFromIssueInfersIssueRepoFromURL(t *testing.T) {
	t.Parallel()
	work := hydrateWorkerInputFromIssue(workerInput{Repo: "acme/looper", IssueNumber: 27}, IssueDetail{Number: 27, Title: "Issue title", URL: "https://github.com/powerformer/looper/issues/27"})
	if work.IssueRepo != "powerformer/looper" {
		t.Fatalf("IssueRepo = %q, want powerformer/looper", work.IssueRepo)
	}
	if !strings.Contains(work.Prompt, "Implement GitHub issue powerformer/looper#27") {
		t.Fatalf("Prompt = %q, want issue repo in prompt", work.Prompt)
	}
	if issueRepoFromURL("https://ghe.example.com/powerformer/looper/issues/27") != "powerformer/looper" {
		t.Fatal("issueRepoFromURL() should infer issue repo from GitHub Enterprise URLs")
	}
	if issueRepoFromURL("https://gitlab.com/powerformer/looper/-/issues/27") != "" {
		t.Fatal("issueRepoFromURL() should ignore non-GitHub hosts")
	}
	if issueRepoFromURL("https://github.com/powerformer/looper/issues/not-a-number") != "" {
		t.Fatal("issueRepoFromURL() should ignore invalid issue URLs")
	}
	if !strings.Contains(buildAgentPullRequestInstruction(work), "Closes powerformer/looper#27") {
		t.Fatalf("instruction = %q, want cross-repo closing reference", buildAgentPullRequestInstruction(work))
	}
}

func TestHydrateWorkerInputFromIssueUsesSourceIssueURLWhenIssueURLMissing(t *testing.T) {
	t.Parallel()
	work := hydrateWorkerInputFromIssue(workerInput{Repo: "acme/looper", IssueNumber: 27, IssueURL: "https://github.com/powerformer/looper/issues/27"}, IssueDetail{Number: 27, Title: "Issue title"})
	if work.IssueRepo != "powerformer/looper" {
		t.Fatalf("IssueRepo = %q, want powerformer/looper", work.IssueRepo)
	}
	if !strings.Contains(work.Prompt, "Implement GitHub issue powerformer/looper#27") {
		t.Fatalf("Prompt = %q, want issue repo inferred from source issue URL", work.Prompt)
	}
	if work.IssueURL != "https://github.com/powerformer/looper/issues/27" {
		t.Fatalf("IssueURL = %q, want source issue URL preserved", work.IssueURL)
	}
}

func TestResolveWorkerInputUsesIssueRepoForIssueHydration(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{issueDetail: IssueDetail{Number: 27, Title: "Cross-repo issue", URL: "https://github.com/powerformer/looper/issues/27"}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})

	project, err := fixture.repos.Projects.GetByID(context.Background(), "project_1")
	if err != nil {
		t.Fatalf("Projects.GetByID() error = %v", err)
	}
	loop, err := fixture.repos.Loops.GetByID(context.Background(), "loop_worker_1")
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	queueItem, err := fixture.repos.Queue.GetByID(context.Background(), "queue_worker_1")
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	payload := `{"title":"Implement worker loop","repo":"acme/looper","issueRepo":"powerformer/looper","issueNumber":27,"baseBranch":"main"}`
	loopMetadata := `{"worker":{"title":"Implement worker loop","repo":"acme/looper","issueRepo":"powerformer/looper","issueNumber":27,"baseBranch":"main"}}`
	loop.MetadataJSON = &loopMetadata
	queueItem.PayloadJSON = &payload

	work, err := runner.resolveWorkerInput(context.Background(), *project, *loop, *queueItem, workerCheckpoint{})
	if err != nil {
		t.Fatalf("resolveWorkerInput() error = %v", err)
	}
	if len(github.viewIssueCalls) != 1 {
		t.Fatalf("len(github.viewIssueCalls) = %d, want 1", len(github.viewIssueCalls))
	}
	if github.viewIssueCalls[0].Repo != "powerformer/looper" {
		t.Fatalf("ViewIssue repo = %q, want powerformer/looper", github.viewIssueCalls[0].Repo)
	}
	if work.IssueRepo != "powerformer/looper" {
		t.Fatalf("work.IssueRepo = %q, want powerformer/looper", work.IssueRepo)
	}
	if !strings.Contains(work.Prompt, "Implement GitHub issue powerformer/looper#27") {
		t.Fatalf("Prompt = %q, want resolved issue repo in prompt", work.Prompt)
	}
	if strings.Contains(work.Prompt, "Implement GitHub issue acme/looper#27") {
		t.Fatalf("Prompt = %q, want prompt to avoid worker repo issue reference", work.Prompt)
	}
}

func TestResolveWorkerInputFallsBackToWorkerRepoForIssueHydration(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{issueDetail: IssueDetail{Number: 27, Title: "Cross-repo issue", URL: "https://github.com/powerformer/looper/issues/27"}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})

	project, err := fixture.repos.Projects.GetByID(context.Background(), "project_1")
	if err != nil {
		t.Fatalf("Projects.GetByID() error = %v", err)
	}
	loop, err := fixture.repos.Loops.GetByID(context.Background(), "loop_worker_1")
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	queueItem, err := fixture.repos.Queue.GetByID(context.Background(), "queue_worker_1")
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	payload := `{"title":"Implement worker loop","repo":"acme/looper","issueNumber":27,"baseBranch":"main"}`
	loopMetadata := `{"worker":{"title":"Implement worker loop","repo":"acme/looper","issueNumber":27,"baseBranch":"main"}}`
	loop.MetadataJSON = &loopMetadata
	queueItem.PayloadJSON = &payload

	work, err := runner.resolveWorkerInput(context.Background(), *project, *loop, *queueItem, workerCheckpoint{})
	if err != nil {
		t.Fatalf("resolveWorkerInput() error = %v", err)
	}
	if len(github.viewIssueCalls) != 1 {
		t.Fatalf("len(github.viewIssueCalls) = %d, want 1", len(github.viewIssueCalls))
	}
	if github.viewIssueCalls[0].Repo != "acme/looper" {
		t.Fatalf("ViewIssue repo = %q, want worker repo fallback for hydration lookup", github.viewIssueCalls[0].Repo)
	}
	if work.IssueRepo != "powerformer/looper" {
		t.Fatalf("work.IssueRepo = %q, want powerformer/looper", work.IssueRepo)
	}
	if !strings.Contains(work.Prompt, "Implement GitHub issue powerformer/looper#27") {
		t.Fatalf("Prompt = %q, want resolved issue repo in prompt", work.Prompt)
	}
}

func TestResolveWorkerInputUsesIssueURLRepoForIssueHydrationLookup(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{issueDetail: IssueDetail{Number: 27, Title: "Cross-repo issue"}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})

	project, err := fixture.repos.Projects.GetByID(context.Background(), "project_1")
	if err != nil {
		t.Fatalf("Projects.GetByID() error = %v", err)
	}
	loop, err := fixture.repos.Loops.GetByID(context.Background(), "loop_worker_1")
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	queueItem, err := fixture.repos.Queue.GetByID(context.Background(), "queue_worker_1")
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	payload := `{"title":"Implement worker loop","repo":"acme/looper","issueNumber":27,"issueUrl":"https://github.com/powerformer/looper/issues/27","baseBranch":"main"}`
	loopMetadata := `{"worker":{"title":"Implement worker loop","repo":"acme/looper","issueNumber":27,"issueUrl":"https://github.com/powerformer/looper/issues/27","baseBranch":"main"}}`
	loop.MetadataJSON = &loopMetadata
	queueItem.PayloadJSON = &payload

	work, err := runner.resolveWorkerInput(context.Background(), *project, *loop, *queueItem, workerCheckpoint{})
	if err != nil {
		t.Fatalf("resolveWorkerInput() error = %v", err)
	}
	if len(github.viewIssueCalls) != 1 {
		t.Fatalf("len(github.viewIssueCalls) = %d, want 1", len(github.viewIssueCalls))
	}
	if github.viewIssueCalls[0].Repo != "powerformer/looper" {
		t.Fatalf("ViewIssue repo = %q, want powerformer/looper inferred from issue URL", github.viewIssueCalls[0].Repo)
	}
	if work.IssueRepo != "powerformer/looper" {
		t.Fatalf("work.IssueRepo = %q, want powerformer/looper", work.IssueRepo)
	}
	if !strings.Contains(work.Prompt, "Implement GitHub issue powerformer/looper#27") {
		t.Fatalf("Prompt = %q, want resolved issue repo in prompt", work.Prompt)
	}
	if strings.Contains(work.Prompt, "Implement GitHub issue acme/looper#27") {
		t.Fatalf("Prompt = %q, want prompt to avoid worker repo issue reference", work.Prompt)
	}
}

func TestResolveWorkerInputRejectsClosedIssueTargetEvenWithSpecPath(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{issueDetail: IssueDetail{Number: 27, Title: "Closed issue", State: "CLOSED"}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})

	project, err := fixture.repos.Projects.GetByID(context.Background(), "project_1")
	if err != nil {
		t.Fatalf("Projects.GetByID() error = %v", err)
	}
	loop, err := fixture.repos.Loops.GetByID(context.Background(), "loop_worker_1")
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	queueItem, err := fixture.repos.Queue.GetByID(context.Background(), "queue_worker_1")
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	payload := `{"title":"Implement worker loop","repo":"acme/looper","issueNumber":27,"specPath":"specs/planner.md","baseBranch":"main"}`
	loopMetadata := `{"worker":{"title":"Implement worker loop","repo":"acme/looper","issueNumber":27,"specPath":"specs/planner.md","baseBranch":"main"}}`
	loop.MetadataJSON = &loopMetadata
	queueItem.PayloadJSON = &payload

	_, err = runner.resolveWorkerInput(context.Background(), *project, *loop, *queueItem, workerCheckpoint{})
	if err == nil {
		t.Fatal("resolveWorkerInput() error = nil, want closed issue validation error")
	}
	var loopErr *loopError
	if !errors.As(err, &loopErr) || loopErr.kind != FailureNonRetryable {
		t.Fatalf("error = %T %[1]v, want non-retryable loopError", err)
	}
	if !strings.Contains(err.Error(), "acme/looper#27 is closed") || !strings.Contains(err.Error(), "open GitHub issue") {
		t.Fatalf("error = %q, want clear closed issue message", err.Error())
	}
}

func TestResolveWorkerInputRejectsPullRequestIssueTarget(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{issueDetail: IssueDetail{Number: 27, Title: "PR", State: "OPEN", IsPullRequest: true}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})

	project, err := fixture.repos.Projects.GetByID(context.Background(), "project_1")
	if err != nil {
		t.Fatalf("Projects.GetByID() error = %v", err)
	}
	loop, err := fixture.repos.Loops.GetByID(context.Background(), "loop_worker_1")
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	queueItem, err := fixture.repos.Queue.GetByID(context.Background(), "queue_worker_1")
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	payload := `{"title":"Implement worker loop","repo":"acme/looper","issueNumber":27,"baseBranch":"main"}`
	loopMetadata := `{"worker":{"title":"Implement worker loop","repo":"acme/looper","issueNumber":27,"baseBranch":"main"}}`
	loop.MetadataJSON = &loopMetadata
	queueItem.PayloadJSON = &payload

	_, err = runner.resolveWorkerInput(context.Background(), *project, *loop, *queueItem, workerCheckpoint{})
	if err == nil {
		t.Fatal("resolveWorkerInput() error = nil, want PR validation error")
	}
	var loopErr *loopError
	if !errors.As(err, &loopErr) || loopErr.kind != FailureNonRetryable {
		t.Fatalf("error = %T %[1]v, want non-retryable loopError", err)
	}
	if !strings.Contains(err.Error(), "acme/looper#27 is a pull request") || !strings.Contains(err.Error(), "open GitHub issue") {
		t.Fatalf("error = %q, want clear pull request message", err.Error())
	}
}

func TestProcessClaimedItemUsesIssueLockForIssueTargetedWorker(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	issueTarget := "issue:acme/looper:27"
	loopMetadata := `{"worker":{"title":"Implement worker loop","repo":"acme/looper","issueNumber":27,"baseBranch":"main"}}`
	payload := `{"title":"Implement worker loop","repo":"acme/looper","issueNumber":27,"baseBranch":"main"}`
	if err := fixture.repos.Loops.Upsert(context.Background(), storage.LoopRecord{
		ID:           "loop_worker_issue",
		Seq:          2,
		ProjectID:    "project_1",
		Type:         "worker",
		TargetType:   "issue",
		TargetID:     &issueTarget,
		Repo:         stringPtr("acme/looper"),
		Status:       "queued",
		MetadataJSON: &loopMetadata,
		NextRunAt:    stringPtr(fixture.nowISO()),
		CreatedAt:    fixture.nowISO(),
		UpdatedAt:    fixture.nowISO(),
	}); err != nil {
		t.Fatalf("Loops.Upsert(issue worker) error = %v", err)
	}
	projectID := "project_1"
	loopID := "loop_worker_issue"
	if err := fixture.repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{
		ID:          "queue_worker_issue",
		ProjectID:   &projectID,
		LoopID:      &loopID,
		Type:        "worker",
		TargetType:  "issue",
		TargetID:    issueTarget,
		Repo:        stringPtr("acme/looper"),
		DedupeKey:   "worker:project_1:acme/looper:27",
		Priority:    1,
		Status:      "queued",
		AvailableAt: fixture.nowISO(),
		Attempts:    0,
		MaxAttempts: 1,
		PayloadJSON: &payload,
		CreatedAt:   fixture.nowISO(),
		UpdatedAt:   fixture.nowISO(),
	}); err != nil {
		t.Fatalf("Queue.Upsert(issue queue) error = %v", err)
	}

	runner := New(Options{
		DB:              fixture.coordinator.DB(),
		Repos:           fixture.repos,
		GitHub:          &fakeGitHubGateway{},
		Git:             &fakeGitGateway{},
		AgentExecutor:   &fakeAgentExecutor{},
		Logger:          fixture.logger,
		Now:             fixture.now,
		OpenPRStrategy:  config.OpenPRStrategyManual,
		AllowAutoCommit: true,
	})

	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "worker-issue", "worker")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNextOfType() = (%#v, %v), want claimed issue worker", claim, err)
	}
	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "skipped" {
		t.Fatalf("result = %#v, want skipped", result)
	}
	lock, err := fixture.repos.Locks.Get(context.Background(), "issue:acme/looper:27")
	if err != nil {
		t.Fatalf("Locks.Get() error = %v", err)
	}
	if lock != nil {
		t.Fatalf("lock = %#v, want released issue lock", lock)
	}
}

func TestProcessClaimedItemKeepsIssueLockKeyWhileRetargetingWorkerToPullRequest(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	git := &fakeGitGateway{createResult: CreateWorktreeResult{WorktreePath: filepath.Join(t.TempDir(), "wt"), Branch: "looper/feature", BaseBranch: "main", HeadSHA: "abc123", WorktreeID: "worktree_1"}}
	github := &fakeGitHubGateway{createPRResult: CreatePullRequestResult{Number: 101, URL: "https://example/pr/101"}}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "done", Stdout: "ok", ParseStatus: "parsed"}}}
	completed := make([]RunCompletedInput, 0, 1)

	loop, err := fixture.repos.Loops.GetByID(context.Background(), "loop_worker_1")
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if loop == nil {
		t.Fatal("loop = nil, want seeded worker loop")
	}
	issueTarget := "issue:acme/looper:27"
	loopMetadata := `{"worker":{"title":"Implement worker loop","repo":"acme/looper","issueNumber":27,"baseBranch":"main"}}`
	loop.TargetType = "issue"
	loop.TargetID = &issueTarget
	loop.MetadataJSON = &loopMetadata
	loop.UpdatedAt = fixture.nowISO()
	if err := fixture.repos.Loops.Upsert(context.Background(), *loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	queue, err := fixture.repos.Queue.GetByID(context.Background(), "queue_worker_1")
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	if queue == nil {
		t.Fatal("queue = nil, want seeded worker queue item")
	}
	payload := `{"title":"Implement worker loop","prompt":"Do the thing","repo":"acme/looper","issueNumber":27,"baseBranch":"main"}`
	queue.TargetType = "issue"
	queue.TargetID = issueTarget
	queue.LockKey = stringPtr(issueTarget)
	queue.PayloadJSON = &payload
	queue.UpdatedAt = fixture.nowISO()
	if err := fixture.repos.Queue.Upsert(context.Background(), *queue); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, AllowAutoCommit: true, AllowAutoPush: true, OpenPRStrategy: config.OpenPRStrategyAllDone, OnRunCompleted: func(_ context.Context, input RunCompletedInput) error {
		queue, err := fixture.repos.Queue.GetByID(context.Background(), "queue_worker_1")
		if err != nil {
			t.Fatalf("Queue.GetByID() in notification callback error = %v", err)
		}
		if queue == nil || queue.LockKey == nil || *queue.LockKey != issueTarget {
			t.Fatalf("queue during notification = %#v, want in-flight issue lock key", queue)
		}
		lock, err := fixture.repos.Locks.Get(context.Background(), issueTarget)
		if err != nil {
			t.Fatalf("Locks.Get() in notification callback error = %v", err)
		}
		if lock == nil {
			t.Fatal("issue lock during notification = nil, want held in-flight issue lock")
		}
		completed = append(completed, input)
		return nil
	}})

	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "worker-1", "worker")
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
	if len(completed) != 1 {
		t.Fatalf("len(completed) = %d, want 1", len(completed))
	}

	updatedQueue, err := fixture.repos.Queue.GetByID(context.Background(), "queue_worker_1")
	if err != nil {
		t.Fatalf("Queue.GetByID() after run error = %v", err)
	}
	if updatedQueue == nil || updatedQueue.TargetType != "pull_request" || updatedQueue.TargetID != "pr:acme/looper:101" || updatedQueue.LockKey == nil || *updatedQueue.LockKey != "pr:acme/looper:101" {
		t.Fatalf("queue = %#v, want retargeted queue item with persisted PR lock key", updatedQueue)
	}
	latestRun, err := fixture.repos.Runs.GetLatestByLoopID(context.Background(), "loop_worker_1")
	if err != nil {
		t.Fatalf("Runs.GetLatestByLoopID() error = %v", err)
	}
	if latestRun == nil {
		t.Fatal("latestRun = nil, want persisted worker run")
	}
	latestCheckpoint, err := parseCheckpoint(latestRun.CheckpointJSON)
	if err != nil {
		t.Fatalf("parseCheckpoint(latestRun) error = %v", err)
	}
	if latestCheckpoint.ClaimedLockKey != "pr:acme/looper:101" {
		t.Fatalf("checkpoint.ClaimedLockKey = %q, want pr:acme/looper:101", latestCheckpoint.ClaimedLockKey)
	}
	issueLock, err := fixture.repos.Locks.Get(context.Background(), issueTarget)
	if err != nil {
		t.Fatalf("Locks.Get(issue) after run error = %v", err)
	}
	if issueLock != nil {
		t.Fatalf("issue lock = %#v, want released after run", issueLock)
	}
}

func TestPersistPullRequestReferenceRollsBackLoopWhenQueueUpdateFails(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, Logger: fixture.logger, Now: fixture.now})

	loop, err := fixture.repos.Loops.GetByID(context.Background(), "loop_worker_1")
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if loop == nil {
		t.Fatal("loop = nil, want seeded worker loop")
	}
	queue, err := fixture.repos.Queue.GetByID(context.Background(), "queue_worker_1")
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	if queue == nil {
		t.Fatal("queue = nil, want seeded worker queue")
	}

	originalTargetType := loop.TargetType
	originalTargetID := derefString(loop.TargetID)
	originalPRNumber := loop.PRNumber
	queue.Status = "invalid"
	err = runner.persistPullRequestReference(context.Background(), *loop, *queue, "acme/looper", checkpointPullPR{Number: 101, URL: "https://example/pr/101"})
	if err == nil {
		t.Fatal("persistPullRequestReference() error = nil, want queue upsert failure")
	}

	updatedLoop, err := fixture.repos.Loops.GetByID(context.Background(), "loop_worker_1")
	if err != nil {
		t.Fatalf("Loops.GetByID(updated) error = %v", err)
	}
	if updatedLoop == nil {
		t.Fatal("updated loop = nil, want seeded worker loop")
	}
	if updatedLoop.TargetType != originalTargetType || derefString(updatedLoop.TargetID) != originalTargetID || updatedLoop.PRNumber != originalPRNumber {
		t.Fatalf("loop after failed persist = %#v, want original target %s %s", updatedLoop, originalTargetType, originalTargetID)
	}

	updatedQueue, err := fixture.repos.Queue.GetByID(context.Background(), "queue_worker_1")
	if err != nil {
		t.Fatalf("Queue.GetByID(updated) error = %v", err)
	}
	if updatedQueue == nil {
		t.Fatal("updated queue = nil, want seeded worker queue")
	}
	if updatedQueue.TargetType != "issue" || updatedQueue.TargetID != "issue:acme/looper:27" || updatedQueue.PRNumber != nil {
		t.Fatalf("queue after failed persist = %#v, want original issue target", updatedQueue)
	}
}

func TestProcessClaimedItemResumesFromOpenPRAfterRetryableFailure(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	git := &fakeGitGateway{createResult: CreateWorktreeResult{WorktreePath: filepath.Join(t.TempDir(), "wt"), Branch: "looper/feature", BaseBranch: "main", HeadSHA: "abc123", WorktreeID: "worktree_1"}}
	github := &fakeGitHubGateway{createPRResult: CreatePullRequestResult{Number: 101, URL: "https://example/pr/101"}, createPRErrors: []error{fmt.Errorf("temporary create pr failure")}}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "done", Stdout: "ok", ParseStatus: "parsed"}}}
	started := make([]AgentExecutionStartedInput, 0, 1)
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, AllowAutoCommit: true, AllowAutoPush: true, OpenPRStrategy: config.OpenPRStrategyAllDone, OnAgentExecutionStarted: func(_ context.Context, input AgentExecutionStartedInput) error {
		started = append(started, input)
		return nil
	}})

	claim1, _ := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "worker-1", "worker")
	first, err := runner.ProcessClaimedItem(context.Background(), *claim1)
	if err != nil {
		t.Fatalf("ProcessClaimedItem(first) error = %v", err)
	}
	if first.Status != "failed" || first.FailureKind != FailureRetryableAfterResume {
		t.Fatalf("first = %#v, want retryable_after_resume failure", first)
	}
	if len(github.updateIssueCommentCalls) != 1 {
		t.Fatalf("len(github.updateIssueCommentCalls) after retryable failure = %d, want 1 non-terminal refresh", len(github.updateIssueCommentCalls))
	}
	if body := github.updateIssueCommentCalls[0].Body; !strings.Contains(body, "started work") || strings.Contains(body, "stopped work") || strings.Contains(body, "paused work") {
		t.Fatalf("retryable issue comment body = %q, want in-progress status without terminal failure text", body)
	}
	fixture.advance(5 * time.Second)
	claim2, _ := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "worker-1", "worker")
	second, err := runner.ProcessClaimedItem(context.Background(), *claim2)
	if err != nil {
		t.Fatalf("ProcessClaimedItem(second) error = %v", err)
	}
	if second.Status != "success" {
		t.Fatalf("second = %#v, want success", second)
	}
	if len(agent.starts) != 1 {
		t.Fatalf("len(agent.starts) = %d, want 1 (execute should not rerun)", len(agent.starts))
	}
	if len(started) != 1 {
		t.Fatalf("len(started) = %d, want 1 (start notification should not repeat on resume)", len(started))
	}
	if len(git.createCalls) != 1 {
		t.Fatalf("len(git.createCalls) = %d, want 1 (worktree should not rerun)", len(git.createCalls))
	}
	if len(github.createPRCalls) != 2 {
		t.Fatalf("len(github.createPRCalls) = %d, want 2", len(github.createPRCalls))
	}
	if len(github.createIssueCommentCalls) != 1 {
		t.Fatalf("len(github.createIssueCommentCalls) = %d, want 1 running marker across resume", len(github.createIssueCommentCalls))
	}
}

func TestFindPreviousIssueClaimPrefersNewestMatchingRun(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, Logger: fixture.logger, Now: fixture.now})

	oldCheckpoint := mustMarshalJSON(workerCheckpoint{
		IssueClaim: &checkpointIssueClaim{Repo: "acme/looper", IssueNumber: 27, CommentID: 101, Status: issueClaimStatusRunning},
	})
	if err := fixture.repos.Runs.Upsert(context.Background(), storage.RunRecord{
		ID:             "run_old_claim",
		LoopID:         "loop_worker_1",
		Status:         "failed",
		CheckpointJSON: &oldCheckpoint,
		StartedAt:      fixture.nowISO(),
		CreatedAt:      fixture.nowISO(),
		UpdatedAt:      fixture.nowISO(),
	}); err != nil {
		t.Fatalf("Runs.Upsert(old claim) error = %v", err)
	}

	fixture.advance(time.Second)
	newCheckpoint := mustMarshalJSON(workerCheckpoint{
		IssueClaim: &checkpointIssueClaim{Repo: "acme/looper", IssueNumber: 27, CommentID: 202, Status: issueClaimStatusRunning},
	})
	if err := fixture.repos.Runs.Upsert(context.Background(), storage.RunRecord{
		ID:             "run_new_claim",
		LoopID:         "loop_worker_1",
		Status:         "failed",
		CheckpointJSON: &newCheckpoint,
		StartedAt:      fixture.nowISO(),
		CreatedAt:      fixture.nowISO(),
		UpdatedAt:      fixture.nowISO(),
	}); err != nil {
		t.Fatalf("Runs.Upsert(new claim) error = %v", err)
	}

	fixture.advance(time.Second)
	currentCheckpoint := mustMarshalJSON(workerCheckpoint{})
	if err := fixture.repos.Runs.Upsert(context.Background(), storage.RunRecord{
		ID:             "run_current",
		LoopID:         "loop_worker_1",
		Status:         "running",
		CheckpointJSON: &currentCheckpoint,
		StartedAt:      fixture.nowISO(),
		CreatedAt:      fixture.nowISO(),
		UpdatedAt:      fixture.nowISO(),
	}); err != nil {
		t.Fatalf("Runs.Upsert(current) error = %v", err)
	}

	claim := runner.findPreviousIssueClaim(context.Background(), "loop_worker_1", "run_current", "acme/looper", 27)
	if claim == nil {
		t.Fatal("findPreviousIssueClaim() = nil, want newest prior claim")
	}
	if claim.CommentID != 202 {
		t.Fatalf("claim.CommentID = %d, want 202 from newest run", claim.CommentID)
	}
}

func TestProcessClaimedItemResumeReleasesClaimedLockWhenSetupFails(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	checkpointJSON := `{"claimedLockKey":"worker:project_1"}`
	if err := fixture.repos.Runs.Upsert(context.Background(), storage.RunRecord{ID: "run_failed_resume", LoopID: "loop_worker_1", Status: "failed", LastCompletedStep: stringPtr(string(stepPrepareWork)), CheckpointJSON: &checkpointJSON, StartedAt: fixture.nowISO(), CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}
	if _, err := fixture.coordinator.DB().ExecContext(context.Background(), `
		CREATE TRIGGER loops_fail_running_resume_worker
		BEFORE UPDATE ON loops
		FOR EACH ROW
		WHEN NEW.id = 'loop_worker_1' AND NEW.status = 'running'
		BEGIN
			SELECT RAISE(FAIL, 'forced loop update failure');
		END;
	`); err != nil {
		t.Fatalf("create trigger error = %v", err)
	}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: &fakeGitHubGateway{}, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})

	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "worker-1", "worker")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNextOfType() = (%#v, %v), want claimed item", claim, err)
	}
	_, err = runner.ProcessClaimedItem(context.Background(), *claim)
	if err == nil || !strings.Contains(err.Error(), "forced loop update failure") {
		t.Fatalf("ProcessClaimedItem() error = %v, want forced loop update failure", err)
	}
	lock, err := fixture.repos.Locks.Get(context.Background(), "worker:project_1")
	if err != nil {
		t.Fatalf("Locks.Get() error = %v", err)
	}
	if lock != nil {
		t.Fatalf("lock = %#v, want released claimed lock", lock)
	}
	acquired, err := fixture.repos.Locks.Acquire(context.Background(), storage.LockRecord{Key: "worker:project_1", Owner: "retry", ExpiresAt: fixture.now().Add(time.Minute).UTC().Format("2006-01-02T15:04:05.000Z"), CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()})
	if err != nil {
		t.Fatalf("Locks.Acquire() error = %v", err)
	}
	if !acquired {
		t.Fatal("Locks.Acquire() = false, want claimed lock to be immediately reacquirable")
	}
}

func TestProcessClaimedItemValidationFailureRequeues(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	git := &fakeGitGateway{createResult: CreateWorktreeResult{WorktreePath: filepath.Join(t.TempDir(), "wt"), Branch: "looper/feature", BaseBranch: "main", HeadSHA: "abc123", WorktreeID: "worktree_1"}}
	github := &fakeGitHubGateway{}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "done", Stdout: "ok", ParseStatus: "parsed"}}}
	completed := make([]RunCompletedInput, 0, 1)
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, AllowAutoCommit: true, AllowAutoPush: true, OpenPRStrategy: config.OpenPRStrategyAllDone, ValidationRunner: func(context.Context, ValidationInput) (ValidationResult, error) {
		return ValidationResult{Passed: false, Summary: "Validation failed"}, nil
	}, OnRunCompleted: func(_ context.Context, input RunCompletedInput) error {
		completed = append(completed, input)
		return nil
	}})

	claim, _ := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "worker-1", "worker")
	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "failed" || result.FailureKind != FailureManualIntervention {
		t.Fatalf("result = %#v, want manual_intervention failure", result)
	}
	if len(completed) != 1 {
		t.Fatalf("len(completed) = %d, want 1", len(completed))
	}
	if completed[0].Status != "failed" || completed[0].FailureKind != FailureManualIntervention || completed[0].Summary != "Validation failed" {
		t.Fatalf("completed[0] = %#v, want manual intervention completion notice", completed[0])
	}
	if len(github.updateIssueCommentCalls) == 0 || !strings.Contains(github.updateIssueCommentCalls[len(github.updateIssueCommentCalls)-1].Body, "paused work") {
		t.Fatalf("terminal issue comment updates = %#v, want paused marker body", github.updateIssueCommentCalls)
	}
	queue, err := fixture.repos.Queue.GetByID(context.Background(), claim.ID)
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	if queue == nil || queue.Status != string(FailureManualIntervention) {
		t.Fatalf("queue = %#v, want terminal manual_intervention item", queue)
	}
	loop, err := fixture.repos.Loops.GetByID(context.Background(), result.LoopID)
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if loop == nil || loop.Status != "paused" {
		t.Fatalf("loop = %#v, want paused", loop)
	}
}

func TestProcessClaimedItemPreservesPausedLoopOnRetryableFailureAfterPause(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "done", Stdout: "ok", ParseStatus: "parsed"}}, wait: func(ctx context.Context) error {
		loopID := ""
		items, err := fixture.repos.Queue.List(ctx)
		if err != nil {
			return err
		}
		for _, item := range items {
			if item.Type == "worker" && item.Status == "running" && item.LoopID != nil {
				loopID = *item.LoopID
				break
			}
		}
		if loopID == "" {
			return fmt.Errorf("running worker queue item not found")
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
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: &fakeGitHubGateway{}, Git: &fakeGitGateway{createResult: CreateWorktreeResult{WorktreePath: filepath.Join(t.TempDir(), "wt"), Branch: "looper/feature", BaseBranch: "main", HeadSHA: "abc123", WorktreeID: "worktree_1"}}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, AllowAutoCommit: true, AllowAutoPush: true, OpenPRStrategy: config.OpenPRStrategyAllDone})

	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "worker-1", "worker")
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
	claimedWorker, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "worker-0", "worker")
	if err != nil || claimedWorker == nil {
		t.Fatalf("initial ClaimNextOfType() = (%#v, %v), want claimed worker", claimedWorker, err)
	}
	projectID := "project_1"
	nowISO := fixture.nowISO()
	payload := `{"repo":"acme/looper","baseBranch":"main","prompt":"test"}`
	if err := fixture.repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: "queue_missing_worker", ProjectID: &projectID, Type: "worker", TargetType: "issue", TargetID: "issue:acme/looper:99", Repo: stringPtr("acme/looper"), DedupeKey: "worker:acme/looper:99", Priority: 1, Status: "queued", AvailableAt: nowISO, MaxAttempts: 3, PayloadJSON: &payload, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: &fakeGitHubGateway{}, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})

	result, err := runner.ProcessNext(context.Background(), "worker-1")
	if err != nil {
		t.Fatalf("ProcessNext() error = %v", err)
	}
	if result == nil || result.Status != "failed" || result.FailureKind != FailureNonRetryable || !strings.Contains(result.Summary, "requires loopId") {
		t.Fatalf("result = %#v, want non-retryable missing-loopId failure", result)
	}
	queue, err := fixture.repos.Queue.GetByID(context.Background(), "queue_missing_worker")
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
	loopTarget := "project:project_1"
	if err := fixture.repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: "loop_worker_running", Seq: 2, ProjectID: "project_1", Type: "worker", TargetType: "project", TargetID: &loopTarget, Repo: stringPtr("acme/looper"), Status: "running", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	projectID := "project_1"
	loopID := "loop_worker_running"
	payload := `{"title":"Recover worker loop","prompt":"Do the thing","repo":"acme/looper","baseBranch":"main"}`
	if err := fixture.repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: "queue_worker_running", ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: loopTarget, Repo: stringPtr("acme/looper"), DedupeKey: "worker:loop_worker_running", Priority: 1, Status: "running", AvailableAt: nowISO, Attempts: 0, MaxAttempts: 1, PayloadJSON: &payload, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}
	completed := make([]RunCompletedInput, 0, 1)
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: &fakeGitHubGateway{}, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now, OnRunCompleted: func(_ context.Context, input RunCompletedInput) error {
		completed = append(completed, input)
		return nil
	}})

	result, err := runner.recoverClaimedItem(context.Background(), storage.QueueItemRecord{ID: "queue_worker_running", ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: loopTarget, Repo: stringPtr("acme/looper"), DedupeKey: "worker:loop_worker_running", Priority: 1, Status: "running", AvailableAt: nowISO, Attempts: 0, MaxAttempts: 1, PayloadJSON: &payload, CreatedAt: nowISO, UpdatedAt: nowISO}, fmt.Errorf("persist step failed"))
	if err != nil {
		t.Fatalf("recoverClaimedItem() error = %v", err)
	}
	if result == nil || result.Status != "failed" || result.FailureKind != FailureNonRetryable {
		t.Fatalf("result = %#v, want failed non-retryable recovery", result)
	}
	if len(completed) != 1 || completed[0].Status != "failed" || completed[0].FailureKind != FailureNonRetryable {
		t.Fatalf("completed = %#v, want terminal recovery notification", completed)
	}
	queue, err := fixture.repos.Queue.GetByID(context.Background(), "queue_worker_running")
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

func TestRecoverClaimedItemDoesNotReusePreviousRunID(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	nowISO := fixture.nowISO()
	priorStartedAt := time.Date(2024, time.January, 2, 3, 4, 5, 0, time.UTC).Format(time.RFC3339Nano)
	claimedAt := time.Date(2024, time.January, 2, 3, 5, 5, 0, time.UTC).Format(time.RFC3339Nano)
	loopTarget := "project:project_1"
	if err := fixture.repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: "loop_worker_prior_run", Seq: 2, ProjectID: "project_1", Type: "worker", TargetType: "project", TargetID: &loopTarget, Repo: stringPtr("acme/looper"), Status: "running", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	checkpointJSON := `{"work":{"title":"Older worker run"}}`
	if err := fixture.repos.Runs.Upsert(context.Background(), storage.RunRecord{ID: "run_prior", LoopID: "loop_worker_prior_run", Status: "success", CheckpointJSON: &checkpointJSON, StartedAt: priorStartedAt, EndedAt: &priorStartedAt, CreatedAt: priorStartedAt, UpdatedAt: priorStartedAt}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}
	projectID := "project_1"
	loopID := "loop_worker_prior_run"
	payload := `{"title":"Recover worker loop","prompt":"Do the thing","repo":"acme/looper","baseBranch":"main"}`
	if err := fixture.repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: "queue_worker_prior_run", ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: loopTarget, Repo: stringPtr("acme/looper"), DedupeKey: "worker:loop_worker_prior_run", Priority: 1, Status: "running", AvailableAt: nowISO, ClaimedAt: &claimedAt, Attempts: 0, MaxAttempts: 1, PayloadJSON: &payload, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}
	completed := make([]RunCompletedInput, 0, 1)
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: &fakeGitHubGateway{}, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now, OnRunCompleted: func(_ context.Context, input RunCompletedInput) error {
		completed = append(completed, input)
		return nil
	}})

	result, err := runner.recoverClaimedItem(context.Background(), storage.QueueItemRecord{ID: "queue_worker_prior_run", ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: loopTarget, Repo: stringPtr("acme/looper"), DedupeKey: "worker:loop_worker_prior_run", Priority: 1, Status: "running", AvailableAt: nowISO, ClaimedAt: &claimedAt, Attempts: 0, MaxAttempts: 1, PayloadJSON: &payload, CreatedAt: nowISO, UpdatedAt: nowISO}, fmt.Errorf("project lookup failed"))
	if err != nil {
		t.Fatalf("recoverClaimedItem() error = %v", err)
	}
	if result == nil || result.Status != "failed" || result.FailureKind != FailureNonRetryable {
		t.Fatalf("result = %#v, want failed non-retryable recovery", result)
	}
	if len(completed) != 1 {
		t.Fatalf("completed = %#v, want one recovery notification", completed)
	}
	if completed[0].RunID != "" {
		t.Fatalf("completed[0].RunID = %q, want empty run ID for recovery before new run creation", completed[0].RunID)
	}
}

func TestProcessClaimedItemSkippedFlowEmitsCompletionNotification(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	git := &fakeGitGateway{createResult: CreateWorktreeResult{WorktreePath: filepath.Join(t.TempDir(), "wt"), Branch: "looper/feature", BaseBranch: "main", HeadSHA: "abc123", WorktreeID: "worktree_1"}}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "done", Stdout: "ok", ParseStatus: "parsed"}}}
	completed := make([]RunCompletedInput, 0, 1)
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: &fakeGitHubGateway{}, Git: git, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, AllowAutoCommit: true, AllowAutoPush: false, OpenPRStrategy: config.OpenPRStrategyAllDone, OnRunCompleted: func(_ context.Context, input RunCompletedInput) error {
		completed = append(completed, input)
		return nil
	}})

	claim, _ := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "worker-1", "worker")
	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "skipped" || !strings.Contains(result.Summary, "manual PR opening required") {
		t.Fatalf("result = %#v, want skipped manual intervention summary", result)
	}
	if len(completed) != 1 || completed[0].Status != "skipped" || !strings.Contains(completed[0].Summary, "manual PR opening required") {
		t.Fatalf("completed = %#v, want skipped completion notification", completed)
	}
}

func TestProcessClaimedItemSkipsAutoPROpenWhenGitHubCLIUnavailable(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	git := &fakeGitGateway{createResult: CreateWorktreeResult{WorktreePath: filepath.Join(t.TempDir(), "wt"), Branch: "looper/feature", BaseBranch: "main", HeadSHA: "abc123", WorktreeID: "worktree_1"}}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "done", Stdout: "ok", ParseStatus: "parsed"}}}
	completed := make([]RunCompletedInput, 0, 1)
	githubCLIAvailable := false
	runner := New(Options{
		DB:                 fixture.coordinator.DB(),
		Repos:              fixture.repos,
		GitHub:             &fakeGitHubGateway{},
		GitHubCLIAvailable: &githubCLIAvailable,
		Git:                git,
		AgentExecutor:      agent,
		Logger:             fixture.logger,
		Now:                fixture.now,
		AllowAutoCommit:    true,
		AllowAutoPush:      true,
		OpenPRStrategy:     config.OpenPRStrategyAllDone,
		OnRunCompleted: func(_ context.Context, input RunCompletedInput) error {
			completed = append(completed, input)
			return nil
		},
	})

	claim, _ := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "worker-1", "worker")
	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "skipped" || !strings.Contains(result.Summary, "GitHub CLI unavailable") {
		t.Fatalf("result = %#v, want skipped GitHub CLI unavailable summary", result)
	}
	if len(git.pushCalls) != 0 {
		t.Fatalf("len(git.pushCalls) = %d, want 0 when PR opening is gated before push", len(git.pushCalls))
	}
	if len(completed) != 1 || completed[0].Status != "skipped" || !strings.Contains(completed[0].Summary, "GitHub CLI unavailable") {
		t.Fatalf("completed = %#v, want skipped completion notification for missing GitHub CLI", completed)
	}
}

func TestProcessClaimedItemRechecksGitHubCLIAvailabilityAtRunTime(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	git := &fakeGitGateway{createResult: CreateWorktreeResult{WorktreePath: filepath.Join(t.TempDir(), "wt"), Branch: "looper/feature", BaseBranch: "main", HeadSHA: "abc123", WorktreeID: "worktree_1"}}
	github := &fakeGitHubGateway{createPRResult: CreatePullRequestResult{Number: 101, URL: "https://example/pr/101"}}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "done", Stdout: "ok", ParseStatus: "parsed"}}}
	checkCalls := 0
	runner := New(Options{
		DB:                 fixture.coordinator.DB(),
		Repos:              fixture.repos,
		GitHub:             github,
		GitHubCLIAvailable: func() *bool { value := false; return &value }(),
		GitHubCLIAutoPROpeningAvailable: func(context.Context, string, string) bool {
			checkCalls++
			return true
		},
		Git:             git,
		AgentExecutor:   agent,
		Logger:          fixture.logger,
		Now:             fixture.now,
		AllowAutoCommit: true,
		AllowAutoPush:   true,
		OpenPRStrategy:  config.OpenPRStrategyAllDone,
	})

	claim, _ := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "worker-1", "worker")
	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "success" || result.PullRequestNumber != 101 {
		t.Fatalf("result = %#v, want success with opened PR", result)
	}
	if checkCalls == 0 {
		t.Fatal("runtime GitHub CLI availability check was not called")
	}
	if len(git.pushCalls) != 1 {
		t.Fatalf("len(git.pushCalls) = %d, want 1 after runtime auth recovery", len(git.pushCalls))
	}
	if len(github.createPRCalls) != 1 {
		t.Fatalf("len(github.createPRCalls) = %d, want 1 after runtime auth recovery", len(github.createPRCalls))
	}
}

func TestProcessClaimedItemPullRequestLoopRequiresSpecPath(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	nowISO := fixture.nowISO()
	prNumber := int64(42)
	loopTarget := "pr:acme/looper:42"
	loopMeta := `{"worker":{"repo":"acme/looper","baseBranch":"main"}}`
	if err := fixture.repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: "loop_worker_pr", Seq: 2, ProjectID: "project_1", Type: "worker", TargetType: "pull_request", TargetID: &loopTarget, Repo: stringPtr("acme/looper"), PRNumber: &prNumber, Status: "queued", MetadataJSON: &loopMeta, NextRunAt: &nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	projectID := "project_1"
	loopID := "loop_worker_pr"
	payload := `{"repo":"acme/looper","baseBranch":"main","prompt":"do not use prompt for PR loops"}`
	if err := fixture.repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: "queue_worker_pr", ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "pull_request", TargetID: loopTarget, Repo: stringPtr("acme/looper"), PRNumber: &prNumber, DedupeKey: "worker:acme/looper:42", Priority: 1, Status: "queued", AvailableAt: nowISO, MaxAttempts: 3, PayloadJSON: &payload, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}
	github := &fakeGitHubGateway{prDetail: PullRequestDetail{Number: 42, Title: "Existing PR", BaseRefName: "main", HeadRefName: "feature/pr-42", HeadSHA: "abc123"}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now, AllowAutoCommit: true, AllowAutoPush: true, OpenPRStrategy: config.OpenPRStrategyAllDone})

	claim, _ := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "worker-1", "worker")
	claim, _ = fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "worker-2", "worker")
	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.FailureKind != FailureManualIntervention || !strings.Contains(result.Summary, "No explicit spec path found") {
		t.Fatalf("result = %#v, want manual_intervention spec-path failure", result)
	}
}

func TestProcessClaimedItemRenamesPlannerSpecPRAfterPushExistingTakeover(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	nowISO := fixture.nowISO()
	prNumber := int64(42)
	loopTarget := "pr:acme/looper:42"
	loopMeta := `{"worker":{"title":"Implement login flow","repo":"acme/looper","baseBranch":"main"}}`
	if err := fixture.repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: "loop_worker_pr", Seq: 2, ProjectID: "project_1", Type: "worker", TargetType: "pull_request", TargetID: &loopTarget, Repo: stringPtr("acme/looper"), PRNumber: &prNumber, Status: "queued", MetadataJSON: &loopMeta, NextRunAt: &nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	projectID := "project_1"
	loopID := "loop_worker_pr"
	payload := `{"title":"Implement login flow","repo":"acme/looper","baseBranch":"main"}`
	if err := fixture.repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: "queue_worker_pr", ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "pull_request", TargetID: loopTarget, Repo: stringPtr("acme/looper"), PRNumber: &prNumber, DedupeKey: "worker:acme/looper:42", Priority: 1, Status: "queued", AvailableAt: nowISO, MaxAttempts: 3, PayloadJSON: &payload, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}
	git := &fakeGitGateway{createResult: CreateWorktreeResult{WorktreePath: filepath.Join(t.TempDir(), "wt"), Branch: "feature/pr-42", BaseBranch: "main", HeadSHA: "abc123", WorktreeID: "worktree_1"}}
	github := &fakeGitHubGateway{prDetail: PullRequestDetail{Number: 42, Title: "Spec: Add login flow", Body: "## Summary\n\nSpec: specs/login/spec.md", BaseRefName: "main", HeadRefName: "feature/pr-42", HeadSHA: "abc123"}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now, AllowAutoCommit: true, AllowAutoPush: true, OpenPRStrategy: config.OpenPRStrategyAllDone})

	claim, _ := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "worker-1", "worker")
	claim, _ = fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "worker-2", "worker")
	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "success" || result.PullRequestNumber != 42 {
		t.Fatalf("result = %#v, want success with PR 42", result)
	}
	if len(github.updatePRTitleCalls) != 1 {
		t.Fatalf("len(github.updatePRTitleCalls) = %d, want 1", len(github.updatePRTitleCalls))
	}
	if got := github.updatePRTitleCalls[0].Title; got != "Implement login flow" {
		t.Fatalf("updated title = %q, want worker title", got)
	}
}

func TestRenamePlannerSpecPullRequestAfterTakeoverBackfillsMissingCheckpointTitle(t *testing.T) {
	t.Parallel()
	github := &fakeGitHubGateway{prDetail: PullRequestDetail{Number: 42, Title: "Spec: Add login flow", BaseRefName: "main", HeadRefName: "feature/pr-42", HeadSHA: "abc123"}}
	runner := New(Options{GitHub: github})
	work := workerInput{Title: "Implement login flow", Repo: "acme/looper", BaseBranch: "main", ExecutionMode: "push-existing", PRNumber: 42}

	if err := runner.renamePlannerSpecPullRequestAfterTakeover(context.Background(), work, ""); err != nil {
		t.Fatalf("renamePlannerSpecPullRequestAfterTakeover() error = %v", err)
	}
	if len(github.viewPRCalls) != 1 {
		t.Fatalf("len(github.viewPRCalls) = %d, want 1", len(github.viewPRCalls))
	}
	if len(github.updatePRTitleCalls) != 1 {
		t.Fatalf("len(github.updatePRTitleCalls) = %d, want 1", len(github.updatePRTitleCalls))
	}
	if got := github.updatePRTitleCalls[0].Title; got != "Implement login flow" {
		t.Fatalf("updated title = %q, want worker title", got)
	}
}

func TestProcessClaimedItemPreservesHumanEditedPRAfterPushExistingTakeover(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	nowISO := fixture.nowISO()
	prNumber := int64(42)
	loopTarget := "pr:acme/looper:42"
	loopMeta := `{"worker":{"title":"Implement login flow","repo":"acme/looper","baseBranch":"main"}}`
	if err := fixture.repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: "loop_worker_pr", Seq: 2, ProjectID: "project_1", Type: "worker", TargetType: "pull_request", TargetID: &loopTarget, Repo: stringPtr("acme/looper"), PRNumber: &prNumber, Status: "queued", MetadataJSON: &loopMeta, NextRunAt: &nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	projectID := "project_1"
	loopID := "loop_worker_pr"
	payload := `{"title":"Implement login flow","repo":"acme/looper","baseBranch":"main"}`
	if err := fixture.repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: "queue_worker_pr", ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "pull_request", TargetID: loopTarget, Repo: stringPtr("acme/looper"), PRNumber: &prNumber, DedupeKey: "worker:acme/looper:42", Priority: 1, Status: "queued", AvailableAt: nowISO, MaxAttempts: 3, PayloadJSON: &payload, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}
	git := &fakeGitGateway{createResult: CreateWorktreeResult{WorktreePath: filepath.Join(t.TempDir(), "wt"), Branch: "feature/pr-42", BaseBranch: "main", HeadSHA: "abc123", WorktreeID: "worktree_1"}}
	github := &fakeGitHubGateway{prDetail: PullRequestDetail{Number: 42, Title: "Human-written implementation title", Body: "## Summary\n\nSpec: specs/login/spec.md", BaseRefName: "main", HeadRefName: "feature/pr-42", HeadSHA: "abc123"}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now, AllowAutoCommit: true, AllowAutoPush: true, OpenPRStrategy: config.OpenPRStrategyAllDone})

	claim, _ := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "worker-1", "worker")
	claim, _ = fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "worker-2", "worker")
	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "success" || result.PullRequestNumber != 42 {
		t.Fatalf("result = %#v, want success with PR 42", result)
	}
	if len(github.updatePRTitleCalls) != 0 {
		t.Fatalf("len(github.updatePRTitleCalls) = %d, want 0", len(github.updatePRTitleCalls))
	}
}

func TestProcessClaimedItemPreservesPRTitleEditedDuringTakeover(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	nowISO := fixture.nowISO()
	prNumber := int64(42)
	loopTarget := "pr:acme/looper:42"
	loopMeta := `{"worker":{"title":"Implement login flow","repo":"acme/looper","baseBranch":"main"}}`
	if err := fixture.repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: "loop_worker_pr", Seq: 2, ProjectID: "project_1", Type: "worker", TargetType: "pull_request", TargetID: &loopTarget, Repo: stringPtr("acme/looper"), PRNumber: &prNumber, Status: "queued", MetadataJSON: &loopMeta, NextRunAt: &nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	projectID := "project_1"
	loopID := "loop_worker_pr"
	payload := `{"title":"Implement login flow","repo":"acme/looper","baseBranch":"main"}`
	if err := fixture.repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: "queue_worker_pr", ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "pull_request", TargetID: loopTarget, Repo: stringPtr("acme/looper"), PRNumber: &prNumber, DedupeKey: "worker:acme/looper:42", Priority: 1, Status: "queued", AvailableAt: nowISO, MaxAttempts: 3, PayloadJSON: &payload, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}
	git := &fakeGitGateway{createResult: CreateWorktreeResult{WorktreePath: filepath.Join(t.TempDir(), "wt"), Branch: "feature/pr-42", BaseBranch: "main", HeadSHA: "abc123", WorktreeID: "worktree_1"}}
	humanEditedTitle := "Spec: Revised login flow"
	github := &fakeGitHubGateway{prDetailResponses: []PullRequestDetail{
		{Number: 42, Title: "Spec: Add login flow", Body: "## Summary\n\nSpec: specs/login/spec.md", BaseRefName: "main", HeadRefName: "feature/pr-42", HeadSHA: "abc123"},
		{Number: 42, Title: humanEditedTitle, Body: "## Summary\n\nSpec: specs/login/spec.md", BaseRefName: "main", HeadRefName: "feature/pr-42", HeadSHA: "abc123"},
	}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now, AllowAutoCommit: true, AllowAutoPush: true, OpenPRStrategy: config.OpenPRStrategyAllDone})

	claim, _ := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "worker-1", "worker")
	claim, _ = fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "worker-2", "worker")
	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "success" || result.PullRequestNumber != 42 {
		t.Fatalf("result = %#v, want success with PR 42", result)
	}
	if len(github.viewPRCalls) != 2 {
		t.Fatalf("len(github.viewPRCalls) = %d, want re-fetch before rename", len(github.viewPRCalls))
	}
	if len(github.updatePRTitleCalls) != 0 {
		t.Fatalf("len(github.updatePRTitleCalls) = %d, want 0", len(github.updatePRTitleCalls))
	}
}

func TestProcessClaimedItemIgnoresUpdatePRTitleErrorsDuringPushExistingTakeover(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	nowISO := fixture.nowISO()
	prNumber := int64(42)
	loopTarget := "pr:acme/looper:42"
	loopMeta := `{"worker":{"title":"Implement login flow","repo":"acme/looper","baseBranch":"main"}}`
	if err := fixture.repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: "loop_worker_pr", Seq: 2, ProjectID: "project_1", Type: "worker", TargetType: "pull_request", TargetID: &loopTarget, Repo: stringPtr("acme/looper"), PRNumber: &prNumber, Status: "queued", MetadataJSON: &loopMeta, NextRunAt: &nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	projectID := "project_1"
	loopID := "loop_worker_pr"
	payload := `{"title":"Implement login flow","repo":"acme/looper","baseBranch":"main"}`
	if err := fixture.repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: "queue_worker_pr", ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "pull_request", TargetID: loopTarget, Repo: stringPtr("acme/looper"), PRNumber: &prNumber, DedupeKey: "worker:acme/looper:42", Priority: 1, Status: "queued", AvailableAt: nowISO, MaxAttempts: 3, PayloadJSON: &payload, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}
	git := &fakeGitGateway{createResult: CreateWorktreeResult{WorktreePath: filepath.Join(t.TempDir(), "wt"), Branch: "feature/pr-42", BaseBranch: "main", HeadSHA: "abc123", WorktreeID: "worktree_1"}}
	github := &fakeGitHubGateway{prDetail: PullRequestDetail{Number: 42, Title: "Spec: Add login flow", Body: "## Summary\n\nSpec: specs/login/spec.md", BaseRefName: "main", HeadRefName: "feature/pr-42", HeadSHA: "abc123"}, updatePRTitleErrors: []error{errors.New("title update blocked")}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now, AllowAutoCommit: true, AllowAutoPush: true, OpenPRStrategy: config.OpenPRStrategyAllDone})

	claim, _ := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "worker-1", "worker")
	claim, _ = fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "worker-2", "worker")
	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "success" || result.PullRequestNumber != 42 {
		t.Fatalf("result = %#v, want success with PR 42", result)
	}
	if len(github.updatePRTitleCalls) != 1 {
		t.Fatalf("len(github.updatePRTitleCalls) = %d, want 1", len(github.updatePRTitleCalls))
	}
	if got := github.updatePRTitleCalls[0].Title; got != "Implement login flow" {
		t.Fatalf("updated title = %q, want worker title", got)
	}
}

func TestProcessClaimedItemDoesNotRenamePlannerSpecPRWhenPushFails(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	nowISO := fixture.nowISO()
	prNumber := int64(42)
	loopTarget := "pr:acme/looper:42"
	loopMeta := `{"worker":{"title":"Implement login flow","repo":"acme/looper","baseBranch":"main"}}`
	if err := fixture.repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: "loop_worker_pr", Seq: 2, ProjectID: "project_1", Type: "worker", TargetType: "pull_request", TargetID: &loopTarget, Repo: stringPtr("acme/looper"), PRNumber: &prNumber, Status: "queued", MetadataJSON: &loopMeta, NextRunAt: &nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	projectID := "project_1"
	loopID := "loop_worker_pr"
	payload := `{"title":"Implement login flow","repo":"acme/looper","baseBranch":"main"}`
	if err := fixture.repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: "queue_worker_pr", ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "pull_request", TargetID: loopTarget, Repo: stringPtr("acme/looper"), PRNumber: &prNumber, DedupeKey: "worker:acme/looper:42", Priority: 1, Status: "queued", AvailableAt: nowISO, MaxAttempts: 3, PayloadJSON: &payload, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}
	git := &fakeGitGateway{createResult: CreateWorktreeResult{WorktreePath: filepath.Join(t.TempDir(), "wt"), Branch: "feature/pr-42", BaseBranch: "main", HeadSHA: "abc123", WorktreeID: "worktree_1"}, pushErrors: []error{errors.New("push failed")}}
	github := &fakeGitHubGateway{prDetail: PullRequestDetail{Number: 42, Title: "Spec: Add login flow", Body: "## Summary\n\nSpec: specs/login/spec.md", BaseRefName: "main", HeadRefName: "feature/pr-42", HeadSHA: "abc123"}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now, AllowAutoCommit: true, AllowAutoPush: true, OpenPRStrategy: config.OpenPRStrategyAllDone})

	claim, _ := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "worker-1", "worker")
	claim, _ = fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "worker-2", "worker")
	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "failed" || result.FailureKind != FailureRetryableAfterResume {
		t.Fatalf("result = %#v, want retryable failure", result)
	}
	if len(github.updatePRTitleCalls) != 0 {
		t.Fatalf("len(github.updatePRTitleCalls) = %d, want 0", len(github.updatePRTitleCalls))
	}
}

func TestProcessClaimedItemFindsExistingPRAfterPush(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	branch := buildWorkerBranchName(workerInput{Title: "Implement worker loop", Repo: "acme/looper", BaseBranch: "main", ExecutionMode: "create-pr", IssueNumber: 27}, "loop_worker_1")
	git := &fakeGitGateway{createResult: CreateWorktreeResult{WorktreePath: filepath.Join(t.TempDir(), "wt"), Branch: branch, BaseBranch: "main", HeadSHA: "abc123", WorktreeID: "worktree_1"}}
	github := &fakeGitHubGateway{openPRResponses: [][]PullRequestSummary{{}, {{Number: 201, URL: "https://example/pr/201", State: "OPEN", HeadRefName: branch, BaseRefName: "main"}}}}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "done", Stdout: "ok", ParseStatus: "parsed"}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, AllowAutoCommit: true, AllowAutoPush: true, OpenPRStrategy: config.OpenPRStrategyAllDone})

	claim, _ := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "worker-1", "worker")
	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "success" || result.PullRequestNumber != 201 {
		t.Fatalf("result = %#v, want success with reused PR 201", result)
	}
	if len(github.createPRCalls) != 0 {
		t.Fatalf("len(github.createPRCalls) = %d, want 0", len(github.createPRCalls))
	}
}

func TestProcessClaimedItemFailsWhenCreatedPRNumberIsMissing(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	git := &fakeGitGateway{createResult: CreateWorktreeResult{WorktreePath: filepath.Join(t.TempDir(), "wt"), Branch: "looper/feature", BaseBranch: "main", HeadSHA: "abc123", WorktreeID: "worktree_1"}}
	github := &fakeGitHubGateway{createPRResult: CreatePullRequestResult{Number: 0, URL: "https://example/pr/unparsed"}}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "done", Stdout: "ok", ParseStatus: "parsed"}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, AllowAutoCommit: true, AllowAutoPush: true, OpenPRStrategy: config.OpenPRStrategyAllDone})

	claim, _ := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "worker-1", "worker")
	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "failed" || result.FailureKind != FailureRetryableAfterResume || !strings.Contains(result.Summary, "requires a pull request number") {
		t.Fatalf("result = %#v, want retryable_after_resume missing-pr-number failure", result)
	}
	if len(github.reviewerCalls) != 0 {
		t.Fatalf("len(github.reviewerCalls) = %d, want 0", len(github.reviewerCalls))
	}
	loop, err := fixture.repos.Loops.GetByID(context.Background(), result.LoopID)
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if loop == nil || (loop.MetadataJSON != nil && strings.Contains(*loop.MetadataJSON, `"prNumber":0`)) {
		t.Fatalf("loop = %#v, want no persisted PR number 0", loop)
	}
}

func TestProcessClaimedItemUsesIssueScopedLockForIssueWork(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	runner := New(Options{
		DB:              fixture.coordinator.DB(),
		Repos:           fixture.repos,
		GitHub:          &fakeGitHubGateway{issueCommentResult: IssueCommentResult{ID: 501}},
		Git:             &fakeGitGateway{createResult: CreateWorktreeResult{WorktreePath: filepath.Join(t.TempDir(), "wt"), Branch: "looper/feature", BaseBranch: "main", HeadSHA: "abc123", WorktreeID: "worktree_1"}},
		AgentExecutor:   &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "done", ParseStatus: "parsed"}}},
		Logger:          fixture.logger,
		Now:             fixture.now,
		AllowAutoCommit: true,
		AllowAutoPush:   false,
		OpenPRStrategy:  config.OpenPRStrategyAllDone,
	})

	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "worker-1", "worker")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNextOfType() = (%#v, %v), want claimed item", claim, err)
	}
	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "skipped" {
		t.Fatalf("result = %#v, want skipped after manual PR gating", result)
	}
	lock, err := fixture.repos.Locks.Get(context.Background(), "issue:acme/looper:27")
	if err != nil {
		t.Fatalf("Locks.Get() error = %v", err)
	}
	if lock != nil {
		t.Fatalf("lock = %#v, want issue lock released", lock)
	}
}

func TestRunValidationUsesShellCommandsByDefault(t *testing.T) {
	t.Parallel()
	runner := New(Options{})
	result, err := runner.runValidation(context.Background(), ValidationInput{
		CWD:      t.TempDir(),
		Commands: []string{"printf 'hello'", "printf 'warn' >&2"},
	})
	if err != nil {
		t.Fatalf("runValidation() error = %v", err)
	}
	if !result.Passed {
		t.Fatalf("result = %#v, want passed", result)
	}
	if result.Summary != "Validation passed" {
		t.Fatalf("Summary = %q, want Validation passed", result.Summary)
	}
	if result.Output != "hello\nwarn" {
		t.Fatalf("Output = %q, want joined shell output", result.Output)
	}
}

func TestRunValidationReturnsCommandFailureOutput(t *testing.T) {
	t.Parallel()
	runner := New(Options{})
	result, err := runner.runValidation(context.Background(), ValidationInput{
		CWD:      t.TempDir(),
		Commands: []string{"printf 'bad' >&2; exit 9"},
	})
	if err != nil {
		t.Fatalf("runValidation() error = %v", err)
	}
	if result.Passed {
		t.Fatalf("result = %#v, want failed validation", result)
	}
	if result.Summary != "Validation failed: printf 'bad' >&2; exit 9" {
		t.Fatalf("Summary = %q, want command-specific failure", result.Summary)
	}
	if result.Output != "bad" {
		t.Fatalf("Output = %q, want stderr output", result.Output)
	}
}

func TestProcessClaimedItemExecuteResumeDoesNotRerunAgentAfterTransientInspectFailure(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	git := &fakeGitGateway{
		createResult:  CreateWorktreeResult{WorktreePath: filepath.Join(t.TempDir(), "wt"), Branch: "looper/feature", BaseBranch: "main", HeadSHA: "abc123", WorktreeID: "worktree_1"},
		inspectErrors: []error{fmt.Errorf("temporary inspect failure")},
	}
	github := &fakeGitHubGateway{createPRResult: CreatePullRequestResult{Number: 101, URL: "https://example/pr/101"}}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "done", Stdout: "ok", ParseStatus: "parsed"}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, AllowAutoCommit: true, AllowAutoPush: true, OpenPRStrategy: config.OpenPRStrategyAllDone})

	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "worker-1", "worker")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNextOfType() = (%#v, %v), want claimed item", claim, err)
	}
	first, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem(first) error = %v", err)
	}
	if first.Status != "failed" || first.FailureKind != FailureRetryableAfterResume {
		t.Fatalf("first = %#v, want retryable_after_resume failure", first)
	}
	if len(agent.starts) != 1 {
		t.Fatalf("agent starts after first attempt = %d, want 1", len(agent.starts))
	}

	fixture.advance(5 * time.Second)
	retryClaim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "worker-1", "worker")
	if err != nil || retryClaim == nil {
		t.Fatalf("ClaimNextOfType(retry) = (%#v, %v), want claimed item", retryClaim, err)
	}
	second, err := runner.ProcessClaimedItem(context.Background(), *retryClaim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem(second) error = %v", err)
	}
	if second.Status != "success" {
		t.Fatalf("second = %#v, want success", second)
	}
	if len(agent.starts) != 1 {
		t.Fatalf("len(agent.starts) = %d, want 1", len(agent.starts))
	}
	if len(git.pushCalls) != 1 {
		t.Fatalf("len(git.pushCalls) = %d, want 1", len(git.pushCalls))
	}
	if len(github.createPRCalls) != 1 {
		t.Fatalf("len(github.createPRCalls) = %d, want 1", len(github.createPRCalls))
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
	coordinator, err := storage.OpenSQLiteCoordinator(context.Background(), filepath.Join(t.TempDir(), "worker.sqlite"), storage.SQLiteCoordinatorOptions{BackupDir: t.TempDir()})
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
	metadata := `{"repo":"acme/looper","worktreeRoot":"` + filepath.Join(t.TempDir(), "worktrees") + `"}`
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "project_1", Name: "Looper", RepoPath: filepath.Join(t.TempDir(), "repo"), BaseBranch: &baseBranch, MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	loopTarget := "issue:acme/looper:27"
	loopMetadata := `{"worker":{"title":"Implement worker loop","repo":"acme/looper","issueNumber":27,"baseBranch":"main"}}`
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: "loop_worker_1", Seq: 1, ProjectID: "project_1", Type: "worker", TargetType: "issue", TargetID: &loopTarget, Repo: stringPtr("acme/looper"), Status: "queued", MetadataJSON: &loopMetadata, NextRunAt: &nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	projectID := "project_1"
	loopID := "loop_worker_1"
	payload := `{"title":"Implement worker loop","repo":"acme/looper","issueNumber":27,"baseBranch":"main"}`
	lockKey := "issue:acme/looper:27"
	if err := repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: "queue_worker_1", ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "issue", TargetID: lockKey, Repo: stringPtr("acme/looper"), DedupeKey: "worker:project_1:acme/looper:27", Priority: 1, Status: "queued", AvailableAt: nowISO, Attempts: 0, MaxAttempts: 3, LockKey: &lockKey, PayloadJSON: &payload, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}
	fixture := &runnerFixture{coordinator: coordinator, repos: repos, logger: &testLogger{}, current: now}
	fixture.now = func() time.Time { return fixture.current }
	return fixture
}

func (f *runnerFixture) nowISO() string {
	return fmt.Sprintf("%s.000Z", f.current.UTC().Format("2006-01-02T15:04:05"))
}

func (f *runnerFixture) advance(delta time.Duration) { f.current = f.current.Add(delta) }

type fakeGitHubGateway struct {
	openPRs                 []PullRequestSummary
	openPRResponses         [][]PullRequestSummary
	openPRIndex             int
	prDetail                PullRequestDetail
	prDetailResponses       []PullRequestDetail
	viewPRIndex             int
	issueDetail             IssueDetail
	viewPRCalls             []ViewPullRequestInput
	viewIssueCalls          []ViewIssueInput
	createIssueCommentCalls []IssueCommentInput
	updateIssueCommentCalls []UpdateIssueCommentInput
	issueCommentResult      IssueCommentResult
	createPRResult          CreatePullRequestResult
	createPRErrors          []error
	createPRCalls           []CreatePullRequestInput
	updatePRTitleCalls      []UpdatePullRequestTitleInput
	updatePRTitleErrors     []error
	updatePRTitleIndex      int
	removeLabels            []PullRequestLabelsInput
	reviewerCalls           []PullRequestReviewersInput
	createPRIndex           int
}

func (f *fakeGitHubGateway) ListOpenPullRequests(context.Context, ListOpenPullRequestsInput) ([]PullRequestSummary, error) {
	if f.openPRIndex < len(f.openPRResponses) {
		result := append([]PullRequestSummary(nil), f.openPRResponses[f.openPRIndex]...)
		f.openPRIndex++
		return result, nil
	}
	return append([]PullRequestSummary(nil), f.openPRs...), nil
}

func (f *fakeGitHubGateway) ViewPullRequest(_ context.Context, input ViewPullRequestInput) (PullRequestDetail, error) {
	f.viewPRCalls = append(f.viewPRCalls, input)
	detail := f.prDetail
	if f.viewPRIndex < len(f.prDetailResponses) {
		detail = f.prDetailResponses[f.viewPRIndex]
	}
	f.viewPRIndex++
	if detail.Number == 0 {
		detail.Number = input.PRNumber
	}
	if detail.BaseRefName == "" {
		detail.BaseRefName = "main"
	}
	if detail.HeadRefName == "" {
		detail.HeadRefName = fmt.Sprintf("pr-%d", input.PRNumber)
	}
	if detail.Title == "" {
		detail.Title = "Existing PR"
	}
	if detail.URL == "" {
		detail.URL = fmt.Sprintf("https://example/pr/%d", input.PRNumber)
	}
	return detail, nil
}

func (f *fakeGitHubGateway) ViewIssue(_ context.Context, input ViewIssueInput) (IssueDetail, error) {
	f.viewIssueCalls = append(f.viewIssueCalls, input)
	detail := f.issueDetail
	if detail.Number == 0 {
		detail.Number = input.IssueNumber
	}
	if detail.Title == "" {
		detail.Title = "Issue"
	}
	return detail, nil
}

func (f *fakeGitHubGateway) CreateIssueComment(_ context.Context, input IssueCommentInput) (IssueCommentResult, error) {
	f.createIssueCommentCalls = append(f.createIssueCommentCalls, input)
	if f.issueCommentResult.ID == 0 {
		return IssueCommentResult{ID: 1}, nil
	}
	return f.issueCommentResult, nil
}

func (f *fakeGitHubGateway) UpdateIssueComment(_ context.Context, input UpdateIssueCommentInput) error {
	f.updateIssueCommentCalls = append(f.updateIssueCommentCalls, input)
	return nil
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

func (f *fakeGitHubGateway) UpdatePullRequestTitle(_ context.Context, input UpdatePullRequestTitleInput) error {
	f.updatePRTitleCalls = append(f.updatePRTitleCalls, input)
	if f.updatePRTitleIndex < len(f.updatePRTitleErrors) {
		err := f.updatePRTitleErrors[f.updatePRTitleIndex]
		f.updatePRTitleIndex++
		return err
	}
	return nil
}

func (f *fakeGitHubGateway) RemovePullRequestLabels(_ context.Context, input PullRequestLabelsInput) error {
	f.removeLabels = append(f.removeLabels, input)
	return nil
}

func (f *fakeGitHubGateway) AddPullRequestReviewers(_ context.Context, input PullRequestReviewersInput) error {
	f.reviewerCalls = append(f.reviewerCalls, input)
	return nil
}

type fakeGitGateway struct {
	createResult  CreateWorktreeResult
	prepareResult PrepareWorktreeResult
	inspectResult InspectHeadResult
	inspectErrors []error
	inspectIndex  int
	commitResult  CommitResult
	createCalls   []CreateWorktreeInput
	pushCalls     []PushInput
	prepareCalls  []PrepareWorktreeInput
	inspectCalls  []InspectHeadInput
	commitCalls   []CommitInput
	pushErrors    []error
	pushIndex     int
}

func (f *fakeGitGateway) CreateWorktree(_ context.Context, input CreateWorktreeInput) (CreateWorktreeResult, error) {
	f.createCalls = append(f.createCalls, input)
	return f.createResult, nil
}

func (f *fakeGitGateway) PrepareWorktree(_ context.Context, input PrepareWorktreeInput) (PrepareWorktreeResult, error) {
	f.prepareCalls = append(f.prepareCalls, input)
	if f.prepareResult.HeadSHA == "" {
		return PrepareWorktreeResult{HeadSHA: "abc123", Clean: true}, nil
	}
	return f.prepareResult, nil
}

func (f *fakeGitGateway) InspectHead(_ context.Context, input InspectHeadInput) (InspectHeadResult, error) {
	f.inspectCalls = append(f.inspectCalls, input)
	if f.inspectIndex < len(f.inspectErrors) && f.inspectErrors[f.inspectIndex] != nil {
		err := f.inspectErrors[f.inspectIndex]
		f.inspectIndex++
		return InspectHeadResult{}, err
	}
	f.inspectIndex++
	return f.inspectResult, nil
}

func (f *fakeGitGateway) Commit(_ context.Context, input CommitInput) (CommitResult, error) {
	f.commitCalls = append(f.commitCalls, input)
	if f.commitResult.CommitSHA == "" {
		return CommitResult{CommitSHA: "fallback123"}, nil
	}
	return f.commitResult, nil
}

func (f *fakeGitGateway) Push(_ context.Context, input PushInput) error {
	f.pushCalls = append(f.pushCalls, input)
	if f.pushIndex < len(f.pushErrors) && f.pushErrors[f.pushIndex] != nil {
		err := f.pushErrors[f.pushIndex]
		f.pushIndex++
		return err
	}
	f.pushIndex++
	return nil
}

type fakeAgentExecutor struct {
	starts  []AgentRunInput
	results []AgentResult
	index   int
	waitErr error
	wait    func(context.Context) error
}

func (f *fakeAgentExecutor) Start(_ context.Context, input AgentRunInput) (AgentExecution, error) {
	f.starts = append(f.starts, input)
	result := AgentResult{Status: "completed", Summary: "done", ParseStatus: "parsed"}
	if f.index < len(f.results) {
		result = f.results[f.index]
	}
	if result.Status == "completed" && result.ParseStatus == "" {
		result.ParseStatus = "parsed"
	}
	f.index++
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
