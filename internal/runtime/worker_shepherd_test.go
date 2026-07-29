package runtime

import (
	"testing"
	"time"

	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/loops"
)

func TestShepherdPlaneStateRetryGate(t *testing.T) {
	now := time.Date(2026, 7, 17, 3, 0, 0, 0, time.UTC)

	if !shepherdPlaneStateSyncDue(loops.Shepherd{}, "In Review", now) {
		t.Fatal("never-attempted state should sync immediately")
	}
	if shepherdPlaneStateSyncDue(loops.Shepherd{PlaneState: "in review"}, "In Review", now) {
		t.Fatal("already-synced state should not be written again")
	}
	recent := loops.Shepherd{PlaneStateAttemptAt: now.Add(-5 * time.Minute).Format(time.RFC3339Nano)}
	if shepherdPlaneStateSyncDue(recent, "In Review", now) {
		t.Fatal("failed attempt inside the retry window should be rate-limited")
	}
	stale := loops.Shepherd{PlaneStateAttemptAt: now.Add(-11 * time.Minute).Format(time.RFC3339Nano)}
	if !shepherdPlaneStateSyncDue(stale, "In Review", now) {
		t.Fatal("failed attempt outside the retry window should retry")
	}
}

func check(status, conclusion string) map[string]any {
	return map[string]any{"status": status, "conclusion": conclusion}
}

func TestShepherdTerminalOutcome(t *testing.T) {
	cases := []struct {
		pr   githubinfra.PullRequestDetail
		want string
	}{
		{githubinfra.PullRequestDetail{State: "MERGED"}, "merged"},
		{githubinfra.PullRequestDetail{State: "OPEN", MergedAt: "2026-07-08T00:00:00Z"}, "merged"},
		{githubinfra.PullRequestDetail{State: "CLOSED"}, "abandoned"},
		{githubinfra.PullRequestDetail{State: "OPEN"}, ""},
	}
	for _, c := range cases {
		if got := shepherdTerminalOutcome(c.pr); got != c.want {
			t.Fatalf("shepherdTerminalOutcome(%+v) = %q, want %q", c.pr, got, c.want)
		}
	}
}

func TestShepherdCIPhase(t *testing.T) {
	if got := shepherdCIPhase(githubinfra.PullRequestDetail{Checks: []map[string]any{check("IN_PROGRESS", "")}}); got != "pending" {
		t.Fatalf("pending CI = %q", got)
	}
	if got := shepherdCIPhase(githubinfra.PullRequestDetail{Checks: []map[string]any{check("COMPLETED", "SUCCESS"), check("COMPLETED", "FAILURE")}}); got != "failed" {
		t.Fatalf("failed CI = %q", got)
	}
	if got := shepherdCIPhase(githubinfra.PullRequestDetail{Checks: []map[string]any{check("COMPLETED", "SUCCESS"), check("COMPLETED", "SKIPPED")}}); got != "passed" {
		t.Fatalf("passed CI = %q", got)
	}
}

func TestShepherdActionableAndPhase(t *testing.T) {
	green := []map[string]any{check("COMPLETED", "SUCCESS")}
	// changes requested AT THE CURRENT HEAD → fix
	cr := githubinfra.PullRequestDetail{State: "OPEN", ReviewDecision: "CHANGES_REQUESTED", HeadSHA: "h1", Checks: green,
		Reviews: []map[string]any{{"state": "CHANGES_REQUESTED", "commit": map[string]any{"oid": "h1"}}}}
	if !shepherdActionable(cr) || shepherdPhaseFor(cr) != "fixing" {
		t.Fatalf("CHANGES_REQUESTED on current head should be actionable/fixing")
	}
	// changes requested on an OLD head (we already pushed a fix, awaiting re-review) → NOT actionable
	stale := githubinfra.PullRequestDetail{State: "OPEN", ReviewDecision: "CHANGES_REQUESTED", HeadSHA: "h2", Checks: green,
		Reviews: []map[string]any{{"state": "CHANGES_REQUESTED", "commit": map[string]any{"oid": "h1"}}, {"state": "COMMENTED", "commit": map[string]any{"oid": "h2"}}}}
	if shepherdActionable(stale) {
		t.Fatalf("CHANGES_REQUESTED on an OLD head must NOT be actionable — we already responded, awaiting re-review (stops the spin)")
	}
	// conflict → fix
	conflict := githubinfra.PullRequestDetail{State: "OPEN", ReviewDecision: "APPROVED", HasConflicts: true, Checks: green}
	if !shepherdActionable(conflict) || shepherdPhaseFor(conflict) != "fixing" {
		t.Fatalf("conflict should be actionable/fixing")
	}
	// failing CI → fix
	failing := githubinfra.PullRequestDetail{State: "OPEN", ReviewDecision: "APPROVED", Checks: []map[string]any{check("COMPLETED", "FAILURE")}}
	if !shepherdActionable(failing) {
		t.Fatalf("failing CI should be actionable")
	}
	// approved AT THE CURRENT HEAD + green + clean → NOT actionable (bot waits for
	// a human to merge), phase awaiting_merge
	ready := githubinfra.PullRequestDetail{State: "OPEN", ReviewDecision: "APPROVED", HeadSHA: "hr", Checks: green,
		Reviews: []map[string]any{{"state": "APPROVED", "commit": map[string]any{"oid": "hr"}}}}
	if shepherdActionable(ready) {
		t.Fatalf("approved+green+clean must NOT be actionable — the bot never merges, it waits for a human")
	}
	if shepherdPhaseFor(ready) != "awaiting_merge" {
		t.Fatalf("approved+green+clean phase = %q, want awaiting_merge", shepherdPhaseFor(ready))
	}
}

