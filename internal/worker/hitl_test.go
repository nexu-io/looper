package worker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
)

func TestConsumeAskSentinelReadsAndRemoves(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".looper"), 0o755); err != nil {
		t.Fatalf("MkdirAll error = %v", err)
	}
	path := filepath.Join(dir, ".looper", "ask.json")
	if err := os.WriteFile(path, []byte(`{"question":"Redis or Postgres?","options":["redis","postgres"]}`), 0o644); err != nil {
		t.Fatalf("WriteFile error = %v", err)
	}

	ask, err := consumeAskSentinel(dir)
	if err != nil {
		t.Fatalf("consumeAskSentinel error = %v", err)
	}
	if ask == nil || ask.Question != "Redis or Postgres?" || len(ask.Options) != 2 {
		t.Fatalf("ask = %#v, want the parsed question + 2 options", ask)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatal("sentinel file must be removed after consumption")
	}

	// No sentinel -> nil, nil (both a missing file and an absent .looper dir).
	if again, err := consumeAskSentinel(dir); err != nil || again != nil {
		t.Fatalf("second consumeAskSentinel = (%#v, %v), want (nil, nil)", again, err)
	}
	if missing, err := consumeAskSentinel(t.TempDir()); err != nil || missing != nil {
		t.Fatalf("consumeAskSentinel(empty dir) = (%#v, %v), want (nil, nil)", missing, err)
	}
}

