package sweeper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
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
	closeDueAt := fixture.now.Add(-24 * time.Hour).Format(javaScriptISOStringUTC)
	if err := fixture.repos.SweeperCases.Upsert(context.Background(), storage.SweeperCaseRecord{ID: "case_issue_2", ProjectID: fixture.projectID, Repo: "acme/looper", TargetType: "issue", TargetNumber: 2, Status: "pending", CurrentPhase: "warn", CloseDueAt: &closeDueAt, CreatedAt: fixture.nowISO, UpdatedAt: fixture.nowISO}); err != nil {
		t.Fatalf("SweeperCases.Upsert() error = %v", err)
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

func TestLoadTargetFailsWhenPRReviewThreadsCannotBeFetched(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	fixture.github.prDetails["acme/looper#42"] = githubinfra.PullRequestDetail{Number: 42, Title: "stale pr", State: "open", UpdatedAt: fixture.nowISO, Author: "octo"}
	fixture.github.reviewThreadsErr = errors.New("review threads unavailable")

	_, err := fixture.runner.loadTarget(context.Background(), storage.QueueItemRecord{Repo: stringPtr("acme/looper"), TargetType: "pull_request", TargetID: "acme/looper#42"})
	if err == nil || !strings.Contains(err.Error(), "review threads unavailable") {
		t.Fatalf("loadTarget() error = %v, want review thread fetch failure", err)
	}
}

func TestDiscoverIssuesSkipsExcludedLabelBeforeAuthorAssociationLookup(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	fixture.cfg.Roles.Sweeper.Triggers.ExcludeLabels = []string{"skip-me"}
	fixture.github.issues = []githubinfra.IssueSummary{{Number: 1, Title: "stale bug", Body: "needs cleanup", Author: "octo", Labels: []string{"skip-me"}}}
	fixture.github.viewIssueErr = errors.New("transient gh failure")

	result, err := fixture.runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	if len(result.QueueItems) != 0 || result.Skipped != 1 {
		t.Fatalf("DiscoverIssues() = %#v, want excluded label to be skipped", result)
	}
	if fixture.github.viewIssueCalls != 0 {
		t.Fatalf("ViewIssue() calls = %d, want 0", fixture.github.viewIssueCalls)
	}
}

func TestDiscoverIssuesSkipsCoordinatorManagedExemptLabels(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		label string
	}{
		{name: "dispatch prefix", label: "dispatch/plan"},
		{name: "needs info", label: "needs-info"},
		{name: "hold", label: "looper:hold"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fixture := newRunnerFixture(t)
			fixture.github.issues = []githubinfra.IssueSummary{{Number: 1, Title: "stale bug", Body: "needs cleanup", Author: "octo", Labels: []string{tc.label}}}

			result, err := fixture.runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "acme/looper"})
			if err != nil {
				t.Fatalf("DiscoverIssues() error = %v", err)
			}
			if len(result.QueueItems) != 0 || result.Skipped != 1 {
				t.Fatalf("DiscoverIssues() = %#v, want exempt label to be skipped", result)
			}
		})
	}
}

func TestHasLabelSupportsPrefixWithoutBreakingExactMatch(t *testing.T) {
	t.Parallel()

	labels := []string{"dispatch/plan", "needs-info", "looper:hold"}
	if !hasLabel(labels, "dispatch/*") {
		t.Fatal("hasLabel(dispatch/*) = false, want true")
	}
	if !hasLabel(labels, "needs-info") {
		t.Fatal("hasLabel(needs-info) = false, want true")
	}
	if hasLabel(labels, "dispatch") {
		t.Fatal("hasLabel(dispatch) = true, want false for non-prefix exact mismatch")
	}
}

func TestDiscoverPullRequestsSkipsExcludedAuthorBeforeAuthorAssociationLookup(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	fixture.cfg.Roles.Sweeper.Triggers.ExcludeAuthors = []string{"octo"}
	fixture.github.prs = []githubinfra.PullRequestSummary{{Number: 1, Title: "stale pr", Author: "octo", UpdatedAt: fixture.now.Add(-40 * 24 * time.Hour).Format(javaScriptISOStringUTC)}}
	fixture.github.viewIssueErr = errors.New("transient gh failure")

	result, err := fixture.runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	if len(result.QueueItems) != 0 || result.Skipped != 1 {
		t.Fatalf("DiscoverPullRequests() = %#v, want excluded author to be skipped", result)
	}
	if fixture.github.viewIssueCalls != 0 {
		t.Fatalf("ViewIssue() calls = %d, want 0", fixture.github.viewIssueCalls)
	}
}

func TestDiscoverIssuesBackfillsAssociationUsingHostQualifiedRepo(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	fixture.cfg.Roles.Sweeper.Triggers.ExcludeAuthorAssociations = []string{"OWNER"}
	fixture.github.issues = []githubinfra.IssueSummary{{Number: 1, Title: "stale bug", Body: "needs cleanup", Author: "octo"}}
	fixture.github.issueDetails["github.example.com/acme/looper#1"] = githubinfra.IssueDetail{Number: 1, AuthorAssociation: "OWNER"}

	result, err := fixture.runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "github.example.com/acme/looper"})
	if err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	if len(result.QueueItems) != 0 || result.Skipped != 1 {
		t.Fatalf("DiscoverIssues() = %#v, want excluded owner issue to be skipped", result)
	}
	if fixture.github.viewIssueCalls != 1 {
		t.Fatalf("ViewIssue() calls = %d, want 1", fixture.github.viewIssueCalls)
	}
	if got := fixture.github.viewIssueRepos; len(got) != 1 || got[0] != "github.example.com/acme/looper" {
		t.Fatalf("ViewIssue() repos = %v, want [github.example.com/acme/looper]", got)
	}
	if got := fixture.github.viewIssueCWDs; len(got) != 1 || got[0] == "" {
		t.Fatalf("ViewIssue() CWDs = %v, want project repo path", got)
	}
}

