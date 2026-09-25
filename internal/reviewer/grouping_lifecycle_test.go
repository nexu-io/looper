package reviewer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
)

func groupedReviewLifecycleFixture(t *testing.T) (*Runner, stepInput, *fakeGitHubGateway, *fakeAgentExecutor) {
	t.Helper()
	fixture := newRunnerFixture(t)
	worktree, base, _ := groupingTestRepoWithRename(t)
	if err := os.WriteFile(filepath.Join(worktree, "README.md"), []byte("second group\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var head string
	for _, args := range [][]string{{"add", "README.md"}, {"commit", "-qm", "docs: add second group"}, {"rev-parse", "HEAD"}} {
		out, err := exec.Command("git", append([]string{"-C", worktree}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		head = strings.TrimSpace(string(out))
	}
	ctx := context.Background()
	project, err := fixture.repos.Projects.GetByID(ctx, "project_1")
	if err != nil || project == nil {
		t.Fatalf("load project: %#v, %v", project, err)
	}
	metadata, err := json.Marshal(map[string]string{"worktreeRoot": filepath.Dir(worktree)})
	if err != nil {
		t.Fatal(err)
	}
	project.MetadataJSON = stringPtr(string(metadata))
	if err := fixture.repos.Projects.Upsert(ctx, *project); err != nil {
		t.Fatal(err)
	}
	loop := storage.LoopRecord{ID: "grouped_loop", Seq: 1, ProjectID: project.ID, Type: "reviewer", TargetType: "pull_request", Repo: stringPtr("acme/looper"), PRNumber: int64Ptr(42), Status: "running", CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}
	if err := fixture.repos.Loops.Upsert(ctx, loop); err != nil {
		t.Fatal(err)
	}
	run := storage.RunRecord{ID: "grouped_run", LoopID: loop.ID, Status: "running", StartedAt: fixture.nowISO(), CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}
	if err := fixture.repos.Runs.Upsert(ctx, run); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Roles.Reviewer.Behavior.RelatedFileGroups = config.ReviewerRelatedFileGroupsConfig{Enabled: true, MinChangedFiles: 1}
	cfg.Roles.Reviewer.Behavior.PublishMode = config.ReviewerPublishModeSummaryComment
	cfg.Roles.Reviewer.Behavior.ReviewEvents.Clean = config.ReviewerReviewEventComment
	completed := AgentResult{Status: "completed", Summary: "No actionable findings", Stdout: `__LOOPER_RESULT__={"summary":"No actionable findings","outcome":"clean","findings":[]}`}
	agent := &fakeAgentExecutor{results: []AgentResult{completed, completed, completed}}
	github := &fakeGitHubGateway{viewHeadSHA: head}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, AgentRuntime: string(config.AgentVendorCodex), CustomInstructions: &cfg})
	input := stepInput{Project: *project, Loop: loop, Run: run, Repo: "acme/looper", PRNumber: 42, Checkpoint: reviewerCheckpoint{
		Detail: &checkpointDetail{HeadRefName: "feature", BaseRefName: "main"}, Snapshot: &checkpointSnapshot{BaseSHA: base, HeadSHA: head},
		Worktree: &checkpointWorktree{Path: worktree, Branch: "feature", PreparedAt: fixture.nowISO()},
	}}
	return runner, input, github, agent
}

func TestGroupedReviewRestartsOnRemoteHeadDrift(t *testing.T) {
	for _, afterStarts := range []int{0, 1, 2} {
		t.Run(fmt.Sprintf("after_%d_groups", afterStarts), func(t *testing.T) {
			t.Parallel()
			runner, input, github, agent := groupedReviewLifecycleFixture(t)
			if afterStarts == 0 {
				github.viewHeadSHA = "new-head"
			}
			agent.onStart = func(AgentRunInput) {
				if len(agent.starts) == afterStarts {
					github.viewHeadSHA = "new-head"
				}
			}
			checkpoint, err := runner.executeStep(context.Background(), stepReview, input)
			var failure *loopError
			if !errors.As(err, &failure) || !failure.interrupted || checkpoint.ResumePolicy != "restart_from_discover" {
				t.Fatalf("checkpoint policy = %q, err = %v; want interrupted rediscovery", checkpoint.ResumePolicy, err)
			}
			if len(agent.starts) != afterStarts {
				t.Fatalf("started %d agents after remote drift, want only %d groups", len(agent.starts), afterStarts)
			}
			for _, start := range agent.starts {
				if start.Metadata["phase"] != "review-group" {
					t.Fatal("publish-capable final reviewer started on a stale head")
				}
			}
		})
	}
}

func TestGroupedReviewRenewsAgentBudget(t *testing.T) {
	t.Parallel()
	runner, input, _, agent := groupedReviewLifecycleFixture(t)
	runner.agentTimeout = time.Second
	agent.wait = func(ctx context.Context) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(400 * time.Millisecond):
			return nil
		}
	}
	checkpoint, err := runner.executeStep(context.Background(), stepReview, input)
	if err != nil || checkpoint.PendingReview == nil || len(agent.starts) != 3 {
		t.Fatalf("grouped pass = %v, pending = %#v, starts = %d; want two groups and final review", err, checkpoint.PendingReview, len(agent.starts))
	}
}

