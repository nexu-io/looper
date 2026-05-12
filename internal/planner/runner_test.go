package planner

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/disclosure"
	"github.com/nexu-io/looper/internal/infra/specpr"
	"github.com/nexu-io/looper/internal/lifecycle"
	"github.com/nexu-io/looper/internal/storage"
)

func TestBuildPlannerPromptUsesConcreteDisclosureMetadata(t *testing.T) {
	t.Parallel()

	repoPath := t.TempDir()
	prompt, _ := buildPlannerPrompt(storage.ProjectRecord{ID: "project_1", RepoPath: repoPath}, customInstructionConfig(nil), &checkpointIssue{Repo: "acme/looper", IssueNumber: 156, Title: "fix disclosure", SpecPath: "docs/spec.md"}, &checkpointWorktree{Branch: "looper/fix", BaseBranch: "main"}, true, config.DefaultDisclosureConfig(), "opencode", "openai/gpt-5.5")
	for _, want := range []string{"agent=opencode", "model=openai/gpt-5.5"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
	for _, unwanted := range []string{"agent=<agent-runtime>", "model=<agent-model>", "agent=gpt-5.5", "agent=openai/gpt-5.5"} {
		if strings.Contains(prompt, unwanted) {
			t.Fatalf("prompt contains %q:\n%s", unwanted, prompt)
		}
	}
}

func TestBuildPlannerPromptOmitsMissingAgentRuntime(t *testing.T) {
	t.Parallel()

	repoPath := t.TempDir()
	prompt, _ := buildPlannerPrompt(storage.ProjectRecord{ID: "project_1", RepoPath: repoPath}, customInstructionConfig(nil), &checkpointIssue{Repo: "acme/looper", IssueNumber: 156, Title: "fix disclosure", SpecPath: "docs/spec.md"}, &checkpointWorktree{Branch: "looper/fix", BaseBranch: "main"}, true, config.DefaultDisclosureConfig(), "", "openai/gpt-5.5")
	if strings.Contains(prompt, "agent=") {
		t.Fatalf("prompt should omit missing agent runtime:\n%s", prompt)
	}
	if !strings.Contains(prompt, "model=openai/gpt-5.5") {
		t.Fatalf("prompt should include configured model:\n%s", prompt)
	}
}

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
	loopResult, err := (&Runner{repos: fixture.repos, now: fixture.now}).ensureLoopForIssue(context.Background(), storage.ProjectRecord{ID: "project_1"}, "acme/looper", issue, buildPlannerDiscoveryFingerprint("acme/looper", fixture.now(), issue))
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

func TestDiscoverIssuesUsesSingleServerSideLabelFilterWhenConfiguredWithMultipleLabels(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{issues: []IssueSummary{{Number: 42, Title: "Plan this", Labels: []string{"team:alpha", "team:beta"}}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now, DiscoveryPolicy: DiscoveryPolicy{AutoDiscovery: true, Labels: []string{"team:alpha", "team:beta"}, LabelMode: config.LabelModeAll, RequireAssigneeCurrentUser: false}})

	if _, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	if len(github.listOpenIssueCalls) != 1 {
		t.Fatalf("listOpenIssueCalls = %#v, want one call", github.listOpenIssueCalls)
	}
	if got := github.listOpenIssueCalls[0].Labels; len(got) != 2 || got[0] != "team:alpha" || got[1] != "team:beta" {
		t.Fatalf("ListOpenIssues labels = %#v, want both configured labels", got)
	}
}

func TestDiscoverIssuesQueriesEachServerSideLabelWhenConfiguredWithAnyLabelMode(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{issues: []IssueSummary{{Number: 43, Title: "Plan any", Labels: []string{"team:beta"}}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now, DiscoveryPolicy: DiscoveryPolicy{AutoDiscovery: true, Labels: []string{"team:alpha", "team:beta"}, LabelMode: config.LabelModeAny, RequireAssigneeCurrentUser: false}})

	if _, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	if len(github.listOpenIssueCalls) != 2 {
		t.Fatalf("listOpenIssueCalls = %#v, want two calls", github.listOpenIssueCalls)
	}
	if github.listOpenIssueCalls[0].Label != "team:alpha" || github.listOpenIssueCalls[1].Label != "team:beta" {
		t.Fatalf("ListOpenIssues labels = [%q, %q], want configured labels", github.listOpenIssueCalls[0].Label, github.listOpenIssueCalls[1].Label)
	}
}

func TestDiscoverIssuesSkipsFailedPlannerLoopWhenFingerprintUnchanged(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	nowISO := fixture.nowISO()
	repo := "acme/looper"
	issue := IssueSummary{Number: 77, Title: "Plan this", Body: "same body", URL: "https://github.com/acme/looper/issues/77", Assignees: []string{"octocat"}, Labels: []string{"looper:plan"}}
	fingerprint := buildPlannerDiscoveryFingerprint(repo, fixture.now(), issue)
	metadata := fmt.Sprintf(`{"autonomousRecovery":{"lastFailedDiscoveryFingerprint":%q}}`, fingerprint)
	targetID := buildIssueTargetID(repo, issue.Number)
	if err := fixture.repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: "loop_planner_failed_same_fp", Seq: 88, ProjectID: "project_1", Type: "planner", TargetType: "issue", TargetID: &targetID, Repo: &repo, Status: "failed", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	github := &fakeGitHubGateway{issues: []IssueSummary{issue}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now, AllowAutoPush: boolPtr(true)})

	result, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: repo})
	if err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	if len(result.QueueItems) != 0 {
		t.Fatalf("QueueItems = %#v, want none for unchanged failed fingerprint", result.QueueItems)
	}
}

