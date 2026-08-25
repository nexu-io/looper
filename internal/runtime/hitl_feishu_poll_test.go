package runtime

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
	"github.com/nexu-io/looper/internal/worker"
)

func TestPollFeishuHITLInboxOnce(t *testing.T) {
	// feishu_threads: root om_root_1 -> loop-a (this looper); om_other -> "" (another looper).
	rootToLoop := map[string]string{"om_root_1": "loop-a"}
	seqToLoop := map[int64]string{71: "loop-seq71"}
	var answers, messages []string
	deps := feishuHITLPollDeps{
		loopByRoot: func(_ contextType, root string) string { return rootToLoop[root] },
		loopBySeq:  func(_ contextType, seq int64) string { return seqToLoop[seq] },
		deliverAnswer: func(_ contextType, loopID, answer string) error {
			answers = append(answers, loopID+"="+answer)
			return nil
		},
		enqueueMessage: func(_ contextType, loopID, text string) error {
			messages = append(messages, loopID+"="+text)
			return nil
		},
	}
	events := []feishuInboxEvent{
		{ID: 10, Kind: "message", RootID: "om_root_1", Text: "用 A,改 resize handle"}, // typed -> enqueue
		{ID: 11, Kind: "message", RootID: "om_other", Text: "not ours"},             // another looper -> skip
		{ID: 12, Kind: "message", RootID: "om_root_1", Text: "   "},                 // empty -> skip
		mustCardAction(15, "71", "redis"),                                           // button -> deliver by seq
	}
	n, maxID := pollFeishuHITLInboxOnce(context.Background(), events, deps)
	if n != 2 {
		t.Fatalf("handled = %d, want 2", n)
	}
	if maxID != 15 {
		t.Fatalf("maxID = %d, want 15", maxID)
	}
	// A typed reply is queued (conversational), a button click is a decision.
	if len(messages) != 1 || messages[0] != "loop-a=用 A,改 resize handle" {
		t.Fatalf("enqueued messages = %v, want the typed reply", messages)
	}
	if len(answers) != 1 || answers[0] != "loop-seq71=redis" {
		t.Fatalf("delivered answers = %v, want the button click", answers)
	}
}

