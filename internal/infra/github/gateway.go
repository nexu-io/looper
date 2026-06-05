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
	"sync"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/diffanchor"
	"github.com/nexu-io/looper/internal/disclosure"
	"github.com/nexu-io/looper/internal/infra/shell"
	"github.com/nexu-io/looper/internal/infra/specpr"
	"github.com/nexu-io/looper/internal/storage"
)

const javaScriptISOStringLayout = "2006-01-02T15:04:05.000Z"

const (
	defaultGhCommandTimeout = 60 * time.Second
	prListGhCommandTimeout  = 15 * time.Second
	prDiffGhCommandTimeout  = 180 * time.Second
)

var (
	prListJSONFields = []string{"number", "title", "url", "state", "updatedAt", "isDraft", "reviewDecision", "labels", "headRefName", "baseRefName", "headRefOid", "baseRefOid", "author", "reviewRequests", "reviews", "mergeStateStatus"}
	prViewJSONFields = []string{"number", "title", "body", "url", "state", "createdAt", "updatedAt", "closedAt", "isDraft", "reviewDecision", "labels", "headRefName", "baseRefName", "headRefOid", "baseRefOid", "author", "reviewRequests", "comments", "reviews", "statusCheckRollup", "mergeStateStatus"}
)

var prNumberURLPattern = regexp.MustCompile(`/pull/(\d+)(?:/|$)`)

var ErrDiffTooLarge = errors.New("github pull request diff is too large")

type Options struct {
	GHPath                 string
	CWD                    string
	Now                    func() time.Time
	DiscoveryCacheTTL      time.Duration
	GHRun                  func(context.Context, shell.Options) (shell.Result, error)
	ReviewSubmitDiagnostic func(event string, fields map[string]any)
}

type Gateway struct {
	ghPath                 string
	cwd                    string
	now                    func() time.Time
	discoveryCacheTTL      time.Duration
	discoveryCacheMu       sync.Mutex
	discoveryPRCache       map[string]discoveryPullRequestListCacheEntry
	discoveryReviewPRCache map[string]discoveryPullRequestListCacheEntry
	discoveryIssueCache    map[string]discoveryIssueListCacheEntry
	ghRun                  func(context.Context, shell.Options) (shell.Result, error)
	reviewSubmitDiagnostic func(event string, fields map[string]any)
}

type discoveryPullRequestListCacheEntry struct {
	expiresAt time.Time
	items     []PullRequestSummary
}

type discoveryIssueListCacheEntry struct {
	expiresAt time.Time
	items     []IssueSummary
}

type PullRequestSummary struct {
	Number             int64
	Title              string
	URL                string
	State              string
	UpdatedAt          string
	IsDraft            bool
	ReviewDecision     string
	Labels             []string
	HeadRefName        string
	BaseRefName        string
	HeadSHA            string
	BaseSHA            string
	HasConflicts       bool
	Author             string
	AuthorAssociation  string
	ReviewRequests     []string
	ReviewRequestUsers []GitHubUser
	Reviews            []map[string]any
}

type PullRequestDetail struct {
	Number             int64
	Title              string
	Body               string
	URL                string
	State              string
	CreatedAt          string
	UpdatedAt          string
	ClosedAt           string
	IsDraft            bool
	ReviewDecision     string
	Labels             []string
	HeadRefName        string
	BaseRefName        string
	HeadSHA            string
	BaseSHA            string
	Author             string
	AuthorAssociation  string
	CommentCount       int
	ReviewRequests     []string
	ReviewRequestUsers []GitHubUser
	HasConflicts       bool
	Comments           []map[string]any
	IssueComments      []CommentInfo
	Reviews            []map[string]any
	Checks             []map[string]any
	Mergeable          *bool
	MergeableState     string
	MergedAt           string
	AutoMerge          *PullRequestAutoMerge
}

type PullRequestAutoMerge struct {
	EnabledBy   string
	MergeMethod string
}

type PullRequestCheckRuns struct {
	TotalCount int
	CheckRuns  []PullRequestCheckRun
	Statuses   []PullRequestStatus
}

type PullRequestCheckRun struct {
	Name       string
	Status     string
	Conclusion string
}

type PullRequestStatus struct {
	Context string
	State   string
}

type CommentInfo struct {
	ID                int64
	Author            string
	AuthorAssociation string
	Body              string
	CreatedAt         string
	UpdatedAt         string
	URL               string
}

type CurrentUserIdentity struct {
	Login     string
	NumericID int64
}

type IssueTimelineInput struct {
	Repo        string
	IssueNumber int64
	CWD         string
}
type IssueReactionInput struct {
	Repo        string
	IssueNumber int64
	CommentID   int64
	CWD         string
}
type CreateIssueReactionInput struct {
	Repo        string
	IssueNumber int64
	CommentID   int64
	Content     string
	CWD         string
}
type RepositoryPermissionInput struct {
	Repo string
	User string
	CWD  string
}
type ListIssueBlockedByInput struct {
	Repo        string
	IssueNumber int64
	CWD         string
}
type IssueDependency struct {
	Number int64
	Repo   string
}
type IssueState struct {
	State       string
	StateReason string
}
type LinkedPullRequestsInput struct {
	Repo        string
	IssueNumber int64
	CWD         string
}
type PullRequestReviewStateInput struct {
	Repo     string
	PRNumber int64
	CWD      string
}

type IssueReaction struct {
	ID        int64
	Content   string
	UserLogin string
}
type LinkedPullRequest struct {
	Number         int64
	State          string
	Merged         bool
	MergedAt       string
	MergeCommitSHA string
}
type PullRequestReviewState struct {
	RequestedReviewers  []string
	LatestReviewPerUser map[string]string
	LastReviewAt        string
}

type GitHubUser struct {
	Login string
	ID    int64
}

type PullRequestHeadAndAuthor struct {
	HeadSHA string
	Author  string
}

type RepositorySettingsInput struct {
	Repo string
	CWD  string
}

type RepositorySettings struct {
	AllowSquashMerge bool
	AllowMergeCommit bool
	AllowRebaseMerge bool
	AllowAutoMerge   bool
}

type BranchProtectionInput struct {
	Repo   string
	Branch string
	CWD    string
}

type BranchProtection struct {
	Enabled           bool
	HasRequiredChecks bool
	RequiredChecks    []string
}

type IssueSummary struct {
	Number            int64
	Title             string
	Body              string
	URL               string
	State             string
	UpdatedAt         string
	Author            string
	AuthorAssociation string
	Assignees         []string
	AssigneeUsers     []GitHubUser
	Labels            []string
	IsPullRequest     bool
}

type IssueDetail struct {
	Number            int64
	Title             string
	Body              string
	URL               string
	State             string
	StateReason       string
	CreatedAt         string
	UpdatedAt         string
	ClosedAt          string
	Author            string
	AuthorAssociation string
	Assignees         []string
	AssigneeUsers     []GitHubUser
	Labels            []string
	IsPullRequest     bool
	CommentCount      int
	Comments          []CommentInfo
}

type IssueRepository struct {
	Name     string
	FullName string
	URL      string
	HTMLURL  string
}

type DependencyIssue struct {
	ID            int64
	Number        int64
	Title         string
	URL           string
	HTMLURL       string
	RepositoryURL string
	State         string
	StateReason   string
	Repository    IssueRepository
}

type IssueCommentInput struct {
	Repo        string
	IssueNumber int64
	Body        string
	CWD         string
}

type IssueAssigneesInput struct {
	Repo        string
	IssueNumber int64
	Assignees   []string
	CWD         string
}

