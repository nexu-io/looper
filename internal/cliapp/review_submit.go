package cliapp

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/powerformer/looper/internal/config"
	"github.com/powerformer/looper/internal/diffanchor"
	githubinfra "github.com/powerformer/looper/internal/infra/github"
	"github.com/powerformer/looper/internal/infra/shell"
	"github.com/spf13/cobra"
)

type reviewSubmitPayload struct {
	Body     string                `json:"body"`
	Comments []reviewSubmitComment `json:"comments"`
}

type reviewSubmitComment struct {
	Body      string `json:"body"`
	Path      string `json:"path"`
	Line      int64  `json:"line"`
	Side      string `json:"side"`
	StartLine int64  `json:"start_line"`
	StartSide string `json:"start_side"`
}

func (r *commandRuntime) reviewSubmit(cmd *cobra.Command, args []string) error {
	repo, prNumber, err := parsePullRequestRef(args[0])
	if err != nil {
		return err
	}
	event, err := validateReviewSubmitEvent(getStringFlag(cmd, "event"))
	if err != nil {
		return err
	}
	commitID := strings.TrimSpace(getStringFlag(cmd, "commit-id"))
	if commitID == "" {
		return fmt.Errorf("review submit requires --commit-id expected PR head SHA")
	}

	raw, err := io.ReadAll(cmd.InOrStdin())
	if err != nil {
		return fmt.Errorf("read review payload from stdin: %w", err)
	}
	var payload reviewSubmitPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return fmt.Errorf("parse review payload JSON from stdin: %w", err)
	}

	loaded, err := r.loadConfig()
	if err != nil {
		return err
	}
	policy, err := effectiveReviewSubmitPolicy(
		loaded.Config.Reviewer.ReviewEvents,
		getStringFlag(cmd, "clean-review-event"),
		getStringFlag(cmd, "blocking-review-event"),
	)
	if err != nil {
		return err
	}
	if err := validateReviewSubmitEventAllowed(event, policy); err != nil {
		return err
	}
	if err := validateReviewSubmitBody(payload.Body, payload.Comments, commitID, event, policy); err != nil {
		return err
	}
	if loaded.Config.Tools.GHPath == nil || strings.TrimSpace(*loaded.Config.Tools.GHPath) == "" {
		return fmt.Errorf("GitHub CLI (gh) not found; install gh or set --gh-path <path>")
	}
	cwd, err := r.getwd()
	if err != nil {
		return fmt.Errorf("determine current working directory: %w", err)
	}

	gh := githubinfra.New(githubinfra.Options{GHPath: *loaded.Config.Tools.GHPath, CWD: cwd, GHRun: shell.Run})
	headSHA, err := gh.GetPullRequestHeadSHA(cmd.Context(), githubinfra.ViewPullRequestInput{Repo: repo, PRNumber: prNumber, CWD: cwd})
	if err != nil {
		return fmt.Errorf("validate expected PR head commit: %w", err)
	}
	if err := validateExpectedHeadCommit(commitID, headSHA); err != nil {
		return err
	}
	diff, err := gh.GetPullRequestDiff(cmd.Context(), githubinfra.GetPullRequestDiffInput{Repo: repo, PRNumber: prNumber, CWD: cwd})
	var anchors *diffanchor.Index
	if err != nil {
		if canSubmitWithoutAnchorValidation(err, payload.Comments) {
			return submitReviewWithoutAnchorValidation(cmd, gh, repo, prNumber, event, payload, commitID, cwd)
		}
		return fmt.Errorf("fetch PR diff for anchor validation: %w", err)
	}
	parsedAnchors := diffanchor.Parse(diff)
	anchors = &parsedAnchors

	comments := make([]githubinfra.ReviewComment, 0, len(payload.Comments))
	for _, comment := range payload.Comments {
		comments = append(comments, githubinfra.ReviewComment{Body: comment.Body, Path: comment.Path, Line: comment.Line, Side: comment.Side, StartLine: comment.StartLine, StartSide: comment.StartSide})
	}
	if err := gh.SubmitReview(cmd.Context(), githubinfra.SubmitReviewInput{Repo: repo, PRNumber: prNumber, Event: event, Body: payload.Body, CommitID: commitID, Comments: comments, Anchors: anchors, CWD: cwd}); err != nil {
		return fmt.Errorf("submit validated PR review: %w", err)
	}
	return writeJSON(cmd.OutOrStdout(), map[string]any{"submitted": true})
}

func validateReviewSubmitEvent(raw string) (string, error) {
	event := strings.ToUpper(strings.TrimSpace(raw))
	if event == "" {
		return "", fmt.Errorf("review submit requires --event COMMENT, APPROVE, or REQUEST_CHANGES")
	}
	if event != "COMMENT" && event != "APPROVE" && event != "REQUEST_CHANGES" {
		return "", fmt.Errorf("unsupported review event %q", event)
	}
	return event, nil
}

func validateReviewSubmitPolicy(policy config.ReviewerReviewEventsConfig) error {
	if policy.Clean != config.ReviewerReviewEventComment && policy.Clean != config.ReviewerReviewEventApprove {
		return fmt.Errorf("clean review event policy must be COMMENT or APPROVE")
	}
	if policy.Blocking != config.ReviewerReviewEventComment && policy.Blocking != config.ReviewerReviewEventRequestChanges {
		return fmt.Errorf("blocking review event policy must be COMMENT or REQUEST_CHANGES")
	}
	return nil
}