func TestFeishuHITLPollDeliversAndContinuesReviewFixBudget(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, time.April, 17, 12, 34, 56, 0, time.UTC)
	nowISO := now.UTC().Format("2006-01-02T15:04:05.000Z")
	coordinator, err := storage.OpenSQLiteCoordinator(context.Background(), filepath.Join(root, "looper.sqlite"), storage.SQLiteCoordinatorOptions{
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	if _, err := coordinator.MigrationRunner().RunPending(context.Background(), storage.RunPendingOptions{}); err != nil {
		t.Fatalf("RunPending() error = %v", err)
	}
	repos := storage.NewRepositories(coordinator.DB())
	const projectID = "project_budget_feishu"
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID: projectID, Name: "Budget", RepoPath: root, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	repo := "acme/looper"
	pr := int64(42)
	target := "pr:acme/looper:42"
	reviewerMeta := `{"loop":{"iterationCount":8}}`
	reviewer := storage.LoopRecord{
		ID: "loop_feishu_reviewer", Seq: 71, ProjectID: projectID, Type: "reviewer",
		TargetType: "pull_request", TargetID: &target, Repo: &repo, PRNumber: &pr,
		Status: "running", CreatedAt: nowISO, UpdatedAt: nowISO, MetadataJSON: &reviewerMeta,
	}
	fixer := storage.LoopRecord{
		ID: "loop_feishu_fixer", Seq: 72, ProjectID: projectID, Type: "fixer",
		TargetType: "pull_request", TargetID: &target, Repo: &repo, PRNumber: &pr,
		Status: "queued", CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	if err := repos.Loops.Upsert(context.Background(), reviewer); err != nil {
		t.Fatalf("Loops.Upsert(reviewer) error = %v", err)
	}
	if err := repos.Loops.Upsert(context.Background(), fixer); err != nil {
		t.Fatalf("Loops.Upsert(fixer) error = %v", err)
	}
	for _, item := range []storage.QueueItemRecord{
		{ID: "queue_feishu_reviewer", ProjectID: stringPtr(projectID), LoopID: &reviewer.ID, Type: "reviewer", TargetType: "pull_request", TargetID: target, Status: "queued", Priority: storage.QueuePriorityReviewer, MaxAttempts: 3, AvailableAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO, DedupeKey: "reviewer:queue_feishu_reviewer"},
		{ID: "queue_feishu_fixer", ProjectID: stringPtr(projectID), LoopID: &fixer.ID, Type: "fixer", TargetType: "pull_request", TargetID: target, Status: "queued", Priority: storage.QueuePriorityFixer, MaxAttempts: 3, AvailableAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO, DedupeKey: "fixer:queue_feishu_fixer"},
	} {
		if err := repos.Queue.Upsert(context.Background(), item); err != nil {
			t.Fatalf("Queue.Upsert(%s) error = %v", item.ID, err)
		}
	}

	parked, err := loops.ParkReviewFixBudget(context.Background(), repos, loops.ParkReviewFixBudgetInput{
		Exhausted: reviewer, Role: "reviewer", Repo: repo, PRNumber: pr, Count: 8, Cap: 8, NowISO: nowISO, HITLEnabled: true,
	})
	if err != nil {
		t.Fatalf("ParkReviewFixBudget() error = %v", err)
	}
	ask, ok := loops.ReadHITLAsk(parked.MetadataJSON)
	if !ok || !loops.IsReviewFixBudgetAsk(ask) || ask.Transport != "" {
		t.Fatalf("parked ask = %#v, want undelivered budget ask", ask)
	}

	var sent []worker.HITLAskNotification
	all, err := repos.Loops.List(context.Background())
	if err != nil {
		t.Fatalf("Loops.List() error = %v", err)
	}
	delivered := deliverUndeliveredFeishuBudgetAsks(context.Background(), all, repos, feishuHITLDeliveryDeps{
		sendAsk: func(_ contextType, loop storage.LoopRecord, got loops.HITLAsk) error {
			sent = append(sent, worker.HITLAskNotification{
				LoopID: loop.ID, LoopSeq: loop.Seq, Question: got.Question, Options: got.Options,
			})
			if loop.ID != reviewer.ID || got.Question != ask.Question {
				t.Fatalf("sendAsk = loop %s question %q, want reviewer budget ask", loop.ID, got.Question)
			}
			return nil
		},
		nowISO: nowISO,
	})
	if delivered != 1 || len(sent) != 1 {
		t.Fatalf("deliverUndeliveredFeishuBudgetAsks() = %d sent=%d, want 1", delivered, len(sent))
	}
	if len(sent[0].Options) != 2 || sent[0].Options[0] != loops.ReviewFixBudgetAnswerContinue || sent[0].Options[1] != loops.ReviewFixBudgetAnswerStop {
		t.Fatalf("sent options = %#v, want Continue/Stop", sent[0].Options)
	}
	fresh, err := repos.Loops.GetByID(context.Background(), reviewer.ID)
	if err != nil || fresh == nil {
		t.Fatalf("Loops.GetByID(reviewer) = (%#v, %v)", fresh, err)
	}
	ask, ok = loops.ReadHITLAsk(fresh.MetadataJSON)
	if !ok || ask.Transport != "feishu" {
		t.Fatalf("delivered ask = %#v, want feishu transport", ask)
	}

	card := mustCardAction(20, "71", "Continue")
	n, maxID := pollFeishuHITLInboxOnce(context.Background(), []feishuInboxEvent{card}, feishuHITLPollDeps{
		loopBySeq: func(_ contextType, seq int64) string {
			if seq == reviewer.Seq {
				return reviewer.ID
			}
			return ""
		},
		deliverAnswer: func(ctx contextType, loopID, answer string) error {
			return deliverHITLAnswerToLoopWithCaps(ctx, repos, coordinator.DB(), nowISO, loopID, answer, reviewFixBudgetLiveCaps(nil, ""))
		},
	})
	if n != 1 || maxID != 20 {
		t.Fatalf("poll delivered = %d maxID=%d, want Continue on reviewer", n, maxID)
	}
	fresh, err = repos.Loops.GetByID(context.Background(), reviewer.ID)
	if err != nil || fresh == nil || fresh.Status != "queued" || loops.ReviewerPublishCount(fresh.MetadataJSON) != 0 {
		t.Fatalf("reviewer after continue = (%#v, %v), want queued with reset count", fresh, err)
	}
	sibling, err := repos.Loops.GetByID(context.Background(), fixer.ID)
	if err != nil || sibling == nil || sibling.Status != "queued" || loops.IsSiblingReviewFixBudgetPause(sibling.MetadataJSON) {
		t.Fatalf("fixer after continue = (%#v, %v), want queued and unpaused", sibling, err)
	}
}

func TestFeishuHITLPollTypedBudgetMessageStaysParkedUntilContinue(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, time.April, 17, 12, 34, 56, 0, time.UTC)
	nowISO := now.UTC().Format("2006-01-02T15:04:05.000Z")
	coordinator, err := storage.OpenSQLiteCoordinator(context.Background(), filepath.Join(root, "looper.sqlite"), storage.SQLiteCoordinatorOptions{
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	if _, err := coordinator.MigrationRunner().RunPending(context.Background(), storage.RunPendingOptions{}); err != nil {
		t.Fatalf("RunPending() error = %v", err)
	}
	repos := storage.NewRepositories(coordinator.DB())
	const projectID = "project_budget_feishu_typed"
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID: projectID, Name: "Budget", RepoPath: root, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	repo := "acme/looper"
	pr := int64(42)
	target := "pr:acme/looper:42"
	reviewerMeta := `{"loop":{"iterationCount":8}}`
	reviewer := storage.LoopRecord{
		ID: "loop_feishu_typed_reviewer", Seq: 81, ProjectID: projectID, Type: "reviewer",
		TargetType: "pull_request", TargetID: &target, Repo: &repo, PRNumber: &pr,
		Status: "running", CreatedAt: nowISO, UpdatedAt: nowISO, MetadataJSON: &reviewerMeta,
	}
	fixer := storage.LoopRecord{
		ID: "loop_feishu_typed_fixer", Seq: 82, ProjectID: projectID, Type: "fixer",
		TargetType: "pull_request", TargetID: &target, Repo: &repo, PRNumber: &pr,
		Status: "queued", CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	if err := repos.Loops.Upsert(context.Background(), reviewer); err != nil {
		t.Fatalf("Loops.Upsert(reviewer) error = %v", err)
	}
	if err := repos.Loops.Upsert(context.Background(), fixer); err != nil {
		t.Fatalf("Loops.Upsert(fixer) error = %v", err)
	}
	for _, item := range []storage.QueueItemRecord{
		{ID: "queue_feishu_typed_reviewer", ProjectID: stringPtr(projectID), LoopID: &reviewer.ID, Type: "reviewer", TargetType: "pull_request", TargetID: target, Status: "queued", Priority: storage.QueuePriorityReviewer, MaxAttempts: 3, AvailableAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO, DedupeKey: "reviewer:queue_feishu_typed_reviewer"},
		{ID: "queue_feishu_typed_fixer", ProjectID: stringPtr(projectID), LoopID: &fixer.ID, Type: "fixer", TargetType: "pull_request", TargetID: target, Status: "queued", Priority: storage.QueuePriorityFixer, MaxAttempts: 3, AvailableAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO, DedupeKey: "fixer:queue_feishu_typed_fixer"},
	} {
		if err := repos.Queue.Upsert(context.Background(), item); err != nil {
			t.Fatalf("Queue.Upsert(%s) error = %v", item.ID, err)
		}
	}

	if _, err := loops.ParkReviewFixBudget(context.Background(), repos, loops.ParkReviewFixBudgetInput{
		Exhausted: reviewer, Role: "reviewer", Repo: repo, PRNumber: pr, Count: 8, Cap: 8, NowISO: nowISO, HITLEnabled: true,
	}); err != nil {
		t.Fatalf("ParkReviewFixBudget() error = %v", err)
	}

	all, err := repos.Loops.List(context.Background())
	if err != nil {
		t.Fatalf("Loops.List() error = %v", err)
	}
	if delivered := deliverUndeliveredFeishuBudgetAsks(context.Background(), all, repos, feishuHITLDeliveryDeps{
		sendAsk: func(_ contextType, _ storage.LoopRecord, _ loops.HITLAsk) error { return nil },
		nowISO:  nowISO,
	}); delivered != 1 {
		t.Fatalf("deliverUndeliveredFeishuBudgetAsks() = %d, want 1", delivered)
	}

	var resolved []string
	deps := feishuHITLPollDeps{
		loopByRoot: func(_ contextType, rootID string) string {
			if rootID == "om_budget_root" {
				return reviewer.ID
			}
			return ""
		},
		enqueueMessage: func(ctx contextType, loopID, text string) error {
			return enqueueFeishuHITLMessage(ctx, repos, coordinator.DB(), nil, nowISO, loopID, text, func(_ contextType, answeredLoopID, answer string) {
				resolved = append(resolved, answeredLoopID+"="+answer)
			})
		},
		deliverAnswer: func(ctx contextType, loopID, answer string) error {
			return deliverHITLAnswerToLoopWithCaps(ctx, repos, coordinator.DB(), nowISO, loopID, answer, reviewFixBudgetLiveCaps(nil, ""))
		},
	}

	n, maxID := pollFeishuHITLInboxOnce(context.Background(), []feishuInboxEvent{{
		ID: 30, Kind: "message", RootID: "om_budget_root", Text: "looking into this first",
	}}, deps)
	if n != 1 || maxID != 30 {
		t.Fatalf("typed discussion poll = %d maxID=%d, want handled discussion", n, maxID)
	}
	fresh, err := repos.Loops.GetByID(context.Background(), reviewer.ID)
	if err != nil || fresh == nil || fresh.Status != "awaiting_human" || loops.ReviewerPublishCount(fresh.MetadataJSON) != 8 {
		t.Fatalf("reviewer after typed discussion = (%#v, %v), want still parked", fresh, err)
	}
	if ask, ok := loops.ReadHITLAsk(fresh.MetadataJSON); !ok || !loops.IsReviewFixBudgetAsk(ask) || ask.Transport != "feishu" {
		t.Fatalf("ask after typed discussion = %#v, want delivered budget ask", ask)
	}
	if got := loops.ReadHumanInbox(fresh.MetadataJSON); len(got) != 0 {
		t.Fatalf("human inbox after typed discussion = %#v, want empty", got)
	}
	if len(resolved) != 0 {
		t.Fatalf("card resolution after typed discussion = %#v, want none", resolved)
	}
	sibling, err := repos.Loops.GetByID(context.Background(), fixer.ID)
	if err != nil || sibling == nil || sibling.Status != "paused" || !loops.IsSiblingReviewFixBudgetPause(sibling.MetadataJSON) {
		t.Fatalf("fixer after typed discussion = (%#v, %v), want still paused", sibling, err)
	}

	n, maxID = pollFeishuHITLInboxOnce(context.Background(), []feishuInboxEvent{{
		ID: 31, Kind: "message", RootID: "om_budget_root", Text: "Continue",
	}}, deps)
	if n != 1 || maxID != 31 {
		t.Fatalf("typed Continue poll = %d maxID=%d, want applied Continue", n, maxID)
	}
	fresh, err = repos.Loops.GetByID(context.Background(), reviewer.ID)
	if err != nil || fresh == nil || fresh.Status != "queued" || loops.ReviewerPublishCount(fresh.MetadataJSON) != 0 {
		t.Fatalf("reviewer after typed Continue = (%#v, %v), want queued with reset count", fresh, err)
	}
	sibling, err = repos.Loops.GetByID(context.Background(), fixer.ID)
	if err != nil || sibling == nil || sibling.Status != "queued" || loops.IsSiblingReviewFixBudgetPause(sibling.MetadataJSON) {
		t.Fatalf("fixer after typed Continue = (%#v, %v), want queued and unpaused", sibling, err)
	}
	if len(resolved) != 1 || resolved[0] != reviewer.ID+"=Continue" {
		t.Fatalf("card resolution after typed Continue = %#v, want reviewer Continue", resolved)
	}
}

func TestFeishuContinueUsesLiveProjectCaps(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, time.April, 17, 12, 34, 56, 0, time.UTC)
	nowISO := now.UTC().Format("2006-01-02T15:04:05.000Z")
	coordinator, err := storage.OpenSQLiteCoordinator(context.Background(), filepath.Join(root, "looper.sqlite"), storage.SQLiteCoordinatorOptions{
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	if _, err := coordinator.MigrationRunner().RunPending(context.Background(), storage.RunPendingOptions{}); err != nil {
		t.Fatalf("RunPending() error = %v", err)
	}
	repos := storage.NewRepositories(coordinator.DB())
	const projectID = "project_budget_feishu_caps"
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID: projectID, Name: "Budget", RepoPath: root, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	repo := "acme/looper"
	pr := int64(42)
	target := "pr:acme/looper:42"
	// Reviewer at 2/2 (exhausted); fixer at 1/5 (under cap).
	reviewerMeta := `{"loop":{"iterationCount":2}}`
	fixerMeta := `{"reviewFixBudget":{"pushCount":1}}`
	reviewer := storage.LoopRecord{
		ID: "loop_feishu_caps_rev", Seq: 91, ProjectID: projectID, Type: "reviewer",
		TargetType: "pull_request", TargetID: &target, Repo: &repo, PRNumber: &pr,
		Status: "running", CreatedAt: nowISO, UpdatedAt: nowISO, MetadataJSON: &reviewerMeta,
	}
	fixer := storage.LoopRecord{
		ID: "loop_feishu_caps_fix", Seq: 92, ProjectID: projectID, Type: "fixer",
		TargetType: "pull_request", TargetID: &target, Repo: &repo, PRNumber: &pr,
		Status: "queued", CreatedAt: nowISO, UpdatedAt: nowISO, MetadataJSON: &fixerMeta,
	}
	if err := repos.Loops.Upsert(context.Background(), reviewer); err != nil {
		t.Fatalf("Upsert reviewer: %v", err)
	}
	if err := repos.Loops.Upsert(context.Background(), fixer); err != nil {
		t.Fatalf("Upsert fixer: %v", err)
	}
	if _, err := loops.ParkReviewFixBudget(context.Background(), repos, loops.ParkReviewFixBudgetInput{
		Exhausted: reviewer, Role: "reviewer", Repo: repo, PRNumber: pr, Count: 2, Cap: 2, NowISO: nowISO, HITLEnabled: true,
		LiveCaps: loops.ReviewFixBudgetLiveCaps{ReviewerMaxPublishes: 2, FixerMaxPushes: 5},
	}); err != nil {
		t.Fatalf("Park: %v", err)
	}

	cfg, err := config.DefaultConfig(root)
	if err != nil {
		t.Fatalf("DefaultConfig: %v", err)
	}
	cfg.Roles.Reviewer.Behavior.Loop.MaxPublishesPerPR = 2
	cfg.Roles.Fixer.Behavior.Loop.MaxPushesPerPR = 5

	if err := deliverHITLAnswerToLoopWithCaps(context.Background(), repos, coordinator.DB(), nowISO, reviewer.ID, "Continue", reviewFixBudgetLiveCaps(&cfg, projectID)); err != nil {
		t.Fatalf("Continue: %v", err)
	}
	fresh, _ := repos.Loops.GetByID(context.Background(), reviewer.ID)
	if fresh == nil || fresh.Status != "queued" || loops.ReviewerPublishCount(fresh.MetadataJSON) != 0 {
		t.Fatalf("reviewer after Continue = %#v, want queued with reset meter", fresh)
	}
	sibling, _ := repos.Loops.GetByID(context.Background(), fixer.ID)
	if sibling == nil || sibling.Status != "queued" || loops.FixerPushCount(sibling.MetadataJSON) != 1 {
		t.Fatalf("fixer after Continue = %#v, want queued with preserved pushCount 1", sibling)
	}
}

func TestGenericMessageDoesNotQueueNoAskBudgetHold(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, time.April, 17, 12, 34, 56, 0, time.UTC)
	nowISO := now.UTC().Format("2006-01-02T15:04:05.000Z")
	coordinator, err := storage.OpenSQLiteCoordinator(context.Background(), filepath.Join(root, "looper.sqlite"), storage.SQLiteCoordinatorOptions{
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	if _, err := coordinator.MigrationRunner().RunPending(context.Background(), storage.RunPendingOptions{}); err != nil {
		t.Fatalf("RunPending() error = %v", err)
	}
	repos := storage.NewRepositories(coordinator.DB())
	const projectID = "project_budget_noask_msg"
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID: projectID, Name: "Budget", RepoPath: root, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	repo := "acme/looper"
	pr := int64(42)
	target := "pr:acme/looper:42"
	reviewerMeta := `{"loop":{"iterationCount":3}}`
	reviewer := storage.LoopRecord{
		ID: "loop_noask_msg_rev", Seq: 101, ProjectID: projectID, Type: "reviewer",
		TargetType: "pull_request", TargetID: &target, Repo: &repo, PRNumber: &pr,
		Status: "running", CreatedAt: nowISO, UpdatedAt: nowISO, MetadataJSON: &reviewerMeta,
	}
	if err := repos.Loops.Upsert(context.Background(), reviewer); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if _, err := loops.ParkReviewFixBudget(context.Background(), repos, loops.ParkReviewFixBudgetInput{
		Exhausted: reviewer, Role: "reviewer", Repo: repo, PRNumber: pr, Count: 3, Cap: 3, NowISO: nowISO, HITLEnabled: false,
	}); err != nil {
		t.Fatalf("Park: %v", err)
	}
	// Generic message must not queue a no-ask hold.
	if err := enqueueHumanMessageToLoop(context.Background(), repos, nowISO, reviewer.ID, "please look at this"); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	fresh, _ := repos.Loops.GetByID(context.Background(), reviewer.ID)
	if fresh == nil || fresh.Status != "paused" || !loops.IsReviewFixBudgetExhaustedPause(fresh.MetadataJSON) {
		t.Fatalf("after generic message = %#v, want still paused exhausted hold", fresh)
	}
	if loops.ReviewerPublishCount(fresh.MetadataJSON) != 3 {
		t.Fatalf("publish count = %d, want unchanged 3", loops.ReviewerPublishCount(fresh.MetadataJSON))
	}
}

func mustCardAction(id int64, seq, answer string) feishuInboxEvent {
	e := feishuInboxEvent{ID: id, Kind: "card_action"}
	e.Value.LoopSeq = seq
	e.Value.Answer = answer
	return e
}
