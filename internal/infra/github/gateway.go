package github

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/powerformer/looper/internal/infra/shell"
	"github.com/powerformer/looper/internal/infra/specpr"
	"github.com/powerformer/looper/internal/storage"
)

const javaScriptISOStringLayout = "2006-01-02T15:04:05.000Z"

var prNumberURLPattern = regexp.MustCompile(`/pull/(\d+)(?:/|$)`)

type Options struct {
	GHPath string
	CWD    string
	Now    func() time.Time
	GHRun  func(context.Context, shell.Options) (shell.Result, error)
}

type Gateway struct {
	ghPath string
	cwd    string
	now    func() time.Time
	ghRun  func(context.Context, shell.Options) (shell.Result, error)
}

type PullRequestSummary struct {
	Number         int64
	Title          string
	URL            string
	State          string
	IsDraft        bool
	ReviewDecision string
	Labels         []string
	HeadRefName    string
	BaseRefName    string
	HeadSHA        string
	Author         string
	ReviewRequests []string
}

type PullRequestDetail struct {
	Number         int64
	Title          string
	Body           string
	URL            string
	State          string
	IsDraft        bool
	ReviewDecision string
	Labels         []string
	HeadRefName    string
	BaseRefName    string
	HeadSHA        string
	BaseSHA        string
	Author         string
	ReviewRequests []string
	HasConflicts   bool
	Comments       []map[string]any
	Reviews        []map[string]any
	Checks         []map[string]any
}

type IssueSummary struct {
	Number        int64
	Title         string
	Body          string
	URL           string
	State         string
	Author        string
	Assignees     []string
	Labels        []string
	IsPullRequest bool
}

type IssueDetail = IssueSummary

type IssueCommentInput struct {
	Repo        string
	IssueNumber int64
	Body        string
	CWD         string
}

type IssueCommentResult struct {
	ID  int64
	URL string
}

type UpdateIssueCommentInput struct {
	Repo      string
	CommentID int64
	Body      string
	CWD       string
}

type SubmitReviewInput struct {
	Repo     string
	PRNumber int64
	Event    string
	Body     string
	CommitID string
	Comments []ReviewComment
	CWD      string
}

type ReviewComment struct {
	Body      string
	Path      string
	Line      int64
	Side      string
	StartLine int64
	StartSide string
}

type PullRequestReactionInput struct {
	Repo     string
	PRNumber int64
	Content  string
	CWD      string
}

type PullRequestCommentInput struct {
	Repo     string
	PRNumber int64
	Body     string
	CWD      string
}

type PullRequestLabelsInput struct {
	Repo     string
	PRNumber int64
	Labels   []string
	CWD      string
}

type PullRequestReviewersInput struct {
	Repo      string
	PRNumber  int64
	Reviewers []string
	CWD       string
}

type CreatePullRequestInput struct {
	Repo       string
	HeadBranch string
	BaseBranch string
	Title      string
	Body       string
	CWD        string
}

type CreatePullRequestResult struct {
	Number int64
	URL    string
}

type UpdatePullRequestTitleInput struct {
	Repo     string
	PRNumber int64
	Title    string
	CWD      string
}

type ListOpenPullRequestsInput struct {
	Repo  string
	CWD   string
	Limit int
	Label string
}

type ListOpenIssuesInput struct {
	Repo     string
	CWD      string
	Limit    int
	Assignee string
	Label    string
}

type ViewIssueInput struct {
	Repo        string
	IssueNumber int64
	CWD         string
}

type ViewPullRequestInput struct {
	Repo     string
	PRNumber int64
	CWD      string
}

type ResolveReviewThreadInput struct {
	Repo     string
	ThreadID string
	CWD      string
}

type GetPullRequestDiffInput struct {
	Repo     string
	PRNumber int64
	CWD      string
}

type CapturePullRequestSnapshotInput struct {
	ProjectID  string
	Repo       string
	PRNumber   int64
	CWD        string
	CapturedAt string
}

type ReviewThreadNotFoundError struct {
	ThreadID string
}

func (e *ReviewThreadNotFoundError) Error() string {
	return fmt.Sprintf("Review thread not found: %s", e.ThreadID)
}

