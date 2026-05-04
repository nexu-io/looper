package reviewer

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/powerformer/looper/internal/config"
	"github.com/powerformer/looper/internal/eventlog"
	"github.com/powerformer/looper/internal/infra/specpr"
	"github.com/powerformer/looper/internal/storage"
)

func TestDiscoverPullRequestsCreatesLoopAndQueue(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{}
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
	if loop == nil || loop.Status != "queued" || loop.Repo == nil || *loop.Repo != "acme/looper" {
		t.Fatalf("loop = %#v, want queued reviewer loop", loop)
	}
	queue, err := fixture.repos.Queue.GetByID(context.Background(), result.QueueItems[0].ID)
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	if queue == nil || queue.Status != "queued" || queue.DedupeKey != "reviewer:project_1:"+result.CreatedLoopIDs[0]+":acme/looper:42" {
		t.Fatalf("queue = %#v, want queued reviewer item", queue)
	}
}

func TestDiscoverPullRequestsRecoversRetryableAfterResumeRestartFromDiscover(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	loopID, queueID := seedFailedReviewerRecoveryLoop(t, fixture, failedReviewerRecoverySeed{ResumePolicy: "restart_from_discover", QueueErrorKind: string(FailureRetryableAfterResume), ErrorMessage: "PR head changed before publish: expected old, got new"})
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: &fakeGitHubGateway{}, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now, LoopConfig: config.ReviewerLoopConfig{EnabledByDefault: true, QuietPeriodSeconds: 120, MaxIterationsPerPR: 20, MaxIterationsPerHead: 1, MaxWallClockSeconds: 14400, MaxConsecutiveFailures: 3, MaxAgentExecutionsPerPR: 25}})

	result, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	if len(result.QueueItems) != 1 || result.QueueItems[0].ID != queueID {
		t.Fatalf("QueueItems = %#v, want recovered existing queue item %s", result.QueueItems, queueID)
	}
	loop, _ := fixture.repos.Loops.GetByID(context.Background(), loopID)
	queue, _ := fixture.repos.Queue.GetByID(context.Background(), queueID)
	if loop == nil || loop.Status != "queued" || queue == nil || queue.Status != "queued" || queue.LastError != nil || queue.LastErrorKind != nil {
		t.Fatalf("loop=%#v queue=%#v, want queued loop and cleared failed queue metadata", loop, queue)
	}

	again, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("second DiscoverPullRequests() error = %v", err)
	}
	queues, _ := fixture.repos.Queue.List(context.Background())
	if len(again.QueueItems) != 1 || len(queues) != 1 {
		t.Fatalf("second result=%#v queues=%#v, want idempotent single active queue", again, queues)
	}
}

func TestReviewerFailedLoopRecoveryEligibilityWhitelist(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		seed failedReviewerRecoverySeed
		pr   PullRequestSummary
		want bool
	}{
		{name: "retryable rerun review", seed: failedReviewerRecoverySeed{ResumePolicy: "rerun_review", QueueErrorKind: string(FailureRetryableAfterResume), ErrorMessage: "marker missing"}, pr: PullRequestSummary{Number: 42, State: "OPEN"}, want: true},
		{name: "historical guardrail non retryable", seed: failedReviewerRecoverySeed{ResumePolicy: "replay_step", QueueErrorKind: string(FailureNonRetryable), ErrorMessage: "review request removed before publish"}, pr: PullRequestSummary{Number: 42, State: "OPEN"}, want: true},
		{name: "manual intervention", seed: failedReviewerRecoverySeed{ResumePolicy: "manual_intervention", QueueErrorKind: string(FailureManualIntervention), ErrorMessage: "operator needed"}, pr: PullRequestSummary{Number: 42, State: "OPEN"}, want: false},
		{name: "closed pr", seed: failedReviewerRecoverySeed{ResumePolicy: "restart_from_discover", QueueErrorKind: string(FailureRetryableAfterResume), ErrorMessage: "PR head changed before publish"}, pr: PullRequestSummary{Number: 42, State: "CLOSED"}, want: false},
		{name: "approved by current user on head", seed: failedReviewerRecoverySeed{ResumePolicy: "restart_from_discover", QueueErrorKind: string(FailureRetryableAfterResume), ErrorMessage: "PR head changed before publish"}, pr: PullRequestSummary{Number: 42, State: "OPEN", ReviewDecision: "APPROVED", HeadSHA: "abc123", Reviews: []map[string]any{{"author": map[string]any{"login": "octocat"}, "state": "APPROVED", "commit": map[string]any{"oid": "abc123"}}}}, want: false},
		{name: "approved by another user", seed: failedReviewerRecoverySeed{ResumePolicy: "restart_from_discover", QueueErrorKind: string(FailureRetryableAfterResume), ErrorMessage: "PR head changed before publish"}, pr: PullRequestSummary{Number: 42, State: "OPEN", ReviewDecision: "APPROVED", HeadSHA: "abc123", Reviews: []map[string]any{{"author": map[string]any{"login": "other"}, "state": "APPROVED", "commit": map[string]any{"oid": "abc123"}}}}, want: true},
		{name: "ready label", seed: failedReviewerRecoverySeed{ResumePolicy: "restart_from_discover", QueueErrorKind: string(FailureRetryableAfterResume), ErrorMessage: "PR head changed before publish"}, pr: PullRequestSummary{Number: 42, State: "OPEN", Labels: []string{specpr.ReadyLabel}}, want: false},
		{name: "follow updates disabled", seed: failedReviewerRecoverySeed{ResumePolicy: "restart_from_discover", QueueErrorKind: string(FailureRetryableAfterResume), ErrorMessage: "PR head changed before publish", FollowUpdates: boolPtr(false)}, pr: PullRequestSummary{Number: 42, State: "OPEN"}, want: false},
		{name: "loop disabled", seed: failedReviewerRecoverySeed{ResumePolicy: "restart_from_discover", QueueErrorKind: string(FailureRetryableAfterResume), ErrorMessage: "PR head changed before publish", LoopEnabled: boolPtr(false)}, pr: PullRequestSummary{Number: 42, State: "OPEN"}, want: false},
		{name: "max failure budget", seed: failedReviewerRecoverySeed{ResumePolicy: "restart_from_discover", QueueErrorKind: string(FailureRetryableAfterResume), ErrorMessage: "PR head changed before publish", ConsecutiveFailures: 3}, pr: PullRequestSummary{Number: 42, State: "OPEN"}, want: false},
		{name: "attempt cap", seed: failedReviewerRecoverySeed{ResumePolicy: "restart_from_discover", QueueErrorKind: string(FailureRetryableAfterResume), ErrorMessage: "PR head changed before publish", AutoRecoveryAttempts: maxReviewerAutoRecoveryAttempts}, pr: PullRequestSummary{Number: 42, State: "OPEN"}, want: false},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fixture := newRunnerFixture(t)
			loopID, _ := seedFailedReviewerRecoveryLoop(t, fixture, tt.seed)
			runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: &fakeGitHubGateway{}, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now, LoopConfig: config.ReviewerLoopConfig{EnabledByDefault: true, QuietPeriodSeconds: 120, MaxIterationsPerPR: 20, MaxIterationsPerHead: 1, MaxWallClockSeconds: 14400, MaxConsecutiveFailures: 3, MaxAgentExecutionsPerPR: 25, StopOnApproved: true, StopOnReadyLabel: true}})
			loop, _ := fixture.repos.Loops.GetByID(context.Background(), loopID)
			eligible, _, _, err := runner.failedReviewerLoopRecoveryEligibility(context.Background(), *loop, tt.pr)
			if err != nil {
				t.Fatalf("failedReviewerLoopRecoveryEligibility() error = %v", err)
			}
			if eligible != tt.want {
				t.Fatalf("eligible = %v, want %v", eligible, tt.want)
			}
		})
	}
}

func TestReviewerFailedLoopRecoveryEligibilityHonorsStopOnConfig(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		loopConfig config.ReviewerLoopConfig
		pr         PullRequestSummary
	}{
		{name: "approved allowed", loopConfig: config.ReviewerLoopConfig{EnabledByDefault: true, QuietPeriodSeconds: 120, MaxIterationsPerPR: 20, MaxIterationsPerHead: 1, MaxWallClockSeconds: 14400, MaxConsecutiveFailures: 3, MaxAgentExecutionsPerPR: 25, StopOnApproved: false, StopOnReadyLabel: true}, pr: PullRequestSummary{Number: 42, State: "OPEN", ReviewDecision: "APPROVED"}},
		{name: "ready label allowed", loopConfig: config.ReviewerLoopConfig{EnabledByDefault: true, QuietPeriodSeconds: 120, MaxIterationsPerPR: 20, MaxIterationsPerHead: 1, MaxWallClockSeconds: 14400, MaxConsecutiveFailures: 3, MaxAgentExecutionsPerPR: 25, StopOnApproved: true, StopOnReadyLabel: false}, pr: PullRequestSummary{Number: 42, State: "OPEN", Labels: []string{specpr.ReadyLabel}}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fixture := newRunnerFixture(t)
			loopID, _ := seedFailedReviewerRecoveryLoop(t, fixture, failedReviewerRecoverySeed{ResumePolicy: "restart_from_discover", QueueErrorKind: string(FailureRetryableAfterResume), ErrorMessage: "PR head changed before publish: expected old, got new"})
			runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: &fakeGitHubGateway{}, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now, LoopConfig: tt.loopConfig})
			loop, _ := fixture.repos.Loops.GetByID(context.Background(), loopID)
			eligible, _, _, err := runner.failedReviewerLoopRecoveryEligibility(context.Background(), *loop, tt.pr)
			if err != nil {
				t.Fatalf("failedReviewerLoopRecoveryEligibility() error = %v", err)
			}
			if !eligible {
				t.Fatalf("eligible = false, want true")
			}
		})
	}
}

func TestReviewerFailedLoopRecoveryEligibilitySkipsCurrentLoginForLocalBlockers(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		seed       failedReviewerRecoverySeed
		wantReason string
	}{
		{name: "loop disabled", seed: failedReviewerRecoverySeed{ResumePolicy: "restart_from_discover", QueueErrorKind: string(FailureRetryableAfterResume), ErrorMessage: "PR head changed before publish", LoopEnabled: boolPtr(false)}, wantReason: "loop_disabled"},
		{name: "max consecutive failures", seed: failedReviewerRecoverySeed{ResumePolicy: "restart_from_discover", QueueErrorKind: string(FailureRetryableAfterResume), ErrorMessage: "PR head changed before publish", ConsecutiveFailures: 3}, wantReason: "max_consecutive_failures"},
		{name: "attempt cap", seed: failedReviewerRecoverySeed{ResumePolicy: "restart_from_discover", QueueErrorKind: string(FailureRetryableAfterResume), ErrorMessage: "PR head changed before publish", AutoRecoveryAttempts: maxReviewerAutoRecoveryAttempts}, wantReason: "auto_recovery_attempt_cap"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fixture := newRunnerFixture(t)
			github := &fakeGitHubGateway{currentLoginErr: fmt.Errorf("gh auth failed")}
			loopID, _ := seedFailedReviewerRecoveryLoop(t, fixture, tt.seed)
			runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now, LoopConfig: config.ReviewerLoopConfig{EnabledByDefault: true, QuietPeriodSeconds: 120, MaxIterationsPerPR: 20, MaxIterationsPerHead: 1, MaxWallClockSeconds: 14400, MaxConsecutiveFailures: 3, MaxAgentExecutionsPerPR: 25, StopOnApproved: true, StopOnReadyLabel: true}})
			loop, _ := fixture.repos.Loops.GetByID(context.Background(), loopID)
			eligible, _, reason, err := runner.failedReviewerLoopRecoveryEligibility(context.Background(), *loop, PullRequestSummary{Number: 42, State: "OPEN", HeadSHA: "abc123", Reviews: []map[string]any{{"author": map[string]any{"login": "octocat"}, "state": "APPROVED", "commit": map[string]any{"oid": "abc123"}}}})
			if err != nil {
				t.Fatalf("failedReviewerLoopRecoveryEligibility() error = %v", err)
			}
			if eligible {
				t.Fatalf("eligible = true, want false")
			}
			if reason != tt.wantReason {
				t.Fatalf("reason = %q, want %q", reason, tt.wantReason)
			}
			if github.currentLoginCalls != 0 {
				t.Fatalf("currentLoginCalls = %d, want 0", github.currentLoginCalls)
			}
		})
	}
}

func TestReviewerFailedLoopRecoveryEligibilitySkipsCurrentLoginForDeterministicBlockers(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		seed       failedReviewerRecoverySeed
		mutate     func(t *testing.T, fixture *runnerFixture, loopID string)
		wantReason string
	}{
		{
			name:       "latest queue not failed",
			seed:       failedReviewerRecoverySeed{ResumePolicy: "restart_from_discover", QueueErrorKind: string(FailureRetryableAfterResume), ErrorMessage: "PR head changed before publish"},
			wantReason: "latest_queue_not_failed",
			mutate: func(t *testing.T, fixture *runnerFixture, loopID string) {
				t.Helper()
				queue, err := fixture.repos.Queue.GetLatestByLoopID(context.Background(), loopID)
				if err != nil || queue == nil {
					t.Fatalf("Queue.GetLatestByLoopID() = (%#v, %v), want queue", queue, err)
				}
				queue.Status = "queued"
				if err := fixture.repos.Queue.Upsert(context.Background(), *queue); err != nil {
					t.Fatalf("Queue.Upsert() error = %v", err)
				}
			},
		},
		{
			name:       "latest run not failed",
			seed:       failedReviewerRecoverySeed{ResumePolicy: "restart_from_discover", QueueErrorKind: string(FailureRetryableAfterResume), ErrorMessage: "PR head changed before publish"},
			wantReason: "latest_run_not_failed",
			mutate: func(t *testing.T, fixture *runnerFixture, loopID string) {
				t.Helper()
				run, err := fixture.repos.Runs.GetLatestByLoopID(context.Background(), loopID)
				if err != nil || run == nil {
					t.Fatalf("Runs.GetLatestByLoopID() = (%#v, %v), want run", run, err)
				}
				run.Status = "completed"
				if err := fixture.repos.Runs.Upsert(context.Background(), *run); err != nil {
					t.Fatalf("Runs.Upsert() error = %v", err)
				}
			},
		},
		{name: "manual intervention", seed: failedReviewerRecoverySeed{ResumePolicy: "manual_intervention", QueueErrorKind: string(FailureManualIntervention), ErrorMessage: "operator needed"}, wantReason: "manual_intervention"},
		{name: "not whitelisted", seed: failedReviewerRecoverySeed{ResumePolicy: "replay_step", QueueErrorKind: string(FailureNonRetryable), ErrorMessage: "marker missing"}, wantReason: "not_whitelisted"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fixture := newRunnerFixture(t)
			github := &fakeGitHubGateway{currentLoginErr: fmt.Errorf("gh auth failed")}
			loopID, _ := seedFailedReviewerRecoveryLoop(t, fixture, tt.seed)
			if tt.mutate != nil {
				tt.mutate(t, fixture, loopID)
			}
			runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now, LoopConfig: config.ReviewerLoopConfig{EnabledByDefault: true, QuietPeriodSeconds: 120, MaxIterationsPerPR: 20, MaxIterationsPerHead: 1, MaxWallClockSeconds: 14400, MaxConsecutiveFailures: 3, MaxAgentExecutionsPerPR: 25, StopOnApproved: true, StopOnReadyLabel: true}})
			loop, _ := fixture.repos.Loops.GetByID(context.Background(), loopID)
			eligible, _, reason, err := runner.failedReviewerLoopRecoveryEligibility(context.Background(), *loop, PullRequestSummary{Number: 42, State: "OPEN", HeadSHA: "abc123", Reviews: []map[string]any{{"author": map[string]any{"login": "octocat"}, "state": "APPROVED", "commit": map[string]any{"oid": "abc123"}}}})
			if err != nil {
				t.Fatalf("failedReviewerLoopRecoveryEligibility() error = %v", err)
			}
			if eligible {
				t.Fatalf("eligible = true, want false")
			}
			if reason != tt.wantReason {
				t.Fatalf("reason = %q, want %q", reason, tt.wantReason)
			}
			if github.currentLoginCalls != 0 {
				t.Fatalf("currentLoginCalls = %d, want 0", github.currentLoginCalls)
			}
		})
	}
}

func TestSummaryFromDetailPreservesReviewsForApprovalRecovery(t *testing.T) {
	t.Parallel()
	reviews := []map[string]any{{"author": map[string]any{"login": "octocat"}, "state": "APPROVED", "commit": map[string]any{"oid": "abc123"}}}

	summary := summaryFromDetail(PullRequestDetail{Number: 42, State: "OPEN", HeadSHA: "abc123", Reviews: reviews})

	if !hasApprovedReviewByAuthorForHead(summary.Reviews, "octocat", "abc123") {
		t.Fatalf("summary reviews = %#v, want current-user approved review preserved", summary.Reviews)
	}
}

func TestReviewDisclosureInstructionPreservesVisibleInlineGuidance(t *testing.T) {
	t.Parallel()
	disclosureCfg := config.DefaultDisclosureConfig()
	disclosureCfg.Channels.InlineCommentVisible = true

	instruction := reviewDisclosureInstruction(disclosureCfg, "opencode", "")
	if !strings.Contains(instruction, "Every inline review comment you post must also use looper's configured visible inline disclosure style") {
		t.Fatalf("instruction = %q, want visible inline disclosure guidance", instruction)
	}
	if strings.Contains(instruction, "Do not add looper disclosure footers or hidden looper stamp markers to inline review comments") {
		t.Fatalf("instruction = %q, should not suppress enabled inline disclosure", instruction)
	}
}

func TestReviewDisclosureInstructionUsesHiddenInlineMarkerWhenVisibleDisabled(t *testing.T) {
	t.Parallel()
	disclosureCfg := config.DefaultDisclosureConfig()
	disclosureCfg.Channels.InlineCommentVisible = false

	instruction := reviewDisclosureInstruction(disclosureCfg, "opencode", "")
	if !strings.Contains(instruction, "Every inline review comment you post must include only the hidden looper stamp marker") {
		t.Fatalf("instruction = %q, want hidden inline marker guidance", instruction)
	}
	if strings.Contains(instruction, "Do not add looper disclosure footers or hidden looper stamp markers to inline review comments") {
		t.Fatalf("instruction = %q, should not suppress enabled inline disclosure", instruction)
	}
}

func TestDiscoverPullRequestsAppliesLabelFilters(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{labels: []string{"needs-review", "spec"}, reviewRequests: []string{"octocat"}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now, DiscoveryPolicy: DiscoveryPolicy{AutoDiscovery: true, IncludeDrafts: false, RequireReviewRequest: true, Labels: []string{"needs-review", "spec"}, LabelMode: config.LabelModeAll, IncludeSpecReviewingLabel: false}})

	result, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	if len(result.QueueItems) != 1 || *result.QueueItems[0].PRNumber != 42 {
		t.Fatalf("QueueItems = %#v, want only matching PR #42", result.QueueItems)
	}
	if len(github.listCalls) != 1 || strings.Join(github.listCalls[0].Labels, ",") != "needs-review,spec" {
		t.Fatalf("list calls = %#v, want server-side multi-label filter", github.listCalls)
	}
}

func TestListOpenPullRequestsForDiscoveryCapsAnyModeLabelsToLimit(t *testing.T) {
	t.Parallel()
	github := &fakeGitHubGateway{listOpenByLabel: map[string][]PullRequestSummary{
		"needs-review": {
			{Number: 42, State: "OPEN", Labels: []string{"needs-review"}},
			{Number: 43, State: "OPEN", Labels: []string{"needs-review"}},
		},
		"spec": {
			{Number: 44, State: "OPEN", Labels: []string{"spec"}},
			{Number: 45, State: "OPEN", Labels: []string{"spec"}},
		},
	}}
	runner := New(Options{GitHub: github, DiscoveryPolicy: DiscoveryPolicy{Labels: []string{"needs-review", "spec"}, LabelMode: config.LabelModeAny}})

	prs, err := runner.listOpenPullRequestsForDiscovery(context.Background(), "acme/looper", "/tmp/repo", 2)
	if err != nil {
		t.Fatalf("listOpenPullRequestsForDiscovery() error = %v", err)
	}
	if len(prs) != 2 || prs[0].Number != 42 || prs[1].Number != 43 {
		t.Fatalf("prs = %#v, want first two unique PRs capped to limit", prs)
	}
	if len(github.listCalls) != 1 || github.listCalls[0].Label != "needs-review" {
		t.Fatalf("list calls = %#v, want discovery to stop after reaching limit", github.listCalls)
	}
}

func TestListOpenPullRequestsForDiscoveryCapsAnyModeLabelsToDefaultLimit(t *testing.T) {
	t.Parallel()
	firstPage := make([]PullRequestSummary, 30)
	for i := range firstPage {
		firstPage[i] = PullRequestSummary{Number: int64(42 + i), State: "OPEN", Labels: []string{"needs-review"}}
	}
	github := &fakeGitHubGateway{listOpenByLabel: map[string][]PullRequestSummary{
		"needs-review": firstPage,
		"spec":         {{Number: 99, State: "OPEN", Labels: []string{"spec"}}},
	}}
	runner := New(Options{GitHub: github, DiscoveryPolicy: DiscoveryPolicy{Labels: []string{"needs-review", "spec"}, LabelMode: config.LabelModeAny}})

	prs, err := runner.listOpenPullRequestsForDiscovery(context.Background(), "acme/looper", "/tmp/repo", 0)
	if err != nil {
		t.Fatalf("listOpenPullRequestsForDiscovery() error = %v", err)
	}
	if len(prs) != 30 {
		t.Fatalf("len(prs) = %d, want default cap", len(prs))
	}
	if len(github.listCalls) != 1 || github.listCalls[0].Label != "needs-review" || github.listCalls[0].Limit != 30 {
		t.Fatalf("list calls = %#v, want discovery to use and stop at default limit", github.listCalls)
	}
}

func TestDiscoverPullRequestsReturnsCurrentUserLookupError(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{currentLoginErr: fmt.Errorf("gh auth failed")}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})

	result, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err == nil || !strings.Contains(err.Error(), "gh auth failed") {
		t.Fatalf("DiscoverPullRequests() error = %v, want gh auth failed", err)
	}
	if len(result.QueueItems) != 0 || len(result.CreatedLoopIDs) != 0 {
		t.Fatalf("result = %#v, want no discovery results on auth error", result)
	}
}

func TestDiscoverPullRequestsPreservesPausedLoop(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})

	nowISO := fixture.nowISO()
	repo := "acme/looper"
	prNumber := int64(42)
	loop := storage.LoopRecord{ID: "loop_paused", Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: "paused", CreatedAt: nowISO, UpdatedAt: nowISO}
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

func TestDiscoverPullRequestsSkipsSpecLabelWhenCurrentUserIsNotRequested(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{labels: []string{specpr.ReviewingLabel}, reviewRequests: []string{"alice"}, currentLogin: "bob"}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})

	result, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	if len(result.CreatedLoopIDs) != 0 || len(result.QueueItems) != 0 {
		t.Fatalf("result = %#v, want no created loops or queue items", result)
	}
	items, err := fixture.repos.Queue.List(context.Background())
	if err != nil {
		t.Fatalf("Queue.List() error = %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("len(Queue.List()) = %d, want 0", len(items))
	}
}

func TestDiscoverPullRequestsAllowsSpecLabelWhenCurrentUserIsRequested(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{labels: []string{specpr.ReviewingLabel}, reviewRequests: []string{"bob"}, currentLogin: "bob"}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})

	result, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	if len(result.QueueItems) != 1 {
		t.Fatalf("len(QueueItems) = %d, want 1", len(result.QueueItems))
	}
}

func TestDiscoverPullRequestsSkipsAutomaticFollowUpWhenCurrentUserIsNotRequested(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{reviewRequests: []string{"alice"}, currentLogin: "bob"}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})
	nowISO := fixture.nowISO()
	repo := "acme/looper"
	prNumber := int64(42)
	metadata := `{"followUpdates":true}`
	loop := storage.LoopRecord{ID: "loop_follow", Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: "completed", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	result, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: repo})
	if err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	if len(result.QueueItems) != 0 {
		t.Fatalf("len(QueueItems) = %d, want 0", len(result.QueueItems))
	}
}

func TestDiscoverPullRequestsAllowsAutomaticFollowUpWhenCurrentUserIsRequested(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{reviewRequests: []string{"bob"}, currentLogin: "bob"}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})
	nowISO := fixture.nowISO()
	repo := "acme/looper"
	prNumber := int64(42)
	metadata := `{"followUpdates":true}`
	loop := storage.LoopRecord{ID: "loop_follow_requested", Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: "completed", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	result, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: repo})
	if err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	if len(result.QueueItems) != 1 {
		t.Fatalf("len(QueueItems) = %d, want 1", len(result.QueueItems))
	}
}

func TestDiscoverPullRequestsSkipsDraftFollowUpWithoutTerminatingLoop(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{reviewRequests: []string{"alice"}, currentLogin: "octocat", viewDraft: true}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})
	nowISO := fixture.nowISO()
	repo := "acme/looper"
	prNumber := int64(42)
	metadata := `{"followUpdates":true}`
	loop := storage.LoopRecord{ID: "loop_draft_follow", Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: "completed", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	result, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: repo})
	if err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	if len(result.QueueItems) != 0 {
		t.Fatalf("len(QueueItems) = %d, want 0", len(result.QueueItems))
	}
	persisted, err := fixture.repos.Loops.GetByID(context.Background(), loop.ID)
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if persisted == nil || persisted.Status != "completed" {
		t.Fatalf("loop = %#v, want completed follow-up loop preserved", persisted)
	}
}

func TestDiscoverPullRequestsDebouncesContinuousFollowUp(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{reviewRequests: []string{"octocat"}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now, LoopConfig: config.ReviewerLoopConfig{EnabledByDefault: true, QuietPeriodSeconds: 120, MaxIterationsPerPR: 20, MaxIterationsPerHead: 1, MaxWallClockSeconds: 14400, MaxConsecutiveFailures: 3, MaxAgentExecutionsPerPR: 25}})
	nowISO := fixture.nowISO()
	repo := "acme/looper"
	prNumber := int64(42)
	metadata := `{"followUpdates":true,"lastPublishedHeadSha":"old-head","loop":{"enabled":true,"iterationCount":1,"iterationsByHead":{"old-head":1}}}`
	loop := storage.LoopRecord{ID: "loop_debounce", Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: "waiting", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	result, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: repo})
	if err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	if len(result.QueueItems) != 1 {
		t.Fatalf("len(QueueItems) = %d, want 1", len(result.QueueItems))
	}
	wantAvailableAt := eventlog.FormatJavaScriptISOString(fixture.now().Add(120 * time.Second))
	if result.QueueItems[0].AvailableAt != wantAvailableAt {
		t.Fatalf("AvailableAt = %q, want %q", result.QueueItems[0].AvailableAt, wantAvailableAt)
	}
	persistedLoop, err := fixture.repos.Loops.GetByID(context.Background(), loop.ID)
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if persistedLoop == nil || persistedLoop.Status != "queued" || persistedLoop.NextRunAt == nil || *persistedLoop.NextRunAt != wantAvailableAt {
		t.Fatalf("loop after debounce = %#v, want queued with delayed next run", persistedLoop)
	}

	result, err = runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: repo})
	if err != nil {
		t.Fatalf("DiscoverPullRequests() second error = %v", err)
	}
	if len(result.QueueItems) != 1 || result.QueueItems[0].ID == "" {
		t.Fatalf("second result = %#v, want one deduped queued item", result)
	}
	items, err := fixture.repos.Queue.List(context.Background())
	if err != nil {
		t.Fatalf("Queue.List() error = %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("len(Queue.List()) = %d, want deduped single item", len(items))
	}
	if items[0].AvailableAt != wantAvailableAt {
		t.Fatalf("deduped AvailableAt = %q, want original %q", items[0].AvailableAt, wantAvailableAt)
	}

	fixture.advance(30 * time.Second)
	result, err = runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: repo})
	if err != nil {
		t.Fatalf("DiscoverPullRequests() third error = %v", err)
	}
	if len(result.QueueItems) != 1 {
		t.Fatalf("third len(QueueItems) = %d, want one deduped queued item", len(result.QueueItems))
	}
	if result.QueueItems[0].AvailableAt != wantAvailableAt {
		t.Fatalf("third AvailableAt = %q, want original %q", result.QueueItems[0].AvailableAt, wantAvailableAt)
	}
}

