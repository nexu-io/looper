package webhookforward

import (
	"context"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/fixer"
	"github.com/nexu-io/looper/internal/reviewer"
	"github.com/nexu-io/looper/internal/storage"
)

func TestForwardDedupesDeliveriesWithinTTLAndExpiresAfterAnHour(t *testing.T) {
	repos := newTestRepositories(t)
	seedProject(t, repos, "project_1", "acme/looper")
	now := time.Date(2026, time.May, 16, 12, 0, 0, 0, time.UTC)
	reviewerRunner := newFakeTargetedRunner(nil)
	forwarder := New(Options{Repos: repos, Config: testConfig(t), Reviewer: reviewerRunner, Fixer: targetedFixerAdapter{runner: newFakeTargetedRunner(nil)}, Now: func() time.Time { return now }, MaxConcurrent: 1, QueueCapacity: 8})
	defer forwarder.Close()

	first, err := forwarder.Forward(context.Background(), DeliveryRequest{DeliveryID: "delivery-1", EventType: "pull_request", Payload: pullRequestPayload("review_requested", "acme/looper", 42)})
	if err != nil {
		t.Fatalf("first Forward() error = %v", err)
	}
	if first.Status != "accepted" {
		t.Fatalf("first status = %q, want accepted", first.Status)
	}
	reviewerRunner.waitForCalls(t, 1)

	second, err := forwarder.Forward(context.Background(), DeliveryRequest{DeliveryID: "delivery-1", EventType: "pull_request", Payload: pullRequestPayload("review_requested", "acme/looper", 42)})
	if err != nil {
		t.Fatalf("second Forward() error = %v", err)
	}
	if second.Status != "duplicate" {
		t.Fatalf("second status = %q, want duplicate", second.Status)
	}
	reviewerRunner.assertCallCount(t, 1)

	now = now.Add(time.Hour + time.Second)
	third, err := forwarder.Forward(context.Background(), DeliveryRequest{DeliveryID: "delivery-1", EventType: "pull_request", Payload: pullRequestPayload("review_requested", "acme/looper", 42)})
	if err != nil {
		t.Fatalf("third Forward() error = %v", err)
	}
	if third.Status != "accepted" {
		t.Fatalf("third status = %q, want accepted", third.Status)
	}
	reviewerRunner.waitForCalls(t, 2)

	stats := forwarder.Stats()
	if stats.DeliveriesDeduped != 1 {
		t.Fatalf("DeliveriesDeduped = %d, want 1", stats.DeliveriesDeduped)
	}
}

func TestForwardIgnoresUnsupportedAndIssueComments(t *testing.T) {
	repos := newTestRepositories(t)
	seedProject(t, repos, "project_1", "acme/looper")
	reviewerRunner := newFakeTargetedRunner(nil)
	fixerRunner := newFakeTargetedRunner(nil)
	forwarder := New(Options{Repos: repos, Config: testConfig(t), Reviewer: reviewerRunner, Fixer: targetedFixerAdapter{runner: fixerRunner}, MaxConcurrent: 1, QueueCapacity: 8})
	defer forwarder.Close()

	for _, request := range []DeliveryRequest{
		{DeliveryID: "ignored-1", EventType: "issues", Payload: []byte(`{"action":"opened"}`)},
		{DeliveryID: "ignored-2", EventType: "issue_comment", Payload: []byte(`{"action":"created","repository":{"full_name":"acme/looper"},"issue":{"number":42}}`)},
		{DeliveryID: "ignored-3", EventType: "issue_comment", Payload: []byte(`{"action":"created","repository":{"full_name":"acme/looper"},"issue":{"number":42,"pull_request":{"url":"https://api.github.com/repos/acme/looper/pulls/42"}}}`)},
	} {
		result, err := forwarder.Forward(context.Background(), request)
		if err != nil {
			t.Fatalf("Forward(%s) error = %v", request.EventType, err)
		}
		if result.Status != "ignored" {
			t.Fatalf("Forward(%s) status = %q, want ignored", request.EventType, result.Status)
		}
	}
	shortSleep()
	reviewerRunner.assertCallCount(t, 0)
	fixerRunner.assertCallCount(t, 0)
}

