package reviewer

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/infra/shell"
	"github.com/nexu-io/looper/internal/networkpolicy"
	"github.com/nexu-io/looper/internal/storage"
)

func TestDiscoverPullRequestsRequeuesLooperEngagedFollowUpAfterCurrentHeadSkip(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		recovered, forgejo, self bool
	}{
		{"missing publication", false, false, false},
		{"already recovered publication", true, false, false},
		{"Forgejo missing publication", false, true, false},
		{"Forgejo already recovered publication", true, true, false},
		{"Forgejo self blocking COMMENT", false, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fixture := newRunnerFixture(t)
			const loopID = "loop_follow_engaged_without_metadata"
			github := &fakeGitHubGateway{
				currentLogin:   "bob",
				reviewRequests: []string{},
				viewHeadSHA:    "new-head",
				reviews: []map[string]any{{
					"author": map[string]any{"login": "bob"},
					"commit": map[string]any{"oid": "old-head"},
					"state":  "CHANGES_REQUESTED",
					"body":   "<!-- looper:review id=reviewer:" + loopID + " head=old-head outcome=blocking -->",
				}},
			}
			agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "No actionable findings", Stdout: `__LOOPER_RESULT__={"summary":"posted clean follow-up review"}`, ParseStatus: "parsed"}}}
			options := Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, DiscoveryPolicy: DiscoveryPolicy{AutoDiscovery: true, IncludeDrafts: false, RequireReviewRequest: true, Labels: []string{}, LabelMode: config.LabelModeAll}, ReviewEvents: config.ReviewerReviewEventsConfig{Clean: config.ReviewerReviewEventComment, Blocking: config.ReviewerReviewEventRequestChanges}, LoopConfig: testReviewerLoopConfig()}
			if tc.forgejo {
				cfg, err := config.DefaultConfig(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				cfg.Providers = []config.ProviderConfig{{ID: "fj", Kind: config.ProviderKindForgejo, BaseURL: "https://code.example", TokenEnv: stringPtr("FORGEJO_TOKEN")}}
				cfg.Projects = []config.ProjectRefConfig{{ID: "project_1", Provider: "fj", Repo: "acme/looper"}}
				cfg.Roles.Reviewer.Discovery.AutoDiscovery = true
				cfg.Roles.Reviewer.Discovery.Triggers.RequireReviewRequest = true
				cfg.Roles.Reviewer.Discovery.Triggers.Labels = []string{}
				cfg.Roles.Reviewer.Discovery.Triggers.EnableSelfReview = tc.self
				cfg.Roles.Reviewer.Behavior.ReviewEvents = options.ReviewEvents
				cfg.Roles.Reviewer.Behavior.Loop = testReviewerLoopConfig()
				options.CustomInstructions = &cfg
				github.comments = []map[string]any{{"body": "Old finding, explicitly fixed by the fixer", "isResolved": false}}
				if tc.self {
					github.author = "bob"
					github.reviews[0]["state"] = "COMMENTED"
				}
			}
			runner := New(options)
			nowISO := fixture.nowISO()
			repo := "acme/looper"
			prNumber := int64(42)
			metadata := `{"followUpdates":true,"lastFilterSkip":{"kind":"not_requested","headSha":"new-head","reviewerLogin":"bob"},"loop":{"enabled":true,"iterationCount":1,"iterationsByHead":{"old-head":1}}}`
			if tc.recovered {
				metadata = strings.Replace(metadata, `"followUpdates":true`, `"followUpdates":true,"lastPublishedHeadSha":"old-head"`, 1)
			}
			loop := storage.LoopRecord{ID: loopID, Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: "completed", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}
			if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
				t.Fatalf("Loops.Upsert() error = %v", err)
			}

			result, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: repo})
			if err != nil {
				t.Fatalf("DiscoverPullRequests() error = %v", err)
			}
			if len(result.QueueItems) != 1 {
				t.Fatalf("len(QueueItems) = %d, want 1 engaged follow-up", len(result.QueueItems))
			}
			persistedLoop, err := fixture.repos.Loops.GetByID(context.Background(), loop.ID)
			if err != nil {
				t.Fatalf("Loops.GetByID() error = %v", err)
			}
			if persistedLoop == nil || persistedLoop.MetadataJSON == nil || !contains(*persistedLoop.MetadataJSON, `"lastPublishedHeadSha":"old-head"`) {
				t.Fatalf("loop after discovery = %#v, want GitHub engagement head backfilled", persistedLoop)
			}
			fixture.advance(time.Hour)
			claimed, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
			if err != nil || claimed == nil {
				t.Fatalf("ClaimNextOfType() = (%#v, %v), want engaged follow-up", claimed, err)
			}
			processed, err := runner.ProcessClaimedItem(context.Background(), *claimed)
			if err != nil {
				t.Fatalf("ProcessClaimedItem() error = %v", err)
			}
			if processed.Status != "success" || len(agent.starts) != 1 {
				t.Fatalf("ProcessClaimedItem() = %#v, agent starts = %d; want full follow-up review", processed, len(agent.starts))
			}
			if tc.forgejo && (github.listReviewThreadsCalls != 0 || len(github.resolveThreadCalls) != 0) {
				t.Fatal("Forgejo follow-up attempted unsupported thread resolution")
			}
			if tc.forgejo {
				repeated, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: repo})
				if err != nil || len(repeated.QueueItems) != 0 {
					t.Fatalf("same-head rediscovery = %#v, %v; want no duplicate review", repeated, err)
				}
			}

		})
	}
}

