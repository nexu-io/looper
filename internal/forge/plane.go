package forge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/config"
)

const (
	defaultPlaneTimeout       = 30 * time.Second
	defaultPlaneBaseURL       = "https://plane.powerformer.net/api/v1"
	maxPlaneResponseBodyBytes = 8 << 20
	planeWorkItemPageSize     = 100
	planeMaxWorkItemPages     = 1000
	// planeUserAgent overrides Go's default net/http User-Agent, which Cloudflare's
	// WAF in front of Plane blocks outright. Every request MUST set a custom UA.
	planeUserAgent = "looper-forge/1.0 (+https://github.com/nexu-io/looper)"
)

// PlaneClient is a task-source provider backed by a Plane project. It reads
// work-items (issues), labels, comments, and assignees from Plane's REST API.
// It deliberately does NOT implement pull-request / diff / review operations:
// those belong to the project's GitHub code repo and are handled by the GitHub
// path in the scheduler adapters (see internal/runtime/scheduler.go).
type PlaneClient struct {
	baseURL    *url.URL
	webBaseURL string
	token      string
	userAgent  string
	workspace  string
	projectID  string
	httpClient *http.Client
	// repo carries the plane provider id/kind plus the GitHub code repo where
	// pull requests are opened (Repo == owner/name on GitHub).
	repo RepositoryRef
}

type PlaneOption func(*PlaneClient)

// WithPlaneHTTPClient injects an *http.Client (used by tests to avoid network).
func WithPlaneHTTPClient(client *http.Client) PlaneOption {
	return func(plane *PlaneClient) {
		if client != nil {
			plane.httpClient = client
		}
	}
}

// WithPlaneUserAgent overrides the default User-Agent header.
func WithPlaneUserAgent(userAgent string) PlaneOption {
	return func(plane *PlaneClient) {
		if strings.TrimSpace(userAgent) != "" {
			plane.userAgent = userAgent
		}
	}
}

// WithPlaneTimeout overrides the HTTP client timeout.
func WithPlaneTimeout(timeout time.Duration) PlaneOption {
	return func(plane *PlaneClient) {
		if timeout > 0 && plane.httpClient != nil {
			plane.httpClient.Timeout = timeout
		}
	}
}

// NewPlaneClient builds a Plane task-source client. codeRepo (ref.Repo) is the
// GitHub repository (owner/name) where pull requests are opened for work done on
// these work-items; it is retained only for Repository() reporting.
func NewPlaneClient(ref RepositoryRef, workspace, projectID, token string, options ...PlaneOption) (*PlaneClient, error) {
	baseURL, err := parsePlaneBaseURL(ref.BaseURL)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(ref.ProviderID) == "" {
		return nil, fmt.Errorf("plane client: provider id is required")
	}
	if strings.TrimSpace(workspace) == "" {
		return nil, fmt.Errorf("plane client: workspace is required")
	}
	if strings.TrimSpace(projectID) == "" {
		return nil, fmt.Errorf("plane client: projectId is required")
	}
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("plane client: token is required")
	}
	client := &PlaneClient{
		baseURL:    baseURL,
		webBaseURL: planeWebBaseURL(baseURL),
		token:      token,
		userAgent:  planeUserAgent,
		workspace:  strings.TrimSpace(workspace),
		projectID:  strings.TrimSpace(projectID),
		httpClient: &http.Client{Timeout: defaultPlaneTimeout},
		repo: RepositoryRef{
			ProviderID: strings.TrimSpace(ref.ProviderID),
			Kind:       ProviderKindPlane,
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
		client.httpClient = &http.Client{Timeout: defaultPlaneTimeout}
	}
	if client.httpClient.Timeout == 0 {
		client.httpClient.Timeout = defaultPlaneTimeout
	}
	return client, nil
}