func New(options Options) *Gateway {
	ghPath := strings.TrimSpace(options.GHPath)
	if ghPath == "" {
		ghPath = "gh"
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	ghRun := options.GHRun
	if ghRun == nil {
		ghRun = shell.Run
	}
	return &Gateway{ghPath: ghPath, cwd: options.CWD, now: now, ghRun: ghRun}
}

func (g *Gateway) ListOpenPullRequests(ctx context.Context, input ListOpenPullRequestsInput) ([]PullRequestSummary, error) {
	args := []string{"pr", "list", "--repo", input.Repo, "--state", "open", "--limit", fmt.Sprintf("%d", defaultLimit(input.Limit))}
	if strings.TrimSpace(input.Label) != "" {
		args = append(args, "--label", input.Label)
	}
	args = append(args, "--json", strings.Join([]string{"number", "title", "url", "state", "isDraft", "reviewDecision", "labels", "headRefName", "baseRefName", "headRefOid", "author", "reviewRequests"}, ","))

	result, err := g.runGh(ctx, input.CWD, "", args...)
	if err != nil {
		return nil, err
	}
	rows, err := decodeJSONArray(result.Stdout)
	if err != nil {
		return nil, err
	}
	out := make([]PullRequestSummary, 0, len(rows))
	for _, row := range rows {
		out = append(out, PullRequestSummary{
			Number:         asInt64(row["number"]),
			Title:          asString(row["title"]),
			URL:            asString(row["url"]),
			State:          asString(row["state"]),
			IsDraft:        asBool(row["isDraft"]),
			ReviewDecision: asString(row["reviewDecision"]),
			Labels:         extractLabelNames(row["labels"]),
			HeadRefName:    asString(row["headRefName"]),
			BaseRefName:    asString(row["baseRefName"]),
			HeadSHA:        asString(row["headRefOid"]),
			Author:         extractAuthor(row["author"]),
			ReviewRequests: extractReviewRequestLogins(row["reviewRequests"]),
		})
	}
	return out, nil
}

func (g *Gateway) ListOpenIssues(ctx context.Context, input ListOpenIssuesInput) ([]IssueSummary, error) {
	args := []string{"issue", "list", "--repo", input.Repo, "--state", "open", "--limit", fmt.Sprintf("%d", defaultLimit(input.Limit))}
	if strings.TrimSpace(input.Assignee) != "" {
		args = append(args, "--assignee", input.Assignee)
	}
	if strings.TrimSpace(input.Label) != "" {
		args = append(args, "--label", input.Label)
	}
	args = append(args, "--json", strings.Join([]string{"number", "title", "body", "url", "state", "author", "assignees", "labels"}, ","))

	result, err := g.runGh(ctx, input.CWD, "", args...)
	if err != nil {
		return nil, err
	}
	rows, err := decodeJSONArray(result.Stdout)
	if err != nil {
		return nil, err
	}
	out := make([]IssueSummary, 0, len(rows))
	for _, row := range rows {
		out = append(out, IssueSummary{
			Number:    asInt64(row["number"]),
			Title:     asString(row["title"]),
			Body:      asString(row["body"]),
			URL:       asString(row["url"]),
			State:     asString(row["state"]),
			Author:    extractAuthor(row["author"]),
			Assignees: extractActorLogins(row["assignees"]),
			Labels:    extractLabelNames(row["labels"]),
		})
	}
	return out, nil
}

func (g *Gateway) ViewIssue(ctx context.Context, input ViewIssueInput) (IssueDetail, error) {
	result, err := g.runGh(ctx, input.CWD, "", "api", fmt.Sprintf("repos/%s/issues/%d", input.Repo, input.IssueNumber))
	if err != nil {
		return IssueDetail{}, err
	}
	row, err := decodeJSONObject(result.Stdout)
	if err != nil {
		return IssueDetail{}, err
	}
	return IssueDetail{
		Number:        asInt64(row["number"]),
		Title:         asString(row["title"]),
		Body:          asString(row["body"]),
		URL:           firstNonEmpty(asString(row["html_url"]), asString(row["url"])),
		State:         asString(row["state"]),
		Author:        extractAuthor(firstNonNil(row["user"], row["author"])),
		Assignees:     extractActorLogins(row["assignees"]),
		Labels:        extractLabelNames(row["labels"]),
		IsPullRequest: row["pull_request"] != nil,
	}, nil
}

func (g *Gateway) CreateIssueComment(ctx context.Context, input IssueCommentInput) (IssueCommentResult, error) {
	result, err := g.runGh(ctx, input.CWD, "", "api", fmt.Sprintf("repos/%s/issues/%d/comments", input.Repo, input.IssueNumber), "--method", "POST", "-f", "body="+input.Body)
	if err != nil {
		return IssueCommentResult{}, err
	}
	row, err := decodeJSONObject(result.Stdout)
	if err != nil {
		return IssueCommentResult{}, err
	}
	return IssueCommentResult{ID: asInt64(row["id"]), URL: asString(row["html_url"])}, nil
}

func (g *Gateway) UpdateIssueComment(ctx context.Context, input UpdateIssueCommentInput) error {
	_, err := g.runGh(ctx, input.CWD, "", "api", fmt.Sprintf("repos/%s/issues/comments/%d", input.Repo, input.CommentID), "--method", "PATCH", "-f", "body="+input.Body)
	return err
}

func (g *Gateway) ViewPullRequest(ctx context.Context, input ViewPullRequestInput) (PullRequestDetail, error) {
	result, err := g.runGh(ctx, input.CWD, "", "pr", "view", fmt.Sprintf("%d", input.PRNumber), "--repo", input.Repo, "--json", strings.Join([]string{"number", "title", "body", "url", "state", "isDraft", "reviewDecision", "labels", "headRefName", "baseRefName", "headRefOid", "baseRefOid", "author", "reviewRequests", "comments", "reviews", "statusCheckRollup", "mergeStateStatus"}, ","))
	if err != nil {
		return PullRequestDetail{}, err
	}
	row, err := decodeJSONObject(result.Stdout)
	if err != nil {
		return PullRequestDetail{}, err
	}
	threads, err := g.fetchReviewThreads(ctx, input.Repo, input.PRNumber, input.CWD)
	if err != nil {
		return PullRequestDetail{}, err
	}
	return PullRequestDetail{
		Number:         asInt64(row["number"]),
		Title:          asString(row["title"]),
		Body:           asString(row["body"]),
		URL:            asString(row["url"]),
		State:          asString(row["state"]),
		IsDraft:        asBool(row["isDraft"]),
		ReviewDecision: asString(row["reviewDecision"]),
		Labels:         extractLabelNames(row["labels"]),
		HeadRefName:    asString(row["headRefName"]),
		BaseRefName:    asString(row["baseRefName"]),
		HeadSHA:        asString(row["headRefOid"]),
		BaseSHA:        asString(row["baseRefOid"]),
		Author:         extractAuthor(row["author"]),
		ReviewRequests: extractReviewRequestLogins(row["reviewRequests"]),
		HasConflicts:   asString(row["mergeStateStatus"]) == "DIRTY",
		Comments:       threads,
		Reviews:        toObjectSlice(row["reviews"]),
		Checks:         toObjectSlice(row["statusCheckRollup"]),
	}, nil
}

func (g *Gateway) ResolveReviewThread(ctx context.Context, input ResolveReviewThreadInput) error {
	thread, err := g.getReviewThread(ctx, input.ThreadID, input.CWD)
	if err != nil {
		return err
	}
	if thread == nil {
		return &ReviewThreadNotFoundError{ThreadID: input.ThreadID}
	}
	if thread.IsResolved {
		return nil
	}
	result, err := g.runGh(ctx, input.CWD, "", "api", "graphql", "-f", "query="+strings.Join([]string{
		"mutation($threadId: ID!) {",
		"  resolveReviewThread(input: { threadId: $threadId }) {",
		"    thread { id isResolved }",
		"  }",
		"}",
	}, "\n"), "-F", "threadId="+input.ThreadID)
	if err != nil {
		return err
	}
	row, err := decodeJSONObject(result.Stdout)
	if err != nil {
		return err
	}
	data, _ := row["data"].(map[string]any)
	resolveRow, _ := data["resolveReviewThread"].(map[string]any)
	threadRow, _ := resolveRow["thread"].(map[string]any)
	if threadRow == nil || !asBool(threadRow["isResolved"]) {
		return fmt.Errorf("failed to resolve review thread %s", input.ThreadID)
	}
	return nil
}

func (g *Gateway) GetPullRequestDiff(ctx context.Context, input GetPullRequestDiffInput) (string, error) {
	result, err := g.runGh(ctx, input.CWD, "", "pr", "diff", fmt.Sprintf("%d", input.PRNumber), "--repo", input.Repo)
	if err != nil {
		return "", err
	}
	return result.Stdout, nil
}

func (g *Gateway) SubmitReview(ctx context.Context, input SubmitReviewInput) error {
	if len(input.Comments) > 0 {
		payload := map[string]any{
			"event":     input.Event,
			"body":      emptyToNil(input.Body),
			"commit_id": emptyToNil(input.CommitID),
		}
		comments := make([]map[string]any, 0, len(input.Comments))
		for _, comment := range input.Comments {
			row := map[string]any{"body": comment.Body}
			if comment.Path != "" {
				row["path"] = comment.Path
			}
			if comment.Line > 0 {
				row["line"] = comment.Line
			}
			if comment.Side != "" {
				row["side"] = comment.Side
			}
			if comment.StartLine > 0 {
				row["start_line"] = comment.StartLine
			}
			if comment.StartSide != "" {
				row["start_side"] = comment.StartSide
			}
			comments = append(comments, row)
		}
		payload["comments"] = comments
		body, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("marshal gh review payload: %w", err)
		}
		_, err = g.runGh(ctx, input.CWD, string(body), "api", fmt.Sprintf("repos/%s/pulls/%d/reviews", input.Repo, input.PRNumber), "--method", "POST", "--input", "-")
		return err
	}
	args := []string{"pr", "review", fmt.Sprintf("%d", input.PRNumber), "--repo", input.Repo, "--" + strings.ToLower(strings.ReplaceAll(input.Event, "_", "-"))}
	if input.Body != "" {
		args = append(args, "--body", input.Body)
	}
	_, err := g.runGh(ctx, input.CWD, "", args...)
	return err
}

