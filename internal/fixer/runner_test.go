package fixer

import (
	"context"
	"errors"
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

func TestBuildFixerPromptUsesConcreteDisclosureMetadata(t *testing.T) {
	t.Parallel()

	detail := &checkpointDetail{State: "OPEN", HeadSHA: "abc123", BaseRefName: "main", HeadRefName: "feature"}
	prompt, _ := buildFixerPrompt("project_1", customInstructionConfig(nil), "acme/looper", 42, detail, []FixItem{{ID: "fix-1", Summary: "repair disclosure"}}, true, config.DefaultDisclosureConfig(), "opencode", "openai/gpt-5.5")
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

func TestBuildFixerPromptOmitsMissingAgentRuntime(t *testing.T) {
	t.Parallel()

	detail := &checkpointDetail{State: "OPEN", HeadSHA: "abc123", BaseRefName: "main", HeadRefName: "feature"}
	prompt, _ := buildFixerPrompt("project_1", customInstructionConfig(nil), "acme/looper", 42, detail, []FixItem{{ID: "fix-1", Summary: "repair disclosure"}}, true, config.DefaultDisclosureConfig(), "", "openai/gpt-5.5")
	if strings.Contains(prompt, "agent=") {
		t.Fatalf("prompt should omit missing agent runtime:\n%s", prompt)
	}
	if !strings.Contains(prompt, "model=openai/gpt-5.5") {
		t.Fatalf("prompt should include configured model:\n%s", prompt)
	}
}

func TestBuildFixerPromptIncludesMinimalPRSeedFetchContract(t *testing.T) {
	t.Parallel()

	detail := &checkpointDetail{State: "OPEN", HeadSHA: "abc123", BaseRefName: "main", HeadRefName: "feature/fix"}
	prompt, _ := buildFixerPrompt("project_1", customInstructionConfig(nil), "acme/looper", 42, detail, []FixItem{{ID: "fix-1", ThreadID: "thread-1", Summary: "repair disclosure"}}, false, config.DefaultDisclosureConfig(), "opencode", "openai/gpt-5.5")
	for _, want := range []string{
		"Minimal PR seed",
		"\"repo\": \"acme/looper\"",
		"\"pr_number\": 42",
		"\"base_ref\": \"main\"",
		"\"head_ref\": \"feature/fix\"",
		"\"head_sha\": \"abc123\"",
		"\"task_intent\": \"repair_pull_request_feedback\"",
		"\"fix_item_ids\"",
		"gh pr view <pr-url> -R <repo> --json number,title,body,state,isDraft,baseRefName,headRefName,headRefOid,url,labels",
		"gh pr diff <pr-url> -R <repo> --name-only",
		"gh pr diff <pr-url> -R <repo> --patch",
		"gh pr checks <pr-url> -R <repo>",
		"git diff <base>...<head> -- <path>",
		"gh api repos/{owner}/{repo}/pulls/{number}/comments --paginate",
		"gh api repos/{owner}/{repo}/pulls/{number}/reviews --paginate",
		"gh api repos/{owner}/{repo}/issues/{number}/comments --paginate",
		"structured error with `type` set to one of `auth`, `network`, `rate_limit`, or `pr_drift`",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "gh pr diff -- <path>") {
		t.Fatalf("prompt contains unsupported gh pr diff pathspec instruction:\n%s", prompt)
	}
}

func TestRunPrepareWorktreeStepRecreatesUnsafeCheckpointAtRepoPath(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	repoPath := t.TempDir()
	git := &fakeGitGateway{createResult: CreateWorktreeResult{WorktreePath: filepath.Join(t.TempDir(), "wt"), Branch: "feature/fix-42", HeadSHA: "base-head"}, prepareResult: PrepareWorktreeResult{HeadSHA: "head-1", Clean: true}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, Git: git, Logger: fixture.logger, Now: fixture.now})

	checkpoint, err := runner.runPrepareWorktreeStep(context.Background(), stepInput{
		Project:  storage.ProjectRecord{ID: "project_1", RepoPath: repoPath},
		Repo:     "acme/looper",
		PRNumber: 42,
		Checkpoint: fixerCheckpoint{
			Detail:   &checkpointDetail{HeadRefName: "feature/fix-42", BaseRefName: "main", HeadSHA: "head-1"},
			Worktree: &checkpointWorktree{Path: repoPath, Branch: "feature/fix-42", PreparedAt: "stale"},
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
	git := &fakeGitGateway{createResult: CreateWorktreeResult{WorktreePath: filepath.Join(worktreeRoot, "wt"), Branch: "feature/fix-42", HeadSHA: "base-head"}, prepareResult: PrepareWorktreeResult{HeadSHA: "head-1", Clean: true}}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "applied fixes", ParseStatus: "parsed"}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, Git: git, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now})

	checkpoint, err := runner.runPrepareWorktreeStep(context.Background(), stepInput{
		Project:  storage.ProjectRecord{ID: "project_1", RepoPath: repoPath, MetadataJSON: &metadata},
		Repo:     "acme/looper",
		PRNumber: 42,
		Checkpoint: fixerCheckpoint{
			Detail:   &checkpointDetail{HeadRefName: "feature/fix-42", BaseRefName: "main", HeadSHA: "head-1"},
			Worktree: &checkpointWorktree{Path: legacyPath, Branch: "feature/fix-42", PreparedAt: "stale"},
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
		t.Fatalf("checkpoint.Worktree.Path = %q, want recreated path outside legacy prepared worktree", checkpoint.Worktree.Path)
	}
	if git.createCalls[0].WorktreeRoot != worktreeRoot {
		t.Fatalf("CreateWorktree().WorktreeRoot = %q, want %q", git.createCalls[0].WorktreeRoot, worktreeRoot)
	}
	if len(git.prepareCalls) != 1 {
		t.Fatalf("len(git.prepareCalls) = %d, want 1", len(git.prepareCalls))
	}
}

func TestBuildFixerMinimalPRSeedUsesEnterpriseHost(t *testing.T) {
	t.Parallel()

	seed := buildFixerMinimalPRSeed("ghe.example.com/acme/looper", 42, &checkpointDetail{}, nil)
	if !strings.Contains(seed, "\"url\": \"https://ghe.example.com/acme/looper/pull/42\"") {
		t.Fatalf("seed = %q, want enterprise host PR URL", seed)
	}
}

func TestDiscoverPullRequestsCreatesLoopAndQueue(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{
		listOpen: []PullRequestSummary{
			{Number: 42, State: "OPEN", HeadSHA: "head-42"},
			{Number: 43, State: "OPEN", IsDraft: true, HeadSHA: "head-43"},
			{Number: 44, State: "OPEN", HeadSHA: "head-44"},
		},
		viewResponses: []PullRequestDetail{
			{Number: 42, State: "OPEN", HeadSHA: "head-42", Comments: []map[string]any{{"id": "c1", "threadId": "t1", "body": "please fix"}}},
			{Number: 44, State: "OPEN", HeadSHA: "head-44"},
		},
	}
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
	if loop == nil || loop.Type != "fixer" || loop.Status != "queued" || loop.Repo == nil || *loop.Repo != "acme/looper" || loop.PRNumber == nil || *loop.PRNumber != 42 {
		t.Fatalf("loop = %#v, want queued fixer loop for #42", loop)
	}
	queue, err := fixture.repos.Queue.GetByID(context.Background(), result.QueueItems[0].ID)
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	if queue == nil || queue.Status != "queued" || !strings.HasPrefix(queue.DedupeKey, "fixer:project_1:"+result.CreatedLoopIDs[0]+":acme/looper:42:") {
		t.Fatalf("queue = %#v, want queued fixer item for #42", queue)
	}
}

func TestDiscoverPullRequestsSkipsPRsNotOwnedByCurrentUser(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{
		currentUser: "looper-bot",
		listOpen: []PullRequestSummary{
			{Number: 42, State: "OPEN", HeadSHA: "head-42", Author: "human"},
			{Number: 43, State: "OPEN", HeadSHA: "head-43", Author: "looper-bot"},
		},
		viewResponses: []PullRequestDetail{
			{Number: 43, State: "OPEN", HeadSHA: "head-43", Comments: []map[string]any{{"id": "c1", "threadId": "t1", "body": "please fix"}}},
		},
	}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})

	result, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	if len(result.QueueItems) != 1 || result.QueueItems[0].PRNumber == nil || *result.QueueItems[0].PRNumber != 43 {
		t.Fatalf("QueueItems = %#v, want only owned PR #43", result.QueueItems)
	}
	if len(github.listCalls) != 1 || github.listCalls[0].Author != "looper-bot" {
		t.Fatalf("list calls = %#v, want author-filtered discovery", github.listCalls)
	}
	if github.viewIndex != 1 {
		t.Fatalf("view calls = %d, want 1", github.viewIndex)
	}
}

func TestDiscoverPullRequestsFixAllPullRequestsOptIn(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{
		currentUser: "looper-bot",
		listOpen:    []PullRequestSummary{{Number: 42, State: "OPEN", HeadSHA: "head-42", Author: "human"}},
		viewResponses: []PullRequestDetail{
			{Number: 42, State: "OPEN", HeadSHA: "head-42", Comments: []map[string]any{{"id": "c1", "threadId": "t1", "body": "please fix"}}},
		},
	}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now, FixAllPullRequests: true})

	result, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	if len(result.QueueItems) != 1 || result.QueueItems[0].PRNumber == nil || *result.QueueItems[0].PRNumber != 42 {
		t.Fatalf("QueueItems = %#v, want opted-in foreign PR #42", result.QueueItems)
	}
}

func TestDiscoverPullRequestsAppliesLabelFilters(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{
		listOpen: []PullRequestSummary{
			{Number: 42, State: "OPEN", HeadSHA: "head-42", Labels: []string{"bug", "urgent"}},
			{Number: 43, State: "OPEN", HeadSHA: "head-43", Labels: []string{"bug"}},
		},
		viewResponses: []PullRequestDetail{{Number: 42, State: "OPEN", HeadSHA: "head-42", Labels: []string{"bug", "urgent"}, Comments: []map[string]any{{"id": "c1", "threadId": "t1", "body": "please fix"}}}},
	}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now, DiscoveryPolicy: DiscoveryPolicy{AutoDiscovery: true, IncludeDrafts: false, AuthorFilter: config.FixerAuthorFilterCurrentUser, Labels: []string{"bug", "urgent"}, LabelMode: config.LabelModeAll}})

	result, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	if len(result.QueueItems) != 1 || *result.QueueItems[0].PRNumber != 42 {
		t.Fatalf("QueueItems = %#v, want only matching PR #42", result.QueueItems)
	}
	if len(github.listCalls) != 1 || strings.Join(github.listCalls[0].Labels, ",") != "bug,urgent" {
		t.Fatalf("list calls = %#v, want server-side multi-label filter", github.listCalls)
	}
}

func TestDiscoverPullRequestsQueriesEachAnyModeLabel(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{
		listOpen:      []PullRequestSummary{{Number: 42, State: "OPEN", HeadSHA: "head-42", Labels: []string{"bug"}}},
		viewResponses: []PullRequestDetail{{Number: 42, State: "OPEN", HeadSHA: "head-42", Labels: []string{"bug"}, Comments: []map[string]any{{"id": "c1", "threadId": "t1", "body": "please fix"}}}},
	}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now, DiscoveryPolicy: DiscoveryPolicy{AutoDiscovery: true, IncludeDrafts: false, AuthorFilter: config.FixerAuthorFilterCurrentUser, Labels: []string{"bug", "urgent"}, LabelMode: config.LabelModeAny}})

	result, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	if len(result.QueueItems) != 1 || *result.QueueItems[0].PRNumber != 42 {
		t.Fatalf("QueueItems = %#v, want matching PR #42", result.QueueItems)
	}
	if len(github.listCalls) != 2 || github.listCalls[0].Label != "bug" || github.listCalls[1].Label != "urgent" {
		t.Fatalf("list calls = %#v, want one server-side query per any-mode label", github.listCalls)
	}
}

func TestListOpenPullRequestsForDiscoveryCapsAnyModeLabelsToLimit(t *testing.T) {
	t.Parallel()
	github := &fakeGitHubGateway{listOpenByLabel: map[string][]PullRequestSummary{
		"bug": {
			{Number: 42, State: "OPEN", HeadSHA: "head-42", Labels: []string{"bug"}},
			{Number: 43, State: "OPEN", HeadSHA: "head-43", Labels: []string{"bug"}},
		},
		"urgent": {
			{Number: 44, State: "OPEN", HeadSHA: "head-44", Labels: []string{"urgent"}},
			{Number: 45, State: "OPEN", HeadSHA: "head-45", Labels: []string{"urgent"}},
		},
	}}
	runner := New(Options{GitHub: github, DiscoveryPolicy: DiscoveryPolicy{Labels: []string{"bug", "urgent"}, LabelMode: config.LabelModeAny, AuthorFilter: config.FixerAuthorFilterAny}})

	prs, err := runner.listOpenPullRequestsForDiscovery(context.Background(), "acme/looper", "/tmp/repo", 2, "looper")
	if err != nil {
		t.Fatalf("listOpenPullRequestsForDiscovery() error = %v", err)
	}
	if len(prs) != 2 || prs[0].Number != 42 || prs[1].Number != 43 {
		t.Fatalf("prs = %#v, want first two unique PRs capped to limit", prs)
	}
	if len(github.listCalls) != 1 || github.listCalls[0].Label != "bug" {
		t.Fatalf("list calls = %#v, want discovery to stop after reaching limit", github.listCalls)
	}
}

func TestListOpenPullRequestsForDiscoveryCapsAnyModeLabelsToDefaultLimit(t *testing.T) {
	t.Parallel()
	firstPage := make([]PullRequestSummary, 30)
	for i := range firstPage {
		firstPage[i] = PullRequestSummary{Number: int64(42 + i), State: "OPEN", HeadSHA: fmt.Sprintf("head-%d", 42+i), Labels: []string{"bug"}}
	}
	github := &fakeGitHubGateway{listOpenByLabel: map[string][]PullRequestSummary{
		"bug":    firstPage,
		"urgent": {{Number: 99, State: "OPEN", HeadSHA: "head-99", Labels: []string{"urgent"}}},
	}}
	runner := New(Options{GitHub: github, DiscoveryPolicy: DiscoveryPolicy{Labels: []string{"bug", "urgent"}, LabelMode: config.LabelModeAny, AuthorFilter: config.FixerAuthorFilterAny}})

	prs, err := runner.listOpenPullRequestsForDiscovery(context.Background(), "acme/looper", "/tmp/repo", 0, "looper")
	if err != nil {
		t.Fatalf("listOpenPullRequestsForDiscovery() error = %v", err)
	}
	if len(prs) != 30 {
		t.Fatalf("len(prs) = %d, want default cap", len(prs))
	}
	if len(github.listCalls) != 1 || github.listCalls[0].Label != "bug" || github.listCalls[0].Limit != 30 {
		t.Fatalf("list calls = %#v, want discovery to use and stop at default limit", github.listCalls)
	}
}

func TestDiscoverPullRequestsPreservesPausedLoop(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	repo := "acme/looper"
	prNumber := int64(42)
	nowISO := fixture.nowISO()
	github := &fakeGitHubGateway{
		listOpen:      []PullRequestSummary{{Number: prNumber, State: "OPEN", HeadSHA: "head-42"}},
		viewResponses: []PullRequestDetail{{Number: prNumber, State: "OPEN", HeadSHA: "head-42", Comments: []map[string]any{{"id": "c1", "threadId": "t1", "body": "please fix"}}}},
	}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})
	loop := storage.LoopRecord{ID: "loop_paused", Seq: 1, ProjectID: "project_1", Type: "fixer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: "paused", CreatedAt: nowISO, UpdatedAt: nowISO}
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

func TestEnqueueScopesFixerDedupeKeyToLoop(t *testing.T) {
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
	headSHA := "head-42"
	fixItemsHash := "fix-hash"

	if err := fixture.repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: project2ID, Name: "Looper Two", RepoPath: repoPath2, BaseBranch: &baseBranch, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert(project_2) error = %v", err)
	}
	for _, loop := range []storage.LoopRecord{
		{ID: loop1ID, Seq: 1, ProjectID: "project_1", Type: "fixer", TargetType: "pull_request", Status: "queued", CreatedAt: nowISO, UpdatedAt: nowISO},
		{ID: loop2ID, Seq: 2, ProjectID: project2ID, Type: "fixer", TargetType: "pull_request", Status: "queued", CreatedAt: nowISO, UpdatedAt: nowISO},
	} {
		if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
			t.Fatalf("Loops.Upsert(%s) error = %v", loop.ID, err)
		}
	}

	first, err := runner.enqueue(context.Background(), enqueueInput{ProjectID: "project_1", LoopID: loop1ID, Repo: repo, PRNumber: prNumber, HeadSHA: headSHA, FixItemsHash: fixItemsHash})
	if err != nil {
		t.Fatalf("enqueue(first) error = %v", err)
	}
	second, err := runner.enqueue(context.Background(), enqueueInput{ProjectID: project2ID, LoopID: loop2ID, Repo: repo, PRNumber: prNumber, HeadSHA: headSHA, FixItemsHash: fixItemsHash})
	if err != nil {
		t.Fatalf("enqueue(second) error = %v", err)
	}
	if first.ID == second.ID {
		t.Fatalf("enqueue(second) reused queue item %q across loops", second.ID)
	}
	if second.LoopID == nil || *second.LoopID != loop2ID {
		t.Fatalf("second loopID = %#v, want %q", second.LoopID, loop2ID)
	}
	if second.DedupeKey != buildFixerDedupeKey(project2ID, loop2ID, repo, prNumber, headSHA, fixItemsHash) {
		t.Fatalf("second dedupe key = %q, want scoped fixer key", second.DedupeKey)
	}
	items, err := fixture.repos.Queue.List(context.Background())
	if err != nil {
		t.Fatalf("Queue.List() error = %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("len(Queue.List()) = %d, want 2", len(items))
	}
}

