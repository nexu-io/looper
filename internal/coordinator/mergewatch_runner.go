package coordinator

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/coordinator/mergewatch"
	"github.com/nexu-io/looper/internal/disclosure"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
)

var mergeWatchPRURLPattern = regexp.MustCompile(`/pull/(\d+)(?:/|$)`)
var mergeWatchClosingReferencePattern = regexp.MustCompile(`(?i)(?:close|closes|closed|fix|fixes|fixed|resolve|resolves|resolved)\s+([\w.-]+/[\w.-]+#\d+|#\d+)`)

type mergeWatchComment struct {
	ID      int64
	Summary string
	Marker  mergewatch.PriorWatchMarker
	Body    string
}

func (r *Runner) applyMergeWatch(ctx context.Context, repo, cwd string, loaded []loadedIssue, roles config.RoleConfigs) (map[int64]struct{}, error) {
	result := map[int64]struct{}{}
	if r.github == nil {
		return result, nil
	}
	currentLogin, err := r.github.GetCurrentUserLoginForRepo(ctx, repo, cwd)
	if err != nil {
		return nil, err
	}
	budget, err := time.ParseDuration(strings.TrimSpace(roles.Coordinator.MergeWatch.MaxIndeterminateDuration))
	if err != nil {
		return nil, err
	}
	for _, issue := range loaded {
		if !issueHasCoordinatorTracking(issue.detail.Labels, roles.Coordinator.Triage.TriagedLabel) {
			continue
		}
		lock := r.watchLock(repo, issue.detail.Number)
		lock.Lock()
		removed, applyErr := r.applyMergeWatchLocked(ctx, repo, cwd, issue, roles, currentLogin, budget)
		lock.Unlock()
		if applyErr != nil {
			return nil, applyErr
		}
		if removed {
			result[issue.detail.Number] = struct{}{}
		}
	}
	return result, nil
}

