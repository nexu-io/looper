package runtime

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/eventlog"
	"github.com/nexu-io/looper/internal/infra/notify"
	"github.com/nexu-io/looper/internal/storage"
)

// Cross-component: notification delivery failure is audited and never changes
// durable loop/queue authority.
func TestHumanAttentionContract_NotifyFailureIsolation(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	root := t.TempDir()
	capturePath := filepath.Join(root, "osascript.log")
	failScript := filepath.Join(root, "osascript-fail")
	writeHumanAttentionOsascript(t, failScript, capturePath, true)

	coordinator := openMigratedCoordinator(t, filepath.Join(root, "fail.sqlite"), filepath.Join(root, "backups"))
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.July, 29, 12, 0, 0, 0, time.UTC)
	nowISO := eventlog.FormatJavaScriptISOString(now)

	projectID := "project_notify_fail"
	if err := repos.Projects.Upsert(ctx, storage.ProjectRecord{
		ID: projectID, Name: "Notify Fail", RepoPath: filepath.Join(root, "repo"),
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	failLoopID := "loop_notify_fail"
	failTarget := projectID
	if err := repos.Loops.Upsert(ctx, storage.LoopRecord{
		ID: failLoopID, Seq: 619, ProjectID: projectID, Type: "planner",
		TargetType: "project", TargetID: &failTarget, Status: "awaiting_human",
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert(fail loop) error = %v", err)
	}
	if err := repos.Runs.Upsert(ctx, storage.RunRecord{
		ID: "run_notify_fail", LoopID: failLoopID, Status: "interrupted",
		StartedAt: nowISO, EndedAt: &nowISO, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Runs.Upsert(fail run) error = %v", err)
	}
	failGateway := notify.NewGateway(notify.Options{
		Config: config.NotificationConfig{
			InApp: true,
			Osascript: config.OsascriptNotificationConfig{
				Enabled:               true,
				SoundForLevels:        []config.NotificationSoundLevel{config.NotificationSoundLevelActionRequired},
				ThrottleWindowSeconds: 60,
			},
		},
		OsascriptPath:     failScript,
		LogFilePath:       filepath.Join(root, "logs", "looperd.log"),
		DashboardBaseURL:  "http://127.0.0.1:17310",
		DashboardAuthMode: config.AuthModeNone,
		Repositories:      repos,
		Now:               func() time.Time { return now },
	})
	notifyDurableHumanAttention(ctx, failGateway, repos, failLoopID)

	loopAfter, err := repos.Loops.GetByID(ctx, failLoopID)
	if err != nil || loopAfter == nil {
		t.Fatalf("Loops.GetByID after notify failure = %#v, err=%v", loopAfter, err)
	}
	if loopAfter.Status != "awaiting_human" {
		t.Fatalf("loop status after notify failure = %q, want awaiting_human (unchanged)", loopAfter.Status)
	}
	// in_app still audits success; osascript records failed — loop authority untouched.
	notifications, err := repos.Notifications.List(ctx, 50)
	if err != nil {
		t.Fatalf("Notifications.List() error = %v", err)
	}
	var sawFailedOsascript bool
	for _, n := range notifications {
		if n.LoopID != nil && *n.LoopID == failLoopID && n.Channel == "osascript" && n.Status == "failed" {
			sawFailedOsascript = true
		}
	}
	if !sawFailedOsascript {
		t.Fatal("want audited osascript failure for notify-failure isolation case")
	}
}

func TestResolveDashboardBaseURL_NoTokensOrSensitivePath(t *testing.T) {
	t.Parallel()

	base := notify.ResolveDashboardBaseURL(config.ServerConfig{Host: "0.0.0.0", Port: 17310})
	if base != "http://127.0.0.1:17310" {
		t.Fatalf("ResolveDashboardBaseURL(wildcard) = %q", base)
	}
	// Userinfo / query must not leak into deep links — fall back to host/port origin.
	leaky := "https://user:secret@evil.example/?token=abc#frag"
	if got := notify.ResolveDashboardBaseURL(config.ServerConfig{
		Host: "0.0.0.0", Port: 17310, BaseURL: &leaky,
	}); got != "http://127.0.0.1:17310" {
		t.Fatalf("ResolveDashboardBaseURL(leaky) = %q, want host/port fallback", got)
	}
	gateway := notify.NewGateway(notify.Options{DashboardBaseURL: base})
	u, err := gateway.DashboardLoopDetailURL(42)
	if err != nil {
		t.Fatalf("DashboardLoopDetailURL() error = %v", err)
	}
	if u != "http://127.0.0.1:17310/dashboard/loops/42" {
		t.Fatalf("DashboardLoopDetailURL() = %q", u)
	}
	if strings.Contains(u, "token") || strings.Contains(u, "code=") || strings.Contains(u, "answer") || strings.Contains(u, "secret") {
		t.Fatalf("deep link must not contain secrets: %q", u)
	}
}
