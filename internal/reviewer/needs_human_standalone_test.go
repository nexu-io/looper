package reviewer

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
)

func standaloneReviewerTestConfig(t *testing.T) config.Config {
	t.Helper()
	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig: %v", err)
	}
	cfg.HITL.Enabled = false
	cfg.Roles.Fixer.AutoDiscovery = false
	return cfg
}

func TestStandaloneNativeNeedsHumanPublishesWithoutParking(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name           string
		stdout         string
		inlineComments []string
		wantStatus     string
		wantRetryable  bool
	}{
		{
			name:           "pure_needs_human_published_inline",
			stdout:         `__LOOPER_RESULT__={"summary":"Human decision needed on API contract","outcome":"non_blocking","findings":[{"title":"[P2] Confirm API compatibility","body":"Spec and caller disagree on empty response shape.","disposition":"needs_human","severity":"non_blocking","scopeBasis":"ambiguous_intent","scopeEvidence":"PR non-goals vs existing handler","path":"a.go","line":10}]}`,
			inlineComments: []string{"**[P2] Confirm API compatibility**\n\nSpec and caller disagree."},
			wantStatus:     "success",
		},
		{
			name:           "mixed_must_fix_and_needs_human_published_inline",
			stdout:         `__LOOPER_RESULT__={"summary":"Blocking bug plus scope question","outcome":"blocking","findings":[{"title":"[P1] Nil pointer deref","body":"Missing nil guard.","disposition":"must_fix","severity":"blocking","scopeBasis":"introduced_regression","scopeEvidence":"diff line 4","path":"a.go","line":4},{"title":"[P2] Confirm retry cap","body":"Unclear whether cap applies here.","disposition":"needs_human","severity":"non_blocking","scopeBasis":"ambiguous_intent","scopeEvidence":"AGENTS.md","path":"b.go","line":9}]}`,
			inlineComments: []string{"**[P1] Nil pointer deref**", "**[P2] Confirm retry cap**"},
			wantStatus:     "success",
		},
		{
			name:           "pure_needs_human_missing_inline_comment_retries",
			stdout:         `__LOOPER_RESULT__={"summary":"Human decision needed","outcome":"non_blocking","findings":[{"title":"[P2] Confirm API compatibility","body":"Unclear contract.","disposition":"needs_human","severity":"non_blocking","scopeBasis":"ambiguous_intent","scopeEvidence":"PR description","path":"a.go","line":10}]}`,
			inlineComments: nil,
			wantStatus:     "failed",
			wantRetryable:  true,
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fixture := newRunnerFixture(t)
			ctx := context.Background()
			nowISO := fixture.nowISO()
			repo := "acme/looper"
			prNumber := int64(42)
			loopID := "loop_standalone_native_" + tc.name
			metadata := `{"followUpdates":true,"loop":{"enabled":true}}`
			loop := storage.LoopRecord{
				ID: loopID, Seq: 301, ProjectID: "project_1", Type: "reviewer",
				TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber,
				Status: "queued", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO,
			}
			if err := fixture.repos.Loops.Upsert(ctx, loop); err != nil {
				t.Fatalf("Loops.Upsert: %v", err)
			}
			cfg := standaloneReviewerTestConfig(t)
			github := &fakeGitHubGateway{
				reviewRequests:                  []string{"octocat"},
				reviewMarkerMissing:             len(tc.inlineComments) == 0,
				reviewMarkerEvent:               ReviewEventComment,
				reviewMarkerOutcome:             "non_blocking",
				reviewMarkerInlineCommentBodies: tc.inlineComments,
			}
			agent := &fakeAgentExecutor{results: []AgentResult{{
				Status: "completed", Summary: "Review finished", Stdout: tc.stdout, ParseStatus: "parsed",
			}}}
			runner := New(Options{
				DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{},
				AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, CustomInstructions: &cfg,
				LoopConfig: config.ReviewerLoopConfig{EnabledByDefault: true, QuietPeriodSeconds: 120, MaxIterationsPerPR: 20, MaxIterationsPerHead: 2, MaxWallClockSeconds: 14400, MaxConsecutiveFailures: 3, MaxAgentExecutionsPerPR: 25},
			})
			queue, err := runner.enqueue(ctx, enqueueInput{ProjectID: "project_1", LoopID: loop.ID, Repo: repo, PRNumber: prNumber})
			if err != nil {
				t.Fatalf("enqueue: %v", err)
			}
			claimed, err := fixture.repos.Queue.ClaimNextOfType(ctx, nowISO, "reviewer-worker-1", "reviewer")
			if err != nil || claimed == nil || claimed.ID != queue.ID {
				t.Fatalf("ClaimNextOfType = (%#v, %v)", claimed, err)
			}
			result, err := runner.ProcessClaimedItem(ctx, *claimed)
			if err != nil {
				t.Fatalf("ProcessClaimedItem: %v", err)
			}
			if result.Status != tc.wantStatus {
				t.Fatalf("result = %#v, want status=%s", result, tc.wantStatus)
			}
			updated, err := fixture.repos.Loops.GetByID(ctx, loopID)
			if err != nil || updated == nil {
				t.Fatalf("Loops.GetByID = (%#v, %v)", updated, err)
			}
			if loops.IsReviewScopeHumanHold(*updated) || loops.HasPendingReviewScopeHuman(*updated) {
				t.Fatalf("standalone reviewer must not park on needs_human: status=%s meta=%s", updated.Status, derefString(updated.MetadataJSON))
			}
			if tc.wantRetryable {
				if result.FailureKind != FailureRetryableAfterResume {
					t.Fatalf("FailureKind = %q, want %q", result.FailureKind, FailureRetryableAfterResume)
				}
				return
			}
			if updated.Status != "waiting" {
				t.Fatalf("status = %q, want waiting", updated.Status)
			}
			gotHead, _ := stringFromAny(parseJSONObject(updated.MetadataJSON)["lastPublishedHeadSha"])
			if gotHead == "" {
				t.Fatalf("lastPublishedHeadSha empty after standalone publish: %s", derefString(updated.MetadataJSON))
			}
		})
	}
}

