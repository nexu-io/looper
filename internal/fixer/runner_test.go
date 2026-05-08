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
	if result.Status != "failed" || result.FailureKind != FailureManualIntervention || !contains(result.Summary, "produced no new commits") {
		t.Fatalf("result = %#v, want manual intervention failure for no-op repair", result)
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
	if checkpoint.ResolvedComments != nil {
		t.Fatalf("checkpoint.ResolvedComments = %#v, want unresolved comments left untouched", checkpoint.ResolvedComments)
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

func TestRunResolveCommentsStepBlocksWithoutVerifiedPushEvidence(t *testing.T) {
	t.Parallel()

	runner := New(Options{GitHub: &fakeGitHubGateway{viewResponses: []PullRequestDetail{{
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
	}}}})
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

	_, err := runner.runResolveCommentsStep(context.Background(), stepInput{
		Project:    storage.ProjectRecord{RepoPath: t.TempDir()},
		Repo:       "acme/looper",
		PRNumber:   42,
		Checkpoint: checkpoint,
	})
	if err == nil {
		t.Fatal("runResolveCommentsStep() error = nil, want manual intervention")
	}
	var loopErr *loopError
	if !errors.As(err, &loopErr) {
		t.Fatalf("error = %T, want *loopError", err)
	}
	if loopErr.kind != FailureManualIntervention {
		t.Fatalf("loopErr.kind = %v, want %v", loopErr.kind, FailureManualIntervention)
	}
	if !contains(loopErr.Error(), "produced no new commits") {
		t.Fatalf("error = %q, want no-new-commits message", loopErr.Error())
	}
}

func TestRunResolveCommentsStepChecksHeadDriftBeforeNoFixBlock(t *testing.T) {
	t.Parallel()

	github := &fakeGitHubGateway{viewResponses: []PullRequestDetail{
		{
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
	checkpoint := fixerCheckpoint{
		FixItems:   []FixItem{{Type: "comment", ID: "c1", ThreadID: "t1", Summary: "please fix"}},
		Validation: &ValidationResult{Passed: true, Summary: "ok"},
		Push:       &checkpointPush{Pushed: false, Branch: "feature/fix-42", Remote: "origin", SkippedReason: "No new commits to push"},
		ReconcileCommits: &checkpointReconcileCommits{
			BaseHeadSHA:      "base-head",
			FinalHeadSHA:     "base-head",
			NewCommitSHAs:    nil,
			WorkingTreeClean: true,
		},
	}

	_, err := runner.runResolveCommentsStep(context.Background(), stepInput{
		Project:    storage.ProjectRecord{RepoPath: t.TempDir()},
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
	if github.viewIndex < 2 {
		t.Fatalf("github.viewIndex = %d, want live refresh plus head check before no-fix block", github.viewIndex)
	}
	if len(github.resolveCalls) != 0 {
		t.Fatalf("resolve calls = %d, want none on stale head", len(github.resolveCalls))
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

func TestProcessClaimedItemTreatsAgentSetupFailureAsManualIntervention(t *testing.T) {
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
	if result.Status != "failed" || result.FailureKind != FailureManualIntervention || !contains(result.Summary, "requires a newer version") {
		t.Fatalf("result = %#v, want manual_intervention with real agent error", result)
	}
	queue, err := fixture.repos.Queue.GetByID(context.Background(), claim.ID)
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	if queue == nil || queue.Status != string(FailureManualIntervention) || queue.LastErrorKind == nil || *queue.LastErrorKind != string(FailureManualIntervention) {
		t.Fatalf("queue = %#v, want terminal manual_intervention failure", queue)
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
	if result.Status != "skipped" || !contains(result.Summary, "Auto push disabled") {
		t.Fatalf("result = %#v, want skipped auto-push summary", result)
	}
	if len(git.pushCalls) != 0 {
		t.Fatalf("len(git.pushCalls) = %d, want 0", len(git.pushCalls))
	}
	queue, err := fixture.repos.Queue.GetByID(context.Background(), claim.ID)
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	if queue == nil || queue.Status != "completed" {
		t.Fatalf("queue = %#v, want completed", queue)
	}
	run, err := fixture.repos.Runs.GetByID(context.Background(), result.RunID)
	if err != nil {
		t.Fatalf("Runs.GetByID() error = %v", err)
	}
	if run == nil || run.LastCompletedStep == nil || *run.LastCompletedStep != string(stepPush) {
		t.Fatalf("run = %#v, want lastCompletedStep=push", run)
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
	currentUser      string
	listOpen         []PullRequestSummary
	listOpenByLabel  map[string][]PullRequestSummary
	listCalls        []ListOpenPullRequestsInput
	viewResponses    []PullRequestDetail
	viewIndex        int
	resolveCalls     []ResolveReviewThreadInput
	addLabelCalls    []PullRequestLabelsInput
	removeLabelCalls []PullRequestLabelsInput
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
	}
	if result.Branch == "" {
		result.Branch = input.Branch
	}
	if result.HeadSHA == "" {
		result.HeadSHA = "base-head"
	}
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
