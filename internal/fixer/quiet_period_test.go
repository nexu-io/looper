package fixer

import (
	"context"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/eventlog"
	"github.com/nexu-io/looper/internal/storage"
)

func fixerConfigWithQuiet(t *testing.T, quiet int) *config.Config {
	t.Helper()
	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Roles.Fixer.Behavior.Loop.QuietPeriodSeconds = quiet
	return &cfg
}

func TestDiscoverDebouncesNewFixableWork(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{
		viewResponses: []PullRequestDetail{
			{Number: 42, State: "OPEN", HeadSHA: "head-42", Comments: []map[string]any{{"id": "c1", "threadId": "t1", "body": "please fix"}}},
		},
	}
	runner := New(Options{
		DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{},
		AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now,
		CustomInstructions: fixerConfigWithQuiet(t, 60),
	})

	result, err := runner.DiscoverPullRequest(context.Background(), TargetedDiscoveryInput{ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42})
	if err != nil {
		t.Fatalf("DiscoverPullRequest() error = %v", err)
	}
	if len(result.QueueItems) != 1 {
		t.Fatalf("len(QueueItems) = %d, want 1", len(result.QueueItems))
	}
	wantAvailableAt := eventlog.FormatJavaScriptISOString(fixture.now().Add(60 * time.Second))
	if result.QueueItems[0].AvailableAt != wantAvailableAt {
		t.Fatalf("AvailableAt = %q, want delayed %q", result.QueueItems[0].AvailableAt, wantAvailableAt)
	}
	// Must not be immediately runnable relative to now.
	if !isoTimeAfter(result.QueueItems[0].AvailableAt, fixture.nowISO()) {
		t.Fatalf("AvailableAt = %q should be after now %q", result.QueueItems[0].AvailableAt, fixture.nowISO())
	}
}

func TestDiscoverQuietZeroMatchesImmediateEnqueue(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{
		viewResponses: []PullRequestDetail{
			{Number: 42, State: "OPEN", HeadSHA: "head-42", Comments: []map[string]any{{"id": "c1", "threadId": "t1", "body": "please fix"}}},
		},
	}
	runner := New(Options{
		DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{},
		AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now,
		CustomInstructions: fixerConfigWithQuiet(t, 0),
	})

	result, err := runner.DiscoverPullRequest(context.Background(), TargetedDiscoveryInput{ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42})
	if err != nil {
		t.Fatalf("DiscoverPullRequest() error = %v", err)
	}
	if len(result.QueueItems) != 1 {
		t.Fatalf("len(QueueItems) = %d, want 1", len(result.QueueItems))
	}
	if result.QueueItems[0].AvailableAt != fixture.nowISO() {
		t.Fatalf("AvailableAt = %q, want immediate %q", result.QueueItems[0].AvailableAt, fixture.nowISO())
	}
}

func TestDiscoverUnchangedSetDoesNotExtendForever(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	detail := PullRequestDetail{Number: 42, State: "OPEN", HeadSHA: "head-42", Comments: []map[string]any{{"id": "c1", "threadId": "t1", "body": "please fix"}}}
	github := &fakeGitHubGateway{viewResponses: []PullRequestDetail{detail, detail, detail}}
	runner := New(Options{
		DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{},
		AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now,
		CustomInstructions: fixerConfigWithQuiet(t, 120),
	})

	first, err := runner.DiscoverPullRequest(context.Background(), TargetedDiscoveryInput{ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42})
	if err != nil {
		t.Fatalf("first DiscoverPullRequest() error = %v", err)
	}
	if len(first.QueueItems) != 1 {
		t.Fatalf("first len = %d, want 1", len(first.QueueItems))
	}
	firstAvailableAt := first.QueueItems[0].AvailableAt

	fixture.advance(30 * time.Second)
	second, err := runner.DiscoverPullRequest(context.Background(), TargetedDiscoveryInput{ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42})
	if err != nil {
		t.Fatalf("second DiscoverPullRequest() error = %v", err)
	}
	if len(second.QueueItems) != 1 {
		t.Fatalf("second len = %d, want 1", len(second.QueueItems))
	}
	if second.QueueItems[0].AvailableAt != firstAvailableAt {
		t.Fatalf("unchanged set AvailableAt = %q, want original %q (must not extend forever)", second.QueueItems[0].AvailableAt, firstAvailableAt)
	}
	items, err := fixture.repos.Queue.List(context.Background())
	if err != nil {
		t.Fatalf("Queue.List() error = %v", err)
	}
	if len(items) != 1 || items[0].AvailableAt != firstAvailableAt {
		t.Fatalf("queue = %#v, want single item at %q", items, firstAvailableAt)
	}
}

