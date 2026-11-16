package feishucli

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/config"
)

func TestConfiguredChatAllowsCollaboratorsBeforeSending(t *testing.T) {
	for _, members := range []string{
		`{"users":[{"member_id":"ou_owner"},{"member_id":"ou_colleague"}],"bots":[{"app_id":"cli_bot"}]}`,
		`{"users":[{"member_id":"ou_owner"}],"bots":[{"app_id":"cli_bot"}]}`,
	} {
		c := Client{Config: config.HITLFeishuConfig{ChatID: "oc_test", OwnerOpenID: "ou_owner"}}
		c.Run = func(_ context.Context, _ string, _ []byte, args ...string) ([]byte, error) {
			if args[0] == "whoami" {
				return []byte(`{"appId":"cli_bot","available":true}`), nil
			}
			if args[0] == "im" && args[1] == "+messages-send" {
				return []byte(`{"ok":true,"data":{"message_id":"om_sent"}}`), nil
			}
			if args[1] != "+chat-members-list" {
				t.Fatalf("must not send: %v", args)
			}
			return []byte(`{"ok":true,"data":` + members + `}`), nil
		}
		if _, _, err := c.AppHTTP(context.Background(), http.MethodPost, "https://open.feishu.cn/open-apis/im/v1/messages?receive_id_type=chat_id", nil, []byte(`{"receive_id":"oc_test","msg_type":"interactive","content":"{}"}`)); err != nil {
			t.Fatalf("rejected configured chat: %s: %v", members, err)
		}
	}
	for _, members := range []string{
		`{"users":[{"member_id":"ou_other"}],"bots":[{"app_id":"cli_bot"}]}`,
		`{"users":[{"member_id":"ou_owner"}],"bots":[{"app_id":"cli_other"}]}`,
		`{"users":[{"member_id":"ou_owner"}],"bots":[{"app_id":"cli_bot"}],"has_more":true}`,
	} {
		c := Client{Config: config.HITLFeishuConfig{ChatID: "oc_test", OwnerOpenID: "ou_owner"}}
		c.Run = func(_ context.Context, _ string, _ []byte, args ...string) ([]byte, error) {
			if args[0] == "whoami" {
				return []byte(`{"appId":"cli_bot","available":true}`), nil
			}
			if args[1] != "+chat-members-list" {
				t.Fatalf("must not send: %v", args)
			}
			return []byte(`{"ok":true,"data":` + members + `}`), nil
		}
		if _, _, err := c.AppHTTP(context.Background(), http.MethodPost, "https://open.feishu.cn/open-apis/im/v1/messages?receive_id_type=chat_id", nil, []byte(`{"receive_id":"oc_test","msg_type":"interactive","content":"{}"}`)); err == nil {
			t.Fatalf("accepted invalid configured chat: %s", members)
		}
	}
}

func TestReplyAndPatchRejectMessageFromAnotherChat(t *testing.T) {
	c := Client{Config: config.HITLFeishuConfig{ChatID: "oc_test", OwnerOpenID: "ou_owner"}}
	c.Run = func(_ context.Context, _ string, _ []byte, args ...string) ([]byte, error) {
		switch strings.Join(args[:2], " ") {
		case "whoami --as":
			return []byte(`{"appId":"cli_bot","available":true}`), nil
		case "im +chat-members-list":
			return []byte(`{"ok":true,"data":{"users":[{"member_id":"ou_owner"}],"bots":[{"app_id":"cli_bot"}]}}`), nil
		case "im +messages-mget":
			return []byte(`{"ok":true,"data":{"messages":[{"message_id":"om_root","chat_id":"oc_big_group"}]}}`), nil
		default:
			t.Fatalf("must not send: %v", args)
			return nil, nil
		}
	}
	for _, request := range []struct{ method, suffix string }{{http.MethodPost, "/reply"}, {http.MethodPatch, ""}} {
		if _, _, err := c.AppHTTP(context.Background(), request.method, "https://open.feishu.cn/open-apis/im/v1/messages/om_root"+request.suffix, nil, []byte(`{"msg_type":"interactive","content":"{}"}`)); err == nil {
			t.Fatal("accepted a message in another chat")
		}
	}
}

func TestRepliesBelongToCurrentQuestionAndCollaborators(t *testing.T) {
	cfg := config.HITLFeishuConfig{ChatID: "oc_test", OwnerOpenID: "ou_owner"}
	message := func(id, chat, sender, senderType, text string) Message {
		m := Message{ID: id, ChatID: chat, Content: text}
		m.Sender.ID, m.Sender.Type = sender, senderType
		return m
	}
	messages := []Message{
		message("old", "oc_test", "ou_owner", "user", "old decision"),
		message("question", "oc_test", "cli_bot", "app", "new question"),
		message("bot", "oc_test", "cli_bot", "app", "approve"),
		message("other-chat", "oc_big_group", "ou_owner", "user", "wrong chat"),
		message("other-person", "oc_test", "ou_other", "user", "wrong person"),
		message("reply", "oc_test", "ou_owner", "user", "为什么建议这个？先不要执行"),
	}
	messages[1].Sender.OpenBotID = "ou_bot"
	if got := ReplyAfterQuestion(messages, cfg, "question"); got != "" {
		t.Fatalf("ordinary discussion woke the task: %q", got)
	}
	// Text that looks like a mention, and mentions of another person, cannot wake it.
	messages[len(messages)-1].Content = "@bot 选 2"
	messages[len(messages)-1].Mentions = []struct {
		ID string `json:"id"`
	}{{ID: "ou_other"}}
	if got := ReplyAfterQuestion(messages, cfg, "question"); got != "" {
		t.Fatalf("fake mention woke task: %q", got)
	}
	messages[len(messages)-1].Content = "为什么建议这个？先不要执行"
	messages[len(messages)-1].Mentions[0].ID = "ou_bot"
	got := ReplyAfterQuestion(messages, cfg, "question")
	if !strings.Contains(got, "[群成员 @bot] 为什么建议这个？先不要执行") {
		t.Fatalf("reply = %q", got)
	}
	messages[len(messages)-1].Sender.ID = "ou_other"
	messages[len(messages)-1].Mentions[0].ID = "ou_other"
	got = ReplyAfterQuestion(messages, cfg, "question")
	if got != "" {
		t.Fatalf("non-owner reply was treated as directed: %q", got)
	}
	messages[len(messages)-1].Mentions[0].ID = "ou_bot"
	got = ReplyAfterQuestion(messages, cfg, "question")
	if !strings.Contains(got, "[群成员 @bot] 为什么建议这个？先不要执行") {
		t.Fatalf("collaborator @bot reply = %q", got)
	}
	messages = append(messages, message("discussion", "oc_test", "ou_owner", "user", "补充背景"))
	got = ReplyAfterQuestion(messages, cfg, "question")
	if !strings.Contains(got, "[群内讨论，仅作背景] 补充背景") {
		t.Fatalf("lost discussion context: %q", got)
	}
	messages[len(messages)-2].Deleted = true
	if got := ReplyAfterQuestion(messages, cfg, "question"); got != "" {
		t.Fatalf("deleted mention woke task: %q", got)
	}

	if got := ReplyAfterQuestion(messages, cfg, "missing-question"); got != "" {
		t.Fatalf("accepted replies without locating question: %q", got)
	}
}