type IssueLabelsInput struct {
	Repo        string
	IssueNumber int64
	Labels      []string
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

type DeleteIssueCommentInput struct {
	Repo      string
	CommentID int64
	CWD       string
}

type CloseIssueInput struct {
	Repo        string
	IssueNumber int64
	StateReason string
	CWD         string
}

type ClosePullRequestInput struct {
	Repo     string
	PRNumber int64
	CWD      string
}

type EnableAutoMergeInput struct {
	Repo     string
	PRNumber int64
	Strategy config.ReviewerAutoMergeStrategy
	HeadSHA  string
	CWD      string
}

type PullRequestCheckRunsInput struct {
	Repo string
	Ref  string
	CWD  string
}

type SubmitReviewInput struct {
	Repo       string
	PRNumber   int64
	Event      string
	Body       string
	CommitID   string
	Comments   []ReviewComment
	Anchors    *diffanchor.Index
	Disclosure config.DisclosureConfig
	CWD        string
}

type ReviewComment struct {
	Body            string
	Path            string
	Line            int64
	Side            string
	StartLine       int64
	StartSide       string
	DiagnosticIndex int
}

type VerifyReviewMarkerInput struct {
	Repo                string
	PRNumber            int64
	Marker              string
	AllowedReviewEvents []string
	AuthorLogin         string
	AllowCleanComment   bool
	CWD                 string
}

type ReviewMarkerResult struct {
	Found               bool
	Outcome             string
	Event               string
	AuthorLogin         string
	Body                string
	ReviewID            string
	InlineCommentBodies []string
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

type CompareBranchesInput struct {
	Repo       string
	BaseBranch string
	HeadBranch string
	CWD        string
}

type CompareBranchesResult struct {
	AheadBy      int
	BehindBy     int
	Status       string
	TotalCommits int
}

type UpdatePullRequestTitleInput struct {
	Repo     string
	PRNumber int64
	Title    string
	CWD      string
}

type UpdatePullRequestBodyInput struct {
	Repo     string
	PRNumber int64
	Body     string
	CWD      string
}

type ListOpenPullRequestsInput struct {
	Repo        string
	CWD         string
	Limit       int
	Label       string
	Labels      []string
	Author      string
	BaseRefName string
	Timeout     time.Duration
}

type ListReviewRequestedPullRequestsInput struct {
	Repo     string
	CWD      string
	Limit    int
	Reviewer string
	Timeout  time.Duration
}

type ListOpenIssuesInput struct {
	Repo     string
	CWD      string
	Limit    int
	Assignee string
	Label    string
	Labels   []string
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

type ListReviewThreadsInput struct {
	Repo     string
	PRNumber int64
	CWD      string
	Limit    int
}

type ViewReviewThreadInput struct {
	ThreadID string
	CWD      string
}

type ReviewThread struct {
	ID         string
	IsResolved bool
	Path       string
	Line       int64
	URL        string
	Comments   []ReviewThreadComment
}

type ReviewThreadComment struct {
	ID                string
	Body              string
	Author            string
	AuthorAssociation string
	CreatedAt         string
	UpdatedAt         string
	Path              string
	Line              int64
	OriginalCommitOID string
	CommitOID         string
	URL               string
}

type AddReviewThreadReplyInput struct {
	Repo     string
	ThreadID string
	Body     string
	CWD      string
}

type CompareCommitsInput struct {
	Repo string
	Base string
	Head string
	CWD  string
}

type CompareCommitsResult struct {
	Status string
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

type InitializeLabelsInput struct {
	Repo   string
	CWD    string
	DryRun bool
}

type LabelDefinition struct {
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description"`
}

type LabelInitResult struct {
	Repo    string           `json:"repo"`
	DryRun  bool             `json:"dryRun"`
	Labels  []LabelInitItem  `json:"labels"`
	Summary LabelInitSummary `json:"summary"`
}

type LabelInitItem struct {
	Name        string `json:"name"`
	Status      string `json:"status"`
	Color       string `json:"color"`
	Description string `json:"description"`
	Error       string `json:"error,omitempty"`
}

type LabelInitSummary struct {
	Created int `json:"created"`
	Updated int `json:"updated"`
	Skipped int `json:"skipped"`
	Failed  int `json:"failed"`
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
	return &Gateway{ghPath: ghPath, cwd: options.CWD, now: now, discoveryCacheTTL: options.DiscoveryCacheTTL, discoveryPRCache: map[string]discoveryPullRequestListCacheEntry{}, discoveryReviewPRCache: map[string]discoveryPullRequestListCacheEntry{}, discoveryIssueCache: map[string]discoveryIssueListCacheEntry{}, ghRun: ghRun, reviewSubmitDiagnostic: options.ReviewSubmitDiagnostic}
}

func (g *Gateway) ListOpenPullRequests(ctx context.Context, input ListOpenPullRequestsInput) ([]PullRequestSummary, error) {
	if snapshot := discoverySnapshotFromContext(ctx); snapshot != nil {
		return snapshot.listOpenPullRequests(ctx, input)
	}
	return g.listOpenPullRequestsRaw(ctx, input)
}

func (g *Gateway) ListReviewRequestedPullRequests(ctx context.Context, input ListReviewRequestedPullRequestsInput) ([]PullRequestSummary, error) {
	if snapshot := discoverySnapshotFromContext(ctx); snapshot != nil {
		return snapshot.listReviewRequestedPullRequests(ctx, input)
	}
	return g.listReviewRequestedPullRequestsRaw(ctx, input)
}

func (g *Gateway) listReviewRequestedPullRequestsRaw(ctx context.Context, input ListReviewRequestedPullRequestsInput) ([]PullRequestSummary, error) {
	reviewer := normalizeGitHubLogin(input.Reviewer)
	if reviewer == "" {
		return nil, fmt.Errorf("review requester login is required")
	}
	query := `
query($searchQuery: String!, $first: Int!) {
  search(type: ISSUE, query: $searchQuery, first: $first) {
    nodes {
      ... on PullRequest {
        number
        title
        url
        state
        updatedAt
        isDraft
        reviewDecision
        labels(first: 20) { nodes { name } }
        headRefName
        baseRefName
        headRefOid
        baseRefOid
        mergeStateStatus
        author { login }
        reviews(last: 20) {
          nodes {
            state
            author { login }
            submittedAt
            updatedAt
          }
        }
      }
    }
  }
}`
	searchQuery := fmt.Sprintf("repo:%s is:pr is:open review-requested:%s", input.Repo, reviewer)
	timeout := input.Timeout
	if timeout <= 0 {
		timeout = defaultGhCommandTimeout
	}
	result, err := g.runGhWithTimeout(ctx, input.CWD, "", timeout, "api", "graphql", "-f", "query="+query, "-F", "searchQuery="+searchQuery, "-F", fmt.Sprintf("first=%d", defaultLimit(input.Limit)))
	if err != nil {
		return nil, err
	}
	row, err := decodeJSONObject(result.Stdout)
	if err != nil {
		return nil, err
	}
	nodes := digNodes(row, "data", "search")
	out := make([]PullRequestSummary, 0, len(nodes))
	for _, node := range nodes {
		number := asInt64(node["number"])
		if number <= 0 {
			continue
		}
		out = append(out, PullRequestSummary{
			Number:             number,
			Title:              asString(node["title"]),
			URL:                asString(node["url"]),
			State:              asString(node["state"]),
			UpdatedAt:          asString(node["updatedAt"]),
			IsDraft:            asBool(node["isDraft"]),
			ReviewDecision:     asString(node["reviewDecision"]),
			Labels:             extractLabelNamesFromConnection(node["labels"]),
			HeadRefName:        asString(node["headRefName"]),
			BaseRefName:        asString(node["baseRefName"]),
			HeadSHA:            asString(node["headRefOid"]),
			BaseSHA:            asString(node["baseRefOid"]),
			HasConflicts:       asString(node["mergeStateStatus"]) == "DIRTY",
			Author:             extractAuthor(node["author"]),
			ReviewRequests:     []string{reviewer},
			ReviewRequestUsers: []GitHubUser{{Login: reviewer}},
			Reviews:            extractReviewNodesFromConnection(node["reviews"]),
		})
	}
	return out, nil
}

func (g *Gateway) listOpenPullRequestsRaw(ctx context.Context, input ListOpenPullRequestsInput) ([]PullRequestSummary, error) {
	rows, err := g.listOpenPullRequestRows(ctx, input, prListJSONFields)
	if err != nil && IsInaccessibleReviewRequestReviewerError(err) {
		rows, err = g.listOpenPullRequestRows(ctx, input, withoutJSONField(prListJSONFields, "reviewRequests"))
	}
	if err != nil {
		return nil, err
	}
	out := make([]PullRequestSummary, 0, len(rows))
	for _, row := range rows {
		out = append(out, PullRequestSummary{
			Number:             asInt64(row["number"]),
			Title:              asString(row["title"]),
			URL:                asString(row["url"]),
			State:              asString(row["state"]),
			UpdatedAt:          asString(row["updatedAt"]),
			IsDraft:            asBool(row["isDraft"]),
			ReviewDecision:     asString(row["reviewDecision"]),
			Labels:             extractLabelNames(row["labels"]),
			HeadRefName:        asString(row["headRefName"]),
			BaseRefName:        asString(row["baseRefName"]),
			HeadSHA:            asString(row["headRefOid"]),
			BaseSHA:            asString(row["baseRefOid"]),
			HasConflicts:       asString(row["mergeStateStatus"]) == "DIRTY",
			Author:             extractAuthor(row["author"]),
			AuthorAssociation:  asString(row["authorAssociation"]),
			ReviewRequests:     extractReviewRequestLogins(row["reviewRequests"]),
			ReviewRequestUsers: extractReviewRequestUsers(row["reviewRequests"]),
			Reviews:            toObjectSlice(row["reviews"]),
		})
	}
	return out, nil
}

func (g *Gateway) listOpenPullRequestRows(ctx context.Context, input ListOpenPullRequestsInput, fields []string) ([]map[string]any, error) {
	args := []string{"pr", "list", "--repo", input.Repo, "--state", "open", "--limit", fmt.Sprintf("%d", defaultLimit(input.Limit))}
	labels := prListLabels(input)
	for _, label := range labels {
		args = append(args, "--label", label)
	}
	if strings.TrimSpace(input.Author) != "" {
		args = append(args, "--author", strings.TrimSpace(input.Author))
	}
	if strings.TrimSpace(input.BaseRefName) != "" {
		args = append(args, "--base", strings.TrimSpace(input.BaseRefName))
	}
	args = append(args, "--json", strings.Join(fields, ","))

	timeout := input.Timeout
	if timeout <= 0 {
		timeout = defaultGhCommandTimeout
	}
	result, err := g.runGhWithTimeout(ctx, input.CWD, "", timeout, args...)
	if err != nil {
		return nil, err
	}
	return decodeJSONArray(result.Stdout)
}

func prListLabels(input ListOpenPullRequestsInput) []string {
	labels := input.Labels
	if len(labels) == 0 && strings.TrimSpace(input.Label) != "" {
		labels = []string{input.Label}
	}
	result := []string{}
	seen := map[string]struct{}{}
	for _, label := range labels {
		label = strings.TrimSpace(label)
		if label == "" {
			continue
		}
		key := strings.ToLower(label)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, label)
	}
	return result
}

func (g *Gateway) GetPullRequestAuthor(ctx context.Context, input ViewPullRequestInput) (string, error) {
	result, err := g.runGh(ctx, input.CWD, "", "pr", "view", fmt.Sprintf("%d", input.PRNumber), "--repo", input.Repo, "--json", "author")
	if err != nil {
		return "", err
	}
	row, err := decodeJSONObject(result.Stdout)
	if err != nil {
		return "", err
	}
	return extractAuthor(row["author"]), nil
}

func (g *Gateway) GetPullRequestHeadAndAuthor(ctx context.Context, input ViewPullRequestInput) (PullRequestHeadAndAuthor, error) {
	result, err := g.runGh(ctx, input.CWD, "", "pr", "view", fmt.Sprintf("%d", input.PRNumber), "--repo", input.Repo, "--json", "headRefOid,author")
	if err != nil {
		return PullRequestHeadAndAuthor{}, err
	}
	row, err := decodeJSONObject(result.Stdout)
	if err != nil {
		return PullRequestHeadAndAuthor{}, err
	}
	return PullRequestHeadAndAuthor{HeadSHA: asString(row["headRefOid"]), Author: extractAuthor(row["author"])}, nil
}

func (g *Gateway) ListOpenIssues(ctx context.Context, input ListOpenIssuesInput) ([]IssueSummary, error) {
	if snapshot := discoverySnapshotFromContext(ctx); snapshot != nil {
		return snapshot.listOpenIssues(ctx, input)
	}
	return g.listOpenIssuesRaw(ctx, input)
}

func (g *Gateway) listOpenIssuesRaw(ctx context.Context, input ListOpenIssuesInput) ([]IssueSummary, error) {
	args := []string{"issue", "list", "--repo", input.Repo, "--state", "open", "--limit", fmt.Sprintf("%d", defaultLimit(input.Limit))}
	if strings.TrimSpace(input.Assignee) != "" {
		args = append(args, "--assignee", input.Assignee)
	}
	for _, label := range issueListLabels(input) {
		args = append(args, "--label", label)
	}
	args = append(args, "--json", strings.Join([]string{"number", "title", "body", "url", "state", "updatedAt", "author", "assignees", "labels"}, ","))

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
			Number:            asInt64(row["number"]),
			Title:             asString(row["title"]),
			Body:              asString(row["body"]),
			URL:               asString(row["url"]),
			State:             asString(row["state"]),
			UpdatedAt:         asString(row["updatedAt"]),
			Author:            extractAuthor(row["author"]),
			AuthorAssociation: asString(row["authorAssociation"]),
			Assignees:         extractActorLogins(row["assignees"]),
			AssigneeUsers:     extractActorUsers(row["assignees"]),
			Labels:            extractLabelNames(row["labels"]),
		})
	}
	return out, nil
}

func issueListLabels(input ListOpenIssuesInput) []string {
	labels := input.Labels
	if len(labels) == 0 && strings.TrimSpace(input.Label) != "" {
		labels = []string{input.Label}
	}
	result := []string{}
	seen := map[string]struct{}{}
	for _, label := range labels {
		label = strings.TrimSpace(label)
		if label == "" {
			continue
		}
		key := strings.ToLower(label)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, label)
	}
	return result
}

func (g *Gateway) ViewIssue(ctx context.Context, input ViewIssueInput) (IssueDetail, error) {
	hostname, repo := splitRepoHostname(input.Repo)
	args := []string{"api", fmt.Sprintf("repos/%s/issues/%d", repo, input.IssueNumber)}
	if hostname != "" {
		args = append(args, "--hostname", hostname)
	}
	result, err := g.runGh(ctx, input.CWD, "", args...)
	if err != nil {
		return IssueDetail{}, err
	}
	row, err := decodeJSONObject(result.Stdout)
	if err != nil {
		return IssueDetail{}, err
	}
	commentArgs := []string{"api", "--paginate", "--slurp", fmt.Sprintf("repos/%s/issues/%d/comments", repo, input.IssueNumber)}
	if hostname != "" {
		commentArgs = append(commentArgs, "--hostname", hostname)
	}
	commentsResult, err := g.runGh(ctx, input.CWD, "", commentArgs...)
	if err != nil {
		return IssueDetail{}, err
	}
	commentRows, err := decodeJSONArrayOrPages(commentsResult.Stdout)
	if err != nil {
		return IssueDetail{}, err
	}
	return IssueDetail{
		Number:            asInt64(row["number"]),
		Title:             asString(row["title"]),
		Body:              asString(row["body"]),
		URL:               firstNonEmpty(asString(row["html_url"]), asString(row["url"])),
		State:             asString(row["state"]),
		StateReason:       firstNonEmpty(asString(row["state_reason"]), asString(row["stateReason"])),
		CreatedAt:         firstNonEmpty(asString(row["created_at"]), asString(row["createdAt"])),
		UpdatedAt:         firstNonEmpty(asString(row["updated_at"]), asString(row["updatedAt"])),
		ClosedAt:          firstNonEmpty(asString(row["closed_at"]), asString(row["closedAt"])),
		Author:            extractAuthor(firstNonNil(row["user"], row["author"])),
		AuthorAssociation: asString(row["author_association"]),
		Assignees:         extractActorLogins(row["assignees"]),
		AssigneeUsers:     extractActorUsers(row["assignees"]),
		Labels:            extractLabelNames(row["labels"]),
		IsPullRequest:     row["pull_request"] != nil,
		CommentCount:      len(commentRows),
		Comments:          extractCommentInfos(commentRows),
	}, nil
}

func (g *Gateway) GetIssueState(ctx context.Context, input ViewIssueInput) (IssueState, error) {
	hostname, repo := splitRepoHostname(input.Repo)
	args := []string{"api", fmt.Sprintf("repos/%s/issues/%d", repo, input.IssueNumber)}
	if hostname != "" {
		args = append(args, "--hostname", hostname)
	}
	result, err := g.runGh(ctx, input.CWD, "", args...)
	if err != nil {
		return IssueState{}, err
	}
	row, err := decodeJSONObject(result.Stdout)
	if err != nil {
		return IssueState{}, err
	}
	return IssueState{State: asString(row["state"]), StateReason: firstNonEmpty(asString(row["state_reason"]), asString(row["stateReason"]))}, nil
}

func (g *Gateway) ListIssueBlockedBy(ctx context.Context, input ListIssueBlockedByInput) ([]IssueDependency, error) {
	hostname, repo := splitRepoHostname(input.Repo)
	args := []string{"api", "--paginate", "--slurp", fmt.Sprintf("repos/%s/issues/%d/dependencies/blocked_by", repo, input.IssueNumber)}
	if hostname != "" {
		args = append(args, "--hostname", hostname)
	}
	result, err := g.runGh(ctx, input.CWD, "", args...)
	if err != nil {
		return nil, err
	}
	rows, err := decodeJSONArrayOrPages(result.Stdout)
	if err != nil {
		return nil, err
	}
	out := make([]IssueDependency, 0, len(rows))
	for _, row := range rows {
		dependency := IssueDependency{Number: asInt64(row["number"]), Repo: dependencyRepo(firstNonNil(row["repository"], row["repo"]), asString(row["repository_url"]), input.Repo)}
		if dependency.Number <= 0 {
			continue
		}
		out = append(out, dependency)
	}
	return out, nil
}

func (g *Gateway) ListBlockedByIssues(ctx context.Context, input ViewIssueInput) ([]DependencyIssue, error) {
	return g.listDependencyIssues(ctx, input, "dependencies/blocked_by")
}

func (g *Gateway) ListBlockingIssues(ctx context.Context, input ViewIssueInput) ([]DependencyIssue, error) {
	return g.listDependencyIssues(ctx, input, "dependencies/blocking")
}

func (g *Gateway) ListSubIssues(ctx context.Context, input ViewIssueInput) ([]DependencyIssue, error) {
	return g.listDependencyIssues(ctx, input, "sub_issues")
}

func (g *Gateway) listDependencyIssues(ctx context.Context, input ViewIssueInput, suffix string) ([]DependencyIssue, error) {
	hostname, repo := splitRepoHostname(input.Repo)
	args := []string{"api", "--paginate", "--slurp", fmt.Sprintf("repos/%s/issues/%d/%s", repo, input.IssueNumber, suffix), "-H", "Accept: application/vnd.github+json"}
	if hostname != "" {
		args = append(args, "--hostname", hostname)
	}
	result, err := g.runGh(ctx, input.CWD, "", args...)
	if err != nil {
		return nil, err
	}
	rows, err := decodeJSONArrayOrPages(result.Stdout)
	if err != nil {
		return nil, err
	}
	out := make([]DependencyIssue, 0, len(rows))
	for _, row := range rows {
		out = append(out, extractDependencyIssue(row, input.Repo))
	}
	return out, nil
}

