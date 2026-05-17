package triage

import (
	"context"
	"testing"
	"time"
)

func TestDecideFailsClosedOnMalformedOutput(t *testing.T) {
	t.Parallel()
	decision := Decide(context.Background(), fixtureLLM{raw: `{"disposition":"valid","comment":"bad","labels":{"dispatch":["dispatch/plan","dispatch/implement"]}}`}, Input{Issue: Issue{Title: "Bad", CreatedAt: time.Now().UTC().Format(time.RFC3339)}, Config: testConfig(), Now: time.Now().UTC()})
	if !decision.NoOp {
		t.Fatalf("Decide() = %#v, want no-op on strict parse failure", decision)
	}

	decision = Decide(context.Background(), fixtureLLM{raw: `{"disposition":"valid","comment":"bad","labels":{"kind":["kind/unknown"],"area":["area/coordinator"],"complexity":["complexity/m"],"dispatch":["dispatch/plan"]}}`}, Input{Issue: Issue{Title: "Bad", CreatedAt: time.Now().UTC().Format(time.RFC3339)}, Config: testConfig(), Now: time.Now().UTC()})
	if !decision.NoOp {
		t.Fatal("Decide() should fail closed on unknown label kind")
	}
}

func TestShouldTriageHonorsMaxIssueAgeDays(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.May, 14, 12, 0, 0, 0, time.UTC)
	if ShouldTriage(Issue{CreatedAt: now.Add(-8 * 24 * time.Hour).Format(time.RFC3339)}, testConfig(), now) {
		t.Fatal("ShouldTriage() = true, want false for issue older than maxIssueAgeDays")
	}
	if !ShouldTriage(Issue{CreatedAt: now.Add(-6 * 24 * time.Hour).Format(time.RFC3339)}, testConfig(), now) {
		t.Fatal("ShouldTriage() = false, want true for fresh issue")
	}
}

func TestLimitPerTick(t *testing.T) {
	t.Parallel()
	limited := LimitPerTick([]int{1, 2, 3, 4, 5, 6}, 5)
	if len(limited) != 5 {
		t.Fatalf("len(LimitPerTick()) = %d, want 5", len(limited))
	}
}

func TestShouldReTriageOnAuthorReply(t *testing.T) {
	t.Parallel()
	issue := Issue{
		Author:   "octo",
		Labels:   []string{"needs-info", "triaged"},
		Comments: []Comment{{Author: "octo", CreatedAt: "2026-05-14T12:05:00Z", Body: "Added details"}},
		Timeline: []TimelineEvent{{Event: "labeled", Label: "needs-info", CreatedAt: "2026-05-14T12:00:00Z"}},
	}
	if !ShouldReTriage(issue, testConfig(), time.Now().UTC()) {
		t.Fatal("ShouldReTriage() = false, want true after author clarification")
	}
	decision := ReTriageDecision(testConfig())
	if len(decision.RemoveLabels) != 2 || decision.RemoveLabels[0] != "needs-info" || decision.RemoveLabels[1] != "triaged" {
		t.Fatalf("RemoveLabels = %v, want [needs-info triaged]", decision.RemoveLabels)
	}
}
