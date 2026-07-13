package storage

import (
	"fmt"
	"strings"
)

// IssueLockKey scopes a forge issue lock to the authoritative project binding.
// Existing queue records retain their persisted legacy keys during migration.
func IssueLockKey(projectID, repo string, issueNumber int64) string {
	return fmt.Sprintf("issue:%s:%s:%d", strings.TrimSpace(projectID), strings.ToLower(strings.TrimSpace(repo)), issueNumber)
}

// PullRequestLockKey scopes a forge pull-request lock to the authoritative
// project binding while keeping the key opaque to older queue processors.
func PullRequestLockKey(projectID, repo string, prNumber int64) string {
	return fmt.Sprintf("pr:%s:%s:%d", strings.TrimSpace(projectID), strings.ToLower(strings.TrimSpace(repo)), prNumber)
}

// lockTransitionAlias describes the legacy/scoped equivalent of a forge lock
// key during the rolling migration. The persisted queue key remains opaque;
// this alias is used only to preserve mutual exclusion across versions.
func lockTransitionAlias(key string) (legacyKey, scopedSuffix, kind string, scoped, ok bool) {
	key = strings.TrimSpace(key)
	firstSeparator := strings.IndexByte(key, ':')
	lastSeparator := strings.LastIndexByte(key, ':')
	if firstSeparator <= 0 || lastSeparator <= firstSeparator {
		return "", "", "", false, false
	}
	kind = key[:firstSeparator]
	if kind != "issue" && kind != "pr" {
		return "", "", "", false, false
	}
	beforeRepo := strings.LastIndexByte(key[:lastSeparator], ':')
	if beforeRepo < firstSeparator {
		return "", "", "", false, false
	}
	if beforeRepo == firstSeparator {
		suffix := key[firstSeparator:]
		return key, suffix, kind, false, true
	}
	repoAndNumber := key[beforeRepo:]
	if strings.TrimSpace(key[firstSeparator+1:beforeRepo]) == "" || strings.TrimSpace(key[beforeRepo+1:lastSeparator]) == "" || strings.TrimSpace(key[lastSeparator+1:]) == "" {
		return "", "", "", false, false
	}
	return kind + repoAndNumber, repoAndNumber, kind, true, true
}