func (g *Gateway) FindAnyIssueNumber(ctx context.Context, repo string, cwd string) (int64, error) {
	hostname, repoName := splitRepoHostname(repo)
	for page := 1; ; page++ {
		args := []string{"api", fmt.Sprintf("repos/%s/issues?state=all&per_page=100&page=%d", repoName, page)}
		if hostname != "" {
			args = append(args, "--hostname", hostname)
		}
		result, err := g.runGh(ctx, cwd, "", args...)
		if err != nil {
			return 0, err
		}
		rows, err := decodeJSONArray(result.Stdout)
		if err != nil {
			return 0, err
		}
		if len(rows) == 0 {
			return 0, nil
		}
		for _, row := range rows {
			if row["pull_request"] != nil {
				continue
			}
			if issueNumber := asInt64(row["number"]); issueNumber > 0 {
				return issueNumber, nil
			}
		}
	}
}

func (g *Gateway) ListIssueComments(ctx context.Context, input ViewIssueInput) ([]CommentInfo, error) {
	hostname, repo := splitRepoHostname(input.Repo)
	args := []string{"api", "--paginate", "--slurp", fmt.Sprintf("repos/%s/issues/%d/comments", repo, input.IssueNumber)}
	if hostname != "" {
		args = append(args, "--hostname", hostname)
	}
	result, err := g.runGh(ctx, input.CWD, "", args...)
	if err != nil {
		return nil, err
	}
	rows, err := decodeJSONArrayOrPages(result.Stdout)
	if err != nil {
		return nil, err
	}
	return extractCommentInfos(rows), nil
}

func (g *Gateway) ListIssueTimeline(ctx context.Context, input IssueTimelineInput) ([]map[string]any, error) {
	hostname, repo := splitRepoHostname(input.Repo)
	args := []string{"api", "--paginate", "--slurp", fmt.Sprintf("repos/%s/issues/%d/timeline", repo, input.IssueNumber), "-H", "Accept: application/vnd.github+json"}
	if hostname != "" {
		args = append(args, "--hostname", hostname)
	}
	result, err := g.runGh(ctx, input.CWD, "", args...)
	if err != nil {
		return nil, err
	}
	return decodeJSONArrayOrPages(result.Stdout)
}

func (g *Gateway) ListIssueReactions(ctx context.Context, input IssueReactionInput) ([]IssueReaction, error) {
	hostname, repo := splitRepoHostname(input.Repo)
	endpoint := fmt.Sprintf("repos/%s/issues/%d/reactions", repo, input.IssueNumber)
	if input.CommentID > 0 {
		endpoint = fmt.Sprintf("repos/%s/issues/comments/%d/reactions", repo, input.CommentID)
	}
	args := []string{"api", "--paginate", "--slurp", endpoint, "-H", "Accept: application/vnd.github+json"}
	if hostname != "" {
		args = append(args, "--hostname", hostname)
	}
	result, err := g.runGh(ctx, input.CWD, "", args...)
	if err != nil {
		return nil, err
	}
	rows, err := decodeJSONArrayOrPages(result.Stdout)
	if err != nil {
		return nil, err
	}
	out := make([]IssueReaction, 0, len(rows))
	for _, row := range rows {
		if reaction, ok := normalizeReaction(row); ok {
			out = append(out, IssueReaction{ID: reaction.ID, Content: reaction.Content, UserLogin: reaction.UserLogin})
		}
	}
	return out, nil
}

func (g *Gateway) AddIssueReaction(ctx context.Context, input CreateIssueReactionInput) error {
	content := strings.TrimSpace(input.Content)
	if content == "" {
		return nil
	}
	hostname, repo := splitRepoHostname(input.Repo)
	endpoint := fmt.Sprintf("repos/%s/issues/%d/reactions", repo, input.IssueNumber)
	if input.CommentID > 0 {
		endpoint = fmt.Sprintf("repos/%s/issues/comments/%d/reactions", repo, input.CommentID)
	}
	args := []string{"api", endpoint, "--method", "POST", "-H", "Accept: application/vnd.github+json", "-f", "content=" + content}
	if hostname != "" {
		args = append(args, "--hostname", hostname)
	}
	_, err := g.runGh(ctx, input.CWD, "", args...)
	return err
}

func (g *Gateway) CreateIssueComment(ctx context.Context, input IssueCommentInput) (IssueCommentResult, error) {
	hostname, repo := splitRepoHostname(input.Repo)
	args := []string{"api", fmt.Sprintf("repos/%s/issues/%d/comments", repo, input.IssueNumber), "--method", "POST", "-f", "body=" + input.Body}
	if hostname != "" {
		args = append(args, "--hostname", hostname)
	}
	result, err := g.runGh(ctx, input.CWD, "", args...)
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
	hostname, repo := splitRepoHostname(input.Repo)
	args := []string{"api", fmt.Sprintf("repos/%s/issues/comments/%d", repo, input.CommentID), "--method", "PATCH", "-f", "body=" + input.Body}
	if hostname != "" {
		args = append(args, "--hostname", hostname)
	}
	_, err := g.runGh(ctx, input.CWD, "", args...)
	return err
}

func (g *Gateway) DeleteIssueComment(ctx context.Context, input DeleteIssueCommentInput) error {
	hostname, repo := splitRepoHostname(input.Repo)
	args := []string{"api", fmt.Sprintf("repos/%s/issues/comments/%d", repo, input.CommentID), "--method", "DELETE"}
	if hostname != "" {
		args = append(args, "--hostname", hostname)
	}
	_, err := g.runGh(ctx, input.CWD, "", args...)
	return err
}

func (g *Gateway) CloseIssue(ctx context.Context, input CloseIssueInput) error {
	reason, err := validateCloseIssueStateReason(input.StateReason)
	if err != nil {
		return err
	}
	state, err := g.viewIssueState(ctx, input.Repo, input.IssueNumber, input.CWD)
	if err != nil {
		return err
	}
	if state == "closed" {
		return nil
	}
	_, err = g.runGh(ctx, input.CWD, "", "issue", "close", strconv.FormatInt(input.IssueNumber, 10), "--repo", input.Repo, "--reason", reason)
	if err == nil {
		return nil
	}
	state, stateErr := g.viewIssueState(ctx, input.Repo, input.IssueNumber, input.CWD)
	if stateErr == nil && state == "closed" {
		return nil
	}
	return err
}

func (g *Gateway) AddIssueAssignees(ctx context.Context, input IssueAssigneesInput) error {
	assignees := compactIssueAssignees(input.Assignees)
	if len(assignees) == 0 {
		return nil
	}
	hostname, repo := splitRepoHostname(input.Repo)
	args := []string{"api", fmt.Sprintf("repos/%s/issues/%d/assignees", repo, input.IssueNumber), "--method", "POST"}
	for _, assignee := range assignees {
		args = append(args, "-f", "assignees[]="+assignee)
	}
	if hostname != "" {
		args = append(args, "--hostname", hostname)
	}
	_, err := g.runGh(ctx, input.CWD, "", args...)
	return err
}

func (g *Gateway) AddIssueLabels(ctx context.Context, input IssueLabelsInput) error {
	if len(input.Labels) == 0 {
		return nil
	}
	if err := g.ensureLabelsExist(ctx, input.Repo, input.Labels, input.CWD); err != nil {
		return err
	}
	hostname, repo := splitRepoHostname(input.Repo)
	args := []string{"api", fmt.Sprintf("repos/%s/issues/%d/labels", repo, input.IssueNumber), "--method", "POST"}
	for _, label := range input.Labels {
		args = append(args, "-f", "labels[]="+label)
	}
	if hostname != "" {
		args = append(args, "--hostname", hostname)
	}
	_, err := g.runGh(ctx, input.CWD, "", args...)
	return err
}

func (g *Gateway) RemoveIssueLabels(ctx context.Context, input IssueLabelsInput) error {
	if len(input.Labels) == 0 {
		return nil
	}
	hostname, repo := splitRepoHostname(input.Repo)
	for _, label := range input.Labels {
		args := []string{"api", fmt.Sprintf("repos/%s/issues/%d/labels/%s", repo, input.IssueNumber, encodeURIComponent(label)), "--method", "DELETE"}
		if hostname != "" {
			args = append(args, "--hostname", hostname)
		}
		_, err := g.runGh(ctx, input.CWD, "", args...)
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

func (g *Gateway) GetRepositoryPermission(ctx context.Context, input RepositoryPermissionInput) (string, error) {
	hostname, repo := splitRepoHostname(input.Repo)
	args := []string{"api", fmt.Sprintf("repos/%s/collaborators/%s/permission", repo, input.User)}
	if hostname != "" {
		args = append(args, "--hostname", hostname)
	}
	result, err := g.runGh(ctx, input.CWD, "", args...)
	if err != nil {
		if shellErr, ok := err.(*shell.CommandExecutionError); ok && strings.Contains(strings.ToLower(shellErr.Result.Stderr), "404") {
			return "", nil
		}
		return "", err
	}
	row, err := decodeJSONObject(result.Stdout)
	if err != nil {
		return "", err
	}
	return strings.ToLower(strings.TrimSpace(asString(row["permission"]))), nil
}

func (g *Gateway) GetRepositorySettings(ctx context.Context, input RepositorySettingsInput) (RepositorySettings, error) {
	hostname, repo := splitRepoHostname(input.Repo)
	args := []string{"api", fmt.Sprintf("repos/%s", repo)}
	if hostname != "" {
		args = append(args, "--hostname", hostname)
	}
	result, err := g.runGh(ctx, input.CWD, "", args...)
	if err != nil {
		return RepositorySettings{}, err
	}
	row, err := decodeJSONObject(result.Stdout)
	if err != nil {
		return RepositorySettings{}, err
	}
	return RepositorySettings{
		AllowSquashMerge: asBool(row["allow_squash_merge"]),
		AllowMergeCommit: asBool(row["allow_merge_commit"]),
		AllowRebaseMerge: asBool(row["allow_rebase_merge"]),
		AllowAutoMerge:   asBool(row["allow_auto_merge"]),
	}, nil
}

func (g *Gateway) GetBranchProtection(ctx context.Context, input BranchProtectionInput) (BranchProtection, error) {
	hostname, repo := splitRepoHostname(input.Repo)
	args := []string{"api", fmt.Sprintf("repos/%s/branches/%s/protection", repo, encodeURIComponent(input.Branch))}
	if hostname != "" {
		args = append(args, "--hostname", hostname)
	}
	result, err := g.runGh(ctx, input.CWD, "", args...)
	if err != nil {
		if IsNotFoundError(err) {
			return BranchProtection{}, nil
		}
		return BranchProtection{}, err
	}
	row, err := decodeJSONObject(result.Stdout)
	if err != nil {
		return BranchProtection{}, err
	}
	requiredStatusChecks, _ := row["required_status_checks"].(map[string]any)
	hasRequiredChecks := false
	requiredChecks := []string{}
	if requiredStatusChecks != nil {
		for _, check := range toObjectSlice(requiredStatusChecks["checks"]) {
			name := firstNonEmpty(asString(check["context"]), asString(check["name"]))
			if strings.TrimSpace(name) != "" {
				requiredChecks = append(requiredChecks, name)
			}
		}
		if len(requiredChecks) > 0 {
			hasRequiredChecks = true
		}
		if contexts, ok := requiredStatusChecks["contexts"].([]any); ok && len(contexts) > 0 {
			hasRequiredChecks = true
			for _, context := range contexts {
				name := asString(context)
				if strings.TrimSpace(name) != "" {
					requiredChecks = append(requiredChecks, name)
				}
			}
		}
	}
	return BranchProtection{Enabled: true, HasRequiredChecks: hasRequiredChecks, RequiredChecks: uniqueStrings(requiredChecks)}, nil
}

func compactIssueAssignees(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			out = append(out, value)
		}
	}
	return out
}

func (g *Gateway) ViewPullRequest(ctx context.Context, input ViewPullRequestInput) (PullRequestDetail, error) {
	if snapshot := discoverySnapshotFromContext(ctx); snapshot != nil {
		return snapshot.viewPullRequest(ctx, input)
	}
	return g.viewPullRequestRaw(ctx, input)
}

func (g *Gateway) viewPullRequestRaw(ctx context.Context, input ViewPullRequestInput) (PullRequestDetail, error) {
	row, err := g.viewPullRequestRow(ctx, input, prViewJSONFields)
	if err != nil && IsInaccessibleReviewRequestReviewerError(err) {
		row, err = g.viewPullRequestRow(ctx, input, withoutJSONField(prViewJSONFields, "reviewRequests"))
	}
	if err != nil {
		return PullRequestDetail{}, err
	}
	threads, err := g.fetchReviewThreads(ctx, input.Repo, input.PRNumber, input.CWD)
	if err != nil {
		return PullRequestDetail{}, err
	}
	return PullRequestDetail{
		Number:             asInt64(row["number"]),
		Title:              asString(row["title"]),
		Body:               asString(row["body"]),
		URL:                asString(row["url"]),
		State:              asString(row["state"]),
		CreatedAt:          asString(row["createdAt"]),
		UpdatedAt:          asString(row["updatedAt"]),
		ClosedAt:           asString(row["closedAt"]),
		IsDraft:            asBool(row["isDraft"]),
		ReviewDecision:     asString(row["reviewDecision"]),
		Labels:             extractLabelNames(row["labels"]),
		HeadRefName:        asString(row["headRefName"]),
		BaseRefName:        asString(row["baseRefName"]),
		HeadSHA:            asString(row["headRefOid"]),
		BaseSHA:            asString(row["baseRefOid"]),
		Author:             extractAuthor(row["author"]),
		AuthorAssociation:  asString(row["authorAssociation"]),
		CommentCount:       len(toObjectSlice(row["comments"])),
		ReviewRequests:     extractReviewRequestLogins(row["reviewRequests"]),
		ReviewRequestUsers: extractReviewRequestUsers(row["reviewRequests"]),
		HasConflicts:       asString(row["mergeStateStatus"]) == "DIRTY",
		Comments:           threads,
		IssueComments:      extractCommentInfos(toObjectSlice(row["comments"])),
		Reviews:            toObjectSlice(row["reviews"]),
		Checks:             toObjectSlice(row["statusCheckRollup"]),
		Mergeable:          boolPtrFromValue(row["mergeable"]),
		MergeableState:     asString(row["mergeable_state"]),
		MergedAt:           asString(row["merged_at"]),
		AutoMerge:          extractAutoMerge(row["auto_merge"]),
	}, nil
}

