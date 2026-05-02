package cliapp

import (
	"errors"
	"strings"
	"testing"

	"github.com/powerformer/looper/internal/config"
	githubinfra "github.com/powerformer/looper/internal/infra/github"
)

var commentOnlyReviewPolicy = config.ReviewerReviewEventsConfig{Clean: config.ReviewerReviewEventComment, Blocking: config.ReviewerReviewEventComment}
var decisionReviewPolicy = config.ReviewerReviewEventsConfig{Clean: config.ReviewerReviewEventApprove, Blocking: config.ReviewerReviewEventRequestChanges}

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

func TestValidateReviewSubmitEventAcceptsRequestChanges(t *testing.T) {
	t.Parallel()

	if event, err := validateReviewSubmitEvent("comment"); err != nil || event != "COMMENT" {
		t.Fatalf("validateReviewSubmitEvent(comment) = %q, %v; want COMMENT, nil", event, err)
	}
	if event, err := validateReviewSubmitEvent("APPROVE"); err != nil || event != "APPROVE" {
		t.Fatalf("validateReviewSubmitEvent(APPROVE) = %q, %v; want APPROVE, nil", event, err)
	}
	if event, err := validateReviewSubmitEvent("REQUEST_CHANGES"); err != nil || event != "REQUEST_CHANGES" {
		t.Fatalf("validateReviewSubmitEvent(REQUEST_CHANGES) = %q, %v; want REQUEST_CHANGES, nil", event, err)
	}
}

