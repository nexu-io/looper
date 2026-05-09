package sweeper

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/storage"
)

func TestDiscoverIssuesSkipsWhenAutoDiscoveryDisabledForProject(t *testing.T) {
	t.Parallel()

	repos := newTestRepositories(t)
	now := time.Date(2026, time.May, 9, 12, 0, 0, 0, time.UTC)
	nowISO := now.Format(javaScriptISOStringUTC)
	projectID := "demo"
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Demo", RepoPath: filepath.Join(t.TempDir(), "repo"), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	defaultConfig, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	runner := New(Options{Repos: repos, Now: func() time.Time { return now }, Config: &defaultConfig})
	result, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: projectID, Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	if result.Skipped != 1 || len(result.QueueItems) != 0 {
		t.Fatalf("DiscoverIssues() = %#v, want one skipped result with no queue items", result)
	}
}

func TestDiscoverIssuesEnqueuesWarnAndCloseCandidates(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	fixture.github.issues = []githubinfra.IssueSummary{
		{Number: 1, Title: "stale bug", Body: "needs cleanup", UpdatedAt: fixture.now.Add(-91 * 24 * time.Hour).Format(time.RFC3339), Author: "octo", Labels: nil},
		{Number: 2, Title: "pending bug", Body: "already warned", Author: "octo", Labels: []string{"looper:sweep-pending"}},
	}
	payload := mustMarshalPayload(sweeperPayload{Phase: "warn", Outcome: outcomePending, Repo: "acme/looper", TargetType: "issue", TargetNumber: 2, CloseBy: fixture.now.Add(-24 * time.Hour).Format(javaScriptISOStringUTC)})
	if err := fixture.repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: "prior-warn", ProjectID: stringPtr(fixture.projectID), Type: QueueTypeWarn, TargetType: "issue", TargetID: "acme/looper#2", Repo: stringPtr("acme/looper"), DedupeKey: "done:warn:2", Priority: 1, Status: "completed", AvailableAt: fixture.nowISO, PayloadJSON: &payload, MaxAttempts: 3, CreatedAt: fixture.nowISO, UpdatedAt: fixture.nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	result, err := fixture.runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	if len(result.QueueItems) != 2 {
		t.Fatalf("len(QueueItems) = %d, want 2", len(result.QueueItems))
	}
	types := []string{result.QueueItems[0].Type, result.QueueItems[1].Type}
	if !(containsString(types, QueueTypeWarn) && containsString(types, QueueTypeClose)) {
		t.Fatalf("queue types = %v, want warn and close", types)
	}
}

func TestProcessWarnSkipsFreshStaleIssueCandidates(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	fixture.cfg.Roles.Sweeper.DryRun = false
	fixture.github.issueDetails["acme/looper#1"] = githubinfra.IssueDetail{Number: 1, Title: "fresh bug", Body: "needs cleanup", State: "open", UpdatedAt: fixture.now.Add(-24 * time.Hour).Format(time.RFC3339), Author: "octo"}
	queueID := "queue_sweeper_warn_fresh_issue"
	if err := fixture.repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: queueID, ProjectID: &fixture.projectID, Type: QueueTypeWarn, TargetType: "issue", TargetID: "acme/looper#1", Repo: stringPtr("acme/looper"), DedupeKey: "sweeper:warn:acme/looper#1", Priority: 1, Status: "running", AvailableAt: fixture.nowISO, MaxAttempts: 3, CreatedAt: fixture.nowISO, UpdatedAt: fixture.nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	result, err := fixture.runner.ProcessClaimedQueueItem(context.Background(), storage.QueueItemRecord{ID: queueID, Type: QueueTypeWarn})
	if err != nil {
		t.Fatalf("ProcessClaimedQueueItem() error = %v", err)
	}
	if result == nil || result.Status != "skipped" {
		t.Fatalf("ProcessClaimedQueueItem() = %#v, want skipped result", result)
	}
	if len(fixture.github.createdComments) != 0 {
		t.Fatalf("createdComments = %#v, want no warning comment for a recently updated issue", fixture.github.createdComments)
	}
}

