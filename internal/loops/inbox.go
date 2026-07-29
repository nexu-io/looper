package loops

import "encoding/json"

const humanInboxMetadataKey = "humanInbox"
const humanInboxCap = 20

// HumanMessage is retained for local/runtime handoff messages. Feishu is
// notification-only and has no inbound route that writes this mailbox.
type HumanMessage struct {
	At   string `json:"at"`
	Text string `json:"text"`
}

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

func AppendHumanMessage(metadataJSON *string, message HumanMessage) (string, error) {
	messages := append(ReadHumanInbox(metadataJSON), message)
	if len(messages) > humanInboxCap {
		messages = messages[len(messages)-humanInboxCap:]
	}
	return marshalWithHumanInbox(metadataJSON, messages)
}

func ClearHumanInbox(metadataJSON *string) (string, error) {
	return marshalWithHumanInbox(metadataJSON, nil)
}

func marshalWithHumanInbox(metadataJSON *string, messages []HumanMessage) (string, error) {
	meta := parseMetadataObject(metadataJSON)
	if len(messages) == 0 {
		delete(meta, humanInboxMetadataKey)
	} else {
		encoded, err := json.Marshal(messages)
		if err != nil {
			return "", err
		}
		var values []any
		if err := json.Unmarshal(encoded, &values); err != nil {
			return "", err
		}
		meta[humanInboxMetadataKey] = values
	}
	out, err := json.Marshal(meta)
	if err != nil {
		return "", err
	}
	return string(out), nil
}