func TestForwardRoutesPullRequestLabelChangesToFixerOnly(t *testing.T) {
	repos := newTestRepositories(t)
	seedProject(t, repos, "project_1", "acme/looper")
	reviewerRunner := newFakeTargetedRunner(nil)
	fixerRunner := newFakeTargetedRunner(nil)
	forwarder := New(Options{Repos: repos, Config: testConfig(t), Reviewer: reviewerRunner, Fixer: targetedFixerAdapter{runner: fixerRunner}, MaxConcurrent: 1, QueueCapacity: 8})
	defer forwarder.Close()

	for i, action := range []string{"labeled", "unlabeled"} {
		if _, err := forwarder.Forward(context.Background(), DeliveryRequest{DeliveryID: "label-" + action, EventType: "pull_request", Payload: pullRequestPayload(action, "acme/looper", int64(42+i))}); err != nil {
			t.Fatalf("Forward(%s) error = %v", action, err)
		}
	}

	fixerRunner.waitForCalls(t, 2)
	reviewerRunner.assertCallCount(t, 0)
	fixerRunner.assertPRCount(t, 42, 1)
	fixerRunner.assertPRCount(t, 43, 1)
}

func TestForwardTriggersFixerForFailedCheckWebhookEvents(t *testing.T) {
	repos := newTestRepositories(t)
	seedProject(t, repos, "project_1", "acme/looper")
	reviewerRunner := newFakeTargetedRunner(nil)
	fixerRunner := newFakeTargetedRunner(nil)
	forwarder := New(Options{Repos: repos, Config: testConfig(t), Reviewer: reviewerRunner, Fixer: targetedFixerAdapter{runner: fixerRunner}, MaxConcurrent: 1, QueueCapacity: 8})
	defer forwarder.Close()

	requests := []DeliveryRequest{
		{DeliveryID: "check-run-1", EventType: "check_run", Payload: checkRunPayload("completed", "failure", "acme/looper", 42)},
		{DeliveryID: "check-run-2", EventType: "check_run", Payload: checkRunFallbackPayload("completed", "timed_out", "acme/looper", 43)},
	}

	for _, request := range requests {
		result, err := forwarder.Forward(context.Background(), request)
		if err != nil {
			t.Fatalf("Forward(%s) error = %v", request.EventType, err)
		}
		if result.Status != "accepted" {
			t.Fatalf("Forward(%s) status = %q, want accepted", request.EventType, result.Status)
		}
	}

	fixerRunner.waitForCalls(t, 2)
	fixerRunner.assertPRCount(t, 42, 1)
	fixerRunner.assertPRCount(t, 43, 1)
	reviewerRunner.assertCallCount(t, 0)
}

func TestForwardIgnoresNonPRAndNonFailedCheckWebhookEvents(t *testing.T) {
	repos := newTestRepositories(t)
	seedProject(t, repos, "project_1", "acme/looper")
	reviewerRunner := newFakeTargetedRunner(nil)
	fixerRunner := newFakeTargetedRunner(nil)
	forwarder := New(Options{Repos: repos, Config: testConfig(t), Reviewer: reviewerRunner, Fixer: targetedFixerAdapter{runner: fixerRunner}, MaxConcurrent: 1, QueueCapacity: 8})
	defer forwarder.Close()

	for _, request := range []DeliveryRequest{
		{DeliveryID: "check-run-no-pr", EventType: "check_run", Payload: []byte(`{"action":"completed","repository":{"full_name":"acme/looper"},"check_run":{"conclusion":"failure","pull_requests":[]}}`)},
		{DeliveryID: "check-run-success", EventType: "check_run", Payload: checkRunPayload("completed", "success", "acme/looper", 42)},
		{DeliveryID: "check-run-pending", EventType: "check_run", Payload: checkRunPayload("requested", "", "acme/looper", 42)},
	} {
		result, err := forwarder.Forward(context.Background(), request)
		if err != nil {
			t.Fatalf("Forward(%s) error = %v", request.DeliveryID, err)
		}
		if result.Status != "ignored" {
			t.Fatalf("Forward(%s) status = %q, want ignored", request.DeliveryID, result.Status)
		}
	}

	shortSleep()
	reviewerRunner.assertCallCount(t, 0)
	fixerRunner.assertCallCount(t, 0)
}