func TestDiscoverIssuesSkipsWhenIssueLaneDisabled(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	fixture.cfg.Roles.Sweeper.Triggers.IncludeIssues = false
	fixture.github.issues = []githubinfra.IssueSummary{{Number: 1, Title: "stale bug", Body: "needs cleanup", Author: "octo"}}

	result, err := fixture.runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	if result.Skipped != 1 || len(result.QueueItems) != 0 {
		t.Fatalf("DiscoverIssues() = %#v, want one skipped result with no queue items", result)
	}
	if fixture.github.listIssuesCalls != 0 {
		t.Fatalf("ListOpenIssues() calls = %d, want 0", fixture.github.listIssuesCalls)
	}
}

func TestDiscoverPullRequestsSkipsWhenPRLaneDisabled(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	fixture.cfg.Roles.Sweeper.Triggers.IncludePullRequests = false
	fixture.github.prs = []githubinfra.PullRequestSummary{{Number: 1, Title: "stale pr", Author: "octo"}}

	result, err := fixture.runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	if result.Skipped != 1 || len(result.QueueItems) != 0 {
		t.Fatalf("DiscoverPullRequests() = %#v, want one skipped result with no queue items", result)
	}
	if fixture.github.listPRCalls != 0 {
		t.Fatalf("ListOpenPullRequests() calls = %d, want 0", fixture.github.listPRCalls)
	}
}

func TestProcessWarnSkipsFreshAbandonedPRCandidates(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	fixture.cfg.Roles.Sweeper.DryRun = false
	fixture.github.prDetails["acme/looper#1"] = githubinfra.PullRequestDetail{Number: 1, Title: "fresh pr", Body: "work in progress", State: "open", UpdatedAt: fixture.now.Add(-24 * time.Hour).Format(time.RFC3339), Author: "octo"}
	queueID := "queue_sweeper_warn_fresh_pr"
	if err := fixture.repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: queueID, ProjectID: &fixture.projectID, Type: QueueTypeWarn, TargetType: "pull_request", TargetID: "acme/looper#1", Repo: stringPtr("acme/looper"), PRNumber: int64Ptr(1), DedupeKey: "sweeper:warn:acme/looper#1", Priority: 1, Status: "running", AvailableAt: fixture.nowISO, MaxAttempts: 3, CreatedAt: fixture.nowISO, UpdatedAt: fixture.nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	result, err := fixture.runner.ProcessClaimedQueueItem(context.Background(), storage.QueueItemRecord{ID: queueID, Type: QueueTypeWarn})
	if err != nil {
		t.Fatalf("ProcessClaimedQueueItem() error = %v", err)
	}
	if result == nil || result.Status != "skipped" {
		t.Fatalf("ProcessClaimedQueueItem() = %#v, want skipped result", result)
	}
	if len(fixture.github.createdComments) != 0 {
		t.Fatalf("createdComments = %#v, want no warning comment for a recently updated pull request", fixture.github.createdComments)
	}
}

