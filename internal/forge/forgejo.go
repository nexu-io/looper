package forge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/outboundguard"
)

const defaultForgejoTimeout = 30 * time.Second
const maxForgejoResponseBodyBytes = 1 << 20

type ForgejoClient struct {
	baseURL    *url.URL
	token      string
	httpClient *http.Client
	repo       RepositoryRef
}

type ForgejoOption func(*ForgejoClient)

func WithHTTPClient(client *http.Client) ForgejoOption {
	return func(forgejo *ForgejoClient) {
		if client != nil {
			forgejo.httpClient = client
		}
	}
}

func WithTimeout(timeout time.Duration) ForgejoOption {
	return func(forgejo *ForgejoClient) {
		if timeout > 0 {
			forgejo.httpClient.Timeout = timeout
		}
	}
}

func NewForgejoClient(ref RepositoryRef, token string, options ...ForgejoOption) (*ForgejoClient, error) {
	baseURL, err := parseForgejoBaseURL(ref.BaseURL)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(ref.ProviderID) == "" {
		return nil, fmt.Errorf("forgejo client: provider id is required")
	}
	if strings.TrimSpace(ref.Repo) == "" {
		return nil, fmt.Errorf("forgejo client: repo is required")
	}
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("forgejo client: token is required")
	}
	client := &ForgejoClient{
		baseURL:    baseURL,
		token:      token,
		httpClient: &http.Client{Timeout: defaultForgejoTimeout},
		repo: RepositoryRef{
			ProviderID: strings.TrimSpace(ref.ProviderID),
			Kind:       ProviderKindForgejo,
			BaseURL:    strings.TrimRight(baseURL.String(), "/"),
			Repo:       strings.Trim(strings.TrimSpace(ref.Repo), "/"),
		},
	}
	for _, option := range options {
		if option != nil {
			option(client)
		}
	}
	if client.httpClient == nil {
		client.httpClient = &http.Client{Timeout: defaultForgejoTimeout}
	}
	if client.httpClient.Timeout == 0 {
		client.httpClient.Timeout = defaultForgejoTimeout
	}
	return client, nil
}

func NewForgejoClientFromConfig(provider config.ProviderConfig, repo string, options ...ForgejoOption) (*ForgejoClient, error) {
	if provider.Kind != config.ProviderKindForgejo {
		return nil, fmt.Errorf("forgejo client: provider %q kind = %q, want forgejo", provider.ID, provider.Kind)
	}
	if provider.TokenEnv == nil || strings.TrimSpace(*provider.TokenEnv) == "" {
		return nil, fmt.Errorf("forgejo client: provider %q tokenEnv is required", provider.ID)
	}
	token := os.Getenv(strings.TrimSpace(*provider.TokenEnv))
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("forgejo client: environment variable %s is required", strings.TrimSpace(*provider.TokenEnv))
	}
	return NewForgejoClient(RepositoryRef{ProviderID: provider.ID, Kind: ProviderKindForgejo, BaseURL: provider.BaseURL, Repo: repo}, token, options...)
}

func (forgejo *ForgejoClient) Kind() ProviderKind { return ProviderKindForgejo }

func (forgejo *ForgejoClient) Repository() RepositoryRef { return forgejo.repo }

func (forgejo *ForgejoClient) Capabilities() Capabilities {
	capabilities, _ := StaticCapabilities(ProviderKindForgejo)
	return capabilities
}

func (forgejo *ForgejoClient) CurrentUser(ctx context.Context) (Identity, error) {
	var user forgejoUser
	if err := forgejo.do(ctx, http.MethodGet, "user", nil, nil, &user); err != nil {
		return Identity{}, err
	}
	return Identity{Login: user.Login, ID: user.ID}, nil
}

type ListIssuesInput struct {
	State    string
	Labels   []string
	Assignee string
	Limit    int
}

type ListPullRequestsInput struct {
	State  string
	Labels []string
	Limit  int
}

type CompareBranchesInput struct {
	Base string
	Head string
}

