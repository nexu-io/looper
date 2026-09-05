package loops

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
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

func TestIsReviewFixBudgetContinueRequiresExplicitOption(t *testing.T) {
	t.Parallel()
	for _, answer := range []string{"Continue", "continue", " Continue ", "continue another"} {
		if !IsReviewFixBudgetContinue(answer) {
			t.Fatalf("IsReviewFixBudgetContinue(%q) = false, want true", answer)
		}
	}
	for _, answer := range []string{
		"Continue investigating; do not resume yet",
		"continue the investigation",
		"continued",
		"Stop",
		"",
	} {
		if IsReviewFixBudgetContinue(answer) {
			t.Fatalf("IsReviewFixBudgetContinue(%q) = true, want false", answer)
		}
	}
}

func TestParkAndContinueReviewFixBudget(t *testing.T) {
	t.Parallel()
	repos, nowISO := newBudgetFixture(t)
	reviewer := seedBudgetLoop(t, repos, nowISO, "loop_reviewer", "reviewer", "running")
	fixer := seedBudgetLoop(t, repos, nowISO, "loop_fixer", "fixer", "queued")
	seedBudgetQueue(t, repos, nowISO, "queue_reviewer", reviewer.ID, "reviewer", storage.QueuePriorityReviewer)
	seedBudgetQueue(t, repos, nowISO, "queue_fixer", fixer.ID, "fixer", storage.QueuePriorityFixer)

	parked, err := ParkReviewFixBudget(context.Background(), repos, ParkReviewFixBudgetInput{
		Exhausted: reviewer, Role: "reviewer", Repo: "acme/looper", PRNumber: 42, Count: 8, Cap: 8, NowISO: nowISO, HITLEnabled: true,
	})
	if err != nil {
		t.Fatalf("ParkReviewFixBudget() error = %v", err)
	}
	if parked.Status != "awaiting_human" {
		t.Fatalf("exhausted status = %q, want awaiting_human", parked.Status)
	}
	ask, ok := ReadHITLAsk(parked.MetadataJSON)
	if !ok || !IsReviewFixBudgetAsk(ask) || ask.PRNumber != 42 {
		t.Fatalf("HITL ask = %#v, want review-fix budget ask with PR 42", ask)
	}
	if ask.Transport != "" || ask.AskCommentID != 0 {
		t.Fatalf("HITL ask transport = %#v, want unset until GitHub delivery", ask)
	}

	sibling, err := repos.Loops.GetByID(context.Background(), fixer.ID)
	if err != nil || sibling == nil || sibling.Status != "paused" || !IsSiblingReviewFixBudgetPause(sibling.MetadataJSON) {
		t.Fatalf("sibling = (%#v, %v), want paused sibling budget hold", sibling, err)
	}
	for _, queueID := range []string{"queue_reviewer", "queue_fixer"} {
		queue, err := repos.Queue.GetByID(context.Background(), queueID)
		if err != nil || queue == nil || queue.Status != "cancelled" {
			t.Fatalf("%s = (%#v, %v), want cancelled", queueID, queue, err)
		}
	}

	result, err := ApplyReviewFixBudgetAnswer(context.Background(), repos, parked, "Continue", nowISO, testBudgetCaps(8, 8))
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
	for _, queueID := range []string{"queue_reviewer", "queue_fixer"} {
		queue, err := repos.Queue.GetByID(context.Background(), queueID)
		if err != nil || queue == nil || queue.Status != "queued" {
			t.Fatalf("%s after continue = (%#v, %v), want queued", queueID, queue, err)
		}
	}
}

func TestContinueReviewFixBudgetEnqueuesFreshWorkAfterCompletedClaim(t *testing.T) {
	t.Parallel()
	repos, nowISO := newBudgetFixture(t)
	reviewer := seedBudgetLoop(t, repos, nowISO, "loop_reviewer_completed", "reviewer", "running")
	fixer := seedBudgetLoop(t, repos, nowISO, "loop_fixer_completed", "fixer", "queued")
	seedBudgetQueue(t, repos, nowISO, "queue_reviewer_completed", reviewer.ID, "reviewer", storage.QueuePriorityReviewer)
	seedBudgetQueue(t, repos, nowISO, "queue_fixer_completed", fixer.ID, "fixer", storage.QueuePriorityFixer)
	if err := repos.Queue.Complete(context.Background(), "queue_reviewer_completed", nowISO); err != nil {
		t.Fatalf("Queue.Complete() error = %v", err)
	}

	parked, err := ParkReviewFixBudget(context.Background(), repos, ParkReviewFixBudgetInput{
		Exhausted: reviewer, Role: "reviewer", Repo: "acme/looper", PRNumber: 42, Count: 8, Cap: 8, NowISO: nowISO, HITLEnabled: true,
	})
	if err != nil {
		t.Fatalf("ParkReviewFixBudget() error = %v", err)
	}
	completed, err := repos.Queue.GetByID(context.Background(), "queue_reviewer_completed")
	if err != nil || completed == nil || completed.Status != "completed" {
		t.Fatalf("exhausted claim after park = (%#v, %v), want completed", completed, err)
	}
	siblingQueue, err := repos.Queue.GetByID(context.Background(), "queue_fixer_completed")
	if err != nil || siblingQueue == nil || siblingQueue.Status != "cancelled" {
		t.Fatalf("sibling queue after park = (%#v, %v), want cancelled", siblingQueue, err)
	}

	result, err := ApplyReviewFixBudgetAnswer(context.Background(), repos, parked, "Continue", nowISO, testBudgetCaps(8, 8))
	if err != nil || !result.Applied || result.Action != "continue" {
		t.Fatalf("ApplyReviewFixBudgetAnswer() = (%#v, %v)", result, err)
	}
	active, err := repos.Queue.FindActiveByLoopID(context.Background(), reviewer.ID)
	if err != nil || active == nil || active.Status != "queued" || active.ID == "queue_reviewer_completed" {
		t.Fatalf("exhausted active queue = (%#v, %v), want a fresh queued item", active, err)
	}
	completed, err = repos.Queue.GetByID(context.Background(), "queue_reviewer_completed")
	if err != nil || completed == nil || completed.Status != "completed" {
		t.Fatalf("original claim after continue = (%#v, %v), want still completed", completed, err)
	}
	siblingQueue, err = repos.Queue.GetByID(context.Background(), "queue_fixer_completed")
	if err != nil || siblingQueue == nil || siblingQueue.Status != "queued" {
		t.Fatalf("sibling queue after continue = (%#v, %v), want requeued", siblingQueue, err)
	}
}

func TestApplyReviewFixBudgetAnswerStopTerminatesPair(t *testing.T) {
	t.Parallel()
	repos, nowISO := newBudgetFixture(t)
	reviewer := seedBudgetLoop(t, repos, nowISO, "loop_reviewer_stop", "reviewer", "running")
	fixer := seedBudgetLoop(t, repos, nowISO, "loop_fixer_stop", "fixer", "queued")
	parked, err := ParkReviewFixBudget(context.Background(), repos, ParkReviewFixBudgetInput{
		Exhausted: reviewer, Role: "reviewer", Repo: "acme/looper", PRNumber: 42, Count: 8, Cap: 8, NowISO: nowISO, HITLEnabled: true,
	})
	if err != nil {
		t.Fatalf("ParkReviewFixBudget() error = %v", err)
	}

	result, err := ApplyReviewFixBudgetAnswer(context.Background(), repos, parked, "Stop", nowISO, testBudgetCaps(8, 8))
	if err != nil || !result.Applied || result.Action != "stop" {
		t.Fatalf("ApplyReviewFixBudgetAnswer() = (%#v, %v)", result, err)
	}
	if result.Loop.Status != "terminated" {
		t.Fatalf("exhausted after stop = %q, want terminated", result.Loop.Status)
	}
	sibling, err := repos.Loops.GetByID(context.Background(), fixer.ID)
	if err != nil || sibling == nil || sibling.Status != "terminated" {
		t.Fatalf("sibling after stop = (%#v, %v), want terminated", sibling, err)
	}
}

func TestFindSiblingReviewFixLoopsPrefersAutomaticLane(t *testing.T) {
	t.Parallel()
	repo := "acme/looper"
	pr := int64(42)
	reviewer := storage.LoopRecord{ID: "loop_reviewer", ProjectID: "project_1", Type: "reviewer", Repo: &repo, PRNumber: &pr}
	manual := `{"manual":true}`
	manualFixer := storage.LoopRecord{ID: "loop_manual_fixer", ProjectID: "project_1", Type: "fixer", Status: "queued", Repo: &repo, PRNumber: &pr, MetadataJSON: &manual}
	terminalFixer := storage.LoopRecord{ID: "loop_terminal_fixer", ProjectID: "project_1", Type: "fixer", Status: "terminated", Repo: &repo, PRNumber: &pr}
	automaticFixer := storage.LoopRecord{ID: "loop_auto_fixer", ProjectID: "project_1", Type: "fixer", Status: "queued", Repo: &repo, PRNumber: &pr}

	siblings := FindSiblingReviewFixLoops([]storage.LoopRecord{manualFixer, terminalFixer, automaticFixer}, reviewer)
	if len(siblings) != 2 || siblings[0].ID != terminalFixer.ID || siblings[1].ID != automaticFixer.ID {
		t.Fatalf("FindSiblingReviewFixLoops() = %#v, want automatic siblings only", siblings)
	}
}

