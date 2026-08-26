package loops

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/storage"
)

func TestParkAndContinueReviewScopeHumanDoesNotResetMeters(t *testing.T) {
	t.Parallel()
	repos, nowISO := newBudgetFixture(t)
	reviewer := seedBudgetLoop(t, repos, nowISO, "loop_scope_reviewer", "reviewer", "running")
	// iterationCount 2 with cap 8 — not budget-exhausted.
	meta := `{"loop":{"iterationCount":2},"reviewFixBudget":{"pushCount":1}}`
	reviewer.MetadataJSON = &meta
	if err := repos.Loops.Upsert(context.Background(), reviewer); err != nil {
		t.Fatalf("upsert reviewer: %v", err)
	}
	fixer := seedBudgetLoop(t, repos, nowISO, "loop_scope_fixer", "fixer", "queued")
	fixerMeta := `{"reviewFixBudget":{"pushCount":1}}`
	fixer.MetadataJSON = &fixerMeta
	if err := repos.Loops.Upsert(context.Background(), fixer); err != nil {
		t.Fatalf("upsert fixer: %v", err)
	}

	parked, err := ParkReviewScopeHuman(context.Background(), repos, ParkReviewScopeHumanInput{
		Held: reviewer, Role: "reviewer", Repo: "acme/looper", PRNumber: 42,
		NowISO: nowISO, HITLEnabled: false,
		Question: "Is this in PR scope?",
	})
	if err != nil {
		t.Fatalf("ParkReviewScopeHuman() error = %v", err)
	}
	if parked.Status != "paused" || !IsReviewScopeHumanRequiredPause(parked.MetadataJSON) {
		t.Fatalf("parked = %#v, want no-ask scope pause", parked)
	}
	if IsReviewFixBudgetHold(parked) {
		t.Fatal("scope hold must not report as budget hold")
	}
	if !IsReviewFixPairHold(parked) {
		t.Fatal("scope hold must report as pair hold")
	}
	if ReviewerPublishCount(parked.MetadataJSON) != 2 {
		t.Fatalf("publish count = %d, want 2 preserved through park", ReviewerPublishCount(parked.MetadataJSON))
	}
	sibling, err := repos.Loops.GetByID(context.Background(), fixer.ID)
	if err != nil || sibling == nil || sibling.Status != "paused" || !IsSiblingReviewScopeHumanPause(sibling.MetadataJSON) {
		t.Fatalf("sibling = (%#v, %v), want scope sibling pause", sibling, err)
	}

	result, err := ApplyReviewScopeHumanAnswer(context.Background(), repos, parked, "Continue", nowISO)
	if err != nil || !result.Applied || result.Action != "continue" {
		t.Fatalf("ApplyReviewScopeHumanAnswer() = (%#v, %v)", result, err)
	}
	if ReviewerPublishCount(result.Loop.MetadataJSON) != 2 {
		t.Fatalf("after continue publish count = %d, want 2 (no meter reset)", ReviewerPublishCount(result.Loop.MetadataJSON))
	}
	if FixerPushCount(result.Loop.MetadataJSON) != 1 && FixerPushCount(sibling.MetadataJSON) != 1 {
		// Fixer meter lives on fixer loop.
	}
	freshFixer, _ := repos.Loops.GetByID(context.Background(), fixer.ID)
	if freshFixer == nil || FixerPushCount(freshFixer.MetadataJSON) != 1 {
		t.Fatalf("fixer push count after scope continue = %#v, want 1 preserved", freshFixer)
	}
	if result.Loop.Status != "queued" || IsReviewScopeHumanHold(result.Loop) {
		t.Fatalf("continued loop = %#v, want queued and released", result.Loop)
	}
	if freshFixer.Status != "queued" || IsReviewScopeHumanHold(*freshFixer) {
		t.Fatalf("sibling after continue = %#v, want queued and released", freshFixer)
	}
}