func TestDiscoverPullRequestsExtendsDebounceWhenQueuedFollowUpSeesNewHead(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{reviewRequests: []string{"octocat"}, listHeadSHA: "head-one"}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now, LoopConfig: config.ReviewerLoopConfig{EnabledByDefault: true, QuietPeriodSeconds: 120, MaxIterationsPerPR: 20, MaxIterationsPerHead: 1, MaxWallClockSeconds: 14400, MaxConsecutiveFailures: 3, MaxAgentExecutionsPerPR: 25}})
	nowISO := fixture.nowISO()
	repo := "acme/looper"
	prNumber := int64(42)
	metadata := `{"followUpdates":true,"lastPublishedHeadSha":"old-head","loop":{"enabled":true,"iterationCount":1,"iterationsByHead":{"old-head":1}}}`
	loop := storage.LoopRecord{ID: "loop_debounce_new_head", Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: "waiting", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	result, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: repo})
	if err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	if len(result.QueueItems) != 1 {
		t.Fatalf("len(QueueItems) = %d, want 1", len(result.QueueItems))
	}
	firstAvailableAt := eventlog.FormatJavaScriptISOString(fixture.now().Add(120 * time.Second))
	if result.QueueItems[0].AvailableAt != firstAvailableAt {
		t.Fatalf("AvailableAt = %q, want %q", result.QueueItems[0].AvailableAt, firstAvailableAt)
	}

	fixture.advance(30 * time.Second)
	github.listHeadSHA = "head-two"
	result, err = runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: repo})
	if err != nil {
		t.Fatalf("DiscoverPullRequests() second error = %v", err)
	}
	if len(result.QueueItems) != 1 {
		t.Fatalf("second len(QueueItems) = %d, want one deduped queued item", len(result.QueueItems))
	}
	extendedAvailableAt := eventlog.FormatJavaScriptISOString(fixture.now().Add(120 * time.Second))
	if result.QueueItems[0].AvailableAt != extendedAvailableAt {
		t.Fatalf("second AvailableAt = %q, want extended %q", result.QueueItems[0].AvailableAt, extendedAvailableAt)
	}
	items, err := fixture.repos.Queue.List(context.Background())
	if err != nil {
		t.Fatalf("Queue.List() error = %v", err)
	}
	if len(items) != 1 || items[0].AvailableAt != extendedAvailableAt {
		t.Fatalf("queue items = %#v, want single item rescheduled to %q", items, extendedAvailableAt)
	}
	persistedLoop, err := fixture.repos.Loops.GetByID(context.Background(), loop.ID)
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if persistedLoop == nil || persistedLoop.NextRunAt == nil || *persistedLoop.NextRunAt != extendedAvailableAt {
		t.Fatalf("loop after reschedule = %#v, want next run %q", persistedLoop, extendedAvailableAt)
	}
}

func TestDiscoverPullRequestsHonorsMinimumPublishInterval(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{reviewRequests: []string{"octocat"}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now, LoopConfig: config.ReviewerLoopConfig{EnabledByDefault: true, QuietPeriodSeconds: 120, MinPublishIntervalSeconds: 1800, MaxIterationsPerPR: 20, MaxIterationsPerHead: 1, MaxWallClockSeconds: 14400, MaxConsecutiveFailures: 3, MaxAgentExecutionsPerPR: 25}})
	nowISO := fixture.nowISO()
	repo := "acme/looper"
	prNumber := int64(42)
	lastPublishedAt := eventlog.FormatJavaScriptISOString(fixture.now().Add(-10 * time.Minute))
	metadata := fmt.Sprintf(`{"followUpdates":true,"lastPublishedHeadSha":"old-head","lastPublishedAt":%q,"loop":{"enabled":true,"iterationCount":1,"iterationsByHead":{"old-head":1}}}`, lastPublishedAt)
	loop := storage.LoopRecord{ID: "loop_min_interval", Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: "waiting", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	result, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: repo})
	if err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	if len(result.QueueItems) != 1 {
		t.Fatalf("len(QueueItems) = %d, want 1", len(result.QueueItems))
	}
	wantAvailableAt := eventlog.FormatJavaScriptISOString(fixture.now().Add(20 * time.Minute))
	if result.QueueItems[0].AvailableAt != wantAvailableAt {
		t.Fatalf("AvailableAt = %q, want min interval %q", result.QueueItems[0].AvailableAt, wantAvailableAt)
	}
}

func TestLoopEnabledTreatsLegacyMissingMetadataAsDisabled(t *testing.T) {
	t.Parallel()
	runner := New(Options{LoopConfig: config.ReviewerLoopConfig{EnabledByDefault: true, QuietPeriodSeconds: 120, MaxIterationsPerPR: 20, MaxIterationsPerHead: 1, MaxWallClockSeconds: 14400, MaxConsecutiveFailures: 3, MaxAgentExecutionsPerPR: 25}, ReviewEvents: config.ReviewerReviewEventsConfig{Clean: config.ReviewerReviewEventApprove, Blocking: config.ReviewerReviewEventRequestChanges}})

	if runner.loopEnabled(map[string]any{}) {
		t.Fatalf("loopEnabled(empty metadata) = true, want false for legacy persisted loop")
	}

	metadataJSON, err := runner.ensureLoopMetadataJSON(nil, "acme/looper", 42)
	if err != nil {
		t.Fatalf("ensureLoopMetadataJSON() error = %v", err)
	}
	meta := parseJSONObject(&metadataJSON)
	if !runner.loopEnabled(meta) {
		t.Fatalf("loopEnabled(ensured metadata) = false, want creation-time default true")
	}
	if enabled, ok := meta["followUpdates"].(bool); !ok || !enabled {
		t.Fatalf("followUpdates = %#v, want true", meta["followUpdates"])
	}
	reviewEvents, _ := meta["reviewEvents"].(map[string]any)
	if reviewEvents["clean"] != string(config.ReviewerReviewEventApprove) || reviewEvents["blocking"] != string(config.ReviewerReviewEventRequestChanges) {
		t.Fatalf("reviewEvents = %#v, want snapshotted decision policy", reviewEvents)
	}
	current := `{"reviewEvents":{"clean":"BOGUS","blocking":"APPROVE"}}`
	metadataJSON, err = runner.ensureLoopMetadataJSON(&current, "acme/looper", 42)
	if err == nil || !strings.Contains(err.Error(), "reviewEvents.clean") {
		t.Fatalf("ensureLoopMetadataJSON(invalid reviewEvents) error = %v, want validation error", err)
	}
	current = `{"reviewEvents":{"clean":123}}`
	metadataJSON, err = runner.ensureLoopMetadataJSON(&current, "acme/looper", 42)
	if err == nil || !strings.Contains(err.Error(), "reviewEvents.clean") {
		t.Fatalf("ensureLoopMetadataJSON(malformed reviewEvents) error = %v, want validation error", err)
	}
}

func TestEnsureLoopForPullRequestBackfillsLegacyFollowUpdatesDisabled(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: &fakeGitHubGateway{}, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now, LoopConfig: config.ReviewerLoopConfig{EnabledByDefault: true, QuietPeriodSeconds: 120, MaxIterationsPerPR: 20, MaxIterationsPerHead: 1, MaxWallClockSeconds: 14400, MaxConsecutiveFailures: 3, MaxAgentExecutionsPerPR: 25}})
	project, err := fixture.repos.Projects.GetByID(context.Background(), "project_1")
	if err != nil || project == nil {
		t.Fatalf("Projects.GetByID() = (%#v, %v), want project", project, err)
	}
	repo := "acme/looper"
	prNumber := int64(42)
	loop := storage.LoopRecord{ID: "loop_legacy", Seq: 1, ProjectID: project.ID, Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: "completed", CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}
	if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	result, err := runner.ensureLoopForPullRequest(context.Background(), *project, repo, prNumber, &loop)
	if err != nil {
		t.Fatalf("ensureLoopForPullRequest() error = %v", err)
	}
	meta := parseJSONObject(result.record.MetadataJSON)
	if runner.loopEnabled(meta) {
		t.Fatalf("loopEnabled(backfilled legacy metadata) = true, want false")
	}
	if enabled, ok := meta["followUpdates"].(bool); !ok || enabled {
		t.Fatalf("followUpdates = %#v, want false", meta["followUpdates"])
	}
}

func TestDiscoverPullRequestsDoesNotMarkSkippedExistingLoopQueued(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{reviewRequests: []string{"octocat"}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})
	nowISO := fixture.nowISO()
	repo := "acme/looper"
	prNumber := int64(42)
	nextRunAt := "2026-04-11T13:00:00.000Z"
	metadata := `{"followUpdates":true,"lastPublishedHeadSha":"abc123"}`
	loop := storage.LoopRecord{ID: "loop_same_head", Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: "completed", NextRunAt: &nextRunAt, MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	result, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: repo})
	if err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	if len(result.QueueItems) != 0 {
		t.Fatalf("len(QueueItems) = %d, want 0", len(result.QueueItems))
	}
	persisted, err := fixture.repos.Loops.GetByID(context.Background(), loop.ID)
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if persisted == nil || persisted.Status != "completed" || persisted.NextRunAt == nil || *persisted.NextRunAt != nextRunAt {
		t.Fatalf("loop after skipped discovery = %#v, want unchanged completed loop", persisted)
	}
}

func TestReviewerLoopBudgetTerminationReasons(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: &fakeGitHubGateway{}, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now, LoopConfig: config.ReviewerLoopConfig{EnabledByDefault: true, QuietPeriodSeconds: 120, MaxIterationsPerPR: 2, MaxIterationsPerHead: 1, MaxWallClockSeconds: 14400, MaxConsecutiveFailures: 3, MaxAgentExecutionsPerPR: 25}})
	metadata := `{"loop":{"iterationCount":1,"agentExecutionCount":1,"consecutiveFailures":0,"iterationsByHead":{"abc123":1},"startTime":"2026-04-11T12:00:00.000Z"}}`
	loop := storage.LoopRecord{ID: "loop_budget", MetadataJSON: &metadata}
	if got := runner.loopBudgetTerminationReason(loop, "abc123"); got != "max_iterations_per_head" {
		t.Fatalf("loopBudgetTerminationReason() = %q, want max_iterations_per_head", got)
	}

	metadata = `{"loop":{"iterationCount":2,"agentExecutionCount":1,"consecutiveFailures":0,"iterationsByHead":{"old":1},"startTime":"2026-04-11T12:00:00.000Z"}}`
	loop.MetadataJSON = &metadata
	if got := runner.loopBudgetTerminationReason(loop, "abc123"); got != "max_iterations_per_pr" {
		t.Fatalf("loopBudgetTerminationReason() = %q, want max_iterations_per_pr", got)
	}

	unlimitedRunner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: &fakeGitHubGateway{}, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now, LoopConfig: config.ReviewerLoopConfig{EnabledByDefault: true, QuietPeriodSeconds: 120, MaxIterationsPerPR: 20, MaxIterationsPerHead: 2, MaxWallClockSeconds: 0, MaxConsecutiveFailures: 3, MaxAgentExecutionsPerPR: 25}})
	metadata = `{"loop":{"iterationCount":1,"agentExecutionCount":1,"consecutiveFailures":0,"iterationsByHead":{"old":1},"startTime":"2026-04-11T12:00:00.000Z"}}`
	loop.MetadataJSON = &metadata
	if got := unlimitedRunner.loopBudgetTerminationReason(loop, "abc123"); got != "" {
		t.Fatalf("loopBudgetTerminationReason() = %q, want no termination when max wall clock is 0", got)
	}
}

func TestRunFilterStepSkipsAlreadyReviewedHeadBeforeBudgetTermination(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{reviewRequests: []string{"octocat"}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now, LoopConfig: config.ReviewerLoopConfig{EnabledByDefault: true, QuietPeriodSeconds: 120, MaxIterationsPerPR: 20, MaxIterationsPerHead: 1, MaxWallClockSeconds: 14400, MaxConsecutiveFailures: 3, MaxAgentExecutionsPerPR: 25}})
	nowISO := fixture.nowISO()
	repo := "acme/looper"
	prNumber := int64(42)
	metadata := `{"followUpdates":true,"lastPublishedHeadSha":"abc123","loop":{"enabled":true,"iterationCount":1,"iterationsByHead":{"abc123":1},"startTime":"2026-04-11T12:00:00.000Z"}}`
	loop := storage.LoopRecord{ID: "loop_same_head_at_budget", Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: "waiting", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	checkpoint, err := runner.runFilterStep(context.Background(), stepInput{Project: storage.ProjectRecord{ID: "project_1", RepoPath: "/tmp/repos/looper"}, Loop: loop, Repo: repo, PRNumber: prNumber, Checkpoint: reviewerCheckpoint{Detail: &checkpointDetail{State: "OPEN", HeadSHA: "abc123", ReviewRequests: []string{"octocat"}}}})
	if err != nil {
		t.Fatalf("runFilterStep() error = %v", err)
	}
	if !strings.Contains(checkpoint.SkipReason, "Skipped already-reviewed head abc123") {
		t.Fatalf("SkipReason = %q, want already-reviewed head skip", checkpoint.SkipReason)
	}
	persisted, err := fixture.repos.Loops.GetByID(context.Background(), loop.ID)
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if persisted == nil || persisted.Status != "waiting" {
		t.Fatalf("loop after filter = %#v, want waiting loop not terminated", persisted)
	}
}

func TestRunFilterStepSkipsConflictedPullRequest(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: &fakeGitHubGateway{}, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})
	repo := "acme/looper"
	prNumber := int64(42)

	checkpoint, err := runner.runFilterStep(context.Background(), stepInput{Project: storage.ProjectRecord{ID: "project_1", RepoPath: "/tmp/repos/looper"}, Repo: repo, PRNumber: prNumber, Checkpoint: reviewerCheckpoint{Detail: &checkpointDetail{State: "OPEN", HeadSHA: "abc123", HasConflicts: true}}})
	if err != nil {
		t.Fatalf("runFilterStep() error = %v", err)
	}
	if !strings.Contains(checkpoint.SkipReason, "Skipped conflicted pull request acme/looper#42") {
		t.Fatalf("SkipReason = %q, want conflicted PR skip", checkpoint.SkipReason)
	}
}

func TestRunFilterStepSkipsConflictedPullRequestBeforeLoginLookup(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{currentLoginErr: fmt.Errorf("gh auth failed")}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})
	repo := "acme/looper"
	prNumber := int64(42)
	reviews := []map[string]any{{"author": map[string]any{"login": "octocat"}, "state": "APPROVED", "commit": map[string]any{"oid": "abc123"}}}

	checkpoint, err := runner.runFilterStep(context.Background(), stepInput{Project: storage.ProjectRecord{ID: "project_1", RepoPath: "/tmp/repos/looper"}, Repo: repo, PRNumber: prNumber, Checkpoint: reviewerCheckpoint{Detail: &checkpointDetail{State: "OPEN", HeadSHA: "abc123", HasConflicts: true, ReviewDecision: "APPROVED", Reviews: reviews}}})
	if err != nil {
		t.Fatalf("runFilterStep() error = %v", err)
	}
	if checkpoint.SkipKind != "conflicted" {
		t.Fatalf("SkipKind = %q, want conflicted", checkpoint.SkipKind)
	}
	if github.currentLoginCalls != 0 {
		t.Fatalf("GetCurrentUserLogin calls = %d, want 0", github.currentLoginCalls)
	}
}

func TestRunFilterStepTerminatesLegacyApprovedWithoutReviewsBeforeConflictSkip(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: &fakeGitHubGateway{}, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})
	repo := "acme/looper"
	prNumber := int64(42)
	loop := storage.LoopRecord{ID: "loop_approved_conflicted", ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: "queued", CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}
	if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	checkpoint, err := runner.runFilterStep(context.Background(), stepInput{Project: storage.ProjectRecord{ID: "project_1", RepoPath: "/tmp/repos/looper"}, Loop: loop, Repo: repo, PRNumber: prNumber, Checkpoint: reviewerCheckpoint{Detail: &checkpointDetail{State: "OPEN", HeadSHA: "abc123", HasConflicts: true, ReviewDecision: "APPROVED"}}})
	if err != nil {
		t.Fatalf("runFilterStep() error = %v", err)
	}
	if !strings.Contains(checkpoint.SkipReason, "Terminated reviewer loop for approved pull request") {
		t.Fatalf("SkipReason = %q, want approved termination", checkpoint.SkipReason)
	}
	updated, err := fixture.repos.Loops.GetByID(context.Background(), loop.ID)
	if err != nil || updated == nil {
		t.Fatalf("Loops.GetByID() = (%#v, %v), want loop", updated, err)
	}
	if updated.Status != "terminated" {
		t.Fatalf("loop status = %q, want terminated", updated.Status)
	}
}

func TestRunFilterStepDoesNotTerminateWhenOnlyAnotherReviewerApproved(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{currentLogin: "octocat"}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})
	repo := "acme/looper"
	prNumber := int64(42)
	loop := storage.LoopRecord{ID: "loop_other_approved", ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: "queued", CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}
	if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	reviews := []map[string]any{{"author": map[string]any{"login": "other"}, "state": "APPROVED", "commit": map[string]any{"oid": "abc123"}}}

	checkpoint, err := runner.runFilterStep(context.Background(), stepInput{Project: storage.ProjectRecord{ID: "project_1", RepoPath: "/tmp/repos/looper"}, Loop: loop, Repo: repo, PRNumber: prNumber, Checkpoint: reviewerCheckpoint{Detail: &checkpointDetail{State: "OPEN", HeadSHA: "abc123", ReviewDecision: "APPROVED", ReviewRequests: []string{"octocat"}, Reviews: reviews}}})
	if err != nil {
		t.Fatalf("runFilterStep() error = %v", err)
	}
	if checkpoint.SkipKind == "approved" {
		t.Fatalf("SkipKind = %q, want no approved termination", checkpoint.SkipKind)
	}
	updated, err := fixture.repos.Loops.GetByID(context.Background(), loop.ID)
	if err != nil || updated == nil {
		t.Fatalf("Loops.GetByID() = (%#v, %v), want loop", updated, err)
	}
	if updated.Status == "terminated" {
		t.Fatalf("loop status = %q, want not terminated", updated.Status)
	}
}

func TestRunFilterStepTerminatesReadyBeforeConflictSkip(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: &fakeGitHubGateway{}, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})
	repo := "acme/looper"
	prNumber := int64(42)
	loop := storage.LoopRecord{ID: "loop_ready_conflicted", ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: "queued", CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}
	if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	checkpoint, err := runner.runFilterStep(context.Background(), stepInput{Project: storage.ProjectRecord{ID: "project_1", RepoPath: "/tmp/repos/looper"}, Loop: loop, Repo: repo, PRNumber: prNumber, Checkpoint: reviewerCheckpoint{Detail: &checkpointDetail{State: "OPEN", HeadSHA: "abc123", HasConflicts: true, Labels: []string{specpr.ReadyLabel}}}})
	if err != nil {
		t.Fatalf("runFilterStep() error = %v", err)
	}
	if !strings.Contains(checkpoint.SkipReason, "Terminated reviewer loop for ready pull request") {
		t.Fatalf("SkipReason = %q, want ready termination", checkpoint.SkipReason)
	}
	updated, err := fixture.repos.Loops.GetByID(context.Background(), loop.ID)
	if err != nil || updated == nil {
		t.Fatalf("Loops.GetByID() = (%#v, %v), want loop", updated, err)
	}
	if updated.Status != "terminated" {
		t.Fatalf("loop status = %q, want terminated", updated.Status)
	}
}

func TestRunFilterStepTerminatesReadyBeforeLoginLookup(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{currentLoginErr: fmt.Errorf("gh auth failed")}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})
	repo := "acme/looper"
	prNumber := int64(42)
	loop := storage.LoopRecord{ID: "loop_ready_login_error", ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: "queued", CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}
	if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	reviews := []map[string]any{{"author": map[string]any{"login": "octocat"}, "state": "APPROVED", "commit": map[string]any{"oid": "abc123"}}}

	checkpoint, err := runner.runFilterStep(context.Background(), stepInput{Project: storage.ProjectRecord{ID: "project_1", RepoPath: "/tmp/repos/looper"}, Loop: loop, Repo: repo, PRNumber: prNumber, Checkpoint: reviewerCheckpoint{Detail: &checkpointDetail{State: "OPEN", HeadSHA: "abc123", Labels: []string{specpr.ReadyLabel}, Reviews: reviews}}})
	if err != nil {
		t.Fatalf("runFilterStep() error = %v", err)
	}
	if checkpoint.SkipKind != "ready_label" {
		t.Fatalf("SkipKind = %q, want ready_label", checkpoint.SkipKind)
	}
	if github.currentLoginCalls != 0 {
		t.Fatalf("GetCurrentUserLogin calls = %d, want 0", github.currentLoginCalls)
	}
}

func TestRunFilterStepSkipsPullRequestAlreadyReviewedByCurrentUserForHead(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{currentLogin: "OctoCat"}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})
	repo := "acme/looper"
	prNumber := int64(42)
	reviews := []map[string]any{{"author": map[string]any{"login": "octocat"}, "state": "COMMENTED", "commit": map[string]any{"oid": "abc123"}}}

	checkpoint, err := runner.runFilterStep(context.Background(), stepInput{Project: storage.ProjectRecord{ID: "project_1", RepoPath: "/tmp/repos/looper"}, Repo: repo, PRNumber: prNumber, Checkpoint: reviewerCheckpoint{Detail: &checkpointDetail{State: "OPEN", HeadSHA: "abc123", Reviews: reviews}}})
	if err != nil {
		t.Fatalf("runFilterStep() error = %v", err)
	}
	if !strings.Contains(checkpoint.SkipReason, "current user already reviewed head abc123") {
		t.Fatalf("SkipReason = %q, want already-reviewed-by-current-user skip", checkpoint.SkipReason)
	}
}

func TestRunFilterStepRefreshesCurrentLoginBeforeAlreadyReviewedCheck(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{currentLogin: "new-user"}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})
	repo := "acme/looper"
	prNumber := int64(42)
	reviews := []map[string]any{{"author": map[string]any{"login": "old-user"}, "state": "COMMENTED", "commit": map[string]any{"oid": "abc123"}}}

	checkpoint, err := runner.runFilterStep(context.Background(), stepInput{Project: storage.ProjectRecord{ID: "project_1", RepoPath: "/tmp/repos/looper"}, Repo: repo, PRNumber: prNumber, Checkpoint: reviewerCheckpoint{Detail: &checkpointDetail{State: "OPEN", HeadSHA: "abc123", CurrentLogin: "old-user", ReviewRequests: []string{"new-user"}, Reviews: reviews}}})
	if err != nil {
		t.Fatalf("runFilterStep() error = %v", err)
	}
	if checkpoint.SkipReason != "" {
		t.Fatalf("SkipReason = %q, want no skip for stale checkpoint login", checkpoint.SkipReason)
	}
	if checkpoint.Detail.CurrentLogin != "new-user" {
		t.Fatalf("CurrentLogin = %q, want refreshed login", checkpoint.Detail.CurrentLogin)
	}
	if github.currentLoginCalls != 1 {
		t.Fatalf("GetCurrentUserLogin calls = %d, want 1", github.currentLoginCalls)
	}
}

func TestRunFilterStepTerminatesApprovedBeforeAlreadyReviewedSkip(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{currentLogin: "octocat"}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})
	repo := "acme/looper"
	prNumber := int64(42)
	loop := storage.LoopRecord{ID: "loop_approved_already_reviewed", ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: "queued", CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}
	if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	reviews := []map[string]any{{"author": map[string]any{"login": "octocat"}, "state": "APPROVED", "commit": map[string]any{"oid": "abc123"}}}

	checkpoint, err := runner.runFilterStep(context.Background(), stepInput{Project: storage.ProjectRecord{ID: "project_1", RepoPath: "/tmp/repos/looper"}, Loop: loop, Repo: repo, PRNumber: prNumber, Checkpoint: reviewerCheckpoint{Detail: &checkpointDetail{State: "OPEN", HeadSHA: "abc123", ReviewDecision: "APPROVED", Reviews: reviews}}})
	if err != nil {
		t.Fatalf("runFilterStep() error = %v", err)
	}
	if !strings.Contains(checkpoint.SkipReason, "Terminated reviewer loop for approved pull request") {
		t.Fatalf("SkipReason = %q, want approved termination", checkpoint.SkipReason)
	}
	updated, err := fixture.repos.Loops.GetByID(context.Background(), loop.ID)
	if err != nil || updated == nil {
		t.Fatalf("Loops.GetByID() = (%#v, %v), want loop", updated, err)
	}
	if updated.Status != "terminated" {
		t.Fatalf("loop status = %q, want terminated", updated.Status)
	}
}

func TestRunFilterStepRefreshesCurrentLoginBeforeApprovedCheck(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{currentLogin: "new-user"}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})
	repo := "acme/looper"
	prNumber := int64(42)
	loop := storage.LoopRecord{ID: "loop_stale_approved", ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: "queued", CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}
	if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	reviews := []map[string]any{{"author": map[string]any{"login": "old-user"}, "state": "APPROVED", "commit": map[string]any{"oid": "abc123"}}}

	checkpoint, err := runner.runFilterStep(context.Background(), stepInput{Project: storage.ProjectRecord{ID: "project_1", RepoPath: "/tmp/repos/looper"}, Loop: loop, Repo: repo, PRNumber: prNumber, Checkpoint: reviewerCheckpoint{Detail: &checkpointDetail{State: "OPEN", HeadSHA: "abc123", CurrentLogin: "old-user", ReviewRequests: []string{"new-user"}, Reviews: reviews}}})
	if err != nil {
		t.Fatalf("runFilterStep() error = %v", err)
	}
	if checkpoint.SkipReason != "" {
		t.Fatalf("SkipReason = %q, want no termination for stale checkpoint login", checkpoint.SkipReason)
	}
	if checkpoint.Detail.CurrentLogin != "new-user" {
		t.Fatalf("CurrentLogin = %q, want refreshed login", checkpoint.Detail.CurrentLogin)
	}
	updated, err := fixture.repos.Loops.GetByID(context.Background(), loop.ID)
	if err != nil || updated == nil {
		t.Fatalf("Loops.GetByID() = (%#v, %v), want loop", updated, err)
	}
	if updated.Status == "terminated" {
		t.Fatalf("loop status = %q, want not terminated", updated.Status)
	}
}

func TestRunFilterStepTerminatesReadyBeforeAlreadyReviewedSkip(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{currentLogin: "octocat"}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})
	repo := "acme/looper"
	prNumber := int64(42)
	loop := storage.LoopRecord{ID: "loop_ready_already_reviewed", ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: "queued", CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}
	if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	reviews := []map[string]any{{"author": map[string]any{"login": "octocat"}, "state": "COMMENTED", "commit": map[string]any{"oid": "abc123"}}}

	checkpoint, err := runner.runFilterStep(context.Background(), stepInput{Project: storage.ProjectRecord{ID: "project_1", RepoPath: "/tmp/repos/looper"}, Loop: loop, Repo: repo, PRNumber: prNumber, Checkpoint: reviewerCheckpoint{Detail: &checkpointDetail{State: "OPEN", HeadSHA: "abc123", Labels: []string{specpr.ReadyLabel}, Reviews: reviews}}})
	if err != nil {
		t.Fatalf("runFilterStep() error = %v", err)
	}
	if !strings.Contains(checkpoint.SkipReason, "Terminated reviewer loop for ready pull request") {
		t.Fatalf("SkipReason = %q, want ready termination", checkpoint.SkipReason)
	}
	updated, err := fixture.repos.Loops.GetByID(context.Background(), loop.ID)
	if err != nil || updated == nil {
		t.Fatalf("Loops.GetByID() = (%#v, %v), want loop", updated, err)
	}
	if updated.Status != "terminated" {
		t.Fatalf("loop status = %q, want terminated", updated.Status)
	}
}

func TestRunFilterStepAllowsManualReviewAlreadyReviewedByCurrentUserForHead(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{currentLogin: "octocat"}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})
	repo := "acme/looper"
	prNumber := int64(42)
	metadata := `{"manual":true}`
	loop := storage.LoopRecord{ID: "loop_manual_already_reviewed", ProjectID: "project_1", Type: "reviewer", MetadataJSON: &metadata}
	reviews := []map[string]any{{"author": map[string]any{"login": "octocat"}, "state": "COMMENTED", "commit": map[string]any{"oid": "abc123"}}}

	checkpoint, err := runner.runFilterStep(context.Background(), stepInput{Project: storage.ProjectRecord{ID: "project_1", RepoPath: "/tmp/repos/looper"}, Loop: loop, Repo: repo, PRNumber: prNumber, Checkpoint: reviewerCheckpoint{Detail: &checkpointDetail{State: "OPEN", HeadSHA: "abc123", Reviews: reviews}}})
	if err != nil {
		t.Fatalf("runFilterStep() error = %v", err)
	}
	if checkpoint.SkipReason != "" {
		t.Fatalf("SkipReason = %q, want no skip for manual run", checkpoint.SkipReason)
	}
}

func TestRunFilterStepAllowsReviewWhenOnlyCurrentHeadReviewIsUnsubmitted(t *testing.T) {
	t.Parallel()

	for _, state := range []string{"PENDING", "DISMISSED"} {
		t.Run(state, func(t *testing.T) {
			t.Parallel()
			fixture := newRunnerFixture(t)
			github := &fakeGitHubGateway{currentLogin: "octocat"}
			runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})
			repo := "acme/looper"
			prNumber := int64(42)
			reviews := []map[string]any{{"author": map[string]any{"login": "octocat"}, "state": state, "commit": map[string]any{"oid": "abc123"}}}

			checkpoint, err := runner.runFilterStep(context.Background(), stepInput{Project: storage.ProjectRecord{ID: "project_1", RepoPath: "/tmp/repos/looper"}, Repo: repo, PRNumber: prNumber, Checkpoint: reviewerCheckpoint{Detail: &checkpointDetail{State: "OPEN", HeadSHA: "abc123", ReviewRequests: []string{"octocat"}, Reviews: reviews}}})
			if err != nil {
				t.Fatalf("runFilterStep() error = %v", err)
			}
			if checkpoint.SkipReason != "" {
				t.Fatalf("SkipReason = %q, want no skip for %s review", checkpoint.SkipReason, state)
			}
		})
	}
}

