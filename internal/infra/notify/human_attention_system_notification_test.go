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

func TestGatewayHumanAttentionUsesSystemNotification(t *testing.T) {
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
		OsascriptPath: scriptPath,
		Repositories:  repos,
	})

	records := gateway.NotifyHumanAttention(ctx, HumanAttentionInput{
		LoopSeq:  42,
		LoopType: "worker",
		Reason:   HumanAttentionAwaitingHuman,
		EntryKey: "run:run_system_notification",
		Subtitle: "acme/looper",
	})
	if got := notificationStatus(records, "osascript"); got != "success" {
		t.Fatalf("osascript status = %q, want success; records=%#v", got, records)
	}

	osascriptCallsBytes, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", capturePath, err)
	}
	osascriptCalls := string(osascriptCallsBytes)
	assertContains(t, osascriptCalls, "display notification")
	assertContains(t, osascriptCalls, "Looper Needs Attention")
	assertContains(t, osascriptCalls, "sound name")
	if strings.Contains(osascriptCalls, "display dialog") {
		t.Fatalf("human-attention notification must not open a dialog: %q", osascriptCalls)
	}
	if strings.Contains(osascriptCalls, "Open Log") || strings.Contains(osascriptCalls, "Open Loop") {
		t.Fatalf("system notification must not contain dialog actions: %q", osascriptCalls)
	}
}