func TestParkAndStopReviewFixBudgetSkipsManualSibling(t *testing.T) {
	t.Parallel()
	repos, nowISO := newBudgetFixture(t)
	reviewer := seedBudgetLoop(t, repos, nowISO, "loop_reviewer_multi", "reviewer", "running")
	manual := seedBudgetLoop(t, repos, nowISO, "loop_fixer_manual", "fixer", "queued")
	manualMeta := `{"manual":true}`
	manual.MetadataJSON = &manualMeta
	if err := repos.Loops.Upsert(context.Background(), manual); err != nil {
		t.Fatalf("Loops.Upsert(manual) error = %v", err)
	}
	automatic := seedBudgetLoop(t, repos, nowISO, "loop_fixer_auto", "fixer", "queued")
	seedBudgetQueue(t, repos, nowISO, "queue_fixer_manual", manual.ID, "fixer", storage.QueuePriorityFixer)
	seedBudgetQueue(t, repos, nowISO, "queue_fixer_auto", automatic.ID, "fixer", storage.QueuePriorityFixer)

	parked, err := ParkReviewFixBudget(context.Background(), repos, ParkReviewFixBudgetInput{
		Exhausted: reviewer, Role: "reviewer", Repo: "acme/looper", PRNumber: 42, Count: 8, Cap: 8, NowISO: nowISO, HITLEnabled: true,
	})
	if err != nil {
		t.Fatalf("ParkReviewFixBudget() error = %v", err)
	}
	manualAfter, err := repos.Loops.GetByID(context.Background(), manual.ID)
	if err != nil || manualAfter == nil || manualAfter.Status != "queued" {
		t.Fatalf("manual sibling = (%#v, %v), want still queued", manualAfter, err)
	}
	automaticAfter, err := repos.Loops.GetByID(context.Background(), automatic.ID)
	if err != nil || automaticAfter == nil || automaticAfter.Status != "paused" || !IsSiblingReviewFixBudgetPause(automaticAfter.MetadataJSON) {
		t.Fatalf("automatic sibling = (%#v, %v), want paused", automaticAfter, err)
	}

	result, err := ApplyReviewFixBudgetAnswer(context.Background(), repos, parked, "Stop", nowISO, testBudgetCaps(8, 8))
	if err != nil || !result.Applied || result.Action != "stop" {
		t.Fatalf("ApplyReviewFixBudgetAnswer() = (%#v, %v)", result, err)
	}
	manualAfter, err = repos.Loops.GetByID(context.Background(), manual.ID)
	if err != nil || manualAfter == nil || manualAfter.Status != "queued" {
		t.Fatalf("manual sibling after stop = (%#v, %v), want still queued", manualAfter, err)
	}
	automaticAfter, err = repos.Loops.GetByID(context.Background(), automatic.ID)
	if err != nil || automaticAfter == nil || automaticAfter.Status != "terminated" {
		t.Fatalf("automatic sibling after stop = (%#v, %v), want terminated", automaticAfter, err)
	}
}

func TestParkReviewFixBudgetCompletesSiblingAfterPartialPark(t *testing.T) {
	t.Parallel()
	repos, nowISO := newBudgetFixture(t)
	reviewer := seedBudgetLoop(t, repos, nowISO, "loop_reviewer_partial", "reviewer", "running")
	fixer := seedBudgetLoop(t, repos, nowISO, "loop_fixer_partial", "fixer", "queued")
	seedBudgetQueue(t, repos, nowISO, "queue_reviewer_partial", reviewer.ID, "reviewer", storage.QueuePriorityReviewer)
	seedBudgetQueue(t, repos, nowISO, "queue_fixer_partial", fixer.ID, "fixer", storage.QueuePriorityFixer)

	ask := NewReviewFixBudgetAsk("reviewer", "acme/looper", 42, 8, 8, nowISO)
	ask.Transport = "github"
	ask.AskCommentID = 99
	metadata, err := WriteHITLAsk(reviewer.MetadataJSON, ask)
	if err != nil {
		t.Fatalf("WriteHITLAsk() error = %v", err)
	}
	state := ReadReviewFixBudgetState(&metadata)
	state.ExhaustedBy = "reviewer"
	metadata, err = WriteReviewFixBudgetState(&metadata, state)
	if err != nil {
		t.Fatalf("WriteReviewFixBudgetState() error = %v", err)
	}
	reviewer.MetadataJSON = &metadata
	reviewer.Status = "awaiting_human"
	reviewer.NextRunAt = nil
	if err := repos.Loops.Upsert(context.Background(), reviewer); err != nil {
		t.Fatalf("Loops.Upsert(partial reviewer) error = %v", err)
	}

	parked, err := ParkReviewFixBudget(context.Background(), repos, ParkReviewFixBudgetInput{
		Exhausted: reviewer, Role: "reviewer", Repo: "acme/looper", PRNumber: 42, Count: 8, Cap: 8, NowISO: nowISO, HITLEnabled: true,
	})
	if err != nil {
		t.Fatalf("ParkReviewFixBudget() error = %v", err)
	}
	kept, ok := ReadHITLAsk(parked.MetadataJSON)
	if !ok || kept.Transport != "github" || kept.AskCommentID != 99 {
		t.Fatalf("HITL ask after retry = %#v, want preserved github delivery stamps", kept)
	}
	sibling, err := repos.Loops.GetByID(context.Background(), fixer.ID)
	if err != nil || sibling == nil || sibling.Status != "paused" || !IsSiblingReviewFixBudgetPause(sibling.MetadataJSON) {
		t.Fatalf("sibling after retry = (%#v, %v), want paused", sibling, err)
	}
	for _, queueID := range []string{"queue_reviewer_partial", "queue_fixer_partial"} {
		queue, err := repos.Queue.GetByID(context.Background(), queueID)
		if err != nil || queue == nil || queue.Status != "cancelled" {
			t.Fatalf("%s after retry = (%#v, %v), want cancelled", queueID, queue, err)
		}
	}
}

func TestContinueReviewFixBudgetRetriesAfterSiblingAlreadyUnpaused(t *testing.T) {
	t.Parallel()
	repos, nowISO := newBudgetFixture(t)
	reviewer := seedBudgetLoop(t, repos, nowISO, "loop_reviewer_retry", "reviewer", "running")
	fixer := seedBudgetLoop(t, repos, nowISO, "loop_fixer_retry", "fixer", "queued")
	seedBudgetQueue(t, repos, nowISO, "queue_reviewer_retry", reviewer.ID, "reviewer", storage.QueuePriorityReviewer)
	seedBudgetQueue(t, repos, nowISO, "queue_fixer_retry", fixer.ID, "fixer", storage.QueuePriorityFixer)

	parked, err := ParkReviewFixBudget(context.Background(), repos, ParkReviewFixBudgetInput{
		Exhausted: reviewer, Role: "reviewer", Repo: "acme/looper", PRNumber: 42, Count: 8, Cap: 8, NowISO: nowISO, HITLEnabled: true,
	})
	if err != nil {
		t.Fatalf("ParkReviewFixBudget() error = %v", err)
	}
	if err := releaseOneReviewFixBudgetHold(context.Background(), repos, fixer.ID, nowISO); err != nil {
		t.Fatalf("releaseOneReviewFixBudgetHold(sibling) error = %v", err)
	}

	result, err := ApplyReviewFixBudgetAnswer(context.Background(), repos, parked, "Continue", nowISO, testBudgetCaps(8, 8))
	if err != nil || !result.Applied || result.Action != "continue" {
		t.Fatalf("ApplyReviewFixBudgetAnswer() = (%#v, %v)", result, err)
	}
	if result.Loop.Status != "queued" {
		t.Fatalf("continued loop status = %q, want queued", result.Loop.Status)
	}
	if _, ok := ReadHITLAsk(result.Loop.MetadataJSON); ok {
		t.Fatal("continued loop still has a HITL ask")
	}
	sibling, err := repos.Loops.GetByID(context.Background(), fixer.ID)
	if err != nil || sibling == nil || sibling.Status != "queued" || IsSiblingReviewFixBudgetPause(sibling.MetadataJSON) {
		t.Fatalf("sibling after retry continue = (%#v, %v), want queued and unpaused", sibling, err)
	}
}

func TestParkAndContinueReviewFixBudgetSkipsPreexistingPausedSibling(t *testing.T) {
	t.Parallel()
	repos, nowISO := newBudgetFixture(t)
	reviewer := seedBudgetLoop(t, repos, nowISO, "loop_reviewer_pre_paused", "reviewer", "running")
	fixer := seedBudgetLoop(t, repos, nowISO, "loop_fixer_pre_paused", "fixer", "paused")
	pauseMeta := `{"pauseReason":"fixer_zero_progress"}`
	fixer.MetadataJSON = &pauseMeta
	if err := repos.Loops.Upsert(context.Background(), fixer); err != nil {
		t.Fatalf("Loops.Upsert(pre-paused fixer) error = %v", err)
	}
	seedBudgetQueue(t, repos, nowISO, "queue_reviewer_pre_paused", reviewer.ID, "reviewer", storage.QueuePriorityReviewer)
	seedBudgetQueue(t, repos, nowISO, "queue_fixer_pre_paused", fixer.ID, "fixer", storage.QueuePriorityFixer)

	parked, err := ParkReviewFixBudget(context.Background(), repos, ParkReviewFixBudgetInput{
		Exhausted: reviewer, Role: "reviewer", Repo: "acme/looper", PRNumber: 42, Count: 8, Cap: 8, NowISO: nowISO, HITLEnabled: true,
	})
	if err != nil {
		t.Fatalf("ParkReviewFixBudget() error = %v", err)
	}
	sibling, err := repos.Loops.GetByID(context.Background(), fixer.ID)
	if err != nil || sibling == nil || sibling.Status != "paused" || IsSiblingReviewFixBudgetPause(sibling.MetadataJSON) {
		t.Fatalf("sibling after park = (%#v, %v), want still independently paused", sibling, err)
	}
	if reason, _ := stringFromAny(parseMetadataObject(sibling.MetadataJSON)["pauseReason"]); reason != "fixer_zero_progress" {
		t.Fatalf("sibling pauseReason = %q, want fixer_zero_progress", reason)
	}
	siblingQueue, err := repos.Queue.GetByID(context.Background(), "queue_fixer_pre_paused")
	if err != nil || siblingQueue == nil || siblingQueue.Status != "queued" {
		t.Fatalf("sibling queue after park = (%#v, %v), want still queued", siblingQueue, err)
	}

	result, err := ApplyReviewFixBudgetAnswer(context.Background(), repos, parked, "Continue", nowISO, testBudgetCaps(8, 8))
	if err != nil || !result.Applied || result.Action != "continue" {
		t.Fatalf("ApplyReviewFixBudgetAnswer() = (%#v, %v)", result, err)
	}
	sibling, err = repos.Loops.GetByID(context.Background(), fixer.ID)
	if err != nil || sibling == nil || sibling.Status != "paused" || IsSiblingReviewFixBudgetPause(sibling.MetadataJSON) {
		t.Fatalf("sibling after continue = (%#v, %v), want still independently paused", sibling, err)
	}
	if reason, _ := stringFromAny(parseMetadataObject(sibling.MetadataJSON)["pauseReason"]); reason != "fixer_zero_progress" {
		t.Fatalf("sibling pauseReason after continue = %q, want fixer_zero_progress", reason)
	}
	siblingQueue, err = repos.Queue.GetByID(context.Background(), "queue_fixer_pre_paused")
	if err != nil || siblingQueue == nil || siblingQueue.Status != "queued" {
		t.Fatalf("sibling queue after continue = (%#v, %v), want not requeued as budget work", siblingQueue, err)
	}
}