func (g *Gateway) AddPullRequestComment(ctx context.Context, input PullRequestCommentInput) error {
	_, err := g.runGh(ctx, input.CWD, "", "pr", "comment", fmt.Sprintf("%d", input.PRNumber), "--repo", input.Repo, "--body", input.Body)
	return err
}

func (g *Gateway) AddPullRequestReaction(ctx context.Context, input PullRequestReactionInput) error {
	_, err := g.runGh(ctx, input.CWD, "", "api", fmt.Sprintf("repos/%s/issues/%d/reactions", input.Repo, input.PRNumber), "--method", "POST", "-H", "Accept: application/vnd.github+json", "-f", "content="+input.Content)
	return err
}

func (g *Gateway) RemovePullRequestReaction(ctx context.Context, input PullRequestReactionInput) error {
	currentLogin, err := g.GetCurrentUserLogin(ctx, input.CWD)
	if err != nil {
		return err
	}
	if currentLogin == "" {
		return nil
	}
	result, err := g.runGh(ctx, input.CWD, "", "api", fmt.Sprintf("repos/%s/issues/%d/reactions", input.Repo, input.PRNumber), "-H", "Accept: application/vnd.github+json")
	if err != nil {
		return err
	}
	rows, err := decodeJSONArray(result.Stdout)
	if err != nil {
		return err
	}
	for _, row := range rows {
		reaction, ok := normalizeReaction(row)
		if !ok {
			continue
		}
		if reaction.Content != input.Content || !strings.EqualFold(reaction.UserLogin, currentLogin) {
			continue
		}
		if _, err := g.runGh(ctx, input.CWD, "", "api", fmt.Sprintf("repos/%s/issues/%d/reactions/%d", input.Repo, input.PRNumber, reaction.ID), "--method", "DELETE", "-H", "Accept: application/vnd.github+json"); err != nil {
			return err
		}
	}
	return nil
}

