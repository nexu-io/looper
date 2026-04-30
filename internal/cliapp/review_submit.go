package cliapp

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	event := strings.ToUpper(strings.TrimSpace(getStringFlag(cmd, "event")))
	if event == "" {
		return fmt.Errorf("review submit requires --event COMMENT, APPROVE, or REQUEST_CHANGES")
	}
	if event != "COMMENT" && event != "APPROVE" && event != "REQUEST_CHANGES" {
		return fmt.Errorf("unsupported review event %q", event)
	}
	commitID := strings.TrimSpace(getStringFlag(cmd, "commit-id"))

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
	if loaded.Config.Tools.GHPath == nil || strings.TrimSpace(*loaded.Config.Tools.GHPath) == "" {
		return fmt.Errorf("GitHub CLI (gh) not found; install gh or set --gh-path <path>")
	}
	cwd, err := r.getwd()
	if err != nil {
		return fmt.Errorf("determine current working directory: %w", err)
	}

	gh := githubinfra.New(githubinfra.Options{GHPath: *loaded.Config.Tools.GHPath, CWD: cwd, GHRun: shell.Run})
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

func canSubmitWithoutAnchorValidation(err error, comments []reviewSubmitComment) bool {
	return errors.Is(err, githubinfra.ErrDiffTooLarge) && len(comments) == 0
}

func submitReviewWithoutAnchorValidation(cmd *cobra.Command, gh *githubinfra.Gateway, repo string, prNumber int64, event string, payload reviewSubmitPayload, commitID string, cwd string) error {
	if err := gh.SubmitReview(cmd.Context(), githubinfra.SubmitReviewInput{Repo: repo, PRNumber: prNumber, Event: event, Body: payload.Body, CommitID: commitID, CWD: cwd}); err != nil {
		return fmt.Errorf("submit PR review without anchor validation: %w", err)
	}
	return writeJSON(cmd.OutOrStdout(), map[string]any{"submitted": true})
}
