package storage

import (
	"context"
	"path/filepath"
	"testing"
)

func TestWebhookForwardersRepositoryCRUD(t *testing.T) {
	t.Parallel()

	coordinator, err := OpenSQLiteCoordinator(context.Background(), filepath.Join(t.TempDir(), "looper.sqlite"), SQLiteCoordinatorOptions{})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	if _, err := coordinator.MigrationRunner().RunPending(context.Background()); err != nil {
		t.Fatalf("RunPending() error = %v", err)
	}

	repo := NewRepositories(coordinator.DB()).WebhookForwarders
	record := WebhookForwarderRecord{Repo: "nexu-io/looper", PID: 123, ProcessStart: 456, Fingerprint: "abc", Endpoint: "http://127.0.0.1:17310/webhook/forward", Events: "issue_comment,push", GHPath: "/bin/gh", DaemonID: "daemon", SpawnedAt: 1, UpdatedAt: 1}
	if err := repo.Upsert(context.Background(), record); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	record.PID = 789
	record.UpdatedAt = 2
	if err := repo.Upsert(context.Background(), record); err != nil {
		t.Fatalf("second Upsert() error = %v", err)
	}
	records, err := repo.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(records) != 1 || records[0].PID != 789 || records[0].UpdatedAt != 2 {
		t.Fatalf("List() = %#v, want updated singleton record", records)
	}
	if err := repo.Delete(context.Background(), "nexu-io/looper"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if err := repo.Delete(context.Background(), "nexu-io/missing"); err != nil {
		t.Fatalf("Delete(missing) error = %v", err)
	}
	records, err = repo.List(context.Background())
	if err != nil {
		t.Fatalf("List() after delete error = %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("records after delete = %#v, want empty", records)
	}
}
