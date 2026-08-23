package loops

import (
	"context"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/storage"
)

func TestBudgetExhausted(t *testing.T) {
	t.Parallel()
	if BudgetExhausted(8, 0) {
		t.Fatal("cap 0 must be unlimited")
	}
	if BudgetExhausted(7, 8) {
		t.Fatal("7/8 must not be exhausted")
	}
	if !BudgetExhausted(8, 8) {
		t.Fatal("8/8 must be exhausted")
	}
}

func TestParkAndContinueReviewFixBudget(t *testing.T) {
	t.Parallel()
	repos, nowISO := newBudgetFixture(t)
	reviewer := seedBudgetLoop(t, repos, nowISO, "loop_reviewer", "reviewer", "running")
	fixer := seedBudgetLoop(t, repos, nowISO, "loop_fixer", "fixer", "queued")
	if err := repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{
		ID: "queue_fixer", ProjectID: stringPtr("project_1"), LoopID: &fixer.ID, Type: "fixer",
		TargetType: "pull_request", TargetID: "pr:acme/looper:42", Status: "queued",
		Priority: storage.QueuePriorityFixer, MaxAttempts: 3, AvailableAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO, DedupeKey: "fixer:budget",
	}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	parked, err := ParkReviewFixBudget(context.Background(), repos, ParkReviewFixBudgetInput{
		Exhausted: reviewer, Role: "reviewer", Repo: "acme/looper", PRNumber: 42, Count: 8, Cap: 8, NowISO: nowISO,
	})
	if err != nil {
		t.Fatalf("ParkReviewFixBudget() error = %v", err)
	}
	if parked.Status != "awaiting_human" {
		t.Fatalf("exhausted status = %q, want awaiting_human", parked.Status)
	}
	ask, ok := ReadHITLAsk(parked.MetadataJSON)
	if !ok || !IsReviewFixBudgetAsk(ask) {
		t.Fatalf("HITL ask = %#v, want review-fix budget ask", ask)
	}

	sibling, err := repos.Loops.GetByID(context.Background(), fixer.ID)
	if err != nil || sibling == nil || sibling.Status != "paused" || !IsSiblingReviewFixBudgetPause(sibling.MetadataJSON) {
		t.Fatalf("sibling = (%#v, %v), want paused sibling budget hold", sibling, err)
	}
	queue, err := repos.Queue.GetByID(context.Background(), "queue_fixer")
	if err != nil || queue == nil || queue.Status != "cancelled" {
		t.Fatalf("sibling queue = (%#v, %v), want cancelled", queue, err)
	}

	result, err := ApplyReviewFixBudgetAnswer(context.Background(), repos, parked, "Continue", nowISO)
	if err != nil || !result.Applied || result.Action != "continue" {
		t.Fatalf("ApplyReviewFixBudgetAnswer() = (%#v, %v)", result, err)
	}
	if result.Loop.Status != "queued" || ReviewerPublishCount(result.Loop.MetadataJSON) != 0 {
		t.Fatalf("continued loop = %#v, want queued with reset publish count", result.Loop)
	}
	sibling, err = repos.Loops.GetByID(context.Background(), fixer.ID)
	if err != nil || sibling == nil || sibling.Status != "queued" || IsSiblingReviewFixBudgetPause(sibling.MetadataJSON) {
		t.Fatalf("sibling after continue = (%#v, %v), want queued and unpaused", sibling, err)
	}
}

func TestApplyReviewFixBudgetAnswerStopTerminatesPair(t *testing.T) {
	t.Parallel()
	repos, nowISO := newBudgetFixture(t)
	reviewer := seedBudgetLoop(t, repos, nowISO, "loop_reviewer_stop", "reviewer", "awaiting_human")
	_ = seedBudgetLoop(t, repos, nowISO, "loop_fixer_stop", "fixer", "paused")
	metadata, err := WriteHITLAsk(reviewer.MetadataJSON, NewReviewFixBudgetAsk("reviewer", "acme/looper", 42, 8, 8, nowISO))
	if err != nil {
		t.Fatalf("WriteHITLAsk() error = %v", err)
	}
	reviewer.MetadataJSON = &metadata
	if err := repos.Loops.Upsert(context.Background(), reviewer); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	result, err := ApplyReviewFixBudgetAnswer(context.Background(), repos, reviewer, "Stop", nowISO)
	if err != nil || !result.Applied || result.Action != "stop" {
		t.Fatalf("ApplyReviewFixBudgetAnswer() = (%#v, %v)", result, err)
	}
	if result.Loop.Status != "terminated" {
		t.Fatalf("exhausted after stop = %q, want terminated", result.Loop.Status)
	}
	sibling, err := repos.Loops.GetByID(context.Background(), "loop_fixer_stop")
	if err != nil || sibling == nil || sibling.Status != "terminated" {
		t.Fatalf("sibling after stop = (%#v, %v), want terminated", sibling, err)
	}
}

func TestApplyReviewFixBudgetAnswerIgnoresAgentAsk(t *testing.T) {
	t.Parallel()
	repos, nowISO := newBudgetFixture(t)
	loop := seedBudgetLoop(t, repos, nowISO, "loop_agent_ask", "fixer", "awaiting_human")
	metadata, err := WriteHITLAsk(loop.MetadataJSON, HITLAsk{Question: "follow the reviewer?", Status: "awaiting", AskedAt: nowISO})
	if err != nil {
		t.Fatalf("WriteHITLAsk() error = %v", err)
	}
	loop.MetadataJSON = &metadata
	result, err := ApplyReviewFixBudgetAnswer(context.Background(), repos, loop, "Continue", nowISO)
	if err != nil || result.Applied {
		t.Fatalf("ApplyReviewFixBudgetAnswer() = (%#v, %v), want not applied", result, err)
	}
}

func newBudgetFixture(t *testing.T) (*storage.Repositories, string) {
	t.Helper()
	coordinator := openCoordinator(t)
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 17, 12, 34, 56, 0, time.UTC)
	seedProject(t, repos, now)
	return repos, now.UTC().Format("2006-01-02T15:04:05.000Z")
}

func seedBudgetLoop(t *testing.T, repos *storage.Repositories, nowISO, id, loopType, status string) storage.LoopRecord {
	t.Helper()
	repo := "acme/looper"
	pr := int64(42)
	target := "pr:acme/looper:42"
	metadata := `{"loop":{"iterationCount":8}}`
	loop := storage.LoopRecord{
		ID: id, Seq: int64(len(id)), ProjectID: "project_1", Type: loopType, TargetType: "pull_request",
		TargetID: &target, Repo: &repo, PRNumber: &pr, Status: status, CreatedAt: nowISO, UpdatedAt: nowISO,
		MetadataJSON: &metadata,
	}
	if err := repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert(%s) error = %v", id, err)
	}
	return loop
}

func stringPtr(value string) *string { return &value }