func (g *Gateway) viewPullRequestRow(ctx context.Context, input ViewPullRequestInput, fields []string) (map[string]any, error) {
	result, err := g.runGh(ctx, input.CWD, "", "pr", "view", fmt.Sprintf("%d", input.PRNumber), "--repo", input.Repo, "--json", strings.Join(fields, ","))
	if err != nil {
		return nil, err
	}
	return decodeJSONObject(result.Stdout)
}

func (g *Gateway) ViewPullRequestMergeWatch(ctx context.Context, input ViewPullRequestInput) (PullRequestDetail, error) {
	hostname, repo := splitRepoHostname(input.Repo)
	args := []string{"api", fmt.Sprintf("repos/%s/pulls/%d", repo, input.PRNumber), "-H", "Accept: application/vnd.github+json"}
	if hostname != "" {
		args = append(args, "--hostname", hostname)
	}
	result, err := g.runGh(ctx, input.CWD, "", args...)
	if err != nil {
		return PullRequestDetail{}, err
	}
	row, err := decodeJSONObject(result.Stdout)
	if err != nil {
		return PullRequestDetail{}, err
	}
	return PullRequestDetail{
		Number:         asInt64(row["number"]),
		Title:          asString(row["title"]),
		Body:           asString(row["body"]),
		URL:            firstNonEmpty(asString(row["html_url"]), asString(row["url"])),
		State:          asString(row["state"]),
		CreatedAt:      firstNonEmpty(asString(row["created_at"]), asString(row["createdAt"])),
		UpdatedAt:      firstNonEmpty(asString(row["updated_at"]), asString(row["updatedAt"])),
		ClosedAt:       firstNonEmpty(asString(row["closed_at"]), asString(row["closedAt"])),
		MergedAt:       firstNonEmpty(asString(row["merged_at"]), asString(row["mergedAt"])),
		Labels:         extractLabelNames(row["labels"]),
		HeadRefName:    nestedString(row, "head", "ref"),
		BaseRefName:    nestedString(row, "base", "ref"),
		HeadSHA:        nestedString(row, "head", "sha"),
		BaseSHA:        nestedString(row, "base", "sha"),
		Mergeable:      boolPtrFromValue(row["mergeable"]),
		MergeableState: firstNonEmpty(asString(row["mergeable_state"]), asString(row["mergeStateStatus"])),
		AutoMerge:      extractAutoMerge(row["auto_merge"]),
	}, nil
}

func (g *Gateway) ListPullRequestCheckRuns(ctx context.Context, input PullRequestCheckRunsInput) (PullRequestCheckRuns, error) {
	hostname, repo := splitRepoHostname(input.Repo)
	args := []string{"api", fmt.Sprintf("repos/%s/commits/%s/check-runs", repo, encodeURIComponent(input.Ref)), "-H", "Accept: application/vnd.github+json"}
	if hostname != "" {
		args = append(args, "--hostname", hostname)
	}
	result, err := g.runGh(ctx, input.CWD, "", args...)
	if err != nil {
		return PullRequestCheckRuns{}, err
	}
	row, err := decodeJSONObject(result.Stdout)
	if err != nil {
		return PullRequestCheckRuns{}, err
	}
	statusArgs := []string{"api", fmt.Sprintf("repos/%s/commits/%s/status", repo, encodeURIComponent(input.Ref)), "-H", "Accept: application/vnd.github+json"}
	if hostname != "" {
		statusArgs = append(statusArgs, "--hostname", hostname)
	}
	statusResult, err := g.runGh(ctx, input.CWD, "", statusArgs...)
	if err != nil {
		return PullRequestCheckRuns{}, err
	}
	statusRow, err := decodeJSONObject(statusResult.Stdout)
	if err != nil {
		return PullRequestCheckRuns{}, err
	}
	checkRuns := toObjectSlice(row["check_runs"])
	out := PullRequestCheckRuns{TotalCount: int(asInt64(row["total_count"])), CheckRuns: make([]PullRequestCheckRun, 0, len(checkRuns))}
	for _, checkRun := range checkRuns {
		out.CheckRuns = append(out.CheckRuns, PullRequestCheckRun{Name: asString(checkRun["name"]), Status: asString(checkRun["status"]), Conclusion: asString(checkRun["conclusion"])})
	}
	statuses := toObjectSlice(statusRow["statuses"])
	out.Statuses = make([]PullRequestStatus, 0, len(statuses))
	seenContexts := map[string]struct{}{}
	for _, status := range statuses {
		contextName := asString(status["context"])
		key := strings.ToLower(strings.TrimSpace(contextName))
		if key == "" {
			continue
		}
		if _, ok := seenContexts[key]; ok {
			continue
		}
		seenContexts[key] = struct{}{}
		out.Statuses = append(out.Statuses, PullRequestStatus{Context: contextName, State: asString(status["state"])})
	}
	return out, nil
}

func (g *Gateway) ListLinkedPullRequests(ctx context.Context, input LinkedPullRequestsInput) ([]LinkedPullRequest, error) {
	hostname, repo := splitRepoHostname(input.Repo)
	owner, repoName := splitRepoOwnerName(repo)
	out := make([]LinkedPullRequest, 0, 20)
	cursor := ""
	for {
		nodes, nextCursor, hasNextPage, err := g.fetchLinkedPullRequestsPage(ctx, input.CWD, hostname, owner, repoName, input.IssueNumber, cursor)
		if err != nil {
			return nil, err
		}
		for _, node := range nodes {
			mergeCommit, _ := node["mergeCommit"].(map[string]any)
			out = append(out, LinkedPullRequest{Number: asInt64(node["number"]), State: asString(node["state"]), Merged: asString(node["state"]) == "MERGED" || asString(node["mergedAt"]) != "", MergedAt: asString(node["mergedAt"]), MergeCommitSHA: asString(mergeCommit["oid"])})
		}
		if !hasNextPage || nextCursor == "" {
			break
		}
		cursor = nextCursor
	}
	return out, nil
}

func (g *Gateway) ListPullRequestReviewState(ctx context.Context, input PullRequestReviewStateInput) (PullRequestReviewState, error) {
	result, err := g.runGh(ctx, input.CWD, "", "pr", "view", strconv.FormatInt(input.PRNumber, 10), "--repo", input.Repo, "--json", "reviewRequests,reviews")
	if err != nil {
		return PullRequestReviewState{}, err
	}
	row, err := decodeJSONObject(result.Stdout)
	if err != nil {
		return PullRequestReviewState{}, err
	}
	latest := map[string]string{}
	last := ""
	for _, review := range toObjectSlice(row["reviews"]) {
		if user := extractAuthor(review["author"]); user != "" {
			latest[user] = asString(review["state"])
		}
		if at := firstNonEmpty(asString(review["submittedAt"]), asString(review["updatedAt"])); at > last {
			last = at
		}
	}
	return PullRequestReviewState{RequestedReviewers: extractReviewRequestLogins(row["reviewRequests"]), LatestReviewPerUser: latest, LastReviewAt: last}, nil
}

func (g *Gateway) ClosePullRequest(ctx context.Context, input ClosePullRequestInput) error {
	state, err := g.viewPullRequestState(ctx, input.Repo, input.PRNumber, input.CWD)
	if err != nil {
		return err
	}
	if state == "closed" || state == "merged" {
		return nil
	}
	_, err = g.runGh(ctx, input.CWD, "", "pr", "close", strconv.FormatInt(input.PRNumber, 10), "--repo", input.Repo)
	if err == nil {
		return nil
	}
	state, stateErr := g.viewPullRequestState(ctx, input.Repo, input.PRNumber, input.CWD)
	if stateErr == nil && (state == "closed" || state == "merged") {
		return nil
	}
	return err
}

func (g *Gateway) EnableAutoMerge(ctx context.Context, input EnableAutoMergeInput) error {
	strategy := strings.TrimSpace(string(input.Strategy))
	if strategy == "" {
		return fmt.Errorf("auto-merge strategy is required")
	}
	headSHA := strings.TrimSpace(input.HeadSHA)
	if headSHA == "" {
		return fmt.Errorf("auto-merge head SHA is required")
	}
	_, err := g.runGh(ctx, input.CWD, "", "pr", "merge", strconv.FormatInt(input.PRNumber, 10), "--repo", input.Repo, "--auto", "--"+strategy, "--match-head-commit", headSHA)
	return err
}