func TestProcessClaimedItemCompletesSuccessfulFlow(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{
		listOpen: []PullRequestSummary{{Number: 42, State: "OPEN", HeadSHA: "head-1"}},
		viewResponses: []PullRequestDetail{
			{Number: 42, State: "OPEN", HeadSHA: "head-1", HeadRefName: "feature/fix-42", BaseRefName: "main", BaseSHA: "base-1", Comments: []map[string]any{{"id": "c1", "threadId": "t1", "body": "please fix"}}, Checks: []map[string]any{{"name": "ci", "conclusion": "FAILURE"}}},
			{Number: 42, State: "OPEN", HeadSHA: "head-1", HeadRefName: "feature/fix-42", BaseRefName: "main", BaseSHA: "base-1", Comments: []map[string]any{{"id": "c1", "threadId": "t1", "body": "please fix"}}, Checks: []map[string]any{{"name": "ci", "conclusion": "FAILURE"}}},
			{Number: 42, State: "OPEN", HeadSHA: "new-head", HeadRefName: "feature/fix-42", BaseRefName: "main", BaseSHA: "base-1"},
			{Number: 42, State: "OPEN", HeadSHA: "new-head", HeadRefName: "feature/fix-42", BaseRefName: "main", BaseSHA: "base-1"},
			{Number: 42, State: "OPEN", HeadSHA: "new-head", HeadRefName: "feature/fix-42", BaseRefName: "main", BaseSHA: "base-1"},
		},
	}
	git := &fakeGitGateway{
		createResult:  CreateWorktreeResult{WorktreePath: filepath.Join(t.TempDir(), "wt-42"), Branch: "feature/fix-42", HeadSHA: "base-head"},
		prepareResult: PrepareWorktreeResult{HeadSHA: "base-head", Clean: true},
		inspectResults: []InspectHeadResult{
			{HeadSHA: "base-head"},
			{HeadSHA: "new-head", NewCommitSHAs: []string{"new-head"}},
			{HeadSHA: "new-head"},
		},
	}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "applied fixes", ParseStatus: "parsed"}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git, AgentExecutor: agent, ValidationRunner: passValidation, AllowAutoCommit: true, AllowAutoPush: true, AllowRiskyFixes: true, Logger: fixture.logger, Now: fixture.now})

	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "fixer-worker-1", "fixer")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNextOfType() = (%#v, %v), want claimed item", claim, err)
	}

	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "success" {
		t.Fatalf("result = %#v, want success", result)
	}
	if len(agent.starts) != 1 || len(git.pushCalls) != 1 || len(github.resolveCalls) != 1 {
		t.Fatalf("agent starts=%d push calls=%d resolve calls=%d, want 1/1/1", len(agent.starts), len(git.pushCalls), len(github.resolveCalls))
	}
	queue, err := fixture.repos.Queue.GetByID(context.Background(), claim.ID)
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	if queue == nil || queue.Status != "completed" {
		t.Fatalf("queue = %#v, want completed", queue)
	}
	loop, err := fixture.repos.Loops.GetByID(context.Background(), result.LoopID)
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if loop == nil || loop.Status != "completed" {
		t.Fatalf("loop = %#v, want completed", loop)
	}
	run, err := fixture.repos.Runs.GetByID(context.Background(), result.RunID)
	if err != nil {
		t.Fatalf("Runs.GetByID() error = %v", err)
	}
	if run == nil || run.Status != "success" || run.LastCompletedStep == nil || *run.LastCompletedStep != string(stepRecheck) {
		t.Fatalf("run = %#v, want success through recheck", run)
	}
	checkpoint := parseCheckpoint(run.CheckpointJSON)
	if checkpoint.Detail == nil || checkpoint.Detail.HeadSHA != "new-head" {
		t.Fatalf("checkpoint.Detail = %#v, want pushed head new-head", checkpoint.Detail)
	}
	if checkpoint.Push == nil || checkpoint.Push.HeadSHA != "new-head" {
		t.Fatalf("checkpoint.Push = %#v, want recorded pushed head new-head", checkpoint.Push)
	}
	if checkpoint.ReconcileCommits == nil || checkpoint.ReconcileCommits.FinalHeadSHA != "new-head" {
		t.Fatalf("checkpoint.ReconcileCommits = %#v, want refreshed final head new-head", checkpoint.ReconcileCommits)
	}
}

func TestProcessClaimedItemDoesNotResolveCommentsWhenRepairProducesNoCommits(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{
		listOpen: []PullRequestSummary{{Number: 42, State: "OPEN", HeadSHA: "base-head"}},
		viewResponses: []PullRequestDetail{
			{Number: 42, State: "OPEN", HeadSHA: "base-head", HeadRefName: "feature/fix-42", BaseRefName: "main", BaseSHA: "base-1", Comments: []map[string]any{{"id": "c1", "threadId": "t1", "body": "please fix"}}},
			{Number: 42, State: "OPEN", HeadSHA: "base-head", HeadRefName: "feature/fix-42", BaseRefName: "main", BaseSHA: "base-1", Comments: []map[string]any{{"id": "c1", "threadId": "t1", "body": "please fix"}}},
		},
	}
	git := &fakeGitGateway{
		createResult:  CreateWorktreeResult{WorktreePath: filepath.Join(t.TempDir(), "wt-42"), Branch: "feature/fix-42", HeadSHA: "base-head"},
		prepareResult: PrepareWorktreeResult{HeadSHA: "base-head", Clean: true},
		inspectResults: []InspectHeadResult{
			{HeadSHA: "base-head"},
			{HeadSHA: "base-head"},
			{HeadSHA: "base-head"},
		},
	}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "blocked before editing because gh could not validate PR metadata", ParseStatus: "parsed"}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git, AgentExecutor: agent, ValidationRunner: passValidation, AllowAutoCommit: true, AllowAutoPush: true, AllowRiskyFixes: true, Logger: fixture.logger, Now: fixture.now})

	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "fixer-worker-1", "fixer")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNextOfType() = (%#v, %v), want claimed item", claim, err)
	}

	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "failed" || result.FailureKind != FailureRetryableAfterResume || !contains(result.Summary, "produced no new commits") {
		t.Fatalf("result = %#v, want retryable-after-resume failure for no-op repair", result)
	}
	if len(git.commitCalls) != 0 || len(git.pushCalls) != 0 || len(github.resolveCalls) != 0 {
		t.Fatalf("commit calls=%d push calls=%d resolve calls=%d, want 0/0/0 after no-op repair", len(git.commitCalls), len(git.pushCalls), len(github.resolveCalls))
	}
	run, err := fixture.repos.Runs.GetByID(context.Background(), result.RunID)
	if err != nil {
		t.Fatalf("Runs.GetByID() error = %v", err)
	}
	if run == nil || run.Status != "failed" || run.CurrentStep == nil || *run.CurrentStep != string(stepResolveComments) {
		t.Fatalf("run = %#v, want failed run at resolve-comments step", run)
	}
	checkpoint := parseCheckpoint(run.CheckpointJSON)
	if checkpoint.Push == nil || checkpoint.Push.Pushed || checkpoint.Push.SkippedReason == "" {
		t.Fatalf("checkpoint.Push = %#v, want recorded no-op push", checkpoint.Push)
	}
	if checkpoint.ResumePolicy != "restart_from_discover" {
		t.Fatalf("checkpoint.ResumePolicy = %q, want restart_from_discover", checkpoint.ResumePolicy)
	}
	if checkpoint.ResolvedComments != nil {
		t.Fatalf("checkpoint.ResolvedComments = %#v, want unresolved comments left untouched", checkpoint.ResolvedComments)
	}
	queue, err := fixture.repos.Queue.GetByID(context.Background(), claim.ID)
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	if queue == nil || queue.Status != "queued" || queue.LastErrorKind == nil || *queue.LastErrorKind != string(FailureRetryableAfterResume) {
		t.Fatalf("queue = %#v, want queued retryable-after-resume failure", queue)
	}
	loop, err := fixture.repos.Loops.GetByID(context.Background(), result.LoopID)
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if loop == nil || loop.Status != "queued" || loop.NextRunAt == nil {
		t.Fatalf("loop = %#v, want queued loop scheduled for rediscovery retry", loop)
	}
	loopMeta := parseJSONObject(loop.MetadataJSON)
	if got, _ := stringFromAny(loopMeta["lastNoopResolveHeadSha"]); got != "base-head" {
		t.Fatalf("lastNoopResolveHeadSha = %q, want base-head", got)
	}
	if got, _ := stringFromAny(loopMeta["lastNoopResolveFixItemsHash"]); got == "" {
		t.Fatal("lastNoopResolveFixItemsHash = empty, want captured fix-item snapshot")
	}
}

func TestDiscoverPullRequestsSkipsFailedFixerLoopWhenFingerprintUnchanged(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	repo := "acme/looper"
	prNumber := int64(77)
	nowISO := fixture.nowISO()
	detail := PullRequestDetail{Number: prNumber, State: "OPEN", HeadSHA: "same-head", HeadRefName: "feature/fix-77", BaseRefName: "main", BaseSHA: "base-1", Comments: []map[string]any{{"id": "c1", "threadId": "t1", "body": "please fix"}}}
	fingerprint := buildFixerDiscoveryFingerprint(repo, prNumber, detail.HeadSHA, hashFixItems(collectFixItems(detail)))
	metadata := fmt.Sprintf(`{"autonomousRecovery":{"lastFailedDiscoveryFingerprint":%q}}`, fingerprint)
	targetID := buildPullRequestTargetID(repo, prNumber)
	if err := fixture.repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: "loop_fixer_failed_same_fp", Seq: 90, ProjectID: "project_1", Type: "fixer", TargetType: "pull_request", TargetID: &targetID, Repo: &repo, PRNumber: &prNumber, Status: "failed", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	github := &fakeGitHubGateway{listOpen: []PullRequestSummary{{Number: prNumber, State: "OPEN", HeadSHA: "same-head"}}, viewResponses: []PullRequestDetail{detail}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})

	result, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: repo})
	if err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	if len(result.QueueItems) != 0 {
		t.Fatalf("QueueItems = %#v, want none for unchanged failed fingerprint", result.QueueItems)
	}
}

func TestDiscoverPullRequestsRequeuesFailedFixerLoopWhenFingerprintChanges(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	repo := "acme/looper"
	prNumber := int64(78)
	nowISO := fixture.nowISO()
	oldDetail := PullRequestDetail{Number: prNumber, State: "OPEN", HeadSHA: "old-head", HeadRefName: "feature/fix-78", BaseRefName: "main", BaseSHA: "base-1", Comments: []map[string]any{{"id": "c1", "threadId": "t1", "body": "please fix"}}}
	newDetail := PullRequestDetail{Number: prNumber, State: "OPEN", HeadSHA: "new-head", HeadRefName: "feature/fix-78", BaseRefName: "main", BaseSHA: "base-1", Comments: []map[string]any{{"id": "c1", "threadId": "t1", "body": "please fix"}}}
	fingerprint := buildFixerDiscoveryFingerprint(repo, prNumber, oldDetail.HeadSHA, hashFixItems(collectFixItems(oldDetail)))
	metadata := fmt.Sprintf(`{"autonomousRecovery":{"lastFailedDiscoveryFingerprint":%q}}`, fingerprint)
	targetID := buildPullRequestTargetID(repo, prNumber)
	if err := fixture.repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: "loop_fixer_failed_changed_fp", Seq: 91, ProjectID: "project_1", Type: "fixer", TargetType: "pull_request", TargetID: &targetID, Repo: &repo, PRNumber: &prNumber, Status: "failed", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	github := &fakeGitHubGateway{listOpen: []PullRequestSummary{{Number: prNumber, State: "OPEN", HeadSHA: "new-head"}}, viewResponses: []PullRequestDetail{newDetail}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})

	result, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: repo})
	if err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	if len(result.QueueItems) != 1 {
		t.Fatalf("QueueItems = %#v, want one queue item after fingerprint change", result.QueueItems)
	}
}

func TestProcessClaimedItemAllowsNoCommitWhenCommentsAlreadyResolved(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	unresolved := map[string]any{"id": "c1", "threadId": "t1", "body": "please fix"}
	resolved := map[string]any{"id": "c1", "threadId": "t1", "body": "please fix", "isResolved": true, "state": "RESOLVED"}
	github := &fakeGitHubGateway{
		listOpen: []PullRequestSummary{{Number: 42, State: "OPEN", HeadSHA: "base-head"}},
		viewResponses: []PullRequestDetail{
			{Number: 42, State: "OPEN", HeadSHA: "base-head", HeadRefName: "feature/fix-42", BaseRefName: "main", BaseSHA: "base-1", Comments: []map[string]any{unresolved}},
			{Number: 42, State: "OPEN", HeadSHA: "base-head", HeadRefName: "feature/fix-42", BaseRefName: "main", BaseSHA: "base-1", Comments: []map[string]any{unresolved}},
			{Number: 42, State: "OPEN", HeadSHA: "base-head", HeadRefName: "feature/fix-42", BaseRefName: "main", BaseSHA: "base-1", Comments: []map[string]any{resolved}},
			{Number: 42, State: "OPEN", HeadSHA: "base-head", HeadRefName: "feature/fix-42", BaseRefName: "main", BaseSHA: "base-1", Comments: []map[string]any{resolved}},
			{Number: 42, State: "OPEN", HeadSHA: "base-head", HeadRefName: "feature/fix-42", BaseRefName: "main", BaseSHA: "base-1", Comments: []map[string]any{resolved}},
		},
	}
	git := &fakeGitGateway{
		createResult:  CreateWorktreeResult{WorktreePath: filepath.Join(t.TempDir(), "wt-42"), Branch: "feature/fix-42", HeadSHA: "base-head"},
		prepareResult: PrepareWorktreeResult{HeadSHA: "base-head", Clean: true},
		inspectResults: []InspectHeadResult{
			{HeadSHA: "base-head"},
			{HeadSHA: "base-head"},
			{HeadSHA: "base-head"},
		},
	}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "no edit needed because comment was already resolved", ParseStatus: "parsed"}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git, AgentExecutor: agent, ValidationRunner: passValidation, AllowAutoCommit: true, AllowAutoPush: true, AllowRiskyFixes: true, Logger: fixture.logger, Now: fixture.now})

	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "fixer-worker-1", "fixer")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNextOfType() = (%#v, %v), want claimed item", claim, err)
	}

	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "success" {
		t.Fatalf("result = %#v, want success after live comments are already resolved", result)
	}
	if len(git.commitCalls) != 0 || len(git.pushCalls) != 0 || len(github.resolveCalls) != 0 {
		t.Fatalf("commit calls=%d push calls=%d resolve calls=%d, want 0/0/0 after already-resolved live comments", len(git.commitCalls), len(git.pushCalls), len(github.resolveCalls))
	}
}