func TestRunFilterStepAllowsReviewAfterHeadChanges(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{currentLogin: "octocat"}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})
	repo := "acme/looper"
	prNumber := int64(42)
	reviews := []map[string]any{{"author": map[string]any{"login": "octocat"}, "state": "COMMENTED", "commit": map[string]any{"oid": "old-head"}}}

	checkpoint, err := runner.runFilterStep(context.Background(), stepInput{Project: storage.ProjectRecord{ID: "project_1", RepoPath: "/tmp/repos/looper"}, Repo: repo, PRNumber: prNumber, Checkpoint: reviewerCheckpoint{Detail: &checkpointDetail{State: "OPEN", HeadSHA: "new-head", ReviewRequests: []string{"octocat"}, Reviews: reviews}}})
	if err != nil {
		t.Fatalf("runFilterStep() error = %v", err)
	}
	if checkpoint.SkipReason != "" {
		t.Fatalf("SkipReason = %q, want no skip for old-head review", checkpoint.SkipReason)
	}
}

func TestRunFilterStepAllowsReviewWhenReviewCommitMissing(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{currentLogin: "octocat"}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})
	repo := "acme/looper"
	prNumber := int64(42)
	reviews := []map[string]any{{"author": map[string]any{"login": "octocat"}, "state": "COMMENTED"}}

	checkpoint, err := runner.runFilterStep(context.Background(), stepInput{Project: storage.ProjectRecord{ID: "project_1", RepoPath: "/tmp/repos/looper"}, Repo: repo, PRNumber: prNumber, Checkpoint: reviewerCheckpoint{Detail: &checkpointDetail{State: "OPEN", HeadSHA: "abc123", ReviewRequests: []string{"octocat"}, Reviews: reviews}}})
	if err != nil {
		t.Fatalf("runFilterStep() error = %v", err)
	}
	if checkpoint.SkipReason != "" {
		t.Fatalf("SkipReason = %q, want no skip without review commit", checkpoint.SkipReason)
	}
}

func TestRunFilterStepDoesNotTerminateManualNoLoopReviewOnStaleBudget(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: &fakeGitHubGateway{}, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now, LoopConfig: config.ReviewerLoopConfig{EnabledByDefault: true, QuietPeriodSeconds: 120, MaxIterationsPerPR: 20, MaxIterationsPerHead: 1, MaxWallClockSeconds: 60, MaxConsecutiveFailures: 3, MaxAgentExecutionsPerPR: 25}})
	nowISO := fixture.nowISO()
	repo := "acme/looper"
	prNumber := int64(42)
	metadata := `{"manual":true,"followUpdates":false,"loop":{"enabled":false,"iterationCount":99,"agentExecutionCount":99,"consecutiveFailures":99,"iterationsByHead":{"abc123":99},"startTime":"2026-04-11T10:00:00.000Z"}}`
	loop := storage.LoopRecord{ID: "loop_manual_no_loop_stale", Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: "queued", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	checkpoint, err := runner.runFilterStep(context.Background(), stepInput{Project: storage.ProjectRecord{ID: "project_1", RepoPath: "/tmp/repos/looper"}, Loop: loop, Repo: repo, PRNumber: prNumber, Checkpoint: reviewerCheckpoint{Detail: &checkpointDetail{State: "OPEN", HeadSHA: "abc123", ReviewRequests: []string{"octocat"}}}})
	if err != nil {
		t.Fatalf("runFilterStep() error = %v", err)
	}
	if checkpoint.SkipReason != "" {
		t.Fatalf("SkipReason = %q, want no skip so the one-shot review can run", checkpoint.SkipReason)
	}
	persisted, err := fixture.repos.Loops.GetByID(context.Background(), loop.ID)
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if persisted == nil || persisted.Status != "queued" {
		t.Fatalf("loop after filter = %#v, want queued loop not terminated", persisted)
	}
}

func TestDiscoverPullRequestsAllowsManualFollowUpWithoutReviewRequest(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{reviewRequests: []string{"alice"}, currentLogin: "bob"}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})
	nowISO := fixture.nowISO()
	repo := "acme/looper"
	prNumber := int64(42)
	metadata := `{"followUpdates":true,"manual":true}`
	loop := storage.LoopRecord{ID: "loop_manual_follow", Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: "completed", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	result, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: repo})
	if err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	if len(result.QueueItems) != 1 {
		t.Fatalf("len(QueueItems) = %d, want 1", len(result.QueueItems))
	}
}

func TestDiscoverPullRequestsAllowsManualFollowUpWithoutMatchingLabels(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{labels: []string{"different-label"}, reviewRequests: []string{"alice"}, currentLogin: "bob"}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now, DiscoveryPolicy: DiscoveryPolicy{AutoDiscovery: true, IncludeDrafts: false, RequireReviewRequest: true, Labels: []string{"needs-review"}, LabelMode: config.LabelModeAll}})
	nowISO := fixture.nowISO()
	repo := "acme/looper"
	prNumber := int64(42)
	metadata := `{"followUpdates":true,"manual":true}`
	loop := storage.LoopRecord{ID: "loop_manual_follow_labels", Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: "completed", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	result, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: repo})
	if err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	if len(result.QueueItems) != 1 {
		t.Fatalf("len(QueueItems) = %d, want 1", len(result.QueueItems))
	}
}

func TestDiscoverPullRequestsPreservesDisabledLoopEnabledWhenFollowUpdatesAbsent(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: &fakeGitHubGateway{}, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now, LoopConfig: config.ReviewerLoopConfig{EnabledByDefault: true, QuietPeriodSeconds: 120, MaxIterationsPerPR: 20, MaxIterationsPerHead: 2, MaxWallClockSeconds: 14400, MaxConsecutiveFailures: 3, MaxAgentExecutionsPerPR: 25}})
	nowISO := fixture.nowISO()
	repo := "acme/looper"
	prNumber := int64(42)
	metadata := `{"loop":{"enabled":false}}`
	loop := storage.LoopRecord{ID: "loop_disabled_legacy", Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: "completed", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	result, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: repo})
	if err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	if len(result.QueueItems) != 0 {
		t.Fatalf("QueueItems = %#v, want none for disabled follow-up loop", result.QueueItems)
	}
	updated, err := fixture.repos.Loops.GetByID(context.Background(), loop.ID)
	if err != nil || updated == nil {
		t.Fatalf("Loops.GetByID() = (%#v, %v), want loop", updated, err)
	}
	updatedMeta := parseJSONObject(updated.MetadataJSON)
	if got, _ := updatedMeta["followUpdates"].(bool); got {
		t.Fatalf("followUpdates = true, want preserved false from loop.enabled")
	}
}

func TestDiscoverPullRequestsAllowsManualFollowUpAfterSkippedAutomaticLoopForSamePR(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{reviewRequests: []string{"alice"}, currentLogin: "bob"}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})
	nowISO := fixture.nowISO()
	repo := "acme/looper"
	prNumber := int64(42)
	automaticMetadata := `{"followUpdates":true}`
	manualMetadata := `{"followUpdates":true,"manual":true}`
	for _, loop := range []storage.LoopRecord{
		{ID: "loop_auto_follow", Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: "completed", MetadataJSON: &automaticMetadata, CreatedAt: nowISO, UpdatedAt: nowISO},
		{ID: "loop_manual_follow_after_auto", Seq: 2, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: "completed", MetadataJSON: &manualMetadata, CreatedAt: nowISO, UpdatedAt: nowISO},
	} {
		if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
			t.Fatalf("Loops.Upsert(%s) error = %v", loop.ID, err)
		}
	}

	result, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: repo})
	if err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	if len(result.QueueItems) != 1 {
		t.Fatalf("len(QueueItems) = %d, want 1", len(result.QueueItems))
	}
	if result.QueueItems[0].LoopID == nil || *result.QueueItems[0].LoopID != "loop_manual_follow_after_auto" {
		t.Fatalf("queue loopID = %#v, want manual follow-up loop", result.QueueItems[0].LoopID)
	}
}

func TestProcessClaimedItemSkipsQueuedAutomaticLoopWhenCurrentUserIsNotRequested(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{reviewRequests: []string{"alice"}, currentLogin: "bob"}
	agent := &fakeAgentExecutor{}
	git := &fakeGitGateway{}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now})
	nowISO := fixture.nowISO()
	repo := "acme/looper"
	prNumber := int64(42)
	loop := storage.LoopRecord{ID: "loop_api", Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: "queued", CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	queue, err := runner.enqueue(context.Background(), enqueueInput{ProjectID: "project_1", LoopID: loop.ID, Repo: repo, PRNumber: prNumber})
	if err != nil {
		t.Fatalf("enqueue() error = %v", err)
	}
	claimed, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claimed == nil || claimed.ID != queue.ID {
		t.Fatalf("ClaimNextOfType() = (%#v, %v), want queued item %s", claimed, err, queue.ID)
	}

	result, err := runner.ProcessClaimedItem(context.Background(), *claimed)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "skipped" || !strings.Contains(result.Summary, "not requested for review") {
		t.Fatalf("result = %#v, want skipped not requested", result)
	}
	if len(agent.starts) != 0 || len(git.createCalls) != 0 {
		t.Fatalf("agent starts=%d git creates=%d, want no review work", len(agent.starts), len(git.createCalls))
	}
	updatedLoop, err := fixture.repos.Loops.GetByID(context.Background(), result.LoopID)
	if err != nil || updatedLoop == nil {
		t.Fatalf("Loops.GetByID() = (%#v, %v), want loop", updatedLoop, err)
	}
	loopMeta := reviewerLoopMetadata(parseJSONObject(updatedLoop.MetadataJSON))
	if got := intFromAny(loopMeta["agentExecutionCount"]); got != 0 {
		t.Fatalf("agentExecutionCount = %d, want 0 for filter-only skip", got)
	}
	if got := intFromAny(loopMeta["iterationCount"]); got != 0 {
		t.Fatalf("iterationCount = %d, want 0 for filter-only skip", got)
	}
}

func TestDiscoverPullRequestsSuppressesRepeatedConflictSkipUntilHeadChanges(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{hasConflicts: true, reviewDecision: "REVIEW_REQUIRED", reviewRequests: []string{"octocat"}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now, LoopConfig: testReviewerLoopConfig()})
	repo := "acme/looper"

	first, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: repo})
	if err != nil || len(first.QueueItems) != 1 {
		t.Fatalf("first DiscoverPullRequests() = (%#v, %v), want one queue item", first, err)
	}
	claimed, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claimed == nil {
		t.Fatalf("ClaimNextOfType() = (%#v, %v), want claimed queue item", claimed, err)
	}
	processed, err := runner.ProcessClaimedItem(context.Background(), *claimed)
	if err != nil || processed.Status != "skipped" || !strings.Contains(processed.Summary, "conflicted") {
		t.Fatalf("ProcessClaimedItem() = (%#v, %v), want conflicted skip", processed, err)
	}

	github.reviewDecision = "APPROVED"
	second, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: repo})
	if err != nil {
		t.Fatalf("second DiscoverPullRequests() error = %v", err)
	}
	if len(second.QueueItems) != 0 {
		t.Fatalf("second QueueItems = %#v, want no re-enqueue while conflict remains after review decision changes", second.QueueItems)
	}
	items, err := fixture.repos.Queue.List(context.Background())
	if err != nil {
		t.Fatalf("Queue.List() error = %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("len(Queue.List()) = %d, want original completed item only", len(items))
	}

	github.listHeadSHA = "new-head"
	github.viewHeadSHA = "new-head"
	third, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: repo})
	if err != nil {
		t.Fatalf("third DiscoverPullRequests() error = %v", err)
	}
	if len(third.QueueItems) != 1 {
		t.Fatalf("third QueueItems = %#v, want re-enqueue for new head", third.QueueItems)
	}
}

func TestDiscoverPullRequestsRequeuesConflictedSkipWhenConflictClears(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{hasConflicts: true, reviewRequests: []string{"octocat"}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now, LoopConfig: testReviewerLoopConfig()})
	repo := "acme/looper"

	first, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: repo})
	if err != nil || len(first.QueueItems) != 1 {
		t.Fatalf("first DiscoverPullRequests() = (%#v, %v), want one queue item", first, err)
	}
	claimed, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claimed == nil {
		t.Fatalf("ClaimNextOfType() = (%#v, %v), want claimed queue item", claimed, err)
	}
	processed, err := runner.ProcessClaimedItem(context.Background(), *claimed)
	if err != nil || processed.Status != "skipped" || !strings.Contains(processed.Summary, "conflicted") {
		t.Fatalf("ProcessClaimedItem() = (%#v, %v), want conflicted skip", processed, err)
	}

	github.hasConflicts = false
	second, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: repo})
	if err != nil {
		t.Fatalf("second DiscoverPullRequests() error = %v", err)
	}
	if len(second.QueueItems) != 1 {
		t.Fatalf("second QueueItems = %#v, want re-enqueue when conflict clears for same head", second.QueueItems)
	}
}

func TestDiscoverPullRequestsSuppressesRepeatedAlreadyReviewedSkip(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	reviews := []map[string]any{{"author": map[string]any{"login": "octocat"}, "state": "COMMENTED", "commit": map[string]any{"oid": "abc123"}}}
	github := &fakeGitHubGateway{currentLogin: "octocat", reviewDecision: "REVIEW_REQUIRED", reviews: reviews, reviewRequests: []string{"octocat"}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now, LoopConfig: testReviewerLoopConfig()})
	repo := "acme/looper"

	first, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: repo})
	if err != nil || len(first.QueueItems) != 1 {
		t.Fatalf("first DiscoverPullRequests() = (%#v, %v), want one queue item", first, err)
	}
	claimed, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claimed == nil {
		t.Fatalf("ClaimNextOfType() = (%#v, %v), want claimed queue item", claimed, err)
	}
	processed, err := runner.ProcessClaimedItem(context.Background(), *claimed)
	if err != nil || processed.Status != "skipped" || !strings.Contains(processed.Summary, "already reviewed head abc123") {
		t.Fatalf("ProcessClaimedItem() = (%#v, %v), want already-reviewed skip", processed, err)
	}
	loop, err := fixture.repos.Loops.GetByID(context.Background(), first.CreatedLoopIDs[0])
	if err != nil || loop == nil {
		t.Fatalf("Loops.GetByID() = (%#v, %v), want loop", loop, err)
	}
	lastSkip, _ := parseJSONObject(loop.MetadataJSON)["lastFilterSkip"].(map[string]any)
	if got, _ := stringFromAny(lastSkip["reviewerLogin"]); got != "octocat" {
		t.Fatalf("lastFilterSkip.reviewerLogin = %q, want octocat", got)
	}

	github.reviewDecision = "APPROVED"
	second, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: repo})
	if err != nil {
		t.Fatalf("second DiscoverPullRequests() error = %v", err)
	}
	if len(second.QueueItems) != 0 {
		t.Fatalf("second QueueItems = %#v, want no re-enqueue for unchanged already-reviewed head after review decision changes", second.QueueItems)
	}

	github.currentLogin = "looper-bot"
	github.reviewRequests = []string{"looper-bot"}
	third, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: repo})
	if err != nil {
		t.Fatalf("third DiscoverPullRequests() error = %v", err)
	}
	if len(third.QueueItems) != 1 {
		t.Fatalf("third QueueItems = %#v, want re-enqueue when reviewer login changes", third.QueueItems)
	}
}

func TestDiscoverPullRequestsDoesNotRequeueApprovedReadyOrDraftNonActionablePRs(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		github *fakeGitHubGateway
		want   string
	}{
		{name: "approved", github: &fakeGitHubGateway{reviewDecision: "APPROVED", reviewRequests: []string{"octocat"}, reviews: []map[string]any{{"author": map[string]any{"login": "octocat"}, "state": "APPROVED", "commit": map[string]any{"oid": "abc123"}}}}, want: "approved"},
		{name: "ready", github: &fakeGitHubGateway{labels: []string{specpr.ReadyLabel}, reviewRequests: []string{"octocat"}}, want: "ready"},
		{name: "draft", github: &fakeGitHubGateway{listOpenByLabel: map[string][]PullRequestSummary{"": {{Number: 42, Title: "Draft", State: "OPEN", IsDraft: true, HeadSHA: "draft123", ReviewRequests: []string{"octocat"}}}}}, want: "draft"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fixture := newRunnerFixture(t)
			runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: tc.github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now, LoopConfig: testReviewerLoopConfig()})
			repo := "acme/looper"

			first, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: repo})
			if err != nil {
				t.Fatalf("first DiscoverPullRequests() error = %v", err)
			}
			if tc.want == "draft" {
				if len(first.QueueItems) != 0 {
					t.Fatalf("draft QueueItems = %#v, want no queue items", first.QueueItems)
				}
			} else {
				if len(first.QueueItems) != 1 {
					t.Fatalf("first QueueItems = %#v, want one queue item", first.QueueItems)
				}
				claimed, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
				if err != nil || claimed == nil {
					t.Fatalf("ClaimNextOfType() = (%#v, %v), want claimed queue item", claimed, err)
				}
				processed, err := runner.ProcessClaimedItem(context.Background(), *claimed)
				if err != nil || processed.Status != "skipped" || !strings.Contains(processed.Summary, tc.want) {
					t.Fatalf("ProcessClaimedItem() = (%#v, %v), want %s skip", processed, err, tc.want)
				}
			}

			second, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: repo})
			if err != nil {
				t.Fatalf("second DiscoverPullRequests() error = %v", err)
			}
			if len(second.QueueItems) != 0 {
				t.Fatalf("second QueueItems = %#v, want no repeated non-actionable requeue", second.QueueItems)
			}
		})
	}
}

func TestProcessClaimedItemSkipsTerminalLoopWithoutStartingRun(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: &fakeGitHubGateway{}, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})
	nowISO := fixture.nowISO()
	repo := "acme/looper"
	prNumber := int64(42)
	loop := storage.LoopRecord{ID: "loop_terminated_queued", Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: "terminated", CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	queue := storage.QueueItemRecord{ID: "queue_terminal", ProjectID: stringPtr("project_1"), LoopID: stringPtr(loop.ID), Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Priority: 2, Status: "running", AvailableAt: nowISO, Attempts: 1, MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Queue.Upsert(context.Background(), queue); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	result, err := runner.ProcessClaimedItem(context.Background(), queue)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "skipped" || !strings.Contains(result.Summary, "terminated") {
		t.Fatalf("result = %#v, want skipped terminal loop", result)
	}
	runs, err := fixture.repos.Runs.ListByLoop(context.Background(), loop.ID)
	if err != nil {
		t.Fatalf("Runs.ListByLoop() error = %v", err)
	}
	if len(runs) != 0 {
		t.Fatalf("runs = %#v, want none", runs)
	}
}

func TestProcessClaimedItemRetriesWhenCurrentUserLookupFails(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{currentLoginErr: fmt.Errorf("gh auth failed")}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})
	nowISO := fixture.nowISO()
	repo := "acme/looper"
	prNumber := int64(42)
	loop := storage.LoopRecord{ID: "loop_lookup_error", Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: "queued", CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	queue, err := runner.enqueue(context.Background(), enqueueInput{ProjectID: "project_1", LoopID: loop.ID, Repo: repo, PRNumber: prNumber})
	if err != nil {
		t.Fatalf("enqueue() error = %v", err)
	}
	claimed, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claimed == nil || claimed.ID != queue.ID {
		t.Fatalf("ClaimNextOfType() = (%#v, %v), want queued item %s", claimed, err, queue.ID)
	}

	result, err := runner.ProcessClaimedItem(context.Background(), *claimed)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "failed" || result.FailureKind != FailureRetryableTransient {
		t.Fatalf("result = %#v, want retryable transient failure", result)
	}
	queueAfter, err := fixture.repos.Queue.GetByID(context.Background(), queue.ID)
	if err != nil || queueAfter == nil {
		t.Fatalf("Queue.GetByID() = (%#v, %v), want queue", queueAfter, err)
	}
	if queueAfter.Status != "queued" {
		t.Fatalf("queue status = %s, want queued retry", queueAfter.Status)
	}
}

func TestProcessClaimedItemAllowsManualQueuedLoopWithoutReviewRequest(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{reviewRequests: []string{"alice"}, currentLogin: "bob"}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "Manual review", Stdout: `__LOOPER_RESULT__={"summary":"posted review"}`}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now})
	nowISO := fixture.nowISO()
	repo := "acme/looper"
	prNumber := int64(42)
	metadata := `{"manual":true}`
	loop := storage.LoopRecord{ID: "loop_manual_api", Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: "queued", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	queue, err := runner.enqueue(context.Background(), enqueueInput{ProjectID: "project_1", LoopID: loop.ID, Repo: repo, PRNumber: prNumber})
	if err != nil {
		t.Fatalf("enqueue() error = %v", err)
	}
	claimed, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claimed == nil || claimed.ID != queue.ID {
		t.Fatalf("ClaimNextOfType() = (%#v, %v), want queued item %s", claimed, err, queue.ID)
	}

	result, err := runner.ProcessClaimedItem(context.Background(), *claimed)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "success" {
		t.Fatalf("result = %#v, want success", result)
	}
	if len(agent.starts) != 1 {
		t.Fatalf("agent starts=%d, want agent-native review to run", len(agent.starts))
	}
	updatedLoop, err := fixture.repos.Loops.GetByID(context.Background(), result.LoopID)
	if err != nil || updatedLoop == nil {
		t.Fatalf("Loops.GetByID() = (%#v, %v), want loop", updatedLoop, err)
	}
	loopMeta := reviewerLoopMetadata(parseJSONObject(updatedLoop.MetadataJSON))
	if got := intFromAny(loopMeta["agentExecutionCount"]); got != 1 {
		t.Fatalf("agentExecutionCount = %d, want 1 after agent start", got)
	}
}

func TestProcessClaimedItemAllowsManualQueuedLoopWhenApproved(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{reviewDecision: "APPROVED", reviewRequests: []string{"alice"}, currentLogin: "bob"}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "Manual review", Stdout: `__LOOPER_RESULT__={"summary":"posted review"}`}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now})
	nowISO := fixture.nowISO()
	repo := "acme/looper"
	prNumber := int64(42)
	metadata := `{"manual":true}`
	loop := storage.LoopRecord{ID: "loop_manual_approved", Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: "queued", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	queue, err := runner.enqueue(context.Background(), enqueueInput{ProjectID: "project_1", LoopID: loop.ID, Repo: repo, PRNumber: prNumber})
	if err != nil {
		t.Fatalf("enqueue() error = %v", err)
	}
	claimed, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claimed == nil || claimed.ID != queue.ID {
		t.Fatalf("ClaimNextOfType() = (%#v, %v), want queued item %s", claimed, err, queue.ID)
	}

	result, err := runner.ProcessClaimedItem(context.Background(), *claimed)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "success" {
		t.Fatalf("result = %#v, want success", result)
	}
	if len(agent.starts) != 1 {
		t.Fatalf("agent starts=%d, want manual review to bypass approved termination", len(agent.starts))
	}
	updatedLoop, err := fixture.repos.Loops.GetByID(context.Background(), result.LoopID)
	if err != nil || updatedLoop == nil {
		t.Fatalf("Loops.GetByID() = (%#v, %v), want loop", updatedLoop, err)
	}
	if updatedLoop.Status == "terminated" {
		t.Fatalf("loop status = %q, want manual run not terminated", updatedLoop.Status)
	}
}

func TestProcessClaimedItemAllowsManualQueuedLoopWhenReadyLabel(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{labels: []string{specpr.ReadyLabel}, reviewRequests: []string{"alice"}, currentLogin: "bob"}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "Manual review", Stdout: `__LOOPER_RESULT__={"summary":"posted review"}`}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now})
	nowISO := fixture.nowISO()
	repo := "acme/looper"
	prNumber := int64(42)
	metadata := `{"manual":true}`
	loop := storage.LoopRecord{ID: "loop_manual_ready", Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: "queued", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	queue, err := runner.enqueue(context.Background(), enqueueInput{ProjectID: "project_1", LoopID: loop.ID, Repo: repo, PRNumber: prNumber})
	if err != nil {
		t.Fatalf("enqueue() error = %v", err)
	}
	claimed, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claimed == nil || claimed.ID != queue.ID {
		t.Fatalf("ClaimNextOfType() = (%#v, %v), want queued item %s", claimed, err, queue.ID)
	}

	result, err := runner.ProcessClaimedItem(context.Background(), *claimed)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "success" {
		t.Fatalf("result = %#v, want success", result)
	}
	if len(agent.starts) != 1 {
		t.Fatalf("agent starts=%d, want manual review to bypass ready-label termination", len(agent.starts))
	}
	updatedLoop, err := fixture.repos.Loops.GetByID(context.Background(), result.LoopID)
	if err != nil || updatedLoop == nil {
		t.Fatalf("Loops.GetByID() = (%#v, %v), want loop", updatedLoop, err)
	}
	if updatedLoop.Status == "terminated" {
		t.Fatalf("loop status = %q, want manual run not terminated", updatedLoop.Status)
	}
}

func TestNormalizedFindingFingerprintIgnoresReviewMarkerMetadata(t *testing.T) {
	t.Parallel()
	oldHead := normalizedFindingFingerprint("same actionable finding <!-- looper:review loop=loop_1 head=old outcome=actionable -->")
	newHead := normalizedFindingFingerprint("same actionable finding <!-- looper:review loop=loop_1 head=new outcome=actionable -->")
	if oldHead == "" || oldHead != newHead {
		t.Fatalf("fingerprints = %q and %q, want equal non-empty values", oldHead, newHead)
	}
}