func TestParkReviewScopeHumanHITLAskNotBudgetKind(t *testing.T) {
	t.Parallel()
	repos, nowISO := newBudgetFixture(t)
	reviewer := seedBudgetLoop(t, repos, nowISO, "loop_scope_hitl_reviewer", "reviewer", "running")
	fixer := seedBudgetLoop(t, repos, nowISO, "loop_scope_hitl_fixer", "fixer", "queued")
	_ = fixer

	parked, err := ParkReviewScopeHuman(context.Background(), repos, ParkReviewScopeHumanInput{
		Held: reviewer, Role: "reviewer", Repo: "acme/looper", PRNumber: 42,
		NowISO: nowISO, HITLEnabled: true,
		Question: "Does AGENTS.md require this change?",
	})
	if err != nil {
		t.Fatalf("ParkReviewScopeHuman() error = %v", err)
	}
	if parked.Status != "awaiting_human" {
		t.Fatalf("status = %s, want awaiting_human", parked.Status)
	}
	ask, ok := ReadHITLAsk(parked.MetadataJSON)
	if !ok || !IsReviewScopeHumanAsk(ask) || IsReviewFixBudgetAsk(ask) {
		t.Fatalf("ask = (%#v, %v), want scope kind not budget", ask, ok)
	}
	if ask.Question == "" || len(ask.Options) != 2 {
		t.Fatalf("ask = %#v, want question and Continue/Stop options", ask)
	}
	if IsReviewFixBudgetHold(parked) {
		t.Fatal("HITL scope hold must not be a budget hold")
	}
}

func TestIsReviewFixBudgetHoldExcludesPureScopeHold(t *testing.T) {
	t.Parallel()
	repos, nowISO := newBudgetFixture(t)
	reviewer := seedBudgetLoop(t, repos, nowISO, "loop_scope_only", "reviewer", "running")
	parked, err := ParkReviewScopeHuman(context.Background(), repos, ParkReviewScopeHumanInput{
		Held: reviewer, Role: "reviewer", Repo: "acme/looper", PRNumber: 42,
		NowISO: nowISO, HITLEnabled: false,
	})
	if err != nil {
		t.Fatalf("park: %v", err)
	}
	if IsReviewFixBudgetHold(parked) {
		t.Fatal("IsReviewFixBudgetHold must be false for pure scope hold")
	}
	if !IsReviewScopeHumanHold(parked) || !IsReviewFixPairHold(parked) {
		t.Fatal("want scope + pair hold")
	}
}

func TestParkReviewScopeHumanHoldsFailedAndAwaitingHumanSiblings(t *testing.T) {
	t.Parallel()
	repos, nowISO := newBudgetFixture(t)
	reviewer := seedBudgetLoop(t, repos, nowISO, "loop_scope_sib_reviewer", "reviewer", "running")

	// Failed fixer sibling must still receive scope hold metadata.
	failedFixer := seedBudgetLoop(t, repos, nowISO, "loop_scope_sib_failed", "fixer", "failed")
	// Awaiting-human fixer with a mid-run (non-scope) ask must keep the ask body.
	awaitingFixer := seedBudgetLoop(t, repos, nowISO, "loop_scope_sib_awaiting", "fixer", "awaiting_human")
	// Use a distinct PR so FindSibling only pairs same PR — seed both on same PR as reviewer.
	// seedBudgetLoop already uses same repo/PR; having two fixers is ok for the hold check.
	midAsk := HITLAsk{
		Kind: "agent_question", Question: "Which approach should Fixer take?",
		Options: []string{"A", "B"}, Status: "awaiting", AskedAt: nowISO, PRNumber: 42,
	}
	meta, err := WriteHITLAsk(awaitingFixer.MetadataJSON, midAsk)
	if err != nil {
		t.Fatalf("WriteHITLAsk: %v", err)
	}
	awaitingFixer.MetadataJSON = &meta
	if err := repos.Loops.Upsert(context.Background(), awaitingFixer); err != nil {
		t.Fatalf("upsert awaiting fixer: %v", err)
	}

	// Park with only one sibling at a time is not required — park finds all siblings.
	// First park against failed-only pair: remove awaiting temporarily by using failed only.
	// Both are siblings of reviewer (same lane/PR).
	_, err = ParkReviewScopeHuman(context.Background(), repos, ParkReviewScopeHumanInput{
		Held: reviewer, Role: "reviewer", Repo: "acme/looper", PRNumber: 42,
		NowISO: nowISO, HITLEnabled: true,
		Question: "Clarify AGENTS.md rule X before unpause; finding: ambiguous scope",
	})
	if err != nil {
		t.Fatalf("ParkReviewScopeHuman: %v", err)
	}

	freshFailed, err := repos.Loops.GetByID(context.Background(), failedFixer.ID)
	if err != nil || freshFailed == nil {
		t.Fatalf("failed sibling get: (%#v, %v)", freshFailed, err)
	}
	if freshFailed.Status != "failed" {
		t.Fatalf("failed sibling status = %s, want failed preserved", freshFailed.Status)
	}
	if !IsReviewFixPairHold(*freshFailed) || !IsReviewScopeHumanHold(*freshFailed) {
		t.Fatalf("failed sibling must be pair/scope hold: %#v meta=%v", freshFailed, derefMeta(freshFailed.MetadataJSON))
	}

	freshAwaiting, err := repos.Loops.GetByID(context.Background(), awaitingFixer.ID)
	if err != nil || freshAwaiting == nil {
		t.Fatalf("awaiting sibling get: (%#v, %v)", freshAwaiting, err)
	}
	if freshAwaiting.Status != "awaiting_human" {
		t.Fatalf("awaiting sibling status = %s, want awaiting_human preserved", freshAwaiting.Status)
	}
	ask, ok := ReadHITLAsk(freshAwaiting.MetadataJSON)
	if !ok || ask.Question != midAsk.Question || IsReviewScopeHumanAsk(ask) {
		t.Fatalf("mid-run ask lost or replaced: ok=%v ask=%#v", ok, ask)
	}
	if !IsReviewFixPairHold(*freshAwaiting) {
		t.Fatalf("awaiting sibling must refuse independent resume via pair hold: %#v", freshAwaiting)
	}
}

