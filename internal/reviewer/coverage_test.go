package reviewer

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
)

func coverageMustFix() reviewerCommentOnlyFindingResult {
	return reviewerCommentOnlyFindingResult{
		Title: "Bug", Body: "Nil deref", Disposition: reviewFindingDispositionMustFix, Severity: reviewFindingSeverityBlocking,
		ScopeBasis: reviewFindingScopeIntroducedRegression, ScopeEvidence: "new path",
	}
}

func TestValidateReviewerCommentOnlyCompletionCoverage(t *testing.T) {
	t.Parallel()

	oldJSON := `{"summary":"Must fix one","outcome":"blocking","findings":[{"title":"Bug","body":"Nil deref","disposition":"must_fix","severity":"blocking","scopeBasis":"introduced_regression","scopeEvidence":"new path"}]}`
	var old reviewerCommentOnlyCompletion
	if err := json.Unmarshal([]byte(oldJSON), &old); err != nil {
		t.Fatalf("unmarshal old completion: %v", err)
	}
	got, err := validateReviewerCommentOnlyCompletion(old)
	if err != nil {
		t.Fatalf("old JSON without coverage: %v", err)
	}
	if got.Coverage != nil {
		t.Fatalf("missing coverage = %#v, want unknown nil", got.Coverage)
	}

	withCoverage, err := validateReviewerCommentOnlyCompletion(reviewerCommentOnlyCompletion{
		Summary:  "Must fix one",
		Outcome:  "blocking",
		Findings: []reviewerCommentOnlyFindingResult{coverageMustFix()},
		Coverage: &reviewerCoverageReport{
			PassKind:          "first_pass",
			ScopeBasis:        "changed_ranges",
			Reviewed:          []string{"internal/reviewer/runner.go"},
			Incomplete:        []string{"internal/cliapp/app.go"},
			IncompleteReasons: []string{"budget exhausted before cliapp"},
		},
	})
	if err != nil {
		t.Fatalf("findings + incomplete: %v", err)
	}
	if len(withCoverage.Findings) != 1 {
		t.Fatalf("findings dropped: %#v", withCoverage.Findings)
	}
	if withCoverage.Coverage == nil || withCoverage.Coverage.PassKind != "first_pass" || len(withCoverage.Coverage.Incomplete) != 1 {
		t.Fatalf("coverage = %#v", withCoverage.Coverage)
	}

	invalidKind, err := validateReviewerCommentOnlyCompletion(reviewerCommentOnlyCompletion{
		Summary:  "Must fix one",
		Outcome:  "blocking",
		Findings: []reviewerCommentOnlyFindingResult{coverageMustFix()},
		Coverage: &reviewerCoverageReport{PassKind: "full_manifest"},
	})
	if err != nil {
		t.Fatalf("invalid passKind must not fail completion: %v", err)
	}
	if len(invalidKind.Findings) != 1 {
		t.Fatalf("invalid passKind dropped findings: %#v", invalidKind.Findings)
	}
	if invalidKind.Coverage != nil {
		t.Fatalf("invalid passKind coverage = %#v, want nil", invalidKind.Coverage)
	}

	repair, err := validateReviewerCommentOnlyCompletion(reviewerCommentOnlyCompletion{
		Summary:  "Must fix one",
		Outcome:  "blocking",
		Findings: []reviewerCommentOnlyFindingResult{coverageMustFix()},
		Coverage: &reviewerCoverageReport{PassKind: "repair_frontier", Incomplete: []string{"delta only"}},
	})
	if err != nil {
		t.Fatalf("repair_frontier coverage: %v", err)
	}
	if repair.Coverage == nil || repair.Coverage.PassKind != "repair_frontier" {
		t.Fatalf("repair coverage = %#v", repair.Coverage)
	}

	missingKind, err := validateReviewerCommentOnlyCompletion(reviewerCommentOnlyCompletion{
		Summary:  "Must fix one",
		Outcome:  "blocking",
		Findings: []reviewerCommentOnlyFindingResult{coverageMustFix()},
		Coverage: &reviewerCoverageReport{Reviewed: []string{"a.go"}},
	})
	if err != nil {
		t.Fatalf("missing passKind must not fail completion: %v", err)
	}
	if len(missingKind.Findings) != 1 {
		t.Fatalf("missing passKind dropped findings: %#v", missingKind.Findings)
	}
	if missingKind.Coverage != nil {
		t.Fatalf("missing passKind coverage = %#v, want nil", missingKind.Coverage)
	}
}