func TestDiscoverPullRequestsBackfillsEngagementWhenDiscoveryViewOmitsReviews(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	const loopID = "loop_follow_engaged_snapshot_omits_reviews"
	// Simulate daemon DiscoverySnapshot lifecycle: discovery ViewPullRequest uses a
	// fixer-profile detail without reviews, so engagement recovery must load
	// reviews via LoadPullRequestReviews rather than trusting detail.Reviews.
	github := &fakeGitHubGateway{
		currentLogin:      "bob",
		reviewRequests:    []string{},
		viewHeadSHA:       "new-head",
		omitReviewsOnView: true,
		reviews: []map[string]any{{
			"author": map[string]any{"login": "bob"},
			"commit": map[string]any{"oid": "old-head"},
			"state":  "CHANGES_REQUESTED",
			"body":   "<!-- looper:review id=reviewer:" + loopID + " head=old-head outcome=blocking -->",
		}},
	}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "No actionable findings", Stdout: `__LOOPER_RESULT__={"summary":"posted clean follow-up review"}`, ParseStatus: "parsed"}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, DiscoveryPolicy: DiscoveryPolicy{AutoDiscovery: true, IncludeDrafts: false, RequireReviewRequest: true, Labels: []string{}, LabelMode: config.LabelModeAll}, ReviewEvents: config.ReviewerReviewEventsConfig{Clean: config.ReviewerReviewEventComment, Blocking: config.ReviewerReviewEventRequestChanges}, LoopConfig: testReviewerLoopConfig()})
	nowISO := fixture.nowISO()
	repo := "acme/looper"
	prNumber := int64(42)
	metadata := `{"followUpdates":true,"lastFilterSkip":{"kind":"not_requested","headSha":"old-head","reviewerLogin":"bob"},"loop":{"enabled":true,"iterationCount":1,"iterationsByHead":{"old-head":1}}}`
	loop := storage.LoopRecord{ID: loopID, Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: "completed", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	snapshotGateway := githubinfra.New(githubinfra.Options{GHRun: func(context.Context, shell.Options) (shell.Result, error) {
		return shell.Result{}, errors.New("discovery snapshot gateway should not be called by fake reviewer path")
	}})
	snapshot := githubinfra.NewDiscoverySnapshot(snapshotGateway, githubinfra.NewDiscoveryTickState(), githubinfra.DiscoverySnapshotOptions{})
	if snapshot == nil {
		t.Fatal("NewDiscoverySnapshot() = nil, want non-nil scheduler-style snapshot")
	}
	result, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: repo, Snapshot: snapshot})
	if err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	if len(result.QueueItems) != 1 {
		t.Fatalf("len(QueueItems) = %d, want 1 engaged follow-up after review reload", len(result.QueueItems))
	}
	if github.loadReviewsCalls < 1 {
		t.Fatalf("LoadPullRequestReviews calls = %d, want >= 1 when discovery view omits reviews", github.loadReviewsCalls)
	}
	persistedLoop, err := fixture.repos.Loops.GetByID(context.Background(), loop.ID)
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if persistedLoop == nil || persistedLoop.MetadataJSON == nil || !contains(*persistedLoop.MetadataJSON, `"lastPublishedHeadSha":"old-head"`) {
		t.Fatalf("loop after discovery = %#v, want engagement head backfilled from reloaded reviews", persistedLoop)
	}
}