func TestDiscoverPullRequestsSkipsRepeatedNoopResolveRediscovery(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	repo := "acme/looper"
	prNumber := int64(42)
	nowISO := fixture.nowISO()
	comment := map[string]any{"id": "c1", "threadId": "t1", "body": "please fix"}
	detail := PullRequestDetail{Number: prNumber, State: "OPEN", HeadSHA: "head-1", HeadRefName: "feature/fix-42", BaseRefName: "main", BaseSHA: "base-1", Comments: []map[string]any{comment}}
	fixItemsHash := hashFixItems(collectFixItems(detail))
	metadata := mustMarshalJSON(map[string]any{"lastNoopResolveHeadSha": detail.HeadSHA, "lastNoopResolveFixItemsHash": fixItemsHash})
	loopTarget := buildPullRequestTargetID(repo, prNumber)
	if err := fixture.repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: "loop_fixer_noop", Seq: 1, ProjectID: "project_1", Type: "fixer", TargetType: "pull_request", TargetID: &loopTarget, Repo: &repo, PRNumber: &prNumber, Status: "failed", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	github := &fakeGitHubGateway{listOpen: []PullRequestSummary{{Number: prNumber, State: "OPEN", HeadSHA: detail.HeadSHA}}, viewResponses: []PullRequestDetail{detail}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})

	result, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: repo})
	if err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	if len(result.QueueItems) != 0 {
		t.Fatalf("QueueItems = %#v, want no repeated queue item for unchanged no-op resolve state", result.QueueItems)
	}
	if result.Skipped == 0 {
		t.Fatalf("result = %#v, want skipped discovery count", result)
	}
}

func TestDiscoverPullRequestsRequeuesNoopResolveLoopWhenStateChanges(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		detail PullRequestDetail
	}{
		{name: "head changes", detail: PullRequestDetail{Number: 42, State: "OPEN", HeadSHA: "head-2", HeadRefName: "feature/fix-42", BaseRefName: "main", BaseSHA: "base-1", Comments: []map[string]any{{"id": "c1", "threadId": "t1", "body": "please fix"}}}},
		{name: "fix items change", detail: PullRequestDetail{Number: 42, State: "OPEN", HeadSHA: "head-1", HeadRefName: "feature/fix-42", BaseRefName: "main", BaseSHA: "base-1", Comments: []map[string]any{{"id": "c1", "threadId": "t1", "body": "please fix"}, {"id": "c2", "threadId": "t2", "body": "also fix this"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fixture := newRunnerFixture(t)
			repo := "acme/looper"
			prNumber := int64(42)
			nowISO := fixture.nowISO()
			baseline := PullRequestDetail{Number: prNumber, State: "OPEN", HeadSHA: "head-1", HeadRefName: "feature/fix-42", BaseRefName: "main", BaseSHA: "base-1", Comments: []map[string]any{{"id": "c1", "threadId": "t1", "body": "please fix"}}}
			metadata := mustMarshalJSON(map[string]any{"lastNoopResolveHeadSha": baseline.HeadSHA, "lastNoopResolveFixItemsHash": hashFixItems(collectFixItems(baseline))})
			loopTarget := buildPullRequestTargetID(repo, prNumber)
			if err := fixture.repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: "loop_fixer_noop", Seq: 1, ProjectID: "project_1", Type: "fixer", TargetType: "pull_request", TargetID: &loopTarget, Repo: &repo, PRNumber: &prNumber, Status: "failed", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
				t.Fatalf("Loops.Upsert() error = %v", err)
			}

			github := &fakeGitHubGateway{listOpen: []PullRequestSummary{{Number: prNumber, State: "OPEN", HeadSHA: tc.detail.HeadSHA}}, viewResponses: []PullRequestDetail{tc.detail}}
			runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})

			result, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: repo})
			if err != nil {
				t.Fatalf("DiscoverPullRequests() error = %v", err)
			}
			if len(result.QueueItems) != 1 {
				t.Fatalf("QueueItems = %#v, want one queue item after state change", result.QueueItems)
			}
		})
	}
}

func TestRunResolveCommentsStepBlocksWithoutVerifiedPushEvidence(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	loopMetadata := `{}`
	repo := "acme/looper"
	prNumber := int64(42)
	loopTarget := buildPullRequestTargetID("acme/looper", 42)
	loop := storage.LoopRecord{ID: "loop_1", ProjectID: "project_1", Type: "fixer", TargetType: "pull_request", TargetID: &loopTarget, Repo: &repo, PRNumber: &prNumber, Status: "running", MetadataJSON: &loopMetadata, CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}
	if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	github := &fakeGitHubGateway{viewResponses: []PullRequestDetail{{
		Number:      42,
		State:       "OPEN",
		HeadSHA:     "base-head",
		HeadRefName: "feature/fix-42",
		BaseRefName: "main",
		BaseSHA:     "base-1",
		Comments: []map[string]any{{
			"id":       "c1",
			"threadId": "t1",
			"body":     "please fix",
		}},
	}}}
	runner := New(Options{Repos: fixture.repos, GitHub: github, Now: fixture.now})
	checkpoint := fixerCheckpoint{
		FixItems:   []FixItem{{Type: "comment", ID: "c1", ThreadID: "t1", Summary: "please fix"}},
		Validation: &ValidationResult{Passed: true, Summary: "ok"},
		Push:       &checkpointPush{Pushed: false, Branch: "feature/fix-42", Remote: "origin", SkippedReason: "No new commits to push"},
		Lifecycle:  &lifecycle.State{Pushed: true},
		ReconcileCommits: &checkpointReconcileCommits{
			BaseHeadSHA:      "base-head",
			FinalHeadSHA:     "base-head",
			NewCommitSHAs:    nil,
			WorkingTreeClean: true,
		},
	}

	updated, err := runner.runResolveCommentsStep(context.Background(), stepInput{
		Project:    storage.ProjectRecord{ID: "project_1", RepoPath: t.TempDir()},
		Loop:       loop,
		Repo:       "acme/looper",
		PRNumber:   42,
		Checkpoint: checkpoint,
	})
	if err == nil {
		t.Fatal("runResolveCommentsStep() error = nil, want rediscoverable retry")
	}
	var loopErr *loopError
	if !errors.As(err, &loopErr) {
		t.Fatalf("error = %T, want *loopError", err)
	}
	if loopErr.kind != FailureRetryableAfterResume {
		t.Fatalf("loopErr.kind = %v, want %v", loopErr.kind, FailureRetryableAfterResume)
	}
	if !contains(loopErr.Error(), "produced no new commits") {
		t.Fatalf("error = %q, want no-new-commits message", loopErr.Error())
	}
	if updated.ResumePolicy != "restart_from_discover" {
		t.Fatalf("updated.ResumePolicy = %q, want restart_from_discover", updated.ResumePolicy)
	}
}

func TestRunResolveCommentsStepRefreshesVerifiedNoPushHeadBeforeResolve(t *testing.T) {
	t.Parallel()

	github := &fakeGitHubGateway{viewResponses: []PullRequestDetail{
		{
			Number:      42,
			State:       "OPEN",
			HeadSHA:     "old-head",
			HeadRefName: "feature/fix-42",
			BaseRefName: "main",
			BaseSHA:     "base-1",
			Comments: []map[string]any{{
				"id":       "c1",
				"threadId": "t1",
				"body":     "please fix",
			}},
		},
		{
			Number:      42,
			State:       "OPEN",
			HeadSHA:     "new-head",
			HeadRefName: "feature/fix-42",
			BaseRefName: "main",
			BaseSHA:     "base-1",
			Comments: []map[string]any{{
				"id":       "c1",
				"threadId": "t1",
				"body":     "please fix",
			}},
		},
	}}
	runner := New(Options{GitHub: github, Sleep: func(time.Duration) {}})
	fixItems := []FixItem{{Type: "comment", ID: "c1", ThreadID: "t1", Summary: "please fix"}}
	fixItemsHash := hashFixItems(fixItems)
	loopMetadata := fmt.Sprintf("{\"lastFixHeadSha\":\"new-head\",\"lastFixItemsHash\":\"%s\"}", fixItemsHash)
	checkpoint := fixerCheckpoint{
		FixItems:     fixItems,
		FixItemsHash: fixItemsHash,
		Validation:   &ValidationResult{Passed: true, Summary: "ok"},
		Push:         &checkpointPush{Pushed: false, Branch: "feature/fix-42", Remote: "origin", SkippedReason: "No new commits to push"},
		ReconcileCommits: &checkpointReconcileCommits{
			BaseHeadSHA:      "base-head",
			FinalHeadSHA:     "base-head",
			NewCommitSHAs:    nil,
			WorkingTreeClean: true,
		},
	}

	updated, err := runner.runResolveCommentsStep(context.Background(), stepInput{
		Project:    storage.ProjectRecord{RepoPath: t.TempDir()},
		Loop:       storage.LoopRecord{MetadataJSON: &loopMetadata},
		Repo:       "acme/looper",
		PRNumber:   42,
		Checkpoint: checkpoint,
	})
	if err != nil {
		t.Fatalf("runResolveCommentsStep() error = %v", err)
	}
	if github.viewIndex < 2 {
		t.Fatalf("github.viewIndex = %d, want live refresh plus head check", github.viewIndex)
	}
	if len(github.resolveCalls) != 1 {
		t.Fatalf("resolve calls = %d, want 1 after refreshing verified head", len(github.resolveCalls))
	}
	if updated.Detail == nil || updated.Detail.HeadSHA != "new-head" {
		t.Fatalf("updated.Detail = %#v, want refreshed new-head detail", updated.Detail)
	}
	if updated.ReconcileCommits == nil || updated.ReconcileCommits.FinalHeadSHA != "base-head" {
		t.Fatalf("updated.ReconcileCommits = %#v, want stale reconcile head preserved", updated.ReconcileCommits)
	}
	if len(github.replyCalls) != 1 || !contains(github.replyCalls[0].Body, "new-hea") {
		t.Fatalf("reply calls = %#v, want reply referencing verified head", github.replyCalls)
	}
	if updated.ResumePolicy != "advance_from_checkpoint" {
		t.Fatalf("updated.ResumePolicy = %q, want advance_from_checkpoint", updated.ResumePolicy)
	}
}

func TestRunResolveCommentsStepFailsWhenHeadChangesAfterVerifiedNoPushRefresh(t *testing.T) {
	t.Parallel()

	github := &fakeGitHubGateway{viewResponses: []PullRequestDetail{
		{
			Number:      42,
			State:       "OPEN",
			HeadSHA:     "new-head",
			HeadRefName: "feature/fix-42",
			BaseRefName: "main",
			BaseSHA:     "base-1",
			Comments: []map[string]any{{
				"id":       "c1",
				"threadId": "t1",
				"body":     "please fix",
			}},
		},
		{
			Number:      42,
			State:       "OPEN",
			HeadSHA:     "newer-head",
			HeadRefName: "feature/fix-42",
			BaseRefName: "main",
			BaseSHA:     "base-1",
			Comments: []map[string]any{{
				"id":       "c1",
				"threadId": "t1",
				"body":     "please fix",
			}},
		},
	}}
	runner := New(Options{GitHub: github, Sleep: func(time.Duration) {}})
	fixItems := []FixItem{{Type: "comment", ID: "c1", ThreadID: "t1", Summary: "please fix"}}
	fixItemsHash := hashFixItems(fixItems)
	loopMetadata := fmt.Sprintf("{\"lastFixHeadSha\":\"new-head\",\"lastFixItemsHash\":\"%s\"}", fixItemsHash)
	checkpoint := fixerCheckpoint{
		FixItems:         fixItems,
		FixItemsHash:     fixItemsHash,
		Validation:       &ValidationResult{Passed: true, Summary: "ok"},
		Push:             &checkpointPush{Pushed: false, Branch: "feature/fix-42", Remote: "origin", SkippedReason: "No new commits to push"},
		ReconcileCommits: &checkpointReconcileCommits{BaseHeadSHA: "base-head", FinalHeadSHA: "base-head", WorkingTreeClean: true},
	}

	_, err := runner.runResolveCommentsStep(context.Background(), stepInput{
		Project:    storage.ProjectRecord{RepoPath: t.TempDir()},
		Loop:       storage.LoopRecord{MetadataJSON: &loopMetadata},
		Repo:       "acme/looper",
		PRNumber:   42,
		Checkpoint: checkpoint,
	})
	if err == nil {
		t.Fatal("runResolveCommentsStep() error = nil, want stale-head retry")
	}
	var loopErr *loopError
	if !errors.As(err, &loopErr) {
		t.Fatalf("error = %T, want *loopError", err)
	}
	if loopErr.kind != FailureRetryableAfterResume {
		t.Fatalf("loopErr.kind = %v, want %v", loopErr.kind, FailureRetryableAfterResume)
	}
	if !contains(loopErr.Error(), "PR head changed before resolving comments") {
		t.Fatalf("error = %q, want stale-head message", loopErr.Error())
	}
	if len(github.resolveCalls) != 0 {
		t.Fatalf("resolve calls = %d, want none on concurrent head change", len(github.resolveCalls))
	}
}

func TestRunResolveCommentsStepFailsWhenLiveFixItemsDriftFromVerifiedNoPushSnapshot(t *testing.T) {
	t.Parallel()

	github := &fakeGitHubGateway{viewResponses: []PullRequestDetail{
		{
			Number:      42,
			State:       "OPEN",
			HeadSHA:     "new-head",
			HeadRefName: "feature/fix-42",
			BaseRefName: "main",
			BaseSHA:     "base-1",
			Comments: []map[string]any{{
				"id":       "c1",
				"threadId": "t1",
				"body":     "please fix",
			}, {
				"id":       "c2",
				"threadId": "t2",
				"body":     "new feedback",
			}},
		},
		{
			Number:      42,
			State:       "OPEN",
			HeadSHA:     "new-head",
			HeadRefName: "feature/fix-42",
			BaseRefName: "main",
			BaseSHA:     "base-1",
			Comments: []map[string]any{{
				"id":       "c1",
				"threadId": "t1",
				"body":     "please fix",
			}, {
				"id":       "c2",
				"threadId": "t2",
				"body":     "new feedback",
			}},
		},
	}}
	runner := New(Options{GitHub: github, Sleep: func(time.Duration) {}})
	fixItems := []FixItem{{Type: "comment", ID: "c1", ThreadID: "t1", Summary: "please fix"}}
	fixItemsHash := hashFixItems(fixItems)
	loopMetadata := fmt.Sprintf("{\"lastFixHeadSha\":\"new-head\",\"lastFixItemsHash\":\"%s\"}", fixItemsHash)
	checkpoint := fixerCheckpoint{
		FixItems:         fixItems,
		FixItemsHash:     fixItemsHash,
		Validation:       &ValidationResult{Passed: true, Summary: "ok"},
		Push:             &checkpointPush{Pushed: false, Branch: "feature/fix-42", Remote: "origin", SkippedReason: "No new commits to push"},
		ReconcileCommits: &checkpointReconcileCommits{BaseHeadSHA: "base-head", FinalHeadSHA: "base-head", WorkingTreeClean: true},
	}

	_, err := runner.runResolveCommentsStep(context.Background(), stepInput{
		Project:    storage.ProjectRecord{RepoPath: t.TempDir()},
		Loop:       storage.LoopRecord{MetadataJSON: &loopMetadata},
		Repo:       "acme/looper",
		PRNumber:   42,
		Checkpoint: checkpoint,
	})
	if err == nil {
		t.Fatal("runResolveCommentsStep() error = nil, want stale-head retry")
	}
	var loopErr *loopError
	if !errors.As(err, &loopErr) {
		t.Fatalf("error = %T, want *loopError", err)
	}
	if loopErr.kind != FailureRetryableAfterResume {
		t.Fatalf("loopErr.kind = %v, want %v", loopErr.kind, FailureRetryableAfterResume)
	}
	if !contains(loopErr.Error(), "PR head changed before resolving comments") {
		t.Fatalf("error = %q, want stale-head message when verified snapshot no longer matches live comments", loopErr.Error())
	}
	if len(github.resolveCalls) != 0 {
		t.Fatalf("resolve calls = %d, want none when live fix items drift", len(github.resolveCalls))
	}
}

func TestRunResolveCommentsStepUsesRefreshedPushHeadSHA(t *testing.T) {
	t.Parallel()

	github := &fakeGitHubGateway{viewResponses: []PullRequestDetail{{
		Number:      42,
		State:       "OPEN",
		HeadSHA:     "new-head",
		HeadRefName: "feature/fix-42",
		BaseRefName: "main",
		BaseSHA:     "base-1",
	}}}
	runner := New(Options{GitHub: github})
	checkpoint := fixerCheckpoint{
		Detail:     &checkpointDetail{HeadSHA: "old-head", HeadRefName: "feature/fix-42", BaseRefName: "main"},
		FixItems:   []FixItem{{Type: "comment", ID: "c1", ThreadID: "t1", Summary: "please fix"}},
		Validation: &ValidationResult{Passed: true, Summary: "ok"},
		Push:       &checkpointPush{Pushed: true, Branch: "feature/fix-42", Remote: "origin", HeadSHA: "new-head"},
		Lifecycle:  &lifecycle.State{Pushed: true},
		ReconcileCommits: &checkpointReconcileCommits{
			BaseHeadSHA:      "base-head",
			FinalHeadSHA:     "old-head",
			NewCommitSHAs:    []string{"new-head"},
			WorkingTreeClean: true,
		},
	}

	updated, err := runner.runResolveCommentsStep(context.Background(), stepInput{
		Project:    storage.ProjectRecord{RepoPath: t.TempDir()},
		Repo:       "acme/looper",
		PRNumber:   42,
		Checkpoint: checkpoint,
	})
	if err != nil {
		t.Fatalf("runResolveCommentsStep() error = %v", err)
	}
	if len(github.resolveCalls) != 1 {
		t.Fatalf("resolve calls = %d, want 1", len(github.resolveCalls))
	}
	if len(github.replyCalls) != 1 || !contains(github.replyCalls[0].Body, "new-hea") {
		t.Fatalf("reply calls = %#v, want reply referencing pushed head", github.replyCalls)
	}
	if updated.ResumePolicy != "advance_from_checkpoint" {
		t.Fatalf("updated.ResumePolicy = %q, want advance_from_checkpoint", updated.ResumePolicy)
	}
}

