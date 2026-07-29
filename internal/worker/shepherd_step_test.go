package worker

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
)

// Stage A (inert surface): stepShepherd is OFF the linear workerStepSequence so
// the normal impl flow still ends at open-pr (inert). stepsFrom special-cases it
// to just [stepShepherd] — never defaulting to the whole sequence (the v1 bug
// where forcing an unknown start step re-ran the worker from prepare-work) — and
// nextWorkerStep=="" so a shepherd run does exactly one pass per enqueue.

func TestStepShepherdIsTerminalSelfRepeating(t *testing.T) {
	for _, s := range workerStepSequence {
		if s == stepShepherd {
			t.Fatalf("stepShepherd must NOT be in the linear workerStepSequence (breaks inert normal flow): %v", workerStepSequence)
		}
	}
	if got := workerStepSequence[len(workerStepSequence)-1]; got != stepOpenPR {
		t.Fatalf("normal flow must still end at open-pr, got %v", got)
	}
	if got := stepsFrom(stepShepherd); len(got) != 1 || got[0] != stepShepherd {
		t.Fatalf("stepsFrom(stepShepherd) = %v, want [shepherd]", got)
	}
	if got := nextWorkerStep(stepShepherd); got != "" {
		t.Fatalf("nextWorkerStep(stepShepherd) = %q, want \"\"", got)
	}
	// asWorkerStep must recognize shepherd (off-sequence) so a resumed shepherd
	// run whose prior pass completed "shepherd" does not fail createRunContext.
	if got := asWorkerStep("shepherd"); got != stepShepherd {
		t.Fatalf("asWorkerStep(shepherd) = %q, want stepShepherd", got)
	}
	// the normal flow's last step still resolves without shepherd tacked on
	if got := stepsFrom(stepOpenPR); len(got) != 1 || got[0] != stepOpenPR {
		t.Fatalf("stepsFrom(open-pr) = %v, want [open-pr] (shepherd not appended)", got)
	}
}

// Stage B (durable-marker control): once $.shepherd.active is set and the
// checkpoint has a PR, the start-step resolver forces stepShepherd REGARDLESS of
// loop.Status or the failed/interrupted resume gate — the v2 bug was keying on
// loop.Status, which the failure path moves to queued/paused. Here the prior run
// FAILED at validate (which would normally resume mid-sequence), yet the marker
// still routes the next run to stepShepherd and carries the PR forward.
func TestCreateRunContextForcesShepherdViaMarker(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, Logger: fixture.logger, Now: fixture.now})

	meta, err := loops.WriteShepherd(nil, loops.Shepherd{Active: true, Phase: "reviewing"})
	if err != nil {
		t.Fatalf("WriteShepherd() error = %v", err)
	}
	prTargetID := "pr:acme/looper:101"
	prNumber := int64(101)
	nowISO := fixture.nowISO()
	loop := storage.LoopRecord{ID: "loop_shepherd_1", Seq: 7, ProjectID: "project_1", Type: "worker", TargetType: "pull_request", TargetID: &prTargetID, Repo: stringPtr("acme/looper"), PRNumber: &prNumber, Status: "queued", MetadataJSON: &meta, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	checkpointJSON := mustMarshalJSON(workerCheckpoint{
		Work:           &workerInput{Title: "Worker task", Repo: "acme/looper", IssueNumber: 42, ExecutionMode: "create-pr"},
		ClaimedLockKey: "pr:acme/looper:101",
		Worktree:       &checkpointWorktree{ID: "wt_1", Path: filepath.Join(t.TempDir(), "wt"), Branch: "looper/feature"},
		PullRequest:    &checkpointPullPR{Number: 101, URL: "https://example/pr/101"},
	})
	if err := fixture.repos.Runs.Upsert(context.Background(), storage.RunRecord{ID: "run_prev", LoopID: "loop_shepherd_1", Status: "failed", CurrentStep: stringPtr(string(stepValidate)), LastCompletedStep: stringPtr(string(stepExecute)), CheckpointJSON: &checkpointJSON, StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}

	rc, err := runner.createRunContext(context.Background(), loop)
	if err != nil {
		t.Fatalf("createRunContext() error = %v", err)
	}
	if rc.StartStep != stepShepherd {
		t.Fatalf("StartStep = %q, want shepherd (durable marker must override a failed-at-validate resume)", rc.StartStep)
	}
	if rc.Checkpoint.PullRequest == nil || rc.Checkpoint.PullRequest.Number != 101 {
		t.Fatalf("shepherd run lost the PR checkpoint: %#v", rc.Checkpoint.PullRequest)
	}
}

// The shepherd prompt MUST forbid the bot from ever merging or self-approving —
// the final merge is a human colleague's action. This is a hard product rule
// (feedback_bot_never_merges_human_merges), guarded here so a prompt edit can't
// silently reintroduce a self-merge.
func TestBuildShepherdPromptForbidsMergeAndSelfApprove(t *testing.T) {
	p := buildShepherdPrompt(workerInput{Repo: "acme/looper"}, 101, false)
	for _, must := range []string{"NEVER merge", "gh pr merge", "auto", "NEVER submit an approving review", "human"} {
		if !strings.Contains(p, must) {
			t.Fatalf("shepherd prompt missing guardrail %q:\n%s", must, p)
		}
	}
	// sanity: it references the actual PR
	if !strings.Contains(p, "acme/looper#101") {
		t.Fatalf("shepherd prompt does not reference the PR: %s", p)
	}
	// session-lost path tells the agent to rebuild from the diff
	if !strings.Contains(buildShepherdPrompt(workerInput{Repo: "a/b"}, 1, true), "could not be recovered") {
		t.Fatal("session-lost prompt missing rebuild-from-diff instruction")
	}
}

// Stage E (gating, integration): with ShepherdEnabled and the issue carrying
// looper:auto, a completed create-pr flow enters the shepherding steady state
// (marker active, status shepherding) instead of completing — so the reconciler
// drives the PR to merge. Default-off / no-label paths are covered by the
// unchanged create-pr flow tests staying "completed".
func TestProcessClaimedItemEntersShepherdingUnderLooperAuto(t *testing.T) {
	fixture := newRunnerFixture(t)
	git := &fakeGitGateway{createResult: CreateWorktreeResult{WorktreePath: filepath.Join(t.TempDir(), "wt"), Branch: "looper/feature", BaseBranch: "main", HeadSHA: "abc123", WorktreeID: "worktree_1"}}
	github := &fakeGitHubGateway{createPRResult: CreatePullRequestResult{Number: 101, URL: "https://example/pr/101"}, issueDetail: IssueDetail{Number: 27, Title: "t", State: "open", Labels: []string{"looper:auto"}}}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "done", Stdout: "ok", ParseStatus: "parsed"}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, AllowAutoCommit: true, AllowAutoPush: true, ShepherdEnabled: true, OpenPRStrategy: config.OpenPRStrategyAllDone})

	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "worker-1", "worker")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNextOfType() = (%#v, %v)", claim, err)
	}
	if _, err := runner.ProcessClaimedItem(context.Background(), *claim); err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	loop, err := fixture.repos.Loops.GetByID(context.Background(), "loop_worker_1")
	if err != nil || loop == nil {
		t.Fatalf("Loops.GetByID() = (%#v, %v)", loop, err)
	}
	if loop.Status != "shepherding" {
		t.Fatalf("loop status = %q, want shepherding (looper:auto + ShepherdEnabled must not complete)", loop.Status)
	}
	if !loops.ShepherdActive(loop.MetadataJSON) {
		t.Fatalf("$.shepherd.active not set: %v", derefStr(loop.MetadataJSON))
	}
}