func TestDiscoverIssuesRequeuesFailedPlannerLoopWhenFingerprintChanges(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	nowISO := fixture.nowISO()
	repo := "acme/looper"
	oldIssue := IssueSummary{Number: 78, Title: "Plan this", Body: "old body", URL: "https://github.com/acme/looper/issues/78", Assignees: []string{"octocat"}, Labels: []string{"looper:plan"}}
	newIssue := IssueSummary{Number: 78, Title: "Plan this", Body: "new body", URL: "https://github.com/acme/looper/issues/78", Assignees: []string{"octocat"}, Labels: []string{"looper:plan"}}
	fingerprint := buildPlannerDiscoveryFingerprint(repo, fixture.now(), oldIssue)
	metadata := fmt.Sprintf(`{"autonomousRecovery":{"lastFailedDiscoveryFingerprint":%q}}`, fingerprint)
	targetID := buildIssueTargetID(repo, newIssue.Number)
	if err := fixture.repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: "loop_planner_failed_changed_fp", Seq: 89, ProjectID: "project_1", Type: "planner", TargetType: "issue", TargetID: &targetID, Repo: &repo, Status: "failed", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	github := &fakeGitHubGateway{issues: []IssueSummary{newIssue}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now, AllowAutoPush: boolPtr(true)})

	result, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: repo})
	if err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	if len(result.QueueItems) != 1 {
		t.Fatalf("QueueItems = %#v, want one queue item after fingerprint change", result.QueueItems)
	}
}

func TestRunPrepareWorktreeStepRecreatesUnsafeCheckpointAtRepoPath(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	repoPath := t.TempDir()
	git := &fakeGitGateway{createResult: CreateWorktreeResult{WorktreePath: filepath.Join(t.TempDir(), "wt"), Branch: "looper/planner/42-plan-this", BaseBranch: "main"}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, Git: git, Logger: fixture.logger, Now: fixture.now})

	checkpoint, err := runner.runPrepareWorktreeStep(context.Background(), stepInput{
		Project: storage.ProjectRecord{ID: "project_1", RepoPath: repoPath},
		Checkpoint: plannerCheckpoint{
			Issue:    &checkpointIssue{Repo: "acme/looper", IssueNumber: 42, Title: "Plan this", SpecPath: "specs/42.md"},
			Worktree: &checkpointWorktree{Path: repoPath, Branch: "stale", BaseBranch: "main"},
		},
	})
	if err != nil {
		t.Fatalf("runPrepareWorktreeStep() error = %v", err)
	}
	if len(git.createCalls) != 1 {
		t.Fatalf("len(git.createCalls) = %d, want 1", len(git.createCalls))
	}
	if checkpoint.Worktree == nil || checkpoint.Worktree.Path != git.createResult.WorktreePath {
		t.Fatalf("checkpoint.Worktree = %#v, want recreated worktree", checkpoint.Worktree)
	}
	if checkpoint.ResumePolicy != "advance_from_checkpoint" {
		t.Fatalf("ResumePolicy = %q, want advance_from_checkpoint", checkpoint.ResumePolicy)
	}
}

