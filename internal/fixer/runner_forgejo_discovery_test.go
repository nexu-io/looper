package fixer

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/forge"
	"github.com/nexu-io/looper/internal/storage"
)

func TestForgejoAutoDiscoveryUsesReviewerSummaryWithoutReadingNativeComments(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	detail := forgejoDiscoveryDetail(t, "head-1", 1)
	github := &fakeGitHubGateway{
		currentUser:    "looper",
		listOpen:       []PullRequestSummary{{Number: 42, State: "OPEN", HeadSHA: "head-1", Author: "looper"}},
		viewResponses:  []PullRequestDetail{detail},
		nativeComments: []NativeReviewComment{{ProviderCommentID: 101, Body: "Rename this helper", Author: "alice", ObservedFingerprint: NativeReviewCommentFingerprint(101, "u1"), ResolverPresent: true}},
		listNativeErr:  errors.New("native API must not be called during automatic discovery"),
	}
	cfg := forgejoFixerDiscoveryConfig(t, fixture)
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now, CustomInstructions: cfg})

	result, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	if len(result.QueueItems) != 1 || len(result.CreatedLoopIDs) != 1 {
		t.Fatalf("result = %#v, want one automatic Forgejo fixer loop and queue item", result)
	}
	if len(github.listCalls) != 1 || github.listCalls[0].Author != "looper" {
		t.Fatalf("list calls = %#v, want current-user author policy preserved", github.listCalls)
	}
	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("duplicate DiscoverPullRequests() error = %v", err)
	}
	queueItems, err := fixture.repos.Queue.List(context.Background())
	if err != nil {
		t.Fatalf("Queue.List() error = %v", err)
	}
	if len(queueItems) != 1 {
		t.Fatalf("queue items = %#v, duplicate ticks must keep one active fixer item", queueItems)
	}
}

func TestForgejoAutoDiscoverySuppressesConsumedReviewerRoundUntilHeadChanges(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		liveHead   string
		fixedHead  string
		result     forge.FixerItemResult
		partial    bool
		wantQueued int
	}{
		{name: "fixed same head", liveHead: "head-1", fixedHead: "head-1", result: forge.FixerItemResultFixed, wantQueued: 0},
		{name: "declined same head", liveHead: "head-1", fixedHead: "head-1", result: forge.FixerItemResultDeclined, wantQueued: 0},
		{name: "deferred same head", liveHead: "head-1", fixedHead: "head-1", result: forge.FixerItemResultDeferred, wantQueued: 0},
		{name: "partial same head", liveHead: "head-1", fixedHead: "head-1", result: forge.FixerItemResultFixed, partial: true, wantQueued: 1},
		{name: "changed head", liveHead: "head-2", fixedHead: "head-1", result: forge.FixerItemResultFixed, wantQueued: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newRunnerFixture(t)
			items := []forge.ReviewItem{{ReviewItemID: "R-001", Status: forge.ReviewItemStatusOpen, Title: "Fix parsing", Body: "Parser must fail fast.", LastSeenRoundID: 3}}
			if tc.partial {
				items = append(items, forge.ReviewItem{ReviewItemID: "R-002", Status: forge.ReviewItemStatusOpen, Title: "Fix rendering", Body: "Renderer must preserve results.", LastSeenRoundID: 3})
			}
			detail := forgejoDiscoveryDetailWithItems(t, tc.liveHead, 3, items)
			fixerSummary := forge.NewFixerSummary(4, 3, []forge.FixerResult{{ReviewItemID: "R-001", Result: tc.result, Explanation: "Recorded the decision."}})
			fixerSummary.ObservedHeadSHA = tc.fixedHead
			marker, err := forge.RenderFixerSummary(fixerSummary)
			if err != nil {
				t.Fatalf("RenderFixerSummary() error = %v", err)
			}
			detail.IssueComments = append(detail.IssueComments, map[string]any{"id": int64(2), "body": marker, "author": map[string]any{"login": "looper"}})
			github := &fakeGitHubGateway{currentUser: "looper", listOpen: []PullRequestSummary{{Number: 42, State: "OPEN", HeadSHA: tc.liveHead, Author: "looper"}}, viewResponses: []PullRequestDetail{detail}}
			cfg := forgejoFixerDiscoveryConfig(t, fixture)
			runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now, CustomInstructions: cfg})

			result, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
			if err != nil {
				t.Fatalf("DiscoverPullRequests() error = %v", err)
			}
			if len(result.QueueItems) != tc.wantQueued {
				t.Fatalf("QueueItems = %#v, want %d", result.QueueItems, tc.wantQueued)
			}
		})
	}
}

