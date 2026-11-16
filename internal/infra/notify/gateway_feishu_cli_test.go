package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/infra/feishucli"
	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
)

func TestFeishuCLIPreservesOriginalCardsAcrossProgressDecisionsAndRestart(t *testing.T) {
	ctx := context.Background()
	repos := storage.NewRepositories(openNotifyCoordinator(t, t.TempDir()).DB())
	now := "2026-11-15T01:00:00.000Z"
	if err := repos.Projects.Upsert(ctx, storage.ProjectRecord{ID: "p", Name: "Test", RepoPath: t.TempDir(), CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	loop := storage.LoopRecord{ID: "loop", Seq: 1, ProjectID: "p", Type: "worker", TargetType: "project", TargetID: stringPointer("project:p"), Status: "running", MetadataJSON: stringPointer(`{"title":"修复问候语","worker":{"issueUrl":"https://plane.example/workspace/browse/LOOP-42"}}`), CreatedAt: now, UpdatedAt: now}
	if err := repos.Loops.Upsert(ctx, loop); err != nil {
		t.Fatal(err)
	}
	type sent struct{ kind, content, parent string }
	messages := map[string]sent{}
	patches := map[string]int{}
	count := 0
	arg := func(args []string, flag string) string {
		for i := range args {
			if args[i] == flag && i+1 < len(args) {
				return args[i+1]
			}
		}
		return ""
	}
	client := &feishucli.Client{Config: config.HITLFeishuConfig{ChatID: "oc_private", OwnerOpenID: "ou_owner"}}
	client.Run = func(_ context.Context, _ string, input []byte, args ...string) ([]byte, error) {
		var data any = map[string]any{}
		switch strings.Join(args[:2], " ") {
		case "whoami --as":
			return []byte(`{"appId":"cli_bot","available":true}`), nil
		case "im +chat-members-list":
			return []byte(`{"ok":true,"data":{"users":[{"member_id":"ou_owner"}],"bots":[{"app_id":"cli_bot"}]}}`), nil
		case "im +messages-mget":
			id := arg(args, "--message-ids")
			if _, ok := messages[id]; !ok {
				t.Fatalf("unknown message %s", id)
			}
			data = map[string]any{"messages": []any{map[string]string{"message_id": id, "chat_id": "oc_private"}}}
		case "im +messages-send", "im +messages-reply":
			count++
			id := fmt.Sprintf("om_%d", count)
			messages[id] = sent{kind: arg(args, "--msg-type"), content: arg(args, "--content"), parent: arg(args, "--message-id")}
			data = map[string]string{"message_id": id, "chat_id": "oc_private"}
		case "api PATCH":
			id := strings.TrimPrefix(args[2], "/open-apis/im/v1/messages/")
			var body struct {
				Content string `json:"content"`
			}
			if err := json.Unmarshal(input, &body); err != nil {
				t.Fatal(err)
			}
			m := messages[id]
			m.content = body.Content
			messages[id] = m
			patches[id]++
		default:
			t.Fatalf("unexpected CLI request: %v", args)
		}
		return json.Marshal(map[string]any{"ok": true, "data": data})
	}
	newGateway := func() *Gateway {
		return NewGateway(Options{Repositories: repos, FeishuCLI: client, Config: config.NotificationConfig{Webhook: config.WebhookNotificationConfig{Enabled: true, Mode: "app", ChatID: "oc_private"}}, Now: func() time.Time { return time.Date(2026, 11, 15, 1, 0, 0, 0, time.UTC) }, ResolveOwnerOpenID: func(string) string { return "ou_owner" }})
	}
	gateway := newGateway()
	gateway.RefreshThreadHeader(ctx, loop.ID, []string{"exec: go test ./..."}, 10)
	root, _ := repos.FeishuThreads.RootByLoop(ctx, loop.ID)
	feed, _ := repos.FeishuLiveFeeds.MessageByLoop(ctx, loop.ID)
	if root == "" || feed == "" || root == feed || messages[root].kind != "interactive" || messages[feed].kind != "interactive" || messages[feed].parent != root {
		t.Fatalf("missing original task/live cards: root=%q feed=%q messages=%+v", root, feed, messages)
	}
	if !strings.Contains(messages[root].content, "实现中") || !strings.Contains(messages[root].content, "https://plane.example/workspace/browse/LOOP-42") {
		t.Fatalf("root not running: %s", messages[root].content)
	}
	loadLoop := func() storage.LoopRecord {
		current, err := repos.Loops.GetByID(ctx, "loop")
		if err != nil || current == nil {
			t.Fatalf("read loop: %v", err)
		}
		return *current
	}
	loop = loadLoop()
	loop.Status = "awaiting_human"
	meta, _ := loops.WriteHITLAsk(loop.MetadataJSON, loops.HITLAsk{Transport: "feishu", Status: "awaiting", Question: "接受这次修改吗？", Options: []string{"接受", "调整"}, SessionID: "native"})
	loop.MetadataJSON = &meta
	if err := repos.Loops.Upsert(ctx, loop); err != nil {
		t.Fatal(err)
	}
	if err := gateway.SendHITLAsk(ctx, HITLAskCard{Transport: "feishu", LoopID: loop.ID, LoopSeq: loop.Seq, Title: "接受这次修改吗？", Question: "接受这次修改吗？", Options: []string{"接受", "调整"}}); err != nil {
		t.Fatal(err)
	}
	loop = loadLoop()
	ask, _ := loops.ReadHITLAsk(loop.MetadataJSON)
	question := ask.QuestionMessageID
	if question == "" || messages[question].parent != root || !strings.Contains(messages[root].content, "ou_owner") || !strings.Contains(messages[question].content, "@bot 回复") || strings.Contains(messages[question].content, "回复不会被读取") {
		t.Fatalf("question/hold card missing: ask=%+v root=%s question=%s", ask, messages[root].content, messages[question].content)
	}
	if !strings.Contains(messages[question].content, "1. 接受") || !strings.Contains(messages[question].content, "2. 调整") || strings.Contains(messages[question].content, "前往回答") {
		t.Fatalf("question duplicates its title or loses task context: %s", messages[question].content)
	}
	// A delayed intent for a previous ask must not replace the current question.
	if err := gateway.SendHITLAsk(ctx, HITLAskCard{Transport: "feishu", LoopID: loop.ID, AskedAt: "old", Question: "接受这次修改吗？"}); err != nil {
		t.Fatal(err)
	}
	staleAsk, _ := loops.ReadHITLAsk(loadLoop().MetadataJSON)
	if count != 3 || staleAsk.QuestionMessageID != question {
		t.Fatal("stale intent replaced the question")
	}
	// A fresh gateway preserves the conversation; receiving a reply does not rewrite it.
	gateway = newGateway()
	gateway.MarkAskAnswered(ctx, loop.ID, "为什么这样改？")
	if patches[question] != 0 || strings.Contains(messages[question].content, "已收到回复") {
		t.Fatal("reply was duplicated into an acknowledgement card")
	}
	loop = loadLoop()
	loop.Status = "running"
	if err := repos.Loops.Upsert(ctx, loop); err != nil {
		t.Fatal(err)
	}
	gateway.RefreshThreadHeader(ctx, loop.ID, []string{"exec: go test ./..."}, 20)
	loop = loadLoop()
	loop.Status = "completed"
	if err := repos.Loops.Upsert(ctx, loop); err != nil {
		t.Fatal(err)
	}
	gateway.RefreshThreadHeader(ctx, loop.ID, nil, 0)
	if strings.Contains(messages[feed].content, "实时") || !strings.Contains(messages[feed].content, "执行记录") {
		t.Fatalf("finished feed still promises live updates: %s", messages[feed].content)
	}
	if count != 3 || patches[feed] != 1 || patches[root] < 3 || !strings.Contains(messages[root].content, "已完成") || !strings.Contains(messages[root].content, "https://plane.example/workspace/browse/LOOP-42") {
		t.Fatalf("cards were replaced or stale: count=%d patches=%v root=%s", count, patches, messages[root].content)
	}
}

func TestFeishuAskDoesNotRepeatQuestionAsTitle(t *testing.T) {
	for _, title := range []string{"", "为什么推荐这个方案？"} {
		card, err := buildFeishuAskCard(HITLAskCard{Transport: "feishu", Title: title, Question: "为什么推荐这个方案？"})
		var parsed struct {
			Body map[string]any `json:"body"`
		}
		if err == nil {
			err = json.Unmarshal(card, &parsed)
		}
		body, _ := json.Marshal(parsed.Body)
		if err != nil || strings.Count(string(body), "为什么推荐这个方案？") != 1 {
			t.Fatalf("question repeated: %s, error: %v", card, err)
		}
	}
}