func TestRunPrepareWorktreeStepRecreatesCheckpointOutsideWorktreeRoot(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	repoPath := t.TempDir()
	worktreeRoot := filepath.Join(t.TempDir(), "worktrees")
	legacyPath := filepath.Join(t.TempDir(), "legacy-wt")
	metadata := fmt.Sprintf(`{"worktreeRoot":%q}`, worktreeRoot)
	git := &fakeGitGateway{createResult: CreateWorktreeResult{WorktreePath: filepath.Join(worktreeRoot, "wt"), Branch: "looper/planner/42-plan-this", BaseBranch: "main"}}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "wrote spec"}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, Git: git, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now})

	checkpoint, err := runner.runPrepareWorktreeStep(context.Background(), stepInput{
		Project: storage.ProjectRecord{ID: "project_1", RepoPath: repoPath, MetadataJSON: &metadata},
		Checkpoint: plannerCheckpoint{
			Issue:    &checkpointIssue{Repo: "acme/looper", IssueNumber: 42, Title: "Plan this", SpecPath: "specs/42.md"},
			Worktree: &checkpointWorktree{Path: legacyPath, Branch: "stale", BaseBranch: "main"},
		},
	})
	if err != nil {
		t.Fatalf("runPrepareWorktreeStep() error = %v", err)
	}
	if len(git.createCalls) != 1 {
		t.Fatalf("len(git.createCalls) = %d, want 1", len(git.createCalls))
	}
	if checkpoint.Worktree == nil || checkpoint.Worktree.Path != git.createResult.WorktreePath {
		t.Fatalf("checkpoint.Worktree = %#v, want recreated worktree", checkpoint.Worktree)
	}
	if checkpoint.Worktree.Path == legacyPath {
		t.Fatalf("checkpoint.Worktree.Path = %q, want recreated path outside legacy worktree", checkpoint.Worktree.Path)
	}
	if git.createCalls[0].WorktreeRoot != worktreeRoot {
		t.Fatalf("CreateWorktree().WorktreeRoot = %q, want %q", git.createCalls[0].WorktreeRoot, worktreeRoot)
	}
}

func TestRunWriteSpecStepRecreatesCheckpointOutsideWorktreeRootAndRunsAgent(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	repoPath := t.TempDir()
	worktreeRoot := filepath.Join(t.TempDir(), "worktrees")
	legacyPath := filepath.Join(t.TempDir(), "legacy-wt")
	metadata := fmt.Sprintf(`{"worktreeRoot":%q}`, worktreeRoot)
	issue := &checkpointIssue{Repo: "acme/looper", IssueNumber: 42, Title: "Plan this", SpecPath: "specs/42.md"}
	loopResult, err := (&Runner{repos: fixture.repos, now: fixture.now}).ensureLoopForIssue(context.Background(), storage.ProjectRecord{ID: "project_1"}, issue.Repo, IssueSummary{Number: issue.IssueNumber, Title: issue.Title}, buildPlannerDiscoveryFingerprint(issue.Repo, fixture.now(), IssueSummary{Number: issue.IssueNumber, Title: issue.Title}))
	if err != nil {
		t.Fatalf("ensureLoopForIssue() error = %v", err)
	}
	runID := "run_write_spec_rebuild"
	if err := fixture.repos.Runs.Upsert(context.Background(), storage.RunRecord{ID: runID, LoopID: loopResult.record.ID, Status: "running", CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}
	git := &fakeGitGateway{createResult: CreateWorktreeResult{WorktreePath: filepath.Join(worktreeRoot, "wt"), Branch: "looper/planner/42-plan-this", BaseBranch: "main"}}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "wrote spec"}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, Git: git, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now})

	checkpoint, err := runner.runWriteSpecStep(context.Background(), stepInput{
		Project: storage.ProjectRecord{ID: "project_1", RepoPath: repoPath, MetadataJSON: &metadata},
		Loop:    loopResult.record,
		Run:     storage.RunRecord{ID: runID, LoopID: loopResult.record.ID},
		Checkpoint: plannerCheckpoint{
			Issue:     issue,
			Worktree:  &checkpointWorktree{Path: legacyPath, Branch: "stale", BaseBranch: "main"},
			WriteSpec: &checkpointWriteSpec{Status: "completed"},
		},
	})
	if err != nil {
		t.Fatalf("runWriteSpecStep() error = %v", err)
	}
	if len(git.createCalls) != 1 {
		t.Fatalf("len(git.createCalls) = %d, want 1", len(git.createCalls))
	}
	if checkpoint.Worktree == nil || checkpoint.Worktree.Path != git.createResult.WorktreePath {
		t.Fatalf("checkpoint.Worktree = %#v, want recreated worktree", checkpoint.Worktree)
	}
	if git.createCalls[0].WorktreeRoot != worktreeRoot {
		t.Fatalf("CreateWorktree().WorktreeRoot = %q, want %q", git.createCalls[0].WorktreeRoot, worktreeRoot)
	}
	if len(agent.starts) != 1 {
		t.Fatalf("len(agent.starts) = %d, want 1", len(agent.starts))
	}
	if agent.starts[0].WorkingDirectory != git.createResult.WorktreePath {
		t.Fatalf("agent WorkingDirectory = %q, want rebuilt worktree %q", agent.starts[0].WorkingDirectory, git.createResult.WorktreePath)
	}
	if checkpoint.WriteSpec == nil || checkpoint.WriteSpec.Status != "completed" || !checkpoint.WriteSpec.GitReconciled {
		t.Fatalf("checkpoint.WriteSpec = %#v, want completed and reconciled write-spec after worktree recovery", checkpoint.WriteSpec)
	}
}

