package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/eventlog"
	"github.com/nexu-io/looper/internal/storage"
)

// Post-claim human-attention delivery must not use the detached WithoutCancel
// dispatch context: BeginShutdown cancels a runtime-owned parent, and Stop
// drains so a 35s osascript cannot outlive the shutdown timeout and persist
// through a closed coordinator.
func TestHumanAttentionContract_PostClaimNotifyCanceledOnShutdown(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	root := t.TempDir()
	startedPath := filepath.Join(root, "osascript.started")
	scriptPath := filepath.Join(root, "osascript")
	// Block until SIGTERM/SIGINT (shell.Run kill on ctx cancel). Without cancel,
	// a hung notification helper would outlive shutdownTimeout.
	script := "#!/bin/sh\n: > \"" + startedPath + "\"\ntrap 'exit 0' TERM INT\nwhile true; do sleep 0.05; done\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile(osascript) error = %v", err)
	}

	coordinator := openMigratedCoordinator(t, filepath.Join(root, "post-claim-cancel.sqlite"), filepath.Join(root, "backups"))
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.July, 29, 18, 0, 0, 0, time.UTC)
	nowISO := eventlog.FormatJavaScriptISOString(now)
	projectID := "project_post_claim_cancel"
	if err := repos.Projects.Upsert(ctx, storage.ProjectRecord{
		ID: projectID, Name: "Post Claim Cancel", RepoPath: filepath.Join(root, "repo"),
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	loopID := "loop_post_claim_cancel"
	target := projectID
	if err := repos.Loops.Upsert(ctx, storage.LoopRecord{
		ID: loopID, Seq: 901, ProjectID: projectID, Type: "worker",
		TargetType: "project", TargetID: &target, Status: "awaiting_human",
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	cfg, err := config.DefaultConfig(root)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Notifications.InApp = true
	cfg.Notifications.Osascript.Enabled = true
	cfg.Notifications.Osascript.ThrottleWindowSeconds = 60
	osascriptPath := scriptPath
	cfg.Tools.OsascriptPath = &osascriptPath
	cfg.Daemon.LogDir = filepath.Join(root, "logs")

	admission := NewAdmission()
	if err := admission.MarkReady("test post-claim cancel"); err != nil {
		t.Fatalf("MarkReady() error = %v", err)
	}
	rt := &Runtime{
		config:           cfg,
		logger:           &testLogger{},
		now:              func() time.Time { return now },
		shutdownTimeout:  1500 * time.Millisecond,
		services:         Services{Coordinator: coordinator, Repositories: repos},
		admission:        admission,
		activeExecutions: NewActiveExecutionRegistry(),
		shutdownCh:       make(chan struct{}),
	}

	// Simulate post-finalize callback under detached dispatch ctx (WithoutCancel).
	// Runtime must ignore that and schedule under its cancelable parent.
	dispatchCtx := context.WithoutCancel(context.Background())
	rt.notifyHumanAttentionPostClaim(dispatchCtx, loopID)

	// Wait until the fake osascript has started so we know cancel has work to do.
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(startedPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("osascript did not start; post-claim notify may not have launched")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// BeginShutdown cancels HA delivery; stop drains the WaitGroup.
	start := time.Now()
	rt.BeginShutdown("test cancel post-claim human-attention")
	rt.stopHumanAttentionRecoveryNotify()
	elapsed := time.Since(start)
	if elapsed > 3*time.Second {
		t.Fatalf("stopHumanAttentionRecoveryNotify() elapsed = %v, want cancel-bounded", elapsed)
	}

	// After cancel+drain, SQLite close must not race a still-running persist.
	if err := coordinator.Close(); err != nil {
		t.Fatalf("coordinator.Close() after drained HA notify error = %v", err)
	}
}

// Post-recovery rescan is cancel/done-tracked and joined on Stop so SQLite
// close cannot race the background query (CI TempDir state/ cleanup failures).
func TestHumanAttentionContract_RecoveryNotifyJoinedBeforeStop(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	root := t.TempDir()
	dbPath := filepath.Join(root, "state", "join-stop.sqlite")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(state) error = %v", err)
	}
	cfg, err := config.DefaultConfig(root)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	backupDir := filepath.Join(root, "backups")
	cfg.Storage.DBPath = dbPath
	cfg.Storage.BackupDir = &backupDir
	cfg.Daemon.LogDir = filepath.Join(root, "logs")
	cfg.Daemon.WorkingDirectory = root
	cfg.Notifications.InApp = true
	cfg.Notifications.Osascript.Enabled = false
	// No coding agent → scheduler may wait, but recovery rescan still schedules.
	now := time.Date(2026, time.July, 29, 16, 0, 0, 0, time.UTC)
	rt := New(Options{
		Config: cfg,
		Logger: &testLogger{},
		Now:    func() time.Time { return now },
		RunSchedulerTick: func(context.Context, Services) error {
			return nil
		},
		ShutdownTimeout: 2 * time.Second,
	})
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if err := rt.CompleteStartup(ctx); err != nil {
		t.Fatalf("CompleteStartup() error = %v", err)
	}
	// Wait path used by API fixtures after CompleteStartup.
	if err := rt.WaitForHumanAttentionRecoveryNotify(ctx); err != nil {
		t.Fatalf("WaitForHumanAttentionRecoveryNotify() error = %v", err)
	}
	// Stop must join (or cancel+join) before closing SQLite; no hang / panic.
	rt.Stop("test join human-attention recovery notify")
	// Second wait is a no-op once the done channel was cleared.
	if err := rt.WaitForHumanAttentionRecoveryNotify(ctx); err != nil {
		t.Fatalf("WaitForHumanAttentionRecoveryNotify() after Stop = %v", err)
	}
}

// When an active recovery/post-claim osascript ignores cancel long enough that
// stopHumanAttentionRecoveryNotify exceeds shutdownTimeout, Stop must treat
// that as a drain failure and retain SQLite ownership (#577) rather than
// closing the coordinator under a still-live notify process.
func TestHumanAttentionContract_NotifyDrainTimeoutRetainsStorage(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	root := t.TempDir()
	startedPath := filepath.Join(root, "osascript.started")
	scriptPath := filepath.Join(root, "osascript")
	// Ignore TERM/INT so shell.Run spends its full grace (~5s) before SIGKILL;
	// shutdownTimeout is much shorter so stop reports drain failure first.
	script := "#!/bin/sh\n: > \"" + startedPath + "\"\ntrap '' TERM INT\nwhile true; do sleep 0.05; done\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile(osascript) error = %v", err)
	}

	coordinator := openMigratedCoordinator(t, filepath.Join(root, "drain-timeout.sqlite"), filepath.Join(root, "backups"))
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.July, 29, 19, 0, 0, 0, time.UTC)
	nowISO := eventlog.FormatJavaScriptISOString(now)
	projectID := "project_ha_drain_timeout"
	if err := repos.Projects.Upsert(ctx, storage.ProjectRecord{
		ID: projectID, Name: "HA Drain Timeout", RepoPath: filepath.Join(root, "repo"),
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	loopID := "loop_ha_drain_timeout"
	target := projectID
	if err := repos.Loops.Upsert(ctx, storage.LoopRecord{
		ID: loopID, Seq: 902, ProjectID: projectID, Type: "worker",
		TargetType: "project", TargetID: &target, Status: "awaiting_human",
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	cfg, err := config.DefaultConfig(root)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Notifications.InApp = true
	cfg.Notifications.Osascript.Enabled = true
	cfg.Notifications.Osascript.ThrottleWindowSeconds = 60
	osascriptPath := scriptPath
	cfg.Tools.OsascriptPath = &osascriptPath
	cfg.Daemon.LogDir = filepath.Join(root, "logs")
	cfg.Storage.DBPath = filepath.Join(root, "drain-timeout.sqlite")
	backupDir := filepath.Join(root, "backups")
	cfg.Storage.BackupDir = &backupDir

	admission := NewAdmission()
	if err := admission.MarkReady("test ha drain timeout"); err != nil {
		t.Fatalf("MarkReady() error = %v", err)
	}
	// Short stop budget: shell.Run grace is ~5s, so drain must time out.
	rt := &Runtime{
		config:            cfg,
		logger:            &testLogger{},
		now:               func() time.Time { return now },
		shutdownTimeout:   200 * time.Millisecond,
		services:          Services{Coordinator: coordinator, Repositories: repos},
		admission:         admission,
		activeExecutions:  NewActiveExecutionRegistry(),
		shutdownCh:        make(chan struct{}),
		ownershipAcquired: true,
	}
	// Join residual HA work before coordinator cleanup (Stop retained storage
	// while shell.Run may still be in SIGKILL grace).
	t.Cleanup(func() {
		done := make(chan struct{})
		go func() {
			rt.humanAttentionNotifyWG.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			t.Log("humanAttentionNotifyWG still live after retain-storage Stop (shell kill grace)")
		}
	})

	rt.notifyHumanAttentionPostClaim(context.WithoutCancel(context.Background()), loopID)

	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(startedPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("blocking osascript did not start")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Full Stop: HA drain timeout must feed retain-storage.
	rt.Stop("test human-attention notify drain timeout")
	if err := rt.ShutdownDrainError(); err == nil {
		t.Fatal("ShutdownDrainError() = nil after HA notify drain timeout, want error")
	} else if !errors.Is(err, errShutdownDrainTimeout) {
		t.Fatalf("ShutdownDrainError() = %v, want errShutdownDrainTimeout", err)
	}
	if !rt.StorageRetained() {
		t.Fatal("StorageRetained() = false after HA notify drain timeout, want true")
	}
	if services := rt.Services(); services.Coordinator == nil {
		t.Fatal("Services().Coordinator = nil after drain timeout Stop, want retained storage")
	}
}