func TestDiscoverIssuesHonorsDailyWarnAndCloseLimits(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	fixture.cfg.Roles.Sweeper.Limits.MaxWarningsPerRepoPerDay = 1
	fixture.cfg.Roles.Sweeper.Limits.MaxClosesPerRepoPerDay = 1
	fixture.github.issues = []githubinfra.IssueSummary{
		{Number: 1, Title: "stale bug", Body: "needs cleanup", Author: "octo", Labels: nil},
		{Number: 2, Title: "pending bug", Body: "already warned", Author: "octo", Labels: []string{"looper:sweep-pending"}},
	}
	priorWarn := mustMarshalPayload(sweeperPayload{Phase: "warn", Outcome: outcomePending, Repo: "acme/looper", TargetType: "issue", TargetNumber: 9})
	priorClose := mustMarshalPayload(sweeperPayload{Phase: "close", Outcome: outcomeClosed, Repo: "acme/looper", TargetType: "issue", TargetNumber: 10})
	closeState := mustMarshalPayload(sweeperPayload{Phase: "warn", Outcome: outcomePending, Repo: "acme/looper", TargetType: "issue", TargetNumber: 2, CloseBy: fixture.now.Add(-24 * time.Hour).Format(javaScriptISOStringUTC)})
	for _, item := range []storage.QueueItemRecord{
		{ID: "prior-warn", ProjectID: stringPtr(fixture.projectID), Type: QueueTypeWarn, TargetType: "issue", TargetID: "acme/looper#9", Repo: stringPtr("acme/looper"), DedupeKey: "done:warn:9", Priority: 1, Status: "completed", AvailableAt: fixture.nowISO, PayloadJSON: &priorWarn, MaxAttempts: 3, CreatedAt: fixture.nowISO, UpdatedAt: fixture.nowISO},
		{ID: "prior-close", ProjectID: stringPtr(fixture.projectID), Type: QueueTypeClose, TargetType: "issue", TargetID: "acme/looper#10", Repo: stringPtr("acme/looper"), DedupeKey: "done:close:10", Priority: 1, Status: "completed", AvailableAt: fixture.nowISO, PayloadJSON: &priorClose, MaxAttempts: 3, CreatedAt: fixture.nowISO, UpdatedAt: fixture.nowISO},
		{ID: "close-state", ProjectID: stringPtr(fixture.projectID), Type: QueueTypeWarn, TargetType: "issue", TargetID: "acme/looper#2", Repo: stringPtr("acme/looper"), DedupeKey: "done:warn:2", Priority: 1, Status: "completed", AvailableAt: fixture.nowISO, PayloadJSON: &closeState, MaxAttempts: 3, CreatedAt: fixture.nowISO, UpdatedAt: fixture.nowISO},
	} {
		item := item
		if err := fixture.repos.Queue.Upsert(context.Background(), item); err != nil {
			t.Fatalf("Queue.Upsert() error = %v", err)
		}
	}

	result, err := fixture.runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	if len(result.QueueItems) != 0 {
		t.Fatalf("QueueItems = %#v, want none after daily budgets exhausted", result.QueueItems)
	}
}

func TestProcessWarnPostsWarningAndMarksPending(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	fixture.cfg.Roles.Sweeper.DryRun = false
	fixture.github.issueDetails["acme/looper#42"] = githubinfra.IssueDetail{Number: 42, Title: "Bug", Body: "already fixed by #9", State: "open", Author: "octo", Labels: nil}
	queueID := "queue_sweeper_warn_1"
	if err := fixture.repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: queueID, ProjectID: &fixture.projectID, Type: QueueTypeWarn, TargetType: "issue", TargetID: "acme/looper#42", Repo: stringPtr("acme/looper"), DedupeKey: "sweeper:warn:acme/looper#42", Priority: 1, Status: "running", AvailableAt: fixture.nowISO, MaxAttempts: 3, CreatedAt: fixture.nowISO, UpdatedAt: fixture.nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	result, err := fixture.runner.ProcessClaimedQueueItem(context.Background(), storage.QueueItemRecord{ID: queueID, Type: QueueTypeWarn})
	if err != nil {
		t.Fatalf("ProcessClaimedQueueItem() error = %v", err)
	}
	if result == nil || result.Status != "completed" {
		t.Fatalf("ProcessClaimedQueueItem() = %#v, want completed result", result)
	}
	if len(fixture.github.createdComments) != 1 {
		t.Fatalf("createdComments = %d, want 1", len(fixture.github.createdComments))
	}
	if len(fixture.github.addedLabels["acme/looper#42"]) != 1 || fixture.github.addedLabels["acme/looper#42"][0] != "looper:sweep-pending" {
		t.Fatalf("addedLabels = %#v, want pending label", fixture.github.addedLabels)
	}
	stored, err := fixture.repos.Queue.GetByID(context.Background(), queueID)
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	payload := fixture.runner.readPayload(*stored)
	if payload.Outcome != outcomePending || payload.WarningCommentID == 0 {
		t.Fatalf("payload = %#v, want pending outcome with comment id", payload)
	}
}