func (g *Gateway) AddPullRequestLabels(ctx context.Context, input PullRequestLabelsInput) error {
	if len(input.Labels) == 0 {
		return nil
	}
	if err := g.ensureLabelsExist(ctx, input.Repo, input.Labels, input.CWD); err != nil {
		return err
	}
	args := []string{"api", fmt.Sprintf("repos/%s/issues/%d/labels", input.Repo, input.PRNumber), "--method", "POST"}
	for _, label := range input.Labels {
		args = append(args, "-f", "labels[]="+label)
	}
	_, err := g.runGh(ctx, input.CWD, "", args...)
	return err
}

func (g *Gateway) RemovePullRequestLabels(ctx context.Context, input PullRequestLabelsInput) error {
	if len(input.Labels) == 0 {
		return nil
	}
	for _, label := range input.Labels {
		_, err := g.runGh(ctx, input.CWD, "", "api", fmt.Sprintf("repos/%s/issues/%d/labels/%s", input.Repo, input.PRNumber, encodeURIComponent(label)), "--method", "DELETE")
		if err == nil {
			continue
		}
		if isMissingPullRequestLabelDelete(err) {
			continue
		}
		return err
	}
	return nil
}

func (g *Gateway) AddPullRequestReviewers(ctx context.Context, input PullRequestReviewersInput) error {
	if len(input.Reviewers) == 0 {
		return nil
	}
	args := []string{"api", fmt.Sprintf("repos/%s/pulls/%d/requested_reviewers", input.Repo, input.PRNumber), "--method", "POST"}
	for _, reviewer := range input.Reviewers {
		args = append(args, "-f", "reviewers[]="+reviewer)
	}
	_, err := g.runGh(ctx, input.CWD, "", args...)
	return err
}

