package cliapp

import (
	"errors"
	"strings"
	"testing"

	githubinfra "github.com/powerformer/looper/internal/infra/github"
)

func TestCanSubmitWithoutAnchorValidationOnlyAllowsLargeDiffTopLevelReviews(t *testing.T) {
	t.Parallel()

	if !canSubmitWithoutAnchorValidation(githubinfra.ErrDiffTooLarge, nil) {
		t.Fatalf("canSubmitWithoutAnchorValidation() = false, want true for large diff top-level review")
	}
	if canSubmitWithoutAnchorValidation(githubinfra.ErrDiffTooLarge, []reviewSubmitComment{{Body: "inline", Path: "app.go", Line: 10, Side: "RIGHT"}}) {
		t.Fatalf("canSubmitWithoutAnchorValidation() = true, want false when inline comments need validation")
	}
	if canSubmitWithoutAnchorValidation(errors.New("network failed"), nil) {
		t.Fatalf("canSubmitWithoutAnchorValidation() = true, want false for generic diff errors")
	}
}

func TestValidateExpectedHeadCommit(t *testing.T) {
	t.Parallel()

	if err := validateExpectedHeadCommit("abc123", "ABC123"); err != nil {
		t.Fatalf("validateExpectedHeadCommit() error = %v", err)
	}
	if err := validateExpectedHeadCommit("", "abc123"); err == nil || !strings.Contains(err.Error(), "requires --commit-id") {
		t.Fatalf("validateExpectedHeadCommit(empty) error = %v, want commit-id requirement", err)
	}
	if err := validateExpectedHeadCommit("abc123", "def456"); err == nil || !strings.Contains(err.Error(), "expected head commit abc123 but PR head is def456") {
		t.Fatalf("validateExpectedHeadCommit(stale) error = %v, want stale head failure", err)
	}
}

func TestValidateReviewSubmitEventRejectsRequestChanges(t *testing.T) {
	t.Parallel()

	if event, err := validateReviewSubmitEvent("comment"); err != nil || event != "COMMENT" {
		t.Fatalf("validateReviewSubmitEvent(comment) = %q, %v; want COMMENT, nil", event, err)
	}
	if event, err := validateReviewSubmitEvent("APPROVE"); err != nil || event != "APPROVE" {
		t.Fatalf("validateReviewSubmitEvent(APPROVE) = %q, %v; want APPROVE, nil", event, err)
	}
	if _, err := validateReviewSubmitEvent("REQUEST_CHANGES"); err == nil || !strings.Contains(err.Error(), "unsupported review event") {
		t.Fatalf("validateReviewSubmitEvent(REQUEST_CHANGES) error = %v, want unsupported event", err)
	}
}

func TestValidateReviewSubmitBodyRequiresSingleMatchingMarker(t *testing.T) {
	t.Parallel()
	body := "Review body\n<!-- looper:review id=abc head=def outcome=actionable -->"
	if err := validateReviewSubmitBody(body, "def", "COMMENT"); err != nil {
		t.Fatalf("validateReviewSubmitBody() error = %v", err)
	}
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{name: "missing", body: "Review body", want: "exactly one"},
		{name: "multiple", body: body + "\n<!-- looper:review id=abc head=def outcome=actionable -->", want: "exactly one"},
		{name: "malformed", body: "<!-- looper:review id=abc head=def -->", want: "exactly one"},
		{name: "stale", body: body, want: "does not match --commit-id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			commitID := "def"
			if tc.name == "stale" {
				commitID = "new"
			}
			err := validateReviewSubmitBody(tc.body, commitID, "COMMENT")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("validateReviewSubmitBody() error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestValidateReviewSubmitBodyRejectsApproveActionableMismatch(t *testing.T) {
	t.Parallel()
	body := "<!-- looper:review id=abc head=def outcome=actionable -->"
	if err := validateReviewSubmitBody(body, "def", "APPROVE"); err == nil || !strings.Contains(err.Error(), "does not match APPROVE") {
		t.Fatalf("validateReviewSubmitBody(APPROVE actionable) error = %v, want mismatch", err)
	}
}

func TestValidateReviewSubmitEventAllowedRejectsApproveWhenDisabled(t *testing.T) {
	t.Parallel()
	if err := validateReviewSubmitEventAllowed("APPROVE", false); err == nil || !strings.Contains(err.Error(), "allowAutoApprove") {
		t.Fatalf("validateReviewSubmitEventAllowed(APPROVE,false) error = %v, want allowAutoApprove rejection", err)
	}
	if err := validateReviewSubmitEventAllowed("APPROVE", true); err != nil {
		t.Fatalf("validateReviewSubmitEventAllowed(APPROVE,true) error = %v", err)
	}
	if err := validateReviewSubmitEventAllowed("COMMENT", false); err != nil {
		t.Fatalf("validateReviewSubmitEventAllowed(COMMENT,false) error = %v", err)
	}
}
