package loops

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
)

// DiscoveryFingerprintVersion is bumped whenever the canonical discovery
// fingerprint payload changes shape so old persisted fingerprints become
// non-comparable instead of silently mismatching.
const DiscoveryFingerprintVersion = "v1"

// metadataAutonomousRecoveryKey is the top-level key under LoopRecord.MetadataJSON
// where autonomous-recovery state lives.
const metadataAutonomousRecoveryKey = "autonomousRecovery"

// metadataLastFailedDiscoveryFingerprintKey is the field under
// metadataAutonomousRecoveryKey where the last terminal-failed discovery
// fingerprint is stored.
const metadataLastFailedDiscoveryFingerprintKey = "lastFailedDiscoveryFingerprint"

// ComputeDiscoveryFingerprint produces a stable hash over the supplied parts.
// Input order is preserved so callers must pass a canonical, role-specific
// ordering. Slice values are sorted in place by the caller before hashing.
func ComputeDiscoveryFingerprint(parts ...string) string {
	h := sha256.New()
	for i, part := range parts {
		if i > 0 {
			h.Write([]byte{0x1f}) // ASCII unit separator avoids collisions with normal text.
		}
		h.Write([]byte(part))
	}
	return DiscoveryFingerprintVersion + ":" + hex.EncodeToString(h.Sum(nil))
}

// CanonicalSortedStrings returns a copy of values trimmed, lowercased, sorted,
// and deduplicated. Used to normalize labels/assignees before fingerprinting.
func CanonicalSortedStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		v = strings.ToLower(v)
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// LastFailedDiscoveryFingerprint returns the persisted fingerprint of the last
// terminal-failed discovery attempt, or empty string if none. metadataJSON may
// be nil.
func LastFailedDiscoveryFingerprint(metadataJSON *string) string {
	if metadataJSON == nil {
		return ""
	}
	raw := strings.TrimSpace(*metadataJSON)
	if raw == "" {
		return ""
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return ""
	}
	autonomous, _ := parsed[metadataAutonomousRecoveryKey].(map[string]any)
	if autonomous == nil {
		return ""
	}
	fingerprint, _ := autonomous[metadataLastFailedDiscoveryFingerprintKey].(string)
	return strings.TrimSpace(fingerprint)
}

// MergeLastFailedDiscoveryFingerprint returns a JSON object encoded as a string
// containing the existing metadata merged with the supplied fingerprint under
// the autonomousRecovery namespace. The fingerprint may be empty, in which case
// the field is cleared so the next discovery is allowed to revive the loop.
func MergeLastFailedDiscoveryFingerprint(metadataJSON *string, fingerprint string) (string, error) {
	parsed := map[string]any{}
	if metadataJSON != nil {
		raw := strings.TrimSpace(*metadataJSON)
		if raw != "" {
			if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
				return "", err
			}
		}
	}
	autonomous, _ := parsed[metadataAutonomousRecoveryKey].(map[string]any)
	if autonomous == nil {
		autonomous = map[string]any{}
	}
	if strings.TrimSpace(fingerprint) == "" {
		delete(autonomous, metadataLastFailedDiscoveryFingerprintKey)
	} else {
		autonomous[metadataLastFailedDiscoveryFingerprintKey] = fingerprint
	}
	if len(autonomous) == 0 {
		delete(parsed, metadataAutonomousRecoveryKey)
	} else {
		parsed[metadataAutonomousRecoveryKey] = autonomous
	}
	encoded, err := json.Marshal(parsed)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

// ShouldSuppressFailedRediscovery reports whether autonomous discovery should
// leave a previously-failed loop in failed state instead of reviving it.
//
// The contract is: suppress only when (a) loop is currently failed, (b) we have
// a recorded last-failed fingerprint, and (c) the current discovery fingerprint
// matches it exactly.
//
// All other cases (no stored fingerprint, status not failed, mismatched
// fingerprint) return false so the loop is revived as before.
func ShouldSuppressFailedRediscovery(loopStatus, storedFingerprint, currentFingerprint string) bool {
	if strings.TrimSpace(loopStatus) != "failed" {
		return false
	}
	stored := strings.TrimSpace(storedFingerprint)
	current := strings.TrimSpace(currentFingerprint)
	if stored == "" || current == "" {
		return false
	}
	return stored == current
}
