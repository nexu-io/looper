package sweeper

import (
	"context"
	"encoding/json"
	"errors"
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
		{ID: "proposal_warn_done", CaseID: "case_warn", ProjectID: fixture.projectID, Repo: "acme/looper", TargetType: "issue", TargetNumber: 9, SchemaVersion: 1, ProposerKind: "heuristic_v1", FactBundleJSON: "{}", FingerprintJSON: `{"hash":"warn"}`, ProposalJSON: `{"decision":"warn"}`, Decision: "warn", Category: "stale", ConfidenceScore: 80, ValidationStatus: &validation, ApplyStatus: stringPtr("completed_warned"), AppliedAt: &fixture.nowISO, CreatedAt: fixture.nowISO},
		{ID: "proposal_close_done", CaseID: "case_close", ProjectID: fixture.projectID, Repo: "acme/looper", TargetType: "issue", TargetNumber: 10, SchemaVersion: 1, ProposerKind: "heuristic_v1", FactBundleJSON: "{}", FingerprintJSON: `{"hash":"close"}`, ProposalJSON: `{"decision":"close"}`, Decision: "close", Category: "stale", ConfidenceScore: 80, ValidationStatus: &validation, ApplyStatus: stringPtr("completed_closed"), AppliedAt: &fixture.nowISO, CreatedAt: fixture.nowISO},
		{ID: "proposal_close_inflight", CaseID: "case_pending", ProjectID: fixture.projectID, Repo: "acme/looper", TargetType: "issue", TargetNumber: 2, SchemaVersion: 1, ProposerKind: "heuristic_v1", FactBundleJSON: "{}", FingerprintJSON: `{"hash":"pending"}`, ProposalJSON: `{"decision":"close"}`, Decision: "close", Category: "stale", ConfidenceScore: 80, ValidationStatus: &validation, ApplyStatus: stringPtr("partial:commented"), CreatedAt: fixture.nowISO},
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

func TestProcessWarnAgentApplyUsesAgentProposalAndPersistsRawResult(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	fixture.cfg.Roles.Sweeper.DryRun = false
	fixture.cfg.Roles.Sweeper.Proposer.Mode = config.SweeperProposerModeAgentApply
	fixture.github.issueDetails["acme/looper#61"] = githubinfra.IssueDetail{Number: 61, Title: "Stale bug", Body: "needs cleanup", State: "open", UpdatedAt: fixture.now.Add(-100 * 24 * time.Hour).Format(time.RFC3339), Author: "octo"}
	fixture.agent.results = []AgentResult{{Status: "completed", Stdout: `{"schemaVersion":1,"decision":"warn","category":"stale","confidenceScore":88,"summary":"agent warning","rationale":"agent determined stale inactivity","markerUUID":"marker-agent-61"}`}}
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
	fixture.agent.results = []AgentResult{{Status: "completed", Stdout: `{"schemaVersion":1,"decision":"warn","category":"stale","confidenceScore":88,"summary":"agent warning","rationale":"agent determined stale inactivity","markerUUID":"marker-agent-62"}`}}
	fixture.github.addIssueLabelsErr = errors.New("temporary label failure")
	queueID := "queue_sweeper_warn_agent_62"
	if err := fixture.repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: queueID, ProjectID: &fixture.projectID, Type: QueueTypeWarn, TargetType: "issue", TargetID: "acme/looper#62", Repo: stringPtr("acme/looper"), DedupeKey: "sweeper:warn:acme/looper#62", Priority: 1, Status: "running", AvailableAt: fixture.nowISO, MaxAttempts: 3, CreatedAt: fixture.nowISO, UpdatedAt: fixture.nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	if _, err := fixture.runner.ProcessClaimedQueueItem(context.Background(), storage.QueueItemRecord{ID: queueID, Type: QueueTypeWarn}); err == nil {
		t.Fatal("first ProcessClaimedQueueItem() error = nil, want retryable label failure")
	}
	if len(fixture.agent.calls) != 1 {
		t.Fatalf("agent calls after first attempt = %d, want 1", len(fixture.agent.calls))
	}
	if err := fixture.repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: queueID, ProjectID: &fixture.projectID, Type: QueueTypeWarn, TargetType: "issue", TargetID: "acme/looper#62", Repo: stringPtr("acme/looper"), DedupeKey: "sweeper:warn:acme/looper#62", Priority: 1, Status: "running", AvailableAt: fixture.nowISO, MaxAttempts: 3, CreatedAt: fixture.nowISO, UpdatedAt: fixture.nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() retry error = %v", err)
	}

	result, err := fixture.runner.ProcessClaimedQueueItem(context.Background(), storage.QueueItemRecord{ID: queueID, Type: QueueTypeWarn})
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
	if err := fixture.repos.SweeperProposals.Insert(context.Background(), storage.SweeperProposalRecord{ID: "proposal_warn_42", CaseID: "case_close_42", ProjectID: fixture.projectID, Repo: "acme/looper", TargetType: "issue", TargetNumber: 42, SchemaVersion: 1, ProposerKind: "heuristic_v1", FactBundleJSON: "{}", FingerprintJSON: warnFingerprint, ProposalJSON: `{"decision":"warn"}`, Decision: "warn", Category: categoryAlreadyFixed, ConfidenceScore: 90, Rationale: &rationale, MarkerUUID: &marker, ValidationStatus: &validation, ApplyStatus: stringPtr("completed_warned"), AppliedAt: &warnedAt, CreatedAt: warnedAt}); err != nil {
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
	if err := fixture.repos.SweeperProposals.Insert(context.Background(), storage.SweeperProposalRecord{ID: "proposal_warn_agent", CaseID: "case_close_agent", ProjectID: fixture.projectID, Repo: "acme/looper", TargetType: "issue", TargetNumber: 63, SchemaVersion: 1, ProposerKind: proposerKindAgentV1, FactBundleJSON: "{}", FingerprintJSON: warnFingerprint, ProposalJSON: `{"schemaVersion":1,"decision":"warn"}`, RawResultJSON: stringPtr(`{"stdout":"warn"}`), Decision: "warn", Category: categoryStale, ConfidenceScore: 80, Rationale: &rationale, MarkerUUID: &marker, ValidationStatus: &validation, ApplyStatus: stringPtr("completed_warned"), AppliedAt: &warnedAt, CreatedAt: warnedAt}); err != nil {
		t.Fatalf("SweeperProposals.Insert() error = %v", err)
	}
	fixture.agent.results = []AgentResult{{Status: "completed", Stdout: `{"schemaVersion":1,"decision":"close","category":"stale","confidenceScore":91,"summary":"agent close","rationale":"agent confirmed long-running stale inactivity"}`}}
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
	if err := fixture.repos.SweeperProposals.Insert(context.Background(), storage.SweeperProposalRecord{ID: "proposal_resume", CaseID: "case_resume", ProjectID: fixture.projectID, Repo: "acme/looper", TargetType: "issue", TargetNumber: 44, SchemaVersion: 1, ProposerKind: "heuristic_v1", FactBundleJSON: "{}", FingerprintJSON: `{"hash":"resume"}`, ProposalJSON: `{"decision":"warn"}`, Decision: "warn", Category: categoryAlreadyFixed, ConfidenceScore: 90, MarkerUUID: &marker, ValidationStatus: &validation, CreatedAt: fixture.nowISO}); err != nil {
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

	if _, err := fixture.runner.ProcessClaimedQueueItem(context.Background(), storage.QueueItemRecord{ID: queueID, Type: QueueTypeWarn}); err == nil {
		t.Fatal("ProcessClaimedQueueItem() error = nil, want transient label failure")
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

	result, err := fixture.runner.ProcessClaimedQueueItem(context.Background(), storage.QueueItemRecord{ID: queueID, Type: QueueTypeWarn})
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
	if err := fixture.repos.SweeperProposals.Insert(context.Background(), storage.SweeperProposalRecord{ID: "proposal_warn_legacy_close", CaseID: "case_legacy_close", ProjectID: fixture.projectID, Repo: "acme/looper", TargetType: "issue", TargetNumber: 46, SchemaVersion: 1, ProposerKind: "heuristic_v1", FactBundleJSON: "{}", FingerprintJSON: fingerprint, ProposalJSON: `{"decision":"warn"}`, Decision: "warn", Category: categoryAlreadyFixed, ConfidenceScore: 90, Rationale: &rationale, MarkerUUID: &marker, ValidationStatus: &validation, ApplyStatus: stringPtr("completed_warned"), AppliedAt: &warnedAt, CreatedAt: warnedAt}); err != nil {
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

func TestProcessCloseSkipsStaleProposal(t *testing.T) {
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
	if err := fixture.repos.SweeperProposals.Insert(context.Background(), storage.SweeperProposalRecord{ID: "proposal_stale", CaseID: caseRecord.ID, ProjectID: fixture.projectID, Repo: "acme/looper", TargetType: "issue", TargetNumber: 52, SchemaVersion: 1, ProposerKind: "heuristic_v1", FactBundleJSON: "{}", FingerprintJSON: oldFingerprint, ProposalJSON: `{"decision":"warn"}`, Decision: "warn", Category: "already_fixed", ConfidenceScore: 90, Rationale: &rationale, ValidationStatus: &validation, ApplyStatus: &applyStatus, CreatedAt: fixture.nowISO}); err != nil {
		t.Fatalf("SweeperProposals.Insert() error = %v", err)
	}
	caseRecord.LastProposalID = stringPtr("proposal_stale")
	caseRecord.UpdatedAt = fixture.nowISO
	if err := fixture.repos.SweeperCases.Upsert(context.Background(), *caseRecord); err != nil {
		t.Fatalf("SweeperCases.Upsert() error = %v", err)
	}
	payloadJSON := mustMarshalPayload(sweeperPayload{CaseID: caseRecord.ID})
	fixture.github.issueDetails["acme/looper#52"] = githubinfra.IssueDetail{Number: 52, Title: "Bug", Body: "already fixed by #9", State: "open", UpdatedAt: fixture.now.Add(-8 * 24 * time.Hour).Format(time.RFC3339), Author: "octo", Labels: []string{"looper:sweep-pending"}}
	queueID := "queue_sweeper_close_stale"
	if err := fixture.repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: queueID, ProjectID: &fixture.projectID, Type: QueueTypeClose, TargetType: "issue", TargetID: "acme/looper#52", Repo: stringPtr("acme/looper"), DedupeKey: "sweeper:close:acme/looper#52", Priority: 1, Status: "running", AvailableAt: fixture.nowISO, MaxAttempts: 3, PayloadJSON: &payloadJSON, CreatedAt: fixture.nowISO, UpdatedAt: fixture.nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	result, err := fixture.runner.ProcessClaimedQueueItem(context.Background(), storage.QueueItemRecord{ID: queueID, Type: QueueTypeClose})
	if err != nil {
		t.Fatalf("ProcessClaimedQueueItem() error = %v", err)
	}
	if result == nil || result.Status != "skipped" {
		t.Fatalf("ProcessClaimedQueueItem() = %#v, want skipped stale result", result)
	}
	if len(fixture.github.closedIssues) != 0 {
		t.Fatalf("closedIssues = %#v, want no close when stale proposal detected", fixture.github.closedIssues)
	}
	proposal, err := fixture.repos.SweeperProposals.GetByID(context.Background(), "proposal_stale")
	if err != nil {
		t.Fatalf("SweeperProposals.GetByID() error = %v", err)
	}
	if proposal == nil || proposal.ApplyStatus == nil || *proposal.ApplyStatus != "skipped_stale_proposal" {
		t.Fatalf("proposal = %#v, want skipped_stale_proposal receipt", proposal)
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
	if err := fixture.repos.SweeperProposals.Insert(context.Background(), storage.SweeperProposalRecord{ID: "proposal_warn_7", CaseID: "case_reconcile_7", ProjectID: fixture.projectID, Repo: "acme/looper", TargetType: "issue", TargetNumber: 7, SchemaVersion: 1, ProposerKind: "heuristic_v1", FactBundleJSON: "{}", FingerprintJSON: `{"hash":"warn7"}`, ProposalJSON: `{"decision":"warn"}`, Decision: "warn", Category: categoryStale, ConfidenceScore: 80, Rationale: &rationale, MarkerUUID: &marker, ValidationStatus: &validation, ApplyStatus: stringPtr("completed_warned"), AppliedAt: &warnedAt, CreatedAt: warnedAt}); err != nil {
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
	if err := fixture.repos.SweeperProposals.Insert(context.Background(), storage.SweeperProposalRecord{ID: "proposal_warn_pending", CaseID: "case_reconcile_pending", ProjectID: fixture.projectID, Repo: "acme/looper", TargetType: "issue", TargetNumber: 7, SchemaVersion: 1, ProposerKind: "heuristic_v1", FactBundleJSON: "{}", FingerprintJSON: `{"hash":"warn-pending"}`, ProposalJSON: `{"decision":"warn"}`, Decision: "warn", Category: categoryStale, ConfidenceScore: 80, Rationale: &rationale, MarkerUUID: &marker, ValidationStatus: &validation, ApplyStatus: stringPtr("completed_warned"), AppliedAt: &warnedAt, CreatedAt: warnedAt}); err != nil {
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
	if err := fixture.repos.SweeperProposals.Insert(context.Background(), storage.SweeperProposalRecord{ID: "proposal_warn_legacy_reconcile", CaseID: "case_legacy_reconcile", ProjectID: fixture.projectID, Repo: "acme/looper", TargetType: "issue", TargetNumber: 17, SchemaVersion: 1, ProposerKind: "heuristic_v1", FactBundleJSON: "{}", FingerprintJSON: `{"hash":"legacy-reconcile"}`, ProposalJSON: `{"decision":"warn"}`, Decision: "warn", Category: categoryStale, ConfidenceScore: 80, Rationale: &rationale, MarkerUUID: &marker, ValidationStatus: &validation, ApplyStatus: stringPtr("completed_warned"), AppliedAt: &warnedAt, CreatedAt: warnedAt}); err != nil {
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
	listIssuesCalls   int
	listPRCalls       int
	createdComments   []githubinfra.IssueCommentInput
	updatedComments   []githubinfra.UpdateIssueCommentInput
	closedIssues      []githubinfra.CloseIssueInput
	closedPRs         []githubinfra.ClosePullRequestInput
	addedLabels       map[string][]string
	removedLabels     map[string][]string
	addIssueLabelsErr error
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

func mustMarshalLegacyPayload(payload sweeperPayload) string {
	encoded, _ := json.Marshal(payloadEnvelope{Sweeper: payload})
	return string(encoded)
}