func TestParkAndContinueReviewFixBudgetSkipsFailedSibling(t *testing.T) {
	t.Parallel()
	for _, status := range []string{"failed", "interrupted"} {
		status := status
		t.Run(status, func(t *testing.T) {
			t.Parallel()
			repos, nowISO := newBudgetFixture(t)
			reviewer := seedBudgetLoop(t, repos, nowISO, "loop_reviewer_"+status, "reviewer", "running")
			fixer := seedBudgetLoop(t, repos, nowISO, "loop_fixer_"+status, "fixer", status)
			seedBudgetQueue(t, repos, nowISO, "queue_reviewer_"+status, reviewer.ID, "reviewer", storage.QueuePriorityReviewer)
			seedBudgetQueue(t, repos, nowISO, "queue_fixer_"+status, fixer.ID, "fixer", storage.QueuePriorityFixer)

			parked, err := ParkReviewFixBudget(context.Background(), repos, ParkReviewFixBudgetInput{
				Exhausted: reviewer, Role: "reviewer", Repo: "acme/looper", PRNumber: 42, Count: 8, Cap: 8, NowISO: nowISO, HITLEnabled: true,
			})
			if err != nil {
				t.Fatalf("ParkReviewFixBudget() error = %v", err)
			}
			sibling, err := repos.Loops.GetByID(context.Background(), fixer.ID)
			if err != nil || sibling == nil || sibling.Status != status || IsSiblingReviewFixBudgetPause(sibling.MetadataJSON) {
				t.Fatalf("sibling after park = (%#v, %v), want still %s", sibling, err, status)
			}
			siblingQueue, err := repos.Queue.GetByID(context.Background(), "queue_fixer_"+status)
			if err != nil || siblingQueue == nil || siblingQueue.Status != "queued" {
				t.Fatalf("sibling queue after park = (%#v, %v), want still queued", siblingQueue, err)
			}

			result, err := ApplyReviewFixBudgetAnswer(context.Background(), repos, parked, "Continue", nowISO, testBudgetCaps(8, 8))
			if err != nil || !result.Applied || result.Action != "continue" {
				t.Fatalf("ApplyReviewFixBudgetAnswer() = (%#v, %v)", result, err)
			}
			sibling, err = repos.Loops.GetByID(context.Background(), fixer.ID)
			if err != nil || sibling == nil || sibling.Status != status || IsSiblingReviewFixBudgetPause(sibling.MetadataJSON) {
				t.Fatalf("sibling after continue = (%#v, %v), want still %s", sibling, err, status)
			}
			siblingQueue, err = repos.Queue.GetByID(context.Background(), "queue_fixer_"+status)
			if err != nil || siblingQueue == nil || siblingQueue.Status != "queued" {
				t.Fatalf("sibling queue after continue = (%#v, %v), want not requeued as fresh work", siblingQueue, err)
			}
		})
	}
}

