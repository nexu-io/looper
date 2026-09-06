package reviewer

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/forge"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/reviewer/criteria"
	"github.com/nexu-io/looper/internal/storage"
)

// The live Forgejo merge endpoint checks required CI and the supplied head only
// for immediate merges. It consumes the review request when approval is submitted.
type forgejoImmediateMergeGateway struct {
	*fakeGitHubGateway
	checksReady       bool
	changeHeadAtMerge bool
	loseMergeResponse bool
	mergedHeads       []string
}

func (g *forgejoImmediateMergeGateway) SubmitReview(ctx context.Context, input githubinfra.SubmitReviewInput) error {
	if err := g.fakeGitHubGateway.SubmitReview(ctx, input); err != nil {
		return err
	}
	g.reviewRequests = []string{}
	row := g.reviews[len(g.reviews)-1]
	row["commit_id"] = input.CommitID
	return nil
}

func (g *forgejoImmediateMergeGateway) EnableAutoMerge(_ context.Context, input githubinfra.EnableAutoMergeInput) error {
	g.enableAutoMergeCalls = append(g.enableAutoMergeCalls, input)
	if g.changeHeadAtMerge {
		g.viewHeadSHA = "changed-head"
	}
	if !g.checksReady {
		return &forge.ForgejoHTTPError{StatusCode: http.StatusMethodNotAllowed, Message: "PR is not ready to be merged: required CI is pending"}
	}
	if input.HeadSHA != firstNonEmpty(g.viewHeadSHA, "abc123") {
		return &forge.ForgejoHTTPError{StatusCode: http.StatusConflict, Message: "head out of date"}
	}
	g.mergedHeads = append(g.mergedHeads, input.HeadSHA)
	if g.loseMergeResponse {
		g.viewState = "CLOSED"
		return fmt.Errorf("connection reset after merge response")
	}
	return nil
}

