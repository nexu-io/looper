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