func TestContinueReviewScopeHumanPreservesIndependentAndBudgetSiblingLifecycle(t *testing.T) {
	t.Parallel()
	repos, nowISO := newBudgetFixture(t)
	reviewer := seedBudgetLoop(t, repos, nowISO, "loop_scope_cont_rev", "reviewer", "running")

	// Ordinary queued sibling → requeued after scope Continue.
	queuedFixer := seedBudgetLoop(t, repos, nowISO, "loop_scope_cont_queued", "fixer", "queued")

	// Budget-paused sibling must stay paused + budget-held after scope Continue.
	budgetFixer := seedBudgetLoop(t, repos, nowISO, "loop_scope_cont_budget", "fixer", "paused")
	budgetMeta := `{"loop":{"iterationCount":0},"reviewFixBudget":{"siblingOf":"reviewer","pauseReason":"sibling_review_fix_budget"},"pauseReason":"sibling_review_fix_budget"}`
	budgetFixer.MetadataJSON = &budgetMeta
	if err := repos.Loops.Upsert(context.Background(), budgetFixer); err != nil {
		t.Fatalf("upsert budget fixer: %v", err)
	}

	// Independently paused sibling keeps its pauseReason and stays paused.
	indepFixer := seedBudgetLoop(t, repos, nowISO, "loop_scope_cont_indep", "fixer", "paused")
	indepMeta := `{"pauseReason":"fixer_zero_progress"}`
	indepFixer.MetadataJSON = &indepMeta
	if err := repos.Loops.Upsert(context.Background(), indepFixer); err != nil {
		t.Fatalf("upsert indep fixer: %v", err)
	}

	// Failed sibling keeps failed.
	failedFixer := seedBudgetLoop(t, repos, nowISO, "loop_scope_cont_failed", "fixer", "failed")

	// Park against reviewer; all same-lane fixers are siblings.
	parked, err := ParkReviewScopeHuman(context.Background(), repos, ParkReviewScopeHumanInput{
		Held: reviewer, Role: "reviewer", Repo: "acme/looper", PRNumber: 42,
		NowISO: nowISO, HITLEnabled: false,
		Question: "Clarify scope",
	})
	if err != nil {
		t.Fatalf("ParkReviewScopeHuman: %v", err)
	}

	result, err := ApplyReviewScopeHumanAnswer(context.Background(), repos, parked, "Continue", nowISO)
	if err != nil || !result.Applied || result.Action != "continue" {
		t.Fatalf("Continue = (%#v, %v)", result, err)
	}
	if result.Loop.Status != "queued" || IsReviewScopeHumanHold(result.Loop) {
		t.Fatalf("reviewer after continue = %#v, want queued and released", result.Loop)
	}

	freshQueued, err := repos.Loops.GetByID(context.Background(), queuedFixer.ID)
	if err != nil || freshQueued == nil || freshQueued.Status != "queued" || IsReviewScopeHumanHold(*freshQueued) {
		t.Fatalf("queued sibling after continue = (%#v, %v), want queued and released", freshQueued, err)
	}

	freshBudget, err := repos.Loops.GetByID(context.Background(), budgetFixer.ID)
	if err != nil || freshBudget == nil {
		t.Fatalf("budget sibling get: (%#v, %v)", freshBudget, err)
	}
	if freshBudget.Status != "paused" || !IsReviewFixBudgetHold(*freshBudget) {
		t.Fatalf("budget sibling after continue = %#v, want paused + budget hold", freshBudget)
	}
	if IsReviewScopeHumanHold(*freshBudget) {
		t.Fatalf("budget sibling still has scope hold: %#v", freshBudget)
	}
	if reason, _ := stringFromAny(parseMetadataObject(freshBudget.MetadataJSON)["pauseReason"]); reason != ReviewFixBudgetPauseReason {
		t.Fatalf("budget sibling pauseReason = %q, want %q", reason, ReviewFixBudgetPauseReason)
	}

	freshIndep, err := repos.Loops.GetByID(context.Background(), indepFixer.ID)
	if err != nil || freshIndep == nil || freshIndep.Status != "paused" {
		t.Fatalf("indep sibling after continue = (%#v, %v), want paused", freshIndep, err)
	}
	if IsReviewScopeHumanHold(*freshIndep) {
		t.Fatalf("indep sibling still has scope hold: %#v", freshIndep)
	}
	if reason, _ := stringFromAny(parseMetadataObject(freshIndep.MetadataJSON)["pauseReason"]); reason != "fixer_zero_progress" {
		t.Fatalf("indep sibling pauseReason = %q, want fixer_zero_progress", reason)
	}

	freshFailed, err := repos.Loops.GetByID(context.Background(), failedFixer.ID)
	if err != nil || freshFailed == nil || freshFailed.Status != "failed" {
		t.Fatalf("failed sibling after continue = (%#v, %v), want failed", freshFailed, err)
	}
	if IsReviewScopeHumanHold(*freshFailed) {
		t.Fatalf("failed sibling still has scope hold: %#v", freshFailed)
	}
}

