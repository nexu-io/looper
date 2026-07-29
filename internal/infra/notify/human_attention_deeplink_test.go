package notify

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
)

func TestGatewayActionRequiredOpensDashboardLoopDetailDeepLink(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	rootDir := t.TempDir()
	capturePath := filepath.Join(rootDir, "osascript.log")
	scriptPath := filepath.Join(rootDir, "osascript")
	writeExecutableScript(t, scriptPath, "#!/bin/sh\nprintf '%s\n' \"$*\" >> \""+capturePath+"\"\n")

	coordinator := openNotifyCoordinator(t, rootDir)
	repos := storage.NewRepositories(coordinator.DB())
	gateway := NewGateway(Options{
		Config: config.NotificationConfig{
			InApp: true,
			Osascript: config.OsascriptNotificationConfig{
				Enabled:               true,
				SoundForLevels:        []config.NotificationSoundLevel{config.NotificationSoundLevelActionRequired},
				ThrottleWindowSeconds: 60,
			},
		},
		OsascriptPath:     scriptPath,
		LogFilePath:       filepath.Join(rootDir, "logs", "looperd.log"),
		DashboardBaseURL:  "http://127.0.0.1:17310",
		DashboardAuthMode: config.AuthModeNone,
		Repositories:      repos,
	})

	// Avoid FK targets that do not exist in this isolated DB; deep-link uses LoopSeq only.
	records := gateway.NotifyHumanAttention(ctx, HumanAttentionInput{
		LoopSeq:    42,
		LoopType:   "worker",
		Reason:     HumanAttentionAwaitingHuman,
		EntryKey:   "run:run_1",
		Subtitle:   "acme/looper",
		EntityType: "loop",
		EntityID:   "loop_1",
	})
	if got := notificationStatus(records, "osascript"); got != "success" {
		t.Fatalf("osascript status = %q, want success; records=%#v", got, records)
	}

	osascriptCallsBytes, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", capturePath, err)
	}
	osascriptCalls := string(osascriptCallsBytes)
	assertContains(t, osascriptCalls, "Open Loop")
	assertContains(t, osascriptCalls, "http://127.0.0.1:17310/dashboard/loops/42")
	if strings.Contains(osascriptCalls, "token") || strings.Contains(osascriptCalls, "code=") {
		t.Fatalf("deep link must not include auth material: %q", osascriptCalls)
	}

	// Permanent entry dedupe: second emit for same entry is a no-op.
	second := gateway.NotifyHumanAttention(ctx, HumanAttentionInput{
		LoopSeq:  42,
		Reason:   HumanAttentionAwaitingHuman,
		EntryKey: "run:run_1",
	})
	if len(second) != 0 {
		t.Fatalf("second NotifyHumanAttention records = %#v, want empty (deduped)", second)
	}
}
