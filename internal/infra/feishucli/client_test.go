package feishucli

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/config"
)

func TestConfiguredChatSendsDirectlyAsBot(t *testing.T) {
	c := Client{Config: config.HITLFeishuConfig{ChatID: "oc_test"}}
	calls := 0
	c.Run = func(_ context.Context, _ string, _ []byte, args ...string) ([]byte, error) {
		calls++
		if !strings.HasPrefix(strings.Join(args, " "), "im +messages-send --chat-id oc_test ") || !strings.HasSuffix(strings.Join(args, " "), "--as bot") {
			t.Fatalf("unexpected preflight or identity: %v", args)
		}
		return []byte(`{"ok":true,"data":{"message_id":"om_sent"}}`), nil
	}
	if _, _, err := c.AppHTTP(context.Background(), http.MethodPost, "https://open.feishu.cn/open-apis/im/v1/messages?receive_id_type=chat_id", nil, []byte(`{"receive_id":"oc_test","msg_type":"interactive","content":"{}"}`)); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("calls=%d", calls)
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
	if got := ReplyAfterQuestion(messages, cfg, "question"); got.Text != "" {
		t.Fatalf("ordinary discussion woke the task: %q", got)
	}
	// Text that looks like a mention, and mentions of another person, cannot wake it.
	messages[len(messages)-1].Content = "@bot 选 2"
	messages[len(messages)-1].Mentions = []Mention{{ID: "ou_other"}}
	if got := ReplyAfterQuestion(messages, cfg, "question"); got.Text != "" {
		t.Fatalf("fake mention woke task: %q", got)
	}
	messages[len(messages)-1].Content = "为什么建议这个？先不要执行"
	messages[len(messages)-1].Mentions[0].ID = "ou_bot"
	got := ReplyAfterQuestion(messages, cfg, "question")
	if !strings.Contains(got.Text, "[群成员 @bot] 为什么建议这个？先不要执行") {
		t.Fatalf("reply = %q", got)
	}
	messages[len(messages)-1].Sender.ID = "ou_other"
	messages[len(messages)-1].Mentions[0].ID = "ou_other"
	got = ReplyAfterQuestion(messages, cfg, "question")
	if got.Text != "" {
		t.Fatalf("non-owner reply was treated as directed: %q", got)
	}
	messages[len(messages)-1].Mentions[0].ID = "ou_bot"
	got = ReplyAfterQuestion(messages, cfg, "question")
	if !strings.Contains(got.Text, "[群成员 @bot] 为什么建议这个？先不要执行") {
		t.Fatalf("collaborator @bot reply = %q", got)
	}
	messages = append(messages, message("discussion", "oc_test", "ou_owner", "user", "补充背景"))
	got = ReplyAfterQuestion(messages, cfg, "question")
	if !strings.Contains(got.Text, "[群内讨论，仅作背景] 补充背景") {
		t.Fatalf("lost discussion context: %q", got)
	}
	messages[len(messages)-2].Deleted = true
	if got := ReplyAfterQuestion(messages, cfg, "question"); got.Text != "" {
		t.Fatalf("deleted mention woke task: %q", got)
	}

	if got := ReplyAfterQuestion(messages, cfg, "missing-question"); got.Text != "" {
		t.Fatalf("accepted replies without locating question: %q", got)
	}
}

func TestReplySeparatesBotDisplayNameFromHumanAnswer(t *testing.T) {
	question := Message{ID: "question", ChatID: "oc_task"}
	question.Sender.Type, question.Sender.OpenBotID = "app", "ou_bot"
	answer := Message{ID: "answer", ChatID: "oc_task", Content: "@杨瑾龙的飞书 CLI 验收通过", Mentions: []Mention{{ID: "ou_bot", Name: "杨瑾龙的飞书 CLI"}}}
	answer.Sender.Type = "user"
	discussion := Message{ID: "discussion", ChatID: "oc_task", Content: "补充背景"}
	discussion.Sender.Type = "user"
	got := ReplyAfterQuestion([]Message{question, discussion, answer}, config.HITLFeishuConfig{ChatID: "oc_task"}, "question")
	if !strings.Contains(got.Text, "@bot 验收通过") || strings.Contains(got.Text, "飞书 CLI") || len(got.MessageIDs) != 1 || got.MessageIDs[0] != "answer" {
		t.Fatalf("reply = %+v", got)
	}
}

func TestProcessingReactionUsesBotIdentity(t *testing.T) {
	c := Client{}
	c.Run = func(_ context.Context, _ string, _ []byte, args ...string) ([]byte, error) {
		want := `im reactions create --params {"message_id":"om_answer"} --data {"reaction_type":{"emoji_type":"OnIt"}} --as bot`
		if strings.Join(args, " ") != want {
			t.Fatalf("args = %v", args)
		}
		return []byte(`{"ok":true,"identity":"bot","data":{"reaction_id":"reaction"}}`), nil
	}
	if err := c.MarkReplyProcessing(context.Background(), "om_answer"); err != nil {
		t.Fatal(err)
	}
}
