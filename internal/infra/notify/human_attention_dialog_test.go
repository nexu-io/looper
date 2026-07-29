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

func TestGatewayHumanAttentionFallsBackToDaemonLogWithoutDeepLink(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	rootDir := t.TempDir()
	capturePath := filepath.Join(rootDir, "osascript.log")
	scriptPath := filepath.Join(rootDir, "osascript")
	writeExecutableScript(t, scriptPath, "#!/bin/sh\nprintf '%s\n' \"$*\" >> \""+capturePath+"\"\n")
	logPath := filepath.Join(rootDir, "logs", "looperd.log")

	coordinator := openNotifyCoordinator(t, rootDir)
	repos := storage.NewRepositories(coordinator.DB())
	gateway := NewGateway(Options{
		Config: config.NotificationConfig{
			InApp: true,
			Osascript: config.OsascriptNotificationConfig{
				Enabled:               true,
				ThrottleWindowSeconds: 60,
			},
		},
		OsascriptPath: scriptPath,
		LogFilePath:   logPath,
		Repositories:  repos,
	})

	// Human-attention without loop seq / base URL → Open Log dialog.
	gateway.NotifyHumanAttention(ctx, HumanAttentionInput{
		Reason:   HumanAttentionManualIntervention,
		EntryKey: "queue:q1:t1",
	})

	osascriptCallsBytes, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", capturePath, err)
	}
	osascriptCalls := string(osascriptCallsBytes)
	assertContains(t, osascriptCalls, "Open Log")
	assertContains(t, osascriptCalls, logPath)
	if strings.Contains(osascriptCalls, "Open Loop") {
		t.Fatalf("osascript calls = %q, want Open Log without Open Loop", osascriptCalls)
	}
}

func TestGatewayOrdinaryActionRequiredStaysLightweightOsascript(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	rootDir := t.TempDir()
	capturePath := filepath.Join(rootDir, "osascript.log")
	scriptPath := filepath.Join(rootDir, "osascript")
	writeExecutableScript(t, scriptPath, "#!/bin/sh\nprintf '%s\n' \"$*\" >> \""+capturePath+"\"\n")
	logPath := filepath.Join(rootDir, "logs", "looperd.log")

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
		LogFilePath:   logPath,
		Repositories:  repos,
	})

	// Worker skipped / PR-ready shape: action_required without OperatorAttention.
	gateway.Notify(ctx, SystemNotificationPayload{
		Level:     "action_required",
		Title:     "Looper Worker Needs Attention",
		Body:      "skipped: dirty worktree",
		Sound:     "Funk",
		DedupeKey: "runtime.worker.action_required:run_1",
	})

	osascriptCallsBytes, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", capturePath, err)
	}
	osascriptCalls := string(osascriptCallsBytes)
	assertContains(t, osascriptCalls, "display notification")
	if strings.Contains(osascriptCalls, "display dialog") {
		t.Fatalf("ordinary action_required must stay lightweight, got dialog: %q", osascriptCalls)
	}
	if strings.Contains(osascriptCalls, "Open Log") || strings.Contains(osascriptCalls, "Open Loop") {
		t.Fatalf("ordinary action_required must not offer Open Log/Loop: %q", osascriptCalls)
	}
}