func TestForwardFansOutToMultipleProjectsForSameRepo(t *testing.T) {
	repos := newTestRepositories(t)
	seedProject(t, repos, "project_1", "acme/looper")
	seedProject(t, repos, "project_2", "acme/looper")
	reviewerRunner := newFakeTargetedRunner(nil)
	forwarder := New(Options{Repos: repos, Config: testConfig(t), Reviewer: reviewerRunner, Fixer: targetedFixerAdapter{runner: newFakeTargetedRunner(nil)}, MaxConcurrent: 2, QueueCapacity: 8})
	defer forwarder.Close()

	result, err := forwarder.Forward(context.Background(), DeliveryRequest{DeliveryID: "fanout-1", EventType: "pull_request", Payload: pullRequestPayload("review_requested", "Acme/Looper", 55)})
	if err != nil {
		t.Fatalf("Forward() error = %v", err)
	}
	if result.WorkItems != 2 {
		t.Fatalf("WorkItems = %d, want 2", result.WorkItems)
	}
	reviewerRunner.waitForCalls(t, 2)
	reviewerRunner.assertProjects(t, []string{"project_1", "project_2"})
	reviewerRunner.assertRepos(t, []string{"acme/looper", "acme/looper"})
}

func TestForwardCoalescesQueuedWorkAndUnionsLanes(t *testing.T) {
	repos := newTestRepositories(t)
	seedProject(t, repos, "project_1", "acme/looper")
	reviewerRunner := newFakeTargetedRunner(make(chan struct{}))
	fixerRunner := newFakeTargetedRunner(nil)
	forwarder := New(Options{Repos: repos, Config: testConfig(t), Reviewer: reviewerRunner, Fixer: targetedFixerAdapter{runner: fixerRunner}, MaxConcurrent: 1, QueueCapacity: 8})
	defer forwarder.Close()

	if _, err := forwarder.Forward(context.Background(), DeliveryRequest{DeliveryID: "busy-pr1", EventType: "pull_request", Payload: pullRequestPayload("opened", "acme/looper", 1)}); err != nil {
		t.Fatalf("busy Forward() error = %v", err)
	}
	reviewerRunner.waitForCall(t, 1)

	if _, err := forwarder.Forward(context.Background(), DeliveryRequest{DeliveryID: "queue-pr2-a", EventType: "pull_request", Payload: pullRequestPayload("review_requested", "acme/looper", 2)}); err != nil {
		t.Fatalf("queue Forward(a) error = %v", err)
	}
	if _, err := forwarder.Forward(context.Background(), DeliveryRequest{DeliveryID: "queue-pr2-b", EventType: "pull_request_review_comment", Payload: pullRequestPayload("created", "acme/looper", 2)}); err != nil {
		t.Fatalf("queue Forward(b) error = %v", err)
	}

	close(reviewerRunner.block)
	reviewerRunner.waitForCalls(t, 2)
	fixerRunner.waitForCalls(t, 2)

	reviewerRunner.assertPRCount(t, 2, 1)
	fixerRunner.assertPRCount(t, 2, 1)
	stats := forwarder.Stats()
	if stats.QueueCoalesced == 0 {
		t.Fatalf("QueueCoalesced = %d, want > 0", stats.QueueCoalesced)
	}
}

func TestForwardUsesBoundedConcurrencyAndPerWorkKeyLocking(t *testing.T) {
	repos := newTestRepositories(t)
	seedProject(t, repos, "project_1", "acme/looper")
	reviewerRunner := newFakeTargetedRunner(make(chan struct{}))
	forwarder := New(Options{Repos: repos, Config: testConfig(t), Reviewer: reviewerRunner, Fixer: targetedFixerAdapter{runner: newFakeTargetedRunner(nil)}, MaxConcurrent: 2, QueueCapacity: 8})
	defer forwarder.Close()

	requests := []DeliveryRequest{
		{DeliveryID: "lock-1", EventType: "pull_request", Payload: pullRequestPayload("review_requested", "acme/looper", 10)},
		{DeliveryID: "lock-2", EventType: "pull_request", Payload: pullRequestPayload("review_requested", "acme/looper", 11)},
		{DeliveryID: "lock-3", EventType: "pull_request", Payload: pullRequestPayload("review_requested", "acme/looper", 10)},
	}
	for _, request := range requests[:2] {
		if _, err := forwarder.Forward(context.Background(), request); err != nil {
			t.Fatalf("Forward(%s) error = %v", request.DeliveryID, err)
		}
	}
	reviewerRunner.waitForCalls(t, 2)
	if _, err := forwarder.Forward(context.Background(), requests[2]); err != nil {
		t.Fatalf("Forward(lock-3) error = %v", err)
	}
	close(reviewerRunner.block)
	reviewerRunner.waitForCalls(t, 3)

	if reviewerRunner.maxActive() > 2 {
		t.Fatalf("max active = %d, want <= 2", reviewerRunner.maxActive())
	}
	if reviewerRunner.maxActiveForKey("project_1|acme/looper|10") > 1 {
		t.Fatalf("max active for same PR = %d, want 1", reviewerRunner.maxActiveForKey("project_1|acme/looper|10"))
	}
}

