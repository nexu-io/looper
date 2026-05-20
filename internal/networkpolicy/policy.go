package networkpolicy

import (
	"fmt"
	"strings"

	"github.com/nexu-io/looper/internal/config"
)

const (
	workerReadyLabel = "looper:worker-ready"
	targetPrefix     = "looper:target:"
)

type GitHubUser struct {
	Login string
	ID    int64
}

type ProjectPolicy struct {
	Mode         config.NetworkMode
	NodeName     string
	GitHubLogin  string
	GitHubUserID int64
}

type MatchMode string

const (
	MatchModeNone          MatchMode = "none"
	MatchModeNumeric       MatchMode = "numeric"
	MatchModeLoginFallback MatchMode = "login_fallback"
)

type ClaimDecision struct {
	Allowed     bool
	Reason      string
	MatchMode   MatchMode
	TargetLabel string
}

func ProjectPolicyForProject(cfg config.Config, projectID string) ProjectPolicy {
	policy := ProjectPolicy{Mode: config.NetworkModeOff, NodeName: strings.TrimSpace(cfg.Network.NodeName), GitHubLogin: normalizeLogin(cfg.Network.GitHubLogin), GitHubUserID: cfg.Network.GitHubUserID}
	for _, project := range cfg.Projects {
		if project.ID != projectID {
			continue
		}
		policy.Mode = normalizeMode(project.Network.Mode)
		return policy
	}
	return policy
}

func IsRouted(policy ProjectPolicy) bool {
	return normalizeMode(policy.Mode) == config.NetworkModeRouted
}

func EvaluateWorker(policy ProjectPolicy, labels []string, assignees []GitHubUser) ClaimDecision {
	if !IsRouted(policy) {
		return ClaimDecision{Allowed: true, MatchMode: MatchModeNone}
	}
	if !hasLabel(labels, workerReadyLabel) {
		return ClaimDecision{Reason: "missing looper:worker-ready label", MatchMode: MatchModeNone}
	}
	decision := evaluateTarget(policy, labels)
	if !decision.Allowed {
		return decision
	}
	matched, matchMode := matchLocalIdentity(policy, assignees)
	if !matched {
		return ClaimDecision{Reason: "local GitHub identity is not assigned", MatchMode: matchMode, TargetLabel: decision.TargetLabel}
	}
	decision.MatchMode = matchMode
	return decision
}

func EvaluateReviewer(policy ProjectPolicy, labels []string, reviewRequests []GitHubUser) ClaimDecision {
	if !IsRouted(policy) {
		return ClaimDecision{Allowed: true, MatchMode: MatchModeNone}
	}
	decision := evaluateTarget(policy, labels)
	if !decision.Allowed {
		return decision
	}
	matched, matchMode := matchLocalIdentity(policy, reviewRequests)
	if !matched {
		return ClaimDecision{Reason: "local GitHub identity is not requested for review", MatchMode: matchMode, TargetLabel: decision.TargetLabel}
	}
	decision.MatchMode = matchMode
	return decision
}

func evaluateTarget(policy ProjectPolicy, labels []string) ClaimDecision {
	targetLabels := collectTargetLabels(labels)
	if len(targetLabels) == 0 {
		return ClaimDecision{Reason: "missing looper:target:<node_name> label", MatchMode: MatchModeNone}
	}
	if len(targetLabels) > 1 {
		return ClaimDecision{Reason: "multiple looper:target:<node_name> labels present", MatchMode: MatchModeNone}
	}
	targetLabel := targetLabels[0]
	targetNode := trimTargetPrefix(targetLabel)
	if !strings.EqualFold(strings.TrimSpace(targetNode), strings.TrimSpace(policy.NodeName)) {
		return ClaimDecision{Reason: fmt.Sprintf("target label %s does not match local node %s", targetLabel, strings.TrimSpace(policy.NodeName)), MatchMode: MatchModeNone, TargetLabel: targetLabel}
	}
	return ClaimDecision{Allowed: true, MatchMode: MatchModeNone, TargetLabel: targetLabel}
}

func collectTargetLabels(labels []string) []string {
	result := make([]string, 0, 1)
	for _, label := range labels {
		trimmed := strings.TrimSpace(label)
		if strings.HasPrefix(strings.ToLower(trimmed), targetPrefix) {
			result = append(result, trimmed)
		}
	}
	return result
}

func trimTargetPrefix(label string) string {
	trimmed := strings.TrimSpace(label)
	if len(trimmed) < len(targetPrefix) {
		return trimmed
	}
	if strings.EqualFold(trimmed[:len(targetPrefix)], targetPrefix) {
		return trimmed[len(targetPrefix):]
	}
	return trimmed
}

func matchLocalIdentity(policy ProjectPolicy, users []GitHubUser) (bool, MatchMode) {
	if policy.GitHubUserID > 0 {
		hasNumericCandidate := false
		for _, user := range users {
			if user.ID <= 0 {
				continue
			}
			hasNumericCandidate = true
			if user.ID == policy.GitHubUserID {
				return true, MatchModeNumeric
			}
		}
		if hasNumericCandidate {
			return false, MatchModeNumeric
		}
	}
	if policy.GitHubLogin == "" {
		return false, MatchModeNone
	}
	for _, user := range users {
		if normalizeLogin(user.Login) == policy.GitHubLogin {
			return true, MatchModeLoginFallback
		}
	}
	return false, MatchModeLoginFallback
}

func normalizeMode(mode config.NetworkMode) config.NetworkMode {
	if strings.TrimSpace(string(mode)) == string(config.NetworkModeRouted) {
		return config.NetworkModeRouted
	}
	return config.NetworkModeOff
}

func normalizeLogin(login string) string {
	return strings.ToLower(strings.TrimSpace(login))
}

func hasLabel(labels []string, want string) bool {
	want = strings.TrimSpace(want)
	for _, label := range labels {
		if strings.EqualFold(strings.TrimSpace(label), want) {
			return true
		}
	}
	return false
}
