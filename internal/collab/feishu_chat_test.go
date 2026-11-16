package collab

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/infra/feishucli"
	"github.com/nexu-io/looper/internal/storage"
	"github.com/nexu-io/looper/internal/worker"
)

func TestTaskChatManagerSharesScopeBeforeAnyThreadIsPosted(t *testing.T) {
	ctx := context.Background()
	db, err := storage.OpenSQLiteCoordinator(ctx, filepath.Join(t.TempDir(), "state.sqlite"), storage.SQLiteCoordinatorOptions{Migrations: storage.EmbeddedMigrations})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.MigrationRunner().RunPending(ctx, storage.RunPendingOptions{}); err != nil {
		t.Fatal(err)
	}
	repos := storage.NewRepositories(db.DB())
	now := "2026-11-16T00:00:00.000Z"
	project := storage.ProjectRecord{ID: "openmotion", Name: "OpenMotion", RepoPath: t.TempDir(), CreatedAt: now, UpdatedAt: now}
	if err := repos.Projects.Upsert(ctx, project); err != nil {
		t.Fatal(err)
	}
	input := worker.EnqueueInput{ProjectID: project.ID, Repo: "nexu-io/openmotion", Title: "Fix search", IssueNumber: 5113, Module: "motion"}
	encoded, _ := json.Marshal(map[string]any{"worker": input})
	meta := string(encoded)
	loop := storage.LoopRecord{ID: "loop_1", Seq: 1, ProjectID: project.ID, Type: "worker", TargetType: "issue", Status: "queued", Repo: stringPtr("nexu-io/openmotion"), MetadataJSON: &meta, CreatedAt: now, UpdatedAt: now}
	if err := repos.Loops.Upsert(ctx, loop); err != nil {
		t.Fatal(err)
	}
	var calls []string
	client := &feishucli.Client{Config: config.HITLFeishuConfig{AutoCreateTaskChats: true, OwnerOpenID: "ou_owner", InviteOpenIDs: []string{"ou_qa"}}}
	client.Run = func(_ context.Context, _ string, input []byte, args ...string) ([]byte, error) {
		calls = append(calls, strings.Join(args, " "))
		if strings.Contains(strings.Join(args, " "), "+chat-create") {
			// A stop can arrive while the network request is in flight.
			fresh := loop
			fresh.Status = "terminated"
			freshMeta := strings.TrimSuffix(meta, "}") + `,"stopReason":"owner stopped"}`
			fresh.MetadataJSON = &freshMeta
			if err := repos.Loops.Upsert(ctx, fresh); err != nil {
				t.Fatal(err)
			}
			return []byte(`{"ok":true,"data":{"chat_id":"oc_issue"}}`), nil
		}
		if strings.Contains(strings.Join(args, " "), "chat.members create") {
			var body map[string]any
			if err := json.Unmarshal(input, &body); err != nil {
				t.Fatal(err)
			}
			return []byte(`{"ok":true,"data":{}}`), nil
		}
		if strings.Contains(strings.Join(args, " "), "+messages-send") {
			return []byte(`{"ok":true,"data":{"message_id":"om_group_info"}}`), nil
		}
		t.Fatalf("unexpected Feishu call: %v", args)
		return nil, nil
	}
	manager := &taskChatManager{client: client, config: client.Config, repos: repos, defaultState: filepath.Join(t.TempDir(), "default.json")}
	if err := manager.prepare(ctx, loop.ID); err != nil {
		t.Fatal(err)
	}
	updated, err := repos.Loops.GetByID(ctx, loop.ID)
	if err != nil || updated == nil {
		t.Fatalf("read loop: %v", err)
	}
	if chatID, key, ok := readChat(updated.MetadataJSON); !ok || chatID != "oc_issue" || key != "module:openmotion:motion" {
		t.Fatalf("assignment = %q %q %v", chatID, key, ok)
	}
	if updated.Status != "terminated" || !strings.Contains(*updated.MetadataJSON, `"stopReason":"owner stopped"`) {
		t.Fatalf("provisioning overwrote concurrent stop: %+v", updated)
	}
	if len(calls) != 3 || !strings.Contains(calls[0], "+chat-create") || !strings.Contains(calls[1], "chat.members create") || !strings.Contains(calls[2], "+messages-send") {
		t.Fatalf("calls = %#v", calls)
	}
	// A sibling item joins the scope before the first worker creates its
	// anchor. No second group and no shared thread identity are needed.
	sibling := loop
	sibling.ID, sibling.Seq = "loop_2", 2
	if err := repos.Loops.Upsert(ctx, sibling); err != nil {
		t.Fatal(err)
	}
	if err := manager.prepare(ctx, sibling.ID); err != nil {
		t.Fatal(err)
	}
	second, _ := repos.Loops.GetByID(ctx, sibling.ID)
	if chatID, _, _ := readChat(second.MetadataJSON); chatID != "oc_issue" {
		t.Fatalf("sibling chat=%s", chatID)
	}
	if len(calls) != 4 || strings.Contains(calls[3], "+chat-create") || strings.Contains(calls[3], "+messages-send") {
		t.Fatalf("duplicate group: %v", calls)
	}
}