func (g *Gateway) CreatePullRequest(ctx context.Context, input CreatePullRequestInput) (CreatePullRequestResult, error) {
	result, err := g.runGh(ctx, input.CWD, "", "pr", "create", "--repo", input.Repo, "--head", input.HeadBranch, "--base", input.BaseBranch, "--title", input.Title, "--body", input.Body)
	if err != nil {
		return CreatePullRequestResult{}, err
	}
	prURL := strings.TrimSpace(result.Stdout)
	if prURL == "" {
		return CreatePullRequestResult{}, &shell.CommandExecutionError{Message: "gh pr create returned an empty URL", Result: result}
	}
	return CreatePullRequestResult{Number: parsePRNumberFromURL(prURL), URL: prURL}, nil
}

func (g *Gateway) UpdatePullRequestTitle(ctx context.Context, input UpdatePullRequestTitleInput) error {
	_, err := g.runGh(ctx, input.CWD, "", "pr", "edit", strconv.FormatInt(input.PRNumber, 10), "--repo", input.Repo, "--title", input.Title)
	return err
}

func (g *Gateway) IsAuthenticated(ctx context.Context, cwd, hostname string) (bool, error) {
	args := []string{"auth", "status"}
	if strings.TrimSpace(hostname) != "" {
		args = append(args, "--hostname", strings.TrimSpace(hostname))
	}
	_, err := g.runGh(ctx, cwd, "", args...)
	if err == nil {
		return true, nil
	}
	var commandErr *shell.CommandExecutionError
	if errors.As(err, &commandErr) {
		return false, nil
	}
	return false, err
}

func (g *Gateway) GetCurrentUserLogin(ctx context.Context, cwd string) (string, error) {
	result, err := g.runGh(ctx, cwd, "", "api", "user", "--jq", ".login")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(result.Stdout), nil
}