func TestForgejoAutoMergePublishRetryRetainsApprovalAndRejectsHeadDrift(t *testing.T) {
	for _, mode := range []string{"CI succeeds", "approval removed before retry", "head changes before retry", "head changes during merge request", "merge response lost", "merge response lost then head changes"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			fixture := newRunnerFixture(t)
			gateway := &forgejoImmediateMergeGateway{fakeGitHubGateway: &fakeGitHubGateway{
				author: "octocat", currentLogin: "reviewer", reviewMarkerMissing: true, reviewRequests: []string{"reviewer"},
				labels: []string{"looper:worker-ready"}, viewBody: "Closes #358", viewDiff: "diff --git a/app.go b/app.go\n@@ -1 +1 @@\n-old\n+new\n",
				issueDetail: githubinfra.IssueDetail{Number: 358, Body: "## Acceptance criteria\n- ship app change\n", Labels: []string{"triaged"}},
			}}
			cfg := reviewerAutoMergeTestConfig(t)
			cfg.Providers = []config.ProviderConfig{{ID: "fj", Kind: config.ProviderKindForgejo, BaseURL: "https://code.example", TokenEnv: stringPtr("FORGEJO_TOKEN")}}
			cfg.Projects = []config.ProjectRefConfig{{ID: "project_1", Provider: "fj", Repo: "acme/looper"}}
			cfg.Roles.Reviewer.Discovery.Triggers.RequireReviewRequest = true
			cfg.Roles.Reviewer.Discovery.Triggers.Labels = nil
			cfg.Roles.Reviewer.Behavior.Loop = testReviewerLoopConfig()
			agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "No actionable findings", Stdout: `__LOOPER_RESULT__={"summary":"No actionable findings"}`, ParseStatus: "parsed"}}}
			runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: gateway, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, CustomInstructions: cfg, CriteriaVerifier: stubCriteriaVerifier{responses: map[criteria.AcceptanceCriterion]criteria.CriterionAssessment{"ship app change": {Verdict: criteria.VerdictPass, Justification: "present in diff", Evidence: []criteria.Evidence{{FilePath: "app.go", StartLine: 1, EndLine: 1}}}}}})
			ctx := context.Background()
			repo, number, metadata := "acme/looper", int64(42), `{"followUpdates":true,"loop":{"enabled":true}}`
			loop := storage.LoopRecord{ID: "loop_forgejo_ci", Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &number, Status: "queued", MetadataJSON: &metadata, CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}
			if err := fixture.repos.Loops.Upsert(ctx, loop); err != nil {
				t.Fatal(err)
			}
			if _, err := runner.enqueue(ctx, enqueueInput{ProjectID: loop.ProjectID, LoopID: loop.ID, Repo: repo, PRNumber: number}); err != nil {
				t.Fatal(err)
			}
			claimAndProcess := func() ProcessResult {
				t.Helper()
				claim, err := fixture.repos.Queue.ClaimNextOfType(ctx, fixture.nowISO(), "reviewer-worker", "reviewer")
				if err != nil || claim == nil {
					t.Fatalf("claim = %#v, %v", claim, err)
				}
				result, err := runner.ProcessClaimedItem(ctx, *claim)
				if err != nil {
					t.Fatal(err)
				}
				return result
			}
			first := claimAndProcess()
			if first.Status != "failed" || first.FailureKind != FailureRetryableAfterResume || len(gateway.enableAutoMergeCalls) != 1 || len(gateway.mergedHeads) != 0 {
				t.Fatalf("pending CI = %#v, merge attempts=%d, merged=%v", first, len(gateway.enableAutoMergeCalls), gateway.mergedHeads)
			}
			persisted, err := fixture.repos.Loops.GetByID(ctx, loop.ID)
			if err != nil || persisted == nil || strings.Contains(derefString(persisted.MetadataJSON), "lastPublishedHeadSha") {
				t.Fatalf("merge failure recorded publish success: %#v, %v", persisted, err)
			}
			prior, err := fixture.repos.Runs.GetByID(ctx, first.RunID)
			if err != nil || prior == nil {
				t.Fatalf("first run = %#v, %v", prior, err)
			}
			pending := parseCheckpoint(prior.CheckpointJSON)
			if pending.PendingReview == nil || !pending.PendingReview.CleanNoop || pending.PendingReview.HeadSHA != "abc123" || derefString(prior.LastCompletedStep) != string(stepReview) {
				t.Fatalf("lost publish checkpoint: %#v", pending)
			}
			gateway.checksReady = true
			if mode == "head changes before retry" {
				gateway.viewHeadSHA = "changed-head"
			}
			if mode == "approval removed before retry" {
				gateway.reviews = nil
			}
			gateway.changeHeadAtMerge = mode == "head changes during merge request"
			gateway.loseMergeResponse = strings.HasPrefix(mode, "merge response lost")
			fixture.advance(time.Hour)
			second := claimAndProcess()
			if len(agent.starts) != 1 || len(gateway.submitReviewCalls) != 1 {
				t.Fatalf("retry reran review: agents=%d approvals=%d", len(agent.starts), len(gateway.submitReviewCalls))
			}
			switch mode {
			case "CI succeeds":
				if second.Status != "success" || len(gateway.enableAutoMergeCalls) != 2 || len(gateway.mergedHeads) != 1 || gateway.mergedHeads[0] != "abc123" {
					t.Fatalf("CI completion = %#v, attempts=%d merged=%v", second, len(gateway.enableAutoMergeCalls), gateway.mergedHeads)
				}
			case "head changes before retry":
				if second.Status != "skipped" || !strings.Contains(second.Summary, "head changed") || len(gateway.enableAutoMergeCalls) != 1 || len(gateway.mergedHeads) != 0 {
					t.Fatalf("head drift = %#v, attempts=%d merged=%v", second, len(gateway.enableAutoMergeCalls), gateway.mergedHeads)
				}
			case "approval removed before retry":
				if second.Status != "skipped" || !strings.Contains(second.Summary, "not requested") || len(gateway.enableAutoMergeCalls) != 1 || len(gateway.mergedHeads) != 0 {
					t.Fatalf("removed approval authorized a merge: %#v, attempts=%d merged=%v", second, len(gateway.enableAutoMergeCalls), gateway.mergedHeads)
				}
			case "head changes during merge request":
				if second.Status != "failed" || !strings.Contains(second.Summary, "head out of date") || len(gateway.enableAutoMergeCalls) != 2 || len(gateway.mergedHeads) != 0 {
					t.Fatalf("atomic head rejection = %#v, attempts=%d merged=%v", second, len(gateway.enableAutoMergeCalls), gateway.mergedHeads)
				}
			case "merge response lost", "merge response lost then head changes":
				if second.Status != "failed" || len(gateway.mergedHeads) != 1 {
					t.Fatalf("lost response = %#v, merged=%v", second, gateway.mergedHeads)
				}
				if mode == "merge response lost then head changes" {
					gateway.viewHeadSHA = "changed-head"
				}
				fixture.advance(time.Minute)
				third := claimAndProcess()
				if third.Status != "skipped" || len(gateway.enableAutoMergeCalls) != 2 || len(gateway.submitReviewCalls) != 1 || len(agent.starts) != 1 {
					t.Fatalf("merged PR retry = %#v, attempts=%d approvals=%d agents=%d", third, len(gateway.enableAutoMergeCalls), len(gateway.submitReviewCalls), len(agent.starts))
				}
				if mode == "merge response lost then head changes" && !strings.Contains(third.Summary, "head changed") {
					t.Fatalf("new head falsely accepted old approval: %#v", third)
				}
			}
			persisted, err = fixture.repos.Loops.GetByID(ctx, loop.ID)
			if err != nil || persisted == nil {
				t.Fatalf("final loop = %#v, %v", persisted, err)
			}
			if strings.Contains(derefString(persisted.MetadataJSON), `"lastPublishedHeadSha":"changed-head"`) {
				t.Fatalf("unreviewed head recorded as published: %s", derefString(persisted.MetadataJSON))
			}
		})
	}
}
