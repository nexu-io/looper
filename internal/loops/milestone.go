package loops

import "encoding/json"

const milestonesMetadataKey = "milestones"

// milestonesCap bounds the retained milestone log so a long-running loop's
// metadata (and the anchor card) stays compact; the oldest are dropped.
const milestonesCap = 12

// Milestone is one human-scannable event in a loop's story — a decision, a phase
// completing, a PR opening — timestamped so the anchor reads as a narrative
// (who decided what, how long each phase took, the PR link) instead of a single
// current-status line.
type Milestone struct {
	At   string `json:"at"`   // ISO-8601 timestamp
	Text string `json:"text"` // already-formatted, human-facing (lark_md ok)
}

// ReadMilestones returns a loop's milestone log in chronological order.
func ReadMilestones(metadataJSON *string) []Milestone {
	meta := parseMetadataObject(metadataJSON)
	raw, ok := meta[milestonesMetadataKey]
	if !ok {
		return nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var out []Milestone
	if err := json.Unmarshal(encoded, &out); err != nil {
		return nil
	}
	return out
}

// AppendMilestone adds one milestone to a loop's metadata (trimming to the most
// recent milestonesCap), preserving all other keys, and returns the updated JSON.
// A milestone whose text duplicates the immediately-preceding one is dropped so a
// retried event doesn't stutter the log.
func AppendMilestone(metadataJSON *string, m Milestone) (string, error) {
	existing := ReadMilestones(metadataJSON)
	if n := len(existing); n > 0 && existing[n-1].Text == m.Text {
		return marshalWithMilestones(metadataJSON, existing)
	}
	existing = append(existing, m)
	if len(existing) > milestonesCap {
		existing = existing[len(existing)-milestonesCap:]
	}
	return marshalWithMilestones(metadataJSON, existing)
}

func marshalWithMilestones(metadataJSON *string, milestones []Milestone) (string, error) {
	meta := parseMetadataObject(metadataJSON)
	encoded, err := json.Marshal(milestones)
	if err != nil {
		return "", err
	}
	var asSlice []any
	if err := json.Unmarshal(encoded, &asSlice); err != nil {
		return "", err
	}
	meta[milestonesMetadataKey] = asSlice
	out, err := json.Marshal(meta)
	if err != nil {
		return "", err
	}
	return string(out), nil
}
