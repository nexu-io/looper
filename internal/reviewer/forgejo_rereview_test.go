package reviewer

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
)

func TestForgejoRereviewPollingAndPublishRecovery(t *testing.T) {
	for _, mode := range []string{"lost response", "withdrawn before execution", "head changed before execution"} {
		t.Run(mode, func(t *testing.T) {
			fixture := newRunnerFixture(t)
			ctx := context.Background()
			const repo, reviewer, head = "acme/looper", "looper-reviewer", "same-head"
			const requestID int64 = 12345678
			prNumber := int64(42)
			prior := map[string]any{
				"id": int64(100), "state": "COMMENTED", "author": map[string]any{"login": reviewer},
				"commit": map[string]any{"oid": head}, "body": "<!-- looper:review id=reviewer:loop_rereview:same-head head=same-head outcome=non_blocking -->",
			}
			request := map[string]any{"id": requestID, "state": "REQUEST_REVIEW", "author": map[string]any{"login": reviewer}}
			remote := &fakeGitHubGateway{currentLogin: reviewer, viewHeadSHA: head, reviewMarkerMissing: true}
			update := func(requested []string, reviews []map[string]any) {
				remote.reviewRequests, remote.reviews = requested, reviews
				remote.listOpenByLabel = map[string][]PullRequestSummary{
					"": {{Number: prNumber, State: "open", HeadSHA: remote.viewHeadSHA, Author: "alice", ReviewRequests: requested, Reviews: reviews}},
				}
			}
			update(nil, []map[string]any{prior})
			cfg, err := config.DefaultConfig(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			cfg.Providers = []config.ProviderConfig{{ID: "fj", Kind: config.ProviderKindForgejo, BaseURL: "https://code.example", TokenEnv: stringPtr("FORGEJO_TOKEN")}}
			cfg.Projects = []config.ProjectRefConfig{{ID: "project_1", Provider: "fj", Repo: repo}}
			cfg.Roles.Reviewer.Discovery.AutoDiscovery = true
			cfg.Roles.Reviewer.Discovery.Triggers.RequireReviewRequest = false
			cfg.Roles.Reviewer.Discovery.Triggers.Labels = nil
			cfg.Roles.Reviewer.Behavior.Loop = testReviewerLoopConfig()
			cfg.Roles.Reviewer.Behavior.ReviewEvents.Clean = config.ReviewerReviewEventApprove
			cfg.Roles.Reviewer.Behavior.Loop.QuietPeriodSeconds = 0
			agent := &fakeAgentExecutor{results: []AgentResult{{}}, waitErr: errors.New("lost Forgejo response")}
			agent.onStart = func(input AgentRunInput) {
				id := fmt.Sprintf("reviewer:loop_rereview:%s:rereview:%d", head, requestID)
				if input.IdempotencyKey != id || input.Metadata["forgejoRereviewMarker"] != id {
					t.Fatalf("unexpected publication authority: %q", input.IdempotencyKey)
				}
				submitted := map[string]any{
					"id": requestID + 1, "state": "COMMENTED", "author": map[string]any{"login": reviewer},
					"commit": map[string]any{"oid": head}, "body": fmt.Sprintf("Reviewed new evidence.\n\n<!-- looper:review id=%s head=%s outcome=non_blocking -->", id, head),
				}
				update(nil, []map[string]any{prior, submitted})
			}
			r := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: remote, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, CustomInstructions: &cfg})
			meta := `{"followUpdates":true,"lastPublishedHeadSha":"same-head","loop":{"enabled":true}}`
			loop := storage.LoopRecord{ID: "loop_rereview", Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Repo: stringPtr(repo), PRNumber: &prNumber, Status: "waiting", MetadataJSON: &meta, CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}
			if err := fixture.repos.Loops.Upsert(ctx, loop); err != nil {
				t.Fatal(err)
			}
			poll := func() []storage.QueueItemRecord {
				t.Helper()
				result, err := r.DiscoverPullRequests(ctx, DiscoveryInput{ProjectID: loop.ProjectID, Repo: repo})
				if err != nil {
					t.Fatal(err)
				}
				return result.QueueItems
			}
			if queued := poll(); len(queued) != 0 {
				t.Fatalf("already-reviewed SHA queued without a new request: %v", queued)
			}
			update([]string{reviewer}, []map[string]any{prior, request})
			first, next := poll(), poll()
			if len(first) != 1 || len(next) != 1 || first[0].ID != next[0].ID || forgejoRereviewIDFromQueue(first[0].PayloadJSON) != requestID || first[0].AvailableAt != next[0].AvailableAt {
				t.Fatalf("polls must reuse one queue item: %v, %v", first, next)
			}
			switch mode {
			case "withdrawn before execution":
				update(nil, []map[string]any{prior})
			case "head changed before execution":
				remote.viewHeadSHA = "new-head"
			}
			claim, err := fixture.repos.Queue.ClaimNextOfType(ctx, first[0].AvailableAt, "reviewer-worker", "reviewer")
			if err != nil || claim == nil {
				t.Fatalf("claim = %v, %v", claim, err)
			}
			result, err := r.ProcessClaimedItem(ctx, *claim)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "lost response" {
				if result.Status != "success" || len(agent.starts) != 1 || len(poll()) != 0 {
					t.Fatalf("lost response = %#v, agent starts = %d", result, len(agent.starts))
				}
				stored, err := fixture.repos.Loops.GetByID(ctx, loop.ID)
				if err != nil || stored == nil || !strings.Contains(derefString(stored.MetadataJSON), ":rereview:12345678") {
					t.Fatalf("publication identity was not recorded: %v, %v", stored, err)
				}
			} else if len(agent.starts) != 0 || !strings.Contains(result.Summary, "no longer current") {
				t.Fatalf("invalid request reached agent or lacked skip reason: %#v, starts = %d", result, len(agent.starts))
			}
		})
	}
}