func TestForgejoAutoDiscoveryIgnoresNativeCommentsRegardlessOfResolverField(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name            string
		resolverPresent bool
		resolved        bool
	}{
		{name: "resolver null", resolverPresent: true},
		{name: "resolver absent", resolverPresent: false},
		{name: "already resolved", resolverPresent: true, resolved: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newRunnerFixture(t)
			detail := PullRequestDetail{Number: 42, State: "OPEN", HeadSHA: "head-1", HeadRefName: "feature/fix", BaseRefName: "main", BaseSHA: "base-1", Author: "looper"}
			github := &fakeGitHubGateway{
				currentUser:    "looper",
				listOpen:       []PullRequestSummary{{Number: 42, State: "OPEN", HeadSHA: "head-1", Author: "looper"}},
				viewResponses:  []PullRequestDetail{detail},
				nativeComments: []NativeReviewComment{{ProviderCommentID: 101, Body: "Fix this", Author: "alice", ObservedFingerprint: NativeReviewCommentFingerprint(101, "u1"), ResolverPresent: tc.resolverPresent, IsResolved: tc.resolved}},
				listNativeErr:  errors.New("resolver response fields are not API capability authority"),
			}
			cfg := forgejoFixerDiscoveryConfig(t, fixture)
			runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now, CustomInstructions: cfg})

			result, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
			if err != nil {
				t.Fatalf("DiscoverPullRequests() error = %v", err)
			}
			if len(result.QueueItems) != 0 {
				t.Fatalf("QueueItems = %#v, native comments must not authorize automatic Forgejo fixer work", result.QueueItems)
			}
		})
	}
}

func TestForgejoAutomaticCollectDoesNotAttachNativeComments(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	cfg := forgejoFixerDiscoveryConfig(t, fixture)
	detail := forgejoDiscoveryDetail(t, "head-1", 1)
	untrusted := cloneObjectSlice(detail.IssueComments)[0]
	untrusted["id"] = int64(2)
	untrusted["author"] = map[string]any{"login": "mallory"}
	detail.IssueComments = append(detail.IssueComments, untrusted)
	github := &fakeGitHubGateway{currentUser: "looper", nativeComments: []NativeReviewComment{
		{ProviderCommentID: 101, Body: "Fix this", Author: "alice", ObservedFingerprint: NativeReviewCommentFingerprint(101, "u1"), ResolverPresent: true},
		{ProviderCommentID: 102, Body: "Unsupported", Author: "bob", ObservedFingerprint: NativeReviewCommentFingerprint(102, "u2"), ResolverPresent: false},
	}, listNativeErr: errors.New("automatic collect must not read native comments")}
	runner := New(Options{GitHub: github, CustomInstructions: cfg})
	project, err := fixture.repos.Projects.GetByID(context.Background(), "project_1")
	if err != nil || project == nil {
		t.Fatalf("Projects.GetByID() = (%#v, %v)", project, err)
	}
	checkpoint, err := runner.runCollectFixesStep(context.Background(), stepInput{Project: *project, Loop: storage.LoopRecord{ProjectID: "project_1"}, Repo: "acme/looper", PRNumber: 42, Checkpoint: fixerCheckpoint{Detail: pullRequestCheckpointDetail(detail)}})
	if err != nil {
		t.Fatalf("runCollectFixesStep() error = %v", err)
	}
	if len(checkpoint.FixItems) != 1 || checkpoint.FixItems[0].ID != "R-001" {
		t.Fatalf("FixItems = %#v, want only the trusted reviewer summary item", checkpoint.FixItems)
	}
}

func TestForgejoAutomaticCollectSkipsConsumedReviewerRound(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		liveHead  string
		fixedHead string
		partial   bool
		wantItems int
	}{
		{name: "same head", liveHead: "head-1", fixedHead: "head-1", wantItems: 0},
		{name: "partial same head", liveHead: "head-1", fixedHead: "head-1", partial: true, wantItems: 2},
		{name: "changed head", liveHead: "head-2", fixedHead: "head-1", wantItems: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newRunnerFixture(t)
			cfg := forgejoFixerDiscoveryConfig(t, fixture)
			items := []forge.ReviewItem{{ReviewItemID: "R-001", Status: forge.ReviewItemStatusOpen, Title: "Fix parsing", Body: "Parser must fail fast.", LastSeenRoundID: 3}}
			if tc.partial {
				items = append(items, forge.ReviewItem{ReviewItemID: "R-002", Status: forge.ReviewItemStatusOpen, Title: "Fix rendering", Body: "Renderer must preserve results.", LastSeenRoundID: 3})
			}
			detail := forgejoDiscoveryDetailWithItems(t, tc.liveHead, 3, items)
			fixerSummary := forge.NewFixerSummary(4, 3, []forge.FixerResult{{ReviewItemID: "R-001", Result: forge.FixerItemResultFixed, Explanation: "Recorded the decision."}})
			fixerSummary.ObservedHeadSHA = tc.fixedHead
			marker, err := forge.RenderFixerSummary(fixerSummary)
			if err != nil {
				t.Fatalf("RenderFixerSummary() error = %v", err)
			}
			detail.IssueComments = append(detail.IssueComments, map[string]any{"id": int64(2), "body": marker, "author": map[string]any{"login": "looper"}})
			github := &fakeGitHubGateway{currentUser: "looper"}
			runner := New(Options{GitHub: github, CustomInstructions: cfg})
			project, err := fixture.repos.Projects.GetByID(context.Background(), "project_1")
			if err != nil || project == nil {
				t.Fatalf("Projects.GetByID() = (%#v, %v)", project, err)
			}

			checkpoint, err := runner.runCollectFixesStep(context.Background(), stepInput{Project: *project, Loop: storage.LoopRecord{ProjectID: "project_1"}, Repo: "acme/looper", PRNumber: 42, Checkpoint: fixerCheckpoint{Detail: pullRequestCheckpointDetail(detail)}})
			if err != nil {
				t.Fatalf("runCollectFixesStep() error = %v", err)
			}
			if len(checkpoint.FixItems) != tc.wantItems {
				t.Fatalf("FixItems = %#v, want %d", checkpoint.FixItems, tc.wantItems)
			}
			if tc.wantItems == 0 && checkpoint.SkipReason == "" {
				t.Fatal("SkipReason is empty for consumed Reviewer Summary")
			}
		})
	}
}