func TestDiscoverPullRequestsBackfillsAssociationUsingHostQualifiedRepo(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	fixture.cfg.Roles.Sweeper.Triggers.ExcludeAuthorAssociations = []string{"MEMBER"}
	fixture.github.prs = []githubinfra.PullRequestSummary{{Number: 1, Title: "stale pr", Author: "octo", UpdatedAt: fixture.now.Add(-40 * 24 * time.Hour).Format(javaScriptISOStringUTC)}}
	fixture.github.issueDetails["github.example.com/acme/looper#1"] = githubinfra.IssueDetail{Number: 1, AuthorAssociation: "MEMBER"}

	result, err := fixture.runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "github.example.com/acme/looper"})
	if err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	if len(result.QueueItems) != 0 || result.Skipped != 1 {
		t.Fatalf("DiscoverPullRequests() = %#v, want excluded member PR to be skipped", result)
	}
	if fixture.github.viewIssueCalls != 1 {
		t.Fatalf("ViewIssue() calls = %d, want 1", fixture.github.viewIssueCalls)
	}
	if got := fixture.github.viewIssueRepos; len(got) != 1 || got[0] != "github.example.com/acme/looper" {
		t.Fatalf("ViewIssue() repos = %v, want [github.example.com/acme/looper]", got)
	}
	if got := fixture.github.viewIssueCWDs; len(got) != 1 || got[0] == "" {
		t.Fatalf("ViewIssue() CWDs = %v, want project repo path", got)
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
	for _, record := range []storage.SweeperCaseRecord{
		{ID: "case_warn", ProjectID: fixture.projectID, Repo: "acme/looper", TargetType: "issue", TargetNumber: 9, Status: "pending", CurrentPhase: "warn", CreatedAt: fixture.nowISO, UpdatedAt: fixture.nowISO},
		{ID: "case_close", ProjectID: fixture.projectID, Repo: "acme/looper", TargetType: "issue", TargetNumber: 10, Status: "terminal", CurrentPhase: "terminal", CreatedAt: fixture.nowISO, UpdatedAt: fixture.nowISO},
		{ID: "case_pending", ProjectID: fixture.projectID, Repo: "acme/looper", TargetType: "issue", TargetNumber: 2, Status: "pending", CurrentPhase: "warn", CloseDueAt: stringPtr(fixture.now.Add(-24 * time.Hour).Format(javaScriptISOStringUTC)), CreatedAt: fixture.nowISO, UpdatedAt: fixture.nowISO},
	} {
		record := record
		if err := fixture.repos.SweeperCases.Upsert(context.Background(), record); err != nil {
			t.Fatalf("SweeperCases.Upsert() error = %v", err)
		}
	}
	validation := "passed"
	for _, proposal := range []storage.SweeperProposalRecord{
		{ID: "proposal_warn_done", CaseID: "case_warn", ProjectID: fixture.projectID, Repo: "acme/looper", TargetType: "issue", TargetNumber: 9, SchemaVersion: 2, ProposerKind: "heuristic_v1", FactBundleJSON: "{}", FingerprintJSON: `{"hash":"warn"}`, ProposalJSON: `{"decision":"warn"}`, Decision: "warn", Category: "stale", ConfidenceScore: 80, ValidationStatus: &validation, ApplyStatus: stringPtr("completed_warned"), AppliedAt: &fixture.nowISO, CreatedAt: fixture.nowISO},
		{ID: "proposal_close_done", CaseID: "case_close", ProjectID: fixture.projectID, Repo: "acme/looper", TargetType: "issue", TargetNumber: 10, SchemaVersion: 2, ProposerKind: "heuristic_v1", FactBundleJSON: "{}", FingerprintJSON: `{"hash":"close"}`, ProposalJSON: `{"decision":"close"}`, Decision: "close", Category: "stale", ConfidenceScore: 80, ValidationStatus: &validation, ApplyStatus: stringPtr("completed_closed"), AppliedAt: &fixture.nowISO, CreatedAt: fixture.nowISO},
		{ID: "proposal_close_inflight", CaseID: "case_pending", ProjectID: fixture.projectID, Repo: "acme/looper", TargetType: "issue", TargetNumber: 2, SchemaVersion: 2, ProposerKind: "heuristic_v1", FactBundleJSON: "{}", FingerprintJSON: `{"hash":"pending"}`, ProposalJSON: `{"decision":"close"}`, Decision: "close", Category: "stale", ConfidenceScore: 80, ValidationStatus: &validation, ApplyStatus: stringPtr("partial:commented"), CreatedAt: fixture.nowISO},
	} {
		proposal := proposal
		if err := fixture.repos.SweeperProposals.Insert(context.Background(), proposal); err != nil {
			t.Fatalf("SweeperProposals.Insert() error = %v", err)
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

func TestDiscoverReconcileBuildsQueueItemsFromSweeperCases(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	category := categoryStale
	confidence := int64(80)
	warningCommentID := int64(123)
	closeDueAt := fixture.now.Add(-2 * time.Hour).Format(javaScriptISOStringUTC)
	warnedAt := fixture.now.Add(-8 * 24 * time.Hour).Format(javaScriptISOStringUTC)
	if err := fixture.repos.SweeperCases.Upsert(context.Background(), storage.SweeperCaseRecord{ID: "case_reconcile", ProjectID: fixture.projectID, Repo: "acme/looper", TargetType: "issue", TargetNumber: 7, Status: "pending", CurrentPhase: "warn", CurrentCategory: &category, CurrentConfidenceScore: &confidence, WarningCommentID: &warningCommentID, WarnedAt: &warnedAt, CloseDueAt: &closeDueAt, CreatedAt: fixture.nowISO, UpdatedAt: fixture.nowISO}); err != nil {
		t.Fatalf("SweeperCases.Upsert() error = %v", err)
	}

	result, err := fixture.runner.DiscoverReconcile(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("DiscoverReconcile() error = %v", err)
	}
	if len(result.QueueItems) != 1 || result.QueueItems[0].Type != QueueTypeReconcile {
		t.Fatalf("DiscoverReconcile() = %#v, want one reconcile queue item", result)
	}
	payload := fixture.runner.readPayload(result.QueueItems[0])
	if payload.CaseID != "case_reconcile" || payload.ProposalID != "" || payload.WarningCommentID != 0 || payload.CloseBy != "" || payload.Outcome != "" {
		t.Fatalf("payload = %#v, want sweeper case derived reconcile payload", payload)
	}
}

func TestDiscoverMaintenanceReconcileIgnoresAutoDiscoveryFlag(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	fixture.cfg.Roles.Sweeper.AutoDiscovery = false
	if err := fixture.repos.SweeperCases.Upsert(context.Background(), storage.SweeperCaseRecord{ID: "case_maint", ProjectID: fixture.projectID, Repo: "acme/looper", TargetType: "issue", TargetNumber: 8, Status: "pending", CurrentPhase: "warn", CreatedAt: fixture.nowISO, UpdatedAt: fixture.nowISO}); err != nil {
		t.Fatalf("SweeperCases.Upsert() error = %v", err)
	}

	result, err := fixture.runner.DiscoverMaintenanceReconcile(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("DiscoverMaintenanceReconcile() error = %v", err)
	}
	if len(result.QueueItems) != 1 || result.QueueItems[0].Type != QueueTypeReconcile {
		t.Fatalf("DiscoverMaintenanceReconcile() = %#v, want one reconcile queue item despite auto-discovery off", result)
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
	if payload.CaseID == "" || payload.ProposalID == "" || payload.WarningCommentID != 0 || payload.Outcome != "" {
		t.Fatalf("payload = %#v, want lean persisted queue metadata", payload)
	}
	caseRecord, err := fixture.repos.SweeperCases.GetByProjectRepoTarget(context.Background(), fixture.projectID, "acme/looper", "issue", 42)
	if err != nil {
		t.Fatalf("SweeperCases.GetByProjectRepoTarget() error = %v", err)
	}
	if caseRecord == nil || caseRecord.Status != "pending" || caseRecord.LastProposalID == nil || *caseRecord.LastProposalID == "" {
		t.Fatalf("caseRecord = %#v, want pending case with latest proposal", caseRecord)
	}
	proposal, err := fixture.repos.SweeperProposals.GetByID(context.Background(), *caseRecord.LastProposalID)
	if err != nil {
		t.Fatalf("SweeperProposals.GetByID() error = %v", err)
	}
	if proposal == nil || proposal.ApplyStatus == nil || *proposal.ApplyStatus != "completed_warned" || proposal.Decision != "warn" {
		t.Fatalf("proposal = %#v, want completed warn proposal", proposal)
	}
}

func TestProcessWarnSetsRepoBeforeEnsuringCase(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	fixture.cfg.Roles.Sweeper.DryRun = true
	queueItem := storage.QueueItemRecord{ProjectID: &fixture.projectID, Type: QueueTypeWarn, TargetType: "issue", TargetID: "acme/looper#142", Repo: stringPtr("acme/looper")}
	fixture.github.issueDetails["acme/looper#142"] = githubinfra.IssueDetail{Number: 142, Title: "Bug", Body: "already fixed by #9", State: "open", UpdatedAt: fixture.now.Add(-91 * 24 * time.Hour).Format(time.RFC3339), Author: "octo"}

	payload, status, _, err := fixture.runner.processWarn(context.Background(), queueItem, sweeperPayload{})
	if err != nil {
		t.Fatalf("processWarn() error = %v", err)
	}
	if status != "skipped" {
		t.Fatalf("processWarn() status = %q, want skipped", status)
	}
	caseRecord, err := fixture.repos.SweeperCases.GetByProjectRepoTarget(context.Background(), fixture.projectID, "acme/looper", "issue", 142)
	if err != nil {
		t.Fatalf("SweeperCases.GetByProjectRepoTarget() error = %v", err)
	}
	if caseRecord == nil {
		t.Fatal("caseRecord = nil, want repo-scoped case")
	}
	proposal, err := fixture.repos.SweeperProposals.GetByID(context.Background(), payload.ProposalID)
	if err != nil {
		t.Fatalf("SweeperProposals.GetByID() error = %v", err)
	}
	if proposal == nil || proposal.Repo != "acme/looper" {
		t.Fatalf("proposal = %#v, want repo-scoped proposal", proposal)
	}
	bundle, err := parseFactBundle(proposal.FactBundleJSON)
	if err != nil {
		t.Fatalf("parseFactBundle() error = %v", err)
	}
	if bundle.Repo != "acme/looper" {
		t.Fatalf("bundle.Repo = %q, want acme/looper", bundle.Repo)
	}
}

func TestProcessWarnKeepsUnrelatedDryRunOnly(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	fixture.cfg.Roles.Sweeper.DryRun = false
	fixture.cfg.Roles.Sweeper.Categories.Unrelated.Enabled = true
	fixture.github.issueDetails["acme/looper#63"] = githubinfra.IssueDetail{Number: 63, Title: "Support question", Body: "question about setup", State: "open", UpdatedAt: fixture.now.Add(-100 * 24 * time.Hour).Format(time.RFC3339), Author: "octo"}
	queueID := "queue_sweeper_warn_unrelated_63"
	if err := fixture.repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: queueID, ProjectID: &fixture.projectID, Type: QueueTypeWarn, TargetType: "issue", TargetID: "acme/looper#63", Repo: stringPtr("acme/looper"), DedupeKey: "sweeper:warn:acme/looper#63", Priority: 1, Status: "running", AvailableAt: fixture.nowISO, MaxAttempts: 3, CreatedAt: fixture.nowISO, UpdatedAt: fixture.nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	result, err := fixture.runner.ProcessClaimedQueueItem(context.Background(), storage.QueueItemRecord{ID: queueID, Type: QueueTypeWarn})
	if err != nil {
		t.Fatalf("ProcessClaimedQueueItem() error = %v", err)
	}
	if result == nil || result.Status != "skipped" || result.Summary != "sweeper dry-run warning" {
		t.Fatalf("ProcessClaimedQueueItem() = %#v, want unrelated dry-run skip", result)
	}
	if len(fixture.github.createdComments) != 0 {
		t.Fatalf("createdComments = %#v, want no live warning for unrelated category", fixture.github.createdComments)
	}
	caseRecord, err := fixture.repos.SweeperCases.GetByProjectRepoTarget(context.Background(), fixture.projectID, "acme/looper", "issue", 63)
	if err != nil {
		t.Fatalf("SweeperCases.GetByProjectRepoTarget() error = %v", err)
	}
	if caseRecord == nil || caseRecord.CurrentCategory == nil || *caseRecord.CurrentCategory != categoryUnrelated || caseRecord.Status != "open" {
		t.Fatalf("caseRecord = %#v, want unrelated open case", caseRecord)
	}
	proposal, err := fixture.repos.SweeperProposals.GetByID(context.Background(), *caseRecord.LastProposalID)
	if err != nil {
		t.Fatalf("SweeperProposals.GetByID() error = %v", err)
	}
	if proposal == nil || proposal.ApplyStatus == nil || *proposal.ApplyStatus != "skipped_dry_run" {
		t.Fatalf("proposal = %#v, want skipped dry-run apply receipt", proposal)
	}
}

func TestProcessWarnKeepsRouteSecurityDryRunOnlyInHeuristicMode(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	fixture.cfg.Roles.Sweeper.DryRun = false
	fixture.cfg.Roles.Sweeper.Proposer.Mode = config.SweeperProposerModeHeuristicFallback
	fixture.github.issueDetails["acme/looper#65"] = githubinfra.IssueDetail{Number: 65, Title: "Security issue", Body: "security incident details", State: "open", UpdatedAt: fixture.now.Add(-100 * 24 * time.Hour).Format(time.RFC3339), Author: "octo"}
	queueID := "queue_sweeper_warn_route_security_65"
	if err := fixture.repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: queueID, ProjectID: &fixture.projectID, Type: QueueTypeWarn, TargetType: "issue", TargetID: "acme/looper#65", Repo: stringPtr("acme/looper"), DedupeKey: "sweeper:warn:acme/looper#65", Priority: 1, Status: "running", AvailableAt: fixture.nowISO, MaxAttempts: 3, CreatedAt: fixture.nowISO, UpdatedAt: fixture.nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	result, err := fixture.runner.ProcessClaimedQueueItem(context.Background(), storage.QueueItemRecord{ID: queueID, Type: QueueTypeWarn})
	if err != nil {
		t.Fatalf("ProcessClaimedQueueItem() error = %v", err)
	}
	if result == nil || result.Status != "skipped" || result.Summary != "sweeper dry-run quarantine" {
		t.Fatalf("ProcessClaimedQueueItem() = %#v, want route_security dry-run skip", result)
	}
	if got := fixture.github.addedLabels["acme/looper#65"]; len(got) != 0 {
		t.Fatalf("addedLabels = %#v, want no live quarantine label application", fixture.github.addedLabels)
	}
	caseRecord, err := fixture.repos.SweeperCases.GetByProjectRepoTarget(context.Background(), fixture.projectID, "acme/looper", "issue", 65)
	if err != nil {
		t.Fatalf("SweeperCases.GetByProjectRepoTarget() error = %v", err)
	}
	if caseRecord == nil || caseRecord.CurrentCategory == nil || *caseRecord.CurrentCategory != categoryRouteSecurity {
		t.Fatalf("caseRecord = %#v, want route_security case record", caseRecord)
	}
}

func TestProcessWarnDoesNotRouteGenericSecurityText(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	fixture.cfg.Roles.Sweeper.DryRun = false
	fixture.cfg.Roles.Sweeper.Proposer.Mode = config.SweeperProposerModeHeuristicFallback
	fixture.github.issueDetails["acme/looper#66"] = githubinfra.IssueDetail{Number: 66, Title: "Security policy docs update", Body: "Please refresh the security policy and docs for the next release.", State: "open", UpdatedAt: fixture.now.Add(-100 * 24 * time.Hour).Format(time.RFC3339), Author: "octo"}
	queueID := "queue_sweeper_warn_generic_security_66"
	if err := fixture.repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: queueID, ProjectID: &fixture.projectID, Type: QueueTypeWarn, TargetType: "issue", TargetID: "acme/looper#66", Repo: stringPtr("acme/looper"), DedupeKey: "sweeper:warn:acme/looper#66", Priority: 1, Status: "running", AvailableAt: fixture.nowISO, MaxAttempts: 3, CreatedAt: fixture.nowISO, UpdatedAt: fixture.nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	result, err := fixture.runner.ProcessClaimedQueueItem(context.Background(), storage.QueueItemRecord{ID: queueID, Type: QueueTypeWarn})
	if err != nil {
		t.Fatalf("ProcessClaimedQueueItem() error = %v", err)
	}
	if result == nil || result.Status != "completed" || result.Summary != "sweeper warned issue #66" {
		t.Fatalf("ProcessClaimedQueueItem() = %#v, want normal warning flow for generic security text", result)
	}
	if got := fixture.github.addedLabels["acme/looper#66"]; !containsString(got, "looper:sweep-pending") {
		t.Fatalf("addedLabels = %#v, want pending label not quarantine", fixture.github.addedLabels)
	}
	if got := fixture.github.addedLabels["acme/looper#66"]; containsString(got, fixture.cfg.Roles.Sweeper.Security.QuarantineLabel) {
		t.Fatalf("addedLabels = %#v, want no quarantine label for generic security text", fixture.github.addedLabels)
	}
	caseRecord, err := fixture.repos.SweeperCases.GetByProjectRepoTarget(context.Background(), fixture.projectID, "acme/looper", "issue", 66)
	if err != nil {
		t.Fatalf("SweeperCases.GetByProjectRepoTarget() error = %v", err)
	}
	if caseRecord == nil || caseRecord.CurrentCategory == nil || *caseRecord.CurrentCategory == categoryRouteSecurity {
		t.Fatalf("caseRecord = %#v, want non-route_security case record", caseRecord)
	}
}

func TestProcessWarnAgentApplyUsesAgentProposalAndPersistsRawResult(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	fixture.cfg.Roles.Sweeper.DryRun = false
	fixture.cfg.Roles.Sweeper.Proposer.Mode = config.SweeperProposerModeAgentApply
	fixture.github.issueDetails["acme/looper#61"] = githubinfra.IssueDetail{Number: 61, Title: "Stale bug", Body: "needs cleanup", State: "open", UpdatedAt: fixture.now.Add(-100 * 24 * time.Hour).Format(time.RFC3339), Author: "octo"}
	fixture.agent.results = []AgentResult{{Status: "completed", Stdout: `{"schemaVersion":2,"decision":"warn","category":"stale","confidenceScore":88,"summary":"agent warning","rationale":"agent determined stale inactivity","markerUUID":"marker-agent-61"}`}}
	queueID := "queue_sweeper_warn_agent_61"
	if err := fixture.repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: queueID, ProjectID: &fixture.projectID, Type: QueueTypeWarn, TargetType: "issue", TargetID: "acme/looper#61", Repo: stringPtr("acme/looper"), DedupeKey: "sweeper:warn:acme/looper#61", Priority: 1, Status: "running", AvailableAt: fixture.nowISO, MaxAttempts: 3, CreatedAt: fixture.nowISO, UpdatedAt: fixture.nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	result, err := fixture.runner.ProcessClaimedQueueItem(context.Background(), storage.QueueItemRecord{ID: queueID, Type: QueueTypeWarn})
	if err != nil {
		t.Fatalf("ProcessClaimedQueueItem() error = %v", err)
	}
	if result == nil || result.Status != "completed" {
		t.Fatalf("ProcessClaimedQueueItem() = %#v, want completed result", result)
	}
	if len(fixture.agent.calls) != 1 {
		t.Fatalf("agent calls = %d, want 1", len(fixture.agent.calls))
	}
	caseRecord, err := fixture.repos.SweeperCases.GetByProjectRepoTarget(context.Background(), fixture.projectID, "acme/looper", "issue", 61)
	if err != nil || caseRecord == nil || caseRecord.LastProposalID == nil {
		t.Fatalf("GetByProjectRepoTarget() = %#v, %v, want case with proposal", caseRecord, err)
	}
	proposals, err := fixture.repos.SweeperProposals.ListByCaseID(context.Background(), caseRecord.ID)
	if err != nil {
		t.Fatalf("ListByCaseID() error = %v", err)
	}
	if len(proposals) != 2 {
		t.Fatalf("len(proposals) = %d, want heuristic shadow + agent proposal", len(proposals))
	}
	if caseRecord.LastProposalID == nil {
		t.Fatalf("caseRecord = %#v, want last proposal id", caseRecord)
	}
	latest, err := fixture.repos.SweeperProposals.GetByID(context.Background(), *caseRecord.LastProposalID)
	if err != nil {
		t.Fatalf("GetByID() error = %v", err)
	}
	if latest == nil || latest.ProposerKind != proposerKindAgentV1 || latest.RawResultJSON == nil {
		t.Fatalf("latest proposal = %#v, want agent proposal with raw result", latest)
	}
	if len(fixture.github.createdComments) != 1 || !strings.Contains(fixture.github.createdComments[0].Body, "agent determined stale inactivity") || !strings.Contains(fixture.github.createdComments[0].Body, "marker-agent-61") {
		t.Fatalf("createdComments = %#v, want agent rationale and marker in warning comment", fixture.github.createdComments)
	}
}

func TestProcessWarnAgentApplyRetryReusesExistingAgentProposal(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	fixture.cfg.Roles.Sweeper.DryRun = false
	fixture.cfg.Roles.Sweeper.Proposer.Mode = config.SweeperProposerModeAgentApply
	fixture.github.issueDetails["acme/looper#62"] = githubinfra.IssueDetail{Number: 62, Title: "Stale bug", Body: "needs cleanup", State: "open", UpdatedAt: fixture.now.Add(-100 * 24 * time.Hour).Format(time.RFC3339), Author: "octo"}
	fixture.agent.results = []AgentResult{{Status: "completed", Stdout: `{"schemaVersion":2,"decision":"warn","category":"stale","confidenceScore":88,"summary":"agent warning","rationale":"agent determined stale inactivity","markerUUID":"marker-agent-62"}`}}
	fixture.github.addIssueLabelsErr = errors.New("temporary label failure")
	queueID := "queue_sweeper_warn_agent_62"
	if err := fixture.repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: queueID, ProjectID: &fixture.projectID, Type: QueueTypeWarn, TargetType: "issue", TargetID: "acme/looper#62", Repo: stringPtr("acme/looper"), DedupeKey: "sweeper:warn:acme/looper#62", Priority: 1, Status: "running", AvailableAt: fixture.nowISO, MaxAttempts: 3, CreatedAt: fixture.nowISO, UpdatedAt: fixture.nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	result, err := fixture.runner.ProcessClaimedQueueItem(context.Background(), storage.QueueItemRecord{ID: queueID, Type: QueueTypeWarn})
	if err != nil {
		t.Fatalf("first ProcessClaimedQueueItem() error = %v, want recovered failed result", err)
	}
	if result == nil || result.Status != "failed" {
		t.Fatalf("first ProcessClaimedQueueItem() = %#v, want failed result", result)
	}
	if len(fixture.agent.calls) != 1 {
		t.Fatalf("agent calls after first attempt = %d, want 1", len(fixture.agent.calls))
	}
	stored, err := fixture.repos.Queue.GetByID(context.Background(), queueID)
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	if stored == nil || stored.Status != "queued" || stored.Attempts != 1 || stored.LastErrorKind == nil || *stored.LastErrorKind != "retryable_transient" {
		t.Fatalf("stored queue item = %#v, want queued retry after first failure", stored)
	}
	if stored.AvailableAt == fixture.nowISO {
		t.Fatalf("stored queue item available_at = %q, want retry backoff", stored.AvailableAt)
	}
	fixture.github.issueDetails["acme/looper#62"] = githubinfra.IssueDetail{Number: 62, Title: "Stale bug", Body: "needs cleanup", State: "open", UpdatedAt: fixture.now.Add(-100 * 24 * time.Hour).Format(time.RFC3339), Author: "octo", Comments: []githubinfra.CommentInfo{{ID: 1, Body: fixture.github.createdComments[0].Body}}}
	fixture.github.addIssueLabelsErr = nil

	result, err = fixture.runner.ProcessClaimedQueueItem(context.Background(), storage.QueueItemRecord{ID: queueID, Type: QueueTypeWarn})
	if err != nil {
		t.Fatalf("retry ProcessClaimedQueueItem() error = %v", err)
	}
	if result == nil || result.Status != "completed" {
		t.Fatalf("retry ProcessClaimedQueueItem() = %#v, want completed result", result)
	}
	if len(fixture.agent.calls) != 1 {
		t.Fatalf("agent calls after retry = %d, want 1", len(fixture.agent.calls))
	}
	if len(fixture.github.createdComments) != 1 {
		t.Fatalf("createdComments = %#v, want single warning comment across retry", fixture.github.createdComments)
	}
}

func TestReplayCaseProposalDryRunPersistsValidatedAgentProposalWithoutMutatingCase(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	fixture.cfg.Roles.Sweeper.Proposer.Mode = config.SweeperProposerModeAgentApply
	caseRecord := seedReplayCase(t, fixture, storage.SweeperCaseRecord{
		ID:           "case_replay_agent",
		ProjectID:    fixture.projectID,
		Repo:         "acme/looper",
		TargetType:   "issue",
		TargetNumber: 71,
		Status:       "pending",
		CurrentPhase: "warn",
		CreatedAt:    fixture.nowISO,
		UpdatedAt:    fixture.nowISO,
	}, seedReplayProposalInput{
		proposalID:     "proposal_replay_base_agent",
		decision:       "warn",
		category:       categoryStale,
		proposerKind:   "heuristic_v1",
		factBundle:     replayFactBundle("acme/looper", "issue", 71, fixture.now.Add(-100*24*time.Hour)),
		createdAt:      fixture.now.Add(-time.Hour).Format(javaScriptISOStringUTC),
		lastProposalID: true,
	})
	fixture.agent.results = []AgentResult{{Status: "completed", Stdout: `{"schemaVersion":2,"decision":"warn","category":"stale","confidenceScore":88,"summary":"agent replay warning","rationale":"agent replay rationale","markerUUID":"marker-replay-71"}`}}

	proposal, err := fixture.runner.ReplayCaseProposalDryRun(context.Background(), caseRecord.ID)
	if err != nil {
		t.Fatalf("ReplayCaseProposalDryRun() error = %v", err)
	}
	if proposal == nil || proposal.ProposerKind != proposerKindAgentV1 || proposal.ValidationStatus == nil || *proposal.ValidationStatus != "passed" || proposal.RawResultJSON == nil {
		t.Fatalf("ReplayCaseProposalDryRun() = %#v, want persisted validated agent proposal", proposal)
	}
	if proposal.CaseID != caseRecord.ID || proposal.Decision != "warn" {
		t.Fatalf("replay proposal = %#v, want warn proposal for same case", proposal)
	}
	if len(fixture.agent.calls) != 1 {
		t.Fatalf("agent calls = %d, want 1", len(fixture.agent.calls))
	}

	persistedCase, err := fixture.repos.SweeperCases.GetByID(context.Background(), caseRecord.ID)
	if err != nil {
		t.Fatalf("SweeperCases.GetByID() error = %v", err)
	}
	if persistedCase == nil || persistedCase.CurrentPhase != "warn" || persistedCase.Status != "pending" || persistedCase.LastProposalID == nil || *persistedCase.LastProposalID != "proposal_replay_base_agent" {
		t.Fatalf("persisted case = %#v, want unchanged case state and last proposal id", persistedCase)
	}

	proposals, err := fixture.repos.SweeperProposals.ListByCaseID(context.Background(), caseRecord.ID)
	if err != nil {
		t.Fatalf("SweeperProposals.ListByCaseID() error = %v", err)
	}
	if len(proposals) != 2 || proposals[0].ID != proposal.ID || proposals[1].ID != "proposal_replay_base_agent" {
		t.Fatalf("ListByCaseID() = %#v, want new replay proposal followed by base proposal", proposals)
	}
}

func TestProcessWarnWritesDurableMarkdownReport(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	reportDir := t.TempDir()
	fixture.cfg.Roles.Sweeper.Reporting.DurableReportsDir = reportDir
	fixture.cfg.Roles.Sweeper.DryRun = false
	fixture.github.issueDetails["acme/looper#64"] = githubinfra.IssueDetail{Number: 64, Title: "Bug", Body: "already fixed by #9", State: "open", Author: "octo", Labels: nil}
	queueID := "queue_sweeper_warn_report_64"
	if err := fixture.repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: queueID, ProjectID: &fixture.projectID, Type: QueueTypeWarn, TargetType: "issue", TargetID: "acme/looper#64", Repo: stringPtr("acme/looper"), DedupeKey: "sweeper:warn:acme/looper#64", Priority: 1, Status: "running", AvailableAt: fixture.nowISO, MaxAttempts: 3, CreatedAt: fixture.nowISO, UpdatedAt: fixture.nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	if _, err := fixture.runner.ProcessClaimedQueueItem(context.Background(), storage.QueueItemRecord{ID: queueID, Type: QueueTypeWarn}); err != nil {
		t.Fatalf("ProcessClaimedQueueItem() error = %v", err)
	}
	reportPath := filepath.Join(reportDir, fixture.projectID, "acme", "looper", "issue-64.md")
	reportBytes, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatalf("os.ReadFile(%q) error = %v", reportPath, err)
	}
	report := string(reportBytes)
	if !strings.Contains(report, "# Sweeper Report") || !strings.Contains(report, "Repo: acme/looper") || !strings.Contains(report, "Apply status: completed_warned") || !strings.Contains(report, "already_fixed") {
		t.Fatalf("report = %q, want durable markdown summary", report)
	}
}

func TestReplayCaseProposalDryRunWritesDurableMarkdownReport(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	reportDir := t.TempDir()
	fixture.cfg.Roles.Sweeper.Reporting.DurableReportsDir = reportDir
	fixture.cfg.Roles.Sweeper.Proposer.Mode = config.SweeperProposerModeAgentApply
	caseRecord := seedReplayCase(t, fixture, storage.SweeperCaseRecord{
		ID:           "case_replay_report",
		ProjectID:    fixture.projectID,
		Repo:         "acme/looper",
		TargetType:   "issue",
		TargetNumber: 74,
		Status:       "pending",
		CurrentPhase: "warn",
		CreatedAt:    fixture.nowISO,
		UpdatedAt:    fixture.nowISO,
	}, seedReplayProposalInput{
		proposalID:     "proposal_replay_base_report",
		decision:       "warn",
		category:       categoryStale,
		proposerKind:   "heuristic_v1",
		factBundle:     replayFactBundle("acme/looper", "issue", 74, fixture.now.Add(-100*24*time.Hour)),
		createdAt:      fixture.now.Add(-time.Hour).Format(javaScriptISOStringUTC),
		lastProposalID: true,
	})
	fixture.agent.results = []AgentResult{{Status: "completed", Stdout: `{"schemaVersion":2,"decision":"warn","category":"stale","confidenceScore":88,"summary":"agent replay warning","rationale":"agent replay rationale","markerUUID":"marker-replay-74"}`}}

	proposal, err := fixture.runner.ReplayCaseProposalDryRun(context.Background(), caseRecord.ID)
	if err != nil {
		t.Fatalf("ReplayCaseProposalDryRun() error = %v", err)
	}
	reportPath := filepath.Join(reportDir, fixture.projectID, "acme", "looper", "issue-74.md")
	reportBytes, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatalf("os.ReadFile(%q) error = %v", reportPath, err)
	}
	report := string(reportBytes)
	if proposal == nil || !strings.Contains(report, "Proposal ID: "+proposal.ID) || !strings.Contains(report, "Validation status: passed") || !strings.Contains(report, "agent replay rationale") {
		t.Fatalf("report = %q, want replay proposal durable report", report)
	}
}

func TestReplayCaseProposalDryRunFallsBackToHeuristicProposal(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		mode      config.SweeperProposerMode
		dropAgent bool
	}{
		{name: "heuristic fallback mode", mode: config.SweeperProposerModeHeuristicFallback},
		{name: "missing agent", mode: config.SweeperProposerModeAgentApply, dropAgent: true},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			fixture := newRunnerFixture(t)
			fixture.cfg.Roles.Sweeper.Proposer.Mode = tc.mode
			if tc.dropAgent {
				fixture.runner.agent = nil
			}
			caseRecord := seedReplayCase(t, fixture, storage.SweeperCaseRecord{
				ID:           "case_replay_heuristic_" + strings.ReplaceAll(tc.name, " ", "_"),
				ProjectID:    fixture.projectID,
				Repo:         "acme/looper",
				TargetType:   "issue",
				TargetNumber: 72,
				Status:       "pending",
				CurrentPhase: "warn",
				CreatedAt:    fixture.nowISO,
				UpdatedAt:    fixture.nowISO,
			}, seedReplayProposalInput{
				proposalID:     "proposal_replay_base_heuristic",
				decision:       "warn",
				category:       categoryStale,
				proposerKind:   "heuristic_v1",
				factBundle:     replayFactBundle("acme/looper", "issue", 72, fixture.now.Add(-120*24*time.Hour)),
				createdAt:      fixture.now.Add(-time.Hour).Format(javaScriptISOStringUTC),
				lastProposalID: true,
			})

			proposal, err := fixture.runner.ReplayCaseProposalDryRun(context.Background(), caseRecord.ID)
			if err != nil {
				t.Fatalf("ReplayCaseProposalDryRun() error = %v", err)
			}
			if proposal == nil || proposal.ProposerKind != "heuristic_v1" || proposal.ValidationStatus == nil || *proposal.ValidationStatus != "passed" {
				t.Fatalf("ReplayCaseProposalDryRun() = %#v, want persisted heuristic proposal", proposal)
			}
			if proposal.RawResultJSON != nil {
				t.Fatalf("heuristic replay proposal raw result = %#v, want nil", proposal.RawResultJSON)
			}
			if len(fixture.agent.calls) != 0 {
				t.Fatalf("agent calls = %d, want 0", len(fixture.agent.calls))
			}
		})
	}
}