func TestProcessCloseClosesAndReconcilesLabels(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	fixture.cfg.Roles.Sweeper.DryRun = false
	payload := sweeperPayload{Phase: "warn", Outcome: outcomePending, Category: categoryAlreadyFixed, Repo: "acme/looper", TargetType: "issue", TargetNumber: 42, WarningCommentID: 99, WarningMarkerUUID: "marker", CommentBody: "warning", PendingLabel: "looper:sweep-pending"}
	payloadJSON := mustMarshalPayload(payload)
	fixture.github.issueDetails["acme/looper#42"] = githubinfra.IssueDetail{Number: 42, Title: "Bug", Body: "already fixed by #9", State: "open", UpdatedAt: fixture.now.Add(-8 * 24 * time.Hour).Format(time.RFC3339), Author: "octo", Labels: []string{"looper:sweep-pending"}}
	queueID := "queue_sweeper_close_1"
	if err := fixture.repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: queueID, ProjectID: &fixture.projectID, Type: QueueTypeClose, TargetType: "issue", TargetID: "acme/looper#42", Repo: stringPtr("acme/looper"), DedupeKey: "sweeper:close:acme/looper#42", Priority: 1, Status: "running", AvailableAt: fixture.nowISO, MaxAttempts: 3, PayloadJSON: &payloadJSON, CreatedAt: fixture.nowISO, UpdatedAt: fixture.nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	result, err := fixture.runner.ProcessClaimedQueueItem(context.Background(), storage.QueueItemRecord{ID: queueID, Type: QueueTypeClose})
	if err != nil {
		t.Fatalf("ProcessClaimedQueueItem() error = %v", err)
	}
	if result == nil || result.Status != "completed" {
		t.Fatalf("ProcessClaimedQueueItem() = %#v, want completed result", result)
	}
	if fixture.github.closedIssues[0].StateReason != "completed" {
		t.Fatalf("closed issue reason = %#v, want completed", fixture.github.closedIssues)
	}
	if !containsString(fixture.github.removedLabels["acme/looper#42"], "looper:sweep-pending") {
		t.Fatalf("removed labels = %#v, want pending removed", fixture.github.removedLabels)
	}
	if !containsString(fixture.github.addedLabels["acme/looper#42"], "looper:swept") {
		t.Fatalf("added labels = %#v, want swept label", fixture.github.addedLabels)
	}
}

func TestProcessCloseRemovesPendingLabelWhenKeepLabelCancelsClose(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	fixture.cfg.Roles.Sweeper.DryRun = false
	payload := sweeperPayload{Phase: "warn", Outcome: outcomePending, Category: categoryStale, Repo: "acme/looper", TargetType: "issue", TargetNumber: 42, WarningCommentID: 99, WarningMarkerUUID: "marker", CommentBody: "warning", PendingLabel: "looper:sweep-pending"}
	payloadJSON := mustMarshalPayload(payload)
	fixture.github.issueDetails["acme/looper#42"] = githubinfra.IssueDetail{Number: 42, Title: "Bug", Body: "stale", State: "open", UpdatedAt: fixture.now.Add(-91 * 24 * time.Hour).Format(time.RFC3339), Author: "octo", Labels: []string{"looper:sweep-pending", fixture.cfg.Roles.Sweeper.Lifecycle.KeepLabel}}
	queueID := "queue_sweeper_close_keep"
	if err := fixture.repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: queueID, ProjectID: &fixture.projectID, Type: QueueTypeClose, TargetType: "issue", TargetID: "acme/looper#42", Repo: stringPtr("acme/looper"), DedupeKey: "sweeper:close:acme/looper#42", Priority: 1, Status: "running", AvailableAt: fixture.nowISO, MaxAttempts: 3, PayloadJSON: &payloadJSON, CreatedAt: fixture.nowISO, UpdatedAt: fixture.nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	result, err := fixture.runner.ProcessClaimedQueueItem(context.Background(), storage.QueueItemRecord{ID: queueID, Type: QueueTypeClose})
	if err != nil {
		t.Fatalf("ProcessClaimedQueueItem() error = %v", err)
	}
	if result == nil || result.Status != "completed" {
		t.Fatalf("ProcessClaimedQueueItem() = %#v, want completed result", result)
	}
	if !containsString(fixture.github.removedLabels["acme/looper#42"], "looper:sweep-pending") {
		t.Fatalf("removed labels = %#v, want pending removed when keep label cancels close", fixture.github.removedLabels)
	}
	if len(fixture.github.closedIssues) != 0 {
		t.Fatalf("closedIssues = %#v, want no close when keep label is present", fixture.github.closedIssues)
	}
}