// The QA validation gate: approved + green + clean holds at awaiting_validation
// (→ @QA) while `needs-validation` is present and `validated` is not; once validated
// (or the PR never needed it) it's awaiting_merge (→ @owner). The bot never merges.
func TestShepherdValidationGate(t *testing.T) {
	green := []map[string]any{check("COMPLETED", "SUCCESS")}
	base := githubinfra.PullRequestDetail{
		State: "OPEN", ReviewDecision: "APPROVED", HeadSHA: "h", Checks: green,
		Reviews: []map[string]any{{"state": "APPROVED", "commit": map[string]any{"oid": "h"}}},
	}
	// needs-validation, not validated → awaiting_validation, NOT actionable (bot waits for QA)
	needsVal := base
	needsVal.Labels = []string{"size/M", "needs-validation"}
	if got := shepherdPhaseFor(needsVal); got != "awaiting_validation" {
		t.Fatalf("needs-validation (unvalidated) phase = %q, want awaiting_validation", got)
	}
	if shepherdActionable(needsVal) {
		t.Fatal("awaiting_validation must NOT be actionable — the bot waits for QA, never merges")
	}
	// + validated → awaiting_merge
	validated := base
	validated.Labels = []string{"needs-validation", "validated"}
	if got := shepherdPhaseFor(validated); got != "awaiting_merge" {
		t.Fatalf("needs-validation + validated phase = %q, want awaiting_merge", got)
	}
	// no needs-validation label at all → straight to awaiting_merge (no QA gate)
	noVal := base
	noVal.Labels = []string{"size/M"}
	if got := shepherdPhaseFor(noVal); got != "awaiting_merge" {
		t.Fatalf("no needs-validation phase = %q, want awaiting_merge", got)
	}
	// the QA-gate labels are in the wake signal, so a `validated` landing at the same
	// head/decision still wakes the reconcile to re-evaluate + re-report.
	if foldShepherdSignal(needsVal) == foldShepherdSignal(validated) {
		t.Fatal("adding `validated` must change the folded signal (else QA sign-off is missed)")
	}
}

func TestSyncShepherdPhaseRepairsFixingDriftWithoutSignalChange(t *testing.T) {
	green := []map[string]any{check("COMPLETED", "SUCCESS")}
	pr := githubinfra.PullRequestDetail{
		State: "OPEN", ReviewDecision: "APPROVED", HeadSHA: "ready-head", Checks: green,
		Reviews: []map[string]any{{"state": "APPROVED", "commit": map[string]any{"oid": "ready-head"}}},
		Labels:  []string{"needs-validation"},
	}
	marker := loops.Shepherd{Active: true, Phase: "fixing", LastSignal: foldShepherdSignal(pr)}

	if !syncShepherdPhase(&marker, pr) {
		t.Fatal("syncShepherdPhase() = false, want phase drift repaired")
	}
	if marker.Phase != "awaiting_validation" {
		t.Fatalf("phase = %q, want awaiting_validation", marker.Phase)
	}
	if marker.HeadSHA != "ready-head" {
		t.Fatalf("head = %q, want ready-head", marker.HeadSHA)
	}
	if syncShepherdPhase(&marker, pr) {
		t.Fatal("second syncShepherdPhase() = true, want no-op once aligned")
	}
}