func TestForwardRetriesTransientTargetedFailuresWithoutEscalation(t *testing.T) {
	repos := newTestRepositories(t)
	seedProject(t, repos, "project_1", "acme/looper")
	reviewerRunner := newFakeTargetedRunner(nil)
	reviewerRunner.failOnce("project_1|acme/looper|77")
	forwarder := New(Options{Repos: repos, Config: testConfig(t), Reviewer: reviewerRunner, Fixer: targetedFixerAdapter{runner: newFakeTargetedRunner(nil)}, MaxConcurrent: 1, QueueCapacity: 8, RetryDelay: time.Millisecond})
	defer forwarder.Close()

	if _, err := forwarder.Forward(context.Background(), DeliveryRequest{DeliveryID: "retry-1", EventType: "pull_request", Payload: pullRequestPayload("review_requested", "acme/looper", 77)}); err != nil {
		t.Fatalf("Forward() error = %v", err)
	}
	reviewerRunner.waitForCalls(t, 2)
	if reviewerRunner.nonTargetedCalls() != 0 {
		t.Fatalf("non-targeted calls = %d, want 0", reviewerRunner.nonTargetedCalls())
	}
	stats := forwarder.Stats()
	if stats.ExecutionsRetried != 1 {
		t.Fatalf("ExecutionsRetried = %d, want 1", stats.ExecutionsRetried)
	}
	if stats.ExecutionsSucceeded != 1 {
		t.Fatalf("ExecutionsSucceeded = %d, want 1", stats.ExecutionsSucceeded)
	}
}

func TestForwardPreservesCoalescedWorkWhenRequeueHitsCapacity(t *testing.T) {
	repos := newTestRepositories(t)
	seedProject(t, repos, "project_1", "acme/looper")
	reviewerRunner := newFakeTargetedRunner(make(chan struct{}))
	fixerRunner := newFakeTargetedRunner(nil)
	forwarder := New(Options{Repos: repos, Config: testConfig(t), Reviewer: reviewerRunner, Fixer: targetedFixerAdapter{runner: fixerRunner}, MaxConcurrent: 1, QueueCapacity: 1})
	defer forwarder.Close()

	if _, err := forwarder.Forward(context.Background(), DeliveryRequest{DeliveryID: "running-pr1", EventType: "pull_request", Payload: pullRequestPayload("review_requested", "acme/looper", 1)}); err != nil {
		t.Fatalf("Forward(running-pr1) error = %v", err)
	}
	reviewerRunner.waitForCall(t, 1)

	if _, err := forwarder.Forward(context.Background(), DeliveryRequest{DeliveryID: "queued-pr2", EventType: "pull_request", Payload: pullRequestPayload("review_requested", "acme/looper", 2)}); err != nil {
		t.Fatalf("Forward(queued-pr2) error = %v", err)
	}
	if _, err := forwarder.Forward(context.Background(), DeliveryRequest{DeliveryID: "coalesced-pr1", EventType: "pull_request", Payload: pullRequestPayload("labeled", "acme/looper", 1)}); err != nil {
		t.Fatalf("Forward(coalesced-pr1) error = %v", err)
	}

	close(reviewerRunner.block)
	reviewerRunner.waitForCalls(t, 2)
	fixerRunner.waitForCalls(t, 1)

	reviewerRunner.assertPRCount(t, 1, 1)
	reviewerRunner.assertPRCount(t, 2, 1)
	fixerRunner.assertPRCount(t, 1, 1)
	stats := forwarder.Stats()
	if stats.QueueRejected != 0 {
		t.Fatalf("QueueRejected = %d, want 0", stats.QueueRejected)
	}
	if stats.QueueCoalesced == 0 {
		t.Fatalf("QueueCoalesced = %d, want > 0", stats.QueueCoalesced)
	}
}