func TestStopReviewFixBudgetRetriesAfterSiblingAlreadyTerminated(t *testing.T) {
	t.Parallel()
	repos, nowISO := newBudgetFixture(t)
	reviewer := seedBudgetLoop(t, repos, nowISO, "loop_reviewer_stop_retry", "reviewer", "running")
	fixer := seedBudgetLoop(t, repos, nowISO, "loop_fixer_stop_retry", "fixer", "queued")
	seedBudgetQueue(t, repos, nowISO, "queue_reviewer_stop_retry", reviewer.ID, "reviewer", storage.QueuePriorityReviewer)
	seedBudgetQueue(t, repos, nowISO, "queue_fixer_stop_retry", fixer.ID, "fixer", storage.QueuePriorityFixer)

	parked, err := ParkReviewFixBudget(context.Background(), repos, ParkReviewFixBudgetInput{
		Exhausted: reviewer, Role: "reviewer", Repo: "acme/looper", PRNumber: 42, Count: 8, Cap: 8, NowISO: nowISO, HITLEnabled: true,
	})
	if err != nil {
		t.Fatalf("ParkReviewFixBudget() error = %v", err)
	}
	if _, err := terminateReviewFixLoop(context.Background(), repos, fixer, nowISO, ReviewFixBudgetTerminationReason); err != nil {
		t.Fatalf("terminateReviewFixLoop(sibling) error = %v", err)
	}

	result, err := ApplyReviewFixBudgetAnswer(context.Background(), repos, parked, "Stop", nowISO, testBudgetCaps(8, 8))
	if err != nil || !result.Applied || result.Action != "stop" {
		t.Fatalf("ApplyReviewFixBudgetAnswer() = (%#v, %v)", result, err)
	}
	if result.Loop.Status != "terminated" {
		t.Fatalf("exhausted after retry stop = %q, want terminated", result.Loop.Status)
	}
	if _, ok := ReadHITLAsk(result.Loop.MetadataJSON); ok {
		t.Fatal("exhausted loop still has a HITL ask after retry stop")
	}
	sibling, err := repos.Loops.GetByID(context.Background(), fixer.ID)
	if err != nil || sibling == nil || sibling.Status != "terminated" {
		t.Fatalf("sibling after retry stop = (%#v, %v), want terminated", sibling, err)
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
	result, err := ApplyReviewFixBudgetAnswer(context.Background(), repos, loop, "Continue", nowISO, testBudgetCaps(8, 8))
	if err != nil || result.Applied {
		t.Fatalf("ApplyReviewFixBudgetAnswer() = (%#v, %v), want not applied", result, err)
	}
}

func TestBudgetExhaustedIndependentCapsAndZeroDisablesRole(t *testing.T) {
	t.Parallel()
	if BudgetExhausted(3, 0) {
		t.Fatal("cap 0 must disable only that role")
	}
	if !BudgetExhausted(3, 3) {
		t.Fatal("3/3 must be exhausted")
	}
	if BudgetExhausted(2, 3) {
		t.Fatal("2/3 must not be exhausted")
	}
	// Live cap lowered below current count halts on next admission.
	if !BudgetExhausted(5, 3) {
		t.Fatal("count above lowered live cap must be exhausted")
	}
}

func TestParkNoHITLPausesPairWithoutAsk(t *testing.T) {
	t.Parallel()
	repos, nowISO := newBudgetFixture(t)
	reviewer := seedBudgetLoop(t, repos, nowISO, "loop_reviewer_nohitl", "reviewer", "running")
	fixer := seedBudgetLoop(t, repos, nowISO, "loop_fixer_nohitl", "fixer", "queued")
	seedBudgetQueue(t, repos, nowISO, "queue_reviewer_nohitl", reviewer.ID, "reviewer", storage.QueuePriorityReviewer)
	seedBudgetQueue(t, repos, nowISO, "queue_fixer_nohitl", fixer.ID, "fixer", storage.QueuePriorityFixer)

	parked, err := ParkReviewFixBudget(context.Background(), repos, ParkReviewFixBudgetInput{
		Exhausted: reviewer, Role: "reviewer", Repo: "acme/looper", PRNumber: 42, Count: 3, Cap: 3, NowISO: nowISO, HITLEnabled: false,
	})
	if err != nil {
		t.Fatalf("ParkReviewFixBudget() error = %v", err)
	}
	if parked.Status != "paused" || !IsReviewFixBudgetExhaustedPause(parked.MetadataJSON) {
		t.Fatalf("exhausted = %#v, want paused with review_fix_budget_exhausted", parked)
	}
	if _, ok := ReadHITLAsk(parked.MetadataJSON); ok {
		t.Fatal("no-HITL park must not write a HITL ask")
	}
	sibling, err := repos.Loops.GetByID(context.Background(), fixer.ID)
	if err != nil || sibling == nil || sibling.Status != "paused" || !IsSiblingReviewFixBudgetPause(sibling.MetadataJSON) {
		t.Fatalf("sibling = (%#v, %v), want paired budget pause", sibling, err)
	}
	for _, queueID := range []string{"queue_reviewer_nohitl", "queue_fixer_nohitl"} {
		queue, err := repos.Queue.GetByID(context.Background(), queueID)
		if err != nil || queue == nil || queue.Status != "cancelled" {
			t.Fatalf("%s = (%#v, %v), want cancelled", queueID, queue, err)
		}
	}
}

func TestContinueNoHITLFromEitherRoleResetsOnlyExhaustedMeters(t *testing.T) {
	t.Parallel()
	repos, nowISO := newBudgetFixture(t)
	reviewerMeta := `{"loop":{"iterationCount":3}}`
	fixerMeta := `{"reviewFixBudget":{"pushCount":1}}`
	reviewer := seedBudgetLoop(t, repos, nowISO, "loop_reviewer_cont_nohitl", "reviewer", "running")
	reviewer.MetadataJSON = &reviewerMeta
	if err := repos.Loops.Upsert(context.Background(), reviewer); err != nil {
		t.Fatalf("Upsert reviewer: %v", err)
	}
	fixer := seedBudgetLoop(t, repos, nowISO, "loop_fixer_cont_nohitl", "fixer", "queued")
	fixer.MetadataJSON = &fixerMeta
	if err := repos.Loops.Upsert(context.Background(), fixer); err != nil {
		t.Fatalf("Upsert fixer: %v", err)
	}
	seedBudgetQueue(t, repos, nowISO, "queue_reviewer_cont_nohitl", reviewer.ID, "reviewer", storage.QueuePriorityReviewer)
	seedBudgetQueue(t, repos, nowISO, "queue_fixer_cont_nohitl", fixer.ID, "fixer", storage.QueuePriorityFixer)

	parked, err := ParkReviewFixBudget(context.Background(), repos, ParkReviewFixBudgetInput{
		Exhausted: reviewer, Role: "reviewer", Repo: "acme/looper", PRNumber: 42, Count: 3, Cap: 3, NowISO: nowISO, HITLEnabled: false,
	})
	if err != nil {
		t.Fatalf("ParkReviewFixBudget() error = %v", err)
	}
	// Unpause from sibling side.
	sibling, err := repos.Loops.GetByID(context.Background(), fixer.ID)
	if err != nil || sibling == nil {
		t.Fatalf("sibling get: %v", err)
	}
	result, err := ApplyReviewFixBudgetAnswer(context.Background(), repos, *sibling, "Continue", nowISO, testBudgetCaps(3, 3))
	if err != nil || !result.Applied || result.Action != "continue" {
		t.Fatalf("Continue from sibling = (%#v, %v)", result, err)
	}
	reviewerAfter, err := repos.Loops.GetByID(context.Background(), parked.ID)
	if err != nil || reviewerAfter == nil || reviewerAfter.Status != "queued" || ReviewerPublishCount(reviewerAfter.MetadataJSON) != 0 {
		t.Fatalf("reviewer after continue = (%#v, %v), want queued with reset count", reviewerAfter, err)
	}
	fixerAfter, err := repos.Loops.GetByID(context.Background(), fixer.ID)
	if err != nil || fixerAfter == nil || fixerAfter.Status != "queued" || FixerPushCount(fixerAfter.MetadataJSON) != 1 {
		t.Fatalf("fixer after continue = (%#v, %v), want queued with preserved unused push budget", fixerAfter, err)
	}
}

func TestContinueResetsBothMetersWhenBothExhausted(t *testing.T) {
	t.Parallel()
	repos, nowISO := newBudgetFixture(t)
	reviewerMeta := `{"loop":{"iterationCount":3}}`
	fixerMeta := `{"reviewFixBudget":{"pushCount":3}}`
	reviewer := seedBudgetLoop(t, repos, nowISO, "loop_reviewer_both", "reviewer", "running")
	reviewer.MetadataJSON = &reviewerMeta
	if err := repos.Loops.Upsert(context.Background(), reviewer); err != nil {
		t.Fatalf("Upsert reviewer: %v", err)
	}
	fixer := seedBudgetLoop(t, repos, nowISO, "loop_fixer_both", "fixer", "queued")
	fixer.MetadataJSON = &fixerMeta
	if err := repos.Loops.Upsert(context.Background(), fixer); err != nil {
		t.Fatalf("Upsert fixer: %v", err)
	}
	parked, err := ParkReviewFixBudget(context.Background(), repos, ParkReviewFixBudgetInput{
		Exhausted: reviewer, Role: "reviewer", Repo: "acme/looper", PRNumber: 42, Count: 3, Cap: 3, NowISO: nowISO, HITLEnabled: true,
	})
	if err != nil {
		t.Fatalf("ParkReviewFixBudget() error = %v", err)
	}
	result, err := ApplyReviewFixBudgetAnswer(context.Background(), repos, parked, "Continue", nowISO, testBudgetCaps(3, 3))
	if err != nil || !result.Applied {
		t.Fatalf("Continue = (%#v, %v)", result, err)
	}
	if ReviewerPublishCount(result.Loop.MetadataJSON) != 0 {
		t.Fatalf("reviewer count = %d, want 0", ReviewerPublishCount(result.Loop.MetadataJSON))
	}
	fixerAfter, _ := repos.Loops.GetByID(context.Background(), fixer.ID)
	if fixerAfter == nil || FixerPushCount(fixerAfter.MetadataJSON) != 0 {
		t.Fatalf("fixer count = %#v, want reset to 0", fixerAfter)
	}
}

func TestContinueReviewFixBudgetDoesNotResetHistoricalSiblingMeters(t *testing.T) {
	t.Parallel()
	for _, status := range []string{"terminated", "completed"} {
		status := status
		t.Run(status, func(t *testing.T) {
			t.Parallel()
			repos, nowISO := newBudgetFixture(t)
			reviewerMeta := `{"loop":{"iterationCount":3}}`
			liveFixerMeta := `{"reviewFixBudget":{"pushCount":1}}`
			historicalMeta := `{"reviewFixBudget":{"pushCount":3}}`
			reviewer := seedBudgetLoop(t, repos, nowISO, "loop_reviewer_hist_"+status, "reviewer", "running")
			reviewer.MetadataJSON = &reviewerMeta
			if err := repos.Loops.Upsert(context.Background(), reviewer); err != nil {
				t.Fatalf("Upsert reviewer: %v", err)
			}
			liveFixer := seedBudgetLoop(t, repos, nowISO, "loop_fixer_hist_live_"+status, "fixer", "queued")
			liveFixer.MetadataJSON = &liveFixerMeta
			if err := repos.Loops.Upsert(context.Background(), liveFixer); err != nil {
				t.Fatalf("Upsert live fixer: %v", err)
			}
			historical := seedBudgetLoop(t, repos, nowISO, "loop_fixer_hist_"+status, "fixer", status)
			historical.MetadataJSON = &historicalMeta
			if err := repos.Loops.Upsert(context.Background(), historical); err != nil {
				t.Fatalf("Upsert historical fixer: %v", err)
			}

			parked, err := ParkReviewFixBudget(context.Background(), repos, ParkReviewFixBudgetInput{
				Exhausted: reviewer, Role: "reviewer", Repo: "acme/looper", PRNumber: 42, Count: 3, Cap: 3, NowISO: nowISO, HITLEnabled: true,
			})
			if err != nil {
				t.Fatalf("ParkReviewFixBudget() error = %v", err)
			}
			result, err := ApplyReviewFixBudgetAnswer(context.Background(), repos, parked, "Continue", nowISO, testBudgetCaps(3, 3))
			if err != nil || !result.Applied {
				t.Fatalf("Continue = (%#v, %v)", result, err)
			}
			if ReviewerPublishCount(result.Loop.MetadataJSON) != 0 {
				t.Fatalf("reviewer count = %d, want 0", ReviewerPublishCount(result.Loop.MetadataJSON))
			}
			liveAfter, err := repos.Loops.GetByID(context.Background(), liveFixer.ID)
			if err != nil || liveAfter == nil || liveAfter.Status != "queued" || FixerPushCount(liveAfter.MetadataJSON) != 1 {
				t.Fatalf("live fixer = (%#v, %v), want queued with preserved unused push budget", liveAfter, err)
			}
			histAfter, err := repos.Loops.GetByID(context.Background(), historical.ID)
			if err != nil || histAfter == nil || histAfter.Status != status || FixerPushCount(histAfter.MetadataJSON) != 3 {
				t.Fatalf("historical fixer = (%#v, %v), want %s with original pushCount 3", histAfter, err, status)
			}
		})
	}
}

func TestStopReviewFixBudgetDoesNotTerminateHistoricalSibling(t *testing.T) {
	t.Parallel()
	for _, status := range []string{"terminated", "completed"} {
		status := status
		t.Run(status, func(t *testing.T) {
			t.Parallel()
			repos, nowISO := newBudgetFixture(t)
			contMeta := `{"manual":true,"followUpdates":true,"loop":{"iterationCount":3}}`
			liveFixerMeta := `{"manual":true,"followUpdates":true,"reviewFixBudget":{"pushCount":1}}`
			historicalMeta := `{"manual":true,"followUpdates":true,"reviewFixBudget":{"pushCount":3,"exhaustedBy":"fixer"}}`
			reviewer := seedBudgetLoop(t, repos, nowISO, "loop_reviewer_stop_hist_"+status, "reviewer", "running")
			reviewer.MetadataJSON = &contMeta
			if err := repos.Loops.Upsert(context.Background(), reviewer); err != nil {
				t.Fatalf("Upsert reviewer: %v", err)
			}
			liveFixer := seedBudgetLoop(t, repos, nowISO, "loop_fixer_stop_hist_live_"+status, "fixer", "queued")
			liveFixer.MetadataJSON = &liveFixerMeta
			if err := repos.Loops.Upsert(context.Background(), liveFixer); err != nil {
				t.Fatalf("Upsert live fixer: %v", err)
			}
			historical := seedBudgetLoop(t, repos, nowISO, "loop_fixer_stop_hist_"+status, "fixer", status)
			historical.MetadataJSON = &historicalMeta
			if err := repos.Loops.Upsert(context.Background(), historical); err != nil {
				t.Fatalf("Upsert historical fixer: %v", err)
			}

			parked, err := ParkReviewFixBudget(context.Background(), repos, ParkReviewFixBudgetInput{
				Exhausted: reviewer, Role: "reviewer", Repo: "acme/looper", PRNumber: 42, Count: 3, Cap: 3, NowISO: nowISO, HITLEnabled: true,
			})
			if err != nil {
				t.Fatalf("ParkReviewFixBudget() error = %v", err)
			}
			result, err := ApplyReviewFixBudgetAnswer(context.Background(), repos, parked, "Stop", nowISO, testBudgetCaps(3, 3))
			if err != nil || !result.Applied || result.Action != "stop" {
				t.Fatalf("Stop = (%#v, %v)", result, err)
			}
			if result.Loop.Status != "terminated" {
				t.Fatalf("exhausted after stop = %q, want terminated", result.Loop.Status)
			}
			liveAfter, err := repos.Loops.GetByID(context.Background(), liveFixer.ID)
			if err != nil || liveAfter == nil || liveAfter.Status != "terminated" {
				t.Fatalf("live fixer = (%#v, %v), want terminated", liveAfter, err)
			}
			histAfter, err := repos.Loops.GetByID(context.Background(), historical.ID)
			if err != nil || histAfter == nil || histAfter.Status != status || FixerPushCount(histAfter.MetadataJSON) != 3 {
				t.Fatalf("historical fixer = (%#v, %v), want %s with original pushCount 3", histAfter, err, status)
			}
			if histAfter.MetadataJSON == nil || *histAfter.MetadataJSON != historicalMeta {
				t.Fatalf("historical metadata = %v, want unchanged", histAfter.MetadataJSON)
			}
		})
	}
}

func TestStopNoHITLFromEitherRoleTerminatesPair(t *testing.T) {
	t.Parallel()
	repos, nowISO := newBudgetFixture(t)
	reviewer := seedBudgetLoop(t, repos, nowISO, "loop_reviewer_stop_nohitl", "reviewer", "running")
	fixer := seedBudgetLoop(t, repos, nowISO, "loop_fixer_stop_nohitl", "fixer", "queued")
	parked, err := ParkReviewFixBudget(context.Background(), repos, ParkReviewFixBudgetInput{
		Exhausted: reviewer, Role: "reviewer", Repo: "acme/looper", PRNumber: 42, Count: 3, Cap: 3, NowISO: nowISO, HITLEnabled: false,
	})
	if err != nil {
		t.Fatalf("ParkReviewFixBudget() error = %v", err)
	}
	sibling, err := repos.Loops.GetByID(context.Background(), fixer.ID)
	if err != nil || sibling == nil {
		t.Fatalf("sibling get: %v", err)
	}
	result, err := ApplyReviewFixBudgetAnswer(context.Background(), repos, *sibling, "Stop", nowISO, testBudgetCaps(3, 3))
	if err != nil || !result.Applied || result.Action != "stop" {
		t.Fatalf("Stop from sibling = (%#v, %v)", result, err)
	}
	reviewerAfter, _ := repos.Loops.GetByID(context.Background(), parked.ID)
	fixerAfter, _ := repos.Loops.GetByID(context.Background(), fixer.ID)
	if reviewerAfter == nil || reviewerAfter.Status != "terminated" || fixerAfter == nil || fixerAfter.Status != "terminated" {
		t.Fatalf("pair after stop = reviewer %#v fixer %#v, want both terminated", reviewerAfter, fixerAfter)
	}
}

func TestFindSiblingSameLanePairing(t *testing.T) {
	t.Parallel()
	repo := "acme/looper"
	pr := int64(42)
	reviewer := storage.LoopRecord{ID: "loop_reviewer", ProjectID: "project_1", Type: "reviewer", Repo: &repo, PRNumber: &pr}
	oneShot := `{"manual":true,"followUpdates":false}`
	continuous := `{"manual":true,"followUpdates":true}`
	oneShotFixer := storage.LoopRecord{ID: "loop_oneshot", ProjectID: "project_1", Type: "fixer", Status: "queued", Repo: &repo, PRNumber: &pr, MetadataJSON: &oneShot}
	continuousFixer := storage.LoopRecord{ID: "loop_continuous", ProjectID: "project_1", Type: "fixer", Status: "queued", Repo: &repo, PRNumber: &pr, MetadataJSON: &continuous}
	autoFixer := storage.LoopRecord{ID: "loop_auto", ProjectID: "project_1", Type: "fixer", Status: "queued", Repo: &repo, PRNumber: &pr}

	// Automatic reviewer pairs only with automatic fixer.
	got := FindSiblingReviewFixLoops([]storage.LoopRecord{oneShotFixer, continuousFixer, autoFixer}, reviewer)
	if len(got) != 1 || got[0].ID != autoFixer.ID {
		t.Fatalf("automatic siblings = %#v, want only auto fixer", got)
	}
	if ParticipatesInReviewFixBudget(oneShotFixer) {
		t.Fatal("one-shot manual must not participate")
	}
	if !ParticipatesInReviewFixBudget(continuousFixer) {
		t.Fatal("continuous manual must participate")
	}

	// Continuous manual reviewer pairs only with continuous manual fixer.
	contReviewerMeta := continuous
	contReviewer := storage.LoopRecord{ID: "loop_cont_reviewer", ProjectID: "project_1", Type: "reviewer", Repo: &repo, PRNumber: &pr, MetadataJSON: &contReviewerMeta}
	got = FindSiblingReviewFixLoops([]storage.LoopRecord{oneShotFixer, continuousFixer, autoFixer}, contReviewer)
	if len(got) != 1 || got[0].ID != continuousFixer.ID {
		t.Fatalf("continuous_manual siblings = %#v, want only continuous fixer", got)
	}
}

func TestParkContinuousManualPairsWithSameLaneOnly(t *testing.T) {
	t.Parallel()
	repos, nowISO := newBudgetFixture(t)
	contMeta := `{"manual":true,"followUpdates":true,"loop":{"iterationCount":3}}`
	autoMeta := `{"loop":{"iterationCount":0}}`
	reviewer := seedBudgetLoop(t, repos, nowISO, "loop_cont_rev_park", "reviewer", "running")
	reviewer.MetadataJSON = &contMeta
	if err := repos.Loops.Upsert(context.Background(), reviewer); err != nil {
		t.Fatalf("Upsert reviewer: %v", err)
	}
	autoFixer := seedBudgetLoop(t, repos, nowISO, "loop_auto_fix_park", "fixer", "queued")
	autoFixer.MetadataJSON = &autoMeta
	if err := repos.Loops.Upsert(context.Background(), autoFixer); err != nil {
		t.Fatalf("Upsert auto fixer: %v", err)
	}
	contFixer := seedBudgetLoop(t, repos, nowISO, "loop_cont_fix_park", "fixer", "queued")
	contFixerMeta := `{"manual":true,"followUpdates":true}`
	contFixer.MetadataJSON = &contFixerMeta
	if err := repos.Loops.Upsert(context.Background(), contFixer); err != nil {
		t.Fatalf("Upsert cont fixer: %v", err)
	}

	_, err := ParkReviewFixBudget(context.Background(), repos, ParkReviewFixBudgetInput{
		Exhausted: reviewer, Role: "reviewer", Repo: "acme/looper", PRNumber: 42, Count: 3, Cap: 3, NowISO: nowISO, HITLEnabled: false,
	})
	if err != nil {
		t.Fatalf("ParkReviewFixBudget() error = %v", err)
	}
	autoAfter, _ := repos.Loops.GetByID(context.Background(), autoFixer.ID)
	contAfter, _ := repos.Loops.GetByID(context.Background(), contFixer.ID)
	if autoAfter == nil || autoAfter.Status != "queued" || IsSiblingReviewFixBudgetPause(autoAfter.MetadataJSON) {
		t.Fatalf("automatic fixer = %#v, want untouched", autoAfter)
	}
	if contAfter == nil || contAfter.Status != "paused" || !IsSiblingReviewFixBudgetPause(contAfter.MetadataJSON) {
		t.Fatalf("continuous fixer = %#v, want paired pause", contAfter)
	}
}

func TestIsReviewFixBudgetHoldCoversExhaustedAndSibling(t *testing.T) {
	t.Parallel()
	repos, nowISO := newBudgetFixture(t)
	reviewer := seedBudgetLoop(t, repos, nowISO, "loop_hold_rev", "reviewer", "running")
	fixer := seedBudgetLoop(t, repos, nowISO, "loop_hold_fix", "fixer", "queued")
	parked, err := ParkReviewFixBudget(context.Background(), repos, ParkReviewFixBudgetInput{
		Exhausted: reviewer, Role: "reviewer", Repo: "acme/looper", PRNumber: 42, Count: 3, Cap: 3, NowISO: nowISO, HITLEnabled: false,
	})
	if err != nil {
		t.Fatalf("park: %v", err)
	}
	if !IsReviewFixBudgetHold(parked) {
		t.Fatal("exhausted no-HITL hold must be detected")
	}
	sibling, _ := repos.Loops.GetByID(context.Background(), fixer.ID)
	if sibling == nil || !IsReviewFixBudgetHold(*sibling) {
		t.Fatalf("sibling hold = %#v", sibling)
	}
}

func TestParkSiblingFailureDoesNotLeaveSiblingRunnableWhileExhaustedHeld(t *testing.T) {
	// Serial: uses package-level inject hook.
	coordinator := openCoordinator(t)
	db := coordinator.DB()
	repos := storage.NewRepositories(db)
	now := time.Date(2026, time.April, 17, 12, 34, 56, 0, time.UTC)
	nowISO := now.UTC().Format("2006-01-02T15:04:05.000Z")
	seedProject(t, repos, now)
	reviewer := seedBudgetLoop(t, repos, nowISO, "loop_sib_fail_rev", "reviewer", "running")
	fixer := seedBudgetLoop(t, repos, nowISO, "loop_sib_fail_fix", "fixer", "queued")
	seedBudgetQueue(t, repos, nowISO, "queue_sib_fail_fix", fixer.ID, "fixer", storage.QueuePriorityFixer)

	reviewFixBudgetSiblingParkHook = func(sibling storage.LoopRecord) error {
		if sibling.ID == fixer.ID {
			return fmt.Errorf("injected sibling park failure")
		}
		return nil
	}
	t.Cleanup(func() { reviewFixBudgetSiblingParkHook = nil })

	_, err := ParkReviewFixBudget(context.Background(), repos, ParkReviewFixBudgetInput{
		Exhausted: reviewer, Role: "reviewer", Repo: "acme/looper", PRNumber: 42, Count: 3, Cap: 3,
		NowISO: nowISO, HITLEnabled: false, DB: db,
		LiveCaps: testBudgetCaps(3, 3),
	})
	if err == nil {
		t.Fatal("ParkReviewFixBudget() error = nil, want injected sibling failure")
	}

	exhausted, _ := repos.Loops.GetByID(context.Background(), reviewer.ID)
	sibling, _ := repos.Loops.GetByID(context.Background(), fixer.ID)
	// TX rollback: exhausted must not be held while sibling remains runnable.
	if exhausted != nil && IsReviewFixBudgetHold(*exhausted) && sibling != nil && (sibling.Status == "queued" || sibling.Status == "running") {
		t.Fatalf("partial park left exhausted held and sibling runnable: exhausted=%#v sibling=%#v", exhausted, sibling)
	}
	if sibling != nil && sibling.Status == "queued" && exhausted != nil && exhausted.Status == "paused" {
		t.Fatalf("sibling still queued while exhausted paused: exhausted=%#v sibling=%#v", exhausted, sibling)
	}
}

func TestContinueFailureDoesNotLeaveSiblingQueuedWhileExhaustedHeld(t *testing.T) {
	// Serial: uses package-level inject hook.
	coordinator := openCoordinator(t)
	db := coordinator.DB()
	repos := storage.NewRepositories(db)
	now := time.Date(2026, time.April, 17, 12, 34, 56, 0, time.UTC)
	nowISO := now.UTC().Format("2006-01-02T15:04:05.000Z")
	seedProject(t, repos, now)
	reviewer := seedBudgetLoop(t, repos, nowISO, "loop_cont_fail_rev", "reviewer", "running")
	fixer := seedBudgetLoop(t, repos, nowISO, "loop_cont_fail_fix", "fixer", "queued")
	parked, err := ParkReviewFixBudget(context.Background(), repos, ParkReviewFixBudgetInput{
		Exhausted: reviewer, Role: "reviewer", Repo: "acme/looper", PRNumber: 42, Count: 3, Cap: 3,
		NowISO: nowISO, HITLEnabled: true, DB: db, LiveCaps: testBudgetCaps(3, 3),
	})
	if err != nil {
		t.Fatalf("park: %v", err)
	}

	// Fail after exhausted side is released (first release). With TX, both roll back.
	released := 0
	reviewFixBudgetReleaseHook = func(loopID string) error {
		released++
		if released == 1 {
			return fmt.Errorf("injected continue failure after first release")
		}
		return nil
	}
	t.Cleanup(func() { reviewFixBudgetReleaseHook = nil })

	_, err = storage.WithTransactionValue(context.Background(), db, nil, func(tx *sql.Tx) (storage.LoopRecord, error) {
		txRepos := storage.NewRepositories(tx)
		fresh, getErr := txRepos.Loops.GetByID(context.Background(), parked.ID)
		if getErr != nil || fresh == nil {
			return storage.LoopRecord{}, fmt.Errorf("fresh: %v", getErr)
		}
		result, applyErr := ApplyReviewFixBudgetAnswer(context.Background(), txRepos, *fresh, "Continue", nowISO, testBudgetCaps(3, 3))
		if applyErr != nil {
			return storage.LoopRecord{}, applyErr
		}
		return result.Loop, nil
	})
	if err == nil {
		t.Fatal("Continue TX error = nil, want injected failure")
	}

	exhausted, _ := repos.Loops.GetByID(context.Background(), reviewer.ID)
	sibling, _ := repos.Loops.GetByID(context.Background(), fixer.ID)
	if sibling != nil && sibling.Status == "queued" && exhausted != nil && IsReviewFixBudgetHold(*exhausted) {
		t.Fatalf("sibling queued while exhausted still held after failed Continue: exhausted=%#v sibling=%#v", exhausted, sibling)
	}
	// TX rollback: pair should still be held.
	if exhausted == nil || !IsReviewFixBudgetHold(*exhausted) {
		t.Fatalf("exhausted after failed Continue = %#v, want still held (TX rollback)", exhausted)
	}
	if sibling == nil || !IsReviewFixBudgetHold(*sibling) {
		t.Fatalf("sibling after failed Continue = %#v, want still held (TX rollback)", sibling)
	}
}

func TestHandoffEventIncludesHeadAndConcreteResumeCommands(t *testing.T) {
	t.Parallel()
	repos, nowISO := newBudgetFixture(t)
	reviewerMeta := `{"lastPublishedHeadSha":"abc123def","lastReviewedSignalFingerprint":"sig-abc","loop":{"iterationCount":3}}`
	reviewer := seedBudgetLoop(t, repos, nowISO, "loop_handoff_head", "reviewer", "running")
	reviewer.MetadataJSON = &reviewerMeta
	if err := repos.Loops.Upsert(context.Background(), reviewer); err != nil {
		t.Fatalf("upsert reviewer: %v", err)
	}

	parked, err := ParkReviewFixBudget(context.Background(), repos, ParkReviewFixBudgetInput{
		Exhausted: reviewer, Role: "reviewer", Repo: "acme/looper", PRNumber: 42, Count: 3, Cap: 3,
		NowISO: nowISO, HITLEnabled: true, LiveCaps: testBudgetCaps(3, 3),
	})
	if err != nil {
		t.Fatalf("park: %v", err)
	}
	events, err := repos.Events.ListByEntity(context.Background(), "loop", parked.ID)
	if err != nil {
		t.Fatalf("ListByEntity: %v", err)
	}
	var handoff *storage.EventLogRecord
	for i := range events {
		if events[i].EventType == reviewFixBudgetHandoffEventType {
			handoff = &events[i]
			break
		}
	}
	if handoff == nil {
		t.Fatal("missing handoff event")
	}
	payloadJSON := handoff.PayloadJSON
	payload := parseMetadataObject(&payloadJSON)
	if head, _ := payload["head"].(string); head != "abc123def" {
		t.Fatalf("head = %q, want abc123def", head)
	}
	resume, _ := payload["resume"].(string)
	wantCLI := fmt.Sprintf("looper unpause %d / looper stop %d", parked.Seq, parked.Seq)
	if !strings.Contains(resume, "Continue / Stop") || !strings.Contains(resume, wantCLI) {
		t.Fatalf("resume = %q, want Continue/Stop plus %q", resume, wantCLI)
	}
	if lane, _ := payload["lane"].(string); lane != reviewFixBudgetLaneAutomatic {
		t.Fatalf("lane = %q, want %q", lane, reviewFixBudgetLaneAutomatic)
	}
	if signal, _ := payload["lastReviewedSignalFingerprint"].(string); signal != "sig-abc" {
		t.Fatalf("lastReviewedSignalFingerprint = %q, want sig-abc", signal)
	}
}

func TestBudgetHandoffReadsReviewerSignalForBothExhaustedRoles(t *testing.T) {
	t.Parallel()
	for _, exhaustedRole := range []string{"reviewer", "fixer"} {
		t.Run(exhaustedRole, func(t *testing.T) {
			t.Parallel()
			repos, nowISO := newBudgetFixture(t)
			reviewerMeta := `{"lastPublishedHeadSha":"abc123def","lastReviewedSignalFingerprint":"sig-reviewer","loop":{"iterationCount":3}}`
			reviewer := seedBudgetLoop(t, repos, nowISO, "loop_pair_sig_"+exhaustedRole+"_rev", "reviewer", "running")
			reviewer.MetadataJSON = &reviewerMeta
			if err := repos.Loops.Upsert(context.Background(), reviewer); err != nil {
				t.Fatalf("upsert reviewer: %v", err)
			}
			fixerMeta := `{"reviewFixBudget":{"pushCount":3}}`
			fixer := seedBudgetLoop(t, repos, nowISO, "loop_pair_sig_"+exhaustedRole+"_fix", "fixer", "queued")
			fixer.MetadataJSON = &fixerMeta
			if err := repos.Loops.Upsert(context.Background(), fixer); err != nil {
				t.Fatalf("upsert fixer: %v", err)
			}
			exhausted := reviewer
			if exhaustedRole == "fixer" {
				exhausted = fixer
			}
			parked, err := ParkReviewFixBudget(context.Background(), repos, ParkReviewFixBudgetInput{
				Exhausted: exhausted, Role: exhaustedRole, Repo: "acme/looper", PRNumber: 42, Count: 3, Cap: 3,
				NowISO: nowISO, HITLEnabled: false, LiveCaps: testBudgetCaps(3, 3),
			})
			if err != nil {
				t.Fatalf("park: %v", err)
			}
			payload := lastBudgetHandoffPayload(t, repos, parked.ID)
			if signal, _ := payload["lastReviewedSignalFingerprint"].(string); signal != "sig-reviewer" {
				t.Fatalf("lastReviewedSignalFingerprint = %q, want reviewer sibling sig-reviewer", signal)
			}
		})
	}
}

func TestBudgetHandoffRefreshesWhenPairSignalChanges(t *testing.T) {
	t.Parallel()
	for _, exhaustedRole := range []string{"reviewer", "fixer"} {
		t.Run(exhaustedRole, func(t *testing.T) {
			t.Parallel()
			repos, nowISO := newBudgetFixture(t)
			reviewerMeta := `{"lastPublishedHeadSha":"abc123def","lastReviewedSignalFingerprint":"sig-old","loop":{"iterationCount":3}}`
			reviewer := seedBudgetLoop(t, repos, nowISO, "loop_refresh_"+exhaustedRole+"_rev", "reviewer", "running")
			reviewer.MetadataJSON = &reviewerMeta
			if err := repos.Loops.Upsert(context.Background(), reviewer); err != nil {
				t.Fatalf("upsert reviewer: %v", err)
			}
			fixer := seedBudgetLoop(t, repos, nowISO, "loop_refresh_"+exhaustedRole+"_fix", "fixer", "queued")
			exhausted := reviewer
			if exhaustedRole == "fixer" {
				exhausted = fixer
			}
			parked, err := ParkReviewFixBudget(context.Background(), repos, ParkReviewFixBudgetInput{
				Exhausted: exhausted, Role: exhaustedRole, Repo: "acme/looper", PRNumber: 42, Count: 3, Cap: 3,
				NowISO: nowISO, HITLEnabled: false, LiveCaps: testBudgetCaps(3, 3),
			})
			if err != nil {
				t.Fatalf("park: %v", err)
			}
			if got := lastBudgetHandoffSignal(t, repos, parked.ID); got != "sig-old" {
				t.Fatalf("initial handoff signal = %q, want sig-old", got)
			}
			if err := RefreshReviewFixPairHandoff(context.Background(), repos, reviewer, "sig-new", nowISO); err != nil {
				t.Fatalf("RefreshReviewFixPairHandoff: %v", err)
			}
			if count := countBudgetHandoffs(t, repos, parked.ID); count != 2 {
				t.Fatalf("handoffs after signal change = %d, want 2", count)
			}
			if got := lastBudgetHandoffSignal(t, repos, parked.ID); got != "sig-new" {
				t.Fatalf("refreshed handoff signal = %q, want sig-new", got)
			}
			if err := RefreshReviewFixPairHandoff(context.Background(), repos, reviewer, "sig-new", nowISO); err != nil {
				t.Fatalf("idempotent refresh: %v", err)
			}
			if count := countBudgetHandoffs(t, repos, parked.ID); count != 2 {
				t.Fatalf("handoffs after unchanged refresh = %d, want 2", count)
			}
		})
	}
}

func TestRefreshHandoffDoesNotResurrectReleasedHold(t *testing.T) {
	repos, nowISO := newBudgetFixture(t)
	reviewerMeta := `{"lastPublishedHeadSha":"abc123def","lastReviewedSignalFingerprint":"sig-old","loop":{"iterationCount":3}}`
	reviewer := seedBudgetLoop(t, repos, nowISO, "loop_refresh_continue_rev", "reviewer", "running")
	reviewer.MetadataJSON = &reviewerMeta
	if err := repos.Loops.Upsert(context.Background(), reviewer); err != nil {
		t.Fatalf("upsert reviewer: %v", err)
	}
	fixer := seedBudgetLoop(t, repos, nowISO, "loop_refresh_continue_fix", "fixer", "queued")
	parked, err := ParkReviewFixBudget(context.Background(), repos, ParkReviewFixBudgetInput{
		Exhausted: reviewer, Role: "reviewer", Repo: "acme/looper", PRNumber: 42, Count: 3, Cap: 3,
		NowISO: nowISO, HITLEnabled: false, LiveCaps: testBudgetCaps(3, 3),
	})
	if err != nil {
		t.Fatalf("park: %v", err)
	}
	reviewFixBudgetHandoffPersistHook = func(exhausted storage.LoopRecord) error {
		result, err := ApplyReviewFixBudgetAnswer(context.Background(), repos, exhausted, "Continue", nowISO, testBudgetCaps(3, 3))
		if err != nil {
			return err
		}
		if !result.Applied {
			return fmt.Errorf("continue not applied")
		}
		return nil
	}
	t.Cleanup(func() { reviewFixBudgetHandoffPersistHook = nil })
	if err := RefreshReviewFixPairHandoff(context.Background(), repos, parked, "sig-new", nowISO); err != nil {
		t.Fatalf("RefreshReviewFixPairHandoff: %v", err)
	}
	after, err := repos.Loops.GetByID(context.Background(), parked.ID)
	if err != nil || after == nil {
		t.Fatalf("get reviewer: (%#v, %v)", after, err)
	}
	if IsReviewFixBudgetHold(*after) {
		t.Fatalf("Continue must remain released, got hold status=%s meta=%s", after.Status, derefLoopString(after.MetadataJSON))
	}
	sibling, err := repos.Loops.GetByID(context.Background(), fixer.ID)
	if err != nil || sibling == nil {
		t.Fatalf("get fixer: (%#v, %v)", sibling, err)
	}
	if IsReviewFixBudgetHold(*sibling) {
		t.Fatalf("sibling Continue must remain released, got hold status=%s meta=%s", sibling.Status, derefLoopString(sibling.MetadataJSON))
	}
}

func TestRefreshHandoffDoesNotResurrectHoldReleasedAfterGetByID(t *testing.T) {
	repos, nowISO := newBudgetFixture(t)
	reviewerMeta := `{"lastPublishedHeadSha":"abc123def","lastReviewedSignalFingerprint":"sig-old","loop":{"iterationCount":3}}`
	reviewer := seedBudgetLoop(t, repos, nowISO, "loop_refresh_after_get_rev", "reviewer", "running")
	reviewer.MetadataJSON = &reviewerMeta
	if err := repos.Loops.Upsert(context.Background(), reviewer); err != nil {
		t.Fatalf("upsert reviewer: %v", err)
	}
	fixer := seedBudgetLoop(t, repos, nowISO, "loop_refresh_after_get_fix", "fixer", "queued")
	parked, err := ParkReviewFixBudget(context.Background(), repos, ParkReviewFixBudgetInput{
		Exhausted: reviewer, Role: "reviewer", Repo: "acme/looper", PRNumber: 42, Count: 3, Cap: 3,
		NowISO: nowISO, HITLEnabled: false, LiveCaps: testBudgetCaps(3, 3),
	})
	if err != nil {
		t.Fatalf("park: %v", err)
	}
	reviewFixBudgetHandoffAfterRefreshHook = func(exhausted storage.LoopRecord) error {
		result, err := ApplyReviewFixBudgetAnswer(context.Background(), repos, exhausted, "Continue", nowISO, testBudgetCaps(3, 3))
		if err != nil {
			return err
		}
		if !result.Applied {
			return fmt.Errorf("continue not applied")
		}
		return nil
	}
	t.Cleanup(func() { reviewFixBudgetHandoffAfterRefreshHook = nil })
	if err := RefreshReviewFixPairHandoff(context.Background(), repos, parked, "sig-new", nowISO); err != nil {
		t.Fatalf("RefreshReviewFixPairHandoff: %v", err)
	}
	after, err := repos.Loops.GetByID(context.Background(), parked.ID)
	if err != nil || after == nil {
		t.Fatalf("get reviewer: (%#v, %v)", after, err)
	}
	if IsReviewFixBudgetHold(*after) {
		t.Fatalf("Continue must remain released, got hold status=%s meta=%s", after.Status, derefLoopString(after.MetadataJSON))
	}
	sibling, err := repos.Loops.GetByID(context.Background(), fixer.ID)
	if err != nil || sibling == nil {
		t.Fatalf("get fixer: (%#v, %v)", sibling, err)
	}
	if IsReviewFixBudgetHold(*sibling) {
		t.Fatalf("sibling Continue must remain released, got hold status=%s meta=%s", sibling.Status, derefLoopString(sibling.MetadataJSON))
	}
}

func TestHandoffEventIncludesBothRoleMetersAndRetriesOnReentry(t *testing.T) {
	t.Parallel()
	repos, nowISO := newBudgetFixture(t)
	reviewerMeta := `{"loop":{"iterationCount":3}}`
	reviewer := seedBudgetLoop(t, repos, nowISO, "loop_handoff_rev", "reviewer", "running")
	reviewer.MetadataJSON = &reviewerMeta
	if err := repos.Loops.Upsert(context.Background(), reviewer); err != nil {
		t.Fatalf("upsert reviewer: %v", err)
	}
	fixerMeta := `{"reviewFixBudget":{"pushCount":2}}`
	fixer := seedBudgetLoop(t, repos, nowISO, "loop_handoff_fix", "fixer", "queued")
	fixer.MetadataJSON = &fixerMeta
	if err := repos.Loops.Upsert(context.Background(), fixer); err != nil {
		t.Fatalf("upsert fixer: %v", err)
	}

	parked, err := ParkReviewFixBudget(context.Background(), repos, ParkReviewFixBudgetInput{
		Exhausted: reviewer, Role: "reviewer", Repo: "acme/looper", PRNumber: 42, Count: 3, Cap: 3,
		NowISO: nowISO, HITLEnabled: false, LiveCaps: testBudgetCaps(3, 5),
	})
	if err != nil {
		t.Fatalf("park: %v", err)
	}
	events, err := repos.Events.ListByEntity(context.Background(), "loop", parked.ID)
	if err != nil {
		t.Fatalf("ListByEntity: %v", err)
	}
	var handoff *storage.EventLogRecord
	for i := range events {
		if events[i].EventType == reviewFixBudgetHandoffEventType {
			handoff = &events[i]
			break
		}
	}
	if handoff == nil {
		t.Fatal("missing handoff event")
	}
	payloadJSON := handoff.PayloadJSON
	payload := parseMetadataObject(&payloadJSON)
	reviewerPayload, _ := payload["reviewer"].(map[string]any)
	fixerPayload, _ := payload["fixer"].(map[string]any)
	if intFromMetadata(reviewerPayload["count"]) != 3 || intFromMetadata(reviewerPayload["cap"]) != 3 {
		t.Fatalf("reviewer meters = %#v, want 3/3", reviewerPayload)
	}
	if intFromMetadata(fixerPayload["count"]) != 2 || intFromMetadata(fixerPayload["cap"]) != 5 {
		t.Fatalf("fixer meters = %#v, want 2/5", fixerPayload)
	}
	resume, _ := payload["resume"].(string)
	wantResume := fmt.Sprintf("looper unpause %d / looper stop %d", parked.Seq, parked.Seq)
	if resume != wantResume {
		t.Fatalf("resume = %q, want %q", resume, wantResume)
	}
	if _, ok := payload["head"]; ok {
		t.Fatalf("head = %#v, want omitted when metadata has no head sha", payload["head"])
	}

	// Clear handoff marker and delete event to force re-entry retry.
	state := ReadReviewFixBudgetState(parked.MetadataJSON)
	state.HandoffEventAt = ""
	encoded, err := WriteReviewFixBudgetState(parked.MetadataJSON, state)
	if err != nil {
		t.Fatalf("clear handoff marker: %v", err)
	}
	parked.MetadataJSON = &encoded
	if err := repos.Loops.Upsert(context.Background(), parked); err != nil {
		t.Fatalf("upsert cleared: %v", err)
	}
	// Re-entry should rewrite HandoffEventAt (event append is idempotent enough via marker).
	if _, err := ParkReviewFixBudget(context.Background(), repos, ParkReviewFixBudgetInput{
		Exhausted: parked, Role: "reviewer", Repo: "acme/looper", PRNumber: 42, Count: 3, Cap: 3,
		NowISO: nowISO, HITLEnabled: false, LiveCaps: testBudgetCaps(3, 5),
	}); err != nil {
		t.Fatalf("re-entry park: %v", err)
	}
	fresh, _ := repos.Loops.GetByID(context.Background(), parked.ID)
	if fresh == nil || strings.TrimSpace(ReadReviewFixBudgetState(fresh.MetadataJSON).HandoffEventAt) == "" {
		t.Fatalf("re-entry did not restore handoff marker: %#v", fresh)
	}
}

func TestContinueClearsHandoffMarkerWithoutMeterReset(t *testing.T) {
	t.Parallel()
	repos, nowISO := newBudgetFixture(t)
	reviewerMeta := `{"loop":{"iterationCount":3}}`
	reviewer := seedBudgetLoop(t, repos, nowISO, "loop_reviewer_cap_raise", "reviewer", "running")
	reviewer.MetadataJSON = &reviewerMeta
	if err := repos.Loops.Upsert(context.Background(), reviewer); err != nil {
		t.Fatalf("upsert reviewer: %v", err)
	}
	fixer := seedBudgetLoop(t, repos, nowISO, "loop_fixer_cap_raise", "fixer", "queued")
	parked, err := ParkReviewFixBudget(context.Background(), repos, ParkReviewFixBudgetInput{
		Exhausted: reviewer, Role: "reviewer", Repo: "acme/looper", PRNumber: 42, Count: 3, Cap: 3,
		NowISO: nowISO, HITLEnabled: false, LiveCaps: testBudgetCaps(3, 3),
	})
	if err != nil {
		t.Fatalf("park: %v", err)
	}
	freshParked, err := repos.Loops.GetByID(context.Background(), parked.ID)
	if err != nil || freshParked == nil {
		t.Fatalf("GetByID parked = (%v, %v)", freshParked, err)
	}
	parked = *freshParked
	if strings.TrimSpace(ReadReviewFixBudgetState(parked.MetadataJSON).HandoffEventAt) == "" {
		t.Fatal("expected handoff marker after first park")
	}

	result, err := ApplyReviewFixBudgetAnswer(context.Background(), repos, parked, "Continue", nowISO, testBudgetCaps(8, 8))
	if err != nil || !result.Applied {
		t.Fatalf("Continue after cap raise = (%#v, %v)", result, err)
	}
	if ReviewerPublishCount(result.Loop.MetadataJSON) != 3 {
		t.Fatalf("reviewer count = %d, want 3 (no reset under raised cap)", ReviewerPublishCount(result.Loop.MetadataJSON))
	}
	if strings.TrimSpace(ReadReviewFixBudgetState(result.Loop.MetadataJSON).HandoffEventAt) != "" {
		t.Fatalf("HandoffEventAt survived Continue without meter reset: %#v", result.Loop.MetadataJSON)
	}
	sibling, err := repos.Loops.GetByID(context.Background(), fixer.ID)
	if err != nil || sibling == nil || strings.TrimSpace(ReadReviewFixBudgetState(sibling.MetadataJSON).HandoffEventAt) != "" {
		t.Fatalf("sibling marker after Continue = (%#v, %v), want cleared", sibling, err)
	}

	continued := result.Loop
	meta := parseMetadataObject(continued.MetadataJSON)
	loopMeta, _ := meta[reviewerLoopMetadataKey].(map[string]any)
	if loopMeta == nil {
		loopMeta = map[string]any{}
	}
	loopMeta[reviewerIterationCountKey] = 8
	meta[reviewerLoopMetadataKey] = loopMeta
	encoded, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal raised count: %v", err)
	}
	raised := string(encoded)
	continued.MetadataJSON = &raised
	if err := repos.Loops.Upsert(context.Background(), continued); err != nil {
		t.Fatalf("upsert raised count: %v", err)
	}
	if _, err := ParkReviewFixBudget(context.Background(), repos, ParkReviewFixBudgetInput{
		Exhausted: continued, Role: "reviewer", Repo: "acme/looper", PRNumber: 42, Count: 8, Cap: 8,
		NowISO: nowISO, HITLEnabled: false, LiveCaps: testBudgetCaps(8, 8),
	}); err != nil {
		t.Fatalf("re-park after cap raise: %v", err)
	}
	fresh, err := repos.Loops.GetByID(context.Background(), continued.ID)
	if err != nil || fresh == nil || strings.TrimSpace(ReadReviewFixBudgetState(fresh.MetadataJSON).HandoffEventAt) == "" {
		t.Fatalf("new hold episode did not emit a handoff marker: (%#v, %v)", fresh, err)
	}
	events, err := repos.Events.ListByEntity(context.Background(), "loop", continued.ID)
	if err != nil {
		t.Fatalf("ListByEntity: %v", err)
	}
	handoffs := 0
	for i := range events {
		if events[i].EventType == reviewFixBudgetHandoffEventType {
			handoffs++
		}
	}
	if handoffs < 2 {
		t.Fatalf("handoff events = %d, want a fresh event for the new hold episode", handoffs)
	}
}