func TestDiscoverPullRequestsRejectsNonAuthorizingEngagementMarkers(t *testing.T) {
	t.Parallel()
	// Publish verification rejects policy-inconsistent and noncanonical markers;
	// discovery must not backfill lastPublishedHeadSha from them.
	cases := []struct {
		name   string
		state  string
		body   string
		policy config.ReviewerReviewEventsConfig
	}{
		{
			name:   "commented_blocking_under_request_changes_policy",
			state:  "COMMENTED",
			body:   "<!-- looper:review id=reviewer:loop_reject head=old-head outcome=blocking -->",
			policy: config.ReviewerReviewEventsConfig{Clean: config.ReviewerReviewEventComment, Blocking: config.ReviewerReviewEventRequestChanges},
		},
		{
			name:   "outcome_BLOCKING_casing",
			state:  "CHANGES_REQUESTED",
			body:   "<!-- looper:review id=reviewer:loop_reject head=old-head outcome=BLOCKING -->",
			policy: config.ReviewerReviewEventsConfig{Clean: config.ReviewerReviewEventComment, Blocking: config.ReviewerReviewEventRequestChanges},
		},
		{
			name:   "uppercase_marker_tag",
			state:  "CHANGES_REQUESTED",
			body:   "<!-- LOOPER:REVIEW id=reviewer:loop_reject head=old-head outcome=blocking -->",
			policy: config.ReviewerReviewEventsConfig{Clean: config.ReviewerReviewEventComment, Blocking: config.ReviewerReviewEventRequestChanges},
		},
		{
			name:   "review_extra_suffix_marker",
			state:  "CHANGES_REQUESTED",
			body:   "<!-- looper:review-extra id=reviewer:loop_reject head=old-head outcome=blocking -->",
			policy: config.ReviewerReviewEventsConfig{Clean: config.ReviewerReviewEventComment, Blocking: config.ReviewerReviewEventRequestChanges},
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fixture := newRunnerFixture(t)
			const loopID = "loop_reject"
			github := &fakeGitHubGateway{
				currentLogin:   "bob",
				author:         "alice",
				reviewRequests: []string{},
				viewHeadSHA:    "new-head",
				reviews: []map[string]any{{
					"author": map[string]any{"login": "bob"},
					"commit": map[string]any{"oid": "old-head"},
					"state":  tc.state,
					"body":   strings.ReplaceAll(tc.body, "loop_reject", loopID),
				}},
			}
			runner := New(Options{
				DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{},
				AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now,
				DiscoveryPolicy: DiscoveryPolicy{AutoDiscovery: true, IncludeDrafts: false, RequireReviewRequest: true, Labels: []string{}, LabelMode: config.LabelModeAll},
				ReviewEvents:    tc.policy,
				LoopConfig:      testReviewerLoopConfig(),
			})
			nowISO := fixture.nowISO()
			repo := "acme/looper"
			prNumber := int64(42)
			metadata := `{"followUpdates":true,"lastFilterSkip":{"kind":"not_requested","headSha":"old-head","reviewerLogin":"bob"},"loop":{"enabled":true,"iterationCount":1,"iterationsByHead":{"old-head":1}}}`
			loop := storage.LoopRecord{ID: loopID, Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: "completed", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}
			if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
				t.Fatalf("Loops.Upsert() error = %v", err)
			}

			result, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: repo})
			if err != nil {
				t.Fatalf("DiscoverPullRequests() error = %v", err)
			}
			if len(result.QueueItems) != 0 {
				t.Fatalf("len(QueueItems) = %d, want 0 (rejected marker must not authorize follow-up)", len(result.QueueItems))
			}
			persistedLoop, err := fixture.repos.Loops.GetByID(context.Background(), loop.ID)
			if err != nil {
				t.Fatalf("Loops.GetByID() error = %v", err)
			}
			if persistedLoop == nil || persistedLoop.MetadataJSON == nil {
				t.Fatalf("loop after discovery = %#v, want preserved metadata", persistedLoop)
			}
			if contains(*persistedLoop.MetadataJSON, `"lastPublishedHeadSha"`) {
				t.Fatalf("loop metadata = %s, want no lastPublishedHeadSha backfill", *persistedLoop.MetadataJSON)
			}
		})
	}
}