func TestStandaloneCommentOnlyNeedsHumanPublishesAndNeverParks(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	ctx := context.Background()
	nowISO := fixture.nowISO()
	repo := "acme/looper"
	prNumber := int64(42)
	loopID := "loop_standalone_comment_only"
	metadata := `{"followUpdates":true,"loop":{"enabled":true}}`
	loop := storage.LoopRecord{
		ID: loopID, Seq: 302, ProjectID: "project_1", Type: "reviewer",
		TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber,
		Status: "queued", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	if err := fixture.repos.Loops.Upsert(ctx, loop); err != nil {
		t.Fatalf("Loops.Upsert: %v", err)
	}
	cfg := standaloneReviewerTestConfig(t)
	cfg.Roles.Reviewer.Behavior.ReviewEvents.Clean = config.ReviewerReviewEventComment
	stdout := `__LOOPER_RESULT__={"summary":"Scope clarification required","outcome":"non_blocking","findings":[{"title":"[P2] Clarify timeout policy","body":"PR and spec disagree on default timeout.","disposition":"needs_human","severity":"non_blocking","scopeBasis":"ambiguous_intent","scopeEvidence":"spec section 4","files":["a.go"]},{"title":"[P3] Rename helper later","body":"Optional rename.","disposition":"follow_up","severity":"nit","scopeBasis":"independent_improvement","scopeEvidence":"style","files":["a.go"]}]}`
	github := &fakeGitHubGateway{reviewRequests: []string{"octocat"}}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "Scope clarification required", Stdout: stdout, ParseStatus: "parsed"}}}
	runner := New(Options{
		DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{},
		AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, CommentOnlyPublish: true,
		CustomInstructions: &cfg,
		LoopConfig:         config.ReviewerLoopConfig{EnabledByDefault: true, QuietPeriodSeconds: 120, MaxIterationsPerPR: 20, MaxIterationsPerHead: 2, MaxWallClockSeconds: 14400, MaxConsecutiveFailures: 3, MaxAgentExecutionsPerPR: 25},
		ReviewEvents:       cfg.Roles.Reviewer.Behavior.ReviewEvents,
	})
	queue, err := runner.enqueue(ctx, enqueueInput{ProjectID: "project_1", LoopID: loop.ID, Repo: repo, PRNumber: prNumber})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	claimed, err := fixture.repos.Queue.ClaimNextOfType(ctx, nowISO, "reviewer-worker-1", "reviewer")
	if err != nil || claimed == nil || claimed.ID != queue.ID {
		t.Fatalf("ClaimNextOfType = (%#v, %v)", claimed, err)
	}
	result, err := runner.ProcessClaimedItem(ctx, *claimed)
	if err != nil {
		t.Fatalf("ProcessClaimedItem: %v", err)
	}
	if result.Status != "success" {
		t.Fatalf("result = %#v, want success", result)
	}
	if len(github.issueCommentCalls) != 1 {
		t.Fatalf("issueCommentCalls = %d, want 1 published comment containing needs_human", len(github.issueCommentCalls))
	}
	body := github.issueCommentCalls[0].Body
	if !strings.Contains(body, "Clarify timeout policy") || !strings.Contains(body, "PR and spec disagree on default timeout.") {
		t.Fatalf("published comment missing needs_human finding: %s", body)
	}
	if strings.Contains(body, "Rename helper later") || strings.Contains(body, "follow_up") {
		t.Fatalf("published comment must still suppress follow_up: %s", body)
	}
	updated, err := fixture.repos.Loops.GetByID(ctx, loopID)
	if err != nil || updated == nil {
		t.Fatalf("Loops.GetByID = (%#v, %v)", updated, err)
	}
	if updated.Status != "waiting" || loops.IsReviewScopeHumanHold(*updated) {
		t.Fatalf("standalone comment-only loop = status=%s meta=%s, want waiting with no scope hold", updated.Status, derefString(updated.MetadataJSON))
	}
}

