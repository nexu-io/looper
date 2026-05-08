package cliapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/config"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/infra/shell"
	"github.com/spf13/cobra"
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
	if err := validateReviewSubmitBody(body, nil, "def", "COMMENT", commentOnlyReviewPolicy, "octocat"); err != nil {
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
			err := validateReviewSubmitBody(tc.body, nil, commitID, "COMMENT", commentOnlyReviewPolicy, "octocat")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("validateReviewSubmitBody() error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestValidateReviewSubmitBodyRejectsApproveActionableMismatch(t *testing.T) {
	t.Parallel()
	body := "<!-- looper:review id=abc head=def outcome=actionable -->"
	if err := validateReviewSubmitBody(body, nil, "def", "APPROVE", decisionReviewPolicy, "octocat"); err == nil || !strings.Contains(err.Error(), "does not match APPROVE") {
		t.Fatalf("validateReviewSubmitBody(APPROVE actionable) error = %v, want mismatch", err)
	}
}

func TestValidateReviewSubmitBodyAllowsRequestChangesOnlyForBlocking(t *testing.T) {
	t.Parallel()
	body := "<!-- looper:review id=abc head=def outcome=blocking -->"
	if err := validateReviewSubmitBody(body, []reviewSubmitComment{{Body: "blocking", Path: "main.go", Line: 10, Side: "RIGHT"}}, "def", "REQUEST_CHANGES", decisionReviewPolicy, "octocat"); err != nil {
		t.Fatalf("validateReviewSubmitBody(REQUEST_CHANGES blocking) error = %v", err)
	}
	nonBlocking := "<!-- looper:review id=abc head=def outcome=non_blocking -->"
	if err := validateReviewSubmitBody(nonBlocking, nil, "def", "REQUEST_CHANGES", decisionReviewPolicy, "octocat"); err == nil || !strings.Contains(err.Error(), "does not match REQUEST_CHANGES") {
		t.Fatalf("validateReviewSubmitBody(REQUEST_CHANGES non_blocking) error = %v, want mismatch", err)
	}
}

func TestValidateReviewSubmitBodyRejectsCleanApproveWithInlineComments(t *testing.T) {
	t.Parallel()
	body := "<!-- looper:review id=abc head=def outcome=clean -->"
	err := validateReviewSubmitBody(body, []reviewSubmitComment{{Body: "inline", Path: "main.go", Line: 10, Side: "RIGHT"}}, "def", "APPROVE", decisionReviewPolicy, "octocat")
	if err == nil || !strings.Contains(err.Error(), "without inline comments") {
		t.Fatalf("validateReviewSubmitBody(APPROVE with comments) error = %v, want inline rejection", err)
	}
}

func TestValidateReviewSubmitBodyRequiresHumanCleanApproveBody(t *testing.T) {
	t.Parallel()
	marker := "<!-- looper:review id=abc head=def outcome=clean -->"
	stamp := "<!-- looper:stamp v=1 -->\n<sub>Generated by Looper 0.0.0-dev · runner=reviewer · agent=opencode</sub>"
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{name: "marker only", body: marker, want: "must start with an @mention"},
		{name: "disclosure only", body: marker + "\n\n" + stamp, want: "must start with an @mention"},
		{name: "wrong author", body: "@someone Thanks for the thoughtful update with clear safe changes and encouraging maintainable implementation.\n\n" + marker, want: "must start with an @mention"},
		{name: "too terse", body: "@octocat Nice work.\n\n" + marker, want: "short human summary"},
		{name: "hidden html filler", body: "@octocat <!-- these hidden filler words should not count toward the human summary requirement -->\n\n" + marker, want: "short human summary"},
		{name: "reference definition filler", body: "@octocat\n\n[hidden]:https://example.com\n  these hidden filler words should not count toward the human summary requirement\n\n" + marker, want: "short human summary"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateReviewSubmitBody(tc.body, nil, "def", "APPROVE", decisionReviewPolicy, "octocat")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("validateReviewSubmitBody() error = %v, want %q", err, tc.want)
			}
		})
	}

	body := strings.Join([]string{
		"@octocat Thanks for the thoughtful update — the changes are clear and well scoped.",
		"Summary: this keeps the approval flow safe while preserving the intended reviewer behavior.",
		"Nice work tightening this up; this should be easier to maintain going forward.",
		marker,
		stamp,
	}, "\n\n")
	if err := validateReviewSubmitBody(body, nil, "def", "APPROVE", decisionReviewPolicy, "OctoCat"); err != nil {
		t.Fatalf("validateReviewSubmitBody(APPROVE human body) error = %v", err)
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

func TestEffectiveReviewSubmitEventDowngradesSelfAuthoredApproval(t *testing.T) {
	t.Parallel()

	runner := &reviewSubmitFakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		args := strings.Join(options.Args, " ")
		if args != "api user --jq .login" {
			t.Fatalf("unexpected gh args: %q", args)
		}
		return shell.Result{Stdout: "Reviewer\n"}, nil
	}
	gh := githubinfra.New(githubinfra.Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	cmd := &cobra.Command{Use: "test"}
	stderr := &bytes.Buffer{}
	cmd.SetErr(stderr)

	got, err := (&commandRuntime{}).effectiveReviewSubmitEvent(cmd, gh, "acme/looper", 42, "APPROVE", "reviewer", "")
	if err != nil {
		t.Fatalf("effectiveReviewSubmitEvent() error = %v", err)
	}
	if got != "COMMENT" {
		t.Fatalf("effectiveReviewSubmitEvent() = %q, want COMMENT", got)
	}
	if log := stderr.String(); !strings.Contains(log, "downgrading APPROVE review to COMMENT") || !strings.Contains(log, "GitHub does not allow self-approval") {
		t.Fatalf("stderr = %q, want self-approval downgrade log", log)
	}
}

func TestEffectiveReviewSubmitEventKeepsApprovalForDifferentAuthor(t *testing.T) {
	t.Parallel()

	runner := &reviewSubmitFakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		args := strings.Join(options.Args, " ")
		if args != "api user --jq .login" {
			t.Fatalf("unexpected gh args: %q", args)
		}
		return shell.Result{Stdout: "reviewer\n"}, nil
	}
	gh := githubinfra.New(githubinfra.Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	cmd := &cobra.Command{Use: "test"}
	stderr := &bytes.Buffer{}
	cmd.SetErr(stderr)

	got, err := (&commandRuntime{}).effectiveReviewSubmitEvent(cmd, gh, "acme/looper", 42, "APPROVE", "octocat", "")
	if err != nil {
		t.Fatalf("effectiveReviewSubmitEvent() error = %v", err)
	}
	if got != "APPROVE" {
		t.Fatalf("effectiveReviewSubmitEvent() = %q, want APPROVE", got)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestEffectiveReviewSubmitEventDoesNotFetchUserForComment(t *testing.T) {
	t.Parallel()

	runner := &reviewSubmitFakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		t.Fatalf("unexpected gh args: %q", strings.Join(options.Args, " "))
		return shell.Result{}, nil
	}
	gh := githubinfra.New(githubinfra.Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	cmd := &cobra.Command{Use: "test"}

	got, err := (&commandRuntime{}).effectiveReviewSubmitEvent(cmd, gh, "acme/looper", 42, "COMMENT", "reviewer", "")
	if err != nil {
		t.Fatalf("effectiveReviewSubmitEvent() error = %v", err)
	}
	if got != "COMMENT" {
		t.Fatalf("effectiveReviewSubmitEvent() = %q, want COMMENT", got)
	}
}

func TestWriteReviewSubmitDiagnosticWritesStructuredJSON(t *testing.T) {
	t.Parallel()
	stderr := &bytes.Buffer{}
	payload := reviewSubmitPayload{
		Body:     "Please revisit this.\n<!-- looper:review id=abc head=def outcome=blocking -->",
		Comments: []reviewSubmitComment{{Body: "anchor", Path: "app.go", Line: 7, Side: "RIGHT", StartLine: 5, StartSide: "RIGHT"}},
	}
	writeReviewSubmitDiagnostic(stderr, "github_review_submit_validation_failed", reviewSubmitDiagnosticFields{
		Repo:     "acme/looper",
		PRNumber: 42,
		Event:    "REQUEST_CHANGES",
		CommitID: "def",
		Payload:  payload,
		Error:    "review marker outcome=clean does not match REQUEST_CHANGES event",
	})
	var entry map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(stderr.Bytes()), &entry); err != nil {
		t.Fatalf("decode stderr JSON: %v\n%s", err, stderr.String())
	}
	if entry["event"] != "github_review_submit_validation_failed" {
		t.Fatalf("event = %#v, want validation_failed", entry["event"])
	}
	if entry["repo"] != "acme/looper" || entry["pr_number"] != float64(42) || entry["commit_id"] != "def" {
		t.Fatalf("entry = %#v, want repo/pr/commit", entry)
	}
	payloadEntry, _ := entry["payload"].(map[string]any)
	marker, _ := payloadEntry["body_marker"].(map[string]any)
	if marker["id"] != "abc" || marker["head"] != "def" || marker["outcome"] != "blocking" {
		t.Fatalf("body marker = %#v, want marker fields", marker)
	}
	comments, _ := payloadEntry["comments"].([]any)
	if len(comments) != 1 {
		t.Fatalf("comments = %#v, want one comment", comments)
	}
	comment, _ := comments[0].(map[string]any)
	if comment["path"] != "app.go" || comment["line"] != float64(7) || comment["side"] != "RIGHT" || comment["start_line"] != float64(5) || comment["start_side"] != "RIGHT" {
		t.Fatalf("comment = %#v, want anchor summary", comment)
	}
	if entry["error"] == "" {
		t.Fatalf("entry = %#v, want error field", entry)
	}
}

type reviewSubmitFakeGHRunner struct {
	t       *testing.T
	respond func(options shell.Options) (shell.Result, error)
}

func (f *reviewSubmitFakeGHRunner) run(_ context.Context, options shell.Options) (shell.Result, error) {
	f.t.Helper()
	if f.respond == nil {
		f.t.Fatalf("fake GH runner missing responder for args: %q", strings.Join(options.Args, " "))
	}
	return f.respond(options)
}
