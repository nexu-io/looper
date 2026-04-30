package cliapp

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

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
	if err := validateReviewSubmitEventAllowed(event, loaded.Config.Defaults.AllowAutoApprove); err != nil {
		return err
	}
	if err := validateReviewSubmitBody(payload.Body, commitID, event); err != nil {
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
		return "", fmt.Errorf("review submit requires --event COMMENT or APPROVE")
	}
	if event != "COMMENT" && event != "APPROVE" {
		return "", fmt.Errorf("unsupported review event %q", event)
	}
	return event, nil
}

func validateReviewSubmitEventAllowed(event string, allowAutoApprove bool) error {
	if strings.EqualFold(strings.TrimSpace(event), "APPROVE") && !allowAutoApprove {
		return fmt.Errorf("review submit --event APPROVE requires defaults.allowAutoApprove=true")
	}
	return nil
}

var reviewSubmitMarkerRE = regexp.MustCompile(`<!--\s*looper:review\s+([^>]*)-->`)

func validateReviewSubmitBody(body string, commitID string, event string) error {
	matches := reviewSubmitMarkerRE.FindAllStringSubmatch(body, -1)
	if len(matches) != 1 {
		return fmt.Errorf("review body must contain exactly one well-formed looper review marker")
	}
	fields := parseReviewSubmitMarkerFields(matches[0][1])
	if fields["id"] == "" || fields["head"] == "" || (fields["outcome"] != "clean" && fields["outcome"] != "actionable") {
		return fmt.Errorf("review body must contain exactly one well-formed looper review marker")
	}
	if !strings.EqualFold(fields["head"], strings.TrimSpace(commitID)) {
		return fmt.Errorf("review marker head=%s does not match --commit-id %s", fields["head"], strings.TrimSpace(commitID))
	}
	if event == "APPROVE" && fields["outcome"] != "clean" {
		return fmt.Errorf("review marker outcome=%s does not match APPROVE event", fields["outcome"])
	}
	return nil
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