func TestForwardKeepsFixedSizeRecentOutcomes(t *testing.T) {
	repos := newTestRepositories(t)
	seedProject(t, repos, "project_1", "acme/looper")
	reviewerRunner := newFakeTargetedRunner(nil)
	forwarder := New(Options{Repos: repos, Config: testConfig(t), Reviewer: reviewerRunner, Fixer: targetedFixerAdapter{runner: newFakeTargetedRunner(nil)}, MaxConcurrent: 1, QueueCapacity: 8, RecentOutcomeLimit: 2})
	defer forwarder.Close()

	for i, deliveryID := range []string{"recent-1", "recent-2", "recent-3"} {
		if _, err := forwarder.Forward(context.Background(), DeliveryRequest{DeliveryID: deliveryID, EventType: "pull_request", Payload: pullRequestPayload("review_requested", "acme/looper", int64(i+1))}); err != nil {
			t.Fatalf("Forward(%s) error = %v", deliveryID, err)
		}
	}
	reviewerRunner.waitForCalls(t, 3)
	stats := forwarder.Stats()
	if len(stats.RecentOutcomes) != 2 {
		t.Fatalf("len(RecentOutcomes) = %d, want 2", len(stats.RecentOutcomes))
	}
	if stats.RecentOutcomes[0].Number != 2 || stats.RecentOutcomes[1].Number != 3 {
		t.Fatalf("RecentOutcomes numbers = %#v, want [2 3]", []int64{stats.RecentOutcomes[0].Number, stats.RecentOutcomes[1].Number})
	}
}

func TestForwardRoutesPushToBaseBranchFixerDiscovery(t *testing.T) {
	repos := newTestRepositories(t)
	seedProject(t, repos, "project_1", "acme/looper")
	fixerRunner := newFakeTargetedRunner(nil)
	forwarder := New(Options{Repos: repos, Config: testConfig(t), Reviewer: newFakeTargetedRunner(nil), Fixer: targetedFixerAdapter{runner: fixerRunner}, MaxConcurrent: 1, QueueCapacity: 8})
	defer forwarder.Close()

	result, err := forwarder.Forward(context.Background(), DeliveryRequest{DeliveryID: "push-1", EventType: "push", Payload: []byte(`{"ref":"refs/heads/main","repository":{"full_name":"acme/looper"}}`)})
	if err != nil {
		t.Fatalf("Forward() error = %v", err)
	}
	if result.Status != "accepted" || result.WorkItems != 1 {
		t.Fatalf("result = %#v, want accepted one work item", result)
	}
	fixerRunner.waitForCalls(t, 1)
	fixerRunner.assertBaseBranchCount(t, "project_1", "acme/looper", "main", 1)
}

func TestForwardPreservesBranchCaseInBaseBranchWorkKeys(t *testing.T) {
	repos := newTestRepositories(t)
	seedProject(t, repos, "project_1", "acme/looper")
	fixerRunner := newFakeTargetedRunner(make(chan struct{}))
	forwarder := New(Options{Repos: repos, Config: testConfig(t), Reviewer: newFakeTargetedRunner(nil), Fixer: targetedFixerAdapter{runner: fixerRunner}, MaxConcurrent: 1, QueueCapacity: 8})
	defer forwarder.Close()

	if _, err := forwarder.Forward(context.Background(), DeliveryRequest{DeliveryID: "push-case-1", EventType: "push", Payload: []byte(`{"ref":"refs/heads/Release","repository":{"full_name":"acme/looper"}}`)}); err != nil {
		t.Fatalf("Forward(Release) error = %v", err)
	}
	fixerRunner.waitForCall(t, 1)
	if _, err := forwarder.Forward(context.Background(), DeliveryRequest{DeliveryID: "push-case-2", EventType: "push", Payload: []byte(`{"ref":"refs/heads/release","repository":{"full_name":"acme/looper"}}`)}); err != nil {
		t.Fatalf("Forward(release) error = %v", err)
	}

	close(fixerRunner.block)
	fixerRunner.waitForCalls(t, 2)
	fixerRunner.assertBaseBranchCount(t, "project_1", "acme/looper", "Release", 1)
	fixerRunner.assertBaseBranchCount(t, "project_1", "acme/looper", "release", 1)
	stats := forwarder.Stats()
	if stats.QueueCoalesced != 0 {
		t.Fatalf("QueueCoalesced = %d, want 0", stats.QueueCoalesced)
	}
}