// NewPlaneClientFromConfig builds a Plane client from a provider config. codeRepo
// is the project's GitHub code repo (project.Repo). The Plane API key is read
// from the env var named by provider.TokenEnv.
func NewPlaneClientFromConfig(provider config.ProviderConfig, codeRepo string, options ...PlaneOption) (*PlaneClient, error) {
	if provider.Kind != config.ProviderKindPlane {
		return nil, fmt.Errorf("plane client: provider %q kind = %q, want plane", provider.ID, provider.Kind)
	}
	if provider.TokenEnv == nil || strings.TrimSpace(*provider.TokenEnv) == "" {
		return nil, fmt.Errorf("plane client: provider %q tokenEnv is required", provider.ID)
	}
	token := os.Getenv(strings.TrimSpace(*provider.TokenEnv))
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("plane client: environment variable %s is required", strings.TrimSpace(*provider.TokenEnv))
	}
	workspace := ""
	if provider.Workspace != nil {
		workspace = strings.TrimSpace(*provider.Workspace)
	}
	projectID := ""
	if provider.ProjectID != nil {
		projectID = strings.TrimSpace(*provider.ProjectID)
	}
	baseURL := strings.TrimSpace(provider.BaseURL)
	if baseURL == "" {
		baseURL = defaultPlaneBaseURL
	}
	return NewPlaneClient(RepositoryRef{ProviderID: provider.ID, Kind: ProviderKindPlane, BaseURL: baseURL, Repo: codeRepo}, workspace, projectID, token, options...)
}

func (plane *PlaneClient) Kind() ProviderKind { return ProviderKindPlane }

func (plane *PlaneClient) Repository() RepositoryRef { return plane.repo }

func (plane *PlaneClient) Capabilities() Capabilities {
	capabilities, _ := StaticCapabilities(ProviderKindPlane)
	return capabilities
}

// Workspace and ProjectID expose the Plane coordinates the client reads from.
func (plane *PlaneClient) Workspace() string { return plane.workspace }
func (plane *PlaneClient) ProjectID() string { return plane.projectID }

func (plane *PlaneClient) CurrentUser(ctx context.Context) (Identity, error) {
	var user planeUser
	if err := plane.do(ctx, http.MethodGet, "users/me", nil, nil, &user); err != nil {
		return Identity{}, err
	}
	return Identity{Login: user.login()}, nil
}

// ListOpenIssues returns Plane work-items mapped onto looper's Issue type. When
// input.Labels is non-empty the results are filtered client-side to those that
// carry ALL requested label names (label UUIDs are resolved to names first).
func (plane *PlaneClient) ListOpenIssues(ctx context.Context, input ListIssuesInput) ([]Issue, error) {
	labelNames, err := plane.labelNamesByID(ctx)
	if err != nil {
		return nil, err
	}
	required := normalizedLabelSet(input.Labels)
	assignee := strings.TrimSpace(input.Assignee)
	// When filtering by label/assignee, the match may sit beyond the first
	// `Limit` work-items, so fetch ALL pages and let Limit cap the FILTERED
	// output below — not the pre-filter fetch (which would silently drop matches).
	fetchLimit := input.Limit
	if len(required) > 0 || assignee != "" {
		fetchLimit = 0
	}
	workItems, err := plane.listWorkItems(ctx, fetchLimit)
	if err != nil {
		return nil, err
	}
	issues := make([]Issue, 0, len(workItems))
	for _, item := range workItems {
		issue := plane.convertWorkItem(item, labelNames)
		if !issueHasAllLabels(issue.Labels, required) {
			continue
		}
		if assignee != "" && !workItemHasAssignee(item, assignee) {
			continue
		}
		issues = append(issues, issue)
		if input.Limit > 0 && len(issues) >= input.Limit {
			break
		}
	}
	return issues, nil
}

// ViewIssue resolves a work-item by its per-project sequence_id (looper's Issue
// number) and maps it onto the Issue type.
func (plane *PlaneClient) ViewIssue(ctx context.Context, number int64) (Issue, error) {
	item, err := plane.resolveWorkItem(ctx, number)
	if err != nil {
		return Issue{}, err
	}
	labelNames, err := plane.labelNamesByID(ctx)
	if err != nil {
		return Issue{}, err
	}
	return plane.convertWorkItem(item, labelNames), nil
}