func TestBuildReviewPromptIncludesCoverageContract(t *testing.T) {
	t.Parallel()

	prompt := buildReviewPrompt("acme/looper", 42, reviewerCheckpoint{Snapshot: &checkpointSnapshot{HeadSHA: "abc123"}}, "run_1", "reviewer:loop:abc123", config.ReviewerReviewEventsConfig{Clean: config.ReviewerReviewEventComment, Blocking: config.ReviewerReviewEventComment}, false, config.ReviewerScopeChangedRanges, config.DefaultDisclosureConfig(), "opencode", "", "/opt/looper/bin/looper")
	for _, want := range []string{
		"You MAY include optional `coverage`",
		"Missing coverage means unknown, not complete",
		"Coverage never changes publish eligibility",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestParseReviewerNativeCompletionKeepsCleanCoverage(t *testing.T) {
	t.Parallel()
	got, err := parseReviewerNativeCompletion(AgentResult{
		Stdout:  `__LOOPER_RESULT__={"summary":"No actionable findings","outcome":"clean","findings":[],"coverage":{"passKind":"first_pass","incomplete":["internal/cliapp/app.go"],"incompleteReasons":["budget"]}}`,
		Summary: "No actionable findings",
	})
	if err != nil {
		t.Fatalf("parse native clean coverage: %v", err)
	}
	if len(got.Findings) != 0 || got.Coverage == nil || got.Coverage.PassKind != "first_pass" || len(got.Coverage.Incomplete) != 1 {
		t.Fatalf("native clean coverage = %#v", got)
	}

	dropped, err := parseReviewerNativeCompletion(AgentResult{
		Stdout:  `__LOOPER_RESULT__={"summary":"No actionable findings","outcome":"clean","findings":[],"coverage":{"reviewed":["a.go"]}}`,
		Summary: "No actionable findings",
	})
	if err != nil {
		t.Fatalf("parse native missing passKind: %v", err)
	}
	if dropped.Coverage != nil {
		t.Fatalf("missing passKind coverage = %#v, want nil", dropped.Coverage)
	}
}

func TestRunReviewStepPersistsCleanNativeCoverage(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	agent := &fakeAgentExecutor{results: []AgentResult{{
		Status:      "completed",
		Summary:     "No actionable findings",
		ParseStatus: "parsed",
		Stdout:      `__LOOPER_RESULT__={"summary":"No actionable findings","outcome":"clean","findings":[],"coverage":{"passKind":"first_pass","incomplete":["internal/cliapp/app.go"],"incompleteReasons":["budget"]}}`,
	}}}
	runner := New(Options{
		DB:            fixture.coordinator.DB(),
		Repos:         fixture.repos,
		GitHub:        &fakeGitHubGateway{},
		Git:           &fakeGitGateway{},
		AgentExecutor: agent,
		Logger:        fixture.logger,
		Now:           fixture.now,
		ReviewEvents:  config.ReviewerReviewEventsConfig{Clean: config.ReviewerReviewEventComment},
	})
	project, err := fixture.repos.Projects.GetByID(context.Background(), "project_1")
	if err != nil || project == nil {
		t.Fatalf("Projects.GetByID() = (%#v, %v), want project", project, err)
	}
	checkpoint, err := runner.runReviewStep(context.Background(), stepInput{
		Project:  *project,
		Loop:     storage.LoopRecord{ID: "loop_1"},
		Run:      storage.RunRecord{ID: "run_1"},
		Repo:     "acme/looper",
		PRNumber: 42,
		Checkpoint: reviewerCheckpoint{
			Detail:   &checkpointDetail{HeadRefName: "feature/review-me", BaseRefName: "main"},
			Snapshot: &checkpointSnapshot{HeadSHA: "abc123"},
			Worktree: &checkpointWorktree{Path: t.TempDir(), Branch: "feature/review-me", PreparedAt: fixture.nowISO()},
		},
	})
	if err != nil {
		t.Fatalf("runReviewStep() error = %v", err)
	}
	if checkpoint.PendingReview == nil || !strings.Contains(checkpoint.PendingReview.ReviewerSummaryJSON, `"passKind":"first_pass"`) {
		t.Fatalf("pending = %#v, want persisted coverage for clean native review", checkpoint.PendingReview)
	}
}
