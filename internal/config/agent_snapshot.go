package config

import (
	"encoding/json"
	"fmt"
	"strings"
)

// AgentSnapshot is the durable identity of the agent used for a run.
// It stores only vendor/model/profile identity — never params or env.
//
// Authority: when runs.agent_snapshot_json is non-empty with a vendor, this
// snapshot is execution authority for spawn, prompts, HITL, and disclosure on
// that run lineage. It is not the agent's structured output: vendor/model is
// operator/config policy captured at run create, not something the agent emits.
//
// Trade-off: costs a persisted column, sticky copy across failed/interrupted
// retries, parse validation, and a legacy-null fallback path. Simpler options
// fail the sticky-identity contract — re-resolving live config mid-lineage can
// switch CLI vendor after hot reload (breaking native resume and params
// ownership), and agent output cannot authoritatively choose the executable.
type AgentSnapshot struct {
	Vendor    string  `json:"vendor"`
	Model     *string `json:"model,omitempty"`
	ProfileID string  `json:"profileId,omitempty"`
}

// AgentSnapshotFromResolved builds a snapshot from a resolved coding-role agent.
func AgentSnapshotFromResolved(r ResolvedAgent) AgentSnapshot {
	return AgentSnapshot{
		Vendor:    string(r.Vendor),
		Model:     r.Model,
		ProfileID: strings.TrimSpace(r.ProfileID),
	}
}

// AgentSnapshotFromIdentity builds a snapshot from frozen runner identity fields.
// model is a pointer so nil (unset) and non-nil empty (explicit suppress to the
// vendor default) stay distinct through freeze; collapsing empty to omitempty
// nil would make ParamsForRoleVendor preserve params --model/-m on thaw.
func AgentSnapshotFromIdentity(vendor string, model *string, profileID string) AgentSnapshot {
	snapshot := AgentSnapshot{
		Vendor:    strings.TrimSpace(vendor),
		ProfileID: strings.TrimSpace(profileID),
	}
	if model != nil {
		trimmed := strings.TrimSpace(*model)
		snapshot.Model = &trimmed
	}
	return snapshot
}

// MarshalAgentSnapshot encodes a snapshot as JSON text for runs.agent_snapshot_json.
func MarshalAgentSnapshot(s AgentSnapshot) (string, error) {
	encoded, err := json.Marshal(s)
	if err != nil {
		return "", fmt.Errorf("marshal agent snapshot: %w", err)
	}
	return string(encoded), nil
}

// ParseAgentSnapshot decodes runs.agent_snapshot_json.
func ParseAgentSnapshot(raw string) (AgentSnapshot, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return AgentSnapshot{}, fmt.Errorf("agent snapshot is empty")
	}
	var snapshot AgentSnapshot
	if err := json.Unmarshal([]byte(trimmed), &snapshot); err != nil {
		return AgentSnapshot{}, fmt.Errorf("parse agent snapshot: %w", err)
	}
	return snapshot, nil
}

// ResolveRunAgentSnapshotJSON picks the durable agent snapshot for a new run.
// sticky is true when continuing any failed/interrupted predecessor run (any
// step, including first-step retries). A non-empty predecessor snapshot is
// copied so identity stays sticky across the retry lineage, but only after
// parse + non-empty vendor validation (invalid predecessor fails loudly).
// Otherwise the snapshot is built from the runner's frozen vendor/model/profile.
// model is a pointer so explicit empty suppress survives freeze.
// legacyResume is true when continuing a predecessor that had no snapshot (pre-migration).
// Marshal failures return an error so callers can fail run creation loudly.
func ResolveRunAgentSnapshotJSON(predecessorSnapshot *string, sticky bool, vendor string, model *string, profileID string) (snapshotJSON *string, legacyResume bool, err error) {
	if sticky {
		if predecessorSnapshot != nil {
			if trimmed := strings.TrimSpace(*predecessorSnapshot); trimmed != "" {
				snapshot, parseErr := ParseAgentSnapshot(trimmed)
				if parseErr != nil {
					return nil, false, parseErr
				}
				if strings.TrimSpace(snapshot.Vendor) == "" {
					return nil, false, fmt.Errorf("agent snapshot missing vendor")
				}
				copied := trimmed
				return &copied, false, nil
			}
		}
		legacyResume = true
	}
	if strings.TrimSpace(vendor) == "" {
		// No durable identity to freeze; leave null so IdentityFromRunSnapshot
		// can fall back to the runner's live fields (tests / unconfigured agent).
		return nil, legacyResume, nil
	}
	encoded, marshalErr := MarshalAgentSnapshot(AgentSnapshotFromIdentity(vendor, model, profileID))
	if marshalErr != nil {
		return nil, legacyResume, marshalErr
	}
	return &encoded, legacyResume, nil
}

// IdentityFromRunSnapshot returns the vendor/model/profile that should drive a run.
// When snapshotJSON is non-empty and has a non-empty vendor it is the authority
// (fromSnapshot=true). Malformed non-empty snapshots, or snapshots with an empty
// vendor, return an error (do not fall back to live identity).
// Only empty/null snapshots fall back to the runner's frozen identity
// (fromSnapshot=false) for pre-migration legacy runs.
// model is a pointer so nil (unset) and non-nil empty (suppress) stay distinct.
func IdentityFromRunSnapshot(snapshotJSON *string, fallbackVendor string, fallbackModel *string, fallbackProfile string) (vendor string, model *string, profile string, fromSnapshot bool, err error) {
	fallbackVendor = strings.TrimSpace(fallbackVendor)
	fallbackProfile = strings.TrimSpace(fallbackProfile)
	if snapshotJSON == nil || strings.TrimSpace(*snapshotJSON) == "" {
		return fallbackVendor, fallbackModel, fallbackProfile, false, nil
	}
	snapshot, parseErr := ParseAgentSnapshot(*snapshotJSON)
	if parseErr != nil {
		return "", nil, "", false, parseErr
	}
	vendor = strings.TrimSpace(snapshot.Vendor)
	if vendor == "" {
		return "", nil, "", false, fmt.Errorf("agent snapshot missing vendor")
	}
	if snapshot.Model != nil {
		m := strings.TrimSpace(*snapshot.Model)
		model = &m
	}
	profile = strings.TrimSpace(snapshot.ProfileID)
	return vendor, model, profile, true, nil
}
