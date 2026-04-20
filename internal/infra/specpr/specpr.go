package specpr

import (
	"regexp"
	"strings"
)

const (
	ReviewingLabel  = "looper:spec-reviewing"
	ReadyLabel      = "looper:spec-ready"
	NeedsHumanLabel = "looper:needs-human"
)

type PullRequestPhase string

const (
	PhaseSpec           PullRequestPhase = "spec"
	PhaseImplementation PullRequestPhase = "implementation"
)

var pathPattern = regexp.MustCompile(`(?mi)^Spec:\s*(.+)$`)

func NormalizeLabel(label string) string {
	return strings.ToLower(strings.TrimSpace(label))
}

func HasLabel(labels []string, target string) bool {
	normalizedTarget := NormalizeLabel(target)
	for _, label := range labels {
		if NormalizeLabel(label) == normalizedTarget {
			return true
		}
	}
	return false
}

func ResolvePullRequestPhase(labels []string) PullRequestPhase {
	if HasLabel(labels, ReviewingLabel) {
		return PhaseSpec
	}
	return PhaseImplementation
}

func ParseSpecPathFromPullRequestBody(body string) string {
	matches := pathPattern.FindStringSubmatch(body)
	if len(matches) < 2 {
		return ""
	}
	return strings.TrimSpace(matches[1])
}

func CountUnresolvedReviewThreads(comments []map[string]any) int {
	count := 0
	for _, comment := range comments {
		state, _ := comment["state"].(string)
		if state != "" {
			if !strings.EqualFold(state, "RESOLVED") {
				count++
			}
			continue
		}
		isResolved, _ := comment["isResolved"].(bool)
		if !isResolved {
			count++
		}
	}
	return count
}

func IsReviewClean(reviewDecision string, comments []map[string]any) bool {
	return CountUnresolvedReviewThreads(comments) == 0 && !strings.EqualFold(reviewDecision, "CHANGES_REQUESTED")
}
