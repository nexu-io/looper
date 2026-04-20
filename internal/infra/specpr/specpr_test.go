package specpr

import "testing"

func TestHasLabelNormalizesCaseAndWhitespace(t *testing.T) {
	t.Parallel()
	if !HasLabel([]string{"  LOOPER:SPEC-REVIEWING  ", "other"}, ReviewingLabel) {
		t.Fatal("HasLabel() = false, want true")
	}
}

func TestResolvePullRequestPhase(t *testing.T) {
	t.Parallel()
	if got := ResolvePullRequestPhase([]string{"looper:spec-reviewing"}); got != PhaseSpec {
		t.Fatalf("ResolvePullRequestPhase(spec) = %q, want %q", got, PhaseSpec)
	}
	if got := ResolvePullRequestPhase([]string{"looper:ready"}); got != PhaseImplementation {
		t.Fatalf("ResolvePullRequestPhase(implementation) = %q, want %q", got, PhaseImplementation)
	}
}

func TestParseSpecPathFromPullRequestBody(t *testing.T) {
	t.Parallel()
	body := "## Summary\n\nSpec: specs/2026-04-20-demo/spec.md\nIssue: acme/looper#42"
	if got := ParseSpecPathFromPullRequestBody(body); got != "specs/2026-04-20-demo/spec.md" {
		t.Fatalf("ParseSpecPathFromPullRequestBody() = %q, want spec path", got)
	}
	if got := ParseSpecPathFromPullRequestBody("no spec here"); got != "" {
		t.Fatalf("ParseSpecPathFromPullRequestBody(no match) = %q, want empty", got)
	}
}

func TestCountUnresolvedReviewThreads(t *testing.T) {
	t.Parallel()
	comments := []map[string]any{{"state": "UNRESOLVED"}, {"isResolved": false}, {"state": "resolved"}, {"isResolved": true}, nil}
	if got := CountUnresolvedReviewThreads(comments); got != 3 {
		t.Fatalf("CountUnresolvedReviewThreads() = %d, want 3", got)
	}
}

func TestIsReviewClean(t *testing.T) {
	t.Parallel()
	if !IsReviewClean("APPROVED", []map[string]any{{"state": "RESOLVED"}}) {
		t.Fatal("IsReviewClean() = false, want true")
	}
	if IsReviewClean("CHANGES_REQUESTED", nil) {
		t.Fatal("IsReviewClean() = true, want false for requested changes")
	}
}
