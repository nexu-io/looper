package github

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
)

type DiscoverySnapshotOptions struct {
	PullRequestLimit int
	IssueLimit       int
}

type discoverySnapshotContextKey struct{}

type DiscoveryTickState struct {
	mu         sync.Mutex
	userLogins map[string]string
}

type DiscoverySnapshot struct {
	gateway    *Gateway
	tick       *DiscoveryTickState
	prLimit    int
	issueLimit int

	mu               sync.Mutex
	openPRs          []PullRequestSummary
	openPRsFetched   bool
	openPRsFetchRepo string
	openPRsFetchCWD  string
	openPRsLimit     int

	openIssues          []IssueSummary
	openIssuesFetched   bool
	openIssuesFetchRepo string
	openIssuesFetchCWD  string
	openIssuesLimit     int

	prDetails map[string]PullRequestDetail
}

func NewDiscoveryTickState() *DiscoveryTickState {
	return &DiscoveryTickState{userLogins: map[string]string{}}
}

func NewDiscoverySnapshot(gateway *Gateway, tick *DiscoveryTickState, options DiscoverySnapshotOptions) *DiscoverySnapshot {
	if gateway == nil {
		return nil
	}
	if tick == nil {
		tick = NewDiscoveryTickState()
	}
	return &DiscoverySnapshot{gateway: gateway, tick: tick, prLimit: max(defaultLimit(options.PullRequestLimit), 30), issueLimit: max(defaultLimit(options.IssueLimit), 30), prDetails: map[string]PullRequestDetail{}}
}

func ContextWithDiscoverySnapshot(ctx context.Context, snapshot *DiscoverySnapshot) context.Context {
	if snapshot == nil {
		return ctx
	}
	return context.WithValue(ctx, discoverySnapshotContextKey{}, snapshot)
}

func discoverySnapshotFromContext(ctx context.Context) *DiscoverySnapshot {
	if ctx == nil {
		return nil
	}
	snapshot, _ := ctx.Value(discoverySnapshotContextKey{}).(*DiscoverySnapshot)
	return snapshot
}

func (s *DiscoverySnapshot) listOpenPullRequests(ctx context.Context, input ListOpenPullRequestsInput) ([]PullRequestSummary, error) {
	if err := s.ensureOpenPullRequests(ctx, input); err != nil {
		return nil, err
	}
	prs := s.filteredPullRequests(input)
	if s.shouldFallbackToFilteredPullRequestQuery(input, len(prs)) {
		return s.gateway.listOpenPullRequestsRaw(ctx, input)
	}
	return limitPullRequests(prs, input.Limit), nil
}

func (s *DiscoverySnapshot) ensureOpenPullRequests(ctx context.Context, input ListOpenPullRequestsInput) error {
	s.mu.Lock()
	ready := s.openPRsFetched && s.openPRsFetchRepo == input.Repo && s.openPRsFetchCWD == input.CWD
	s.mu.Unlock()
	if ready {
		return nil
	}
	requiredLimit := s.prLimit
	prs, err := s.gateway.listOpenPullRequestsRaw(ctx, ListOpenPullRequestsInput{Repo: input.Repo, CWD: input.CWD, Limit: requiredLimit, Timeout: input.Timeout})
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.openPRs = prs
	s.openPRsFetched = true
	s.openPRsFetchRepo = input.Repo
	s.openPRsFetchCWD = input.CWD
	s.openPRsLimit = requiredLimit
	s.mu.Unlock()
	return nil
}

func (s *DiscoverySnapshot) listOpenIssues(ctx context.Context, input ListOpenIssuesInput) ([]IssueSummary, error) {
	if err := s.ensureOpenIssues(ctx, input); err != nil {
		return nil, err
	}
	issues := s.filteredIssues(input)
	if s.shouldFallbackToFilteredIssueQuery(input, len(issues)) {
		return s.gateway.listOpenIssuesRaw(ctx, input)
	}
	return limitIssues(issues, input.Limit), nil
}

func (s *DiscoverySnapshot) ensureOpenIssues(ctx context.Context, input ListOpenIssuesInput) error {
	s.mu.Lock()
	ready := s.openIssuesFetched && s.openIssuesFetchRepo == input.Repo && s.openIssuesFetchCWD == input.CWD
	s.mu.Unlock()
	if ready {
		return nil
	}
	requiredLimit := s.issueLimit
	issues, err := s.gateway.listOpenIssuesRaw(ctx, ListOpenIssuesInput{Repo: input.Repo, CWD: input.CWD, Limit: requiredLimit})
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.openIssues = issues
	s.openIssuesFetched = true
	s.openIssuesFetchRepo = input.Repo
	s.openIssuesFetchCWD = input.CWD
	s.openIssuesLimit = requiredLimit
	s.mu.Unlock()
	return nil
}

func (s *DiscoverySnapshot) viewPullRequest(ctx context.Context, input ViewPullRequestInput) (PullRequestDetail, error) {
	key := snapshotPullRequestDetailKey(input)
	s.mu.Lock()
	if detail, ok := s.prDetails[key]; ok {
		s.mu.Unlock()
		return detail, nil
	}
	s.mu.Unlock()
	detail, err := s.gateway.viewPullRequestRaw(ctx, input)
	if err != nil {
		return PullRequestDetail{}, err
	}
	s.mu.Lock()
	s.prDetails[key] = detail
	s.mu.Unlock()
	return detail, nil
}

