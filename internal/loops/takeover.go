package loops

import "encoding/json"

const takeoverResumeMetadataKey = "takeoverResume"

// TakeoverResume records that a loop was handed back after an interactive human
// takeover, carrying the native session id the human drove so the daemon's next
// worker run resumes THAT session (seeing the human's turns) rather than starting
// a fresh one. Consumed after one resume.
type TakeoverResume struct {
	SessionID string `json:"sessionId,omitempty"`
	Prompt    string `json:"prompt,omitempty"`
}

// ReadTakeoverResume returns the pending takeover-resume marker, if any.
func ReadTakeoverResume(metadataJSON *string) (TakeoverResume, bool) {
	meta := parseMetadataObject(metadataJSON)
	raw, ok := meta[takeoverResumeMetadataKey]
	if !ok {
		return TakeoverResume{}, false
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return TakeoverResume{}, false
	}
	var tr TakeoverResume
	if err := json.Unmarshal(encoded, &tr); err != nil {
		return TakeoverResume{}, false
	}
	return tr, true
}

// WriteTakeoverResume merges the takeover-resume marker into a loop's metadata,
// preserving all other keys.
func WriteTakeoverResume(metadataJSON *string, tr TakeoverResume) (string, error) {
	meta := parseMetadataObject(metadataJSON)
	encoded, err := json.Marshal(tr)
	if err != nil {
		return "", err
	}
	var asMap map[string]any
	if err := json.Unmarshal(encoded, &asMap); err != nil {
		return "", err
	}
	meta[takeoverResumeMetadataKey] = asMap
	out, err := json.Marshal(meta)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// ClearTakeoverResume removes the takeover-resume marker from a loop's metadata.
func ClearTakeoverResume(metadataJSON *string) (string, error) {
	meta := parseMetadataObject(metadataJSON)
	delete(meta, takeoverResumeMetadataKey)
	out, err := json.Marshal(meta)
	if err != nil {
		return "", err
	}
	return string(out), nil
}