type fakeTargetedRunner struct {
	mu                   sync.Mutex
	block                chan struct{}
	calls                []targetedCall
	active               int
	maxConcurrent        int
	activeByKey          map[string]int
	maxConcurrentByKey   map[string]int
	failuresRemaining    map[string]int
	nonTargetedCallCount int
}

type targetedCall struct {
	ProjectID  string
	Repo       string
	PRNumber   int64
	BaseBranch string
}

type temporaryError struct{ message string }

func (e temporaryError) Error() string   { return e.message }
func (e temporaryError) Temporary() bool { return true }

func newFakeTargetedRunner(block chan struct{}) *fakeTargetedRunner {
	return &fakeTargetedRunner{block: block, activeByKey: map[string]int{}, maxConcurrentByKey: map[string]int{}, failuresRemaining: map[string]int{}}
}

func (f *fakeTargetedRunner) DiscoverPullRequest(_ context.Context, input reviewer.TargetedDiscoveryInput) (reviewer.DiscoveryResult, error) {
	return f.run(input.ProjectID, input.Repo, input.PRNumber)
}

func (f *fakeTargetedRunner) run(projectID, repo string, prNumber int64) (reviewer.DiscoveryResult, error) {
	key := runnerKey(projectID, repo, prNumber)
	f.mu.Lock()
	f.calls = append(f.calls, targetedCall{ProjectID: projectID, Repo: repo, PRNumber: prNumber})
	f.active++
	if f.active > f.maxConcurrent {
		f.maxConcurrent = f.active
	}
	f.activeByKey[key]++
	if f.activeByKey[key] > f.maxConcurrentByKey[key] {
		f.maxConcurrentByKey[key] = f.activeByKey[key]
	}
	shouldFail := f.failuresRemaining[key] > 0
	if shouldFail {
		f.failuresRemaining[key]--
	}
	block := f.block
	f.mu.Unlock()
	if block != nil {
		<-block
	}
	f.mu.Lock()
	f.active--
	f.activeByKey[key]--
	f.mu.Unlock()
	if shouldFail {
		return reviewer.DiscoveryResult{}, temporaryError{message: "temporary github failure"}
	}
	return reviewer.DiscoveryResult{}, nil
}

func (f *fakeTargetedRunner) runBaseBranch(projectID, repo, baseBranch string) error {
	f.mu.Lock()
	f.calls = append(f.calls, targetedCall{ProjectID: projectID, Repo: repo, BaseBranch: baseBranch})
	block := f.block
	f.mu.Unlock()
	if block != nil {
		<-block
	}
	return nil
}

func (f *fakeTargetedRunner) failOnce(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failuresRemaining[key] = 1
}

func (f *fakeTargetedRunner) waitForCall(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		got := len(f.calls)
		f.mu.Unlock()
		if got >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("call count did not reach %d", want)
}

func (f *fakeTargetedRunner) waitForCalls(t *testing.T, want int) {
	f.waitForCall(t, want)
}

func (f *fakeTargetedRunner) assertCallCount(t *testing.T, want int) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) != want {
		t.Fatalf("call count = %d, want %d", len(f.calls), want)
	}
}

func (f *fakeTargetedRunner) assertProjects(t *testing.T, want []string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	got := make([]string, 0, len(f.calls))
	for _, call := range f.calls {
		got = append(got, call.ProjectID)
	}
	assertStringMultiset(t, got, want, "projects")
}

func (f *fakeTargetedRunner) assertRepos(t *testing.T, want []string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	got := make([]string, 0, len(f.calls))
	for _, call := range f.calls {
		got = append(got, call.Repo)
	}
	assertStringMultiset(t, got, want, "repos")
}

func (f *fakeTargetedRunner) assertPRCount(t *testing.T, prNumber int64, want int) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	for _, call := range f.calls {
		if call.PRNumber == prNumber {
			count++
		}
	}
	if count != want {
		t.Fatalf("PR %d call count = %d, want %d", prNumber, count, want)
	}
}

