package workerhost

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
)

func TestFeishuReplyInAssignedChatResumesEvenWhenReactionFails(t *testing.T) {
	for _, failReaction := range []bool{false, true} {
		t.Run(map[bool]string{false: "receipt", true: "receipt_failure"}[failReaction], func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			db, err := storage.OpenSQLiteCoordinator(ctx, filepath.Join(dir, "state.sqlite"), storage.SQLiteCoordinatorOptions{Migrations: storage.EmbeddedMigrations})
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err := db.MigrationRunner().RunPending(ctx, storage.RunPendingOptions{}); err != nil {
				t.Fatal(err)
			}
			repos := storage.NewRepositories(db.DB())
			now := "2026-11-16T13:25:00.000Z"
			if err := repos.Projects.Upsert(ctx, storage.ProjectRecord{ID: "project", Name: "Test", RepoPath: dir, CreatedAt: now, UpdatedAt: now}); err != nil {
				t.Fatal(err)
			}
			meta, _ := loops.WriteFeishuChat(nil, "oc_assigned", "")
			meta, _ = loops.WriteHITLAsk(&meta, loops.HITLAsk{Transport: "feishu", Status: "awaiting", QuestionMessageID: "question", SessionID: "native-session", Question: "请验收"})
			if err := repos.Loops.Upsert(ctx, storage.LoopRecord{ID: "loop", ProjectID: "project", Type: "worker", Status: "awaiting_human", TargetType: "project", MetadataJSON: &meta, CreatedAt: now, UpdatedAt: now}); err != nil {
				t.Fatal(err)
			}
			if err := repos.FeishuThreads.Upsert(ctx, "root", "loop", "", "oc_assigned", now); err != nil {
				t.Fatal(err)
			}
			// Reading the actual thread is sufficient. Any membership/whoami
			// request is unexpected and fails this real polling path.
			reaction := `printf '%s' '{"ok":true,"data":{}}'`
			if failReaction {
				reaction = "exit 1"
			}
			cli := filepath.Join(dir, "lark-cli")
			script := `#!/bin/sh
case "$*" in *"--as user"*) exit 2 ;; esac
case "$1 $2" in
 "im +messages-mget") printf '%s' '{"ok":true,"data":{"messages":[{"message_id":"root","chat_id":"oc_assigned","thread_id":"thread"}]}}' ;;
 "im +threads-messages-list") printf '%s' '{"ok":true,"data":{"messages":[{"message_id":"question","chat_id":"oc_assigned","sender":{"sender_type":"app","open_bot_id":"ou_bot"}},{"message_id":"answer","chat_id":"oc_assigned","sender":{"sender_type":"user"},"content":"@杨瑾龙的飞书 CLI 验收通过","mentions":[{"id":"ou_bot","name":"杨瑾龙的飞书 CLI"}]}]}}' ;;
 "im reactions")
  case "$*" in
   *om_wrong*|*"--as user"*) exit 2 ;;
   *'"message_id":"answer"'*"--as bot"*) ` + reaction + ` ;;
   *) exit 2 ;;
  esac ;;
 *) exit 2 ;;
esac
`
			if err := os.WriteFile(cli, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			cfg := config.Config{HITL: config.HITLConfig{Enabled: true, AnswerTransport: "feishu", Feishu: &config.HITLFeishuConfig{CLIPath: cli, ChatID: "oc_default"}}}
			woke := false
			err = PollPrivateFeishu(ctx, FeishuPollInput{Config: &cfg, Repos: repos, Now: time.Now, OnHITLAnswerDelivered: func(_ context.Context, id, answer string) {
				woke = id == "loop" && strings.Contains(answer, "@bot 验收通过") && !strings.Contains(answer, "飞书 CLI")
			}})
			if (err != nil) != failReaction || !woke {
				t.Fatalf("wake=%v, err=%v", woke, err)
			}
			loop, err := repos.Loops.GetByID(ctx, "loop")
			if err != nil {
				t.Fatal(err)
			}
			ask, _ := loops.ReadHITLAsk(loop.MetadataJSON)
			if loop.Status != "running" || ask.Status != "answered" || ask.SessionID != "native-session" {
				t.Fatalf("reply lost: %+v, %+v", loop, ask)
			}
		})
	}
}
