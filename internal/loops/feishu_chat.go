package loops

import (
	"encoding/json"
	"strings"
)

const feishuChatMetadataKey = "feishuChatId"
const feishuChatKeyMetadataKey = "feishuChatKey"

// ReadFeishuChat returns the task's durable collaboration chat assignment.
// Keeping this in loop metadata avoids a schema migration and survives host
// restarts while preserving all other worker/checkpoint metadata.
func ReadFeishuChat(metadataJSON *string) (chatID, key string, ok bool) {
	meta := parseMetadataObject(metadataJSON)
	chatID, _ = meta[feishuChatMetadataKey].(string)
	key, _ = meta[feishuChatKeyMetadataKey].(string)
	chatID = strings.TrimSpace(chatID)
	key = strings.TrimSpace(key)
	return chatID, key, chatID != ""
}

func WriteFeishuChat(metadataJSON *string, chatID, key string) (string, error) {
	meta := parseMetadataObject(metadataJSON)
	meta[feishuChatMetadataKey] = strings.TrimSpace(chatID)
	if strings.TrimSpace(key) == "" {
		delete(meta, feishuChatKeyMetadataKey)
	} else {
		meta[feishuChatKeyMetadataKey] = strings.TrimSpace(key)
	}
	out, err := json.Marshal(meta)
	return string(out), err
}