func TestProcessClaimedItemManualPlannerBypassesDiscoveryChecks(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	issue := IssueSummary{Number: 42, Title: "Plan this"}
	loopResult, err := (&Runner{repos: fixture.repos, now: fixture.now}).ensureLoopForIssue(context.Background(), storage.ProjectRecord{ID: "project_1"}, "acme/looper", issue, buildPlannerDiscoveryFingerprint("acme/looper", fixture.now(), issue))
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
	loopResult, err := (&Runner{repos: fixture.repos, now: fixture.now}).ensureLoopForIssue(context.Background(), storage.ProjectRecord{ID: "project_1"}, "acme/looper", issue, buildPlannerDiscoveryFingerprint("acme/looper", fixture.now(), issue))
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
	loopResult, err := (&Runner{repos: fixture.repos, now: fixture.now}).ensureLoopForIssue(context.Background(), storage.ProjectRecord{ID: "project_1"}, "acme/looper", issue, buildPlannerDiscoveryFingerprint("acme/looper", fixture.now(), issue))
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
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "wrote spec", Stdout: "done"}}}
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

func TestProcessClaimedItemUsesConfiguredPlannerPolicyLabels(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{issues: []IssueSummary{{Number: 42, Title: "Plan this", Labels: []string{"team:alpha"}}}, issueDetail: IssueDetail{Number: 42, Title: "Plan this", Body: "details", URL: "https://example/issues/42", Labels: []string{"team:alpha"}}, createPRResult: CreatePullRequestResult{Number: 101, URL: "https://example/pr/101"}, loginErr: fmt.Errorf("login unavailable")}
	git := &fakeGitGateway{createResult: CreateWorktreeResult{ID: "worktree_1", WorktreePath: filepath.Join(t.TempDir(), "wt"), Branch: "looper/planner/42-plan-this", BaseBranch: "main"}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git, AgentExecutor: &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "wrote spec"}}}, Logger: fixture.logger, Now: fixture.now, AllowAutoPush: boolPtr(true), DiscoveryPolicy: DiscoveryPolicy{AutoDiscovery: true, Labels: []string{"team:alpha"}, LabelMode: config.LabelModeAll, RequireAssigneeCurrentUser: false}})

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
	if len(github.addAssigneeCalls) != 0 {
		t.Fatalf("addAssigneeCalls = %#v, want no self-assignment when assignee policy is disabled", github.addAssigneeCalls)
	}
	if github.loginCalls != 0 {
		t.Fatalf("loginCalls = %d, want no login lookup when assignee policy is disabled", github.loginCalls)
	}
}

func TestProcessClaimedItemExcludesCurrentUserFromAssigneeReviewersWhenAssigneePolicyDisabled(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{issues: []IssueSummary{{Number: 42, Title: "Plan this", Assignees: []string{"octocat"}, Labels: []string{"team:alpha"}}}, issueDetail: IssueDetail{Number: 42, Title: "Plan this", Body: "details", URL: "https://example/issues/42", Assignees: []string{"octocat"}, Labels: []string{"team:alpha"}}, createPRResult: CreatePullRequestResult{Number: 101, URL: "https://example/pr/101"}}
	git := &fakeGitGateway{createResult: CreateWorktreeResult{ID: "worktree_1", WorktreePath: filepath.Join(t.TempDir(), "wt"), Branch: "looper/planner/42-plan-this", BaseBranch: "main"}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git, AgentExecutor: &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "wrote spec"}}}, Logger: fixture.logger, Now: fixture.now, AllowAutoPush: boolPtr(true), DiscoveryPolicy: DiscoveryPolicy{AutoDiscovery: true, Labels: []string{"team:alpha"}, LabelMode: config.LabelModeAll, RequireAssigneeCurrentUser: false}})

	_, _ = runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "planner-worker-1", "planner")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNextOfType() = (%#v, %v), want claimed item", claim, err)
	}
	if _, err := runner.ProcessClaimedItem(context.Background(), *claim); err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if github.loginCalls != 1 {
		t.Fatalf("loginCalls = %d, want login lookup before resolving assignee reviewers", github.loginCalls)
	}
	if len(github.addReviewerCalls) != 0 {
		t.Fatalf("addReviewerCalls = %#v, want no self review request", github.addReviewerCalls)
	}
}