type CompareBranchesResult struct {
	Status       string
	AheadBy      int
	BehindBy     int
	TotalCommits int
}

type CreatePullRequestInput struct {
	Title string
	Body  string
	Head  string
	Base  string
}

type UpdatePullRequestInput struct {
	Number int64
	Title  *string
	Body   *string
}

type CreateCommentInput struct {
	IssueNumber int64
	Body        string
}

type UpdateCommentInput struct {
	CommentID int64
	Body      string
}

type Issue struct {
	Number    int64
	Title     string
	Body      string
	State     string
	HTMLURL   string
	UpdatedAt string
	User      Identity
	Labels    []Label
	Assignees []Identity
}

type PullRequest struct {
	Number    int64
	Title     string
	Body      string
	State     string
	IsDraft   bool
	HTMLURL   string
	UpdatedAt string
	User      Identity
	Head      BranchRef
	Base      BranchRef
	Labels    []Label
	Assignees []Identity
}

type BranchRef struct {
	Name string
	SHA  string
}

type Label struct {
	ID   int64
	Name string
}

type Comment struct {
	ID        int64
	Body      string
	HTMLURL   string
	UpdatedAt string
	User      Identity
}

type PullRequestReviewComment struct {
	ID                  int64
	Body                string
	HTMLURL             string
	UpdatedAt           string
	User                Identity
	Path                string
	CommitID            string
	OriginalCommitID    string
	Position            int
	OriginalPosition    int
	DiffHunk            string
	PullRequestReviewID int64
	Resolver            ForgejoReviewCommentResolverField
}

type ForgejoReviewCommentResolverField struct {
	Present bool
	Value   *Identity
}

type ForgejoHTTPError struct {
	Method     string
	Path       string
	StatusCode int
	Message    string
}

func (err *ForgejoHTTPError) Error() string {
	return fmt.Sprintf("forgejo API %s %s returned HTTP %d: %s", err.Method, err.Path, err.StatusCode, err.Message)
}

func (err *ForgejoHTTPError) HTTPStatusCode() int {
	if err == nil {
		return 0
	}
	return err.StatusCode
}

func (forgejo *ForgejoClient) ListOpenIssues(ctx context.Context, input ListIssuesInput) ([]Issue, error) {
	if strings.TrimSpace(input.State) == "" {
		input.State = "open"
	}
	query := url.Values{"state": {input.State}}
	if len(input.Labels) > 0 {
		query.Set("labels", strings.Join(input.Labels, ","))
	}
	if strings.TrimSpace(input.Assignee) != "" {
		query.Set("assignee", strings.TrimSpace(input.Assignee))
	}
	var output []forgejoIssue
	if err := forgejo.getPaged(ctx, forgejo.repoPath("issues"), query, input.Limit, &output); err != nil {
		return nil, err
	}
	issues := make([]Issue, 0, len(output))
	for _, issue := range output {
		issues = append(issues, convertIssue(issue))
	}
	return issues, nil
}

func (forgejo *ForgejoClient) ViewIssue(ctx context.Context, number int64) (Issue, error) {
	var output forgejoIssue
	if err := forgejo.do(ctx, http.MethodGet, forgejo.repoPath("issues", strconv.FormatInt(number, 10)), nil, nil, &output); err != nil {
		return Issue{}, err
	}
	return convertIssue(output), nil
}

func (forgejo *ForgejoClient) ListOpenPullRequests(ctx context.Context, input ListPullRequestsInput) ([]PullRequest, error) {
	if strings.TrimSpace(input.State) == "" {
		input.State = "open"
	}
	query := url.Values{"state": {input.State}}
	limit := input.Limit
	if len(input.Labels) > 0 {
		limit = 0
	}
	var output []forgejoPullRequest
	if err := forgejo.getPaged(ctx, forgejo.repoPath("pulls"), query, limit, &output); err != nil {
		return nil, err
	}
	pulls := make([]PullRequest, 0, len(output))
	for _, pull := range output {
		converted := convertPullRequest(pull)
		if !hasAllLabelNames(converted.Labels, input.Labels) {
			continue
		}
		pulls = append(pulls, converted)
		if input.Limit > 0 && len(pulls) >= input.Limit {
			break
		}
	}
	return pulls, nil
}

