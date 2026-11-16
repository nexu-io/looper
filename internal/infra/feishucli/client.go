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
	ID       string `json:"message_id"`
	ChatID   string `json:"chat_id"`
	ThreadID string `json:"thread_id"`
	Content  string `json:"content"`
	Deleted  bool   `json:"deleted"`
	Mentions []struct {
		ID string `json:"id"`
	} `json:"mentions"`
	Sender struct {
		ID        string `json:"id"`
		Type      string `json:"sender_type"`
		OpenBotID string `json:"open_bot_id"`
	} `json:"sender"`
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

// VerifyPrivateChat enforces the configured-chat boundary before any write.
// The chat may contain additional human collaborators; it only needs to retain
// the configured Looper owner and the current bot. Reading membership uses the
// authorized user; outbound messages always use the bot.
func (c Client) VerifyPrivateChat(ctx context.Context) error {
	var members struct {
		Users []struct {
			ID string `json:"member_id"`
		} `json:"users"`
		Bots []struct {
			AppID string `json:"app_id"`
		} `json:"bots"`
		HasMore     bool              `json:"has_more"`
		Truncations []json.RawMessage `json:"truncations"`
	}
	if err := c.call(ctx, &members, "im", "+chat-members-list", "--chat-id", c.Config.ChatID, "--as", "user", "--page-all"); err != nil {
		return err
	}
	identity, err := c.run(ctx, nil, "whoami", "--as", "bot")
	if err != nil {
		return err
	}
	var bot struct {
		AppID     string `json:"appId"`
		Available bool   `json:"available"`
	}
	if err := json.Unmarshal(identity, &bot); err != nil {
		return err
	}
	ownerPresent := false
	for _, member := range members.Users {
		if strings.TrimSpace(member.ID) == strings.TrimSpace(c.Config.OwnerOpenID) {
			ownerPresent = true
			break
		}
	}
	botPresent := false
	for _, member := range members.Bots {
		if strings.TrimSpace(member.AppID) == strings.TrimSpace(bot.AppID) {
			botPresent = true
			break
		}
	}
	if members.HasMore || len(members.Truncations) != 0 || !ownerPresent || !bot.Available || !botPresent {
		return fmt.Errorf("Feishu chat must contain the configured owner and current bot")
	}
	return nil
}

func (c Client) root(ctx context.Context, id string) (Message, error) {
	var data struct {
		Messages []Message `json:"messages"`
	}
	if err := c.call(ctx, &data, "im", "+messages-mget", "--message-ids", id, "--as", "user", "--no-reactions"); err != nil {
		return Message{}, err
	}
	for _, message := range data.Messages {
		if message.ID == id && message.ChatID == c.Config.ChatID && !message.Deleted {
			return message, nil
		}
	}
	return Message{}, fmt.Errorf("message %s is absent from the configured test chat", id)
}

func (c Client) ThreadMessages(ctx context.Context, rootID string) ([]Message, error) {
	root, err := c.root(ctx, rootID)
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
	err = c.call(ctx, &data, "im", "+threads-messages-list", "--thread", root.ThreadID, "--as", "user", "--no-reactions", "--order", "asc", "--page-all")
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
func ReplyAfterQuestion(messages []Message, cfg config.HITLFeishuConfig, questionID string) string {
	botID := ""
	for _, message := range messages {
		if message.ID == questionID && message.ChatID == cfg.ChatID && message.Sender.Type == "app" && !message.Deleted {
			botID = message.Sender.OpenBotID
			break
		}
	}
	if botID == "" {
		return ""
	}
	after := false
	var replies []string
	directed := false
	for _, message := range messages {
		if message.ID == questionID {
			after = true
			continue
		}
		if after && message.ChatID == cfg.ChatID && message.Sender.Type == "user" && !message.Deleted && strings.TrimSpace(message.Content) != "" {
			label := "[群内讨论，仅作背景] "
			for _, mention := range message.Mentions {
				if mention.ID == botID {
					directed = true
					label = "[群成员 @bot] "
					break
				}
			}
			replies = append(replies, label+message.Content)
		}
	}
	if !directed {
		return ""
	}
	return strings.Join(replies, "\n\n")
}