func TestProcessClaimedItemFingerprintsPublishedReviewBodyForIdenticalOutput(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{reviewRequests: []string{"octocat"}, reviewMarkerBody: "new actionable finding <!-- looper:review loop=loop_identical_output outcome=actionable -->"}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "Same findings", Stdout: `__LOOPER_RESULT__={"summary":"posted review"}`}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, LoopConfig: config.ReviewerLoopConfig{EnabledByDefault: true, QuietPeriodSeconds: 120, MaxIterationsPerPR: 20, MaxIterationsPerHead: 2, MaxWallClockSeconds: 14400, MaxConsecutiveFailures: 3, MaxAgentExecutionsPerPR: 25, StopOnIdenticalOutput: true}})
	ctx := context.Background()
	nowISO := fixture.nowISO()
	repo := "acme/looper"
	prNumber := int64(42)
	metadata := fmt.Sprintf(`{"followUpdates":true,"loop":{"enabled":true,"lastOutputFingerprint":%q}}`, normalizedFindingFingerprint("previous actionable finding"))
	loop := storage.LoopRecord{ID: "loop_identical_output", Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: "queued", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Loops.Upsert(ctx, loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	queue, err := runner.enqueue(ctx, enqueueInput{ProjectID: "project_1", LoopID: loop.ID, Repo: repo, PRNumber: prNumber})
	if err != nil {
		t.Fatalf("enqueue() error = %v", err)
	}
	claimed, err := fixture.repos.Queue.ClaimNextOfType(ctx, fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claimed == nil || claimed.ID != queue.ID {
		t.Fatalf("ClaimNextOfType() = (%#v, %v), want queued item %s", claimed, err, queue.ID)
	}
	result, err := runner.ProcessClaimedItem(ctx, *claimed)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "success" {
		t.Fatalf("result = %#v, want success", result)
	}
	updatedLoop, err := fixture.repos.Loops.GetByID(ctx, loop.ID)
	if err != nil || updatedLoop == nil {
		t.Fatalf("Loops.GetByID() = (%#v, %v), want loop", updatedLoop, err)
	}
	if updatedLoop.Status != "waiting" {
		t.Fatalf("loop status = %q, want waiting", updatedLoop.Status)
	}
	if updatedLoop.MetadataJSON == nil || contains(*updatedLoop.MetadataJSON, `"terminationReason":"identical_output"`) {
		t.Fatalf("loop metadata = %#v, want no identical_output termination", updatedLoop.MetadataJSON)
	}
	if want := normalizedFindingFingerprint(github.reviewMarkerBody); !contains(*updatedLoop.MetadataJSON, fmt.Sprintf(`"lastOutputFingerprint":"%s"`, want)) {
		t.Fatalf("loop metadata = %#v, want published review body fingerprint %s", updatedLoop.MetadataJSON, want)
	}
}

func TestProcessClaimedItemFingerprintsPublishedReviewInlineComments(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{reviewRequests: []string{"octocat"}, reviewMarkerBody: "review overview <!-- looper:review loop=loop_inline_fingerprint outcome=actionable -->", reviewMarkerInlineCommentBodies: []string{"different actionable inline finding"}}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "Same overview", Stdout: `__LOOPER_RESULT__={"summary":"posted review"}`}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, LoopConfig: config.ReviewerLoopConfig{EnabledByDefault: true, QuietPeriodSeconds: 120, MaxIterationsPerPR: 20, MaxIterationsPerHead: 2, MaxWallClockSeconds: 14400, MaxConsecutiveFailures: 3, MaxAgentExecutionsPerPR: 25, StopOnIdenticalOutput: true}})
	ctx := context.Background()
	nowISO := fixture.nowISO()
	repo := "acme/looper"
	prNumber := int64(42)
	metadata := fmt.Sprintf(`{"followUpdates":true,"loop":{"enabled":true,"lastOutputFingerprint":%q}}`, normalizedFindingFingerprint(github.reviewMarkerBody+"\nprevious inline finding"))
	loop := storage.LoopRecord{ID: "loop_inline_fingerprint", Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: "queued", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Loops.Upsert(ctx, loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	queue, err := runner.enqueue(ctx, enqueueInput{ProjectID: "project_1", LoopID: loop.ID, Repo: repo, PRNumber: prNumber})
	if err != nil {
		t.Fatalf("enqueue() error = %v", err)
	}
	claimed, err := fixture.repos.Queue.ClaimNextOfType(ctx, fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claimed == nil || claimed.ID != queue.ID {
		t.Fatalf("ClaimNextOfType() = (%#v, %v), want queued item %s", claimed, err, queue.ID)
	}
	result, err := runner.ProcessClaimedItem(ctx, *claimed)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "success" {
		t.Fatalf("result = %#v, want success", result)
	}
	updatedLoop, err := fixture.repos.Loops.GetByID(ctx, loop.ID)
	if err != nil || updatedLoop == nil || updatedLoop.MetadataJSON == nil {
		t.Fatalf("Loops.GetByID() = (%#v, %v), want loop metadata", updatedLoop, err)
	}
	if contains(*updatedLoop.MetadataJSON, `"terminationReason":"identical_output"`) {
		t.Fatalf("loop metadata = %#v, want distinct inline comments to avoid identical_output", updatedLoop.MetadataJSON)
	}
	want := normalizedFindingFingerprint(github.reviewMarkerBody + "\n" + strings.Join(github.reviewMarkerInlineCommentBodies, "\n"))
	if !contains(*updatedLoop.MetadataJSON, fmt.Sprintf(`"lastOutputFingerprint":"%s"`, want)) {
		t.Fatalf("loop metadata = %s, want inline comment fingerprint %s", *updatedLoop.MetadataJSON, want)
	}
}

func TestProcessClaimedItemDetectsDuplicatePublishedFindings(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	body := "duplicate actionable finding <!-- looper:review loop=loop_duplicate_finding outcome=actionable -->"
	fingerprint := normalizedFindingFingerprint(body)
	github := &fakeGitHubGateway{reviewRequests: []string{"octocat"}, reviewMarkerBody: body}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "Duplicate findings", Stdout: `__LOOPER_RESULT__={"summary":"posted review"}`}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, LoopConfig: config.ReviewerLoopConfig{EnabledByDefault: true, QuietPeriodSeconds: 120, MaxIterationsPerPR: 20, MaxIterationsPerHead: 2, MaxWallClockSeconds: 14400, MaxConsecutiveFailures: 3, MaxAgentExecutionsPerPR: 25}, DetectDuplicateFindings: true})
	ctx := context.Background()
	nowISO := fixture.nowISO()
	repo := "acme/looper"
	prNumber := int64(42)
	metadata := fmt.Sprintf(`{"followUpdates":true,"loop":{"enabled":true,"publishedFindingFingerprints":[%q]}}`, fingerprint)
	loop := storage.LoopRecord{ID: "loop_duplicate_finding", Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: "queued", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Loops.Upsert(ctx, loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	queue, err := runner.enqueue(ctx, enqueueInput{ProjectID: "project_1", LoopID: loop.ID, Repo: repo, PRNumber: prNumber})
	if err != nil {
		t.Fatalf("enqueue() error = %v", err)
	}
	claimed, err := fixture.repos.Queue.ClaimNextOfType(ctx, fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claimed == nil || claimed.ID != queue.ID {
		t.Fatalf("ClaimNextOfType() = (%#v, %v), want queued item %s", claimed, err, queue.ID)
	}

	result, err := runner.ProcessClaimedItem(ctx, *claimed)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "success" {
		t.Fatalf("result = %#v, want success", result)
	}
	updatedLoop, err := fixture.repos.Loops.GetByID(ctx, loop.ID)
	if err != nil || updatedLoop == nil || updatedLoop.MetadataJSON == nil {
		t.Fatalf("Loops.GetByID() = (%#v, %v), want loop metadata", updatedLoop, err)
	}
	if !contains(*updatedLoop.MetadataJSON, `"duplicateFindingsDetected":1`) {
		t.Fatalf("loop metadata = %#v, want duplicateFindingsDetected counter", updatedLoop.MetadataJSON)
	}
	if contains(*updatedLoop.MetadataJSON, "duplicateFindingsSuppressed") {
		t.Fatalf("loop metadata = %#v, want no suppressed counter for detection-only accounting", updatedLoop.MetadataJSON)
	}
	if len(agent.starts) != 1 {
		t.Fatalf("agent starts = %d, want review still executed for detection-only duplicate tracking", len(agent.starts))
	}
}

func TestProcessClaimedItemRecordsCleanNoopWithoutReviewMarkerForCommentPolicy(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{reviewRequests: []string{"octocat"}, reviewMarkerMissing: true}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "No actionable findings; added clean signal", Stdout: `__LOOPER_RESULT__={"summary":"No actionable findings; added clean signal"}`, ParseStatus: "parsed"}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, LoopConfig: config.ReviewerLoopConfig{EnabledByDefault: true, QuietPeriodSeconds: 120, MaxIterationsPerPR: 20, MaxIterationsPerHead: 2, MaxWallClockSeconds: 14400, MaxConsecutiveFailures: 3, MaxAgentExecutionsPerPR: 25}})
	ctx := context.Background()
	nowISO := fixture.nowISO()
	repo := "acme/looper"
	prNumber := int64(42)
	metadata := `{"followUpdates":true,"loop":{"enabled":true}}`
	loop := storage.LoopRecord{ID: "loop_clean_noop", Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: "queued", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Loops.Upsert(ctx, loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	queue, err := runner.enqueue(ctx, enqueueInput{ProjectID: "project_1", LoopID: loop.ID, Repo: repo, PRNumber: prNumber})
	if err != nil {
		t.Fatalf("enqueue() error = %v", err)
	}
	claimed, err := fixture.repos.Queue.ClaimNextOfType(ctx, fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claimed == nil || claimed.ID != queue.ID {
		t.Fatalf("ClaimNextOfType() = (%#v, %v), want queued item %s", claimed, err, queue.ID)
	}

	result, err := runner.ProcessClaimedItem(ctx, *claimed)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "success" {
		t.Fatalf("result = %#v, want success", result)
	}
	if github.reviewMarkerCalls != 0 {
		t.Fatalf("reviewMarkerCalls = %d, want no review marker lookup for clean no-op", github.reviewMarkerCalls)
	}
	if len(github.addReactionCalls) != 1 {
		t.Fatalf("addReactionCalls = %d, want one clean signal reaction", len(github.addReactionCalls))
	}
	updatedLoop, err := fixture.repos.Loops.GetByID(ctx, loop.ID)
	if err != nil || updatedLoop == nil || updatedLoop.MetadataJSON == nil {
		t.Fatalf("Loops.GetByID() = (%#v, %v), want loop metadata", updatedLoop, err)
	}
	if !contains(*updatedLoop.MetadataJSON, `"lastPublishedHeadSha":"abc123"`) {
		t.Fatalf("loop metadata = %s, want clean no-op recorded as published for head", *updatedLoop.MetadataJSON)
	}
	if contains(*updatedLoop.MetadataJSON, `"lastOutputFingerprint"`) {
		t.Fatalf("loop metadata = %s, want clean no-op excluded from output fingerprinting", *updatedLoop.MetadataJSON)
	}
}

func TestProcessClaimedItemRejectsCleanNoopWithoutApprovedMarkerForApprovePolicy(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{reviewRequests: []string{"octocat"}, reviewMarkerMissing: true}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "No actionable findings", Stdout: `__LOOPER_RESULT__={"summary":"No actionable findings"}`, ParseStatus: "parsed"}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, ReviewEvents: config.ReviewerReviewEventsConfig{Clean: config.ReviewerReviewEventApprove}, LoopConfig: testReviewerLoopConfig()})

	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNext() = (%#v, %v), want claimed queue item", claim, err)
	}
	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "failed" || result.FailureKind != FailureRetryableAfterResume || !contains(result.Summary, "requires an APPROVED review marker") {
		t.Fatalf("result = %#v, want retryable approve-marker-required failure", result)
	}
	if github.reviewMarkerCalls == 0 {
		t.Fatalf("reviewMarkerCalls = %d, want marker lookup before rejecting clean APPROVE summary", github.reviewMarkerCalls)
	}
	if len(github.addReactionCalls) != 0 {
		t.Fatalf("addReactionCalls = %#v, want no reaction for rejected clean no-op", github.addReactionCalls)
	}
}

func TestProcessClaimedItemAcceptsCleanNoopWithApprovedMarkerForApprovePolicy(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{reviewRequests: []string{"octocat"}, reviewMarkerOutcome: "clean", reviewMarkerEvent: ReviewEventApprove, reviewMarkerBody: strings.Join([]string{
		"@octocat Thanks for the thoughtful update — the changes are clear and well scoped.",
		"Summary: this keeps the approval flow safe while preserving the intended reviewer behavior.",
		"<!-- looper:review outcome=clean -->",
	}, "\n\n")}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "No actionable findings", Stdout: `__LOOPER_RESULT__={"summary":"No actionable findings"}`, ParseStatus: "parsed"}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, ReviewEvents: config.ReviewerReviewEventsConfig{Clean: config.ReviewerReviewEventApprove}, LoopConfig: testReviewerLoopConfig()})

	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNext() = (%#v, %v), want claimed queue item", claim, err)
	}
	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "success" {
		t.Fatalf("result = %#v, want success", result)
	}
	if github.reviewMarkerCalls < 2 {
		t.Fatalf("reviewMarkerCalls = %d, want review-step and publish marker verification", github.reviewMarkerCalls)
	}
	if len(github.addReactionCalls) != 1 {
		t.Fatalf("addReactionCalls = %#v, want clean signal reaction", github.addReactionCalls)
	}
	if claim.LoopID == nil {
		t.Fatal("claim.LoopID = nil, want associated loop ID")
	}
	updatedLoop, err := fixture.repos.Loops.GetByID(context.Background(), *claim.LoopID)
	if err != nil || updatedLoop == nil || updatedLoop.MetadataJSON == nil {
		t.Fatalf("Loops.GetByID() = (%#v, %v), want loop metadata", updatedLoop, err)
	}
	if contains(*updatedLoop.MetadataJSON, `"lastOutputFingerprint"`) {
		t.Fatalf("loop metadata = %s, want accepted clean no-op excluded from output fingerprinting", *updatedLoop.MetadataJSON)
	}
}

func TestValidateCleanApprovedReviewMarkerBodyAcceptsCaseInsensitiveAuthorMention(t *testing.T) {
	t.Parallel()
	checkpoint := reviewerCheckpoint{
		Detail:   &checkpointDetail{Author: "OctoCat"},
		Snapshot: &checkpointSnapshot{Author: "OctoCat"},
	}
	detail := PullRequestDetail{Author: "OctoCat"}
	marker := ReviewMarkerResult{Body: strings.Join([]string{
		"@octocat Thanks for the thoughtful update — the changes are clear and well scoped.",
		"Summary: this keeps the approval flow safe while preserving the intended reviewer behavior.",
		"<!-- looper:review outcome=clean -->",
	}, "\n\n")}

	if err := validateCleanApprovedReviewMarkerBody(marker, cleanReviewAuthorLogin(checkpoint, detail)); err != nil {
		t.Fatalf("validateCleanApprovedReviewMarkerBody() error = %v", err)
	}
}

func TestProcessClaimedItemRejectsCleanNoopWithInvalidApprovedMarkerBodyForApprovePolicy(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{reviewRequests: []string{"octocat"}, reviewMarkerOutcome: "clean", reviewMarkerEvent: ReviewEventApprove, reviewMarkerBody: "@octocat <!-- hidden filler words should not count toward this approval body -->\n\n[hidden]:https://example.com\n  more hidden filler words that should not count here\n\n<!-- looper:review outcome=clean -->"}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "No actionable findings", Stdout: `__LOOPER_RESULT__={"summary":"No actionable findings"}`, ParseStatus: "parsed"}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, ReviewEvents: config.ReviewerReviewEventsConfig{Clean: config.ReviewerReviewEventApprove}, LoopConfig: testReviewerLoopConfig()})

	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNext() = (%#v, %v), want claimed queue item", claim, err)
	}
	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "failed" || result.FailureKind != FailureRetryableAfterResume || !contains(result.Summary, "short human summary") {
		t.Fatalf("result = %#v, want retryable invalid approval body failure", result)
	}
	if github.reviewMarkerCalls == 0 {
		t.Fatalf("reviewMarkerCalls = %d, want marker lookup before rejecting invalid clean APPROVE body", github.reviewMarkerCalls)
	}
	if len(github.addReactionCalls) != 0 {
		t.Fatalf("addReactionCalls = %#v, want no reaction for rejected clean no-op", github.addReactionCalls)
	}
}

func TestProcessClaimedItemRejectsCleanNoopResumeWithInvalidApprovedMarkerBodyForApprovePolicy(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	ctx := context.Background()
	repo := "acme/looper"
	prNumber := int64(42)
	github := &fakeGitHubGateway{reviewRequests: []string{"octocat"}, reviewMarkerOutcome: "clean", reviewMarkerEvent: ReviewEventApprove, reviewMarkerBody: "@octocat <!-- hidden filler words should not count toward this approval body --> <!-- looper:review outcome=clean -->"}
	agent := &fakeAgentExecutor{}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, ReviewEvents: config.ReviewerReviewEventsConfig{Clean: config.ReviewerReviewEventApprove}, LoopConfig: testReviewerLoopConfig()})
	loopTarget := "pr:42"
	loop := storage.LoopRecord{ID: "loop_clean_approve_resume", Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", TargetID: &loopTarget, Repo: &repo, PRNumber: &prNumber, Status: "queued", CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}
	if err := fixture.repos.Loops.Upsert(ctx, loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	if _, err := runner.enqueue(ctx, enqueueInput{ProjectID: loop.ProjectID, LoopID: loop.ID, Repo: repo, PRNumber: prNumber}); err != nil {
		t.Fatalf("enqueue() error = %v", err)
	}
	claim, err := fixture.repos.Queue.ClaimNextOfType(ctx, fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNextOfType() = (%#v, %v), want claimed queue item", claim, err)
	}
	checkpoint := reviewerCheckpoint{
		Detail:        &checkpointDetail{Title: "Review me", State: "OPEN", HeadSHA: "abc123", ReviewRequests: []string{"octocat"}},
		Snapshot:      &checkpointSnapshot{HeadSHA: "abc123"},
		PendingReview: &pendingReviewCheckpoint{HeadSHA: "abc123", IdempotencyKey: "idem", Event: reviewEventAgentNative, Summary: "No actionable findings"},
		ResumePolicy:  "advance_from_checkpoint",
	}
	checkpointJSON := mustMarshalJSON(checkpoint)
	run := storage.RunRecord{ID: "run_clean_approve_resume", LoopID: loop.ID, Status: "failed", CurrentStep: stringPtr(string(stepPublish)), LastCompletedStep: stringPtr(string(stepReview)), CheckpointJSON: &checkpointJSON, StartedAt: fixture.nowISO(), CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}
	if err := fixture.repos.Runs.Upsert(ctx, run); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}

	result, err := runner.ProcessClaimedItem(ctx, *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "failed" || result.FailureKind != FailureRetryableAfterResume || !contains(result.Summary, "short human summary") {
		t.Fatalf("result = %#v, want retryable invalid approval body failure", result)
	}
	if len(agent.starts) != 0 {
		t.Fatalf("len(agent.starts) = %d, want no review rerun in failed publish attempt", len(agent.starts))
	}
	if len(github.addReactionCalls) != 0 {
		t.Fatalf("addReactionCalls = %#v, want no reaction for rejected clean no-op resume", github.addReactionCalls)
	}
}

func TestProcessClaimedItemDoesNotStopOnRepeatedCleanNoopSummary(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{reviewRequests: []string{"octocat"}, reviewMarkerMissing: true}
	cleanSummary := "No actionable findings; added clean signal"
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: cleanSummary, Stdout: `__LOOPER_RESULT__={"summary":"No actionable findings; added clean signal"}`, ParseStatus: "parsed"}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, LoopConfig: config.ReviewerLoopConfig{EnabledByDefault: true, QuietPeriodSeconds: 120, MaxIterationsPerPR: 20, MaxIterationsPerHead: 2, MaxWallClockSeconds: 14400, MaxConsecutiveFailures: 3, MaxAgentExecutionsPerPR: 25, StopOnIdenticalOutput: true}})
	ctx := context.Background()
	nowISO := fixture.nowISO()
	repo := "acme/looper"
	prNumber := int64(42)
	metadata := fmt.Sprintf(`{"followUpdates":true,"loop":{"enabled":true,"lastOutputFingerprint":%q}}`, normalizedFindingFingerprint(cleanSummary))
	loop := storage.LoopRecord{ID: "loop_repeated_clean_noop", Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: "queued", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Loops.Upsert(ctx, loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	queue, err := runner.enqueue(ctx, enqueueInput{ProjectID: "project_1", LoopID: loop.ID, Repo: repo, PRNumber: prNumber})
	if err != nil {
		t.Fatalf("enqueue() error = %v", err)
	}
	claimed, err := fixture.repos.Queue.ClaimNextOfType(ctx, fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claimed == nil || claimed.ID != queue.ID {
		t.Fatalf("ClaimNextOfType() = (%#v, %v), want queued item %s", claimed, err, queue.ID)
	}

	result, err := runner.ProcessClaimedItem(ctx, *claimed)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "success" {
		t.Fatalf("result = %#v, want success", result)
	}
	updatedLoop, err := fixture.repos.Loops.GetByID(ctx, loop.ID)
	if err != nil || updatedLoop == nil || updatedLoop.MetadataJSON == nil {
		t.Fatalf("Loops.GetByID() = (%#v, %v), want loop metadata", updatedLoop, err)
	}
	if updatedLoop.Status != "waiting" {
		t.Fatalf("loop status = %q, want waiting", updatedLoop.Status)
	}
	if contains(*updatedLoop.MetadataJSON, `"terminationReason":"identical_output"`) {
		t.Fatalf("loop metadata = %#v, want repeated clean no-op not to terminate", updatedLoop.MetadataJSON)
	}
	if contains(*updatedLoop.MetadataJSON, `"identicalOutputCount"`) {
		t.Fatalf("loop metadata = %#v, want clean no-op excluded from identical output accounting", updatedLoop.MetadataJSON)
	}
}

func TestProcessClaimedItemSkipsCleanNoopWhenReviewRequestRemovedBeforePublish(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{removeReviewRequestOnSecondView: true, reviewRequests: []string{"octocat"}, reviewMarkerMissing: true}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "No actionable findings; added clean signal", Stdout: `__LOOPER_RESULT__={"summary":"No actionable findings; added clean signal"}`, ParseStatus: "parsed"}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, DiscoveryPolicy: DiscoveryPolicy{AutoDiscovery: true, IncludeDrafts: false, RequireReviewRequest: true, Labels: []string{}, LabelMode: config.LabelModeAll}, LoopConfig: config.ReviewerLoopConfig{EnabledByDefault: true, QuietPeriodSeconds: 120, MaxIterationsPerPR: 20, MaxIterationsPerHead: 2, MaxWallClockSeconds: 14400, MaxConsecutiveFailures: 3, MaxAgentExecutionsPerPR: 25}})
	ctx := context.Background()
	nowISO := fixture.nowISO()
	repo := "acme/looper"
	prNumber := int64(42)
	metadata := `{"followUpdates":true,"loop":{"enabled":true}}`
	loop := storage.LoopRecord{ID: "loop_clean_noop_request_removed", Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: "queued", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Loops.Upsert(ctx, loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	queue, err := runner.enqueue(ctx, enqueueInput{ProjectID: "project_1", LoopID: loop.ID, Repo: repo, PRNumber: prNumber})
	if err != nil {
		t.Fatalf("enqueue() error = %v", err)
	}
	claimed, err := fixture.repos.Queue.ClaimNextOfType(ctx, fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claimed == nil || claimed.ID != queue.ID {
		t.Fatalf("ClaimNextOfType() = (%#v, %v), want queued item %s", claimed, err, queue.ID)
	}

	result, err := runner.ProcessClaimedItem(ctx, *claimed)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "skipped" || !contains(result.Summary, "not requested for review") {
		t.Fatalf("result = %#v, want skipped not requested", result)
	}
	if len(github.addReactionCalls) != 0 {
		t.Fatalf("addReactionCalls = %d, want no clean signal reaction", len(github.addReactionCalls))
	}
	updatedLoop, err := fixture.repos.Loops.GetByID(ctx, loop.ID)
	if err != nil || updatedLoop == nil || updatedLoop.MetadataJSON == nil {
		t.Fatalf("Loops.GetByID() = (%#v, %v), want loop metadata", updatedLoop, err)
	}
	if contains(*updatedLoop.MetadataJSON, `"lastPublishedHeadSha":"abc123"`) {
		t.Fatalf("loop metadata = %s, want no clean no-op publish progress", *updatedLoop.MetadataJSON)
	}
}

func TestProcessClaimedItemRestartsFromDiscoverWhenCleanNoopHeadChangesBeforePublish(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{changeHeadOnSecondView: true, reviewRequests: []string{"octocat"}, reviewMarkerMissing: true}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "No actionable findings; added clean signal", Stdout: `__LOOPER_RESULT__={"summary":"No actionable findings; added clean signal"}`, ParseStatus: "parsed"}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, LoopConfig: config.ReviewerLoopConfig{EnabledByDefault: true, QuietPeriodSeconds: 120, MaxIterationsPerPR: 20, MaxIterationsPerHead: 2, MaxWallClockSeconds: 14400, MaxConsecutiveFailures: 3, MaxAgentExecutionsPerPR: 25}})
	ctx := context.Background()
	nowISO := fixture.nowISO()
	repo := "acme/looper"
	prNumber := int64(42)
	metadata := `{"followUpdates":true,"loop":{"enabled":true}}`
	loop := storage.LoopRecord{ID: "loop_clean_noop_head_changed", Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: "queued", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Loops.Upsert(ctx, loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	queue, err := runner.enqueue(ctx, enqueueInput{ProjectID: "project_1", LoopID: loop.ID, Repo: repo, PRNumber: prNumber})
	if err != nil {
		t.Fatalf("enqueue() error = %v", err)
	}
	claimed, err := fixture.repos.Queue.ClaimNextOfType(ctx, fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claimed == nil || claimed.ID != queue.ID {
		t.Fatalf("ClaimNextOfType() = (%#v, %v), want queued item %s", claimed, err, queue.ID)
	}

	result, err := runner.ProcessClaimedItem(ctx, *claimed)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "failed" || result.FailureKind != FailureRetryableAfterResume || !contains(result.Summary, "PR head changed before publish") {
		t.Fatalf("result = %#v, want standard retryable head-change failure", result)
	}
	latestRun, err := fixture.repos.Runs.GetLatestByLoopID(ctx, result.LoopID)
	if err != nil || latestRun == nil {
		t.Fatalf("GetLatestByLoopID() = (%#v, %v), want failed run", latestRun, err)
	}
	if !contains(derefString(latestRun.Summary), "PR head changed before publish") {
		t.Fatalf("Summary = %q, want standard head-change message", derefString(latestRun.Summary))
	}
	resumed, err := runner.createRunContext(ctx, loop)
	if err != nil {
		t.Fatalf("createRunContext() error = %v", err)
	}
	if resumed.StartStep != stepDiscover {
		t.Fatalf("StartStep = %q, want discover", resumed.StartStep)
	}
}

func TestProcessClaimedItemTransitionsSpecLabelsForCleanNoop(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{labels: []string{specpr.ReviewingLabel}, reviewRequests: []string{"octocat"}, reviewMarkerMissing: true}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "No actionable findings; added clean signal", Stdout: `__LOOPER_RESULT__={"summary":"No actionable findings; added clean signal"}`, ParseStatus: "parsed"}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, AllowAutoApprove: true, ReviewEvents: config.ReviewerReviewEventsConfig{Clean: config.ReviewerReviewEventComment}, LoopConfig: config.ReviewerLoopConfig{EnabledByDefault: true, QuietPeriodSeconds: 120, MaxIterationsPerPR: 20, MaxIterationsPerHead: 2, MaxWallClockSeconds: 14400, MaxConsecutiveFailures: 3, MaxAgentExecutionsPerPR: 25}})
	ctx := context.Background()
	nowISO := fixture.nowISO()
	repo := "acme/looper"
	prNumber := int64(42)
	metadata := `{"followUpdates":true,"loop":{"enabled":true}}`
	loop := storage.LoopRecord{ID: "loop_clean_noop_spec", Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: "queued", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Loops.Upsert(ctx, loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	queue, err := runner.enqueue(ctx, enqueueInput{ProjectID: "project_1", LoopID: loop.ID, Repo: repo, PRNumber: prNumber})
	if err != nil {
		t.Fatalf("enqueue() error = %v", err)
	}
	claimed, err := fixture.repos.Queue.ClaimNextOfType(ctx, fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claimed == nil || claimed.ID != queue.ID {
		t.Fatalf("ClaimNextOfType() = (%#v, %v), want queued item %s", claimed, err, queue.ID)
	}

	result, err := runner.ProcessClaimedItem(ctx, *claimed)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "success" {
		t.Fatalf("result = %#v, want success", result)
	}
	if len(github.removeLabelCalls) != 0 {
		t.Fatalf("removeLabelCalls = %#v, want no spec-reviewing removal for clean COMMENT no-op", github.removeLabelCalls)
	}
	if len(github.addLabelCalls) != 0 {
		t.Fatalf("addLabelCalls = %#v, want no spec-ready add for clean COMMENT no-op", github.addLabelCalls)
	}
	if github.viewCalls < 1 {
		t.Fatalf("viewCalls = %d, want publish detail check", github.viewCalls)
	}
}

func TestProcessClaimedItemDoesNotTreatCleanNoopAsApprovedTransitionForApprovePolicy(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{labels: []string{specpr.ReviewingLabel}, reviewRequests: []string{"octocat"}, reviewMarkerMissing: true}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "No actionable findings; added clean signal", Stdout: `__LOOPER_RESULT__={"summary":"No actionable findings; added clean signal"}`, ParseStatus: "parsed"}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, AllowAutoApprove: false, ReviewEvents: config.ReviewerReviewEventsConfig{Clean: config.ReviewerReviewEventApprove}, LoopConfig: config.ReviewerLoopConfig{EnabledByDefault: true, QuietPeriodSeconds: 120, MaxIterationsPerPR: 20, MaxIterationsPerHead: 2, MaxWallClockSeconds: 14400, MaxConsecutiveFailures: 3, MaxAgentExecutionsPerPR: 25}})
	ctx := context.Background()
	nowISO := fixture.nowISO()
	repo := "acme/looper"
	prNumber := int64(42)
	metadata := `{"followUpdates":true,"loop":{"enabled":true}}`
	loop := storage.LoopRecord{ID: "loop_clean_noop_explicit_approve_spec", Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: "queued", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Loops.Upsert(ctx, loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	queue, err := runner.enqueue(ctx, enqueueInput{ProjectID: "project_1", LoopID: loop.ID, Repo: repo, PRNumber: prNumber})
	if err != nil {
		t.Fatalf("enqueue() error = %v", err)
	}
	claimed, err := fixture.repos.Queue.ClaimNextOfType(ctx, fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claimed == nil || claimed.ID != queue.ID {
		t.Fatalf("ClaimNextOfType() = (%#v, %v), want queued item %s", claimed, err, queue.ID)
	}

	result, err := runner.ProcessClaimedItem(ctx, *claimed)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "failed" || result.FailureKind != FailureRetryableAfterResume || !contains(result.Summary, "requires an APPROVED review marker") {
		t.Fatalf("result = %#v, want retryable approve-marker-required failure", result)
	}
	if len(github.removeLabelCalls) != 0 {
		t.Fatalf("removeLabelCalls = %#v, want no spec-reviewing removal", github.removeLabelCalls)
	}
	if len(github.addLabelCalls) != 0 {
		t.Fatalf("addLabelCalls = %#v, want no spec-ready add", github.addLabelCalls)
	}
}

func TestProcessClaimedItemDoesNotTransitionSpecLabelsForCleanNoopCommentPolicy(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{labels: []string{specpr.ReviewingLabel}, reviewRequests: []string{"octocat"}, reviewMarkerMissing: true}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "No actionable findings; added clean signal", Stdout: `__LOOPER_RESULT__={"summary":"No actionable findings; added clean signal"}`, ParseStatus: "parsed"}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, AllowAutoApprove: true, ReviewEvents: config.ReviewerReviewEventsConfig{Clean: config.ReviewerReviewEventComment}, LoopConfig: config.ReviewerLoopConfig{EnabledByDefault: true, QuietPeriodSeconds: 120, MaxIterationsPerPR: 20, MaxIterationsPerHead: 2, MaxWallClockSeconds: 14400, MaxConsecutiveFailures: 3, MaxAgentExecutionsPerPR: 25}})
	ctx := context.Background()
	nowISO := fixture.nowISO()
	repo := "acme/looper"
	prNumber := int64(42)
	metadata := `{"followUpdates":true,"loop":{"enabled":true}}`
	loop := storage.LoopRecord{ID: "loop_clean_noop_comment_policy_spec", Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: "queued", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Loops.Upsert(ctx, loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	queue, err := runner.enqueue(ctx, enqueueInput{ProjectID: "project_1", LoopID: loop.ID, Repo: repo, PRNumber: prNumber})
	if err != nil {
		t.Fatalf("enqueue() error = %v", err)
	}
	claimed, err := fixture.repos.Queue.ClaimNextOfType(ctx, fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claimed == nil || claimed.ID != queue.ID {
		t.Fatalf("ClaimNextOfType() = (%#v, %v), want queued item %s", claimed, err, queue.ID)
	}

	result, err := runner.ProcessClaimedItem(ctx, *claimed)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "success" {
		t.Fatalf("result = %#v, want success", result)
	}
	if len(github.addReactionCalls) != 1 || github.addReactionCalls[0].Content != "+1" {
		t.Fatalf("addReactionCalls = %#v, want one +1 reaction", github.addReactionCalls)
	}
	if len(github.removeLabelCalls) != 0 {
		t.Fatalf("removeLabelCalls = %#v, want no spec-reviewing removal for COMMENT clean policy", github.removeLabelCalls)
	}
	if len(github.addLabelCalls) != 0 {
		t.Fatalf("addLabelCalls = %#v, want no spec-ready add for COMMENT clean policy", github.addLabelCalls)
	}
}

func TestProcessClaimedItemDoesNotTreatActionableSummaryMentioningPriorCleanReviewAsNoop(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{reviewRequests: []string{"octocat"}, reviewMarkerMissing: true}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "Actionable issue found; prior clean review is outdated", Stdout: `__LOOPER_RESULT__={"summary":"Actionable issue found; prior clean review is outdated"}`, ParseStatus: "parsed"}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now})

	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNext() = (%#v, %v), want claimed queue item", claim, err)
	}
	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "failed" || result.FailureKind != FailureRetryableAfterResume || !contains(result.Summary, "no matching GitHub review marker") {
		t.Fatalf("result = %#v, want retryable missing-marker failure", result)
	}
	if github.reviewMarkerCalls == 0 {
		t.Fatalf("reviewMarkerCalls = %d, want marker lookup for actionable summary", github.reviewMarkerCalls)
	}
	if len(github.addReactionCalls) != 0 {
		t.Fatalf("addReactionCalls = %#v, want no clean noop reaction", github.addReactionCalls)
	}
}