func (forgejo *ForgejoClient) ViewPullRequest(ctx context.Context, number int64) (PullRequest, error) {
	var output forgejoPullRequest
	if err := forgejo.do(ctx, http.MethodGet, forgejo.repoPath("pulls", strconv.FormatInt(number, 10)), nil, nil, &output); err != nil {
		return PullRequest{}, err
	}
	return convertPullRequest(output), nil
}

func (forgejo *ForgejoClient) CompareBranches(ctx context.Context, input CompareBranchesInput) (CompareBranchesResult, error) {
	var output forgejoCompareBranches
	path := forgejo.repoPath("compare", url.PathEscape(strings.TrimSpace(input.Base))+"..."+url.PathEscape(strings.TrimSpace(input.Head)))
	if err := forgejo.do(ctx, http.MethodGet, path, nil, nil, &output); err != nil {
		return CompareBranchesResult{}, err
	}
	if output.Status == "" && output.AheadBy == 0 && output.TotalCommits > 0 {
		output.Status = "ahead"
		output.AheadBy = output.TotalCommits
	}
	return CompareBranchesResult{
		Status:       output.Status,
		AheadBy:      output.AheadBy,
		BehindBy:     output.BehindBy,
		TotalCommits: output.TotalCommits,
	}, nil
}

func (forgejo *ForgejoClient) PullRequestDiff(ctx context.Context, number int64) (string, error) {
	var diff string
	if err := forgejo.do(ctx, http.MethodGet, forgejo.repoPath("pulls", strconv.FormatInt(number, 10)+".diff"), nil, nil, &diff); err != nil {
		return "", err
	}
	return diff, nil
}

func (forgejo *ForgejoClient) CreatePullRequest(ctx context.Context, input CreatePullRequestInput) (PullRequest, error) {
	if err := outboundguard.Validate(
		outboundguard.Field{Name: "pull request title", Text: input.Title},
		outboundguard.Field{Name: "pull request body", Text: input.Body},
	); err != nil {
		return PullRequest{}, err
	}
	payload := map[string]string{"title": input.Title, "body": input.Body, "head": input.Head, "base": input.Base}
	var output forgejoPullRequest
	if err := forgejo.do(ctx, http.MethodPost, forgejo.repoPath("pulls"), nil, payload, &output); err != nil {
		return PullRequest{}, err
	}
	return convertPullRequest(output), nil
}

func (forgejo *ForgejoClient) UpdatePullRequest(ctx context.Context, input UpdatePullRequestInput) (PullRequest, error) {
	fields := make([]outboundguard.Field, 0, 2)
	if input.Title != nil {
		fields = append(fields, outboundguard.Field{Name: "pull request title", Text: *input.Title})
	}
	if input.Body != nil {
		fields = append(fields, outboundguard.Field{Name: "pull request body", Text: *input.Body})
	}
	if err := outboundguard.Validate(fields...); err != nil {
		return PullRequest{}, err
	}
	payload := map[string]string{}
	if input.Title != nil {
		payload["title"] = *input.Title
	}
	if input.Body != nil {
		payload["body"] = *input.Body
	}
	var output forgejoPullRequest
	if err := forgejo.do(ctx, http.MethodPatch, forgejo.repoPath("pulls", strconv.FormatInt(input.Number, 10)), nil, payload, &output); err != nil {
		return PullRequest{}, err
	}
	return convertPullRequest(output), nil
}

func (forgejo *ForgejoClient) AddIssueLabels(ctx context.Context, issueNumber int64, labels []string) ([]Label, error) {
	var output []forgejoLabel
	if err := forgejo.do(ctx, http.MethodPost, forgejo.repoPath("issues", strconv.FormatInt(issueNumber, 10), "labels"), nil, map[string][]string{"labels": labels}, &output); err != nil {
		return nil, err
	}
	return convertLabels(output), nil
}