func (r *Runner) applyMergeWatchLocked(ctx context.Context, repo, cwd string, issue loadedIssue, roles config.RoleConfigs, currentLogin string, maxIndeterminateDuration time.Duration) (bool, error) {
	marker := findMergeWatchComment(issue.detail.Comments, currentLogin)
	watchedPR, ok, err := r.resolveWatchedPR(ctx, repo, cwd, issue, marker, currentLogin)
	if err != nil || !ok {
		return false, err
	}
	if marker != nil && marker.Marker.NextRetryAt != nil && r.now().UTC().Before(marker.Marker.NextRetryAt.UTC()) {
		return false, nil
	}
	snapshot, tempErr, err := r.mergeWatchSnapshot(ctx, repo, cwd, issue.detail.Number, watchedPR, currentLogin)
	if err != nil {
		return false, err
	}
	if tempErr != nil {
		snapshot.TemporaryError = tempErr
		if snapshot.HeadSHA == "" && marker != nil && marker.Marker.PRNumber == snapshot.PRNumber {
			snapshot.HeadSHA = marker.Marker.HeadSHA
		}
	}
	action := mergewatch.Classify(snapshot, markerState(marker), mergewatch.RetryBudget{Now: r.now().UTC(), TransientRetries: roles.Coordinator.MergeWatch.TransientRetries, MaxIndeterminateDuration: maxIndeterminateDuration})
	baseMarker := mergeWatchBaseMarker(marker, snapshot, roles.Coordinator.MergeWatch.TransientRetries)
	if action.Kind != mergewatch.ActionTransientError && (!snapshot.HasLooperLabel || (snapshot.AutoMergeEnabled && !snapshot.AutoMergeOwnedByLooper)) {
		return false, r.deleteMergeWatchComment(ctx, repo, cwd, marker)
	}
	switch action.Kind {
	case mergewatch.ActionMerged, mergewatch.ActionHumanDisabledAutoMerge:
		return false, r.deleteMergeWatchComment(ctx, repo, cwd, marker)
	case mergewatch.ActionStillPending:
		baseMarker.FirstUnknownAt = nil
		baseMarker.NextRetryAt = nil
		baseMarker.Retries = roles.Coordinator.MergeWatch.TransientRetries
		if marker == nil || mergeWatchCommentNeedsUpdate(marker, baseMarker, "") {
			return false, r.upsertMergeWatchComment(ctx, repo, cwd, issue.detail.Number, marker, baseMarker, "")
		}
		return false, nil
	case mergewatch.ActionIndeterminate:
		baseMarker.FirstUnknownAt = action.FirstUnknownAt
		baseMarker.NextRetryAt = nil
		return false, r.upsertMergeWatchComment(ctx, repo, cwd, issue.detail.Number, marker, baseMarker, "")
	case mergewatch.ActionConflict, mergewatch.ActionRedCI:
		labels := requiredPRTriggerLabels(roles.Fixer.Triggers)
		if len(labels) > 0 {
			if err := r.github.AddPullRequestLabels(ctx, githubinfra.PullRequestLabelsInput{Repo: repo, PRNumber: snapshot.PRNumber, Labels: labels, CWD: cwd}); err != nil {
				return false, err
			}
		}
		baseMarker.FirstUnknownAt = nil
		baseMarker.NextRetryAt = nil
		baseMarker.Retries = roles.Coordinator.MergeWatch.TransientRetries
		summary := fmt.Sprintf("Coordinator merge-watch routed PR #%d to Fixer for %s.", snapshot.PRNumber, strings.ToLower(string(action.Kind)))
		return false, r.upsertMergeWatchComment(ctx, repo, cwd, issue.detail.Number, marker, baseMarker, summary)
	case mergewatch.ActionTransientError:
		if action.Exhausted {
			if err := r.removeIssueLabels(ctx, repo, cwd, issue.detail.Number, issue.detail.Labels, retriageCleanupPatterns(roles, roles.Coordinator.Triage.TriagedLabel)); err != nil {
				return false, err
			}
			return true, r.deleteMergeWatchComment(ctx, repo, cwd, marker)
		}
		baseMarker.FirstUnknownAt = nil
		if action.SuggestedDelay > 0 {
			next := r.now().UTC().Add(action.SuggestedDelay)
			baseMarker.NextRetryAt = &next
		}
		baseMarker.Retries = action.RetriesLeft
		return false, r.upsertMergeWatchComment(ctx, repo, cwd, issue.detail.Number, marker, baseMarker, "")
	case mergewatch.ActionBranchProtectionChanged:
		if err := r.removeIssueLabels(ctx, repo, cwd, issue.detail.Number, issue.detail.Labels, retriageCleanupPatterns(roles, roles.Coordinator.Triage.TriagedLabel)); err != nil {
			return false, err
		}
		return true, r.deleteMergeWatchComment(ctx, repo, cwd, marker)
	default:
		return false, nil
	}
}

func (r *Runner) resolveWatchedPR(ctx context.Context, repo, cwd string, issue loadedIssue, marker *mergeWatchComment, currentLogin string) (int64, bool, error) {
	linked := linkedPullRequestNumbers(issue.rawTimeline)
	if marker != nil && marker.Marker.PRNumber > 0 {
		for _, linkedPR := range linked {
			if linkedPR == marker.Marker.PRNumber {
				detail, err := r.github.ViewPullRequestMergeWatch(ctx, githubinfra.ViewPullRequestInput{Repo: repo, PRNumber: linkedPR, CWD: cwd})
				if err == nil && prLinksIssue(repo, issue.detail.Number, detail.Body) {
					return marker.Marker.PRNumber, true, nil
				}
			}
		}
	}
	eligible := []int64{}
	for _, prNumber := range linked {
		detail, err := r.github.ViewPullRequestMergeWatch(ctx, githubinfra.ViewPullRequestInput{Repo: repo, PRNumber: prNumber, CWD: cwd})
		if err != nil {
			continue
		}
		if detail.AutoMerge == nil || !strings.EqualFold(strings.TrimSpace(detail.AutoMerge.EnabledBy), strings.TrimSpace(currentLogin)) || !hasLooperLabel(detail.Labels) || !prLinksIssue(repo, issue.detail.Number, detail.Body) {
			continue
		}
		eligible = append(eligible, prNumber)
	}
	if len(eligible) != 1 {
		if len(eligible) > 1 && r.logger != nil {
			r.logger.Warn("coordinator merge-watch skipped ambiguous linked PR set", map[string]any{"repo": repo, "issue": issue.detail.Number, "count": len(eligible)})
		}
		return 0, false, nil
	}
	return eligible[0], true, nil
}