func TestProcessCloseRemovesPendingLabelWhenTargetAlreadyClosed(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	fixture.cfg.Roles.Sweeper.DryRun = false
	payload := sweeperPayload{Phase: "warn", Outcome: outcomePending, Category: categoryStale, Repo: "acme/looper", TargetType: "issue", TargetNumber: 42, WarningCommentID: 99, WarningMarkerUUID: "marker", CommentBody: "warning", PendingLabel: "looper:sweep-pending"}
	payloadJSON := mustMarshalPayload(payload)
	fixture.github.issueDetails["acme/looper#42"] = githubinfra.IssueDetail{Number: 42, Title: "Bug", Body: "stale", State: "closed", UpdatedAt: fixture.now.Add(-91 * 24 * time.Hour).Format(time.RFC3339), Author: "octo", Labels: []string{"looper:sweep-pending"}}
	queueID := "queue_sweeper_close_already_closed"
	if err := fixture.repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: queueID, ProjectID: &fixture.projectID, Type: QueueTypeClose, TargetType: "issue", TargetID: "acme/looper#42", Repo: stringPtr("acme/looper"), DedupeKey: "sweeper:close:acme/looper#42", Priority: 1, Status: "running", AvailableAt: fixture.nowISO, MaxAttempts: 3, PayloadJSON: &payloadJSON, CreatedAt: fixture.nowISO, UpdatedAt: fixture.nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	result, err := fixture.runner.ProcessClaimedQueueItem(context.Background(), storage.QueueItemRecord{ID: queueID, Type: QueueTypeClose})
	if err != nil {
		t.Fatalf("ProcessClaimedQueueItem() error = %v", err)
	}
	if result == nil || result.Status != "completed" {
		t.Fatalf("ProcessClaimedQueueItem() = %#v, want completed result", result)
	}
	if !containsString(fixture.github.removedLabels["acme/looper#42"], "looper:sweep-pending") {
		t.Fatalf("removed labels = %#v, want pending removed for already closed target", fixture.github.removedLabels)
	}
	if len(fixture.github.closedIssues) != 0 {
		t.Fatalf("closedIssues = %#v, want no close when target is already closed", fixture.github.closedIssues)
	}
}

func TestProcessCloseRemovesPendingLabelWhenReclassificationCancelsClose(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	fixture.cfg.Roles.Sweeper.DryRun = false
	payload := sweeperPayload{Phase: "warn", Outcome: outcomePending, Category: categoryStale, Repo: "acme/looper", TargetType: "issue", TargetNumber: 42, WarningCommentID: 99, WarningMarkerUUID: "marker", CommentBody: "warning", PendingLabel: "looper:sweep-pending"}
	payloadJSON := mustMarshalPayload(payload)
	fixture.github.issueDetails["acme/looper#42"] = githubinfra.IssueDetail{Number: 42, Title: "Bug", Body: "fresh activity", State: "open", UpdatedAt: fixture.now.Format(time.RFC3339), Author: "octo", Labels: []string{"looper:sweep-pending"}}
	queueID := "queue_sweeper_close_reclassified"
	if err := fixture.repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: queueID, ProjectID: &fixture.projectID, Type: QueueTypeClose, TargetType: "issue", TargetID: "acme/looper#42", Repo: stringPtr("acme/looper"), DedupeKey: "sweeper:close:acme/looper#42", Priority: 1, Status: "running", AvailableAt: fixture.nowISO, MaxAttempts: 3, PayloadJSON: &payloadJSON, CreatedAt: fixture.nowISO, UpdatedAt: fixture.nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	result, err := fixture.runner.ProcessClaimedQueueItem(context.Background(), storage.QueueItemRecord{ID: queueID, Type: QueueTypeClose})
	if err != nil {
		t.Fatalf("ProcessClaimedQueueItem() error = %v", err)
	}
	if result == nil || result.Status != "completed" {
		t.Fatalf("ProcessClaimedQueueItem() = %#v, want completed result", result)
	}
	if !containsString(fixture.github.removedLabels["acme/looper#42"], "looper:sweep-pending") {
		t.Fatalf("removed labels = %#v, want pending removed when close is cancelled", fixture.github.removedLabels)
	}
	if len(fixture.github.closedIssues) != 0 {
		t.Fatalf("closedIssues = %#v, want no close when classification changes", fixture.github.closedIssues)
	}
}

