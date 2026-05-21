package config

import "strings"

var enhancedReviewerTransientPatterns = []string{
	": eof",
	"connection closed by",
	"closed by remote host",
	"connection reset by peer",
	"remote end hung up unexpectedly",
	"early eof",
	"rpc failed",
	"curl 56",
	"curl 92",
	"tls handshake timeout",
	"i/o timeout",
	"context deadline exceeded",
	"server closed idle connection",
	"http2: client connection lost",
	"stream error",
}

var enhancedReviewerRemoteDependencyMarkers = []string{
	"github.com",
	"api.github.com",
	"/graphql",
	"git fetch",
	"git ls-remote",
	"git pull",
	"git remote",
	"refs/pull/",
	"origin/",
	"remote repository",
}

func NormalizeReviewerRetryConfig(policy ReviewerRetryConfig) ReviewerRetryConfig {
	defaults := DefaultReviewerRetryConfig()
	if policy.AutoRecoveryMaxAttempts <= 0 {
		policy.AutoRecoveryMaxAttempts = defaults.AutoRecoveryMaxAttempts
	}
	if policy.MaxDelayMS <= 0 {
		policy.MaxDelayMS = defaults.MaxDelayMS
	}
	if policy.ExtraTransientErrorPatterns == nil {
		policy.ExtraTransientErrorPatterns = []string{}
		return policy
	}
	trimmed := make([]string, 0, len(policy.ExtraTransientErrorPatterns))
	seen := make(map[string]struct{}, len(policy.ExtraTransientErrorPatterns))
	for _, pattern := range policy.ExtraTransientErrorPatterns {
		pattern = strings.TrimSpace(pattern)
		key := strings.ToLower(pattern)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		trimmed = append(trimmed, pattern)
	}
	policy.ExtraTransientErrorPatterns = trimmed
	return policy
}

func ReviewerRetryMessageMatches(policy ReviewerRetryConfig, message string) bool {
	policy = NormalizeReviewerRetryConfig(policy)
	if !policy.EnhancedTransientClassification {
		return false
	}
	lower := strings.ToLower(strings.TrimSpace(message))
	if lower == "" {
		return false
	}
	for _, pattern := range policy.ExtraTransientErrorPatterns {
		pattern = strings.ToLower(strings.TrimSpace(pattern))
		if pattern != "" && strings.Contains(lower, pattern) {
			return true
		}
	}
	if !looksLikeReviewerRemoteDependencyFailure(lower) {
		return false
	}
	for _, pattern := range enhancedReviewerTransientPatterns {
		if strings.Contains(lower, pattern) {
			return true
		}
	}
	return false
}

func looksLikeReviewerRemoteDependencyFailure(message string) bool {
	for _, marker := range enhancedReviewerRemoteDependencyMarkers {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}