func TestProcessClaimedItemSelfAssignsIssue(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	issue := IssueSummary{Number: 42, Title: "Plan this"}
	loopResult, err := (&Runner{repos: fixture.repos, now: fixture.now}).ensureLoopForIssue(context.Background(), storage.ProjectRecord{ID: "project_1"}, "acme/looper", issue, buildPlannerDiscoveryFingerprint("acme/looper", fixture.now(), issue))
	if err != nil {
		t.Fatalf("ensureLoopForIssue() error = %v", err)
	}
	queueItem, err := (&Runner{repos: fixture.repos, now: fixture.now, retryMaxAttempts: 3}).enqueue(context.Background(), enqueueInput{ProjectID: "project_1", LoopID: loopResult.record.ID, Repo: "acme/looper", IssueNumber: issue.Number, Payload: map[string]any{"issueNumber": issue.Number, "manual": true}})
	if err != nil {
		t.Fatalf("enqueue() error = %v", err)
	}
	github := &fakeGitHubGateway{issueDetail: IssueDetail{Number: 42, Title: "Plan this", Body: "details", URL: "https://example/issues/42", Assignees: []string{"teammate"}, Labels: []string{"looper:plan"}}, createPRResult: CreatePullRequestResult{Number: 101, URL: "https://example/pr/101"}}
	git := &fakeGitGateway{createResult: CreateWorktreeResult{ID: "worktree_1", WorktreePath: filepath.Join(t.TempDir(), "wt"), Branch: "looper/planner/42-plan-this", BaseBranch: "main"}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git, AgentExecutor: &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "wrote spec"}}}, Logger: fixture.logger, Now: fixture.now, AllowAutoPush: boolPtr(true)})

	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "planner-worker-1", "planner")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNextOfType() = (%#v, %v), want claimed item", claim, err)
	}
	if claim.ID != queueItem.ID {
		t.Fatalf("claim.ID = %q, want %q", claim.ID, queueItem.ID)
	}
	if _, err := runner.ProcessClaimedItem(context.Background(), *claim); err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if len(github.addAssigneeCalls) != 1 {
		t.Fatalf("addAssigneeCalls = %#v, want one self-assignment", github.addAssigneeCalls)
	}
	call := github.addAssigneeCalls[0]
	if call.Repo != "acme/looper" || call.IssueNumber != 42 || len(call.Assignees) != 1 || call.Assignees[0] != "octocat" {
		t.Fatalf("add assignee call = %#v, want octocat on acme/looper#42", call)
	}
}

func TestProcessClaimedItemSurfacesIssueSelfAssignmentFailure(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	issue := IssueSummary{Number: 42, Title: "Plan this"}
	loopResult, err := (&Runner{repos: fixture.repos, now: fixture.now}).ensureLoopForIssue(context.Background(), storage.ProjectRecord{ID: "project_1"}, "acme/looper", issue, buildPlannerDiscoveryFingerprint("acme/looper", fixture.now(), issue))
	if err != nil {
		t.Fatalf("ensureLoopForIssue() error = %v", err)
	}
	if _, err := (&Runner{repos: fixture.repos, now: fixture.now, retryMaxAttempts: 3}).enqueue(context.Background(), enqueueInput{ProjectID: "project_1", LoopID: loopResult.record.ID, Repo: "acme/looper", IssueNumber: issue.Number, Payload: map[string]any{"issueNumber": issue.Number, "manual": true}}); err != nil {
		t.Fatalf("enqueue() error = %v", err)
	}
	github := &fakeGitHubGateway{issueDetail: IssueDetail{Number: 42, Title: "Plan this", Body: "details", URL: "https://example/issues/42", Assignees: []string{"teammate"}, Labels: []string{"looper:plan"}}, addAssigneeErr: fmt.Errorf("permission denied")}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})

	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "planner-worker-1", "planner")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNextOfType() = (%#v, %v), want claimed item", claim, err)
	}
	result, err := runner.ProcessClaimedQueueItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedQueueItem() error = %v", err)
	}
	if result == nil || result.Status != "failed" || result.FailureKind != FailureRetryableAfterResume || !strings.Contains(result.Summary, "Unable to assign issue acme/looper#42 to octocat") {
		t.Fatalf("result = %#v, want clear retryable assignment failure", result)
	}
	acquired, err := fixture.repos.Locks.Acquire(context.Background(), storage.LockRecord{Key: "issue:acme/looper:42", Owner: "retry", ExpiresAt: fixture.now().Add(time.Minute).UTC().Format("2006-01-02T15:04:05.000Z"), CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()})
	if err != nil {
		t.Fatalf("Locks.Acquire() error = %v", err)
	}
	if !acquired {
		t.Fatal("Locks.Acquire() = false, want assignment failure to release issue lock")
	}
}