func (r *Runner) mergeWatchSnapshot(ctx context.Context, repo, cwd string, issueNumber, prNumber int64, currentLogin string) (mergewatch.PRSnapshot, *mergewatch.TemporaryError, error) {
	detail, err := r.github.ViewPullRequestMergeWatch(ctx, githubinfra.ViewPullRequestInput{Repo: repo, PRNumber: prNumber, CWD: cwd})
	if err != nil {
		if isTransientMergeWatchError(err) {
			return mergewatch.PRSnapshot{Repo: repo, PRNumber: prNumber, IssueNumber: issueNumber}, &mergewatch.TemporaryError{SuggestedDelay: time.Minute}, nil
		}
		return mergewatch.PRSnapshot{}, nil, err
	}
	checkRuns, err := r.github.ListPullRequestCheckRuns(ctx, githubinfra.PullRequestCheckRunsInput{Repo: repo, Ref: detail.HeadSHA, CWD: cwd})
	if err != nil {
		if isTransientMergeWatchError(err) {
			return mergewatch.PRSnapshot{Repo: repo, PRNumber: prNumber, IssueNumber: issueNumber, HeadSHA: detail.HeadSHA, AutoMergeEnabled: detail.AutoMerge != nil, AutoMergeOwnedByLooper: detail.AutoMerge != nil && strings.EqualFold(detail.AutoMerge.EnabledBy, currentLogin), Mergeable: detail.Mergeable, MergeableState: detail.MergeableState}, &mergewatch.TemporaryError{SuggestedDelay: time.Minute}, nil
		}
		return mergewatch.PRSnapshot{}, nil, err
	}
	checks := mergewatch.RequiredCheckSummary{}
	mergeableState := strings.ToLower(strings.TrimSpace(detail.MergeableState))
	protection, err := r.github.GetBranchProtection(ctx, githubinfra.BranchProtectionInput{Repo: repo, Branch: detail.BaseRefName, CWD: cwd})
	if err != nil {
		if isTransientMergeWatchError(err) {
			return mergeWatchPartialSnapshot(repo, issueNumber, prNumber, detail, currentLogin), &mergewatch.TemporaryError{SuggestedDelay: time.Minute}, nil
		}
		return mergewatch.PRSnapshot{}, nil, err
	}
	requiredChecks := map[string]struct{}{}
	for _, name := range protection.RequiredChecks {
		requiredChecks[strings.ToLower(strings.TrimSpace(name))] = struct{}{}
	}
	seenChecks := map[string]struct{}{}
	for _, checkRun := range checkRuns.CheckRuns {
		status := strings.ToLower(strings.TrimSpace(checkRun.Status))
		conclusion := strings.ToLower(strings.TrimSpace(checkRun.Conclusion))
		nameKey := strings.ToLower(strings.TrimSpace(checkRun.Name))
		seenChecks[nameKey] = struct{}{}
		switch {
		case status != "completed" && requiredCheck(requiredChecks, nameKey):
			checks.Pending = append(checks.Pending, checkRun.Name)
		case mergeableState == "unstable" && requiredCheck(requiredChecks, nameKey) && (conclusion == "failure" || conclusion == "timed_out" || conclusion == "cancelled" || conclusion == "action_required" || conclusion == "startup_failure" || conclusion == "stale"):
			checks.Failed = append(checks.Failed, checkRun.Name)
		}
	}
	for _, status := range checkRuns.Statuses {
		contextKey := strings.ToLower(strings.TrimSpace(status.Context))
		state := strings.ToLower(strings.TrimSpace(status.State))
		seenChecks[contextKey] = struct{}{}
		switch {
		case requiredCheck(requiredChecks, contextKey) && state == "pending":
			checks.Pending = append(checks.Pending, status.Context)
		case mergeableState == "unstable" && requiredCheck(requiredChecks, contextKey) && (state == "failure" || state == "error"):
			checks.Failed = append(checks.Failed, status.Context)
		}
	}
	for requiredName := range requiredChecks {
		if _, ok := seenChecks[requiredName]; !ok {
			checks.Missing = append(checks.Missing, requiredName)
		}
	}
	open := strings.EqualFold(detail.State, "open")
	return mergewatch.PRSnapshot{
		Repo:                   repo,
		PRNumber:               prNumber,
		IssueNumber:            issueNumber,
		HeadSHA:                detail.HeadSHA,
		Merged:                 detail.MergedAt != "" || strings.EqualFold(detail.State, "merged"),
		Open:                   open,
		AutoMergeEnabled:       detail.AutoMerge != nil,
		AutoMergeOwnedByLooper: detail.AutoMerge != nil && strings.EqualFold(strings.TrimSpace(detail.AutoMerge.EnabledBy), strings.TrimSpace(currentLogin)),
		HasLooperLabel:         hasLooperLabel(detail.Labels),
		Mergeable:              detail.Mergeable,
		MergeableState:         mergeableState,
		RequiredChecks:         checks,
	}, nil, nil
}