func TestRunResolveCommentsStepPreservesCheckpointLabelSnapshotOnLiveRefresh(t *testing.T) {
	t.Parallel()

	github := &fakeGitHubGateway{viewResponses: []PullRequestDetail{{
		Number:      42,
		State:       "OPEN",
		HeadSHA:     "base-head",
		HeadRefName: "feature/fix-42",
		BaseRefName: "main",
		BaseSHA:     "base-1",
		Labels:      nil,
		Comments: []map[string]any{{
			"id":         "c1",
			"threadId":   "t1",
			"body":       "please fix",
			"isResolved": true,
			"state":      "RESOLVED",
		}},
	}}}
	runner := New(Options{GitHub: github, Now: time.Now})
	originalLabels := []string{specpr.ReviewingLabel}
	checkpoint := fixerCheckpoint{
		Detail: &checkpointDetail{
			Labels:      append([]string(nil), originalLabels...),
			HeadSHA:     "base-head",
			HeadRefName: "feature/fix-42",
			BaseRefName: "main",
			Comments: []map[string]any{{
				"id":       "c1",
				"threadId": "t1",
				"body":     "please fix",
			}},
		},
		FixItems:         []FixItem{{Type: "comment", ID: "c1", ThreadID: "t1", Summary: "please fix"}},
		Validation:       &ValidationResult{Passed: true, Summary: "ok"},
		Push:             &checkpointPush{Pushed: false, Branch: "feature/fix-42", Remote: "origin", SkippedReason: "No new commits to push"},
		ReconcileCommits: &checkpointReconcileCommits{BaseHeadSHA: "base-head", FinalHeadSHA: "base-head", WorkingTreeClean: true},
	}

	updated, err := runner.runResolveCommentsStep(context.Background(), stepInput{
		Project:    storage.ProjectRecord{RepoPath: t.TempDir()},
		Repo:       "acme/looper",
		PRNumber:   42,
		Checkpoint: checkpoint,
	})
	if err != nil {
		t.Fatalf("runResolveCommentsStep() error = %v", err)
	}
	if updated.Detail == nil {
		t.Fatal("updated.Detail = nil, want merged detail")
	}
	if !specpr.HasLabel(updated.Detail.Labels, specpr.ReviewingLabel) {
		t.Fatalf("updated.Detail.Labels = %#v, want preserved %q label", updated.Detail.Labels, specpr.ReviewingLabel)
	}
	if len(updated.Detail.Labels) != 1 {
		t.Fatalf("updated.Detail.Labels = %#v, want preserved snapshot only", updated.Detail.Labels)
	}
}

func TestRunPrepareWorktreeStepPreservesExistingLifecycle(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	git := &fakeGitGateway{createResult: CreateWorktreeResult{WorktreePath: filepath.Join(t.TempDir(), "wt-42"), Branch: "feature/fix-42", HeadSHA: "base-head"}, prepareResult: PrepareWorktreeResult{HeadSHA: "base-head", Clean: true}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, Git: git, Logger: fixture.logger, Now: fixture.now})
	baseBranch := "main"
	checkpoint := fixerCheckpoint{
		Detail:    &checkpointDetail{HeadRefName: "feature/fix-42", BaseRefName: "main", HeadSHA: "head-1"},
		Lifecycle: &lifecycle.State{Branch: "feature/fix-42", BaseBranch: "main", CommitSHAs: []string{"commit-1"}, Pushed: true, PRNumber: 42, PRURL: "https://example/pr/42", Actions: lifecycle.Actions{Commit: lifecycle.ActionSourceAgent, Push: lifecycle.ActionSourceAgent, PR: lifecycle.ActionSourceFallback}},
	}

	prepared, err := runner.runPrepareWorktreeStep(context.Background(), stepInput{
		Project:    storage.ProjectRecord{ID: "project_1", RepoPath: t.TempDir(), BaseBranch: &baseBranch},
		Repo:       "acme/looper",
		PRNumber:   42,
		Checkpoint: checkpoint,
	})
	if err != nil {
		t.Fatalf("runPrepareWorktreeStep() error = %v", err)
	}
	if prepared.Lifecycle == nil || len(prepared.Lifecycle.CommitSHAs) != 1 || prepared.Lifecycle.CommitSHAs[0] != "commit-1" || !prepared.Lifecycle.Pushed || prepared.Lifecycle.PRNumber != 42 || prepared.Lifecycle.PRURL == "" || prepared.Lifecycle.Actions.PR != lifecycle.ActionSourceFallback {
		t.Fatalf("Lifecycle = %#v, want existing lifecycle metadata preserved", prepared.Lifecycle)
	}
}

func TestProcessClaimedItemSkipsPRsNotOwnedByCurrentUser(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{
		currentUser:   "looper-bot",
		viewResponses: []PullRequestDetail{{Number: 42, State: "OPEN", HeadSHA: "head-1", HeadRefName: "feature/fix-42", BaseRefName: "main", BaseSHA: "base-1", Author: "human", Comments: []map[string]any{{"id": "c1", "threadId": "t1", "body": "please fix"}}}},
	}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})
	repo := "acme/looper"
	prNumber := int64(42)
	nowISO := fixture.nowISO()
	loop := storage.LoopRecord{ID: "loop_foreign", Seq: 1, ProjectID: "project_1", Type: "fixer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: "queued", CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	queue := storage.QueueItemRecord{ID: "queue_foreign", ProjectID: stringPtr("project_1"), LoopID: &loop.ID, Type: "fixer", TargetType: "pull_request", TargetID: "pr:acme/looper:42", Repo: &repo, PRNumber: &prNumber, DedupeKey: "fixer:foreign", Priority: storage.QueuePriorityFixer, Status: "running", AvailableAt: nowISO, Attempts: 1, MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Queue.Upsert(context.Background(), queue); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	result, err := runner.ProcessClaimedItem(context.Background(), queue)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "skipped" || !contains(result.Summary, "does not match fixer owner") {
		t.Fatalf("result = %#v, want skipped foreign PR", result)
	}
}

func TestProcessClaimedItemFailsWhenRepairCompletionResultMissing(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{
		listOpen: []PullRequestSummary{{Number: 42, State: "OPEN", HeadSHA: "head-1"}},
		viewResponses: []PullRequestDetail{
			{Number: 42, State: "OPEN", HeadSHA: "head-1", HeadRefName: "feature/fix-42", BaseRefName: "main", BaseSHA: "base-1", Comments: []map[string]any{{"id": "c1", "threadId": "t1", "body": "please fix"}}},
			{Number: 42, State: "OPEN", HeadSHA: "head-1", HeadRefName: "feature/fix-42", BaseRefName: "main", BaseSHA: "base-1", Comments: []map[string]any{{"id": "c1", "threadId": "t1", "body": "please fix"}}},
		},
	}
	validationCalls := 0
	git := &fakeGitGateway{
		createResult:  CreateWorktreeResult{WorktreePath: filepath.Join(t.TempDir(), "wt-42"), Branch: "feature/fix-42", HeadSHA: "base-head"},
		prepareResult: PrepareWorktreeResult{HeadSHA: "base-head", Clean: true},
	}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "upstream server_error", ParseStatus: "missing"}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git, AgentExecutor: agent, ValidationRunner: func(context.Context, ValidationInput) (ValidationResult, error) {
		validationCalls++
		return ValidationResult{Passed: true, Summary: "ok"}, nil
	}, AllowAutoCommit: true, AllowAutoPush: true, AllowRiskyFixes: true, Logger: fixture.logger, Now: fixture.now})

	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "fixer-worker-1", "fixer")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNextOfType() = (%#v, %v), want claimed item", claim, err)
	}

	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "failed" || result.FailureKind != FailureRetryableTransient || !contains(result.Summary, "server_error") {
		t.Fatalf("result = %#v, want retryable failed result with upstream error", result)
	}
	if validationCalls != 0 {
		t.Fatalf("validationCalls = %d, want repair failure to stop before validation", validationCalls)
	}
	if len(git.pushCalls) != 0 || len(github.resolveCalls) != 0 {
		t.Fatalf("push calls=%d resolve calls=%d, want 0/0 after invalid repair completion", len(git.pushCalls), len(github.resolveCalls))
	}
	loop, err := fixture.repos.Loops.GetByID(context.Background(), result.LoopID)
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
	if run == nil || run.Status != "failed" || run.CurrentStep == nil || *run.CurrentStep != string(stepRepair) {
		t.Fatalf("run = %#v, want failed run at repair step", run)
	}
	if run.LastCompletedStep != nil && *run.LastCompletedStep == string(stepRecheck) {
		t.Fatalf("run = %#v, want downstream steps to remain incomplete", run)
	}
}

func TestProcessClaimedItemPersistsCheckpointWhenRepairReturnsNonCompleted(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{
		listOpen: []PullRequestSummary{{Number: 42, State: "OPEN", HeadSHA: "head-1"}},
		viewResponses: []PullRequestDetail{
			{Number: 42, State: "OPEN", HeadSHA: "head-1", HeadRefName: "feature/fix-42", BaseRefName: "main", BaseSHA: "base-1", Comments: []map[string]any{{"id": "c1", "threadId": "t1", "body": "please fix"}}},
			{Number: 42, State: "OPEN", HeadSHA: "head-1", HeadRefName: "feature/fix-42", BaseRefName: "main", BaseSHA: "base-1", Comments: []map[string]any{{"id": "c1", "threadId": "t1", "body": "please fix"}}},
		},
	}
	git := &fakeGitGateway{
		createResult:  CreateWorktreeResult{WorktreePath: filepath.Join(t.TempDir(), "wt-42"), Branch: "feature/fix-42", HeadSHA: "base-head"},
		prepareResult: PrepareWorktreeResult{HeadSHA: "base-head", Clean: true},
	}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "failed", Summary: "upstream server_error"}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git, AgentExecutor: agent, AllowAutoCommit: true, AllowAutoPush: true, AllowRiskyFixes: true, Logger: fixture.logger, Now: fixture.now})

	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "fixer-worker-1", "fixer")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNextOfType() = (%#v, %v), want claimed item", claim, err)
	}
	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "failed" || result.FailureKind != FailureRetryableTransient || !contains(result.Summary, "server_error") {
		t.Fatalf("result = %#v, want retryable failed result with upstream error", result)
	}
	run, err := fixture.repos.Runs.GetByID(context.Background(), result.RunID)
	if err != nil {
		t.Fatalf("Runs.GetByID() error = %v", err)
	}
	checkpoint := parseCheckpoint(run.CheckpointJSON)
	if checkpoint.ResumePolicy != "retry_from_timeout_context" {
		t.Fatalf("checkpoint.ResumePolicy = %q, want retry_from_timeout_context", checkpoint.ResumePolicy)
	}
	if checkpoint.Repair == nil || checkpoint.Repair.Status != "failed" || checkpoint.Repair.Summary != "upstream server_error" {
		t.Fatalf("checkpoint.Repair = %#v, want persisted repair checkpoint", checkpoint.Repair)
	}
}

func TestProcessClaimedItemTreatsAgentSetupFailureAsRetryableTransient(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{
		listOpen: []PullRequestSummary{{Number: 42, State: "OPEN", HeadSHA: "head-1"}},
		viewResponses: []PullRequestDetail{
			{Number: 42, State: "OPEN", HeadSHA: "head-1", HeadRefName: "feature/fix-42", BaseRefName: "main", BaseSHA: "base-1", Comments: []map[string]any{{"id": "c1", "threadId": "t1", "body": "please fix"}}},
			{Number: 42, State: "OPEN", HeadSHA: "head-1", HeadRefName: "feature/fix-42", BaseRefName: "main", BaseSHA: "base-1", Comments: []map[string]any{{"id": "c1", "threadId": "t1", "body": "please fix"}}},
		},
	}
	git := &fakeGitGateway{createResult: CreateWorktreeResult{WorktreePath: filepath.Join(t.TempDir(), "wt-42"), Branch: "feature/fix-42", HeadSHA: "base-head"}, prepareResult: PrepareWorktreeResult{HeadSHA: "base-head", Clean: true}}
	errorMessage := "The 'gpt-5.5' model requires a newer version of Codex. Please upgrade to the latest app or CLI and try again."
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "failed", Stderr: errorMessage}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git, AgentExecutor: agent, ValidationRunner: passValidation, AllowAutoCommit: true, AllowAutoPush: true, AllowRiskyFixes: true, Logger: fixture.logger, Now: fixture.now})

	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "fixer-worker-1", "fixer")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNextOfType() = (%#v, %v), want claimed item", claim, err)
	}

	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "failed" || result.FailureKind != FailureRetryableTransient || !contains(result.Summary, "requires a newer version") {
		t.Fatalf("result = %#v, want retryable_transient with real agent error", result)
	}
	queue, err := fixture.repos.Queue.GetByID(context.Background(), claim.ID)
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	if queue == nil || queue.Status != "queued" || queue.LastErrorKind == nil || *queue.LastErrorKind != string(FailureRetryableTransient) {
		t.Fatalf("queue = %#v, want queued retryable_transient failure", queue)
	}
	loop, err := fixture.repos.Loops.GetByID(context.Background(), result.LoopID)
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if loop == nil || loop.Status != "queued" || loop.NextRunAt == nil {
		t.Fatalf("loop = %#v, want queued loop awaiting retry", loop)
	}
}

func TestRunRepairStepFailsResumedCompletedCheckpointWithoutParsedResult(t *testing.T) {
	t.Parallel()

	runner := New(Options{})
	checkpoint, err := runner.runRepairStep(context.Background(), stepInput{
		Checkpoint: fixerCheckpoint{
			Repair: &checkpointRepair{
				Summary:     "upstream server_error",
				ParseStatus: "",
			},
		},
	})
	if err == nil {
		t.Fatalf("runRepairStep() error = nil, want parse-status failure")
	}
	if checkpoint.Repair == nil {
		t.Fatal("checkpoint.Repair = nil, want checkpoint preserved")
	}
	var loopErr *loopError
	if !errors.As(err, &loopErr) {
		t.Fatalf("error = %T, want *loopError", err)
	}
	if loopErr.kind != FailureRetryableTransient {
		t.Fatalf("loopErr.kind = %v, want %v", loopErr.kind, FailureRetryableTransient)
	}
	if !contains(err.Error(), "server_error") {
		t.Fatalf("error = %q, want upstream summary", err.Error())
	}
}

