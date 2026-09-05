package runtime

import (
	"context"
	"errors"
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

func TestPollFeishuHITLInboxOnceDoesNotAdvanceCursorPastFailedDelivery(t *testing.T) {
	var answers []string
	attempts := 0
	deps := feishuHITLPollDeps{
		loopBySeq: func(_ contextType, seq int64) string {
			switch seq {
			case 91:
				return "loop-scope"
			case 99:
				return "loop-later"
			default:
				return ""
			}
		},
		deliverAnswer: func(_ contextType, loopID, answer string) error {
			attempts++
			if attempts == 1 {
				return errors.New("apply failed")
			}
			answers = append(answers, loopID+"="+answer)
			return nil
		},
	}
	events := []feishuInboxEvent{
		mustCardAction(50, "91", "Stop"),
		mustCardAction(51, "99", "Continue"), // later event must stay unconsumed
	}
	n, maxID := pollFeishuHITLInboxOnce(context.Background(), events, deps)
	if n != 0 || maxID != 0 {
		t.Fatalf("failed Stop poll = %d maxID=%d, want unconsumed cursor", n, maxID)
	}
	if len(answers) != 0 {
		t.Fatalf("answers = %v, want none on first failed Stop", answers)
	}
	n, maxID = pollFeishuHITLInboxOnce(context.Background(), events, deps)
	if n != 2 || maxID != 51 {
		t.Fatalf("retry poll = %d maxID=%d, want Stop then preserved later Continue", n, maxID)
	}
	if len(answers) != 2 || answers[0] != "loop-scope=Stop" || answers[1] != "loop-later=Continue" {
		t.Fatalf("answers = %v, want retried Stop then later Continue", answers)
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
			return deliverHITLAnswerToLoopWithCaps(ctx, repos, coordinator.DB(), nowISO, loopID, answer, reviewFixBudgetLiveCaps(nil, ""), nil)
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
			}, nil, nil)
		},
		deliverAnswer: func(ctx contextType, loopID, answer string) error {
			return deliverHITLAnswerToLoopWithCaps(ctx, repos, coordinator.DB(), nowISO, loopID, answer, reviewFixBudgetLiveCaps(nil, ""), nil)
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

	if err := deliverHITLAnswerToLoopWithCaps(context.Background(), repos, coordinator.DB(), nowISO, reviewer.ID, "Continue", reviewFixBudgetLiveCaps(&cfg, projectID), nil); err != nil {
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

func TestFeishuHITLPollTypedScopeStopDrainsLiveSibling(t *testing.T) {
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
	const projectID = "project_scope_feishu_typed_stop"
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID: projectID, Name: "Scope", RepoPath: root, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	repo := "acme/looper"
	pr := int64(42)
	target := "pr:acme/looper:42"
	reviewer := storage.LoopRecord{
		ID: "loop_feishu_typed_scope_reviewer", Seq: 91, ProjectID: projectID, Type: "reviewer",
		TargetType: "pull_request", TargetID: &target, Repo: &repo, PRNumber: &pr,
		Status: "running", CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	fixer := storage.LoopRecord{
		ID: "loop_feishu_typed_scope_fixer", Seq: 92, ProjectID: projectID, Type: "fixer",
		TargetType: "pull_request", TargetID: &target, Repo: &repo, PRNumber: &pr,
		Status: "running", CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	if err := repos.Loops.Upsert(context.Background(), reviewer); err != nil {
		t.Fatalf("Loops.Upsert(reviewer) error = %v", err)
	}
	if err := repos.Loops.Upsert(context.Background(), fixer); err != nil {
		t.Fatalf("Loops.Upsert(fixer) error = %v", err)
	}
	parked, err := loops.ParkReviewScopeHuman(context.Background(), repos, loops.ParkReviewScopeHumanInput{
		Held: reviewer, Role: "reviewer", Repo: repo, PRNumber: pr, NowISO: nowISO, HITLEnabled: true,
		Question: "Clarify AGENTS.md rule X before unpause",
	})
	if err != nil {
		t.Fatalf("ParkReviewScopeHuman: %v", err)
	}
	reg := NewActiveExecutionRegistry()
	var drained []string
	drain := func(ctx context.Context, loop storage.LoopRecord) error {
		if loop.Status == "terminated" || loop.Status == "stopped" {
			t.Fatalf("drain after terminalize: status=%s", loop.Status)
		}
		drained = append(drained, loop.ID)
		return drainReviewFixPairExecutions(ctx, repos, loop, reg)
	}
	deps := feishuHITLPollDeps{
		loopByRoot: func(_ contextType, rootID string) string {
			if rootID == "om_scope_root" {
				return parked.ID
			}
			return ""
		},
		enqueueMessage: func(ctx contextType, loopID, text string) error {
			return enqueueFeishuHITLMessage(ctx, repos, coordinator.DB(), nil, nowISO, loopID, text, nil, drain, nil)
		},
	}
	n, maxID := pollFeishuHITLInboxOnce(context.Background(), []feishuInboxEvent{{
		ID: 40, Kind: "message", RootID: "om_scope_root", Text: "Stop",
	}}, deps)
	if n != 1 || maxID != 40 {
		t.Fatalf("typed Stop poll = %d maxID=%d, want applied Stop", n, maxID)
	}
	if len(drained) != 1 || drained[0] != parked.ID {
		t.Fatalf("typed Stop drained = %v, want [%s] before terminalize", drained, parked.ID)
	}
	fresh, err := repos.Loops.GetByID(context.Background(), reviewer.ID)
	if err != nil || fresh == nil || fresh.Status != "terminated" {
		t.Fatalf("reviewer after typed Stop = (%#v, %v), want terminated", fresh, err)
	}
	sibling, err := repos.Loops.GetByID(context.Background(), fixer.ID)
	if err != nil || sibling == nil || sibling.Status != "terminated" {
		t.Fatalf("fixer after typed Stop = (%#v, %v), want terminated", sibling, err)
	}
	if !reg.LoopStopActive(reviewer.ID) || !reg.LoopStopActive(fixer.ID) {
		t.Fatal("pair stop gates not closed after typed Feishu scope Stop")
	}
}

func TestFeishuHITLPollScopeStopRetriesAfterFailedDurableMutation(t *testing.T) {
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
	const projectID = "project_scope_feishu_stop_retry"
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID: projectID, Name: "Scope", RepoPath: root, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	repo := "acme/looper"
	pr := int64(42)
	target := "pr:acme/looper:42"
	reviewer := storage.LoopRecord{
		ID: "loop_feishu_scope_stop_retry_reviewer", Seq: 93, ProjectID: projectID, Type: "reviewer",
		TargetType: "pull_request", TargetID: &target, Repo: &repo, PRNumber: &pr,
		Status: "running", CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	fixer := storage.LoopRecord{
		ID: "loop_feishu_scope_stop_retry_fixer", Seq: 94, ProjectID: projectID, Type: "fixer",
		TargetType: "pull_request", TargetID: &target, Repo: &repo, PRNumber: &pr,
		Status: "running", CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	if err := repos.Loops.Upsert(context.Background(), reviewer); err != nil {
		t.Fatalf("Loops.Upsert(reviewer) error = %v", err)
	}
	if err := repos.Loops.Upsert(context.Background(), fixer); err != nil {
		t.Fatalf("Loops.Upsert(fixer) error = %v", err)
	}
	parked, err := loops.ParkReviewScopeHuman(context.Background(), repos, loops.ParkReviewScopeHumanInput{
		Held: reviewer, Role: "reviewer", Repo: repo, PRNumber: pr, NowISO: nowISO, HITLEnabled: true,
		Question: "Clarify AGENTS.md rule X before unpause",
	})
	if err != nil {
		t.Fatalf("ParkReviewScopeHuman: %v", err)
	}
	reg := NewActiveExecutionRegistry()
	var drained []string
	drain := func(ctx context.Context, loop storage.LoopRecord) error {
		drained = append(drained, loop.ID)
		return drainReviewFixPairExecutions(ctx, repos, loop, reg)
	}
	attempts := 0
	deps := feishuHITLPollDeps{
		loopBySeq: func(_ contextType, seq int64) string {
			if seq == reviewer.Seq {
				return parked.ID
			}
			return ""
		},
		deliverAnswer: func(ctx contextType, loopID, answer string) error {
			if _, err := drainScopeHoldOnStop(ctx, repos, loopID, answer, drain); err != nil {
				return err
			}
			attempts++
			if attempts == 1 {
				return errors.New("apply review scope stop failed")
			}
			return deliverHITLAnswerToLoopWithCaps(ctx, repos, coordinator.DB(), nowISO, loopID, answer, reviewFixBudgetLiveCaps(nil, ""), nil)
		},
	}
	events := []feishuInboxEvent{mustCardAction(50, "93", "Stop")}
	n, maxID := pollFeishuHITLInboxOnce(context.Background(), events, deps)
	if n != 0 || maxID != 0 {
		t.Fatalf("failed scope Stop poll = %d maxID=%d, want unconsumed event", n, maxID)
	}
	if len(drained) != 1 || drained[0] != parked.ID {
		t.Fatalf("first Stop drained = %v, want [%s]", drained, parked.ID)
	}
	held, err := repos.Loops.GetByID(context.Background(), reviewer.ID)
	if err != nil || held == nil || !loops.IsReviewScopeHumanHold(*held) {
		t.Fatalf("reviewer after failed Stop = (%#v, %v), want still held", held, err)
	}
	if !reg.LoopStopActive(reviewer.ID) || !reg.LoopStopActive(fixer.ID) {
		t.Fatal("pair stop gates not closed after drain that preceded failed apply")
	}
	n, maxID = pollFeishuHITLInboxOnce(context.Background(), events, deps)
	if n != 1 || maxID != 50 {
		t.Fatalf("retried scope Stop poll = %d maxID=%d, want applied Stop", n, maxID)
	}
	fresh, err := repos.Loops.GetByID(context.Background(), reviewer.ID)
	if err != nil || fresh == nil || fresh.Status != "terminated" {
		t.Fatalf("reviewer after retried Stop = (%#v, %v), want terminated", fresh, err)
	}
	sibling, err := repos.Loops.GetByID(context.Background(), fixer.ID)
	if err != nil || sibling == nil || sibling.Status != "terminated" {
		t.Fatalf("fixer after retried Stop = (%#v, %v), want terminated", sibling, err)
	}
}

func TestFeishuHITLPollTypedScopeContinueResolvesOverlayCard(t *testing.T) {
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
	const projectID = "project_scope_feishu_typed_continue"
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID: projectID, Name: "Scope", RepoPath: root, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	repo := "acme/looper"
	pr := int64(42)
	target := "pr:acme/looper:42"
	reviewer := storage.LoopRecord{
		ID: "loop_feishu_typed_scope_continue_rev", Seq: 93, ProjectID: projectID, Type: "reviewer",
		TargetType: "pull_request", TargetID: &target, Repo: &repo, PRNumber: &pr,
		Status: "running", CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	agentAsk := loops.HITLAsk{
		Kind: "agent_question", Question: "Which approach should Fixer take?",
		Options: []string{"A", "B"}, Status: "awaiting", AskedAt: nowISO,
		Transport: "feishu",
	}
	fixerMeta, err := loops.WriteHITLAsk(nil, agentAsk)
	if err != nil {
		t.Fatalf("WriteHITLAsk(fixer): %v", err)
	}
	fixer := storage.LoopRecord{
		ID: "loop_feishu_typed_scope_continue_fix", Seq: 94, ProjectID: projectID, Type: "fixer",
		TargetType: "pull_request", TargetID: &target, Repo: &repo, PRNumber: &pr,
		Status: "awaiting_human", CreatedAt: nowISO, UpdatedAt: nowISO, MetadataJSON: &fixerMeta,
	}
	if err := repos.Loops.Upsert(context.Background(), reviewer); err != nil {
		t.Fatalf("Loops.Upsert(reviewer) error = %v", err)
	}
	if err := repos.Loops.Upsert(context.Background(), fixer); err != nil {
		t.Fatalf("Loops.Upsert(fixer) error = %v", err)
	}
	parked, err := loops.ParkReviewScopeHuman(context.Background(), repos, loops.ParkReviewScopeHumanInput{
		Held: reviewer, Role: "reviewer", Repo: repo, PRNumber: pr, NowISO: nowISO, HITLEnabled: true,
		Question: "Clarify AGENTS.md rule X before unpause",
	})
	if err != nil {
		t.Fatalf("ParkReviewScopeHuman: %v", err)
	}
	overlaid, err := repos.Loops.GetByID(context.Background(), fixer.ID)
	if err != nil || overlaid == nil || !loops.IsReviewScopeHumanHold(*overlaid) {
		t.Fatalf("fixer after overlay = (%#v, %v), want preserved ask under scope hold", overlaid, err)
	}

	var resolved []string
	deps := feishuHITLPollDeps{
		loopByRoot: func(_ contextType, rootID string) string {
			if rootID == "om_scope_continue_root" {
				return parked.ID
			}
			return ""
		},
		enqueueMessage: func(ctx contextType, loopID, text string) error {
			return enqueueFeishuHITLMessage(ctx, repos, coordinator.DB(), nil, nowISO, loopID, text, func(_ contextType, answeredLoopID, answer string) {
				resolved = append(resolved, answeredLoopID+"="+answer)
			}, nil, nil)
		},
	}
	n, maxID := pollFeishuHITLInboxOnce(context.Background(), []feishuInboxEvent{{
		ID: 41, Kind: "message", RootID: "om_scope_continue_root", Text: "Continue",
	}}, deps)
	if n != 1 || maxID != 41 {
		t.Fatalf("typed Continue poll = %d maxID=%d, want applied Continue", n, maxID)
	}
	if len(resolved) != 1 || resolved[0] != parked.ID+"=Continue" {
		t.Fatalf("card resolution after typed scope Continue = %#v, want primary Continue", resolved)
	}
	fresh, err := repos.Loops.GetByID(context.Background(), reviewer.ID)
	if err != nil || fresh == nil || fresh.Status != "queued" || loops.IsReviewScopeHumanHold(*fresh) {
		t.Fatalf("reviewer after typed Continue = (%#v, %v), want queued and released", fresh, err)
	}
	sibling, err := repos.Loops.GetByID(context.Background(), fixer.ID)
	if err != nil || sibling == nil || sibling.Status != "awaiting_human" || loops.IsReviewScopeHumanHold(*sibling) {
		t.Fatalf("fixer after typed Continue = (%#v, %v), want awaiting preserved agent ask", sibling, err)
	}
	remaining, ok := loops.ReadHITLAsk(sibling.MetadataJSON)
	if !ok || remaining.Question != agentAsk.Question || remaining.Answer != "" {
		t.Fatalf("fixer ask after typed Continue = (%#v, %v), want unanswered agent question", remaining, ok)
	}
}

func TestFeishuHITLPollTypedScopeDecisionResolvesPrimaryPreservesSiblingAgentCard(t *testing.T) {
	for _, tc := range []struct {
		name           string
		text           string
		reviewerStatus string
		fixerStatus    string
	}{
		{name: "continue", text: "Continue", reviewerStatus: "queued", fixerStatus: "awaiting_human"},
		{name: "stop", text: "Stop", reviewerStatus: "terminated", fixerStatus: "terminated"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
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
			projectID := "project_scope_feishu_sibling_card_" + tc.name
			if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{
				ID: projectID, Name: "Scope", RepoPath: root, CreatedAt: nowISO, UpdatedAt: nowISO,
			}); err != nil {
				t.Fatalf("Projects.Upsert() error = %v", err)
			}
			repo := "acme/looper"
			pr := int64(42)
			target := "pr:acme/looper:42"
			reviewer := storage.LoopRecord{
				ID: "loop_feishu_sibling_card_rev_" + tc.name, Seq: 97, ProjectID: projectID, Type: "reviewer",
				TargetType: "pull_request", TargetID: &target, Repo: &repo, PRNumber: &pr,
				Status: "running", CreatedAt: nowISO, UpdatedAt: nowISO,
			}
			agentAsk := loops.HITLAsk{
				Kind: "agent_question", Question: "Which approach should Fixer take?",
				Options: []string{"A", "B"}, Status: "awaiting", AskedAt: nowISO,
				Transport: "feishu",
			}
			fixerMeta, err := loops.WriteHITLAsk(nil, agentAsk)
			if err != nil {
				t.Fatalf("WriteHITLAsk(fixer): %v", err)
			}
			fixer := storage.LoopRecord{
				ID: "loop_feishu_sibling_card_fix_" + tc.name, Seq: 98, ProjectID: projectID, Type: "fixer",
				TargetType: "pull_request", TargetID: &target, Repo: &repo, PRNumber: &pr,
				Status: "awaiting_human", CreatedAt: nowISO, UpdatedAt: nowISO, MetadataJSON: &fixerMeta,
			}
			if err := repos.Loops.Upsert(context.Background(), reviewer); err != nil {
				t.Fatalf("Loops.Upsert(reviewer) error = %v", err)
			}
			if err := repos.Loops.Upsert(context.Background(), fixer); err != nil {
				t.Fatalf("Loops.Upsert(fixer) error = %v", err)
			}
			if _, err := loops.ParkReviewScopeHuman(context.Background(), repos, loops.ParkReviewScopeHumanInput{
				Held: reviewer, Role: "reviewer", Repo: repo, PRNumber: pr, NowISO: nowISO, HITLEnabled: true,
				Question: "Clarify AGENTS.md rule X before unpause",
			}); err != nil {
				t.Fatalf("ParkReviewScopeHuman: %v", err)
			}
			overlaid, err := repos.Loops.GetByID(context.Background(), fixer.ID)
			if err != nil || overlaid == nil || !loops.IsReviewScopeHumanHold(*overlaid) {
				t.Fatalf("fixer after overlay = (%#v, %v), want preserved ask under scope hold", overlaid, err)
			}
			if ask, ok := loops.ReadHITLAsk(overlaid.MetadataJSON); !ok || loops.IsReviewScopeHumanAsk(ask) || loops.IsReviewFixBudgetAsk(ask) {
				t.Fatalf("overlaid ask = (%#v, %v), want ordinary agent question", ask, ok)
			}

			threadRoot := "om_scope_sibling_root_" + tc.name
			var resolved []string
			deps := feishuHITLPollDeps{
				loopByRoot: func(_ contextType, rootID string) string {
					if rootID == threadRoot {
						return fixer.ID
					}
					return ""
				},
				enqueueMessage: func(ctx contextType, loopID, text string) error {
					return enqueueFeishuHITLMessage(ctx, repos, coordinator.DB(), nil, nowISO, loopID, text, func(_ contextType, answeredLoopID, answer string) {
						resolved = append(resolved, answeredLoopID+"="+answer)
					}, nil, nil)
				},
			}
			n, maxID := pollFeishuHITLInboxOnce(context.Background(), []feishuInboxEvent{{
				ID: 42, Kind: "message", RootID: threadRoot, Text: tc.text,
			}}, deps)
			if n != 1 || maxID != 42 {
				t.Fatalf("typed sibling %s poll = %d maxID=%d, want applied %s", tc.text, n, maxID, tc.text)
			}
			if len(resolved) != 1 || resolved[0] != reviewer.ID+"="+tc.text {
				t.Fatalf("card resolution after sibling-root %s = %#v, want primary %s only", tc.text, resolved, tc.text)
			}
			fresh, err := repos.Loops.GetByID(context.Background(), reviewer.ID)
			if err != nil || fresh == nil || fresh.Status != tc.reviewerStatus || loops.IsReviewScopeHumanHold(*fresh) {
				t.Fatalf("reviewer after sibling %s = (%#v, %v), want %s and released", tc.text, fresh, err, tc.reviewerStatus)
			}
			sibling, err := repos.Loops.GetByID(context.Background(), fixer.ID)
			if err != nil || sibling == nil || sibling.Status != tc.fixerStatus || loops.IsReviewScopeHumanHold(*sibling) {
				t.Fatalf("fixer after sibling %s = (%#v, %v), want %s without overlay", tc.text, sibling, err, tc.fixerStatus)
			}
			if tc.fixerStatus != "awaiting_human" {
				return
			}
			remaining, ok := loops.ReadHITLAsk(sibling.MetadataJSON)
			if !ok || remaining.Question != agentAsk.Question || remaining.Answer != "" {
				t.Fatalf("fixer ask after sibling Continue = (%#v, %v), want unanswered agent question", remaining, ok)
			}
		})
	}
}

func TestFeishuHITLPollOverlayResidualCardPreservedThroughScopeContinue(t *testing.T) {
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
	const projectID = "project_scope_feishu_residual_card"
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID: projectID, Name: "Scope", RepoPath: root, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	repo := "acme/looper"
	pr := int64(42)
	target := "pr:acme/looper:42"
	reviewer := storage.LoopRecord{
		ID: "loop_feishu_residual_card_rev", Seq: 101, ProjectID: projectID, Type: "reviewer",
		TargetType: "pull_request", TargetID: &target, Repo: &repo, PRNumber: &pr,
		Status: "running", CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	agentAsk := loops.HITLAsk{
		Kind: "agent_question", Question: "Which approach should Fixer take?",
		Options: []string{"A", "B"}, Status: "awaiting", AskedAt: nowISO,
		Transport: "feishu",
	}
	fixerMeta, err := loops.WriteHITLAsk(nil, agentAsk)
	if err != nil {
		t.Fatalf("WriteHITLAsk(fixer): %v", err)
	}
	fixer := storage.LoopRecord{
		ID: "loop_feishu_residual_card_fix", Seq: 102, ProjectID: projectID, Type: "fixer",
		TargetType: "pull_request", TargetID: &target, Repo: &repo, PRNumber: &pr,
		Status: "awaiting_human", CreatedAt: nowISO, UpdatedAt: nowISO, MetadataJSON: &fixerMeta,
	}
	if err := repos.Loops.Upsert(context.Background(), reviewer); err != nil {
		t.Fatalf("Loops.Upsert(reviewer) error = %v", err)
	}
	if err := repos.Loops.Upsert(context.Background(), fixer); err != nil {
		t.Fatalf("Loops.Upsert(fixer) error = %v", err)
	}
	if _, err := loops.ParkReviewScopeHuman(context.Background(), repos, loops.ParkReviewScopeHumanInput{
		Held: reviewer, Role: "reviewer", Repo: repo, PRNumber: pr, NowISO: nowISO, HITLEnabled: true,
		Question: "Clarify AGENTS.md rule X before unpause",
	}); err != nil {
		t.Fatalf("ParkReviewScopeHuman: %v", err)
	}
	overlaid, err := repos.Loops.GetByID(context.Background(), fixer.ID)
	if err != nil || overlaid == nil || !loops.IsReviewScopeHumanHold(*overlaid) {
		t.Fatalf("fixer after overlay = (%#v, %v), want preserved ask under scope hold", overlaid, err)
	}

	var resolved []string
	deps := feishuHITLPollDeps{
		loopBySeq: func(_ contextType, seq int64) string {
			switch seq {
			case reviewer.Seq:
				return reviewer.ID
			case fixer.Seq:
				return fixer.ID
			default:
				return ""
			}
		},
		deliverAnswer: func(ctx contextType, loopID, answer string) error {
			return deliverFeishuHITLCardAction(ctx, repos, coordinator.DB(), nil, nowISO, loopID, answer, nil, func(_ contextType, answeredLoopID, got string) {
				resolved = append(resolved, answeredLoopID+"="+got)
			}, nil)
		},
	}
	n, maxID := pollFeishuHITLInboxOnce(context.Background(), []feishuInboxEvent{
		mustCardAction(60, "102", "A"),
	}, deps)
	if n != 1 || maxID != 60 {
		t.Fatalf("overlay residual poll = %d maxID=%d, want A consumed while held", n, maxID)
	}
	if len(resolved) != 1 || resolved[0] != fixer.ID+"=A" {
		t.Fatalf("card resolution after residual A = %#v, want sibling A only", resolved)
	}
	heldReviewer, err := repos.Loops.GetByID(context.Background(), reviewer.ID)
	if err != nil || heldReviewer == nil || !loops.IsReviewScopeHumanHold(*heldReviewer) {
		t.Fatalf("reviewer after residual A = (%#v, %v), want still scope-held", heldReviewer, err)
	}
	heldSibling, err := repos.Loops.GetByID(context.Background(), fixer.ID)
	if err != nil || heldSibling == nil || heldSibling.Status != "awaiting_human" || !loops.IsReviewScopeHumanHold(*heldSibling) {
		t.Fatalf("fixer after residual A = (%#v, %v), want held awaiting ordinary ask", heldSibling, err)
	}
	stored, ok := loops.ReadHITLAsk(heldSibling.MetadataJSON)
	if !ok || stored.Question != agentAsk.Question || stored.Answer != "A" || stored.Status != "answered" {
		t.Fatalf("fixer ask after residual A = (%#v, %v), want durable answered A while held", stored, ok)
	}

	n, maxID = pollFeishuHITLInboxOnce(context.Background(), []feishuInboxEvent{
		mustCardAction(61, "101", "Continue"),
	}, deps)
	if n != 1 || maxID != 61 {
		t.Fatalf("scope Continue poll = %d maxID=%d, want primary Continue consumed", n, maxID)
	}
	if len(resolved) != 2 || resolved[1] != reviewer.ID+"=Continue" {
		t.Fatalf("card resolution after Continue = %#v, want residual A then primary Continue", resolved)
	}
	fresh, err := repos.Loops.GetByID(context.Background(), reviewer.ID)
	if err != nil || fresh == nil || fresh.Status != "queued" || loops.IsReviewScopeHumanHold(*fresh) {
		t.Fatalf("reviewer after Continue = (%#v, %v), want queued and released", fresh, err)
	}
	sibling, err := repos.Loops.GetByID(context.Background(), fixer.ID)
	if err != nil || sibling == nil || sibling.Status != "queued" || loops.IsReviewScopeHumanHold(*sibling) {
		t.Fatalf("fixer after Continue = (%#v, %v), want queued resume from stored A", sibling, err)
	}
	answered, ok := loops.ReadHITLAsk(sibling.MetadataJSON)
	if !ok || answered.Question != agentAsk.Question || answered.Answer != "A" || answered.Status != "answered" {
		t.Fatalf("fixer ask after Continue = (%#v, %v), want stored A applied without a second click", answered, ok)
	}
}

func TestFeishuHITLPollOverlayTypedReplyPreservedThroughScopeContinue(t *testing.T) {
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
	const projectID = "project_scope_feishu_residual_typed"
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID: projectID, Name: "Scope", RepoPath: root, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	repo := "acme/looper"
	pr := int64(42)
	target := "pr:acme/looper:42"
	reviewer := storage.LoopRecord{
		ID: "loop_feishu_residual_typed_rev", Seq: 103, ProjectID: projectID, Type: "reviewer",
		TargetType: "pull_request", TargetID: &target, Repo: &repo, PRNumber: &pr,
		Status: "running", CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	agentAsk := loops.HITLAsk{
		Kind: "agent_question", Question: "Which approach should Fixer take?",
		Options: []string{"A", "B"}, Status: "awaiting", AskedAt: nowISO,
		Transport: "feishu",
	}
	fixerMeta, err := loops.WriteHITLAsk(nil, agentAsk)
	if err != nil {
		t.Fatalf("WriteHITLAsk(fixer): %v", err)
	}
	fixer := storage.LoopRecord{
		ID: "loop_feishu_residual_typed_fix", Seq: 104, ProjectID: projectID, Type: "fixer",
		TargetType: "pull_request", TargetID: &target, Repo: &repo, PRNumber: &pr,
		Status: "awaiting_human", CreatedAt: nowISO, UpdatedAt: nowISO, MetadataJSON: &fixerMeta,
	}
	if err := repos.Loops.Upsert(context.Background(), reviewer); err != nil {
		t.Fatalf("Loops.Upsert(reviewer) error = %v", err)
	}
	if err := repos.Loops.Upsert(context.Background(), fixer); err != nil {
		t.Fatalf("Loops.Upsert(fixer) error = %v", err)
	}
	if _, err := loops.ParkReviewScopeHuman(context.Background(), repos, loops.ParkReviewScopeHumanInput{
		Held: reviewer, Role: "reviewer", Repo: repo, PRNumber: pr, NowISO: nowISO, HITLEnabled: true,
		Question: "Clarify AGENTS.md rule X before unpause",
	}); err != nil {
		t.Fatalf("ParkReviewScopeHuman: %v", err)
	}
	overlaid, err := repos.Loops.GetByID(context.Background(), fixer.ID)
	if err != nil || overlaid == nil || !loops.IsReviewScopeHumanHold(*overlaid) {
		t.Fatalf("fixer after overlay = (%#v, %v), want preserved ask under scope hold", overlaid, err)
	}

	const siblingRoot = "om_residual_typed_root"
	var resolved []string
	deps := feishuHITLPollDeps{
		loopByRoot: func(_ contextType, rootID string) string {
			if rootID == siblingRoot {
				return fixer.ID
			}
			return ""
		},
		loopBySeq: func(_ contextType, seq int64) string {
			switch seq {
			case reviewer.Seq:
				return reviewer.ID
			case fixer.Seq:
				return fixer.ID
			default:
				return ""
			}
		},
		deliverAnswer: func(ctx contextType, loopID, answer string) error {
			return deliverFeishuHITLCardAction(ctx, repos, coordinator.DB(), nil, nowISO, loopID, answer, nil, func(_ contextType, answeredLoopID, got string) {
				resolved = append(resolved, answeredLoopID+"="+got)
			}, nil)
		},
		enqueueMessage: func(ctx contextType, loopID, text string) error {
			return enqueueFeishuHITLMessage(ctx, repos, coordinator.DB(), nil, nowISO, loopID, text, func(_ contextType, answeredLoopID, got string) {
				resolved = append(resolved, answeredLoopID+"="+got)
			}, nil, nil)
		},
	}
	n, maxID := pollFeishuHITLInboxOnce(context.Background(), []feishuInboxEvent{{
		ID: 70, Kind: "message", RootID: siblingRoot, Text: "A",
	}}, deps)
	if n != 1 || maxID != 70 {
		t.Fatalf("overlay typed poll = %d maxID=%d, want A consumed while held", n, maxID)
	}
	if len(resolved) != 1 || resolved[0] != fixer.ID+"=A" {
		t.Fatalf("resolution after typed residual A = %#v, want sibling A only", resolved)
	}
	heldReviewer, err := repos.Loops.GetByID(context.Background(), reviewer.ID)
	if err != nil || heldReviewer == nil || !loops.IsReviewScopeHumanHold(*heldReviewer) {
		t.Fatalf("reviewer after typed residual A = (%#v, %v), want still scope-held", heldReviewer, err)
	}
	heldSibling, err := repos.Loops.GetByID(context.Background(), fixer.ID)
	if err != nil || heldSibling == nil || heldSibling.Status != "awaiting_human" || !loops.IsReviewScopeHumanHold(*heldSibling) {
		t.Fatalf("fixer after typed residual A = (%#v, %v), want held awaiting ordinary ask", heldSibling, err)
	}
	stored, ok := loops.ReadHITLAsk(heldSibling.MetadataJSON)
	if !ok || stored.Question != agentAsk.Question || stored.Answer != "A" || stored.Status != "answered" {
		t.Fatalf("fixer ask after typed residual A = (%#v, %v), want durable answered A while held", stored, ok)
	}

	n, maxID = pollFeishuHITLInboxOnce(context.Background(), []feishuInboxEvent{
		mustCardAction(71, "103", "Continue"),
	}, deps)
	if n != 1 || maxID != 71 {
		t.Fatalf("scope Continue poll = %d maxID=%d, want primary Continue consumed", n, maxID)
	}
	if len(resolved) != 2 || resolved[1] != reviewer.ID+"=Continue" {
		t.Fatalf("resolution after Continue = %#v, want residual A then primary Continue", resolved)
	}
	fresh, err := repos.Loops.GetByID(context.Background(), reviewer.ID)
	if err != nil || fresh == nil || fresh.Status != "queued" || loops.IsReviewScopeHumanHold(*fresh) {
		t.Fatalf("reviewer after Continue = (%#v, %v), want queued and released", fresh, err)
	}
	sibling, err := repos.Loops.GetByID(context.Background(), fixer.ID)
	if err != nil || sibling == nil || sibling.Status != "queued" || loops.IsReviewScopeHumanHold(*sibling) {
		t.Fatalf("fixer after Continue = (%#v, %v), want queued resume from stored A", sibling, err)
	}
	answered, ok := loops.ReadHITLAsk(sibling.MetadataJSON)
	if !ok || answered.Question != agentAsk.Question || answered.Answer != "A" || answered.Status != "answered" {
		t.Fatalf("fixer ask after Continue = (%#v, %v), want stored A applied without repeating typed input", answered, ok)
	}
}
func TestFeishuResidualCardDoesNotClobberPairTransition(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer string
	}{
		{name: "continue", answer: "Continue"},
		{name: "stop", answer: "Stop"},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
			projectID := "project_residual_race_" + tc.name
			if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{
				ID: projectID, Name: "Scope", RepoPath: root, CreatedAt: nowISO, UpdatedAt: nowISO,
			}); err != nil {
				t.Fatalf("Projects.Upsert() error = %v", err)
			}
			repo := "acme/looper"
			pr := int64(42)
			target := "pr:acme/looper:42"
			reviewer := storage.LoopRecord{
				ID: "loop_residual_race_rev_" + tc.name, Seq: 201, ProjectID: projectID, Type: "reviewer",
				TargetType: "pull_request", TargetID: &target, Repo: &repo, PRNumber: &pr,
				Status: "running", CreatedAt: nowISO, UpdatedAt: nowISO,
			}
			agentAsk := loops.HITLAsk{
				Kind: "agent_question", Question: "Which approach should Fixer take?",
				Options: []string{"A", "B"}, Status: "awaiting", AskedAt: nowISO,
				Transport: "feishu",
			}
			fixerMeta, err := loops.WriteHITLAsk(nil, agentAsk)
			if err != nil {
				t.Fatalf("WriteHITLAsk(fixer): %v", err)
			}
			fixer := storage.LoopRecord{
				ID: "loop_residual_race_fix_" + tc.name, Seq: 202, ProjectID: projectID, Type: "fixer",
				TargetType: "pull_request", TargetID: &target, Repo: &repo, PRNumber: &pr,
				Status: "awaiting_human", CreatedAt: nowISO, UpdatedAt: nowISO, MetadataJSON: &fixerMeta,
			}
			if err := repos.Loops.Upsert(context.Background(), reviewer); err != nil {
				t.Fatalf("Loops.Upsert(reviewer) error = %v", err)
			}
			if err := repos.Loops.Upsert(context.Background(), fixer); err != nil {
				t.Fatalf("Loops.Upsert(fixer) error = %v", err)
			}
			if _, err := loops.ParkReviewScopeHuman(context.Background(), repos, loops.ParkReviewScopeHumanInput{
				Held: reviewer, Role: "reviewer", Repo: repo, PRNumber: pr, NowISO: nowISO, HITLEnabled: true,
				Question: "Clarify AGENTS.md rule X before unpause",
			}); err != nil {
				t.Fatalf("ParkReviewScopeHuman: %v", err)
			}
			afterFeishuResidualCardLoadHook = func() {
				fresh, getErr := repos.Loops.GetByID(context.Background(), reviewer.ID)
				if getErr != nil || fresh == nil {
					t.Errorf("hook GetByID: (%v, %v)", fresh, getErr)
					return
				}
				if _, applyErr := loops.ApplyReviewScopeHumanAnswer(context.Background(), repos, *fresh, tc.answer, nowISO); applyErr != nil {
					t.Errorf("racing %s: %v", tc.answer, applyErr)
				}
			}
			t.Cleanup(func() { afterFeishuResidualCardLoadHook = nil })
			if err := preserveFeishuOverlayResidualCardAnswer(context.Background(), repos, coordinator.DB(), fixer.ID, "A", nowISO); err != nil {
				t.Fatalf("preserve residual A: %v", err)
			}
			freshReviewer, err := repos.Loops.GetByID(context.Background(), reviewer.ID)
			if err != nil || freshReviewer == nil || loops.IsReviewScopeHumanHold(*freshReviewer) {
				t.Fatalf("reviewer after residual vs %s = (%#v, %v), want hold released", tc.answer, freshReviewer, err)
			}
			freshFixer, err := repos.Loops.GetByID(context.Background(), fixer.ID)
			if err != nil || freshFixer == nil || loops.IsReviewScopeHumanHold(*freshFixer) {
				t.Fatalf("fixer after residual vs %s = (%#v, %v), want hold not restored", tc.answer, freshFixer, err)
			}
			if tc.answer == "Stop" {
				if freshReviewer.Status != "terminated" || freshFixer.Status != "terminated" {
					t.Fatalf("pair after residual vs Stop = reviewer=%s fixer=%s, want terminated", freshReviewer.Status, freshFixer.Status)
				}
				return
			}
			if freshReviewer.Status != "queued" {
				t.Fatalf("reviewer after residual vs Continue = %s, want queued", freshReviewer.Status)
			}
			if freshFixer.Status == "terminated" {
				t.Fatalf("fixer after residual vs Continue = %s, want released sibling", freshFixer.Status)
			}
		})
	}
}

func TestEnqueueFeishuHITLMessageFailsClosedWhenLookupErrors(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, time.April, 17, 12, 34, 56, 0, time.UTC)
	nowISO := now.UTC().Format("2006-01-02T15:04:05.000Z")
	dbPath := filepath.Join(root, "looper.sqlite")
	coordinator, err := storage.OpenSQLiteCoordinator(context.Background(), dbPath, storage.SQLiteCoordinatorOptions{
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	if _, err := coordinator.MigrationRunner().RunPending(context.Background(), storage.RunPendingOptions{}); err != nil {
		t.Fatalf("RunPending() error = %v", err)
	}
	repos := storage.NewRepositories(coordinator.DB())
	const projectID = "project_scope_feishu_lookup_err"
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID: projectID, Name: "Scope", RepoPath: root, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	repo := "acme/looper"
	pr := int64(42)
	target := "pr:acme/looper:42"
	reviewer := storage.LoopRecord{
		ID: "loop_feishu_lookup_err_rev", Seq: 95, ProjectID: projectID, Type: "reviewer",
		TargetType: "pull_request", TargetID: &target, Repo: &repo, PRNumber: &pr,
		Status: "running", CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	fixer := storage.LoopRecord{
		ID: "loop_feishu_lookup_err_fix", Seq: 96, ProjectID: projectID, Type: "fixer",
		TargetType: "pull_request", TargetID: &target, Repo: &repo, PRNumber: &pr,
		Status: "running", CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	if err := repos.Loops.Upsert(context.Background(), reviewer); err != nil {
		t.Fatalf("Loops.Upsert(reviewer) error = %v", err)
	}
	if err := repos.Loops.Upsert(context.Background(), fixer); err != nil {
		t.Fatalf("Loops.Upsert(fixer) error = %v", err)
	}
	if _, err := loops.ParkReviewScopeHuman(context.Background(), repos, loops.ParkReviewScopeHumanInput{
		Held: reviewer, Role: "reviewer", Repo: repo, PRNumber: pr, NowISO: nowISO, HITLEnabled: true,
		Question: "Clarify AGENTS.md rule X before unpause",
	}); err != nil {
		t.Fatalf("ParkReviewScopeHuman: %v", err)
	}
	if err := coordinator.Close(); err != nil {
		t.Fatalf("close coordinator: %v", err)
	}

	resolved := 0
	if err := enqueueFeishuHITLMessage(context.Background(), repos, nil, nil, nowISO, reviewer.ID, "Continue", func(context.Context, string, string) {
		resolved++
	}, nil, nil); err == nil {
		t.Fatal("enqueueFeishuHITLMessage error = nil, want lookup failure")
	}
	if resolved != 0 {
		t.Fatalf("card resolved %d times, want 0 when lookup fails", resolved)
	}

	reopened, err := storage.OpenSQLiteCoordinator(context.Background(), dbPath, storage.SQLiteCoordinatorOptions{
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("reopen coordinator: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if _, err := reopened.MigrationRunner().RunPending(context.Background(), storage.RunPendingOptions{}); err != nil {
		t.Fatalf("reopen RunPending: %v", err)
	}
	freshRepos := storage.NewRepositories(reopened.DB())
	fresh, err := freshRepos.Loops.GetByID(context.Background(), reviewer.ID)
	if err != nil || fresh == nil || !loops.IsReviewScopeHumanHold(*fresh) {
		t.Fatalf("reviewer after lookup failure = (%#v, %v), want still held", fresh, err)
	}
	sibling, err := freshRepos.Loops.GetByID(context.Background(), fixer.ID)
	if err != nil || sibling == nil || sibling.Status == "queued" || sibling.Status == "terminated" {
		t.Fatalf("fixer after lookup failure = (%#v, %v), want still parked", sibling, err)
	}
}

func TestFeishuScopeAskDeliveryDoesNotClobberContinueOrStop(t *testing.T) {
	for _, tc := range []struct {
		name          string
		race          string
		wantDelivered int
		wantStatus    string
		wantHold      bool
	}{
		{name: "no_race", wantDelivered: 1, wantStatus: "awaiting_human", wantHold: true},
		{name: "continue", race: loops.ReviewFixBudgetAnswerContinue, wantDelivered: 0, wantStatus: "queued", wantHold: false},
		{name: "stop", race: loops.ReviewFixBudgetAnswerStop, wantDelivered: 0, wantStatus: "terminated", wantHold: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
			projectID := "project_scope_feishu_delivery_" + tc.name
			if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{
				ID: projectID, Name: "Scope", RepoPath: root, CreatedAt: nowISO, UpdatedAt: nowISO,
			}); err != nil {
				t.Fatalf("Projects.Upsert() error = %v", err)
			}
			repo := "acme/looper"
			pr := int64(42)
			target := "pr:acme/looper:42"
			reviewer := storage.LoopRecord{
				ID: "loop_scope_feishu_delivery_rev_" + tc.name, Seq: 401, ProjectID: projectID, Type: "reviewer",
				TargetType: "pull_request", TargetID: &target, Repo: &repo, PRNumber: &pr,
				Status: "running", CreatedAt: nowISO, UpdatedAt: nowISO,
			}
			fixer := storage.LoopRecord{
				ID: "loop_scope_feishu_delivery_fix_" + tc.name, Seq: 402, ProjectID: projectID, Type: "fixer",
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
				{ID: "queue_scope_feishu_rev_" + tc.name, ProjectID: stringPtr(projectID), LoopID: &reviewer.ID, Type: "reviewer", TargetType: "pull_request", TargetID: target, Status: "queued", Priority: storage.QueuePriorityReviewer, MaxAttempts: 3, AvailableAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO, DedupeKey: "reviewer:queue_scope_feishu_rev_" + tc.name},
				{ID: "queue_scope_feishu_fix_" + tc.name, ProjectID: stringPtr(projectID), LoopID: &fixer.ID, Type: "fixer", TargetType: "pull_request", TargetID: target, Status: "queued", Priority: storage.QueuePriorityFixer, MaxAttempts: 3, AvailableAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO, DedupeKey: "fixer:queue_scope_feishu_fix_" + tc.name},
			} {
				if err := repos.Queue.Upsert(context.Background(), item); err != nil {
					t.Fatalf("Queue.Upsert(%s) error = %v", item.ID, err)
				}
			}
			if _, err := loops.ParkReviewScopeHuman(context.Background(), repos, loops.ParkReviewScopeHumanInput{
				Held: reviewer, Role: "reviewer", Repo: repo, PRNumber: pr, NowISO: nowISO, HITLEnabled: true,
				Question: "Clarify AGENTS.md rule X before unpause",
			}); err != nil {
				t.Fatalf("ParkReviewScopeHuman: %v", err)
			}
			all, err := repos.Loops.List(context.Background())
			if err != nil {
				t.Fatalf("Loops.List() error = %v", err)
			}
			var closed []string
			delivered := deliverUndeliveredFeishuBudgetAsks(context.Background(), all, repos, feishuHITLDeliveryDeps{
				sendAsk: func(ctx contextType, loop storage.LoopRecord, _ loops.HITLAsk) error {
					if tc.race != "" {
						fresh, err := repos.Loops.GetByID(ctx, loop.ID)
						if err != nil || fresh == nil {
							t.Fatalf("GetByID during sendAsk = (%#v, %v)", fresh, err)
						}
						if _, err := loops.ApplyReviewScopeHumanAnswer(ctx, repos, *fresh, tc.race, nowISO); err != nil {
							t.Fatalf("ApplyReviewScopeHumanAnswer(%s): %v", tc.race, err)
						}
					}
					return nil
				},
				closeAsk: func(ctx contextType, loopID string) {
					closeObsoleteFeishuPairAskCard(ctx, repos, loopID, func(_ context.Context, id, answer string) {
						closed = append(closed, id+"="+answer)
					})
				},

				nowISO: nowISO,
			})
			if delivered != tc.wantDelivered {
				t.Fatalf("deliverUndeliveredFeishuBudgetAsks() = %d, want %d", delivered, tc.wantDelivered)
			}
			fresh, err := repos.Loops.GetByID(context.Background(), reviewer.ID)
			if err != nil || fresh == nil {
				t.Fatalf("Loops.GetByID(reviewer) = (%#v, %v)", fresh, err)
			}
			if fresh.Status != tc.wantStatus {
				t.Fatalf("reviewer status = %s, want %s meta=%s", fresh.Status, tc.wantStatus, derefString(fresh.MetadataJSON))
			}
			if loops.IsReviewScopeHumanHold(*fresh) != tc.wantHold {
				t.Fatalf("reviewer hold = %v, want %v meta=%s", loops.IsReviewScopeHumanHold(*fresh), tc.wantHold, derefString(fresh.MetadataJSON))
			}
			ask, ok := loops.ReadHITLAsk(fresh.MetadataJSON)
			if tc.wantHold {
				if !ok || !loops.IsReviewScopeHumanAsk(ask) || ask.Transport != "feishu" {
					t.Fatalf("delivered scope ask = (%#v, %v), want feishu transport", ask, ok)
				}
				if len(closed) != 0 {
					t.Fatalf("closed = %v, want empty when persist kept the ask", closed)
				}
				return
			}
			if ok && loops.IsReviewScopeHumanAsk(ask) {
				t.Fatalf("stale persist restored scope ask after %s: %#v", tc.race, ask)
			}
			if len(closed) != 1 || closed[0] != reviewer.ID+"="+tc.race {
				t.Fatalf("closed = %v, want posted card closed after %s", closed, tc.race)
			}

		})
	}
}
func mustCardAction(id int64, seq, answer string) feishuInboxEvent {
	e := feishuInboxEvent{ID: id, Kind: "card_action"}
	e.Value.LoopSeq = seq
	e.Value.Answer = answer
	return e
}