func TestWorkerDoesNotAttachOldVendorSessionsAfterVendorChange(t *testing.T) {
	fixture := newRunnerFixture(t)
	ctx := context.Background()
	loop, err := fixture.repos.Loops.GetByID(ctx, "loop_worker_1")
	if err != nil || loop == nil {
		t.Fatalf("Loops.GetByID() = (%#v, %v)", loop, err)
	}

	sessionID := "codex-session-123"
	metadata, err := loops.WriteHITLAsk(loop.MetadataJSON, loops.HITLAsk{
		Question:  "Which datastore?",
		SessionID: sessionID,
		Vendor:    "codex",
		Answer:    "postgres",
		Status:    "answered",
	})
	if err != nil {
		t.Fatalf("WriteHITLAsk() error = %v", err)
	}
	metadata, err = loops.WriteTakeoverResume(&metadata, loops.TakeoverResume{SessionID: sessionID})
	if err != nil {
		t.Fatalf("WriteTakeoverResume() error = %v", err)
	}
	loop.MetadataJSON = &metadata
	if err := fixture.repos.Loops.Upsert(ctx, *loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	projectID := loop.ProjectID
	loopID := loop.ID
	if err := fixture.repos.AgentExecutions.Upsert(ctx, storage.AgentExecutionRecord{
		ID:              "agent_old_vendor",
		ProjectID:       &projectID,
		LoopID:          &loopID,
		Vendor:          "codex",
		Status:          "completed",
		NativeSessionID: &sessionID,
		StartedAt:       fixture.nowISO(),
		CreatedAt:       fixture.nowISO(),
		UpdatedAt:       fixture.nowISO(),
	}); err != nil {
		t.Fatalf("AgentExecutions.Upsert() error = %v", err)
	}

	sameVendor := New(Options{Repos: fixture.repos, AgentRuntime: "codex"})
	if prompt, gotSession := sameVendor.pendingHumanAnswer(ctx, loop); !strings.Contains(prompt, "postgres") || gotSession != sessionID {
		t.Fatalf("same-vendor HITL resume = (%q, %q), want decision prompt + %q", prompt, gotSession, sessionID)
	}
	if prompt, gotSession := sameVendor.pendingTakeoverResume(ctx, loop); prompt == "" || gotSession != sessionID {
		t.Fatalf("same-vendor takeover resume = (%q, %q), want prompt + %q", prompt, gotSession, sessionID)
	}
	if got := sameVendor.latestNativeSessionID(ctx, loop.ID); got != sessionID {
		t.Fatalf("same-vendor mailbox session = %q, want %q", got, sessionID)
	}

	newVendor := New(Options{Repos: fixture.repos, AgentRuntime: "claude-code"})
	if prompt, gotSession := newVendor.pendingHumanAnswer(ctx, loop); !strings.Contains(prompt, "postgres") || !strings.Contains(prompt, "vendor changed") || gotSession != "" {
		t.Fatalf("cross-vendor HITL resume = (%q, %q), want decision prompt + fresh session", prompt, gotSession)
	}
	if prompt, gotSession := newVendor.pendingTakeoverResume(ctx, loop); !strings.Contains(prompt, "fresh session") || gotSession != "" {
		t.Fatalf("cross-vendor takeover resume = (%q, %q), want checkpoint prompt + fresh session", prompt, gotSession)
	}
	if got := newVendor.latestNativeSessionID(ctx, loop.ID); got != "" {
		t.Fatalf("cross-vendor mailbox session = %q, want fresh session", got)
	}
}

func TestSuspendForHumanTransitionsAndNotifies(t *testing.T) {
	fixture := newRunnerFixture(t)
	ctx := context.Background()
	nowISO := fixture.nowISO()

	// Move the seeded loop + queue item into an in-flight state.
	loop, err := fixture.repos.Loops.GetByID(ctx, "loop_worker_1")
	if err != nil || loop == nil {
		t.Fatalf("Loops.GetByID error = %v", err)
	}
	loop.Status = "running"
	if err := fixture.repos.Loops.Upsert(ctx, *loop); err != nil {
		t.Fatalf("Loops.Upsert error = %v", err)
	}
	queueItem, err := fixture.repos.Queue.GetByID(ctx, "queue_worker_1")
	if err != nil || queueItem == nil {
		t.Fatalf("Queue.GetByID error = %v", err)
	}
	queueItem.Status = "running"
	if err := fixture.repos.Queue.Upsert(ctx, *queueItem); err != nil {
		t.Fatalf("Queue.Upsert error = %v", err)
	}
	run := storage.RunRecord{ID: "run_worker_1", LoopID: "loop_worker_1", Status: "running", CurrentStep: stringPtr("execute"), LastCompletedStep: stringPtr("plan"), StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Runs.Upsert(ctx, run); err != nil {
		t.Fatalf("Runs.Upsert error = %v", err)
	}
	project, err := fixture.repos.Projects.GetByID(ctx, "project_1")
	if err != nil || project == nil {
		t.Fatalf("Projects.GetByID error = %v", err)
	}

	var sent []HITLAskNotification
	runner := New(Options{
		DB:          fixture.coordinator.DB(),
		Repos:       fixture.repos,
		Logger:      fixture.logger,
		Now:         fixture.now,
		HITLEnabled: true,
		// This test exercises the Feishu-notify transport; github is the default and
		// is covered separately.
		HITLAnswerTransport: "feishu",
		HITLNotify: func(_ context.Context, n HITLAskNotification) error {
			sent = append(sent, n)
			return nil
		},
	})

	awaiting := &awaitingHumanError{question: "Which datastore?", options: []string{"redis", "postgres"}, sessionID: "sess-xyz", executionID: "agent-1", vendor: "codex"}
	result, err := runner.suspendForHuman(ctx, stepInput{Project: *project, Loop: *loop, Run: run, QueueItem: *queueItem}, run, workerCheckpoint{}, awaiting)
	if err != nil {
		t.Fatalf("suspendForHuman error = %v", err)
	}
	if result.Status != "awaiting_human" {
		t.Fatalf("result.Status = %q, want awaiting_human", result.Status)
	}

	// Loop suspended + ask persisted.
	got, err := fixture.repos.Loops.GetByID(ctx, "loop_worker_1")
	if err != nil || got == nil {
		t.Fatalf("Loops.GetByID error = %v", err)
	}
	if got.Status != "awaiting_human" {
		t.Fatalf("loop status = %q, want awaiting_human", got.Status)
	}
	ask, ok := loops.ReadHITLAsk(got.MetadataJSON)
	if !ok || ask.Question != "Which datastore?" || ask.SessionID != "sess-xyz" || ask.Status != "awaiting" {
		t.Fatalf("persisted ask = %#v (ok=%v), want the question + session + awaiting", ask, ok)
	}

	// Queue item cancelled so the scheduler stops retrying (resume requeues it).
	q, err := fixture.repos.Queue.GetByID(ctx, "queue_worker_1")
	if err != nil || q == nil {
		t.Fatalf("Queue.GetByID error = %v", err)
	}
	if q.Status != "cancelled" {
		t.Fatalf("queue status = %q, want cancelled", q.Status)
	}

	// Run ended as interrupted (resumable from checkpoint).
	finishedRun, err := fixture.repos.Runs.GetByID(ctx, "run_worker_1")
	if err != nil || finishedRun == nil {
		t.Fatalf("Runs.GetByID error = %v", err)
	}
	if finishedRun.Status != "interrupted" {
		t.Fatalf("run status = %q, want interrupted", finishedRun.Status)
	}

	// Ask-card sent.
	if len(sent) != 1 {
		t.Fatalf("HITLNotify calls = %d, want 1", len(sent))
	}
	if sent[0].LoopSeq != 1 || sent[0].Question != "Which datastore?" || len(sent[0].Options) != 2 {
		t.Fatalf("notification = %#v, want loop seq 1 + question + 2 options", sent[0])
	}
}

func TestSuspendForHumanDeliversAskToGitHub(t *testing.T) {
	fixture := newRunnerFixture(t)
	ctx := context.Background()
	nowISO := fixture.nowISO()

	loop, err := fixture.repos.Loops.GetByID(ctx, "loop_worker_1")
	if err != nil || loop == nil {
		t.Fatalf("Loops.GetByID error = %v", err)
	}
	loop.Status = "running"
	pr := int64(42)
	loop.PRNumber = &pr // already has a PR, so no draft-PR git push is needed
	repo := "acme/widgets"
	loop.Repo = &repo
	if err := fixture.repos.Loops.Upsert(ctx, *loop); err != nil {
		t.Fatalf("Loops.Upsert error = %v", err)
	}
	queueItem, err := fixture.repos.Queue.GetByID(ctx, "queue_worker_1")
	if err != nil || queueItem == nil {
		t.Fatalf("Queue.GetByID error = %v", err)
	}
	queueItem.Status = "running"
	if err := fixture.repos.Queue.Upsert(ctx, *queueItem); err != nil {
		t.Fatalf("Queue.Upsert error = %v", err)
	}
	run := storage.RunRecord{ID: "run_worker_1", LoopID: "loop_worker_1", Status: "running", CurrentStep: stringPtr("execute"), StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Runs.Upsert(ctx, run); err != nil {
		t.Fatalf("Runs.Upsert error = %v", err)
	}
	project, err := fixture.repos.Projects.GetByID(ctx, "project_1")
	if err != nil || project == nil {
		t.Fatalf("Projects.GetByID error = %v", err)
	}

	gh := &fakeGitHubGateway{issueCommentResult: IssueCommentResult{ID: 777}}
	runner := New(Options{
		DB:          fixture.coordinator.DB(),
		Repos:       fixture.repos,
		Logger:      fixture.logger,
		Now:         fixture.now,
		HITLEnabled: true,
		// github is the default transport; be explicit for the test's intent.
		HITLAnswerTransport: "github",
		HITLGitHub:          HITLGitHubSettings{AwaitingLabel: "looper:awaiting-human", MentionLogins: []string{"lefarcen"}},
		GitHub:              gh,
	})

	awaiting := &awaitingHumanError{question: "Redis or Postgres?", options: []string{"redis", "postgres"}, sessionID: "sess-1", vendor: "codex"}
	loopForStep, _ := fixture.repos.Loops.GetByID(ctx, "loop_worker_1")
	result, err := runner.suspendForHuman(ctx, stepInput{Project: *project, Loop: *loopForStep, Run: run, QueueItem: *queueItem}, run, workerCheckpoint{PullRequest: &checkpointPullPR{Number: 42}}, awaiting)
	if err != nil {
		t.Fatalf("suspendForHuman error = %v", err)
	}
	if result.Status != "awaiting_human" {
		t.Fatalf("result.Status = %q, want awaiting_human", result.Status)
	}

	// Posted the ask as a PR comment carrying the marker + question.
	if len(gh.createIssueCommentCalls) != 1 {
		t.Fatalf("CreateIssueComment calls = %d, want 1", len(gh.createIssueCommentCalls))
	}
	c := gh.createIssueCommentCalls[0]
	if c.IssueNumber != 42 || c.Repo != "acme/widgets" {
		t.Fatalf("comment target = %s#%d, want acme/widgets#42", c.Repo, c.IssueNumber)
	}
	if !containsAll(c.Body, hitlGitHubAskMarkerPrefix, "Redis or Postgres?", "@lefarcen") {
		t.Fatalf("comment body missing marker/question/mention: %s", c.Body)
	}
	// Labelled awaiting-human.
	if len(gh.addLabels) != 1 || len(gh.addLabels[0].Labels) != 1 || gh.addLabels[0].Labels[0] != "looper:awaiting-human" {
		t.Fatalf("addLabels = %#v, want one looper:awaiting-human on the PR", gh.addLabels)
	}
	// Ask metadata records the github correlation.
	got, _ := fixture.repos.Loops.GetByID(ctx, "loop_worker_1")
	ask, ok := loops.ReadHITLAsk(got.MetadataJSON)
	if !ok || ask.Transport != "github" || ask.PRNumber != 42 || ask.AskCommentID != 777 {
		t.Fatalf("ask = %#v, want github transport + pr 42 + comment 777", ask)
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}