func (forgejo *ForgejoClient) RemoveIssueLabel(ctx context.Context, issueNumber int64, label string) error {
	return forgejo.do(ctx, http.MethodDelete, forgejo.repoPath("issues", strconv.FormatInt(issueNumber, 10), "labels", url.PathEscape(label)), nil, nil, nil)
}

func (forgejo *ForgejoClient) AddIssueAssignees(ctx context.Context, issueNumber int64, assignees []string) error {
	return forgejo.do(ctx, http.MethodPost, forgejo.repoPath("issues", strconv.FormatInt(issueNumber, 10), "assignees"), nil, map[string][]string{"assignees": assignees}, nil)
}

func (forgejo *ForgejoClient) RemoveIssueAssignees(ctx context.Context, issueNumber int64, assignees []string) error {
	return forgejo.do(ctx, http.MethodDelete, forgejo.repoPath("issues", strconv.FormatInt(issueNumber, 10), "assignees"), nil, map[string][]string{"assignees": assignees}, nil)
}

func (forgejo *ForgejoClient) CreateIssueComment(ctx context.Context, input CreateCommentInput) (Comment, error) {
	if err := outboundguard.Validate(outboundguard.Field{Name: "issue comment body", Text: input.Body}); err != nil {
		return Comment{}, err
	}
	var output forgejoComment
	if err := forgejo.do(ctx, http.MethodPost, forgejo.repoPath("issues", strconv.FormatInt(input.IssueNumber, 10), "comments"), nil, map[string]string{"body": input.Body}, &output); err != nil {
		return Comment{}, err
	}
	return convertComment(output), nil
}

func (forgejo *ForgejoClient) ListIssueComments(ctx context.Context, issueNumber int64) ([]Comment, error) {
	var output []forgejoComment
	if err := forgejo.getPaged(ctx, forgejo.repoPath("issues", strconv.FormatInt(issueNumber, 10), "comments"), nil, 0, &output); err != nil {
		return nil, err
	}
	comments := make([]Comment, 0, len(output))
	for _, comment := range output {
		comments = append(comments, convertComment(comment))
	}
	return comments, nil
}

func (forgejo *ForgejoClient) UpdateIssueComment(ctx context.Context, input UpdateCommentInput) (Comment, error) {
	if err := outboundguard.Validate(outboundguard.Field{Name: "issue comment body", Text: input.Body}); err != nil {
		return Comment{}, err
	}
	var output forgejoComment
	if err := forgejo.do(ctx, http.MethodPatch, forgejo.repoPath("issues", "comments", strconv.FormatInt(input.CommentID, 10)), nil, map[string]string{"body": input.Body}, &output); err != nil {
		return Comment{}, err
	}
	return convertComment(output), nil
}

func (forgejo *ForgejoClient) ListPullRequestReviewComments(ctx context.Context, number int64) ([]PullRequestReviewComment, error) {
	var reviews []forgejoPullRequestReview
	if err := forgejo.getPaged(ctx, forgejo.repoPath("pulls", strconv.FormatInt(number, 10), "reviews"), nil, 0, &reviews); err != nil {
		return nil, err
	}
	comments := make([]PullRequestReviewComment, 0)
	for _, review := range reviews {
		var output []forgejoPullRequestReviewComment
		if err := forgejo.getPaged(ctx, forgejo.repoPath("pulls", strconv.FormatInt(number, 10), "reviews", strconv.FormatInt(review.ID, 10), "comments"), nil, 0, &output); err != nil {
			return nil, err
		}
		for _, comment := range output {
			comments = append(comments, convertPullRequestReviewComment(comment))
		}
	}
	return comments, nil
}

