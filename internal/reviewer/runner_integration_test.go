package reviewer

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/e2e/harness"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/reviewer/criteria"
	"github.com/nexu-io/looper/internal/storage"
)

func TestReviewerAutoMergeHappyPathWithFakeGH(t *testing.T) {
	bins := harness.MustBinaries(t)
	fakeGH := harness.NewFakeGH(t, bins, harness.GHSchema{JSONFieldAllowlist: map[string][]string{"pr view": {"number", "title", "body", "url", "state", "createdAt", "updatedAt", "closedAt", "isDraft", "reviewDecision", "labels", "headRefName", "baseRefName", "headRefOid", "baseRefOid", "author", "authorAssociation", "reviewRequests", "comments", "reviews", "statusCheckRollup", "mergeStateStatus"}}})
	for key, value := range fakeGH.EnvMap() {
		t.Setenv(key, value)
	}
	fakeGH.WriteState(t, harness.GHState{
		Commands: map[string]any{"pr diff": map[string]any{"stdout": json.RawMessage(`"diff --git a/app.go b/app.go\n@@ -1,1 +1,2 @@\n-old\n+new\n+more\n"`)}},
		Routes: map[string]any{
			"repos/acme/looper/issues/358":               json.RawMessage(`{"number":358,"title":"Auto merge","body":"## Acceptance criteria\n- ship app change\n- add more\n","html_url":"https://example.test/issues/358","state":"open","created_at":"2026-05-14T12:00:00Z","updated_at":"2026-05-14T12:00:00Z","user":{"login":"octo"},"labels":[{"name":"triaged"},{"name":"dispatch/plan"}]}`),
			"repos/acme/looper":                          json.RawMessage(`{"allow_squash_merge":true,"allow_merge_commit":true,"allow_rebase_merge":true,"allow_auto_merge":true}`),
			"repos/acme/looper/branches/main/protection": json.RawMessage(`{"required_status_checks":{"contexts":["ci"]}}`),
		},
		CurrentUserLogin: "reviewer",
		PullRequests: map[string]harness.GHPullRequest{
			"acme/looper#42": {Number: 42, Repo: "acme/looper", Title: "Review me", Body: "Implements feature.\n\nCloses #358", State: "OPEN", Labels: []string{"looper:worker-ready"}, HeadRefName: "feature/review-me", BaseRefName: "main", HeadSHA: "abc123", BaseSHA: "base123", Author: "octocat", ReviewRequests: []string{"reviewer"}},
		},
	})

	fixture := newRunnerFixture(t)
	ctx := context.Background()
	repoPath := t.TempDir()
	baseBranch := "main"
	if err := fixture.repos.Projects.Upsert(ctx, storage.ProjectRecord{ID: "project_1", Name: "Looper", RepoPath: repoPath, BaseBranch: &baseBranch, CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	github := reviewerIntegrationGatewayAdapter{Gateway: githubinfra.New(githubinfra.Options{GHPath: fakeGH.Path, CWD: repoPath, Now: fixture.now})}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "No actionable findings", Stdout: `__LOOPER_RESULT__={"summary":"No actionable findings"}`, ParseStatus: "parsed"}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, ReviewEvents: config.ReviewerReviewEventsConfig{Clean: config.ReviewerReviewEventApprove}, LoopConfig: testReviewerLoopConfig(), CustomInstructions: reviewerAutoMergeTestConfig(t), CriteriaVerifier: stubCriteriaVerifier{responses: map[criteria.AcceptanceCriterion]criteria.CriterionAssessment{
		"ship app change": {Verdict: criteria.VerdictPass, Justification: "present in diff", Evidence: []criteria.Evidence{{FilePath: "app.go", StartLine: 1, EndLine: 2}}},
		"add more":        {Verdict: criteria.VerdictPass, Justification: "present in diff", Evidence: []criteria.Evidence{{FilePath: "app.go", StartLine: 2, EndLine: 2}}},
	}}})
	repo := "acme/looper"
	prNumber := int64(42)
	metadata := `{"followUpdates":true,"loop":{"enabled":true}}`
	loop := storage.LoopRecord{ID: "loop_fakegh_auto_merge_pass", Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: "queued", MetadataJSON: &metadata, CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}
	if err := fixture.repos.Loops.Upsert(ctx, loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	queue, err := runner.enqueue(ctx, enqueueInput{ProjectID: "project_1", LoopID: loop.ID, Repo: repo, PRNumber: prNumber})
	if err != nil {
		t.Fatalf("enqueue() error = %v", err)
	}
	claimed, err := fixture.repos.Queue.ClaimNextOfType(ctx, fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claimed == nil || claimed.ID != queue.ID {
		t.Fatalf("ClaimNextOfType() = (%#v, %v), want queue item %s", claimed, err, queue.ID)
	}
	result, err := runner.ProcessClaimedItem(ctx, *claimed)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	logBytes, err := os.ReadFile(fakeGH.InvocationLog)
	if err != nil {
		t.Fatalf("ReadFile(invocation log) error = %v", err)
	}
	if result.Status != "success" {
		t.Fatalf("result = %#v\ninvocations:\n%s", result, string(logBytes))
	}
	assertOrderedText(t, string(logBytes),
		`"argv":["api","repos/acme/looper/pulls/42/reviews","--method","POST","--input","-","--include"]`,
		`"argv":["pr","merge","42","--repo","acme/looper","--auto","--squash","--match-head-commit","abc123"]`,
	)
	if !strings.Contains(string(logBytes), criteriaVerificationHeading) {
		t.Fatalf("invocation log missing criteria verification heading:\n%s", string(logBytes))
	}
}

func TestReviewerAutoMergeCriteriaFailWithFakeGH(t *testing.T) {
	bins := harness.MustBinaries(t)
	fakeGH := harness.NewFakeGH(t, bins, harness.GHSchema{JSONFieldAllowlist: map[string][]string{"pr view": {"number", "title", "body", "url", "state", "createdAt", "updatedAt", "closedAt", "isDraft", "reviewDecision", "labels", "headRefName", "baseRefName", "headRefOid", "baseRefOid", "author", "authorAssociation", "reviewRequests", "comments", "reviews", "statusCheckRollup", "mergeStateStatus"}}})
	for key, value := range fakeGH.EnvMap() {
		t.Setenv(key, value)
	}
	fakeGH.WriteState(t, harness.GHState{
		Commands: map[string]any{"pr diff": map[string]any{"stdout": json.RawMessage(`"diff --git a/app.go b/app.go\n@@ -1,1 +1,1 @@\n-old\n+new\n"`)}},
		Routes: map[string]any{
			"repos/acme/looper/issues/358": json.RawMessage(`{"number":358,"title":"Auto merge","body":"## Acceptance criteria\n- ship app change\n- add tests\n","html_url":"https://example.test/issues/358","state":"open","created_at":"2026-05-14T12:00:00Z","updated_at":"2026-05-14T12:00:00Z","user":{"login":"octo"},"labels":[{"name":"triaged"},{"name":"dispatch/plan"}]}`),
		},
		CurrentUserLogin: "reviewer",
		PullRequests: map[string]harness.GHPullRequest{
			"acme/looper#42": {Number: 42, Repo: "acme/looper", Title: "Review me", Body: "Implements feature.\n\nCloses #358", State: "OPEN", Labels: []string{"looper:worker-ready"}, HeadRefName: "feature/review-me", BaseRefName: "main", HeadSHA: "abc123", BaseSHA: "base123", Author: "octocat", ReviewRequests: []string{"reviewer"}},
		},
	})

	fixture := newRunnerFixture(t)
	ctx := context.Background()
	repoPath := t.TempDir()
	baseBranch := "main"
	if err := fixture.repos.Projects.Upsert(ctx, storage.ProjectRecord{ID: "project_1", Name: "Looper", RepoPath: repoPath, BaseBranch: &baseBranch, CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	github := reviewerIntegrationGatewayAdapter{Gateway: githubinfra.New(githubinfra.Options{GHPath: fakeGH.Path, CWD: repoPath, Now: fixture.now})}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "No actionable findings", Stdout: `__LOOPER_RESULT__={"summary":"No actionable findings"}`, ParseStatus: "parsed"}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, ReviewEvents: config.ReviewerReviewEventsConfig{Clean: config.ReviewerReviewEventApprove}, LoopConfig: testReviewerLoopConfig(), CustomInstructions: reviewerAutoMergeTestConfig(t), CriteriaVerifier: stubCriteriaVerifier{responses: map[criteria.AcceptanceCriterion]criteria.CriterionAssessment{
		"ship app change": {Verdict: criteria.VerdictPass, Justification: "present in diff", Evidence: []criteria.Evidence{{FilePath: "app.go", StartLine: 1, EndLine: 1}}},
		"add tests":       {Verdict: criteria.VerdictFail, Justification: "no test change in diff"},
	}}})
	repo := "acme/looper"
	prNumber := int64(42)
	metadata := `{"followUpdates":true,"loop":{"enabled":true}}`
	loop := storage.LoopRecord{ID: "loop_fakegh_auto_merge_fail", Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: "queued", MetadataJSON: &metadata, CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}
	if err := fixture.repos.Loops.Upsert(ctx, loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	queue, err := runner.enqueue(ctx, enqueueInput{ProjectID: "project_1", LoopID: loop.ID, Repo: repo, PRNumber: prNumber})
	if err != nil {
		t.Fatalf("enqueue() error = %v", err)
	}
	claimed, err := fixture.repos.Queue.ClaimNextOfType(ctx, fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claimed == nil || claimed.ID != queue.ID {
		t.Fatalf("ClaimNextOfType() = (%#v, %v), want queue item %s", claimed, err, queue.ID)
	}
	result, err := runner.ProcessClaimedItem(ctx, *claimed)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	logBytes, err := os.ReadFile(fakeGH.InvocationLog)
	if err != nil {
		t.Fatalf("ReadFile(invocation log) error = %v", err)
	}
	if result.Status != "success" {
		t.Fatalf("result = %#v\ninvocations:\n%s", result, string(logBytes))
	}
	assertOrderedText(t, string(logBytes),
		`"argv":["api","repos/acme/looper/pulls/42/reviews","--method","POST","--input","-","--include"]`,
		`"argv":["api","repos/acme/looper/issues/358/labels/triaged","--method","DELETE"]`,
		`"argv":["api","repos/acme/looper/issues/358/labels/dispatch%2Fplan","--method","DELETE"]`,
	)
	if strings.Contains(string(logBytes), `"argv":["pr","merge","42"`) {
		t.Fatalf("criteria-fail path unexpectedly enabled auto-merge:\n%s", string(logBytes))
	}
	if !strings.Contains(string(logBytes), criteriaFailCommentMarker) && !strings.Contains(string(logBytes), "criteria-fail") {
		t.Fatalf("invocation log missing criteria-fail marker:\n%s", string(logBytes))
	}
}

func assertOrderedText(t *testing.T, text string, parts ...string) {
	t.Helper()
	index := 0
	for _, part := range parts {
		next := strings.Index(text[index:], part)
		if next < 0 {
			t.Fatalf("missing ordered text %q in:\n%s", part, text)
		}
		index += next + len(part)
	}
}

type reviewerIntegrationGatewayAdapter struct{ *githubinfra.Gateway }

func (a reviewerIntegrationGatewayAdapter) ListOpenPullRequests(ctx context.Context, input ListOpenPullRequestsInput) ([]PullRequestSummary, error) {
	prs, err := a.Gateway.ListOpenPullRequests(ctx, githubinfra.ListOpenPullRequestsInput{Repo: input.Repo, CWD: input.CWD, Limit: input.Limit, Label: input.Label, Labels: input.Labels})
	if err != nil {
		return nil, err
	}
	out := make([]PullRequestSummary, 0, len(prs))
	for _, pr := range prs {
		out = append(out, PullRequestSummary{Number: pr.Number, Title: pr.Title, State: pr.State, IsDraft: pr.IsDraft, ReviewDecision: pr.ReviewDecision, Labels: append([]string(nil), pr.Labels...), HeadSHA: pr.HeadSHA, BaseSHA: pr.BaseSHA, HasConflicts: pr.HasConflicts, Author: pr.Author, ReviewRequests: append([]string(nil), pr.ReviewRequests...), Reviews: pr.Reviews})
	}
	return out, nil
}

func (a reviewerIntegrationGatewayAdapter) ListReviewRequestedPullRequests(ctx context.Context, input ListReviewRequestedPullRequestsInput) ([]PullRequestSummary, error) {
	prs, err := a.Gateway.ListReviewRequestedPullRequests(ctx, githubinfra.ListReviewRequestedPullRequestsInput{Repo: input.Repo, CWD: input.CWD, Limit: input.Limit, Reviewer: input.Reviewer})
	if err != nil {
		return nil, err
	}
	out := make([]PullRequestSummary, 0, len(prs))
	for _, pr := range prs {
		out = append(out, PullRequestSummary{Number: pr.Number, Title: pr.Title, State: pr.State, IsDraft: pr.IsDraft, ReviewDecision: pr.ReviewDecision, Labels: append([]string(nil), pr.Labels...), HeadSHA: pr.HeadSHA, BaseSHA: pr.BaseSHA, HasConflicts: pr.HasConflicts, Author: pr.Author, ReviewRequests: append([]string(nil), pr.ReviewRequests...), Reviews: pr.Reviews})
	}
	return out, nil
}

func (a reviewerIntegrationGatewayAdapter) GetCurrentUserLogin(ctx context.Context, cwd string) (string, error) {
	return a.Gateway.GetCurrentUserLogin(ctx, cwd)
}

func (a reviewerIntegrationGatewayAdapter) ViewPullRequest(ctx context.Context, input ViewPullRequestInput) (PullRequestDetail, error) {
	detail, err := a.Gateway.ViewPullRequest(ctx, githubinfra.ViewPullRequestInput{Repo: input.Repo, PRNumber: input.PRNumber, CWD: input.CWD})
	if err != nil {
		return PullRequestDetail{}, err
	}
	diff, _ := a.Gateway.GetPullRequestDiff(ctx, githubinfra.GetPullRequestDiffInput{Repo: input.Repo, PRNumber: input.PRNumber, CWD: input.CWD})
	issueComments := make([]map[string]any, 0, len(detail.IssueComments))
	for _, comment := range detail.IssueComments {
		issueComments = append(issueComments, map[string]any{"body": comment.Body})
	}
	return PullRequestDetail{Number: detail.Number, Title: detail.Title, Body: detail.Body, State: detail.State, IsDraft: detail.IsDraft, ReviewDecision: detail.ReviewDecision, Labels: append([]string(nil), detail.Labels...), HeadSHA: detail.HeadSHA, BaseSHA: detail.BaseSHA, HeadRefName: detail.HeadRefName, BaseRefName: detail.BaseRefName, Author: detail.Author, ReviewRequests: append([]string(nil), detail.ReviewRequests...), HasConflicts: detail.HasConflicts, Diff: diff, Comments: detail.Comments, IssueComments: issueComments, Reviews: detail.Reviews}, nil
}

func (a reviewerIntegrationGatewayAdapter) GetPullRequestHeadSHA(ctx context.Context, input ViewPullRequestInput) (string, error) {
	return a.Gateway.GetPullRequestHeadSHA(ctx, githubinfra.ViewPullRequestInput{Repo: input.Repo, PRNumber: input.PRNumber, CWD: input.CWD})
}

func (a reviewerIntegrationGatewayAdapter) ViewIssue(ctx context.Context, input githubinfra.ViewIssueInput) (githubinfra.IssueDetail, error) {
	return a.Gateway.ViewIssue(ctx, input)
}

func (a reviewerIntegrationGatewayAdapter) GetRepositorySettings(ctx context.Context, input githubinfra.RepositorySettingsInput) (githubinfra.RepositorySettings, error) {
	return a.Gateway.GetRepositorySettings(ctx, input)
}

func (a reviewerIntegrationGatewayAdapter) GetBranchProtection(ctx context.Context, input githubinfra.BranchProtectionInput) (githubinfra.BranchProtection, error) {
	return a.Gateway.GetBranchProtection(ctx, input)
}

func (a reviewerIntegrationGatewayAdapter) CapturePullRequestSnapshot(ctx context.Context, input CapturePullRequestSnapshotInput) (storage.PullRequestSnapshotRecord, error) {
	return a.Gateway.CapturePullRequestSnapshot(ctx, githubinfra.CapturePullRequestSnapshotInput{ProjectID: input.ProjectID, Repo: input.Repo, PRNumber: input.PRNumber, CWD: input.CWD, CapturedAt: input.CapturedAt})
}

func (a reviewerIntegrationGatewayAdapter) FindReviewMarker(ctx context.Context, input VerifyReviewMarkerInput) (ReviewMarkerResult, error) {
	found, err := a.Gateway.FindReviewMarker(ctx, githubinfra.VerifyReviewMarkerInput{Repo: input.Repo, PRNumber: input.PRNumber, Marker: input.Marker, AllowedReviewEvents: reviewEventsToStringsLocal(input.AllowedReviewEvents), AuthorLogin: input.AuthorLogin, AllowCleanComment: input.AllowCleanComment, CWD: input.CWD})
	if err != nil {
		return ReviewMarkerResult{}, err
	}
	return ReviewMarkerResult{Found: found.Found, Outcome: found.Outcome, Event: ReviewEvent(found.Event), AuthorLogin: found.AuthorLogin, Body: found.Body, InlineCommentBodies: append([]string(nil), found.InlineCommentBodies...)}, nil
}

func (a reviewerIntegrationGatewayAdapter) CreateIssueComment(ctx context.Context, input IssueCommentInput) (IssueCommentResult, error) {
	comment, err := a.Gateway.CreateIssueComment(ctx, githubinfra.IssueCommentInput{Repo: input.Repo, IssueNumber: input.IssueNumber, Body: input.Body, CWD: input.CWD})
	if err != nil {
		return IssueCommentResult{}, err
	}
	return IssueCommentResult{ID: comment.ID, URL: comment.URL}, nil
}

func (a reviewerIntegrationGatewayAdapter) SubmitReview(ctx context.Context, input githubinfra.SubmitReviewInput) error {
	return a.Gateway.SubmitReview(ctx, input)
}

func (a reviewerIntegrationGatewayAdapter) EnableAutoMerge(ctx context.Context, input githubinfra.EnableAutoMergeInput) error {
	return a.Gateway.EnableAutoMerge(ctx, input)
}

func (a reviewerIntegrationGatewayAdapter) AddPullRequestReaction(ctx context.Context, input PullRequestReactionInput) error {
	return a.Gateway.AddPullRequestReaction(ctx, githubinfra.PullRequestReactionInput{Repo: input.Repo, PRNumber: input.PRNumber, Content: input.Content, CWD: input.CWD})
}

func (a reviewerIntegrationGatewayAdapter) RemovePullRequestReaction(ctx context.Context, input PullRequestReactionInput) error {
	return a.Gateway.RemovePullRequestReaction(ctx, githubinfra.PullRequestReactionInput{Repo: input.Repo, PRNumber: input.PRNumber, Content: input.Content, CWD: input.CWD})
}

func (a reviewerIntegrationGatewayAdapter) AddPullRequestLabels(ctx context.Context, input PullRequestLabelsInput) error {
	return a.Gateway.AddPullRequestLabels(ctx, githubinfra.PullRequestLabelsInput{Repo: input.Repo, PRNumber: input.PRNumber, Labels: input.Labels, CWD: input.CWD})
}

func (a reviewerIntegrationGatewayAdapter) RemovePullRequestLabels(ctx context.Context, input PullRequestLabelsInput) error {
	return a.Gateway.RemovePullRequestLabels(ctx, githubinfra.PullRequestLabelsInput{Repo: input.Repo, PRNumber: input.PRNumber, Labels: input.Labels, CWD: input.CWD})
}

func (a reviewerIntegrationGatewayAdapter) RemoveIssueLabels(ctx context.Context, input githubinfra.IssueLabelsInput) error {
	return a.Gateway.RemoveIssueLabels(ctx, input)
}

func (a reviewerIntegrationGatewayAdapter) ListReviewThreads(ctx context.Context, input ListReviewThreadsInput) ([]ReviewThread, error) {
	threads, err := a.Gateway.ListReviewThreads(ctx, githubinfra.ListReviewThreadsInput{Repo: input.Repo, PRNumber: input.PRNumber, CWD: input.CWD, Limit: input.Limit})
	if err != nil {
		return nil, err
	}
	out := make([]ReviewThread, 0, len(threads))
	for _, thread := range threads {
		comments := make([]ReviewThreadComment, 0, len(thread.Comments))
		for _, comment := range thread.Comments {
			comments = append(comments, ReviewThreadComment{ID: comment.ID, Body: comment.Body, Author: comment.Author, CreatedAt: comment.CreatedAt, UpdatedAt: comment.UpdatedAt, Path: comment.Path, Line: comment.Line, OriginalCommitOID: comment.OriginalCommitOID, CommitOID: comment.CommitOID, URL: comment.URL})
		}
		out = append(out, ReviewThread{ID: thread.ID, IsResolved: thread.IsResolved, Path: thread.Path, Line: thread.Line, URL: thread.URL, Comments: comments})
	}
	return out, nil
}

func (a reviewerIntegrationGatewayAdapter) AddReviewThreadReply(ctx context.Context, input AddReviewThreadReplyInput) error {
	return a.Gateway.AddReviewThreadReply(ctx, githubinfra.AddReviewThreadReplyInput{Repo: input.Repo, ThreadID: input.ThreadID, Body: input.Body, CWD: input.CWD})
}

func (a reviewerIntegrationGatewayAdapter) ResolveReviewThread(ctx context.Context, input ResolveReviewThreadInput) error {
	return a.Gateway.ResolveReviewThread(ctx, githubinfra.ResolveReviewThreadInput{Repo: input.Repo, ThreadID: input.ThreadID, CWD: input.CWD})
}

func reviewEventsToStringsLocal(events []ReviewEvent) []string {
	result := make([]string, 0, len(events))
	for _, event := range events {
		result = append(result, string(event))
	}
	return result
}
