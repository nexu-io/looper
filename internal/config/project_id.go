package config

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	legacyProjectIDPrefix              = "legacy-id-"
	maxProjectWorktreeDirectoryNameLen = 255
)

var (
	canonicalProjectDirectoryNamePattern           = regexp.MustCompile(`^[a-z0-9._-]+$`)
	windowsReservedProjectDirectoryBasenamePattern = regexp.MustCompile(`^(con|prn|aux|nul|com[1-9]|lpt[1-9])(?:\..*)?$`)
)

func ToRepoWorktreeDirectoryName(repoIdentity string) string {
	canonical := canonicalizeRepoIdentity(repoIdentity)
	hash := sha256.Sum256([]byte(canonical))

	return "repo-" + hex.EncodeToString(hash[:])
}

func ToProjectWorktreeDirectoryName(projectID string) string {
	if isCanonicalProjectDirectoryName(projectID) {
		return projectID
	}

	encodedProjectID := hex.EncodeToString([]byte(projectID))
	if encodedProjectID == "" {
		encodedProjectID = "empty"
	}

	if len(legacyProjectIDPrefix)+len(encodedProjectID) <= maxProjectWorktreeDirectoryNameLen {
		return legacyProjectIDPrefix + encodedProjectID
	}

	hash := sha256.Sum256([]byte(projectID))
	return legacyProjectIDPrefix + hex.EncodeToString(hash[:])
}

func canonicalizeRepoIdentity(repoIdentity string) string {
	resolved, err := filepath.Abs(filepath.Clean(repoIdentity))
	if err != nil {
		resolved = filepath.Clean(repoIdentity)
	}

	canonical, err := filepath.EvalSymlinks(resolved)
	if err != nil {
		return filepath.Clean(resolved)
	}

	return filepath.Clean(canonical)
}

func isCanonicalProjectDirectoryName(projectID string) bool {
	if projectID == "" || strings.ContainsAny(projectID, `/\`) || filepath.IsAbs(projectID) {
		return false
	}

	if projectID == "." || projectID == ".." || strings.HasPrefix(projectID, legacyProjectIDPrefix) {
		return false
	}

	if !canonicalProjectDirectoryNamePattern.MatchString(projectID) || len(projectID) > maxProjectWorktreeDirectoryNameLen {
		return false
	}

	lowerProjectID := strings.ToLower(projectID)
	if strings.HasSuffix(lowerProjectID, ".") || windowsReservedProjectDirectoryBasenamePattern.MatchString(lowerProjectID) {
		return false
	}

	return true
}