func TestLooperReviewEngagementHeadMatchesPublishVerifierGates(t *testing.T) {
	t.Parallel()
	const loopID = "loop_engagement_policy"
	marker := func(body string) []map[string]any {
		return []map[string]any{{
			"author": map[string]any{"login": "bob"},
			"commit": map[string]any{"oid": "old-head"},
			"state":  "CHANGES_REQUESTED",
			"body":   body,
		}}
	}
	withState := func(state, body string) []map[string]any {
		return []map[string]any{{
			"author": map[string]any{"login": "bob"},
			"commit": map[string]any{"oid": "old-head"},
			"state":  state,
			"body":   body,
		}}
	}
	allowedRC := []ReviewEvent{ReviewEventComment, ReviewEventRequestChanges}
	allowedApprove := []ReviewEvent{ReviewEventComment, ReviewEventApprove}
	allowedComment := []ReviewEvent{ReviewEventComment}
	canonicalBlocking := "<!-- looper:review id=reviewer:" + loopID + " head=old-head outcome=blocking -->"
	canonicalClean := "<!-- looper:review id=reviewer:" + loopID + " head=old-head outcome=clean -->"

	// Policy: COMMENTED+blocking is only valid when REQUEST_CHANGES is not required.
	if got := looperReviewEngagementHead(withState("COMMENTED", canonicalBlocking), "bob", loopID, "new-head", allowedRC, false, false); got != "" {
		t.Fatalf("COMMENTED+blocking under REQUEST_CHANGES policy = %q, want empty", got)
	}
	if got := looperReviewEngagementHead(withState("COMMENTED", canonicalBlocking), "bob", loopID, "new-head", allowedComment, false, false); got != "old-head" {
		t.Fatalf("COMMENTED+blocking under COMMENT policy = %q, want old-head", got)
	}
	if got := looperReviewEngagementHead(marker(canonicalBlocking), "bob", loopID, "new-head", allowedRC, false, false); got != "old-head" {
		t.Fatalf("CHANGES_REQUESTED+blocking = %q, want old-head", got)
	}

	// Grammar: case-sensitive outer marker; publish verifier rejects these.
	for _, body := range []string{
		"<!-- LOOPER:REVIEW id=reviewer:" + loopID + " head=old-head outcome=blocking -->",
		"<!-- looper:review-extra id=reviewer:" + loopID + " head=old-head outcome=blocking -->",
		"<!-- looper:review id=reviewer:" + loopID + " head=old-head outcome=BLOCKING -->",
	} {
		if got := looperReviewEngagementHead(marker(body), "bob", loopID, "new-head", allowedRC, false, false); got != "" {
			t.Fatalf("noncanonical body %q engagement head = %q, want empty", body, got)
		}
	}

	// Human-body quality belongs to publish validation, not engagement authority.
	if got := looperReviewEngagementHead(withState("APPROVED", canonicalClean), "bob", loopID, "new-head", allowedApprove, false, false); got != "old-head" {
		t.Fatalf("APPROVED clean marker-only = %q, want old-head engagement", got)
	}

	if got := looperReviewEngagementHead(withState("COMMENTED", canonicalClean), "bob", loopID, "new-head", allowedApprove, true, false); got != "old-head" {
		t.Fatalf("self-approval clean marker-only = %q, want old-head engagement", got)
	}
	// COMMENT clean policy does not require body validation.
	if got := looperReviewEngagementHead(withState("COMMENTED", canonicalClean), "bob", loopID, "new-head", allowedComment, false, false); got != "old-head" {
		t.Fatalf("COMMENT policy clean = %q, want old-head", got)
	}

}