func (s *DiscoverySnapshot) filteredPullRequests(input ListOpenPullRequestsInput) []PullRequestSummary {
	s.mu.Lock()
	prs := append([]PullRequestSummary(nil), s.openPRs...)
	s.mu.Unlock()
	return filterPullRequests(prs, input)
}

func (s *DiscoverySnapshot) filteredIssues(input ListOpenIssuesInput) []IssueSummary {
	s.mu.Lock()
	issues := append([]IssueSummary(nil), s.openIssues...)
	s.mu.Unlock()
	return filterIssues(issues, input)
}

func (s *DiscoverySnapshot) shouldFallbackToFilteredPullRequestQuery(input ListOpenPullRequestsInput, filteredCount int) bool {
	if !hasPullRequestFilters(input) {
		return defaultLimit(input.Limit) > s.prLimit
	}
	if filteredCount >= defaultLimit(input.Limit) {
		return false
	}
	s.mu.Lock()
	truncated := len(s.openPRs) >= s.openPRsLimit
	s.mu.Unlock()
	return truncated
}

func (s *DiscoverySnapshot) shouldFallbackToFilteredIssueQuery(input ListOpenIssuesInput, filteredCount int) bool {
	if !hasIssueFilters(input) {
		return defaultLimit(input.Limit) > s.issueLimit
	}
	if filteredCount >= defaultLimit(input.Limit) {
		return false
	}
	s.mu.Lock()
	truncated := len(s.openIssues) >= s.openIssuesLimit
	s.mu.Unlock()
	return truncated
}

func (s *DiscoverySnapshot) getCurrentUserLogin(ctx context.Context, cwd string) (string, error) {
	if s.tick == nil {
		return s.gateway.getCurrentUserLoginRaw(ctx, cwd)
	}
	cacheKey := strings.TrimSpace(cwd)
	s.tick.mu.Lock()
	if login, ok := s.tick.userLogins[cacheKey]; ok {
		s.tick.mu.Unlock()
		return login, nil
	}
	s.tick.mu.Unlock()
	login, err := s.gateway.getCurrentUserLoginRaw(ctx, cwd)
	if err != nil {
		return "", err
	}
	s.tick.mu.Lock()
	s.tick.userLogins[cacheKey] = login
	s.tick.mu.Unlock()
	return login, nil
}

func filterPullRequests(prs []PullRequestSummary, input ListOpenPullRequestsInput) []PullRequestSummary {
	labels := prListLabels(input)
	author := strings.TrimSpace(input.Author)
	baseRefName := strings.TrimSpace(input.BaseRefName)
	filtered := prs[:0]
	for _, pr := range prs {
		if author != "" && !strings.EqualFold(strings.TrimSpace(pr.Author), author) {
			continue
		}
		if baseRefName != "" && strings.TrimSpace(pr.BaseRefName) != baseRefName {
			continue
		}
		if len(labels) > 0 && !hasAllLabels(pr.Labels, labels) {
			continue
		}
		filtered = append(filtered, pr)
	}
	return filtered
}

func filterIssues(issues []IssueSummary, input ListOpenIssuesInput) []IssueSummary {
	labels := issueListLabels(input)
	assignee := strings.TrimSpace(input.Assignee)
	filtered := issues[:0]
	for _, issue := range issues {
		if assignee != "" && !includesLogin(issue.Assignees, assignee) {
			continue
		}
		if len(labels) > 0 && !hasAllLabels(issue.Labels, labels) {
			continue
		}
		filtered = append(filtered, issue)
	}
	return filtered
}

func hasAllLabels(labels []string, required []string) bool {
	if len(required) == 0 {
		return true
	}
	normalized := make([]string, 0, len(labels))
	for _, label := range labels {
		normalized = append(normalized, strings.ToLower(strings.TrimSpace(label)))
	}
	for _, requiredLabel := range required {
		if !slices.Contains(normalized, strings.ToLower(strings.TrimSpace(requiredLabel))) {
			return false
		}
	}
	return true
}

func includesLogin(logins []string, required string) bool {
	required = strings.TrimSpace(required)
	for _, login := range logins {
		if strings.EqualFold(strings.TrimSpace(login), required) {
			return true
		}
	}
	return false
}

func limitPullRequests(prs []PullRequestSummary, limit int) []PullRequestSummary {
	effective := defaultLimit(limit)
	if len(prs) <= effective {
		return prs
	}
	return prs[:effective]
}

func limitIssues(issues []IssueSummary, limit int) []IssueSummary {
	effective := defaultLimit(limit)
	if len(issues) <= effective {
		return issues
	}
	return issues[:effective]
}

func snapshotPullRequestDetailKey(input ViewPullRequestInput) string {
	return fmt.Sprintf("%s|%s|%d", input.Repo, input.CWD, input.PRNumber)
}

func hasPullRequestFilters(input ListOpenPullRequestsInput) bool {
	return strings.TrimSpace(input.Author) != "" || strings.TrimSpace(input.BaseRefName) != "" || len(prListLabels(input)) > 0
}

func hasIssueFilters(input ListOpenIssuesInput) bool {
	return strings.TrimSpace(input.Assignee) != "" || len(issueListLabels(input)) > 0
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
