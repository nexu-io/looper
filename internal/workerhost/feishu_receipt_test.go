package workerhost

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
)

func TestFeishuReceiptFollowsAcceptanceAndNeverReplays(t *testing.T) {
	for _, mode := range []string{"accepted", "ordinary-discussion", "stale", "reaction-error"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			db, err := storage.OpenSQLiteCoordinator(ctx, filepath.Join(dir, "state.sqlite"), storage.SQLiteCoordinatorOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err := db.MigrationRunner().RunPending(ctx); err != nil {
				t.Fatal(err)
			}
			repos := storage.NewRepositories(db.DB())
			now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
			stamp, base, target, loopID := now.Format("2006-01-02T15:04:05.000Z"), "main", "project:project", "loop"
			if err := repos.Projects.Upsert(ctx, storage.ProjectRecord{ID: "project", Name: "test", RepoPath: dir, BaseBranch: &base, CreatedAt: stamp, UpdatedAt: stamp}); err != nil {
				t.Fatal(err)
			}
			meta, err := loops.WriteHITLAsk(nil, loops.HITLAsk{Transport: "feishu", Status: "awaiting", Question: "Keep this?", QuestionMessageID: "om_question"})
			if err != nil {
				t.Fatal(err)
			}
			loop := storage.LoopRecord{ID: loopID, Seq: 1, ProjectID: "project", Type: "worker", TargetType: "project", TargetID: &target, Status: "awaiting_human", MetadataJSON: &meta, CreatedAt: stamp, UpdatedAt: stamp}
			if err := repos.Loops.Upsert(ctx, loop); err != nil {
				t.Fatal(err)
			}
			if err := repos.Queue.Upsert(ctx, storage.QueueItemRecord{ID: "queue", LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: target, Status: "cancelled", Priority: storage.QueuePriorityWorker, MaxAttempts: 3, DedupeKey: "worker", AvailableAt: stamp, CreatedAt: stamp, UpdatedAt: stamp}); err != nil {
				t.Fatal(err)
			}
			if err := repos.FeishuThreads.Upsert(ctx, "om_anchor", loopID, "", "oc_test", stamp); err != nil {
				t.Fatal(err)
			}
			calls, cli := filepath.Join(dir, "calls"), filepath.Join(dir, "lark-cli")
			quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
			mentions := `[{"id":"ou_bot"}]`
			if mode == "ordinary-discussion" {
				mentions = `[]`
			}
			reaction := `{"ok":true,"data":{"reaction_id":"receipt"}}`
			if mode == "reaction-error" {
				reaction = `{"ok":false,"error":{"message":"reaction denied"}}`
			}
			script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >> %s
case "$1 $2" in
  'whoami --as') printf '%%s' '{"appId":"cli_bot","available":true}' ;;
  'im +chat-members-list') printf '%%s' '{"ok":true,"data":{"users":[{"member_id":"ou_owner"}],"bots":[{"app_id":"cli_bot"}]}}' ;;
  'im +messages-mget')
    case " $* " in
      *' --message-ids om_reply '*) printf '%%s' '{"ok":true,"data":{"messages":[{"message_id":"om_reply","chat_id":"oc_test","sender":{"sender_type":"user"}}]}}' ;;
      *) printf '%%s' '{"ok":true,"data":{"messages":[{"message_id":"om_anchor","chat_id":"oc_test","thread_id":"omt_thread"}]}}' ;;
    esac ;;
  'im +threads-messages-list') printf '%%s' '{"ok":true,"data":{"messages":[{"message_id":"om_question","chat_id":"oc_test","sender":{"sender_type":"app","open_bot_id":"ou_bot"}},{"message_id":"om_background","chat_id":"oc_test","content":"background","sender":{"sender_type":"user"}},{"message_id":"om_reply","chat_id":"oc_test","content":"please change it","sender":{"sender_type":"user"},"mentions":%s}]}}' ;;
  'im reactions') printf '%%s' '%s' ;;
  *) exit 1 ;;
esac
`, quote(calls), mentions, reaction)
			if err := os.WriteFile(cli, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			cfg := config.Config{HITL: config.HITLConfig{Enabled: true, AnswerTransport: "feishu", Feishu: &config.HITLFeishuConfig{ChatID: "oc_test", OwnerOpenID: "ou_owner", CLIPath: cli}}}
			wakes := 0
			input := FeishuPollInput{Config: &cfg, Repos: repos, Now: func() time.Time { return now }, OnHITLAnswerDelivered: func(_ context.Context, _ string, _ string) {
				current, _ := repos.Loops.GetByID(ctx, loopID)
				queue, _ := repos.Queue.GetByID(ctx, "queue")
				ask, _ := loops.ReadHITLAsk(current.MetadataJSON)
				if current.Status != "running" || queue.Status != "queued" || ask.Status != "answered" {
					t.Fatal("woke before durable acceptance")
				}
				log, _ := os.ReadFile(calls)
				if strings.Contains(string(log), "im reactions create") {
					t.Fatal("receipt was sent before acceptance/wake")
				}
				wakes++
			}}
			if mode == "stale" {
				input.AllowClaim = func() error { loop.Status = "terminated"; return repos.Loops.Upsert(ctx, loop) }
			}
			err = PollPrivateFeishu(ctx, input)
			if (err != nil) != (mode == "reaction-error") {
				t.Fatalf("unexpected poll error: %v", err)
			}
			expected := 1
			if mode == "ordinary-discussion" || mode == "stale" {
				expected = 0
			}
			if wakes != expected {
				t.Fatalf("wakes=%d, want %d", wakes, expected)
			}
			if err := PollPrivateFeishu(ctx, input); err != nil {
				t.Fatal(err)
			}
			log, _ := os.ReadFile(calls)
			if got := strings.Count(string(log), "im reactions create"); got != expected {
				t.Fatalf("receipt attempts=%d, want %d; %s", got, expected, log)
			}
			if strings.Contains(string(log), `"message_id":"om_background"`) {
				t.Fatal("acknowledged ordinary background")
			}
		})
	}
}