func (f *fakeTargetedRunner) assertBaseBranchCount(t *testing.T, projectID, repo, baseBranch string, want int) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	for _, call := range f.calls {
		if call.ProjectID == projectID && call.Repo == repo && call.BaseBranch == baseBranch {
			count++
		}
	}
	if count != want {
		t.Fatalf("base branch %s/%s@%s call count = %d, want %d", projectID, repo, baseBranch, count, want)
	}
}

func (f *fakeTargetedRunner) maxActive() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maxConcurrent
}

func (f *fakeTargetedRunner) maxActiveForKey(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maxConcurrentByKey[key]
}

func (f *fakeTargetedRunner) nonTargetedCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.nonTargetedCallCount
}

func testConfig(t *testing.T) config.Config {
	t.Helper()
	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Roles.Reviewer.Discovery.AutoDiscovery = true
	cfg.Roles.Fixer.AutoDiscovery = true
	return cfg
}

func newTestRepositories(t *testing.T) *storage.Repositories {
	t.Helper()
	root := t.TempDir()
	coordinator, err := storage.OpenSQLiteCoordinator(context.Background(), filepath.Join(root, "looper.sqlite"), storage.SQLiteCoordinatorOptions{Migrations: storage.EmbeddedMigrations, BackupDir: filepath.Join(root, "backups")})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	if _, err := coordinator.MigrationRunner().RunPending(context.Background()); err != nil {
		t.Fatalf("RunPending() error = %v", err)
	}
	return storage.NewRepositories(coordinator.DB())
}

func seedProject(t *testing.T, repos *storage.Repositories, projectID, repo string) {
	t.Helper()
	metadata := `{"repo":"` + repo + `"}`
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: projectID, RepoPath: "/tmp/" + projectID, MetadataJSON: &metadata, CreatedAt: "2026-05-16T12:00:00.000Z", UpdatedAt: "2026-05-16T12:00:00.000Z"}); err != nil {
		t.Fatalf("Projects.Upsert(%s) error = %v", projectID, err)
	}
}

func pullRequestPayload(action, repo string, prNumber int64) []byte {
	return []byte(`{"action":"` + action + `","repository":{"full_name":"` + repo + `"},"pull_request":{"number":` + itoa(prNumber) + `}}`)
}

func checkRunPayload(action, conclusion, repo string, prNumber int64) []byte {
	return []byte(`{"action":"` + action + `","repository":{"full_name":"` + repo + `"},"check_run":{"conclusion":"` + conclusion + `","pull_requests":[{"number":` + itoa(prNumber) + `}]}}`)
}

func checkRunFallbackPayload(action, conclusion, repo string, prNumber int64) []byte {
	return []byte(`{"action":"` + action + `","repository":{"full_name":"` + repo + `"},"check_run":{"conclusion":"` + conclusion + `","pull_requests":[],"check_suite":{"pull_requests":[{"number":` + itoa(prNumber) + `}]}}}`)
}

func itoa(value int64) string {
	return strconv.FormatInt(value, 10)
}

func runnerKey(projectID, repo string, prNumber int64) string {
	return projectID + "|" + repo + "|" + itoa(prNumber)
}

func shortSleep() {
	time.Sleep(50 * time.Millisecond)
}

func assertStringMultiset(t *testing.T, got, want []string, label string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s = %#v, want %#v", label, got, want)
	}
	counts := map[string]int{}
	for _, item := range got {
		counts[item]++
	}
	for _, item := range want {
		counts[item]--
	}
	for _, count := range counts {
		if count != 0 {
			t.Fatalf("%s = %#v, want %#v", label, got, want)
		}
	}
}

var _ TargetedFixer = targetedFixerAdapter{}

type targetedFixerAdapter struct{ runner *fakeTargetedRunner }

func (a targetedFixerAdapter) DiscoverPullRequest(_ context.Context, input fixer.TargetedDiscoveryInput) (fixer.DiscoveryResult, error) {
	_, err := a.runner.run(input.ProjectID, input.Repo, input.PRNumber)
	return fixer.DiscoveryResult{}, err
}

func (a targetedFixerAdapter) DiscoverPullRequestsForBaseBranchUpdate(_ context.Context, input fixer.BaseBranchDiscoveryInput) (fixer.DiscoveryResult, error) {
	return fixer.DiscoveryResult{}, a.runner.runBaseBranch(input.ProjectID, input.Repo, input.BaseRefName)
}