func (g *Gateway) CapturePullRequestSnapshot(ctx context.Context, input CapturePullRequestSnapshotInput) (storage.PullRequestSnapshotRecord, error) {
	detail, err := g.ViewPullRequest(ctx, ViewPullRequestInput{Repo: input.Repo, PRNumber: input.PRNumber, CWD: input.CWD})
	if err != nil {
		return storage.PullRequestSnapshotRecord{}, err
	}
	diff, err := g.GetPullRequestDiff(ctx, GetPullRequestDiffInput{Repo: input.Repo, PRNumber: input.PRNumber, CWD: input.CWD})
	if err != nil {
		return storage.PullRequestSnapshotRecord{}, err
	}
	capturedAt := strings.TrimSpace(input.CapturedAt)
	if capturedAt == "" {
		capturedAt = g.now().UTC().Format(javaScriptISOStringLayout)
	}
	payload, err := json.Marshal(map[string]any{"detail": detail, "diff": diff})
	if err != nil {
		return storage.PullRequestSnapshotRecord{}, fmt.Errorf("marshal pull request snapshot payload: %w", err)
	}
	unresolvedCount := int64(countUnresolvedThreads(detail.Comments))
	return storage.PullRequestSnapshotRecord{
		ID:                    randomID(),
		ProjectID:             input.ProjectID,
		Repo:                  input.Repo,
		PRNumber:              input.PRNumber,
		HeadSHA:               valueOr(detail.HeadSHA, "unknown"),
		BaseSHA:               stringPtrIfNotEmpty(detail.BaseSHA),
		Title:                 stringPtrIfNotEmpty(detail.Title),
		Body:                  stringPtrIfNotEmpty(detail.Body),
		Author:                stringPtrIfNotEmpty(detail.Author),
		DiffRef:               stringPtr(fmt.Sprintf("gh:pr-diff:%s:%d", input.Repo, input.PRNumber)),
		ChecksSummary:         stringPtrIfNotEmpty(summarizeChecks(detail.Checks)),
		UnresolvedThreadCount: &unresolvedCount,
		ReviewState:           stringPtrIfNotEmpty(detail.ReviewDecision),
		PayloadJSON:           stringPtr(string(payload)),
		CapturedAt:            capturedAt,
		CreatedAt:             capturedAt,
	}, nil
}

type reviewThreadNode struct {
	ID         string
	IsResolved bool
}

type githubReaction struct {
	ID        int64
	Content   string
	UserLogin string
}

func (g *Gateway) fetchReviewThreads(ctx context.Context, repo string, prNumber int64, cwd string) ([]map[string]any, error) {
	owner, name, err := parseRepo(repo)
	if err != nil {
		return nil, err
	}
	result, err := g.runGh(ctx, cwd, "", "api", "graphql", "-f", "query="+strings.Join([]string{
		"query($owner: String!, $name: String!, $prNumber: Int!) {",
		"  repository(owner: $owner, name: $name) {",
		"    pullRequest(number: $prNumber) {",
		"      reviewThreads(first: 100) {",
		"        nodes {",
		"          id",
		"          isResolved",
		"          comments(first: 1) {",
		"            nodes { id body }",
		"          }",
		"        }",
		"      }",
		"    }",
		"  }",
		"}",
	}, "\n"), "-F", "owner="+owner, "-F", "name="+name, "-F", fmt.Sprintf("prNumber=%d", prNumber))
	if err != nil {
		return nil, err
	}
	row, err := decodeJSONObject(result.Stdout)
	if err != nil {
		return nil, err
	}
	data, _ := row["data"].(map[string]any)
	repoRow, _ := data["repository"].(map[string]any)
	prRow, _ := repoRow["pullRequest"].(map[string]any)
	threadsRow, _ := prRow["reviewThreads"].(map[string]any)
	nodes, _ := threadsRow["nodes"].([]any)
	out := make([]map[string]any, 0, len(nodes))
	for _, node := range nodes {
		normalized, ok := normalizeReviewThread(node)
		if ok {
			out = append(out, normalized)
		}
	}
	return out, nil
}

func (g *Gateway) getReviewThread(ctx context.Context, threadID, cwd string) (*reviewThreadNode, error) {
	result, err := g.runGh(ctx, cwd, "", "api", "graphql", "-f", "query="+strings.Join([]string{
		"query($threadId: ID!) {",
		"  node(id: $threadId) {",
		"    ... on PullRequestReviewThread {",
		"      id",
		"      isResolved",
		"    }",
		"  }",
		"}",
	}, "\n"), "-F", "threadId="+threadID)
	if err != nil {
		return nil, err
	}
	row, err := decodeJSONObject(result.Stdout)
	if err != nil {
		return nil, err
	}
	data, _ := row["data"].(map[string]any)
	node, _ := data["node"].(map[string]any)
	id := asString(node["id"])
	if id == "" {
		return nil, nil
	}
	return &reviewThreadNode{ID: id, IsResolved: asBool(node["isResolved"])}, nil
}