func TestReplayCaseProposalDryRunPersistsFailedValidationProposalForInvalidAgentOutput(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	fixture.cfg.Roles.Sweeper.Proposer.Mode = config.SweeperProposerModeAgentApply
	caseRecord := seedReplayCase(t, fixture, storage.SweeperCaseRecord{
		ID:           "case_replay_invalid",
		ProjectID:    fixture.projectID,
		Repo:         "acme/looper",
		TargetType:   "issue",
		TargetNumber: 73,
		Status:       "pending",
		CurrentPhase: "warn",
		CreatedAt:    fixture.nowISO,
		UpdatedAt:    fixture.nowISO,
	}, seedReplayProposalInput{
		proposalID:     "proposal_replay_base_invalid",
		decision:       "warn",
		category:       categoryStale,
		proposerKind:   "heuristic_v1",
		factBundle:     replayFactBundle("acme/looper", "issue", 73, fixture.now.Add(-100*24*time.Hour)),
		createdAt:      fixture.now.Add(-time.Hour).Format(javaScriptISOStringUTC),
		lastProposalID: true,
	})
	fixture.agent.results = []AgentResult{{Status: "completed", Stdout: `{"schemaVersion":2,"decision":"warn","category":"stale","confidenceScore":88,"summary":"missing marker","rationale":"invalid replay validation"}`}}

	proposal, err := fixture.runner.ReplayCaseProposalDryRun(context.Background(), caseRecord.ID)
	if err == nil {
		t.Fatal("ReplayCaseProposalDryRun() error = nil, want validation error")
	}
	if proposal != nil {
		t.Fatalf("ReplayCaseProposalDryRun() proposal = %#v, want nil on validation failure", proposal)
	}

	proposals, listErr := fixture.repos.SweeperProposals.ListByCaseID(context.Background(), caseRecord.ID)
	if listErr != nil {
		t.Fatalf("SweeperProposals.ListByCaseID() error = %v", listErr)
	}
	if len(proposals) != 2 {
		t.Fatalf("len(proposals) = %d, want 2", len(proposals))
	}
	failed := proposals[0]
	if failed.ValidationStatus == nil || *failed.ValidationStatus != "failed" || failed.ValidationError == nil || !strings.Contains(*failed.ValidationError, "markerUUID is required") || failed.RawResultJSON == nil {
		t.Fatalf("failed replay proposal = %#v, want failed validation proposal with raw result", failed)
	}
	if failed.Decision != "no_action" || failed.Category != categoryNone {
		t.Fatalf("failed replay proposal = %#v, want no_action/none placeholder", failed)
	}
}

func TestListCasesAndInspectCaseUseSharedOperatorPlumbing(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	seedReplayCase(t, fixture, storage.SweeperCaseRecord{
		ID:           "case_operator_warn",
		ProjectID:    fixture.projectID,
		Repo:         "acme/looper",
		TargetType:   "issue",
		TargetNumber: 81,
		Status:       "pending",
		CurrentPhase: "warn",
		CreatedAt:    fixture.nowISO,
		UpdatedAt:    fixture.now.Add(time.Minute).Format(javaScriptISOStringUTC),
	}, seedReplayProposalInput{proposalID: "proposal_operator_warn", decision: "warn", category: categoryStale, proposerKind: "heuristic_v1", factBundle: replayFactBundle("acme/looper", "issue", 81, fixture.now.Add(-90*24*time.Hour)), createdAt: fixture.nowISO, lastProposalID: true})
	seedReplayCase(t, fixture, storage.SweeperCaseRecord{
		ID:           "case_operator_terminal",
		ProjectID:    fixture.projectID,
		Repo:         "acme/looper",
		TargetType:   "issue",
		TargetNumber: 82,
		Status:       "terminal",
		CurrentPhase: "terminal",
		CreatedAt:    fixture.nowISO,
		UpdatedAt:    fixture.now.Format(javaScriptISOStringUTC),
	}, seedReplayProposalInput{proposalID: "proposal_operator_terminal", decision: "close", category: categoryStale, proposerKind: proposerKindAgentV1, factBundle: replayFactBundle("acme/looper", "issue", 82, fixture.now.Add(-120*24*time.Hour)), createdAt: fixture.nowISO, lastProposalID: true})

	records, err := fixture.runner.ListCases(context.Background(), CaseQuery{ProjectID: fixture.projectID, Repo: "acme/looper", Phase: "warn", Limit: 10})
	if err != nil {
		t.Fatalf("ListCases() error = %v", err)
	}
	if len(records) != 1 || records[0].ID != "case_operator_warn" {
		t.Fatalf("ListCases() = %#v, want only warn-phase case", records)
	}

	inspection, err := fixture.runner.InspectCase(context.Background(), "case_operator_warn")
	if err != nil {
		t.Fatalf("InspectCase() error = %v", err)
	}
	if inspection == nil || inspection.Case.ID != "case_operator_warn" || len(inspection.Proposals) != 1 || inspection.Proposals[0].ID != "proposal_operator_warn" {
		t.Fatalf("InspectCase() = %#v, want case with its proposal", inspection)
	}
}

func TestRepoOperatorStatsSummarizesProposalAndTimeoutData(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	seedReplayCase(t, fixture, storage.SweeperCaseRecord{
		ID:           "case_stats_warn",
		ProjectID:    fixture.projectID,
		Repo:         "acme/looper",
		TargetType:   "issue",
		TargetNumber: 91,
		Status:       "pending",
		CurrentPhase: "warn",
		CreatedAt:    fixture.nowISO,
		UpdatedAt:    fixture.nowISO,
	}, seedReplayProposalInput{proposalID: "proposal_stats_warn", decision: "warn", category: categoryStale, proposerKind: proposerKindAgentV1, factBundle: replayFactBundle("acme/looper", "issue", 91, fixture.now.Add(-90*24*time.Hour)), createdAt: fixture.nowISO, lastProposalID: true})
	if err := fixture.repos.SweeperProposals.UpdateApplyReceipt(context.Background(), "proposal_stats_warn", "completed_warned", nil, nil, nil); err != nil {
		t.Fatalf("UpdateApplyReceipt() error = %v", err)
	}
	seedReplayCase(t, fixture, storage.SweeperCaseRecord{
		ID:           "case_stats_close",
		ProjectID:    fixture.projectID,
		Repo:         "acme/looper",
		TargetType:   "issue",
		TargetNumber: 92,
		Status:       "terminal",
		CurrentPhase: "terminal",
		CreatedAt:    fixture.nowISO,
		UpdatedAt:    fixture.nowISO,
	}, seedReplayProposalInput{proposalID: "proposal_stats_close", decision: "close", category: categoryAbandonedPR, proposerKind: "heuristic_v1", factBundle: replayFactBundle("acme/looper", "issue", 92, fixture.now.Add(-120*24*time.Hour)), createdAt: fixture.now.Add(time.Minute).Format(javaScriptISOStringUTC), lastProposalID: true})
	timeoutRaw := `{"status":"timeout","timeoutType":"max_runtime"}`
	validation := "passed"
	if err := fixture.repos.SweeperProposals.Insert(context.Background(), storage.SweeperProposalRecord{
		ID:               "proposal_stats_timeout",
		CaseID:           "case_stats_warn",
		ProjectID:        fixture.projectID,
		Repo:             "acme/looper",
		TargetType:       "issue",
		TargetNumber:     91,
		SchemaVersion:    1,
		ProposerKind:     proposerKindAgentV1,
		FactBundleJSON:   mustMarshalJSON(replayFactBundle("acme/looper", "issue", 91, fixture.now.Add(-90*24*time.Hour))),
		FingerprintJSON:  `{"hash":"stats-timeout"}`,
		ProposalJSON:     `{"schemaVersion":2,"decision":"warn","category":"stale","confidenceScore":80,"summary":"timeout replay","rationale":"timeout replay rationale","markerUUID":"marker-timeout"}`,
		RawResultJSON:    &timeoutRaw,
		Decision:         "warn",
		Category:         categoryStale,
		ConfidenceScore:  80,
		ValidationStatus: &validation,
		CreatedAt:        fixture.now.Add(2 * time.Minute).Format(javaScriptISOStringUTC),
	}); err != nil {
		t.Fatalf("SweeperProposals.Insert(timeout) error = %v", err)
	}
	stats, err := fixture.runner.RepoOperatorStats(context.Background(), fixture.projectID, "acme/looper", 10)
	if err != nil {
		t.Fatalf("RepoOperatorStats() error = %v", err)
	}
	if stats.CaseCount != 2 || stats.ProposalCount != 3 {
		t.Fatalf("RepoOperatorStats() = %#v, want 2 cases and 3 proposals", stats)
	}
	if stats.ProposalsByProposerKind[proposerKindAgentV1] != 2 || stats.ProposalsByProposerKind["heuristic_v1"] != 1 {
		t.Fatalf("ProposalsByProposerKind = %#v, want two agent and one heuristic proposals", stats.ProposalsByProposerKind)
	}
	if stats.ApplyOutcomes["completed_warned"] != 1 || stats.ApplyOutcomes["pending"] != 2 {
		t.Fatalf("ApplyOutcomes = %#v, want completed_warned=1 pending=2", stats.ApplyOutcomes)
	}
	if stats.CurrentPhases["warn"] != 1 || stats.CurrentPhases["terminal"] != 1 {
		t.Fatalf("CurrentPhases = %#v, want warn=1 terminal=1", stats.CurrentPhases)
	}
	if stats.StaleRate <= 0 || stats.AgentProposalCount != 2 || stats.AgentTimeouts != 1 || stats.AgentTimeoutRate <= 0 {
		t.Fatalf("RepoOperatorStats() = %#v, want stale proposals plus non-zero timeout rate", stats)
	}
}

func TestProjectMetadataAutoDryRunOverridesSweeperRoleConfig(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	fixture.cfg.Roles.Sweeper.DryRun = false
	metadata := `{"repo":"acme/looper","sweeper":{"autoDryRun":true,"autoDryRunReason":"timeout threshold exceeded"}}`
	project, err := fixture.repos.Projects.GetByID(context.Background(), fixture.projectID)
	if err != nil {
		t.Fatalf("Projects.GetByID() error = %v", err)
	}
	project.MetadataJSON = &metadata
	if err := fixture.repos.Projects.Upsert(context.Background(), *project); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	_, roleCfg, err := fixture.runner.projectConfig(context.Background(), fixture.projectID)
	if err != nil {
		t.Fatalf("projectConfig() error = %v", err)
	}
	if !roleCfg.DryRun {
		t.Fatalf("roleCfg.DryRun = false, want true from project metadata override")
	}
}