func TestReviewsForEngagementBackfillTreatsEmptyPresentListAsAuthoritative(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	const loopID = "loop_empty_reviews_no_reload"
	github := &fakeGitHubGateway{
		currentLogin:   "bob",
		reviewRequests: []string{},
		viewHeadSHA:    "new-head",
		reviews:        []map[string]any{},
	}
	runner := New(Options{
		DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{},
		AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now,
		DiscoveryPolicy: DiscoveryPolicy{AutoDiscovery: true, IncludeDrafts: false, RequireReviewRequest: true, Labels: []string{}, LabelMode: config.LabelModeAll},
		ReviewEvents:    config.ReviewerReviewEventsConfig{Clean: config.ReviewerReviewEventComment, Blocking: config.ReviewerReviewEventRequestChanges},
		LoopConfig:      testReviewerLoopConfig(),
	})
	nowISO := fixture.nowISO()
	repo := "acme/looper"
	prNumber := int64(42)
	metadata := `{"followUpdates":true,"loop":{"enabled":true,"iterationCount":1,"iterationsByHead":{"old-head":1}}}`
	loop := storage.LoopRecord{ID: loopID, Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: "completed", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	project, err := fixture.repos.Projects.GetByID(context.Background(), "project_1")
	if err != nil || project == nil {
		t.Fatalf("Projects.GetByID() = (%#v, %v)", project, err)
	}

	got, err := runner.reviewsForEngagementBackfill(context.Background(), *project, loop, PullRequestDetail{
		Number: prNumber, HeadSHA: "new-head", Reviews: []map[string]any{},
	})
	if err != nil {
		t.Fatalf("reviewsForEngagementBackfill(empty present) error = %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("reviewsForEngagementBackfill(empty present) = %#v, want empty non-nil slice", got)
	}
	if github.loadReviewsCalls != 0 {
		t.Fatalf("LoadPullRequestReviews calls = %d, want 0 when reviews list is present and empty", github.loadReviewsCalls)
	}

	got, err = runner.reviewsForEngagementBackfill(context.Background(), *project, loop, PullRequestDetail{
		Number: prNumber, HeadSHA: "new-head", Reviews: nil,
	})
	if err != nil {
		t.Fatalf("reviewsForEngagementBackfill(nil) error = %v", err)
	}
	if got == nil {
		t.Fatal("reviewsForEngagementBackfill(nil) = nil, want loaded empty list from gateway")
	}
	if github.loadReviewsCalls != 1 {
		t.Fatalf("LoadPullRequestReviews calls = %d, want 1 when reviews field is omitted", github.loadReviewsCalls)
	}
}

// Bypass discovery entirely: a persisted queue/checkpoint must be sufficient to
// reach a full review or finish its pending publication after a daemon restart.
func TestQueuedAndResumedReviewerRecoverEngagementWithoutDiscovery(t *testing.T) {
	for _, mode := range []string{"queued", "review", "publish", "legacy-thread-only", "disposition-new-head", "request-consumed"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			fixture := newRunnerFixture(t)
			ctx := context.Background()
			repo, loopID := "acme/looper", "loop_recover_"+mode
			prNumber := int64(42)
			reviews := []map[string]any{{"author": map[string]any{"login": "bob"}, "commit": map[string]any{"oid": "old-head"}, "state": "CHANGES_REQUESTED", "body": "<!-- looper:review id=reviewer:" + loopID + " head=old-head outcome=blocking -->"}}
			github := &fakeGitHubGateway{currentLogin: "bob", author: "alice", reviewRequests: []string{}, viewHeadSHA: "new-head", reviews: reviews}
			agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "Follow-up review", Stdout: `__LOOPER_RESULT__={"summary":"posted follow-up review"}`, ParseStatus: "parsed"}}}
			runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, CommentOnlyPublish: mode == "publish", Logger: fixture.logger, Now: fixture.now, DiscoveryPolicy: DiscoveryPolicy{RequireReviewRequest: true}, ReviewEvents: config.ReviewerReviewEventsConfig{Clean: config.ReviewerReviewEventComment, Blocking: config.ReviewerReviewEventRequestChanges}, LoopConfig: testReviewerLoopConfig()})
			if mode == "request-consumed" {
				github.reviewRequests = []string{"bob"}
				github.removeReviewRequestOnSecondView = true
			}
			agent.onStart = func(input AgentRunInput) {
				stored, err := fixture.repos.Loops.GetByID(ctx, loopID)
				if err != nil || stored == nil {
					t.Fatalf("load at agent start: %#v, %v", stored, err)
				}
				if got, _ := stringFromAny(parseJSONObject(stored.MetadataJSON)["lastPublishedHeadSha"]); got != "old-head" {
					t.Fatalf("agent started before recovering prior publication: %q", got)
				}
				if !strings.Contains(input.Prompt, "a fresh current-user review request is not required") {
					t.Fatal("agent prompt still requires a fresh request")
				}
			}
			now := fixture.nowISO()
			meta := `{"followUpdates":true,"loop":{"enabled":true}}`
			loop := storage.LoopRecord{ID: loopID, Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: "queued", MetadataJSON: &meta, CreatedAt: now, UpdatedAt: now}
			if err := fixture.repos.Loops.Upsert(ctx, loop); err != nil {
				t.Fatal(err)
			}
			if mode == "review" || mode == "publish" || mode == "legacy-thread-only" {
				checkpoint := reviewerCheckpoint{
					ResumePolicy: "advance_from_checkpoint",
					Detail:       &checkpointDetail{Title: "Review", State: "OPEN", HeadSHA: "new-head", BaseSHA: "base", HeadRefName: "feature/review-me", BaseRefName: "main", Author: "alice", CurrentLogin: "bob", Reviews: reviews},
					Snapshot:     &checkpointSnapshot{HeadSHA: "new-head"},
					Worktree:     &checkpointWorktree{Path: t.TempDir(), Branch: "pr-42", BaseBranch: "main", PreparedAt: now},
					// Pending review lets a resumed review/publish retain its checkpoint even
					// though the consumed request is omitted by JSON's empty-list encoding.
					PendingReview: &pendingReviewCheckpoint{HeadSHA: "new-head", IdempotencyKey: agentNativeReviewID(loopID, "new-head"), Event: reviewEventAgentNative, Summary: "No actionable findings remain", Outcome: "clean"},
				}
				if mode == "review" || mode == "legacy-thread-only" {
					checkpoint.PendingReview = nil
					checkpoint.Detail.ReviewRequests = []string{"someone-else"}
					checkpoint.ThreadResolutionFollowUpOnly = mode == "legacy-thread-only"
				}
				lastStep := stepThreadResolution
				if mode == "publish" {
					lastStep = stepReview
					checkpoint.PendingReview.ReviewerSummaryJSON = `{"summary":"No actionable findings remain","outcome":"clean","findings":[]}`
				}
				run := storage.RunRecord{ID: "run_previous", LoopID: loopID, Status: "failed", CurrentStep: stringPtr("review"), LastCompletedStep: stringPtr(string(lastStep)), CheckpointJSON: stringPtr(mustMarshalJSON(checkpoint)), StartedAt: now, EndedAt: &now, CreatedAt: now, UpdatedAt: now}
				if err := fixture.repos.Runs.Upsert(ctx, run); err != nil {
					t.Fatal(err)
				}
			}
			queuedHead := "new-head"
			if mode == "disposition-new-head" {
				queuedHead = "old-head"
			}
			if _, err := runner.enqueue(ctx, enqueueInput{ProjectID: "project_1", LoopID: loopID, Repo: repo, PRNumber: prNumber, HeadSHA: queuedHead, DispositionOnly: mode == "disposition-new-head"}); err != nil {
				t.Fatal(err)
			}
			claimed, err := fixture.repos.Queue.ClaimNextOfType(ctx, now, "worker", "reviewer")
			if err != nil || claimed == nil {
				t.Fatalf("claim = %#v, %v", claimed, err)
			}
			result, err := runner.ProcessClaimedItem(ctx, *claimed)
			if err != nil || result.Status != "success" {
				t.Fatalf("process = %#v, %v", result, err)
			}
			wantStarts := 1
			if mode == "publish" {
				wantStarts = 0
				if len(github.issueCommentCalls) != 1 {
					t.Fatalf("publication calls = %d, want 1", len(github.issueCommentCalls))
				}
			}
			if len(agent.starts) != wantStarts {
				t.Fatalf("agent starts = %d, want %d", len(agent.starts), wantStarts)
			}
			updated, err := fixture.repos.Loops.GetByID(ctx, loopID)
			if err != nil || updated == nil {
				t.Fatalf("loop = %#v, %v", updated, err)
			}
			if got, _ := stringFromAny(parseJSONObject(updated.MetadataJSON)["lastPublishedHeadSha"]); got != "new-head" {
				t.Fatalf("published head = %q", got)
			}
		})
	}
}

