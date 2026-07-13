package cliapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/diffanchor"
	"github.com/nexu-io/looper/internal/disclosure"
	"github.com/nexu-io/looper/internal/domain"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/infra/shell"
	"github.com/nexu-io/looper/internal/outboundguard"
	"github.com/nexu-io/looper/internal/storage"
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

type reviewSubmitDiagnosticFields struct {
	Repo        string
	PRNumber    int64
	Event       string
	CommitID    string
	Payload     reviewSubmitPayload
	Error       string
	Extra       map[string]any
	RedactPaths bool
}

type reviewSubmitPullRequestViewer interface {
	ViewPullRequest(context.Context, githubinfra.ViewPullRequestInput) (githubinfra.PullRequestDetail, error)
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
		loaded.Config.Roles.Reviewer.Behavior.ReviewEvents,
		getStringFlag(cmd, "clean-review-event"),
		getStringFlag(cmd, "blocking-review-event"),
	)
	if err != nil {
		return err
	}
	if err := validateReviewSubmitEventAllowed(event, policy); err != nil {
		return err
	}
	if loaded.Config.Tools.GHPath == nil || strings.TrimSpace(*loaded.Config.Tools.GHPath) == "" {
		return fmt.Errorf("GitHub CLI (gh) not found; install gh or set --gh-path <path>")
	}
	cwd, err := r.getwd()
	if err != nil {
		return fmt.Errorf("determine current working directory: %w", err)
	}

	diagnosticWriter := func(event string, fields map[string]any) {
		writeReviewSubmitDiagnosticEntry(cmd.ErrOrStderr(), event, fields)
	}
	gh := githubinfra.New(githubinfra.Options{GHPath: *loaded.Config.Tools.GHPath, CWD: cwd, GHRun: shell.Run, ReviewSubmitDiagnostic: diagnosticWriter})
	detail, err := gh.ViewPullRequest(cmd.Context(), githubinfra.ViewPullRequestInput{Repo: repo, PRNumber: prNumber, CWD: cwd})
	if err != nil {
		return fmt.Errorf("refresh pull request before review submit: %w", err)
	}
	if err := validateExpectedHeadCommit(commitID, detail.HeadSHA); err != nil {
		return err
	}
	if err := r.validateReviewerReviewSubmitHold(cmd, loaded.Config, repo, prNumber, getBoolFlag(cmd, "reviewer-manual"), getStringFlag(cmd, "reviewer-run-id"), detail.Labels); err != nil {
		return err
	}
	if err := validateReviewSubmitBody(payload.Body, payload.Comments, commitID, event, policy, detail.Author); err != nil {
		// Always redact paths on pre-gate validation diagnostics: a malformed
		// marker or APPROVE-with-comments path never reaches SubmitReview's
		// content guard, and path may itself be secret-shaped.
		writeReviewSubmitDiagnostic(cmd.ErrOrStderr(), "github_review_submit_validation_failed", reviewSubmitDiagnosticFields{
			Repo: repo, PRNumber: prNumber, Event: event, CommitID: commitID, Payload: payload, Error: err.Error(), RedactPaths: true,
		})
		return err
	}
	submissionEvent, err := r.effectiveReviewSubmitEvent(cmd, gh, repo, prNumber, event, detail.Author, cwd)
	if err != nil {
		return err
	}
	diff, err := gh.GetPullRequestDiff(cmd.Context(), githubinfra.GetPullRequestDiffInput{Repo: repo, PRNumber: prNumber, CWD: cwd})
	var anchors *diffanchor.Index
	if err != nil {
		if canSubmitWithoutAnchorValidation(err, payload.Comments) {
			if err := r.validateLatestReviewerReviewSubmitHold(cmd, gh, loaded.Config, repo, prNumber, getBoolFlag(cmd, "reviewer-manual"), getStringFlag(cmd, "reviewer-run-id"), cwd); err != nil {
				return err
			}
			return submitReviewWithoutAnchorValidation(cmd, gh, repo, prNumber, submissionEvent, payload, commitID, cwd, loaded.Config.Disclosure)
		}
		return fmt.Errorf("fetch PR diff for anchor validation: %w", err)
	}
	parsedAnchors := diffanchor.Parse(diff)
	anchors = &parsedAnchors

	comments := make([]githubinfra.ReviewComment, 0, len(payload.Comments))
	for _, comment := range payload.Comments {
		comments = append(comments, githubinfra.ReviewComment{Body: comment.Body, Path: comment.Path, Line: comment.Line, Side: comment.Side, StartLine: comment.StartLine, StartSide: comment.StartSide})
	}
	if err := r.validateLatestReviewerReviewSubmitHold(cmd, gh, loaded.Config, repo, prNumber, getBoolFlag(cmd, "reviewer-manual"), getStringFlag(cmd, "reviewer-run-id"), cwd); err != nil {
		return err
	}
	if err := gh.SubmitReview(cmd.Context(), githubinfra.SubmitReviewInput{Repo: repo, PRNumber: prNumber, Event: submissionEvent, Body: payload.Body, CommitID: commitID, Comments: comments, Anchors: anchors, Disclosure: loaded.Config.Disclosure, CWD: cwd}); err != nil {
		return wrapReviewSubmitError(cmd, repo, prNumber, submissionEvent, commitID, payload, "submit validated PR review", err)
	}
	return writeJSON(cmd.OutOrStdout(), map[string]any{"submitted": true})
}

