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

	reviewRequestedPRs map[string]reviewRequestedPullRequestSnapshotEntry

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
	return &DiscoverySnapshot{gateway: gateway, tick: tick, prLimit: max(defaultLimit(options.PullRequestLimit), 30), issueLimit: max(defaultLimit(options.IssueLimit), 30), reviewRequestedPRs: map[string]reviewRequestedPullRequestSnapshotEntry{}, prDetails: map[string]PullRequestDetail{}}
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

func (s *DiscoverySnapshot) listReviewRequestedPullRequests(ctx context.Context, input ListReviewRequestedPullRequestsInput) ([]PullRequestSummary, error) {
	key := reviewRequestedPullRequestSnapshotKey(input)
	s.mu.Lock()
	if entry, ok := s.reviewRequestedPRs[key]; ok {
		prs := clonePullRequestSummaries(entry.items)
		s.mu.Unlock()
		return limitPullRequests(prs, input.Limit), nil
	}
	s.mu.Unlock()

	requiredLimit := s.prLimit
	prs, err := s.gateway.listReviewRequestedPullRequestsForDiscovery(ctx, ListReviewRequestedPullRequestsInput{Repo: input.Repo, CWD: input.CWD, Limit: requiredLimit, Reviewer: input.Reviewer, Timeout: input.Timeout})
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.reviewRequestedPRs[key] = reviewRequestedPullRequestSnapshotEntry{items: clonePullRequestSummaries(prs)}
	s.mu.Unlock()
	return limitPullRequests(clonePullRequestSummaries(prs), input.Limit), nil
}

func (s *DiscoverySnapshot) ensureOpenPullRequests(ctx context.Context, input ListOpenPullRequestsInput) error {
	s.mu.Lock()
	ready := s.openPRsFetched && s.openPRsFetchRepo == input.Repo && s.openPRsFetchCWD == input.CWD
	s.mu.Unlock()
	if ready {
		return nil
	}
	requiredLimit := s.prLimit
	prs, err := s.gateway.listOpenPullRequestsForDiscovery(ctx, ListOpenPullRequestsInput{Repo: input.Repo, CWD: input.CWD, Limit: requiredLimit, Timeout: input.Timeout})
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.openPRs = clonePullRequestSummaries(prs)
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
	issues, err := s.gateway.listOpenIssuesForDiscovery(ctx, ListOpenIssuesInput{Repo: input.Repo, CWD: input.CWD, Limit: requiredLimit})
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.openIssues = cloneIssueSummaries(issues)
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
	prs := clonePullRequestSummaries(s.openPRs)
	s.mu.Unlock()
	return filterPullRequests(prs, input)
}

func (s *DiscoverySnapshot) filteredIssues(input ListOpenIssuesInput) []IssueSummary {
	s.mu.Lock()
	issues := cloneIssueSummaries(s.openIssues)
	s.mu.Unlock()
	return filterIssues(issues, input)
}

func (g *Gateway) listOpenPullRequestsForDiscovery(ctx context.Context, input ListOpenPullRequestsInput) ([]PullRequestSummary, error) {
	if g.discoveryCacheTTL <= 0 {
		return g.listOpenPullRequestsRaw(ctx, input)
	}
	key := discoveryPullRequestListCacheKey(input)
	now := g.now().UTC()
	g.discoveryCacheMu.Lock()
	if entry, ok := g.discoveryPRCache[key]; ok && now.Before(entry.expiresAt) {
		items := clonePullRequestSummaries(entry.items)
		g.discoveryCacheMu.Unlock()
		return items, nil
	}
	g.discoveryCacheMu.Unlock()

	prs, err := g.listOpenPullRequestsRaw(ctx, input)
	if err != nil {
		return nil, err
	}
	expiresAt := g.now().UTC().Add(g.discoveryCacheTTL)
	g.discoveryCacheMu.Lock()
	g.discoveryPRCache[key] = discoveryPullRequestListCacheEntry{expiresAt: expiresAt, items: clonePullRequestSummaries(prs)}
	g.discoveryCacheMu.Unlock()
	return clonePullRequestSummaries(prs), nil
}

func (g *Gateway) listReviewRequestedPullRequestsForDiscovery(ctx context.Context, input ListReviewRequestedPullRequestsInput) ([]PullRequestSummary, error) {
	if g.discoveryCacheTTL <= 0 {
		return g.listReviewRequestedPullRequestsRaw(ctx, input)
	}
	key := discoveryReviewRequestedPullRequestListCacheKey(input)
	now := g.now().UTC()
	g.discoveryCacheMu.Lock()
	if entry, ok := g.discoveryReviewPRCache[key]; ok && now.Before(entry.expiresAt) {
		items := clonePullRequestSummaries(entry.items)
		g.discoveryCacheMu.Unlock()
		return items, nil
	}
	g.discoveryCacheMu.Unlock()

	prs, err := g.listReviewRequestedPullRequestsRaw(ctx, input)
	if err != nil {
		return nil, err
	}
	expiresAt := g.now().UTC().Add(g.discoveryCacheTTL)
	g.discoveryCacheMu.Lock()
	g.discoveryReviewPRCache[key] = discoveryPullRequestListCacheEntry{expiresAt: expiresAt, items: clonePullRequestSummaries(prs)}
	g.discoveryCacheMu.Unlock()
	return clonePullRequestSummaries(prs), nil
}