func TestProcessWarnDiagnosticModePersistsFreshShadowAndAgentProposal(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	fixture.cfg.Roles.Sweeper.DryRun = true
	fixture.cfg.Roles.Sweeper.Proposer.Mode = config.SweeperProposerModeAgentApply
	fixture.cfg.Roles.Sweeper.Proposer.DiagnosticMode = true
	caseRecord := seedReplayCase(t, fixture, storage.SweeperCaseRecord{
		ID:           "case_diagnostic_warn",
		ProjectID:    fixture.projectID,
		Repo:         "acme/looper",
		TargetType:   "issue",
		TargetNumber: 96,
		Status:       "pending",
		CurrentPhase: "warn",
		CreatedAt:    fixture.nowISO,
		UpdatedAt:    fixture.nowISO,
	}, seedReplayProposalInput{proposalID: "proposal_diagnostic_existing_agent", decision: "warn", category: categoryStale, proposerKind: proposerKindAgentV1, factBundle: replayFactBundle("acme/looper", "issue", 96, fixture.now.Add(-100*24*time.Hour)), createdAt: fixture.now.Add(-time.Hour).Format(javaScriptISOStringUTC), lastProposalID: true})
	validation := "passed"
	if err := fixture.repos.SweeperProposals.UpdateApplyReceipt(context.Background(), "proposal_diagnostic_existing_agent", "completed_warned", nil, nil, nil); err != nil {
		t.Fatalf("UpdateApplyReceipt() error = %v", err)
	}
	proposal, err := fixture.repos.SweeperProposals.GetByID(context.Background(), "proposal_diagnostic_existing_agent")
	if err != nil {
		t.Fatalf("GetByID() error = %v", err)
	}
	proposal.ValidationStatus = &validation
	proposal.SchemaVersion = 1
	if err := fixture.repos.SweeperCases.Upsert(context.Background(), storage.SweeperCaseRecord{ID: caseRecord.ID, ProjectID: caseRecord.ProjectID, Repo: caseRecord.Repo, TargetType: caseRecord.TargetType, TargetNumber: caseRecord.TargetNumber, Status: caseRecord.Status, CurrentPhase: caseRecord.CurrentPhase, LastProposalID: &proposal.ID, CreatedAt: caseRecord.CreatedAt, UpdatedAt: caseRecord.UpdatedAt}); err != nil {
		t.Fatalf("SweeperCases.Upsert() error = %v", err)
	}
	fixture.github.issueDetails["acme/looper#96"] = githubinfra.IssueDetail{Number: 96, Title: "Old stale issue", Body: "still stale", State: "open", UpdatedAt: fixture.now.Add(-100 * 24 * time.Hour).Format(time.RFC3339), Author: "octo", Labels: nil}
	fixture.agent.results = []AgentResult{{Status: "completed", Stdout: `{"schemaVersion":2,"decision":"warn","category":"stale","confidenceScore":90,"summary":"diagnostic agent warning","rationale":"diagnostic rationale","markerUUID":"marker-diagnostic-96"}`}}
	payloadJSON := mustMarshalPayload(sweeperPayload{CaseID: caseRecord.ID})
	queueID := "queue_sweeper_warn_diagnostic"
	if err := fixture.repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: queueID, ProjectID: &fixture.projectID, Type: QueueTypeWarn, TargetType: "issue", TargetID: "acme/looper#96", Repo: stringPtr("acme/looper"), DedupeKey: "sweeper:warn:acme/looper#96", Priority: 1, Status: "running", AvailableAt: fixture.nowISO, MaxAttempts: 3, PayloadJSON: &payloadJSON, CreatedAt: fixture.nowISO, UpdatedAt: fixture.nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	result, err := fixture.runner.ProcessClaimedQueueItem(context.Background(), storage.QueueItemRecord{ID: queueID, Type: QueueTypeWarn})
	if err != nil {
		t.Fatalf("ProcessClaimedQueueItem() error = %v", err)
	}
	if result == nil || result.Status != "skipped" {
		t.Fatalf("ProcessClaimedQueueItem() = %#v, want dry-run skipped result", result)
	}
	proposals, err := fixture.repos.SweeperProposals.ListByCaseID(context.Background(), caseRecord.ID)
	if err != nil {
		t.Fatalf("ListByCaseID() error = %v", err)
	}
	if len(proposals) != 3 {
		t.Fatalf("len(proposals) = %d, want existing agent + fresh heuristic shadow + fresh agent", len(proposals))
	}
	agentCount := 0
	heuristicCount := 0
	foundExisting := false
	for _, proposal := range proposals {
		switch proposal.ProposerKind {
		case proposerKindAgentV1:
			agentCount++
		case "heuristic_v1":
			heuristicCount++
		}
		if proposal.ID == "proposal_diagnostic_existing_agent" {
			foundExisting = true
		}
	}
	if agentCount != 2 || heuristicCount != 1 || !foundExisting {
		t.Fatalf("proposals = %#v, want two agent proposals, one heuristic shadow, and the original agent proposal preserved", proposals)
	}
}

func TestProcessCloseClosesAndReconcilesLabels(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	fixture.cfg.Roles.Sweeper.DryRun = false
	category := categoryAlreadyFixed
	confidence := int64(90)
	marker := "marker"
	warningCommentID := int64(99)
	warnedAt := fixture.now.Add(-8 * 24 * time.Hour).Format(javaScriptISOStringUTC)
	closeDueAt := fixture.now.Add(-24 * time.Hour).Format(javaScriptISOStringUTC)
	validation := "passed"
	rationale := "target appears already fixed"
	if err := fixture.repos.SweeperCases.Upsert(context.Background(), storage.SweeperCaseRecord{ID: "case_close_42", ProjectID: fixture.projectID, Repo: "acme/looper", TargetType: "issue", TargetNumber: 42, Status: "pending", CurrentPhase: "warn", CurrentCategory: &category, CurrentConfidenceScore: &confidence, WarningCommentID: &warningCommentID, WarningMarkerUUID: &marker, WarnedAt: &warnedAt, CloseDueAt: &closeDueAt, LastProposalID: stringPtr("proposal_warn_42"), CreatedAt: fixture.nowISO, UpdatedAt: fixture.nowISO}); err != nil {
		t.Fatalf("SweeperCases.Upsert() error = %v", err)
	}
	closeCaseRecord, err := fixture.repos.SweeperCases.GetByProjectRepoTarget(context.Background(), fixture.projectID, "acme/looper", "issue", 42)
	if err != nil {
		t.Fatalf("SweeperCases.GetByProjectRepoTarget() error = %v", err)
	}
	warnTarget := liveTarget{Number: 42, State: "open", Title: "Bug", Body: "already fixed by #9", UpdatedAt: fixture.now.Add(-8 * 24 * time.Hour).Format(time.RFC3339), Author: "octo", Labels: []string{"looper:sweep-pending"}}
	warnFingerprint, err := BuildFingerprint(fixture.runner.buildFactBundle(warnTarget, closeCaseRecord, fixture.cfg.Roles.Sweeper))
	if err != nil {
		t.Fatalf("BuildFingerprint() error = %v", err)
	}
	if err := fixture.repos.SweeperProposals.Insert(context.Background(), storage.SweeperProposalRecord{ID: "proposal_warn_42", CaseID: "case_close_42", ProjectID: fixture.projectID, Repo: "acme/looper", TargetType: "issue", TargetNumber: 42, SchemaVersion: 2, ProposerKind: "heuristic_v1", FactBundleJSON: "{}", FingerprintJSON: warnFingerprint, ProposalJSON: `{"decision":"warn"}`, Decision: "warn", Category: categoryAlreadyFixed, ConfidenceScore: 90, Rationale: &rationale, MarkerUUID: &marker, ValidationStatus: &validation, ApplyStatus: stringPtr("completed_warned"), AppliedAt: &warnedAt, CreatedAt: warnedAt}); err != nil {
		t.Fatalf("SweeperProposals.Insert() error = %v", err)
	}
	payloadJSON := mustMarshalPayload(sweeperPayload{CaseID: "case_close_42"})
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
	caseRecord, err := fixture.repos.SweeperCases.GetByProjectRepoTarget(context.Background(), fixture.projectID, "acme/looper", "issue", 42)
	if err != nil {
		t.Fatalf("SweeperCases.GetByProjectRepoTarget() error = %v", err)
	}
	if caseRecord == nil || caseRecord.Status != "terminal" || caseRecord.TerminalOutcome == nil || *caseRecord.TerminalOutcome != outcomeClosed {
		t.Fatalf("caseRecord = %#v, want terminal closed case", caseRecord)
	}
	proposal, err := fixture.repos.SweeperProposals.GetLatestByCaseID(context.Background(), caseRecord.ID)
	if err != nil {
		t.Fatalf("SweeperProposals.GetLatestByCaseID() error = %v", err)
	}
	if proposal == nil || proposal.ApplyStatus == nil || *proposal.ApplyStatus != "completed_closed" || proposal.Decision != "close" {
		t.Fatalf("proposal = %#v, want completed close proposal", proposal)
	}
	if proposal.ID == "proposal_warn_42" {
		t.Fatalf("proposal = %#v, want new close proposal instead of mutating warn proposal", proposal)
	}
}

func TestProcessCloseAgentApplyConsumesAgentCloseProposal(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	fixture.cfg.Roles.Sweeper.DryRun = false
	fixture.cfg.Roles.Sweeper.Proposer.Mode = config.SweeperProposerModeAgentApply
	category := categoryStale
	confidence := int64(80)
	marker := "marker-agent-close"
	warningCommentID := int64(77)
	warnedAt := fixture.now.Add(-8 * 24 * time.Hour).Format(javaScriptISOStringUTC)
	closeDueAt := fixture.now.Add(-24 * time.Hour).Format(javaScriptISOStringUTC)
	validation := "passed"
	rationale := "agent warned stale inactivity"
	if err := fixture.repos.SweeperCases.Upsert(context.Background(), storage.SweeperCaseRecord{ID: "case_close_agent", ProjectID: fixture.projectID, Repo: "acme/looper", TargetType: "issue", TargetNumber: 63, Status: "pending", CurrentPhase: "warn", CurrentCategory: &category, CurrentConfidenceScore: &confidence, WarningCommentID: &warningCommentID, WarningMarkerUUID: &marker, WarnedAt: &warnedAt, CloseDueAt: &closeDueAt, LastProposalID: stringPtr("proposal_warn_agent"), CreatedAt: fixture.nowISO, UpdatedAt: fixture.nowISO}); err != nil {
		t.Fatalf("SweeperCases.Upsert() error = %v", err)
	}
	caseRecord, err := fixture.repos.SweeperCases.GetByProjectRepoTarget(context.Background(), fixture.projectID, "acme/looper", "issue", 63)
	if err != nil {
		t.Fatalf("SweeperCases.GetByProjectRepoTarget() error = %v", err)
	}
	warnTarget := liveTarget{Number: 63, State: "open", Title: "stale bug", Body: "needs cleanup", UpdatedAt: fixture.now.Add(-100 * 24 * time.Hour).Format(time.RFC3339), Author: "octo", Labels: []string{"looper:sweep-pending"}}
	warnFingerprint, err := BuildFingerprint(fixture.runner.buildFactBundle(warnTarget, caseRecord, fixture.cfg.Roles.Sweeper))
	if err != nil {
		t.Fatalf("BuildFingerprint() error = %v", err)
	}
	if err := fixture.repos.SweeperProposals.Insert(context.Background(), storage.SweeperProposalRecord{ID: "proposal_warn_agent", CaseID: "case_close_agent", ProjectID: fixture.projectID, Repo: "acme/looper", TargetType: "issue", TargetNumber: 63, SchemaVersion: 2, ProposerKind: proposerKindAgentV1, FactBundleJSON: "{}", FingerprintJSON: warnFingerprint, ProposalJSON: `{"schemaVersion":2,"decision":"warn"}`, RawResultJSON: stringPtr(`{"stdout":"warn"}`), Decision: "warn", Category: categoryStale, ConfidenceScore: 80, Rationale: &rationale, MarkerUUID: &marker, ValidationStatus: &validation, ApplyStatus: stringPtr("completed_warned"), AppliedAt: &warnedAt, CreatedAt: warnedAt}); err != nil {
		t.Fatalf("SweeperProposals.Insert() error = %v", err)
	}
	fixture.agent.results = []AgentResult{{Status: "completed", Stdout: `{"schemaVersion":2,"decision":"close","category":"stale","confidenceScore":91,"summary":"agent close","rationale":"agent confirmed long-running stale inactivity"}`}}
	payloadJSON := mustMarshalPayload(sweeperPayload{CaseID: "case_close_agent"})
	fixture.github.issueDetails["acme/looper#63"] = githubinfra.IssueDetail{Number: 63, Title: "stale bug", Body: "needs cleanup", State: "open", UpdatedAt: fixture.now.Add(-100 * 24 * time.Hour).Format(time.RFC3339), Author: "octo", Labels: []string{"looper:sweep-pending"}}
	queueID := "queue_sweeper_close_agent"
	if err := fixture.repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: queueID, ProjectID: &fixture.projectID, Type: QueueTypeClose, TargetType: "issue", TargetID: "acme/looper#63", Repo: stringPtr("acme/looper"), DedupeKey: "sweeper:close:acme/looper#63", Priority: 1, Status: "running", AvailableAt: fixture.nowISO, PayloadJSON: &payloadJSON, MaxAttempts: 3, CreatedAt: fixture.nowISO, UpdatedAt: fixture.nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	result, err := fixture.runner.ProcessClaimedQueueItem(context.Background(), storage.QueueItemRecord{ID: queueID, Type: QueueTypeClose})
	if err != nil {
		t.Fatalf("ProcessClaimedQueueItem() error = %v", err)
	}
	if result == nil || result.Status != "completed" {
		t.Fatalf("ProcessClaimedQueueItem() = %#v, want completed", result)
	}
	if len(fixture.agent.calls) != 1 {
		t.Fatalf("agent calls = %d, want 1", len(fixture.agent.calls))
	}
	updatedCase, err := fixture.repos.SweeperCases.GetByProjectRepoTarget(context.Background(), fixture.projectID, "acme/looper", "issue", 63)
	if err != nil {
		t.Fatalf("GetByProjectRepoTarget() error = %v", err)
	}
	if updatedCase == nil || updatedCase.LastProposalID == nil {
		t.Fatalf("updated case = %#v, want last proposal id", updatedCase)
	}
	latest, err := fixture.repos.SweeperProposals.GetByID(context.Background(), *updatedCase.LastProposalID)
	if err != nil {
		t.Fatalf("GetByID() error = %v", err)
	}
	if latest == nil || latest.ProposerKind != proposerKindAgentV1 || latest.Decision != "close" || latest.RawResultJSON == nil {
		t.Fatalf("case last proposal = %#v, want agent close proposal with raw result", latest)
	}
	if len(fixture.github.closedIssues) != 1 {
		t.Fatalf("closedIssues = %#v, want one close", fixture.github.closedIssues)
	}
}

