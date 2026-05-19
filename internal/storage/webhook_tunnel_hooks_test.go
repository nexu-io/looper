package storage

import (
	"context"
	"path/filepath"
	"testing"
)

func TestWebhookTunnelHooksRepositoryCRUD(t *testing.T) {
	t.Parallel()
	coordinator, err := OpenSQLiteCoordinator(context.Background(), filepath.Join(t.TempDir(), "looper.sqlite"), SQLiteCoordinatorOptions{})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	if _, err := coordinator.MigrationRunner().RunPending(context.Background()); err != nil {
		t.Fatalf("RunPending() error = %v", err)
	}

	repo := NewRepositories(coordinator.DB()).WebhookTunnelHooks
	lastDisableAt := int64(15)
	record := WebhookTunnelHookRecord{Repo: "nexu-io/looper", HookID: 42, ManagedURL: "https://example.test/hook", SecretRef: "secret://hook", ConsecutiveDisables: 2, LastDisableAt: &lastDisableAt, Orphaned: false, CreatedAt: 10, UpdatedAt: 10}
	if err := repo.Upsert(context.Background(), record); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}

	got, ok, err := repo.Get(context.Background(), record.Repo)
	if err != nil || !ok {
		t.Fatalf("Get() = (%#v, %v, %v), want record, true, nil", got, ok, err)
	}
	if got.HookID != 42 || got.ManagedURL != record.ManagedURL || got.LastDisableAt == nil || *got.LastDisableAt != lastDisableAt {
		t.Fatalf("Get() = %#v, want inserted record", got)
	}

	if err := repo.UpdatePing(context.Background(), record.Repo, 22); err != nil {
		t.Fatalf("UpdatePing() error = %v", err)
	}
	if err := repo.MarkOrphaned(context.Background(), record.Repo, true, 23); err != nil {
		t.Fatalf("MarkOrphaned() error = %v", err)
	}

	got, ok, err = repo.Get(context.Background(), record.Repo)
	if err != nil || !ok {
		t.Fatalf("Get() after updates = (%#v, %v, %v), want record, true, nil", got, ok, err)
	}
	if got.LastPingAt == nil || *got.LastPingAt != 22 || !got.Orphaned || got.UpdatedAt != 23 {
		t.Fatalf("Get() after updates = %#v, want ping/orphaned updates", got)
	}

	list, err := repo.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(list) != 1 || list[0].Repo != record.Repo {
		t.Fatalf("List() = %#v, want singleton record", list)
	}

	if err := repo.Delete(context.Background(), record.Repo); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	_, ok, err = repo.Get(context.Background(), record.Repo)
	if err != nil {
		t.Fatalf("Get() after delete error = %v", err)
	}
	if ok {
		t.Fatal("Get() after delete ok = true, want false")
	}
	if err := repo.Delete(context.Background(), "missing/repo"); err != nil {
		t.Fatalf("Delete(missing) error = %v", err)
	}
}