func TestValidateReviewSubmitBodyRequiresSingleMatchingMarker(t *testing.T) {
	t.Parallel()
	body := "Review body\n<!-- looper:review id=abc head=def outcome=actionable -->"
	if err := validateReviewSubmitBody(body, nil, "def", "COMMENT", commentOnlyReviewPolicy); err != nil {
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
			err := validateReviewSubmitBody(tc.body, nil, commitID, "COMMENT", commentOnlyReviewPolicy)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("validateReviewSubmitBody() error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestValidateReviewSubmitBodyRejectsApproveActionableMismatch(t *testing.T) {
	t.Parallel()
	body := "<!-- looper:review id=abc head=def outcome=actionable -->"
	if err := validateReviewSubmitBody(body, nil, "def", "APPROVE", decisionReviewPolicy); err == nil || !strings.Contains(err.Error(), "does not match APPROVE") {
		t.Fatalf("validateReviewSubmitBody(APPROVE actionable) error = %v, want mismatch", err)
	}
}

func TestValidateReviewSubmitBodyAllowsRequestChangesOnlyForBlocking(t *testing.T) {
	t.Parallel()
	body := "<!-- looper:review id=abc head=def outcome=blocking -->"
	if err := validateReviewSubmitBody(body, []reviewSubmitComment{{Body: "blocking", Path: "main.go", Line: 10, Side: "RIGHT"}}, "def", "REQUEST_CHANGES", decisionReviewPolicy); err != nil {
		t.Fatalf("validateReviewSubmitBody(REQUEST_CHANGES blocking) error = %v", err)
	}
	nonBlocking := "<!-- looper:review id=abc head=def outcome=non_blocking -->"
	if err := validateReviewSubmitBody(nonBlocking, nil, "def", "REQUEST_CHANGES", decisionReviewPolicy); err == nil || !strings.Contains(err.Error(), "does not match REQUEST_CHANGES") {
		t.Fatalf("validateReviewSubmitBody(REQUEST_CHANGES non_blocking) error = %v, want mismatch", err)
	}
}

func TestValidateReviewSubmitBodyRejectsCleanApproveWithInlineComments(t *testing.T) {
	t.Parallel()
	body := "<!-- looper:review id=abc head=def outcome=clean -->"
	err := validateReviewSubmitBody(body, []reviewSubmitComment{{Body: "inline", Path: "main.go", Line: 10, Side: "RIGHT"}}, "def", "APPROVE", decisionReviewPolicy)
	if err == nil || !strings.Contains(err.Error(), "without inline comments") {
		t.Fatalf("validateReviewSubmitBody(APPROVE with comments) error = %v, want inline rejection", err)
	}
}

func TestValidateReviewSubmitEventAllowedRejectsApproveWhenDisabled(t *testing.T) {
	t.Parallel()
	if err := validateReviewSubmitEventAllowed("APPROVE", commentOnlyReviewPolicy); err == nil || !strings.Contains(err.Error(), "reviewer.reviewEvents.clean=APPROVE") {
		t.Fatalf("validateReviewSubmitEventAllowed(APPROVE,commentOnly) error = %v, want policy rejection", err)
	}
	if err := validateReviewSubmitEventAllowed("APPROVE", decisionReviewPolicy); err != nil {
		t.Fatalf("validateReviewSubmitEventAllowed(APPROVE,decision) error = %v", err)
	}
	if err := validateReviewSubmitEventAllowed("REQUEST_CHANGES", commentOnlyReviewPolicy); err == nil || !strings.Contains(err.Error(), "reviewer.reviewEvents.blocking=REQUEST_CHANGES") {
		t.Fatalf("validateReviewSubmitEventAllowed(REQUEST_CHANGES,commentOnly) error = %v, want policy rejection", err)
	}
	if err := validateReviewSubmitEventAllowed("REQUEST_CHANGES", decisionReviewPolicy); err != nil {
		t.Fatalf("validateReviewSubmitEventAllowed(REQUEST_CHANGES,decision) error = %v", err)
	}
	if err := validateReviewSubmitEventAllowed("COMMENT", commentOnlyReviewPolicy); err != nil {
		t.Fatalf("validateReviewSubmitEventAllowed(COMMENT,commentOnly) error = %v", err)
	}
}

func TestValidateReviewSubmitPolicyRejectsInvalidOverrides(t *testing.T) {
	t.Parallel()
	if err := validateReviewSubmitPolicy(config.ReviewerReviewEventsConfig{Clean: config.ReviewerReviewEventRequestChanges, Blocking: config.ReviewerReviewEventComment}); err == nil || !strings.Contains(err.Error(), "COMMENT or APPROVE") {
		t.Fatalf("validateReviewSubmitPolicy(invalid clean) error = %v, want clean rejection", err)
	}
	if err := validateReviewSubmitPolicy(config.ReviewerReviewEventsConfig{Clean: config.ReviewerReviewEventComment, Blocking: config.ReviewerReviewEventApprove}); err == nil || !strings.Contains(err.Error(), "COMMENT or REQUEST_CHANGES") {
		t.Fatalf("validateReviewSubmitPolicy(invalid blocking) error = %v, want blocking rejection", err)
	}
}

func TestEffectiveReviewSubmitPolicyHonorsDecisionOverrides(t *testing.T) {
	t.Parallel()

	policy, err := effectiveReviewSubmitPolicy(commentOnlyReviewPolicy, "APPROVE", "REQUEST_CHANGES")
	if err != nil {
		t.Fatalf("effectiveReviewSubmitPolicy(decision overrides) error = %v", err)
	}
	if policy != decisionReviewPolicy {
		t.Fatalf("effectiveReviewSubmitPolicy(decision overrides) = %+v, want %+v", policy, decisionReviewPolicy)
	}
}

func TestEffectiveReviewSubmitPolicyAllowsBaseAndNarrowingOverrides(t *testing.T) {
	t.Parallel()

	policy, err := effectiveReviewSubmitPolicy(decisionReviewPolicy, "COMMENT", "COMMENT")
	if err != nil {
		t.Fatalf("effectiveReviewSubmitPolicy(narrow to comment) error = %v", err)
	}
	if policy.Clean != config.ReviewerReviewEventComment || policy.Blocking != config.ReviewerReviewEventComment {
		t.Fatalf("effectiveReviewSubmitPolicy(narrow to comment) = %+v, want both COMMENT", policy)
	}

	policy, err = effectiveReviewSubmitPolicy(decisionReviewPolicy, "APPROVE", "REQUEST_CHANGES")
	if err != nil {
		t.Fatalf("effectiveReviewSubmitPolicy(base decisions) error = %v", err)
	}
	if policy != decisionReviewPolicy {
		t.Fatalf("effectiveReviewSubmitPolicy(base decisions) = %+v, want %+v", policy, decisionReviewPolicy)
	}
}