func TestProcessReconcileCancelsWhenPendingLabelRemoved(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	fixture.cfg.Roles.Sweeper.DryRun = false
	payloadJSON := mustMarshalPayload(sweeperPayload{Phase: "warn", Outcome: outcomePending, Repo: "acme/looper", TargetType: "issue", TargetNumber: 7, WarningCommentID: 123, CommentBody: "warning"})
	fixture.github.issueDetails["acme/looper#7"] = githubinfra.IssueDetail{Number: 7, Title: "Bug", Body: "stale", State: "open", Author: "octo", Labels: nil}
	queueID := "queue_sweeper_reconcile_1"
	if err := fixture.repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: queueID, ProjectID: &fixture.projectID, Type: QueueTypeReconcile, TargetType: "issue", TargetID: "acme/looper#7", Repo: stringPtr("acme/looper"), DedupeKey: "sweeper:reconcile:acme/looper#7", Priority: 1, Status: "running", AvailableAt: fixture.nowISO, MaxAttempts: 3, PayloadJSON: &payloadJSON, CreatedAt: fixture.nowISO, UpdatedAt: fixture.nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	result, err := fixture.runner.ProcessClaimedQueueItem(context.Background(), storage.QueueItemRecord{ID: queueID, Type: QueueTypeReconcile})
	if err != nil {
		t.Fatalf("ProcessClaimedQueueItem() error = %v", err)
	}
	if result == nil || result.Status != "completed" {
		t.Fatalf("ProcessClaimedQueueItem() = %#v, want completed result", result)
	}
	if len(fixture.github.updatedComments) != 1 || !strings.Contains(fixture.github.updatedComments[0].Body, "pending label was removed") {
		t.Fatalf("updatedComments = %#v, want cancellation note", fixture.github.updatedComments)
	}
}

func TestProcessReconcileKeepsWarnPhaseWhilePendingLabelRemains(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	fixture.cfg.Roles.Sweeper.DryRun = false
	payloadJSON := mustMarshalPayload(sweeperPayload{Phase: "warn", Outcome: outcomePending, Repo: "acme/looper", TargetType: "issue", TargetNumber: 7, WarningCommentID: 123, CommentBody: "warning"})
	fixture.github.issueDetails["acme/looper#7"] = githubinfra.IssueDetail{Number: 7, Title: "Bug", Body: "stale", State: "open", Author: "octo", Labels: []string{"looper:sweep-pending"}}
	queueID := "queue_sweeper_reconcile_pending"
	if err := fixture.repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: queueID, ProjectID: &fixture.projectID, Type: QueueTypeReconcile, TargetType: "issue", TargetID: "acme/looper#7", Repo: stringPtr("acme/looper"), DedupeKey: "sweeper:reconcile:acme/looper#7", Priority: 1, Status: "running", AvailableAt: fixture.nowISO, MaxAttempts: 3, PayloadJSON: &payloadJSON, CreatedAt: fixture.nowISO, UpdatedAt: fixture.nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	result, err := fixture.runner.ProcessClaimedQueueItem(context.Background(), storage.QueueItemRecord{ID: queueID, Type: QueueTypeReconcile})
	if err != nil {
		t.Fatalf("ProcessClaimedQueueItem() error = %v", err)
	}
	if result == nil || result.Status != "skipped" {
		t.Fatalf("ProcessClaimedQueueItem() = %#v, want skipped result", result)
	}
	stored, err := fixture.repos.Queue.GetByID(context.Background(), queueID)
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	if payload := fixture.runner.readPayload(*stored); payload.Phase != "warn" || payload.Outcome != outcomePending {
		t.Fatalf("payload = %#v, want warn phase with pending outcome preserved", payload)
	}
	if len(fixture.github.updatedComments) != 0 {
		t.Fatalf("updatedComments = %#v, want none while pending label remains", fixture.github.updatedComments)
	}
}

func TestDiscoverIssuesSkipsExcludedAuthorAssociations(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	fixture.github.issues = []githubinfra.IssueSummary{{Number: 1, Title: "stale bug", Body: "needs cleanup", Author: "octo", AuthorAssociation: "OWNER"}}

	result, err := fixture.runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	if len(result.QueueItems) != 0 || result.Skipped != 1 {
		t.Fatalf("DiscoverIssues() = %#v, want excluded association to be skipped", result)
	}
}