// The wake key must NOT include updatedAt (or any field the agent's own
// comments/pushes bump gratuitously) beyond headSHA, else a fix would
// self-retrigger forever. Two PRs differing only in updatedAt fold to the same
// signal.
func TestFoldShepherdSignalIgnoresUpdatedAt(t *testing.T) {
	green := []map[string]any{check("COMPLETED", "SUCCESS")}
	a := githubinfra.PullRequestDetail{State: "OPEN", ReviewDecision: "APPROVED", HeadSHA: "sha1", Checks: green, UpdatedAt: "2026-07-08T00:00:00Z"}
	b := githubinfra.PullRequestDetail{State: "OPEN", ReviewDecision: "APPROVED", HeadSHA: "sha1", Checks: green, UpdatedAt: "2026-07-08T09:99:99Z"}
	if foldShepherdSignal(a) != foldShepherdSignal(b) {
		t.Fatalf("signal must ignore updatedAt: %q vs %q", foldShepherdSignal(a), foldShepherdSignal(b))
	}
	// a new head IS a real change
	c := githubinfra.PullRequestDetail{State: "OPEN", ReviewDecision: "APPROVED", HeadSHA: "sha2", Checks: green}
	if foldShepherdSignal(a) == foldShepherdSignal(c) {
		t.Fatal("a new head must change the signal")
	}
}

// A new review round (reviewer submits again) must change the wake signal even at
// the same head with the same aggregate decision — else the shepherd never wakes
// to address the re-review (the live-e2e bug). The agent's own thread replies are
// NOT top-level reviews, so they don't move latestReviewMarker.
func TestSignalCatchesNewReviewRound(t *testing.T) {
	base := githubinfra.PullRequestDetail{State: "OPEN", ReviewDecision: "CHANGES_REQUESTED", HeadSHA: "sha1",
		Checks:  []map[string]any{check("COMPLETED", "SUCCESS")},
		Reviews: []map[string]any{{"state": "CHANGES_REQUESTED", "submittedAt": "2026-07-08T00:00:00Z"}},
	}
	round2 := base
	round2.Reviews = []map[string]any{
		{"state": "CHANGES_REQUESTED", "submittedAt": "2026-07-08T00:00:00Z", "commit": map[string]any{"oid": "sha1"}},
		{"state": "CHANGES_REQUESTED", "submittedAt": "2026-07-08T09:00:00Z", "commit": map[string]any{"oid": "sha1"}}, // nettee re-reviewed at the SAME head
	}
	if foldShepherdSignal(base) == foldShepherdSignal(round2) {
		t.Fatal("a new review round at the same head/decision must change the signal (else re-review is missed)")
	}
	if !shepherdActionable(round2) {
		t.Fatal("CHANGES_REQUESTED at the current head must be actionable")
	}
}

// A stale CHANGES_REQUESTED on an OLD head must NOT block awaiting_merge once the
// CURRENT head is approved + green (the live-e2e case: two colleagues approved
// b8653748 while nettee's old CHANGES_REQUESTED kept the aggregate at
// CHANGES_REQUESTED). The bot then shows 待合并 and waits for a human to merge.
func TestAwaitingMergeIgnoresStaleAggregateDecision(t *testing.T) {
	green := []map[string]any{check("COMPLETED", "SUCCESS")}
	pr := githubinfra.PullRequestDetail{
		State: "OPEN", ReviewDecision: "CHANGES_REQUESTED", HeadSHA: "cur", Checks: green,
		Reviews: []map[string]any{
			{"state": "CHANGES_REQUESTED", "commit": map[string]any{"oid": "old"}}, // stale, old head
			{"state": "APPROVED", "commit": map[string]any{"oid": "cur"}},          // fresh approval of current head
			{"state": "APPROVED", "commit": map[string]any{"oid": "cur"}},
		},
	}
	if shepherdActionable(pr) {
		t.Fatal("approved current head with no change-request-here must NOT be actionable")
	}
	if got := shepherdPhaseFor(pr); got != "awaiting_merge" {
		t.Fatalf("phase = %q, want awaiting_merge (stale aggregate CHANGES_REQUESTED must not wedge it)", got)
	}
}