func TestParkReviewFixBudgetFailsClosedWhenRefreshErrors(t *testing.T) {
	coordinator := openCoordinator(t)
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 17, 12, 34, 56, 0, time.UTC)
	seedProject(t, repos, now)
	nowISO := now.UTC().Format("2006-01-02T15:04:05.000Z")
	reviewer := seedBudgetLoop(t, repos, nowISO, "loop_reviewer_refresh_err", "reviewer", "running")
	if err := coordinator.Close(); err != nil {
		t.Fatalf("close coordinator: %v", err)
	}
	if _, err := ParkReviewFixBudget(context.Background(), repos, ParkReviewFixBudgetInput{
		Exhausted: reviewer, Role: "reviewer", Repo: "acme/looper", PRNumber: 42, Count: 3, Cap: 3,
		NowISO: nowISO, HITLEnabled: false, LiveCaps: testBudgetCaps(3, 3),
	}); err == nil {
		t.Fatal("ParkReviewFixBudget error = nil, want refresh error")
	}
}

func testBudgetCaps(reviewerCap, fixerCap int) ReviewFixBudgetLiveCaps {
	return ReviewFixBudgetLiveCaps{ReviewerMaxPublishes: reviewerCap, FixerMaxPushes: fixerCap}
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
	// Stable unique seq from id bytes so parallel tests and multi-loop fixtures do not collide.
	var seq int64
	for _, b := range id {
		seq = seq*33 + int64(b)
	}
	if seq < 0 {
		seq = -seq
	}
	loop := storage.LoopRecord{
		ID: id, Seq: seq, ProjectID: "project_1", Type: loopType, TargetType: "pull_request",
		TargetID: &target, Repo: &repo, PRNumber: &pr, Status: status, CreatedAt: nowISO, UpdatedAt: nowISO,
		MetadataJSON: &metadata,
	}
	if err := repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert(%s) error = %v", id, err)
	}
	return loop
}