func TestProcessClaimedItemStopsOnIdenticalReviewBodyWithDifferentMarkerHead(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	body := "same actionable finding <!-- looper:review loop=loop_identical_marker_head head=new-head outcome=actionable -->"
	github := &fakeGitHubGateway{reviewRequests: []string{"octocat"}, reviewMarkerBody: body}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "Same findings", Stdout: `__LOOPER_RESULT__={"summary":"posted review"}`}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, LoopConfig: config.ReviewerLoopConfig{EnabledByDefault: true, QuietPeriodSeconds: 120, MaxIterationsPerPR: 20, MaxIterationsPerHead: 2, MaxWallClockSeconds: 14400, MaxConsecutiveFailures: 3, MaxAgentExecutionsPerPR: 25, StopOnIdenticalOutput: true}})
	ctx := context.Background()
	nowISO := fixture.nowISO()
	repo := "acme/looper"
	prNumber := int64(42)
	previous := normalizedFindingFingerprint("same actionable finding <!-- looper:review loop=loop_identical_marker_head head=old-head outcome=actionable -->")
	metadata := fmt.Sprintf(`{"followUpdates":true,"loop":{"enabled":true,"lastOutputFingerprint":%q}}`, previous)
	loop := storage.LoopRecord{ID: "loop_identical_marker_head", Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: "queued", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Loops.Upsert(ctx, loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	queue, err := runner.enqueue(ctx, enqueueInput{ProjectID: "project_1", LoopID: loop.ID, Repo: repo, PRNumber: prNumber})
	if err != nil {
		t.Fatalf("enqueue() error = %v", err)
	}
	claimed, err := fixture.repos.Queue.ClaimNextOfType(ctx, fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claimed == nil || claimed.ID != queue.ID {
		t.Fatalf("ClaimNextOfType() = (%#v, %v), want queued item %s", claimed, err, queue.ID)
	}
	extraQueue := storage.QueueItemRecord{ID: "queue_identical_marker_head_extra", ProjectID: stringPtr("project_1"), LoopID: stringPtr(loop.ID), Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, DedupeKey: "reviewer:extra", Priority: storage.QueuePriorityReviewer, Status: "queued", AvailableAt: nowISO, Attempts: 0, MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Queue.Upsert(ctx, extraQueue); err != nil {
		t.Fatalf("Queue.Upsert(extra) error = %v", err)
	}

	result, err := runner.ProcessClaimedItem(ctx, *claimed)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "success" {
		t.Fatalf("result = %#v, want success", result)
	}
	updatedLoop, err := fixture.repos.Loops.GetByID(ctx, loop.ID)
	if err != nil || updatedLoop == nil {
		t.Fatalf("Loops.GetByID() = (%#v, %v), want loop", updatedLoop, err)
	}
	if updatedLoop.Status != "terminated" {
		t.Fatalf("loop status = %q, want terminated", updatedLoop.Status)
	}
	if updatedLoop.MetadataJSON == nil || !contains(*updatedLoop.MetadataJSON, `"terminationReason":"identical_output"`) {
		t.Fatalf("loop metadata = %#v, want identical_output termination", updatedLoop.MetadataJSON)
	}
	cancelledQueue, err := fixture.repos.Queue.GetByID(ctx, extraQueue.ID)
	if err != nil || cancelledQueue == nil {
		t.Fatalf("Queue.GetByID(extra) = (%#v, %v), want queue", cancelledQueue, err)
	}
	if cancelledQueue.Status != "cancelled" {
		t.Fatalf("extra queue status = %q, want cancelled", cancelledQueue.Status)
	}
}

func TestDiscoverPullRequestsDoesNotReactivateTerminalLoop(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: &fakeGitHubGateway{}, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})
	ctx := context.Background()
	nowISO := fixture.nowISO()
	repo := "acme/looper"
	prNumber := int64(42)
	metadata := `{"followUpdates":true,"loop":{"enabled":true,"status":"terminated","terminationReason":"identical_output"}}`
	loop := storage.LoopRecord{ID: "loop_terminal", Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: "terminated", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Loops.Upsert(ctx, loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	result, err := runner.DiscoverPullRequests(ctx, DiscoveryInput{ProjectID: "project_1", Repo: repo, Limit: 10})
	if err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	if len(result.QueueItems) != 0 {
		t.Fatalf("QueueItems = %#v, want none", result.QueueItems)
	}
	updatedLoop, err := fixture.repos.Loops.GetByID(ctx, loop.ID)
	if err != nil || updatedLoop == nil {
		t.Fatalf("Loops.GetByID() = (%#v, %v), want loop", updatedLoop, err)
	}
	if updatedLoop.Status != "terminated" {
		t.Fatalf("loop status = %q, want terminated", updatedLoop.Status)
	}
}

func TestDiscoverPullRequestsDoesNotReactivateMetadataTerminalLoop(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: &fakeGitHubGateway{}, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})
	ctx := context.Background()
	nowISO := fixture.nowISO()
	repo := "acme/looper"
	prNumber := int64(42)
	metadata := `{"followUpdates":true,"loop":{"enabled":true,"status":"terminated","terminationReason":"identical_output"}}`
	loop := storage.LoopRecord{ID: "loop_metadata_terminal", Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: "completed", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Loops.Upsert(ctx, loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	result, err := runner.DiscoverPullRequests(ctx, DiscoveryInput{ProjectID: "project_1", Repo: repo, Limit: 10})
	if err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	if len(result.QueueItems) != 0 {
		t.Fatalf("QueueItems = %#v, want none", result.QueueItems)
	}
	updatedLoop, err := fixture.repos.Loops.GetByID(ctx, loop.ID)
	if err != nil || updatedLoop == nil {
		t.Fatalf("Loops.GetByID() = (%#v, %v), want loop", updatedLoop, err)
	}
	if updatedLoop.Status != "completed" {
		t.Fatalf("loop status = %q, want unchanged completed", updatedLoop.Status)
	}
}

func TestProcessClaimedItemRestartsAutomaticResumeFromDiscoverForFreshReviewRequests(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{reviewRequests: []string{"octocat"}}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "Resumed review", Stdout: `__LOOPER_RESULT__={"summary":"posted review"}`}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now})
	nowISO := fixture.nowISO()
	repo := "acme/looper"
	prNumber := int64(42)
	loop := storage.LoopRecord{ID: "loop_legacy_resume", Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: "queued", CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	legacyCheckpoint := reviewerCheckpoint{Detail: &checkpointDetail{Title: "Review me", State: "OPEN", HeadSHA: "abc123"}, ResumePolicy: "advance_from_checkpoint"}
	legacyRun := storage.RunRecord{ID: "run_legacy", LoopID: loop.ID, Status: "failed", CurrentStep: stringPtr(string(stepClaim)), LastCompletedStep: stringPtr(string(stepFilter)), CheckpointJSON: stringPtr(mustMarshalJSON(legacyCheckpoint)), StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Runs.Upsert(context.Background(), legacyRun); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}
	queue, err := runner.enqueue(context.Background(), enqueueInput{ProjectID: "project_1", LoopID: loop.ID, Repo: repo, PRNumber: prNumber})
	if err != nil {
		t.Fatalf("enqueue() error = %v", err)
	}
	claimed, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claimed == nil || claimed.ID != queue.ID {
		t.Fatalf("ClaimNextOfType() = (%#v, %v), want queued item %s", claimed, err, queue.ID)
	}

	result, err := runner.ProcessClaimedItem(context.Background(), *claimed)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "success" {
		t.Fatalf("result = %#v, want success after fresh discover", result)
	}
	if github.viewCalls == 0 || len(agent.starts) != 1 {
		t.Fatalf("viewCalls=%d agent starts=%d, want fresh discover and review", github.viewCalls, len(agent.starts))
	}
}

func TestEnqueueScopesReviewerDedupeKeyToLoop(t *testing.T) {
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

	if err := fixture.repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: project2ID, Name: "Looper Two", RepoPath: repoPath2, BaseBranch: &baseBranch, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert(project_2) error = %v", err)
	}
	for _, loop := range []storage.LoopRecord{
		{ID: loop1ID, Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Status: "queued", CreatedAt: nowISO, UpdatedAt: nowISO},
		{ID: loop2ID, Seq: 2, ProjectID: project2ID, Type: "reviewer", TargetType: "pull_request", Status: "queued", CreatedAt: nowISO, UpdatedAt: nowISO},
	} {
		if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
			t.Fatalf("Loops.Upsert(%s) error = %v", loop.ID, err)
		}
	}

	first, err := runner.enqueue(context.Background(), enqueueInput{ProjectID: "project_1", LoopID: loop1ID, Repo: repo, PRNumber: prNumber})
	if err != nil {
		t.Fatalf("enqueue(first) error = %v", err)
	}
	second, err := runner.enqueue(context.Background(), enqueueInput{ProjectID: project2ID, LoopID: loop2ID, Repo: repo, PRNumber: prNumber})
	if err != nil {
		t.Fatalf("enqueue(second) error = %v", err)
	}
	if first.ID == second.ID {
		t.Fatalf("enqueue(second) reused queue item %q across loops", second.ID)
	}
	if second.LoopID == nil || *second.LoopID != loop2ID {
		t.Fatalf("second loopID = %#v, want %q", second.LoopID, loop2ID)
	}
	if second.DedupeKey != buildReviewerDedupeKey(project2ID, loop2ID, repo, prNumber) {
		t.Fatalf("second dedupe key = %q, want scoped reviewer key", second.DedupeKey)
	}
	items, err := fixture.repos.Queue.List(context.Background())
	if err != nil {
		t.Fatalf("Queue.List() error = %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("len(Queue.List()) = %d, want 2", len(items))
	}
}

func TestProcessClaimedItemCompletesAgentNativeReviewWithoutGoPublish(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "Please add tests", Stdout: `__LOOPER_RESULT__={"summary":"posted review"}`}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, AllowAutoApprove: true})

	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	firstClaim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || firstClaim == nil {
		t.Fatalf("ClaimNext() = (%#v, %v), want claimed queue item", firstClaim, err)
	}
	firstResult, err := runner.ProcessClaimedItem(context.Background(), *firstClaim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem(first) error = %v", err)
	}
	if firstResult.Status != "success" {
		t.Fatalf("first result = %#v, want success", firstResult)
	}
	if len(agent.starts) != 1 {
		t.Fatalf("len(agent.starts) = %d, want 1", len(agent.starts))
	}
	runs, err := fixture.repos.Runs.ListByLoop(context.Background(), firstResult.LoopID)
	if err != nil {
		t.Fatalf("Runs.ListByLoop() error = %v", err)
	}
	if len(runs) == 0 || runs[0].LastCompletedStep == nil || *runs[0].LastCompletedStep != string(stepPublish) {
		t.Fatalf("runs[0] = %#v, want lastCompletedStep=publish", runs)
	}
	queueAfterSuccess, err := fixture.repos.Queue.GetByID(context.Background(), firstClaim.ID)
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	if queueAfterSuccess == nil || queueAfterSuccess.Status != "completed" {
		t.Fatalf("queue after success = %#v, want completed", queueAfterSuccess)
	}
	loop, err := fixture.repos.Loops.GetByID(context.Background(), firstResult.LoopID)
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if loop == nil || loop.Status != "completed" || loop.MetadataJSON == nil || !contains(*loop.MetadataJSON, `"lastPublishedHeadSha":"abc123"`) {
		t.Fatalf("loop after success = %#v, want completed with lastPublishedHeadSha", loop)
	}
}

func TestProcessClaimedItemAgentNativeReviewCompletesWithoutGoPublish(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{reviewRequests: []string{"octocat"}}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "Please add tests", Stdout: `__LOOPER_RESULT__={"summary":"posted review"}`}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, AllowAutoApprove: true})

	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	firstClaim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || firstClaim == nil {
		t.Fatalf("ClaimNext() = (%#v, %v), want claimed queue item", firstClaim, err)
	}
	firstResult, err := runner.ProcessClaimedItem(context.Background(), *firstClaim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem(first) error = %v", err)
	}
	if firstResult.Status != "success" {
		t.Fatalf("first result = %#v, want success", firstResult)
	}
	if len(agent.starts) != 1 {
		t.Fatalf("agent starts=%d, want agent review", len(agent.starts))
	}
}

func TestProcessClaimedItemRequiresSideEffectsBeforeRecordingPublishSuccess(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{reviewRequests: []string{"octocat"}, reviewMarkerOutcome: "clean", addReactionErr: fmt.Errorf("reaction failed")}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "LGTM", Stdout: `__LOOPER_RESULT__={"summary":"posted review"}`}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, AllowAutoApprove: true})

	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNext() = (%#v, %v), want claimed queue item", claim, err)
	}
	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "failed" || result.FailureKind != FailureRetryableAfterResume || !contains(result.Summary, "Failed to add clean-review reaction") {
		t.Fatalf("result = %#v, want retryable side-effect failure", result)
	}
	loop, err := fixture.repos.Loops.GetByID(context.Background(), result.LoopID)
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if loop == nil || (loop.MetadataJSON != nil && contains(*loop.MetadataJSON, `"lastPublishedHeadSha":"abc123"`)) {
		t.Fatalf("loop after failed side effect = %#v, want no lastPublishedHeadSha", loop)
	}
}

func TestProcessClaimedItemRequiresActionableSideEffectsBeforeRecordingPublishSuccess(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{reviewRequests: []string{"octocat"}, reviewMarkerOutcome: "actionable", removeReactionErr: fmt.Errorf("remove reaction failed")}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "Please add tests", Stdout: `__LOOPER_RESULT__={"summary":"posted review"}`}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, AllowAutoApprove: true})

	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNext() = (%#v, %v), want claimed queue item", claim, err)
	}
	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "failed" || result.FailureKind != FailureRetryableAfterResume || !contains(result.Summary, "Failed to remove stale clean-review reaction") {
		t.Fatalf("result = %#v, want retryable side-effect failure", result)
	}
	loop, err := fixture.repos.Loops.GetByID(context.Background(), result.LoopID)
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if loop == nil || (loop.MetadataJSON != nil && contains(*loop.MetadataJSON, `"lastPublishedHeadSha":"abc123"`)) {
		t.Fatalf("loop after failed actionable side effect = %#v, want no lastPublishedHeadSha", loop)
	}
}

func TestProcessClaimedItemAppliesCleanSpecSideEffectsBeforePublishSuccess(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{labels: []string{specpr.ReviewingLabel}, reviewRequests: []string{"octocat"}, reviewMarkerOutcome: "clean", reviewMarkerEvent: ReviewEventApprove}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "LGTM", Stdout: `__LOOPER_RESULT__={"summary":"posted review"}`}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, AllowAutoApprove: false, ReviewEvents: config.ReviewerReviewEventsConfig{Clean: config.ReviewerReviewEventApprove}})

	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNext() = (%#v, %v), want claimed queue item", claim, err)
	}
	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "success" {
		t.Fatalf("result = %#v, want success", result)
	}
	if len(github.addReactionCalls) != 1 || github.addReactionCalls[0].Content != "+1" {
		t.Fatalf("addReactionCalls = %#v, want one +1 reaction", github.addReactionCalls)
	}
	if len(github.removeLabelCalls) != 1 || github.removeLabelCalls[0].Labels[0] != specpr.ReviewingLabel {
		t.Fatalf("removeLabelCalls = %#v, want spec-reviewing removal", github.removeLabelCalls)
	}
	if len(github.addLabelCalls) != 1 || github.addLabelCalls[0].Labels[0] != specpr.ReadyLabel {
		t.Fatalf("addLabelCalls = %#v, want spec-ready add", github.addLabelCalls)
	}
}

func TestProcessClaimedItemAppliesCleanSpecSideEffectsWithConfiguredReviewingLabel(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	const reviewingLabel = "team:spec-reviewing"
	github := &fakeGitHubGateway{labels: []string{reviewingLabel}, reviewRequests: []string{"octocat"}, reviewMarkerOutcome: "clean", reviewMarkerEvent: ReviewEventApprove}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "LGTM", Stdout: `__LOOPER_RESULT__={"summary":"posted review"}`}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, AllowAutoApprove: true, DiscoveryPolicy: DiscoveryPolicy{AutoDiscovery: true, IncludeDrafts: false, RequireReviewRequest: true, Labels: []string{}, LabelMode: config.LabelModeAll, IncludeSpecReviewingLabel: true, SpecReviewingLabel: reviewingLabel}})

	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNext() = (%#v, %v), want claimed queue item", claim, err)
	}
	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "success" {
		t.Fatalf("result = %#v, want success", result)
	}
	if len(github.removeLabelCalls) != 1 || github.removeLabelCalls[0].Labels[0] != reviewingLabel {
		t.Fatalf("removeLabelCalls = %#v, want configured reviewing label removal", github.removeLabelCalls)
	}
	if len(github.addLabelCalls) != 1 || github.addLabelCalls[0].Labels[0] != specpr.ReadyLabel {
		t.Fatalf("addLabelCalls = %#v, want spec-ready add", github.addLabelCalls)
	}
}

func TestProcessClaimedItemRefreshesReviewStateBeforeSpecReadyTransition(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{labels: []string{specpr.ReviewingLabel}, reviewRequests: []string{"octocat"}, reviewDecision: "CHANGES_REQUESTED", useReviewStateAfterFirstView: true, reviewMarkerOutcome: "clean", reviewMarkerEvent: ReviewEventApprove}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "LGTM", Stdout: `__LOOPER_RESULT__={"summary":"posted review"}`}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, AllowAutoApprove: true})

	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNext() = (%#v, %v), want claimed queue item", claim, err)
	}
	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "success" {
		t.Fatalf("result = %#v, want success", result)
	}
	if github.viewCalls < 2 {
		t.Fatalf("viewCalls = %d, want publish detail refresh before spec-ready transition", github.viewCalls)
	}
	if len(github.removeLabelCalls) != 1 || github.removeLabelCalls[0].Labels[0] != specpr.ReviewingLabel {
		t.Fatalf("removeLabelCalls = %#v, want spec-reviewing removal after refresh", github.removeLabelCalls)
	}
	if len(github.addLabelCalls) != 1 || github.addLabelCalls[0].Labels[0] != specpr.ReadyLabel {
		t.Fatalf("addLabelCalls = %#v, want spec-ready add after refresh", github.addLabelCalls)
	}
}

func TestProcessClaimedItemDoesNotTransitionSpecLabelsForCleanCommentReview(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{labels: []string{specpr.ReviewingLabel}, reviewRequests: []string{"octocat"}, reviewMarkerOutcome: "clean", reviewMarkerEvent: ReviewEventComment}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "LGTM", Stdout: `__LOOPER_RESULT__={"summary":"posted review"}`}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, AllowAutoApprove: false, ReviewEvents: config.ReviewerReviewEventsConfig{Clean: config.ReviewerReviewEventApprove}})

	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNext() = (%#v, %v), want claimed queue item", claim, err)
	}
	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "success" {
		t.Fatalf("result = %#v, want success", result)
	}
	if len(github.addReactionCalls) != 1 || github.addReactionCalls[0].Content != "+1" {
		t.Fatalf("addReactionCalls = %#v, want one +1 reaction", github.addReactionCalls)
	}
	if len(github.removeLabelCalls) != 0 {
		t.Fatalf("removeLabelCalls = %#v, want no spec-reviewing removal for COMMENT review", github.removeLabelCalls)
	}
	if len(github.addLabelCalls) != 0 {
		t.Fatalf("addLabelCalls = %#v, want no spec-ready add for COMMENT review", github.addLabelCalls)
	}
}

func TestProcessClaimedItemDoesNotTransitionSpecLabelsWhenPRReviewStateIsNotClean(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name           string
		reviewDecision string
		comments       []map[string]any
	}{
		{name: "changes requested", reviewDecision: "CHANGES_REQUESTED"},
		{name: "unresolved thread", comments: []map[string]any{{"state": "UNRESOLVED"}}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fixture := newRunnerFixture(t)
			github := &fakeGitHubGateway{labels: []string{specpr.ReviewingLabel}, reviewRequests: []string{"octocat"}, reviewDecision: tt.reviewDecision, comments: tt.comments, reviewMarkerOutcome: "clean", reviewMarkerEvent: ReviewEventApprove}
			agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "LGTM", Stdout: `__LOOPER_RESULT__={"summary":"posted review"}`}}}
			runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, AllowAutoApprove: true})

			if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
				t.Fatalf("DiscoverPullRequests() error = %v", err)
			}
			claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
			if err != nil || claim == nil {
				t.Fatalf("ClaimNext() = (%#v, %v), want claimed queue item", claim, err)
			}
			result, err := runner.ProcessClaimedItem(context.Background(), *claim)
			if err != nil {
				t.Fatalf("ProcessClaimedItem() error = %v", err)
			}
			if result.Status != "success" {
				t.Fatalf("result = %#v, want success", result)
			}
			if len(github.addReactionCalls) != 1 || github.addReactionCalls[0].Content != "+1" {
				t.Fatalf("addReactionCalls = %#v, want one +1 reaction", github.addReactionCalls)
			}
			if len(github.removeLabelCalls) != 0 {
				t.Fatalf("removeLabelCalls = %#v, want no spec-reviewing removal for unclean PR", github.removeLabelCalls)
			}
			if len(github.addLabelCalls) != 0 {
				t.Fatalf("addLabelCalls = %#v, want no spec-ready add for unclean PR", github.addLabelCalls)
			}
		})
	}
}

func TestProcessClaimedItemFailsWhenAgentMissingCompletionMarker(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{reviewRequests: []string{"octocat"}, reviewMarkerMissing: true}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "posted maybe", Stdout: "posted maybe"}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now})

	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNext() = (%#v, %v), want claimed queue item", claim, err)
	}
	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "failed" || result.FailureKind != FailureNonRetryable || !contains(result.Summary, "valid completion marker") {
		t.Fatalf("result = %#v, want non-retryable completion marker failure", result)
	}
}

func TestProcessClaimedItemRecoversMissingCompletionMarkerWhenReviewMarkerExists(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{reviewRequests: []string{"octocat"}, reviewMarkerOutcome: "actionable"}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "posted maybe", Stdout: "posted maybe"}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now})

	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNext() = (%#v, %v), want claimed queue item", claim, err)
	}
	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "success" {
		t.Fatalf("result = %#v, want success after marker recovery", result)
	}
	loop, err := fixture.repos.Loops.GetByID(context.Background(), result.LoopID)
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if loop == nil || loop.MetadataJSON == nil || !contains(*loop.MetadataJSON, `"lastPublishedHeadSha":"abc123"`) {
		t.Fatalf("loop after recovered publish = %#v, want lastPublishedHeadSha recorded", loop)
	}
	if github.reviewMarkerCalls != 2 {
		t.Fatalf("review marker calls = %d, want parse recovery lookup plus publish verification", github.reviewMarkerCalls)
	}
}

func TestProcessClaimedItemRecoversFailedAgentRunWhenReviewMarkerExists(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{reviewRequests: []string{"octocat"}, removeReviewRequestOnSecondView: true, reviewMarkerOutcome: "clean"}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "failed", Summary: "posted review, failed to add reaction"}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now})

	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNext() = (%#v, %v), want claimed queue item", claim, err)
	}
	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "success" {
		t.Fatalf("result = %#v, want success after failed-run marker recovery", result)
	}
	if github.reviewMarkerCalls != 2 {
		t.Fatalf("review marker calls = %d, want non-completed recovery lookup plus publish verification", github.reviewMarkerCalls)
	}
	if len(agent.starts) != 1 {
		t.Fatalf("len(agent.starts) = %d, want one review execution", len(agent.starts))
	}
}

func TestProcessClaimedItemRetriesWhenAgentReviewMarkerMissing(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{reviewRequests: []string{"octocat"}, reviewMarkerMissing: true, reviewMarkerBodyExplicit: true, reviewMarkerInlineCommentBodies: []string{"inline-only retry finding"}}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "posted", Stdout: `__LOOPER_RESULT__={"summary":"posted review"}`}, {Status: "completed", Summary: "posted again", Stdout: `__LOOPER_RESULT__={"summary":"posted review again"}`}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now})

	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNext() = (%#v, %v), want claimed queue item", claim, err)
	}
	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "failed" || result.FailureKind != FailureRetryableAfterResume || !contains(result.Summary, "no matching GitHub review marker") {
		t.Fatalf("result = %#v, want retryable missing marker failure", result)
	}
	if len(agent.starts) != 1 {
		t.Fatalf("len(agent.starts) after first attempt = %d, want 1", len(agent.starts))
	}
	github.reviewMarkerMissing = false
	fixture.advance(time.Hour)
	claim, err = fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claim == nil {
		t.Fatalf("retry ClaimNext() = (%#v, %v), want claimed queue item", claim, err)
	}
	result, err = runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("retry ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "success" {
		t.Fatalf("retry result = %#v, want success", result)
	}
	if len(agent.starts) != 1 {
		t.Fatalf("len(agent.starts) after retry = %d, want marker recheck without review rerun", len(agent.starts))
	}
	if github.reviewMarkerCalls != 2 {
		t.Fatalf("review marker calls = %d, want initial lookup plus retry", github.reviewMarkerCalls)
	}
	updatedLoop, err := fixture.repos.Loops.GetByID(context.Background(), result.LoopID)
	if err != nil || updatedLoop == nil || updatedLoop.MetadataJSON == nil {
		t.Fatalf("Loops.GetByID() = (%#v, %v), want loop metadata", updatedLoop, err)
	}
	want := normalizedFindingFingerprint(strings.Join(github.reviewMarkerInlineCommentBodies, "\n"))
	if !contains(*updatedLoop.MetadataJSON, fmt.Sprintf(`"lastOutputFingerprint":"%s"`, want)) {
		t.Fatalf("loop metadata = %s, want inline-only retry fingerprint %s", *updatedLoop.MetadataJSON, want)
	}
}