func TestStandaloneConfigWithActiveManualFixerStillParksScopeHold(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		fixerStatus string
		wantPark    bool
	}{
		{name: "active_manual_fixer_parks", fixerStatus: "waiting", wantPark: true},
		{name: "terminated_fixer_stays_standalone", fixerStatus: "terminated", wantPark: false},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fixture := newRunnerFixture(t)
			ctx := context.Background()
			nowISO := fixture.nowISO()
			repo := "acme/looper"
			prNumber := int64(42)
			loopID := "loop_standalone_fixer_check_" + tc.name
			metadata := `{"followUpdates":true,"loop":{"enabled":true}}`
			reviewerLoop := storage.LoopRecord{
				ID: loopID, Seq: 303, ProjectID: "project_1", Type: "reviewer",
				TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber,
				Status: "queued", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO,
			}
			fixerLoop := storage.LoopRecord{
				ID: "loop_fixer_" + tc.name, Seq: 304, ProjectID: "project_1", Type: "fixer",
				TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber,
				Status: tc.fixerStatus, MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO,
			}
			if err := fixture.repos.Loops.Upsert(ctx, reviewerLoop); err != nil {
				t.Fatalf("upsert reviewer: %v", err)
			}
			if err := fixture.repos.Loops.Upsert(ctx, fixerLoop); err != nil {
				t.Fatalf("upsert fixer: %v", err)
			}
			cfg := standaloneReviewerTestConfig(t)
			stdout := `__LOOPER_RESULT__={"summary":"Need human","outcome":"blocking","findings":[{"title":"Ambiguous","body":"Unclear","disposition":"needs_human","severity":"blocking","scopeBasis":"ambiguous_intent","scopeEvidence":"AGENTS.md","path":"a.go","line":1}]}`
			github := &fakeGitHubGateway{
				reviewRequests:                  []string{"octocat"},
				reviewMarkerMissing:             tc.wantPark,
				reviewMarkerEvent:               ReviewEventComment,
				reviewMarkerOutcome:             "blocking",
				reviewMarkerInlineCommentBodies: []string{"Ambiguous"},
			}
			runner := New(Options{
				DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{},
				AgentExecutor: &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "Need human", Stdout: stdout, ParseStatus: "parsed"}}},
				Logger:        fixture.logger, Now: fixture.now, CustomInstructions: &cfg,
				LoopConfig: config.ReviewerLoopConfig{EnabledByDefault: true, QuietPeriodSeconds: 120, MaxIterationsPerPR: 20, MaxIterationsPerHead: 2, MaxWallClockSeconds: 14400, MaxConsecutiveFailures: 3, MaxAgentExecutionsPerPR: 25},
			})
			queue, err := runner.enqueue(ctx, enqueueInput{ProjectID: "project_1", LoopID: reviewerLoop.ID, Repo: repo, PRNumber: prNumber})
			if err != nil {
				t.Fatalf("enqueue: %v", err)
			}
			claimed, err := fixture.repos.Queue.ClaimNextOfType(ctx, nowISO, "reviewer-worker-1", "reviewer")
			if err != nil || claimed == nil || claimed.ID != queue.ID {
				t.Fatalf("ClaimNextOfType = (%#v, %v)", claimed, err)
			}
			result, err := runner.ProcessClaimedItem(ctx, *claimed)
			if err != nil {
				t.Fatalf("ProcessClaimedItem: %v", err)
			}
			updated, err := fixture.repos.Loops.GetByID(ctx, loopID)
			if err != nil || updated == nil {
				t.Fatalf("Loops.GetByID = (%#v, %v)", updated, err)
			}
			if tc.wantPark {
				if result.Status != "skipped" || !loops.IsReviewScopeHumanHold(*updated) {
					t.Fatalf("active fixer pair = (result=%#v, status=%s, meta=%s), want parked scope hold", result, updated.Status, derefString(updated.MetadataJSON))
				}
				if got := loops.ReadReviewScopeHumanState(updated.MetadataJSON).HeadSHA; got != "abc123" {
					t.Fatalf("scope hold HeadSHA = %q, want abc123", got)
				}
				return
			}
			if result.Status != "success" || updated.Status != "waiting" || loops.IsReviewScopeHumanHold(*updated) {
				t.Fatalf("terminated fixer standalone = (result=%#v, status=%s, meta=%s), want success/waiting without hold", result, updated.Status, derefString(updated.MetadataJSON))
			}
		})
	}
}