func (g *Gateway) ensureLabelsExist(ctx context.Context, repo string, labels []string, cwd string) error {
	seen := map[string]struct{}{}
	for _, label := range labels {
		if _, ok := seen[label]; ok {
			continue
		}
		seen[label] = struct{}{}
		if _, err := g.runGh(ctx, cwd, "", "label", "create", label, "--repo", repo, "--color", resolveLabelColor(label), "--description", resolveLabelDescription(label), "--force"); err != nil {
			return err
		}
	}
	return nil
}

func (g *Gateway) runGh(ctx context.Context, cwd, stdin string, args ...string) (shell.Result, error) {
	return g.ghRun(ctx, shell.Options{Command: g.ghPath, Args: args, CWD: valueOr(strings.TrimSpace(cwd), g.cwd), Stdin: stdin})
}

func defaultLimit(limit int) int {
	if limit <= 0 {
		return 30
	}
	return limit
}

func parseRepo(repo string) (string, string, error) {
	parts := strings.Split(repo, "/")
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return "", "", fmt.Errorf("invalid GitHub repo: %s", repo)
	}
	return parts[0], parts[1], nil
}

func summarizeChecks(checks []map[string]any) string {
	if len(checks) == 0 {
		return ""
	}
	states := make([]string, 0, len(checks))
	for _, check := range checks {
		state := asString(check["conclusion"])
		if state == "" {
			state = asString(check["state"])
		}
		if state == "" {
			state = "unknown"
		}
		states = append(states, state)
	}
	return strings.Join(states, ", ")
}

func countUnresolvedThreads(comments []map[string]any) int {
	count := 0
	for _, comment := range comments {
		state := asString(comment["state"])
		if state == "" {
			if value, ok := comment["isResolved"].(bool); ok {
				state = fmt.Sprintf("%t", value)
			}
		}
		if state != "RESOLVED" && state != "true" {
			count++
		}
	}
	return count
}

func normalizeReviewThread(value any) (map[string]any, bool) {
	row, ok := value.(map[string]any)
	if !ok {
		return nil, false
	}
	threadID := asString(row["id"])
	if threadID == "" {
		return nil, false
	}
	commentsRow, _ := row["comments"].(map[string]any)
	nodes, _ := commentsRow["nodes"].([]any)
	var first map[string]any
	if len(nodes) > 0 {
		first, _ = nodes[0].(map[string]any)
	}
	commentID := threadID
	if id := asString(first["id"]); id != "" {
		commentID = id
	}
	isResolved := asBool(row["isResolved"])
	out := map[string]any{
		"id":         commentID,
		"threadId":   threadID,
		"state":      ternary(isResolved, "RESOLVED", "UNRESOLVED"),
		"isResolved": isResolved,
	}
	if body := asString(first["body"]); body != "" {
		out["body"] = body
	}
	return out, true
}

func normalizeReaction(value any) (githubReaction, bool) {
	row, ok := value.(map[string]any)
	if !ok {
		return githubReaction{}, false
	}
	id := asInt64(row["id"])
	content := asString(row["content"])
	userLogin := extractAuthor(row["user"])
	if id <= 0 || content == "" || userLogin == "" {
		return githubReaction{}, false
	}
	return githubReaction{ID: id, Content: content, UserLogin: userLogin}, true
}

func resolveLabelColor(label string) string {
	switch strings.ToLower(strings.TrimSpace(label)) {
	case "looper:plan":
		return "5319e7"
	case specpr.ReviewingLabel:
		return "1d76db"
	case specpr.ReadyLabel:
		return "0e8a16"
	case specpr.NeedsHumanLabel:
		return "d93f0b"
	default:
		return "5319e7"
	}
}

func resolveLabelDescription(label string) string {
	switch strings.ToLower(strings.TrimSpace(label)) {
	case "looper:plan":
		return "Picked up automatically by planner"
	case specpr.ReviewingLabel:
		return "Spec PR is under review"
	case specpr.ReadyLabel:
		return "Spec PR is ready for implementation"
	case specpr.NeedsHumanLabel:
		return "Looper requires manual intervention"
	default:
		return "Managed by looper"
	}
}