func (g *Gateway) GetPullRequestHeadSHA(ctx context.Context, input ViewPullRequestInput) (string, error) {
	result, err := g.runGh(ctx, input.CWD, "", "pr", "view", fmt.Sprintf("%d", input.PRNumber), "--repo", input.Repo, "--json", "headRefOid")
	if err != nil {
		return "", err
	}
	row, err := decodeJSONObject(result.Stdout)
	if err != nil {
		return "", err
	}
	return asString(row["headRefOid"]), nil
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

func (g *Gateway) ViewReviewThread(ctx context.Context, input ViewReviewThreadInput) (ReviewThread, error) {
	thread, err := g.getReviewThread(ctx, input.ThreadID, input.CWD)
	if err != nil {
		return ReviewThread{}, err
	}
	if thread == nil {
		return ReviewThread{}, nil
	}
	out := ReviewThread{ID: thread.ID, IsResolved: thread.IsResolved}
	cursor := ""
	for {
		nodes, nextCursor, hasNextPage, err := g.fetchReviewThreadCommentsPage(ctx, input.CWD, input.ThreadID, cursor)
		if err != nil {
			return ReviewThread{}, err
		}
		out.Comments = appendReviewThreadComments(out.Comments, nodes)
		if !hasNextPage {
			break
		}
		cursor = nextCursor
	}
	return out, nil
}

func (g *Gateway) ListReviewThreads(ctx context.Context, input ListReviewThreadsInput) ([]ReviewThread, error) {
	limit := input.Limit
	if limit <= 0 {
		limit = 100
	}
	owner, name, err := parseRepo(input.Repo)
	if err != nil {
		return nil, err
	}
	out := make([]ReviewThread, 0, min(limit, 100))
	threadsCursor := ""
	for len(out) < limit {
		pageSize := min(100, limit-len(out))
		nodes, nextCursor, hasNextPage, err := g.fetchReviewThreadPage(ctx, input.CWD, owner, name, input.PRNumber, pageSize, threadsCursor)
		if err != nil {
			return nil, err
		}
		for _, node := range nodes {
			threadRow, _ := node.(map[string]any)
			id := asString(threadRow["id"])
			if id == "" {
				continue
			}
			thread := ReviewThread{ID: id, IsResolved: asBool(threadRow["isResolved"]), Path: asString(threadRow["path"]), Line: asInt64(threadRow["line"])}
			commentsRow, _ := threadRow["comments"].(map[string]any)
			commentNodes, _ := commentsRow["nodes"].([]any)
			thread.Comments = appendReviewThreadComments(thread.Comments, commentNodes)
			commentPageInfo, _ := commentsRow["pageInfo"].(map[string]any)
			commentCursor := asString(commentPageInfo["endCursor"])
			for asBool(commentPageInfo["hasNextPage"]) && commentCursor != "" {
				moreComments, nextCommentCursor, hasMoreComments, err := g.fetchReviewThreadCommentsPage(ctx, input.CWD, id, commentCursor)
				if err != nil {
					return nil, err
				}
				thread.Comments = appendReviewThreadComments(thread.Comments, moreComments)
				commentPageInfo = map[string]any{"hasNextPage": hasMoreComments, "endCursor": nextCommentCursor}
				commentCursor = nextCommentCursor
			}
			if thread.URL == "" && len(thread.Comments) > 0 {
				thread.URL = thread.Comments[0].URL
			}
			out = append(out, thread)
			if len(out) == limit {
				break
			}
		}
		if !hasNextPage || nextCursor == "" {
			break
		}
		threadsCursor = nextCursor
	}
	return out, nil
}

func (g *Gateway) AddReviewThreadReply(ctx context.Context, input AddReviewThreadReplyInput) error {
	result, err := g.runGh(ctx, input.CWD, "", "api", "graphql", "-f", "query="+strings.Join([]string{
		"mutation($threadId: ID!, $body: String!) {",
		"  addPullRequestReviewThreadReply(input: { pullRequestReviewThreadId: $threadId, body: $body }) {",
		"    comment { id }",
		"  }",
		"}",
	}, "\n"), "-F", "threadId="+input.ThreadID, "-f", "body="+input.Body)
	if err != nil {
		return err
	}
	row, err := decodeJSONObject(result.Stdout)
	if err != nil {
		return err
	}
	data, _ := row["data"].(map[string]any)
	replyRow, _ := data["addPullRequestReviewThreadReply"].(map[string]any)
	commentRow, _ := replyRow["comment"].(map[string]any)
	if asString(commentRow["id"]) == "" {
		return fmt.Errorf("failed to add review thread reply %s", input.ThreadID)
	}
	return nil
}

// CompareCommits returns the GitHub compare API status between Base and Head
// (one of "identical", "ahead", "behind", "diverged"). It is used by the
// fixer to decide whether a previously-pushed fix commit is still reachable
// from the live PR head.
func (g *Gateway) CompareCommits(ctx context.Context, input CompareCommitsInput) (CompareCommitsResult, error) {
	if input.Repo == "" || input.Base == "" || input.Head == "" {
		return CompareCommitsResult{}, fmt.Errorf("compare commits requires repo, base, and head")
	}
	result, err := g.runGh(ctx, input.CWD, "", "api", fmt.Sprintf("repos/%s/compare/%s...%s", input.Repo, input.Base, input.Head))
	if err != nil {
		return CompareCommitsResult{}, err
	}
	row, err := decodeJSONObject(result.Stdout)
	if err != nil {
		return CompareCommitsResult{}, err
	}
	return CompareCommitsResult{Status: asString(row["status"])}, nil
}

func (g *Gateway) GetPullRequestDiff(ctx context.Context, input GetPullRequestDiffInput) (string, error) {
	result, err := g.runGhWithTimeout(ctx, input.CWD, "", prDiffGhCommandTimeout, "pr", "diff", fmt.Sprintf("%d", input.PRNumber), "--repo", input.Repo)
	if err != nil {
		if isDiffTooLargeError(err) {
			return "", ErrDiffTooLarge
		}
		return "", err
	}
	return result.Stdout, nil
}

func (g *Gateway) SubmitReview(ctx context.Context, input SubmitReviewInput) error {
	request := g.reviewSubmitRequest(input)
	if marker, ok := findReviewIdempotencyMarker(input.Body, ""); ok && marker.Outcome == "clean" && len(input.Comments) > 0 {
		err := fmt.Errorf("clean review marker cannot be submitted with review comments")
		g.emitReviewSubmitDiagnostic("github_review_submit_validation_failed", map[string]any{"request": request, "error": err.Error()})
		return err
	}
	var flags []reviewQualityFlag
	var processing reviewCommentProcessing
	input.Body, input.Comments, flags, processing = normalizeReviewAnchors(input.Body, input.Comments, input.Anchors)
	request = g.reviewSubmitRequest(input)
	request["comment_processing"] = map[string]any{
		"original_count":   processing.OriginalCount,
		"submitted_count":  processing.SubmittedCount,
		"normalized_count": processing.NormalizedCount,
		"downgraded_count": processing.DowngradedCount,
		"dropped_count":    processing.DroppedCount,
		"comments":         processing.Comments,
	}
	gateApplies, err := reviewQualityGateApplies(input.Event, input.Body)
	if err != nil {
		g.emitReviewSubmitDiagnostic("github_review_submit_validation_failed", map[string]any{"request": request, "error": err.Error()})
		return err
	}
	if gateApplies && len(flags) > 0 {
		err := fmt.Errorf("review quality gate failed: %s", formatReviewQualityFlags(flags))
		g.emitReviewSubmitDiagnostic("github_review_submit_validation_failed", map[string]any{"request": request, "error": err.Error(), "quality_flags": reviewQualityFlagsSummary(flags)})
		return err
	}
	comments := input.Comments[:0]
	for _, comment := range input.Comments {
		comment.Body = normalizeInlineReviewDisclosure(comment.Body, input.Disclosure)
		if strings.TrimSpace(comment.Body) == "" {
			processing.DroppedCount++
			markReviewCommentDropped(processing.Comments, comment.DiagnosticIndex)
			continue
		}
		comments = append(comments, comment)
	}
	input.Comments = comments
	processing.SubmittedCount = len(input.Comments)
	request = g.reviewSubmitRequest(input)
	request["comment_processing"] = map[string]any{
		"original_count":   processing.OriginalCount,
		"submitted_count":  processing.SubmittedCount,
		"normalized_count": processing.NormalizedCount,
		"downgraded_count": processing.DowngradedCount,
		"dropped_count":    processing.DroppedCount,
		"comments":         processing.Comments,
	}
	if len(input.Comments) > 0 || strings.TrimSpace(input.CommitID) != "" {
		endpoint := fmt.Sprintf("repos/%s/pulls/%d/reviews", input.Repo, input.PRNumber)
		payload := map[string]any{
			"event":     input.Event,
			"body":      emptyToNil(input.Body),
			"commit_id": emptyToNil(input.CommitID),
		}
		if len(input.Comments) > 0 {
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
		}
		body, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("marshal gh review payload: %w", err)
		}
		request["method"] = "POST"
		request["endpoint"] = endpoint
		g.emitReviewSubmitDiagnostic("github_review_submit_prepared", map[string]any{"request": request})
		result, err := g.runGh(ctx, input.CWD, string(body), "api", endpoint, "--method", "POST", "--input", "-", "--include")
		if err != nil {
			fields := map[string]any{"request": request, "gh_stdout": reviewSubmitRawStdout(result.Stdout), "gh_stderr": result.Stderr, "gh_error": sanitizeReviewSubmitCommandError(err.Error())}
			if response, ok := parseReviewSubmitHTTPResponse(result.Stdout); ok {
				if response.StatusCode > 0 {
					fields["http_status"] = response.StatusCode
				}
				if response.Body != "" {
					fields["response_body"] = response.Body
				}
				if len(response.Headers) > 0 {
					fields["response_headers"] = response.Headers
				}
			}
			g.emitReviewSubmitDiagnostic("github_review_submit_failed", fields)
			return err
		}
		return err
	}
	args := []string{"pr", "review", fmt.Sprintf("%d", input.PRNumber), "--repo", input.Repo, "--" + strings.ToLower(strings.ReplaceAll(input.Event, "_", "-"))}
	if input.Body != "" {
		args = append(args, "--body", input.Body)
	}
	_, err = g.runGh(ctx, input.CWD, "", args...)
	return err
}

func (g *Gateway) emitReviewSubmitDiagnostic(event string, fields map[string]any) {
	if g.reviewSubmitDiagnostic != nil {
		g.reviewSubmitDiagnostic(event, fields)
	}
}

func (g *Gateway) reviewSubmitRequest(input SubmitReviewInput) map[string]any {
	endpoint := fmt.Sprintf("repos/%s/pulls/%d/reviews", input.Repo, input.PRNumber)
	request := map[string]any{
		"repo":      input.Repo,
		"pr_number": input.PRNumber,
		"event":     input.Event,
		"commit_id": strings.TrimSpace(input.CommitID),
		"method":    "POST",
		"endpoint":  endpoint,
		"payload": map[string]any{
			"body_marker": reviewSubmitBodyMarkerSummary(input.Body),
			"comments":    reviewSubmitCommentsSummary(input.Comments),
		},
	}
	return request
}

func reviewSubmitBodyMarkerSummary(body string) map[string]any {
	if marker, ok := findReviewIdempotencyMarker(body, ""); ok {
		return map[string]any{"id": marker.ID, "head": marker.Head, "outcome": marker.Outcome}
	}
	return map[string]any{}
}

func reviewSubmitCommentsSummary(comments []ReviewComment) []map[string]any {
	summary := make([]map[string]any, 0, len(comments))
	for idx, comment := range comments {
		entry := map[string]any{"index": reviewCommentDiagnosticIndex(comment, idx)}
		for key, value := range reviewCommentAnchorMap(comment) {
			entry[key] = value
		}
		summary = append(summary, entry)
	}
	return summary
}

func reviewCommentDiagnosticIndex(comment ReviewComment, fallback int) int {
	if comment.DiagnosticIndex != 0 || fallback == 0 {
		return comment.DiagnosticIndex
	}
	return fallback
}

func reviewQualityFlagsSummary(flags []reviewQualityFlag) []map[string]any {
	summary := make([]map[string]any, 0, len(flags))
	for _, flag := range flags {
		summary = append(summary, map[string]any{"kind": flag.Kind, "detail": flag.Detail})
	}
	return summary
}

func reviewSubmitRawStdout(raw string) string {
	if response, ok := parseReviewSubmitHTTPResponse(raw); ok && response.Body != "" {
		return response.Body
	}
	return raw
}

func sanitizeReviewSubmitCommandError(message string) string {
	message = strings.TrimSpace(message)
	if head, _, ok := strings.Cut(message, "\nstdout:"); ok {
		return strings.TrimSpace(head)
	}
	return message
}

type reviewSubmitHTTPResponse struct {
	StatusCode int
	Headers    map[string]any
	Body       string
}

func parseReviewSubmitHTTPResponse(raw string) (reviewSubmitHTTPResponse, bool) {
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	parts := strings.SplitN(raw, "\n\n", 2)
	if len(parts) != 2 {
		return reviewSubmitHTTPResponse{}, false
	}
	headLines := strings.Split(parts[0], "\n")
	if len(headLines) == 0 || !strings.HasPrefix(headLines[0], "HTTP/") {
		return reviewSubmitHTTPResponse{}, false
	}
	response := reviewSubmitHTTPResponse{Headers: map[string]any{}, Body: strings.TrimSpace(parts[1])}
	fields := strings.Fields(headLines[0])
	if len(fields) >= 2 {
		if status, err := strconv.Atoi(fields[1]); err == nil {
			response.StatusCode = status
		}
	}
	for _, line := range headLines[1:] {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		normalizedKey := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(key), "-", "_"))
		switch normalizedKey {
		case "x_github_request_id", "x_ratelimit_limit", "x_ratelimit_remaining", "x_ratelimit_reset", "retry_after", "content_type":
			response.Headers[normalizedKey] = strings.TrimSpace(value)
		}
	}
	return response, true
}

func normalizeInlineReviewDisclosure(body string, disclosureCfg config.DisclosureConfig) string {
	if !hasInlineReviewDisclosure(body) {
		return body
	}
	if !disclosureCfg.Enabled || !disclosureCfg.Channels.ReviewComment {
		return stripInlineReviewDisclosure(body)
	}
	if disclosureCfg.Channels.InlineCommentVisible || !containsVisibleInlineReviewDisclosure(body) {
		return body
	}
	cleaned := disclosure.StripMarkdownStamp(body)
	if strings.TrimSpace(cleaned) == "" {
		return disclosure.Marker
	}
	return strings.TrimRight(cleaned, "\n") + "\n\n" + disclosure.Marker
}

func stripInlineReviewDisclosure(body string) string {
	if !hasInlineReviewDisclosure(body) {
		return body
	}
	return disclosure.StripMarkdownStamp(body)
}

func hasInlineReviewDisclosure(body string) bool {
	return strings.Contains(body, disclosure.Marker) || containsVisibleInlineReviewDisclosure(body)
}

func containsVisibleInlineReviewDisclosure(body string) bool {
	return strings.Contains(body, "Generated by Looper") ||
		strings.Contains(body, "Powered by Looper") ||
		strings.Contains(body, "Generated by "+disclosure.RepoLinkHTML) ||
		strings.Contains(body, "Powered by "+disclosure.RepoLinkHTML) ||
		strings.Contains(body, "Generated by [Looper]("+disclosure.RepoURL+")") ||
		strings.Contains(body, "Powered by [Looper]("+disclosure.RepoURL+")") ||
		strings.Contains(body, "Generated by [Looper]("+disclosure.LegacyRepoURL+")") ||
		strings.Contains(body, "Powered by [Looper]("+disclosure.LegacyRepoURL+")")
}

func (g *Gateway) AddPullRequestComment(ctx context.Context, input PullRequestCommentInput) error {
	_, err := g.runGh(ctx, input.CWD, "", "pr", "comment", fmt.Sprintf("%d", input.PRNumber), "--repo", input.Repo, "--body", input.Body)
	return err
}

func (g *Gateway) HasReviewMarker(ctx context.Context, input VerifyReviewMarkerInput) (bool, error) {
	marker, err := g.FindReviewMarker(ctx, input)
	if err != nil {
		return false, err
	}
	return marker.Found, nil
}

func (g *Gateway) FindReviewMarker(ctx context.Context, input VerifyReviewMarkerInput) (ReviewMarkerResult, error) {
	if strings.TrimSpace(input.Marker) == "" {
		return ReviewMarkerResult{}, nil
	}
	reviewsResult, err := g.runGh(ctx, input.CWD, "", "api", "--paginate", "--slurp", fmt.Sprintf("repos/%s/pulls/%d/reviews", input.Repo, input.PRNumber))
	if err != nil {
		return ReviewMarkerResult{}, err
	}
	marker := findAllowedReviewMarker(reviewsResult.Stdout, input.Marker, input.AllowedReviewEvents, input.AuthorLogin, input.AllowCleanComment)
	if marker.Found && marker.ReviewID != "" {
		comments, err := g.fetchReviewCommentBodies(ctx, input.Repo, input.PRNumber, marker.ReviewID, input.CWD)
		if err != nil {
			return ReviewMarkerResult{}, err
		}
		marker.InlineCommentBodies = comments
	}
	return marker, nil
}

func (g *Gateway) fetchReviewCommentBodies(ctx context.Context, repo string, prNumber int64, reviewID string, cwd string) ([]string, error) {
	result, err := g.runGh(ctx, cwd, "", "api", "--paginate", "--slurp", fmt.Sprintf("repos/%s/pulls/%d/reviews/%s/comments", repo, prNumber, reviewID))
	if err != nil {
		return nil, err
	}
	rows, err := decodeJSONArrayOrPages(result.Stdout)
	if err != nil {
		return nil, err
	}
	bodies := make([]string, 0, len(rows))
	for _, row := range rows {
		if body := asString(row["body"]); strings.TrimSpace(body) != "" {
			bodies = append(bodies, body)
		}
	}
	return bodies, nil
}