func mergeWatchPartialSnapshot(repo string, issueNumber, prNumber int64, detail githubinfra.PullRequestDetail, currentLogin string) mergewatch.PRSnapshot {
	return mergewatch.PRSnapshot{
		Repo:                   repo,
		PRNumber:               prNumber,
		IssueNumber:            issueNumber,
		HeadSHA:                detail.HeadSHA,
		Open:                   strings.EqualFold(detail.State, "open"),
		AutoMergeEnabled:       detail.AutoMerge != nil,
		AutoMergeOwnedByLooper: detail.AutoMerge != nil && strings.EqualFold(strings.TrimSpace(detail.AutoMerge.EnabledBy), strings.TrimSpace(currentLogin)),
		HasLooperLabel:         hasLooperLabel(detail.Labels),
		Mergeable:              detail.Mergeable,
		MergeableState:         strings.ToLower(strings.TrimSpace(detail.MergeableState)),
	}
}

func mergeWatchBaseMarker(marker *mergeWatchComment, snapshot mergewatch.PRSnapshot, fallbackRetries int) mergewatch.PriorWatchMarker {
	if marker != nil && marker.Marker.PRNumber == snapshot.PRNumber && (snapshot.HeadSHA == "" || marker.Marker.HeadSHA == snapshot.HeadSHA) {
		return marker.Marker
	}
	return mergewatch.PriorWatchMarker{PRNumber: snapshot.PRNumber, HeadSHA: snapshot.HeadSHA, Retries: fallbackRetries}
}

func markerState(marker *mergeWatchComment) *mergewatch.PriorWatchMarker {
	if marker == nil {
		return nil
	}
	copy := marker.Marker
	return &copy
}

func requiredPRTriggerLabels(cfg config.FixerRoleTriggersConfig) []string {
	if cfg.LabelMode == config.LabelModeAll {
		return append([]string(nil), cfg.Labels...)
	}
	if len(cfg.Labels) == 0 {
		return nil
	}
	return []string{cfg.Labels[0]}
}

func retriageCleanupPatterns(roles config.RoleConfigs, triagedLabel string) []string {
	patterns := []string{triagedLabel, "dispatch/*"}
	patterns = append(patterns, requiredTriggerLabels(roles.Planner.Triggers)...)
	patterns = append(patterns, requiredTriggerLabels(roles.Worker.Triggers)...)
	return patterns
}

func (r *Runner) watchLock(repo string, issueNumber int64) *sync.Mutex {
	key := fmt.Sprintf("%s#%d", repo, issueNumber)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.watchLocks[key] == nil {
		r.watchLocks[key] = &sync.Mutex{}
	}
	return r.watchLocks[key]
}

