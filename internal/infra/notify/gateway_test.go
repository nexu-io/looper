package notify

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
)

func TestGatewayPersistsInAppNotificationsAndDedupesOsascriptDelivery(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	rootDir := t.TempDir()
	capturePath := filepath.Join(rootDir, "osascript.log")
	scriptPath := filepath.Join(rootDir, "osascript")
	writeExecutableScript(t, scriptPath, "#!/bin/sh\nprintf '%s\n' \"$*\" >> \""+capturePath+"\"\n")

	coordinator := openNotifyCoordinator(t, rootDir)
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 11, 12, 0, 0, 0, time.UTC)

	gateway := NewGateway(Options{
		Config: config.NotificationConfig{
			InApp: true,
			Osascript: config.OsascriptNotificationConfig{
				Enabled:               true,
				SoundForLevels:        []config.NotificationSoundLevel{config.NotificationSoundLevelFailure, config.NotificationSoundLevelActionRequired},
				ThrottleWindowSeconds: 60,
			},
		},
		OsascriptPath: scriptPath,
		LogFilePath:   filepath.Join(rootDir, "logs", "looperd.log"),
		Repositories:  repos,
		Now:           func() time.Time { return now },
	})

	first := gateway.Notify(ctx, SystemNotificationPayload{
		Level:      "failure",
		Title:      "Worker blocked",
		Subtitle:   "task_1",
		Body:       "Needs attention",
		Sound:      "Funk",
		EntityType: "task",
		EntityID:   "task_1",
		DedupeKey:  "worker.blocked:task:task_1",
	})
	second := gateway.Notify(ctx, SystemNotificationPayload{
		Level:      "failure",
		Title:      "Worker blocked",
		Subtitle:   "task_1",
		Body:       "Needs attention",
		Sound:      "Funk",
		EntityType: "task",
		EntityID:   "task_1",
		DedupeKey:  "worker.blocked:task:task_1",
	})

	if got := notificationStatus(first, "osascript"); got != "success" {
		t.Fatalf("first osascript status = %q, want success", got)
	}
	if got := notificationStatus(second, "osascript"); got != "skipped" {
		t.Fatalf("second osascript status = %q, want skipped", got)
	}

	notifications, err := repos.Notifications.List(ctx, 10)
	if err != nil {
		t.Fatalf("Notifications.List() error = %v", err)
	}
	if len(notifications) != 6 {
		t.Fatalf("Notifications.List() len = %d, want 6", len(notifications))
	}

	events, err := repos.Events.ListByEntity(ctx, "task", "task_1")
	if err != nil {
		t.Fatalf("Events.ListByEntity() error = %v", err)
	}
	if len(events) != 6 {
		t.Fatalf("Events.ListByEntity() len = %d, want 6", len(events))
	}

	osascriptCallsBytes, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", capturePath, err)
	}
	osascriptCalls := string(osascriptCallsBytes)
	assertContains(t, osascriptCalls, "display dialog")
	assertContains(t, osascriptCalls, "Open Log")
	assertContains(t, osascriptCalls, "open ")
	assertContains(t, osascriptCalls, filepath.Join(rootDir, "logs", "looperd.log"))
}

func TestGatewayUsesLightweightOsascriptNotificationForNonFailureLevels(t *testing.T) {
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
				SoundForLevels:        []config.NotificationSoundLevel{config.NotificationSoundLevelFailure, config.NotificationSoundLevelActionRequired},
				ThrottleWindowSeconds: 60,
			},
		},
		OsascriptPath: scriptPath,
		Repositories:  repos,
	})

	gateway.Notify(ctx, SystemNotificationPayload{
		Level:    "success",
		Title:    "Loop completed",
		Subtitle: "loop_1",
		Body:     "All good",
		Sound:    "Funk",
	})

	osascriptCallsBytes, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", capturePath, err)
	}
	osascriptCalls := string(osascriptCallsBytes)
	assertContains(t, osascriptCalls, "display notification")
	if strings.Contains(osascriptCalls, "display dialog") {
		t.Fatalf("osascript calls = %q, want no display dialog", osascriptCalls)
	}
}

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

func TestDashboardDeepLinkUsable_OriginAndAuthPolicy(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		baseURL  string
		authMode config.AuthMode
		want     bool
	}{
		{name: "loopback none", baseURL: "http://127.0.0.1:17310", authMode: config.AuthModeNone, want: true},
		{name: "localhost none", baseURL: "http://localhost:17310", authMode: config.AuthModeNone, want: true},
		{name: "loopback local-token", baseURL: "http://127.0.0.1:17310", authMode: config.AuthModeLocalToken, want: false},
		{name: "non-loopback http none", baseURL: "http://dash.example:8080", authMode: config.AuthModeNone, want: false},
		{name: "non-loopback https none", baseURL: "https://dash.example", authMode: config.AuthModeNone, want: false},
		{name: "non-loopback https local-token", baseURL: "https://dash.example", authMode: config.AuthModeLocalToken, want: false},
		{name: "empty base", baseURL: "", authMode: config.AuthModeNone, want: false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := NewGateway(Options{
				DashboardBaseURL:  tc.baseURL,
				DashboardAuthMode: tc.authMode,
			})
			if got := g.dashboardDeepLinkUsable(); got != tc.want {
				t.Fatalf("dashboardDeepLinkUsable() = %v, want %v (base=%q auth=%q)", got, tc.want, tc.baseURL, tc.authMode)
			}
		})
	}
}