func jsonBodiesContainAllowedReviewMarker(raw string, marker string, allowedReviewEvents []string) bool {
	return findAllowedReviewMarker(raw, marker, allowedReviewEvents, "", false).Found
}

func findAllowedReviewMarker(raw string, marker string, allowedReviewEvents []string, authorLogin string, allowCleanComment bool) ReviewMarkerResult {
	expectedAuthorLogin := normalizeReviewMarkerLogin(authorLogin)
	var rows []map[string]any
	if err := json.Unmarshal([]byte(raw), &rows); err != nil {
		var pages [][]map[string]any
		if err := json.Unmarshal([]byte(raw), &pages); err != nil {
			if parsedMarker, ok := findReviewIdempotencyMarker(raw, marker); expectedAuthorLogin == "" && len(allowedReviewEvents) == 0 && ok {
				return ReviewMarkerResult{Found: true, Outcome: parsedMarker.Outcome, Body: raw}
			}
			return ReviewMarkerResult{}
		}
		for _, page := range pages {
			rows = append(rows, page...)
		}
	}
	var newest ReviewMarkerResult
	for _, row := range rows {
		body, ok := row["body"].(string)
		if !ok {
			continue
		}
		parsedMarker, ok := findReviewIdempotencyMarker(body, marker)
		if !ok {
			continue
		}
		author := reviewAuthorLogin(row)
		if expectedAuthorLogin != "" && normalizeReviewMarkerLogin(author) != expectedAuthorLogin {
			continue
		}
		event := reviewEventFromStateString(row["state"])
		if reviewMarkerEventAllowedForOutcome(parsedMarker.Outcome, event, allowedReviewEvents, allowCleanComment) {
			newest = ReviewMarkerResult{Found: true, Outcome: parsedMarker.Outcome, Event: event, AuthorLogin: author, Body: body, ReviewID: reviewIDString(row)}
		}
	}
	return newest
}

func reviewMarkerEventAllowedForOutcome(outcome string, event string, allowedReviewEvents []string, allowCleanComment bool) bool {
	if event == "" {
		return false
	}
	if len(allowedReviewEvents) == 0 {
		return true
	}
	if !reviewEventAllowed(event, allowedReviewEvents) {
		return false
	}
	switch outcome {
	case "clean":
		if allowCleanComment && event == "COMMENT" {
			return true
		}
		if reviewEventAllowed("APPROVE", allowedReviewEvents) {
			return event == "APPROVE"
		}
		return event == "COMMENT"
	case "blocking":
		if reviewEventAllowed("REQUEST_CHANGES", allowedReviewEvents) {
			return event == "REQUEST_CHANGES"
		}
		return event == "COMMENT"
	case "non_blocking", "actionable":
		return event == "COMMENT"
	default:
		return false
	}
}

func reviewIDString(row map[string]any) string {
	if id := asString(row["id"]); id != "" {
		return id
	}
	if id := asInt64(row["id"]); id != 0 {
		return fmt.Sprintf("%d", id)
	}
	return ""
}

func reviewAuthorLogin(row map[string]any) string {
	user, _ := row["user"].(map[string]any)
	login, _ := user["login"].(string)
	return login
}

func normalizeReviewMarkerLogin(login string) string {
	return strings.ToLower(strings.TrimSpace(login))
}

func reviewMarkerOutcome(body string, marker string) string {
	if parsedMarker, ok := findReviewIdempotencyMarker(body, marker); ok {
		return parsedMarker.Outcome
	}
	return ""
}

type reviewIdempotencyMarker struct {
	ID      string
	Head    string
	Outcome string
}

var reviewMarkerRE = regexp.MustCompile(`<!--\s*looper:review\s+([^>]*)-->`)

func findReviewIdempotencyMarker(body string, marker string) (reviewIdempotencyMarker, bool) {
	marker = strings.TrimSpace(marker)
	for _, parsedMarker := range parseReviewIdempotencyMarkers(body) {
		if marker == "" || parsedMarker.matches(marker) {
			return parsedMarker, true
		}
	}
	return reviewIdempotencyMarker{}, false
}

func parseReviewIdempotencyMarkers(body string) []reviewIdempotencyMarker {
	matches := reviewMarkerRE.FindAllStringSubmatch(body, -1)
	markers := make([]reviewIdempotencyMarker, 0, len(matches))
	for _, match := range matches {
		if len(match) != 2 {
			continue
		}
		fields := parseReviewMarkerFields(match[1])
		parsedMarker := reviewIdempotencyMarker{ID: fields["id"], Head: fields["head"], Outcome: fields["outcome"]}
		if parsedMarker.ID == "" || parsedMarker.Head == "" || !isValidReviewMarkerOutcome(parsedMarker.Outcome) {
			continue
		}
		markers = append(markers, parsedMarker)
	}
	return markers
}

func isValidReviewMarkerOutcome(outcome string) bool {
	switch outcome {
	case "clean", "non_blocking", "blocking", "actionable":
		return true
	default:
		return false
	}
}