func TestForgejoAutoDiscoveryFailsLoudOnDuplicateReviewerSummaryAuthority(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	detail := forgejoDiscoveryDetail(t, "head-1", 1)
	duplicate := cloneObjectSlice(detail.IssueComments)[0]
	duplicate["id"] = int64(2)
	detail.IssueComments = append(detail.IssueComments, duplicate)
	github := &fakeGitHubGateway{currentUser: "looper", listOpen: []PullRequestSummary{{Number: 42, State: "OPEN", HeadSHA: "head-1", Author: "looper"}}, viewResponses: []PullRequestDetail{detail}}
	cfg := forgejoFixerDiscoveryConfig(t, fixture)
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now, CustomInstructions: cfg})

	_, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err == nil || !strings.Contains(err.Error(), "duplicated") {
		t.Fatalf("DiscoverPullRequests() error = %v, want duplicate Reviewer Summary authority failure", err)
	}
}

func TestForgejoAutoDiscoveryIgnoresUntrustedSummaryMarker(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	detail := forgejoDiscoveryDetail(t, "head-1", 1)
	detail.Labels = []string{"looper:fix"}
	detail.IssueComments[0]["author"] = map[string]any{"login": "mallory"}
	github := &fakeGitHubGateway{currentUser: "looper", listOpen: []PullRequestSummary{{Number: 42, State: "OPEN", HeadSHA: "head-1", Author: "looper", Labels: []string{"looper:fix"}}}, viewResponses: []PullRequestDetail{detail}}
	cfg := forgejoFixerDiscoveryConfig(t, fixture)
	cfg.Roles.Fixer.Triggers.Labels = []string{"looper:fix"}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now, CustomInstructions: cfg})

	result, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	if len(result.QueueItems) != 0 {
		t.Fatalf("QueueItems = %#v, label and untrusted summary must not invent repair authority", result.QueueItems)
	}
}

func forgejoDiscoveryDetail(t *testing.T, head string, reviewRound int) PullRequestDetail {
	t.Helper()
	return forgejoDiscoveryDetailWithItems(t, head, reviewRound, []forge.ReviewItem{{ReviewItemID: "R-001", Status: forge.ReviewItemStatusOpen, Title: "Fix parsing", Body: "Parser must fail fast.", LastSeenRoundID: reviewRound}})
}

func forgejoDiscoveryDetailWithItems(t *testing.T, head string, reviewRound int, items []forge.ReviewItem) PullRequestDetail {
	t.Helper()
	summary := forge.NewReviewerSummary(reviewRound, items)
	marker, err := forge.RenderReviewerSummary(summary)
	if err != nil {
		t.Fatalf("RenderReviewerSummary() error = %v", err)
	}
	return PullRequestDetail{Number: 42, State: "OPEN", HeadSHA: head, HeadRefName: "feature/fix", BaseRefName: "main", BaseSHA: "base-1", Author: "looper", IssueComments: []map[string]any{{"id": int64(1), "body": marker, "author": map[string]any{"login": "looper"}}}}
}

func forgejoFixerDiscoveryConfig(t *testing.T, fixture *runnerFixture) *config.Config {
	t.Helper()
	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	tokenEnv := "FORGEJO_TOKEN"
	cfg.Providers = []config.ProviderConfig{{ID: "forgejo", Kind: config.ProviderKindForgejo, BaseURL: "https://forgejo.test", TokenEnv: &tokenEnv}}
	cfg.Projects = []config.ProjectRefConfig{{ID: "project_1", Name: "Looper", Provider: "forgejo", Repo: "acme/looper", RepoPath: fixtureProjectPath(t, fixture)}}
	return &cfg
}

func fixtureProjectPath(t *testing.T, fixture *runnerFixture) string {
	t.Helper()
	project, err := fixture.repos.Projects.GetByID(context.Background(), "project_1")
	if err != nil || project == nil {
		t.Fatalf("Projects.GetByID() = (%#v, %v)", project, err)
	}
	return project.RepoPath
}