func TestProcessClaimedItemAdoptsOpenBranchPRWithoutRewritingHumanBody(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	branch := "looper/planner/42-plan-this"
	github := &fakeGitHubGateway{issues: []IssueSummary{{Number: 42, Title: "Plan this", Assignees: []string{"octocat"}, Labels: []string{"looper:plan"}}}, issueDetail: IssueDetail{Number: 42, Title: "Plan this", Body: "details", URL: "https://example/issues/42", Assignees: []string{"octocat"}, Labels: []string{"looper:plan"}}, openPullRequests: []PullRequestSummary{{Number: 202, URL: "https://example/pr/202", State: "OPEN", HeadRefName: branch, BaseRefName: "main"}}}
	git := &fakeGitGateway{createResult: CreateWorktreeResult{ID: "worktree_1", WorktreePath: filepath.Join(t.TempDir(), "wt"), Branch: branch, BaseBranch: "main"}}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "wrote spec", Lifecycle: &lifecycle.State{Branch: branch, BaseBranch: "main", PRURL: "https://example/pr/202", Actions: lifecycle.Actions{PR: lifecycle.ActionSourceAgent}}}}}
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
	if result.Status != "success" || result.PullRequestNumber != 202 {
		t.Fatalf("result = %#v, want success with adopted PR 202", result)
	}
	if len(github.createPRCalls) != 0 {
		t.Fatalf("createPRCalls = %#v, want no fallback CreatePullRequest", github.createPRCalls)
	}
	if len(github.updatePRBodyCalls) != 0 {
		t.Fatalf("updatePRBodyCalls = %#v, want no body rewrite for human-authored PR", github.updatePRBodyCalls)
	}
	if len(github.listOpenPRCalls) != 1 {
		t.Fatalf("listOpenPRCalls = %d, want 1", len(github.listOpenPRCalls))
	}
	loop, err := fixture.repos.Loops.GetByID(context.Background(), result.LoopID)
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if loop == nil || loop.PRNumber == nil || *loop.PRNumber != 202 || loop.MetadataJSON == nil || !strings.Contains(*loop.MetadataJSON, `"prNumber":202`) {
		t.Fatalf("loop = %#v, want adopted PR persisted", loop)
	}
}

func TestProcessClaimedItemAdoptsLifecyclePRAndStampsMissingDisclosure(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	branch := "looper/planner/42-plan-this"
	github := &fakeGitHubGateway{
		issues:      []IssueSummary{{Number: 42, Title: "Plan this", Assignees: []string{"octocat"}, Labels: []string{"looper:plan"}}},
		issueDetail: IssueDetail{Number: 42, Title: "Plan this", Body: "details", URL: "https://example/issues/42", Assignees: []string{"octocat"}, Labels: []string{"looper:plan"}},
		prDetail:    PullRequestDetail{Number: 202, URL: "https://example/pr/202", State: "OPEN", HeadRefName: branch, BaseRefName: "main", Body: "## Summary\n\nLifecycle-created body"},
	}
	git := &fakeGitGateway{createResult: CreateWorktreeResult{ID: "worktree_1", WorktreePath: filepath.Join(t.TempDir(), "wt"), Branch: branch, BaseBranch: "main"}}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "wrote spec", Lifecycle: &lifecycle.State{Branch: branch, BaseBranch: "main", PRNumber: 202, PRURL: "https://example/pr/202", Actions: lifecycle.Actions{PR: lifecycle.ActionSourceAgent}}}}}
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
	if result.Status != "success" || result.PullRequestNumber != 202 {
		t.Fatalf("result = %#v, want success with adopted PR 202", result)
	}
	if len(github.updatePRBodyCalls) != 1 {
		t.Fatalf("updatePRBodyCalls = %#v, want one disclosure rewrite", github.updatePRBodyCalls)
	}
	if !strings.Contains(github.updatePRBodyCalls[0].Body, disclosure.Marker) {
		t.Fatalf("updated body = %q, want disclosure marker", github.updatePRBodyCalls[0].Body)
	}
	if !strings.Contains(github.updatePRBodyCalls[0].Body, "runner=planner") {
		t.Fatalf("updated body = %q, want planner disclosure footer", github.updatePRBodyCalls[0].Body)
	}
}