func (forgejo *ForgejoClient) ResolvePullRequestReviewComment(ctx context.Context, number int64, commentID int64) error {
	return forgejo.do(ctx, http.MethodPost, forgejo.repoPath("pulls", "comments", strconv.FormatInt(commentID, 10), "resolve"), nil, nil, nil)
}

func parseForgejoBaseURL(value string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, fmt.Errorf("forgejo client: baseURL must be an absolute http(s) URL")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	return parsed, nil
}

func (forgejo *ForgejoClient) repoPath(parts ...string) string {
	return strings.Join(append([]string{"repos", forgejo.repo.Repo}, parts...), "/")
}

func (forgejo *ForgejoClient) getPaged(ctx context.Context, path string, query url.Values, limit int, out any) error {
	page := 1
	remaining := limit
	all := bytes.Buffer{}
	for {
		pageQuery := cloneValues(query)
		pageQuery.Set("page", strconv.Itoa(page))
		if remaining > 0 && remaining < 50 {
			pageQuery.Set("limit", strconv.Itoa(remaining))
		} else {
			pageQuery.Set("limit", "50")
		}
		var raw json.RawMessage
		response, err := forgejo.doRaw(ctx, http.MethodGet, path, pageQuery, nil)
		if err != nil {
			return err
		}
		raw = response.body
		trimmed := bytes.TrimSpace(raw)
		if len(trimmed) > 0 && string(trimmed) != "[]" {
			if all.Len() == 0 {
				all.WriteByte('[')
			} else {
				all.WriteByte(',')
			}
			all.Write(bytes.TrimSuffix(bytes.TrimPrefix(trimmed, []byte("[")), []byte("]")))
		}
		if remaining > 0 {
			var pageItems []json.RawMessage
			if err := json.Unmarshal(raw, &pageItems); err != nil {
				return fmt.Errorf("forgejo API decode %s: %w", path, err)
			}
			remaining -= len(pageItems)
			if remaining <= 0 {
				break
			}
		}
		if !hasNextPage(response.header, page) {
			break
		}
		page++
	}
	if all.Len() == 0 {
		all.WriteString("[]")
	} else {
		all.WriteByte(']')
	}
	if err := json.Unmarshal(all.Bytes(), out); err != nil {
		return fmt.Errorf("forgejo API decode %s: %w", path, err)
	}
	return nil
}

type rawResponse struct {
	body   []byte
	header http.Header
}

func (forgejo *ForgejoClient) do(ctx context.Context, method string, path string, query url.Values, payload any, out any) error {
	response, err := forgejo.doRaw(ctx, method, path, query, payload)
	if err != nil {
		return err
	}
	if out == nil || len(bytes.TrimSpace(response.body)) == 0 {
		return nil
	}
	if text, ok := out.(*string); ok {
		*text = string(response.body)
		return nil
	}
	if err := json.Unmarshal(response.body, out); err != nil {
		return fmt.Errorf("forgejo API decode %s %s: %w", method, path, err)
	}
	return nil
}

func (forgejo *ForgejoClient) doRaw(ctx context.Context, method string, path string, query url.Values, payload any) (rawResponse, error) {
	apiURL, err := forgejo.apiURL(path)
	if err != nil {
		return rawResponse{}, err
	}
	apiURL.RawQuery = query.Encode()
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return rawResponse{}, fmt.Errorf("forgejo API encode %s %s: %w", method, path, err)
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, apiURL.String(), body)
	if err != nil {
		return rawResponse{}, fmt.Errorf("forgejo API build request %s %s: %w", method, path, err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "token "+forgejo.token)
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := forgejo.httpClient.Do(request)
	if err != nil {
		return rawResponse{}, fmt.Errorf("forgejo API %s %s failed: %w", method, path, err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maxForgejoResponseBodyBytes+1))
	if err != nil {
		return rawResponse{}, fmt.Errorf("forgejo API read response %s %s: %w", method, path, err)
	}
	if len(responseBody) > maxForgejoResponseBodyBytes {
		return rawResponse{}, fmt.Errorf("forgejo API %s %s response exceeds %d bytes", method, path, maxForgejoResponseBodyBytes)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return rawResponse{}, &ForgejoHTTPError{Method: method, Path: path, StatusCode: response.StatusCode, Message: sanitizeForgejoErrorBody(responseBody, forgejo.token)}
	}
	return rawResponse{body: responseBody, header: response.Header.Clone()}, nil
}