func TestInterruptedOpenPRAdoptsExistingPRBeforeRecoveringWorktree(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	branch := buildWorkerBranchName(workerInput{Title: "Implement worker loop", Repo: "acme/looper", BaseBranch: "main", ExecutionMode: "create-pr", IssueNumber: 27}, "loop_worker_1")
	missingWorktree := filepath.Join(t.TempDir(), "deleted-worktree")
	checkpointJSON := mustMarshalJSON(workerCheckpoint{
		Work:       &workerInput{Title: "Implement worker loop", Repo: "acme/looper", BaseBranch: "main", ExecutionMode: "create-pr", IssueNumber: 27},
		Worktree:   &checkpointWorktree{ID: "wt_deleted", Path: missingWorktree, Branch: branch, BaseBranch: "main", HeadSHA: "abc123"},
		Execution:  &checkpointExecution{Status: "completed", ParseStatus: "parsed", Summary: "implemented"},
		Validation: &ValidationResult{Passed: true, Summary: "passed"},
	})
	if err := fixture.repos.Runs.Upsert(context.Background(), storage.RunRecord{ID: "run_interrupted_open_pr", LoopID: "loop_worker_1", Status: "failed", CurrentStep: stringPtr(string(stepOpenPR)), LastCompletedStep: stringPtr(string(stepValidate)), CheckpointJSON: &checkpointJSON, StartedAt: fixture.nowISO(), CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}

	git := &fakeGitGateway{}
	github := &fakeGitHubGateway{
		openPRs:     []PullRequestSummary{{Number: 201, URL: "https://example/pr/201", State: "OPEN", HeadRefName: branch, BaseRefName: "main"}},
		prDetail:    PullRequestDetail{Number: 201, URL: "https://example/pr/201", State: "OPEN", HeadRefName: branch, BaseRefName: "main", Body: "## Summary\n\nExisting PR"},
		issueDetail: IssueDetail{Number: 27, Title: "Implement worker loop", State: "OPEN"},
	}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now, AllowAutoCommit: true, AllowAutoPush: true, ShepherdEnabled: true, OpenPRStrategy: config.OpenPRStrategyAllDone})

	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "worker-1", "worker")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNextOfType() = (%#v, %v), want claimed item", claim, err)
	}
	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "success" || result.PullRequestNumber != 201 {
		t.Fatalf("result = %#v, want adopted PR 201", result)
	}
	if len(git.restoreCalls) != 0 || len(git.createCalls) != 0 || len(git.pushCalls) != 0 || len(github.createPRCalls) != 0 {
		t.Fatalf("restore/create-worktree/push/create-pr = %d/%d/%d/%d, want 0/0/0/0", len(git.restoreCalls), len(git.createCalls), len(git.pushCalls), len(github.createPRCalls))
	}

	loop, err := fixture.repos.Loops.GetByID(context.Background(), "loop_worker_1")
	if err != nil || loop == nil {
		t.Fatalf("Loops.GetByID() = (%#v, %v), want loop", loop, err)
	}
	if loop.Status != "shepherding" || loop.PRNumber == nil || *loop.PRNumber != 201 || !loops.ShepherdActive(loop.MetadataJSON) {
		t.Fatalf("loop = %#v, want durable PR 201 in shepherding", loop)
	}
	queue, err := fixture.repos.Queue.GetByID(context.Background(), claim.ID)
	if err != nil || queue == nil {
		t.Fatalf("Queue.GetByID() = (%#v, %v), want queue item", queue, err)
	}
	if queue.PRNumber == nil || *queue.PRNumber != 201 || queue.TargetID != "pr:acme/looper:201" {
		t.Fatalf("queue = %#v, want retargeted PR 201", queue)
	}
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