// AddIssueLabels attaches label names to a work-item, creating any that do not
// yet exist. Plane PATCH replaces the whole label set, so the current labels are
// read and merged (never clobbered). Returns the resulting label set by name.
func (plane *PlaneClient) AddIssueLabels(ctx context.Context, issueNumber int64, labels []string) ([]Label, error) {
	item, err := plane.resolveWorkItem(ctx, issueNumber)
	if err != nil {
		return nil, err
	}
	nameToID, idToName, err := plane.labelIndex(ctx)
	if err != nil {
		return nil, err
	}
	next := append([]string(nil), item.Labels...)
	seen := make(map[string]struct{}, len(next))
	for _, id := range next {
		seen[id] = struct{}{}
	}
	for _, name := range labels {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		id, ok := nameToID[strings.ToLower(name)]
		if !ok {
			created, err := plane.createLabel(ctx, name)
			if err != nil {
				return nil, err
			}
			id = created.ID
			nameToID[strings.ToLower(created.Name)] = created.ID
			idToName[created.ID] = created.Name
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		next = append(next, id)
	}
	if err := plane.patchWorkItem(ctx, item.ID, map[string]any{"labels": next}); err != nil {
		return nil, err
	}
	result := make([]Label, 0, len(next))
	for _, id := range next {
		result = append(result, Label{Name: idToName[id]})
	}
	return result, nil
}

// RemoveIssueLabel detaches a single label (by name) from a work-item.
func (plane *PlaneClient) RemoveIssueLabel(ctx context.Context, issueNumber int64, label string) error {
	item, err := plane.resolveWorkItem(ctx, issueNumber)
	if err != nil {
		return err
	}
	nameToID, _, err := plane.labelIndex(ctx)
	if err != nil {
		return err
	}
	target, ok := nameToID[strings.ToLower(strings.TrimSpace(label))]
	if !ok {
		// Nothing to remove; treat as success (idempotent).
		return nil
	}
	next := make([]string, 0, len(item.Labels))
	for _, id := range item.Labels {
		if id == target {
			continue
		}
		next = append(next, id)
	}
	return plane.patchWorkItem(ctx, item.ID, map[string]any{"labels": next})
}

// AddIssueAssignees merges assignee ids (Plane user UUIDs) onto a work-item.
func (plane *PlaneClient) AddIssueAssignees(ctx context.Context, issueNumber int64, assignees []string) error {
	item, err := plane.resolveWorkItem(ctx, issueNumber)
	if err != nil {
		return err
	}
	next := append([]string(nil), item.Assignees...)
	seen := make(map[string]struct{}, len(next))
	for _, id := range next {
		seen[id] = struct{}{}
	}
	for _, id := range assignees {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		next = append(next, id)
	}
	return plane.patchWorkItem(ctx, item.ID, map[string]any{"assignees": next})
}

// CreateIssueComment posts an HTML comment onto a work-item. Plane comment ids
// are UUIDs and cannot be represented in looper's int64 Comment.ID, so the
// returned Comment.ID is 0 and HTMLURL points at the work-item web page.
func (plane *PlaneClient) CreateIssueComment(ctx context.Context, input CreateCommentInput) (Comment, error) {
	item, err := plane.resolveWorkItem(ctx, input.IssueNumber)
	if err != nil {
		return Comment{}, err
	}
	path := plane.projectPath("work-items", item.ID, "comments")
	if err := plane.do(ctx, http.MethodPost, path, nil, map[string]string{"comment_html": input.Body}, nil); err != nil {
		return Comment{}, err
	}
	return Comment{Body: input.Body, HTMLURL: plane.workItemWebURL(item.ID)}, nil
}

// ListIssueComments returns the work-item comments (oldest first). Comment.ID is
// 0 because Plane comment ids are UUIDs (see CreateIssueComment).
func (plane *PlaneClient) ListIssueComments(ctx context.Context, issueNumber int64) ([]Comment, error) {
	item, err := plane.resolveWorkItem(ctx, issueNumber)
	if err != nil {
		return nil, err
	}
	path := plane.projectPath("work-items", item.ID, "comments")
	var page planeCommentPage
	if err := plane.do(ctx, http.MethodGet, path, nil, nil, &page); err != nil {
		return nil, err
	}
	comments := make([]Comment, 0, len(page.results()))
	for _, comment := range page.results() {
		body := comment.CommentStripped
		if strings.TrimSpace(body) == "" {
			body = stripHTMLTags(comment.CommentHTML)
		}
		comments = append(comments, Comment{Body: body, UpdatedAt: comment.CreatedAt, User: Identity{Login: comment.Actor}, HTMLURL: plane.workItemWebURL(item.ID)})
	}
	return comments, nil
}

// resolveWorkItem finds the full work-item (including its UUID, needed for
// mutations) by its per-project sequence_id.
func (plane *PlaneClient) resolveWorkItem(ctx context.Context, number int64) (planeWorkItem, error) {
	items, err := plane.listWorkItems(ctx, 0)
	if err != nil {
		return planeWorkItem{}, err
	}
	for _, item := range items {
		if item.SequenceID == number {
			return item, nil
		}
	}
	return planeWorkItem{}, fmt.Errorf("plane client: work-item with sequence_id %d not found in project %s", number, plane.projectID)
}

// listWorkItems fetches all work-items in the project following Plane's cursor
// pagination. limit, when > 0, stops paging once that many items are collected.
func (plane *PlaneClient) listWorkItems(ctx context.Context, limit int) ([]planeWorkItem, error) {
	out := make([]planeWorkItem, 0, planeWorkItemPageSize)
	cursor := ""
	for page := 0; page < planeMaxWorkItemPages; page++ {
		query := url.Values{"per_page": {fmt.Sprintf("%d", planeWorkItemPageSize)}}
		if cursor != "" {
			query.Set("cursor", cursor)
		}
		var body planeWorkItemPage
		if err := plane.do(ctx, http.MethodGet, plane.projectPath("work-items"), query, nil, &body); err != nil {
			return nil, err
		}
		out = append(out, body.results()...)
		if limit > 0 && len(out) >= limit {
			return out, nil
		}
		if !body.NextPageResults || strings.TrimSpace(body.NextCursor) == "" {
			return out, nil
		}
		cursor = body.NextCursor
	}
	return out, nil
}

func (plane *PlaneClient) labelNamesByID(ctx context.Context) (map[string]string, error) {
	_, idToName, err := plane.labelIndex(ctx)
	return idToName, err
}

// labelIndex returns two maps: lowercased-name -> id, and id -> canonical name.
func (plane *PlaneClient) labelIndex(ctx context.Context) (map[string]string, map[string]string, error) {
	var page planeLabelPage
	if err := plane.do(ctx, http.MethodGet, plane.projectPath("labels"), nil, nil, &page); err != nil {
		return nil, nil, err
	}
	labels := page.results()
	nameToID := make(map[string]string, len(labels))
	idToName := make(map[string]string, len(labels))
	for _, label := range labels {
		nameToID[strings.ToLower(strings.TrimSpace(label.Name))] = label.ID
		idToName[label.ID] = label.Name
	}
	return nameToID, idToName, nil
}

func (plane *PlaneClient) createLabel(ctx context.Context, name string) (planeLabel, error) {
	var created planeLabel
	if err := plane.do(ctx, http.MethodPost, plane.projectPath("labels"), nil, map[string]string{"name": name, "color": "#f59e0b"}, &created); err != nil {
		return planeLabel{}, err
	}
	return created, nil
}

func (plane *PlaneClient) patchWorkItem(ctx context.Context, workItemID string, payload map[string]any) error {
	return plane.do(ctx, http.MethodPatch, plane.projectPath("work-items", workItemID), nil, payload, nil)
}

func (plane *PlaneClient) convertWorkItem(item planeWorkItem, labelNames map[string]string) Issue {
	labels := make([]Label, 0, len(item.Labels))
	for _, id := range item.Labels {
		name := labelNames[id]
		if strings.TrimSpace(name) == "" {
			name = id
		}
		labels = append(labels, Label{Name: name})
	}
	assignees := make([]Identity, 0, len(item.Assignees))
	for _, id := range item.Assignees {
		assignees = append(assignees, Identity{Login: id})
	}
	return Issue{
		Number:    item.SequenceID,
		Title:     item.Name,
		Body:      stripHTMLTags(item.DescriptionHTML),
		State:     "open",
		HTMLURL:   plane.workItemWebURL(item.ID),
		UpdatedAt: item.UpdatedAt,
		Labels:    labels,
		Assignees: assignees,
	}
}

func (plane *PlaneClient) workspacePath(parts ...string) string {
	return strings.Join(append([]string{"workspaces", plane.workspace}, parts...), "/")
}

func (plane *PlaneClient) projectPath(parts ...string) string {
	return plane.workspacePath(append([]string{"projects", plane.projectID}, parts...)...)
}

func (plane *PlaneClient) workItemWebURL(workItemID string) string {
	if strings.TrimSpace(workItemID) == "" {
		return ""
	}
	return fmt.Sprintf("%s/%s/projects/%s/issues/%s", plane.webBaseURL, plane.workspace, plane.projectID, workItemID)
}

func (plane *PlaneClient) do(ctx context.Context, method, path string, query url.Values, payload any, out any) error {
	apiURL, err := plane.apiURL(path)
	if err != nil {
		return err
	}
	if len(query) > 0 {
		apiURL.RawQuery = query.Encode()
	}
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("plane API encode %s %s: %w", method, path, err)
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, apiURL.String(), body)
	if err != nil {
		return fmt.Errorf("plane API build request %s %s: %w", method, path, err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("X-API-Key", plane.token)
	// Cloudflare WAF blocks Go's default User-Agent; always send a custom one.
	request.Header.Set("User-Agent", plane.userAgent)
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := plane.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("plane API %s %s failed: %w", method, path, err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maxPlaneResponseBodyBytes+1))
	if err != nil {
		return fmt.Errorf("plane API read response %s %s: %w", method, path, err)
	}
	if len(responseBody) > maxPlaneResponseBodyBytes {
		return fmt.Errorf("plane API %s %s response exceeds %d bytes", method, path, maxPlaneResponseBodyBytes)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("plane API %s %s returned HTTP %d: %s", method, path, response.StatusCode, sanitizePlaneErrorBody(responseBody, plane.token))
	}
	if out == nil || len(bytes.TrimSpace(responseBody)) == 0 {
		return nil
	}
	if err := json.Unmarshal(responseBody, out); err != nil {
		return fmt.Errorf("plane API decode %s %s: %w", method, path, err)
	}
	return nil
}

// apiURL builds an absolute API URL. Plane's REST base already includes /api/v1
// (e.g. https://plane.powerformer.net/api/v1) so path is appended directly.
func (plane *PlaneClient) apiURL(path string) (*url.URL, error) {
	cleanPath := strings.TrimLeft(path, "/")
	apiURL := *plane.baseURL
	apiURL.Path = strings.TrimRight(plane.baseURL.Path, "/") + "/" + cleanPath + "/"
	return &apiURL, nil
}

func parsePlaneBaseURL(value string) (*url.URL, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		trimmed = defaultPlaneBaseURL
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, fmt.Errorf("plane client: baseURL must be an absolute http(s) URL")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	return parsed, nil
}

// planeWebBaseURL derives the human web host by stripping a trailing /api/vN.
func planeWebBaseURL(baseURL *url.URL) string {
	web := *baseURL
	web.Path = planeAPIVersionSuffix.ReplaceAllString(web.Path, "")
	return strings.TrimRight(web.String(), "/")
}

func sanitizePlaneErrorBody(body []byte, token string) string {
	message := strings.TrimSpace(string(body))
	if message == "" {
		message = http.StatusText(http.StatusInternalServerError)
	}
	if len(message) > 500 {
		message = message[:500]
	}
	if strings.TrimSpace(token) != "" {
		message = strings.ReplaceAll(message, token, "[REDACTED]")
	}
	return message
}

var (
	planeAPIVersionSuffix = regexp.MustCompile(`/api/v\d+/?$`)
	planeHTMLTagPattern   = regexp.MustCompile(`</?[A-Za-z][^>]*>`)
	planeBlockCloseTag    = regexp.MustCompile(`(?i)</(p|div|br|li|h[1-6]|tr)\s*>`)
)

// stripHTMLTags converts Plane's description_html / comment_html into plain text:
// block-closing tags become newlines, all other tags are dropped, and HTML
// entities are unescaped. It is intentionally conservative (no sanitizer dep).
func stripHTMLTags(input string) string {
	if strings.TrimSpace(input) == "" {
		return ""
	}
	text := planeBlockCloseTag.ReplaceAllString(input, "\n")
	text = planeHTMLTagPattern.ReplaceAllString(text, "")
	text = html.UnescapeString(text)
	// Collapse runs of 3+ newlines to at most 2 and trim surrounding whitespace.
	lines := strings.Split(text, "\n")
	trimmed := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed = append(trimmed, strings.TrimRight(line, " \t\r"))
	}
	return strings.TrimSpace(strings.Join(trimmed, "\n"))
}