func TestProcessCloseRetriesCloseBeforeRemovingPendingLabel(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	fixture.cfg.Roles.Sweeper.DryRun = false
	category := categoryAlreadyFixed
	confidence := int64(90)
	marker := "marker-close-retry"
	warningCommentID := int64(100)
	warnedAt := fixture.now.Add(-8 * 24 * time.Hour).Format(javaScriptISOStringUTC)
	closeDueAt := fixture.now.Add(-24 * time.Hour).Format(javaScriptISOStringUTC)
	validation := "passed"
	rationale := "target appears already fixed"
	if err := fixture.repos.SweeperCases.Upsert(context.Background(), storage.SweeperCaseRecord{ID: "case_close_retry", ProjectID: fixture.projectID, Repo: "acme/looper", TargetType: "issue", TargetNumber: 43, Status: "pending", CurrentPhase: "warn", CurrentCategory: &category, CurrentConfidenceScore: &confidence, WarningCommentID: &warningCommentID, WarningMarkerUUID: &marker, WarnedAt: &warnedAt, CloseDueAt: &closeDueAt, LastProposalID: stringPtr("proposal_warn_close_retry"), CreatedAt: fixture.nowISO, UpdatedAt: fixture.nowISO}); err != nil {
		t.Fatalf("SweeperCases.Upsert() error = %v", err)
	}
	closeCaseRecord, err := fixture.repos.SweeperCases.GetByProjectRepoTarget(context.Background(), fixture.projectID, "acme/looper", "issue", 43)
	if err != nil {
		t.Fatalf("SweeperCases.GetByProjectRepoTarget() error = %v", err)
	}
	warnTarget := liveTarget{Number: 43, State: "open", Title: "Bug", Body: "already fixed by #9", UpdatedAt: fixture.now.Add(-8 * 24 * time.Hour).Format(time.RFC3339), Author: "octo", Labels: []string{"looper:sweep-pending"}}
	warnFingerprint, err := BuildFingerprint(fixture.runner.buildFactBundle(warnTarget, closeCaseRecord, fixture.cfg.Roles.Sweeper))
	if err != nil {
		t.Fatalf("BuildFingerprint() error = %v", err)
	}
	if err := fixture.repos.SweeperProposals.Insert(context.Background(), storage.SweeperProposalRecord{ID: "proposal_warn_close_retry", CaseID: "case_close_retry", ProjectID: fixture.projectID, Repo: "acme/looper", TargetType: "issue", TargetNumber: 43, SchemaVersion: 2, ProposerKind: "heuristic_v1", FactBundleJSON: "{}", FingerprintJSON: warnFingerprint, ProposalJSON: `{"decision":"warn"}`, Decision: "warn", Category: categoryAlreadyFixed, ConfidenceScore: 90, Rationale: &rationale, MarkerUUID: &marker, ValidationStatus: &validation, ApplyStatus: stringPtr("completed_warned"), AppliedAt: &warnedAt, CreatedAt: warnedAt}); err != nil {
		t.Fatalf("SweeperProposals.Insert() error = %v", err)
	}
	payloadJSON := mustMarshalPayload(sweeperPayload{CaseID: "case_close_retry"})
	fixture.github.issueDetails["acme/looper#43"] = githubinfra.IssueDetail{Number: 43, Title: "Bug", Body: "already fixed by #9", State: "open", UpdatedAt: fixture.now.Add(-8 * 24 * time.Hour).Format(time.RFC3339), Author: "octo", Labels: []string{"looper:sweep-pending"}}
	fixture.github.closeIssueErr = errors.New("temporary close failure")
	queueID := "queue_sweeper_close_retry"
	if err := fixture.repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: queueID, ProjectID: &fixture.projectID, Type: QueueTypeClose, TargetType: "issue", TargetID: "acme/looper#43", Repo: stringPtr("acme/looper"), DedupeKey: "sweeper:close:acme/looper#43", Priority: 1, Status: "running", AvailableAt: fixture.nowISO, MaxAttempts: 3, PayloadJSON: &payloadJSON, CreatedAt: fixture.nowISO, UpdatedAt: fixture.nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	result, err := fixture.runner.ProcessClaimedQueueItem(context.Background(), storage.QueueItemRecord{ID: queueID, Type: QueueTypeClose})
	if err != nil {
		t.Fatalf("ProcessClaimedQueueItem() error = %v", err)
	}
	if result == nil || result.Status != "failed" {
		t.Fatalf("ProcessClaimedQueueItem() = %#v, want failed result", result)
	}
	if got := fixture.github.removedLabels["acme/looper#43"]; len(got) != 0 {
		t.Fatalf("removedLabels = %#v, want pending label untouched after close failure", fixture.github.removedLabels)
	}
	stored, err := fixture.repos.Queue.GetByID(context.Background(), queueID)
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	if stored == nil || stored.Status != "queued" || stored.Attempts != 1 {
		t.Fatalf("stored queue item = %#v, want queued retry after close failure", stored)
	}

	result, err = fixture.runner.ProcessClaimedQueueItem(context.Background(), storage.QueueItemRecord{ID: queueID, Type: QueueTypeClose})
	if err != nil {
		t.Fatalf("ProcessClaimedQueueItem(retry) error = %v", err)
	}
	if result == nil || result.Status != "completed" {
		t.Fatalf("ProcessClaimedQueueItem(retry) = %#v, want completed result", result)
	}
	if !containsString(fixture.github.removedLabels["acme/looper#43"], "looper:sweep-pending") {
		t.Fatalf("removedLabels = %#v, want pending label removed only after successful close", fixture.github.removedLabels)
	}
}

func TestProcessWarnResumesFromMarkerWithoutDuplicateComment(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	fixture.cfg.Roles.Sweeper.DryRun = false
	marker := "sweeper_marker_existing"
	fixture.github.issueDetails["acme/looper#44"] = githubinfra.IssueDetail{
		Number: 44,
		Title:  "Bug",
		Body:   "already fixed by #9",
		State:  "open",
		Author: "octo",
		Comments: []githubinfra.CommentInfo{{
			ID:   777,
			Body: "warning\n<!-- looper:sweeper:warn id=" + marker + " -->",
		}},
	}
	queueID := "queue_sweeper_warn_resume"
	validation := "passed"
	if err := fixture.repos.SweeperCases.Upsert(context.Background(), storage.SweeperCaseRecord{ID: "case_resume", ProjectID: fixture.projectID, Repo: "acme/looper", TargetType: "issue", TargetNumber: 44, Status: "open", CurrentPhase: "warn", CreatedAt: fixture.nowISO, UpdatedAt: fixture.nowISO}); err != nil {
		t.Fatalf("SweeperCases.Upsert() error = %v", err)
	}
	if err := fixture.repos.SweeperProposals.Insert(context.Background(), storage.SweeperProposalRecord{ID: "proposal_resume", CaseID: "case_resume", ProjectID: fixture.projectID, Repo: "acme/looper", TargetType: "issue", TargetNumber: 44, SchemaVersion: 2, ProposerKind: "heuristic_v1", FactBundleJSON: "{}", FingerprintJSON: `{"hash":"resume"}`, ProposalJSON: `{"decision":"warn"}`, Decision: "warn", Category: categoryAlreadyFixed, ConfidenceScore: 90, MarkerUUID: &marker, ValidationStatus: &validation, CreatedAt: fixture.nowISO}); err != nil {
		t.Fatalf("SweeperProposals.Insert() error = %v", err)
	}
	payloadJSON := mustMarshalPayload(sweeperPayload{CaseID: "case_resume", ProposalID: "proposal_resume"})
	if err := fixture.repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: queueID, ProjectID: &fixture.projectID, Type: QueueTypeWarn, TargetType: "issue", TargetID: "acme/looper#44", Repo: stringPtr("acme/looper"), DedupeKey: "sweeper:warn:acme/looper#44", Priority: 1, Status: "running", AvailableAt: fixture.nowISO, MaxAttempts: 3, PayloadJSON: &payloadJSON, CreatedAt: fixture.nowISO, UpdatedAt: fixture.nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	result, err := fixture.runner.ProcessClaimedQueueItem(context.Background(), storage.QueueItemRecord{ID: queueID, Type: QueueTypeWarn})
	if err != nil {
		t.Fatalf("ProcessClaimedQueueItem() error = %v", err)
	}
	if result == nil || result.Status != "completed" {
		t.Fatalf("ProcessClaimedQueueItem() = %#v, want completed result", result)
	}
	if len(fixture.github.createdComments) != 0 {
		t.Fatalf("createdComments = %#v, want no duplicate warning comment", fixture.github.createdComments)
	}
	if !containsString(fixture.github.addedLabels["acme/looper#44"], "looper:sweep-pending") {
		t.Fatalf("added labels = %#v, want pending label added during resume", fixture.github.addedLabels)
	}
	stored, err := fixture.repos.Queue.GetByID(context.Background(), queueID)
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	payload := fixture.runner.readPayload(*stored)
	if payload.CaseID != "case_resume" || payload.ProposalID != "proposal_resume" || payload.WarningCommentID != 0 || payload.Outcome != "" {
		t.Fatalf("payload = %#v, want lean resume metadata", payload)
	}
}

func TestProcessWarnRecreatesMissingPersistedWarningComment(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	fixture.cfg.Roles.Sweeper.DryRun = false
	marker := "sweeper_marker_missing"
	warningCommentID := int64(777)
	staleWarnedAt := fixture.now.Add(-14 * 24 * time.Hour).Format(javaScriptISOStringUTC)
	staleCloseDueAt := fixture.now.Add(-7 * 24 * time.Hour).Format(javaScriptISOStringUTC)
	fixture.github.issueDetails["acme/looper#47"] = githubinfra.IssueDetail{Number: 47, Title: "Bug", Body: "already fixed by #9", State: "open", Author: "octo"}
	if err := fixture.repos.SweeperCases.Upsert(context.Background(), storage.SweeperCaseRecord{ID: "case_missing_comment", ProjectID: fixture.projectID, Repo: "acme/looper", TargetType: "issue", TargetNumber: 47, Status: "pending", CurrentPhase: "warn", WarningCommentID: &warningCommentID, WarningMarkerUUID: &marker, WarnedAt: &staleWarnedAt, CloseDueAt: &staleCloseDueAt, CreatedAt: fixture.nowISO, UpdatedAt: fixture.nowISO}); err != nil {
		t.Fatalf("SweeperCases.Upsert() error = %v", err)
	}
	payloadJSON := mustMarshalPayload(sweeperPayload{CaseID: "case_missing_comment"})
	queueID := "queue_sweeper_warn_missing_comment"
	if err := fixture.repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: queueID, ProjectID: &fixture.projectID, Type: QueueTypeWarn, TargetType: "issue", TargetID: "acme/looper#47", Repo: stringPtr("acme/looper"), DedupeKey: "sweeper:warn:acme/looper#47", Priority: 1, Status: "running", AvailableAt: fixture.nowISO, MaxAttempts: 3, PayloadJSON: &payloadJSON, CreatedAt: fixture.nowISO, UpdatedAt: fixture.nowISO}); err != nil {
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
		t.Fatalf("createdComments = %#v, want warning comment recreated", fixture.github.createdComments)
	}
	if !strings.Contains(fixture.github.createdComments[0].Body, marker) {
		t.Fatalf("created comment body = %q, want marker %q", fixture.github.createdComments[0].Body, marker)
	}
	if !containsString(fixture.github.addedLabels["acme/looper#47"], "looper:sweep-pending") {
		t.Fatalf("added labels = %#v, want pending label added", fixture.github.addedLabels)
	}
	if strings.Contains(fixture.github.createdComments[0].Body, staleCloseDueAt) {
		t.Fatalf("created comment body = %q, want recreated warning to drop stale close-by %q", fixture.github.createdComments[0].Body, staleCloseDueAt)
	}
	caseRecord, err := fixture.repos.SweeperCases.GetByProjectRepoTarget(context.Background(), fixture.projectID, "acme/looper", "issue", 47)
	if err != nil {
		t.Fatalf("SweeperCases.GetByProjectRepoTarget() error = %v", err)
	}
	if caseRecord == nil || caseRecord.WarnedAt == nil || caseRecord.CloseDueAt == nil {
		t.Fatalf("caseRecord = %#v, want recreated warning timestamps checkpointed", caseRecord)
	}
	if *caseRecord.WarnedAt == staleWarnedAt || *caseRecord.CloseDueAt == staleCloseDueAt {
		t.Fatalf("caseRecord = %#v, want recreated warning to refresh stale timestamps", caseRecord)
	}
}

func TestProcessWarnRecoversAfterCommentPostedButLabelFails(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	fixture.cfg.Roles.Sweeper.DryRun = false
	fixture.github.addIssueLabelsErr = errors.New("temporary label failure")
	fixture.github.issueDetails["acme/looper#45"] = githubinfra.IssueDetail{Number: 45, Title: "Bug", Body: "already fixed by #9", State: "open", Author: "octo"}
	queueID := "queue_sweeper_warn_retry"
	if err := fixture.repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: queueID, ProjectID: &fixture.projectID, Type: QueueTypeWarn, TargetType: "issue", TargetID: "acme/looper#45", Repo: stringPtr("acme/looper"), DedupeKey: "sweeper:warn:acme/looper#45", Priority: 1, Status: "running", AvailableAt: fixture.nowISO, MaxAttempts: 3, CreatedAt: fixture.nowISO, UpdatedAt: fixture.nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	result, err := fixture.runner.ProcessClaimedQueueItem(context.Background(), storage.QueueItemRecord{ID: queueID, Type: QueueTypeWarn})
	if err != nil {
		t.Fatalf("ProcessClaimedQueueItem() error = %v, want recovered failed result", err)
	}
	if result == nil || result.Status != "failed" {
		t.Fatalf("ProcessClaimedQueueItem() = %#v, want failed result", result)
	}
	if len(fixture.github.createdComments) != 1 {
		t.Fatalf("createdComments = %d, want exactly one posted warning comment", len(fixture.github.createdComments))
	}
	stored, err := fixture.repos.Queue.GetByID(context.Background(), queueID)
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	payload := fixture.runner.readPayload(*stored)
	if payload.CaseID == "" || payload.ProposalID == "" || payload.WarningCommentID != 0 || payload.WarningMarkerUUID != "" {
		t.Fatalf("payload = %#v, want lean persisted retry metadata after partial warn", payload)
	}
	if stored.Status != "queued" || stored.Attempts != 1 || stored.LastErrorKind == nil || *stored.LastErrorKind != "retryable_transient" {
		t.Fatalf("stored queue item = %#v, want queued retry after partial warn failure", stored)
	}
	if stored.AvailableAt == fixture.nowISO {
		t.Fatalf("stored queue item available_at = %q, want retry backoff", stored.AvailableAt)
	}
	proposal, err := fixture.repos.SweeperProposals.GetByID(context.Background(), payload.ProposalID)
	if err != nil {
		t.Fatalf("SweeperProposals.GetByID() error = %v", err)
	}
	if proposal == nil || proposal.ApplyStatus == nil || *proposal.ApplyStatus != "failed_retryable" {
		t.Fatalf("proposal = %#v, want failed_retryable apply receipt after label failure", proposal)
	}
	caseRecord, err := fixture.repos.SweeperCases.GetByProjectRepoTarget(context.Background(), fixture.projectID, "acme/looper", "issue", 45)
	if err != nil {
		t.Fatalf("SweeperCases.GetByProjectRepoTarget() error = %v", err)
	}
	if caseRecord == nil || caseRecord.WarningCommentID == nil || *caseRecord.WarningCommentID == 0 || caseRecord.WarnedAt == nil || caseRecord.CloseDueAt == nil {
		t.Fatalf("caseRecord = %#v, want partial warning checkpointed to case row", caseRecord)
	}
	firstWarnedAt := *caseRecord.WarnedAt
	firstCloseDueAt := *caseRecord.CloseDueAt
	fixture.github.issueDetails["acme/looper#45"] = githubinfra.IssueDetail{Number: 45, Title: "Bug", Body: "already fixed by #9", State: "open", Author: "octo", Comments: []githubinfra.CommentInfo{{ID: 1, Body: fixture.github.createdComments[0].Body}}}
	fixture.github.addIssueLabelsErr = nil

	result, err = fixture.runner.ProcessClaimedQueueItem(context.Background(), storage.QueueItemRecord{ID: queueID, Type: QueueTypeWarn})
	if err != nil {
		t.Fatalf("ProcessClaimedQueueItem(retry) error = %v", err)
	}
	if result == nil || result.Status != "completed" {
		t.Fatalf("ProcessClaimedQueueItem(retry) = %#v, want completed result", result)
	}
	if len(fixture.github.createdComments) != 1 {
		t.Fatalf("createdComments after retry = %d, want no duplicate comment", len(fixture.github.createdComments))
	}
	if !containsString(fixture.github.addedLabels["acme/looper#45"], "looper:sweep-pending") {
		t.Fatalf("added labels = %#v, want pending label on retry", fixture.github.addedLabels)
	}
	caseRecord, err = fixture.repos.SweeperCases.GetByProjectRepoTarget(context.Background(), fixture.projectID, "acme/looper", "issue", 45)
	if err != nil {
		t.Fatalf("SweeperCases.GetByProjectRepoTarget(retry) error = %v", err)
	}
	if caseRecord == nil || caseRecord.WarnedAt == nil || caseRecord.CloseDueAt == nil || *caseRecord.WarnedAt != firstWarnedAt || *caseRecord.CloseDueAt != firstCloseDueAt {
		t.Fatalf("caseRecord after retry = %#v, want warned_at/close_due_at preserved", caseRecord)
	}
}

func TestProcessWarnFailsQueueItemAfterMaxAttempts(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	fixture.cfg.Roles.Sweeper.DryRun = false
	fixture.github.addIssueLabelsErr = errors.New("temporary label failure")
	fixture.github.issueDetails["acme/looper#46"] = githubinfra.IssueDetail{Number: 46, Title: "Bug", Body: "already fixed by #9", State: "open", Author: "octo"}
	queueID := "queue_sweeper_warn_fail_terminal"
	if err := fixture.repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: queueID, ProjectID: &fixture.projectID, Type: QueueTypeWarn, TargetType: "issue", TargetID: "acme/looper#46", Repo: stringPtr("acme/looper"), DedupeKey: "sweeper:warn:acme/looper#46", Priority: 1, Status: "running", AvailableAt: fixture.nowISO, Attempts: 2, MaxAttempts: 3, CreatedAt: fixture.nowISO, UpdatedAt: fixture.nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	result, err := fixture.runner.ProcessClaimedQueueItem(context.Background(), storage.QueueItemRecord{ID: queueID, Type: QueueTypeWarn})
	if err != nil {
		t.Fatalf("ProcessClaimedQueueItem() error = %v, want recovered failed result", err)
	}
	if result == nil || result.Status != "failed" {
		t.Fatalf("ProcessClaimedQueueItem() = %#v, want failed result", result)
	}
	stored, err := fixture.repos.Queue.GetByID(context.Background(), queueID)
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	if stored == nil || stored.Status != "failed" || stored.Attempts != 3 || stored.FinishedAt == nil || stored.LastErrorKind == nil || *stored.LastErrorKind != "retryable_transient" {
		t.Fatalf("stored queue item = %#v, want terminal failed queue item after max attempts", stored)
	}
}

