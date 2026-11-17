package eventlog

import (
	"context"
	"github.com/nexu-io/looper/internal/storage"
	"path/filepath"
	"testing"
)

func TestTrackingReplayDoesNotReplaceFacts(t *testing.T) {
	ctx := context.Background()
	db, err := storage.OpenSQLiteCoordinator(ctx, filepath.Join(t.TempDir(), "state.sqlite"), storage.SQLiteCoordinatorOptions{Migrations: storage.EmbeddedMigrations})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.MigrationRunner().RunPending(ctx, storage.RunPendingOptions{}); err != nil {
		t.Fatal(err)
	}
	repos := storage.NewRepositories(db.DB())
	for _, value := range []string{"first", "replayed"} {
		if err := Track(ctx, repos, AppendInput{ID: "stable", EventType: "test", Payload: map[string]string{"status": value}}); err != nil {
			t.Fatal(err)
		}
	}
	events, err := repos.Events.List(ctx, 10)
	if err != nil || len(events) != 1 || events[0].PayloadJSON != `{"status":"first"}` {
		t.Fatalf("replay replaced facts: %+v %v", events, err)
	}
}