func normalizedLabelSet(labels []string) map[string]struct{} {
	set := make(map[string]struct{}, len(labels))
	for _, label := range labels {
		label = strings.ToLower(strings.TrimSpace(label))
		if label == "" {
			continue
		}
		set[label] = struct{}{}
	}
	return set
}

func issueHasAllLabels(labels []Label, required map[string]struct{}) bool {
	if len(required) == 0 {
		return true
	}
	present := make(map[string]struct{}, len(labels))
	for _, label := range labels {
		name := strings.ToLower(strings.TrimSpace(label.Name))
		if name == "" {
			continue
		}
		present[name] = struct{}{}
	}
	for name := range required {
		if _, ok := present[name]; !ok {
			return false
		}
	}
	return true
}

func workItemHasAssignee(item planeWorkItem, assignee string) bool {
	for _, id := range item.Assignees {
		if strings.EqualFold(strings.TrimSpace(id), assignee) {
			return true
		}
	}
	return false
}

type planeWorkItem struct {
	ID              string   `json:"id"`
	SequenceID      int64    `json:"sequence_id"`
	Name            string   `json:"name"`
	DescriptionHTML string   `json:"description_html"`
	Labels          []string `json:"labels"`
	Assignees       []string `json:"assignees"`
	State           string   `json:"state"`
	UpdatedAt       string   `json:"updated_at"`
}

