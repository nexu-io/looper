package fixer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
)

func TestFixerHITLParksResumesAndConsumesAnswer(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := newRunnerFixture(t)
	project, err := fixture.repos.Projects.GetByID(ctx, "project_1")
	if err != nil || project == nil {
		t.Fatalf("Projects.GetByID() = (%#v, %v)", project, err)
	}
	worktreeRoot := t.TempDir()
	worktreePath := filepath.Join(worktreeRoot, "wt")
	projectMetadata := `{"worktreeRoot":` + mustJSON(t, worktreeRoot) + `}`
	project.MetadataJSON = &projectMetadata

	repo := "acme/looper"
	prNumber := int64(42)
	projectID := project.ID
	loopID := "loop_fixer_hitl"
	targetID := buildPullRequestTargetID(repo, prNumber)
	loop := storage.LoopRecord{
		ID: loopID, Seq: 91, ProjectID: projectID, Type: "fixer",
		TargetType: "pull_request", TargetID: &targetID, Repo: &repo,
		PRNumber: &prNumber, Status: "running",
		CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO(),
	}
	if err := fixture.repos.Loops.Upsert(ctx, loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	run1 := storage.RunRecord{
		ID: "run_fixer_hitl_1", LoopID: loopID, Status: "running",
		CurrentStep: stringPtr(string(stepRepair)), StartedAt: fixture.nowISO(),
		LastCompletedStep: stringPtr(string(stepPrepareWorktree)),
		CreatedAt:         fixture.nowISO(), UpdatedAt: fixture.nowISO(),
	}
	if err := fixture.repos.Runs.Upsert(ctx, run1); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}
	queue := storage.QueueItemRecord{
		ID: "queue_fixer_hitl_1", ProjectID: &projectID, LoopID: &loopID,
		Type: "fixer", TargetType: "pull_request", TargetID: targetID,
		Repo: &repo, PRNumber: &prNumber, DedupeKey: "fixer:hitl:test",
		Priority: storage.QueuePriorityFixer, Status: "running",
		AvailableAt: fixture.nowISO(), MaxAttempts: 3,
		CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO(),
	}
	if err := fixture.repos.Queue.Upsert(ctx, queue); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}
	sessionID := "session-fixer-hitl"
	if err := fixture.repos.AgentExecutions.Upsert(ctx, storage.AgentExecutionRecord{
		ID: "agent_fixer_hitl_1", ProjectID: &projectID, LoopID: &loopID,
		RunID: &run1.ID, Vendor: "codex", Status: "completed",
		NativeSessionID: &sessionID, StartedAt: fixture.nowISO(),
		CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO(),
	}); err != nil {
		t.Fatalf("AgentExecutions.Upsert() error = %v", err)
	}

	needsHuman := `__LOOPER_RESULT__={"summary":"blocked on intent","review_thread_replies":[{"fixItemId":"c1","threadId":"t1","action":"needs_human","explanation":"The reviewer request conflicts with the documented PR intent; choose which authority should win."}]}`
	fixed := `__LOOPER_RESULT__={"summary":"applied decision","review_thread_replies":[{"fixItemId":"c1","threadId":"t1","action":"fixed","explanation":"Kept the documented PR behavior as directed."}]}`
	agent := &fakeAgentExecutor{results: []AgentResult{
		{Status: "completed", Summary: "blocked on intent", ParseStatus: "parsed", Stdout: needsHuman},
		{Status: "completed", Summary: "applied decision", ParseStatus: "parsed", Stdout: fixed},
	}}
	git := &fakeGitGateway{}
	github := &fakeGitHubGateway{
		reviews: []ReviewSummary{{ID: 10, State: "CHANGES_REQUESTED", Author: "reviewer-x"}},
	}
	runner := New(Options{
		DB: fixture.coordinator.DB(), Repos: fixture.repos,
		GitHub: github, Git: git,
		AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now,
		AgentRuntime: "codex", AllowAutoPush: true, AllowRiskyFixes: true, HITLEnabled: true,
	})
	checkpoint := fixerCheckpoint{
		Detail:       &checkpointDetail{HeadSHA: "head-1", HeadRefName: "feature/fix-42", BaseRefName: "main"},
		FixItems:     []FixItem{{Type: "conflict", ID: "conflict-1", Summary: "base merge conflict"}, {Type: "comment", ID: "c1", ThreadID: "t1", Summary: "conflicting request"}},
		FixItemsHash: "fix-items-1",
		Worktree:     &checkpointWorktree{Path: worktreePath, Branch: "feature/fix-42", PreparedAt: fixture.nowISO()},
	}
	step := stepInput{Project: *project, Loop: loop, Run: run1, QueueItem: queue, Repo: repo, PRNumber: prNumber, Checkpoint: checkpoint}

	parkedCheckpoint, err := runner.runRepairStep(ctx, step)
	awaiting, ok := asAwaitingHumanError(err)
	if !ok {
		t.Fatalf("runRepairStep() error = %v, want awaitingHumanError", err)
	}
	if !strings.Contains(agent.starts[0].Prompt, "Do not push") || agent.starts[0].NativeSessionID != "" {
		t.Fatalf("initial agent input = %#v, want local-only fresh turn", agent.starts[0])
	}
	if len(git.mergeBaseCalls) != 1 {
		t.Fatalf("initial base merge calls = %d, want 1", len(git.mergeBaseCalls))
	}
	badRun := run1
	badRun.ID = "run_fixer_hitl_bad_fk"
	badRun.LoopID = "missing_loop"
	if _, err := runner.suspendForHuman(ctx, step, badRun, parkedCheckpoint, awaiting); err == nil {
		t.Fatal("suspendForHuman(bad run) error = nil, want transaction rollback")
	}
	rolledBackLoop, _ := fixture.repos.Loops.GetByID(ctx, loopID)
	rolledBackQueue, _ := fixture.repos.Queue.GetByID(ctx, queue.ID)
	if rolledBackLoop.Status != "running" || rolledBackQueue.Status != "running" {
		t.Fatalf("failed park leaked partial state: loop=%s queue=%s", rolledBackLoop.Status, rolledBackQueue.Status)
	}
	if err := os.MkdirAll(filepath.Join(worktreePath, ".looper"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.looper) error = %v", err)
	}
	dismissPath := filepath.Join(worktreePath, ".looper", "dismiss.json")
	if err := os.WriteFile(dismissPath, []byte(`{"dismissals":[{"reviewer":"reviewer-x","reason":"stale pre-answer intent"}]}`), 0o644); err != nil {
		t.Fatalf("WriteFile(pre-answer dismiss.json) error = %v", err)
	}
	result, err := runner.suspendForHuman(ctx, step, run1, parkedCheckpoint, awaiting)
	if err != nil || result.Status != "awaiting_human" {
		t.Fatalf("suspendForHuman() = (%#v, %v)", result, err)
	}
	if _, err := os.Stat(dismissPath); !os.IsNotExist(err) {
		t.Fatalf("pre-answer dismissal intent still present after park: %v", err)
	}
	persistedLoop, _ := fixture.repos.Loops.GetByID(ctx, loopID)
	ask, ok := loops.ReadHITLAsk(persistedLoop.MetadataJSON)
	if !ok || ask.Status != "awaiting" || ask.SessionID != sessionID {
		t.Fatalf("parked ask = %#v, found=%v", ask, ok)
	}
	for _, option := range ask.Options {
		if option == "Provide different guidance" {
			t.Fatalf("parked ask includes directly submitted custom-guidance placeholder: %#v", ask.Options)
		}
	}
	persistedQueue, _ := fixture.repos.Queue.GetByID(ctx, queue.ID)
	persistedRun, _ := fixture.repos.Runs.GetByID(ctx, run1.ID)
	if persistedQueue.Status != "cancelled" || persistedRun.Status != "interrupted" {
		t.Fatalf("parked states: queue=%s run=%s", persistedQueue.Status, persistedRun.Status)
	}

	ask.Answer = "Keep the documented PR intent."
	ask.Status = "answered"
	metadata, err := loops.WriteHITLAsk(persistedLoop.MetadataJSON, ask)
	if err != nil {
		t.Fatalf("WriteHITLAsk() error = %v", err)
	}
	persistedLoop.MetadataJSON = &metadata
	persistedLoop.Status = "running"
	if err := fixture.repos.Loops.Upsert(ctx, *persistedLoop); err != nil {
		t.Fatalf("Loops.Upsert(answer) error = %v", err)
	}
	// Disabling HITL stops new asks, but must not revoke an already accepted
	// operator answer or re-enable agent-side remote mutation during its resume.
	runner.hitlEnabled = false
	resume, err := runner.createRunContext(ctx, *persistedLoop)
	if err != nil {
		t.Fatalf("createRunContext(resume) error = %v", err)
	}
	if !resume.Resumed || resume.StartStep != stepRepair ||
		resume.Checkpoint.Worktree == nil || resume.Checkpoint.Worktree.PreparedAt == "" {
		t.Fatalf("resume context = %#v, want direct repair with prepared worktree preserved", resume)
	}
	step.Loop = *persistedLoop
	step.Run = resume.Run
	step.Checkpoint = resume.Checkpoint
	resumed, err := runner.runRepairStep(ctx, step)
	if err != nil || resumed.Repair == nil {
		t.Fatalf("resumed runRepairStep() = (%#v, %v)", resumed.Repair, err)
	}
	if agent.starts[1].NativeSessionID != sessionID ||
		!strings.Contains(agent.starts[1].NativeResumePrompt, "Keep the documented PR intent") ||
		!strings.Contains(agent.starts[1].Prompt, "Do not push") {
		t.Fatalf("resume agent input = %#v", agent.starts[1])
	}
	finishedLoop, _ := fixture.repos.Loops.GetByID(ctx, loopID)
	consumed, _ := loops.ReadHITLAsk(finishedLoop.MetadataJSON)
	if consumed.Status != "consumed" {
		t.Fatalf("answer status = %q, want consumed", consumed.Status)
	}
	if len(git.prepareCalls) != 0 {
		t.Fatalf("PrepareWorktree calls = %d, want no reset-capable prepare on HITL resume", len(git.prepareCalls))
	}
	if len(git.mergeBaseCalls) != 1 {
		t.Fatalf("base merge calls after HITL resume = %d, want initial merge only", len(git.mergeBaseCalls))
	}

	// Simulate a daemon exit after the durable repair consumed the answer but
	// before the outer lifecycle marked stepRepair complete.
	crashedRun := resume.Run
	crashedRun.Status = "interrupted"
	crashedRun.CurrentStep = stringPtr(string(stepRepair))
	crashedRun.LastCompletedStep = stringPtr(string(stepPrepareWorktree))
	crashedRun.StartedAt = fixture.now().Add(time.Second).UTC().Format(time.RFC3339Nano)
	crashedCheckpointJSON := mustMarshalJSON(resumed)
	crashedRun.CheckpointJSON = &crashedCheckpointJSON
	crashedRun.UpdatedAt = fixture.nowISO()
	if err := fixture.repos.Runs.Upsert(ctx, crashedRun); err != nil {
		t.Fatalf("Runs.Upsert(crashed repair) error = %v", err)
	}
	recovered, err := runner.createRunContext(ctx, *finishedLoop)
	if err != nil {
		t.Fatalf("createRunContext(consumed repair) error = %v", err)
	}
	if !recovered.Resumed || recovered.StartStep != stepRepair || recovered.Checkpoint.Repair == nil ||
		recovered.Checkpoint.Worktree == nil || recovered.Checkpoint.Worktree.PreparedAt == "" {
		t.Fatalf("consumed repair recovery = %#v, want durable repair replay without prepare", recovered)
	}

	if err := os.WriteFile(dismissPath, []byte(`{"dismissals":[{"reviewer":"reviewer-x","reason":"conflicts with the approved direction"}]}`), 0o644); err != nil {
		t.Fatalf("WriteFile(dismiss.json) error = %v", err)
	}
	step.Loop = *finishedLoop
	step.Run = recovered.Run
	step.Checkpoint = recovered.Checkpoint
	if _, err := runner.runRepairStep(ctx, step); err != nil {
		t.Fatalf("runRepairStep(durable repair retry) error = %v", err)
	}
	if len(github.dismissedReviews) != 1 {
		t.Fatalf("dismissed reviews = %d, want replayed attempt", len(github.dismissedReviews))
	}
}