func TestGroupedReviewHonorsParentCancellation(t *testing.T) {
	t.Parallel()
	runner, input, _, agent := groupedReviewLifecycleFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	agent.onStart = func(AgentRunInput) { cancel() }
	agent.wait = func(ctx context.Context) error { return ctx.Err() }
	_, err := runner.executeStep(ctx, stepReview, input)
	if !errors.Is(err, context.Canceled) || len(agent.starts) != 1 {
		t.Fatalf("canceled grouped pass: err=%v, starts=%d", err, len(agent.starts))
	}
}

func TestGroupedReviewRetainsResumeContextAfterRetry(t *testing.T) {
	t.Parallel()
	runner, input, _, agent := groupedReviewLifecycleFixture(t)
	ctx := context.Background()
	for i, phase := range []string{"review", "review-group"} {
		record := storage.AgentExecutionRecord{
			ID: fmt.Sprintf("previous_%d", i), ProjectID: &input.Project.ID, LoopID: &input.Loop.ID, RunID: &input.Run.ID,
			Vendor: string(config.AgentVendorCodex), Status: "completed", MetadataJSON: stringPtr(fmt.Sprintf(`{"metadata":{"phase":%q}}`, phase)),
			StartedAt: fmt.Sprintf("2026-04-11T11:00:0%d.000Z", i), CreatedAt: input.Run.CreatedAt, UpdatedAt: input.Run.UpdatedAt,
		}
		if phase == "review" {
			record.NativeSessionID, record.NativeResumeStatus = stringPtr("pending-review"), stringPtr("pending")
		}
		if err := runner.repos.AgentExecutions.Upsert(ctx, record); err != nil {
			t.Fatal(err)
		}
	}
	agent.results[0] = AgentResult{Status: "completed", Summary: "Grouped contract finding", Stdout: `__LOOPER_RESULT__={"summary":"Grouped contract finding","outcome":"blocking","findings":[{"title":"Grouped contract finding","body":"Introduced broken contract","disposition":"must_fix","severity":"blocking","scopeBasis":"introduced_regression","scopeEvidence":"changed call site"}]}`}
	if _, err := runner.executeStep(ctx, stepReview, input); err != nil {
		t.Fatal(err)
	}
	if len(agent.starts) != 3 {
		t.Fatalf("starts = %d, want two groups and final review", len(agent.starts))
	}
	final := agent.starts[2]
	for _, prompt := range []string{final.Prompt, final.NativeResumePrompt} {
		if !strings.Contains(prompt, "Related-file group plan") || !strings.Contains(prompt, "Grouped contract finding") {
			t.Fatalf("final review lost grouped context: %s", prompt)
		}
	}
	if !strings.Contains(final.NativeResumePrompt, "Continue the existing Looper reviewer review task") {
		t.Fatalf("lost pending session after a persisted group: %s", final.NativeResumePrompt)
	}
}