func TestProcessClaimedItemRerunsReviewAfterRepeatedAgentReviewMarkerMisses(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{reviewRequests: []string{"octocat"}, reviewMarkerMissing: true}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "posted", Stdout: `__LOOPER_RESULT__={"summary":"posted review"}`}, {Status: "completed", Summary: "posted again", Stdout: `__LOOPER_RESULT__={"summary":"posted review again"}`}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now})

	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNext() = (%#v, %v), want claimed queue item", claim, err)
	}
	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "failed" || result.FailureKind != FailureRetryableAfterResume || !contains(result.Summary, "retrying marker verification") {
		t.Fatalf("result = %#v, want retryable marker recheck failure", result)
	}
	if len(agent.starts) != 1 {
		t.Fatalf("len(agent.starts) after first attempt = %d, want 1", len(agent.starts))
	}

	fixture.advance(time.Hour)
	claim, err = fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claim == nil {
		t.Fatalf("retry ClaimNext() = (%#v, %v), want claimed queue item", claim, err)
	}
	result, err = runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("retry ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "failed" || result.FailureKind != FailureRetryableAfterResume || !contains(result.Summary, "no matching GitHub review marker") {
		t.Fatalf("retry result = %#v, want retryable missing marker failure", result)
	}
	if len(agent.starts) != 1 {
		t.Fatalf("len(agent.starts) after marker retry = %d, want no review rerun yet", len(agent.starts))
	}
	latestRun, err := fixture.repos.Runs.GetLatestByLoopID(context.Background(), result.LoopID)
	if err != nil {
		t.Fatalf("GetLatestByLoopID() error = %v", err)
	}
	if latestRun == nil {
		t.Fatal("latest run = nil, want failed run")
	}
	checkpoint := parseCheckpoint(latestRun.CheckpointJSON)
	if checkpoint.ResumePolicy != "rerun_review" || checkpoint.PendingReview != nil {
		t.Fatalf("checkpoint = %#v, want cleared pending review with rerun_review after repeated marker misses", checkpoint)
	}
}

func TestProcessClaimedItemRecordsReviewWhenRequestRemovedAfterMarkerAppears(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{reviewRequests: []string{"octocat"}, reviewMarkerMissing: true}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "posted", Stdout: `__LOOPER_RESULT__={"summary":"posted review"}`}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now})

	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNext() = (%#v, %v), want claimed queue item", claim, err)
	}
	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "failed" || result.FailureKind != FailureRetryableAfterResume || !contains(result.Summary, "no matching GitHub review marker") {
		t.Fatalf("result = %#v, want retryable missing marker failure", result)
	}
	github.reviewMarkerMissing = false
	github.reviewRequests = []string{"someoneelse"}
	fixture.advance(time.Hour)
	claim, err = fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claim == nil {
		t.Fatalf("retry ClaimNext() = (%#v, %v), want claimed queue item", claim, err)
	}
	result, err = runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("retry ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "success" {
		t.Fatalf("retry result = %#v, want publish success after marker appears", result)
	}
	if len(agent.starts) != 1 {
		t.Fatalf("len(agent.starts) after retry = %d, want no second review", len(agent.starts))
	}
	loop, err := fixture.repos.Loops.GetByID(context.Background(), result.LoopID)
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if loop == nil || loop.MetadataJSON == nil || !contains(*loop.MetadataJSON, `"lastPublishedHeadSha":"abc123"`) {
		t.Fatalf("loop after marker recovery = %#v, want lastPublishedHeadSha recorded", loop)
	}
}

func TestProcessClaimedItemSkipsRerunReviewWhenRequestRemovedAndMarkerMissing(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{reviewRequests: []string{"octocat"}, reviewMarkerMissing: true}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "posted", Stdout: `__LOOPER_RESULT__={"summary":"posted review"}`}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now})

	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNext() = (%#v, %v), want claimed queue item", claim, err)
	}
	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "failed" || result.FailureKind != FailureRetryableAfterResume || !contains(result.Summary, "no matching GitHub review marker") {
		t.Fatalf("result = %#v, want retryable missing marker failure", result)
	}
	github.reviewRequests = []string{"someoneelse"}
	fixture.advance(time.Hour)
	claim, err = fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claim == nil {
		t.Fatalf("retry ClaimNext() = (%#v, %v), want claimed queue item", claim, err)
	}
	result, err = runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("retry ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "skipped" || !contains(result.Summary, "current user is not requested for review") {
		t.Fatalf("retry result = %#v, want eligibility skip when marker is still missing", result)
	}
	if len(agent.starts) != 1 {
		t.Fatalf("len(agent.starts) after retry = %d, want no second review", len(agent.starts))
	}
}

func TestProcessClaimedItemRejectsUnverifiableLegacyPendingReview(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{reviewRequests: []string{"octocat"}, reviewMarkerMissing: true}
	agent := &fakeAgentExecutor{}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now})
	ctx := context.Background()

	prNumber := int64(42)
	repo := "acme/looper"
	loopTarget := "pr:42"
	loop := storage.LoopRecord{ID: "loop_legacy", Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", TargetID: &loopTarget, Repo: &repo, PRNumber: &prNumber, Status: "queued", CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}
	if err := fixture.repos.Loops.Upsert(ctx, loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	if _, err := runner.enqueue(ctx, enqueueInput{ProjectID: loop.ProjectID, LoopID: loop.ID, Repo: repo, PRNumber: prNumber}); err != nil {
		t.Fatalf("enqueue() error = %v", err)
	}
	claim, err := fixture.repos.Queue.ClaimNextOfType(ctx, fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNextOfType() = (%#v, %v), want claimed queue item", claim, err)
	}

	legacyCheckpoint := reviewerCheckpoint{
		Detail:        &checkpointDetail{Title: "Review me", State: "OPEN", HeadSHA: "abc123", ReviewRequests: []string{"octocat"}},
		Snapshot:      &checkpointSnapshot{HeadSHA: "abc123"},
		PendingReview: &pendingReviewCheckpoint{HeadSHA: "abc123", Event: ReviewEventComment, Summary: "legacy review already posted"},
		ResumePolicy:  "advance_from_checkpoint",
	}
	checkpointJSON := mustMarshalJSON(legacyCheckpoint)
	run := storage.RunRecord{ID: "run_legacy", LoopID: loop.ID, Status: "failed", CurrentStep: stringPtr(string(stepPublish)), LastCompletedStep: stringPtr(string(stepReview)), CheckpointJSON: &checkpointJSON, StartedAt: fixture.nowISO(), CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}
	if err := fixture.repos.Runs.Upsert(ctx, run); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}

	result, err := runner.ProcessClaimedItem(ctx, *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "failed" || result.FailureKind != FailureRetryableAfterResume || !contains(result.Summary, "Legacy pending review checkpoint cannot be verified") {
		t.Fatalf("result = %#v, want retryable legacy verification failure", result)
	}
	if len(agent.starts) != 0 {
		t.Fatalf("len(agent.starts) = %d, want no review rerun in failed publish attempt", len(agent.starts))
	}
	if github.reviewMarkerCalls != 0 {
		t.Fatalf("reviewMarkerCalls = %d, want agent-native marker lookup skipped for legacy pending review", github.reviewMarkerCalls)
	}
	updatedLoop, err := fixture.repos.Loops.GetByID(ctx, loop.ID)
	if err != nil || updatedLoop == nil {
		t.Fatalf("Loops.GetByID() = (%#v, %v), want loop", updatedLoop, err)
	}
	if updatedLoop.MetadataJSON != nil && contains(*updatedLoop.MetadataJSON, `"lastPublishedHeadSha":"abc123"`) {
		t.Fatalf("loop metadata = %v, want no legacy publish progress", updatedLoop.MetadataJSON)
	}
}

func TestProcessClaimedItemRetriesWhenAgentNativeReviewApprovesWithoutPermission(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{reviewRequests: []string{"octocat"}, reviewMarkerEvent: ReviewEventApprove}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "Looks good", Stdout: `__LOOPER_RESULT__={"summary":"posted review"}`}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, AllowAutoApprove: false})

	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNext() = (%#v, %v), want claimed queue item", claim, err)
	}
	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "failed" || result.FailureKind != FailureRetryableAfterResume || !contains(result.Summary, "no matching GitHub review marker") {
		t.Fatalf("result = %#v, want retryable disallowed approval marker failure", result)
	}
	loop, err := fixture.repos.Loops.GetByID(context.Background(), result.LoopID)
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if loop == nil || (loop.MetadataJSON != nil && contains(*loop.MetadataJSON, `"lastPublishedHeadSha":"abc123"`)) {
		t.Fatalf("loop after failed publish = %#v, want no lastPublishedHeadSha", loop)
	}
}

func TestProcessClaimedItemRecordsAgentNativePublishWhenReviewRequestRemovedAfterPosting(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{reviewRequests: []string{"octocat"}, removeReviewRequestOnSecondView: true}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "Please add tests", Stdout: `__LOOPER_RESULT__={"summary":"posted review"}`}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, AllowAutoApprove: true})

	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNext() = (%#v, %v), want claimed queue item", claim, err)
	}
	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "success" {
		t.Fatalf("result = %#v, want publish success after marker verification", result)
	}
	loop, err := fixture.repos.Loops.GetByID(context.Background(), result.LoopID)
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if loop == nil || loop.MetadataJSON == nil || !contains(*loop.MetadataJSON, `"lastPublishedHeadSha":"abc123"`) {
		t.Fatalf("loop after verified publish = %#v, want lastPublishedHeadSha recorded", loop)
	}
}

func TestProcessClaimedItemRecordsPublishedHeadForAgentNativeReview(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{reviewRequests: []string{"octocat"}}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "Please add tests", Stdout: `__LOOPER_RESULT__={"summary":"posted review"}`}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, AllowAutoApprove: true})

	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	firstClaim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || firstClaim == nil {
		t.Fatalf("ClaimNext() = (%#v, %v), want claimed queue item", firstClaim, err)
	}
	firstResult, err := runner.ProcessClaimedItem(context.Background(), *firstClaim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem(first) error = %v", err)
	}
	if firstResult.Status != "success" {
		t.Fatalf("first result = %#v, want success", firstResult)
	}
	if len(agent.starts) != 1 {
		t.Fatalf("agent starts=%d, want agent-native review", len(agent.starts))
	}
	loop, err := fixture.repos.Loops.GetByID(context.Background(), firstResult.LoopID)
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if loop == nil || loop.MetadataJSON == nil || !contains(*loop.MetadataJSON, `"lastPublishedHeadSha":"abc123"`) {
		t.Fatalf("loop after skipped retry = %#v, want lastPublishedHeadSha recorded", loop)
	}
}

func TestProcessClaimedItemAgentNativeReviewCompletesWithoutPublishRetry(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	fixture.repos.Locks.SetNow(fixture.now)
	github := &fakeGitHubGateway{}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "Please add tests", Stdout: `__LOOPER_RESULT__={"summary":"posted review"}`}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, AllowAutoApprove: true})

	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	claim1, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claim1 == nil {
		t.Fatalf("first ClaimNextOfType() = (%#v, %v), want claimed item", claim1, err)
	}
	first, err := runner.ProcessClaimedItem(context.Background(), *claim1)
	if err != nil {
		t.Fatalf("ProcessClaimedItem(first) error = %v", err)
	}
	if first.Status != "success" {
		t.Fatalf("first = %#v, want success", first)
	}
	queue, err := fixture.repos.Queue.GetByID(context.Background(), claim1.ID)
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	if queue == nil || queue.Status != "completed" {
		t.Fatalf("queue = %#v, want completed item", queue)
	}
}

func TestProcessClaimedItemRestartsFromDiscoverWhenHeadChangesBeforePublish(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{changeHeadOnSecondView: true}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "Review old head", Stdout: `__LOOPER_RESULT__={"summary":"posted review"}`}, {Status: "completed", Summary: "Review new head", Stdout: `__LOOPER_RESULT__={"summary":"posted review"}`}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now})

	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	firstClaim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || firstClaim == nil {
		t.Fatalf("ClaimNext() = (%#v, %v), want claimed queue item", firstClaim, err)
	}
	firstResult, err := runner.ProcessClaimedItem(context.Background(), *firstClaim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem(first) error = %v", err)
	}
	if firstResult.Status != "failed" || firstResult.FailureKind != FailureRetryableAfterResume || !contains(firstResult.Summary, "PR head changed before publish") {
		t.Fatalf("first result = %#v, want retryable head-change failure", firstResult)
	}
	if len(agent.starts) != 1 {
		t.Fatalf("agent starts=%d, want 1", len(agent.starts))
	}
}

func TestProcessClaimedItemNotifiesWhenReviewAgentStarts(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "Looks good", Stdout: `__LOOPER_RESULT__={"summary":"posted review"}`}}}
	notifications := make([]AgentExecutionStartedInput, 0, 1)
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, OnAgentExecutionStarted: func(_ context.Context, input AgentExecutionStartedInput) error {
		notifications = append(notifications, input)
		return nil
	}})

	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	claimed, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claimed == nil {
		t.Fatalf("ClaimNext() = (%#v, %v), want claimed queue item", claimed, err)
	}
	result, err := runner.ProcessClaimedItem(context.Background(), *claimed)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "success" {
		t.Fatalf("result = %#v, want success", result)
	}
	if len(notifications) != 1 {
		t.Fatalf("len(notifications) = %d, want 1", len(notifications))
	}
	if notifications[0].Subtitle != "acme/looper#42" || notifications[0].Body != "Review started" {
		t.Fatalf("notifications[0] = %#v, want review-start payload", notifications[0])
	}
}

func TestProcessClaimedItemRunsReviewerInDedicatedWorktree(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{}
	git := &fakeGitGateway{}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "Looks good", Stdout: `__LOOPER_RESULT__={"summary":"posted review"}`}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now})

	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	claimed, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claimed == nil {
		t.Fatalf("ClaimNext() = (%#v, %v), want claimed queue item", claimed, err)
	}
	result, err := runner.ProcessClaimedItem(context.Background(), *claimed)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "success" {
		t.Fatalf("result = %#v, want success", result)
	}
	if len(git.createCalls) != 1 {
		t.Fatalf("len(git.createCalls) = %d, want 1", len(git.createCalls))
	}
	if git.createCalls[0].Branch != "pr-42-head" {
		t.Fatalf("create branch = %q, want PR-scoped branch", git.createCalls[0].Branch)
	}
	if git.createCalls[0].PRNumber != 42 {
		t.Fatalf("create PR number = %d, want 42", git.createCalls[0].PRNumber)
	}
	if len(git.prepareCalls) != 1 {
		t.Fatalf("len(git.prepareCalls) = %d, want 1", len(git.prepareCalls))
	}
	if git.prepareCalls[0].Branch != "pr-42-head" {
		t.Fatalf("prepare branch = %q, want PR-scoped branch", git.prepareCalls[0].Branch)
	}
	if git.prepareCalls[0].Ref != "refs/pull/42/head" {
		t.Fatalf("prepare ref = %q, want PR head ref", git.prepareCalls[0].Ref)
	}
	if len(agent.starts) != 1 {
		t.Fatalf("len(agent.starts) = %d, want 1", len(agent.starts))
	}
	if len(git.cleanupCalls) != 1 {
		t.Fatalf("len(git.cleanupCalls) = %d, want 1", len(git.cleanupCalls))
	}
	if agent.starts[0].WorkingDirectory != git.worktreePath {
		t.Fatalf("agent working dir = %q, want %q", agent.starts[0].WorkingDirectory, git.worktreePath)
	}
	project, err := fixture.repos.Projects.GetByID(context.Background(), "project_1")
	if err != nil || project == nil {
		t.Fatalf("Projects.GetByID() = (%#v, %v), want project", project, err)
	}
	if agent.starts[0].WorkingDirectory == project.RepoPath {
		t.Fatalf("agent working dir = repo path %q, want dedicated worktree", project.RepoPath)
	}
}

func TestRunPrepareWorktreeStepFallsBackWhenCheckpointLacksHeadRef(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	git := &fakeGitGateway{}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: &fakeGitHubGateway{}, Git: git, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})

	project, err := fixture.repos.Projects.GetByID(context.Background(), "project_1")
	if err != nil || project == nil {
		t.Fatalf("Projects.GetByID() = (%#v, %v), want project", project, err)
	}

	checkpoint, err := runner.runPrepareWorktreeStep(context.Background(), stepInput{
		Project:  *project,
		Repo:     "acme/looper",
		PRNumber: 42,
		Checkpoint: reviewerCheckpoint{
			Detail:   &checkpointDetail{HeadSHA: "abc123", BaseRefName: "main"},
			Snapshot: &checkpointSnapshot{HeadSHA: "abc123"},
		},
	})
	if err != nil {
		t.Fatalf("runPrepareWorktreeStep() error = %v", err)
	}
	if len(git.createCalls) != 1 {
		t.Fatalf("len(git.createCalls) = %d, want 1", len(git.createCalls))
	}
	if git.createCalls[0].Branch != "pr-42-head" {
		t.Fatalf("create branch = %q, want fallback branch", git.createCalls[0].Branch)
	}
	if git.createCalls[0].PRNumber != 42 {
		t.Fatalf("create PR number = %d, want 42", git.createCalls[0].PRNumber)
	}
	if len(git.prepareCalls) != 1 {
		t.Fatalf("len(git.prepareCalls) = %d, want 1", len(git.prepareCalls))
	}
	if git.prepareCalls[0].Ref != "refs/pull/42/head" {
		t.Fatalf("prepare ref = %q, want PR head ref", git.prepareCalls[0].Ref)
	}
	if checkpoint.Worktree == nil || checkpoint.Worktree.Branch != "pr-42-head" {
		t.Fatalf("checkpoint worktree = %#v, want fallback branch", checkpoint.Worktree)
	}
}

func TestReviewerWorktreeBranchIgnoresHeadRefName(t *testing.T) {
	t.Parallel()

	branch := reviewerWorktreeBranch(42, reviewerCheckpoint{
		Detail:   &checkpointDetail{HeadRefName: "patch-1"},
		Worktree: &checkpointWorktree{Branch: "pr-42-head"},
	})
	if branch != "pr-42-head" {
		t.Fatalf("reviewerWorktreeBranch() = %q, want existing PR-scoped branch", branch)
	}

	branch = reviewerWorktreeBranch(42, reviewerCheckpoint{
		Detail: &checkpointDetail{HeadRefName: "main"},
	})
	if branch != "pr-42-head" {
		t.Fatalf("reviewerWorktreeBranch() = %q, want PR-scoped fallback", branch)
	}
}

func TestRunReviewStepRepreparesMissingReviewerWorktree(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	git := &fakeGitGateway{worktreePath: filepath.Join(t.TempDir(), "reviewer-worktree")}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "Looks good", Stdout: `__LOOPER_RESULT__={"summary":"posted review"}`}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: &fakeGitHubGateway{}, Git: git, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now})

	project, err := fixture.repos.Projects.GetByID(context.Background(), "project_1")
	if err != nil || project == nil {
		t.Fatalf("Projects.GetByID() = (%#v, %v), want project", project, err)
	}

	checkpoint, err := runner.runReviewStep(context.Background(), stepInput{
		Project:  *project,
		Loop:     storage.LoopRecord{ID: "loop_1"},
		Run:      storage.RunRecord{ID: "run_1"},
		Repo:     "acme/looper",
		PRNumber: 42,
		Checkpoint: reviewerCheckpoint{
			Detail:   &checkpointDetail{HeadRefName: "feature/review-me", BaseRefName: "main"},
			Snapshot: &checkpointSnapshot{HeadSHA: "abc123"},
			Worktree: &checkpointWorktree{Path: filepath.Join(t.TempDir(), "deleted-worktree"), Branch: "feature/review-me", PreparedAt: fixture.nowISO()},
		},
	})
	if err != nil {
		t.Fatalf("runReviewStep() error = %v", err)
	}
	if len(git.createCalls) != 1 || len(git.prepareCalls) != 1 {
		t.Fatalf("createCalls=%d prepareCalls=%d, want 1 each", len(git.createCalls), len(git.prepareCalls))
	}
	if len(agent.starts) != 1 {
		t.Fatalf("len(agent.starts) = %d, want 1", len(agent.starts))
	}
	if agent.starts[0].WorkingDirectory != git.worktreePath {
		t.Fatalf("agent working dir = %q, want %q", agent.starts[0].WorkingDirectory, git.worktreePath)
	}
	if checkpoint.Worktree == nil || checkpoint.Worktree.Path != git.worktreePath {
		t.Fatalf("checkpoint worktree = %#v, want recreated worktree path", checkpoint.Worktree)
	}
}

func TestRunReviewStepUsesStableIdempotencyKeyAcrossRuns(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	agent := &fakeAgentExecutor{results: []AgentResult{
		{Status: "completed", Summary: "Looks good", Stdout: `__LOOPER_RESULT__={"summary":"posted review"}`},
		{Status: "completed", Summary: "Looks good", Stdout: `__LOOPER_RESULT__={"summary":"posted review"}`},
	}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: &fakeGitHubGateway{}, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now})

	project, err := fixture.repos.Projects.GetByID(context.Background(), "project_1")
	if err != nil || project == nil {
		t.Fatalf("Projects.GetByID() = (%#v, %v), want project", project, err)
	}
	worktree := &checkpointWorktree{Path: t.TempDir(), Branch: "pr-42-head", PreparedAt: fixture.nowISO()}
	input := stepInput{
		Project:  *project,
		Loop:     storage.LoopRecord{ID: "loop_stable_review"},
		Repo:     "acme/looper",
		PRNumber: 42,
		Checkpoint: reviewerCheckpoint{
			Detail:   &checkpointDetail{HeadRefName: "feature/review-me", BaseRefName: "main"},
			Snapshot: &checkpointSnapshot{HeadSHA: "abc123"},
			Worktree: worktree,
		},
	}

	input.Run = storage.RunRecord{ID: "run_1"}
	if _, err := runner.runReviewStep(context.Background(), input); err != nil {
		t.Fatalf("runReviewStep(run_1) error = %v", err)
	}
	input.Run = storage.RunRecord{ID: "run_2"}
	if _, err := runner.runReviewStep(context.Background(), input); err != nil {
		t.Fatalf("runReviewStep(run_2) error = %v", err)
	}
	if len(agent.starts) != 2 {
		t.Fatalf("len(agent.starts) = %d, want 2", len(agent.starts))
	}
	want := "reviewer:loop_stable_review:abc123"
	for i, start := range agent.starts {
		if start.IdempotencyKey != want {
			t.Fatalf("start %d idempotency key = %q, want %q", i, start.IdempotencyKey, want)
		}
	}
}

func TestRunReviewStepPersistsRepreparedWorktreeBeforeAgentStart(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	git := &fakeGitGateway{worktreePath: filepath.Join(t.TempDir(), "reviewer-worktree")}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: &fakeGitHubGateway{}, Git: git, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})

	project, err := fixture.repos.Projects.GetByID(context.Background(), "project_1")
	if err != nil || project == nil {
		t.Fatalf("Projects.GetByID() = (%#v, %v), want project", project, err)
	}
	prNumber := int64(42)
	loopTarget := "pr:42"
	loop := storage.LoopRecord{ID: "loop_1", Seq: 1, ProjectID: project.ID, Type: "reviewer", TargetType: "pull_request", TargetID: &loopTarget, Repo: stringPtr("acme/looper"), PRNumber: &prNumber, Status: "running", CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}
	if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	initialCheckpoint := reviewerCheckpoint{
		Detail:   &checkpointDetail{HeadRefName: "feature/review-me", BaseRefName: "main"},
		Snapshot: &checkpointSnapshot{HeadSHA: "abc123"},
		Worktree: &checkpointWorktree{Path: filepath.Join(t.TempDir(), "deleted-worktree"), Branch: "feature/review-me", PreparedAt: fixture.nowISO()},
	}
	checkpointJSON := mustMarshalJSON(initialCheckpoint)
	run := storage.RunRecord{ID: "run_1", LoopID: loop.ID, Status: "running", CurrentStep: stringPtr(string(stepReview)), CheckpointJSON: &checkpointJSON, StartedAt: fixture.nowISO(), CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}
	if err := fixture.repos.Runs.Upsert(context.Background(), run); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}

	checkpoint, err := runner.runReviewStep(context.Background(), stepInput{
		Project:    *project,
		Loop:       loop,
		Run:        run,
		Repo:       "acme/looper",
		PRNumber:   prNumber,
		Checkpoint: initialCheckpoint,
	})
	if err == nil || !contains(err.Error(), "no queued agent result") {
		t.Fatalf("runReviewStep() error = %v, want no queued agent result", err)
	}
	if checkpoint.Worktree == nil || checkpoint.Worktree.Path != git.worktreePath {
		t.Fatalf("checkpoint worktree = %#v, want recreated worktree path", checkpoint.Worktree)
	}
	persistedRun, err := fixture.repos.Runs.GetByID(context.Background(), run.ID)
	if err != nil || persistedRun == nil {
		t.Fatalf("Runs.GetByID() = (%#v, %v), want run", persistedRun, err)
	}
	persistedCheckpoint := parseCheckpoint(persistedRun.CheckpointJSON)
	if persistedCheckpoint.Worktree == nil || persistedCheckpoint.Worktree.Path != git.worktreePath {
		t.Fatalf("persisted checkpoint worktree = %#v, want recreated worktree path", persistedCheckpoint.Worktree)
	}
}

func TestProcessClaimedItemRetryAfterReviewFailureRepreparesWorktree(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	git := &fakeGitGateway{worktreePath: filepath.Join(t.TempDir(), "reviewer-worktree")}
	github := &fakeGitHubGateway{reviewMarkerMissing: true}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "failed", Summary: "agent failed"}, {Status: "completed", Summary: "Looks good", Stdout: `__LOOPER_RESULT__={"summary":"posted review"}`}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now})

	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	firstClaim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || firstClaim == nil {
		t.Fatalf("first ClaimNextOfType() = (%#v, %v), want claimed item", firstClaim, err)
	}
	firstResult, err := runner.ProcessClaimedItem(context.Background(), *firstClaim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem(first) error = %v", err)
	}
	if firstResult.Status != "failed" || firstResult.FailureKind != FailureRetryableTransient {
		t.Fatalf("first result = %#v, want retryable_transient failure", firstResult)
	}

	fixture.advance(5 * time.Second)
	github.reviewMarkerMissing = false
	retryClaim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || retryClaim == nil {
		t.Fatalf("retry ClaimNextOfType() = (%#v, %v), want claimed item", retryClaim, err)
	}
	retryResult, err := runner.ProcessClaimedItem(context.Background(), *retryClaim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem(retry) error = %v", err)
	}
	if retryResult.Status != "success" {
		t.Fatalf("retry result = %#v, want success", retryResult)
	}
	if len(git.createCalls) != 2 || len(git.prepareCalls) != 2 {
		t.Fatalf("createCalls=%d prepareCalls=%d, want 2 each", len(git.createCalls), len(git.prepareCalls))
	}
	if len(agent.starts) != 2 {
		t.Fatalf("len(agent.starts) = %d, want 2", len(agent.starts))
	}
}

func TestProcessClaimedItemPersistsFailedLoopWhenMaxConsecutiveFailuresReached(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{reviewMarkerMissing: true}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "failed", Summary: "agent failed"}}}
	runner := New(Options{
		DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now,
		RetryMaxAttempts: 3,
		LoopConfig:       config.ReviewerLoopConfig{EnabledByDefault: true, QuietPeriodSeconds: 120, MaxIterationsPerPR: 20, MaxIterationsPerHead: 1, MaxWallClockSeconds: 14400, MaxConsecutiveFailures: 1, MaxAgentExecutionsPerPR: 25},
	})

	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNextOfType() = (%#v, %v), want claimed item", claim, err)
	}

	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "failed" || result.FailureKind != FailureRetryableTransient {
		t.Fatalf("result = %#v, want retryable failed result", result)
	}
	loop, err := fixture.repos.Loops.GetByID(context.Background(), *claim.LoopID)
	if err != nil || loop == nil {
		t.Fatalf("Loops.GetByID() = (%#v, %v), want loop", loop, err)
	}
	if loop.Status != "failed" || loop.NextRunAt != nil {
		t.Fatalf("loop = %#v, want failed loop with no next run", loop)
	}
	loopMeta := reviewerLoopMetadata(parseJSONObject(loop.MetadataJSON))
	if loopMeta["status"] != "failed" || loopMeta["terminationReason"] != "max_consecutive_failures" {
		t.Fatalf("loop metadata = %#v, want max consecutive failure terminal metadata", loopMeta)
	}
	queue, err := fixture.repos.Queue.GetByID(context.Background(), claim.ID)
	if err != nil || queue == nil {
		t.Fatalf("Queue.GetByID() = (%#v, %v), want queue", queue, err)
	}
	if queue.Status != "failed" || queue.FinishedAt == nil {
		t.Fatalf("queue = %#v, want failed terminal queue item", queue)
	}
}

