package reviewer

import (
	"context"
	"testing"

	"github.com/nexu-io/looper/internal/infra/shell"
	"github.com/nexu-io/looper/internal/storage"
)

func seedCompletedFollowUpLoop(t *testing.T, fixture *runnerFixture, id string) storage.LoopRecord {
	t.Helper()
	nowISO := fixture.nowISO()
	repo := "acme/looper"
	prNumber := int64(42)
	metadata := `{"followUpdates":true}`
	loop := storage.LoopRecord{ID: id, Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: "completed", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	return loop
}

func assertFollowUpLoopRetired(t *testing.T, fixture *runnerFixture, runner *Runner, github *fakeGitHubGateway, loopID string) {
	t.Helper()
	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	persisted, err := fixture.repos.Loops.GetByID(context.Background(), loopID)
	if err != nil || persisted == nil {
		t.Fatalf("Loops.GetByID() = (%#v, %v), want loop", persisted, err)
	}
	if terminalReviewerLoopReason(*persisted) == "" {
		t.Fatalf("loop status = %q, want a terminal follow-up loop", persisted.Status)
	}
	followUps, err := runner.listFollowUpLoops(context.Background(), "project_1", "acme/looper")
	if err != nil {
		t.Fatalf("listFollowUpLoops() error = %v", err)
	}
	if len(followUps) != 0 {
		t.Fatalf("follow-up loops = %d, want the retired loop excluded", len(followUps))
	}
	views := github.viewCalls
	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("second DiscoverPullRequests() error = %v", err)
	}
	if github.viewCalls != views {
		t.Fatalf("second discovery viewed the PR again (%d -> %d), want the retired loop left alone", views, github.viewCalls)
	}
}

func TestDiscoverPullRequestsRetiresCompletedFollowUpForMergedPR(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{reviewRequests: []string{"alice"}, currentLogin: "octocat", viewState: "MERGED"}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})
	loop := seedCompletedFollowUpLoop(t, fixture, "loop_merged_follow")
	assertFollowUpLoopRetired(t, fixture, runner, github, loop.ID)
}

func TestDiscoverPullRequestsRetiresFollowUpForClosedDraftPR(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{reviewRequests: []string{"alice"}, currentLogin: "octocat", viewState: "CLOSED", viewDraft: true}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})
	loop := seedCompletedFollowUpLoop(t, fixture, "loop_closed_draft_follow")
	assertFollowUpLoopRetired(t, fixture, runner, github, loop.ID)
}

func TestDiscoverPullRequestsRetiresFollowUpForMissingPR(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	missing := &shell.CommandExecutionError{Message: "Command exited with code 1", Result: shell.Result{Stderr: "GraphQL: Could not resolve to a PullRequest with the number of 42. (repository.pullRequest)"}}
	github := &fakeGitHubGateway{reviewRequests: []string{"alice"}, currentLogin: "octocat", viewErrs: []error{missing, missing, missing}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})
	loop := seedCompletedFollowUpLoop(t, fixture, "loop_missing_follow")
	assertFollowUpLoopRetired(t, fixture, runner, github, loop.ID)
}

func TestDiscoverPullRequestsKeepsFollowUpOnTransientViewFailure(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	transient := &shell.CommandExecutionError{Message: "Command exited with code 1", Result: shell.Result{Stderr: `Post "https://api.github.com/graphql": dial tcp 198.18.0.2:443: i/o timeout`}}
	github := &fakeGitHubGateway{reviewRequests: []string{"alice"}, currentLogin: "octocat", viewErrs: []error{transient}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})
	loop := seedCompletedFollowUpLoop(t, fixture, "loop_transient_follow")

	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	persisted, err := fixture.repos.Loops.GetByID(context.Background(), loop.ID)
	if err != nil || persisted == nil {
		t.Fatalf("Loops.GetByID() = (%#v, %v), want loop", persisted, err)
	}
	if persisted.Status != "completed" || terminalReviewerLoopReason(*persisted) != "" {
		t.Fatalf("loop = %#v, want the follow-up loop kept after a transient view failure", persisted)
	}
}