func TestFixerHITLPromptIsLimitedToNonNativeReviewThreads(t *testing.T) {
	t.Parallel()
	native := []FixItem{{Type: "comment", ID: "native-1", ThreadID: "101", Source: NativeReviewCommentSource, ProviderCommentID: 101}}
	if instruction := fixerHITLPromptFor(native); instruction != "" {
		t.Fatalf("native-only HITL instruction = %q, want empty", instruction)
	}
	regular := []FixItem{{Type: "comment", ID: "c1", ThreadID: "t1"}}
	instruction := fixerHITLPromptFor(regular)
	if !strings.Contains(instruction, "only to non-native comment fix items") ||
		!strings.Contains(instruction, "Never use \"needs_human\" in Forgejo repair_results") ||
		!strings.Contains(instruction, "Classify every listed item before editing") ||
		!strings.Contains(instruction, "the same behavior needs a second repair") ||
		!strings.Contains(instruction, "STOP THE ENTIRE TURN BEFORE MAKING EDITS") ||
		!strings.Contains(instruction, "a correct result, not a failure") {
		t.Fatalf("regular HITL instruction missing source boundary: %q", instruction)
	}
}

func TestValidateNeedsHumanRepliesRejectsMixedValidAndUnboundEntries(t *testing.T) {
	t.Parallel()
	stdout := `__LOOPER_RESULT__={"review_thread_replies":[` +
		`{"fixItemId":"c1","threadId":"t1","action":"needs_human","explanation":"Choose the intended behavior."},` +
		`{"fixItemId":"stale","threadId":"old","action":"needs_human","explanation":"Stale question."}` +
		`]}`
	items := []FixItem{{Type: "comment", ID: "c1", ThreadID: "t1"}}
	accepted := normalizeReplyExplanationActions(parseReplyExplanations(stdout, "", items))
	if len(accepted) != 1 {
		t.Fatalf("accepted replies = %#v, want only the bound entry", accepted)
	}
	if err := validateNeedsHumanReplies(stdout, "", items, accepted); err == nil {
		t.Fatal("validateNeedsHumanReplies() error = nil, want mixed decision set rejected")
	}
}
