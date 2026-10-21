package reviewer

import (
	"context"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
)

func seedFollowUpLoop(t *testing.T, fixture *runnerFixture, seq int64, id, repo string) {
	t.Helper()
	nowISO := fixture.nowISO()
	prNumber := int64(42)
	metadata := `{"followUpdates":true}`
	loop := storage.LoopRecord{ID: id, Seq: seq, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: "completed", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
}

func followUpIntervalRunner(t *testing.T, fixture *runnerFixture, github *fakeGitHubGateway, interval string) *Runner {
	t.Helper()
	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Projects = []config.ProjectRefConfig{{ID: "project_1", Name: "Looper", RepoPath: t.TempDir()}}
	cfg.Roles.Reviewer.Discovery.FollowUpInterval = interval
	return New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now, DiscoveryPolicy: DiscoveryPolicy{AutoDiscovery: true}, CustomInstructions: &cfg})
}

func discoverOnce(t *testing.T, runner *Runner, repo string) {
	t.Helper()
	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: repo}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
}

func TestDiscoverPullRequestsSweepsFollowUpsOnlyOncePerInterval(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{reviewRequests: []string{"alice"}, currentLogin: "octocat"}
	runner := followUpIntervalRunner(t, fixture, github, "1h")
	seedFollowUpLoop(t, fixture, 1, "loop_follow", "acme/looper")

	discoverOnce(t, runner, "acme/looper")
	swept := github.viewCalls
	if swept == 0 {
		t.Fatalf("viewCalls = 0, want the first pass to sweep follow-up loops")
	}
	listed := len(github.listReviewRequestedCalls)

	fixture.current = fixture.current.Add(30 * time.Minute)
	discoverOnce(t, runner, "acme/looper")
	if github.viewCalls != swept {
		t.Fatalf("viewCalls = %d, want %d: a pass inside the interval must not sweep follow-up loops", github.viewCalls, swept)
	}
	if len(github.listReviewRequestedCalls) <= listed {
		t.Fatalf("review-requested listings = %d, want new review requests still discovered inside the interval", len(github.listReviewRequestedCalls))
	}

	fixture.current = fixture.current.Add(31 * time.Minute)
	discoverOnce(t, runner, "acme/looper")
	if github.viewCalls <= swept {
		t.Fatalf("viewCalls = %d, want a new sweep once the interval has elapsed", github.viewCalls)
	}
}

func TestDiscoverPullRequestsSweepsFollowUpsEveryPassWithoutInterval(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{reviewRequests: []string{"alice"}, currentLogin: "octocat"}
	runner := followUpIntervalRunner(t, fixture, github, "")
	seedFollowUpLoop(t, fixture, 1, "loop_follow", "acme/looper")

	discoverOnce(t, runner, "acme/looper")
	first := github.viewCalls
	discoverOnce(t, runner, "acme/looper")
	if first == 0 || github.viewCalls != 2*first {
		t.Fatalf("viewCalls = %d after two passes (first %d), want every pass to sweep", github.viewCalls, first)
	}
}

func TestDiscoverPullRequestsTracksFollowUpIntervalPerRepo(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{reviewRequests: []string{"alice"}, currentLogin: "octocat"}
	runner := followUpIntervalRunner(t, fixture, github, "1h")
	seedFollowUpLoop(t, fixture, 1, "loop_follow_a", "acme/looper")
	seedFollowUpLoop(t, fixture, 2, "loop_follow_b", "acme/other")

	discoverOnce(t, runner, "acme/looper")
	afterFirstRepo := github.viewCalls
	discoverOnce(t, runner, "acme/other")
	if github.viewCalls <= afterFirstRepo {
		t.Fatalf("viewCalls = %d, want the second repo swept on its own first pass", github.viewCalls)
	}
}