func decodeJSONObject(value string) (map[string]any, error) {
	var out map[string]any
	if err := json.Unmarshal([]byte(value), &out); err != nil {
		return nil, invalidJSONError(value, err)
	}
	if out == nil {
		return map[string]any{}, nil
	}
	return out, nil
}

func decodeJSONArray(value string) ([]map[string]any, error) {
	var out []map[string]any
	if err := json.Unmarshal([]byte(value), &out); err != nil {
		return nil, invalidJSONError(value, err)
	}
	if out == nil {
		return []map[string]any{}, nil
	}
	return out, nil
}

func invalidJSONError(stdout string, err error) error {
	return &shell.CommandExecutionError{Message: "Invalid gh JSON payload", Result: shell.Result{ExitCode: 0, Stdout: stdout, Stderr: err.Error()}}
}

func toObjectSlice(value any) []map[string]any {
	items, ok := value.([]any)
	if !ok {
		return []map[string]any{}
	}
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if row, ok := item.(map[string]any); ok {
			out = append(out, row)
		}
	}
	return out
}

func extractAuthor(value any) string {
	row, ok := value.(map[string]any)
	if !ok {
		return ""
	}
	if login := asString(row["login"]); login != "" {
		return login
	}
	return asString(row["name"])
}

func extractReviewRequestLogins(value any) []string {
	items, ok := value.([]any)
	if !ok {
		return []string{}
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		if login := extractReviewRequestLogin(item); login != "" {
			out = append(out, login)
		}
	}
	return out
}

func extractReviewRequestLogin(value any) string {
	row, ok := value.(map[string]any)
	if !ok {
		return ""
	}
	if asString(row["__typename"]) == "User" {
		return asString(row["login"])
	}
	reviewer, _ := row["requestedReviewer"].(map[string]any)
	if asString(reviewer["__typename"]) == "User" {
		return asString(reviewer["login"])
	}
	return ""
}

func extractActorLogins(value any) []string {
	items, ok := value.([]any)
	if !ok {
		return []string{}
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		if login := extractAuthor(item); login != "" {
			out = append(out, login)
		}
	}
	return out
}

func extractLabelNames(value any) []string {
	items, ok := value.([]any)
	if !ok {
		return []string{}
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		row, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if name := asString(row["name"]); name != "" {
			out = append(out, name)
		}
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func firstNonNil(values ...any) any {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func parsePRNumberFromURL(value string) int64 {
	match := prNumberURLPattern.FindStringSubmatch(value)
	if len(match) != 2 {
		return 0
	}
	return asInt64(match[1])
}

func isMissingPullRequestLabelDelete(err error) bool {
	commandErr, ok := err.(*shell.CommandExecutionError)
	if !ok {
		return false
	}
	output := strings.ToLower(commandErr.Result.Stdout + "\n" + commandErr.Result.Stderr)
	return strings.Contains(output, "404") && strings.Contains(output, "label")
}

func asString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case json.Number:
		return typed.String()
	default:
		return ""
	}
}

func asBool(value any) bool {
	typed, ok := value.(bool)
	return ok && typed
}

func asInt64(value any) int64 {
	switch typed := value.(type) {
	case float64:
		return int64(typed)
	case int64:
		return typed
	case int:
		return int64(typed)
	case json.Number:
		result, _ := typed.Int64()
		return result
	case string:
		var result int64
		_, _ = fmt.Sscanf(strings.TrimSpace(typed), "%d", &result)
		return result
	default:
		return 0
	}
}

func randomID() string {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		panic(err)
	}
	bytes[6] = (bytes[6] & 0x0f) | 0x40
	bytes[8] = (bytes[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", bytes[0:4], bytes[4:6], bytes[6:8], bytes[8:10], bytes[10:16])
}

func encodeURIComponent(value string) string {
	return strings.ReplaceAll(url.QueryEscape(value), "+", "%20")
}

func stringPtr(value string) *string { return &value }

func stringPtrIfNotEmpty(value string) *string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return &value
}

func emptyToNil(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func valueOr(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func ternary[T any](condition bool, truthy, falsy T) T {
	if condition {
		return truthy
	}
	return falsy
}
