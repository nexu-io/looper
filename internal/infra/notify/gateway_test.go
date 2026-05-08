package notify

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
)

type mockProvider struct {
	name        string
	available   bool
	sendFn      func(context.Context, SystemNotificationPayload) error
	called      []SystemNotificationPayload
	mu          sync.Mutex
}

func (p *mockProvider) Name() string { return p.name }

func (p *mockProvider) IsAvailable() bool { return p.available }

func (p *mockProvider) Send(ctx context.Context, payload SystemNotificationPayload) error {
	p.mu.Lock()
	p.called = append(p.called, payload)
	p.mu.Unlock()
	if p.sendFn != nil {
		return p.sendFn(ctx, payload)
	}
	return nil
}

type recordingProvider struct {
	name     string
	available bool
	Calls    []string
	mu       sync.Mutex
}

func (p *recordingProvider) Name() string { return p.name }

func (p *recordingProvider) IsAvailable() bool { return p.available }

func (p *recordingProvider) Send(ctx context.Context, payload SystemNotificationPayload) error {
	p.mu.Lock()
	p.Calls = append(p.Calls, fmt.Sprintf("%s|%s|%s|%s", payload.Level, payload.Title, payload.Body, payload.DedupeKey))
	p.mu.Unlock()
	return nil
}

func TestGatewayPersistsInAppNotificationsAndDedupesProviderDelivery(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	rootDir := t.TempDir()

	coordinator := openNotifyCoordinator(t, rootDir)
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 11, 12, 0, 0, 0, time.UTC)

	prov := &mockProvider{
		name:      "test-provider",
		available: true,
	}

	gateway := NewGateway(Options{
		Config: config.NotificationConfig{
			InApp: true,
			Osascript: config.OsascriptNotificationConfig{
				ThrottleWindowSeconds: 60,
			},
		},
		Providers:    []Provider{prov},
		Repositories: repos,
		Now:          func() time.Time { return now },
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

	if got := notificationStatus(first, "test-provider"); got != "success" {
		t.Fatalf("first provider status = %q, want success", got)
	}
	if got := notificationStatus(second, "test-provider"); got != "skipped" {
		t.Fatalf("second provider status = %q, want skipped", got)
	}

	notifications, err := repos.Notifications.List(ctx, 10)
	if err != nil {
		t.Fatalf("Notifications.List() error = %v", err)
	}
	if len(notifications) != 4 {
		t.Fatalf("Notifications.List() len = %d, want 4", len(notifications))
	}

	events, err := repos.Events.ListByEntity(ctx, "task", "task_1")
	if err != nil {
		t.Fatalf("Events.ListByEntity() error = %v", err)
	}
	if len(events) != 4 {
		t.Fatalf("Events.ListByEntity() len = %d, want 4", len(events))
	}
}

func TestGatewaySkipsUnavailableProviders(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	rootDir := t.TempDir()

	coordinator := openNotifyCoordinator(t, rootDir)
	repos := storage.NewRepositories(coordinator.DB())

	prov := &recordingProvider{
		name:      "unavailable",
		available: false,
	}

	gateway := NewGateway(Options{
		Config: config.NotificationConfig{
			InApp: false,
		},
		Providers:    []Provider{prov},
		Repositories: repos,
	})

	records := gateway.Notify(ctx, SystemNotificationPayload{
		Level: "info",
		Title: "Test",
		Body:  "Body",
	})

	if len(prov.Calls) != 0 {
		t.Fatalf("unavailable provider was called %d times, want 0", len(prov.Calls))
	}

	if got := notificationStatus(records, "unavailable"); got != "skipped" {
		t.Fatalf("status = %q, want skipped", got)
	}
}

func TestGatewayUsesMultipleProviders(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	rootDir := t.TempDir()

	coordinator := openNotifyCoordinator(t, rootDir)
	repos := storage.NewRepositories(coordinator.DB())

	provA := &recordingProvider{name: "provider-a", available: true}
	provB := &recordingProvider{name: "provider-b", available: true}

	gateway := NewGateway(Options{
		Config: config.NotificationConfig{
			InApp: true,
		},
		Providers:    []Provider{provA, provB},
		Repositories: repos,
	})

	records := gateway.Notify(ctx, SystemNotificationPayload{
		Level: "success",
		Title: "Multi test",
		Body:  "Multiple providers",
	})

	if len(provA.Calls) != 1 {
		t.Fatalf("provider-a calls = %d, want 1", len(provA.Calls))
	}
	if len(provB.Calls) != 1 {
		t.Fatalf("provider-b calls = %d, want 1", len(provB.Calls))
	}

	if got := notificationStatus(records, "provider-a"); got != "success" {
		t.Fatalf("provider-a status = %q, want success", got)
	}
	if got := notificationStatus(records, "provider-b"); got != "success" {
		t.Fatalf("provider-b status = %q, want success", got)
	}
}

func TestGatewayHandlesProviderFailure(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	rootDir := t.TempDir()

	coordinator := openNotifyCoordinator(t, rootDir)
	repos := storage.NewRepositories(coordinator.DB())

	prov := &mockProvider{
		name:      "failing",
		available: true,
		sendFn: func(ctx context.Context, payload SystemNotificationPayload) error {
			return fmt.Errorf("provider error")
		},
	}

	gateway := NewGateway(Options{
		Config: config.NotificationConfig{
			InApp: false,
		},
		Providers:    []Provider{prov},
		Repositories: repos,
	})

	records := gateway.Notify(ctx, SystemNotificationPayload{
		Level: "info",
		Title: "Failure test",
	})

	if got := notificationStatus(records, "failing"); got != "failed" {
		t.Fatalf("status = %q, want failed", got)
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
