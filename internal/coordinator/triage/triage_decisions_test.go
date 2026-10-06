package triage

import (
	"context"
	"strings"
	"testing"
	"time"
)

type fixtureLLM struct{ raw string }

func (f fixtureLLM) Complete(context.Context, Request) (string, error) { return f.raw, nil }

func TestDecideValidDisposition(t *testing.T) {
	t.Parallel()
	decision := Decide(context.Background(), fixtureLLM{raw: `{"disposition":"valid","comment":"Looks actionable.","labels":{"kind":["kind/bug"],"area":["area/coordinator"],"complexity":["complexity/m"],"dispatch":["dispatch/plan"]}}`}, Input{Issue: Issue{Title: "Coordinator bug", CreatedAt: time.Now().UTC().Format(time.RFC3339)}, Config: testConfig(), Now: time.Now().UTC()})
	if decision.NoOp {
		t.Fatal("Decide() returned no-op for valid output")
	}
	if got, want := decision.ApplyLabels, []string{"kind/bug", "area/coordinator", "complexity/m", "dispatch/plan", "triaged"}; len(got) != len(want) {
		t.Fatalf("ApplyLabels len = %d, want %d", len(got), len(want))
	}
	if !decision.MarkTriaged {
		t.Fatal("MarkTriaged = false, want true")
	}
}

func TestDecideValidDispositionFromFencedJSON(t *testing.T) {
	t.Parallel()
	raw := "```json\n" + `{"disposition":"valid","comment":"Looks actionable.","labels":{"kind":["kind/bug"],"area":["area/coordinator"],"complexity":["complexity/m"],"dispatch":["dispatch/plan"]}}` + "\n```"
	decision := Decide(context.Background(), fixtureLLM{raw: raw}, Input{Issue: Issue{Title: "Coordinator bug", CreatedAt: time.Now().UTC().Format(time.RFC3339)}, Config: testConfig(), Now: time.Now().UTC()})
	if decision.NoOp {
		t.Fatal("Decide() returned no-op for fenced JSON output")
	}
	if got, want := decision.ApplyLabels, []string{"kind/bug", "area/coordinator", "complexity/m", "dispatch/plan", "triaged"}; len(got) != len(want) {
		t.Fatalf("ApplyLabels len = %d, want %d", len(got), len(want))
	}
}

func TestDecideValidDispositionFromTextWrappedJSON(t *testing.T) {
	t.Parallel()
	raw := "Here is the triage decision:\n" + `{"disposition":"valid","comment":"Looks actionable.","labels":{"kind":["kind/docs"],"area":["area/docs"],"complexity":["complexity/s"],"dispatch":["dispatch/plan"]}}` + "\nThanks."
	decision := Decide(context.Background(), fixtureLLM{raw: raw}, Input{Issue: Issue{Title: "Docs gap", CreatedAt: time.Now().UTC().Format(time.RFC3339)}, Config: testConfig(), Now: time.Now().UTC()})
	if decision.NoOp {
		t.Fatal("Decide() returned no-op for text-wrapped JSON output")
	}
	if got, want := decision.ApplyLabels, []string{"kind/docs", "area/docs", "complexity/s", "dispatch/plan", "triaged"}; len(got) != len(want) {
		t.Fatalf("ApplyLabels len = %d, want %d", len(got), len(want))
	}
}

func TestDecideValidDispositionDefaultsMissingArea(t *testing.T) {
	t.Parallel()
	decision := Decide(context.Background(), fixtureLLM{raw: `{"disposition":"valid","comment":"Needs planning.","labels":{"kind":["kind/feature"],"area":[],"complexity":["complexity/l"],"dispatch":["dispatch/plan"]}}`}, Input{Issue: Issue{Title: "Spaced repetition", CreatedAt: time.Now().UTC().Format(time.RFC3339)}, Config: testConfig(), Now: time.Now().UTC()})
	if decision.NoOp {
		t.Fatal("Decide() returned no-op for valid output with missing area")
	}
	if got, want := decision.ApplyLabels, []string{"kind/feature", "area/planner", "complexity/l", "dispatch/plan", "triaged"}; len(got) != len(want) {
		t.Fatalf("ApplyLabels len = %d, want %d", len(got), len(want))
	} else {
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("ApplyLabels = %v, want %v", got, want)
			}
		}
	}
}