func TestProcessClaimedItemWriteSpecResumeDoesNotRerunAgentAfterTransientInspectFailure(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{issues: []IssueSummary{{Number: 42, Title: "Plan this", Assignees: []string{"octocat"}, Labels: []string{"looper:plan"}}}, issueDetail: IssueDetail{Number: 42, Title: "Plan this", Assignees: []string{"octocat"}, Labels: []string{"looper:plan"}, Body: "details", URL: "https://example/issues/42"}, createPRResult: CreatePullRequestResult{Number: 101, URL: "https://example/pr/101"}}
	git := &fakeGitGateway{
		createResult:  CreateWorktreeResult{ID: "worktree_1", WorktreePath: filepath.Join(t.TempDir(), "wt"), Branch: "looper/planner/42-plan-this", BaseBranch: "main"},
		inspectErrors: []error{fmt.Errorf("temporary inspect failure")},
	}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "wrote spec"}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, AllowAutoPush: boolPtr(true)})

	_, _ = runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "planner-worker-1", "planner")
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
	retryClaim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "planner-worker-1", "planner")
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

func TestValidatedLifecyclePullRequestTreatsLookupErrorAsNonAdoptable(t *testing.T) {
	t.Parallel()

	runner := New(Options{GitHub: &fakeGitHubGateway{viewPRErr: fmt.Errorf("not found")}})
	state := &lifecycle.State{PRNumber: 84, PRURL: "https://example/pr/84"}
	adopted, err := runner.validatedLifecyclePullRequest(context.Background(), stepInput{Project: storage.ProjectRecord{RepoPath: t.TempDir()}}, checkpointIssue{Repo: "acme/looper"}, checkpointWorktree{Branch: "looper/test", BaseBranch: "main"}, state)
	if err != nil {
		t.Fatalf("validatedLifecyclePullRequest() error = %v", err)
	}
	if adopted != nil {
		t.Fatalf("validatedLifecyclePullRequest() = %#v, want nil", adopted)
	}
}

func TestProcessClaimedItemResumeReleasesClaimedLockWhenSetupFails(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	issue := IssueSummary{Number: 42, Title: "Plan this", Assignees: []string{"octocat"}, Labels: []string{"looper:plan"}}
	loopResult, err := (&Runner{repos: fixture.repos, now: fixture.now}).ensureLoopForIssue(context.Background(), storage.ProjectRecord{ID: "project_1"}, "acme/looper", issue, buildPlannerDiscoveryFingerprint("acme/looper", fixture.now(), issue))
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
	checkpoint := parseCheckpoint(run.CheckpointJSON)
	if checkpoint.ResumePolicy != "retry_from_timeout_context" {
		t.Fatalf("checkpoint.ResumePolicy = %q, want retry_from_timeout_context", checkpoint.ResumePolicy)
	}
	if checkpoint.WriteSpec == nil || checkpoint.WriteSpec.Status != "failed" || checkpoint.WriteSpec.Summary != "agent failed" {
		t.Fatalf("checkpoint.WriteSpec = %#v, want failed persisted write-spec checkpoint", checkpoint.WriteSpec)
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

func TestWriteSpecSetupFailureStaysRetryable(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{issues: []IssueSummary{{Number: 42, Title: "Plan this", Assignees: []string{"octocat"}, Labels: []string{"looper:plan"}}}, issueDetail: IssueDetail{Number: 42, Title: "Plan this", Assignees: []string{"octocat"}, Labels: []string{"looper:plan"}}}
	git := &fakeGitGateway{createResult: CreateWorktreeResult{ID: "worktree_1", WorktreePath: filepath.Join(t.TempDir(), "wt"), Branch: "looper/planner/42-plan-this", BaseBranch: "main"}}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "failed", Summary: "unsupported model in agent configuration for codex"}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, AllowAutoPush: boolPtr(true)})

	_, _ = runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	claim, _ := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "planner-worker-1", "planner")
	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "failed" || result.FailureKind != FailureRetryableTransient {
		t.Fatalf("result = %#v, want retryable_transient setup failure", result)
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
		t.Fatalf("loop = %#v, want queued", loop)
	}
}