func linkedPullRequestNumbers(timeline []map[string]any) []int64 {
	seen := map[int64]struct{}{}
	out := []int64{}
	for _, event := range timeline {
		for _, candidate := range []any{event["source"], event["pull_request"], event["issue"]} {
			if prNumber := pullRequestNumberFromTimelineCandidate(candidate); prNumber > 0 {
				if _, ok := seen[prNumber]; !ok {
					seen[prNumber] = struct{}{}
					out = append(out, prNumber)
				}
			}
		}
	}
	return out
}

func pullRequestNumberFromTimelineCandidate(candidate any) int64 {
	row, ok := candidate.(map[string]any)
	if !ok {
		return 0
	}
	if nested, ok := row["issue"].(map[string]any); ok {
		row = nested
	}
	if pullRequest := row["pull_request"]; pullRequest != nil {
		if prRow, ok := pullRequest.(map[string]any); ok {
			if number := asInt64(prRow["number"]); number > 0 {
				return number
			}
			if url := firstNonEmpty(asString(prRow["html_url"]), asString(prRow["url"])); url != "" {
				return pullRequestNumberFromURL(url)
			}
		}
	}
	if number := asInt64(row["number"]); number > 0 && row["pull_request"] != nil {
		return number
	}
	if url := firstNonEmpty(asString(row["html_url"]), asString(row["url"])); url != "" {
		return pullRequestNumberFromURL(url)
	}
	return 0
}

func pullRequestNumberFromURL(raw string) int64 {
	match := mergeWatchPRURLPattern.FindStringSubmatch(strings.TrimSpace(raw))
	if len(match) != 2 {
		return 0
	}
	value, _ := strconv.ParseInt(match[1], 10, 64)
	return value
}

func findMergeWatchComment(comments []githubinfra.CommentInfo, currentLogin string) *mergeWatchComment {
	for i := len(comments) - 1; i >= 0; i-- {
		if !strings.EqualFold(strings.TrimSpace(comments[i].Author), strings.TrimSpace(currentLogin)) {
			continue
		}
		marker, ok := parseMergeWatchComment(comments[i])
		if ok {
			return &marker
		}
	}
	return nil
}

func parseMergeWatchComment(comment githubinfra.CommentInfo) (mergeWatchComment, bool) {
	lines := strings.Split(strings.TrimSpace(comment.Body), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, mergeWatchCommentMarkerPrefix) {
			continue
		}
		payload := strings.TrimSuffix(strings.TrimPrefix(line, mergeWatchCommentMarkerPrefix), "-->")
		fields := strings.Fields(strings.TrimSpace(payload))
		marker := mergewatch.PriorWatchMarker{Retries: 0}
		for _, field := range fields {
			parts := strings.SplitN(field, "=", 2)
			if len(parts) != 2 {
				continue
			}
			switch parts[0] {
			case "pr":
				marker.PRNumber = asInt64(parts[1])
			case "head_sha":
				marker.HeadSHA = parts[1]
			case "retries":
				marker.Retries = int(asInt64(parts[1]))
			case "first_unknown_at":
				if when, err := time.Parse(time.RFC3339, parts[1]); err == nil {
					marker.FirstUnknownAt = &when
				}
			case "next_retry_at":
				if when, err := time.Parse(time.RFC3339, parts[1]); err == nil {
					marker.NextRetryAt = &when
				}
			}
		}
		summary := ""
		if idx := strings.Index(comment.Body, line); idx > 0 {
			summary = strings.TrimSpace(comment.Body[:idx])
		}
		return mergeWatchComment{ID: comment.ID, Summary: summary, Marker: marker, Body: comment.Body}, true
	}
	return mergeWatchComment{}, false
}

func mergeWatchCommentNeedsUpdate(existing *mergeWatchComment, next mergewatch.PriorWatchMarker, summary string) bool {
	line := fmt.Sprintf("<!-- looper:coordinator:merge-watch pr=%d head_sha=%s retries=%d first_unknown_at=%s next_retry_at=%s -->", next.PRNumber, next.HeadSHA, next.Retries, mergeWatchTime(next.FirstUnknownAt), mergeWatchTime(next.NextRetryAt))
	return !strings.Contains(existing.Body, line) || strings.TrimSpace(existing.Summary) != strings.TrimSpace(summary)
}