func TestContinueReviewScopeHumanKeepsManuallyPausedSiblingPaused(t *testing.T) {
	t.Parallel()
	repos, nowISO := newBudgetFixture(t)
	reviewer := seedBudgetLoop(t, repos, nowISO, "loop_scope_manual_rev", "reviewer", "running")

	// Manual pause: status=paused, no pauseReason (mutateLoopStatus shape).
	manualFixer := seedBudgetLoop(t, repos, nowISO, "loop_scope_manual_fixer", "fixer", "paused")
	manualMeta := `{"loop":{"iterationCount":0}}`
	manualFixer.MetadataJSON = &manualMeta
	if err := repos.Loops.Upsert(context.Background(), manualFixer); err != nil {
		t.Fatalf("upsert manual fixer: %v", err)
	}

	// Budget-paused sibling must still stay budget-held after scope Continue.
	budgetFixer := seedBudgetLoop(t, repos, nowISO, "loop_scope_manual_budget", "fixer", "paused")
	budgetMeta := `{"loop":{"iterationCount":0},"reviewFixBudget":{"siblingOf":"reviewer","pauseReason":"sibling_review_fix_budget"},"pauseReason":"sibling_review_fix_budget"}`
	budgetFixer.MetadataJSON = &budgetMeta
	if err := repos.Loops.Upsert(context.Background(), budgetFixer); err != nil {
		t.Fatalf("upsert budget fixer: %v", err)
	}

	parked, err := ParkReviewScopeHuman(context.Background(), repos, ParkReviewScopeHumanInput{
		Held: reviewer, Role: "reviewer", Repo: "acme/looper", PRNumber: 42,
		NowISO: nowISO, HITLEnabled: false,
		Question: "Clarify scope",
	})
	if err != nil {
		t.Fatalf("ParkReviewScopeHuman: %v", err)
	}

	// Manual sibling must carry scope overlay without a top-level scope stamp.
	freshManual, err := repos.Loops.GetByID(context.Background(), manualFixer.ID)
	if err != nil || freshManual == nil {
		t.Fatalf("manual sibling get: (%#v, %v)", freshManual, err)
	}
	if freshManual.Status != "paused" {
		t.Fatalf("manual sibling status after park = %s, want paused", freshManual.Status)
	}
	if reason, _ := stringFromAny(parseMetadataObject(freshManual.MetadataJSON)["pauseReason"]); reason != "" {
		t.Fatalf("manual sibling top-level pauseReason = %q, want empty (overlay only)", reason)
	}
	if !IsReviewScopeHumanHold(*freshManual) {
		t.Fatalf("manual sibling must be scope-held via overlay: %#v", freshManual)
	}

	result, err := ApplyReviewScopeHumanAnswer(context.Background(), repos, parked, "Continue", nowISO)
	if err != nil || !result.Applied || result.Action != "continue" {
		t.Fatalf("Continue = (%#v, %v)", result, err)
	}

	freshManual, err = repos.Loops.GetByID(context.Background(), manualFixer.ID)
	if err != nil || freshManual == nil {
		t.Fatalf("manual sibling after continue get: (%#v, %v)", freshManual, err)
	}
	if freshManual.Status != "paused" {
		t.Fatalf("manual sibling after continue = %#v, want paused (not queued)", freshManual)
	}
	if IsReviewScopeHumanHold(*freshManual) {
		t.Fatalf("manual sibling still has scope hold after continue: %#v", freshManual)
	}

	freshBudget, err := repos.Loops.GetByID(context.Background(), budgetFixer.ID)
	if err != nil || freshBudget == nil {
		t.Fatalf("budget sibling get: (%#v, %v)", freshBudget, err)
	}
	if freshBudget.Status != "paused" || !IsReviewFixBudgetHold(*freshBudget) {
		t.Fatalf("budget sibling after continue = %#v, want paused + budget hold", freshBudget)
	}
}