func TestPublishAutoPushDisabledPausesPlannerLoop(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{issues: []IssueSummary{{Number: 42, Title: "Plan this", Assignees: []string{"octocat"}, Labels: []string{"looper:plan"}}}, issueDetail: IssueDetail{Number: 42, Title: "Plan this", Assignees: []string{"octocat"}, Labels: []string{"looper:plan"}}}
	git := &fakeGitGateway{createResult: CreateWorktreeResult{ID: "worktree_1", WorktreePath: filepath.Join(t.TempDir(), "wt"), Branch: "looper/planner/42-plan-this", BaseBranch: "main"}}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "wrote spec"}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, AllowAutoPush: boolPtr(false)})

	_, _ = runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	claim, _ := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "planner-worker-1", "planner")
	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "failed" || result.FailureKind != FailureManualIntervention || !strings.Contains(result.Summary, "manual publish required") {
		t.Fatalf("result = %#v, want manual_intervention manual publish failure", result)
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
	issues             []IssueSummary
	listOpenIssueCalls []ListOpenIssuesInput
	issueDetail        IssueDetail
	openPullRequests   []PullRequestSummary
	prDetail           PullRequestDetail
	viewPRErr          error
	createPRResult     CreatePullRequestResult
	createPRErrors     []error
	createPRIndex      int
	listOpenPRCalls    []ListOpenPullRequestsInput
	createPRCalls      []CreatePullRequestInput
	updatePRBodyCalls  []UpdatePullRequestBodyInput
	addLabelCalls      []PullRequestLabelsInput
	addReviewerCalls   []PullRequestReviewersInput
	addAssigneeCalls   []IssueAssigneesInput
	addAssigneeErr     error
	login              string
	loginErr           error
	loginCalls         int
}

func (f *fakeGitHubGateway) ListOpenIssues(_ context.Context, input ListOpenIssuesInput) ([]IssueSummary, error) {
	f.listOpenIssueCalls = append(f.listOpenIssueCalls, input)
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

func (f *fakeGitHubGateway) GetCurrentUserLogin(context.Context, string) (string, error) {
	f.loginCalls++
	if f.loginErr != nil {
		return "", f.loginErr
	}
	if f.login != "" {
		return f.login, nil
	}
	return "octocat", nil
}

func (f *fakeGitHubGateway) AddIssueAssignees(_ context.Context, input IssueAssigneesInput) error {
	f.addAssigneeCalls = append(f.addAssigneeCalls, input)
	return f.addAssigneeErr
}

func (f *fakeGitHubGateway) ListOpenPullRequests(_ context.Context, input ListOpenPullRequestsInput) ([]PullRequestSummary, error) {
	f.listOpenPRCalls = append(f.listOpenPRCalls, input)
	return append([]PullRequestSummary(nil), f.openPullRequests...), nil
}

func (f *fakeGitHubGateway) ViewPullRequest(_ context.Context, input ViewPullRequestInput) (PullRequestDetail, error) {
	if f.viewPRErr != nil {
		return PullRequestDetail{}, f.viewPRErr
	}
	detail := f.prDetail
	if detail.Number == 0 {
		detail.Number = input.PRNumber
	}
	return detail, nil
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

func (f *fakeGitHubGateway) UpdatePullRequestBody(_ context.Context, input UpdatePullRequestBodyInput) error {
	f.updatePRBodyCalls = append(f.updatePRBodyCalls, input)
	return nil
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
	createResult  CreateWorktreeResult
	inspectResult InspectHeadResult
	inspectErrors []error
	inspectIndex  int
	commitResult  CommitResult
	createCalls   []CreateWorktreeInput
	inspectCalls  []InspectHeadInput
	commitCalls   []CommitInput
	pushCalls     []PushInput
}

func (f *fakeGitGateway) CreateWorktree(_ context.Context, input CreateWorktreeInput) (CreateWorktreeResult, error) {
	f.createCalls = append(f.createCalls, input)
	result := f.createResult
	if result.WorktreePath == "" {
		result.WorktreePath = filepath.Join(input.WorktreeRoot, "wt")
	} else if input.WorktreeRoot != "" {
		result.WorktreePath = filepath.Join(input.WorktreeRoot, filepath.Base(result.WorktreePath))
	}
	if result.Branch == "" {
		result.Branch = input.Branch
	}
	if result.BaseBranch == "" {
		result.BaseBranch = input.BaseBranch
	}
	f.createResult = result
	return result, nil
}

func (f *fakeGitGateway) Push(_ context.Context, input PushInput) error {
	f.pushCalls = append(f.pushCalls, input)
	return nil
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