func mergeWatchTime(value *time.Time) string {
	if value == nil || value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339)
}

func (r *Runner) upsertMergeWatchComment(ctx context.Context, repo, cwd string, issueNumber int64, existing *mergeWatchComment, marker mergewatch.PriorWatchMarker, summary string) error {
	body := strings.TrimSpace(summary)
	line := fmt.Sprintf("<!-- looper:coordinator:merge-watch pr=%d head_sha=%s retries=%d first_unknown_at=%s next_retry_at=%s -->", marker.PRNumber, marker.HeadSHA, marker.Retries, mergeWatchTime(marker.FirstUnknownAt), mergeWatchTime(marker.NextRetryAt))
	if body != "" {
		body += "\n\n" + line
	} else {
		body = line
	}
	body = disclosure.FromConfig(*r.config).Markdown(body, "coordinator", disclosure.ChannelIssueComment)
	if existing != nil {
		if strings.TrimSpace(existing.Body) == strings.TrimSpace(body) {
			return nil
		}
		return r.github.UpdateIssueComment(ctx, githubinfra.UpdateIssueCommentInput{Repo: repo, CommentID: existing.ID, Body: body, CWD: cwd})
	}
	_, err := r.github.CreateIssueComment(ctx, githubinfra.IssueCommentInput{Repo: repo, IssueNumber: issueNumber, Body: body, CWD: cwd})
	return err
}

func (r *Runner) deleteMergeWatchComment(ctx context.Context, repo, cwd string, existing *mergeWatchComment) error {
	if existing == nil {
		return nil
	}
	return r.github.DeleteIssueComment(ctx, githubinfra.DeleteIssueCommentInput{Repo: repo, CommentID: existing.ID, CWD: cwd})
}

func hasLooperLabel(labels []string) bool {
	for _, label := range labels {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(label)), "looper:") {
			return true
		}
	}
	return false
}

func isTransientMergeWatchError(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "timeout") || strings.Contains(text, "timed out") || strings.Contains(text, "secondary rate") || strings.Contains(text, "abuse") || strings.Contains(text, "429") || strings.Contains(text, "502") || strings.Contains(text, "503") || strings.Contains(text, "504") || strings.Contains(text, "500")
}

func prLinksIssue(repo string, issueNumber int64, body string) bool {
	for _, match := range mergeWatchClosingReferencePattern.FindAllStringSubmatch(body, -1) {
		if len(match) < 2 {
			continue
		}
		reference := strings.TrimSpace(match[1])
		if strings.HasPrefix(reference, "#") && asInt64(reference[1:]) == issueNumber {
			return true
		}
		parts := strings.Split(reference, "#")
		if len(parts) == 2 && strings.EqualFold(strings.TrimSpace(parts[0]), strings.TrimSpace(repo)) && asInt64(parts[1]) == issueNumber {
			return true
		}
	}
	return false
}

func requiredCheck(required map[string]struct{}, name string) bool {
	if len(required) == 0 {
		return false
	}
	_, ok := required[name]
	return ok
}

func issueHasCoordinatorTracking(labels []string, triagedLabel string) bool {
	for _, label := range labels {
		normalized := strings.ToLower(strings.TrimSpace(label))
		if normalized == strings.ToLower(strings.TrimSpace(triagedLabel)) || strings.HasPrefix(normalized, "dispatch/") {
			return true
		}
	}
	return false
}

func asInt64(value any) int64 {
	if text, ok := value.(string); ok {
		out, _ := strconv.ParseInt(strings.TrimSpace(text), 10, 64)
		return out
	}
	if number, ok := value.(float64); ok {
		return int64(number)
	}
	out, _ := strconv.ParseInt(strings.TrimSpace(fmt.Sprint(value)), 10, 64)
	return out
}
