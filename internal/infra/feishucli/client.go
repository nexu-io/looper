// Package feishucli transports private test-chat messages through an already
// authenticated lark-cli. Task state and agent execution stay in Looper.
package feishucli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/config"
)

type Client struct {
	Config config.HITLFeishuConfig
	Run    func(context.Context, string, []byte, ...string) ([]byte, error)
}

type Message struct {
	ID       string    `json:"message_id"`
	ChatID   string    `json:"chat_id"`
	ThreadID string    `json:"thread_id"`
	Content  string    `json:"content"`
	Deleted  bool      `json:"deleted"`
	Mentions []Mention `json:"mentions"`
	Sender   struct {
		ID        string `json:"id"`
		Type      string `json:"sender_type"`
		OpenBotID string `json:"open_bot_id"`
	} `json:"sender"`
}

type Mention struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Key  string `json:"key"`
}

type Reply struct {
	Text       string
	MessageIDs []string
}

// MarkReplyProcessing is a receipt, not a decision or an acceptance result.
func (c Client) MarkReplyProcessing(ctx context.Context, messageID string) error {
	params, _ := json.Marshal(map[string]string{"message_id": messageID})
	_, err := c.callData(ctx, nil, "im", "reactions", "create", "--params", string(params), "--data", `{"reaction_type":{"emoji_type":"OnIt"}}`, "--as", "bot")
	return err
}

// CreateGroup creates a private Feishu group owned by the Looper bot. The
// caller deliberately supplies the member list; role discovery stays with the
// agent and is never hard-coded into the transport.
func (c Client) CreateGroup(ctx context.Context, name, description string, users []string) (string, error) {
	args := []string{"im", "+chat-create", "--name", strings.TrimSpace(name), "--chat-mode", "group", "--type", "private", "--as", "bot", "--set-bot-manager"}
	if description = strings.TrimSpace(description); description != "" {
		args = append(args, "--description", description)
	}
	if ids := cleanOpenIDs(users); len(ids) > 0 {
		args = append(args, "--users", strings.Join(ids, ","))
	}
	data, err := c.callData(ctx, nil, args...)
	if err != nil {
		return "", err
	}
	var result struct {
		ChatID string `json:"chat_id"`
		Chat   struct {
			ChatID string `json:"chat_id"`
		} `json:"chat"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return "", fmt.Errorf("decode created Feishu chat: %w", err)
	}
	chatID := strings.TrimSpace(result.ChatID)
	if chatID == "" {
		chatID = strings.TrimSpace(result.Chat.ChatID)
	}
	if chatID == "" {
		return "", fmt.Errorf("Feishu chat create returned no chat_id")
	}
	return chatID, nil
}

// InitializeAnnouncement fills a new group's announcement. An existing
// announcement belongs to its human editors and is left intact.
func (c Client) InitializeAnnouncement(ctx context.Context, chatID, title, link, rule string) error {
	path := "/open-apis/docx/v1/chats/" + chatID + "/announcement/blocks"
	var blocks struct {
		Items []struct {
			ID       string   `json:"block_id"`
			Type     int      `json:"block_type"`
			Children []string `json:"children"`
		} `json:"items"`
	}
	if err := c.call(ctx, &blocks, "api", "GET", path, "--as", "bot"); err != nil {
		return err
	}
	root := ""
	for _, block := range blocks.Items {
		if block.Type == 1 {
			if len(block.Children) > 0 {
				return nil
			}
			root = block.ID
		}
	}
	if root == "" {
		return fmt.Errorf("Feishu announcement has no root block")
	}
	text := func(content string, style map[string]any) any {
		return map[string]any{"block_type": 2, "text": map[string]any{"elements": []any{
			map[string]any{"text_run": map[string]any{"content": content, "text_element_style": style}},
		}}}
	}
	children := []any{text(title, map[string]any{"bold": true})}
	if link != "" {
		children = append(children, text("在 Plane 查看", map[string]any{"link": map[string]string{"url": link}}))
	}
	children = append(children, text(rule, map[string]any{}))
	body, err := json.Marshal(map[string]any{"children": children})
	if err != nil {
		return err
	}
	_, err = c.callData(ctx, body, "api", "POST", path+"/"+root+"/children", "--data", "-", "--as", "bot")
	if err != nil {
		return err
	}
	// Writing the document alone does not display it above the conversation.
	_, err = c.callData(ctx, nil, "api", "POST", "/open-apis/im/v1/chats/"+chatID+"/top_notice/put_top_notice",
		"--data", `{"chat_top_notice":[{"action_type":"2"}]}`, "--as", "bot")
	return err
}

// InviteMembers is best-effort at the API level: succeed_type=1 lets Feishu
// add visible members while reporting unavailable ids instead of rejecting the
// entire group. The agent can still mention and invite additional people later.
func (c Client) InviteMembers(ctx context.Context, chatID string, users []string) error {
	chatID = strings.TrimSpace(chatID)
	ids := cleanOpenIDs(users)
	if chatID == "" || len(ids) == 0 {
		return nil
	}
	body, err := json.Marshal(map[string]any{"id_list": ids})
	if err != nil {
		return err
	}
	data, err := c.callData(ctx, body, "im", "chat.members", "create", "--chat-id", chatID, "--member-id-type", "open_id", "--succeed-type", "1", "--data", "-", "--as", "bot")
	if err != nil {
		return err
	}
	var result struct {
		Invalid []string `json:"invalid_id_list"`
		Missing []string `json:"not_existed_id_list"`
		Pending []string `json:"pending_approval_id_list"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return err
	}
	unavailable := append(append(result.Invalid, result.Missing...), result.Pending...)
	if len(unavailable) > 0 {
		return fmt.Errorf("Feishu members unavailable or pending approval: %s; invite them through an authorized group member", strings.Join(unavailable, ", "))
	}
	return nil
}