func TestCreateRunContextRewindsToPrepareWhenPostRepairResumeCheckpointParseStatusIsInvalid(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	repo := "acme/looper"
	prNumber := int64(42)
	loopTarget := "pr:acme/looper:42"
	nowISO := fixture.nowISO()
	if err := fixture.repos.Loops.Upsert(context.Background(), storage.LoopRecord{
		ID:         "loop_fixer_rewind_invalid_repair",
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
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, Logger: fixture.logger, Now: fixture.now})
	checkpointJSON := mustMarshalJSON(fixerCheckpoint{
		ClaimedLockKey: "pr:acme/looper:42",
		FixItems:       []FixItem{{Type: "comment", ID: "c1", ThreadID: "t1"}},
		Worktree:       &checkpointWorktree{Path: filepath.Join(t.TempDir(), "wt-42"), Branch: "feature/fix-42", HeadSHA: "head-1", BaseHeadSHA: "base-1", PreparedAt: nowISO},
		Lifecycle:      &lifecycle.State{Branch: "feature/fix-42", BaseBranch: "main", CommitSHAs: []string{"commit-1"}, Pushed: true, PRNumber: 42, PRURL: "https://example/pr/42", Actions: lifecycle.Actions{Commit: lifecycle.ActionSourceAgent, Push: lifecycle.ActionSourceAgent, PR: lifecycle.ActionSourceFallback}},
		Repair:         &checkpointRepair{Summary: "upstream server_error", ParseStatus: "", CompletedAt: nowISO},
		Validation:     &ValidationResult{Passed: true, Summary: "stale"},
		Push:           &checkpointPush{Pushed: true, Branch: "feature/fix-42", Remote: "origin", PushedAt: nowISO},
		ResolvedComments: &checkpointResolvedComments{
			Items: []checkpointResolvedComment{{FixItemID: "c1", ThreadID: "t1", Status: "resolved", UpdatedAt: nowISO}},
		},
		Recheck: &checkpointRecheck{RemainingFixItems: []FixItem{{Type: "comment", ID: "c1", ThreadID: "t1"}}},
	})
	if err := fixture.repos.Runs.Upsert(context.Background(), storage.RunRecord{
		ID:                "run_failed_after_recheck",
		LoopID:            "loop_fixer_rewind_invalid_repair",
		Status:            "failed",
		CurrentStep:       stringPtr(string(stepRecheck)),
		LastCompletedStep: stringPtr(string(stepResolveComments)),
		CheckpointJSON:    &checkpointJSON,
		StartedAt:         nowISO,
		CreatedAt:         nowISO,
		UpdatedAt:         nowISO,
	}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}
	loop, err := fixture.repos.Loops.GetByID(context.Background(), "loop_fixer_rewind_invalid_repair")
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if loop == nil {
		t.Fatal("loop = nil, want fixer loop")
	}

	resumed, err := runner.createRunContext(context.Background(), *loop)
	if err != nil {
		t.Fatalf("createRunContext() error = %v", err)
	}
	if !resumed.Resumed || resumed.StartStep != stepPrepareWorktree {
		t.Fatalf("resumed = %#v, want prepare-worktree rewind", resumed)
	}
	if resumed.Checkpoint.Repair != nil {
		t.Fatalf("Repair = %#v, want cleared repair checkpoint", resumed.Checkpoint.Repair)
	}
	if resumed.Checkpoint.Validation != nil || resumed.Checkpoint.Push != nil || resumed.Checkpoint.ResolvedComments != nil || resumed.Checkpoint.Recheck != nil {
		t.Fatalf("checkpoint = %#v, want post-repair checkpoints cleared", resumed.Checkpoint)
	}
	if resumed.Checkpoint.Worktree == nil || resumed.Checkpoint.Worktree.PreparedAt != "" {
		t.Fatalf("Worktree = %#v, want worktree retained but marked for reprepare", resumed.Checkpoint.Worktree)
	}
	if resumed.Checkpoint.Lifecycle == nil || len(resumed.Checkpoint.Lifecycle.CommitSHAs) != 1 || !resumed.Checkpoint.Lifecycle.Pushed || resumed.Checkpoint.Lifecycle.PRNumber != 42 || resumed.Checkpoint.Lifecycle.Actions.PR != lifecycle.ActionSourceFallback {
		t.Fatalf("Lifecycle = %#v, want lifecycle metadata preserved across prepare rewind", resumed.Checkpoint.Lifecycle)
	}
	if resumed.Run.LastCompletedStep == nil || *resumed.Run.LastCompletedStep != string(stepCollectFixes) {
		t.Fatalf("run.LastCompletedStep = %#v, want collect-fixes", resumed.Run.LastCompletedStep)
	}
}

func TestCreateRunContextRestartsFromDiscoverForRediscoverableCheckpoint(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	repo := "acme/looper"
	prNumber := int64(42)
	loopTarget := "pr:acme/looper:42"
	nowISO := fixture.nowISO()
	if err := fixture.repos.Loops.Upsert(context.Background(), storage.LoopRecord{
		ID:         "loop_fixer_manual_restart",
		Seq:        1,
		ProjectID:  "project_1",
		Type:       "fixer",
		TargetType: "pull_request",
		TargetID:   &loopTarget,
		Repo:       &repo,
		PRNumber:   &prNumber,
		Status:     "running",
		CreatedAt:  nowISO,
		UpdatedAt:  nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	checkpointJSON := mustMarshalJSON(fixerCheckpoint{
		ResumePolicy: "restart_from_discover",
		Detail: &checkpointDetail{
			State:       "OPEN",
			HeadSHA:     "head-1",
			HeadRefName: "feature/fix-42",
			BaseRefName: "main",
			Comments:    []map[string]any{{"id": "c1", "threadId": "t1", "body": "please fix"}},
		},
		FixItems:   []FixItem{{Type: "comment", ID: "c1", ThreadID: "t1"}},
		Worktree:   &checkpointWorktree{Path: filepath.Join(t.TempDir(), "wt-42"), Branch: "feature/fix-42", HeadSHA: "head-1", BaseHeadSHA: "head-1", PreparedAt: nowISO},
		Repair:     &checkpointRepair{Status: "completed", Summary: "nothing to change", CompletedAt: nowISO},
		Validation: &ValidationResult{Passed: true, Summary: "passed"},
		Push:       &checkpointPush{Pushed: false, Branch: "feature/fix-42", Remote: "origin", SkippedReason: "No new commits to push"},
	})
	manualReason := "resolve-comments refused because fixer produced no new commits to push; leaving review threads unresolved"
	if err := fixture.repos.Runs.Upsert(context.Background(), storage.RunRecord{
		ID:                "run_manual_intervention",
		LoopID:            "loop_fixer_manual_restart",
		Status:            "failed",
		CurrentStep:       stringPtr(string(stepResolveComments)),
		LastCompletedStep: stringPtr(string(stepPush)),
		CheckpointJSON:    &checkpointJSON,
		Summary:           &manualReason,
		ErrorMessage:      &manualReason,
		StartedAt:         nowISO,
		CreatedAt:         nowISO,
		UpdatedAt:         nowISO,
	}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, Logger: fixture.logger, Now: fixture.now})
	loop, err := fixture.repos.Loops.GetByID(context.Background(), "loop_fixer_manual_restart")
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if loop == nil {
		t.Fatal("loop = nil, want fixer loop")
	}

	resumed, err := runner.createRunContext(context.Background(), *loop)
	if err != nil {
		t.Fatalf("createRunContext() error = %v", err)
	}
	if resumed.Resumed || resumed.StartStep != stepDiscoverPR {
		t.Fatalf("resumed = %#v, want fresh discover run", resumed)
	}
	if resumed.Checkpoint.Detail != nil || len(resumed.Checkpoint.FixItems) != 0 || resumed.Checkpoint.Worktree != nil || resumed.Checkpoint.Push != nil {
		t.Fatalf("checkpoint = %#v, want cleared manual-intervention checkpoint", resumed.Checkpoint)
	}
	if resumed.Checkpoint.ResumePolicy != "replay_step" {
		t.Fatalf("ResumePolicy = %q, want replay_step", resumed.Checkpoint.ResumePolicy)
	}
}

func TestProcessClaimedQueueItemResumeValidationFailureUpdatesLoopState(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	git := &fakeGitGateway{}
	repo := "acme/looper"
	prNumber := int64(42)
	loopTarget := "pr:acme/looper:42"
	nowISO := fixture.nowISO()
	if err := fixture.repos.Loops.Upsert(context.Background(), storage.LoopRecord{
		ID:         "loop_fixer_resume_parse_status",
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
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, Git: git, Logger: fixture.logger, Now: fixture.now})
	checkpointJSON := mustMarshalJSON(fixerCheckpoint{
		Worktree: &checkpointWorktree{
			Path:   filepath.Join(t.TempDir(), "wt-42"),
			Branch: "feature/fix-42",
		},
		Repair: &checkpointRepair{
			Summary:     "upstream server_error",
			ParseStatus: "",
		},
	})
	if err := fixture.repos.Runs.Upsert(context.Background(), storage.RunRecord{
		ID:                "run_failed_after_repair",
		LoopID:            "loop_fixer_resume_parse_status",
		Status:            "failed",
		LastCompletedStep: stringPtr(string(stepRepair)),
		CheckpointJSON:    &checkpointJSON,
		StartedAt:         nowISO,
		CreatedAt:         nowISO,
		UpdatedAt:         nowISO,
	}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}
	projectID := "project_1"
	loopID := "loop_fixer_resume_parse_status"
	if err := fixture.repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{
		ID:          "queue_fixer_resume_parse_status",
		ProjectID:   &projectID,
		LoopID:      &loopID,
		Type:        "fixer",
		TargetType:  "pull_request",
		TargetID:    loopTarget,
		Repo:        &repo,
		PRNumber:    &prNumber,
		DedupeKey:   "fixer:acme/looper:42:resume-parse",
		Priority:    1,
		Status:      "queued",
		AvailableAt: nowISO,
		MaxAttempts: 1,
		CreatedAt:   nowISO,
		UpdatedAt:   nowISO,
	}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "fixer-worker-1", "fixer")
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

	queue, err := fixture.repos.Queue.GetByID(context.Background(), "queue_fixer_resume_parse_status")
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	if queue == nil || queue.Status != "failed" {
		t.Fatalf("queue = %#v, want failed terminal queue item", queue)
	}

	loop, err := fixture.repos.Loops.GetByID(context.Background(), "loop_fixer_resume_parse_status")
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if loop == nil || loop.Status != "failed" || loop.NextRunAt != nil {
		t.Fatalf("loop = %#v, want failed terminal loop", loop)
	}
	if len(git.cleanupCalls) != 1 {
		t.Fatalf("len(git.cleanupCalls) = %d, want 1", len(git.cleanupCalls))
	}
	if git.cleanupCalls[0].WorktreePath == "" || git.cleanupCalls[0].Branch != "feature/fix-42" {
		t.Fatalf("cleanup call = %#v, want persisted worktree cleanup", git.cleanupCalls[0])
	}
}

func TestProcessClaimedItemRestartsFromDiscoverAfterRemoteHeadChangeAtPush(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{
		listOpen: []PullRequestSummary{{Number: 42, State: "OPEN", HeadSHA: "head-1"}},
		viewResponses: []PullRequestDetail{
			{Number: 42, State: "OPEN", HeadSHA: "head-1", HeadRefName: "feature/fix-42", BaseRefName: "main", BaseSHA: "base-1", Comments: []map[string]any{{"id": "c1", "threadId": "t1", "body": "please fix"}}},
			{Number: 42, State: "OPEN", HeadSHA: "head-1", HeadRefName: "feature/fix-42", BaseRefName: "main", BaseSHA: "base-1", Comments: []map[string]any{{"id": "c1", "threadId": "t1", "body": "please fix"}}},
			{Number: 42, State: "OPEN", HeadSHA: "head-2", HeadRefName: "feature/fix-42", BaseRefName: "main", BaseSHA: "base-1", Comments: []map[string]any{{"id": "c1", "threadId": "t1", "body": "please fix"}}},
			{Number: 42, State: "OPEN", HeadSHA: "head-2", HeadRefName: "feature/fix-42", BaseRefName: "main", BaseSHA: "base-1", Comments: []map[string]any{{"id": "c1", "threadId": "t1", "body": "please fix"}}},
			{Number: 42, State: "OPEN", HeadSHA: "new-head-2", HeadRefName: "feature/fix-42", BaseRefName: "main", BaseSHA: "base-1"},
			{Number: 42, State: "OPEN", HeadSHA: "new-head-2", HeadRefName: "feature/fix-42", BaseRefName: "main", BaseSHA: "base-1"},
			{Number: 42, State: "OPEN", HeadSHA: "new-head-2", HeadRefName: "feature/fix-42", BaseRefName: "main", BaseSHA: "base-1"},
		},
	}
	git := &fakeGitGateway{
		createResult:  CreateWorktreeResult{WorktreePath: filepath.Join(t.TempDir(), "wt-42"), Branch: "feature/fix-42", HeadSHA: "base-head"},
		prepareResult: PrepareWorktreeResult{HeadSHA: "base-head", Clean: true},
		inspectResults: []InspectHeadResult{
			{HeadSHA: "base-head"}, {HeadSHA: "new-head-1", NewCommitSHAs: []string{"new-head-1"}}, {HeadSHA: "new-head-1"},
			{HeadSHA: "base-head"}, {HeadSHA: "new-head-2", NewCommitSHAs: []string{"new-head-2"}}, {HeadSHA: "new-head-2"},
		},
		pushErrors: []error{fmt.Errorf("remote head changed while pushing")},
	}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "first repair", ParseStatus: "parsed"}, {Status: "completed", Summary: "second repair", ParseStatus: "parsed"}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git, AgentExecutor: agent, ValidationRunner: passValidation, AllowAutoCommit: true, AllowAutoPush: true, AllowRiskyFixes: true, Logger: fixture.logger, Now: fixture.now, Sleep: func(time.Duration) {}})

	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	claim1, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "fixer-worker-1", "fixer")
	if err != nil || claim1 == nil {
		t.Fatalf("first ClaimNextOfType() = (%#v, %v), want claimed item", claim1, err)
	}
	first, err := runner.ProcessClaimedItem(context.Background(), *claim1)
	if err != nil {
		t.Fatalf("ProcessClaimedItem(first) error = %v", err)
	}
	if first.Status != "failed" || first.FailureKind != FailureRetryableAfterResume || !contains(first.Summary, "remote head changed") {
		t.Fatalf("first result = %#v, want retryable remote-head failure", first)
	}
	if len(agent.starts) != 1 {
		t.Fatalf("len(agent.starts) after first run = %d, want 1", len(agent.starts))
	}

	fixture.advance(5 * time.Second)
	claim2, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "fixer-worker-1", "fixer")
	if err != nil || claim2 == nil {
		t.Fatalf("retry ClaimNextOfType() = (%#v, %v), want claimed item", claim2, err)
	}
	second, err := runner.ProcessClaimedItem(context.Background(), *claim2)
	if err != nil {
		t.Fatalf("ProcessClaimedItem(retry) error = %v", err)
	}
	if second.Status != "success" {
		t.Fatalf("second result = %#v, want success", second)
	}
	if len(agent.starts) != 2 {
		t.Fatalf("len(agent.starts) = %d, want 2 (restart from discover)", len(agent.starts))
	}
	if len(git.createCalls) != 2 {
		t.Fatalf("len(git.createCalls) = %d, want 2 (worktree rebuilt)", len(git.createCalls))
	}
}

func TestProcessClaimedItemResumeReacquiresPullRequestLock(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	fixture.repos.Locks.SetNow(fixture.now)
	github := &fakeGitHubGateway{
		listOpen:      []PullRequestSummary{{Number: 42, State: "OPEN", HeadSHA: "head-1"}},
		viewResponses: []PullRequestDetail{{Number: 42, State: "OPEN", HeadSHA: "head-1", HeadRefName: "feature/fix-42", BaseRefName: "main", BaseSHA: "base-1", Comments: []map[string]any{{"id": "c1", "threadId": "t1", "body": "please fix"}}}},
	}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "applied fixes", ParseStatus: "parsed"}}}
	validationCalls := 0
	git := &fakeGitGateway{createResult: CreateWorktreeResult{WorktreePath: filepath.Join(t.TempDir(), "wt-42"), Branch: "feature/fix-42", HeadSHA: "base-head"}, prepareResult: PrepareWorktreeResult{HeadSHA: "base-head", Clean: true}, inspectResults: []InspectHeadResult{{HeadSHA: "base-head"}, {HeadSHA: "new-head", NewCommitSHAs: []string{"new-head"}}, {HeadSHA: "new-head"}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, AllowAutoCommit: true, AllowRiskyFixes: true, ValidationRunner: func(context.Context, ValidationInput) (ValidationResult, error) {
		validationCalls++
		if validationCalls == 1 {
			return ValidationResult{Passed: false, Summary: "Validation failed"}, nil
		}
		return ValidationResult{Passed: true, Summary: "ok"}, nil
	}})

	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	claim1, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "fixer-worker-1", "fixer")
	if err != nil || claim1 == nil {
		t.Fatalf("first ClaimNextOfType() = (%#v, %v), want claimed item", claim1, err)
	}
	first, err := runner.ProcessClaimedItem(context.Background(), *claim1)
	if err != nil {
		t.Fatalf("ProcessClaimedItem(first) error = %v", err)
	}
	if first.Status != "failed" || first.FailureKind != FailureRetryableAfterResume {
		t.Fatalf("first = %#v, want retryable-after-resume validation failure", first)
	}
	fixture.advance(5 * time.Second)
	claim2, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "fixer-worker-1", "fixer")
	if err != nil || claim2 == nil {
		t.Fatalf("retry ClaimNextOfType() = (%#v, %v), want claimed item", claim2, err)
	}
	lockKey := buildPullRequestLockKey(*claim2)
	if acquired, err := fixture.repos.Locks.Acquire(context.Background(), storage.LockRecord{Key: lockKey, Owner: "other-fixer", ExpiresAt: fixture.now().Add(time.Minute).UTC().Format("2006-01-02T15:04:05.000Z"), CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}); err != nil {
		t.Fatalf("Locks.Acquire() error = %v", err)
	} else if !acquired {
		t.Fatal("Locks.Acquire() = false, want competing lock holder")
	}

	result, err := runner.ProcessClaimedItem(context.Background(), *claim2)
	if err == nil || !contains(err.Error(), lockKey) {
		t.Fatalf("ProcessClaimedItem(retry) error = %v, want lock reacquire failure", err)
	}
	if result != (ProcessResult{}) {
		t.Fatalf("result = %#v, want zero result on resume lock failure", result)
	}
	if validationCalls != 1 {
		t.Fatalf("validationCalls = %d, want 1 (resume should stop before validate reruns)", validationCalls)
	}
	queue, err := fixture.repos.Queue.GetByID(context.Background(), claim2.ID)
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	if queue == nil || queue.Status != "running" {
		t.Fatalf("queue = %#v, want still-running claimed item after setup failure", queue)
	}
}