func TestGatewayHumanAttentionLocalTokenFallsBackToDaemonLog(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	rootDir := t.TempDir()
	capturePath := filepath.Join(rootDir, "osascript.log")
	scriptPath := filepath.Join(rootDir, "osascript")
	writeExecutableScript(t, scriptPath, "#!/bin/sh\nprintf '%s\n' \"$*\" >> \""+capturePath+"\"\n")
	logPath := filepath.Join(rootDir, "logs", "looperd.log")

	coordinator := openNotifyCoordinator(t, rootDir)
	repos := storage.NewRepositories(coordinator.DB())
	gateway := NewGateway(Options{
		Config: config.NotificationConfig{
			InApp: true,
			Osascript: config.OsascriptNotificationConfig{
				Enabled:               true,
				ThrottleWindowSeconds: 60,
			},
		},
		OsascriptPath:     scriptPath,
		LogFilePath:       logPath,
		DashboardBaseURL:  "http://127.0.0.1:17310",
		DashboardAuthMode: config.AuthModeLocalToken,
		Repositories:      repos,
	})

	records := gateway.NotifyHumanAttention(ctx, HumanAttentionInput{
		LoopSeq:  42,
		Reason:   HumanAttentionAwaitingHuman,
		EntryKey: "run:run_local_token",
	})
	if got := notificationStatus(records, "osascript"); got != "success" {
		t.Fatalf("osascript status = %q, want success; records=%#v", got, records)
	}

	osascriptCallsBytes, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", capturePath, err)
	}
	osascriptCalls := string(osascriptCallsBytes)
	assertContains(t, osascriptCalls, "Open Log")
	assertContains(t, osascriptCalls, logPath)
	if strings.Contains(osascriptCalls, "Open Loop") {
		t.Fatalf("local-token must not offer unusable Open Loop: %q", osascriptCalls)
	}
	if strings.Contains(osascriptCalls, "/dashboard/loops/") {
		t.Fatalf("local-token must not open bare dashboard deep link: %q", osascriptCalls)
	}
	if strings.Contains(osascriptCalls, "code=") || strings.Contains(osascriptCalls, "token") {
		t.Fatalf("notification must not embed auth material: %q", osascriptCalls)
	}
}

func TestGatewayHumanAttentionNonLoopbackFallsBackToDaemonLog(t *testing.T) {
	// Non-loopback baseUrl + authMode=none must not open a remote dashboard that
	// the CLI open policy rejects (non-loopback requires HTTPS + local-token).
	t.Parallel()

	ctx := context.Background()
	rootDir := t.TempDir()
	capturePath := filepath.Join(rootDir, "osascript.log")
	scriptPath := filepath.Join(rootDir, "osascript")
	writeExecutableScript(t, scriptPath, "#!/bin/sh\nprintf '%s\n' \"$*\" >> \""+capturePath+"\"\n")
	logPath := filepath.Join(rootDir, "logs", "looperd.log")

	coordinator := openNotifyCoordinator(t, rootDir)
	repos := storage.NewRepositories(coordinator.DB())
	gateway := NewGateway(Options{
		Config: config.NotificationConfig{
			InApp: true,
			Osascript: config.OsascriptNotificationConfig{
				Enabled:               true,
				ThrottleWindowSeconds: 60,
			},
		},
		OsascriptPath:     scriptPath,
		LogFilePath:       logPath,
		DashboardBaseURL:  "http://dash.example:8080",
		DashboardAuthMode: config.AuthModeNone,
		Repositories:      repos,
	})

	records := gateway.NotifyHumanAttention(ctx, HumanAttentionInput{
		LoopSeq:  42,
		Reason:   HumanAttentionAwaitingHuman,
		EntryKey: "run:run_non_loopback",
	})
	if got := notificationStatus(records, "osascript"); got != "success" {
		t.Fatalf("osascript status = %q, want success; records=%#v", got, records)
	}

	osascriptCallsBytes, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", capturePath, err)
	}
	osascriptCalls := string(osascriptCallsBytes)
	assertContains(t, osascriptCalls, "Open Log")
	assertContains(t, osascriptCalls, logPath)
	if strings.Contains(osascriptCalls, "Open Loop") {
		t.Fatalf("non-loopback authMode=none must not offer Open Loop: %q", osascriptCalls)
	}
	if strings.Contains(osascriptCalls, "dash.example") || strings.Contains(osascriptCalls, "/dashboard/loops/") {
		t.Fatalf("non-loopback must not open remote dashboard deep link: %q", osascriptCalls)
	}
}