func TestDiscoverChangedSetResetsQuietWindow(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	firstDetail := PullRequestDetail{Number: 42, State: "OPEN", HeadSHA: "head-42", Comments: []map[string]any{{"id": "c1", "threadId": "t1", "body": "please fix"}}}
	secondDetail := PullRequestDetail{Number: 42, State: "OPEN", HeadSHA: "head-42", Comments: []map[string]any{
		{"id": "c1", "threadId": "t1", "body": "please fix"},
		{"id": "c2", "threadId": "t2", "body": "also this"},
	}}
	github := &fakeGitHubGateway{viewResponses: []PullRequestDetail{firstDetail, secondDetail}}
	runner := New(Options{
		DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{},
		AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now,
		CustomInstructions: fixerConfigWithQuiet(t, 120),
	})

	first, err := runner.DiscoverPullRequest(context.Background(), TargetedDiscoveryInput{ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42})
	if err != nil {
		t.Fatalf("first DiscoverPullRequest() error = %v", err)
	}
	if len(first.QueueItems) != 1 {
		t.Fatalf("first len = %d, want 1", len(first.QueueItems))
	}
	firstAvailableAt := first.QueueItems[0].AvailableAt
	firstDedupe := first.QueueItems[0].DedupeKey

	fixture.advance(30 * time.Second)
	second, err := runner.DiscoverPullRequest(context.Background(), TargetedDiscoveryInput{ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42})
	if err != nil {
		t.Fatalf("second DiscoverPullRequest() error = %v", err)
	}
	if len(second.QueueItems) != 1 {
		t.Fatalf("second len = %d, want 1 (loop-scoped coalesce)", len(second.QueueItems))
	}
	extended := eventlog.FormatJavaScriptISOString(fixture.now().Add(120 * time.Second))
	if second.QueueItems[0].AvailableAt != extended {
		t.Fatalf("AvailableAt after signal change = %q, want extended %q (was %q)", second.QueueItems[0].AvailableAt, extended, firstAvailableAt)
	}
	if second.QueueItems[0].DedupeKey == firstDedupe {
		t.Fatalf("dedupe key unchanged after hash change: %q", firstDedupe)
	}
	items, err := fixture.repos.Queue.List(context.Background())
	if err != nil {
		t.Fatalf("Queue.List() error = %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("len(queue) = %d, want 1 coalesced item (hash change must not bypass loop-scoped debounce)", len(items))
	}
	if items[0].AvailableAt != extended {
		t.Fatalf("coalesced AvailableAt = %q, want %q", items[0].AvailableAt, extended)
	}
}

func TestDiscoverHeadSHAChangeResetsQuietWindow(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	// Same fix-item contents; only head SHA advances (new commit while review set is unchanged).
	firstDetail := PullRequestDetail{Number: 42, State: "OPEN", HeadSHA: "head-old", Comments: []map[string]any{{"id": "c1", "threadId": "t1", "body": "please fix"}}}
	secondDetail := PullRequestDetail{Number: 42, State: "OPEN", HeadSHA: "head-new", Comments: []map[string]any{{"id": "c1", "threadId": "t1", "body": "please fix"}}}
	github := &fakeGitHubGateway{viewResponses: []PullRequestDetail{firstDetail, secondDetail}}
	runner := New(Options{
		DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{},
		AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now,
		CustomInstructions: fixerConfigWithQuiet(t, 120),
	})

	first, err := runner.DiscoverPullRequest(context.Background(), TargetedDiscoveryInput{ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42})
	if err != nil {
		t.Fatalf("first DiscoverPullRequest() error = %v", err)
	}
	if len(first.QueueItems) != 1 {
		t.Fatalf("first len = %d, want 1", len(first.QueueItems))
	}
	firstAvailableAt := first.QueueItems[0].AvailableAt
	firstDedupe := first.QueueItems[0].DedupeKey

	fixture.advance(30 * time.Second)
	second, err := runner.DiscoverPullRequest(context.Background(), TargetedDiscoveryInput{ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42})
	if err != nil {
		t.Fatalf("second DiscoverPullRequest() error = %v", err)
	}
	if len(second.QueueItems) != 1 {
		t.Fatalf("second len = %d, want 1 (loop-scoped coalesce)", len(second.QueueItems))
	}
	extended := eventlog.FormatJavaScriptISOString(fixture.now().Add(120 * time.Second))
	if second.QueueItems[0].AvailableAt != extended {
		t.Fatalf("AvailableAt after head SHA change = %q, want extended %q (was %q)", second.QueueItems[0].AvailableAt, extended, firstAvailableAt)
	}
	if second.QueueItems[0].DedupeKey == firstDedupe {
		t.Fatalf("dedupe key unchanged after head SHA change: %q", firstDedupe)
	}
	if got := headShaFromQueueItem(second.QueueItems[0]); got != "head-new" {
		t.Fatalf("queued headSha = %q, want head-new", got)
	}
	items, err := fixture.repos.Queue.List(context.Background())
	if err != nil {
		t.Fatalf("Queue.List() error = %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("len(queue) = %d, want 1 coalesced item", len(items))
	}
	if items[0].AvailableAt != extended {
		t.Fatalf("coalesced AvailableAt = %q, want %q", items[0].AvailableAt, extended)
	}
	if got := headShaFromQueueItem(items[0]); got != "head-new" {
		t.Fatalf("persisted headSha = %q, want head-new", got)
	}
}

func TestDiscoverMidRunPendingAppliesQuietOnScheduleAfterRun(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	runner := New(Options{
		DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: &fakeGitHubGateway{}, Git: &fakeGitGateway{},
		AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now,
		CustomInstructions: fixerConfigWithQuiet(t, 90),
	})

	nowISO := fixture.nowISO()
	repo := "acme/looper"
	prNumber := int64(84)
	loopID := "loop_fixer_quiet_pending"
	loopTarget := buildPullRequestTargetID(repo, prNumber)
	metadata := mustMarshalJSON(map[string]any{
		"followUpdates": true,
		"pendingFixerRediscovery": map[string]any{
			"headSha": "head-84", "fixItemsStateHash": "state-84", "unresolvedThreadIds": []string{"t1"}, "recordedAt": nowISO,
		},
	})
	loop := storage.LoopRecord{
		ID: loopID, Seq: 1, ProjectID: "project_1", Type: "fixer", TargetType: "pull_request",
		TargetID: &loopTarget, Repo: &repo, PRNumber: &prNumber, Status: "waiting",
		MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	scheduled, err := runner.schedulePendingRediscoveryAfterRun(context.Background(), loop, repo, prNumber)
	if err != nil {
		t.Fatalf("schedulePendingRediscoveryAfterRun() error = %v", err)
	}
	if !scheduled {
		t.Fatal("schedulePendingRediscoveryAfterRun() = false, want true")
	}
	queueItem, err := fixture.repos.Queue.FindActiveByLoopID(context.Background(), loopID)
	if err != nil {
		t.Fatalf("FindActiveByLoopID() error = %v", err)
	}
	if queueItem == nil || queueItem.Status != "queued" {
		t.Fatalf("queueItem = %#v, want queued follow-up", queueItem)
	}
	wantAvailableAt := eventlog.FormatJavaScriptISOString(fixture.now().Add(90 * time.Second))
	if queueItem.AvailableAt != wantAvailableAt {
		t.Fatalf("post-run AvailableAt = %q, want quiet-delayed %q", queueItem.AvailableAt, wantAvailableAt)
	}
}

func TestProjectQuietPeriodOverrideAppliesOnDiscovery(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Roles.Fixer.Behavior.Loop.QuietPeriodSeconds = 30
	projectQuiet := 180
	repoPath := "/tmp/demo-repo"
	cfg.Projects = []config.ProjectRefConfig{{
		ID: "project_1", Name: "Demo", RepoPath: repoPath,
		Roles: &config.PartialRoleConfigs{
			Fixer: &config.PartialFixerRoleConfig{
				Behavior: &config.PartialFixerBehaviorConfig{
					Loop: &config.PartialFixerLoopConfig{QuietPeriodSeconds: &projectQuiet},
				},
			},
		},
	}}
	github := &fakeGitHubGateway{
		viewResponses: []PullRequestDetail{
			{Number: 42, State: "OPEN", HeadSHA: "head-42", Comments: []map[string]any{{"id": "c1", "threadId": "t1", "body": "please fix"}}},
		},
	}
	runner := New(Options{
		DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{},
		AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now,
		CustomInstructions: &cfg,
	})

	result, err := runner.DiscoverPullRequest(context.Background(), TargetedDiscoveryInput{ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42})
	if err != nil {
		t.Fatalf("DiscoverPullRequest() error = %v", err)
	}
	if len(result.QueueItems) != 1 {
		t.Fatalf("len(QueueItems) = %d, want 1", len(result.QueueItems))
	}
	want := eventlog.FormatJavaScriptISOString(fixture.now().Add(180 * time.Second))
	if result.QueueItems[0].AvailableAt != want {
		t.Fatalf("AvailableAt = %q, want project override %q", result.QueueItems[0].AvailableAt, want)
	}
}
