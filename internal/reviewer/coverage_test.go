package reviewer

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/config"
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