func TestHasSecuritySensitiveSignalAvoidsShortTokenFalsePositives(t *testing.T) {
	t.Parallel()

	if hasSecuritySensitiveSignal("source map update") {
		t.Fatal("hasSecuritySensitiveSignal(source map update) = true, want false")
	}
	if hasSecuritySensitiveSignal("vulnerability disclosure policy docs update") {
		t.Fatal("hasSecuritySensitiveSignal(vulnerability disclosure policy docs update) = true, want false")
	}
}

func TestClassifyQueueFailureTreatsDeterministicErrorsAsNonRetryable(t *testing.T) {
	t.Parallel()

	for _, tc := range []string{
		"project not found: looper",
		"invalid sweeper target id \"oops\"",
		"invalid sweeper target id \"repo#abc\": strconv.ParseInt: parsing \"abc\": invalid syntax",
		"sweeper agent proposer is not configured",
	} {
		kind, _ := classifyQueueFailure(fmt.Errorf(tc))
		if kind != "non_retryable" {
			t.Fatalf("classifyQueueFailure(%q) kind = %q, want non_retryable", tc, kind)
		}
	}
}

func TestProcessCloseRemovesPendingLabelWhenKeepLabelCancelsClose(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	fixture.cfg.Roles.Sweeper.DryRun = false
	payload := sweeperPayload{Phase: "warn", Outcome: outcomePending, Category: categoryStale, Repo: "acme/looper", TargetType: "issue", TargetNumber: 42, WarningCommentID: 99, WarningMarkerUUID: "marker", CommentBody: "warning", PendingLabel: "looper:sweep-pending"}
	payloadJSON := mustMarshalLegacyPayload(payload)
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

func TestProcessCloseLegacyProposalIDCreatesNewCloseProposal(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	fixture.cfg.Roles.Sweeper.DryRun = false
	category := categoryAlreadyFixed
	confidence := int64(90)
	marker := "marker_legacy_close"
	warnedAt := fixture.now.Add(-8 * 24 * time.Hour).Format(javaScriptISOStringUTC)
	closeDueAt := fixture.now.Add(-24 * time.Hour).Format(javaScriptISOStringUTC)
	validation := "passed"
	rationale := "target appears already fixed"
	warningCommentID := int64(99)
	if err := fixture.repos.SweeperCases.Upsert(context.Background(), storage.SweeperCaseRecord{ID: "case_legacy_close", ProjectID: fixture.projectID, Repo: "acme/looper", TargetType: "issue", TargetNumber: 46, Status: "pending", CurrentPhase: "warn", CurrentCategory: &category, CurrentConfidenceScore: &confidence, WarningCommentID: &warningCommentID, WarningMarkerUUID: &marker, WarnedAt: &warnedAt, CloseDueAt: &closeDueAt, LastProposalID: stringPtr("proposal_warn_legacy_close"), CreatedAt: fixture.nowISO, UpdatedAt: fixture.nowISO}); err != nil {
		t.Fatalf("SweeperCases.Upsert() error = %v", err)
	}
	caseRecord, err := fixture.repos.SweeperCases.GetByProjectRepoTarget(context.Background(), fixture.projectID, "acme/looper", "issue", 46)
	if err != nil {
		t.Fatalf("SweeperCases.GetByProjectRepoTarget() error = %v", err)
	}
	fingerprint, err := BuildFingerprint(fixture.runner.buildFactBundle(liveTarget{Number: 46, State: "open", Title: "Bug", Body: "already fixed by #9", UpdatedAt: fixture.now.Add(-8 * 24 * time.Hour).Format(time.RFC3339), Author: "octo", Labels: []string{"looper:sweep-pending"}}, caseRecord, fixture.cfg.Roles.Sweeper))
	if err != nil {
		t.Fatalf("BuildFingerprint() error = %v", err)
	}
	if err := fixture.repos.SweeperProposals.Insert(context.Background(), storage.SweeperProposalRecord{ID: "proposal_warn_legacy_close", CaseID: "case_legacy_close", ProjectID: fixture.projectID, Repo: "acme/looper", TargetType: "issue", TargetNumber: 46, SchemaVersion: 2, ProposerKind: "heuristic_v1", FactBundleJSON: "{}", FingerprintJSON: fingerprint, ProposalJSON: `{"decision":"warn"}`, Decision: "warn", Category: categoryAlreadyFixed, ConfidenceScore: 90, Rationale: &rationale, MarkerUUID: &marker, ValidationStatus: &validation, ApplyStatus: stringPtr("completed_warned"), AppliedAt: &warnedAt, CreatedAt: warnedAt}); err != nil {
		t.Fatalf("SweeperProposals.Insert() error = %v", err)
	}
	payloadJSON := mustMarshalLegacyPayload(sweeperPayload{CaseID: "case_legacy_close", ProposalID: "proposal_warn_legacy_close", Phase: "close", Repo: "acme/looper", TargetType: "issue", TargetNumber: 46})
	fixture.github.issueDetails["acme/looper#46"] = githubinfra.IssueDetail{Number: 46, Title: "Bug", Body: "already fixed by #9", State: "open", UpdatedAt: fixture.now.Add(-8 * 24 * time.Hour).Format(time.RFC3339), Author: "octo", Labels: []string{"looper:sweep-pending"}}
	queueID := "queue_sweeper_close_legacy_id"
	if err := fixture.repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: queueID, ProjectID: &fixture.projectID, Type: QueueTypeClose, TargetType: "issue", TargetID: "acme/looper#46", Repo: stringPtr("acme/looper"), DedupeKey: "sweeper:close:acme/looper#46", Priority: 1, Status: "running", AvailableAt: fixture.nowISO, MaxAttempts: 3, PayloadJSON: &payloadJSON, CreatedAt: fixture.nowISO, UpdatedAt: fixture.nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}
	result, err := fixture.runner.ProcessClaimedQueueItem(context.Background(), storage.QueueItemRecord{ID: queueID, Type: QueueTypeClose})
	if err != nil {
		t.Fatalf("ProcessClaimedQueueItem() error = %v", err)
	}
	if result == nil || result.Status != "completed" {
		t.Fatalf("ProcessClaimedQueueItem() = %#v, want completed result", result)
	}
	proposal, err := fixture.repos.SweeperProposals.GetLatestByCaseID(context.Background(), "case_legacy_close")
	if err != nil {
		t.Fatalf("SweeperProposals.GetLatestByCaseID() error = %v", err)
	}
	if proposal == nil || proposal.Decision != "close" || proposal.ID == "proposal_warn_legacy_close" {
		t.Fatalf("proposal = %#v, want new close proposal created from legacy proposal_id payload", proposal)
	}
}