func seedBudgetQueue(t *testing.T, repos *storage.Repositories, nowISO, id, loopID, queueType string, priority int64) {
	t.Helper()
	if err := repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{
		ID: id, ProjectID: stringPtr("project_1"), LoopID: &loopID, Type: queueType,
		TargetType: "pull_request", TargetID: "pr:acme/looper:42", Status: "queued",
		Priority: priority, MaxAttempts: 3, AvailableAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO, DedupeKey: queueType + ":" + id,
	}); err != nil {
		t.Fatalf("Queue.Upsert(%s) error = %v", id, err)
	}
}

func stringPtr(value string) *string { return &value }

func lastBudgetHandoffPayload(t *testing.T, repos *storage.Repositories, loopID string) map[string]any {
	t.Helper()
	events, err := repos.Events.ListByEntity(context.Background(), "loop", loopID)
	if err != nil {
		t.Fatalf("ListByEntity: %v", err)
	}
	var last storage.EventLogRecord
	found := false
	for i := range events {
		if events[i].EventType == reviewFixBudgetHandoffEventType {
			last = events[i]
			found = true
		}
	}
	if !found {
		t.Fatal("missing budget handoff event")
	}
	payloadJSON := last.PayloadJSON
	return parseMetadataObject(&payloadJSON)
}

func lastBudgetHandoffSignal(t *testing.T, repos *storage.Repositories, loopID string) string {
	t.Helper()
	payload := lastBudgetHandoffPayload(t, repos, loopID)
	signal, _ := payload["lastReviewedSignalFingerprint"].(string)
	return signal
}

func countBudgetHandoffs(t *testing.T, repos *storage.Repositories, loopID string) int {
	t.Helper()
	events, err := repos.Events.ListByEntity(context.Background(), "loop", loopID)
	if err != nil {
		t.Fatalf("ListByEntity: %v", err)
	}
	n := 0
	for i := range events {
		if events[i].EventType == reviewFixBudgetHandoffEventType {
			n++
		}
	}
	return n
}
