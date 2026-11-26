package fixer

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
)

func TestDiscoverPullRequestsSkipsForks(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	detail := PullRequestDetail{Number: 42, State: "OPEN", HeadSHA: "fork-head", IsCrossRepository: true, HasConflicts: true}
	github := &fakeGitHubGateway{listOpen: []PullRequestSummary{{Number: 42, State: "OPEN"}}, viewResponses: []PullRequestDetail{detail}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Logger: fixture.logger, Now: fixture.now})
	result, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Skipped != 1 || len(result.QueueItems) != 0 || len(result.CreatedLoopIDs) != 0 {
		t.Fatalf("discovery = %#v, want fork skipped before queue/worktree creation", result)
	}
	// Simulate a queue created before fork detection was available.
	github.viewResponses[0].IsCrossRepository = false
	legacy, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil || len(legacy.QueueItems) != 1 {
		t.Fatalf("legacy discovery = %#v, err = %v", legacy, err)
	}
	item := legacy.QueueItems[0]
	item.MaxAttempts = -1
	if err := fixture.repos.Queue.Upsert(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	github.viewResponses[0].IsCrossRepository = true
	if _, err := runner.ProcessNext(context.Background(), "test"); err != nil {
		t.Fatal(err)
	}
	queue, err := fixture.repos.Queue.GetByID(context.Background(), item.ID)
	if err != nil || queue.Status != "manual_intervention" {
		t.Fatalf("queue = %#v, err = %v, want fork parked despite unlimited retries", queue, err)
	}
	loop, err := fixture.repos.Loops.GetByID(context.Background(), *item.LoopID)
	if err != nil || loop.Status != "paused" || loop.NextRunAt != nil {
		t.Fatalf("loop = %#v, err = %v, want fork paused", loop, err)
	}
}

func TestResolveCommentsStopsOnPolicyDenial(t *testing.T) {
	t.Parallel()
	const denial = "Native GitHub API writes blocked: use Odcrew; unsupported operations require manual intervention."
	for _, tc := range []struct {
		name, action, failure string
		wantKind              QueueFailureKind
		wantResume            string
	}{
		{"fixed", "fixed", denial, FailureManualIntervention, loops.ResumePolicyManualIntervention},
		{"declined", "declined", denial, FailureManualIntervention, loops.ResumePolicyManualIntervention},
		{"transient_decline", "declined", "connection timed out", FailureRetryableAfterResume, loops.ResumePolicyReplayStep},
	} {
		t.Run(tc.name, func(t *testing.T) {
			github := &fakeGitHubGateway{
				viewResponses: []PullRequestDetail{{Number: 42, State: "OPEN", HeadSHA: "fix-head", Comments: []map[string]any{{"id": "c1", "threadId": "t1", "body": "please fix"}}}},
				threads:       []ReviewThread{{ID: "t1", Comments: []ReviewThreadComment{{ID: "c1", Body: "please fix"}}}},
				replyErr:      errors.New(tc.failure), resolveErr: errors.New(tc.failure),
			}
			runner := New(Options{GitHub: github})
			items := []FixItem{{Type: "comment", ID: "c1", ThreadID: "t1", Summary: "please fix"}}
			checkpoint := fixerCheckpoint{
				FixItems: items, FixItemsHash: hashFixItems(items),
				Validation:       &ValidationResult{Passed: true, HeadSHA: "fix-head"},
				Push:             &checkpointPush{Pushed: false, SkippedReason: "No new commits to push"},
				Repair:           &checkpointRepair{ReplyExplanations: []replyExplanationEntry{{FixItemID: "c1", ThreadID: "t1", Action: tc.action, Explanation: "This change would bypass the protected branch policy; the existing implementation correctly preserves that boundary.", ThreadCommentsObserved: hashReviewThreadComments(ReviewThread{Comments: []ReviewThreadComment{{ID: "c1"}}})}}},
				ReconcileCommits: &checkpointReconcileCommits{BaseHeadSHA: "base-head", FinalHeadSHA: "base-head", WorkingTreeClean: true},
			}
			updated, err := runner.runResolveCommentsStep(context.Background(), stepInput{Project: storage.ProjectRecord{RepoPath: t.TempDir()}, Repo: "acme/looper", PRNumber: 42, Checkpoint: checkpoint})
			var loopErr *loopError
			if !errors.As(err, &loopErr) || loopErr.kind != tc.wantKind {
				t.Fatalf("error = %v, want %s", err, tc.wantKind)
			}
			if tc.wantKind == FailureManualIntervention && !strings.Contains(err.Error(), tc.failure) {
				t.Fatalf("error = %v, want original policy denial", err)
			}
			if updated.ResumePolicy != tc.wantResume {
				t.Fatalf("resume policy = %q, want %s", updated.ResumePolicy, tc.wantResume)
			}
		})
	}
}