func TestProcessCloseRemovesPendingLabelWhenTargetAlreadyClosed(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	fixture.cfg.Roles.Sweeper.DryRun = false
	payload := sweeperPayload{Phase: "warn", Outcome: outcomePending, Category: categoryStale, Repo: "acme/looper", TargetType: "issue", TargetNumber: 42, WarningCommentID: 99, WarningMarkerUUID: "marker", CommentBody: "warning", PendingLabel: "looper:sweep-pending"}
	payloadJSON := mustMarshalLegacyPayload(payload)
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
	payloadJSON := mustMarshalLegacyPayload(payload)
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

func TestStaleProposalStatusForApplyDetectsFingerprintDrift(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	fixture.cfg.Roles.Sweeper.DryRun = false
	target := liveTarget{Number: 52, State: "open", Title: "Bug", Body: "already fixed by #9", UpdatedAt: fixture.now.Add(-8 * 24 * time.Hour).Format(time.RFC3339), Author: "octo", Labels: []string{"looper:sweep-pending"}}
	caseRecord, err := fixture.runner.ensureCase(context.Background(), fixture.projectID, target, sweeperPayload{Repo: "acme/looper"}, fixture.cfg.Roles.Sweeper)
	if err != nil {
		t.Fatalf("ensureCase() error = %v", err)
	}
	oldBundle := fixture.runner.buildFactBundle(liveTarget{Number: 52, State: "open", Title: "Bug", Body: "already fixed by #9", UpdatedAt: fixture.now.Add(-20 * 24 * time.Hour).Format(time.RFC3339), Author: "octo", Labels: []string{"looper:sweep-pending"}}, caseRecord, fixture.cfg.Roles.Sweeper)
	oldFingerprint, err := BuildFingerprint(oldBundle)
	if err != nil {
		t.Fatalf("BuildFingerprint() error = %v", err)
	}
	validation := "passed"
	applyStatus := "pending"
	rationale := "target appears already fixed"
	if err := fixture.repos.SweeperProposals.Insert(context.Background(), storage.SweeperProposalRecord{ID: "proposal_stale", CaseID: caseRecord.ID, ProjectID: fixture.projectID, Repo: "acme/looper", TargetType: "issue", TargetNumber: 52, SchemaVersion: 2, ProposerKind: "heuristic_v1", FactBundleJSON: "{}", FingerprintJSON: oldFingerprint, ProposalJSON: `{"decision":"close"}`, Decision: "close", Category: "already_fixed", ConfidenceScore: 90, Rationale: &rationale, ValidationStatus: &validation, ApplyStatus: &applyStatus, CreatedAt: fixture.nowISO}); err != nil {
		t.Fatalf("SweeperProposals.Insert() error = %v", err)
	}
	caseRecord.LastProposalID = stringPtr("proposal_stale")
	caseRecord.UpdatedAt = fixture.nowISO
	if err := fixture.repos.SweeperCases.Upsert(context.Background(), *caseRecord); err != nil {
		t.Fatalf("SweeperCases.Upsert() error = %v", err)
	}
	proposal, err := fixture.repos.SweeperProposals.GetByID(context.Background(), "proposal_stale")
	if err != nil {
		t.Fatalf("SweeperProposals.GetByID() error = %v", err)
	}
	stale, priorProposal, fingerprintJSON, err := fixture.runner.staleProposalStatusForApply(liveTarget{Number: 52, State: "open", Title: "Bug", Body: "already fixed by #9", UpdatedAt: fixture.now.Add(-8 * 24 * time.Hour).Format(time.RFC3339), Author: "octo", Labels: []string{"looper:sweep-pending"}}, caseRecord, fixture.cfg.Roles.Sweeper, proposal)
	if err != nil {
		t.Fatalf("staleProposalStatusForApply() error = %v", err)
	}
	if !stale {
		t.Fatal("staleProposalStatusForApply() stale = false, want true")
	}
	if priorProposal == nil || priorProposal.ID != "proposal_stale" {
		t.Fatalf("priorProposal = %#v, want proposal_stale", priorProposal)
	}
	if fingerprintJSON == oldFingerprint {
		t.Fatalf("fingerprintJSON = %q, want refreshed fingerprint", fingerprintJSON)
	}
}

func TestStaleProposalStatusForApplyCountsReviewThreadActivity(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	target := liveTarget{
		Number:        53,
		State:         "open",
		Title:         "PR with review discussion",
		Body:          "already fixed by #9",
		Author:        "octo",
		Labels:        []string{"looper:sweep-pending"},
		IsPR:          true,
		HeadSHA:       "abc123",
		ReviewThreads: []githubinfra.ReviewThread{{Comments: []githubinfra.ReviewThreadComment{{Author: "reviewer", CreatedAt: "2026-05-01T12:00:00Z"}}}},
	}
	caseRecord, err := fixture.runner.ensureCase(context.Background(), fixture.projectID, target, sweeperPayload{Repo: "acme/looper"}, fixture.cfg.Roles.Sweeper)
	if err != nil {
		t.Fatalf("ensureCase() error = %v", err)
	}

	withoutThread := target
	withoutThread.ReviewThreads = nil
	oldFingerprint, err := BuildFingerprint(fixture.runner.buildFactBundle(withoutThread, caseRecord, fixture.cfg.Roles.Sweeper))
	if err != nil {
		t.Fatalf("BuildFingerprint(withoutThread) error = %v", err)
	}

	withThreadBundle := fixture.runner.buildFactBundle(target, caseRecord, fixture.cfg.Roles.Sweeper)
	if withThreadBundle.LastHumanCommentAt != "2026-05-01T12:00:00Z" {
		t.Fatalf("LastHumanCommentAt = %q, want review thread timestamp", withThreadBundle.LastHumanCommentAt)
	}
	if withThreadBundle.HumanCommentCountSinceOpen != 1 {
		t.Fatalf("HumanCommentCountSinceOpen = %d, want 1", withThreadBundle.HumanCommentCountSinceOpen)
	}

	newFingerprint, err := BuildFingerprint(withThreadBundle)
	if err != nil {
		t.Fatalf("BuildFingerprint(withThread) error = %v", err)
	}
	if newFingerprint == oldFingerprint {
		t.Fatal("BuildFingerprint() ignored review thread activity")
	}
	proposal := &storage.SweeperProposalRecord{FingerprintJSON: oldFingerprint}
	stale, _, refreshedFingerprint, err := fixture.runner.staleProposalStatusForApply(target, caseRecord, fixture.cfg.Roles.Sweeper, proposal)
	if err != nil {
		t.Fatalf("staleProposalStatusForApply() error = %v", err)
	}
	if !stale {
		t.Fatal("staleProposalStatusForApply() stale = false, want true")
	}
	if refreshedFingerprint != newFingerprint {
		t.Fatalf("refreshed fingerprint = %q, want %q", refreshedFingerprint, newFingerprint)
	}
}

func TestProcessCloseCreatesCloseProposalBeforeCheckingWarnFingerprint(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	fixture.cfg.Roles.Sweeper.DryRun = false
	category := categoryAlreadyFixed
	confidence := int64(90)
	marker := "marker_close_from_warn"
	warningCommentID := int64(99)
	warnedAt := fixture.now.Add(-20 * 24 * time.Hour).Format(javaScriptISOStringUTC)
	closeDueAt := fixture.now.Add(-24 * time.Hour).Format(javaScriptISOStringUTC)
	validation := "passed"
	rationale := "target appears already fixed"
	if err := fixture.repos.SweeperCases.Upsert(context.Background(), storage.SweeperCaseRecord{ID: "case_close_from_warn", ProjectID: fixture.projectID, Repo: "acme/looper", TargetType: "issue", TargetNumber: 154, Status: "pending", CurrentPhase: "warn", CurrentCategory: &category, CurrentConfidenceScore: &confidence, WarningCommentID: &warningCommentID, WarningMarkerUUID: &marker, WarnedAt: &warnedAt, CloseDueAt: &closeDueAt, LastProposalID: stringPtr("proposal_warn_close_from_warn"), CreatedAt: fixture.nowISO, UpdatedAt: fixture.nowISO}); err != nil {
		t.Fatalf("SweeperCases.Upsert() error = %v", err)
	}
	caseRecord, err := fixture.repos.SweeperCases.GetByProjectRepoTarget(context.Background(), fixture.projectID, "acme/looper", "issue", 154)
	if err != nil {
		t.Fatalf("SweeperCases.GetByProjectRepoTarget() error = %v", err)
	}
	oldTarget := liveTarget{Number: 154, State: "open", Title: "Bug", Body: "already fixed by #9", UpdatedAt: fixture.now.Add(-20 * 24 * time.Hour).Format(time.RFC3339), Author: "octo", Labels: []string{"looper:sweep-pending"}}
	oldFingerprint, err := BuildFingerprint(fixture.runner.buildFactBundle(oldTarget, caseRecord, fixture.cfg.Roles.Sweeper))
	if err != nil {
		t.Fatalf("BuildFingerprint() error = %v", err)
	}
	if err := fixture.repos.SweeperProposals.Insert(context.Background(), storage.SweeperProposalRecord{ID: "proposal_warn_close_from_warn", CaseID: caseRecord.ID, ProjectID: fixture.projectID, Repo: "acme/looper", TargetType: "issue", TargetNumber: 154, SchemaVersion: 2, ProposerKind: "heuristic_v1", FactBundleJSON: "{}", FingerprintJSON: oldFingerprint, ProposalJSON: `{"decision":"warn"}`, Decision: "warn", Category: categoryAlreadyFixed, ConfidenceScore: 90, Rationale: &rationale, MarkerUUID: &marker, ValidationStatus: &validation, ApplyStatus: stringPtr("completed_warned"), AppliedAt: &warnedAt, CreatedAt: warnedAt}); err != nil {
		t.Fatalf("SweeperProposals.Insert() error = %v", err)
	}
	payloadJSON := mustMarshalPayload(sweeperPayload{CaseID: caseRecord.ID})
	fixture.github.issueDetails["acme/looper#154"] = githubinfra.IssueDetail{Number: 154, Title: "Bug", Body: "already fixed by #9", State: "open", UpdatedAt: fixture.now.Add(-8 * 24 * time.Hour).Format(time.RFC3339), Author: "octo", Labels: []string{"looper:sweep-pending"}}
	queueID := "queue_sweeper_close_from_warn"
	if err := fixture.repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: queueID, ProjectID: &fixture.projectID, Type: QueueTypeClose, TargetType: "issue", TargetID: "acme/looper#154", Repo: stringPtr("acme/looper"), DedupeKey: "sweeper:close:acme/looper#154", Priority: 1, Status: "running", AvailableAt: fixture.nowISO, MaxAttempts: 3, PayloadJSON: &payloadJSON, CreatedAt: fixture.nowISO, UpdatedAt: fixture.nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	result, err := fixture.runner.ProcessClaimedQueueItem(context.Background(), storage.QueueItemRecord{ID: queueID, Type: QueueTypeClose})
	if err != nil {
		t.Fatalf("ProcessClaimedQueueItem() error = %v", err)
	}
	if result == nil || result.Status != "completed" {
		t.Fatalf("ProcessClaimedQueueItem() = %#v, want completed result", result)
	}
	if len(fixture.github.closedIssues) != 1 {
		t.Fatalf("closedIssues = %#v, want close to proceed", fixture.github.closedIssues)
	}
	latest, err := fixture.repos.SweeperProposals.GetLatestByCaseID(context.Background(), caseRecord.ID)
	if err != nil {
		t.Fatalf("SweeperProposals.GetLatestByCaseID() error = %v", err)
	}
	if latest == nil || latest.Decision != "close" || latest.ID == "proposal_warn_close_from_warn" {
		t.Fatalf("latest proposal = %#v, want fresh close proposal", latest)
	}
	if latest.ApplyStatus == nil || *latest.ApplyStatus != "completed_closed" {
		t.Fatalf("latest proposal = %#v, want completed close receipt", latest)
	}
}

func TestProcessCloseRegeneratesObsoleteAgentProposal(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	fixture.cfg.Roles.Sweeper.DryRun = false
	fixture.cfg.Roles.Sweeper.Proposer.Mode = config.SweeperProposerModeAgentApply
	category := categoryAlreadyFixed
	confidence := int64(90)
	marker := "marker_close_obsolete"
	warningCommentID := int64(41)
	warnedAt := fixture.now.Add(-20 * 24 * time.Hour).Format(javaScriptISOStringUTC)
	closeDueAt := fixture.now.Add(-24 * time.Hour).Format(javaScriptISOStringUTC)
	validation := "passed"
	rationale := "target appears already fixed"
	if err := fixture.repos.SweeperCases.Upsert(context.Background(), storage.SweeperCaseRecord{ID: "case_close_obsolete_agent", ProjectID: fixture.projectID, Repo: "acme/looper", TargetType: "issue", TargetNumber: 188, Status: "pending", CurrentPhase: "warn", CurrentCategory: &category, CurrentConfidenceScore: &confidence, WarningCommentID: &warningCommentID, WarningMarkerUUID: &marker, WarnedAt: &warnedAt, CloseDueAt: &closeDueAt, LastProposalID: stringPtr("proposal_close_obsolete_agent"), CreatedAt: fixture.nowISO, UpdatedAt: fixture.nowISO}); err != nil {
		t.Fatalf("SweeperCases.Upsert() error = %v", err)
	}
	if err := fixture.repos.SweeperProposals.Insert(context.Background(), storage.SweeperProposalRecord{ID: "proposal_close_obsolete_agent", CaseID: "case_close_obsolete_agent", ProjectID: fixture.projectID, Repo: "acme/looper", TargetType: "issue", TargetNumber: 188, SchemaVersion: 1, ProposerKind: proposerKindAgentV1, FactBundleJSON: "{}", FingerprintJSON: `{"hash":"close-obsolete"}`, ProposalJSON: `{"schemaVersion":1,"decision":"close","category":"already_fixed","confidenceScore":90,"summary":"obsolete close","rationale":"target appears already fixed"}`, Decision: "close", Category: categoryAlreadyFixed, ConfidenceScore: 90, Rationale: &rationale, ValidationStatus: &validation, ApplyStatus: stringPtr("pending"), CreatedAt: warnedAt}); err != nil {
		t.Fatalf("SweeperProposals.Insert() error = %v", err)
	}
	fixture.github.issueDetails["acme/looper#188"] = githubinfra.IssueDetail{Number: 188, Title: "Bug", Body: "already fixed by #9", State: "open", UpdatedAt: fixture.now.Add(-8 * 24 * time.Hour).Format(time.RFC3339), Author: "octo", Labels: []string{"looper:sweep-pending"}}
	fixture.agent.results = []AgentResult{{Status: "completed", Stdout: `{"schemaVersion":2,"decision":"close","category":"already_fixed","confidenceScore":93,"summary":"agent close","rationale":"agent confirmed fix landed"}`}}
	payloadJSON := mustMarshalPayload(sweeperPayload{CaseID: "case_close_obsolete_agent"})
	queueID := "queue_sweeper_close_obsolete_agent"
	if err := fixture.repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: queueID, ProjectID: &fixture.projectID, Type: QueueTypeClose, TargetType: "issue", TargetID: "acme/looper#188", Repo: stringPtr("acme/looper"), DedupeKey: "sweeper:close:acme/looper#188", Priority: 1, Status: "running", AvailableAt: fixture.nowISO, MaxAttempts: 3, PayloadJSON: &payloadJSON, CreatedAt: fixture.nowISO, UpdatedAt: fixture.nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	result, err := fixture.runner.ProcessClaimedQueueItem(context.Background(), storage.QueueItemRecord{ID: queueID, Type: QueueTypeClose})
	if err != nil {
		t.Fatalf("ProcessClaimedQueueItem() error = %v", err)
	}
	if result == nil || result.Status != "completed" {
		t.Fatalf("ProcessClaimedQueueItem() = %#v, want completed result", result)
	}
	if len(fixture.agent.calls) != 1 {
		t.Fatalf("agent calls = %d, want 1 regenerated close proposal", len(fixture.agent.calls))
	}
	if len(fixture.github.closedIssues) != 1 {
		t.Fatalf("closedIssues = %#v, want close to proceed after regenerating proposal", fixture.github.closedIssues)
	}
	obsolete, err := fixture.repos.SweeperProposals.GetByID(context.Background(), "proposal_close_obsolete_agent")
	if err != nil {
		t.Fatalf("SweeperProposals.GetByID() obsolete error = %v", err)
	}
	if obsolete == nil || obsolete.ApplyStatus == nil || *obsolete.ApplyStatus != "skipped_schema_obsolete" {
		t.Fatalf("obsolete proposal = %#v, want skipped_schema_obsolete receipt", obsolete)
	}
	latest, err := fixture.repos.SweeperProposals.GetLatestByCaseID(context.Background(), "case_close_obsolete_agent")
	if err != nil {
		t.Fatalf("SweeperProposals.GetLatestByCaseID() error = %v", err)
	}
	if latest == nil || latest.ID == "proposal_close_obsolete_agent" || latest.ProposerKind != proposerKindAgentV1 || latest.Decision != "close" {
		t.Fatalf("latest proposal = %#v, want fresh agent close proposal", latest)
	}
	if latest.ApplyStatus == nil || *latest.ApplyStatus != "completed_closed" {
		t.Fatalf("latest proposal = %#v, want completed close receipt", latest)
	}
}

func TestProcessCloseSetsRepoBeforeEnsuringCase(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	fixture.cfg.Roles.Sweeper.DryRun = true
	queueItem := storage.QueueItemRecord{ProjectID: &fixture.projectID, Type: QueueTypeClose, TargetType: "issue", TargetID: "acme/looper#153", Repo: stringPtr("acme/looper")}
	fixture.github.issueDetails["acme/looper#153"] = githubinfra.IssueDetail{Number: 153, Title: "Bug", Body: "already fixed by #9", State: "open", UpdatedAt: fixture.now.Add(-91 * 24 * time.Hour).Format(time.RFC3339), Author: "octo", Labels: []string{"looper:sweep-pending"}}

	payload, status, _, err := fixture.runner.processClose(context.Background(), queueItem, sweeperPayload{})
	if err != nil {
		t.Fatalf("processClose() error = %v", err)
	}
	if status != "skipped" {
		t.Fatalf("processClose() status = %q, want skipped", status)
	}
	caseRecord, err := fixture.repos.SweeperCases.GetByProjectRepoTarget(context.Background(), fixture.projectID, "acme/looper", "issue", 153)
	if err != nil {
		t.Fatalf("SweeperCases.GetByProjectRepoTarget() error = %v", err)
	}
	if caseRecord == nil {
		t.Fatal("caseRecord = nil, want repo-scoped case")
	}
	proposal, err := fixture.repos.SweeperProposals.GetByID(context.Background(), payload.ProposalID)
	if err != nil {
		t.Fatalf("SweeperProposals.GetByID() error = %v", err)
	}
	if proposal == nil || proposal.Repo != "acme/looper" {
		t.Fatalf("proposal = %#v, want repo-scoped proposal", proposal)
	}
	bundle, err := parseFactBundle(proposal.FactBundleJSON)
	if err != nil {
		t.Fatalf("parseFactBundle() error = %v", err)
	}
	if bundle.Repo != "acme/looper" {
		t.Fatalf("bundle.Repo = %q, want acme/looper", bundle.Repo)
	}
}

func TestProcessReconcileCancelsWhenPendingLabelRemoved(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	fixture.cfg.Roles.Sweeper.DryRun = false
	category := categoryStale
	confidence := int64(80)
	marker := "marker_reconcile"
	warnedAt := fixture.now.Add(-8 * 24 * time.Hour).Format(javaScriptISOStringUTC)
	closeDueAt := fixture.now.Add(-24 * time.Hour).Format(javaScriptISOStringUTC)
	validation := "passed"
	rationale := "open item matched stale sweeper heuristics"
	warningCommentID := int64(123)
	if err := fixture.repos.SweeperCases.Upsert(context.Background(), storage.SweeperCaseRecord{ID: "case_reconcile_7", ProjectID: fixture.projectID, Repo: "acme/looper", TargetType: "issue", TargetNumber: 7, Status: "pending", CurrentPhase: "warn", CurrentCategory: &category, CurrentConfidenceScore: &confidence, WarningCommentID: &warningCommentID, WarningMarkerUUID: &marker, WarnedAt: &warnedAt, CloseDueAt: &closeDueAt, LastProposalID: stringPtr("proposal_warn_7"), CreatedAt: fixture.nowISO, UpdatedAt: fixture.nowISO}); err != nil {
		t.Fatalf("SweeperCases.Upsert() error = %v", err)
	}
	if err := fixture.repos.SweeperProposals.Insert(context.Background(), storage.SweeperProposalRecord{ID: "proposal_warn_7", CaseID: "case_reconcile_7", ProjectID: fixture.projectID, Repo: "acme/looper", TargetType: "issue", TargetNumber: 7, SchemaVersion: 2, ProposerKind: "heuristic_v1", FactBundleJSON: "{}", FingerprintJSON: `{"hash":"warn7"}`, ProposalJSON: `{"decision":"warn"}`, Decision: "warn", Category: categoryStale, ConfidenceScore: 80, Rationale: &rationale, MarkerUUID: &marker, ValidationStatus: &validation, ApplyStatus: stringPtr("completed_warned"), AppliedAt: &warnedAt, CreatedAt: warnedAt}); err != nil {
		t.Fatalf("SweeperProposals.Insert() error = %v", err)
	}
	payloadJSON := mustMarshalPayload(sweeperPayload{CaseID: "case_reconcile_7"})
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
	category := categoryStale
	confidence := int64(80)
	marker := "marker_reconcile_pending"
	warnedAt := fixture.now.Add(-8 * 24 * time.Hour).Format(javaScriptISOStringUTC)
	closeDueAt := fixture.now.Add(24 * time.Hour).Format(javaScriptISOStringUTC)
	validation := "passed"
	rationale := "open item matched stale sweeper heuristics"
	warningCommentID := int64(123)
	if err := fixture.repos.SweeperCases.Upsert(context.Background(), storage.SweeperCaseRecord{ID: "case_reconcile_pending", ProjectID: fixture.projectID, Repo: "acme/looper", TargetType: "issue", TargetNumber: 7, Status: "pending", CurrentPhase: "warn", CurrentCategory: &category, CurrentConfidenceScore: &confidence, WarningCommentID: &warningCommentID, WarningMarkerUUID: &marker, WarnedAt: &warnedAt, CloseDueAt: &closeDueAt, LastProposalID: stringPtr("proposal_warn_pending"), CreatedAt: fixture.nowISO, UpdatedAt: fixture.nowISO}); err != nil {
		t.Fatalf("SweeperCases.Upsert() error = %v", err)
	}
	if err := fixture.repos.SweeperProposals.Insert(context.Background(), storage.SweeperProposalRecord{ID: "proposal_warn_pending", CaseID: "case_reconcile_pending", ProjectID: fixture.projectID, Repo: "acme/looper", TargetType: "issue", TargetNumber: 7, SchemaVersion: 2, ProposerKind: "heuristic_v1", FactBundleJSON: "{}", FingerprintJSON: `{"hash":"warn-pending"}`, ProposalJSON: `{"decision":"warn"}`, Decision: "warn", Category: categoryStale, ConfidenceScore: 80, Rationale: &rationale, MarkerUUID: &marker, ValidationStatus: &validation, ApplyStatus: stringPtr("completed_warned"), AppliedAt: &warnedAt, CreatedAt: warnedAt}); err != nil {
		t.Fatalf("SweeperProposals.Insert() error = %v", err)
	}
	payloadJSON := mustMarshalPayload(sweeperPayload{CaseID: "case_reconcile_pending"})
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
	if payload := fixture.runner.readPayload(*stored); payload.CaseID == "" || payload.ProposalID == "" || payload.Phase != "" || payload.Outcome != "" {
		t.Fatalf("payload = %#v, want lean queue metadata preserved", payload)
	}
	caseRecord, err := fixture.repos.SweeperCases.GetByProjectRepoTarget(context.Background(), fixture.projectID, "acme/looper", "issue", 7)
	if err != nil {
		t.Fatalf("SweeperCases.GetByProjectRepoTarget() error = %v", err)
	}
	if caseRecord == nil || caseRecord.Status != "pending" || caseRecord.CurrentPhase != "warn" {
		t.Fatalf("caseRecord = %#v, want warn/pending case preserved", caseRecord)
	}
	if len(fixture.github.updatedComments) != 0 {
		t.Fatalf("updatedComments = %#v, want none while pending label remains", fixture.github.updatedComments)
	}
}

func TestProcessReconcileLegacyProposalIDCreatesNewCancelProposal(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	fixture.cfg.Roles.Sweeper.DryRun = false
	category := categoryStale
	confidence := int64(80)
	marker := "marker_legacy_reconcile"
	warnedAt := fixture.now.Add(-8 * 24 * time.Hour).Format(javaScriptISOStringUTC)
	closeDueAt := fixture.now.Add(-24 * time.Hour).Format(javaScriptISOStringUTC)
	validation := "passed"
	rationale := "open item matched stale sweeper heuristics"
	warningCommentID := int64(123)
	if err := fixture.repos.SweeperCases.Upsert(context.Background(), storage.SweeperCaseRecord{ID: "case_legacy_reconcile", ProjectID: fixture.projectID, Repo: "acme/looper", TargetType: "issue", TargetNumber: 17, Status: "pending", CurrentPhase: "warn", CurrentCategory: &category, CurrentConfidenceScore: &confidence, WarningCommentID: &warningCommentID, WarningMarkerUUID: &marker, WarnedAt: &warnedAt, CloseDueAt: &closeDueAt, LastProposalID: stringPtr("proposal_warn_legacy_reconcile"), CreatedAt: fixture.nowISO, UpdatedAt: fixture.nowISO}); err != nil {
		t.Fatalf("SweeperCases.Upsert() error = %v", err)
	}
	if err := fixture.repos.SweeperProposals.Insert(context.Background(), storage.SweeperProposalRecord{ID: "proposal_warn_legacy_reconcile", CaseID: "case_legacy_reconcile", ProjectID: fixture.projectID, Repo: "acme/looper", TargetType: "issue", TargetNumber: 17, SchemaVersion: 2, ProposerKind: "heuristic_v1", FactBundleJSON: "{}", FingerprintJSON: `{"hash":"legacy-reconcile"}`, ProposalJSON: `{"decision":"warn"}`, Decision: "warn", Category: categoryStale, ConfidenceScore: 80, Rationale: &rationale, MarkerUUID: &marker, ValidationStatus: &validation, ApplyStatus: stringPtr("completed_warned"), AppliedAt: &warnedAt, CreatedAt: warnedAt}); err != nil {
		t.Fatalf("SweeperProposals.Insert() error = %v", err)
	}
	payloadJSON := mustMarshalLegacyPayload(sweeperPayload{CaseID: "case_legacy_reconcile", ProposalID: "proposal_warn_legacy_reconcile", Phase: "reconcile", Repo: "acme/looper", TargetType: "issue", TargetNumber: 17})
	fixture.github.issueDetails["acme/looper#17"] = githubinfra.IssueDetail{Number: 17, Title: "Bug", Body: "stale", State: "open", Author: "octo", Labels: nil}
	queueID := "queue_sweeper_reconcile_legacy_id"
	if err := fixture.repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: queueID, ProjectID: &fixture.projectID, Type: QueueTypeReconcile, TargetType: "issue", TargetID: "acme/looper#17", Repo: stringPtr("acme/looper"), DedupeKey: "sweeper:reconcile:acme/looper#17", Priority: 1, Status: "running", AvailableAt: fixture.nowISO, MaxAttempts: 3, PayloadJSON: &payloadJSON, CreatedAt: fixture.nowISO, UpdatedAt: fixture.nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}
	result, err := fixture.runner.ProcessClaimedQueueItem(context.Background(), storage.QueueItemRecord{ID: queueID, Type: QueueTypeReconcile})
	if err != nil {
		t.Fatalf("ProcessClaimedQueueItem() error = %v", err)
	}
	if result == nil || result.Status != "completed" {
		t.Fatalf("ProcessClaimedQueueItem() = %#v, want completed result", result)
	}
	proposal, err := fixture.repos.SweeperProposals.GetLatestByCaseID(context.Background(), "case_legacy_reconcile")
	if err != nil {
		t.Fatalf("SweeperProposals.GetLatestByCaseID() error = %v", err)
	}
	if proposal == nil || proposal.Decision != "cancel" || proposal.ID == "proposal_warn_legacy_reconcile" {
		t.Fatalf("proposal = %#v, want new cancel proposal created from legacy proposal_id payload", proposal)
	}
}