func TestEngagementBackfillDoesNotOverwriteConcurrentPublication(t *testing.T) {
	// Deliberately sequential: updateLoopBeforeWriteHook is the existing CAS race
	// affordance. Interleave publication after recovery's read and before its CAS.
	fixture := newRunnerFixture(t)
	ctx := context.Background()
	repo, loopID := "acme/looper", "loop_racing_recovery"
	pr := int64(42)
	meta := `{"followUpdates":true,"loop":{"enabled":true}}`
	now := fixture.nowISO()
	loop := storage.LoopRecord{ID: loopID, Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &pr, Status: "running", MetadataJSON: &meta, CreatedAt: now, UpdatedAt: now}
	if err := fixture.repos.Loops.Upsert(ctx, loop); err != nil {
		t.Fatal(err)
	}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, Logger: fixture.logger, Now: fixture.now, ReviewEvents: config.ReviewerReviewEventsConfig{Clean: config.ReviewerReviewEventComment, Blocking: config.ReviewerReviewEventRequestChanges}})
	project, err := fixture.repos.Projects.GetByID(ctx, "project_1")
	if err != nil || project == nil {
		t.Fatalf("project = %#v, %v", project, err)
	}
	published := false
	oldHook := updateLoopBeforeWriteHook
	defer func() { updateLoopBeforeWriteHook = oldHook }()
	updateLoopBeforeWriteHook = func(current storage.LoopRecord) error {
		if published {
			return nil
		}
		published = true
		raw := `{"followUpdates":true,"lastPublishedHeadSha":"new-head","loop":{"enabled":true,"iterationCount":2}}`
		current.MetadataJSON = &raw
		return fixture.repos.Loops.Upsert(ctx, current)
	}
	reviews := []map[string]any{{"author": map[string]any{"login": "bob"}, "state": "CHANGES_REQUESTED", "commit": map[string]any{"oid": "old-head"}, "body": "<!-- looper:review id=reviewer:" + loopID + " head=old-head outcome=blocking -->"}}
	got, err := runner.backfillPublishedHeadFromLooperReview(ctx, *project, loop, PullRequestDetail{HeadSHA: "new-head", Reviews: reviews}, "bob")
	if err != nil {
		t.Fatal(err)
	}
	if !published {
		t.Fatal("publication interleave did not run")
	}
	stored, err := fixture.repos.Loops.GetByID(ctx, loopID)
	if err != nil || stored == nil {
		t.Fatalf("stored = %#v, %v", stored, err)
	}
	for _, record := range []storage.LoopRecord{got, *stored} {
		parsed := parseJSONObject(record.MetadataJSON)
		if parsed["lastPublishedHeadSha"] != "new-head" || intFromAny(reviewerLoopMetadata(parsed)["iterationCount"]) != 2 {
			t.Fatalf("recovery overwrote publication: %s", *record.MetadataJSON)
		}
	}
}