func TestStandaloneNarrowDispositionNeedsHumanRepliesWithoutParking(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	ctx := context.Background()
	nowISO := fixture.nowISO()
	repo := "acme/looper"
	prNumber := int64(42)
	metadata := `{"followUpdates":true,"loop":{"enabled":true}}`
	loop := storage.LoopRecord{
		ID: "loop_standalone_disp_nh", Seq: 305, ProjectID: "project_1", Type: "reviewer",
		TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber,
		Status: "running", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	if err := fixture.repos.Loops.Upsert(ctx, loop); err != nil {
		t.Fatalf("Loops.Upsert: %v", err)
	}
	cfg := standaloneReviewerTestConfig(t)
	threads := []ReviewThread{{
		ID: "thread_1", Path: "a.go", Line: 10,
		Comments: []ReviewThreadComment{
			{ID: "c1", Author: "looper-bot", Body: "Check boundary\n<!-- looper:stamp v=1 -->"},
			{ID: "c2", Author: "alice", AuthorAssociation: "OWNER", Body: "/looper wontfix maybe later"},
		},
	}}
	github := &fakeGitHubGateway{
		currentLogin:   "looper-bot",
		reviewThreads:  threads,
		reviewRequests: []string{"looper-bot"},
	}
	agent := &fakeAgentExecutor{results: []AgentResult{{
		Status: "completed", Summary: "classified", ParseStatus: "parsed",
		Stdout: `{"decisions":[{"threadId":"thread_1","decision":"needs_human","evidence":"PR author did not state whether safety rule X can be waived","confidence":"high"}]}`,
	}}}
	runner := New(Options{
		DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{},
		AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, CustomInstructions: &cfg,
		LoopConfig:       testReviewerLoopConfig(),
		ThreadResolution: config.ReviewerThreadResolutionConfig{Enabled: false, Mode: config.ReviewerThreadResolutionModeReportOnly, MaxThreadsPerRun: 10},
	})
	project, err := fixture.repos.Projects.GetByID(ctx, "project_1")
	if err != nil || project == nil {
		t.Fatalf("project = (%#v, %v)", project, err)
	}
	_, err = runner.runThreadResolutionStep(ctx, stepInput{
		Project: *project, Loop: loop, Run: storage.RunRecord{ID: "run_standalone_disp"},
		Repo: repo, PRNumber: prNumber,
		Checkpoint: reviewerCheckpoint{
			DispositionOnly: true,
			Detail:          &checkpointDetail{HeadRefName: "feature/x", BaseRefName: "main", HeadSHA: "abc123", State: "OPEN", Author: "alice"},
			Snapshot:        &checkpointSnapshot{HeadSHA: "abc123"},
			Worktree:        &checkpointWorktree{Path: t.TempDir(), Branch: "feature/x", PreparedAt: nowISO},
		},
	})
	if err != nil {
		t.Fatalf("runThreadResolutionStep: %v", err)
	}
	if len(github.addThreadReplyCalls) != 1 {
		t.Fatalf("addThreadReplyCalls = %d, want 1 visible human-judgment reply", len(github.addThreadReplyCalls))
	}
	reply := github.addThreadReplyCalls[0].Body
	if !strings.Contains(reply, "needs human judgment") || !strings.Contains(reply, "decision=needs_human") {
		t.Fatalf("reply = %q, want needs_human explanation + audit marker", reply)
	}
	if len(github.resolveThreadCalls) != 0 {
		t.Fatalf("resolveThreadCalls = %d, want 0 (thread stays open)", len(github.resolveThreadCalls))
	}
	updated, err := fixture.repos.Loops.GetByID(ctx, loop.ID)
	if err != nil || updated == nil {
		t.Fatalf("Loops.GetByID = (%#v, %v)", updated, err)
	}
	if loops.IsReviewScopeHumanHold(*updated) {
		t.Fatalf("standalone disposition needs_human must not park loop: meta=%s", derefString(updated.MetadataJSON))
	}
	meta := parseJSONObject(updated.MetadataJSON)
	if fp, _ := stringFromAny(meta[metadataLastReviewedSignalFingerprintKey]); fp == "" {
		t.Fatalf("expected lastReviewedSignalFingerprint committed after reply: meta=%s", derefString(updated.MetadataJSON))
	}
}

func TestDiscoveryWakesReviewScopeHoldOnNewHeadAndRereview(t *testing.T) {
	t.Parallel()

	t.Run("pair_unchanged_head_stays_paused", func(t *testing.T) {
		t.Parallel()
		fixture := newRunnerFixture(t)
		ctx := context.Background()
		nowISO := fixture.nowISO()
		repo := "acme/looper"
		prNumber := int64(42)
		meta := `{"followUpdates":true,"loop":{"enabled":true,"iterationCount":1},"pauseReason":"review_scope_human_required","reviewScopeHuman":{"heldBy":"reviewer","pauseReason":"review_scope_human_required","headSha":"head_v1"}}`
		loop := storage.LoopRecord{
			ID: "loop_pair_stay_paused", Seq: 310, ProjectID: "project_1", Type: "reviewer",
			TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber,
			Status: "paused", MetadataJSON: &meta, CreatedAt: nowISO, UpdatedAt: nowISO,
		}
		if err := fixture.repos.Loops.Upsert(ctx, loop); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		cfg, _ := config.DefaultConfig(t.TempDir())
		cfg.HITL.Enabled = false
		cfg.Roles.Fixer.AutoDiscovery = true
		github := &fakeGitHubGateway{
			currentLogin:    "looper-bot",
			reviewRequests:  []string{"looper-bot"},
			listOpenByLabel: map[string][]PullRequestSummary{"": {{Number: prNumber, State: "OPEN", HeadSHA: "head_v1", Author: "alice", ReviewRequests: []string{"looper-bot"}}}},
			viewHeadSHA:     "head_v1",
			author:          "alice",
		}
		runner := New(Options{
			DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{},
			Logger: fixture.logger, Now: fixture.now, CustomInstructions: &cfg,
			DiscoveryPolicy: DiscoveryPolicy{AutoDiscovery: true, RequireReviewRequest: false, EnableSelfReview: true},
			LoopConfig:      testReviewerLoopConfig(),
		})
		res, err := runner.DiscoverPullRequests(ctx, DiscoveryInput{ProjectID: "project_1", Repo: repo})
		if err != nil {
			t.Fatalf("DiscoverPullRequests: %v", err)
		}
		if len(res.QueueItems) != 0 {
			t.Fatalf("QueueItems = %#v, want 0 on unchanged head in pair mode", res.QueueItems)
		}
		after, _ := fixture.repos.Loops.GetByID(ctx, loop.ID)
		if after == nil || after.Status != "paused" || !loops.IsReviewScopeHumanHold(*after) {
			t.Fatalf("loop after unchanged-head poll = %#v, want still paused scope hold", after)
		}
	})

	t.Run("pair_new_head_wakes_and_releases_sibling_without_meter_reset", func(t *testing.T) {
		t.Parallel()
		fixture := newRunnerFixture(t)
		ctx := context.Background()
		nowISO := fixture.nowISO()
		repo := "acme/looper"
		prNumber := int64(42)
		// Legacy paused loop without headSha in reviewScopeHuman — recovers held head from latest run checkpoint.
		revMeta := `{"followUpdates":true,"loop":{"enabled":true,"iterationCount":2},"lastPublishedHeadSha":"head_v0","pauseReason":"review_scope_human_required","reviewScopeHuman":{"heldBy":"reviewer","pauseReason":"review_scope_human_required"}}`
		fixMeta := `{"reviewFixBudget":{"pushCount":1},"pauseReason":"sibling_review_scope_human","reviewScopeHuman":{"heldBy":"reviewer","pauseReason":"sibling_review_scope_human"}}`
		revLoop := storage.LoopRecord{
			ID: "loop_pair_new_head_rev", Seq: 311, ProjectID: "project_1", Type: "reviewer",
			TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber,
			Status: "paused", MetadataJSON: &revMeta, CreatedAt: nowISO, UpdatedAt: nowISO,
		}
		fixLoop := storage.LoopRecord{
			ID: "loop_pair_new_head_fix", Seq: 312, ProjectID: "project_1", Type: "fixer",
			TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber,
			Status: "paused", MetadataJSON: &fixMeta, CreatedAt: nowISO, UpdatedAt: nowISO,
		}
		if err := fixture.repos.Loops.Upsert(ctx, revLoop); err != nil {
			t.Fatalf("upsert rev: %v", err)
		}
		if err := fixture.repos.Loops.Upsert(ctx, fixLoop); err != nil {
			t.Fatalf("upsert fix: %v", err)
		}
		cpJSON, _ := json.Marshal(reviewerCheckpoint{Snapshot: &checkpointSnapshot{HeadSHA: "head_v1"}})
		cpStr := string(cpJSON)
		if err := fixture.repos.Runs.Upsert(ctx, storage.RunRecord{
			ID: "run_legacy_scope", LoopID: revLoop.ID, Status: "success", CheckpointJSON: &cpStr,
			StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO,
		}); err != nil {
			t.Fatalf("upsert run: %v", err)
		}
		cfg, _ := config.DefaultConfig(t.TempDir())
		cfg.HITL.Enabled = false
		cfg.Roles.Fixer.AutoDiscovery = true
		cfg.Roles.Reviewer.Discovery.Triggers.RequireReviewRequest = false
		// Exercise follow-up sweep path (openPRs empty; listFollowUpLoops finds paused scope loop).
		github := &fakeGitHubGateway{
			currentLogin:    "looper-bot",
			listOpenByLabel: map[string][]PullRequestSummary{"": {}},
			viewHeadSHA:     "head_v2",
			author:          "alice",
		}
		runner := New(Options{
			DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{},
			Logger: fixture.logger, Now: fixture.now, CustomInstructions: &cfg,
			DiscoveryPolicy: DiscoveryPolicy{AutoDiscovery: true, RequireReviewRequest: false, EnableSelfReview: true},
			LoopConfig:      testReviewerLoopConfig(),
		})
		res, err := runner.DiscoverPullRequests(ctx, DiscoveryInput{ProjectID: "project_1", Repo: repo})
		if err != nil {
			t.Fatalf("DiscoverPullRequests: %v", err)
		}
		if len(res.QueueItems) != 1 {
			t.Fatalf("QueueItems = %#v, want 1 fresh item after new head_v2", res.QueueItems)
		}
		if res.QueueItems[0].PayloadJSON == nil || !strings.Contains(*res.QueueItems[0].PayloadJSON, `"head_v2"`) {
			t.Fatalf("queue payload = %v, want head_v2", res.QueueItems[0].PayloadJSON)
		}
		revAfter, _ := fixture.repos.Loops.GetByID(ctx, revLoop.ID)
		if revAfter == nil || revAfter.Status != "queued" || loops.IsReviewScopeHumanHold(*revAfter) {
			t.Fatalf("reviewer after new-head wake = %#v, want queued with no scope hold", revAfter)
		}
		if got := loops.ReviewerPublishCount(revAfter.MetadataJSON); got != 2 {
			t.Fatalf("reviewer iterationCount = %d, want 2 preserved", got)
		}
		fixAfter, _ := fixture.repos.Loops.GetByID(ctx, fixLoop.ID)
		if fixAfter == nil || fixAfter.Status != "waiting" || loops.IsReviewScopeHumanHold(*fixAfter) {
			t.Fatalf("fixer sibling after new-head wake = %#v, want waiting with no scope hold", fixAfter)
		}
		if got := loops.FixerPushCount(fixAfter.MetadataJSON); got != 1 {
			t.Fatalf("fixer pushCount = %d, want 1 preserved", got)
		}
	})

	t.Run("forgejo_explicit_rereview_wakes_scope_hold_on_same_head", func(t *testing.T) {
		t.Parallel()
		fixture := newRunnerFixture(t)
		ctx := context.Background()
		nowISO := fixture.nowISO()
		repo := "acme/looper"
		prNumber := int64(42)
		const head = "same-head"
		const rereviewReqID int64 = 999
		revMeta := `{"followUpdates":true,"loop":{"enabled":true,"iterationCount":1},"lastPublishedHeadSha":"same-head","pauseReason":"review_scope_human_required","reviewScopeHuman":{"heldBy":"reviewer","pauseReason":"review_scope_human_required","headSha":"same-head"}}`
		revLoop := storage.LoopRecord{
			ID: "loop_forgejo_rereview_wake", Seq: 313, ProjectID: "project_1", Type: "reviewer",
			TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber,
			Status: "paused", MetadataJSON: &revMeta, CreatedAt: nowISO, UpdatedAt: nowISO,
		}
		if err := fixture.repos.Loops.Upsert(ctx, revLoop); err != nil {
			t.Fatalf("upsert rev: %v", err)
		}
		cfg, _ := config.DefaultConfig(t.TempDir())
		cfg.HITL.Enabled = false
		cfg.Roles.Fixer.AutoDiscovery = true
		cfg.Providers = []config.ProviderConfig{{ID: "fj", Kind: config.ProviderKindForgejo}}
		cfg.Projects = []config.ProjectRefConfig{{ID: "project_1", Name: "Forgejo", Provider: "fj", Repo: repo, RepoPath: t.TempDir()}}
		reviews := []map[string]any{
			{"id": int64(500), "state": "COMMENTED", "commit": map[string]any{"oid": head}, "author": map[string]any{"login": "looper-bot"}},
			{"id": rereviewReqID, "state": "REQUEST_REVIEW", "author": map[string]any{"login": "looper-bot"}},
		}
		github := &fakeGitHubGateway{
			currentLogin:   "looper-bot",
			reviewRequests: []string{"looper-bot"},
			viewHeadSHA:    head,
			author:         "alice",
			reviews:        reviews,
			reviewRequestedPullRequests: []PullRequestSummary{{
				Number: prNumber, State: "OPEN", HeadSHA: head, Author: "alice",
				ReviewRequests: []string{"looper-bot"}, Reviews: reviews,
			}},
		}
		runner := New(Options{
			DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{},
			Logger: fixture.logger, Now: fixture.now, CustomInstructions: &cfg,
			DiscoveryPolicy: DiscoveryPolicy{AutoDiscovery: true, RequireReviewRequest: true, EnableSelfReview: false},
			LoopConfig:      testReviewerLoopConfig(),
		})
		res, err := runner.DiscoverPullRequests(ctx, DiscoveryInput{ProjectID: "project_1", Repo: repo})
		if err != nil {
			t.Fatalf("DiscoverPullRequests: %v", err)
		}
		if len(res.QueueItems) != 1 {
			t.Fatalf("QueueItems = %#v, want 1 item after explicit REQUEST_REVIEW", res.QueueItems)
		}
		if got := forgejoRereviewIDFromQueue(res.QueueItems[0].PayloadJSON); got != rereviewReqID {
			t.Fatalf("forgejoRereviewId = %d, want %d", got, rereviewReqID)
		}
		after, _ := fixture.repos.Loops.GetByID(ctx, revLoop.ID)
		if after == nil || after.Status != "queued" || loops.IsReviewScopeHumanHold(*after) {
			t.Fatalf("loop after Forgejo rereview wake = %#v, want queued without scope hold", after)
		}
	})

	t.Run("legacy_standalone_scope_hold_self_heals", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name              string
			lastPublishedHead string
			wantQueueItems    int
			wantLoopStatus    string
		}{
			{name: "unpublished_pure_needs_human_requeues", lastPublishedHead: "", wantQueueItems: 1, wantLoopStatus: "queued"},
			{name: "already_published_same_head_returns_to_waiting", lastPublishedHead: "head_v1", wantQueueItems: 0, wantLoopStatus: "waiting"},
		} {
			tc := tc
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				fixture := newRunnerFixture(t)
				ctx := context.Background()
				nowISO := fixture.nowISO()
				repo := "acme/looper"
				prNumber := int64(42)
				lastPubField := ""
				if tc.lastPublishedHead != "" {
					lastPubField = `"lastPublishedHeadSha":"` + tc.lastPublishedHead + `",`
				}
				meta := `{"followUpdates":true,"loop":{"enabled":true},` + lastPubField + `"lastReviewedSignalFingerprint":"` + ComputeReviewSignalFingerprintForLogin("head_v1", nil, "looper-bot") + `","pauseReason":"review_scope_human_required","reviewScopeHuman":{"heldBy":"reviewer","pauseReason":"review_scope_human_required","headSha":"head_v1"}}`
				loop := storage.LoopRecord{
					ID: "loop_legacy_standalone_" + tc.name, Seq: 314, ProjectID: "project_1", Type: "reviewer",
					TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber,
					Status: "paused", MetadataJSON: &meta, CreatedAt: nowISO, UpdatedAt: nowISO,
				}
				if err := fixture.repos.Loops.Upsert(ctx, loop); err != nil {
					t.Fatalf("upsert: %v", err)
				}
				cfg := standaloneReviewerTestConfig(t)
				cfg.Roles.Reviewer.Discovery.Triggers.RequireReviewRequest = false
				github := &fakeGitHubGateway{
					currentLogin:    "looper-bot",
					viewHeadSHA:     "head_v1",
					author:          "alice",
					listOpenByLabel: map[string][]PullRequestSummary{"": {{Number: prNumber, State: "OPEN", HeadSHA: "head_v1", Author: "alice"}}},
				}
				runner := New(Options{
					DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{},
					Logger: fixture.logger, Now: fixture.now, CustomInstructions: &cfg,
					DiscoveryPolicy: DiscoveryPolicy{AutoDiscovery: true, RequireReviewRequest: false, EnableSelfReview: true},
					LoopConfig:      testReviewerLoopConfig(),
				})
				res, err := runner.DiscoverPullRequests(ctx, DiscoveryInput{ProjectID: "project_1", Repo: repo})
				if err != nil {
					t.Fatalf("DiscoverPullRequests: %v", err)
				}
				if len(res.QueueItems) != tc.wantQueueItems {
					t.Fatalf("QueueItems = %d, want %d", len(res.QueueItems), tc.wantQueueItems)
				}
				after, _ := fixture.repos.Loops.GetByID(ctx, loop.ID)
				if after == nil || after.Status != tc.wantLoopStatus || loops.IsReviewScopeHumanHold(*after) {
					t.Fatalf("legacy standalone loop = %#v, want status=%s with no scope hold", after, tc.wantLoopStatus)
				}
			})
		}
	})

	t.Run("budget_hold_is_not_bypassed_by_new_head", func(t *testing.T) {
		t.Parallel()
		fixture := newRunnerFixture(t)
		ctx := context.Background()
		nowISO := fixture.nowISO()
		repo := "acme/looper"
		prNumber := int64(42)
		meta := `{"followUpdates":true,"loop":{"enabled":true,"iterationCount":3},"pauseReason":"review_fix_budget_exhausted","reviewFixBudget":{"exhaustedBy":"reviewer","pauseReason":"review_fix_budget_exhausted"},"reviewScopeHuman":{"heldBy":"reviewer","pauseReason":"review_scope_human_required","headSha":"head_v1"}}`
		loop := storage.LoopRecord{
			ID: "loop_budget_and_scope", Seq: 315, ProjectID: "project_1", Type: "reviewer",
			TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber,
			Status: "paused", MetadataJSON: &meta, CreatedAt: nowISO, UpdatedAt: nowISO,
		}
		if err := fixture.repos.Loops.Upsert(ctx, loop); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		cfg := standaloneReviewerTestConfig(t)
		cfg.Roles.Reviewer.Behavior.Loop.MaxPublishesPerPR = 3
		cfg.Roles.Reviewer.Discovery.Triggers.RequireReviewRequest = false
		github := &fakeGitHubGateway{
			currentLogin:    "looper-bot",
			viewHeadSHA:     "head_v2",
			author:          "alice",
			listOpenByLabel: map[string][]PullRequestSummary{"": {{Number: prNumber, State: "OPEN", HeadSHA: "head_v2", Author: "alice"}}},
		}
		runner := New(Options{
			DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{},
			Logger: fixture.logger, Now: fixture.now, CustomInstructions: &cfg,
			DiscoveryPolicy: DiscoveryPolicy{AutoDiscovery: true, RequireReviewRequest: false, EnableSelfReview: true},
			LoopConfig:      cfg.Roles.Reviewer.Behavior.Loop,
		})
		res, err := runner.DiscoverPullRequests(ctx, DiscoveryInput{ProjectID: "project_1", Repo: repo})
		if err != nil {
			t.Fatalf("DiscoverPullRequests: %v", err)
		}
		if len(res.QueueItems) != 0 {
			t.Fatalf("QueueItems = %#v, want 0 (budget hold must not be bypassed)", res.QueueItems)
		}
		after, _ := fixture.repos.Loops.GetByID(ctx, loop.ID)
		if after == nil || !loops.IsReviewFixBudgetHold(*after) {
			t.Fatalf("loop = %#v, want still budget-held", after)
		}
	})
}