// wrapReviewSubmitError keeps content-safety rejections actionable for agents
// still in-session: surface the gate reason + recovery guidance, never the
// rejected payload, and record a validation diagnostic without raw paths.
func wrapReviewSubmitError(cmd *cobra.Command, repo string, prNumber int64, event string, commitID string, payload reviewSubmitPayload, prefix string, err error) error {
	if outboundguard.IsRejection(err) {
		writeReviewSubmitDiagnostic(cmd.ErrOrStderr(), "github_review_submit_validation_failed", reviewSubmitDiagnosticFields{
			Repo: repo, PRNumber: prNumber, Event: event, CommitID: commitID, Payload: payload, Error: err.Error(), RedactPaths: true,
		})
		return fmt.Errorf("%s blocked by content safety gate: %w", prefix, err)
	}
	return fmt.Errorf("%s: %w", prefix, err)
}

func (r *commandRuntime) effectiveReviewSubmitEvent(cmd *cobra.Command, gh *githubinfra.Gateway, repo string, prNumber int64, event string, authorLogin string, cwd string) (string, error) {
	if !strings.EqualFold(strings.TrimSpace(event), "APPROVE") || strings.TrimSpace(authorLogin) == "" {
		return event, nil
	}
	currentLogin, err := gh.GetCurrentUserLogin(cmd.Context(), cwd)
	if err != nil {
		return "", fmt.Errorf("determine authenticated GitHub user for self-approval check: %w", err)
	}
	if !sameGitHubLogin(currentLogin, authorLogin) {
		return event, nil
	}
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "looper: downgrading APPROVE review to COMMENT for %s#%d because authenticated GitHub user %q authored the pull request and GitHub does not allow self-approval\n", repo, prNumber, strings.TrimSpace(currentLogin))
	return "COMMENT", nil
}

func sameGitHubLogin(a string, b string) bool {
	a = strings.TrimSpace(strings.TrimPrefix(a, "@"))
	b = strings.TrimSpace(strings.TrimPrefix(b, "@"))
	return a != "" && strings.EqualFold(a, b)
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
			return fmt.Errorf("review submit --event APPROVE requires roles.reviewer.behavior.reviewEvents.clean=APPROVE")
		}
	case "REQUEST_CHANGES":
		if policy.Blocking != config.ReviewerReviewEventRequestChanges {
			return fmt.Errorf("review submit --event REQUEST_CHANGES requires roles.reviewer.behavior.reviewEvents.blocking=REQUEST_CHANGES")
		}
	}
	return nil
}

var reviewSubmitMarkerRE = regexp.MustCompile(`<!--\s*looper:review\s+([^>]*)-->`)
var markdownHTMLCommentRE = regexp.MustCompile(`(?s)<!--.*?-->`)
var markdownReferenceDefinitionRE = regexp.MustCompile(`(?m)^\s{0,3}\[[^\]\n]+\]:[^\n]*(?:\n[ \t]+[^\n]*)*`)