func TestTaskChatManagerUsesConfiguredDefaultForUnscopedRun(t *testing.T) {
	ctx := context.Background()
	db, err := storage.OpenSQLiteCoordinator(ctx, filepath.Join(t.TempDir(), "state.sqlite"), storage.SQLiteCoordinatorOptions{Migrations: storage.EmbeddedMigrations})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.MigrationRunner().RunPending(ctx, storage.RunPendingOptions{}); err != nil {
		t.Fatal(err)
	}
	repos := storage.NewRepositories(db.DB())
	now := "2026-11-16T00:00:00.000Z"
	if err := repos.Projects.Upsert(ctx, storage.ProjectRecord{ID: "local", Name: "Local", RepoPath: t.TempDir(), CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	meta := `{"worker":{"title":"Ad hoc"}}`
	loop := storage.LoopRecord{ID: "loop_2", Seq: 1, ProjectID: "local", Type: "worker", TargetType: "project", MetadataJSON: &meta, CreatedAt: now, UpdatedAt: now}
	if err := repos.Loops.Upsert(ctx, loop); err != nil {
		t.Fatal(err)
	}
	client := &feishucli.Client{Config: config.HITLFeishuConfig{ChatID: "oc_default"}}
	client.Run = func(_ context.Context, _ string, _ []byte, args ...string) ([]byte, error) {
		t.Fatalf("configured default must not create a group: %v", args)
		return nil, nil
	}
	manager := &taskChatManager{client: client, config: client.Config, repos: repos, defaultState: filepath.Join(t.TempDir(), "default.json")}
	if err := manager.prepare(ctx, loop.ID); err != nil {
		t.Fatal(err)
	}
	updated, _ := repos.Loops.GetByID(ctx, loop.ID)
	if chatID, key, ok := readChat(updated.MetadataJSON); !ok || chatID != "oc_default" || key != "" {
		t.Fatalf("default assignment = %q %q %v", chatID, key, ok)
	}
	// A failed scoped creation may already have started a conversation in
	// the fallback group. Retrying must not move it to a new group.
	meta = `{"worker":{"module":"motion"}}`
	loop.MetadataJSON = &meta
	if err := repos.Loops.Upsert(ctx, loop); err != nil {
		t.Fatal(err)
	}
	if err := repos.FeishuThreads.Upsert(ctx, "om_existing", loop.ID, "task", "oc_default", now); err != nil {
		t.Fatal(err)
	}
	manager.config.AutoCreateTaskChats = true
	if err := manager.prepare(ctx, loop.ID); err != nil {
		t.Fatal(err)
	}
	updated, _ = repos.Loops.GetByID(ctx, loop.ID)
	if chatID, _, _ := readChat(updated.MetadataJSON); chatID != "oc_default" {
		t.Fatalf("existing fallback conversation moved to %s", chatID)
	}
}

func readChat(metadata *string) (string, string, bool) {
	var data map[string]any
	if metadata == nil || json.Unmarshal([]byte(*metadata), &data) != nil {
		return "", "", false
	}
	chatID, _ := data["feishuChatId"].(string)
	key, _ := data["feishuChatKey"].(string)
	return chatID, key, chatID != ""
}

func stringPtr(value string) *string { return &value }

func TestPlaneChatScopeOverridesRepositoryIssueNumber(t *testing.T) {
	loop := storage.LoopRecord{ProjectID: "local"}
	for _, input := range []worker.EnqueueInput{
		{StrictDispatchID: "dispatch", IssueNumber: 99, Repo: "acme/repo"},
		{StrictDispatchID: "dispatch", IssueNumber: 100, Repo: "acme/other"},
	} {
		if key := taskChatKey(loop, input); key != "" {
			t.Fatalf("isolated Plane bug must use default group, got %s", key)
		}
		input.ChatScope = "plane:project:requirement:root"
		if key := taskChatKey(loop, input); key != input.ChatScope {
			t.Fatalf("requirement scope lost: %s", key)
		}
	}
}
