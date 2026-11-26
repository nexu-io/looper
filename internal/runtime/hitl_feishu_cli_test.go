package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
)

func TestFeishuAnswerRequeuesSameLoopAndRejectsEarlierQuestion(t *testing.T) {
	ctx := context.Background()
	repos := newEnqueueTestRepos(t)
	now := "2026-11-14T12:00:00.000Z"
	base, target := "main", "project:project"
	if err := repos.Projects.Upsert(ctx, storage.ProjectRecord{ID: "project", Name: "test", RepoPath: t.TempDir(), BaseBranch: &base, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	meta, _ := loops.WriteHITLAsk(nil, loops.HITLAsk{Transport: "feishu", Status: "awaiting", Question: "A or B?", QuestionMessageID: "new-question", SessionID: "same-session"})
	loopID := "worker"
	if err := repos.Loops.Upsert(ctx, storage.LoopRecord{ID: loopID, Seq: 1, ProjectID: "project", Type: "worker", TargetType: "project", TargetID: &target, Status: "awaiting_human", MetadataJSON: &meta, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := repos.Queue.Upsert(ctx, storage.QueueItemRecord{ID: "queue", LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: "project:project", Status: "cancelled", Priority: storage.QueuePriorityWorker, MaxAttempts: 3, DedupeKey: "worker", AvailableAt: now, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := repos.Runs.Upsert(ctx, storage.RunRecord{ID: "parked-run", LoopID: loopID, Status: "interrupted", StartedAt: now, EndedAt: &now, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rt := New(Options{Config: cfg})
	rt.activeExecutions = NewActiveExecutionRegistry()
	reconcileAt := time.Date(2026, 11, 14, 13, 0, 0, 0, time.UTC)
	// Both periodic reconciliation and daemon restart must preserve the hold.
	for _, mode := range []staleRunReconcileMode{staleRunReconcileModeLive, staleRunReconcileModeStartup} {
		if _, err := rt.reconcileStaleRunningRunsWithMode(ctx, repos, reconcileAt, mode); err != nil {
			t.Fatal(err)
		}
		parked, _ := repos.Loops.GetByID(ctx, loopID)
		active, _ := repos.Queue.CountActiveByLoopID(ctx, loopID)
		if parked.Status != "awaiting_human" || active != 0 {
			t.Fatalf("recovery %s woke unanswered task: loop=%s active=%d", mode, parked.Status, active)
		}
	}
	if err := deliverHITLAnswerToLoopForQuestion(ctx, repos, now, loopID, "stale answer", "old-question"); err != nil {
		t.Fatal(err)
	}
	before, _ := repos.Loops.GetByID(ctx, loopID)
	if before.Status != "awaiting_human" {
		t.Fatalf("stale answer woke loop: %s", before.Status)
	}
	if err := deliverHITLAnswerToLoopForQuestion(ctx, repos, now, loopID, "为什么建议 A？", "new-question"); err != nil {
		t.Fatal(err)
	}
	after, _ := repos.Loops.GetByID(ctx, loopID)
	ask, _ := loops.ReadHITLAsk(after.MetadataJSON)
	queue, _ := repos.Queue.GetByID(ctx, "queue")
	if after.Status != "running" || queue.Status != "queued" || ask.SessionID != "same-session" || ask.Answer != "为什么建议 A？" || ask.Status != "answered" {
		t.Fatalf("loop=%s queue=%s ask=%+v", after.Status, queue.Status, ask)
	}
	// The scheduler owns the resumed claim before the worker creates its new run.
	// The preceding interrupted run is not a reason to reclaim that live queue.
	queue.Status = "running"
	if err := repos.Queue.Upsert(ctx, *queue); err != nil {
		t.Fatal(err)
	}
	lease, err := rt.activeExecutions.AdmitOperation(ctx, OperationMeta{ClaimedBy: "scheduler"})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if _, err := lease.BindClaim(*queue); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.reconcileStaleRunningRunsWithMode(ctx, repos, reconcileAt, staleRunReconcileModeLive); err != nil {
		t.Fatal(err)
	}
	queue, _ = repos.Queue.GetByID(ctx, "queue")
	if queue.Status != "running" {
		t.Fatalf("recovery reclaimed an owned resume: queue=%s", queue.Status)
	}
}