func (forgejo *ForgejoClient) apiURL(path string) (*url.URL, error) {
	cleanPath := strings.TrimLeft(path, "/")
	decodedPath, err := url.PathUnescape(cleanPath)
	if err != nil {
		return nil, fmt.Errorf("forgejo API invalid path %q: %w", path, err)
	}
	apiURL := *forgejo.baseURL
	apiURL.Path = strings.TrimRight(forgejo.baseURL.Path, "/") + "/api/v1/" + decodedPath
	apiURL.RawPath = strings.TrimRight(forgejo.baseURL.EscapedPath(), "/") + "/api/v1/" + cleanPath
	return &apiURL, nil
}

func cloneValues(input url.Values) url.Values {
	output := make(url.Values, len(input))
	for key, values := range input {
		output[key] = append([]string(nil), values...)
	}
	return output
}

func hasNextPage(header http.Header, currentPage int) bool {
	if totalPages := strings.TrimSpace(header.Get("X-Total-Pages")); totalPages != "" {
		parsed, err := strconv.Atoi(totalPages)
		return err == nil && currentPage < parsed
	}
	return strings.Contains(header.Get("Link"), `rel="next"`)
}

func sanitizeForgejoErrorBody(body []byte, token string) string {
	message := strings.TrimSpace(string(body))
	if message == "" {
		message = http.StatusText(http.StatusInternalServerError)
	}
	if strings.TrimSpace(token) != "" {
		message = strings.ReplaceAll(message, token, "[REDACTED]")
	}
	return message
}

type forgejoUser struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
}

type forgejoLabel struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type forgejoIssue struct {
	Number    int64          `json:"number"`
	Title     string         `json:"title"`
	Body      string         `json:"body"`
	State     string         `json:"state"`
	HTMLURL   string         `json:"html_url"`
	UpdatedAt string         `json:"updated_at"`
	User      forgejoUser    `json:"user"`
	Labels    []forgejoLabel `json:"labels"`
	Assignees []forgejoUser  `json:"assignees"`
}

type forgejoPullRequest struct {
	Number    int64          `json:"number"`
	Title     string         `json:"title"`
	Body      string         `json:"body"`
	State     string         `json:"state"`
	Draft     bool           `json:"draft"`
	HTMLURL   string         `json:"html_url"`
	UpdatedAt string         `json:"updated_at"`
	User      forgejoUser    `json:"user"`
	Head      forgejoBranch  `json:"head"`
	Base      forgejoBranch  `json:"base"`
	Labels    []forgejoLabel `json:"labels"`
	Assignees []forgejoUser  `json:"assignees"`
}

type forgejoBranch struct {
	Name string `json:"name"`
	Ref  string `json:"ref"`
	SHA  string `json:"sha"`
}

type forgejoCompareBranches struct {
	Status       string `json:"status"`
	AheadBy      int    `json:"ahead_by"`
	BehindBy     int    `json:"behind_by"`
	TotalCommits int    `json:"total_commits"`
}

type forgejoComment struct {
	ID        int64       `json:"id"`
	Body      string      `json:"body"`
	HTMLURL   string      `json:"html_url"`
	UpdatedAt string      `json:"updated_at"`
	User      forgejoUser `json:"user"`
}

type forgejoResolverField struct {
	Present bool
	Value   *forgejoUser
}

func (field *forgejoResolverField) UnmarshalJSON(data []byte) error {
	field.Present = true
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		field.Value = nil
		return nil
	}
	var user forgejoUser
	if err := json.Unmarshal(data, &user); err != nil {
		return err
	}
	field.Value = &user
	return nil
}