func TestDecideValidDispositionDefaultsMissingDispatch(t *testing.T) {
	t.Parallel()
	decision := Decide(context.Background(), fixtureLLM{raw: `{"disposition":"valid","comment":"Docs task.","labels":{"kind":["kind/docs"],"area":["area/docs"],"complexity":["complexity/s"],"dispatch":[]}}`}, Input{Issue: Issue{Title: "Gemini docs", CreatedAt: time.Now().UTC().Format(time.RFC3339)}, Config: testConfig(), Now: time.Now().UTC()})
	if decision.NoOp {
		t.Fatal("Decide() returned no-op for valid output with missing dispatch")
	}
	if got, want := decision.ApplyLabels, []string{"kind/docs", "area/docs", "complexity/s", "dispatch/plan", "triaged"}; len(got) != len(want) {
		t.Fatalf("ApplyLabels len = %d, want %d", len(got), len(want))
	} else {
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("ApplyLabels = %v, want %v", got, want)
			}
		}
	}
}

func TestDecideValidDispositionSelectsFirstAllowedExtraArea(t *testing.T) {
	t.Parallel()
	decision := Decide(context.Background(), fixtureLLM{raw: `{"disposition":"valid","comment":"Needs planning.","labels":{"kind":["kind/feature"],"area":["area/api","area/planner"],"complexity":["complexity/l"],"dispatch":["dispatch/plan"]}}`}, Input{Issue: Issue{Title: "Spaced repetition", CreatedAt: time.Now().UTC().Format(time.RFC3339)}, Config: testConfig(), Now: time.Now().UTC()})
	if decision.NoOp {
		t.Fatal("Decide() returned no-op for valid output with multiple area labels")
	}
	if got, want := decision.ApplyLabels, []string{"kind/feature", "area/api", "complexity/l", "dispatch/plan", "triaged"}; len(got) != len(want) {
		t.Fatalf("ApplyLabels len = %d, want %d", len(got), len(want))
	} else {
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("ApplyLabels = %v, want %v", got, want)
			}
		}
	}
}

func TestDecideValidDispositionSelectsFirstAllowedExtraDispatch(t *testing.T) {
	t.Parallel()
	decision := Decide(context.Background(), fixtureLLM{raw: `{"disposition":"valid","comment":"Parser bug.","labels":{"kind":["kind/bug"],"area":["area/runtime"],"complexity":["complexity/s"],"dispatch":["dispatch/plan","dispatch/implement"]}}`}, Input{Issue: Issue{Title: "Parser bug", CreatedAt: time.Now().UTC().Format(time.RFC3339)}, Config: testConfig(), Now: time.Now().UTC()})
	if decision.NoOp {
		t.Fatal("Decide() returned no-op for valid output with multiple dispatch labels")
	}
	if got, want := decision.ApplyLabels, []string{"kind/bug", "area/runtime", "complexity/s", "dispatch/plan", "triaged"}; len(got) != len(want) {
		t.Fatalf("ApplyLabels len = %d, want %d", len(got), len(want))
	} else {
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("ApplyLabels = %v, want %v", got, want)
			}
		}
	}
}

func TestDecideOutOfScopeDisposition(t *testing.T) {
	t.Parallel()
	decision := Decide(context.Background(), fixtureLLM{raw: `{"disposition":"out-of-scope","comment":"Not aligned.","labels":{}}`}, Input{Issue: Issue{Title: "Unfit", CreatedAt: time.Now().UTC().Format(time.RFC3339)}, Config: testConfig(), Now: time.Now().UTC()})
	if decision.NoOp {
		t.Fatal("Decide() returned no-op for out-of-scope output")
	}
	if len(decision.ApplyLabels) != 2 || decision.ApplyLabels[0] != "wontfix" || decision.ApplyLabels[1] != "triaged" {
		t.Fatalf("ApplyLabels = %v, want [wontfix triaged]", decision.ApplyLabels)
	}
}