func effectiveReviewSubmitPolicy(base config.ReviewerReviewEventsConfig, cleanOverride string, blockingOverride string) (config.ReviewerReviewEventsConfig, error) {
	if err := validateReviewSubmitPolicy(base); err != nil {
		return config.ReviewerReviewEventsConfig{}, err
	}
	policy := base
	if value := strings.TrimSpace(cleanOverride); value != "" {
		policy.Clean = config.ReviewerReviewEvent(strings.ToUpper(value))
	}
	if value := strings.TrimSpace(blockingOverride); value != "" {
		policy.Blocking = config.ReviewerReviewEvent(strings.ToUpper(value))
	}
	if err := validateReviewSubmitPolicy(policy); err != nil {
		return config.ReviewerReviewEventsConfig{}, err
	}
	return policy, nil
}

func validateReviewSubmitEventAllowed(event string, policy config.ReviewerReviewEventsConfig) error {
	switch strings.ToUpper(strings.TrimSpace(event)) {
	case "APPROVE":
		if policy.Clean != config.ReviewerReviewEventApprove {
			return fmt.Errorf("review submit --event APPROVE requires reviewer.reviewEvents.clean=APPROVE")
		}
	case "REQUEST_CHANGES":
		if policy.Blocking != config.ReviewerReviewEventRequestChanges {
			return fmt.Errorf("review submit --event REQUEST_CHANGES requires reviewer.reviewEvents.blocking=REQUEST_CHANGES")
		}
	}
	return nil
}

var reviewSubmitMarkerRE = regexp.MustCompile(`<!--\s*looper:review\s+([^>]*)-->`)

func validateReviewSubmitBody(body string, comments []reviewSubmitComment, commitID string, event string, policy config.ReviewerReviewEventsConfig) error {
	matches := reviewSubmitMarkerRE.FindAllStringSubmatch(body, -1)
	if len(matches) != 1 {
		return fmt.Errorf("review body must contain exactly one well-formed looper review marker")
	}
	fields := parseReviewSubmitMarkerFields(matches[0][1])
	outcome := fields["outcome"]
	if fields["id"] == "" || fields["head"] == "" || !isValidReviewSubmitOutcome(outcome) {
		return fmt.Errorf("review body must contain exactly one well-formed looper review marker")
	}
	if !strings.EqualFold(fields["head"], strings.TrimSpace(commitID)) {
		return fmt.Errorf("review marker head=%s does not match --commit-id %s", fields["head"], strings.TrimSpace(commitID))
	}
	switch event {
	case "APPROVE":
		if outcome != "clean" {
			return fmt.Errorf("review marker outcome=%s does not match APPROVE event", outcome)
		}
		if len(comments) > 0 {
			return fmt.Errorf("APPROVE reviews require clean outcome without inline comments")
		}
	case "REQUEST_CHANGES":
		if outcome != "blocking" {
			return fmt.Errorf("review marker outcome=%s does not match REQUEST_CHANGES event", outcome)
		}
	case "COMMENT":
		if outcome == "clean" && policy.Clean == config.ReviewerReviewEventApprove {
			return fmt.Errorf("review marker outcome=clean requires APPROVE under effective policy")
		}
		if outcome == "blocking" && policy.Blocking == config.ReviewerReviewEventRequestChanges {
			return fmt.Errorf("review marker outcome=blocking requires REQUEST_CHANGES under effective policy")
		}
	}
	return nil
}

func isValidReviewSubmitOutcome(outcome string) bool {
	switch outcome {
	case "clean", "non_blocking", "blocking", "actionable":
		return true
	default:
		return false
	}
}

func parseReviewSubmitMarkerFields(segment string) map[string]string {
	fields := map[string]string{}
	for _, field := range strings.Fields(segment) {
		key, value, ok := strings.Cut(field, "=")
		if !ok {
			continue
		}
		fields[strings.ToLower(strings.TrimSpace(key))] = strings.TrimSpace(value)
	}
	return fields
}

func validateExpectedHeadCommit(expected string, actual string) error {
	expected = strings.TrimSpace(expected)
	actual = strings.TrimSpace(actual)
	if expected == "" {
		return fmt.Errorf("review submit requires --commit-id expected PR head SHA")
	}
	if actual == "" {
		return fmt.Errorf("validate expected PR head commit: PR head SHA is empty")
	}
	if !strings.EqualFold(expected, actual) {
		return fmt.Errorf("review submit expected head commit %s but PR head is %s; refresh the review before submitting", expected, actual)
	}
	return nil
}

func canSubmitWithoutAnchorValidation(err error, comments []reviewSubmitComment) bool {
	return errors.Is(err, githubinfra.ErrDiffTooLarge) && len(comments) == 0
}

func submitReviewWithoutAnchorValidation(cmd *cobra.Command, gh *githubinfra.Gateway, repo string, prNumber int64, event string, payload reviewSubmitPayload, commitID string, cwd string) error {
	if err := gh.SubmitReview(cmd.Context(), githubinfra.SubmitReviewInput{Repo: repo, PRNumber: prNumber, Event: event, Body: payload.Body, CommitID: commitID, CWD: cwd}); err != nil {
		return fmt.Errorf("submit PR review without anchor validation: %w", err)
	}
	return writeJSON(cmd.OutOrStdout(), map[string]any{"submitted": true})
}