type forgejoPullRequestReviewComment struct {
	ID                  int64                `json:"id"`
	Body                string               `json:"body"`
	HTMLURL             string               `json:"html_url"`
	UpdatedAt           string               `json:"updated_at"`
	User                forgejoUser          `json:"user"`
	Path                string               `json:"path"`
	CommitID            string               `json:"commit_id"`
	OriginalCommitID    string               `json:"original_commit_id"`
	Position            int                  `json:"position"`
	OriginalPosition    int                  `json:"original_position"`
	DiffHunk            string               `json:"diff_hunk"`
	PullRequestReviewID int64                `json:"pull_request_review_id"`
	Resolver            forgejoResolverField `json:"resolver"`
}

type forgejoPullRequestReview struct {
	ID int64 `json:"id"`
}

func convertIssue(input forgejoIssue) Issue {
	return Issue{Number: input.Number, Title: input.Title, Body: input.Body, State: input.State, HTMLURL: input.HTMLURL, UpdatedAt: input.UpdatedAt, User: convertUser(input.User), Labels: convertLabels(input.Labels), Assignees: convertUsers(input.Assignees)}
}

func convertPullRequest(input forgejoPullRequest) PullRequest {
	return PullRequest{Number: input.Number, Title: input.Title, Body: input.Body, State: input.State, IsDraft: input.Draft, HTMLURL: input.HTMLURL, UpdatedAt: input.UpdatedAt, User: convertUser(input.User), Head: convertBranch(input.Head), Base: convertBranch(input.Base), Labels: convertLabels(input.Labels), Assignees: convertUsers(input.Assignees)}
}

func convertComment(input forgejoComment) Comment {
	return Comment{ID: input.ID, Body: input.Body, HTMLURL: input.HTMLURL, UpdatedAt: input.UpdatedAt, User: convertUser(input.User)}
}

func convertPullRequestReviewComment(input forgejoPullRequestReviewComment) PullRequestReviewComment {
	comment := PullRequestReviewComment{
		ID:                  input.ID,
		Body:                input.Body,
		HTMLURL:             input.HTMLURL,
		UpdatedAt:           input.UpdatedAt,
		User:                convertUser(input.User),
		Path:                input.Path,
		CommitID:            input.CommitID,
		OriginalCommitID:    input.OriginalCommitID,
		Position:            input.Position,
		OriginalPosition:    input.OriginalPosition,
		DiffHunk:            input.DiffHunk,
		PullRequestReviewID: input.PullRequestReviewID,
		Resolver:            ForgejoReviewCommentResolverField{Present: input.Resolver.Present},
	}
	if input.Resolver.Value != nil {
		resolver := convertUser(*input.Resolver.Value)
		comment.Resolver.Value = &resolver
	}
	return comment
}

func convertUser(input forgejoUser) Identity { return Identity{Login: input.Login, ID: input.ID} }

func convertUsers(input []forgejoUser) []Identity {
	users := make([]Identity, 0, len(input))
	for _, user := range input {
		users = append(users, convertUser(user))
	}
	return users
}

func convertLabels(input []forgejoLabel) []Label {
	labels := make([]Label, 0, len(input))
	for _, label := range input {
		labels = append(labels, Label{ID: label.ID, Name: label.Name})
	}
	return labels
}

func convertBranch(input forgejoBranch) BranchRef {
	name := input.Name
	if name == "" {
		name = input.Ref
	}
	return BranchRef{Name: name, SHA: input.SHA}
}

func hasAllLabelNames(labels []Label, required []string) bool {
	if len(required) == 0 {
		return true
	}
	names := make(map[string]struct{}, len(labels))
	for _, label := range labels {
		name := strings.TrimSpace(label.Name)
		if name == "" {
			continue
		}
		names[strings.ToLower(name)] = struct{}{}
	}
	for _, label := range required {
		name := strings.ToLower(strings.TrimSpace(label))
		if name == "" {
			continue
		}
		if _, ok := names[name]; !ok {
			return false
		}
	}
	return true
}
