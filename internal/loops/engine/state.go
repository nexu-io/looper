package engine

import (
	"encoding/json"
	"strings"
)

const StateMetadataKey = "lifecycleState"

type Phase string

const (
	PhaseRunning Phase = "running"
	PhaseBlocked Phase = "blocked"
	PhaseDone    Phase = "done"
	PhaseDead    Phase = "dead"
)

type State struct {
	Phase     Phase  `json:"phase"`
	Condition string `json:"condition,omitempty"`
	Outcome   string `json:"outcome,omitempty"`
	Reason    string `json:"reason,omitempty"`
	UpdatedAt string `json:"updatedAt,omitempty"`
}

func FromLegacy(status, condition, nowISO string) State {
	state := State{Phase: PhaseRunning, UpdatedAt: nowISO}
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "paused", "waiting", "awaiting_human", "human_takeover", "shepherding":
		state.Phase, state.Condition = PhaseBlocked, firstNonEmpty(condition, strings.TrimSpace(status))
	case "completed", "terminated", "stopped":
		state.Phase, state.Outcome = PhaseDone, strings.TrimSpace(status)
	case "failed", "interrupted":
		state.Phase, state.Reason = PhaseDead, strings.TrimSpace(status)
	}
	return state
}

func Read(metadataJSON *string) (State, bool) {
	meta := map[string]any{}
	if metadataJSON == nil || json.Unmarshal([]byte(*metadataJSON), &meta) != nil {
		return State{}, false
	}
	raw, ok := meta[StateMetadataKey]
	if !ok {
		return State{}, false
	}
	payload, _ := json.Marshal(raw)
	var state State
	if json.Unmarshal(payload, &state) != nil || !state.Valid() {
		return State{}, false
	}
	return state, true
}

func Write(metadataJSON *string, state State) (string, error) {
	meta := map[string]any{}
	if metadataJSON != nil && strings.TrimSpace(*metadataJSON) != "" {
		if err := json.Unmarshal([]byte(*metadataJSON), &meta); err != nil {
			return "", err
		}
	}
	meta[StateMetadataKey] = state
	payload, err := json.Marshal(meta)
	return string(payload), err
}

func (s State) Valid() bool {
	return s.Phase == PhaseRunning || s.Phase == PhaseBlocked || s.Phase == PhaseDone || s.Phase == PhaseDead
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
