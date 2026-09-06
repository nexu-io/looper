package cliapp

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
)

// Exercise persisted holds and their Continue transitions, then check the
// operator output against the lifecycle that actually remains.
func TestHandoffGuidancePreservesUnderlyingLifecycle(t *testing.T) {
	for _, initial := range []string{"failed", "budget"} {
		t.Run(initial, func(t *testing.T) {
			ctx := context.Background()
			_, repos := writeEmptyRunStatsCommandFixtureWithRepos(t)
			now := "2026-09-06T00:00:00.000Z"
			project := storage.ProjectRecord{ID: "handoff_project", Name: "Handoff", RepoPath: t.TempDir(), CreatedAt: now, UpdatedAt: now}
			if err := repos.Projects.Upsert(ctx, project); err != nil {
				t.Fatal(err)
			}
			pr := int64(646)
			reviewer := storage.LoopRecord{ID: "handoff_reviewer", Seq: 1, ProjectID: project.ID, Type: "reviewer", TargetType: "pull_request", Repo: stringPtr("nexu-io/looper"), PRNumber: &pr, Status: "running", CreatedAt: now, UpdatedAt: now}
			fixer := reviewer
			fixer.ID, fixer.Seq, fixer.Type, fixer.Status = "handoff_fixer", 2, "fixer", "failed"
			if initial == "budget" {
				fixer.Status = "queued"
			}
			for _, loop := range []storage.LoopRecord{reviewer, fixer} {
				if err := repos.Loops.Upsert(ctx, loop); err != nil {
					t.Fatal(err)
				}
			}
			getFixer := func() storage.LoopRecord {
				t.Helper()
				got, err := repos.Loops.GetByID(ctx, fixer.ID)
				if err != nil || got == nil {
					t.Fatalf("fixer: %v, %v", got, err)
				}
				return *got
			}
			if initial == "budget" {
				if _, err := loops.ParkReviewFixBudget(ctx, repos, loops.ParkReviewFixBudgetInput{Exhausted: fixer, Role: "fixer", Repo: "nexu-io/looper", PRNumber: pr, Count: 3, Cap: 3, NowISO: now}); err != nil {
					t.Fatal(err)
				}
				fresh, err := repos.Loops.GetByID(ctx, reviewer.ID)
				if err != nil || fresh == nil {
					t.Fatalf("reviewer: %v, %v", fresh, err)
				}
				reviewer = *fresh
			}
			parked, err := loops.ParkReviewScopeHuman(ctx, repos, loops.ParkReviewScopeHumanInput{Held: reviewer, Role: "reviewer", Repo: "nexu-io/looper", PRNumber: pr, NowISO: now, Question: "Clarify scope"})
			if err != nil {
				t.Fatal(err)
			}
			held := getFixer()
			if !loops.IsReviewScopeHumanHold(held) {
				t.Fatal("missing scope overlay")
			}
			parsed := parseLoopDiagnosticMetadata(held.MetadataJSON)
			message := "unusable worktree path preserved"
			run := &storage.RunRecord{Status: "failed", ErrorMessage: &message}
			if initial == "budget" {
				run = nil
			}
			diagnosis := diagnoseLoop(held, run, nil, parsed, true)
			output := loopInspectOutput{Loop: diagnosticLoopOutput(held), Metadata: parsed, Handoff: reviewFixInspectHandoff(held, parsed), Diagnosis: diagnosis}
			var buf bytes.Buffer
			if initial == "budget" {
				if !loops.IsReviewFixBudgetHold(held) {
					t.Fatal("missing budget hold")
				}
				if err := writeHumanLoopInspect(&buf, output); err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(buf.String(), "other holds, including scope holds") || strings.Contains(buf.String(), "Resume:") {
					t.Fatalf("misleading output: %s", &buf)
				}
				result, err := loops.ApplyReviewFixBudgetAnswer(ctx, repos, held, "Continue", now, loops.ReviewFixBudgetLiveCaps{ReviewerMaxPublishes: 3, FixerMaxPushes: 3})
				if err != nil || !result.Applied {
					t.Fatalf("budget Continue: %v, %v", result, err)
				}
				after := getFixer()
				if after.Status != "paused" || !loops.IsReviewScopeHumanHold(after) || loops.IsReviewFixBudgetHold(after) {
					t.Fatalf("expected remaining scope hold: %+v", after)
				}
			} else {
				baseline := diagnoseLoop(fixer, run, nil, loopDiagnosticMetadata{}, true).RecommendedAction
				if err := writeHumanLoopFailures(&buf, loopFailuresOutput{Items: []loopInspectOutput{output}}); err != nil {
					t.Fatal(err)
				}
				for _, want := range []string{baseline, "looper unpause 2", "releases only the scope hold"} {
					if !strings.Contains(buf.String(), want) {
						t.Fatalf("failures output %q missing %q", buf.String(), want)
					}
				}
				result, err := loops.ApplyReviewScopeHumanAnswer(ctx, repos, parked, "Continue", now)
				if err != nil || !result.Applied {
					t.Fatalf("scope Continue: %v, %v", result, err)
				}
				after := getFixer()
				if after.Status != "failed" || loops.IsReviewScopeHumanHold(after) {
					t.Fatalf("expected independent failure: %+v", after)
				}
				if got := diagnoseLoop(after, run, nil, parseLoopDiagnosticMetadata(after.MetadataJSON), true).RecommendedAction; got != baseline {
					t.Fatalf("recovery after release = %q, want %q", got, baseline)
				}
			}
		})
	}
}