func TestDiscoverIssuesSkipsReopenedSweptItemWithinCooldown(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	fixture.github.issues = []githubinfra.IssueSummary{{Number: 1, Title: "stale bug", Body: "needs cleanup", Author: "octo", Labels: []string{fixture.cfg.Roles.Sweeper.Lifecycle.ClosedLabel}}}
	closedAt := fixture.now.Add(-10 * 24 * time.Hour).Format(javaScriptISOStringUTC)
	payloadJSON := mustMarshalPayload(sweeperPayload{Phase: "close", Outcome: outcomeClosed, Repo: "acme/looper", TargetType: "issue", TargetNumber: 1, ClosedLabel: fixture.cfg.Roles.Sweeper.Lifecycle.ClosedLabel})
	if err := fixture.repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: "queue_sweeper_closed_recent", ProjectID: &fixture.projectID, Type: QueueTypeClose, TargetType: "issue", TargetID: "acme/looper#1", Repo: stringPtr("acme/looper"), DedupeKey: "sweeper:close:acme/looper#1", Priority: 1, Status: "completed", AvailableAt: closedAt, MaxAttempts: 3, PayloadJSON: &payloadJSON, CreatedAt: closedAt, UpdatedAt: closedAt}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	result, err := fixture.runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	if len(result.QueueItems) != 0 || result.Skipped != 1 {
		t.Fatalf("DiscoverIssues() = %#v, want reopened swept item skipped within cooldown", result)
	}
}

func TestDiscoverIssuesAllowsReopenedSweptItemAfterCooldown(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	fixture.github.issues = []githubinfra.IssueSummary{{Number: 1, Title: "stale bug", Body: "needs cleanup", Author: "octo", Labels: []string{fixture.cfg.Roles.Sweeper.Lifecycle.ClosedLabel}}}
	closedAt := fixture.now.Add(-31 * 24 * time.Hour).Format(javaScriptISOStringUTC)
	payloadJSON := mustMarshalPayload(sweeperPayload{Phase: "close", Outcome: outcomeClosed, Repo: "acme/looper", TargetType: "issue", TargetNumber: 1, ClosedLabel: fixture.cfg.Roles.Sweeper.Lifecycle.ClosedLabel})
	if err := fixture.repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: "queue_sweeper_closed_old", ProjectID: &fixture.projectID, Type: QueueTypeClose, TargetType: "issue", TargetID: "acme/looper#1", Repo: stringPtr("acme/looper"), DedupeKey: "sweeper:close:acme/looper#1", Priority: 1, Status: "completed", AvailableAt: closedAt, MaxAttempts: 3, PayloadJSON: &payloadJSON, CreatedAt: closedAt, UpdatedAt: closedAt}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	result, err := fixture.runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	if len(result.QueueItems) != 1 {
		t.Fatalf("DiscoverIssues() queue items = %#v, want reopened swept item re-queued after cooldown", result.QueueItems)
	}
	if result.QueueItems[0].Type != QueueTypeWarn {
		t.Fatalf("QueueItems[0].Type = %q, want %q", result.QueueItems[0].Type, QueueTypeWarn)
	}
}

func TestProcessClaimedQueueItemRejectsUnsupportedQueueType(t *testing.T) {
	t.Parallel()

	runner := New(Options{})
	result, err := runner.ProcessClaimedQueueItem(context.Background(), storage.QueueItemRecord{ID: "queue_1", Type: "worker"})
	if err == nil {
		t.Fatal("ProcessClaimedQueueItem() error = nil, want unsupported type error")
	}
	if result != nil {
		t.Fatalf("ProcessClaimedQueueItem() result = %#v, want nil on unsupported type", result)
	}
}

type runnerFixture struct {
	repos     *storage.Repositories
	runner    *Runner
	github    *stubGitHub
	cfg       *config.Config
	projectID string
	now       time.Time
	nowISO    string
}

func newRunnerFixture(t *testing.T) runnerFixture {
	t.Helper()
	repos := newTestRepositories(t)
	now := time.Date(2026, time.May, 9, 12, 0, 0, 0, time.UTC)
	nowISO := now.Format(javaScriptISOStringUTC)
	projectID := "demo"
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Demo", RepoPath: filepath.Join(t.TempDir(), "repo"), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Roles.Sweeper.AutoDiscovery = true
	github := &stubGitHub{issueDetails: map[string]githubinfra.IssueDetail{}, prDetails: map[string]githubinfra.PullRequestDetail{}, addedLabels: map[string][]string{}, removedLabels: map[string][]string{}}
	runner := New(Options{Repos: repos, GitHub: github, Now: func() time.Time { return now }, Config: &cfg})
	return runnerFixture{repos: repos, runner: runner, github: github, cfg: &cfg, projectID: projectID, now: now, nowISO: nowISO}
}