func cleanOpenIDs(ids []string) []string {
	seen := map[string]struct{}{}
	result := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || !strings.HasPrefix(id, "ou_") {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		result = append(result, id)
	}
	return result
}

func (c Client) run(ctx context.Context, input []byte, args ...string) ([]byte, error) {
	command := c.Config.CLIPath
	if command == "" {
		command = "lark-cli"
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if c.Run != nil {
		return c.Run(ctx, command, input, args...)
	}
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Stdin = bytes.NewReader(input)
	cmd.Env = append(os.Environ(), "LARKSUITE_CLI_NO_UPDATE_NOTIFIER=1", "LARKSUITE_CLI_NO_SKILLS_NOTIFIER=1")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("lark-cli request failed: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

func (c Client) call(ctx context.Context, out any, args ...string) error {
	data, err := c.callData(ctx, nil, args...)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}

func (c Client) callData(ctx context.Context, input []byte, args ...string) (json.RawMessage, error) {
	raw, err := c.run(ctx, input, args...)
	if err != nil {
		return nil, err
	}
	var envelope struct {
		OK    bool            `json:"ok"`
		Data  json.RawMessage `json:"data"`
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("decode lark-cli response: %w", err)
	}
	if !envelope.OK {
		return nil, fmt.Errorf("lark-cli: %s", envelope.Error)
	}
	return envelope.Data, nil
}

func (c Client) root(ctx context.Context, id string) (Message, error) {
	return c.rootInChat(ctx, id, c.Config.ChatID)
}

func (c Client) rootInChat(ctx context.Context, id, expectedChatID string) (Message, error) {
	var data struct {
		Messages []Message `json:"messages"`
	}
	if err := c.call(ctx, &data, "im", "+messages-mget", "--message-ids", id, "--as", "bot", "--no-reactions"); err != nil {
		return Message{}, err
	}
	for _, message := range data.Messages {
		if message.ID == id && (strings.TrimSpace(expectedChatID) == "" || message.ChatID == strings.TrimSpace(expectedChatID)) && !message.Deleted {
			return message, nil
		}
	}
	return Message{}, fmt.Errorf("message %s is absent from the configured test chat", id)
}

func (c Client) ThreadMessages(ctx context.Context, rootID string) ([]Message, error) {
	return c.ThreadMessagesInChat(ctx, rootID, c.Config.ChatID)
}

func (c Client) ThreadMessagesInChat(ctx context.Context, rootID, chatID string) ([]Message, error) {
	root, err := c.rootInChat(ctx, rootID, chatID)
	if err != nil {
		return nil, err
	}
	if root.ThreadID == "" {
		return []Message{root}, nil
	}
	var data struct {
		Messages []Message `json:"messages"`
		HasMore  bool      `json:"has_more"`
	}
	err = c.call(ctx, &data, "im", "+threads-messages-list", "--thread", root.ThreadID, "--as", "bot", "--no-reactions", "--order", "asc", "--page-all")
	if err != nil {
		return nil, err
	}
	if data.HasMore {
		return nil, fmt.Errorf("Feishu thread is incomplete; refusing to infer an answer")
	}
	return data.Messages, nil
}

// ReplyAfterQuestion returns conversation after this exact question. It never
// treats ordinary discussion as a command. Only a real mention of the question's
// bot wakes the task; other owner messages are supplied as labeled context.
func ReplyAfterQuestion(messages []Message, cfg config.HITLFeishuConfig, questionID string) Reply {
	botID := ""
	for _, message := range messages {
		if message.ID == questionID && message.ChatID == cfg.ChatID && message.Sender.Type == "app" && !message.Deleted {
			botID = message.Sender.OpenBotID
			break
		}
	}
	if botID == "" {
		return Reply{}
	}
	after := false
	var replies []string
	var directedIDs []string
	for _, message := range messages {
		if message.ID == questionID {
			after = true
			continue
		}
		if after && message.ChatID == cfg.ChatID && message.Sender.Type == "user" && !message.Deleted && strings.TrimSpace(message.Content) != "" {
			label := "[群内讨论，仅作背景] "
			content := message.Content
			for _, mention := range message.Mentions {
				if mention.ID == botID {
					directedIDs = append(directedIDs, message.ID)
					label = "[群成员 @bot] "
					// CLI renders a real mention as @display-name. Keep it visibly
					// separate from the person's words so the bot name is not read
					// as the subject of an approval (e.g. "飞书 CLI 验收通过").
					if mention.Name != "" {
						content = strings.ReplaceAll(content, "@"+mention.Name, "@bot")
					}
					if mention.Key != "" {
						content = strings.ReplaceAll(content, mention.Key, "@bot")
					}
					break
				}
			}
			replies = append(replies, label+content)
		}
	}
	if len(directedIDs) == 0 {
		return Reply{}
	}
	return Reply{Text: strings.Join(replies, "\n\n"), MessageIDs: directedIDs}
}
