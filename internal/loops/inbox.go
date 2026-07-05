package loops

import "encoding/json"

const humanInboxMetadataKey = "humanInbox"

// humanInboxCap bounds the pending human messages retained for a loop so a chatty
// thread can't grow metadata unbounded; oldest are dropped.
const humanInboxCap = 20

// HumanMessage is one free-text message a human sent into a loop's thread at any
// time — a follow-up question, a clarification, a new instruction — queued until
// the loop's next turn drains it and feeds it to the agent (same session). Unlike
// a button-click decision, a message does not by itself resolve a pending ask; the
// agent reads it in context and decides whether to proceed, answer, or re-ask.
type HumanMessage struct {
	At   string `json:"at"`
	Text string `json:"text"`
}

// ReadHumanInbox returns a loop's queued human messages in arrival order.
func ReadHumanInbox(metadataJSON *string) []HumanMessage {
	meta := parseMetadataObject(metadataJSON)
	raw, ok := meta[humanInboxMetadataKey]
	if !ok {
		return nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var out []HumanMessage
	if err := json.Unmarshal(encoded, &out); err != nil {
		return nil
	}
	return out
}

// AppendHumanMessage queues one human message (trimming to the most recent
// humanInboxCap), preserving all other metadata keys.
func AppendHumanMessage(metadataJSON *string, m HumanMessage) (string, error) {
	msgs := append(ReadHumanInbox(metadataJSON), m)
	if len(msgs) > humanInboxCap {
		msgs = msgs[len(msgs)-humanInboxCap:]
	}
	return marshalWithHumanInbox(metadataJSON, msgs)
}

// ClearHumanInbox drops all queued human messages (called after the agent drains
// them in a turn).
func ClearHumanInbox(metadataJSON *string) (string, error) {
	return marshalWithHumanInbox(metadataJSON, nil)
}

func marshalWithHumanInbox(metadataJSON *string, msgs []HumanMessage) (string, error) {
	meta := parseMetadataObject(metadataJSON)
	if len(msgs) == 0 {
		delete(meta, humanInboxMetadataKey)
	} else {
		encoded, err := json.Marshal(msgs)
		if err != nil {
			return "", err
		}
		var asSlice []any
		if err := json.Unmarshal(encoded, &asSlice); err != nil {
			return "", err
		}
		meta[humanInboxMetadataKey] = asSlice
	}
	out, err := json.Marshal(meta)
	if err != nil {
		return "", err
	}
	return string(out), nil
}