func TestApplyReviewScopeHumanStopUsesScopeTerminationReason(t *testing.T) {
	t.Parallel()
	repos, nowISO := newBudgetFixture(t)
	reviewer := seedBudgetLoop(t, repos, nowISO, "loop_scope_stop_reviewer", "reviewer", "running")
	fixer := seedBudgetLoop(t, repos, nowISO, "loop_scope_stop_fixer", "fixer", "queued")
	_ = fixer

	parked, err := ParkReviewScopeHuman(context.Background(), repos, ParkReviewScopeHumanInput{
		Held: reviewer, Role: "reviewer", Repo: "acme/looper", PRNumber: 42,
		NowISO: nowISO, HITLEnabled: true,
		Question: "Clarify scope",
	})
	if err != nil {
		t.Fatalf("park: %v", err)
	}
	result, err := ApplyReviewScopeHumanAnswer(context.Background(), repos, parked, "Stop", nowISO)
	if err != nil || !result.Applied || result.Action != "stop" {
		t.Fatalf("Stop = (%#v, %v)", result, err)
	}
	if result.Loop.Status != "terminated" {
		t.Fatalf("status = %s, want terminated", result.Loop.Status)
	}
	meta := parseMetadataObject(result.Loop.MetadataJSON)
	loopMeta, _ := meta["loop"].(map[string]any)
	if loopMeta == nil {
		t.Fatalf("missing loop metadata: %#v", meta)
	}
	if got, _ := loopMeta["terminationReason"].(string); got != ReviewScopeHumanRequiredReason {
		t.Fatalf("terminationReason = %q, want %q (not budget exhausted)", got, ReviewScopeHumanRequiredReason)
	}
	if got, _ := loopMeta["terminationReason"].(string); got == ReviewFixBudgetTerminationReason {
		t.Fatal("scope Stop must not write review_fix_budget_exhausted")
	}
}