func TestProcessNextSetupFailureMarksQueueFailed(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	projectID := "project_1"
	prNumber := int64(99)
	nowISO := fixture.nowISO()
	if err := fixture.repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: "queue_missing_fixer", ProjectID: &projectID, Type: "fixer", TargetType: "pull_request", TargetID: "pr:acme/looper:99", Repo: stringPtr("acme/looper"), PRNumber: &prNumber, DedupeKey: "fixer:acme/looper:99:test", Priority: 1, Status: "queued", AvailableAt: nowISO, MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: &fakeGitHubGateway{}, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})

	result, err := runner.ProcessNext(context.Background(), "fixer-worker-1")
	if err != nil {
		t.Fatalf("ProcessNext() error = %v", err)
	}
	if result == nil || result.Status != "failed" || result.FailureKind != FailureNonRetryable || !contains(result.Summary, "requires loopId") {
		t.Fatalf("result = %#v, want non-retryable missing-loopId failure", result)
	}
	queue, err := fixture.repos.Queue.GetByID(context.Background(), "queue_missing_fixer")
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
	prNumber := int64(42)
	loopTarget := "pr:acme/looper:42"
	if err := fixture.repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: "loop_fixer_running", Seq: 2, ProjectID: "project_1", Type: "fixer", TargetType: "pull_request", TargetID: &loopTarget, Repo: stringPtr("acme/looper"), PRNumber: &prNumber, Status: "running", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	projectID := "project_1"
	loopID := "loop_fixer_running"
	if err := fixture.repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: "queue_fixer_running", ProjectID: &projectID, LoopID: &loopID, Type: "fixer", TargetType: "pull_request", TargetID: loopTarget, Repo: stringPtr("acme/looper"), PRNumber: &prNumber, DedupeKey: "fixer:acme/looper:42:test", Priority: 1, Status: "running", AvailableAt: nowISO, Attempts: 0, MaxAttempts: 1, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: &fakeGitHubGateway{}, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})

	result, err := runner.recoverClaimedItem(context.Background(), storage.QueueItemRecord{ID: "queue_fixer_running", ProjectID: &projectID, LoopID: &loopID, Type: "fixer", TargetType: "pull_request", TargetID: loopTarget, Repo: stringPtr("acme/looper"), PRNumber: &prNumber, DedupeKey: "fixer:acme/looper:42:test", Priority: 1, Status: "running", AvailableAt: nowISO, Attempts: 0, MaxAttempts: 1, CreatedAt: nowISO, UpdatedAt: nowISO}, fmt.Errorf("persist step failed"))
	if err != nil {
		t.Fatalf("recoverClaimedItem() error = %v", err)
	}
	if result == nil || result.Status != "failed" || result.FailureKind != FailureNonRetryable {
		t.Fatalf("result = %#v, want failed non-retryable recovery", result)
	}
	queue, err := fixture.repos.Queue.GetByID(context.Background(), "queue_fixer_running")
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

func TestProcessClaimedItemAutoPushDisabledSkipsManualIntervention(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{
		listOpen: []PullRequestSummary{{Number: 42, State: "OPEN", HeadSHA: "head-1"}},
		viewResponses: []PullRequestDetail{
			{Number: 42, State: "OPEN", HeadSHA: "head-1", HeadRefName: "feature/fix-42", BaseRefName: "main", BaseSHA: "base-1", Comments: []map[string]any{{"id": "c1", "threadId": "t1", "body": "please fix"}}},
			{Number: 42, State: "OPEN", HeadSHA: "head-1", HeadRefName: "feature/fix-42", BaseRefName: "main", BaseSHA: "base-1", Comments: []map[string]any{{"id": "c1", "threadId": "t1", "body": "please fix"}}},
		},
	}
	git := &fakeGitGateway{
		createResult:  CreateWorktreeResult{WorktreePath: filepath.Join(t.TempDir(), "wt-42"), Branch: "feature/fix-42", HeadSHA: "base-head"},
		prepareResult: PrepareWorktreeResult{HeadSHA: "base-head", Clean: true},
		inspectResults: []InspectHeadResult{
			{HeadSHA: "base-head"},
			{HeadSHA: "new-head", NewCommitSHAs: []string{"new-head"}},
			{HeadSHA: "new-head"},
		},
	}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "applied fixes", ParseStatus: "parsed"}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git, AgentExecutor: agent, ValidationRunner: passValidation, AllowAutoCommit: true, AllowAutoPush: false, AllowRiskyFixes: true, Logger: fixture.logger, Now: fixture.now})

	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "fixer-worker-1", "fixer")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNextOfType() = (%#v, %v), want claimed item", claim, err)
	}

	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "failed" || result.FailureKind != FailureManualIntervention || !contains(result.Summary, "Auto push disabled") {
		t.Fatalf("result = %#v, want manual-intervention auto-push summary", result)
	}
	if len(git.pushCalls) != 0 {
		t.Fatalf("len(git.pushCalls) = %d, want 0", len(git.pushCalls))
	}
	queue, err := fixture.repos.Queue.GetByID(context.Background(), claim.ID)
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	if queue == nil || queue.Status != string(FailureManualIntervention) || queue.LastErrorKind == nil || *queue.LastErrorKind != string(FailureManualIntervention) {
		t.Fatalf("queue = %#v, want failed manual_intervention queue item", queue)
	}
	run, err := fixture.repos.Runs.GetByID(context.Background(), result.RunID)
	if err != nil {
		t.Fatalf("Runs.GetByID() error = %v", err)
	}
	if run == nil || run.Status != "failed" || run.CurrentStep == nil || *run.CurrentStep != string(stepPush) {
		t.Fatalf("run = %#v, want failed push step run", run)
	}
	checkpoint := parseCheckpoint(run.CheckpointJSON)
	if checkpoint.ResumePolicy != "manual_intervention" {
		t.Fatalf("checkpoint.ResumePolicy = %q, want manual_intervention", checkpoint.ResumePolicy)
	}
	if checkpoint.Push == nil || checkpoint.Push.SkippedReason != "Auto push disabled" {
		t.Fatalf("checkpoint.Push = %#v, want recorded auto-push-disabled checkpoint", checkpoint.Push)
	}
	loop, err := fixture.repos.Loops.GetByID(context.Background(), result.LoopID)
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if loop == nil || loop.Status != "paused" || loop.NextRunAt != nil {
		t.Fatalf("loop = %#v, want paused hard-hold loop", loop)
	}
}

func TestRunPrepareWorktreeStepPausesDirtyWorktree(t *testing.T) {
	t.Parallel()

	runner := New(Options{Git: &fakeGitGateway{createResult: CreateWorktreeResult{WorktreePath: filepath.Join(t.TempDir(), "wt-42"), Branch: "feature/fix-42", HeadSHA: "base-head"}, prepareResult: PrepareWorktreeResult{HeadSHA: "base-head", Clean: false}}})
	checkpoint, err := runner.runPrepareWorktreeStep(context.Background(), stepInput{
		Project:    storage.ProjectRecord{ID: "project_1", RepoPath: t.TempDir()},
		Repo:       "acme/looper",
		PRNumber:   42,
		Checkpoint: fixerCheckpoint{Detail: &checkpointDetail{HeadSHA: "base-head", HeadRefName: "feature/fix-42", BaseRefName: "main"}},
	})
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

func TestRunRepairStepRequiresManualInterventionForRiskyConflictWhenDisabled(t *testing.T) {
	t.Parallel()

	runner := New(Options{AllowRiskyFixes: false})
	checkpoint, err := runner.runRepairStep(context.Background(), stepInput{
		Repo:     "acme/looper",
		PRNumber: 42,
		Checkpoint: fixerCheckpoint{
			FixItems: []FixItem{{Type: "conflict", Summary: "merge conflict"}},
		},
	})
	if err == nil {
		t.Fatal("runRepairStep() error = nil, want manual intervention")
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
	if !contains(loopErr.Error(), "risky conflict fixes require manual intervention") {
		t.Fatalf("error = %q, want risky conflict summary", loopErr.Error())
	}
}

func TestRunRepairStepRecreatesCheckpointOutsideWorktreeRootAndRunsAgent(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	repoPath := t.TempDir()
	worktreeRoot := filepath.Join(t.TempDir(), "worktrees")
	legacyPath := filepath.Join(t.TempDir(), "legacy-wt")
	metadata := fmt.Sprintf(`{"worktreeRoot":%q}`, worktreeRoot)
	git := &fakeGitGateway{createResult: CreateWorktreeResult{WorktreePath: filepath.Join(worktreeRoot, "wt"), Branch: "feature/fix-42", HeadSHA: "base-head"}, prepareResult: PrepareWorktreeResult{HeadSHA: "head-1", Clean: true}}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "applied fixes", ParseStatus: "parsed"}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, Git: git, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now})

	checkpoint, err := runner.runRepairStep(context.Background(), stepInput{
		Project:  storage.ProjectRecord{ID: "project_1", RepoPath: repoPath, MetadataJSON: &metadata},
		Repo:     "acme/looper",
		PRNumber: 42,
		Checkpoint: fixerCheckpoint{
			Detail:       &checkpointDetail{HeadRefName: "feature/fix-42", BaseRefName: "main", HeadSHA: "head-1"},
			FixItems:     []FixItem{{ID: "fix-1", Summary: "repair disclosure"}},
			Worktree:     &checkpointWorktree{Path: legacyPath, Branch: "feature/fix-42", PreparedAt: "stale"},
			FixItemsHash: "hash-1",
		},
	})
	if err != nil {
		t.Fatalf("runRepairStep() error = %v", err)
	}
	if len(git.createCalls) != 1 {
		t.Fatalf("len(git.createCalls) = %d, want 1", len(git.createCalls))
	}
	if len(git.prepareCalls) != 1 {
		t.Fatalf("len(git.prepareCalls) = %d, want 1", len(git.prepareCalls))
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
	if checkpoint.Repair == nil || checkpoint.Repair.Summary != "applied fixes" {
		t.Fatalf("checkpoint.Repair = %#v, want completed repair after worktree recovery", checkpoint.Repair)
	}
}

func TestReconcileCommitsRequiresManualInterventionWhenAutoCommitDisabledAndDirty(t *testing.T) {
	t.Parallel()

	runner := New(Options{Git: &fakeGitGateway{inspectResults: []InspectHeadResult{{HeadSHA: "head-1", HasUncommittedChanges: true, ChangedFiles: []string{"file.txt"}}}}, AllowAutoCommit: false})
	project := storage.ProjectRecord{ID: "project_1", RepoPath: t.TempDir()}
	checkpoint, err := runner.reconcileCommits(context.Background(), project, fixerCheckpoint{Worktree: &checkpointWorktree{Path: filepath.Join(t.TempDir(), "wt-42"), Branch: "feature/fix-42", HeadSHA: "head-1", BaseHeadSHA: "head-1"}}, "fix: test")
	if err == nil {
		t.Fatal("reconcileCommits() error = nil, want manual intervention")
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
	project.MetadataJSON = nil
	project.UpdatedAt = fixture.nowISO()
	if err := fixture.repos.Projects.Upsert(context.Background(), *project); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	github := &fakeGitHubGateway{
		listOpen:      []PullRequestSummary{{Number: 42, State: "OPEN", HeadSHA: "head-1"}},
		viewResponses: []PullRequestDetail{{Number: 42, State: "OPEN", HeadSHA: "head-1", HeadRefName: "feature/fix-42", BaseRefName: "main", BaseSHA: "base-1", Comments: []map[string]any{{"id": "c1", "threadId": "t1", "body": "please fix"}}}},
	}
	git := &fakeGitGateway{createResult: CreateWorktreeResult{WorktreePath: filepath.Join(t.TempDir(), "wt-42"), Branch: "feature/fix-42", HeadSHA: "base-head"}, prepareResult: PrepareWorktreeResult{HeadSHA: "base-head", Clean: true}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git, AgentExecutor: &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "applied fixes", ParseStatus: "parsed"}}}, ValidationRunner: passValidation, AllowAutoCommit: true, AllowAutoPush: false, AllowRiskyFixes: true, Logger: fixture.logger, Now: fixture.now})

	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "fixer-worker-1", "fixer")
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

type runnerFixture struct {
	coordinator *storage.SQLiteCoordinator
	repos       *storage.Repositories
	logger      *testLogger
	current     time.Time
	now         func() time.Time
}

func newRunnerFixture(t *testing.T) *runnerFixture {
	t.Helper()
	coordinator, err := storage.OpenSQLiteCoordinator(context.Background(), filepath.Join(t.TempDir(), "fixer.sqlite"), storage.SQLiteCoordinatorOptions{BackupDir: t.TempDir()})
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
	currentUser           string
	listOpen              []PullRequestSummary
	listOpenByLabel       map[string][]PullRequestSummary
	listCalls             []ListOpenPullRequestsInput
	viewResponses         []PullRequestDetail
	viewIndex             int
	resolveCalls          []ResolveReviewThreadInput
	addLabelCalls         []PullRequestLabelsInput
	removeLabelCalls      []PullRequestLabelsInput
	replyCalls            []AddReviewThreadReplyInput
	replyErr              error
	createIssueComments   []IssueCommentInput
	updateIssueComments   []UpdateIssueCommentInput
	createIssueCommentErr error
	updateIssueCommentErr error
	nextIssueCommentID    int64
}

func (f *fakeGitHubGateway) ListOpenPullRequests(_ context.Context, input ListOpenPullRequestsInput) ([]PullRequestSummary, error) {
	f.listCalls = append(f.listCalls, input)
	if f.listOpenByLabel != nil {
		return append([]PullRequestSummary(nil), f.listOpenByLabel[input.Label]...), nil
	}
	result := append([]PullRequestSummary(nil), f.listOpen...)
	for index := range result {
		if result[index].Author == "" {
			result[index].Author = firstNonEmpty(f.currentUser, "looper")
		}
	}
	return result, nil
}

func (f *fakeGitHubGateway) GetCurrentUserLogin(context.Context, string) (string, error) {
	return firstNonEmpty(f.currentUser, "looper"), nil
}

func (f *fakeGitHubGateway) GetPullRequestAuthor(_ context.Context, input ViewPullRequestInput) (string, error) {
	for _, detail := range f.viewResponses {
		if detail.Number == input.PRNumber && detail.Author != "" {
			return detail.Author, nil
		}
	}
	return firstNonEmpty(f.currentUser, "looper"), nil
}

func (f *fakeGitHubGateway) ViewPullRequest(_ context.Context, input ViewPullRequestInput) (PullRequestDetail, error) {
	if len(f.viewResponses) == 0 {
		return PullRequestDetail{Number: input.PRNumber, State: "OPEN", HeadSHA: "head-default", HeadRefName: "feature/default", BaseRefName: "main", BaseSHA: "base-default"}, nil
	}
	idx := f.viewIndex
	if idx >= len(f.viewResponses) {
		idx = len(f.viewResponses) - 1
	}
	result := f.viewResponses[idx]
	f.viewIndex++
	if result.Number == 0 {
		result.Number = input.PRNumber
	}
	if result.Author == "" {
		result.Author = firstNonEmpty(f.currentUser, "looper")
	}
	return result, nil
}

func (f *fakeGitHubGateway) ResolveReviewThread(_ context.Context, input ResolveReviewThreadInput) error {
	f.resolveCalls = append(f.resolveCalls, input)
	return nil
}

func (f *fakeGitHubGateway) AddReviewThreadReply(_ context.Context, input AddReviewThreadReplyInput) error {
	f.replyCalls = append(f.replyCalls, input)
	return f.replyErr
}

func (f *fakeGitHubGateway) CreateIssueComment(_ context.Context, input IssueCommentInput) (IssueCommentResult, error) {
	f.createIssueComments = append(f.createIssueComments, input)
	if f.createIssueCommentErr != nil {
		return IssueCommentResult{}, f.createIssueCommentErr
	}
	if f.nextIssueCommentID == 0 {
		f.nextIssueCommentID = 9000
	}
	id := f.nextIssueCommentID
	f.nextIssueCommentID++
	return IssueCommentResult{ID: id, URL: fmt.Sprintf("https://example.test/c/%d", id)}, nil
}