func parseReviewMarkerFields(segment string) map[string]string {
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

func (m reviewIdempotencyMarker) matches(marker string) bool {
	fields := parseReviewMarkerFields(strings.TrimPrefix(marker, "looper:review"))
	if id := fields["id"]; id != "" && id != m.ID {
		return false
	}
	if idPrefix := fields["id_prefix"]; idPrefix != "" && !strings.HasPrefix(m.ID, idPrefix) {
		return false
	}
	if head := fields["head"]; head != "" && head != m.Head {
		return false
	}
	if outcome := fields["outcome"]; outcome != "" && outcome != m.Outcome {
		return false
	}
	return strings.HasPrefix(marker, "looper:review") || strings.Contains(marker, "id=") || strings.Contains(marker, "head=") || strings.Contains(marker, "outcome=")
}

func reviewStateAllowed(raw any, allowedReviewEvents []string) bool {
	event := reviewEventFromStateString(raw)
	return reviewEventAllowed(event, allowedReviewEvents)
}

func reviewEventFromStateString(raw any) string {
	state, _ := raw.(string)
	return reviewEventFromState(state)
}

func reviewEventAllowed(event string, allowedReviewEvents []string) bool {
	if event == "" {
		return false
	}
	for _, allowed := range allowedReviewEvents {
		if strings.EqualFold(event, allowed) {
			return true
		}
	}
	return false
}

func reviewEventFromState(state string) string {
	switch strings.ToUpper(strings.TrimSpace(state)) {
	case "APPROVED":
		return "APPROVE"
	case "COMMENTED":
		return "COMMENT"
	case "CHANGES_REQUESTED":
		return "REQUEST_CHANGES"
	default:
		return ""
	}
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
	result, err := g.runGh(ctx, input.CWD, "", "api", "--paginate", "--slurp", fmt.Sprintf("repos/%s/issues/%d/reactions", input.Repo, input.PRNumber), "-H", "Accept: application/vnd.github+json")
	if err != nil {
		return err
	}
	rows, err := decodeJSONArrayOrPages(result.Stdout)
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

func (g *Gateway) CompareBranches(ctx context.Context, input CompareBranchesInput) (CompareBranchesResult, error) {
	hostname, repo := splitRepoHostname(input.Repo)
	comparison := encodeURIComponent(input.BaseBranch) + "..." + encodeURIComponent(input.HeadBranch)
	args := []string{"api", fmt.Sprintf("repos/%s/compare/%s", repo, comparison)}
	if hostname != "" {
		args = append(args, "--hostname", hostname)
	}
	result, err := g.runGh(ctx, input.CWD, "", args...)
	if err != nil {
		return CompareBranchesResult{}, err
	}
	var payload struct {
		AheadBy      int    `json:"ahead_by"`
		BehindBy     int    `json:"behind_by"`
		Status       string `json:"status"`
		TotalCommits int    `json:"total_commits"`
	}
	if err := json.Unmarshal([]byte(result.Stdout), &payload); err != nil {
		return CompareBranchesResult{}, err
	}
	return CompareBranchesResult{AheadBy: payload.AheadBy, BehindBy: payload.BehindBy, Status: payload.Status, TotalCommits: payload.TotalCommits}, nil
}

func (g *Gateway) UpdatePullRequestTitle(ctx context.Context, input UpdatePullRequestTitleInput) error {
	_, err := g.runGh(ctx, input.CWD, "", "pr", "edit", strconv.FormatInt(input.PRNumber, 10), "--repo", input.Repo, "--title", input.Title)
	return err
}

func (g *Gateway) UpdatePullRequestBody(ctx context.Context, input UpdatePullRequestBodyInput) error {
	_, err := g.runGh(ctx, input.CWD, "", "pr", "edit", strconv.FormatInt(input.PRNumber, 10), "--repo", input.Repo, "--body", input.Body)
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
	if snapshot := discoverySnapshotFromContext(ctx); snapshot != nil {
		return snapshot.getCurrentUserLogin(ctx, cwd)
	}
	return g.getCurrentUserLoginRaw(ctx, cwd)
}

func (g *Gateway) GetCurrentUserIdentity(ctx context.Context, cwd string) (CurrentUserIdentity, error) {
	result, err := g.runGh(ctx, cwd, "", "api", "user", "--jq", `{login: .login, id: .id}`)
	if err != nil {
		if isUserLoginUnsupportedForCurrentToken(err) {
			login, viewErr := g.getViewerLogin(ctx, cwd, "")
			if viewErr != nil {
				return CurrentUserIdentity{}, viewErr
			}
			return CurrentUserIdentity{Login: login}, nil
		}
		return CurrentUserIdentity{}, err
	}
	row, err := decodeJSONObject(result.Stdout)
	if err != nil {
		return CurrentUserIdentity{}, err
	}
	return CurrentUserIdentity{Login: strings.TrimSpace(asString(row["login"])), NumericID: asInt64(row["id"])}, nil
}

func (g *Gateway) getCurrentUserLoginRaw(ctx context.Context, cwd string) (string, error) {
	result, err := g.runGh(ctx, cwd, "", "api", "user", "--jq", ".login")
	if err != nil {
		if isUserLoginUnsupportedForCurrentToken(err) {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(result.Stdout), nil
}

func (g *Gateway) GetCurrentUserLoginForRepo(ctx context.Context, repo string, cwd string) (string, error) {
	hostname, _ := splitRepoHostname(repo)
	args := []string{"api", "user", "--jq", ".login"}
	if hostname != "" {
		args = append(args, "--hostname", hostname)
	}
	result, err := g.runGh(ctx, cwd, "", args...)
	if err != nil {
		if isUserLoginUnsupportedForCurrentToken(err) {
			return g.getViewerLogin(ctx, cwd, hostname)
		}
		return "", err
	}
	return strings.TrimSpace(result.Stdout), nil
}

func (g *Gateway) getViewerLogin(ctx context.Context, cwd string, hostname string) (string, error) {
	args := []string{"api", "graphql", "-f", `query=query { viewer { login } }`}
	if hostname != "" {
		args = append(args, "--hostname", hostname)
	}
	result, err := g.runGh(ctx, cwd, "", args...)
	if err != nil {
		return "", err
	}
	row, err := decodeJSONObject(result.Stdout)
	if err != nil {
		return "", err
	}
	data, _ := row["data"].(map[string]any)
	viewer, _ := data["viewer"].(map[string]any)
	return strings.TrimSpace(asString(viewer["login"])), nil
}

func isUserLoginUnsupportedForCurrentToken(err error) bool {
	var commandErr *shell.CommandExecutionError
	if !errors.As(err, &commandErr) {
		return false
	}
	combined := strings.ToLower(strings.TrimSpace(strings.Join([]string{commandErr.Error(), commandErr.Result.Stdout, commandErr.Result.Stderr}, "\n")))
	return strings.Contains(combined, "resource not accessible by integration")
}

func (g *Gateway) DetectCurrentRepository(ctx context.Context, cwd string) (string, error) {
	result, err := g.runGh(ctx, cwd, "", "repo", "view", "--json", "nameWithOwner,url")
	if err != nil {
		return "", err
	}
	row, err := decodeJSONObject(result.Stdout)
	if err != nil {
		return "", err
	}
	repo := hostQualifiedRepo(asString(row["nameWithOwner"]), asString(row["url"]))
	if err := validateGitHubRepoSlug(repo); err != nil {
		return "", err
	}
	return repo, nil
}

func (g *Gateway) InitializeLabels(ctx context.Context, input InitializeLabelsInput) (LabelInitResult, error) {
	repo := strings.TrimSpace(input.Repo)
	if err := validateGitHubRepoSlug(repo); err != nil {
		return LabelInitResult{}, err
	}

	existing, err := g.listRepositoryLabels(ctx, repo, input.CWD)
	if err != nil {
		return LabelInitResult{}, err
	}

	result := LabelInitResult{Repo: repo, DryRun: input.DryRun, Labels: make([]LabelInitItem, 0, len(StandardLooperLabels()))}
	for _, definition := range StandardLooperLabels() {
		item := LabelInitItem{Name: definition.Name, Color: definition.Color, Description: definition.Description}
		current, ok := existing[strings.ToLower(definition.Name)]
		switch {
		case !ok:
			item.Status = "created"
			if !input.DryRun {
				_, err = g.runGh(ctx, input.CWD, "", "label", "create", definition.Name, "--repo", repo, "--color", definition.Color, "--description", definition.Description)
			}
		case normalizeLabelColor(current.Color) == normalizeLabelColor(definition.Color) && strings.TrimSpace(current.Description) == definition.Description:
			item.Status = "skipped"
		default:
			item.Status = "updated"
			if !input.DryRun {
				_, err = g.runGh(ctx, input.CWD, "", "label", "edit", current.Name, "--repo", repo, "--color", definition.Color, "--description", definition.Description)
			}
		}

		if err != nil {
			item.Status = "failed"
			item.Error = err.Error()
			err = nil
		}
		result.Labels = append(result.Labels, item)
		incrementLabelSummary(&result.Summary, item.Status)
	}

	if result.Summary.Failed > 0 {
		return result, fmt.Errorf("%d label mutation(s) failed", result.Summary.Failed)
	}
	return result, nil
}

func (g *Gateway) CapturePullRequestSnapshot(ctx context.Context, input CapturePullRequestSnapshotInput) (storage.PullRequestSnapshotRecord, error) {
	detail, err := g.ViewPullRequest(ctx, ViewPullRequestInput{Repo: input.Repo, PRNumber: input.PRNumber, CWD: input.CWD})
	if err != nil {
		return storage.PullRequestSnapshotRecord{}, err
	}
	diff, err := g.GetPullRequestDiff(ctx, GetPullRequestDiffInput{Repo: input.Repo, PRNumber: input.PRNumber, CWD: input.CWD})
	if err != nil {
		if !errors.Is(err, ErrDiffTooLarge) {
			return storage.PullRequestSnapshotRecord{}, err
		}
	}
	capturedAt := strings.TrimSpace(input.CapturedAt)
	if capturedAt == "" {
		capturedAt = g.now().UTC().Format(javaScriptISOStringLayout)
	}
	payloadMap := map[string]any{"detail": detail, "diff": diff}
	if errors.Is(err, ErrDiffTooLarge) {
		payloadMap["diffTruncated"] = true
		payloadMap["diffTruncationReason"] = "github_too_large"
	}
	payload, err := json.Marshal(payloadMap)
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
	out := make([]map[string]any, 0, 100)
	cursor := ""
	for {
		nodes, nextCursor, hasNextPage, err := g.fetchReviewThreadsSummaryPage(ctx, cwd, owner, name, prNumber, cursor)
		if err != nil {
			return nil, err
		}
		for _, node := range nodes {
			normalized, ok := normalizeReviewThread(node)
			if ok {
				threadRow, _ := node.(map[string]any)
				commentsRow, _ := threadRow["comments"].(map[string]any)
				commentNodes, _ := commentsRow["nodes"].([]any)
				pageInfo, _ := commentsRow["pageInfo"].(map[string]any)
				commentCursor := asString(pageInfo["endCursor"])
				hasMoreComments := asBool(pageInfo["hasNextPage"])
				allCommentNodes := append([]any(nil), commentNodes...)
				for hasMoreComments {
					moreComments, nextCommentCursor, hasMore, err := g.fetchReviewThreadCommentsPage(ctx, cwd, asString(normalized["threadId"]), commentCursor)
					if err != nil {
						return nil, err
					}
					allCommentNodes = append(allCommentNodes, moreComments...)
					commentCursor = nextCommentCursor
					hasMoreComments = hasMore
				}
				if fingerprint := reviewThreadFingerprintFromNodes(allCommentNodes); fingerprint != "" {
					normalized["threadFingerprint"] = fingerprint
				}
				out = append(out, normalized)
			}
		}
		if !hasNextPage || nextCursor == "" {
			break
		}
		cursor = nextCursor
	}
	return out, nil
}

func (g *Gateway) fetchReviewThreadPage(ctx context.Context, cwd, owner, name string, prNumber int64, limit int, cursor string) ([]any, string, bool, error) {
	args := []string{"api", "graphql", "-f", "query=" + strings.Join([]string{
		"query($owner: String!, $name: String!, $prNumber: Int!, $limit: Int!, $after: String) {",
		"  repository(owner: $owner, name: $name) {",
		"    pullRequest(number: $prNumber) {",
		"      reviewThreads(first: $limit, after: $after) {",
		"        nodes {",
		"          id isResolved path line",
		"          comments(first: 100) {",
		"            nodes {",
		"              id body createdAt updatedAt path line url authorAssociation",
		"              author { login }",
		"              originalCommit { oid }",
		"              commit { oid }",
		"            }",
		"            pageInfo { hasNextPage endCursor }",
		"          }",
		"        }",
		"        pageInfo { hasNextPage endCursor }",
		"      }",
		"    }",
		"  }",
		"}",
	}, "\n"), "-F", "owner=" + owner, "-F", "name=" + name, "-F", fmt.Sprintf("prNumber=%d", prNumber), "-F", fmt.Sprintf("limit=%d", limit)}
	if cursor != "" {
		args = append(args, "-F", "after="+cursor)
	}
	result, err := g.runGh(ctx, cwd, "", args...)
	if err != nil {
		return nil, "", false, err
	}
	return decodeReviewThreadsResponse(result.Stdout)
}

func (g *Gateway) fetchReviewThreadsSummaryPage(ctx context.Context, cwd, owner, name string, prNumber int64, cursor string) ([]any, string, bool, error) {
	args := []string{"api", "graphql", "-f", "query=" + strings.Join([]string{
		"query($owner: String!, $name: String!, $prNumber: Int!, $after: String) {",
		"  repository(owner: $owner, name: $name) {",
		"    pullRequest(number: $prNumber) {",
		"      reviewThreads(first: 100, after: $after) {",
		"        nodes {",
		"          id",
		"          isResolved",
		"          path",
		"          line",
		"          comments(first: 100) {",
		"            nodes { id body updatedAt url path line authorAssociation author { login } }",
		"            pageInfo { hasNextPage endCursor }",
		"          }",
		"        }",
		"        pageInfo { hasNextPage endCursor }",
		"      }",
		"    }",
		"  }",
		"}",
	}, "\n"), "-F", "owner=" + owner, "-F", "name=" + name, "-F", fmt.Sprintf("prNumber=%d", prNumber)}
	if cursor != "" {
		args = append(args, "-F", "after="+cursor)
	}
	result, err := g.runGh(ctx, cwd, "", args...)
	if err != nil {
		return nil, "", false, err
	}
	return decodeReviewThreadsResponse(result.Stdout)
}

func (g *Gateway) fetchReviewThreadCommentsPage(ctx context.Context, cwd, threadID, cursor string) ([]any, string, bool, error) {
	args := []string{"api", "graphql", "-f", "query=" + strings.Join([]string{
		"query($threadId: ID!, $after: String) {",
		"  node(id: $threadId) {",
		"    ... on PullRequestReviewThread {",
		"      comments(first: 100, after: $after) {",
		"        nodes {",
		"          id body createdAt updatedAt path line url authorAssociation",
		"          author { login }",
		"          originalCommit { oid }",
		"          commit { oid }",
		"        }",
		"        pageInfo { hasNextPage endCursor }",
		"      }",
		"    }",
		"  }",
		"}",
	}, "\n"), "-F", "threadId=" + threadID}
	if cursor != "" {
		args = append(args, "-F", "after="+cursor)
	}
	result, err := g.runGh(ctx, cwd, "", args...)
	if err != nil {
		return nil, "", false, err
	}
	row, err := decodeJSONObject(result.Stdout)
	if err != nil {
		return nil, "", false, err
	}
	data, _ := row["data"].(map[string]any)
	node, _ := data["node"].(map[string]any)
	commentsRow, _ := node["comments"].(map[string]any)
	nodes, _ := commentsRow["nodes"].([]any)
	pageInfo, _ := commentsRow["pageInfo"].(map[string]any)
	return nodes, asString(pageInfo["endCursor"]), asBool(pageInfo["hasNextPage"]), nil
}

func decodeReviewThreadsResponse(stdout string) ([]any, string, bool, error) {
	row, err := decodeJSONObject(stdout)
	if err != nil {
		return nil, "", false, err
	}
	data, _ := row["data"].(map[string]any)
	repoRow, _ := data["repository"].(map[string]any)
	prRow, _ := repoRow["pullRequest"].(map[string]any)
	threadsRow, _ := prRow["reviewThreads"].(map[string]any)
	nodes, _ := threadsRow["nodes"].([]any)
	pageInfo, _ := threadsRow["pageInfo"].(map[string]any)
	return nodes, asString(pageInfo["endCursor"]), asBool(pageInfo["hasNextPage"]), nil
}

func (g *Gateway) fetchLinkedPullRequestsPage(ctx context.Context, cwd, hostname, owner, repo string, issueNumber int64, cursor string) ([]map[string]any, string, bool, error) {
	args := []string{"api", "graphql", "-f", "query=" + strings.Join([]string{
		"query($owner: String!, $repo: String!, $number: Int!, $after: String) {",
		"  repository(owner: $owner, name: $repo) {",
		"    issue(number: $number) {",
		"      closedByPullRequestsReferences(first: 20, after: $after) {",
		"        nodes {",
		"          number",
		"          state",
		"          mergedAt",
		"          mergeCommit { oid }",
		"        }",
		"        pageInfo { hasNextPage endCursor }",
		"      }",
		"    }",
		"  }",
		"}",
	}, "\n"), "-F", "owner=" + owner, "-F", "repo=" + repo, "-F", fmt.Sprintf("number=%d", issueNumber)}
	if cursor != "" {
		args = append(args, "-F", "after="+cursor)
	}
	if strings.TrimSpace(hostname) != "" {
		args = append(args, "--hostname", hostname)
	}
	result, err := g.runGh(ctx, cwd, "", args...)
	if err != nil {
		return nil, "", false, err
	}
	row, err := decodeJSONObject(result.Stdout)
	if err != nil {
		return nil, "", false, err
	}
	data, _ := row["data"].(map[string]any)
	repository, _ := data["repository"].(map[string]any)
	issue, _ := repository["issue"].(map[string]any)
	refs, _ := issue["closedByPullRequestsReferences"].(map[string]any)
	nodesAny, _ := refs["nodes"].([]any)
	nodes := make([]map[string]any, 0, len(nodesAny))
	for _, node := range nodesAny {
		nodeRow, _ := node.(map[string]any)
		if nodeRow == nil {
			continue
		}
		nodes = append(nodes, nodeRow)
	}
	pageInfo, _ := refs["pageInfo"].(map[string]any)
	return nodes, asString(pageInfo["endCursor"]), asBool(pageInfo["hasNextPage"]), nil
}

func appendReviewThreadComments(dst []ReviewThreadComment, nodes []any) []ReviewThreadComment {
	for _, commentNode := range nodes {
		commentRow, _ := commentNode.(map[string]any)
		commentID := asString(commentRow["id"])
		if commentID == "" {
			continue
		}
		dst = append(dst, ReviewThreadComment{ID: commentID, Body: asString(commentRow["body"]), Author: extractAuthor(commentRow["author"]), AuthorAssociation: asString(commentRow["authorAssociation"]), CreatedAt: asString(commentRow["createdAt"]), UpdatedAt: asString(commentRow["updatedAt"]), Path: asString(commentRow["path"]), Line: asInt64(commentRow["line"]), OriginalCommitOID: extractOID(commentRow["originalCommit"]), CommitOID: extractOID(commentRow["commit"]), URL: asString(commentRow["url"])})
	}
	return dst
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
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

func (g *Gateway) listRepositoryLabels(ctx context.Context, repo string, cwd string) (map[string]LabelDefinition, error) {
	result, err := g.runGh(ctx, cwd, "", "label", "list", "--repo", repo, "--limit", "1000", "--json", "name,color,description")
	if err != nil {
		return nil, err
	}
	rows, err := decodeJSONArray(result.Stdout)
	if err != nil {
		return nil, err
	}
	out := make(map[string]LabelDefinition, len(rows))
	for _, row := range rows {
		name := strings.TrimSpace(asString(row["name"]))
		if name == "" {
			continue
		}
		out[strings.ToLower(name)] = LabelDefinition{Name: name, Color: normalizeLabelColor(asString(row["color"])), Description: strings.TrimSpace(asString(row["description"]))}
	}
	return out, nil
}

func (g *Gateway) runGh(ctx context.Context, cwd, stdin string, args ...string) (shell.Result, error) {
	return g.runGhWithTimeout(ctx, cwd, stdin, defaultGhCommandTimeout, args...)
}

func (g *Gateway) runGhWithTimeout(ctx context.Context, cwd, stdin string, timeout time.Duration, args ...string) (shell.Result, error) {
	result, err := g.ghRun(ctx, shell.Options{Command: g.ghPath, Args: args, CWD: valueOr(strings.TrimSpace(cwd), g.cwd), Stdin: stdin, Timeout: timeout})
	if err != nil && isTransientGitHubMessage(strings.Join([]string{err.Error(), result.Stdout, result.Stderr}, "\n")) {
		return result, &TransientError{Err: err}
	}
	return result, err
}

func isDiffTooLargeError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "http 406") || strings.Contains(message, "too_large") || strings.Contains(message, "diff exceeded maximum number of lines")
}

func defaultLimit(limit int) int {
	if limit <= 0 {
		return 30
	}
	return limit
}

func withoutJSONField(fields []string, field string) []string {
	out := make([]string, 0, len(fields))
	for _, candidate := range fields {
		if candidate != field {
			out = append(out, candidate)
		}
	}
	return out
}

func parseRepo(repo string) (string, string, error) {
	parts := strings.Split(repo, "/")
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return "", "", fmt.Errorf("invalid GitHub repo: %s", repo)
	}
	return parts[0], parts[1], nil
}

func validateGitHubRepoSlug(repo string) error {
	parts := strings.Split(strings.TrimSpace(repo), "/")
	if (len(parts) != 2 && len(parts) != 3) || strings.TrimSpace(parts[len(parts)-2]) == "" || strings.TrimSpace(parts[len(parts)-1]) == "" {
		return fmt.Errorf("invalid GitHub repo: %s", repo)
	}
	if len(parts) == 3 && strings.TrimSpace(parts[0]) == "" {
		return fmt.Errorf("invalid GitHub repo: %s", repo)
	}
	return nil
}

func dependencyRepo(value any, repositoryURL string, fallback string) string {
	repository, _ := value.(map[string]any)
	if fullName := strings.TrimSpace(asString(repository["full_name"])); fullName != "" {
		return hostQualifiedRepo(fullName, firstNonEmpty(repositoryURL, asString(repository["url"])))
	}
	if apiURL := strings.TrimSpace(firstNonEmpty(repositoryURL, asString(repository["url"]))); apiURL != "" {
		if parsed, err := url.Parse(apiURL); err == nil {
			trimmed := strings.Trim(parsed.Path, "/")
			if index := strings.Index(trimmed, "repos/"); index >= 0 {
				nameWithOwner := strings.TrimPrefix(trimmed[index:], "repos/")
				if parsed.Hostname() != "" && parsed.Hostname() != "api.github.com" && parsed.Hostname() != "github.com" {
					return parsed.Hostname() + "/" + nameWithOwner
				}
				return nameWithOwner
			}
		}
	}
	return strings.TrimSpace(fallback)
}

func hostQualifiedRepo(nameWithOwner string, repoURL string) string {
	repo := strings.TrimSpace(nameWithOwner)
	parsed, err := url.Parse(strings.TrimSpace(repoURL))
	if err != nil || parsed.Hostname() == "" || parsed.Hostname() == "github.com" || parsed.Hostname() == "api.github.com" {
		return repo
	}
	return parsed.Hostname() + "/" + repo
}

func splitRepoHostname(repo string) (string, string) {
	parts := strings.Split(strings.TrimSpace(repo), "/")
	if len(parts) == 3 && strings.TrimSpace(parts[0]) != "" {
		return strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]) + "/" + strings.TrimSpace(parts[2])
	}
	return "", strings.TrimSpace(repo)
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
	if author := extractAuthor(first["author"]); author != "" {
		out["author"] = author
	}
	if url := asString(first["url"]); url != "" {
		out["url"] = url
	}
	if path := asString(first["path"]); path != "" {
		out["path"] = path
	} else if path := asString(row["path"]); path != "" {
		out["path"] = path
	}
	if line := asInt64(first["line"]); line > 0 {
		out["line"] = line
	} else if line := asInt64(row["line"]); line > 0 {
		out["line"] = line
	}
	if fingerprint := reviewThreadFingerprintFromNodes(nodes); fingerprint != "" {
		out["threadFingerprint"] = fingerprint
	}
	return out, true
}

