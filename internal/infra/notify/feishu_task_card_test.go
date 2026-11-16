package notify

import (
	"context"
	"encoding/json"
	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestCollaborationCardShowsOnlyCurrentExecution(t *testing.T) {
	ctx := context.Background()
	repos := storage.NewRepositories(openNotifyCoordinator(t, t.TempDir()).DB())
	now := "2026-11-15T01:00:00.000Z"
	if err := repos.Projects.Upsert(ctx, storage.ProjectRecord{ID: "p", Name: "Test", RepoPath: t.TempDir(), CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	meta := `{"title":"确认称呼","worker":{"summary":"旧摘要"},"milestones":[{"at":"2026-11-15T01:00:00Z","text":"已收到回复：为什么推荐小杨？"}]}`
	loop := storage.LoopRecord{ID: "loop", Seq: 4, ProjectID: "p", Type: "worker", TargetType: "project", Status: "completed", MetadataJSON: &meta, CreatedAt: now, UpdatedAt: now}
	if err := repos.Loops.Upsert(ctx, loop); err != nil {
		t.Fatal(err)
	}
	for _, r := range []storage.RunRecord{
		{ID: "old", LoopID: loop.ID, Status: "success", StartedAt: "2026-11-14T01:00:00Z", CreatedAt: now, UpdatedAt: now},
		{ID: "current", LoopID: loop.ID, Status: "success", Summary: stringPointer("手动 PR 阶段的旧技术通知"), StartedAt: now, EndedAt: stringPointer("2026-11-15T01:00:43Z"), CreatedAt: now, UpdatedAt: now},
	} {
		if err := repos.Runs.Upsert(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	for _, e := range []storage.AgentExecutionRecord{
		{ID: "exec-old", RunID: stringPointer("old"), Status: "completed", Summary: stringPointer("旧执行交付"), StartedAt: now, CreatedAt: now, UpdatedAt: now},
		{ID: "exec-current", RunID: stringPointer("current"), Status: "completed", Summary: stringPointer("已改为你好，小杨；本地内容校验通过。"), StartedAt: now, CreatedAt: now, UpdatedAt: now},
	} {
		if err := repos.AgentExecutions.Upsert(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	gateway := NewGateway(Options{Repositories: repos,
		ResolveOwnerOpenID: func(string) string { return "ou_owner" },
		ResolveOwnerName: func(id string) string {
			if id == "ou_owner" {
				return "杨瑾龙"
			}
			return ""
		}})
	raw, ok := gateway.feishuThreadHeaderCard(ctx, loop.ID)
	if !ok {
		t.Fatal("card not rendered")
	}
	var card map[string]any
	if err := json.Unmarshal([]byte(raw), &card); err != nil {
		t.Fatal(err)
	}
	if card["schema"] != "2.0" || card["header"] != nil {
		t.Fatal("expected compact headerless collaboration card")
	}
	elements := card["body"].(map[string]any)["elements"].([]any)
	var outside, history strings.Builder
	for _, item := range elements {
		element := item.(map[string]any)
		encoded, _ := json.Marshal(element)
		if element["tag"] == "collapsible_panel" {
			if element["expanded"] != false {
				t.Fatal("history expanded by default")
			}
			history.Write(encoded)
		} else {
			outside.Write(encoded)
		}
	}
	if !strings.Contains(outside.String(), "已改为你好，小杨") || strings.Contains(outside.String(), "旧摘要") || strings.Contains(outside.String(), "旧执行交付") || strings.Contains(outside.String(), "为什么推荐") {
		t.Fatalf("main card does not show only the current delivery: %s", outside.String())
	}
	if history.Len() != 0 || strings.Contains(raw, "持续协作") || strings.Contains(raw, "此前进展") || strings.Contains(raw, "展开话题") {
		t.Fatalf("main card contains redundant history or instructions: %s", raw)
	}
	if !strings.Contains(outside.String(), "43s") {
		t.Fatal("elapsed time lost after restart")
	}
	if !strings.Contains(outside.String(), "杨瑾龙的 Looper") || strings.Contains(raw, "ou_owner") || strings.Contains(raw, "执行：") {
		t.Fatalf("completed card must retain named attribution without an @ notification: %s", raw)
	}
	columns := elements[0].(map[string]any)["columns"].([]any)
	owner := columns[len(columns)-1].(map[string]any)["elements"].([]any)[0].(map[string]any)
	if owner["content"] != "<text_tag color='neutral'>杨瑾龙的 Looper</text_tag>" || owner["text_size"] != "notation" {
		t.Fatalf("owner must be a compact tag at the right of the heading: %v", owner)
	}
	for _, status := range []string{"queued", "running", "awaiting_human", "completed", "failed"} {
		loop.Status = status
		if err := repos.Loops.Upsert(ctx, loop); err != nil {
			t.Fatal(err)
		}
		updated, _ := gateway.feishuThreadHeaderCard(ctx, loop.ID)
		if !strings.Contains(updated, "杨瑾龙的 Looper") {
			t.Fatalf("%s card lost owner attribution: %s", status, updated)
		}
	}
	// Current question takes precedence over every previous delivery.
	loop.Status = "awaiting_human"
	updated, _ := loops.WriteHITLAsk(loop.MetadataJSON, loops.HITLAsk{Status: "awaiting", Question: "是否接受当前交付？", MentionOpenIDs: []string{"owner", "ou_qa"}})
	loop.MetadataJSON = &updated
	if err := repos.Loops.Upsert(ctx, loop); err != nil {
		t.Fatal(err)
	}
	waiting, _ := gateway.feishuThreadHeaderCard(ctx, loop.ID)
	if !strings.Contains(waiting, "等待 owner") || !strings.Contains(waiting, "ou_qa") || strings.Contains(waiting, "id=owner") || strings.Contains(waiting, "等你") {
		t.Fatalf("waiting person is not explicit: %s", waiting)
	}
	brief, _ := gateway.feishuTaskUpdate(ctx, &loop, nil)
	if brief != "是否接受当前交付？" {
		t.Fatalf("waiting brief=%q", brief)
	}
	// Resuming must not retain the prior run's success summary or elapsed time.
	loop.Status = "running"
	brief, elapsed := gateway.feishuTaskUpdate(ctx, &loop, []string{"✅ go test ./..."})
	if brief != "正在跑测试/构建" || elapsed != 0 {
		t.Fatalf("running brief=%q elapsed=%d", brief, elapsed)
	}
	loop.Status = "failed"
	current, _ := repos.Runs.GetByID(ctx, "current")
	current.ErrorMessage = stringPointer("验证未通过")
	if err := repos.Runs.Upsert(ctx, *current); err != nil {
		t.Fatal(err)
	}
	brief, _ = gateway.feishuTaskUpdate(ctx, &loop, nil)
	if brief != "验证未通过" {
		t.Fatalf("failure masked by earlier success: %q", brief)
	}
}

func TestCollaborationCardExcerptPreservesChinese(t *testing.T) {
	brief := strings.Repeat("中文进展", 80)
	excerpt := taskCardExcerpt(brief, 180)
	if !utf8.ValidString(excerpt) || len([]rune(excerpt)) != 181 {
		t.Fatal("excerpt split Chinese or exceeded its bound")
	}
}

func TestThreadQuestionKeepsDecisionVisibleAndFoldsSupportingDetails(t *testing.T) {
	raw, err := buildFeishuThreadQuestion(HITLAskCard{
		Question:       "请看话题里的截图，页面是否符合预期？",
		Options:        []string{"通过", "需要修改"},
		Recommendation: strings.Repeat("浏览器测试和复现步骤。", 40),
		Consequences:   map[string]string{"通过": "继续跟进当前 PR", "需要修改": "修改后请你复验"},
		OwnerOpenID:    "ou_owner", MentionOpenIds: []string{"ou_qa", "ou_product"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var card map[string]any
	if err := json.Unmarshal(raw, &card); err != nil {
		t.Fatal(err)
	}
	var visible, folded strings.Builder
	for _, item := range card["body"].(map[string]any)["elements"].([]any) {
		element := item.(map[string]any)
		encoded, _ := json.Marshal(element)
		if element["tag"] == "collapsible_panel" {
			if element["expanded"] != false {
				t.Fatal("supporting details must start collapsed")
			}
			folded.Write(encoded)
		} else {
			visible.Write(encoded)
		}
	}
	for _, want := range []string{"页面是否符合预期", "1. 通过", "2. 需要修改", "ou_qa", "ou_product", "@bot"} {
		if !strings.Contains(visible.String(), want) {
			t.Fatalf("decision or selected recipient hidden: %q", want)
		}
	}
	for _, detail := range []string{"浏览器测试和复现步骤", "继续跟进当前 PR", "修改后请你复验"} {
		if strings.Contains(visible.String(), detail) || !strings.Contains(folded.String(), detail) {
			t.Fatalf("supporting detail should only appear when expanded: %q", detail)
		}
	}
	if strings.Contains(visible.String(), "ou_owner") {
		t.Fatal("owner must not replace agent-selected reviewers")
	}
}