func (f *fakeGitHubGateway) UpdateIssueComment(_ context.Context, input UpdateIssueCommentInput) error {
	f.updateIssueComments = append(f.updateIssueComments, input)
	return f.updateIssueCommentErr
}

func (f *fakeGitHubGateway) AddPullRequestLabels(_ context.Context, input PullRequestLabelsInput) error {
	f.addLabelCalls = append(f.addLabelCalls, input)
	return nil
}

func (f *fakeGitHubGateway) RemovePullRequestLabels(_ context.Context, input PullRequestLabelsInput) error {
	f.removeLabelCalls = append(f.removeLabelCalls, input)
	return nil
}

type fakeGitGateway struct {
	createResult   CreateWorktreeResult
	prepareResult  PrepareWorktreeResult
	inspectResults []InspectHeadResult
	inspectIndex   int
	pushErrors     []error
	pushIndex      int

	createCalls  []CreateWorktreeInput
	prepareCalls []PrepareWorktreeInput
	inspectCalls []InspectHeadInput
	commitCalls  []CommitInput
	pushCalls    []PushInput
	cleanupCalls []CleanupWorktreeInput
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
	if result.HeadSHA == "" {
		result.HeadSHA = "base-head"
	}
	f.createResult = result
	return result, nil
}

func (f *fakeGitGateway) PrepareWorktree(_ context.Context, input PrepareWorktreeInput) (PrepareWorktreeResult, error) {
	f.prepareCalls = append(f.prepareCalls, input)
	result := f.prepareResult
	if result.HeadSHA == "" {
		result.HeadSHA = firstNonEmpty(input.ExpectedHeadSHA, "base-head")
	}
	if !result.Clean {
		return result, nil
	}
	return result, nil
}

func (f *fakeGitGateway) InspectHead(_ context.Context, input InspectHeadInput) (InspectHeadResult, error) {
	f.inspectCalls = append(f.inspectCalls, input)
	if len(f.inspectResults) == 0 {
		return InspectHeadResult{HeadSHA: "head"}, nil
	}
	idx := f.inspectIndex
	if idx >= len(f.inspectResults) {
		idx = len(f.inspectResults) - 1
	}
	result := f.inspectResults[idx]
	f.inspectIndex++
	return result, nil
}

func (f *fakeGitGateway) Commit(_ context.Context, input CommitInput) (CommitResult, error) {
	f.commitCalls = append(f.commitCalls, input)
	return CommitResult{CommitSHA: fmt.Sprintf("commit-%d", len(f.commitCalls))}, nil
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

func (f *fakeGitGateway) CleanupWorktree(_ context.Context, input CleanupWorktreeInput) error {
	f.cleanupCalls = append(f.cleanupCalls, input)
	return nil
}

type fakeAgentExecutor struct {
	results []AgentResult
	starts  []AgentRunInput
}

func (f *fakeAgentExecutor) Start(_ context.Context, input AgentRunInput) (AgentExecution, error) {
	f.starts = append(f.starts, input)
	if len(f.results) == 0 {
		return nil, fmt.Errorf("no queued agent result")
	}
	result := f.results[0]
	f.results = f.results[1:]
	return fakeAgentExecution{result: result}, nil
}

type fakeAgentExecution struct{ result AgentResult }

func (f fakeAgentExecution) Wait(context.Context) (AgentResult, error) { return f.result, nil }

func passValidation(context.Context, ValidationInput) (ValidationResult, error) {
	return ValidationResult{Passed: true, Summary: "ok"}, nil
}

type testLogger struct{}

func (*testLogger) Debug(string, map[string]any) {}
func (*testLogger) Info(string, map[string]any)  {}
func (*testLogger) Warn(string, map[string]any)  {}
func (*testLogger) Error(string, map[string]any) {}

func contains(haystack, needle string) bool { return strings.Contains(haystack, needle) }

func TestBuildFixerReplyBodyMentionsAuthorAndCommit(t *testing.T) {
	t.Parallel()
	got := buildFixerReplyBody(FixItem{Type: "comment", ID: "c1", ThreadID: "t1", Author: "alice", Summary: "Use fmt.Errorf instead of errors.New"}, "abcdef1234567", "")
	if !strings.Contains(got, "@alice") {
		t.Fatalf("body missing author mention:\n%s", got)
	}
	if !strings.Contains(got, "abcdef1") {
		t.Fatalf("body missing short commit SHA:\n%s", got)
	}
	if !strings.Contains(got, "> Use fmt.Errorf") {
		t.Fatalf("body missing original summary quote:\n%s", got)
	}
}

func TestBuildFixerReplyBodyHandlesMissingAuthorAndCommit(t *testing.T) {
	t.Parallel()
	got := buildFixerReplyBody(FixItem{Type: "comment", ID: "c1", ThreadID: "t1"}, "", "")
	if strings.Contains(got, "@") {
		t.Fatalf("body should not include @mention when author missing:\n%s", got)
	}
	if strings.Contains(got, " in ") {
		t.Fatalf("body should not advertise commit when SHA missing:\n%s", got)
	}
	if !strings.Contains(got, "fixed") {
		t.Fatalf("body should still announce fix:\n%s", got)
	}
}

func TestBuildFixerReplyBodyPrefersAgentExplanationOverGenericQuote(t *testing.T) {
	t.Parallel()
	got := buildFixerReplyBody(FixItem{Type: "comment", ID: "c1", ThreadID: "t1", Author: "alice", Summary: "Original review comment that should be hidden"}, "abcdef1", "Replaced strings.Title with cases.Title and added empty-string coverage in foo_test.go.")
	if !strings.Contains(got, "Replaced strings.Title") {
		t.Fatalf("body missing agent explanation:\n%s", got)
	}
	if strings.Contains(got, "Original review comment") {
		t.Fatalf("body should drop original quote when agent explanation present:\n%s", got)
	}
}

func TestProcessClaimedItemRepliesToAuthorBeforeResolving(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{
		listOpen: []PullRequestSummary{{Number: 42, State: "OPEN", HeadSHA: "head-1"}},
		viewResponses: []PullRequestDetail{
			{Number: 42, State: "OPEN", HeadSHA: "head-1", HeadRefName: "feature/fix-42", BaseRefName: "main", BaseSHA: "base-1", Comments: []map[string]any{{"id": "c1", "threadId": "t1", "body": "please fix off-by-one", "author": "alice"}}},
			{Number: 42, State: "OPEN", HeadSHA: "head-1", HeadRefName: "feature/fix-42", BaseRefName: "main", BaseSHA: "base-1", Comments: []map[string]any{{"id": "c1", "threadId": "t1", "body": "please fix off-by-one", "author": "alice"}}},
			{Number: 42, State: "OPEN", HeadSHA: "new-head", HeadRefName: "feature/fix-42", BaseRefName: "main", BaseSHA: "base-1"},
			{Number: 42, State: "OPEN", HeadSHA: "new-head", HeadRefName: "feature/fix-42", BaseRefName: "main", BaseSHA: "base-1"},
			{Number: 42, State: "OPEN", HeadSHA: "new-head", HeadRefName: "feature/fix-42", BaseRefName: "main", BaseSHA: "base-1"},
		},
	}
	git := &fakeGitGateway{
		createResult:  CreateWorktreeResult{WorktreePath: filepath.Join(t.TempDir(), "wt-42"), Branch: "feature/fix-42", HeadSHA: "base-head"},
		prepareResult: PrepareWorktreeResult{HeadSHA: "base-head", Clean: true},
		inspectResults: []InspectHeadResult{
			{HeadSHA: "base-head"},
			{HeadSHA: "new-head", NewCommitSHAs: []string{"new-head"}},
			{HeadSHA: "new-head"},
		},
	}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "applied fixes", ParseStatus: "parsed"}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git, AgentExecutor: agent, ValidationRunner: passValidation, AllowAutoCommit: true, AllowAutoPush: true, AllowRiskyFixes: true, Logger: fixture.logger, Now: fixture.now})

	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "fixer-worker-1", "fixer")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNextOfType() = (%#v, %v), want claimed item", claim, err)
	}
	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "success" {
		t.Fatalf("result = %#v, want success", result)
	}
	if len(github.replyCalls) != 1 {
		t.Fatalf("reply calls = %d, want 1", len(github.replyCalls))
	}
	reply := github.replyCalls[0]
	if reply.ThreadID != "t1" {
		t.Fatalf("reply thread id = %q, want t1", reply.ThreadID)
	}
	if !strings.Contains(reply.Body, "@alice") {
		t.Fatalf("reply body missing @author mention: %q", reply.Body)
	}
	if len(github.resolveCalls) != 1 {
		t.Fatalf("resolve calls = %d, want 1", len(github.resolveCalls))
	}
}

func TestParseReplyExplanationsValid(t *testing.T) {
	t.Parallel()
	stdout := strings.Join([]string{
		"some agent log line",
		`__LOOPER_RESULT__={"summary":"applied","review_thread_replies":[{"fixItemId":"c1","threadId":"t1","explanation":"Replaced strings.Title with cases.Title."}]}`,
	}, "\n")
	got := parseReplyExplanations(stdout, "", []FixItem{{Type: "comment", ID: "c1", ThreadID: "t1"}})
	if len(got) != 1 || got[0].FixItemID != "c1" || !strings.Contains(got[0].Explanation, "cases.Title") {
		t.Fatalf("parseReplyExplanations() = %#v", got)
	}
}

func TestParseReplyExplanationsReadsCompletionMarkerFromStderr(t *testing.T) {
	t.Parallel()
	stderr := strings.Join([]string{
		"runtime warning",
		`__LOOPER_RESULT__={"summary":"applied","review_thread_replies":[{"fixItemId":"c1","threadId":"t1","explanation":"Preserved stderr completion markers."}]}`,
	}, "\n")
	got := parseReplyExplanations("", stderr, []FixItem{{Type: "comment", ID: "c1", ThreadID: "t1"}})
	if len(got) != 1 || got[0].Explanation != "Preserved stderr completion markers." {
		t.Fatalf("parseReplyExplanations() = %#v", got)
	}
}

func TestParseReplyExplanationsSkipsTemplateCompletionMarker(t *testing.T) {
	t.Parallel()
	stdout := strings.Join([]string{
		`__LOOPER_RESULT__={"summary":"applied","review_thread_replies":[{"fixItemId":"c1","threadId":"t1","explanation":"Kept scanning backward to the real completion."}]}`,
		`__LOOPER_RESULT__={"summary":"<one-sentence summary>"}`,
	}, "\n")
	got := parseReplyExplanations(stdout, "", []FixItem{{Type: "comment", ID: "c1", ThreadID: "t1"}})
	if len(got) != 1 || got[0].Explanation != "Kept scanning backward to the real completion." {
		t.Fatalf("parseReplyExplanations() = %#v", got)
	}
}

func TestParseReplyExplanationsDropsUnknownAndMismatchedThread(t *testing.T) {
	t.Parallel()
	stdout := `__LOOPER_RESULT__={"review_thread_replies":[` +
		`{"fixItemId":"c-unknown","explanation":"hi"},` +
		`{"fixItemId":"c1","threadId":"t-wrong","explanation":"wrong thread"},` +
		`{"fixItemId":"c1","threadId":"t1","explanation":"  "},` +
		`{"fixItemId":"c1","threadId":"t1","explanation":"good"},` +
		`{"fixItemId":"c1","threadId":"t1","explanation":"duplicate, must drop"}` +
		`]}`
	got := parseReplyExplanations(stdout, "", []FixItem{{Type: "comment", ID: "c1", ThreadID: "t1"}})
	if len(got) != 1 || got[0].Explanation != "good" {
		t.Fatalf("parseReplyExplanations() = %#v, want only the first valid entry", got)
	}
}

func TestParseReplyExplanationsMalformedFallsBack(t *testing.T) {
	t.Parallel()
	if got := parseReplyExplanations("__LOOPER_RESULT__={bad json}", "", []FixItem{{Type: "comment", ID: "c1", ThreadID: "t1"}}); got != nil {
		t.Fatalf("parseReplyExplanations() = %#v, want nil on bad JSON", got)
	}
	if got := parseReplyExplanations("no marker here", "", []FixItem{{Type: "comment", ID: "c1", ThreadID: "t1"}}); got != nil {
		t.Fatalf("parseReplyExplanations() = %#v, want nil when marker missing", got)
	}
}

func TestSanitizeReplyExplanationStripsDangerousFragments(t *testing.T) {
	t.Parallel()
	input := "@looper-bot, @another: <!-- looper:stamp v=1 --> <script>alert(1)</script>Replaced foo with bar as requested by @octocat."
	got := sanitizeReplyExplanation(input)
	if strings.Contains(got, "@") || strings.Contains(got, "<script") || strings.Contains(got, "looper:stamp") {
		t.Fatalf("sanitizeReplyExplanation() = %q, did not strip", got)
	}
	if !strings.Contains(got, "Replaced foo with bar") {
		t.Fatalf("sanitizeReplyExplanation() = %q, lost real content", got)
	}
}

func TestSanitizeReplyExplanationStripsVisibleDisclosureFooter(t *testing.T) {
	t.Parallel()
	stamp := disclosure.Stamper{Config: config.DefaultDisclosureConfig(), Agent: "opencode"}.MarkdownStamp("fixer")
	input := "Kept the real fix summary.\n\n" + stamp
	if got := sanitizeReplyExplanation(input); got != "Kept the real fix summary." {
		t.Fatalf("sanitizeReplyExplanation() = %q, want disclosure-free summary", got)
	}
}

func TestSummarizeFixItemSanitizesFallbackSummary(t *testing.T) {
	t.Parallel()
	stamp := disclosure.Stamper{Config: config.DefaultDisclosureConfig(), Agent: "opencode"}.MarkdownStamp("fixer")
	got := summarizeFixItem(FixItem{Summary: "@looper-bot <b>Clean up</b> the fallback text.\n\n" + stamp})
	if strings.Contains(got, "@") || strings.Contains(got, "<b>") || strings.Contains(got, "looper:stamp") {
		t.Fatalf("summarizeFixItem() = %q, did not sanitize fallback summary", got)
	}
	if strings.Contains(got, "Powered by Looper") || strings.Contains(got, disclosure.Slogan) {
		t.Fatalf("summarizeFixItem() = %q, leaked disclosure footer", got)
	}
	if !strings.Contains(got, "Clean up the fallback text.") {
		t.Fatalf("summarizeFixItem() = %q, lost sanitized content", got)
	}
}

func TestLookupReplyExplanationsRejectsStaleSnapshot(t *testing.T) {
	t.Parallel()
	checkpoint := fixerCheckpoint{
		FixItemsHash: "hash-current",
		Repair: &checkpointRepair{
			FixItemsHash: "hash-old",
			ReplyExplanations: []replyExplanationEntry{
				{FixItemID: "c1", Explanation: "stale"},
			},
		},
	}
	if got := lookupReplyExplanations(checkpoint); got != nil {
		t.Fatalf("lookupReplyExplanations() = %#v, want nil on stale snapshot", got)
	}
}

func TestLookupReplyExplanationsReturnsCurrent(t *testing.T) {
	t.Parallel()
	checkpoint := fixerCheckpoint{
		FixItemsHash: "h1",
		Repair: &checkpointRepair{
			FixItemsHash: "h1",
			ReplyExplanations: []replyExplanationEntry{
				{FixItemID: "c1", Explanation: "good"},
				{FixItemID: "", Explanation: "skip"},
			},
		},
	}
	got := lookupReplyExplanations(checkpoint)
	if len(got) != 1 || got["c1"] != "good" {
		t.Fatalf("lookupReplyExplanations() = %#v", got)
	}
}