func TestDecideOutOfScopeIgnoresExtraLabels(t *testing.T) {
	t.Parallel()
	decision := Decide(context.Background(), fixtureLLM{raw: `{"disposition":"out-of-scope","comment":"Not aligned.","labels":{"kind":["kind/feature"],"area":["area/docs"],"complexity":["complexity/s"],"dispatch":["dispatch/plan"]}}`}, Input{Issue: Issue{Title: "Unfit", CreatedAt: time.Now().UTC().Format(time.RFC3339)}, Config: testConfig(), Now: time.Now().UTC()})
	if decision.NoOp {
		t.Fatal("Decide() returned no-op for out-of-scope output with extra labels")
	}
	if len(decision.ApplyLabels) != 2 || decision.ApplyLabels[0] != "wontfix" || decision.ApplyLabels[1] != "triaged" {
		t.Fatalf("ApplyLabels = %v, want [wontfix triaged]", decision.ApplyLabels)
	}
}

func TestDecideUnclearDisposition(t *testing.T) {
	t.Parallel()
	decision := Decide(context.Background(), fixtureLLM{raw: `{"disposition":"unclear","comment":"Need repro steps.","labels":{}}`}, Input{Issue: Issue{Title: "Need info", CreatedAt: time.Now().UTC().Format(time.RFC3339)}, Config: testConfig(), Now: time.Now().UTC()})
	if decision.NoOp {
		t.Fatal("Decide() returned no-op for unclear output")
	}
	if len(decision.ApplyLabels) != 2 || decision.ApplyLabels[0] != "needs-info" || decision.ApplyLabels[1] != "triaged" {
		t.Fatalf("ApplyLabels = %v, want [needs-info triaged]", decision.ApplyLabels)
	}
}

func TestDecideUnclearIgnoresExtraLabels(t *testing.T) {
	t.Parallel()
	decision := Decide(context.Background(), fixtureLLM{raw: `{"disposition":"unclear","comment":"Need repro steps.","labels":{"kind":["kind/bug"],"area":["area/runtime"],"complexity":["complexity/m"],"dispatch":["dispatch/implement"]}}`}, Input{Issue: Issue{Title: "Need info", CreatedAt: time.Now().UTC().Format(time.RFC3339)}, Config: testConfig(), Now: time.Now().UTC()})
	if decision.NoOp {
		t.Fatal("Decide() returned no-op for unclear output with extra labels")
	}
	if len(decision.ApplyLabels) != 2 || decision.ApplyLabels[0] != "needs-info" || decision.ApplyLabels[1] != "triaged" {
		t.Fatalf("ApplyLabels = %v, want [needs-info triaged]", decision.ApplyLabels)
	}
}

func TestBuildPromptAsksForMaintainerStyleComment(t *testing.T) {
	t.Parallel()
	prompt := BuildPrompt(Input{Issue: Issue{Title: "Mac install failed", Body: "DeepSeek V4 flash failed on my MacBook", Author: "VIVAAN-DHAWAN"}, Config: testConfig()})
	for _, want := range []string{
		"Reporter: @VIVAAN-DHAWAN",
		"Write the comment in a warm maintainer voice",
		"acknowledge the reporter",
		"reference concrete details",
		"name the selected label/status path",
		"give one clear next step",
		"Do not write generic bot boilerplate",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("BuildPrompt() missing %q in:\n%s", want, prompt)
		}
	}
}

func testConfig() Config {
	return Config{TriagedLabel: "triaged", MaxIssueAgeDays: 7, MaxPerTick: 5, OutOfScopeLabel: "wontfix", UnclearLabel: "needs-info", ReTriageOnAuthorReply: true}
}