func newTestRepositories(t *testing.T) *storage.Repositories {
	t.Helper()
	coordinator, err := storage.OpenSQLiteCoordinator(context.Background(), filepath.Join(t.TempDir(), "looper.sqlite"), storage.SQLiteCoordinatorOptions{Migrations: storage.EmbeddedMigrations})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	t.Cleanup(func() {
		if err := coordinator.Close(); err != nil {
			t.Fatalf("coordinator.Close() error = %v", err)
		}
	})
	if _, err := coordinator.MigrationRunner().RunPending(context.Background()); err != nil {
		t.Fatalf("MigrationRunner().RunPending() error = %v", err)
	}
	return storage.NewRepositories(coordinator.DB())
}

type stubGitHub struct {
	issues          []githubinfra.IssueSummary
	prs             []githubinfra.PullRequestSummary
	issueDetails    map[string]githubinfra.IssueDetail
	prDetails       map[string]githubinfra.PullRequestDetail
	listIssuesCalls int
	listPRCalls     int
	createdComments []githubinfra.IssueCommentInput
	updatedComments []githubinfra.UpdateIssueCommentInput
	closedIssues    []githubinfra.CloseIssueInput
	closedPRs       []githubinfra.ClosePullRequestInput
	addedLabels     map[string][]string
	removedLabels   map[string][]string
}

func (g *stubGitHub) ListOpenIssues(context.Context, githubinfra.ListOpenIssuesInput) ([]githubinfra.IssueSummary, error) {
	g.listIssuesCalls++
	return append([]githubinfra.IssueSummary(nil), g.issues...), nil
}

func (g *stubGitHub) ListOpenPullRequests(context.Context, githubinfra.ListOpenPullRequestsInput) ([]githubinfra.PullRequestSummary, error) {
	g.listPRCalls++
	return append([]githubinfra.PullRequestSummary(nil), g.prs...), nil
}

func (g *stubGitHub) ViewIssue(_ context.Context, input githubinfra.ViewIssueInput) (githubinfra.IssueDetail, error) {
	return g.issueDetails[input.Repo+"#"+itoa(input.IssueNumber)], nil
}

func (g *stubGitHub) ViewPullRequest(_ context.Context, input githubinfra.ViewPullRequestInput) (githubinfra.PullRequestDetail, error) {
	return g.prDetails[input.Repo+"#"+itoa(input.PRNumber)], nil
}

func (g *stubGitHub) CreateIssueComment(_ context.Context, input githubinfra.IssueCommentInput) (githubinfra.IssueCommentResult, error) {
	g.createdComments = append(g.createdComments, input)
	return githubinfra.IssueCommentResult{ID: int64(len(g.createdComments))}, nil
}

func (g *stubGitHub) UpdateIssueComment(_ context.Context, input githubinfra.UpdateIssueCommentInput) error {
	g.updatedComments = append(g.updatedComments, input)
	return nil
}

func (g *stubGitHub) CloseIssue(_ context.Context, input githubinfra.CloseIssueInput) error {
	g.closedIssues = append(g.closedIssues, input)
	return nil
}

func (g *stubGitHub) ClosePullRequest(_ context.Context, input githubinfra.ClosePullRequestInput) error {
	g.closedPRs = append(g.closedPRs, input)
	return nil
}

func (g *stubGitHub) AddIssueLabels(_ context.Context, input githubinfra.IssueLabelsInput) error {
	key := input.Repo + "#" + itoa(input.IssueNumber)
	g.addedLabels[key] = append(g.addedLabels[key], input.Labels...)
	return nil
}

func (g *stubGitHub) RemoveIssueLabels(_ context.Context, input githubinfra.IssueLabelsInput) error {
	key := input.Repo + "#" + itoa(input.IssueNumber)
	g.removedLabels[key] = append(g.removedLabels[key], input.Labels...)
	return nil
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func itoa(value int64) string {
	return strings.TrimSpace(strconv.FormatInt(value, 10))
}

func int64Ptr(value int64) *int64 {
	return &value
}