type planeLabel struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type planeUser struct {
	ID          string `json:"id"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
	FirstName   string `json:"first_name"`
}

func (u planeUser) login() string {
	for _, candidate := range []string{u.DisplayName, u.Email, u.FirstName, u.ID} {
		if strings.TrimSpace(candidate) != "" {
			return strings.TrimSpace(candidate)
		}
	}
	return ""
}

// planeWorkItemPage decodes Plane's cursor-paginated work-item list. Plane
// returns either a bare JSON array or an object with results/next_cursor, so
// UnmarshalJSON accepts both shapes.
type planeWorkItemPage struct {
	Results         []planeWorkItem `json:"results"`
	NextCursor      string          `json:"next_cursor"`
	NextPageResults bool            `json:"next_page_results"`
}

func (p planeWorkItemPage) results() []planeWorkItem { return p.Results }

func (p *planeWorkItemPage) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		return json.Unmarshal(trimmed, &p.Results)
	}
	type alias planeWorkItemPage
	return json.Unmarshal(trimmed, (*alias)(p))
}

type planeLabelPage struct {
	Results []planeLabel `json:"results"`
}

func (p planeLabelPage) results() []planeLabel { return p.Results }

func (p *planeLabelPage) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		return json.Unmarshal(trimmed, &p.Results)
	}
	type alias planeLabelPage
	return json.Unmarshal(trimmed, (*alias)(p))
}

type planeComment struct {
	ID              string `json:"id"`
	CommentHTML     string `json:"comment_html"`
	CommentStripped string `json:"comment_stripped"`
	Actor           string `json:"actor"`
	CreatedAt       string `json:"created_at"`
}

type planeCommentPage struct {
	Results []planeComment `json:"results"`
}

func (p planeCommentPage) results() []planeComment { return p.Results }

func (p *planeCommentPage) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		return json.Unmarshal(trimmed, &p.Results)
	}
	type alias planeCommentPage
	return json.Unmarshal(trimmed, (*alias)(p))
}