func TestRecoveredFollowUpInvalidatesOnlyObsoleteRequestSkip(t *testing.T) {
	for _, tc := range []struct {
		name, kind, previous                    string
		follow, enabled, routed, wantSuppressed bool
	}{
		{"new head", "not_requested", "old-head", true, true, false, false},
		{"no publication", "not_requested", "", true, true, false, true},
		{"same head", "not_requested", "new-head", true, true, false, true},
		{"follow disabled", "not_requested", "old-head", false, true, false, true},
		{"loop disabled", "not_requested", "old-head", true, false, false, true},
		{"other skip reason", "conflicted", "old-head", true, true, false, true},
		{"routed request missing", "not_requested", "old-head", true, true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			raw := mustMarshalJSON(map[string]any{
				"followUpdates": tc.follow, "lastPublishedHeadSha": tc.previous, "loop": map[string]any{"enabled": tc.enabled},
				"lastFilterSkip": map[string]any{"kind": tc.kind, "headSha": "new-head", "reviewerLogin": "bob"},
			})
			policy := DiscoveryPolicy{RequireReviewRequest: true}
			if tc.routed {
				policy.RoutedClaimPolicy = networkpolicy.ProjectPolicy{Mode: config.NetworkModeRouted, NodeName: "red", GitHubLogin: "bob", GitHubUserID: 42}
			}
			pr := PullRequestSummary{Number: 42, HeadSHA: "new-head", Author: "alice", HasConflicts: true, ReviewRequests: []string{}, ReviewRequestUsers: []networkpolicy.GitHubUser{}, Labels: []string{"looper:target:red"}}
			if got := reviewerDiscoverySuppressedByLastSkip(storage.LoopRecord{MetadataJSON: &raw}, pr, "bob", policy); got != tc.wantSuppressed {
				t.Fatalf("suppressed = %v, want %v", got, tc.wantSuppressed)
			}
		})
	}
}