func TestRunPrepareWorktreeStepPersistsCreatedWorktreeBeforeManualIntervention(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	clean := false
	git := &fakeGitGateway{worktreePath: filepath.Join(t.TempDir(), "reviewer-worktree"), prepareClean: &clean}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: &fakeGitHubGateway{}, Git: git, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})

	project, err := fixture.repos.Projects.GetByID(context.Background(), "project_1")
	if err != nil || project == nil {
		t.Fatalf("Projects.GetByID() = (%#v, %v), want project", project, err)
	}
	prNumber := int64(42)
	loopTarget := "pr:42"
	loop := storage.LoopRecord{ID: "loop_1", Seq: 1, ProjectID: project.ID, Type: "reviewer", TargetType: "pull_request", TargetID: &loopTarget, Repo: stringPtr("acme/looper"), PRNumber: &prNumber, Status: "running", CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}
	if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	run := storage.RunRecord{ID: "run_1", LoopID: "loop_1", Status: "running", CurrentStep: stringPtr(string(stepWorktree)), CheckpointJSON: stringPtr(mustMarshalJSON(reviewerCheckpoint{})), StartedAt: fixture.nowISO(), CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}
	if err := fixture.repos.Runs.Upsert(context.Background(), run); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}

	_, err = runner.runPrepareWorktreeStep(context.Background(), stepInput{
		Project:  *project,
		Run:      run,
		Repo:     "acme/looper",
		PRNumber: 42,
		Checkpoint: reviewerCheckpoint{
			Detail:   &checkpointDetail{HeadSHA: "abc123", BaseRefName: "main"},
			Snapshot: &checkpointSnapshot{HeadSHA: "abc123"},
		},
	})
	if err == nil || !contains(err.Error(), "manual intervention required") {
		t.Fatalf("runPrepareWorktreeStep() error = %v, want manual intervention required", err)
	}
	persistedRun, err := fixture.repos.Runs.GetByID(context.Background(), run.ID)
	if err != nil || persistedRun == nil {
		t.Fatalf("Runs.GetByID() = (%#v, %v), want run", persistedRun, err)
	}
	persistedCheckpoint := parseCheckpoint(persistedRun.CheckpointJSON)
	if persistedCheckpoint.Worktree == nil || persistedCheckpoint.Worktree.Path != git.worktreePath {
		t.Fatalf("persisted checkpoint worktree = %#v, want created worktree", persistedCheckpoint.Worktree)
	}
	if persistedCheckpoint.Worktree.PreparedAt != "" {
		t.Fatalf("persisted checkpoint preparedAt = %q, want empty before failed prepare", persistedCheckpoint.Worktree.PreparedAt)
	}
}

func TestProcessNextFinalizesClaimedQueueItemOnSetupFailure(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	if _, err := fixture.coordinator.DB().ExecContext(context.Background(), `
		CREATE TRIGGER reviewer_runs_fail_start
		BEFORE INSERT ON runs
		WHEN NEW.status = 'running'
		BEGIN
			SELECT RAISE(FAIL, 'start run blocked');
		END;
	`); err != nil {
		t.Fatalf("create trigger error = %v", err)
	}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: &fakeGitHubGateway{}, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})
	discovery, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}

	result, err := runner.ProcessNext(context.Background(), "reviewer-worker-1")
	if err == nil || !contains(err.Error(), "start run blocked") {
		t.Fatalf("ProcessNext() error = %v, want start run blocked", err)
	}
	if result != nil {
		t.Fatalf("ProcessNext() = %#v, want nil result", result)
	}
	queue, getErr := fixture.repos.Queue.GetByID(context.Background(), discovery.QueueItems[0].ID)
	if getErr != nil {
		t.Fatalf("Queue.GetByID() error = %v", getErr)
	}
	if queue == nil || queue.Status != "failed" || queue.FinishedAt == nil || queue.LastErrorKind == nil || *queue.LastErrorKind != string(FailureNonRetryable) {
		t.Fatalf("queue = %#v, want failed queue item with non_retryable error kind", queue)
	}
	if queue.LastError == nil || !contains(*queue.LastError, "start run blocked") {
		t.Fatalf("queue.LastError = %#v, want start run blocked", queue.LastError)
	}
}

func TestProcessClaimedItemReturnsWhenCompleteRunFails(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "Please add tests", Stdout: `__LOOPER_RESULT__={"summary":"posted review"}`}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, AllowAutoApprove: true})

	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNext() = (%#v, %v), want claimed queue item", claim, err)
	}
	if _, err := fixture.coordinator.DB().ExecContext(context.Background(), `
		CREATE TRIGGER reviewer_runs_fail_complete_insert
		BEFORE INSERT ON runs
		WHEN NEW.status != 'running'
		BEGIN
			SELECT RAISE(FAIL, 'complete run blocked');
		END;
	`); err != nil {
		t.Fatalf("create insert trigger error = %v", err)
	}
	if _, err := fixture.coordinator.DB().ExecContext(context.Background(), `
		CREATE TRIGGER reviewer_runs_fail_complete_update
		BEFORE UPDATE ON runs
		WHEN NEW.status != 'running'
		BEGIN
			SELECT RAISE(FAIL, 'complete run blocked');
		END;
	`); err != nil {
		t.Fatalf("create update trigger error = %v", err)
	}

	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err == nil || !contains(err.Error(), "complete run blocked") {
		t.Fatalf("ProcessClaimedItem() error = %v, want complete run blocked", err)
	}
	if result != (ProcessResult{}) {
		t.Fatalf("ProcessClaimedItem() = %#v, want zero result on completeRun failure", result)
	}
	queue, getErr := fixture.repos.Queue.GetByID(context.Background(), claim.ID)
	if getErr != nil {
		t.Fatalf("Queue.GetByID() error = %v", getErr)
	}
	if queue == nil || queue.Status != "running" || queue.FinishedAt != nil {
		t.Fatalf("queue = %#v, want still-running claimed item", queue)
	}
	loop, getErr := fixture.repos.Loops.GetByID(context.Background(), *claim.LoopID)
	if getErr != nil {
		t.Fatalf("Loops.GetByID() error = %v", getErr)
	}
	if loop == nil || loop.Status != "running" {
		t.Fatalf("loop = %#v, want still-running loop", loop)
	}
	runs, getErr := fixture.repos.Runs.ListByLoop(context.Background(), *claim.LoopID)
	if getErr != nil {
		t.Fatalf("Runs.ListByLoop() error = %v", getErr)
	}
	if len(runs) != 1 || runs[0].Status != "running" {
		t.Fatalf("runs = %#v, want single running run", runs)
	}
}

func TestProcessClaimedItemPreservesPausedLoopOnRetryableFailureAfterPause(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "reviewed", Stdout: `__LOOPER_RESULT__={"summary":"posted review"}`}}, wait: func(ctx context.Context) error {
		items, err := fixture.repos.Queue.List(ctx)
		if err != nil {
			return err
		}
		loopID := ""
		for _, item := range items {
			if item.Type == "reviewer" && item.Status == "running" && item.LoopID != nil {
				loopID = *item.LoopID
				break
			}
		}
		if loopID == "" {
			return fmt.Errorf("running reviewer queue item not found")
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
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: &fakeGitHubGateway{}, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now})

	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNext() = (%#v, %v), want claimed queue item", claim, err)
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

func TestBuildReviewPromptIncludesActionableQualityContract(t *testing.T) {
	t.Parallel()

	prompt := buildReviewPrompt("acme/looper", 42, reviewerCheckpoint{Detail: &checkpointDetail{Labels: []string{specpr.ReviewingLabel}}, Snapshot: &checkpointSnapshot{Title: "Spec PR", HeadSHA: "abc123"}}, "run_1", "reviewer:loop:abc123", config.ReviewerReviewEventsConfig{Clean: config.ReviewerReviewEventApprove, Blocking: config.ReviewerReviewEventComment}, false, config.ReviewerScopeChangedRanges, config.DefaultDisclosureConfig(), "opencode", "", "/opt/looper/bin/looper")
	for _, want := range []string{
		"Every comment MUST include",
		"Bad comment example",
		"Good spec/docs comment example",
		"Spec/docs review rubric",
		"suggestedChange",
		"do not write or publish a bare LGTM review body",
		"Group related findings by file, subsystem, function, or rule",
		"fixture-matrix tests",
		"'/opt/looper/bin/looper' review submit acme/looper#42 --event COMMENT --commit-id abc123 --clean-review-event APPROVE --blocking-review-event COMMENT`",
		"wrapper validates inline anchors against the live PR diff before it calls GitHub",
		"Review pass contract",
		"Do not stop after the first issue",
		"include it in this review rather than deferring it to a later pass",
		"Finding accumulator contract",
		"group repeated patterns into systemic comments with representative examples",
		"Severity rubric",
		"mark a finding as BLOCKING only when",
		"Mark actionable but merge-safe improvements as NON_BLOCKING",
		"NITs must not block merge",
		"Finalization gate before submit",
		"review outcome matches the highest published severity",
		"do not use PATH-based `looper`",
		"repository-local `go run ./cmd/looper`",
		"`gh api repos/acme/looper/pulls/42/reviews`, or `gh pr review` directly",
		"gh api repos/acme/looper/pulls/42/reviews",
		"looper:review id=reviewer:loop:abc123 head=abc123 outcome=clean|non_blocking|blocking",
		"before posting anything",
		"existing PR reviews",
		"Idempotency outcome matching is strict",
		"runner will reconcile the clean-signal +1 reaction and any eligible spec label transition",
		"review request removed before publish",
		"PR head changed before publish",
		"looper:spec-reviewing",
		"Do not add or remove the PR main-conversation +1 reaction yourself",
		"Review body style contract",
		"Never post terminal/tool output",
		"ANSI escape sequences",
		"file-read traces",
		"submit exactly one APPROVE review through the trusted Looper CLI wrapper with `outcome=clean`, no inline `comments`, and no extra PR conversation comment",
		"'/opt/looper/bin/looper' review submit acme/looper#42 --event APPROVE --commit-id abc123 --clean-review-event APPROVE --blocking-review-event COMMENT`",
		"never use an LGTM, empty, or disclosure-only clean body as a fallback",
		"visible body must start with `@<PR-author-login>`",
		"briefly summarize what changed or what you verified",
		"warm, friendly, encouraging acknowledgement of the author's work",
		"wrapper rejects clean APPROVE reviews that do not start with an @mention",
		"<!-- looper:stamp v=1 -->",
		"<sub>Generated by Looper 0.0.0-dev · runner=reviewer · agent=opencode</sub>",
		"Every inline review comment you post must include only the hidden looper stamp marker",
		"Do not write the footer as plain paragraph text",
		"Minimal PR seed",
		"\"repo\": \"acme/looper\"",
		"\"pr_number\": 42",
		"\"head_sha\": \"abc123\"",
		"Agent-side GitHub fetch contract",
		"gh pr view <pr-url> -R <repo> --json number,title,body,state,isDraft,baseRefName,headRefName,headRefOid,url,labels",
		"gh pr diff <pr-url> -R <repo> --name-only",
		"gh pr diff <pr-url> -R <repo> --patch",
		"gh pr checks <pr-url> -R <repo>",
		"git diff <base>...<head> -- <path>",
		"gh api repos/{owner}/{repo}/pulls/{number}/comments --paginate",
		"gh api repos/{owner}/{repo}/pulls/{number}/reviews --paginate",
		"gh api repos/{owner}/{repo}/issues/{number}/comments --paginate",
		"structured error with `type` set to one of `auth`, `network`, `rate_limit`, or `pr_drift`",
		"validate every inline review comment's `path`, `line`, `side`, `start_line`, and `start_side` against the live PR diff fetched with `gh pr diff`",
		"Preserve exact anchors that fit the live diff",
		"safely downgrade it to top-level review body feedback",
		"follow-up quality-gating failure",
		"Resolvable inline review comments are required",
		"PR review `comments` array",
		"not as a separate issue/PR conversation comment",
		"create resolvable GitHub review threads",
		"`path`, `line`, `side`",
		"`start_line` and `start_side`",
		"the detailed findings must live in inline `comments`",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "<!-- looper:review id=reviewer:loop:abc123 head=abc123 run=") {
		t.Fatalf("prompt includes run-scoped idempotency marker:\n%s", prompt)
	}
	if strings.Contains(prompt, "PR conversation comments") {
		t.Fatalf("prompt idempotency source diverges from review marker verification:\n%s", prompt)
	}
	if strings.Contains(prompt, "moving the same actionable feedback into the review body") {
		t.Fatalf("prompt allows weakening resolvable inline comment contract:\n%s", prompt)
	}
	for _, forbidden := range []string{
		"Submit clean reviews as COMMENT",
		"write a concise clean LGTM body",
		"after the APPROVE review is posted",
		"ensure +1 reaction",
		"remove any existing +1 reaction",
	} {
		if strings.Contains(prompt, forbidden) {
			t.Fatalf("prompt contains conflicting no-actionable clean-review instruction %q:\n%s", forbidden, prompt)
		}
	}
}

func TestBuildReviewerMinimalPRSeedUsesEnterpriseHost(t *testing.T) {
	t.Parallel()

	seed := buildReviewerMinimalPRSeed("ghe.example.com/acme/looper", 42, reviewerCheckpoint{Snapshot: &checkpointSnapshot{HeadSHA: "abc123"}}, config.ReviewerScopeFullPR)
	if !strings.Contains(seed, "\"url\": \"https://ghe.example.com/acme/looper/pull/42\"") {
		t.Fatalf("seed = %q, want enterprise host PR URL", seed)
	}
}

func TestBuildReviewPromptKeepsCommentCleanPolicyWithoutApproveInstruction(t *testing.T) {
	t.Parallel()

	prompt := buildReviewPrompt("acme/looper", 42, reviewerCheckpoint{Snapshot: &checkpointSnapshot{HeadSHA: "abc123"}}, "run_1", "reviewer:loop:abc123", config.ReviewerReviewEventsConfig{Clean: config.ReviewerReviewEventComment, Blocking: config.ReviewerReviewEventComment}, false, config.ReviewerScopeChangedRanges, config.DefaultDisclosureConfig(), "opencode", "", "/opt/looper/bin/looper")

	if !strings.Contains(prompt, "do not submit a clean COMMENT or APPROVE review") {
		t.Fatalf("prompt missing reaction-only clean instruction:\n%s", prompt)
	}
	for _, forbidden := range []string{
		"submit exactly one APPROVE review through the trusted Looper CLI wrapper",
		"review submit acme/looper#42 --event APPROVE",
	} {
		if strings.Contains(prompt, forbidden) {
			t.Fatalf("prompt contains unexpected approve clean instruction %q:\n%s", forbidden, prompt)
		}
	}
}

func TestBuildReviewPromptRequiresHumanCleanApproveBodyMentioningAuthor(t *testing.T) {
	t.Parallel()

	prompt := buildReviewPrompt("acme/looper", 42, reviewerCheckpoint{Detail: &checkpointDetail{Author: "octocat"}, Snapshot: &checkpointSnapshot{HeadSHA: "abc123", Author: "octocat"}}, "run_1", "reviewer:loop:abc123", config.ReviewerReviewEventsConfig{Clean: config.ReviewerReviewEventApprove, Blocking: config.ReviewerReviewEventComment}, false, config.ReviewerScopeChangedRanges, config.DefaultDisclosureConfig(), "opencode", "", "/opt/looper/bin/looper")
	for _, want := range []string{
		"visible body must start with `@octocat`",
		"briefly summarize what changed or what you verified",
		"warm, friendly, encouraging acknowledgement of the author's work",
		"Do not use a bare LGTM or marker/disclosure-only body",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestBuildReviewPromptOmitsSubmitPathInstructionWhenTrustedWrapperUnavailable(t *testing.T) {
	t.Parallel()

	prompt := buildReviewPrompt("acme/looper", 42, reviewerCheckpoint{Snapshot: &checkpointSnapshot{Title: "Spec PR", HeadSHA: "abc123"}}, "run_1", "reviewer:loop:abc123", config.ReviewerReviewEventsConfig{Clean: config.ReviewerReviewEventApprove, Blocking: config.ReviewerReviewEventComment}, false, config.ReviewerScopeChangedRanges, config.DefaultDisclosureConfig(), "opencode", "", "")

	if !strings.Contains(prompt, "trusted looper review submit wrapper unavailable") {
		t.Fatalf("prompt missing trusted wrapper unavailable failure instruction:\n%s", prompt)
	}
	for _, want := range []string{
		"do not publish any GitHub review",
		"do not add or remove any GitHub reaction",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing wrapper-unavailable failure guard %q:\n%s", want, prompt)
		}
	}
	for _, forbidden := range []string{
		"When submitting through",
		"'' review submit",
		" review submit acme/looper#42",
		"You must publish the GitHub review yourself by calling looper's enforced review-submit wrapper",
		"finish successfully with the `No actionable findings` summary only",
		"finish successfully with a summary beginning `No actionable findings`",
	} {
		if strings.Contains(prompt, forbidden) {
			t.Fatalf("prompt contains unavailable submit-path instruction %q:\n%s", forbidden, prompt)
		}
	}
}

func TestBuildReviewPromptUsesMinimalSeedInsteadOfEmbeddedDiff(t *testing.T) {
	t.Parallel()

	diff := "diff --git a/docs/spec.md b/docs/spec.md\n@@ -4,3 +4,4 @@\n # Reviewer anchors\n existing\n+new requirement\n tail\n"
	payload, err := json.Marshal(map[string]string{"diff": diff})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	prompt := buildReviewPrompt("acme/looper", 42, reviewerCheckpoint{Snapshot: &checkpointSnapshot{Title: "Spec PR", HeadSHA: "abc123", PayloadJSON: string(payload)}}, "run_1", "reviewer:loop:abc123", config.ReviewerReviewEventsConfig{Clean: config.ReviewerReviewEventComment, Blocking: config.ReviewerReviewEventComment}, false, config.ReviewerScopeChangedRanges, config.DefaultDisclosureConfig(), "opencode", "", "/opt/looper/bin/looper")

	for _, want := range []string{
		"Minimal PR seed",
		"\"repo\": \"acme/looper\"",
		"\"pr_number\": 42",
		"\"head_sha\": \"abc123\"",
		"fetch all mutable PR details yourself",
		"gh pr diff <pr-url> -R <repo> --patch",
		"git diff <base>...<head> -- <path>",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
	for _, forbidden := range []string{
		"gh pr diff -- <path>",
		"ANCHORABLE DIFF LOCATIONS",
		"Diff:\n" + diff,
		"docs/spec.md RIGHT lines 4-7",
	} {
		if strings.Contains(prompt, forbidden) {
			t.Fatalf("prompt includes full snapshot-derived diff content %q:\n%s", forbidden, prompt)
		}
	}
}

func TestBuildReviewPromptRestrictsExistingMarkerSkipWhenApprovalsDisallowed(t *testing.T) {
	t.Parallel()

	prompt := buildReviewPrompt("acme/looper", 42, reviewerCheckpoint{Snapshot: &checkpointSnapshot{HeadSHA: "abc123"}}, "run_1", "reviewer:loop:abc123", config.ReviewerReviewEventsConfig{Clean: config.ReviewerReviewEventComment, Blocking: config.ReviewerReviewEventComment}, false, config.ReviewerScopeChangedRanges, config.DefaultDisclosureConfig(), "opencode", "", "/opt/looper/bin/looper")
	for _, want := range []string{
		"only treat an existing `outcome=clean` marker as satisfied when it is on a COMMENTED review if clean policy is COMMENT",
		"Treat `outcome=non_blocking` or legacy `outcome=actionable` markers as satisfied only when they are on a COMMENTED review",
		"'/opt/looper/bin/looper' review submit acme/looper#42 --event COMMENT --commit-id abc123 --clean-review-event COMMENT --blocking-review-event COMMENT",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "--event COMMENT|APPROVE") {
		t.Fatalf("prompt advertises approvals while allowApprove=false:\n%s", prompt)
	}
}

func TestBuildReviewPromptUsesOutcomeSensitiveIdempotencyForApproveAndRequestChangesPolicies(t *testing.T) {
	t.Parallel()

	prompt := buildReviewPrompt("acme/looper", 42, reviewerCheckpoint{Snapshot: &checkpointSnapshot{HeadSHA: "abc123"}}, "run_1", "reviewer:loop:abc123", config.ReviewerReviewEventsConfig{Clean: config.ReviewerReviewEventApprove, Blocking: config.ReviewerReviewEventRequestChanges}, false, config.ReviewerScopeChangedRanges, config.DefaultDisclosureConfig(), "opencode", "", "/opt/looper/bin/looper")
	for _, want := range []string{
		"or on an APPROVED review if clean policy is APPROVE",
		"Only treat an existing `outcome=blocking` marker as satisfied when it is on a CHANGES_REQUESTED review if blocking policy is REQUEST_CHANGES",
		"Treat `outcome=non_blocking` or legacy `outcome=actionable` markers as satisfied only when they are on a COMMENTED review",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestBuildReviewPromptIncludesReviewerScopeInstruction(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		scope config.ReviewerScope
		want  string
	}{
		{name: "full", scope: config.ReviewerScopeFullPR, want: "Review scope: full_pr"},
		{name: "files", scope: config.ReviewerScopeChangedFiles, want: "Review scope: changed_files"},
		{name: "ranges", scope: config.ReviewerScopeChangedRanges, want: "Review scope: changed_ranges"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			prompt := buildReviewPrompt("acme/looper", 42, reviewerCheckpoint{Snapshot: &checkpointSnapshot{HeadSHA: "abc123"}}, "run_1", "reviewer:loop:abc123", config.ReviewerReviewEventsConfig{Clean: config.ReviewerReviewEventComment, Blocking: config.ReviewerReviewEventComment}, false, tc.scope, config.DefaultDisclosureConfig(), "opencode", "", "/opt/looper/bin/looper")
			if !strings.Contains(prompt, tc.want) {
				t.Fatalf("prompt missing %q:\n%s", tc.want, prompt)
			}
		})
	}
}

func TestBuildReviewPromptFullPRScopeUsesAgentSideFetchContract(t *testing.T) {
	t.Parallel()

	prompt := buildReviewPrompt("acme/looper", 42, reviewerCheckpoint{Snapshot: &checkpointSnapshot{HeadSHA: "abc123"}}, "run_1", "reviewer:loop:abc123", config.ReviewerReviewEventsConfig{Clean: config.ReviewerReviewEventComment, Blocking: config.ReviewerReviewEventComment}, false, config.ReviewerScopeFullPR, config.DefaultDisclosureConfig(), "opencode", "", "/opt/looper/bin/looper")
	for _, want := range []string{
		"Review scope: full_pr",
		"complete diff fetched through `gh` according to the agent-side GitHub fetch contract",
		"supported by the fetched context",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
	for _, forbidden := range []string{
		"complete diff payload below",
		"supported by the included context",
	} {
		if strings.Contains(prompt, forbidden) {
			t.Fatalf("prompt still contains stale embedded-context guidance %q:\n%s", forbidden, prompt)
		}
	}
}

func TestShouldRestartFromDiscoverForAgentNativePreflightFailures(t *testing.T) {
	t.Parallel()

	for _, summary := range []string{
		"PR head changed before publish",
		"review request removed before publish",
	} {
		if !shouldRestartFromDiscover("failed", stepReview, summary) {
			t.Fatalf("shouldRestartFromDiscover(review, %q) = false, want true", summary)
		}
	}
}

func TestShouldRestartFromDiscoverForThreadResolutionHeadChange(t *testing.T) {
	t.Parallel()

	if !shouldRestartFromDiscover("failed", stepThreadResolution, "PR changed during thread reconciliation") {
		t.Fatalf("shouldRestartFromDiscover(thread_resolution head change) = false, want true")
	}
}

func TestProcessClaimedItemRestartsFromDiscoverOnHeadChangeSignal(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{changeHeadOnSecondView: true}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "failed", Summary: "agent reported a generic shell failure"}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now})

	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNext() = (%#v, %v), want claimed queue item", claim, err)
	}
	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "failed" || result.FailureKind != FailureRetryableAfterResume || !contains(result.Summary, "PR head changed before publish") {
		t.Fatalf("result = %#v, want structured head-change retry", result)
	}
	latestRun, err := fixture.repos.Runs.GetLatestByLoopID(context.Background(), result.LoopID)
	if err != nil || latestRun == nil {
		t.Fatalf("GetLatestByLoopID() = (%#v, %v), want failed run", latestRun, err)
	}
	checkpoint := parseCheckpoint(latestRun.CheckpointJSON)
	if checkpoint.ResumePolicy != "restart_from_discover" {
		t.Fatalf("ResumePolicy = %q, want restart_from_discover", checkpoint.ResumePolicy)
	}
	loop, err := fixture.repos.Loops.GetByID(context.Background(), result.LoopID)
	if err != nil || loop == nil {
		t.Fatalf("Loops.GetByID() = (%#v, %v), want loop", loop, err)
	}
	resumed, err := runner.createRunContext(context.Background(), *loop)
	if err != nil {
		t.Fatalf("createRunContext() error = %v", err)
	}
	if resumed.StartStep != stepDiscover {
		t.Fatalf("StartStep = %q, want discover", resumed.StartStep)
	}
}

func TestProcessClaimedItemRestartsFromDiscoverOnReviewRequestSignal(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{removeReviewRequestOnSecondView: true, reviewMarkerMissing: true}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "failed", Summary: "agent reported a generic shell failure"}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now})

	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNext() = (%#v, %v), want claimed queue item", claim, err)
	}
	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "failed" || result.FailureKind != FailureRetryableAfterResume || !contains(result.Summary, "review request removed before publish") {
		t.Fatalf("result = %#v, want structured review-request retry", result)
	}
	latestRun, err := fixture.repos.Runs.GetLatestByLoopID(context.Background(), result.LoopID)
	if err != nil || latestRun == nil {
		t.Fatalf("GetLatestByLoopID() = (%#v, %v), want failed run", latestRun, err)
	}
	checkpoint := parseCheckpoint(latestRun.CheckpointJSON)
	if checkpoint.ResumePolicy != "restart_from_discover" {
		t.Fatalf("ResumePolicy = %q, want restart_from_discover", checkpoint.ResumePolicy)
	}
}

func TestProcessClaimedItemRestartsFromDiscoverOnUnparsedReviewRequestGuardrail(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{reviewMarkerMissing: true}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "review request removed before publish"}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now})

	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNext() = (%#v, %v), want claimed queue item", claim, err)
	}
	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "failed" || result.FailureKind != FailureRetryableAfterResume || !contains(result.Summary, "review request removed before publish") {
		t.Fatalf("result = %#v, want structured review-request retry", result)
	}
	latestRun, err := fixture.repos.Runs.GetLatestByLoopID(context.Background(), result.LoopID)
	if err != nil || latestRun == nil {
		t.Fatalf("GetLatestByLoopID() = (%#v, %v), want failed run", latestRun, err)
	}
	checkpoint := parseCheckpoint(latestRun.CheckpointJSON)
	if checkpoint.ResumePolicy != "restart_from_discover" {
		t.Fatalf("ResumePolicy = %q, want restart_from_discover", checkpoint.ResumePolicy)
	}
}

func TestRunReviewStepIgnoresUnparsedReviewRequestGuardrailWhenPolicyDisabled(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{reviewMarkerMissing: true}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "review request removed before publish"}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, DiscoveryPolicy: DiscoveryPolicy{AutoDiscovery: true, IncludeDrafts: false, RequireReviewRequest: false, Labels: []string{}, LabelMode: config.LabelModeAll}})

	project, err := fixture.repos.Projects.GetByID(context.Background(), "project_1")
	if err != nil || project == nil {
		t.Fatalf("Projects.GetByID() = (%#v, %v), want project", project, err)
	}
	checkpoint, err := runner.runReviewStep(context.Background(), stepInput{
		Project:  *project,
		Loop:     storage.LoopRecord{ID: "loop_policy_disabled", ProjectID: project.ID, Type: "reviewer"},
		Run:      storage.RunRecord{ID: "run_policy_disabled", LoopID: "loop_policy_disabled"},
		Repo:     "acme/looper",
		PRNumber: 42,
		Checkpoint: reviewerCheckpoint{
			Detail:   &checkpointDetail{HeadRefName: "feature/review-me", BaseRefName: "main"},
			Snapshot: &checkpointSnapshot{HeadSHA: "abc123"},
			Worktree: &checkpointWorktree{Path: t.TempDir(), Branch: "pr-42-head", PreparedAt: fixture.nowISO()},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "valid completion marker") {
		t.Fatalf("runReviewStep() error = %v, want marker failure", err)
	}
	if checkpoint.ResumePolicy == "restart_from_discover" {
		t.Fatalf("ResumePolicy = %q, want no rediscovery restart", checkpoint.ResumePolicy)
	}
}

func TestRunReviewStepIgnoresUnparsedReviewRequestGuardrailForManualLoop(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{reviewMarkerMissing: true}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "review request removed before publish"}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, DiscoveryPolicy: DiscoveryPolicy{AutoDiscovery: true, IncludeDrafts: false, RequireReviewRequest: true, Labels: []string{}, LabelMode: config.LabelModeAll}})

	project, err := fixture.repos.Projects.GetByID(context.Background(), "project_1")
	if err != nil || project == nil {
		t.Fatalf("Projects.GetByID() = (%#v, %v), want project", project, err)
	}
	metadata := `{"manual":true}`
	checkpoint, err := runner.runReviewStep(context.Background(), stepInput{
		Project:  *project,
		Loop:     storage.LoopRecord{ID: "loop_manual_guardrail", ProjectID: project.ID, Type: "reviewer", MetadataJSON: &metadata},
		Run:      storage.RunRecord{ID: "run_manual_guardrail", LoopID: "loop_manual_guardrail"},
		Repo:     "acme/looper",
		PRNumber: 42,
		Checkpoint: reviewerCheckpoint{
			Detail:   &checkpointDetail{HeadRefName: "feature/review-me", BaseRefName: "main"},
			Snapshot: &checkpointSnapshot{HeadSHA: "abc123"},
			Worktree: &checkpointWorktree{Path: t.TempDir(), Branch: "pr-42-head", PreparedAt: fixture.nowISO()},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "valid completion marker") {
		t.Fatalf("runReviewStep() error = %v, want marker failure", err)
	}
	if checkpoint.ResumePolicy == "restart_from_discover" {
		t.Fatalf("ResumePolicy = %q, want no rediscovery restart", checkpoint.ResumePolicy)
	}
}

func TestProcessClaimedItemRestartsFromDiscoverOnUnparsedHeadChangeGuardrail(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{reviewMarkerMissing: true}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Stdout: "PR head changed before publish: expected abc123, got new-head"}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now})

	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNext() = (%#v, %v), want claimed queue item", claim, err)
	}
	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "failed" || result.FailureKind != FailureRetryableAfterResume || !contains(result.Summary, "PR head changed before publish") {
		t.Fatalf("result = %#v, want structured head-change retry", result)
	}
	latestRun, err := fixture.repos.Runs.GetLatestByLoopID(context.Background(), result.LoopID)
	if err != nil || latestRun == nil {
		t.Fatalf("GetLatestByLoopID() = (%#v, %v), want failed run", latestRun, err)
	}
	checkpoint := parseCheckpoint(latestRun.CheckpointJSON)
	if checkpoint.ResumePolicy != "restart_from_discover" {
		t.Fatalf("ResumePolicy = %q, want restart_from_discover", checkpoint.ResumePolicy)
	}
}