func reviewThreadFingerprintFromNodes(nodes []any) string {
	parts := make([]string, 0, len(nodes))
	for _, node := range nodes {
		comment, _ := node.(map[string]any)
		if comment == nil {
			continue
		}
		if strings.Contains(asString(comment["body"]), "<!-- looper-fixer-reply ") {
			continue
		}
		id := strings.TrimSpace(asString(comment["id"]))
		updatedAt := strings.TrimSpace(asString(comment["updatedAt"]))
		if id == "" && updatedAt == "" {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s@%s", id, updatedAt))
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "|")
}

func validateCloseIssueStateReason(value string) (string, error) {
	normalized := strings.TrimSpace(value)
	switch normalized {
	case "completed", "not_planned":
		return normalized, nil
	default:
		return "", fmt.Errorf("invalid issue close reason %q", value)
	}
}

func (g *Gateway) viewIssueState(ctx context.Context, repo string, issueNumber int64, cwd string) (string, error) {
	result, err := g.runGh(ctx, cwd, "", "api", fmt.Sprintf("repos/%s/issues/%d", repo, issueNumber), "--jq", ".state")
	if err != nil {
		return "", err
	}
	return strings.ToLower(strings.TrimSpace(result.Stdout)), nil
}

func (g *Gateway) viewPullRequestState(ctx context.Context, repo string, prNumber int64, cwd string) (string, error) {
	result, err := g.runGh(ctx, cwd, "", "pr", "view", strconv.FormatInt(prNumber, 10), "--repo", repo, "--json", "state", "--jq", ".state")
	if err != nil {
		return "", err
	}
	return strings.ToLower(strings.TrimSpace(result.Stdout)), nil
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

func extractDependencyIssue(value map[string]any, defaultRepo string) DependencyIssue {
	repositoryURL := asString(value["repository_url"])
	repo := extractIssueRepository(value["repository"])
	repo = completeIssueRepository(repo, repositoryURL, defaultRepo)
	return DependencyIssue{
		ID:            asInt64(value["id"]),
		Number:        asInt64(value["number"]),
		Title:         asString(value["title"]),
		URL:           asString(value["url"]),
		HTMLURL:       asString(value["html_url"]),
		RepositoryURL: repositoryURL,
		State:         asString(value["state"]),
		StateReason:   asString(value["state_reason"]),
		Repository:    repo,
	}
}

func completeIssueRepository(repo IssueRepository, repositoryURL string, defaultRepo string) IssueRepository {
	fullName, name := parseRepositoryIdentity(repositoryURL)
	if fullName == "" {
		fullName = strings.TrimSpace(defaultRepo)
		_, fallbackRepo := splitRepoHostname(fullName)
		_, name = splitRepoOwnerName(fallbackRepo)
	}
	if repo.Name == "" {
		repo.Name = name
	}
	if repo.FullName == "" {
		repo.FullName = fullName
	}
	if repo.URL == "" {
		repo.URL = repositoryURL
	}
	return repo
}

func parseRepositoryIdentity(repositoryURL string) (fullName string, name string) {
	parsed, err := url.Parse(strings.TrimSpace(repositoryURL))
	if err != nil {
		return "", ""
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	switch {
	case len(parts) >= 3 && parts[0] == "repos":
		parts = parts[:3]
	case len(parts) >= 5 && parts[0] == "api" && parts[1] == "v3" && parts[2] == "repos":
		parts = parts[2:5]
	default:
		return "", ""
	}
	fullName = parts[1] + "/" + parts[2]
	if hostname := strings.TrimSpace(parsed.Hostname()); hostname != "" && hostname != "github.com" && hostname != "api.github.com" {
		fullName = hostname + "/" + fullName
	}
	return fullName, parts[2]
}

func extractIssueRepository(value any) IssueRepository {
	row, _ := value.(map[string]any)
	if row == nil {
		return IssueRepository{}
	}
	return IssueRepository{
		Name:     asString(row["name"]),
		FullName: asString(row["full_name"]),
		URL:      asString(row["url"]),
		HTMLURL:  asString(row["html_url"]),
	}
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

func StandardLooperLabels() []LabelDefinition {
	return []LabelDefinition{
		{Name: "looper:plan", Color: resolveLabelColor("looper:plan"), Description: resolveLabelDescription("looper:plan")},
		{Name: specpr.ReviewingLabel, Color: resolveLabelColor(specpr.ReviewingLabel), Description: resolveLabelDescription(specpr.ReviewingLabel)},
		{Name: specpr.ReadyLabel, Color: resolveLabelColor(specpr.ReadyLabel), Description: resolveLabelDescription(specpr.ReadyLabel)},
		{Name: specpr.NeedsHumanLabel, Color: resolveLabelColor(specpr.NeedsHumanLabel), Description: resolveLabelDescription(specpr.NeedsHumanLabel)},
	}
}

func normalizeLabelColor(value string) string {
	return strings.TrimPrefix(strings.ToLower(strings.TrimSpace(value)), "#")
}

func incrementLabelSummary(summary *LabelInitSummary, status string) {
	switch status {
	case "created":
		summary.Created++
	case "updated":
		summary.Updated++
	case "skipped":
		summary.Skipped++
	case "failed":
		summary.Failed++
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

func decodeJSONArrayOrPages(value string) ([]map[string]any, error) {
	rows, err := decodeJSONArray(value)
	if err == nil {
		return rows, nil
	}
	var pages [][]map[string]any
	if pageErr := json.Unmarshal([]byte(value), &pages); pageErr != nil {
		return nil, err
	}
	for _, page := range pages {
		rows = append(rows, page...)
	}
	if rows == nil {
		return []map[string]any{}, nil
	}
	return rows, nil
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

func extractCommentInfos(value any) []CommentInfo {
	items := toObjectSlice(value)
	if len(items) == 0 {
		if rows, ok := value.([]map[string]any); ok {
			items = append(items, rows...)
		}
	}
	out := make([]CommentInfo, 0, len(items))
	for _, row := range items {
		out = append(out, CommentInfo{
			ID:                asInt64(firstNonNil(row["id"], row["databaseId"])),
			Author:            extractAuthor(firstNonNil(row["author"], row["user"])),
			AuthorAssociation: firstNonEmpty(asString(row["authorAssociation"]), asString(row["author_association"])),
			Body:              asString(row["body"]),
			CreatedAt:         firstNonEmpty(asString(row["createdAt"]), asString(row["created_at"])),
			UpdatedAt:         firstNonEmpty(asString(row["updatedAt"]), asString(row["updated_at"])),
			URL:               firstNonEmpty(asString(row["url"]), asString(row["html_url"])),
		})
	}
	return out
}

func splitRepoOwnerName(repo string) (string, string) {
	parts := strings.SplitN(strings.TrimSpace(repo), "/", 2)
	if len(parts) != 2 {
		return strings.TrimSpace(repo), ""
	}
	return parts[0], parts[1]
}

func digNodes(row map[string]any, path ...string) []map[string]any {
	current := any(row)
	for _, key := range path {
		object, _ := current.(map[string]any)
		current = object[key]
	}
	object, _ := current.(map[string]any)
	return toObjectSlice(object["nodes"])
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

func extractOID(value any) string {
	row, ok := value.(map[string]any)
	if !ok {
		return ""
	}
	return asString(row["oid"])
}

func extractReviewRequestLogins(value any) []string {
	users := extractReviewRequestUsers(value)
	if users == nil {
		return nil
	}
	out := make([]string, 0, len(users))
	for _, user := range users {
		if user.Login != "" {
			out = append(out, user.Login)
		}
	}
	return out
}

func extractReviewRequestUsers(value any) []GitHubUser {
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	out := make([]GitHubUser, 0, len(items))
	for _, item := range items {
		if user, ok := extractReviewRequestUser(item); ok {
			out = append(out, user)
		}
	}
	return out
}

func extractReviewRequestUser(value any) (GitHubUser, bool) {
	row, ok := value.(map[string]any)
	if !ok {
		return GitHubUser{}, false
	}
	if asString(row["__typename"]) == "User" {
		login := asString(row["login"])
		if login == "" {
			return GitHubUser{}, false
		}
		return GitHubUser{Login: login, ID: asInt64(firstNonNil(row["databaseId"], row["id"]))}, true
	}
	reviewer, _ := row["requestedReviewer"].(map[string]any)
	if asString(reviewer["__typename"]) == "User" {
		login := asString(reviewer["login"])
		if login == "" {
			return GitHubUser{}, false
		}
		return GitHubUser{Login: login, ID: asInt64(firstNonNil(reviewer["databaseId"], reviewer["id"]))}, true
	}
	return GitHubUser{}, false
}

func extractActorLogins(value any) []string {
	users := extractActorUsers(value)
	out := make([]string, 0, len(users))
	for _, user := range users {
		if user.Login != "" {
			out = append(out, user.Login)
		}
	}
	return out
}

func extractActorUsers(value any) []GitHubUser {
	items, ok := value.([]any)
	if !ok {
		return []GitHubUser{}
	}
	out := make([]GitHubUser, 0, len(items))
	for _, item := range items {
		if login := extractAuthor(item); login != "" {
			row, _ := item.(map[string]any)
			out = append(out, GitHubUser{Login: login, ID: asInt64(firstNonNil(row["databaseId"], row["id"]))})
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

func extractLabelNamesFromConnection(value any) []string {
	row, _ := value.(map[string]any)
	if row == nil {
		return []string{}
	}
	return extractLabelNames(row["nodes"])
}

func extractReviewNodesFromConnection(value any) []map[string]any {
	row, _ := value.(map[string]any)
	if row == nil {
		return []map[string]any{}
	}
	return toObjectSlice(row["nodes"])
}

func normalizeGitHubLogin(login string) string {
	return strings.ToLower(strings.TrimSpace(login))
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

func boolPtrFromValue(value any) *bool {
	typed, ok := value.(bool)
	if !ok {
		return nil
	}
	return &typed
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

func nestedString(value map[string]any, path ...string) string {
	current := any(value)
	for _, part := range path {
		next, ok := current.(map[string]any)
		if !ok {
			return ""
		}
		current = next[part]
	}
	return asString(current)
}

func extractAutoMerge(value any) *PullRequestAutoMerge {
	row, ok := value.(map[string]any)
	if !ok || row == nil {
		return nil
	}
	return &PullRequestAutoMerge{
		EnabledBy:   extractAuthor(firstNonNil(row["enabled_by"], row["enabledBy"])),
		MergeMethod: firstNonEmpty(asString(row["merge_method"]), asString(row["mergeMethod"])),
	}
}

func uniqueStrings(values []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		normalized := strings.ToLower(strings.TrimSpace(value))
		if normalized == "" {
			continue
		}
		if _, ok := seen[normalized]; ok {
			continue
		}
		seen[normalized] = struct{}{}
		out = append(out, strings.TrimSpace(value))
	}
	return out
}

func ternary[T any](condition bool, truthy, falsy T) T {
	if condition {
		return truthy
	}
	return falsy
}
