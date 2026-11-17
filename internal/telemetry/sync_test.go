package telemetry

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
)

func fixture(t *testing.T) (*storage.SQLiteCoordinator, config.Config) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	db, err := storage.OpenSQLiteCoordinator(ctx, filepath.Join(dir, "test.sqlite"), storage.SQLiteCoordinatorOptions{Migrations: storage.EmbeddedMigrations})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.MigrationRunner().RunPending(ctx, storage.RunPendingOptions{}); err != nil {
		t.Fatal(err)
	}
	repos := storage.NewRepositories(db.DB())
	now := "2026-11-17T00:00:00.000Z"
	if err := repos.Projects.Upsert(ctx, storage.ProjectRecord{ID: "local", Name: "Local", RepoPath: dir, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	metadata := `{"worker":{"title":"Task","strictDispatchId":"4f9b048e-e8cb-438a-b2c4-78421e79100e","prompt":"PRIVATE_PROMPT"}}`
	if err := repos.Loops.Upsert(ctx, storage.LoopRecord{ID: "loop-1", Seq: 1, ProjectID: "local", Type: "worker", TargetType: "project", Status: "running", MetadataJSON: &metadata, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	loopID := "loop-1"
	if err := repos.Events.Append(ctx, storage.EventLogRecord{ID: "event-1", EventType: "agent.invoked", LoopID: &loopID, PayloadJSON: `{"argv":["PRIVATE_ARGV"],"prompt":"PRIVATE_PROMPT","repo":"https://user:secret@example.com/a/b","status":"running"}`, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, "node.pem")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	workspace, project := "workspace", "project-id"
	return db, config.Config{Providers: []config.ProviderConfig{{ID: "plane", Kind: config.ProviderKindPlane, Workspace: &workspace, ProjectID: &project, StrictDispatch: &config.PlaneStrictDispatchConfig{Enabled: true, NodeID: "node", BindingID: "4f9b048e-e8cb-438a-b2c4-78421e79100e", KeyRevision: 1, PrivateKeyFile: keyPath}}}, Projects: []config.ProjectRefConfig{{ID: "local", Provider: "plane"}}}
}

func TestPendingRevisionSurvivesChangesAndAcknowledgementLoss(t *testing.T) {
	db, _ := fixture(t)
	ctx := context.Background()
	first, revision, err := pending(ctx, db.DB(), "destination", "loop-1")
	if err != nil || revision != 1 {
		t.Fatalf("pending: revision=%d %v", revision, err)
	}
	for _, secret := range []string{"PRIVATE_PROMPT", "PRIVATE_ARGV", "secret@example", "stdoutLogPath"} {
		if strings.Contains(string(first), secret) {
			t.Fatalf("leaked %s", secret)
		}
	}
	if _, err := db.DB().Exec(`UPDATE loops SET status='completed' WHERE id='loop-1'`); err != nil {
		t.Fatal(err)
	}
	retry, revision, err := pending(ctx, db.DB(), "destination", "loop-1")
	if err != nil || revision != 1 || string(first) != string(retry) {
		t.Fatal("unacknowledged payload changed", err)
	}
	if _, err := db.DB().Exec(`UPDATE telemetry_checkpoints SET acknowledged=1`); err != nil {
		t.Fatal(err)
	}
	next, revision, err := pending(ctx, db.DB(), "destination", "loop-1")
	if err != nil || revision != 2 || !strings.Contains(string(next), `"status":"completed"`) {
		t.Fatalf("new revision: %d %s %v", revision, next, err)
	}
	if _, err := db.DB().Exec(`UPDATE telemetry_checkpoints SET acknowledged=1 WHERE revision=1`); err != nil {
		t.Fatal(err)
	}
	retry, _, err = pending(ctx, db.DB(), "destination", "loop-1")
	if err != nil || string(retry) != string(next) {
		t.Fatal("late ack discarded newer snapshot", err)
	}
	other, revision, err := pending(ctx, db.DB(), "new-destination", "loop-1")
	if err != nil || revision != 1 {
		t.Fatal("destination state reused", err)
	}
	var a, b map[string]any
	json.Unmarshal(first, &a)
	json.Unmarshal(other, &b)
	if a["databaseId"] != b["databaseId"] {
		t.Fatal("database identity changed")
	}
}

func TestSyncOptInSignedMetadataAndRetry(t *testing.T) {
	db, cfg := fixture(t)
	ctx := context.Background()
	calls := 0
	fail := true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/api/workspaces/workspace/projects/project-id/looper/telemetry/batches/" || r.Header.Get("Looper-Signature") == "" {
			t.Error("unsigned or wrong telemetry endpoint")
		}
		raw, _ := io.ReadAll(r.Body)
		if strings.Contains(string(raw), "PRIVATE_") {
			t.Error("private data uploaded")
		}
		if fail {
			http.Error(w, "offline", 503)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"taskId":"loop-1","revision":1,"storedRevision":1}`)
	}))
	defer server.Close()
	cfg.Providers[0].StrictDispatch.BaseURL = server.URL
	if _, err := Sync(ctx, db.DB(), cfg, false); err != nil || calls != 0 {
		t.Fatal("uploaded without opt-in", err)
	}
	if _, err := Sync(ctx, db.DB(), cfg, true); err == nil {
		t.Fatal("failed upload reported success")
	}
	fail = false
	result, err := Sync(ctx, db.DB(), cfg, true)
	if err != nil || result.Uploaded != 1 {
		t.Fatal("retry failed", result, err)
	}
	result, err = Sync(ctx, db.DB(), cfg, true)
	if err != nil || result.Uploaded != 0 || calls != 2 {
		t.Fatal("unchanged task uploaded again", result, calls, err)
	}
	var status string
	db.DB().QueryRow(`SELECT status FROM loops WHERE id='loop-1'`).Scan(&status)
	if status != "running" {
		t.Fatal("telemetry changed task execution state")
	}
}