func TestBuildReviewPromptStandaloneVsPairMode(t *testing.T) {
	t.Parallel()
	cfg, _ := config.DefaultConfig(t.TempDir())
	checkpoint := reviewerCheckpoint{Snapshot: &checkpointSnapshot{HeadSHA: "abc123"}}
	events := config.ReviewerReviewEventsConfig{Clean: config.ReviewerReviewEventComment, Blocking: config.ReviewerReviewEventRequestChanges}

	pairNative, _ := buildReviewPromptForPairMode("p1", cfg, "acme/looper", 42, checkpoint, "run_1", "reviewer:l:abc123", events, false, true, "", config.ReviewerScopeChangedRanges, config.DefaultDisclosureConfig(), "codex", "", "/opt/looper/bin/looper", false, false, "", "", true)
	if !strings.Contains(pairNative, "`needs_human` parks the pair") {
		t.Fatalf("pair native prompt missing pair park wording")
	}

	standaloneNative, _ := buildReviewPromptForPairMode("p1", cfg, "acme/looper", 42, checkpoint, "run_1", "reviewer:l:abc123", events, false, true, "", config.ReviewerScopeChangedRanges, config.DefaultDisclosureConfig(), "codex", "", "/opt/looper/bin/looper", false, false, "", "", false)
	for _, want := range []string{
		"standalone reviewer mode (no active Fixer)",
		"surface `needs_human` findings directly on the PR as inline review comments",
		"`disposition` (`must_fix` or `needs_human`)",
	} {
		if !strings.Contains(standaloneNative, want) {
			t.Fatalf("standalone native prompt missing %q", want)
		}
	}
	if strings.Contains(standaloneNative, "`needs_human` parks the pair") {
		t.Fatalf("standalone native prompt must not tell agent needs_human parks the pair")
	}

	standaloneCommentOnly, _ := buildReviewPromptForPairMode("p1", cfg, "acme/looper", 42, checkpoint, "run_1", "reviewer:l:abc123", events, false, true, "", config.ReviewerScopeChangedRanges, config.DefaultDisclosureConfig(), "codex", "", "/opt/looper/bin/looper", false, true, "", "", false)
	if !strings.Contains(standaloneCommentOnly, "publishes both `must_fix` and `needs_human` findings into the remote Reviewer Summary") {
		t.Fatalf("standalone comment-only prompt missing needs_human summary publishing wording")
	}
}
