package reviewer

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/powerformer/looper/internal/storage"
)

func TestDiscoverPullRequestsCreatesLoopAndQueue(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})

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
	if queue == nil || queue.Status != "queued" || queue.DedupeKey != "reviewer:acme/looper:42" {
		t.Fatalf("queue = %#v, want queued reviewer item", queue)
	}
}

func TestProcessClaimedItemRetriesPublishFromCheckpointWithoutRerunningReview(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{submitFailuresRemaining: 1}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "Please add tests", Stdout: `{"verdict":"actionable","body":"Please add tests","comments":[{"body":"Please add tests"}]}`}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, AllowAutoApprove: true})

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
	if firstResult.Status != "failed" || firstResult.FailureKind != FailureRetryableAfterResume {
		t.Fatalf("first result = %#v, want retryable_after_resume failure", firstResult)
	}
	if len(agent.starts) != 1 {
		t.Fatalf("len(agent.starts) = %d, want 1", len(agent.starts))
	}
	if len(github.submitCalls) != 1 {
		t.Fatalf("len(github.submitCalls) = %d, want 1", len(github.submitCalls))
	}
	runs, err := fixture.repos.Runs.ListByLoop(context.Background(), firstResult.LoopID)
	if err != nil {
		t.Fatalf("Runs.ListByLoop() error = %v", err)
	}
	if len(runs) == 0 || runs[0].LastCompletedStep == nil || *runs[0].LastCompletedStep != string(stepReview) {
		t.Fatalf("runs[0] = %#v, want lastCompletedStep=review", runs)
	}
	queueAfterFail, err := fixture.repos.Queue.GetByID(context.Background(), firstClaim.ID)
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	if queueAfterFail == nil || queueAfterFail.Status != "queued" {
		t.Fatalf("queue after fail = %#v, want queued retry", queueAfterFail)
	}

	fixture.advance(5 * time.Second)
	retryClaim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || retryClaim == nil {
		t.Fatalf("retry ClaimNext() = (%#v, %v), want claimed queue item", retryClaim, err)
	}
	retryResult, err := runner.ProcessClaimedItem(context.Background(), *retryClaim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem(retry) error = %v", err)
	}
	if retryResult.Status != "success" {
		t.Fatalf("retry result = %#v, want success", retryResult)
	}
	if len(agent.starts) != 1 {
		t.Fatalf("len(agent.starts) after retry = %d, want 1", len(agent.starts))
	}
	if len(github.submitCalls) != 2 {
		t.Fatalf("len(github.submitCalls) after retry = %d, want 2", len(github.submitCalls))
	}
	queueAfterSuccess, err := fixture.repos.Queue.GetByID(context.Background(), retryClaim.ID)
	if err != nil {
		t.Fatalf("Queue.GetByID(retry) error = %v", err)
	}
	if queueAfterSuccess == nil || queueAfterSuccess.Status != "completed" {
		t.Fatalf("queue after success = %#v, want completed", queueAfterSuccess)
	}
	loop, err := fixture.repos.Loops.GetByID(context.Background(), retryResult.LoopID)
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if loop == nil || loop.Status != "completed" || loop.MetadataJSON == nil || !contains(*loop.MetadataJSON, `"lastPublishedHeadSha":"abc123"`) {
		t.Fatalf("loop after success = %#v, want completed with lastPublishedHeadSha", loop)
	}
}

func TestProcessClaimedItemRestartsFromDiscoverWhenHeadChangesBeforePublish(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{changeHeadOnSecondView: true}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "Review old head", Stdout: `{"verdict":"actionable","body":"Review old head","comments":[{"body":"Review old head"}]}`}, {Status: "completed", Summary: "Review new head", Stdout: `{"verdict":"actionable","body":"Review new head","comments":[{"body":"Review new head"}]}`}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now})

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
		t.Fatalf("first result = %#v, want head-change retryable failure", firstResult)
	}
	if len(agent.starts) != 1 || len(github.submitCalls) != 0 {
		t.Fatalf("agent starts=%d submit calls=%d, want 1 and 0", len(agent.starts), len(github.submitCalls))
	}

	fixture.advance(5 * time.Second)
	retryClaim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || retryClaim == nil {
		t.Fatalf("retry ClaimNext() = (%#v, %v), want claimed queue item", retryClaim, err)
	}
	retryResult, err := runner.ProcessClaimedItem(context.Background(), *retryClaim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem(retry) error = %v", err)
	}
	if retryResult.Status != "success" {
		t.Fatalf("retry result = %#v, want success", retryResult)
	}
	if len(agent.starts) != 2 {
		t.Fatalf("len(agent.starts) = %d, want 2 after restart from discover", len(agent.starts))
	}
	if len(github.submitCalls) != 1 || github.submitCalls[0].CommitID != "new-head" {
		t.Fatalf("submit calls = %#v, want single publish for new-head", github.submitCalls)
	}
}

func TestProcessClaimedItemNotifiesWhenReviewAgentStarts(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "Looks good", Stdout: `{"verdict":"clean","body":"","comments":[]}`}}}
	notifications := make([]AgentExecutionStartedInput, 0, 1)
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, OnAgentExecutionStarted: func(_ context.Context, input AgentExecutionStartedInput) error {
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
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: &fakeGitHubGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})
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
	github := &fakeGitHubGateway{submitFailuresRemaining: 1}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "Please add tests", Stdout: `{"verdict":"actionable","body":"Please add tests","comments":[{"body":"Please add tests"}]}`}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, AllowAutoApprove: true})

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

