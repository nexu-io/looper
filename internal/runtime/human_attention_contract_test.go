package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/eventlog"
	"github.com/nexu-io/looper/internal/infra/notify"
	"github.com/nexu-io/looper/internal/storage"
)

// Cross-component contract: durable human-attention transition → one
// action_required notification, permanent entry dedupe, and notification
// failure isolation from loop/queue authority.
func TestHumanAttentionContract_DurableTransitionDedupeAndFailureIsolation(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	root := t.TempDir()
	capturePath := filepath.Join(root, "osascript.log")
	scriptPath := filepath.Join(root, "osascript")
	writeHumanAttentionOsascript(t, scriptPath, capturePath, false)

	coordinator := openMigratedCoordinator(t, filepath.Join(root, "human-attention.sqlite"), filepath.Join(root, "backups"))
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.July, 29, 12, 0, 0, 0, time.UTC)
	nowISO := eventlog.FormatJavaScriptISOString(now)

	projectID := "project_human_attention"
	if err := repos.Projects.Upsert(ctx, storage.ProjectRecord{
		ID: projectID, Name: "Human Attention", RepoPath: filepath.Join(root, "repo"),
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	loopID := "loop_human_attention"
	targetID := projectID
	if err := repos.Loops.Upsert(ctx, storage.LoopRecord{
		ID: loopID, Seq: 616, ProjectID: projectID, Type: "worker",
		TargetType: "project", TargetID: &targetID, Repo: stringPtr("acme/looper"),
		Status: "running", CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert(running) error = %v", err)
	}
	runID := "run_human_attention_1"
	if err := repos.Runs.Upsert(ctx, storage.RunRecord{
		ID: runID, LoopID: loopID, Status: "running",
		StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}

	gateway := notify.NewGateway(notify.Options{
		Config: config.NotificationConfig{
			InApp: true,
			Osascript: config.OsascriptNotificationConfig{
				Enabled:               true,
				SoundForLevels:        []config.NotificationSoundLevel{config.NotificationSoundLevelActionRequired},
				ThrottleWindowSeconds: 1,
			},
		},
		OsascriptPath:    scriptPath,
		LogFilePath:      filepath.Join(root, "logs", "looperd.log"),
		DashboardBaseURL: "http://127.0.0.1:17310",
		Repositories:     repos,
		Now:              func() time.Time { return now },
	})

	// --- Durable transition into awaiting_human emits one action_required ---
	if err := repos.Loops.Upsert(ctx, storage.LoopRecord{
		ID: loopID, Seq: 616, ProjectID: projectID, Type: "worker",
		TargetType: "project", TargetID: &targetID, Repo: stringPtr("acme/looper"),
		Status: "awaiting_human", LastRunAt: &nowISO, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert(awaiting_human) error = %v", err)
	}
	if err := repos.Runs.Upsert(ctx, storage.RunRecord{
		ID: runID, LoopID: loopID, Status: "interrupted", Summary: stringPtr("Awaiting human decision"),
		StartedAt: nowISO, EndedAt: &nowISO, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Runs.Upsert(interrupted) error = %v", err)
	}

	notifyDurableHumanAttention(ctx, gateway, repos, loopID)
	assertHumanAttentionInAppCount(t, repos, loopID, 1)
	assertOsascriptContains(t, capturePath, "Open Loop")
	assertOsascriptContains(t, capturePath, "http://127.0.0.1:17310/dashboard/loops/616")
	assertOsascriptLacksSensitive(t, capturePath)

	// Unchanged parked state / re-observe (daemon restart simulation) must not resend.
	notifyDurableHumanAttention(ctx, gateway, repos, loopID)
	assertHumanAttentionInAppCount(t, repos, loopID, 1)

	// Leave human-attention, then re-enter with a new run → new notification.
	leaveAt := eventlog.FormatJavaScriptISOString(now.Add(time.Minute))
	if err := repos.Loops.Upsert(ctx, storage.LoopRecord{
		ID: loopID, Seq: 616, ProjectID: projectID, Type: "worker",
		TargetType: "project", TargetID: &targetID, Repo: stringPtr("acme/looper"),
		Status: "running", CreatedAt: nowISO, UpdatedAt: leaveAt,
	}); err != nil {
		t.Fatalf("Loops.Upsert(running re-enter) error = %v", err)
	}
	runID2 := "run_human_attention_2"
	reenterAt := eventlog.FormatJavaScriptISOString(now.Add(2 * time.Minute))
	if err := repos.Runs.Upsert(ctx, storage.RunRecord{
		ID: runID2, LoopID: loopID, Status: "interrupted",
		StartedAt: reenterAt, EndedAt: &reenterAt, CreatedAt: reenterAt, UpdatedAt: reenterAt,
	}); err != nil {
		t.Fatalf("Runs.Upsert(run2) error = %v", err)
	}
	if err := repos.Loops.Upsert(ctx, storage.LoopRecord{
		ID: loopID, Seq: 616, ProjectID: projectID, Type: "worker",
		TargetType: "project", TargetID: &targetID, Repo: stringPtr("acme/looper"),
		Status: "awaiting_human", LastRunAt: &reenterAt, CreatedAt: nowISO, UpdatedAt: reenterAt,
	}); err != nil {
		t.Fatalf("Loops.Upsert(awaiting_human re-enter) error = %v", err)
	}
	// Advance clock past osascript throttle so a genuine new entry is not throttle-skipped.
	gateway = notify.NewGateway(notify.Options{
		Config: config.NotificationConfig{
			InApp: true,
			Osascript: config.OsascriptNotificationConfig{
				Enabled:               true,
				SoundForLevels:        []config.NotificationSoundLevel{config.NotificationSoundLevelActionRequired},
				ThrottleWindowSeconds: 1,
			},
		},
		OsascriptPath:    scriptPath,
		LogFilePath:      filepath.Join(root, "logs", "looperd.log"),
		DashboardBaseURL: "http://127.0.0.1:17310",
		Repositories:     repos,
		Now:              func() time.Time { return now.Add(3 * time.Minute) },
	})
	notifyDurableHumanAttention(ctx, gateway, repos, loopID)
	assertHumanAttentionInAppCount(t, repos, loopID, 2)

	// --- Durable manual_intervention condition emits action_required ---
	manualLoopID := "loop_manual_hold"
	manualTarget := "pr:acme/looper:42"
	prNumber := int64(42)
	if err := repos.Loops.Upsert(ctx, storage.LoopRecord{
		ID: manualLoopID, Seq: 617, ProjectID: projectID, Type: "fixer",
		TargetType: "pull_request", TargetID: &manualTarget, Repo: stringPtr("acme/looper"),
		PRNumber: &prNumber, Status: "paused", CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert(manual loop) error = %v", err)
	}
	checkpoint := `{"resumePolicy":"manual_intervention"}`
	if err := repos.Runs.Upsert(ctx, storage.RunRecord{
		ID: "run_manual_hold", LoopID: manualLoopID, Status: "failed",
		CheckpointJSON: &checkpoint, Summary: stringPtr("dirty worktree"),
		StartedAt: nowISO, EndedAt: &nowISO, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Runs.Upsert(manual run) error = %v", err)
	}
	manualKind := "manual_intervention"
	finished := nowISO
	if err := repos.Queue.Upsert(ctx, storage.QueueItemRecord{
		ID: "queue_manual_hold", ProjectID: &projectID, LoopID: &manualLoopID,
		Type: "fixer", TargetType: "pull_request", TargetID: manualTarget,
		Repo: stringPtr("acme/looper"), PRNumber: &prNumber,
		DedupeKey: "fixer:manual_hold", Priority: storage.QueuePriorityFixer,
		Status: "manual_intervention", AvailableAt: nowISO, Attempts: 1, MaxAttempts: 3,
		LastError: stringPtr("dirty worktree"), LastErrorKind: &manualKind,
		FinishedAt: &finished, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Queue.Upsert(manual) error = %v", err)
	}
	notifyDurableHumanAttention(ctx, gateway, repos, manualLoopID)
	assertHumanAttentionInAppCount(t, repos, manualLoopID, 1)
	// Dedupe: unchanged park does not resend.
	notifyDurableHumanAttention(ctx, gateway, repos, manualLoopID)
	assertHumanAttentionInAppCount(t, repos, manualLoopID, 1)

	// Ordinary failure kind parked as queue status manual_intervention must NOT notify.
	ordinaryLoopID := "loop_ordinary_fail"
	ordinaryTarget := projectID
	if err := repos.Loops.Upsert(ctx, storage.LoopRecord{
		ID: ordinaryLoopID, Seq: 618, ProjectID: projectID, Type: "worker",
		TargetType: "project", TargetID: &ordinaryTarget, Status: "paused",
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert(ordinary) error = %v", err)
	}
	ordinaryKind := "retryable_transient"
	if err := repos.Queue.Upsert(ctx, storage.QueueItemRecord{
		ID: "queue_ordinary_fail", ProjectID: &projectID, LoopID: &ordinaryLoopID,
		Type: "worker", TargetType: "project", TargetID: ordinaryTarget,
		DedupeKey: "worker:ordinary", Priority: storage.QueuePriorityWorker,
		Status: "manual_intervention", AvailableAt: nowISO, Attempts: 3, MaxAttempts: 3,
		LastError: stringPtr("network blip"), LastErrorKind: &ordinaryKind,
		FinishedAt: &finished, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Queue.Upsert(ordinary) error = %v", err)
	}
	notifyDurableHumanAttention(ctx, gateway, repos, ordinaryLoopID)
	assertHumanAttentionInAppCount(t, repos, ordinaryLoopID, 0)

	// --- Notification failure is audited and never changes loop/queue ---
	failScript := filepath.Join(root, "osascript-fail")
	writeHumanAttentionOsascript(t, failScript, capturePath, true)
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
		OsascriptPath:    failScript,
		LogFilePath:      filepath.Join(root, "logs", "looperd.log"),
		DashboardBaseURL: "http://127.0.0.1:17310",
		Repositories:     repos,
		Now:              func() time.Time { return now.Add(10 * time.Minute) },
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
	gateway := notify.NewGateway(notify.Options{DashboardBaseURL: base})
	u, err := gateway.DashboardLoopDetailURL(42)
	if err != nil {
		t.Fatalf("DashboardLoopDetailURL() error = %v", err)
	}
	if u != "http://127.0.0.1:17310/dashboard/loops/42" {
		t.Fatalf("DashboardLoopDetailURL() = %q", u)
	}
	if strings.Contains(u, "token") || strings.Contains(u, "code=") || strings.Contains(u, "answer") {
		t.Fatalf("deep link must not contain secrets: %q", u)
	}
}

func writeHumanAttentionOsascript(t *testing.T, path, capturePath string, fail bool) {
	t.Helper()
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"" + capturePath + "\"\n"
	if fail {
		script += "exit 1\n"
	}
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", path, err)
	}
}

func assertHumanAttentionInAppCount(t *testing.T, repos *storage.Repositories, loopID string, want int) {
	t.Helper()
	notifications, err := repos.Notifications.List(context.Background(), 100)
	if err != nil {
		t.Fatalf("Notifications.List() error = %v", err)
	}
	got := 0
	for _, n := range notifications {
		if n.LoopID == nil || *n.LoopID != loopID {
			continue
		}
		if n.Channel != "in_app" || n.Level != "action_required" {
			continue
		}
		if n.DedupeKey == nil || !strings.HasPrefix(*n.DedupeKey, "human_attention:") {
			continue
		}
		got++
	}
	if got != want {
		t.Fatalf("human_attention in_app count for %s = %d, want %d", loopID, got, want)
	}
}

func assertOsascriptContains(t *testing.T, path, want string) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", path, err)
	}
	if !strings.Contains(string(body), want) {
		t.Fatalf("osascript log %q does not contain %q\nlog:\n%s", path, want, body)
	}
}

func assertOsascriptLacksSensitive(t *testing.T, path string) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", path, err)
	}
	lower := strings.ToLower(string(body))
	for _, banned := range []string{"token=", "authorization", "answer=", "password", "secret"} {
		if strings.Contains(lower, banned) {
			t.Fatalf("osascript log contains sensitive fragment %q:\n%s", banned, body)
		}
	}
}