func (g *Gateway) listOpenIssuesForDiscovery(ctx context.Context, input ListOpenIssuesInput) ([]IssueSummary, error) {
	if g.discoveryCacheTTL <= 0 {
		return g.listOpenIssuesRaw(ctx, input)
	}
	key := discoveryIssueListCacheKey(input)
	now := g.now().UTC()
	g.discoveryCacheMu.Lock()
	if entry, ok := g.discoveryIssueCache[key]; ok && now.Before(entry.expiresAt) {
		items := cloneIssueSummaries(entry.items)
		g.discoveryCacheMu.Unlock()
		return items, nil
	}
	g.discoveryCacheMu.Unlock()

	issues, err := g.listOpenIssuesRaw(ctx, input)
	if err != nil {
		return nil, err
	}
	expiresAt := g.now().UTC().Add(g.discoveryCacheTTL)
	g.discoveryCacheMu.Lock()
	g.discoveryIssueCache[key] = discoveryIssueListCacheEntry{expiresAt: expiresAt, items: cloneIssueSummaries(issues)}
	g.discoveryCacheMu.Unlock()
	return cloneIssueSummaries(issues), nil
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

func discoveryPullRequestListCacheKey(input ListOpenPullRequestsInput) string {
	return fmt.Sprintf("pr|%s|%s|%d", strings.TrimSpace(input.Repo), strings.TrimSpace(input.CWD), defaultLimit(input.Limit))
}

func discoveryReviewRequestedPullRequestListCacheKey(input ListReviewRequestedPullRequestsInput) string {
	return fmt.Sprintf("review-pr|%s|%s|%s|%d", strings.TrimSpace(input.Repo), strings.TrimSpace(input.CWD), normalizeGitHubLogin(input.Reviewer), defaultLimit(input.Limit))
}

func discoveryIssueListCacheKey(input ListOpenIssuesInput) string {
	return fmt.Sprintf("issue|%s|%s|%d", strings.TrimSpace(input.Repo), strings.TrimSpace(input.CWD), defaultLimit(input.Limit))
}

func reviewRequestedPullRequestSnapshotKey(input ListReviewRequestedPullRequestsInput) string {
	return fmt.Sprintf("%s|%s|%s", strings.TrimSpace(input.Repo), strings.TrimSpace(input.CWD), normalizeGitHubLogin(input.Reviewer))
}

type reviewRequestedPullRequestSnapshotEntry struct {
	items []PullRequestSummary
}

func clonePullRequestSummaries(prs []PullRequestSummary) []PullRequestSummary {
	if len(prs) == 0 {
		return nil
	}
	out := make([]PullRequestSummary, len(prs))
	for i, pr := range prs {
		out[i] = pr
		out[i].Labels = append([]string(nil), pr.Labels...)
		out[i].ReviewRequests = append([]string(nil), pr.ReviewRequests...)
		out[i].ReviewRequestUsers = append([]GitHubUser(nil), pr.ReviewRequestUsers...)
		out[i].Reviews = cloneObjectMaps(pr.Reviews)
	}
	return out
}

func cloneIssueSummaries(issues []IssueSummary) []IssueSummary {
	if len(issues) == 0 {
		return nil
	}
	out := make([]IssueSummary, len(issues))
	for i, issue := range issues {
		out[i] = issue
		out[i].Assignees = append([]string(nil), issue.Assignees...)
		out[i].AssigneeUsers = append([]GitHubUser(nil), issue.AssigneeUsers...)
		out[i].Labels = append([]string(nil), issue.Labels...)
	}
	return out
}

func cloneObjectMaps(items []map[string]any) []map[string]any {
	if len(items) == 0 {
		return nil
	}
	out := make([]map[string]any, len(items))
	for i, item := range items {
		if item == nil {
			continue
		}
		cloned := make(map[string]any, len(item))
		for key, value := range item {
			cloned[key] = cloneJSONLikeValue(value)
		}
		out[i] = cloned
	}
	return out
}

func cloneJSONLikeValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		cloned := make(map[string]any, len(typed))
		for key, nested := range typed {
			cloned[key] = cloneJSONLikeValue(nested)
		}
		return cloned
	case []any:
		cloned := make([]any, len(typed))
		for i, nested := range typed {
			cloned[i] = cloneJSONLikeValue(nested)
		}
		return cloned
	case []map[string]any:
		cloned := make([]map[string]any, len(typed))
		for i, nested := range typed {
			if nestedMap, ok := cloneJSONLikeValue(nested).(map[string]any); ok {
				cloned[i] = nestedMap
			}
		}
		return cloned
	default:
		return value
	}
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
