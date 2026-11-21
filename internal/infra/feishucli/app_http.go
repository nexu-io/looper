package feishucli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
)

// AppHTTP supplies authentication for Looper's existing notification gateway.
// Card rendering, thread association, progress updates and retries stay there.
func (c Client) AppHTTP(ctx context.Context, method, endpoint string, headers map[string]string, body []byte) (int, []byte, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.Host != "open.feishu.cn" {
		return 0, nil, fmt.Errorf("unsupported Feishu endpoint")
	}
	const messages = "/open-apis/im/v1/messages"
	var args []string
	var input []byte
	switch {
	case method == http.MethodPost && u.Path == "/open-apis/im/v1/images":
		_, params, err := mime.ParseMediaType(headers["Content-Type"])
		if err != nil {
			return 0, nil, err
		}
		reader := multipart.NewReader(bytes.NewReader(body), params["boundary"])
		for {
			part, err := reader.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				return 0, nil, err
			}
			if part.FormName() == "image" {
				input, err = io.ReadAll(io.LimitReader(part, (10<<20)+1))
				if err != nil {
					return 0, nil, err
				}
			}
		}
		if len(input) == 0 || len(input) > 10<<20 {
			return 0, nil, fmt.Errorf("invalid Feishu image size")
		}
		args = []string{"api", "POST", u.Path, "--data", `{"image_type":"message"}`, "--file", "image=-"}
	case strings.HasPrefix(u.Path, messages):
		var payload struct {
			ReceiveID string `json:"receive_id"`
			Type      string `json:"msg_type"`
			Content   string `json:"content"`
			UUID      string `json:"uuid"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return 0, nil, err
		}
		suffix := strings.TrimPrefix(u.Path, messages)
		switch {
		case method == http.MethodPost && suffix == "" && payload.ReceiveID == c.Config.ChatID && u.Query().Get("receive_id_type") == "chat_id":
			args = []string{"im", "+messages-send", "--chat-id", payload.ReceiveID}
		case method == http.MethodPost && suffix == "" && c.Config.AutoCreateTaskChats && strings.HasPrefix(payload.ReceiveID, "oc_") && u.Query().Get("receive_id_type") == "chat_id":
			args = []string{"im", "+messages-send", "--chat-id", payload.ReceiveID}
		case method == http.MethodPost && strings.HasSuffix(suffix, "/reply"):
			id := strings.TrimSuffix(strings.TrimPrefix(suffix, "/"), "/reply")
			root, err := c.rootInChat(ctx, id, "")
			if err != nil {
				return 0, nil, err
			}
			if !c.Config.AutoCreateTaskChats && strings.TrimSpace(c.Config.ChatID) != "" && root.ChatID != strings.TrimSpace(c.Config.ChatID) {
				return 0, nil, fmt.Errorf("Feishu message belongs to another chat")
			}
			args = []string{"im", "+messages-reply", "--message-id", id, "--reply-in-thread"}
		case method == http.MethodPatch && strings.Count(suffix, "/") == 1:
			root, err := c.rootInChat(ctx, strings.TrimPrefix(suffix, "/"), "")
			if err != nil {
				return 0, nil, err
			}
			if !c.Config.AutoCreateTaskChats && strings.TrimSpace(c.Config.ChatID) != "" && root.ChatID != strings.TrimSpace(c.Config.ChatID) {
				return 0, nil, fmt.Errorf("Feishu message belongs to another chat")
			}
			// This CLI version has no typed PATCH command. Use its authenticated API.
			args = []string{"api", "PATCH", u.Path, "--data", "-"}
			input = body
		default:
			return 0, nil, fmt.Errorf("Feishu request is outside the configured test chat")
		}
		if method == http.MethodPost {
			args = append(args, "--msg-type", payload.Type, "--content", payload.Content)
			if payload.UUID != "" {
				args = append(args, "--idempotency-key", payload.UUID)
			}
		}
	default:
		return 0, nil, fmt.Errorf("unsupported Feishu request: %s %s", method, u.Path)
	}
	args = append(args, "--as", "bot")
	data, err := c.callData(ctx, input, args...)
	if err != nil {
		return 0, nil, err
	}
	// The original gateway consumes the OpenAPI response, not the CLI envelope.
	response, err := json.Marshal(struct {
		Code int             `json:"code"`
		Data json.RawMessage `json:"data"`
	}{Data: data})
	return http.StatusOK, response, err
}