func TestRunReviewStepIgnoresPromptEchoedRediscoveryGuardrail(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{reviewMarkerMissing: true}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "Reviewer prompt said to report `PR head changed before publish` before retrying."}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now})

	project, err := fixture.repos.Projects.GetByID(context.Background(), "project_1")
	if err != nil || project == nil {
		t.Fatalf("Projects.GetByID() = (%#v, %v), want project", project, err)
	}
	checkpoint, err := runner.runReviewStep(context.Background(), stepInput{
		Project:  *project,
		Loop:     storage.LoopRecord{ID: "loop_prompt_echo", ProjectID: project.ID, Type: "reviewer"},
		Run:      storage.RunRecord{ID: "run_prompt_echo", LoopID: "loop_prompt_echo"},
		Repo:     "acme/looper",
		PRNumber: 42,
		Checkpoint: reviewerCheckpoint{
			Detail:   &checkpointDetail{HeadRefName: "feature/review-me", BaseRefName: "main"},
			Snapshot: &checkpointSnapshot{HeadSHA: "abc123"},
			Worktree: &checkpointWorktree{Path: t.TempDir(), Branch: "pr-42-head", PreparedAt: fixture.nowISO()},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "valid completion marker") {
		t.Fatalf("runReviewStep() error = %v, want marker failure", err)
	}
	if checkpoint.ResumePolicy == "restart_from_discover" {
		t.Fatalf("ResumePolicy = %q, want no rediscovery restart", checkpoint.ResumePolicy)
	}
}

func TestBuildReviewPromptUsesConfiguredDisclosure(t *testing.T) {
	t.Parallel()

	cfg := config.DefaultDisclosureConfig()
	model := "openai/gpt-5.5"
	prompt := buildReviewPrompt("acme/looper", 42, reviewerCheckpoint{Snapshot: &checkpointSnapshot{HeadSHA: "abc123"}}, "run_1", "reviewer:loop:abc123", config.ReviewerReviewEventsConfig{Clean: config.ReviewerReviewEventComment, Blocking: config.ReviewerReviewEventComment}, false, config.ReviewerScopeChangedRanges, cfg, "claude-code", model, "/opt/looper/bin/looper")
	if !strings.Contains(prompt, "agent=claude-code · model=openai/gpt-5.5") {
		t.Fatalf("prompt missing configured agent/model disclosure:\n%s", prompt)
	}
	if strings.Contains(prompt, "agent=opencode") {
		t.Fatalf("prompt retained hardcoded opencode disclosure:\n%s", prompt)
	}

	cfg.Enabled = false
	disabledPrompt := buildReviewPrompt("acme/looper", 42, reviewerCheckpoint{Snapshot: &checkpointSnapshot{HeadSHA: "abc123"}}, "run_1", "reviewer:loop:abc123", config.ReviewerReviewEventsConfig{Clean: config.ReviewerReviewEventComment, Blocking: config.ReviewerReviewEventComment}, false, config.ReviewerScopeChangedRanges, cfg, "claude-code", model, "/opt/looper/bin/looper")
	if !strings.Contains(disabledPrompt, "disclosure stamping is disabled") {
		t.Fatalf("prompt missing disabled disclosure instruction:\n%s", disabledPrompt)
	}
	if strings.Contains(disabledPrompt, "Generated by looper") {
		t.Fatalf("prompt included disclosure footer while disabled:\n%s", disabledPrompt)
	}
}

func TestBuildReviewPromptDoesNotTransitionSpecLabelsWithoutApprove(t *testing.T) {
	t.Parallel()

	prompt := buildReviewPrompt("acme/looper", 42, reviewerCheckpoint{Detail: &checkpointDetail{Labels: []string{specpr.ReviewingLabel}}, Snapshot: &checkpointSnapshot{Title: "Spec PR", HeadSHA: "abc123"}}, "run_1", "reviewer:loop:abc123", config.ReviewerReviewEventsConfig{Clean: config.ReviewerReviewEventComment, Blocking: config.ReviewerReviewEventComment}, false, config.ReviewerScopeChangedRanges, config.DefaultDisclosureConfig(), "opencode", "", "/opt/looper/bin/looper")
	if !strings.Contains(prompt, "Do not transition spec-review labels") {
		t.Fatalf("prompt missing no-transition instruction:\n%s", prompt)
	}
	if strings.Contains(prompt, "add `looper:spec-ready`") {
		t.Fatalf("prompt allows spec-ready transition when approve is disabled:\n%s", prompt)
	}
}

func TestBuildReviewPromptOmitsReviewRequestGuardrailWhenDisabled(t *testing.T) {
	t.Parallel()

	prompt, _ := buildReviewPromptWithInstructions("", config.Config{}, "acme/looper", 42, reviewerCheckpoint{Snapshot: &checkpointSnapshot{HeadSHA: "abc123"}}, "run_1", "reviewer:loop:abc123", config.ReviewerReviewEventsConfig{Clean: config.ReviewerReviewEventComment, Blocking: config.ReviewerReviewEventComment}, false, false, config.ReviewerScopeChangedRanges, config.DefaultDisclosureConfig(), "opencode", "", "/opt/looper/bin/looper")

	if strings.Contains(prompt, "review request removed before publish") {
		t.Fatalf("prompt retained review-request guardrail while disabled:\n%s", prompt)
	}
	if !strings.Contains(prompt, "does not require a current-user review request") {
		t.Fatalf("prompt missing disabled review-request instruction:\n%s", prompt)
	}
}

func TestRunThreadResolutionStepCommentsAndResolvesObjectiveLooperThread(t *testing.T) {
	t.Parallel()
	policy := defaultThreadResolutionPolicy(t)
	policy.Enabled = true
	policy.Mode = config.ReviewerThreadResolutionModeResolveObjective
	github := &fakeGitHubGateway{currentLogin: "looper-bot", reviewRequests: []string{"looper-bot"}, reviewThreads: []ReviewThread{{ID: "thread_1", Comments: []ReviewThreadComment{{ID: "comment_1", Author: "looper-bot", Body: "Please update this. <!-- looper:stamp v=1 -->", CommitOID: "old-head"}}}}}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Stdout: `{"decisions":[{"threadId":"thread_1","decision":"OBJECTIVELY_FIXED","evidence":"the nil check is now present","confidence":"HIGH"}]}`}}}
	runner := New(Options{GitHub: github, AgentExecutor: agent, ThreadResolution: policy, Now: func() time.Time { return time.Unix(0, 0).UTC() }})

	checkpoint, err := runner.runThreadResolutionStep(context.Background(), threadResolutionStepInput())
	if err != nil {
		t.Fatalf("runThreadResolutionStep() error = %v", err)
	}
	if checkpoint.ThreadResolution == nil || checkpoint.ThreadResolution.Commented != 1 || checkpoint.ThreadResolution.Resolved != 1 {
		t.Fatalf("ThreadResolution = %#v, want one comment and one resolution", checkpoint.ThreadResolution)
	}
	if len(github.addThreadReplyCalls) != 1 || !strings.Contains(github.addThreadReplyCalls[0].Body, "decision=objectively_fixed") {
		t.Fatalf("addThreadReplyCalls = %#v, want objective audit reply", github.addThreadReplyCalls)
	}
	if len(github.resolveThreadCalls) != 1 || github.resolveThreadCalls[0].ThreadID != "thread_1" {
		t.Fatalf("resolveThreadCalls = %#v, want thread_1 resolved", github.resolveThreadCalls)
	}
	if len(agent.starts) != 1 || !strings.Contains(agent.starts[0].IdempotencyKey, "thread_1") {
		t.Fatalf("agent starts = %#v, want thread id in idempotency key", agent.starts)
	}
}

func TestRunThreadResolutionStepRequiresLooperAuthoredMarker(t *testing.T) {
	t.Parallel()
	policy := defaultThreadResolutionPolicy(t)
	policy.Enabled = true
	policy.Mode = config.ReviewerThreadResolutionModeResolveObjective
	github := &fakeGitHubGateway{currentLogin: "looper-bot", reviewRequests: []string{"looper-bot"}, reviewThreads: []ReviewThread{{ID: "thread_1", Comments: []ReviewThreadComment{{ID: "comment_1", Author: "looper-bot", Body: "Manual review comment without Looper marker", CommitOID: "old-head"}}}}}
	agent := &fakeAgentExecutor{}
	runner := New(Options{GitHub: github, AgentExecutor: agent, ThreadResolution: policy, Now: func() time.Time { return time.Unix(0, 0).UTC() }})

	checkpoint, err := runner.runThreadResolutionStep(context.Background(), threadResolutionStepInput())
	if err != nil {
		t.Fatalf("runThreadResolutionStep() error = %v", err)
	}
	if checkpoint.ThreadResolution == nil || checkpoint.ThreadResolution.Reported != 0 || checkpoint.ThreadResolution.Processed != 0 {
		t.Fatalf("ThreadResolution = %#v, want no eligible candidates", checkpoint.ThreadResolution)
	}
	if len(agent.starts) != 0 || len(github.addThreadReplyCalls) != 0 || len(github.resolveThreadCalls) != 0 {
		t.Fatalf("side effects: agent=%d replies=%d resolves=%d, want none", len(agent.starts), len(github.addThreadReplyCalls), len(github.resolveThreadCalls))
	}
}

func TestRunThreadResolutionStepRestartsFromDiscoverOnHeadChange(t *testing.T) {
	t.Parallel()
	policy := defaultThreadResolutionPolicy(t)
	policy.Enabled = true
	policy.Mode = config.ReviewerThreadResolutionModeResolveObjective
	github := &fakeGitHubGateway{currentLogin: "looper-bot", reviewRequests: []string{"looper-bot"}, viewHeadSHA: "new-head", reviewThreads: []ReviewThread{{ID: "thread_1", Comments: []ReviewThreadComment{{ID: "comment_1", Author: "looper-bot", Body: "Please update this. <!-- looper:stamp v=1 -->", CommitOID: "old-head"}}}}}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Stdout: `{"decisions":[{"threadId":"thread_1","decision":"objectively_fixed","evidence":"the nil check is now present","confidence":"high"}]}`}}}
	runner := New(Options{GitHub: github, AgentExecutor: agent, ThreadResolution: policy, Now: func() time.Time { return time.Unix(0, 0).UTC() }})

	checkpoint, err := runner.runThreadResolutionStep(context.Background(), threadResolutionStepInput())
	if err == nil || !strings.Contains(err.Error(), "PR changed during thread reconciliation") {
		t.Fatalf("runThreadResolutionStep() error = %v, want head-change reconciliation error", err)
	}
	if checkpoint.ResumePolicy != "restart_from_discover" {
		t.Fatalf("ResumePolicy = %q, want restart_from_discover", checkpoint.ResumePolicy)
	}
	if len(github.addThreadReplyCalls) != 0 || len(github.resolveThreadCalls) != 0 {
		t.Fatalf("side effects: replies=%d resolves=%d, want none", len(github.addThreadReplyCalls), len(github.resolveThreadCalls))
	}
}

func defaultThreadResolutionPolicy(t *testing.T) config.ReviewerThreadResolutionConfig {
	t.Helper()
	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	return cfg.Reviewer.ThreadResolution
}

func threadResolutionStepInput() stepInput {
	repo := "acme/looper"
	prNumber := int64(42)
	return stepInput{
		Project:  storage.ProjectRecord{ID: "project_1", RepoPath: "/tmp/repo"},
		Loop:     storage.LoopRecord{ID: "loop_1", ProjectID: "project_1", Type: "reviewer", Repo: &repo, PRNumber: &prNumber},
		Run:      storage.RunRecord{ID: "run_1", LoopID: "loop_1"},
		Repo:     repo,
		PRNumber: prNumber,
		Checkpoint: reviewerCheckpoint{
			Detail:   &checkpointDetail{State: "OPEN", HeadSHA: "abc123", ReviewRequests: []string{"looper-bot"}},
			Snapshot: &checkpointSnapshot{HeadSHA: "abc123"},
			Worktree: &checkpointWorktree{Path: "/tmp/repo", HeadSHA: "abc123"},
		},
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
	coordinator, err := storage.OpenSQLiteCoordinator(context.Background(), filepath.Join(t.TempDir(), "reviewer.sqlite"), storage.SQLiteCoordinatorOptions{BackupDir: t.TempDir()})
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

func testReviewerLoopConfig() config.ReviewerLoopConfig {
	return config.ReviewerLoopConfig{EnabledByDefault: true, QuietPeriodSeconds: 60, MinPublishIntervalSeconds: 300, MaxIterationsPerPR: 20, MaxIterationsPerHead: 1, MaxWallClockSeconds: 14400, MaxConsecutiveFailures: 3, MaxAgentExecutionsPerPR: 25, StopOnApproved: true, StopOnReadyLabel: true, StopOnIdenticalOutput: true}
}

func (f *runnerFixture) advance(delta time.Duration) { f.current = f.current.Add(delta) }

func (f *runnerFixture) nowISO() string {
	return fmt.Sprintf("%s.000Z", f.current.UTC().Format("2006-01-02T15:04:05"))
}

func boolPtr(value bool) *bool { return &value }

type failedReviewerRecoverySeed struct {
	ResumePolicy         string
	QueueErrorKind       string
	ErrorMessage         string
	ConsecutiveFailures  int
	AutoRecoveryAttempts int
	FollowUpdates        *bool
	LoopEnabled          *bool
}

func seedFailedReviewerRecoveryLoop(t *testing.T, fixture *runnerFixture, seed failedReviewerRecoverySeed) (string, string) {
	t.Helper()
	ctx := context.Background()
	nowISO := fixture.nowISO()
	repo := "acme/looper"
	prNumber := int64(42)
	targetID := "pr:acme/looper:42"
	loopID := "loop_recover_reviewer"
	queueID := "queue_recover_reviewer"
	consecutive := seed.ConsecutiveFailures
	if consecutive == 0 {
		consecutive = 1
	}
	loopEnabled := true
	if seed.LoopEnabled != nil {
		loopEnabled = *seed.LoopEnabled
	}
	metadataMap := map[string]any{"loop": map[string]any{"enabled": loopEnabled, "failureCount": consecutive, "consecutiveFailures": consecutive, "lastFailure": seed.ErrorMessage, "autoRecoveryAttempts": seed.AutoRecoveryAttempts}}
	if seed.FollowUpdates != nil {
		metadataMap["followUpdates"] = *seed.FollowUpdates
	}
	metadata := mustMarshalJSON(metadataMap)
	if err := fixture.repos.Loops.Upsert(ctx, storage.LoopRecord{ID: loopID, Seq: 165, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", TargetID: &targetID, Repo: &repo, PRNumber: &prNumber, Status: "failed", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	checkpoint := mustMarshalJSON(reviewerCheckpoint{ResumePolicy: seed.ResumePolicy, Detail: &checkpointDetail{State: "OPEN", HeadSHA: "abc123", ReviewRequests: []string{"octocat"}}})
	if err := fixture.repos.Runs.Upsert(ctx, storage.RunRecord{ID: "run_recover_reviewer", LoopID: loopID, Status: "failed", CurrentStep: stringPtr(string(stepPublish)), CheckpointJSON: &checkpoint, Summary: &seed.ErrorMessage, ErrorMessage: &seed.ErrorMessage, StartedAt: nowISO, EndedAt: &nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}
	if err := fixture.repos.Queue.Upsert(ctx, storage.QueueItemRecord{ID: queueID, ProjectID: stringPtr("project_1"), LoopID: &loopID, Type: "reviewer", TargetType: "pull_request", TargetID: targetID, Repo: &repo, PRNumber: &prNumber, DedupeKey: buildReviewerDedupeKey("project_1", loopID, repo, prNumber), Priority: storage.QueuePriorityReviewer, Status: "failed", AvailableAt: nowISO, Attempts: 3, MaxAttempts: 3, FinishedAt: &nowISO, LastError: &seed.ErrorMessage, LastErrorKind: &seed.QueueErrorKind, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}
	return loopID, queueID
}

type fakeGitHubGateway struct {
	changeHeadOnSecondView          bool
	listHeadSHA                     string
	removeReviewRequestOnSecondView bool
	viewCalls                       int
	labels                          []string
	reviewDecision                  string
	comments                        []map[string]any
	reviews                         []map[string]any
	hasConflicts                    bool
	useReviewStateAfterFirstView    bool
	reviewDecisionAfterFirstView    string
	commentsAfterFirstView          []map[string]any
	reviewRequests                  []string
	currentLogin                    string
	currentLoginErr                 error
	currentLoginCalls               int
	reviewMarkerMissing             bool
	reviewMarkerErr                 error
	reviewMarkerEvent               ReviewEvent
	reviewMarkerOutcome             string
	reviewMarkerBody                string
	reviewMarkerBodyExplicit        bool
	reviewMarkerInlineCommentBodies []string
	reviewMarkerCalls               int
	viewDraft                       bool
	viewState                       string
	addReactionErr                  error
	removeReactionErr               error
	addLabelErr                     error
	removeLabelErr                  error
	reviewThreads                   []ReviewThread
	viewHeadSHA                     string
	addThreadReplyCalls             []AddReviewThreadReplyInput
	resolveThreadCalls              []ResolveReviewThreadInput
	addReactionCalls                []PullRequestReactionInput
	removeReactionCalls             []PullRequestReactionInput
	addLabelCalls                   []PullRequestLabelsInput
	removeLabelCalls                []PullRequestLabelsInput
	listCalls                       []ListOpenPullRequestsInput
	listOpenByLabel                 map[string][]PullRequestSummary
}

func (g *fakeGitHubGateway) ListOpenPullRequests(_ context.Context, input ListOpenPullRequestsInput) ([]PullRequestSummary, error) {
	g.listCalls = append(g.listCalls, input)
	if g.listOpenByLabel != nil {
		return append([]PullRequestSummary(nil), g.listOpenByLabel[input.Label]...), nil
	}
	reviewRequests := g.effectiveReviewRequests()
	headSHA := g.listHeadSHA
	if headSHA == "" {
		headSHA = "abc123"
	}
	return []PullRequestSummary{{Number: 42, Title: "Review me", State: "OPEN", ReviewDecision: g.reviewDecision, Labels: append([]string(nil), g.labels...), HeadSHA: headSHA, BaseSHA: "base123", HasConflicts: g.hasConflicts, ReviewRequests: reviewRequests, Reviews: cloneCommentMaps(g.reviews)}, {Number: 99, Title: "Draft", State: "OPEN", IsDraft: true, HeadSHA: "draft123", BaseSHA: "base123", ReviewRequests: reviewRequests}}, nil
}

func (g *fakeGitHubGateway) GetCurrentUserLogin(context.Context, string) (string, error) {
	g.currentLoginCalls++
	if g.currentLoginErr != nil {
		return "", g.currentLoginErr
	}
	if strings.TrimSpace(g.currentLogin) != "" {
		return g.currentLogin, nil
	}
	return "octocat", nil
}

func (g *fakeGitHubGateway) ViewPullRequest(context.Context, ViewPullRequestInput) (PullRequestDetail, error) {
	g.viewCalls++
	headSHA := "abc123"
	if g.viewHeadSHA != "" {
		headSHA = g.viewHeadSHA
	}
	if g.changeHeadOnSecondView && g.viewCalls >= 2 {
		headSHA = "new-head"
	}
	reviewRequests := g.effectiveReviewRequests()
	if g.removeReviewRequestOnSecondView && g.viewCalls >= 2 {
		reviewRequests = nil
	}
	reviewDecision := g.reviewDecision
	comments := g.comments
	if g.useReviewStateAfterFirstView && g.viewCalls > 1 {
		reviewDecision = g.reviewDecisionAfterFirstView
		comments = g.commentsAfterFirstView
	}
	state := g.viewState
	if state == "" {
		state = "OPEN"
	}
	return PullRequestDetail{Number: 42, Title: "Review me", Body: "PR body", State: state, IsDraft: g.viewDraft, ReviewDecision: reviewDecision, Labels: append([]string(nil), g.labels...), HeadSHA: headSHA, BaseSHA: "base123", HeadRefName: "feature/review-me", BaseRefName: "main", Author: "octocat", ReviewRequests: reviewRequests, HasConflicts: g.hasConflicts, ChecksSummary: "SUCCESS", Diff: "diff --git a/a.ts b/a.ts", Comments: cloneCommentMaps(comments), Reviews: cloneCommentMaps(g.reviews)}, nil
}

func cloneCommentMaps(comments []map[string]any) []map[string]any {
	if comments == nil {
		return nil
	}
	cloned := make([]map[string]any, 0, len(comments))
	for _, comment := range comments {
		clonedComment := make(map[string]any, len(comment))
		for key, value := range comment {
			clonedComment[key] = value
		}
		cloned = append(cloned, clonedComment)
	}
	return cloned
}

func (g *fakeGitHubGateway) effectiveReviewRequests() []string {
	if g.reviewRequests != nil {
		return append([]string(nil), g.reviewRequests...)
	}
	return []string{"octocat"}
}

func (g *fakeGitHubGateway) CapturePullRequestSnapshot(_ context.Context, input CapturePullRequestSnapshotInput) (storage.PullRequestSnapshotRecord, error) {
	headSHA := "abc123"
	if g.changeHeadOnSecondView && g.viewCalls >= 2 {
		headSHA = "new-head"
	}
	return storage.PullRequestSnapshotRecord{ID: fmt.Sprintf("snapshot:%d:%s", input.PRNumber, input.CapturedAt), ProjectID: input.ProjectID, Repo: input.Repo, PRNumber: input.PRNumber, HeadSHA: headSHA, BaseSHA: stringPtr("base123"), Title: stringPtr("Review me"), Body: stringPtr("PR body"), Author: stringPtr("octocat"), ChecksSummary: stringPtr("SUCCESS"), PayloadJSON: stringPtr(`{"diff":"diff --git a/a.ts b/a.ts"}`), CapturedAt: input.CapturedAt, CreatedAt: input.CapturedAt}, nil
}

func (g *fakeGitHubGateway) FindReviewMarker(_ context.Context, input VerifyReviewMarkerInput) (ReviewMarkerResult, error) {
	g.reviewMarkerCalls++
	if g.reviewMarkerErr != nil {
		return ReviewMarkerResult{}, g.reviewMarkerErr
	}
	if g.reviewMarkerEvent != "" && !reviewEventIn(input.AllowedReviewEvents, g.reviewMarkerEvent) {
		return ReviewMarkerResult{}, nil
	}
	if g.reviewMarkerMissing {
		return ReviewMarkerResult{}, nil
	}
	outcome := g.reviewMarkerOutcome
	if outcome == "" {
		outcome = "actionable"
	}
	body := g.reviewMarkerBody
	if body == "" && !g.reviewMarkerBodyExplicit {
		if outcome == "clean" && g.reviewMarkerEvent == ReviewEventApprove {
			body = cleanApproveReviewBody("octocat", outcome)
		} else {
			body = "review body <!-- looper:review outcome=" + outcome + " -->"
		}
	}
	return ReviewMarkerResult{Found: true, Outcome: outcome, Event: g.reviewMarkerEvent, Body: body, InlineCommentBodies: append([]string(nil), g.reviewMarkerInlineCommentBodies...)}, nil
}

func cleanApproveReviewBody(author string, outcome string) string {
	return fmt.Sprintf("@%s Thanks for the thoughtful update — I verified the changes are clear, focused, and safe to approve. Nice work tightening this up; it should be easier to maintain going forward.\n\n<!-- looper:review id=abc head=abc123 outcome=%s -->", author, outcome)
}

func (g *fakeGitHubGateway) AddPullRequestReaction(_ context.Context, input PullRequestReactionInput) error {
	g.addReactionCalls = append(g.addReactionCalls, input)
	return g.addReactionErr
}

func (g *fakeGitHubGateway) RemovePullRequestReaction(_ context.Context, input PullRequestReactionInput) error {
	g.removeReactionCalls = append(g.removeReactionCalls, input)
	return g.removeReactionErr
}

func (g *fakeGitHubGateway) AddPullRequestLabels(_ context.Context, input PullRequestLabelsInput) error {
	g.addLabelCalls = append(g.addLabelCalls, input)
	return g.addLabelErr
}

func (g *fakeGitHubGateway) RemovePullRequestLabels(_ context.Context, input PullRequestLabelsInput) error {
	g.removeLabelCalls = append(g.removeLabelCalls, input)
	return g.removeLabelErr
}

func (g *fakeGitHubGateway) ListReviewThreads(context.Context, ListReviewThreadsInput) ([]ReviewThread, error) {
	out := make([]ReviewThread, len(g.reviewThreads))
	copy(out, g.reviewThreads)
	return out, nil
}

func (g *fakeGitHubGateway) AddReviewThreadReply(_ context.Context, input AddReviewThreadReplyInput) error {
	g.addThreadReplyCalls = append(g.addThreadReplyCalls, input)
	return nil
}

func (g *fakeGitHubGateway) ResolveReviewThread(_ context.Context, input ResolveReviewThreadInput) error {
	g.resolveThreadCalls = append(g.resolveThreadCalls, input)
	for i := range g.reviewThreads {
		if g.reviewThreads[i].ID == input.ThreadID {
			g.reviewThreads[i].IsResolved = true
		}
	}
	return nil
}

func reviewEventIn(events []ReviewEvent, want ReviewEvent) bool {
	for _, event := range events {
		if event == want {
			return true
		}
	}
	return false
}

type fakeGitGateway struct {
	worktreePath string
	createCalls  []CreateWorktreeInput
	prepareCalls []PrepareWorktreeInput
	cleanupCalls []CleanupWorktreeInput
	prepareClean *bool
}

func (f *fakeGitGateway) CreateWorktree(_ context.Context, input CreateWorktreeInput) (CreateWorktreeResult, error) {
	f.createCalls = append(f.createCalls, input)
	path := f.worktreePath
	if path == "" {
		path = filepath.Join("/tmp", "reviewer-worktree")
		f.worktreePath = path
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		return CreateWorktreeResult{}, err
	}
	return CreateWorktreeResult{WorktreePath: path, Branch: input.Branch, HeadSHA: "abc123"}, nil
}

func (f *fakeGitGateway) PrepareWorktree(_ context.Context, input PrepareWorktreeInput) (PrepareWorktreeResult, error) {
	f.prepareCalls = append(f.prepareCalls, input)
	clean := true
	if f.prepareClean != nil {
		clean = *f.prepareClean
	}
	return PrepareWorktreeResult{HeadSHA: input.ExpectedHeadSHA, Clean: clean}, nil
}

func (f *fakeGitGateway) CleanupWorktree(_ context.Context, input CleanupWorktreeInput) error {
	f.cleanupCalls = append(f.cleanupCalls, input)
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
	if f.result.Status == "completed" && f.result.ParseStatus == "" && strings.Contains(f.result.Stdout, "__LOOPER_RESULT__=") {
		f.result.ParseStatus = "parsed"
	}
	return f.result, nil
}

type testLogger struct{}

func (*testLogger) Debug(string, map[string]any) {}
func (*testLogger) Info(string, map[string]any)  {}
func (*testLogger) Warn(string, map[string]any)  {}
func (*testLogger) Error(string, map[string]any) {}

func contains(haystack, needle string) bool { return strings.Contains(haystack, needle) }