func TestExtractReviewOutputStripsCompletionMarkerLine(t *testing.T) {
	t.Parallel()

	stdout := strings.Join([]string{
		`{"verdict":"clean","body":"","comments":[]}`,
		`__LOOPER_RESULT__={"summary":"ok"}`,
	}, "\n")
	output := extractReviewOutput(stdout)
	if strings.Contains(output, "__LOOPER_RESULT__") {
		t.Fatalf("output = %q, want marker stripped", output)
	}
	parsed, ok := parseStructuredReviewOutput(output)
	if !ok || !parsed.Clean {
		t.Fatalf("parsed = %#v, %v, want clean structured review", parsed, ok)
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

func (f *runnerFixture) advance(delta time.Duration) { f.current = f.current.Add(delta) }

func (f *runnerFixture) nowISO() string {
	return fmt.Sprintf("%s.000Z", f.current.UTC().Format("2006-01-02T15:04:05"))
}

type fakeGitHubGateway struct {
	submitFailuresRemaining int
	changeHeadOnSecondView  bool
	viewCalls               int
	submitCalls             []SubmitReviewInput
	addedLabels             []PullRequestLabelsInput
	removedLabels           []PullRequestLabelsInput
	prComments              []PullRequestCommentInput
	addedReactions          []PullRequestReactionInput
	removedReactions        []PullRequestReactionInput
	labels                  []string
}

func (g *fakeGitHubGateway) ListOpenPullRequests(context.Context, ListOpenPullRequestsInput) ([]PullRequestSummary, error) {
	return []PullRequestSummary{{Number: 42, Title: "Review me", State: "OPEN", Labels: append([]string(nil), g.labels...), HeadSHA: "abc123", ReviewRequests: []string{"octocat"}}, {Number: 99, Title: "Draft", State: "OPEN", IsDraft: true, HeadSHA: "draft123", ReviewRequests: []string{"octocat"}}}, nil
}

func (g *fakeGitHubGateway) GetCurrentUserLogin(context.Context, string) (string, error) {
	return "octocat", nil
}

func (g *fakeGitHubGateway) ViewPullRequest(context.Context, ViewPullRequestInput) (PullRequestDetail, error) {
	g.viewCalls++
	headSHA := "abc123"
	if g.changeHeadOnSecondView && g.viewCalls >= 2 {
		headSHA = "new-head"
	}
	return PullRequestDetail{Number: 42, Title: "Review me", Body: "PR body", State: "OPEN", Labels: append([]string(nil), g.labels...), HeadSHA: headSHA, BaseSHA: "base123", Author: "octocat", ReviewRequests: []string{"octocat"}, ChecksSummary: "SUCCESS", Diff: "diff --git a/a.ts b/a.ts"}, nil
}

func (g *fakeGitHubGateway) CapturePullRequestSnapshot(_ context.Context, input CapturePullRequestSnapshotInput) (storage.PullRequestSnapshotRecord, error) {
	headSHA := "abc123"
	if g.changeHeadOnSecondView && g.viewCalls >= 2 {
		headSHA = "new-head"
	}
	return storage.PullRequestSnapshotRecord{ID: fmt.Sprintf("snapshot:%d:%s", input.PRNumber, input.CapturedAt), ProjectID: input.ProjectID, Repo: input.Repo, PRNumber: input.PRNumber, HeadSHA: headSHA, BaseSHA: stringPtr("base123"), Title: stringPtr("Review me"), Body: stringPtr("PR body"), Author: stringPtr("octocat"), ChecksSummary: stringPtr("SUCCESS"), PayloadJSON: stringPtr(`{"diff":"diff --git a/a.ts b/a.ts"}`), CapturedAt: input.CapturedAt, CreatedAt: input.CapturedAt}, nil
}

func (g *fakeGitHubGateway) SubmitReview(_ context.Context, input SubmitReviewInput) error {
	g.submitCalls = append(g.submitCalls, input)
	if g.submitFailuresRemaining > 0 {
		g.submitFailuresRemaining--
		return fmt.Errorf("temporary GitHub failure")
	}
	return nil
}

func (g *fakeGitHubGateway) AddPullRequestComment(_ context.Context, input PullRequestCommentInput) error {
	g.prComments = append(g.prComments, input)
	return nil
}

func (g *fakeGitHubGateway) AddPullRequestReaction(_ context.Context, input PullRequestReactionInput) error {
	g.addedReactions = append(g.addedReactions, input)
	return nil
}

func (g *fakeGitHubGateway) RemovePullRequestReaction(_ context.Context, input PullRequestReactionInput) error {
	g.removedReactions = append(g.removedReactions, input)
	return nil
}

func (g *fakeGitHubGateway) AddPullRequestLabels(_ context.Context, input PullRequestLabelsInput) error {
	g.addedLabels = append(g.addedLabels, input)
	g.labels = append(g.labels, input.Labels...)
	return nil
}

func (g *fakeGitHubGateway) RemovePullRequestLabels(_ context.Context, input PullRequestLabelsInput) error {
	g.removedLabels = append(g.removedLabels, input)
	remaining := make([]string, 0, len(g.labels))
	for _, candidate := range g.labels {
		remove := false
		for _, label := range input.Labels {
			if strings.EqualFold(candidate, label) {
				remove = true
				break
			}
		}
		if !remove {
			remaining = append(remaining, candidate)
		}
	}
	g.labels = remaining
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

type testLogger struct{}

func (*testLogger) Debug(string, map[string]any) {}
func (*testLogger) Info(string, map[string]any)  {}
func (*testLogger) Warn(string, map[string]any)  {}
func (*testLogger) Error(string, map[string]any) {}

func contains(haystack, needle string) bool { return strings.Contains(haystack, needle) }