func TestBudgetContinueRestoresScopePausedSiblingInsteadOfQueueing(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		budgetHITL bool
		wantBudget string
	}{
		{name: "awaiting_human_budget_ask", budgetHITL: true, wantBudget: "awaiting_human"},
		{name: "paused_nohitl_budget", budgetHITL: false, wantBudget: "paused"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			repos, nowISO := newBudgetFixture(t)
			suffix := tc.name
			reviewer := seedBudgetLoop(t, repos, nowISO, "loop_scope_budget_ord_rev_"+suffix, "reviewer", "running")
			fixer := seedBudgetLoop(t, repos, nowISO, "loop_scope_budget_ord_fix_"+suffix, "fixer", "queued")
			reviewerQueueID := "queue_scope_budget_ord_rev_" + suffix
			fixerQueueID := "queue_scope_budget_ord_fix_" + suffix
			seedBudgetQueue(t, repos, nowISO, reviewerQueueID, reviewer.ID, "reviewer", storage.QueuePriorityReviewer)
			seedBudgetQueue(t, repos, nowISO, fixerQueueID, fixer.ID, "fixer", storage.QueuePriorityFixer)

			parkedFixer, err := ParkReviewFixBudget(context.Background(), repos, ParkReviewFixBudgetInput{
				Exhausted: fixer, Role: "fixer", Repo: "acme/looper", PRNumber: 42,
				Count: 8, Cap: 8, NowISO: nowISO, HITLEnabled: tc.budgetHITL,
			})
			if err != nil {
				t.Fatalf("ParkReviewFixBudget: %v", err)
			}
			if parkedFixer.Status != tc.wantBudget || !IsReviewFixBudgetHold(parkedFixer) {
				t.Fatalf("budget fixer = %#v, want status %s + budget hold", parkedFixer, tc.wantBudget)
			}

			freshReviewer, err := repos.Loops.GetByID(context.Background(), reviewer.ID)
			if err != nil || freshReviewer == nil {
				t.Fatalf("reviewer after budget park: (%#v, %v)", freshReviewer, err)
			}
			parkedReviewer, err := ParkReviewScopeHuman(context.Background(), repos, ParkReviewScopeHumanInput{
				Held: *freshReviewer, Role: "reviewer", Repo: "acme/looper", PRNumber: 42,
				NowISO: nowISO, HITLEnabled: true,
				Question: "Clarify AGENTS.md rule X before unpause; finding: ambiguous scope",
			})
			if err != nil {
				t.Fatalf("ParkReviewScopeHuman: %v", err)
			}
			if !IsReviewScopeHumanHold(parkedReviewer) || IsReviewFixBudgetHold(parkedReviewer) {
				t.Fatalf("reviewer after scope park = %#v, want scope-only hold", parkedReviewer)
			}

			overlaid, err := repos.Loops.GetByID(context.Background(), fixer.ID)
			if err != nil || overlaid == nil {
				t.Fatalf("fixer after overlay: (%#v, %v)", overlaid, err)
			}
			if overlaid.Status != tc.wantBudget || !IsReviewFixBudgetHold(*overlaid) || !IsReviewScopeHumanHold(*overlaid) {
				t.Fatalf("overlaid fixer = %#v meta=%s, want preserved budget status + scope overlay", overlaid, derefMeta(overlaid.MetadataJSON))
			}
			if ask, ok := ReadHITLAsk(overlaid.MetadataJSON); tc.budgetHITL && (!ok || !IsReviewFixBudgetAsk(ask)) {
				t.Fatalf("overlaid fixer ask = (%#v, %v), want preserved budget ask", ask, ok)
			}

			result, err := ApplyReviewFixBudgetAnswer(context.Background(), repos, *overlaid, "Continue", nowISO, testBudgetCaps(8, 8))
			if err != nil || !result.Applied || result.Action != "continue" {
				t.Fatalf("budget Continue = (%#v, %v)", result, err)
			}

			freshFixer, err := repos.Loops.GetByID(context.Background(), fixer.ID)
			if err != nil || freshFixer == nil {
				t.Fatalf("fixer after budget Continue: (%#v, %v)", freshFixer, err)
			}
			if freshFixer.Status == "queued" {
				t.Fatalf("fixer queued after budget Continue while scope overlay remains: %#v", freshFixer)
			}
			if freshFixer.Status != "paused" || IsReviewFixBudgetHold(*freshFixer) || !IsReviewScopeHumanHold(*freshFixer) {
				t.Fatalf("fixer after budget Continue = %#v meta=%s, want paused scope sibling", freshFixer, derefMeta(freshFixer.MetadataJSON))
			}
			if !IsSiblingReviewScopeHumanPause(freshFixer.MetadataJSON) {
				t.Fatalf("fixer after budget Continue missing sibling scope pause: meta=%s", derefMeta(freshFixer.MetadataJSON))
			}
			if reason, _ := stringFromAny(parseMetadataObject(freshFixer.MetadataJSON)["pauseReason"]); reason != ReviewScopeHumanSiblingPauseReason {
				t.Fatalf("fixer top-level pauseReason = %q, want %q", reason, ReviewScopeHumanSiblingPauseReason)
			}
			fixerQueue, err := repos.Queue.GetByID(context.Background(), fixerQueueID)
			if err != nil || fixerQueue == nil || fixerQueue.Status != "cancelled" {
				t.Fatalf("fixer queue after budget Continue = (%#v, %v), want cancelled", fixerQueue, err)
			}

			freshReviewer, err = repos.Loops.GetByID(context.Background(), reviewer.ID)
			if err != nil || freshReviewer == nil || !IsReviewScopeHumanHold(*freshReviewer) {
				t.Fatalf("reviewer after budget Continue = (%#v, %v), want still scope-held", freshReviewer, err)
			}
			if reason, _ := stringFromAny(parseMetadataObject(freshReviewer.MetadataJSON)["pauseReason"]); reason == ReviewFixBudgetPauseReason {
				t.Fatalf("reviewer still has leftover budget pauseReason after budget Continue: meta=%s", derefMeta(freshReviewer.MetadataJSON))
			}

			scopeResult, err := ApplyReviewScopeHumanAnswer(context.Background(), repos, *freshReviewer, "Continue", nowISO)
			if err != nil || !scopeResult.Applied || scopeResult.Action != "continue" {
				t.Fatalf("scope Continue = (%#v, %v)", scopeResult, err)
			}
			freshFixer, err = repos.Loops.GetByID(context.Background(), fixer.ID)
			if err != nil || freshFixer == nil || freshFixer.Status != "queued" || IsReviewScopeHumanHold(*freshFixer) {
				t.Fatalf("fixer after scope Continue = (%#v, %v), want queued and released", freshFixer, err)
			}
			if scopeResult.Loop.Status != "queued" || IsReviewScopeHumanHold(scopeResult.Loop) {
				t.Fatalf("reviewer after scope Continue = %#v, want queued and released", scopeResult.Loop)
			}
		})
	}
}

