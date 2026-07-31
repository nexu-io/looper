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
	"github.com/nexu-io/looper/internal/storage"
)

func TestDiscoverPullRequestsRequeuesLooperEngagedFollowUpWithoutPublishedHeadMetadata(t *testing.T) {
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
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, DiscoveryPolicy: DiscoveryPolicy{AutoDiscovery: true, IncludeDrafts: false, RequireReviewRequest: true, Labels: []string{}, LabelMode: config.LabelModeAll}, ReviewEvents: config.ReviewerReviewEventsConfig{Clean: config.ReviewerReviewEventComment, Blocking: config.ReviewerReviewEventRequestChanges}, LoopConfig: testReviewerLoopConfig()})
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
	if got := looperReviewEngagementHead(withState("COMMENTED", canonicalBlocking), "bob", loopID, "new-head", allowedRC, false); got != "" {
		t.Fatalf("COMMENTED+blocking under REQUEST_CHANGES policy = %q, want empty", got)
	}
	if got := looperReviewEngagementHead(withState("COMMENTED", canonicalBlocking), "bob", loopID, "new-head", allowedComment, false); got != "old-head" {
		t.Fatalf("COMMENTED+blocking under COMMENT policy = %q, want old-head", got)
	}
	if got := looperReviewEngagementHead(marker(canonicalBlocking), "bob", loopID, "new-head", allowedRC, false); got != "old-head" {
		t.Fatalf("CHANGES_REQUESTED+blocking = %q, want old-head", got)
	}

	// Grammar: case-sensitive outer marker; publish verifier rejects these.
	for _, body := range []string{
		"<!-- LOOPER:REVIEW id=reviewer:" + loopID + " head=old-head outcome=blocking -->",
		"<!-- looper:review-extra id=reviewer:" + loopID + " head=old-head outcome=blocking -->",
		"<!-- looper:review id=reviewer:" + loopID + " head=old-head outcome=BLOCKING -->",
	} {
		if got := looperReviewEngagementHead(marker(body), "bob", loopID, "new-head", allowedRC, false); got != "" {
			t.Fatalf("noncanonical body %q engagement head = %q, want empty", body, got)
		}
	}

	// Human-body quality belongs to publish validation, not engagement authority.
	if got := looperReviewEngagementHead(withState("APPROVED", canonicalClean), "bob", loopID, "new-head", allowedApprove, false); got != "old-head" {
		t.Fatalf("APPROVED clean marker-only = %q, want old-head engagement", got)
	}

	if got := looperReviewEngagementHead(withState("COMMENTED", canonicalClean), "bob", loopID, "new-head", allowedApprove, true); got != "old-head" {
		t.Fatalf("self-approval clean marker-only = %q, want old-head engagement", got)
	}
	// COMMENT clean policy does not require body validation.
	if got := looperReviewEngagementHead(withState("COMMENTED", canonicalClean), "bob", loopID, "new-head", allowedComment, false); got != "old-head" {
		t.Fatalf("COMMENT policy clean = %q, want old-head", got)
	}

	if !isRecognizedReviewOutcome("blocking") || isRecognizedReviewOutcome("BLOCKING") {
		t.Fatal("isRecognizedReviewOutcome must match exact lowercase publish-verification tokens")
	}
	if reviewMarkerOutcomeEventAllowed("BLOCKING", ReviewEventRequestChanges, allowedRC, false) {
		t.Fatal("reviewMarkerOutcomeEventAllowed(BLOCKING) = true, want false")
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
