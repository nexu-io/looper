package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/eventlog"
	networkclient "github.com/nexu-io/looper/internal/network/client"
	"github.com/nexu-io/looper/internal/storage"
)

// Recovery human-attention rescan must not launch until startup is committed.
// If a later CompleteStartup step fails (e.g. network manager), Start closes
// SQLite — a still-running rescan would query/persist through a closed DB.
func TestHumanAttentionContract_RecoveryNotifyOnlyAfterStartupCommitted(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	backupDir := filepath.Join(workingDir, "backups")
	cfg.Storage.BackupDir = &backupDir
	cfg.Daemon.LogDir = filepath.Join(workingDir, "logs")
	cfg.Notifications.InApp = true
	cfg.Notifications.Osascript.Enabled = false
	// Coding agent configured so network manager is exercised after recovery.
	vendor := config.AgentVendorOpenCode
	cfg.Agent.Vendor = &vendor

	coordinator, err := storage.OpenSQLiteCoordinator(context.Background(), cfg.Storage.DBPath, storage.SQLiteCoordinatorOptions{
		BackupDir:  backupDir,
		Migrations: storage.EmbeddedMigrations,
	})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	defer func() { _ = coordinator.Close() }()
	if _, err := coordinator.MigrationRunner().RunPending(context.Background()); err != nil {
		t.Fatalf("MigrationRunner().RunPending() error = %v", err)
	}
	repositories := storage.NewRepositories(coordinator.DB())
	startedAt := time.Date(2026, time.July, 29, 17, 0, 0, 0, time.UTC)
	// Malformed network state makes networkManager.Start fail after recovery.
	statePath := filepath.Join(workingDir, ".looper", "network.json")
	if err := os.MkdirAll(filepath.Dir(statePath), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(statePath, []byte("{"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	// Seed a durable park so a wrongly-early schedule would have work to do.
	nowISO := eventlog.FormatJavaScriptISOString(startedAt)
	projectID := "project_boot_order"
	if err := repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID: projectID, Name: "Boot Order", RepoPath: filepath.Join(workingDir, "repo"),
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	loopID := "loop_boot_order_await"
	target := projectID
	if err := repositories.Loops.Upsert(context.Background(), storage.LoopRecord{
		ID: loopID, Seq: 801, ProjectID: projectID, Type: "worker",
		TargetType: "project", TargetID: &target, Status: "awaiting_human",
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	rt := &Runtime{
		config:           cfg,
		logger:           &testLogger{},
		now:              func() time.Time { return startedAt },
		shutdownTimeout:  2 * time.Second,
		startedAt:        &startedAt,
		services:         Services{Coordinator: coordinator, Repositories: repositories},
		networkManager:   networkclient.NewManager(statePath, cfg, repositories, nil),
		admission:        NewAdmission(),
		startupReadyOnce: sync.Once{},
	}

	err = rt.CompleteStartup(context.Background())
	if err == nil {
		t.Fatal("CompleteStartup() error = nil, want network manager start failure")
	}
	if !strings.Contains(err.Error(), "decode network state") {
		t.Fatalf("CompleteStartup() error = %q, want malformed network state error", err)
	}

	rt.mu.RLock()
	cancel := rt.humanAttentionNotifyCancel
	done := rt.humanAttentionNotifyDone
	rt.mu.RUnlock()
	if cancel != nil || done != nil {
		t.Fatalf("human-attention recovery notify scheduled on failed startup (cancel=%v done=%v); must launch only after MarkReady", cancel != nil, done != nil)
	}
	// No notification rows: rescan must not have run against still-open SQLite
	// before the failure returned (Start would then close the coordinator).
	assertHumanAttentionInAppCount(t, repositories, loopID, 0)
}

// Contract: after MarkReady, if BeginShutdown runs before recovery notify is
// registered, scheduleHumanAttentionRecoveryNotify must not arm a live
// goroutine that can query/persist while admission is already stopping (and
// stopHumanAttentionRecoveryNotify may already have passed a nil cancel).
func TestHumanAttentionContract_RecoveryNotifyDoesNotStartAfterShutdown(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	backupDir := filepath.Join(workingDir, "backups")
	cfg.Storage.BackupDir = &backupDir
	cfg.Daemon.LogDir = filepath.Join(workingDir, "logs")
	cfg.Notifications.InApp = true
	cfg.Notifications.Osascript.Enabled = false

	rt := New(Options{
		Config: cfg,
		Logger: &testLogger{},
	})
	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { rt.Stop("test cleanup") })

	if err := rt.admission.MarkReady("test mark ready"); err != nil {
		t.Fatalf("MarkReady() error = %v", err)
	}
	// Provide a repos pointer so schedule is not a no-op for nil repos.
	rt.mu.Lock()
	repos := rt.services.Repositories
	rt.mu.Unlock()
	if repos == nil {
		t.Fatal("Start() left Repositories nil")
	}

	// Shutdown closes admission before CompleteStartup can register HA recovery.
	rt.BeginShutdown("test drain before human-attention recovery arm")
	if rt.AdmissionState() != AdmissionStopping {
		t.Fatalf("AdmissionState() = %q, want stopping", rt.AdmissionState())
	}

	rt.scheduleHumanAttentionRecoveryNotify(repos)

	rt.mu.Lock()
	cancel := rt.humanAttentionNotifyCancel
	done := rt.humanAttentionNotifyDone
	rt.mu.Unlock()
	if cancel != nil || done != nil {
		// Publish-then-recheck may still arm cancel then refuse the generation;
		// require no live generation (done closed / cleared) so Stop cannot hang.
		if done != nil {
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("human-attention recovery did not exit after post-shutdown schedule")
			}
		}
	}
	if err := rt.AllowClaim(); err == nil {
		t.Fatal("AllowClaim() = nil after shutdown, want refusal so recovery cannot rescan")
	}
}