func derefMeta(m *string) string {
	if m == nil {
		return ""
	}
	return *m
}

func TestParkReviewScopeHumanFailsClosedWhenRefreshErrors(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "loops.sqlite")
	coordinator, err := storage.OpenSQLiteCoordinator(context.Background(), dbPath, storage.SQLiteCoordinatorOptions{BackupDir: t.TempDir()})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	if _, err := coordinator.MigrationRunner().RunPending(context.Background(), storage.RunPendingOptions{}); err != nil {
		t.Fatalf("RunPending() error = %v", err)
	}
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 17, 12, 34, 56, 0, time.UTC)
	seedProject(t, repos, now)
	nowISO := now.UTC().Format("2006-01-02T15:04:05.000Z")
	reviewer := seedBudgetLoop(t, repos, nowISO, "loop_scope_refresh_err_reviewer", "reviewer", "running")
	fixer := seedBudgetLoop(t, repos, nowISO, "loop_scope_refresh_err_fixer", "fixer", "queued")
	if err := coordinator.Close(); err != nil {
		t.Fatalf("close coordinator: %v", err)
	}

	if _, err := ParkReviewScopeHuman(context.Background(), repos, ParkReviewScopeHumanInput{
		Held: reviewer, Role: "reviewer", Repo: "acme/looper", PRNumber: 42,
		NowISO: nowISO, HITLEnabled: true,
		Question: "Clarify AGENTS.md before unpause",
	}); err == nil {
		t.Fatal("ParkReviewScopeHuman error = nil, want refresh error")
	}

	reopened, err := storage.OpenSQLiteCoordinator(context.Background(), dbPath, storage.SQLiteCoordinatorOptions{BackupDir: t.TempDir()})
	if err != nil {
		t.Fatalf("reopen coordinator: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if _, err := reopened.MigrationRunner().RunPending(context.Background(), storage.RunPendingOptions{}); err != nil {
		t.Fatalf("reopen RunPending: %v", err)
	}
	freshRepos := storage.NewRepositories(reopened.DB())
	freshReviewer, err := freshRepos.Loops.GetByID(context.Background(), reviewer.ID)
	if err != nil || freshReviewer == nil || freshReviewer.Status != "running" || IsReviewScopeHumanHold(*freshReviewer) {
		t.Fatalf("reviewer after failed park = (%#v, %v), want still running and unparked", freshReviewer, err)
	}
	freshFixer, err := freshRepos.Loops.GetByID(context.Background(), fixer.ID)
	if err != nil || freshFixer == nil || freshFixer.Status != "queued" || IsReviewScopeHumanHold(*freshFixer) {
		t.Fatalf("fixer after failed park = (%#v, %v), want still queued and unparked", freshFixer, err)
	}
}