func validateReviewSubmitBody(body string, comments []reviewSubmitComment, commitID string, event string, policy config.ReviewerReviewEventsConfig, authorLogin string) error {
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
		if err := validateCleanApproveBody(body, authorLogin); err != nil {
			return err
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

func validateCleanApproveBody(body string, authorLogin string) error {
	visible := cleanReviewHumanBody(body)
	mention := authorMention(authorLogin)
	if mention == "" {
		return fmt.Errorf("APPROVE clean review body requires the PR author login for @mention validation")
	}
	fields := strings.Fields(visible)
	if len(fields) == 0 || !strings.EqualFold(fields[0], mention) {
		return fmt.Errorf("APPROVE clean review body must start with an @mention of the PR author")
	}
	if len(fields) < 12 {
		return fmt.Errorf("APPROVE clean review body must include a short human summary and friendly acknowledgement, not only markers or disclosure")
	}
	return nil
}

func cleanReviewHumanBody(body string) string {
	cleaned := reviewSubmitMarkerRE.ReplaceAllString(body, "")
	cleaned = disclosure.StripMarkdownStamp(cleaned)
	cleaned = markdownHTMLCommentRE.ReplaceAllString(cleaned, "")
	cleaned = markdownReferenceDefinitionRE.ReplaceAllString(cleaned, "")
	return strings.TrimSpace(cleaned)
}

func authorMention(login string) string {
	login = strings.TrimSpace(strings.TrimPrefix(login, "@"))
	if login == "" {
		return ""
	}
	return "@" + login
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

func (r *commandRuntime) validateReviewerReviewSubmitHold(cmd *cobra.Command, cfg config.Config, repo string, prNumber int64, manual bool, runID string, labels []string) error {
	if !domain.IsAutoLaneHeld(domain.LoopTypeReviewer, labels) {
		return nil
	}
	if manual {
		db, err := storage.OpenSQLiteDB(cmd.Context(), cfg.Storage.DBPath)
		if err != nil {
			return fmt.Errorf("validate held manual reviewer run: %w", err)
		}
		defer func() { _ = db.Close() }()
		trusted, err := trustedManualReviewerRun(cmd.Context(), storage.NewRepositories(db), repo, prNumber, runID)
		if err != nil {
			return err
		}
		if trusted {
			return nil
		}
	}
	return fmt.Errorf("reviewer review submit blocked because %s#%d is currently held", repo, prNumber)
}

func (r *commandRuntime) validateLatestReviewerReviewSubmitHold(cmd *cobra.Command, gh reviewSubmitPullRequestViewer, cfg config.Config, repo string, prNumber int64, manual bool, runID string, cwd string) error {
	detail, err := gh.ViewPullRequest(cmd.Context(), githubinfra.ViewPullRequestInput{Repo: repo, PRNumber: prNumber, CWD: cwd})
	if err != nil {
		return fmt.Errorf("refresh pull request hold labels before review submit: %w", err)
	}
	return r.validateReviewerReviewSubmitHold(cmd, cfg, repo, prNumber, manual, runID, detail.Labels)
}

func trustedManualReviewerRun(ctx context.Context, repos *storage.Repositories, repo string, prNumber int64, runID string) (bool, error) {
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return false, nil
	}
	if repos == nil || repos.Runs == nil || repos.Loops == nil {
		return false, fmt.Errorf("validate held manual reviewer run: storage is not configured")
	}
	run, err := repos.Runs.GetByID(ctx, runID)
	if err != nil {
		return false, fmt.Errorf("validate held manual reviewer run: %w", err)
	}
	if run == nil {
		return false, nil
	}
	if run.Status != string(domain.RunStatusRunning) {
		return false, nil
	}
	loop, err := repos.Loops.GetByID(ctx, run.LoopID)
	if err != nil {
		return false, fmt.Errorf("validate held manual reviewer loop: %w", err)
	}
	loopRepo := ""
	if loop != nil && loop.Repo != nil {
		loopRepo = *loop.Repo
	}
	if loop == nil || loop.Type != string(domain.LoopTypeReviewer) || !strings.EqualFold(strings.TrimSpace(loopRepo), strings.TrimSpace(repo)) || loop.PRNumber == nil || *loop.PRNumber != prNumber {
		return false, nil
	}
	if loop.Status != string(domain.LoopStatusRunning) {
		return false, nil
	}
	currentRun, err := currentRunningReviewerRun(ctx, repos, repo, prNumber)
	if err != nil {
		return false, err
	}
	if currentRun == nil || currentRun.ID != run.ID {
		return false, nil
	}
	manualValue, _ := parseReviewSubmitJSONObject(loop.MetadataJSON)["manual"].(bool)
	return manualValue, nil
}

func currentRunningReviewerRun(ctx context.Context, repos *storage.Repositories, repo string, prNumber int64) (*storage.RunRecord, error) {
	loops, err := repos.Loops.ListByStatuses(ctx, []string{string(domain.LoopStatusRunning)})
	if err != nil {
		return nil, fmt.Errorf("validate held manual reviewer loops: %w", err)
	}
	loopIDs := make([]string, 0, len(loops))
	for _, loop := range loops {
		loopRepo := ""
		if loop.Repo != nil {
			loopRepo = *loop.Repo
		}
		if loop.Type == string(domain.LoopTypeReviewer) && strings.EqualFold(strings.TrimSpace(loopRepo), strings.TrimSpace(repo)) && loop.PRNumber != nil && *loop.PRNumber == prNumber {
			loopIDs = append(loopIDs, loop.ID)
		}
	}
	if len(loopIDs) == 0 {
		return nil, nil
	}
	runs, err := repos.Runs.ListLatestByLoopIDs(ctx, loopIDs)
	if err != nil {
		return nil, fmt.Errorf("validate held manual reviewer runs: %w", err)
	}
	var current *storage.RunRecord
	for i := range runs {
		if runs[i].Status != string(domain.RunStatusRunning) {
			continue
		}
		if current == nil || reviewerRunNewer(runs[i], *current) {
			run := runs[i]
			current = &run
		}
	}
	return current, nil
}

func reviewerRunNewer(candidate, current storage.RunRecord) bool {
	if candidate.StartedAt != current.StartedAt {
		return candidate.StartedAt > current.StartedAt
	}
	if candidate.CreatedAt != current.CreatedAt {
		return candidate.CreatedAt > current.CreatedAt
	}
	return candidate.ID > current.ID
}

func parseReviewSubmitJSONObject(value *string) map[string]any {
	if value == nil || strings.TrimSpace(*value) == "" {
		return map[string]any{}
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(*value), &parsed); err != nil || parsed == nil {
		return map[string]any{}
	}
	return parsed
}

func canSubmitWithoutAnchorValidation(err error, comments []reviewSubmitComment) bool {
	return errors.Is(err, githubinfra.ErrDiffTooLarge) && len(comments) == 0
}

func submitReviewWithoutAnchorValidation(cmd *cobra.Command, gh *githubinfra.Gateway, repo string, prNumber int64, event string, payload reviewSubmitPayload, commitID string, cwd string, disclosureCfg config.DisclosureConfig) error {
	if err := gh.SubmitReview(cmd.Context(), githubinfra.SubmitReviewInput{Repo: repo, PRNumber: prNumber, Event: event, Body: payload.Body, CommitID: commitID, Disclosure: disclosureCfg, CWD: cwd}); err != nil {
		return wrapReviewSubmitError(cmd, repo, prNumber, event, commitID, payload, "submit PR review without anchor validation", err)
	}
	return writeJSON(cmd.OutOrStdout(), map[string]any{"submitted": true})
}

func writeReviewSubmitDiagnostic(w io.Writer, event string, fields reviewSubmitDiagnosticFields) {
	entry := map[string]any{
		"repo":         fields.Repo,
		"pr_number":    fields.PRNumber,
		"event":        event,
		"review_event": fields.Event,
		"commit_id":    strings.TrimSpace(fields.CommitID),
		"method":       "POST",
		"endpoint":     fmt.Sprintf("repos/%s/pulls/%d/reviews", fields.Repo, fields.PRNumber),
		"payload": map[string]any{
			"body_marker": reviewSubmitPayloadBodyMarker(fields.Payload.Body),
			"comments":    reviewSubmitPayloadComments(fields.Payload.Comments, fields.RedactPaths),
		},
	}
	if strings.TrimSpace(fields.Error) != "" {
		entry["error"] = strings.TrimSpace(fields.Error)
	}
	for key, value := range fields.Extra {
		entry[key] = value
	}
	writeReviewSubmitDiagnosticEntry(w, event, entry)
}

func writeReviewSubmitDiagnosticEntry(w io.Writer, event string, fields map[string]any) {
	if w == nil {
		return
	}
	entry := map[string]any{"event": event}
	for key, value := range fields {
		entry[key] = value
	}
	encoded, err := json.Marshal(entry)
	if err != nil {
		return
	}
	_, _ = io.Copy(w, bytes.NewReader(append(encoded, '\n')))
}

func reviewSubmitPayloadBodyMarker(body string) map[string]any {
	matches := reviewSubmitMarkerRE.FindAllStringSubmatch(body, -1)
	if len(matches) == 0 {
		return map[string]any{}
	}
	fields := parseReviewSubmitMarkerFields(matches[0][1])
	return map[string]any{"id": fields["id"], "head": fields["head"], "outcome": fields["outcome"]}
}

func reviewSubmitPayloadComments(comments []reviewSubmitComment, redactPaths bool) []map[string]any {
	summary := make([]map[string]any, 0, len(comments))
	for idx, comment := range comments {
		entry := map[string]any{"index": idx}
		if comment.Path != "" {
			if redactPaths {
				entry["path_present"] = true
			} else {
				entry["path"] = comment.Path
			}
		}
		if comment.Line > 0 {
			entry["line"] = comment.Line
		}
		if comment.Side != "" {
			entry["side"] = strings.ToUpper(strings.TrimSpace(comment.Side))
		}
		if comment.StartLine > 0 {
			entry["start_line"] = comment.StartLine
		}
		if comment.StartSide != "" {
			entry["start_side"] = strings.ToUpper(strings.TrimSpace(comment.StartSide))
		}
		summary = append(summary, entry)
	}
	return summary
}