func TestGatewayHumanAttentionSkipsFeishuAppDelivery(t *testing.T) {
	// awaiting_human is local-only so Feishu HITL ask cards are not duplicated.
	// manual_intervention must still reach Feishu app (no HITL ask duplicate).
	t.Setenv("LOOPER_TEST_FEISHU_APP_ID", "cli_app_id")
	t.Setenv("LOOPER_TEST_FEISHU_APP_SECRET", "app_secret_value")

	ctx := context.Background()
	var calls []capturedFeishuCall
	gateway := newFeishuAppGateway(t, appModeConfig(), &calls)
	// Enable in_app so permanent entry dedupe still has a durable record.
	gateway.config.InApp = true

	records := gateway.NotifyHumanAttention(ctx, HumanAttentionInput{
		LoopSeq:  7,
		Reason:   HumanAttentionAwaitingHuman,
		EntryKey: "run:hitl_feishu_dedupe",
	})
	if got := notificationStatus(records, "in_app"); got != "success" {
		t.Fatalf("in_app status = %q, want success; records=%#v", got, records)
	}
	if got := notificationStatus(records, "feishu_app"); got != "" {
		t.Fatalf("feishu_app status = %q, want absent (local-only awaiting_human)", got)
	}
	if len(calls) != 0 {
		t.Fatalf("feishu HTTP calls = %d, want 0 for awaiting_human NotifyHumanAttention", len(calls))
	}

	// Hard manual_intervention parks have no HITL ask duplicate — remote delivery stays on.
	manual := gateway.NotifyHumanAttention(ctx, HumanAttentionInput{
		LoopSeq:  8,
		Reason:   HumanAttentionManualIntervention,
		EntryKey: "queue:q_manual:t1",
	})
	if got := notificationStatus(manual, "in_app"); got != "success" {
		t.Fatalf("manual in_app status = %q, want success; records=%#v", got, manual)
	}
	if got := notificationStatus(manual, "feishu_app"); got != "success" {
		t.Fatalf("manual feishu_app status = %q, want success (remote alert for hard hold)", got)
	}
	if len(calls) == 0 {
		t.Fatal("manual_intervention NotifyHumanAttention should reach Feishu app HTTP")
	}

	// Ordinary Notify still delivers to Feishu app mode.
	ordinary := gateway.Notify(ctx, SystemNotificationPayload{
		Level:     "action_required",
		Title:     "Ordinary",
		Body:      "not human-attention",
		DedupeKey: "ordinary:1",
	})
	if got := notificationStatus(ordinary, "feishu_app"); got != "success" {
		t.Fatalf("ordinary feishu_app status = %q, want success", got)
	}
}

func TestResolveDashboardBaseURL_RejectsNonOriginBaseURL(t *testing.T) {
	t.Parallel()

	fallback := "http://127.0.0.1:17310"
	cases := []struct {
		name    string
		baseURL string
		host    string
		port    int
		want    string
	}{
		{name: "clean http origin", baseURL: "http://dash.example:8080", want: "http://dash.example:8080"},
		{name: "clean https origin trailing slash", baseURL: "https://dash.example/", want: "https://dash.example"},
		{name: "userinfo rejected", baseURL: "https://user:token@dash.example", host: "127.0.0.1", port: 17310, want: fallback},
		{name: "query rejected", baseURL: "https://dash.example/?x=y", host: "127.0.0.1", port: 17310, want: fallback},
		{name: "fragment rejected", baseURL: "https://dash.example/#frag", host: "127.0.0.1", port: 17310, want: fallback},
		{name: "path rejected", baseURL: "https://dash.example/prefix", host: "127.0.0.1", port: 17310, want: fallback},
		{name: "non-http scheme rejected", baseURL: "ftp://dash.example", host: "127.0.0.1", port: 17310, want: fallback},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			base := tc.baseURL
			cfg := config.ServerConfig{Host: tc.host, Port: tc.port, BaseURL: &base}
			if got := ResolveDashboardBaseURL(cfg); got != tc.want {
				t.Fatalf("ResolveDashboardBaseURL(%q) = %q, want %q", tc.baseURL, got, tc.want)
			}
		})
	}
}

func openNotifyCoordinator(t *testing.T, rootDir string) *storage.SQLiteCoordinator {
	t.Helper()

	coordinator, err := storage.OpenSQLiteCoordinator(context.Background(), filepath.Join(rootDir, "state", "looper.sqlite"), storage.SQLiteCoordinatorOptions{
		Migrations: storage.EmbeddedMigrations,
		BackupDir:  filepath.Join(rootDir, "backups"),
	})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	t.Cleanup(func() {
		if closeErr := coordinator.Close(); closeErr != nil {
			t.Fatalf("coordinator.Close() error = %v", closeErr)
		}
	})

	if _, err := coordinator.MigrationRunner().RunPending(context.Background()); err != nil {
		t.Fatalf("MigrationRunner.RunPending() error = %v", err)
	}

	return coordinator
}

func writeExecutableScript(t *testing.T, path string, contents string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(contents), 0o755); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", path, err)
	}
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatalf("Chmod(%q) error = %v", path, err)
	}
}

func notificationStatus(records []storage.NotificationRecord, channel string) string {
	for _, record := range records {
		if record.Channel == channel {
			return record.Status
		}
	}

	return ""
}

func assertContains(t *testing.T, got string, want string) {
	t.Helper()
	if !strings.Contains(got, want) {
		t.Fatalf("string %q does not contain %q", got, want)
	}
}