func TestProcessReconcileSetsRepoBeforePersistingProposal(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	fixture.cfg.Roles.Sweeper.DryRun = false
	category := categoryStale
	confidence := int64(80)
	rationale := "open item matched stale sweeper heuristics"
	if err := fixture.repos.SweeperCases.Upsert(context.Background(), storage.SweeperCaseRecord{ID: "case_reconcile_repo", ProjectID: fixture.projectID, Repo: "acme/looper", TargetType: "issue", TargetNumber: 171, Status: "pending", CurrentPhase: "warn", CurrentCategory: &category, CurrentConfidenceScore: &confidence, CreatedAt: fixture.nowISO, UpdatedAt: fixture.nowISO}); err != nil {
		t.Fatalf("SweeperCases.Upsert() error = %v", err)
	}
	if err := fixture.repos.SweeperProposals.Insert(context.Background(), storage.SweeperProposalRecord{ID: "proposal_warn_repo", CaseID: "case_reconcile_repo", ProjectID: fixture.projectID, Repo: "acme/looper", TargetType: "issue", TargetNumber: 171, SchemaVersion: 2, ProposerKind: "heuristic_v1", FactBundleJSON: "{}", FingerprintJSON: `{"hash":"warn-repo"}`, ProposalJSON: `{"decision":"warn"}`, Decision: "warn", Category: categoryStale, ConfidenceScore: 80, Rationale: &rationale, ValidationStatus: stringPtr("passed"), ApplyStatus: stringPtr("completed_warned"), CreatedAt: fixture.nowISO}); err != nil {
		t.Fatalf("SweeperProposals.Insert() error = %v", err)
	}
	queueItem := storage.QueueItemRecord{ProjectID: &fixture.projectID, Type: QueueTypeReconcile, TargetType: "issue", TargetID: "acme/looper#171", Repo: stringPtr("acme/looper")}
	fixture.github.issueDetails["acme/looper#171"] = githubinfra.IssueDetail{Number: 171, Title: "Bug", Body: "stale", State: "open", UpdatedAt: fixture.now.Add(-91 * 24 * time.Hour).Format(time.RFC3339), Author: "octo", Labels: nil}

	payload, status, _, err := fixture.runner.processReconcile(context.Background(), queueItem, sweeperPayload{CaseID: "case_reconcile_repo"})
	if err != nil {
		t.Fatalf("processReconcile() error = %v", err)
	}
	if status != "completed" {
		t.Fatalf("processReconcile() status = %q, want completed", status)
	}
	proposal, err := fixture.repos.SweeperProposals.GetByID(context.Background(), payload.ProposalID)
	if err != nil {
		t.Fatalf("SweeperProposals.GetByID() error = %v", err)
	}
	if proposal == nil || proposal.CaseID != "case_reconcile_repo" || proposal.Repo != "acme/looper" {
		t.Fatalf("proposal = %#v, want reconcile proposal persisted on repo-scoped case", proposal)
	}
	bundle, err := parseFactBundle(proposal.FactBundleJSON)
	if err != nil {
		t.Fatalf("parseFactBundle() error = %v", err)
	}
	if bundle.Repo != "acme/looper" {
		t.Fatalf("bundle.Repo = %q, want acme/looper", bundle.Repo)
	}
}

func TestDiscoverIssuesSkipsExcludedAuthorAssociations(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	fixture.github.issues = []githubinfra.IssueSummary{{Number: 1, Title: "stale bug", Body: "needs cleanup", Author: "octo"}}
	fixture.github.issueDetails["acme/looper#1"] = githubinfra.IssueDetail{Number: 1, AuthorAssociation: "OWNER"}

	result, err := fixture.runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	if len(result.QueueItems) != 0 || result.Skipped != 1 {
		t.Fatalf("DiscoverIssues() = %#v, want excluded association to be skipped", result)
	}
}

func TestDiscoverPullRequestsSkipsExcludedAuthorAssociations(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	fixture.github.prs = []githubinfra.PullRequestSummary{{Number: 2, Title: "stale pr", Author: "octo", UpdatedAt: fixture.now.Add(-40 * 24 * time.Hour).Format(javaScriptISOStringUTC)}}
	fixture.github.issueDetails["acme/looper#2"] = githubinfra.IssueDetail{Number: 2, AuthorAssociation: "OWNER", IsPullRequest: true}

	result, err := fixture.runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	if len(result.QueueItems) != 0 || result.Skipped != 1 {
		t.Fatalf("DiscoverPullRequests() = %#v, want excluded association to be skipped", result)
	}
}

func TestDiscoverIssuesSkipsWhenAuthorAssociationLookupFails(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	fixture.github.issues = []githubinfra.IssueSummary{{Number: 3, Title: "stale bug", Body: "needs cleanup", Author: "octo"}}
	fixture.github.viewIssueErr = errors.New("transient gh failure")

	result, err := fixture.runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	if len(result.QueueItems) != 0 || result.Skipped != 1 {
		t.Fatalf("DiscoverIssues() = %#v, want lookup failure to fail closed", result)
	}
}

func TestDiscoverPullRequestsSkipsWhenAuthorAssociationLookupFails(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	fixture.github.prs = []githubinfra.PullRequestSummary{{Number: 4, Title: "stale pr", Author: "octo", UpdatedAt: fixture.now.Add(-40 * 24 * time.Hour).Format(javaScriptISOStringUTC)}}
	fixture.github.viewIssueErr = errors.New("transient gh failure")

	result, err := fixture.runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	if len(result.QueueItems) != 0 || result.Skipped != 1 {
		t.Fatalf("DiscoverPullRequests() = %#v, want lookup failure to fail closed", result)
	}
}

func TestDiscoverIssuesSkipsReopenedSweptItemWithinCooldown(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	fixture.github.issues = []githubinfra.IssueSummary{{Number: 1, Title: "stale bug", Body: "needs cleanup", Author: "octo", Labels: []string{fixture.cfg.Roles.Sweeper.Lifecycle.ClosedLabel}}}
	closedAt := fixture.now.Add(-10 * 24 * time.Hour).Format(javaScriptISOStringUTC)
	payloadJSON := mustMarshalPayload(sweeperPayload{})
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
	payloadJSON := mustMarshalPayload(sweeperPayload{})
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
	agent     *stubAgentExecutor
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
	cfg.Roles.Sweeper.Proposer.Mode = config.SweeperProposerModeHeuristicFallback
	github := &stubGitHub{issueDetails: map[string]githubinfra.IssueDetail{}, prDetails: map[string]githubinfra.PullRequestDetail{}, addedLabels: map[string][]string{}, removedLabels: map[string][]string{}}
	agent := &stubAgentExecutor{}
	runner := New(Options{Repos: repos, GitHub: github, Agent: agent, Now: func() time.Time { return now }, Config: &cfg})
	return runnerFixture{repos: repos, runner: runner, github: github, agent: agent, cfg: &cfg, projectID: projectID, now: now, nowISO: nowISO}
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
	issues            []githubinfra.IssueSummary
	prs               []githubinfra.PullRequestSummary
	issueDetails      map[string]githubinfra.IssueDetail
	prDetails         map[string]githubinfra.PullRequestDetail
	viewIssueErr      error
	viewIssueCalls    int
	viewIssueRepos    []string
	viewIssueCWDs     []string
	listIssuesCalls   int
	listPRCalls       int
	createdComments   []githubinfra.IssueCommentInput
	updatedComments   []githubinfra.UpdateIssueCommentInput
	closedIssues      []githubinfra.CloseIssueInput
	closedPRs         []githubinfra.ClosePullRequestInput
	addedLabels       map[string][]string
	removedLabels     map[string][]string
	addIssueLabelsErr error
	closeIssueErr     error
	issueComments     []githubinfra.CommentInfo
	timeline          []map[string]any
	linkedPRs         []githubinfra.LinkedPullRequest
	reviewThreads     []githubinfra.ReviewThread
	reviewThreadsErr  error
	prReviewState     githubinfra.PullRequestReviewState
}

type stubAgentExecutor struct {
	results []AgentResult
	calls   []AgentRunInput
}

type stubAgentExecution struct{ result AgentResult }

func (s *stubAgentExecutor) Start(_ context.Context, input AgentRunInput) (AgentExecution, error) {
	s.calls = append(s.calls, input)
	result := AgentResult{Status: "completed"}
	if len(s.results) > 0 {
		result = s.results[0]
		s.results = s.results[1:]
	}
	return stubAgentExecution{result: result}, nil
}

func (s stubAgentExecution) Wait(context.Context) (AgentResult, error) { return s.result, nil }
func (s stubAgentExecution) Kill(string) error                         { return nil }

func (g *stubGitHub) ListOpenIssues(context.Context, githubinfra.ListOpenIssuesInput) ([]githubinfra.IssueSummary, error) {
	g.listIssuesCalls++
	return append([]githubinfra.IssueSummary(nil), g.issues...), nil
}

func (g *stubGitHub) ListOpenPullRequests(context.Context, githubinfra.ListOpenPullRequestsInput) ([]githubinfra.PullRequestSummary, error) {
	g.listPRCalls++
	return append([]githubinfra.PullRequestSummary(nil), g.prs...), nil
}

func (g *stubGitHub) ViewIssue(_ context.Context, input githubinfra.ViewIssueInput) (githubinfra.IssueDetail, error) {
	g.viewIssueCalls++
	g.viewIssueRepos = append(g.viewIssueRepos, input.Repo)
	g.viewIssueCWDs = append(g.viewIssueCWDs, input.CWD)
	if g.viewIssueErr != nil {
		return githubinfra.IssueDetail{}, g.viewIssueErr
	}
	return g.issueDetails[input.Repo+"#"+itoa(input.IssueNumber)], nil
}

func (g *stubGitHub) ViewPullRequest(_ context.Context, input githubinfra.ViewPullRequestInput) (githubinfra.PullRequestDetail, error) {
	return g.prDetails[input.Repo+"#"+itoa(input.PRNumber)], nil
}

func (g *stubGitHub) ListReviewThreads(context.Context, githubinfra.ListReviewThreadsInput) ([]githubinfra.ReviewThread, error) {
	if g.reviewThreadsErr != nil {
		return nil, g.reviewThreadsErr
	}
	return append([]githubinfra.ReviewThread(nil), g.reviewThreads...), nil
}

func (g *stubGitHub) ListIssueComments(context.Context, githubinfra.ViewIssueInput) ([]githubinfra.CommentInfo, error) {
	return append([]githubinfra.CommentInfo(nil), g.issueComments...), nil
}

func (g *stubGitHub) ListIssueTimeline(context.Context, githubinfra.IssueTimelineInput) ([]map[string]any, error) {
	return append([]map[string]any(nil), g.timeline...), nil
}

func (g *stubGitHub) ListIssueReactions(context.Context, githubinfra.IssueReactionInput) ([]githubinfra.IssueReaction, error) {
	return nil, nil
}

func (g *stubGitHub) ListLinkedPullRequests(context.Context, githubinfra.LinkedPullRequestsInput) ([]githubinfra.LinkedPullRequest, error) {
	return append([]githubinfra.LinkedPullRequest(nil), g.linkedPRs...), nil
}

func (g *stubGitHub) ListPullRequestReviewState(context.Context, githubinfra.PullRequestReviewStateInput) (githubinfra.PullRequestReviewState, error) {
	return g.prReviewState, nil
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
	if g.closeIssueErr != nil {
		err := g.closeIssueErr
		g.closeIssueErr = nil
		return err
	}
	g.closedIssues = append(g.closedIssues, input)
	return nil
}

func (g *stubGitHub) ClosePullRequest(_ context.Context, input githubinfra.ClosePullRequestInput) error {
	g.closedPRs = append(g.closedPRs, input)
	return nil
}

func (g *stubGitHub) AddIssueLabels(_ context.Context, input githubinfra.IssueLabelsInput) error {
	if g.addIssueLabelsErr != nil {
		err := g.addIssueLabelsErr
		g.addIssueLabelsErr = nil
		return err
	}
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

type seedReplayProposalInput struct {
	proposalID     string
	decision       string
	category       string
	proposerKind   string
	factBundle     FactBundle
	createdAt      string
	lastProposalID bool
}

func seedReplayCase(t *testing.T, fixture runnerFixture, record storage.SweeperCaseRecord, proposal seedReplayProposalInput) storage.SweeperCaseRecord {
	t.Helper()
	if err := fixture.repos.SweeperCases.Upsert(context.Background(), record); err != nil {
		t.Fatalf("SweeperCases.Upsert() error = %v", err)
	}

	factBundleJSON, err := json.Marshal(proposal.factBundle)
	if err != nil {
		t.Fatalf("json.Marshal(factBundle) error = %v", err)
	}
	fingerprintJSON, err := BuildFingerprint(proposal.factBundle)
	if err != nil {
		t.Fatalf("BuildFingerprint() error = %v", err)
	}
	validation := "passed"
	proposalJSON := `{"schemaVersion":2,"decision":"` + proposal.decision + `","category":"` + proposal.category + `","confidenceScore":80,"summary":"base proposal","rationale":"base rationale"`
	if proposal.decision == "warn" {
		proposalJSON += `,"markerUUID":"marker-base"`
	}
	proposalJSON += `}`
	if err := fixture.repos.SweeperProposals.Insert(context.Background(), storage.SweeperProposalRecord{
		ID:               proposal.proposalID,
		CaseID:           record.ID,
		ProjectID:        record.ProjectID,
		Repo:             record.Repo,
		TargetType:       record.TargetType,
		TargetNumber:     record.TargetNumber,
		SchemaVersion:    1,
		ProposerKind:     proposal.proposerKind,
		FactBundleJSON:   string(factBundleJSON),
		FingerprintJSON:  fingerprintJSON,
		ProposalJSON:     proposalJSON,
		Decision:         proposal.decision,
		Category:         proposal.category,
		ConfidenceScore:  80,
		ValidationStatus: &validation,
		CreatedAt:        proposal.createdAt,
	}); err != nil {
		t.Fatalf("SweeperProposals.Insert() error = %v", err)
	}
	if proposal.lastProposalID {
		record.LastProposalID = &proposal.proposalID
		if err := fixture.repos.SweeperCases.Upsert(context.Background(), record); err != nil {
			t.Fatalf("SweeperCases.Upsert(update last proposal) error = %v", err)
		}
	}
	return record
}

func replayFactBundle(repo, targetType string, number int64, updatedAt time.Time) FactBundle {
	return FactBundle{
		Repo:              repo,
		TargetType:        targetType,
		Number:            number,
		State:             "open",
		UpdatedAt:         updatedAt.Format(time.RFC3339),
		CreatedAt:         updatedAt.Add(-24 * time.Hour).Format(time.RFC3339),
		Title:             "Replay target",
		Body:              "Needs replay coverage",
		Author:            "octo",
		AuthorAssociation: "CONTRIBUTOR",
		CommentCount:      1,
		IsDraft:           false,
		HeadSHA:           "abc123",
	}
}

func mustMarshalJSON(value any) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func mustMarshalLegacyPayload(payload sweeperPayload) string {
	encoded, _ := json.Marshal(payloadEnvelope{Sweeper: payload})
	return string(encoded)
}