func TestProcessClaimedItemUsesAgentExplanationInReplyBody(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{
		listOpen: []PullRequestSummary{{Number: 42, State: "OPEN", HeadSHA: "head-1"}},
		viewResponses: []PullRequestDetail{
			{Number: 42, State: "OPEN", HeadSHA: "head-1", HeadRefName: "feature/fix-42", BaseRefName: "main", BaseSHA: "base-1", Comments: []map[string]any{{"id": "c1", "threadId": "t1", "body": "please fix off-by-one", "author": "alice"}}},
			{Number: 42, State: "OPEN", HeadSHA: "head-1", HeadRefName: "feature/fix-42", BaseRefName: "main", BaseSHA: "base-1", Comments: []map[string]any{{"id": "c1", "threadId": "t1", "body": "please fix off-by-one", "author": "alice"}}},
			{Number: 42, State: "OPEN", HeadSHA: "new-head", HeadRefName: "feature/fix-42", BaseRefName: "main", BaseSHA: "base-1"},
			{Number: 42, State: "OPEN", HeadSHA: "new-head", HeadRefName: "feature/fix-42", BaseRefName: "main", BaseSHA: "base-1"},
			{Number: 42, State: "OPEN", HeadSHA: "new-head", HeadRefName: "feature/fix-42", BaseRefName: "main", BaseSHA: "base-1"},
		},
	}
	git := &fakeGitGateway{
		createResult:  CreateWorktreeResult{WorktreePath: filepath.Join(t.TempDir(), "wt-42"), Branch: "feature/fix-42", HeadSHA: "base-head"},
		prepareResult: PrepareWorktreeResult{HeadSHA: "base-head", Clean: true},
		inspectResults: []InspectHeadResult{
			{HeadSHA: "base-head"},
			{HeadSHA: "new-head", NewCommitSHAs: []string{"new-head"}},
			{HeadSHA: "new-head"},
		},
	}
	stdout := `__LOOPER_RESULT__={"summary":"done","review_thread_replies":[{"fixItemId":"c1","threadId":"t1","explanation":"Switched the loop bound to len(items)-1 and added a regression test in foo_test.go."}]}` + "\n"
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "applied fixes", ParseStatus: "parsed", Stdout: "runtime log", Stderr: stdout}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git, AgentExecutor: agent, ValidationRunner: passValidation, AllowAutoCommit: true, AllowAutoPush: true, AllowRiskyFixes: true, Logger: fixture.logger, Now: fixture.now})

	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "fixer-worker-1", "fixer")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNextOfType() = (%#v, %v), want claimed item", claim, err)
	}
	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "success" {
		t.Fatalf("result = %#v, want success", result)
	}
	if len(github.replyCalls) != 1 {
		t.Fatalf("reply calls = %d, want 1", len(github.replyCalls))
	}
	reply := github.replyCalls[0]
	if !strings.Contains(reply.Body, "@alice") {
		t.Fatalf("reply body missing @author mention: %q", reply.Body)
	}
	if !strings.Contains(reply.Body, "Switched the loop bound") {
		t.Fatalf("reply body missing agent explanation: %q", reply.Body)
	}
	if strings.Contains(reply.Body, "please fix off-by-one") {
		t.Fatalf("reply body should drop original quote when explanation present: %q", reply.Body)
	}
}

func TestBuildFixerSummaryCommentBodyIncludesMarkerAndItems(t *testing.T) {
	t.Parallel()
	items := []fixerSummaryItem{
		{FixItem: FixItem{Type: "comment", ID: "c1", ThreadID: "t1", Author: "alice", Path: "internal/foo.go", Line: 12, URL: "https://example/threads/t1"}, Status: "resolved", Explanation: "Replaced strings.Title with cases.Title.", ReplyState: "sent"},
		{FixItem: FixItem{Type: "comment", ID: "c2", ThreadID: "t2", Author: "bob", Path: "internal/bar.go", URL: "https://example/threads/t2"}, Status: "failed", ReplyState: "failed"},
		{FixItem: FixItem{Type: "check", Name: "ci"}, Status: "check"},
	}
	got := buildFixerSummaryCommentBody("acme/looper", 42, "abcdef1234567", "abcdef1234567", items)
	if !strings.Contains(got, fixerRoundSummaryMarker("abcdef1234567")) {
		t.Fatalf("body missing round marker:\n%s", got)
	}
	if !strings.Contains(got, "abcdef1") {
		t.Fatalf("body missing short SHA:\n%s", got)
	}
	if !strings.Contains(got, "✅") || !strings.Contains(got, "@alice") {
		t.Fatalf("body missing resolved bullet for alice:\n%s", got)
	}
	if !strings.Contains(got, "⚠️") || !strings.Contains(got, "@bob") {
		t.Fatalf("body missing failed bullet for bob:\n%s", got)
	}
	if !strings.Contains(got, "Replaced strings.Title with cases.Title.") {
		t.Fatalf("body missing explanation:\n%s", got)
	}
	if !strings.Contains(got, "Failing check `ci`") {
		t.Fatalf("body missing failing check item:\n%s", got)
	}
	if !strings.Contains(got, "[thread](https://example/threads/t1)") {
		t.Fatalf("body missing thread link:\n%s", got)
	}
}

func TestFindExistingFixerSummaryCommentIDMatchesTrustedComment(t *testing.T) {
	t.Parallel()
	detail := &checkpointDetail{IssueComments: []map[string]any{
		{"id": float64(101), "body": "unrelated comment"},
		{"id": float64(202), "body": fixerRoundSummaryMarker("abcdef1234567") + "\nLooper fixer round complete\n\n<!-- looper:stamp v=1 -->", "author": map[string]any{"login": "looper"}},
	}}
	id, _ := findExistingFixerSummaryCommentID(detail, "abcdef1234567", "looper")
	if id != 202 {
		t.Fatalf("findExistingFixerSummaryCommentID() = %d, want 202", id)
	}
	if other, _ := findExistingFixerSummaryCommentID(detail, "deadbeef0000000", "looper"); other != 0 {
		t.Fatalf("findExistingFixerSummaryCommentID(other head) = %d, want 0", other)
	}
}

func TestFindExistingFixerSummaryCommentIDSkipsUntrustedMarker(t *testing.T) {
	t.Parallel()
	detail := &checkpointDetail{IssueComments: []map[string]any{
		{"id": float64(101), "body": fixerRoundSummaryMarker("abcdef1234567") + "\nspoofed marker", "author": map[string]any{"login": "someone-else"}},
		{"id": float64(202), "body": fixerRoundSummaryMarker("abcdef1234567") + "\nreal summary", "author": map[string]any{"login": "looper"}},
	}}
	id, _ := findExistingFixerSummaryCommentID(detail, "abcdef1234567", "looper")
	if id != 202 {
		t.Fatalf("findExistingFixerSummaryCommentID() = %d, want trusted comment 202", id)
	}
}

func TestFindExistingFixerSummaryCommentIDSkipsStampedCommentFromOtherAuthor(t *testing.T) {
	t.Parallel()
	detail := &checkpointDetail{IssueComments: []map[string]any{
		{"id": float64(101), "body": fixerRoundSummaryMarker("abcdef1234567") + "\nspoofed marker\n\n<!-- looper:stamp v=1 -->", "author": map[string]any{"login": "someone-else"}},
		{"id": float64(202), "body": fixerRoundSummaryMarker("abcdef1234567") + "\nreal summary\n\n<!-- looper:stamp v=1 -->", "author": map[string]any{"login": "looper"}},
	}}
	id, _ := findExistingFixerSummaryCommentID(detail, "abcdef1234567", "looper")
	if id != 202 {
		t.Fatalf("findExistingFixerSummaryCommentID() = %d, want trusted comment 202", id)
	}
}

func TestFindExistingFixerSummaryCommentIDParsesStringID(t *testing.T) {
	t.Parallel()
	detail := &checkpointDetail{IssueComments: []map[string]any{
		{"id": "202", "body": fixerRoundSummaryMarker("abcdef1234567") + "\nreal summary\n\n<!-- looper:stamp v=1 -->", "author": map[string]any{"login": "looper"}},
	}}
	id, _ := findExistingFixerSummaryCommentID(detail, "abcdef1234567", "looper")
	if id != 202 {
		t.Fatalf("findExistingFixerSummaryCommentID() = %d, want 202 from string id", id)
	}
}

func TestFindExistingFixerSummaryCommentIDParsesGraphQLIDFromURL(t *testing.T) {
	t.Parallel()
	detail := &checkpointDetail{IssueComments: []map[string]any{
		{"id": "IC_kwDOExample", "url": "https://github.com/acme/looper/pull/42#issuecomment-202", "body": fixerRoundSummaryMarker("abcdef1234567") + "\nreal summary\n\n<!-- looper:stamp v=1 -->", "author": map[string]any{"login": "looper"}},
	}}
	id, _ := findExistingFixerSummaryCommentID(detail, "abcdef1234567", "looper")
	if id != 202 {
		t.Fatalf("findExistingFixerSummaryCommentID() = %d, want 202 from issue comment URL", id)
	}
}

func TestFindExistingFixerSummaryCommentIDUsesDatabaseID(t *testing.T) {
	t.Parallel()
	detail := &checkpointDetail{IssueComments: []map[string]any{
		{"id": "IC_kwDOExample", "databaseId": float64(202), "body": fixerRoundSummaryMarker("abcdef1234567") + "\nreal summary\n\n<!-- looper:stamp v=1 -->", "author": map[string]any{"login": "looper"}},
	}}
	id, _ := findExistingFixerSummaryCommentID(detail, "abcdef1234567", "looper")
	if id != 202 {
		t.Fatalf("findExistingFixerSummaryCommentID() = %d, want 202 from databaseId", id)
	}
}

func TestIssueCommentAuthorLoginFallsBackToUser(t *testing.T) {
	t.Parallel()
	comment := map[string]any{"user": map[string]any{"login": "looper"}}
	if got := issueCommentAuthorLogin(comment); got != "looper" {
		t.Fatalf("issueCommentAuthorLogin() = %q, want looper", got)
	}
}

func TestProcessClaimedItemPostsRoundSummaryComment(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{
		listOpen: []PullRequestSummary{{Number: 42, State: "OPEN", HeadSHA: "head-1"}},
		viewResponses: []PullRequestDetail{
			{Number: 42, State: "OPEN", HeadSHA: "head-1", HeadRefName: "feature/fix-42", BaseRefName: "main", BaseSHA: "base-1", Comments: []map[string]any{{"id": "c1", "threadId": "t1", "body": "please fix off-by-one", "author": "alice", "url": "https://example/threads/t1", "path": "foo.go", "line": float64(7)}}},
			{Number: 42, State: "OPEN", HeadSHA: "head-1", HeadRefName: "feature/fix-42", BaseRefName: "main", BaseSHA: "base-1", Comments: []map[string]any{{"id": "c1", "threadId": "t1", "body": "please fix off-by-one", "author": "alice", "url": "https://example/threads/t1", "path": "foo.go", "line": float64(7)}}},
			{Number: 42, State: "OPEN", HeadSHA: "new-head", HeadRefName: "feature/fix-42", BaseRefName: "main", BaseSHA: "base-1"},
			{Number: 42, State: "OPEN", HeadSHA: "new-head", HeadRefName: "feature/fix-42", BaseRefName: "main", BaseSHA: "base-1"},
			{Number: 42, State: "OPEN", HeadSHA: "new-head", HeadRefName: "feature/fix-42", BaseRefName: "main", BaseSHA: "base-1"},
		},
	}
	git := &fakeGitGateway{
		createResult:  CreateWorktreeResult{WorktreePath: filepath.Join(t.TempDir(), "wt-42"), Branch: "feature/fix-42", HeadSHA: "base-head"},
		prepareResult: PrepareWorktreeResult{HeadSHA: "base-head", Clean: true},
		inspectResults: []InspectHeadResult{
			{HeadSHA: "base-head"},
			{HeadSHA: "new-head", NewCommitSHAs: []string{"new-head"}},
			{HeadSHA: "new-head"},
		},
	}
	stdout := `__LOOPER_RESULT__={"summary":"done","review_thread_replies":[{"fixItemId":"c1","threadId":"t1","explanation":"Capped loop bound and added regression test."}]}` + "\n"
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "applied fixes", ParseStatus: "parsed", Stdout: stdout}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git, AgentExecutor: agent, ValidationRunner: passValidation, AllowAutoCommit: true, AllowAutoPush: true, AllowRiskyFixes: true, Logger: fixture.logger, Now: fixture.now})

	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "fixer-worker-1", "fixer")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNextOfType() = (%#v, %v), want claimed item", claim, err)
	}
	if _, err := runner.ProcessClaimedItem(context.Background(), *claim); err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if len(github.createIssueComments) != 1 {
		t.Fatalf("createIssueComments calls = %d, want 1", len(github.createIssueComments))
	}
	if len(github.updateIssueComments) != 0 {
		t.Fatalf("updateIssueComments calls = %d, want 0 on first round", len(github.updateIssueComments))
	}
	body := github.createIssueComments[0].Body
	if !strings.Contains(body, "Looper fixer round complete") {
		t.Fatalf("summary body missing header:\n%s", body)
	}
	if !strings.Contains(body, "Capped loop bound") {
		t.Fatalf("summary body missing agent explanation:\n%s", body)
	}
	if !strings.Contains(body, fixerRoundSummaryMarker("new-head")) {
		t.Fatalf("summary body missing round marker:\n%s", body)
	}
	if !strings.Contains(body, "@alice") {
		t.Fatalf("summary body missing @author mention:\n%s", body)
	}
}

func TestPublishRoundSummaryCommentUpdatesExistingSummaryFromGraphQLID(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, ValidationRunner: passValidation, AllowAutoCommit: true, AllowAutoPush: true, AllowRiskyFixes: true, Logger: fixture.logger, Now: fixture.now})
	checkpoint := fixerCheckpoint{
		Detail:           &checkpointDetail{IssueComments: []map[string]any{{"id": "IC_kwDOExample", "url": "https://github.com/acme/looper/pull/42#issuecomment-202", "body": fixerRoundSummaryMarker("new-head") + "\nold summary\n\n<!-- looper:stamp v=1 -->", "author": map[string]any{"login": "looper"}}}},
		Push:             &checkpointPush{Pushed: true},
		ReconcileCommits: &checkpointReconcileCommits{FinalHeadSHA: "new-head", NewCommitSHAs: []string{"new-head"}},
		ResolvedComments: &checkpointResolvedComments{Items: []checkpointResolvedComment{{FixItemID: "c1", ThreadID: "t1", Status: "resolved", ReplyState: "sent"}}},
		FixItemsHash:     "fix-items-hash",
	}
	fixItems := []FixItem{{ID: "c1", Type: "comment", ThreadID: "t1", Author: "alice", URL: "https://example/threads/t1", Path: "foo.go", Line: 7}}

	runner.publishRoundSummaryComment(context.Background(), stepInput{Repo: "acme/looper", PRNumber: 42, Project: storage.ProjectRecord{RepoPath: t.TempDir()}}, &checkpoint, fixItems, "new-head", map[string]string{"c1": "Capped loop bound and added regression test."})

	if len(github.createIssueComments) != 0 {
		t.Fatalf("createIssueComments calls = %d, want 0 when reusing prior summary", len(github.createIssueComments))
	}
	if len(github.updateIssueComments) != 1 {
		t.Fatalf("updateIssueComments calls = %d, want 1", len(github.updateIssueComments))
	}
	if github.updateIssueComments[0].CommentID != 202 {
		t.Fatalf("updateIssueComments[0].CommentID = %d, want 202", github.updateIssueComments[0].CommentID)
	}
}

func TestProcessClaimedItemSkipsSummaryWhenNoNewCommits(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{
		listOpen: []PullRequestSummary{{Number: 42, State: "OPEN", HeadSHA: "base-head"}},
		viewResponses: []PullRequestDetail{
			{Number: 42, State: "OPEN", HeadSHA: "base-head", HeadRefName: "feature/fix-42", BaseRefName: "main", BaseSHA: "base-1", Comments: []map[string]any{{"id": "c1", "threadId": "t1", "body": "please fix"}}},
			{Number: 42, State: "OPEN", HeadSHA: "base-head", HeadRefName: "feature/fix-42", BaseRefName: "main", BaseSHA: "base-1", Comments: []map[string]any{{"id": "c1", "threadId": "t1", "body": "please fix"}}},
		},
	}
	git := &fakeGitGateway{
		createResult:  CreateWorktreeResult{WorktreePath: filepath.Join(t.TempDir(), "wt-42"), Branch: "feature/fix-42", HeadSHA: "base-head"},
		prepareResult: PrepareWorktreeResult{HeadSHA: "base-head", Clean: true},
		inspectResults: []InspectHeadResult{
			{HeadSHA: "base-head"},
			{HeadSHA: "base-head"},
			{HeadSHA: "base-head"},
		},
	}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "blocked before editing because gh could not validate PR metadata", ParseStatus: "parsed"}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git, AgentExecutor: agent, ValidationRunner: passValidation, AllowAutoCommit: true, AllowAutoPush: true, AllowRiskyFixes: true, Logger: fixture.logger, Now: fixture.now})
	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "fixer-worker-1", "fixer")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNextOfType() = (%#v, %v)", claim, err)
	}
	if _, err := runner.ProcessClaimedItem(context.Background(), *claim); err != nil {
		// expected to fail in resolve-comments because no new commits; we don't care about the error here.
		_ = err
	}
	if len(github.createIssueComments) != 0 {
		t.Fatalf("createIssueComments calls = %d, want 0 when no new commits", len(github.createIssueComments))
	}
}
