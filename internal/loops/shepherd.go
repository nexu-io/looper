package loops

import "encoding/json"

const shepherdMetadataKey = "shepherd"

// Shepherd is the DURABLE control-truth marker for a worker loop that is driving
// its own implementation PR to merge under looper:auto. It lives in loop
// metadata (NOT loop.Status, which failure/HITL paths may transiently move), so
// the start-step resolver keys on Active to force stepShepherd across passes,
// crash restarts, and status churn. Outcome is set to a terminal value once the
// PR merges/closes.
type Shepherd struct {
	// Active gates the whole flow. While true (and the loop has a PR checkpoint)
	// the resolver routes every run to stepShepherd. Cleared on terminal.
	Active bool `json:"active"`
	// Phase drives the card title: reviewing | fixing | awaiting_validation |
	// awaiting_merge.
	Phase string `json:"phase,omitempty"`
	// PassCount is the global stop-ask cap counter (only agent-waking passes).
	PassCount int `json:"passCount,omitempty"`
	// LastSignal is the folded PR signal tuple last acted on, for wake dedupe.
	LastSignal string `json:"lastSignal,omitempty"`
	// RepliedThreads records review-thread ids already answered, so replies are
	// not duplicated across passes.
	RepliedThreads []string `json:"repliedThreads,omitempty"`
	// SessionLost is set when the worker's native session id could not be
	// recovered for a pass, so the prompt tells the agent to rebuild from the PR
	// diff rather than silently starting fresh.
	SessionLost bool `json:"sessionLost,omitempty"`
	// HeadSHA is the PR head last observed, to detect human/agent force-pushes.
	HeadSHA string `json:"headSha,omitempty"`
	// Outcome is the terminal reason once done: "merged" or "abandoned".
	Outcome string `json:"outcome,omitempty"`
	// LastReportedPhase is the last phase a thread milestone note was posted for, so
	// entering a phase (e.g. awaiting_validation → @QA, awaiting_merge → @owner)
	// reports exactly once and re-reports only when the phase genuinely changes.
	LastReportedPhase string `json:"lastReportedPhase,omitempty"`
	// PlaneState is the last Plane work-item lifecycle state successfully synced by
	// the shepherd reconciler. It makes the best-effort notification write durable:
	// a transient Plane outage is retried later instead of leaving the item at Todo.
	PlaneState string `json:"planeState,omitempty"`
	// PlaneStateAttemptAt rate-limits retries while Plane is unavailable.
	PlaneStateAttemptAt string `json:"planeStateAttemptAt,omitempty"`
}

// ReadShepherd returns the shepherd marker, if present.
func ReadShepherd(metadataJSON *string) (Shepherd, bool) {
	meta := parseMetadataObject(metadataJSON)
	raw, ok := meta[shepherdMetadataKey]
	if !ok {
		return Shepherd{}, false
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return Shepherd{}, false
	}
	var s Shepherd
	if err := json.Unmarshal(encoded, &s); err != nil {
		return Shepherd{}, false
	}
	return s, true
}

// ShepherdActive is the hot-path guard the resolver uses: true only when the
// marker exists AND is active.
func ShepherdActive(metadataJSON *string) bool {
	s, ok := ReadShepherd(metadataJSON)
	return ok && s.Active
}

// WriteShepherd merges the shepherd marker into a loop's metadata, preserving
// all other keys.
func WriteShepherd(metadataJSON *string, s Shepherd) (string, error) {
	meta := parseMetadataObject(metadataJSON)
	encoded, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	var asMap map[string]any
	if err := json.Unmarshal(encoded, &asMap); err != nil {
		return "", err
	}
	meta[shepherdMetadataKey] = asMap
	out, err := json.Marshal(meta)
	if err != nil {
		return "", err
	}
	return string(out), nil
}
