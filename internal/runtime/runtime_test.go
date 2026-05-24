package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/infra/shell"
	networkclient "github.com/nexu-io/looper/internal/network/client"
	"github.com/nexu-io/looper/internal/projects"
	"github.com/nexu-io/looper/internal/storage"
	"github.com/nexu-io/looper/internal/webhookforward"
)

func TestRuntimeStartOpensSQLiteAndSyncsConfiguredProjects(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}

	backupDir := t.TempDir()
	dbPath := workingDir + "/runtime.sqlite"
	worktreeRoot := workingDir + "/worktrees"
	startedAt := time.Date(2026, time.April, 17, 12, 34, 56, 0, time.UTC)

	cfg.Storage.DBPath = dbPath
	cfg.Storage.BackupDir = &backupDir
	cfg.Projects = []config.ProjectRefConfig{{
		ID:           "project_1",
		Name:         "Looper",
		RepoPath:     workingDir + "/repo",
		BaseBranch:   nil,
		WorktreeRoot: &worktreeRoot,
	}}

	rt := New(Options{
		Config: cfg,
		Logger: &testLogger{},
		Now: func() time.Time {
			return startedAt
		},
	})

	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() {
		rt.Stop("test cleanup")
	})

	services := rt.Services()
	if services.Coordinator == nil {
		t.Fatal("Services().Coordinator = nil, want initialized coordinator")
	}
	if services.Repositories == nil || services.Repositories.Projects == nil {
		t.Fatal("Services().Repositories.Projects = nil, want initialized repository set")
	}
	if services.Projects == nil || services.Loops == nil || services.Runs == nil {
		t.Fatal("Services() orchestration services = nil, want initialized services")
	}

	project, err := services.Repositories.Projects.GetByID(context.Background(), "project_1")
	if err != nil {
		t.Fatalf("Projects.GetByID() error = %v", err)
	}
	if project == nil {
		t.Fatal("Projects.GetByID() = nil, want synced project")
	}
	if project.BaseBranch == nil || *project.BaseBranch != cfg.Defaults.BaseBranch {
		t.Fatalf("project.BaseBranch = %v, want %q", project.BaseBranch, cfg.Defaults.BaseBranch)
	}
	wantMetadata := `{"repo":null,"worktreeRoot":"` + worktreeRoot + `","source":"config"}`
	if project.MetadataJSON == nil || *project.MetadataJSON != wantMetadata {
		t.Fatalf("project.MetadataJSON = %v, want %q", project.MetadataJSON, wantMetadata)
	}
	if project.CreatedAt != "2026-04-17T12:34:56.000Z" {
		t.Fatalf("project.CreatedAt = %q, want startup timestamp", project.CreatedAt)
	}
	if project.UpdatedAt != "2026-04-17T12:34:56.000Z" {
		t.Fatalf("project.UpdatedAt = %q, want startup timestamp", project.UpdatedAt)
	}
	if got, ok := rt.StartedAt(); !ok || !got.Equal(startedAt) {
		t.Fatalf("StartedAt() = (%v, %t), want (%v, true)", got, ok, startedAt)
	}
}

func TestRuntimeStartIsIdempotent(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}

	cfg.Storage.DBPath = workingDir + "/runtime.sqlite"

	openCalls := 0
	rt := New(Options{
		Config: cfg,
		Logger: &testLogger{},
		OpenSQLiteCoordinator: func(ctx context.Context, dbPath string, options storage.SQLiteCoordinatorOptions) (*storage.SQLiteCoordinator, error) {
			openCalls++
			return storage.OpenSQLiteCoordinator(ctx, dbPath, options)
		},
	})

	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("first Start() error = %v", err)
	}
	defer rt.Stop("test")

	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("second Start() error = %v", err)
	}

	if openCalls != 1 {
		t.Fatalf("openSQLiteCoordinator call count = %d, want 1", openCalls)
	}
	if _, ok := rt.StartedAt(); !ok {
		t.Fatal("StartedAt() ok = false, want true")
	}
}

func TestRuntimeStartClosesCoordinatorWhenCompleteStartupFails(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}

	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	backupDir := filepath.Join(workingDir, "backups")
	cfg.Storage.BackupDir = &backupDir

	startCtx, cancel := context.WithCancel(context.Background())
	rt := New(Options{
		Config: cfg,
		Logger: &testLogger{},
		SyncConfiguredProjects: func(ctx context.Context, service *projects.Service, cfg config.Config, now time.Time) error {
			cancel()
			return nil
		},
	})

	err = rt.Start(startCtx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Start() error = %v, want %v", err, context.Canceled)
	}

	services := rt.Services()
	if services.Coordinator == nil {
		t.Fatal("Services().Coordinator = nil, want closed coordinator for failed startup")
	}
	if err := services.Coordinator.DB().PingContext(context.Background()); err == nil {
		t.Fatal("Services().Coordinator.DB().PingContext() error = nil, want closed database after startup failure")
	}
	if _, ok := rt.StartedAt(); !ok {
		t.Fatal("StartedAt() ok = false, want startup timestamp recorded before completion failure")
	}
	if recovery := rt.RecoverySummary(); recovery != createEmptyRecoverySummary() {
		t.Fatalf("RecoverySummary() = %#v, want empty recovery summary after failed completion", recovery)
	}
	if rt.ownershipAcquired {
		t.Fatal("ownershipAcquired = true, want false after failed CompleteStartup")
	}
	if rt.startupReadyErr == nil {
		t.Fatal("startupReadyErr = nil, want completion failure recorded")
	}

	coordinator, err := storage.OpenSQLiteCoordinator(context.Background(), cfg.Storage.DBPath, storage.SQLiteCoordinatorOptions{
		BackupDir:  backupDir,
		Migrations: storage.EmbeddedMigrations,
	})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() reopen error = %v", err)
	}
	defer func() { _ = coordinator.Close() }()
	reopenedRepos := storage.NewRepositories(coordinator.DB())
	events, err := reopenedRepos.Events.List(context.Background(), 10)
	if err != nil {
		t.Fatalf("Events.List() error = %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("Events.List() = %#v, want no startup event after failed completion", events)
	}

	rt.Stop("cleanup after failed startup")
}

func TestRuntimeCompleteStartupDoesNotStartSchedulerWhenNetworkManagerStartFails(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	backupDir := filepath.Join(workingDir, "backups")
	cfg.Storage.BackupDir = &backupDir
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
	startedAt := time.Now().UTC()
	statePath := filepath.Join(workingDir, ".looper", "network.json")
	if err := os.MkdirAll(filepath.Dir(statePath), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(statePath, []byte("{"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	rt := &Runtime{
		config:           cfg,
		logger:           &testLogger{},
		now:              func() time.Time { return startedAt },
		shutdownTimeout:  time.Second,
		startedAt:        &startedAt,
		services:         Services{Coordinator: coordinator, Repositories: repositories},
		networkManager:   networkclient.NewManager(statePath, cfg, repositories, nil),
		startupReadyOnce: sync.Once{},
	}

	err = rt.CompleteStartup(context.Background())
	if err == nil {
		t.Fatal("CompleteStartup() error = nil, want malformed network state error")
	}
	if !strings.Contains(err.Error(), "decode network state") {
		t.Fatalf("CompleteStartup() error = %q, want malformed network state error", err)
	}

	rt.mu.RLock()
	defer rt.mu.RUnlock()
	if rt.schedulerDone != nil {
		t.Fatal("schedulerDone != nil, want scheduler side effects skipped on network manager start failure")
	}
	if rt.schedulerStop != nil {
		t.Fatal("schedulerStop != nil, want scheduler side effects skipped on network manager start failure")
	}
}

func TestRuntimeStartRunsRecoveryBeforeImmediateSchedulerTick(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}

	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	backupDir := filepath.Join(workingDir, "backups")
	cfg.Storage.BackupDir = &backupDir
	startedAt := time.Date(2026, time.April, 17, 12, 34, 56, 0, time.UTC)
	nowISO := formatJavaScriptISOString(startedAt)

	seedCoordinator, err := storage.OpenSQLiteCoordinator(context.Background(), cfg.Storage.DBPath, storage.SQLiteCoordinatorOptions{
		BackupDir: backupDir,
	})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() seed error = %v", err)
	}
	if _, err := seedCoordinator.MigrationRunner().RunPending(context.Background()); err != nil {
		t.Fatalf("MigrationRunner.RunPending() seed error = %v", err)
	}
	seedRepos := storage.NewRepositories(seedCoordinator.DB())
	baseBranch := "main"
	projectID := "project_1"
	loopID := "loop_1"
	queueID := "queue_1"
	if err := seedRepos.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID:         projectID,
		Name:       "Looper",
		RepoPath:   filepath.Join(workingDir, "repo"),
		BaseBranch: &baseBranch,
		Archived:   false,
		CreatedAt:  nowISO,
		UpdatedAt:  nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() seed error = %v", err)
	}
	if err := seedRepos.Loops.Upsert(context.Background(), storage.LoopRecord{
		ID:         loopID,
		Seq:        1,
		ProjectID:  projectID,
		Type:       "reviewer",
		TargetType: "pull_request",
		TargetID:   stringPtr("pr:acme/looper:42"),
		Repo:       stringPtr("acme/looper"),
		PRNumber:   int64Ptr(42),
		Status:     "running",
		LastRunAt:  stringPtr(nowISO),
		CreatedAt:  nowISO,
		UpdatedAt:  nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert() seed error = %v", err)
	}
	if err := seedRepos.Runs.Upsert(context.Background(), storage.RunRecord{
		ID:                "run_1",
		LoopID:            loopID,
		Status:            "running",
		CurrentStep:       stringPtr("review"),
		LastCompletedStep: stringPtr("snapshot"),
		StartedAt:         nowISO,
		LastHeartbeatAt:   stringPtr(nowISO),
		CreatedAt:         nowISO,
		UpdatedAt:         nowISO,
	}); err != nil {
		t.Fatalf("Runs.Upsert() seed error = %v", err)
	}
	if err := seedRepos.Queue.Upsert(context.Background(), storage.QueueItemRecord{
		ID:          queueID,
		ProjectID:   &projectID,
		LoopID:      &loopID,
		Type:        "reviewer",
		TargetType:  "pull_request",
		TargetID:    "pr:acme/looper:42",
		Repo:        stringPtr("acme/looper"),
		PRNumber:    int64Ptr(42),
		DedupeKey:   "reviewer:acme/looper:42",
		Priority:    2,
		Status:      "running",
		AvailableAt: nowISO,
		Attempts:    0,
		MaxAttempts: 3,
		ClaimedBy:   stringPtr("executor_1"),
		ClaimedAt:   stringPtr(nowISO),
		StartedAt:   stringPtr(nowISO),
		LockKey:     stringPtr("pr:acme/looper:42"),
		CreatedAt:   nowISO,
		UpdatedAt:   nowISO,
	}); err != nil {
		t.Fatalf("Queue.Upsert() seed error = %v", err)
	}
	reason := "claim"
	if _, err := seedRepos.Locks.Acquire(context.Background(), storage.LockRecord{
		Key:       "pr:acme/looper:42",
		Owner:     "reviewer-loop",
		Reason:    &reason,
		ExpiresAt: "2020-01-01T00:00:00.000Z",
		CreatedAt: nowISO,
		UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Locks.Acquire() seed error = %v", err)
	}
	if err := seedCoordinator.Close(); err != nil {
		t.Fatalf("seed coordinator close error = %v", err)
	}

	tickStarted := make(chan struct{})
	var tickOnce sync.Once
	rt := New(Options{
		Config: cfg,
		Logger: &testLogger{},
		Now: func() time.Time {
			return startedAt
		},
		RunSchedulerTick: func(ctx context.Context, services Services) error {
			loop, err := services.Repositories.Loops.GetByID(ctx, loopID)
			if err != nil {
				return err
			}
			if loop == nil || loop.Status != "queued" {
				t.Fatalf("scheduler tick saw loop %#v, want queued recovery state", loop)
			}
			tickOnce.Do(func() { close(tickStarted) })
			return nil
		},
	})

	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { rt.Stop("test cleanup") })

	select {
	case <-tickStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("scheduler tick did not run immediately after startup")
	}

	services := rt.Services()
	run, err := services.Repositories.Runs.GetByID(context.Background(), "run_1")
	if err != nil {
		t.Fatalf("Runs.GetByID() error = %v", err)
	}
	if run == nil || run.Status != "interrupted" {
		t.Fatalf("Runs.GetByID(run_1) = %#v, want interrupted", run)
	}
	loop, err := services.Repositories.Loops.GetByID(context.Background(), loopID)
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if loop == nil || loop.Status != "queued" || loop.NextRunAt == nil || *loop.NextRunAt != nowISO {
		t.Fatalf("Loops.GetByID(loop_1) = %#v, want queued with next_run_at", loop)
	}
	queue, err := services.Repositories.Queue.GetByID(context.Background(), queueID)
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	if queue == nil || queue.Status != "queued" || queue.ClaimedBy != nil || queue.StartedAt != nil {
		t.Fatalf("Queue.GetByID(queue_1) = %#v, want requeued and unclaimed", queue)
	}
	lock, err := services.Repositories.Locks.Get(context.Background(), "pr:acme/looper:42")
	if err != nil {
		t.Fatalf("Locks.Get() error = %v", err)
	}
	if lock != nil {
		t.Fatalf("Locks.Get() = %#v, want released lock", lock)
	}

	recovery := rt.RecoverySummary()
	if recovery.ExpiredLocksReleased != 1 || recovery.InterruptedRunsMarked != 1 || recovery.LoopsRequeued != 1 || recovery.EventsWritten != 4 {
		t.Fatalf("RecoverySummary() = %#v, want one recovered lock/run/loop and 4 events", recovery)
	}

	events, err := services.Repositories.Events.ListByEntity(context.Background(), "loop", loopID)
	if err != nil {
		t.Fatalf("Events.ListByEntity(loop) error = %v", err)
	}
	if !containsEventType(events, "looperd.recovery.loop_requeued") {
		t.Fatalf("loop events = %#v, want looperd.recovery.loop_requeued", events)
	}

	allEvents, err := services.Repositories.Events.List(context.Background(), 20)
	if err != nil {
		t.Fatalf("Events.List() error = %v", err)
	}
	if !containsEventType(allEvents, "looperd.started") {
		t.Fatalf("all events = %#v, want looperd.started", allEvents)
	}
	if len(allEvents) == 0 || allEvents[0].EventType != "looperd.started" {
		t.Fatalf("all events = %#v, want looperd.started listed after recovery events", allEvents)
	}
	if allEvents[0].PayloadJSON == "" {
		t.Fatal("looperd.started payload = nil, want recovery summary")
	}
	var startedPayload struct {
		Recovery RecoverySummary `json:"recovery"`
	}
	if err := json.Unmarshal([]byte(allEvents[0].PayloadJSON), &startedPayload); err != nil {
		t.Fatalf("json.Unmarshal(looperd.started payload) error = %v", err)
	}
	if startedPayload.Recovery != recovery {
		t.Fatalf("started event recovery = %#v, want %#v", startedPayload.Recovery, recovery)
	}
}

func TestBuildRecoveryQueueItemRecordUsesIssueLockForWorkerTargets(t *testing.T) {
	t.Parallel()

	loop := storage.LoopRecord{
		ID:           "loop_worker_issue",
		ProjectID:    "project_1",
		Type:         "worker",
		TargetType:   "issue",
		TargetID:     stringPtr("issue:acme/looper:77"),
		Repo:         stringPtr("acme/looper"),
		Status:       "queued",
		MetadataJSON: stringPtr(`{"worker":{"title":"Implement worker loop","repo":"acme/looper","issueNumber":77,"baseBranch":"main"}}`),
	}

	record, ok, err := buildRecoveryQueueItem(loop, "2026-04-17T12:34:56.000Z", 3)
	if err != nil {
		t.Fatalf("buildRecoveryQueueItemRecord() error = %v", err)
	}
	if !ok {
		t.Fatal("buildRecoveryQueueItemRecord() ok = false, want true")
	}
	if record.TargetType != "issue" {
		t.Fatalf("record.TargetType = %q, want issue", record.TargetType)
	}
	if record.TargetID != "issue:acme/looper:77" {
		t.Fatalf("record.TargetID = %q, want issue target id", record.TargetID)
	}
	if record.LockKey == nil || *record.LockKey != "issue:acme/looper:77" {
		t.Fatalf("record.LockKey = %#v, want issue lock key", record.LockKey)
	}
	if record.DedupeKey != "worker:project_1:acme/looper:77" {
		t.Fatalf("record.DedupeKey = %q, want issue dedupe key", record.DedupeKey)
	}
}

func TestBuildRecoveryQueueItemDoesNotRestoreManualPlannerPayloadFromLoopMetadata(t *testing.T) {
	t.Parallel()

	loop := storage.LoopRecord{
		ID:           "loop_planner_manual",
		ProjectID:    "project_1",
		Type:         "planner",
		TargetType:   "issue",
		TargetID:     stringPtr("issue:acme/looper:82"),
		Repo:         stringPtr("acme/looper"),
		Status:       "queued",
		MetadataJSON: stringPtr(`{"issueNumber":82,"manual":true}`),
	}

	record, ok, err := buildRecoveryQueueItem(loop, "2026-04-17T12:34:56.000Z", 3)
	if err != nil {
		t.Fatalf("buildRecoveryQueueItem() error = %v", err)
	}
	if !ok {
		t.Fatal("buildRecoveryQueueItem() ok = false, want true")
	}
	if record.PayloadJSON == nil {
		t.Fatal("record.PayloadJSON = nil, want planner issue payload")
	}
	payload := map[string]any{}
	if err := json.Unmarshal([]byte(*record.PayloadJSON), &payload); err != nil {
		t.Fatalf("json.Unmarshal(payload) error = %v", err)
	}
	if _, ok := payload["manual"]; ok {
		t.Fatalf("payload = %#v, want recovery payload without manual bypass", payload)
	}
	if payload["issueNumber"] != float64(82) {
		t.Fatalf("payload = %#v, want planner issue payload", payload)
	}
}

func TestRuntimeStartConfiguresDefaultSchedulerTickAtStartup(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}

	vendor := config.AgentVendorCodex
	cfg.Agent.Vendor = &vendor
	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")

	rt := New(Options{Config: cfg, Logger: &testLogger{}})
	if rt.runSchedulerTick == nil {
		t.Fatal("runSchedulerTick = nil before Start(), want startup wrapper")
	}
	if rt.defaultSchedulerTick != nil {
		t.Fatal("defaultSchedulerTick configured before Start(), want deferred startup injection")
	}

	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { rt.Stop("test cleanup") })

	if rt.defaultSchedulerTick == nil {
		t.Fatal("defaultSchedulerTick = nil after Start(), want injected scheduler implementation")
	}
	if err := rt.runSchedulerTick(context.Background(), rt.Services()); err != nil {
		t.Fatalf("runSchedulerTick() error = %v", err)
	}
	if rt.customSchedulerTick {
		t.Fatal("customSchedulerTick = true, want false for default scheduler")
	}
}

func TestRuntimeStartNormalizesStaleQueuedLoops(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}

	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	backupDir := filepath.Join(workingDir, "backups")
	cfg.Storage.BackupDir = &backupDir
	startedAt := time.Date(2026, time.April, 17, 12, 34, 56, 0, time.UTC)
	nowISO := formatJavaScriptISOString(startedAt)

	seedCoordinator := openMigratedCoordinator(t, cfg.Storage.DBPath, backupDir)
	seedRepos := storage.NewRepositories(seedCoordinator.DB())
	baseBranch := "main"
	projectID := "project_1"
	if err := seedRepos.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID:         projectID,
		Name:       "Looper",
		RepoPath:   filepath.Join(workingDir, "repo"),
		BaseBranch: &baseBranch,
		Archived:   false,
		CreatedAt:  nowISO,
		UpdatedAt:  nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() seed error = %v", err)
	}
	seedLoopWithRun(t, seedRepos, projectID, "loop_success", 1, "queued", "success", nowISO)
	seedLoopWithRun(t, seedRepos, projectID, "loop_failed", 2, "queued", "failed", nowISO)
	seedLoopWithRun(t, seedRepos, projectID, "loop_legit", 3, "queued", "success", nowISO)
	seedLoopWithRun(t, seedRepos, projectID, "loop_requeued", 4, "queued", "interrupted", nowISO)
	if err := seedRepos.Queue.Upsert(context.Background(), storage.QueueItemRecord{
		ID:          "queue_legit",
		ProjectID:   &projectID,
		LoopID:      stringPtr("loop_legit"),
		Type:        "worker",
		TargetType:  "pull_request",
		TargetID:    "pr:acme/looper:99",
		DedupeKey:   "worker:acme/looper:99",
		Priority:    1,
		Status:      "queued",
		AvailableAt: nowISO,
		Attempts:    0,
		MaxAttempts: 3,
		CreatedAt:   nowISO,
		UpdatedAt:   nowISO,
	}); err != nil {
		t.Fatalf("Queue.Upsert(queue_legit) seed error = %v", err)
	}
	if err := seedCoordinator.Close(); err != nil {
		t.Fatalf("seed coordinator close error = %v", err)
	}

	rt := New(Options{
		Config: cfg,
		Logger: &testLogger{},
		Now: func() time.Time {
			return startedAt
		},
	})
	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { rt.Stop("test cleanup") })

	services := rt.Services()
	assertLoopStatus(t, services.Repositories, "loop_success", "completed")
	assertLoopStatus(t, services.Repositories, "loop_failed", "failed")
	assertLoopStatus(t, services.Repositories, "loop_legit", "queued")
	assertLoopStatus(t, services.Repositories, "loop_requeued", "queued")

	successEvents, err := services.Repositories.Events.ListByEntity(context.Background(), "loop", "loop_success")
	if err != nil {
		t.Fatalf("Events.ListByEntity(loop_success) error = %v", err)
	}
	if !containsEventType(successEvents, "looperd.recovery.loop_queue_normalized") {
		t.Fatalf("loop_success events = %#v, want normalization event", successEvents)
	}
	legitEvents, err := services.Repositories.Events.ListByEntity(context.Background(), "loop", "loop_legit")
	if err != nil {
		t.Fatalf("Events.ListByEntity(loop_legit) error = %v", err)
	}
	if containsEventType(legitEvents, "looperd.recovery.loop_queue_normalized") {
		t.Fatalf("loop_legit events = %#v, want no normalization event", legitEvents)
	}
	requeuedEvents, err := services.Repositories.Events.ListByEntity(context.Background(), "loop", "loop_requeued")
	if err != nil {
		t.Fatalf("Events.ListByEntity(loop_requeued) error = %v", err)
	}
	if !containsEventType(requeuedEvents, "looperd.recovery.loop_requeued") {
		t.Fatalf("loop_requeued events = %#v, want requeue event", requeuedEvents)
	}
	if containsEventType(requeuedEvents, "looperd.recovery.loop_queue_normalized") {
		t.Fatalf("loop_requeued events = %#v, want no normalization event", requeuedEvents)
	}
}

func TestRuntimeRecoveryRebuildsMissingQueueItemForRequeuedLoop(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}

	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	backupDir := filepath.Join(workingDir, "backups")
	cfg.Storage.BackupDir = &backupDir
	startedAt := time.Date(2026, time.April, 17, 12, 34, 56, 0, time.UTC)
	nowISO := formatJavaScriptISOString(startedAt)

	seedCoordinator := openMigratedCoordinator(t, cfg.Storage.DBPath, backupDir)
	seedRepos := storage.NewRepositories(seedCoordinator.DB())
	baseBranch := "main"
	projectID := "project_1"
	if err := seedRepos.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID:         projectID,
		Name:       "Looper",
		RepoPath:   filepath.Join(workingDir, "repo"),
		BaseBranch: &baseBranch,
		Archived:   false,
		CreatedAt:  nowISO,
		UpdatedAt:  nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() seed error = %v", err)
	}
	prNumber := int64(99)
	metadata := `{"worker":{"title":"Repair queue","prompt":"Rebuild queue item","repo":"acme/looper","baseBranch":"main"}}`
	if err := seedRepos.Loops.Upsert(context.Background(), storage.LoopRecord{
		ID:           "loop_requeued",
		Seq:          1,
		ProjectID:    projectID,
		Type:         "worker",
		TargetType:   "pull_request",
		TargetID:     stringPtr("pr:acme/looper:99"),
		Repo:         stringPtr("acme/looper"),
		PRNumber:     &prNumber,
		Status:       "running",
		MetadataJSON: &metadata,
		NextRunAt:    stringPtr(nowISO),
		CreatedAt:    nowISO,
		UpdatedAt:    nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert(loop_requeued) error = %v", err)
	}
	if err := seedRepos.Runs.Upsert(context.Background(), storage.RunRecord{
		ID:        "run_requeued",
		LoopID:    "loop_requeued",
		Status:    "running",
		StartedAt: nowISO,
		CreatedAt: nowISO,
		UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Runs.Upsert(run_requeued) error = %v", err)
	}
	if err := seedCoordinator.Close(); err != nil {
		t.Fatalf("seed coordinator close error = %v", err)
	}

	rt := New(Options{
		Config: cfg,
		Logger: &testLogger{},
		Now: func() time.Time {
			return startedAt
		},
	})
	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { rt.Stop("test cleanup") })

	services := rt.Services()
	assertLoopStatus(t, services.Repositories, "loop_requeued", "queued")
	queueItems, err := services.Repositories.Queue.List(context.Background())
	if err != nil {
		t.Fatalf("Queue.List() error = %v", err)
	}
	matched := []storage.QueueItemRecord{}
	for _, item := range queueItems {
		if item.LoopID != nil && *item.LoopID == "loop_requeued" {
			matched = append(matched, item)
		}
	}
	if len(matched) != 1 {
		t.Fatalf("queue items for loop_requeued = %#v, want 1 rebuilt queue item", matched)
	}
	queue := matched[0]
	if queue.Status != "queued" {
		t.Fatalf("queue.Status = %q, want queued", queue.Status)
	}
	if queue.TargetID != "pr:acme/looper:99" {
		t.Fatalf("queue.TargetID = %q, want pr:acme/looper:99", queue.TargetID)
	}
	if queue.DedupeKey != "worker:project_1:acme/looper:99" {
		t.Fatalf("queue.DedupeKey = %q, want worker:project_1:acme/looper:99", queue.DedupeKey)
	}
	if queue.PayloadJSON == nil {
		t.Fatal("queue.PayloadJSON = nil, want rebuilt worker payload")
	}
	payload := map[string]any{}
	if err := json.Unmarshal([]byte(*queue.PayloadJSON), &payload); err != nil {
		t.Fatalf("json.Unmarshal(queue.PayloadJSON) error = %v", err)
	}
	if payload["title"] != "Repair queue" {
		t.Fatalf("payload title = %#v, want Repair queue", payload["title"])
	}

	requeuedEvents, err := services.Repositories.Events.ListByEntity(context.Background(), "loop", "loop_requeued")
	if err != nil {
		t.Fatalf("Events.ListByEntity(loop_requeued) error = %v", err)
	}
	if !containsEventType(requeuedEvents, "looperd.recovery.loop_requeued") {
		t.Fatalf("loop_requeued events = %#v, want requeue event", requeuedEvents)
	}
}

func TestRuntimeRecoveryDoesNotRequeueTerminalReviewerLoopWithInterruptedRun(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}

	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	backupDir := filepath.Join(workingDir, "backups")
	cfg.Storage.BackupDir = &backupDir
	startedAt := time.Date(2026, time.April, 17, 12, 34, 56, 0, time.UTC)
	nowISO := formatJavaScriptISOString(startedAt)

	seedCoordinator := openMigratedCoordinator(t, cfg.Storage.DBPath, backupDir)
	seedRepos := storage.NewRepositories(seedCoordinator.DB())
	baseBranch := "main"
	projectID := "project_1"
	if err := seedRepos.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID: projectID, Name: "Looper", RepoPath: filepath.Join(workingDir, "repo"), BaseBranch: &baseBranch,
		Archived: false, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() seed error = %v", err)
	}
	prNumber := int64(137)
	if err := seedRepos.Loops.Upsert(context.Background(), storage.LoopRecord{
		ID: "loop_terminal_reviewer", Seq: 1, ProjectID: projectID, Type: "reviewer", TargetType: "pull_request",
		TargetID: stringPtr("pr:acme/looper:137"), Repo: stringPtr("acme/looper"), PRNumber: &prNumber,
		Status: "terminated", NextRunAt: stringPtr(nowISO), CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert(loop_terminal_reviewer) error = %v", err)
	}
	if err := seedRepos.Runs.Upsert(context.Background(), storage.RunRecord{
		ID: "run_terminal_reviewer", LoopID: "loop_terminal_reviewer", Status: "running", StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Runs.Upsert(run_terminal_reviewer) error = %v", err)
	}
	if err := seedCoordinator.Close(); err != nil {
		t.Fatalf("seed coordinator close error = %v", err)
	}

	rt := New(Options{Config: cfg, Logger: &testLogger{}, Now: func() time.Time { return startedAt }})
	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { rt.Stop("test cleanup") })

	services := rt.Services()
	assertLoopStatus(t, services.Repositories, "loop_terminal_reviewer", "terminated")
	latestRun, err := services.Repositories.Runs.GetLatestByLoopID(context.Background(), "loop_terminal_reviewer")
	if err != nil || latestRun == nil {
		t.Fatalf("Runs.GetLatestByLoopID() = (%#v, %v), want interrupted run", latestRun, err)
	}
	if latestRun.Status != "interrupted" {
		t.Fatalf("latestRun.Status = %q, want interrupted", latestRun.Status)
	}
	queueItems, err := services.Repositories.Queue.List(context.Background())
	if err != nil {
		t.Fatalf("Queue.List() error = %v", err)
	}
	for _, item := range queueItems {
		if item.LoopID != nil && *item.LoopID == "loop_terminal_reviewer" {
			t.Fatalf("unexpected queue item for terminal reviewer loop: %#v", item)
		}
	}
	events, err := services.Repositories.Events.ListByEntity(context.Background(), "loop", "loop_terminal_reviewer")
	if err != nil {
		t.Fatalf("Events.ListByEntity(loop_terminal_reviewer) error = %v", err)
	}
	if containsEventType(events, "looperd.recovery.loop_requeued") {
		t.Fatalf("events = %#v, want no loop_requeued event", events)
	}
}

func TestRuntimeRecoveryRequeuesRunningQueueItemWithoutRun(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}

	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	backupDir := filepath.Join(workingDir, "backups")
	cfg.Storage.BackupDir = &backupDir
	startedAt := time.Date(2026, time.April, 17, 12, 34, 56, 0, time.UTC)
	nowISO := formatJavaScriptISOString(startedAt)
	runningAt := "2026-04-17T12:30:00.000Z"

	seedCoordinator := openMigratedCoordinator(t, cfg.Storage.DBPath, backupDir)
	seedRepos := storage.NewRepositories(seedCoordinator.DB())
	baseBranch := "main"
	projectID := "project_1"
	if err := seedRepos.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID:         projectID,
		Name:       "Looper",
		RepoPath:   filepath.Join(workingDir, "repo"),
		BaseBranch: &baseBranch,
		Archived:   false,
		CreatedAt:  nowISO,
		UpdatedAt:  nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() seed error = %v", err)
	}
	prNumber := int64(99)
	metadata := `{"worker":{"title":"Repair queue","prompt":"Rebuild queue item","repo":"acme/looper","baseBranch":"main"}}`
	if err := seedRepos.Loops.Upsert(context.Background(), storage.LoopRecord{
		ID:           "loop_requeued",
		Seq:          1,
		ProjectID:    projectID,
		Type:         "worker",
		TargetType:   "pull_request",
		TargetID:     stringPtr("pr:acme/looper:99"),
		Repo:         stringPtr("acme/looper"),
		PRNumber:     &prNumber,
		Status:       "running",
		MetadataJSON: &metadata,
		NextRunAt:    stringPtr(nowISO),
		CreatedAt:    nowISO,
		UpdatedAt:    nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert(loop_requeued) error = %v", err)
	}
	if err := seedRepos.Queue.Upsert(context.Background(), storage.QueueItemRecord{
		ID:          "queue_requeued",
		ProjectID:   &projectID,
		LoopID:      stringPtr("loop_requeued"),
		Type:        "worker",
		TargetType:  "pull_request",
		TargetID:    "pr:acme/looper:99",
		DedupeKey:   "worker:project_1:acme/looper:99",
		Priority:    1,
		Status:      "running",
		AvailableAt: nowISO,
		Attempts:    1,
		MaxAttempts: 3,
		ClaimedBy:   stringPtr("worker-a"),
		ClaimedAt:   stringPtr(runningAt),
		StartedAt:   stringPtr(runningAt),
		CreatedAt:   nowISO,
		UpdatedAt:   runningAt,
	}); err != nil {
		t.Fatalf("Queue.Upsert(queue_requeued) error = %v", err)
	}
	if err := seedCoordinator.Close(); err != nil {
		t.Fatalf("seed coordinator close error = %v", err)
	}

	rt := New(Options{
		Config: cfg,
		Logger: &testLogger{},
		Now: func() time.Time {
			return startedAt
		},
	})
	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { rt.Stop("test cleanup") })

	services := rt.Services()
	assertLoopStatus(t, services.Repositories, "loop_requeued", "queued")
	queue, err := services.Repositories.Queue.GetByID(context.Background(), "queue_requeued")
	if err != nil {
		t.Fatalf("Queue.GetByID(queue_requeued) error = %v", err)
	}
	if queue == nil {
		t.Fatal("Queue.GetByID(queue_requeued) = nil, want recovered queue item")
	}
	if queue.Status != "queued" {
		t.Fatalf("queue.Status = %q, want queued", queue.Status)
	}
	if queue.ClaimedBy != nil || queue.ClaimedAt != nil || queue.StartedAt != nil {
		t.Fatalf("queue claim fields = %#v/%#v/%#v, want cleared", queue.ClaimedBy, queue.ClaimedAt, queue.StartedAt)
	}

	requeuedEvents, err := services.Repositories.Events.ListByEntity(context.Background(), "loop", "loop_requeued")
	if err != nil {
		t.Fatalf("Events.ListByEntity(loop_requeued) error = %v", err)
	}
	if !containsEventType(requeuedEvents, "looperd.recovery.loop_requeued") {
		t.Fatalf("loop_requeued events = %#v, want requeue event", requeuedEvents)
	}
}

func TestRuntimeRecoveryCleansOrphanAgentExecutions(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}

	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	backupDir := filepath.Join(workingDir, "backups")
	cfg.Storage.BackupDir = &backupDir
	startedAt := time.Date(2026, time.April, 17, 12, 34, 56, 0, time.UTC)
	nowISO := formatJavaScriptISOString(startedAt)

	seedCoordinator := openMigratedCoordinator(t, cfg.Storage.DBPath, backupDir)
	seedRepos := storage.NewRepositories(seedCoordinator.DB())
	pid := int64(4242)
	nativeSessionID := "codex-session-1"
	if err := seedRepos.AgentExecutions.Upsert(context.Background(), storage.AgentExecutionRecord{
		ID:              "agent_orphan_1",
		Vendor:          "codex",
		Status:          "running",
		PID:             &pid,
		CommandJSON:     stringPtr(`{"command":"codex","args":["exec","fix failing tests"]}`),
		CWD:             stringPtr(workingDir),
		HeartbeatCount:  0,
		NativeSessionID: &nativeSessionID,
		StartedAt:       nowISO,
		CreatedAt:       nowISO,
		UpdatedAt:       nowISO,
	}); err != nil {
		t.Fatalf("AgentExecutions.Upsert() seed error = %v", err)
	}
	if err := seedCoordinator.Close(); err != nil {
		t.Fatalf("seed coordinator close error = %v", err)
	}

	rt := New(Options{
		Config: cfg,
		Logger: &testLogger{},
		Now: func() time.Time {
			return startedAt
		},
		ReadProcessCommand: func(context.Context, int) (string, error) {
			return "codex exec fix failing tests", nil
		},
		SignalProcess: func(gotPID int, signal syscall.Signal) error {
			if signal == syscall.SIGKILL {
				return nil
			}
			if gotPID != -int(pid) || signal != syscall.SIGTERM {
				t.Fatalf("SignalProcess(%d, %v), want (%d, SIGTERM)", gotPID, signal, -pid)
			}
			return nil
		},
	})
	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { rt.Stop("test cleanup") })

	services := rt.Services()
	agentExecution, err := services.Repositories.AgentExecutions.GetByID(context.Background(), "agent_orphan_1")
	if err != nil {
		t.Fatalf("AgentExecutions.GetByID() error = %v", err)
	}
	if agentExecution == nil || agentExecution.Status != "killed" || agentExecution.EndedAt == nil {
		t.Fatalf("AgentExecutions.GetByID(agent_orphan_1) = %#v, want killed with ended_at", agentExecution)
	}
	if agentExecution.NativeSessionID == nil || *agentExecution.NativeSessionID != nativeSessionID || agentExecution.NativeResumeMode == nil || *agentExecution.NativeResumeMode != "native_resume" || agentExecution.NativeResumeStatus == nil || *agentExecution.NativeResumeStatus != "pending" {
		t.Fatalf("AgentExecutions.GetByID(agent_orphan_1) = %#v, want native resume pending metadata", agentExecution)
	}

	events, err := services.Repositories.Events.ListByEntity(context.Background(), "agent_execution", "agent_orphan_1")
	if err != nil {
		t.Fatalf("Events.ListByEntity(agent_execution) error = %v", err)
	}
	if !containsEventType(events, "agent.killed") {
		t.Fatalf("agent_execution events = %#v, want agent.killed", events)
	}

	recovery := rt.RecoverySummary()
	if !recovery.OrphanAgentCleanup.Attempted || recovery.OrphanAgentCleanup.CleanedCount != 1 {
		t.Fatalf("RecoverySummary().OrphanAgentCleanup = %#v, want attempted + cleanedCount=1", recovery.OrphanAgentCleanup)
	}
}

func TestRuntimeRecoverySkipsMismatchedRecoveredPID(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}

	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	backupDir := filepath.Join(workingDir, "backups")
	cfg.Storage.BackupDir = &backupDir
	startedAt := time.Date(2026, time.April, 17, 12, 34, 56, 0, time.UTC)
	nowISO := formatJavaScriptISOString(startedAt)

	seedCoordinator := openMigratedCoordinator(t, cfg.Storage.DBPath, backupDir)
	seedRepos := storage.NewRepositories(seedCoordinator.DB())
	pid := int64(4343)
	if err := seedRepos.AgentExecutions.Upsert(context.Background(), storage.AgentExecutionRecord{
		ID:             "agent_orphan_mismatch",
		Vendor:         "codex",
		Status:         "running",
		PID:            &pid,
		CommandJSON:    stringPtr(`{"command":"codex","args":["exec"]}`),
		CWD:            stringPtr(workingDir),
		HeartbeatCount: 0,
		StartedAt:      nowISO,
		CreatedAt:      nowISO,
		UpdatedAt:      nowISO,
	}); err != nil {
		t.Fatalf("AgentExecutions.Upsert() seed error = %v", err)
	}
	if err := seedCoordinator.Close(); err != nil {
		t.Fatalf("seed coordinator close error = %v", err)
	}

	logger := &testLogger{}
	signaled := false
	rt := New(Options{
		Config: cfg,
		Logger: logger,
		Now: func() time.Time {
			return startedAt
		},
		ReadProcessCommand: func(context.Context, int) (string, error) {
			return "python unrelated.py", nil
		},
		SignalProcess: func(int, syscall.Signal) error {
			signaled = true
			return nil
		},
	})
	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { rt.Stop("test cleanup") })

	services := rt.Services()
	agentExecution, err := services.Repositories.AgentExecutions.GetByID(context.Background(), "agent_orphan_mismatch")
	if err != nil {
		t.Fatalf("AgentExecutions.GetByID() error = %v", err)
	}
	if agentExecution == nil || agentExecution.Status != "running" || agentExecution.EndedAt != nil {
		t.Fatalf("AgentExecutions.GetByID(agent_orphan_mismatch) = %#v, want still running without ended_at", agentExecution)
	}
	if signaled {
		t.Fatal("SignalProcess() called, want mismatched pid to be skipped")
	}
	events, err := services.Repositories.Events.ListByEntity(context.Background(), "agent_execution", "agent_orphan_mismatch")
	if err != nil {
		t.Fatalf("Events.ListByEntity(agent_execution) error = %v", err)
	}
	if containsEventType(events, "agent.killed") {
		t.Fatalf("agent_execution events = %#v, want no agent.killed for mismatched pid", events)
	}
	if !containsEventType(events, "looperd.recovery.process_identity_uncertain") {
		t.Fatalf("agent_execution events = %#v, want uncertain-identity event", events)
	}
	recovery := rt.RecoverySummary()
	if recovery.OrphanAgentCleanup.CleanedCount != 0 {
		t.Fatalf("RecoverySummary().OrphanAgentCleanup = %#v, want cleanedCount=0", recovery.OrphanAgentCleanup)
	}
	if !logger.containsMessage("recovery skipped due to uncertain process identity") {
		t.Fatalf("logger entries = %#v, want uncertain process identity warning", logger.messages())
	}
}

func TestRuntimeRecoveryPreservesRunWithUncertainActiveAgentExecution(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}

	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	backupDir := filepath.Join(workingDir, "backups")
	cfg.Storage.BackupDir = &backupDir
	startedAt := time.Date(2026, time.April, 17, 12, 34, 56, 0, time.UTC)
	nowISO := formatJavaScriptISOString(startedAt)
	oldISO := formatJavaScriptISOString(startedAt.Add(-2 * time.Hour))

	seedCoordinator := openMigratedCoordinator(t, cfg.Storage.DBPath, backupDir)
	seedRepos := storage.NewRepositories(seedCoordinator.DB())
	repo := "nexu-io/looper"
	prNumber := int64(186)
	targetID := "pr:nexu-io/looper:186"
	loopID := "loop_mismatched_agent_running"
	runID := "run_mismatched_agent_running"
	if err := seedRepos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "project_1", Name: "Looper", RepoPath: filepath.Join(workingDir, "repo"), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() seed error = %v", err)
	}
	if err := seedRepos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 186, ProjectID: "project_1", Type: "fixer", TargetType: "pull_request", TargetID: &targetID, Repo: &repo, PRNumber: &prNumber, Status: "running", CreatedAt: oldISO, UpdatedAt: oldISO}); err != nil {
		t.Fatalf("Loops.Upsert() seed error = %v", err)
	}
	if err := seedRepos.Runs.Upsert(context.Background(), storage.RunRecord{ID: runID, LoopID: loopID, Status: "running", CurrentStep: stringPtr("execute"), StartedAt: oldISO, LastHeartbeatAt: &oldISO, CreatedAt: oldISO, UpdatedAt: oldISO}); err != nil {
		t.Fatalf("Runs.Upsert() seed error = %v", err)
	}
	pid := int64(4343)
	if err := seedRepos.AgentExecutions.Upsert(context.Background(), storage.AgentExecutionRecord{
		ID:             "agent_mismatched_running_run",
		ProjectID:      stringPtr("project_1"),
		LoopID:         &loopID,
		RunID:          &runID,
		Vendor:         "codex",
		Status:         "running",
		PID:            &pid,
		CommandJSON:    stringPtr(`{"command":"codex","args":["exec"]}`),
		CWD:            stringPtr(workingDir),
		HeartbeatCount: 0,
		StartedAt:      oldISO,
		CreatedAt:      oldISO,
		UpdatedAt:      oldISO,
	}); err != nil {
		t.Fatalf("AgentExecutions.Upsert() seed error = %v", err)
	}
	if err := seedCoordinator.Close(); err != nil {
		t.Fatalf("seed coordinator close error = %v", err)
	}

	logger := &testLogger{}
	rt := New(Options{
		Config: cfg,
		Logger: logger,
		Now: func() time.Time {
			return startedAt
		},
		ReadProcessCommand: func(context.Context, int) (string, error) {
			return "python unrelated.py", nil
		},
		SignalProcess: func(int, syscall.Signal) error {
			return nil
		},
	})
	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { rt.Stop("test cleanup") })

	services := rt.Services()
	run, err := services.Repositories.Runs.GetByID(context.Background(), runID)
	if err != nil {
		t.Fatalf("Runs.GetByID() error = %v", err)
	}
	if run == nil || run.Status != "running" || run.EndedAt != nil {
		t.Fatalf("Runs.GetByID(%s) = %#v, want preserved running run", runID, run)
	}
	loop, err := services.Repositories.Loops.GetByID(context.Background(), loopID)
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if loop == nil || loop.Status != "running" {
		t.Fatalf("Loops.GetByID(%s) = %#v, want preserved running loop", loopID, loop)
	}
	agentExecution, err := services.Repositories.AgentExecutions.GetByID(context.Background(), "agent_mismatched_running_run")
	if err != nil {
		t.Fatalf("AgentExecutions.GetByID() error = %v", err)
	}
	if agentExecution == nil || agentExecution.Status != "running" {
		t.Fatalf("AgentExecutions.GetByID(agent_mismatched_running_run) = %#v, want still running stale row", agentExecution)
	}
	events, err := services.Repositories.Events.ListByEntity(context.Background(), "agent_execution", "agent_mismatched_running_run")
	if err != nil {
		t.Fatalf("Events.ListByEntity(agent_mismatched_running_run) error = %v", err)
	}
	if !containsEventType(events, "looperd.recovery.process_identity_uncertain") {
		t.Fatalf("agent_execution events = %#v, want uncertain-identity event", events)
	}
	if recovery := rt.RecoverySummary(); recovery.InterruptedRunsMarked != 0 || recovery.LoopsRequeued != 0 || recovery.OrphanAgentCleanup.CleanedCount != 0 {
		t.Fatalf("RecoverySummary() = %#v, want preserved run and loop for uncertain live execution", recovery)
	}
	if !logger.containsMessage("recovery skipped due to uncertain process identity") {
		t.Fatalf("logger entries = %#v, want uncertain process identity warning", logger.messages())
	}
}

func TestCommandPrefixMatchesRejectsTruncatedPromptTail(t *testing.T) {
	t.Parallel()

	if commandPrefixMatches(
		[]string{"codex", "exec", "very long reviewer prompt that may be truncated by ps output"},
		[]string{"codex", "exec", "very long reviewer prompt"},
	) {
		t.Fatal("commandPrefixMatches() = true, want false for truncated prompt tail")
	}
}

func TestCommandPrefixMatchesRejectsMissingTail(t *testing.T) {
	t.Parallel()

	if commandPrefixMatches(
		[]string{"codex", "exec", "very long reviewer prompt that may be truncated by ps output"},
		[]string{"codex", "exec"},
	) {
		t.Fatal("commandPrefixMatches() = true, want false when actual command is missing the trailing token")
	}
}

func TestRuntimeRecoveryPreservesLoopWithActiveAgentExecution(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}

	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	backupDir := filepath.Join(workingDir, "backups")
	cfg.Storage.BackupDir = &backupDir
	startedAt := time.Date(2026, time.April, 17, 12, 34, 56, 0, time.UTC)
	nowISO := formatJavaScriptISOString(startedAt)
	oldISO := formatJavaScriptISOString(startedAt.Add(-2 * time.Hour))

	seedCoordinator := openMigratedCoordinator(t, cfg.Storage.DBPath, backupDir)
	seedRepos := storage.NewRepositories(seedCoordinator.DB())
	repo := "nexu-io/looper"
	prNumber := int64(186)
	targetID := "pr:nexu-io/looper:186"
	loopID := "loop_active_agent_running"
	runID := "run_active_agent_running"
	if err := seedRepos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "project_1", Name: "Looper", RepoPath: filepath.Join(workingDir, "repo"), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() seed error = %v", err)
	}
	if err := seedRepos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 186, ProjectID: "project_1", Type: "fixer", TargetType: "pull_request", TargetID: &targetID, Repo: &repo, PRNumber: &prNumber, Status: "running", CreatedAt: oldISO, UpdatedAt: oldISO}); err != nil {
		t.Fatalf("Loops.Upsert() seed error = %v", err)
	}
	if err := seedRepos.Runs.Upsert(context.Background(), storage.RunRecord{ID: runID, LoopID: loopID, Status: "running", CurrentStep: stringPtr("execute"), StartedAt: oldISO, LastHeartbeatAt: &oldISO, CreatedAt: oldISO, UpdatedAt: oldISO}); err != nil {
		t.Fatalf("Runs.Upsert() seed error = %v", err)
	}
	pid := int64(4444)
	if err := seedRepos.AgentExecutions.Upsert(context.Background(), storage.AgentExecutionRecord{
		ID:             "agent_active_running_run",
		ProjectID:      stringPtr("project_1"),
		LoopID:         &loopID,
		RunID:          &runID,
		Vendor:         "codex",
		Status:         "running",
		PID:            &pid,
		CommandJSON:    stringPtr(`{"command":"codex","args":["exec"]}`),
		CWD:            stringPtr(workingDir),
		HeartbeatCount: 0,
		StartedAt:      oldISO,
		CreatedAt:      oldISO,
		UpdatedAt:      oldISO,
	}); err != nil {
		t.Fatalf("AgentExecutions.Upsert() seed error = %v", err)
	}
	if err := seedCoordinator.Close(); err != nil {
		t.Fatalf("seed coordinator close error = %v", err)
	}

	rt := New(Options{
		Config: cfg,
		Logger: &testLogger{},
		Now: func() time.Time {
			return startedAt
		},
		ReadProcessCommand: func(context.Context, int) (string, error) {
			return "codex exec", nil
		},
		SignalProcess: func(int, syscall.Signal) error {
			return errors.New("process cleanup skipped")
		},
	})
	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { rt.Stop("test cleanup") })

	services := rt.Services()
	loop, err := services.Repositories.Loops.GetByID(context.Background(), loopID)
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if loop == nil || loop.Status != "running" {
		t.Fatalf("Loops.GetByID(%s) = %#v, want preserved running loop", loopID, loop)
	}
	run, err := services.Repositories.Runs.GetByID(context.Background(), runID)
	if err != nil {
		t.Fatalf("Runs.GetByID() error = %v", err)
	}
	if run == nil || run.Status != "running" || run.EndedAt != nil {
		t.Fatalf("Runs.GetByID(%s) = %#v, want preserved running run", runID, run)
	}
	queueItems, err := services.Repositories.Queue.List(context.Background())
	if err != nil {
		t.Fatalf("Queue.List() error = %v", err)
	}
	for _, item := range queueItems {
		if item.LoopID != nil && *item.LoopID == loopID {
			t.Fatalf("unexpected queue item for active running loop: %#v", item)
		}
	}
	if recovery := rt.RecoverySummary(); recovery.InterruptedRunsMarked != 0 || recovery.LoopsRequeued != 0 || recovery.OrphanAgentCleanup.CleanedCount != 0 {
		t.Fatalf("RecoverySummary() = %#v, want no run interruption or loop requeue while active agent remains", recovery)
	}
}

func TestRuntimeReconcileLiveStaleRunningRunsInterruptsAgentBackedRunWithoutExecution(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	backupDir := filepath.Join(workingDir, "backups")
	cfg.Storage.BackupDir = &backupDir
	now := time.Date(2026, time.May, 20, 12, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	oldISO := formatJavaScriptISOString(now.Add(-2 * time.Hour))

	rt := New(Options{Config: cfg, Logger: &testLogger{}, Now: func() time.Time { return now }})
	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer rt.Stop("test cleanup")
	repos := rt.Services().Repositories
	repo := "nexu-io/looper"
	prNumber := int64(186)
	targetID := "pr:nexu-io/looper:186"
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "project_1", Name: "Looper", RepoPath: filepath.Join(workingDir, "repo"), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: "loop_stale_live", Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", TargetID: &targetID, Repo: &repo, PRNumber: &prNumber, Status: "running", CreatedAt: oldISO, UpdatedAt: oldISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	if err := repos.Runs.Upsert(context.Background(), storage.RunRecord{ID: "run_stale_live", LoopID: "loop_stale_live", Status: "running", CurrentStep: stringPtr("review"), StartedAt: oldISO, LastHeartbeatAt: stringPtr(oldISO), CreatedAt: oldISO, UpdatedAt: oldISO}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}

	summary, err := rt.reconcileLiveStaleRunningRuns(context.Background())
	if err != nil {
		t.Fatalf("reconcileLiveStaleRunningRuns() error = %v", err)
	}
	if summary.Mode != "live" || summary.CandidateRuns != 1 || summary.InterruptedRuns != 1 || summary.LoopsRequeued != 1 {
		t.Fatalf("summary = %#v, want one interrupted live stale run and requeued loop", summary)
	}
	run, err := repos.Runs.GetByID(context.Background(), "run_stale_live")
	if err != nil {
		t.Fatalf("Runs.GetByID() error = %v", err)
	}
	if run == nil || run.Status != "interrupted" || run.EndedAt == nil {
		t.Fatalf("run = %#v, want interrupted ended run", run)
	}
	loop, err := repos.Loops.GetByID(context.Background(), "loop_stale_live")
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if loop == nil || loop.Status != "queued" {
		t.Fatalf("loop = %#v, want queued loop", loop)
	}
}

func TestRuntimeReconcileLiveStaleRunningRunsSkipsNonAgentRunWithoutExecution(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	backupDir := filepath.Join(workingDir, "backups")
	cfg.Storage.BackupDir = &backupDir
	now := time.Date(2026, time.May, 20, 12, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	oldISO := formatJavaScriptISOString(now.Add(-2 * time.Hour))

	rt := New(Options{Config: cfg, Logger: &testLogger{}, Now: func() time.Time { return now }})
	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer rt.Stop("test cleanup")
	repos := rt.Services().Repositories
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "project_1", Name: "Looper", RepoPath: filepath.Join(workingDir, "repo"), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: "loop_non_agent_live", Seq: 1, ProjectID: "project_1", Type: "worker", TargetType: "project", Status: "running", CreatedAt: oldISO, UpdatedAt: oldISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	if err := repos.Runs.Upsert(context.Background(), storage.RunRecord{ID: "run_non_agent_live", LoopID: "loop_non_agent_live", Status: "running", CurrentStep: stringPtr("discover-pr"), StartedAt: oldISO, LastHeartbeatAt: stringPtr(oldISO), CreatedAt: oldISO, UpdatedAt: oldISO}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}

	summary, err := rt.reconcileLiveStaleRunningRuns(context.Background())
	if err != nil {
		t.Fatalf("reconcileLiveStaleRunningRuns() error = %v", err)
	}
	if summary.CandidateRuns != 0 || summary.InterruptedRuns != 0 {
		t.Fatalf("summary = %#v, want no live reconciliation for non-agent stale run", summary)
	}
}

func TestRuntimeReconcileLiveStaleRunningRunsInterruptsWorkerExecuteRunWithoutExecution(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	backupDir := filepath.Join(workingDir, "backups")
	cfg.Storage.BackupDir = &backupDir
	now := time.Date(2026, time.May, 20, 12, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	oldISO := formatJavaScriptISOString(now.Add(-2 * time.Hour))

	rt := New(Options{Config: cfg, Logger: &testLogger{}, Now: func() time.Time { return now }})
	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer rt.Stop("test cleanup")
	repos := rt.Services().Repositories
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "project_1", Name: "Looper", RepoPath: filepath.Join(workingDir, "repo"), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: "loop_worker_execute_stale", Seq: 2, ProjectID: "project_1", Type: "worker", TargetType: "project", Status: "running", CreatedAt: oldISO, UpdatedAt: oldISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	if err := repos.Runs.Upsert(context.Background(), storage.RunRecord{ID: "run_worker_execute_stale", LoopID: "loop_worker_execute_stale", Status: "running", CurrentStep: stringPtr("execute"), StartedAt: oldISO, LastHeartbeatAt: stringPtr(oldISO), CreatedAt: oldISO, UpdatedAt: oldISO}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}

	summary, err := rt.reconcileLiveStaleRunningRuns(context.Background())
	if err != nil {
		t.Fatalf("reconcileLiveStaleRunningRuns() error = %v", err)
	}
	if summary.CandidateRuns != 1 || summary.InterruptedRuns != 1 || summary.LoopsRequeued != 1 {
		t.Fatalf("summary = %#v, want interrupted stale worker execute run", summary)
	}
}

func TestRuntimeReconcileStaleRunningRunsSkipsVerifiedLiveExecution(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		reconcile func(*Runtime, context.Context) (StaleRunReconcileSummary, error)
	}{
		{name: "live", reconcile: func(rt *Runtime, ctx context.Context) (StaleRunReconcileSummary, error) {
			return rt.reconcileLiveStaleRunningRuns(ctx)
		}},
		{name: "manual", reconcile: func(rt *Runtime, ctx context.Context) (StaleRunReconcileSummary, error) {
			return rt.ReconcileStaleRunningRuns(ctx)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workingDir := t.TempDir()
			cfg, err := config.DefaultConfig(workingDir)
			if err != nil {
				t.Fatalf("DefaultConfig() error = %v", err)
			}
			cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
			backupDir := filepath.Join(workingDir, "backups")
			cfg.Storage.BackupDir = &backupDir
			now := time.Date(2026, time.May, 20, 12, 0, 0, 0, time.UTC)
			nowISO := formatJavaScriptISOString(now)
			oldISO := formatJavaScriptISOString(now.Add(-2 * time.Hour))
			pid := int64(5151)

			rt := New(Options{Config: cfg, Logger: &testLogger{}, Now: func() time.Time { return now }, ReadProcessCommand: func(context.Context, int) (string, error) { return "codex exec", nil }})
			if err := rt.Start(context.Background()); err != nil {
				t.Fatalf("Start() error = %v", err)
			}
			defer rt.Stop("test cleanup")
			repos := rt.Services().Repositories
			repo := "nexu-io/looper"
			prNumber := int64(188)
			targetID := "pr:nexu-io/looper:188"
			if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "project_1", Name: "Looper", RepoPath: filepath.Join(workingDir, "repo"), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
				t.Fatalf("Projects.Upsert() error = %v", err)
			}
			if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: "loop_live_execution", Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", TargetID: &targetID, Repo: &repo, PRNumber: &prNumber, Status: "running", CreatedAt: oldISO, UpdatedAt: oldISO}); err != nil {
				t.Fatalf("Loops.Upsert() error = %v", err)
			}
			if err := repos.Runs.Upsert(context.Background(), storage.RunRecord{ID: "run_live_execution", LoopID: "loop_live_execution", Status: "running", CurrentStep: stringPtr("review"), StartedAt: oldISO, LastHeartbeatAt: stringPtr(oldISO), CreatedAt: oldISO, UpdatedAt: oldISO}); err != nil {
				t.Fatalf("Runs.Upsert() error = %v", err)
			}
			if err := repos.AgentExecutions.Upsert(context.Background(), storage.AgentExecutionRecord{ID: "exec_live_execution", ProjectID: stringPtr("project_1"), LoopID: stringPtr("loop_live_execution"), RunID: stringPtr("run_live_execution"), Vendor: "codex", Status: "running", PID: &pid, CommandJSON: stringPtr(`{"command":"codex","args":["exec"]}`), CWD: stringPtr(workingDir), StartedAt: oldISO, CreatedAt: oldISO, UpdatedAt: oldISO}); err != nil {
				t.Fatalf("AgentExecutions.Upsert() error = %v", err)
			}

			summary, err := tc.reconcile(rt, context.Background())
			if err != nil {
				t.Fatalf("reconcile error = %v", err)
			}
			if summary.CandidateRuns != 0 || summary.InterruptedRuns != 0 {
				t.Fatalf("summary = %#v, want verified live execution preserved", summary)
			}
			run, _ := repos.Runs.GetByID(context.Background(), "run_live_execution")
			if run == nil || run.Status != "running" {
				t.Fatalf("run = %#v, want still running", run)
			}
		})
	}
}

func TestRuntimeReconcileStaleRunningRunsKeepsSupersededRunWithVerifiedLiveExecution(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		reconcile func(*Runtime, context.Context) (StaleRunReconcileSummary, error)
	}{
		{name: "live", reconcile: func(rt *Runtime, ctx context.Context) (StaleRunReconcileSummary, error) {
			return rt.reconcileLiveStaleRunningRuns(ctx)
		}},
		{name: "manual", reconcile: func(rt *Runtime, ctx context.Context) (StaleRunReconcileSummary, error) {
			return rt.ReconcileStaleRunningRuns(ctx)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workingDir := t.TempDir()
			cfg, err := config.DefaultConfig(workingDir)
			if err != nil {
				t.Fatalf("DefaultConfig() error = %v", err)
			}
			cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
			backupDir := filepath.Join(workingDir, "backups")
			cfg.Storage.BackupDir = &backupDir
			now := time.Date(2026, time.May, 20, 12, 0, 0, 0, time.UTC)
			nowISO := formatJavaScriptISOString(now)
			oldISO := formatJavaScriptISOString(now.Add(-2 * time.Hour))
			completedISO := formatJavaScriptISOString(now.Add(-10 * time.Minute))
			pid := int64(5252)

			rt := New(Options{Config: cfg, Logger: &testLogger{}, Now: func() time.Time { return now }, ReadProcessCommand: func(context.Context, int) (string, error) { return "codex exec", nil }})
			if err := rt.Start(context.Background()); err != nil {
				t.Fatalf("Start() error = %v", err)
			}
			defer rt.Stop("test cleanup")
			repos := rt.Services().Repositories
			repo := "nexu-io/looper"
			prNumber := int64(189)
			targetID := "pr:nexu-io/looper:189"
			if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "project_1", Name: "Looper", RepoPath: filepath.Join(workingDir, "repo"), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
				t.Fatalf("Projects.Upsert() error = %v", err)
			}
			if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: "loop_superseded_live", Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", TargetID: &targetID, Repo: &repo, PRNumber: &prNumber, Status: "completed", CreatedAt: oldISO, UpdatedAt: completedISO}); err != nil {
				t.Fatalf("Loops.Upsert() error = %v", err)
			}
			if err := repos.Runs.Upsert(context.Background(), storage.RunRecord{ID: "run_superseded_old", LoopID: "loop_superseded_live", Status: "running", CurrentStep: stringPtr("review"), StartedAt: oldISO, LastHeartbeatAt: stringPtr(oldISO), CreatedAt: oldISO, UpdatedAt: oldISO}); err != nil {
				t.Fatalf("Runs.Upsert(old) error = %v", err)
			}
			if err := repos.Runs.Upsert(context.Background(), storage.RunRecord{ID: "run_superseded_latest", LoopID: "loop_superseded_live", Status: "success", StartedAt: completedISO, EndedAt: stringPtr(completedISO), CreatedAt: completedISO, UpdatedAt: completedISO}); err != nil {
				t.Fatalf("Runs.Upsert(latest) error = %v", err)
			}
			if err := repos.AgentExecutions.Upsert(context.Background(), storage.AgentExecutionRecord{ID: "exec_superseded_live", ProjectID: stringPtr("project_1"), LoopID: stringPtr("loop_superseded_live"), RunID: stringPtr("run_superseded_old"), Vendor: "codex", Status: "running", PID: &pid, CommandJSON: stringPtr(`{"command":"codex","args":["exec"]}`), CWD: stringPtr(workingDir), StartedAt: oldISO, CreatedAt: oldISO, UpdatedAt: oldISO}); err != nil {
				t.Fatalf("AgentExecutions.Upsert() error = %v", err)
			}

			summary, err := tc.reconcile(rt, context.Background())
			if err != nil {
				t.Fatalf("reconcile error = %v", err)
			}
			if summary.CandidateRuns != 0 || summary.InterruptedRuns != 0 {
				t.Fatalf("summary = %#v, want superseded live run preserved", summary)
			}
			run, _ := repos.Runs.GetByID(context.Background(), "run_superseded_old")
			if run == nil || run.Status != "running" {
				t.Fatalf("run = %#v, want still running", run)
			}
		})
	}
}

func TestRepairStaleRunQueueStateDoesNotRequeueLoopWhileNewerRunIsLive(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	backupDir := filepath.Join(workingDir, "backups")
	cfg.Storage.BackupDir = &backupDir
	now := time.Date(2026, time.May, 20, 12, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	oldISO := formatJavaScriptISOString(now.Add(-2 * time.Hour))

	rt := New(Options{Config: cfg, Logger: &testLogger{}, Now: func() time.Time { return now }})
	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer rt.Stop("test cleanup")
	repos := rt.Services().Repositories
	repo := "nexu-io/looper"
	prNumber := int64(195)
	targetID := "pr:nexu-io/looper:195"
	loopID := "loop_superseded_latest_live"
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "project_1", Name: "Looper", RepoPath: filepath.Join(workingDir, "repo"), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	loop := storage.LoopRecord{ID: loopID, Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", TargetID: &targetID, Repo: &repo, PRNumber: &prNumber, Status: "running", CreatedAt: oldISO, UpdatedAt: oldISO}
	if err := repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	runningQueue := storage.QueueItemRecord{ID: "queue_superseded_running", ProjectID: stringPtr("project_1"), LoopID: &loopID, Type: "reviewer", TargetType: "pull_request", TargetID: targetID, Repo: &repo, PRNumber: &prNumber, DedupeKey: "reviewer:project_1:loop_superseded_latest_live:nexu-io/looper:195", Priority: storage.QueuePriorityReviewer, Status: "running", AvailableAt: oldISO, StartedAt: stringPtr(oldISO), MaxAttempts: 3, CreatedAt: oldISO, UpdatedAt: oldISO}
	if err := repos.Queue.Upsert(context.Background(), runningQueue); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}
	latestRun := &storage.RunRecord{ID: "run_superseded_latest_live", LoopID: loopID, Status: "running", CurrentStep: stringPtr("review"), StartedAt: nowISO, LastHeartbeatAt: stringPtr(nowISO), CreatedAt: nowISO, UpdatedAt: nowISO}

	summary, err := rt.repairStaleRunQueueState(context.Background(), repos, loop, latestRun, true, nowISO)
	if err != nil {
		t.Fatalf("repairStaleRunQueueState() error = %v", err)
	}
	if summary.LoopsRequeued != 0 || summary.QueueItemsRequeued != 0 || summary.QueueItemsCancelled != 1 {
		t.Fatalf("summary = %#v, want no requeue and one stale queue cancellation", summary)
	}
	repairedLoop, err := repos.Loops.GetByID(context.Background(), loopID)
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if repairedLoop == nil || repairedLoop.Status != "running" {
		t.Fatalf("loop = %#v, want loop to stay running under newer live run", repairedLoop)
	}
	repairedQueue, err := repos.Queue.GetByID(context.Background(), runningQueue.ID)
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	if repairedQueue == nil || repairedQueue.Status != "cancelled" {
		t.Fatalf("queue = %#v, want stale running queue item cancelled", repairedQueue)
	}
}

func TestRuntimeReconcileStaleRunningRunsWithMultipleActiveExecutions(t *testing.T) {
	t.Parallel()

	t.Run("verified live execution wins", func(t *testing.T) {
		workingDir := t.TempDir()
		cfg, err := config.DefaultConfig(workingDir)
		if err != nil {
			t.Fatalf("DefaultConfig() error = %v", err)
		}
		cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
		backupDir := filepath.Join(workingDir, "backups")
		cfg.Storage.BackupDir = &backupDir
		now := time.Date(2026, time.May, 20, 12, 0, 0, 0, time.UTC)
		nowISO := formatJavaScriptISOString(now)
		oldISO := formatJavaScriptISOString(now.Add(-2 * time.Hour))
		livePID := int64(5353)
		deadPID := int64(5354)

		rt := New(Options{Config: cfg, Logger: &testLogger{}, Now: func() time.Time { return now }, ReadProcessCommand: func(_ context.Context, pid int) (string, error) {
			switch pid {
			case int(livePID):
				return "codex exec", nil
			case int(deadPID):
				return "", nil
			default:
				return "", nil
			}
		}})
		if err := rt.Start(context.Background()); err != nil {
			t.Fatalf("Start() error = %v", err)
		}
		defer rt.Stop("test cleanup")
		repos := rt.Services().Repositories
		repo := "nexu-io/looper"
		prNumber := int64(190)
		targetID := "pr:nexu-io/looper:190"
		if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "project_1", Name: "Looper", RepoPath: filepath.Join(workingDir, "repo"), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
			t.Fatalf("Projects.Upsert() error = %v", err)
		}
		if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: "loop_multi_exec_live", Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", TargetID: &targetID, Repo: &repo, PRNumber: &prNumber, Status: "running", CreatedAt: oldISO, UpdatedAt: oldISO}); err != nil {
			t.Fatalf("Loops.Upsert() error = %v", err)
		}
		if err := repos.Runs.Upsert(context.Background(), storage.RunRecord{ID: "run_multi_exec_live", LoopID: "loop_multi_exec_live", Status: "running", CurrentStep: stringPtr("review"), StartedAt: oldISO, LastHeartbeatAt: stringPtr(oldISO), CreatedAt: oldISO, UpdatedAt: oldISO}); err != nil {
			t.Fatalf("Runs.Upsert() error = %v", err)
		}
		for _, exec := range []storage.AgentExecutionRecord{{ID: "exec_multi_live", ProjectID: stringPtr("project_1"), LoopID: stringPtr("loop_multi_exec_live"), RunID: stringPtr("run_multi_exec_live"), Vendor: "codex", Status: "running", PID: &livePID, CommandJSON: stringPtr(`{"command":"codex","args":["exec"]}`), CWD: stringPtr(workingDir), StartedAt: oldISO, CreatedAt: oldISO, UpdatedAt: oldISO}, {ID: "exec_multi_dead", ProjectID: stringPtr("project_1"), LoopID: stringPtr("loop_multi_exec_live"), RunID: stringPtr("run_multi_exec_live"), Vendor: "codex", Status: "running", PID: &deadPID, CommandJSON: stringPtr(`{"command":"codex","args":["exec"]}`), CWD: stringPtr(workingDir), StartedAt: oldISO, CreatedAt: oldISO, UpdatedAt: oldISO}} {
			if err := repos.AgentExecutions.Upsert(context.Background(), exec); err != nil {
				t.Fatalf("AgentExecutions.Upsert() error = %v", err)
			}
		}

		summary, err := rt.ReconcileStaleRunningRuns(context.Background())
		if err != nil {
			t.Fatalf("ReconcileStaleRunningRuns() error = %v", err)
		}
		if summary.CandidateRuns != 0 || summary.InterruptedRuns != 0 || summary.SkippedUncertainRuns != 0 {
			t.Fatalf("summary = %#v, want live execution to keep run active", summary)
		}
	})

	t.Run("ambiguous without verified live is uncertain", func(t *testing.T) {
		workingDir := t.TempDir()
		cfg, err := config.DefaultConfig(workingDir)
		if err != nil {
			t.Fatalf("DefaultConfig() error = %v", err)
		}
		cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
		backupDir := filepath.Join(workingDir, "backups")
		cfg.Storage.BackupDir = &backupDir
		now := time.Date(2026, time.May, 20, 12, 0, 0, 0, time.UTC)
		nowISO := formatJavaScriptISOString(now)
		oldISO := formatJavaScriptISOString(now.Add(-2 * time.Hour))
		ambiguousPID := int64(5453)
		deadPID := int64(5454)

		rt := New(Options{Config: cfg, Logger: &testLogger{}, Now: func() time.Time { return now }, ReadProcessCommand: func(_ context.Context, pid int) (string, error) {
			switch pid {
			case int(ambiguousPID):
				return "python unrelated.py", nil
			case int(deadPID):
				return "", nil
			default:
				return "", nil
			}
		}})
		if err := rt.Start(context.Background()); err != nil {
			t.Fatalf("Start() error = %v", err)
		}
		defer rt.Stop("test cleanup")
		repos := rt.Services().Repositories
		repo := "nexu-io/looper"
		prNumber := int64(191)
		targetID := "pr:nexu-io/looper:191"
		if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "project_1", Name: "Looper", RepoPath: filepath.Join(workingDir, "repo"), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
			t.Fatalf("Projects.Upsert() error = %v", err)
		}
		if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: "loop_multi_exec_uncertain", Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", TargetID: &targetID, Repo: &repo, PRNumber: &prNumber, Status: "running", CreatedAt: oldISO, UpdatedAt: oldISO}); err != nil {
			t.Fatalf("Loops.Upsert() error = %v", err)
		}
		if err := repos.Runs.Upsert(context.Background(), storage.RunRecord{ID: "run_multi_exec_uncertain", LoopID: "loop_multi_exec_uncertain", Status: "running", CurrentStep: stringPtr("review"), StartedAt: oldISO, LastHeartbeatAt: stringPtr(oldISO), CreatedAt: oldISO, UpdatedAt: oldISO}); err != nil {
			t.Fatalf("Runs.Upsert() error = %v", err)
		}
		for _, exec := range []storage.AgentExecutionRecord{{ID: "exec_multi_ambiguous", ProjectID: stringPtr("project_1"), LoopID: stringPtr("loop_multi_exec_uncertain"), RunID: stringPtr("run_multi_exec_uncertain"), Vendor: "codex", Status: "running", PID: &ambiguousPID, CommandJSON: stringPtr(`{"command":"codex","args":["exec"]}`), CWD: stringPtr(workingDir), StartedAt: oldISO, CreatedAt: oldISO, UpdatedAt: oldISO}, {ID: "exec_multi_dead_2", ProjectID: stringPtr("project_1"), LoopID: stringPtr("loop_multi_exec_uncertain"), RunID: stringPtr("run_multi_exec_uncertain"), Vendor: "codex", Status: "running", PID: &deadPID, CommandJSON: stringPtr(`{"command":"codex","args":["exec"]}`), CWD: stringPtr(workingDir), StartedAt: oldISO, CreatedAt: oldISO, UpdatedAt: oldISO}} {
			if err := repos.AgentExecutions.Upsert(context.Background(), exec); err != nil {
				t.Fatalf("AgentExecutions.Upsert() error = %v", err)
			}
		}

		summary, err := rt.ReconcileStaleRunningRuns(context.Background())
		if err != nil {
			t.Fatalf("ReconcileStaleRunningRuns() error = %v", err)
		}
		if summary.CandidateRuns != 1 || summary.SkippedUncertainRuns != 1 || summary.InterruptedRuns != 0 {
			t.Fatalf("summary = %#v, want uncertain candidate preserved", summary)
		}
	})
}

func TestRuntimeReconcileStaleRunningRunsCancelsDuplicateActiveQueueItems(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	backupDir := filepath.Join(workingDir, "backups")
	cfg.Storage.BackupDir = &backupDir
	now := time.Date(2026, time.May, 20, 12, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	oldISO := formatJavaScriptISOString(now.Add(-2 * time.Hour))

	rt := New(Options{Config: cfg, Logger: &testLogger{}, Now: func() time.Time { return now }})
	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer rt.Stop("test cleanup")
	repos := rt.Services().Repositories
	repo := "nexu-io/looper"
	prNumber := int64(192)
	targetID := "pr:nexu-io/looper:192"
	loopID := "loop_duplicate_active_queue"
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "project_1", Name: "Looper", RepoPath: filepath.Join(workingDir, "repo"), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 1, ProjectID: "project_1", Type: "fixer", TargetType: "pull_request", TargetID: &targetID, Repo: &repo, PRNumber: &prNumber, Status: "running", CreatedAt: oldISO, UpdatedAt: oldISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	if err := repos.Runs.Upsert(context.Background(), storage.RunRecord{ID: "run_duplicate_active_queue", LoopID: loopID, Status: "running", CurrentStep: stringPtr("repair"), StartedAt: oldISO, LastHeartbeatAt: stringPtr(oldISO), CreatedAt: oldISO, UpdatedAt: oldISO}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}
	for _, item := range []storage.QueueItemRecord{{ID: "queue_duplicate_1", ProjectID: stringPtr("project_1"), LoopID: &loopID, Type: "fixer", TargetType: "pull_request", TargetID: targetID, Repo: &repo, PRNumber: &prNumber, DedupeKey: "fixer:project_1:loop_duplicate_active_queue:nexu-io/looper:192:a", Priority: storage.QueuePriorityFixer, Status: "running", AvailableAt: oldISO, StartedAt: stringPtr(oldISO), MaxAttempts: 3, CreatedAt: oldISO, UpdatedAt: oldISO}, {ID: "queue_duplicate_2", ProjectID: stringPtr("project_1"), LoopID: &loopID, Type: "fixer", TargetType: "pull_request", TargetID: targetID, Repo: &repo, PRNumber: &prNumber, DedupeKey: "fixer:project_1:loop_duplicate_active_queue:nexu-io/looper:192:b", Priority: storage.QueuePriorityFixer, Status: "queued", AvailableAt: oldISO, MaxAttempts: 3, CreatedAt: oldISO, UpdatedAt: oldISO}} {
		if err := repos.Queue.Upsert(context.Background(), item); err != nil {
			t.Fatalf("Queue.Upsert(%s) error = %v", item.ID, err)
		}
	}

	summary, err := rt.ReconcileStaleRunningRuns(context.Background())
	if err != nil {
		t.Fatalf("ReconcileStaleRunningRuns() error = %v", err)
	}
	if summary.InterruptedRuns != 1 || summary.LoopsRequeued != 1 {
		t.Fatalf("summary = %#v, want stale run reconciled", summary)
	}
	activeCount, err := repos.Queue.CountActiveByLoopID(context.Background(), loopID)
	if err != nil {
		t.Fatalf("CountActiveByLoopID() error = %v", err)
	}
	if activeCount > 1 {
		t.Fatalf("active queue count = %d, want at most 1", activeCount)
	}
}

func TestRuntimeReconcileStaleRunningRunsIsIdempotent(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		reconcile func(*Runtime, context.Context) (StaleRunReconcileSummary, error)
	}{
		{name: "live", reconcile: func(rt *Runtime, ctx context.Context) (StaleRunReconcileSummary, error) {
			return rt.reconcileLiveStaleRunningRuns(ctx)
		}},
		{name: "manual", reconcile: func(rt *Runtime, ctx context.Context) (StaleRunReconcileSummary, error) {
			return rt.ReconcileStaleRunningRuns(ctx)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workingDir := t.TempDir()
			cfg, err := config.DefaultConfig(workingDir)
			if err != nil {
				t.Fatalf("DefaultConfig() error = %v", err)
			}
			cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
			backupDir := filepath.Join(workingDir, "backups")
			cfg.Storage.BackupDir = &backupDir
			now := time.Date(2026, time.May, 20, 12, 0, 0, 0, time.UTC)
			nowISO := formatJavaScriptISOString(now)
			oldISO := formatJavaScriptISOString(now.Add(-2 * time.Hour))

			rt := New(Options{Config: cfg, Logger: &testLogger{}, Now: func() time.Time { return now }})
			if err := rt.Start(context.Background()); err != nil {
				t.Fatalf("Start() error = %v", err)
			}
			defer rt.Stop("test cleanup")
			repos := rt.Services().Repositories
			repo := "nexu-io/looper"
			prNumber := int64(193)
			targetID := "pr:nexu-io/looper:193"
			loopID := "loop_idempotent_reconcile"
			if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "project_1", Name: "Looper", RepoPath: filepath.Join(workingDir, "repo"), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
				t.Fatalf("Projects.Upsert() error = %v", err)
			}
			if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 1, ProjectID: "project_1", Type: "fixer", TargetType: "pull_request", TargetID: &targetID, Repo: &repo, PRNumber: &prNumber, Status: "running", CreatedAt: oldISO, UpdatedAt: oldISO}); err != nil {
				t.Fatalf("Loops.Upsert() error = %v", err)
			}
			if err := repos.Runs.Upsert(context.Background(), storage.RunRecord{ID: "run_idempotent_reconcile", LoopID: loopID, Status: "running", CurrentStep: stringPtr("repair"), StartedAt: oldISO, LastHeartbeatAt: stringPtr(oldISO), CreatedAt: oldISO, UpdatedAt: oldISO}); err != nil {
				t.Fatalf("Runs.Upsert() error = %v", err)
			}

			first, err := tc.reconcile(rt, context.Background())
			if err != nil {
				t.Fatalf("first reconcile error = %v", err)
			}
			if first.InterruptedRuns != 1 || first.LoopsRequeued != 1 {
				t.Fatalf("first summary = %#v, want first pass to reconcile stale run", first)
			}
			second, err := tc.reconcile(rt, context.Background())
			if err != nil {
				t.Fatalf("second reconcile error = %v", err)
			}
			if second.InterruptedRuns != 0 || second.LoopsRequeued != 0 || second.QueueItemsRequeued != 0 || second.QueueItemsCancelled != 0 {
				t.Fatalf("second summary = %#v, want idempotent no-op", second)
			}
		})
	}
}

func TestRuntimeReconcileStaleRunningRunsDedupesUncertainIdentityEvents(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	backupDir := filepath.Join(workingDir, "backups")
	cfg.Storage.BackupDir = &backupDir
	now := time.Date(2026, time.May, 20, 12, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	oldISO := formatJavaScriptISOString(now.Add(-2 * time.Hour))
	ambiguousPID := int64(5651)

	rt := New(Options{Config: cfg, Logger: &testLogger{}, Now: func() time.Time { return now }, ReadProcessCommand: func(context.Context, int) (string, error) { return "python unrelated.py", nil }})
	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer rt.Stop("test cleanup")
	repos := rt.Services().Repositories
	repo := "nexu-io/looper"
	prNumber := int64(196)
	targetID := "pr:nexu-io/looper:196"
	loopID := "loop_uncertain_event_dedupe"
	runID := "run_uncertain_event_dedupe"
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "project_1", Name: "Looper", RepoPath: filepath.Join(workingDir, "repo"), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", TargetID: &targetID, Repo: &repo, PRNumber: &prNumber, Status: "running", CreatedAt: oldISO, UpdatedAt: oldISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	if err := repos.Runs.Upsert(context.Background(), storage.RunRecord{ID: runID, LoopID: loopID, Status: "running", CurrentStep: stringPtr("review"), StartedAt: oldISO, LastHeartbeatAt: stringPtr(oldISO), CreatedAt: oldISO, UpdatedAt: oldISO}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}
	if err := repos.AgentExecutions.Upsert(context.Background(), storage.AgentExecutionRecord{ID: "exec_uncertain_event_dedupe", ProjectID: stringPtr("project_1"), LoopID: stringPtr(loopID), RunID: stringPtr(runID), Vendor: "codex", Status: "running", PID: &ambiguousPID, CommandJSON: stringPtr(`{"command":"codex","args":["exec"]}`), CWD: stringPtr(workingDir), StartedAt: oldISO, CreatedAt: oldISO, UpdatedAt: oldISO}); err != nil {
		t.Fatalf("AgentExecutions.Upsert() error = %v", err)
	}

	first, err := rt.reconcileLiveStaleRunningRuns(context.Background())
	if err != nil {
		t.Fatalf("first reconcile error = %v", err)
	}
	second, err := rt.reconcileLiveStaleRunningRuns(context.Background())
	if err != nil {
		t.Fatalf("second reconcile error = %v", err)
	}
	if first.SkippedUncertainRuns != 1 || first.EventsWritten != 1 {
		t.Fatalf("first summary = %#v, want one uncertain event written", first)
	}
	if second.SkippedUncertainRuns != 1 || second.EventsWritten != 0 {
		t.Fatalf("second summary = %#v, want deduped uncertain event without new writes", second)
	}
	events, err := repos.Events.ListByEntity(context.Background(), "agent_execution", "exec_uncertain_event_dedupe")
	if err != nil {
		t.Fatalf("Events.ListByEntity() error = %v", err)
	}
	count := 0
	for _, event := range events {
		if event.EventType == "looperd.recovery.process_identity_uncertain" {
			count += 1
		}
	}
	if count != 1 {
		t.Fatalf("uncertain event count = %d, want 1; events = %#v", count, events)
	}
}

func TestRuntimeReconcileStaleRunningRunsRepairsInterruptedLoopQueueOnLaterPass(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	backupDir := filepath.Join(workingDir, "backups")
	cfg.Storage.BackupDir = &backupDir
	now := time.Date(2026, time.May, 20, 12, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	oldISO := formatJavaScriptISOString(now.Add(-2 * time.Hour))

	rt := New(Options{Config: cfg, Logger: &testLogger{}, Now: func() time.Time { return now }})
	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer rt.Stop("test cleanup")
	repos := rt.Services().Repositories
	repo := "nexu-io/looper"
	prNumber := int64(194)
	targetID := "pr:nexu-io/looper:194"
	loopID := "loop_interrupted_queue_repair"
	queueID := "queue_interrupted_queue_repair"
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "project_1", Name: "Looper", RepoPath: filepath.Join(workingDir, "repo"), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 1, ProjectID: "project_1", Type: "fixer", TargetType: "pull_request", TargetID: &targetID, Repo: &repo, PRNumber: &prNumber, Status: "running", CreatedAt: oldISO, UpdatedAt: oldISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	if err := repos.Runs.Upsert(context.Background(), storage.RunRecord{ID: "run_interrupted_queue_repair", LoopID: loopID, Status: "interrupted", CurrentStep: stringPtr("repair"), StartedAt: oldISO, EndedAt: stringPtr(nowISO), CreatedAt: oldISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}
	if err := repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: queueID, ProjectID: stringPtr("project_1"), LoopID: &loopID, Type: "fixer", TargetType: "pull_request", TargetID: targetID, Repo: &repo, PRNumber: &prNumber, DedupeKey: "fixer:project_1:loop_interrupted_queue_repair:nexu-io/looper:194", Priority: storage.QueuePriorityFixer, Status: "running", AvailableAt: oldISO, StartedAt: stringPtr(oldISO), MaxAttempts: 3, CreatedAt: oldISO, UpdatedAt: oldISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	summary, err := rt.ReconcileStaleRunningRuns(context.Background())
	if err != nil {
		t.Fatalf("ReconcileStaleRunningRuns() error = %v", err)
	}
	if summary.LoopsRequeued != 1 || summary.QueueItemsRequeued != 1 {
		t.Fatalf("summary = %#v, want later pass to repair interrupted loop queue", summary)
	}
	loop, _ := repos.Loops.GetByID(context.Background(), loopID)
	queue, _ := repos.Queue.GetByID(context.Background(), queueID)
	if loop == nil || loop.Status != "queued" {
		t.Fatalf("loop = %#v, want queued", loop)
	}
	if queue == nil || queue.Status != "queued" {
		t.Fatalf("queue = %#v, want queued", queue)
	}
}

func TestRuntimeReconcileStaleRunningRunsRecreatesQueuedItemForInterruptedQueuedLoopWithoutActiveQueue(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	backupDir := filepath.Join(workingDir, "backups")
	cfg.Storage.BackupDir = &backupDir
	now := time.Date(2026, time.May, 20, 12, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	oldISO := formatJavaScriptISOString(now.Add(-2 * time.Hour))

	rt := New(Options{Config: cfg, Logger: &testLogger{}, Now: func() time.Time { return now }})
	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer rt.Stop("test cleanup")
	repos := rt.Services().Repositories
	repo := "nexu-io/looper"
	prNumber := int64(195)
	targetID := "pr:nexu-io/looper:195"
	loopID := "loop_interrupted_needs_queue_recreate"
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "project_1", Name: "Looper", RepoPath: filepath.Join(workingDir, "repo"), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 1, ProjectID: "project_1", Type: "fixer", TargetType: "pull_request", TargetID: &targetID, Repo: &repo, PRNumber: &prNumber, Status: "queued", CreatedAt: oldISO, UpdatedAt: oldISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	if err := repos.Runs.Upsert(context.Background(), storage.RunRecord{ID: "run_interrupted_needs_queue_recreate", LoopID: loopID, Status: "interrupted", CurrentStep: stringPtr("repair"), StartedAt: oldISO, EndedAt: stringPtr(nowISO), CreatedAt: oldISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}

	summary, err := rt.ReconcileStaleRunningRuns(context.Background())
	if err != nil {
		t.Fatalf("ReconcileStaleRunningRuns() error = %v", err)
	}
	if summary.LoopsRequeued != 1 || summary.QueueItemsRequeued != 1 {
		t.Fatalf("summary = %#v, want one queued item recreated", summary)
	}
	activeCount, err := repos.Queue.CountActiveByLoopID(context.Background(), loopID)
	if err != nil {
		t.Fatalf("CountActiveByLoopID() error = %v", err)
	}
	if activeCount != 1 {
		t.Fatalf("active queue count = %d, want 1", activeCount)
	}
	items, err := repos.Queue.List(context.Background())
	if err != nil {
		t.Fatalf("Queue.List() error = %v", err)
	}
	matched := 0
	for _, item := range items {
		if item.LoopID != nil && *item.LoopID == loopID && (item.Status == "queued" || item.Status == "running") {
			matched++
		}
	}
	if matched != 1 {
		t.Fatalf("active queue items for loop = %d, want exactly 1: %#v", matched, items)
	}
}

func TestRuntimeStartBeginsSchedulerPolling(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}

	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	cfg.Scheduler.PollIntervalSeconds = 1
	var tickCount int32
	rt := New(Options{
		Config: cfg,
		Logger: &testLogger{},
		RunSchedulerTick: func(context.Context, Services) error {
			atomic.AddInt32(&tickCount, 1)
			return nil
		},
	})

	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { rt.Stop("test cleanup") })

	waitForCondition(t, 2500*time.Millisecond, func() bool {
		return atomic.LoadInt32(&tickCount) >= 2
	})
	if got := atomic.LoadInt32(&tickCount); got < 2 {
		t.Fatalf("scheduler tick count = %d, want immediate tick plus polling tick", got)
	}
}

func TestRuntimeTriggerSchedulerTickRunsImmediatelyWithoutWaitingForPolling(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}

	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	cfg.Scheduler.PollIntervalSeconds = 3600
	var tickCount int32
	rt := New(Options{
		Config: cfg,
		Logger: &testLogger{},
		RunSchedulerTick: func(context.Context, Services) error {
			atomic.AddInt32(&tickCount, 1)
			return nil
		},
	})

	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { rt.Stop("test cleanup") })

	waitForCondition(t, time.Second, func() bool {
		return atomic.LoadInt32(&tickCount) >= 1
	})

	rt.TriggerSchedulerTick()

	waitForCondition(t, time.Second, func() bool {
		return atomic.LoadInt32(&tickCount) >= 2
	})
	if got := atomic.LoadInt32(&tickCount); got < 2 {
		t.Fatalf("scheduler tick count = %d, want immediate startup tick plus triggered tick", got)
	}
}

func TestRuntimeTriggerSchedulerTickCoalescesWhileTickIsRunning(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}

	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	cfg.Scheduler.PollIntervalSeconds = 3600
	startCh := make(chan struct{}, 4)
	releaseCh := make(chan struct{})
	var tickCount int32
	rt := New(Options{
		Config: cfg,
		Logger: &testLogger{},
		RunSchedulerTick: func(context.Context, Services) error {
			atomic.AddInt32(&tickCount, 1)
			startCh <- struct{}{}
			<-releaseCh
			return nil
		},
	})

	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	var releaseOnce sync.Once
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(releaseCh) })
		rt.Stop("test cleanup")
	})

	select {
	case <-startCh:
	case <-time.After(time.Second):
		t.Fatal("initial scheduler tick did not start")
	}

	rt.TriggerSchedulerTick()
	rt.TriggerSchedulerTick()
	releaseOnce.Do(func() { close(releaseCh) })

	select {
	case <-startCh:
	case <-time.After(time.Second):
		t.Fatal("triggered scheduler tick did not start after in-flight tick completed")
	}

	select {
	case <-startCh:
		t.Fatal("unexpected extra scheduler tick after coalesced trigger")
	case <-time.After(150 * time.Millisecond):
	}
	if got := atomic.LoadInt32(&tickCount); got != 2 {
		t.Fatalf("scheduler tick count = %d, want coalesced triggered rerun", got)
	}
}

func TestRuntimeTriggerSchedulerClaimRunsImmediatelyWithoutWaitingForPolling(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Scheduler.PollIntervalSeconds = 3600

	var tickCount int32
	var claimCount int32
	rt := &Runtime{
		config:                cfg,
		logger:                &testLogger{},
		runSchedulerTick:      func(context.Context, Services) error { atomic.AddInt32(&tickCount, 1); return nil },
		defaultSchedulerClaim: func(context.Context, Services) error { atomic.AddInt32(&claimCount, 1); return nil },
		services:              Services{Repositories: &storage.Repositories{}},
		shutdownTimeout:       time.Second,
	}

	rt.startSchedulerLoop()
	t.Cleanup(func() { rt.stopSchedulerLoop() })

	waitForCondition(t, time.Second, func() bool {
		return atomic.LoadInt32(&claimCount) >= 1 && atomic.LoadInt32(&tickCount) >= 1
	})

	rt.TriggerSchedulerClaim()

	waitForCondition(t, time.Second, func() bool {
		return atomic.LoadInt32(&claimCount) >= 2
	})
	if got := atomic.LoadInt32(&tickCount); got != 1 {
		t.Fatalf("scheduler tick count = %d, want claim wake without extra discovery tick", got)
	}
}

func TestRuntimeRecordWebhookDeliveryTriggersSchedulerTick(t *testing.T) {
	t.Parallel()

	wakeCh := make(chan struct{}, 1)
	claimWakeCh := make(chan struct{}, 1)
	rt := &Runtime{
		schedulerWake:      wakeCh,
		schedulerClaimWake: claimWakeCh,
		webhook: &webhookRuntime{
			now:    func() time.Time { return time.Date(2026, time.May, 16, 12, 0, 0, 0, time.UTC) },
			status: WebhookStatus{RecentOutcomes: []WebhookRecentOutcome{}},
		},
	}

	rt.RecordWebhookDelivery("pull_request", "delivery-1")

	select {
	case <-wakeCh:
	default:
		t.Fatal("schedulerWake was not triggered")
	}
	select {
	case <-claimWakeCh:
	default:
		t.Fatal("schedulerClaimWake was not triggered")
	}
	status := rt.webhook.Status()
	if status.Counters.DeliveriesReceived != 1 {
		t.Fatalf("Status().Counters.DeliveriesReceived = %d, want 1", status.Counters.DeliveriesReceived)
	}
}

func TestRuntimeWebhookStatusMergesForwarderStats(t *testing.T) {
	t.Parallel()

	rt := &Runtime{
		webhook: &webhookRuntime{status: WebhookStatus{Enabled: true, RecentOutcomes: []WebhookRecentOutcome{{At: "2026-05-16T12:00:00.000Z", Outcome: "acknowledged", Message: "stale"}}}},
		webhookForwarder: stubRuntimeWebhookForwarder{stats: webhookforward.Stats{
			DeliveriesReceived:  7,
			DeliveriesDeduped:   1,
			DeliveriesIgnored:   2,
			QueueCapacity:       128,
			QueueEnqueued:       4,
			QueueCoalesced:      3,
			QueueRejected:       1,
			ExecutionsSucceeded: 5,
			ExecutionsFailed:    2,
			InFlight:            2,
			Queued:              6,
			RecentOutcomes: []webhookforward.Outcome{{
				At:         "2026-05-16T12:01:00.000Z",
				Repo:       "nexu-io/looper",
				ObjectType: "pull_request",
				Number:     379,
				EventType:  "pull_request",
				Status:     "completed",
			}},
		}},
	}

	status := rt.WebhookStatus()
	if status.Queue.Pending != 6 || status.Queue.Capacity != 128 || status.Queue.ActiveWorkers != 2 {
		t.Fatalf("Status().Queue = %#v, want pending=6 capacity=128 activeWorkers=2", status.Queue)
	}
	if status.Counters.DeliveriesReceived != 7 || status.Counters.Coalesced != 3 || status.Counters.Dropped != 4 || status.Counters.Queued != 4 || status.Counters.Processed != 5 || status.Counters.Failed != 2 {
		t.Fatalf("Status().Counters = %#v, want merged forwarder counters", status.Counters)
	}
	if len(status.RecentOutcomes) != 1 {
		t.Fatalf("len(Status().RecentOutcomes) = %d, want 1", len(status.RecentOutcomes))
	}
	if status.RecentOutcomes[0].Outcome != "completed" || status.RecentOutcomes[0].Message != "nexu-io/looper · pull_request #379 · pull_request" {
		t.Fatalf("Status().RecentOutcomes[0] = %#v, want merged forwarder outcome", status.RecentOutcomes[0])
	}
}

type stubRuntimeWebhookForwarder struct{ stats webhookforward.Stats }

func (s stubRuntimeWebhookForwarder) Forward(context.Context, webhookforward.DeliveryRequest) (webhookforward.ForwardResult, error) {
	return webhookforward.ForwardResult{}, nil
}

func (s stubRuntimeWebhookForwarder) Stats() webhookforward.Stats { return s.stats }

func (stubRuntimeWebhookForwarder) Close() {}

func TestRuntimeSchedulerPollIntervalUsesWebhookFallbackWhenEnabled(t *testing.T) {
	t.Parallel()

	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Scheduler.PollIntervalSeconds = 30
	cfg.Webhook.Enabled = true
	cfg.Webhook.FallbackPollIntervalSeconds = 90

	if got := schedulerFullPollInterval(cfg); got != 90*time.Second {
		t.Fatalf("schedulerFullPollInterval() = %s, want 90s", got)
	}

	cfg.Webhook.Enabled = false
	if got := schedulerFullPollInterval(cfg); got != 30*time.Second {
		t.Fatalf("schedulerFullPollInterval() = %s, want 30s when webhook disabled", got)
	}
}
func TestRuntimeStopClosesCoordinatorAndUnblocksWaitForShutdown(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}

	cfg.Storage.DBPath = workingDir + "/runtime.sqlite"

	rt := New(Options{Config: cfg, Logger: &testLogger{}})
	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		rt.WaitForShutdown()
	}()

	rt.Stop("SIGTERM")
	rt.Stop("SIGTERM")

	select {
	case <-shutdownDone:
	case <-time.After(2 * time.Second):
		t.Fatal("WaitForShutdown() did not return after Stop()")
	}

	services := rt.Services()
	if services.Coordinator != nil {
		t.Fatal("Services().Coordinator != nil after Stop(), want nil")
	}
	if services.Repositories != nil {
		t.Fatal("Services().Repositories != nil after Stop(), want nil")
	}
	db, err := sql.Open(storage.DriverName, cfg.Storage.DBPath)
	if err != nil {
		t.Fatalf("sql.Open() after Stop() error = %v", err)
	}
	defer db.Close()
}

func TestRuntimeStopTimesOutWaitingForSchedulerLoop(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}

	cfg.Storage.DBPath = workingDir + "/runtime.sqlite"
	cfg.Daemon.ShutdownTimeoutMS = 25
	blockCh := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(blockCh) })
	defer unblock()
	logger := &testLogger{}
	rt := New(Options{
		Config: cfg,
		Logger: logger,
		RunSchedulerTick: func(context.Context, Services) error {
			<-blockCh
			return nil
		},
	})
	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	rt.mu.RLock()
	doneCh := rt.schedulerDone
	rt.mu.RUnlock()
	if doneCh == nil {
		t.Fatal("schedulerDone = nil, want running scheduler loop")
	}

	started := time.Now()
	rt.stopSchedulerLoop()
	elapsed := time.Since(started)
	if elapsed > 250*time.Millisecond {
		t.Fatalf("stopSchedulerLoop() elapsed = %v, want timeout-bounded shutdown", elapsed)
	}
	if !logger.containsMessage("looperd stop timed out waiting for scheduler loop") {
		t.Fatalf("logger entries = %#v, want scheduler timeout warning", logger.messages())
	}

	unblock()
	select {
	case <-doneCh:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("scheduler loop did not exit after unblocking")
	}
	rt.Stop("SIGTERM")
}

func TestRuntimeStopSchedulerLoopKeepsTaskTrackerUntilLoopStops(t *testing.T) {
	t.Parallel()

	stopCh := make(chan struct{})
	doneCh := make(chan struct{})
	cancelCalled := make(chan struct{}, 1)
	taskTracker := &schedulerTaskTracker{}
	rt := &Runtime{shutdownTimeout: time.Second}
	rt.schedulerStop = stopCh
	rt.schedulerDone = doneCh
	rt.schedulerTasks = taskTracker
	rt.schedulerCancel = func() {
		select {
		case cancelCalled <- struct{}{}:
		default:
		}
	}

	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		rt.stopSchedulerLoop()
	}()

	select {
	case <-cancelCalled:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("stopSchedulerLoop() did not invoke cancel")
	}

	rt.mu.RLock()
	currentTracker := rt.schedulerTasks
	rt.mu.RUnlock()
	if currentTracker != taskTracker {
		t.Fatal("schedulerTasks cleared before scheduler loop stopped")
	}

	close(doneCh)

	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("stopSchedulerLoop() did not return after scheduler loop stopped")
	}

	rt.mu.RLock()
	defer rt.mu.RUnlock()
	if rt.schedulerTasks != nil {
		t.Fatal("schedulerTasks not cleared after scheduler loop stopped")
	}
}

func TestRuntimeStartDoesNotStartSchedulerWhenWebhookStartupFails(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	backupDir := t.TempDir()
	repoPath := filepath.Join(workingDir, "repo")
	vendor := config.AgentVendorOpenCode
	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	cfg.Storage.BackupDir = &backupDir
	cfg.Agent.Vendor = &vendor
	cfg.Webhook.Enabled = true
	cfg.Webhook.Mode = config.WebhookModeTunnel
	cfg.Projects = []config.ProjectRefConfig{{ID: "project_1", Name: "Looper", RepoPath: repoPath}}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}
	defer listener.Close()
	cfg.Webhook.ListenPort = listener.Addr().(*net.TCPAddr).Port

	coordinator, err := storage.OpenSQLiteCoordinator(context.Background(), cfg.Storage.DBPath, storage.SQLiteCoordinatorOptions{})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	if _, err := coordinator.MigrationRunner().RunPending(context.Background()); err != nil {
		_ = coordinator.Close()
		t.Fatalf("RunPending() error = %v", err)
	}
	repositories := storage.NewRepositories(coordinator.DB())
	metadata := `{"repo":"acme/looper","source":"config"}`
	nowISO := "2026-05-19T12:00:00.000Z"
	if err := repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "project_1", Name: "Looper", RepoPath: repoPath, MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		_ = coordinator.Close()
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := coordinator.Close(); err != nil {
		t.Fatalf("coordinator.Close() error = %v", err)
	}

	rt := New(Options{
		Config: cfg,
		Logger: &testLogger{},
		Now: func() time.Time {
			return time.Date(2026, time.May, 19, 12, 0, 0, 0, time.UTC)
		},
		RunSchedulerTick: func(context.Context, Services) error { return nil },
	})
	defer rt.Stop("test cleanup")

	err = rt.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "webhook tunnel listener failed") {
		t.Fatalf("Start() error = %v, want tunnel listener startup failure", err)
	}

	rt.mu.RLock()
	defer rt.mu.RUnlock()
	if rt.schedulerDone != nil {
		t.Fatal("scheduler started despite webhook startup failure")
	}
}

func TestDefaultSyncConfiguredProjectsPreservesRepoMetadataWhenRepoPathIsUnchanged(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}

	cfg.Storage.DBPath = workingDir + "/runtime.sqlite"
	coordinator, err := storage.OpenSQLiteCoordinator(context.Background(), cfg.Storage.DBPath, storage.SQLiteCoordinatorOptions{})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	defer coordinator.Close()

	if _, err := coordinator.MigrationRunner().RunPending(context.Background()); err != nil {
		t.Fatalf("RunPending() error = %v", err)
	}

	repositories := storage.NewRepositories(coordinator.DB())
	repoPath := workingDir + "/repo"
	existingMetadata := `{"repo":"nexu-io/looper","worktreeRoot":"/tmp/old","source":"config"}`
	baseBranch := "main"
	if err := repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID:           "project_1",
		Name:         "Looper",
		RepoPath:     repoPath,
		BaseBranch:   &baseBranch,
		Archived:     false,
		MetadataJSON: &existingMetadata,
		CreatedAt:    "2026-04-11T12:00:00.000Z",
		UpdatedAt:    "2026-04-11T12:00:00.000Z",
	}); err != nil {
		t.Fatalf("Projects.Upsert() seed error = %v", err)
	}

	cfg.Projects = []config.ProjectRefConfig{{
		ID:       "project_1",
		Name:     "Looper",
		RepoPath: repoPath,
	}}

	now := time.Date(2026, time.April, 17, 12, 0, 0, 0, time.UTC)
	service := &projects.Service{Repos: repositories, Now: func() time.Time { return now }}
	if err := defaultSyncConfiguredProjects(context.Background(), service, cfg, now); err != nil {
		t.Fatalf("defaultSyncConfiguredProjects() error = %v", err)
	}

	project, err := repositories.Projects.GetByID(context.Background(), "project_1")
	if err != nil {
		t.Fatalf("Projects.GetByID() error = %v", err)
	}
	if project == nil || project.MetadataJSON == nil {
		t.Fatal("project metadata missing after sync")
	}
	const want = `{"repo":"nexu-io/looper","worktreeRoot":null,"source":"config"}`
	if *project.MetadataJSON != want {
		t.Fatalf("project.MetadataJSON = %q, want %q", *project.MetadataJSON, want)
	}
	if project.CreatedAt != "2026-04-11T12:00:00.000Z" {
		t.Fatalf("project.CreatedAt = %q, want preserved timestamp", project.CreatedAt)
	}
	if project.UpdatedAt != "2026-04-17T12:00:00.000Z" {
		t.Fatalf("project.UpdatedAt = %q, want sync timestamp", project.UpdatedAt)
	}
}

func TestRunRecoveryPipelineAutoRecoversFailedReviewerGuardrailLoop(t *testing.T) {
	t.Parallel()
	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Roles.Reviewer.Behavior.Loop.StopOnApproved = true
	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	coordinator, err := storage.OpenSQLiteCoordinator(context.Background(), cfg.Storage.DBPath, storage.SQLiteCoordinatorOptions{})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	defer coordinator.Close()
	if _, err := coordinator.MigrationRunner().RunPending(context.Background()); err != nil {
		t.Fatalf("RunPending() error = %v", err)
	}
	repositories := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 17, 12, 0, 0, 0, time.UTC)
	nowISO := "2026-04-17T12:00:00.000Z"
	if err := repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "project_1", Name: "Looper", RepoPath: filepath.Join(workingDir, "repo"), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	repo := "acme/looper"
	prNumber := int64(42)
	targetID := "pr:acme/looper:42"
	loopID := "loop_runtime_reviewer_recover"
	queueID := "queue_runtime_reviewer_recover"
	metadata := mustMarshalJSON(map[string]any{"loop": map[string]any{"enabled": true, "failureCount": 1, "consecutiveFailures": 1, "lastFailure": "PR head changed before publish"}})
	if err := repositories.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 165, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", TargetID: &targetID, Repo: &repo, PRNumber: &prNumber, Status: "failed", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	checkpoint := `{"resumePolicy":"restart_from_discover","detail":{"state":"OPEN","reviewDecision":"","labels":[]}}`
	errorMessage := "PR head changed before publish: expected old, got new"
	if err := repositories.Runs.Upsert(context.Background(), storage.RunRecord{ID: "run_runtime_reviewer_recover", LoopID: loopID, Status: "failed", CurrentStep: stringPtr("publish"), CheckpointJSON: &checkpoint, Summary: &errorMessage, ErrorMessage: &errorMessage, StartedAt: nowISO, EndedAt: &nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}
	queueKind := "retryable_after_resume"
	if err := repositories.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: queueID, ProjectID: stringPtr("project_1"), LoopID: &loopID, Type: "reviewer", TargetType: "pull_request", TargetID: targetID, Repo: &repo, PRNumber: &prNumber, DedupeKey: "reviewer:project_1:loop_runtime_reviewer_recover:acme/looper:42", Priority: storage.QueuePriorityReviewer, Status: "failed", AvailableAt: nowISO, Attempts: 3, MaxAttempts: 3, FinishedAt: &nowISO, LastError: &errorMessage, LastErrorKind: &queueKind, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}
	rt := New(Options{Config: cfg, Logger: &testLogger{}, Now: func() time.Time { return now }})
	summary, err := rt.runRecoveryPipeline(context.Background(), repositories, nil, now)
	if err != nil {
		t.Fatalf("runRecoveryPipeline() error = %v", err)
	}
	if summary.LoopsRequeued != 1 {
		t.Fatalf("LoopsRequeued = %d, want 1", summary.LoopsRequeued)
	}
	loop, _ := repositories.Loops.GetByID(context.Background(), loopID)
	queue, _ := repositories.Queue.GetByID(context.Background(), queueID)
	if loop == nil || loop.Status != "queued" || queue == nil || queue.Status != "queued" {
		t.Fatalf("loop=%#v queue=%#v, want recovered queued loop and queue", loop, queue)
	}
	summary, err = rt.runRecoveryPipeline(context.Background(), repositories, nil, now)
	if err != nil {
		t.Fatalf("second runRecoveryPipeline() error = %v", err)
	}
	queues, _ := repositories.Queue.List(context.Background())
	if summary.LoopsRequeued != 0 || len(queues) != 1 {
		t.Fatalf("second summary=%#v queues=%#v, want idempotent no duplicate", summary, queues)
	}
}

func TestRecoveryInterruptsOlderRunningRunWhenLatestCompleted(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Roles.Reviewer.Behavior.Loop.StopOnApproved = true
	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	coordinator, err := storage.OpenSQLiteCoordinator(context.Background(), cfg.Storage.DBPath, storage.SQLiteCoordinatorOptions{})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	defer coordinator.Close()
	if _, err := coordinator.MigrationRunner().RunPending(context.Background()); err != nil {
		t.Fatalf("RunPending() error = %v", err)
	}
	repositories := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 17, 12, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	oldISO := formatJavaScriptISOString(now.Add(-2 * time.Hour))
	completedISO := formatJavaScriptISOString(now.Add(-10 * time.Minute))
	if err := repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "project_1", Name: "Looper", RepoPath: filepath.Join(workingDir, "repo"), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	repo := "nexu-io/looper"
	prNumber := int64(184)
	targetID := "pr:nexu-io/looper:184"
	loopID := "loop_recovery_old_running"
	if err := repositories.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 184, ProjectID: "project_1", Type: "fixer", TargetType: "pull_request", TargetID: &targetID, Repo: &repo, PRNumber: &prNumber, Status: "completed", CreatedAt: oldISO, UpdatedAt: completedISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	if err := repositories.Runs.Upsert(context.Background(), storage.RunRecord{ID: "run_old_running", LoopID: loopID, Status: "running", CurrentStep: stringPtr("discover-pr"), StartedAt: oldISO, LastHeartbeatAt: &oldISO, CreatedAt: oldISO, UpdatedAt: oldISO}); err != nil {
		t.Fatalf("Runs.Upsert(old) error = %v", err)
	}
	if err := repositories.Runs.Upsert(context.Background(), storage.RunRecord{ID: "run_latest_success", LoopID: loopID, Status: "success", StartedAt: completedISO, EndedAt: &completedISO, CreatedAt: completedISO, UpdatedAt: completedISO}); err != nil {
		t.Fatalf("Runs.Upsert(latest) error = %v", err)
	}
	repositories.AgentExecutions = nil
	rt := New(Options{Config: cfg, Logger: &testLogger{}, Now: func() time.Time { return now }})
	summary, err := rt.runRecoveryPipeline(context.Background(), repositories, nil, now)
	if err != nil {
		t.Fatalf("runRecoveryPipeline() error = %v", err)
	}
	if summary.InterruptedRunsMarked != 1 {
		t.Fatalf("InterruptedRunsMarked = %d, want 1", summary.InterruptedRunsMarked)
	}
	run, _ := repositories.Runs.GetByID(context.Background(), "run_old_running")
	if run == nil || run.Status != "interrupted" || run.EndedAt == nil {
		t.Fatalf("old run = %#v, want interrupted with ended_at", run)
	}
}

func TestRecoveryInterruptsStaleLatestRunningRunWithoutActivity(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name           string
		loopStatus     string
		heartbeatTime  func(time.Time) string
		hasActiveQueue bool
	}{
		{name: "running stale heartbeat", loopStatus: "running", heartbeatTime: func(now time.Time) string { return formatJavaScriptISOString(now.Add(-2 * time.Hour)) }},
		{name: "paused fresh heartbeat", loopStatus: "paused", heartbeatTime: func(now time.Time) string { return formatJavaScriptISOString(now.Add(-5 * time.Minute)) }},
		{name: "queued fresh heartbeat", loopStatus: "queued", heartbeatTime: func(now time.Time) string { return formatJavaScriptISOString(now.Add(-5 * time.Minute)) }},
		{name: "paused fresh heartbeat with active queue", loopStatus: "paused", heartbeatTime: func(now time.Time) string { return formatJavaScriptISOString(now.Add(-5 * time.Minute)) }, hasActiveQueue: true},
		{name: "queued fresh heartbeat with active queue", loopStatus: "queued", heartbeatTime: func(now time.Time) string { return formatJavaScriptISOString(now.Add(-5 * time.Minute)) }, hasActiveQueue: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workingDir := t.TempDir()
			cfg, err := config.DefaultConfig(workingDir)
			if err != nil {
				t.Fatalf("DefaultConfig() error = %v", err)
			}
			cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
			coordinator, err := storage.OpenSQLiteCoordinator(context.Background(), cfg.Storage.DBPath, storage.SQLiteCoordinatorOptions{})
			if err != nil {
				t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
			}
			defer coordinator.Close()
			if _, err := coordinator.MigrationRunner().RunPending(context.Background()); err != nil {
				t.Fatalf("RunPending() error = %v", err)
			}
			repositories := storage.NewRepositories(coordinator.DB())
			now := time.Date(2026, time.April, 17, 12, 0, 0, 0, time.UTC)
			nowISO := formatJavaScriptISOString(now)
			oldISO := formatJavaScriptISOString(now.Add(-2 * time.Hour))
			heartbeatISO := tt.heartbeatTime(now)
			if err := repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "project_1", Name: "Looper", RepoPath: filepath.Join(workingDir, "repo"), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
				t.Fatalf("Projects.Upsert() error = %v", err)
			}
			repo := "nexu-io/looper"
			prNumber := int64(184)
			targetID := "pr:nexu-io/looper:184"
			loopID := "loop_recovery_stale_latest"
			if err := repositories.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 185, ProjectID: "project_1", Type: "fixer", TargetType: "pull_request", TargetID: &targetID, Repo: &repo, PRNumber: &prNumber, Status: tt.loopStatus, CreatedAt: oldISO, UpdatedAt: heartbeatISO}); err != nil {
				t.Fatalf("Loops.Upsert() error = %v", err)
			}
			if err := repositories.Runs.Upsert(context.Background(), storage.RunRecord{ID: "run_stale_latest", LoopID: loopID, Status: "running", CurrentStep: stringPtr("discover-pr"), StartedAt: oldISO, LastHeartbeatAt: &heartbeatISO, CreatedAt: oldISO, UpdatedAt: heartbeatISO}); err != nil {
				t.Fatalf("Runs.Upsert() error = %v", err)
			}
			if tt.hasActiveQueue {
				if err := repositories.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: "queue_stale_latest", ProjectID: stringPtr("project_1"), LoopID: &loopID, Type: "fixer", TargetType: "pull_request", TargetID: targetID, Repo: &repo, PRNumber: &prNumber, DedupeKey: "fixer:project_1:loop_recovery_stale_latest:nexu-io/looper:184", Priority: storage.QueuePriorityFixer, Status: "queued", AvailableAt: nowISO, Attempts: 0, MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
					t.Fatalf("Queue.Upsert() error = %v", err)
				}
			}
			rt := New(Options{Config: cfg, Logger: &testLogger{}, Now: func() time.Time { return now }})
			summary, err := rt.runRecoveryPipeline(context.Background(), repositories, nil, now)
			if err != nil {
				t.Fatalf("runRecoveryPipeline() error = %v", err)
			}
			if summary.InterruptedRunsMarked != 1 {
				t.Fatalf("InterruptedRunsMarked = %d, want 1", summary.InterruptedRunsMarked)
			}
			run, _ := repositories.Runs.GetByID(context.Background(), "run_stale_latest")
			if run == nil || run.Status != "interrupted" || run.EndedAt == nil {
				t.Fatalf("run = %#v, want interrupted with ended_at", run)
			}
		})
	}
}

func TestShouldAutoRecoverFailedReviewerLoopRefusesUnsafeStates(t *testing.T) {
	t.Parallel()
	errorKind := "retryable_after_resume"
	errorMessage := "PR head changed before publish: expected old, got new"
	step := "publish"
	checkpoint := func(detail string) *string {
		value := `{"resumePolicy":"restart_from_discover",` + detail + `}`
		return &value
	}
	baseLoop := storage.LoopRecord{ID: "loop_recover", Type: "reviewer", Status: "failed", MetadataJSON: stringPtr(`{"loop":{"consecutiveFailures":1}}`)}
	baseRun := storage.RunRecord{ID: "run_recover", LoopID: "loop_recover", Status: "failed", CurrentStep: &step, CheckpointJSON: checkpoint(`"detail":{"state":"OPEN","reviewDecision":"","labels":[]}`), Summary: &errorMessage, ErrorMessage: &errorMessage}
	baseQueue := storage.QueueItemRecord{ID: "queue_recover", LoopID: stringPtr("loop_recover"), Status: "failed", LastError: &errorMessage, LastErrorKind: &errorKind}
	defaultPolicy := runtimeReviewerRecoveryPolicy{stopOnApproved: true, stopOnReadyLabel: true}

	tests := []struct {
		name  string
		loop  storage.LoopRecord
		run   storage.RunRecord
		queue storage.QueueItemRecord
	}{
		{name: "manual intervention kind", loop: baseLoop, run: baseRun, queue: func() storage.QueueItemRecord {
			q := baseQueue
			kind := "manual_intervention"
			q.LastErrorKind = &kind
			return q
		}()},
		{name: "manual intervention resume policy", loop: baseLoop, run: func() storage.RunRecord {
			r := baseRun
			checkpointJSON := `{"resumePolicy":"manual_intervention","detail":{"state":"OPEN","reviewDecision":"","labels":[]}}`
			r.CheckpointJSON = &checkpointJSON
			return r
		}(), queue: baseQueue},
		{name: "closed checkpoint", loop: baseLoop, run: func() storage.RunRecord {
			r := baseRun
			r.CheckpointJSON = checkpoint(`"detail":{"state":"CLOSED","reviewDecision":"","labels":[]}`)
			return r
		}(), queue: baseQueue},
		{name: "draft checkpoint", loop: baseLoop, run: func() storage.RunRecord {
			r := baseRun
			r.CheckpointJSON = checkpoint(`"detail":{"state":"OPEN","isDraft":true,"reviewDecision":"","labels":[]}`)
			return r
		}(), queue: baseQueue},
		{name: "approved by current user on checkpoint head", loop: baseLoop, run: func() storage.RunRecord {
			r := baseRun
			r.CheckpointJSON = checkpoint(`"detail":{"state":"OPEN","headSha":"abc123","currentLogin":"octocat","reviews":[{"author":{"login":"octocat"},"state":"APPROVED","commit":{"oid":"abc123"}}],"labels":[]}`)
			return r
		}(), queue: baseQueue},
		{name: "approved decision before current login checkpoint", loop: baseLoop, run: func() storage.RunRecord {
			r := baseRun
			r.CheckpointJSON = checkpoint(`"detail":{"state":"OPEN","headSha":"abc123","reviewDecision":"APPROVED","reviews":[{"author":{"login":"octocat"},"state":"APPROVED","commit":{"oid":"abc123"}}],"labels":[]}`)
			return r
		}(), queue: baseQueue},
		{name: "follow updates disabled", loop: func() storage.LoopRecord {
			l := baseLoop
			l.MetadataJSON = stringPtr(`{"followUpdates":false,"loop":{"enabled":true,"consecutiveFailures":1}}`)
			return l
		}(), run: baseRun, queue: baseQueue},
		{name: "loop disabled", loop: func() storage.LoopRecord {
			l := baseLoop
			l.MetadataJSON = stringPtr(`{"loop":{"enabled":false,"consecutiveFailures":1}}`)
			return l
		}(), run: baseRun, queue: baseQueue},
		{name: "ready label checkpoint", loop: baseLoop, run: func() storage.RunRecord {
			r := baseRun
			r.CheckpointJSON = checkpoint(`"detail":{"state":"OPEN","reviewDecision":"","labels":["looper:spec-ready"]}`)
			return r
		}(), queue: baseQueue},
		{name: "missing checkpoint detail", loop: baseLoop, run: func() storage.RunRecord {
			r := baseRun
			r.CheckpointJSON = stringPtr(`{"resumePolicy":"restart_from_discover"}`)
			return r
		}(), queue: baseQueue},
		{name: "attempt cap", loop: func() storage.LoopRecord {
			l := baseLoop
			l.MetadataJSON = stringPtr(`{"loop":{"consecutiveFailures":1,"autoRecoveryAttempts":3}}`)
			return l
		}(), run: baseRun, queue: baseQueue},
		{name: "latest queue not failed", loop: baseLoop, run: baseRun, queue: func() storage.QueueItemRecord { q := baseQueue; q.Status = "completed"; return q }()},
		{name: "unrelated run step", loop: baseLoop, run: func() storage.RunRecord {
			r := baseRun
			s := "setup"
			r.CurrentStep = &s
			r.CheckpointJSON = checkpoint(`"detail":{"state":"OPEN","reviewDecision":"","labels":[]}`)
			r.Summary = &errorMessage
			r.ErrorMessage = &errorMessage
			return r
		}(), queue: func() storage.QueueItemRecord {
			q := baseQueue
			kind := "non_retryable"
			q.LastErrorKind = &kind
			return q
		}()},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if shouldAutoRecoverFailedReviewerLoop(tt.loop, &tt.run, &tt.queue, defaultPolicy) {
				t.Fatalf("shouldAutoRecoverFailedReviewerLoop() = true, want false")
			}
		})
	}
}

func TestShouldRequeueLoopKeepsPausedLoopsExcluded(t *testing.T) {
	t.Parallel()

	if shouldRequeueLoop(storage.LoopRecord{Status: "paused"}, &storage.RunRecord{Status: "interrupted"}, false) {
		t.Fatal("shouldRequeueLoop() = true, want false for paused loop")
	}
}

func TestShouldAutoRecoverFailedReviewerLoopIgnoresLegacyBudgetTermination(t *testing.T) {
	t.Parallel()
	errorKind := "retryable_after_resume"
	errorMessage := "PR head changed before publish: expected old, got new"
	step := "publish"
	checkpoint := `{"resumePolicy":"restart_from_discover","detail":{"state":"OPEN","reviewDecision":"","labels":[]}}`
	loop := storage.LoopRecord{ID: "loop_recover", Type: "reviewer", Status: "failed", MetadataJSON: stringPtr(`{"loop":{"enabled":true,"status":"terminated","terminationReason":"max_wall_clock","consecutiveFailures":99,"maxIterationsPerPR":2,"maxIterationsPerHead":1,"maxWallClockSeconds":60,"maxConsecutiveFailures":3,"maxAgentExecutionsPerPR":25}}`)}
	run := storage.RunRecord{ID: "run_recover", LoopID: "loop_recover", Status: "failed", CurrentStep: &step, CheckpointJSON: &checkpoint, Summary: &errorMessage, ErrorMessage: &errorMessage}
	queue := storage.QueueItemRecord{ID: "queue_recover", LoopID: stringPtr("loop_recover"), Status: "failed", LastError: &errorMessage, LastErrorKind: &errorKind}
	policy := runtimeReviewerRecoveryPolicy{stopOnApproved: true, stopOnReadyLabel: true}

	if !shouldAutoRecoverFailedReviewerLoop(loop, &run, &queue, policy) {
		t.Fatalf("shouldAutoRecoverFailedReviewerLoop() = false, want true")
	}
	updated := autoRecoveredReviewerLoop(loop, "2026-04-11T12:00:00.000Z")
	loopMeta := runtimeReviewerLoopMetadata(parseRuntimeJSONObject(updated.MetadataJSON))
	for _, key := range deprecatedReviewerLoopBudgetMetadataKeys {
		if _, ok := loopMeta[key]; ok {
			t.Fatalf("loop metadata retained deprecated budget key %q: %#v", key, loopMeta)
		}
	}
}

func TestShouldAutoRecoverFailedReviewerLoopAllowsRetryableTransientWithAttemptsRemaining(t *testing.T) {
	t.Parallel()
	errorKind := "retryable_transient"
	errorMessage := "reviewer agent timed out (max_runtime)"
	step := "review"
	checkpoint := `{"resumePolicy":"replay_step","detail":{"state":"OPEN","reviewDecision":"","labels":[]}}`
	loop := storage.LoopRecord{ID: "loop_recover", Type: "reviewer", Status: "failed", MetadataJSON: stringPtr(`{"loop":{"enabled":true,"consecutiveFailures":3}}`)}
	run := storage.RunRecord{ID: "run_recover", LoopID: "loop_recover", Status: "failed", CurrentStep: &step, CheckpointJSON: &checkpoint, Summary: &errorMessage, ErrorMessage: &errorMessage}
	queue := storage.QueueItemRecord{ID: "queue_recover", LoopID: stringPtr("loop_recover"), Status: "failed", Attempts: 3, MaxAttempts: 5, LastError: &errorMessage, LastErrorKind: &errorKind}
	policy := runtimeReviewerRecoveryPolicy{stopOnApproved: true, stopOnReadyLabel: true}

	if !shouldAutoRecoverFailedReviewerLoop(loop, &run, &queue, policy) {
		t.Fatalf("shouldAutoRecoverFailedReviewerLoop() = false, want true")
	}
	queue.Attempts = 4
	if shouldAutoRecoverFailedReviewerLoop(loop, &run, &queue, policy) {
		t.Fatalf("shouldAutoRecoverFailedReviewerLoop() = true, want false on final allowed run")
	}
}

func TestShouldAutoRecoverFailedReviewerLoopIgnoresApprovalByAnotherUser(t *testing.T) {
	t.Parallel()
	errorKind := "retryable_after_resume"
	errorMessage := "PR head changed before publish: expected old, got new"
	step := "publish"
	checkpoint := `{"resumePolicy":"restart_from_discover","detail":{"state":"OPEN","reviewDecision":"APPROVED","headSha":"abc123","currentLogin":"octocat","reviews":[{"author":{"login":"other"},"state":"APPROVED","commit":{"oid":"abc123"}}],"labels":[]}}`
	loop := storage.LoopRecord{ID: "loop_recover", Type: "reviewer", Status: "failed", MetadataJSON: stringPtr(`{"loop":{"enabled":true,"consecutiveFailures":1}}`)}
	run := storage.RunRecord{ID: "run_recover", LoopID: "loop_recover", Status: "failed", CurrentStep: &step, CheckpointJSON: &checkpoint, Summary: &errorMessage, ErrorMessage: &errorMessage}
	queue := storage.QueueItemRecord{ID: "queue_recover", LoopID: stringPtr("loop_recover"), Status: "failed", LastError: &errorMessage, LastErrorKind: &errorKind}
	policy := runtimeReviewerRecoveryPolicy{stopOnApproved: true, stopOnReadyLabel: true}

	if !shouldAutoRecoverFailedReviewerLoop(loop, &run, &queue, policy) {
		t.Fatalf("shouldAutoRecoverFailedReviewerLoop() = false, want true")
	}
}

func TestCanRefreshReviewerLoginForRecovery(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		loop      storage.LoopRecord
		latestRun *storage.RunRecord
		want      bool
	}{
		{name: "reviewer failed loop and run", loop: storage.LoopRecord{Type: "reviewer", Status: "failed"}, latestRun: &storage.RunRecord{Status: "failed"}, want: true},
		{name: "non-reviewer loop", loop: storage.LoopRecord{Type: "worker", Status: "failed"}, latestRun: &storage.RunRecord{Status: "failed"}, want: false},
		{name: "non-failed loop status", loop: storage.LoopRecord{Type: "reviewer", Status: "running"}, latestRun: &storage.RunRecord{Status: "failed"}, want: false},
		{name: "nil latest run", loop: storage.LoopRecord{Type: "reviewer", Status: "failed"}, latestRun: nil, want: false},
		{name: "non-failed latest run", loop: storage.LoopRecord{Type: "reviewer", Status: "failed"}, latestRun: &storage.RunRecord{Status: "completed"}, want: false},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := canRefreshReviewerLoginForRecovery(tt.loop, tt.latestRun); got != tt.want {
				t.Fatalf("canRefreshReviewerLoginForRecovery() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestRunRecoveryPipelineDefersReviewerLoginRefresh(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Roles.Reviewer.Behavior.Loop.StopOnApproved = true
	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	coordinator := openMigratedCoordinator(t, cfg.Storage.DBPath, filepath.Join(workingDir, "backups"))
	defer coordinator.Close()
	repositories := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 17, 12, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)

	projectOneRepoPath := filepath.Join(workingDir, "repo-one")
	projectTwoRepoPath := filepath.Join(workingDir, "repo-two")
	if err := repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "project_1", Name: "Looper One", RepoPath: projectOneRepoPath, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert(project_1) error = %v", err)
	}
	if err := repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "project_2", Name: "Looper Two", RepoPath: projectTwoRepoPath, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert(project_2) error = %v", err)
	}

	seedFailedReviewerRecoveryLoop(t, repositories, "project_1", "loop_project1_a", 1, nowISO)
	seedFailedReviewerRecoveryLoop(t, repositories, "project_1", "loop_project1_b", 2, nowISO)
	seedFailedReviewerRecoveryLoop(t, repositories, "project_2", "loop_project2_a", 3, nowISO)

	githubGateway := githubinfra.New(githubinfra.Options{GHRun: func(ctx context.Context, options shell.Options) (shell.Result, error) {
		t.Fatal("startup recovery should not call gh api user")
		return shell.Result{}, nil
	}})
	rt := New(Options{Config: cfg, Logger: &testLogger{}, Now: func() time.Time { return now }})

	summary, err := rt.runRecoveryPipeline(context.Background(), repositories, githubGateway, now)
	if err != nil {
		t.Fatalf("runRecoveryPipeline() error = %v", err)
	}
	if summary.LoopsRequeued != 0 {
		t.Fatalf("LoopsRequeued = %d, want 0 for reviewer loops needing fresh login", summary.LoopsRequeued)
	}
	assertLoopStatus(t, repositories, "loop_project1_a", "failed")
	assertLoopStatus(t, repositories, "loop_project1_b", "failed")
	assertLoopStatus(t, repositories, "loop_project2_a", "failed")
}

func TestDeferredReviewerRecoveryRefreshesLoginAtMostOncePerProject(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Roles.Reviewer.Behavior.Loop.StopOnApproved = true
	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	coordinator := openMigratedCoordinator(t, cfg.Storage.DBPath, filepath.Join(workingDir, "backups"))
	defer coordinator.Close()
	repositories := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 17, 12, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)

	projectOneRepoPath := filepath.Join(workingDir, "repo-one")
	projectTwoRepoPath := filepath.Join(workingDir, "repo-two")
	if err := repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "project_1", Name: "Looper One", RepoPath: projectOneRepoPath, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert(project_1) error = %v", err)
	}
	if err := repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "project_2", Name: "Looper Two", RepoPath: projectTwoRepoPath, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert(project_2) error = %v", err)
	}

	seedFailedReviewerRecoveryLoop(t, repositories, "project_1", "loop_project1_a", 1, nowISO)
	seedFailedReviewerRecoveryLoop(t, repositories, "project_1", "loop_project1_b", 2, nowISO)
	seedFailedReviewerRecoveryLoop(t, repositories, "project_2", "loop_project2_a", 3, nowISO)

	var mu sync.Mutex
	loginCallsByCWD := map[string]int{}
	githubGateway := githubinfra.New(githubinfra.Options{GHRun: func(ctx context.Context, options shell.Options) (shell.Result, error) {
		mu.Lock()
		loginCallsByCWD[options.CWD]++
		mu.Unlock()
		return shell.Result{Stdout: "other\n"}, nil
	}})
	rt := New(Options{Config: cfg, Logger: &testLogger{}, Now: func() time.Time { return now }})

	requeued, err := rt.runDeferredReviewerRecovery(context.Background(), repositories, githubGateway, now)
	if err != nil {
		t.Fatalf("runDeferredReviewerRecovery() error = %v", err)
	}
	if requeued != 3 {
		t.Fatalf("runDeferredReviewerRecovery() = %d, want 3", requeued)
	}

	mu.Lock()
	defer mu.Unlock()
	if got := loginCallsByCWD[projectOneRepoPath]; got != 1 {
		t.Fatalf("GetCurrentUserLogin() calls for %q = %d, want 1", projectOneRepoPath, got)
	}
	if got := loginCallsByCWD[projectTwoRepoPath]; got != 1 {
		t.Fatalf("GetCurrentUserLogin() calls for %q = %d, want 1", projectTwoRepoPath, got)
	}
	if len(loginCallsByCWD) != 2 {
		t.Fatalf("GetCurrentUserLogin() call map = %#v, want exactly two project repo paths", loginCallsByCWD)
	}
	assertLoopStatus(t, repositories, "loop_project1_a", "queued")
	assertLoopStatus(t, repositories, "loop_project1_b", "queued")
	assertLoopStatus(t, repositories, "loop_project2_a", "queued")
}

func TestDeferredReviewerRecoveryDoesNotCacheFailedLoginRefresh(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Roles.Reviewer.Behavior.Loop.StopOnApproved = true
	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	coordinator := openMigratedCoordinator(t, cfg.Storage.DBPath, filepath.Join(workingDir, "backups"))
	defer coordinator.Close()
	repositories := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 17, 12, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)

	projectRepoPath := filepath.Join(workingDir, "repo-one")
	if err := repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "project_1", Name: "Looper One", RepoPath: projectRepoPath, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert(project_1) error = %v", err)
	}

	seedFailedReviewerRecoveryLoop(t, repositories, "project_1", "loop_project1_a", 1, nowISO)
	seedFailedReviewerRecoveryLoop(t, repositories, "project_1", "loop_project1_b", 2, nowISO)

	loginCalls := 0
	githubGateway := githubinfra.New(githubinfra.Options{GHRun: func(ctx context.Context, options shell.Options) (shell.Result, error) {
		loginCalls++
		if loginCalls == 1 {
			return shell.Result{}, errors.New("transient gh timeout")
		}
		return shell.Result{Stdout: "other\n"}, nil
	}})
	rt := New(Options{Config: cfg, Logger: &testLogger{}, Now: func() time.Time { return now }})

	requeued, err := rt.runDeferredReviewerRecovery(context.Background(), repositories, githubGateway, now)
	if err != nil {
		t.Fatalf("runDeferredReviewerRecovery() error = %v", err)
	}
	if requeued != 1 {
		t.Fatalf("runDeferredReviewerRecovery() = %d, want 1", requeued)
	}
	if loginCalls != 2 {
		t.Fatalf("GetCurrentUserLogin() calls = %d, want retry after failed refresh", loginCalls)
	}
	projectLoops, err := repositories.Loops.List(context.Background())
	if err != nil {
		t.Fatalf("Loops.List() error = %v", err)
	}
	queued := 0
	failed := 0
	for _, loop := range projectLoops {
		switch loop.Status {
		case "queued":
			queued++
		case "failed":
			failed++
		}
	}
	if queued != 1 || failed != 1 {
		t.Fatalf("loop statuses queued=%d failed=%d, want queued=1 failed=1", queued, failed)
	}
}

func TestDeferredReviewerRecoverySkipsLoopChangedAfterListing(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Roles.Reviewer.Behavior.Loop.StopOnApproved = true
	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	coordinator := openMigratedCoordinator(t, cfg.Storage.DBPath, filepath.Join(workingDir, "backups"))
	defer coordinator.Close()
	repositories := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 17, 12, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)

	projectRepoPath := filepath.Join(workingDir, "repo-one")
	if err := repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "project_1", Name: "Looper One", RepoPath: projectRepoPath, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert(project_1) error = %v", err)
	}

	loopID := "loop_project1_a"
	seedFailedReviewerRecoveryLoop(t, repositories, "project_1", loopID, 1, nowISO)

	githubGateway := githubinfra.New(githubinfra.Options{GHRun: func(ctx context.Context, options shell.Options) (shell.Result, error) {
		loop, err := repositories.Loops.GetByID(ctx, loopID)
		if err != nil {
			return shell.Result{}, err
		}
		if loop == nil {
			return shell.Result{}, errors.New("missing loop")
		}
		loop.Status = "paused"
		loop.UpdatedAt = nowISO
		if err := repositories.Loops.Upsert(ctx, *loop); err != nil {
			return shell.Result{}, err
		}
		return shell.Result{Stdout: "other\n"}, nil
	}})
	rt := New(Options{Config: cfg, Logger: &testLogger{}, Now: func() time.Time { return now }})

	requeued, err := rt.runDeferredReviewerRecovery(context.Background(), repositories, githubGateway, now)
	if err != nil {
		t.Fatalf("runDeferredReviewerRecovery() error = %v", err)
	}
	if requeued != 0 {
		t.Fatalf("runDeferredReviewerRecovery() = %d, want 0", requeued)
	}
	assertLoopStatus(t, repositories, loopID, "paused")
}

func TestShouldAutoRecoverFailedReviewerLoopUsesRefreshedCurrentLogin(t *testing.T) {
	t.Parallel()
	errorKind := "retryable_after_resume"
	errorMessage := "PR head changed before publish: expected old, got new"
	step := "publish"
	checkpoint := `{"resumePolicy":"restart_from_discover","detail":{"state":"OPEN","reviewDecision":"APPROVED","headSha":"abc123","currentLogin":"octocat","reviews":[{"author":{"login":"octocat"},"state":"APPROVED","commit":{"oid":"abc123"}}],"labels":[]}}`
	loop := storage.LoopRecord{ID: "loop_recover", Type: "reviewer", Status: "failed", MetadataJSON: stringPtr(`{"loop":{"enabled":true,"consecutiveFailures":1}}`)}
	run := storage.RunRecord{ID: "run_recover", LoopID: "loop_recover", Status: "failed", CurrentStep: &step, CheckpointJSON: &checkpoint, Summary: &errorMessage, ErrorMessage: &errorMessage}
	queue := storage.QueueItemRecord{ID: "queue_recover", LoopID: stringPtr("loop_recover"), Status: "failed", LastError: &errorMessage, LastErrorKind: &errorKind}
	policy := runtimeReviewerRecoveryPolicy{stopOnApproved: true, stopOnReadyLabel: true, currentLogin: "other"}

	if !shouldAutoRecoverFailedReviewerLoop(loop, &run, &queue, policy) {
		t.Fatalf("shouldAutoRecoverFailedReviewerLoop() = false, want true")
	}
}

func TestShouldAutoRecoverFailedReviewerLoopHonorsRecoveryPolicy(t *testing.T) {
	t.Parallel()
	errorKind := "retryable_after_resume"
	errorMessage := "PR head changed before publish: expected old, got new"
	step := "publish"
	checkpoint := func(detail string) *string {
		value := `{"resumePolicy":"restart_from_discover",` + detail + `}`
		return &value
	}
	loop := storage.LoopRecord{ID: "loop_recover", Type: "reviewer", Status: "failed", MetadataJSON: stringPtr(`{"loop":{"enabled":true,"consecutiveFailures":1}}`)}
	queue := storage.QueueItemRecord{ID: "queue_recover", LoopID: stringPtr("loop_recover"), Status: "failed", LastError: &errorMessage, LastErrorKind: &errorKind}
	policy := runtimeReviewerRecoveryPolicy{includeDrafts: true, stopOnApproved: false, stopOnReadyLabel: false}

	tests := []struct {
		name string
		run  storage.RunRecord
	}{
		{name: "draft checkpoint", run: storage.RunRecord{ID: "run_recover_draft", LoopID: "loop_recover", Status: "failed", CurrentStep: &step, CheckpointJSON: checkpoint(`"detail":{"state":"OPEN","isDraft":true,"reviewDecision":"","labels":[]}`), Summary: &errorMessage, ErrorMessage: &errorMessage}},
		{name: "approved checkpoint", run: storage.RunRecord{ID: "run_recover_approved", LoopID: "loop_recover", Status: "failed", CurrentStep: &step, CheckpointJSON: checkpoint(`"detail":{"state":"OPEN","reviewDecision":"APPROVED","labels":[]}`), Summary: &errorMessage, ErrorMessage: &errorMessage}},
		{name: "ready label checkpoint", run: storage.RunRecord{ID: "run_recover_ready", LoopID: "loop_recover", Status: "failed", CurrentStep: &step, CheckpointJSON: checkpoint(`"detail":{"state":"OPEN","reviewDecision":"","labels":["looper:spec-ready"]}`), Summary: &errorMessage, ErrorMessage: &errorMessage}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if !shouldAutoRecoverFailedReviewerLoop(loop, &tt.run, &queue, policy) {
				t.Fatalf("shouldAutoRecoverFailedReviewerLoop() = false, want true")
			}
		})
	}
}

func TestDefaultSyncConfiguredProjectsPreservesUnknownMetadataFields(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	coordinator := openMigratedCoordinator(t, filepath.Join(workingDir, "runtime.sqlite"), filepath.Join(workingDir, "backups"))
	ctx := context.Background()
	repos := storage.NewRepositories(coordinator.DB())
	existingMetadata := `{"extra":"value","repo":"nexu-io/looper","worktreeRoot":"/tmp/old","source":"config"}`
	repoPath := "/tmp/repo"
	project := config.ProjectRefConfig{ID: "project_1", Name: "Looper", RepoPath: repoPath}
	createdAt := "2026-04-16T12:00:00.000Z"
	if err := repos.Projects.Upsert(ctx, storage.ProjectRecord{ID: project.ID, Name: project.Name, RepoPath: repoPath, MetadataJSON: &existingMetadata, CreatedAt: createdAt, UpdatedAt: createdAt}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Projects = []config.ProjectRefConfig{project}
	now := time.Date(2026, time.April, 17, 12, 0, 0, 0, time.UTC)
	service := &projects.Service{Repos: repos, Now: func() time.Time { return now }}
	if err := defaultSyncConfiguredProjects(ctx, service, cfg, now); err != nil {
		t.Fatalf("defaultSyncConfiguredProjects() error = %v", err)
	}
	stored, err := repos.Projects.GetByID(ctx, project.ID)
	if err != nil {
		t.Fatalf("Projects.GetByID() error = %v", err)
	}
	if stored == nil || stored.MetadataJSON == nil {
		t.Fatalf("Projects.GetByID() = %#v, want metadata", stored)
	}

	const want = `{"extra":"value","repo":"nexu-io/looper","worktreeRoot":null,"source":"config"}`
	if *stored.MetadataJSON != want {
		t.Fatalf("project.MetadataJSON = %q, want %q", *stored.MetadataJSON, want)
	}
}

func TestRuntimeStartReturnsErrorAfterStop(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}

	cfg.Storage.DBPath = workingDir + "/runtime.sqlite"

	rt := New(Options{Config: cfg, Logger: &testLogger{}})
	rt.Stop("test")

	err = rt.Start(context.Background())
	if err == nil || err.Error() != "runtime already stopped" {
		t.Fatalf("Start() after Stop() error = %v, want runtime already stopped", err)
	}
}

func TestValidateCoordinatorDependencyGatesFailsClosedWhenAPIUnavailable(t *testing.T) {
	t.Parallel()
	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Roles.Coordinator.Enabled = true
	cfg.Roles.Coordinator.Dependencies.Enabled = true
	coordinator := openMigratedCoordinator(t, filepath.Join(workingDir, "runtime.sqlite"), filepath.Join(workingDir, "backups"))
	defer coordinator.Close()
	repositories := storage.NewRepositories(coordinator.DB())
	now := formatJavaScriptISOString(time.Date(2026, time.May, 16, 12, 0, 0, 0, time.UTC))
	metadata := `{"repo":"acme/looper","worktreeRoot":null,"source":"config"}`
	if err := repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "demo", Name: "Demo", RepoPath: workingDir, MetadataJSON: &metadata, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	githubGateway := githubinfra.New(githubinfra.Options{GHRun: func(ctx context.Context, options shell.Options) (shell.Result, error) {
		args := strings.Join(options.Args, " ")
		switch {
		case args == "api repos/acme/looper/issues?state=all&per_page=100&page=1":
			return shell.Result{Stdout: `[{"number":7}]`}, nil
		case strings.Contains(args, "dependencies/blocked_by"):
			result := shell.Result{ExitCode: 1, Stderr: "gh: HTTP 404: Not Found"}
			return result, &shell.CommandExecutionError{Message: "Command exited with code 1", Result: result}
		default:
			t.Fatalf("unexpected gh args: %q", args)
			return shell.Result{}, nil
		}
	}})
	rt := New(Options{Config: cfg, Logger: &testLogger{}})

	err = rt.validateCoordinatorDependencyGates(context.Background(), repositories, githubGateway)
	if err == nil || !strings.Contains(err.Error(), "coordinator dependency gate enabled but dependencies API unavailable on acme/looper") {
		t.Fatalf("validateCoordinatorDependencyGates() error = %v, want actionable unavailable error", err)
	}
}

func TestValidateCoordinatorDependencyGatesAllowsAvailableAPI(t *testing.T) {
	t.Parallel()
	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Roles.Coordinator.Enabled = true
	cfg.Roles.Coordinator.Dependencies.Enabled = true
	coordinator := openMigratedCoordinator(t, filepath.Join(workingDir, "runtime.sqlite"), filepath.Join(workingDir, "backups"))
	defer coordinator.Close()
	repositories := storage.NewRepositories(coordinator.DB())
	now := formatJavaScriptISOString(time.Date(2026, time.May, 16, 12, 0, 0, 0, time.UTC))
	metadata := `{"repo":"acme/looper","worktreeRoot":null,"source":"config"}`
	if err := repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "demo", Name: "Demo", RepoPath: workingDir, MetadataJSON: &metadata, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	githubGateway := githubinfra.New(githubinfra.Options{GHRun: func(ctx context.Context, options shell.Options) (shell.Result, error) {
		args := strings.Join(options.Args, " ")
		switch {
		case args == "api repos/acme/looper/issues?state=all&per_page=100&page=1":
			return shell.Result{Stdout: `[{"number":7}]`}, nil
		case strings.Contains(args, "dependencies/blocked_by"):
			return shell.Result{Stdout: `[]`}, nil
		default:
			t.Fatalf("unexpected gh args: %q", args)
			return shell.Result{}, nil
		}
	}})
	rt := New(Options{Config: cfg, Logger: &testLogger{}})

	if err := rt.validateCoordinatorDependencyGates(context.Background(), repositories, githubGateway); err != nil {
		t.Fatalf("validateCoordinatorDependencyGates() error = %v, want nil", err)
	}
}

func TestValidateCoordinatorDependencyGatesSkipsProbeWhenRepoHasNoIssues(t *testing.T) {
	t.Parallel()
	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Roles.Coordinator.Enabled = true
	cfg.Roles.Coordinator.Dependencies.Enabled = true
	coordinator := openMigratedCoordinator(t, filepath.Join(workingDir, "runtime.sqlite"), filepath.Join(workingDir, "backups"))
	defer coordinator.Close()
	repositories := storage.NewRepositories(coordinator.DB())
	now := formatJavaScriptISOString(time.Date(2026, time.May, 16, 12, 0, 0, 0, time.UTC))
	metadata := `{"repo":"acme/looper","worktreeRoot":null,"source":"config"}`
	if err := repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "demo", Name: "Demo", RepoPath: workingDir, MetadataJSON: &metadata, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	blockedByCalls := 0
	githubGateway := githubinfra.New(githubinfra.Options{GHRun: func(ctx context.Context, options shell.Options) (shell.Result, error) {
		args := strings.Join(options.Args, " ")
		switch {
		case args == "api repos/acme/looper/issues?state=all&per_page=100&page=1":
			return shell.Result{Stdout: `[{"number":12,"pull_request":{}}]`}, nil
		case args == "api repos/acme/looper/issues?state=all&per_page=100&page=2":
			return shell.Result{Stdout: `[]`}, nil
		case strings.Contains(args, "dependencies/blocked_by"):
			blockedByCalls++
			return shell.Result{Stdout: `[]`}, nil
		default:
			t.Fatalf("unexpected gh args: %q", args)
			return shell.Result{}, nil
		}
	}})
	rt := New(Options{Config: cfg, Logger: &testLogger{}})

	if err := rt.validateCoordinatorDependencyGates(context.Background(), repositories, githubGateway); err != nil {
		t.Fatalf("validateCoordinatorDependencyGates() error = %v, want nil", err)
	}
	if blockedByCalls != 0 {
		t.Fatalf("dependencies/blocked_by call count = %d, want 0", blockedByCalls)
	}
}

func TestValidateCoordinatorDependencyGatesReturnsInvalidJSONFromIssueProbe(t *testing.T) {
	t.Parallel()
	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Roles.Coordinator.Enabled = true
	cfg.Roles.Coordinator.Dependencies.Enabled = true
	coordinator := openMigratedCoordinator(t, filepath.Join(workingDir, "runtime.sqlite"), filepath.Join(workingDir, "backups"))
	defer coordinator.Close()
	repositories := storage.NewRepositories(coordinator.DB())
	now := formatJavaScriptISOString(time.Date(2026, time.May, 16, 12, 0, 0, 0, time.UTC))
	metadata := `{"repo":"acme/looper","worktreeRoot":null,"source":"config"}`
	if err := repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "demo", Name: "Demo", RepoPath: workingDir, MetadataJSON: &metadata, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	githubGateway := githubinfra.New(githubinfra.Options{GHRun: func(ctx context.Context, options shell.Options) (shell.Result, error) {
		args := strings.Join(options.Args, " ")
		switch {
		case args == "api repos/acme/looper/issues?state=all&per_page=100&page=1":
			return shell.Result{Stdout: `not-json`}, nil
		default:
			t.Fatalf("unexpected gh args: %q", args)
			return shell.Result{}, nil
		}
	}})
	rt := New(Options{Config: cfg, Logger: &testLogger{}})

	err = rt.validateCoordinatorDependencyGates(context.Background(), repositories, githubGateway)
	if err == nil || !strings.Contains(err.Error(), "Invalid gh JSON payload") {
		t.Fatalf("validateCoordinatorDependencyGates() error = %v, want Invalid gh JSON payload", err)
	}
}

func TestValidateCoordinatorDependencyGatesSkipsArchivedProjects(t *testing.T) {
	t.Parallel()
	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Roles.Coordinator.Enabled = true
	cfg.Roles.Coordinator.Dependencies.Enabled = true
	coordinator := openMigratedCoordinator(t, filepath.Join(workingDir, "runtime.sqlite"), filepath.Join(workingDir, "backups"))
	defer coordinator.Close()
	repositories := storage.NewRepositories(coordinator.DB())
	now := formatJavaScriptISOString(time.Date(2026, time.May, 16, 12, 0, 0, 0, time.UTC))
	if err := repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "archived", Name: "Archived", RepoPath: workingDir, Archived: true, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	blockedByCalls := 0
	githubGateway := githubinfra.New(githubinfra.Options{GHRun: func(ctx context.Context, options shell.Options) (shell.Result, error) {
		args := strings.Join(options.Args, " ")
		switch {
		case strings.Contains(args, "issues?state=all"):
			t.Fatalf("unexpected issue listing for archived project: %q", args)
			return shell.Result{}, nil
		case strings.Contains(args, "dependencies/blocked_by"):
			blockedByCalls++
			return shell.Result{Stdout: `[]`}, nil
		default:
			t.Fatalf("unexpected gh args: %q", args)
			return shell.Result{}, nil
		}
	}})
	rt := New(Options{Config: cfg, Logger: &testLogger{}})

	if err := rt.validateCoordinatorDependencyGates(context.Background(), repositories, githubGateway); err != nil {
		t.Fatalf("validateCoordinatorDependencyGates() error = %v, want nil", err)
	}
	if blockedByCalls != 0 {
		t.Fatalf("dependencies/blocked_by call count = %d, want 0", blockedByCalls)
	}
}

func TestFormatJavaScriptISOStringPreservesMilliseconds(t *testing.T) {
	t.Parallel()

	value := time.Date(2026, time.April, 17, 12, 34, 56, 789_123_000, time.UTC)
	if got, want := formatJavaScriptISOString(value), "2026-04-17T12:34:56.789Z"; got != want {
		t.Fatalf("formatJavaScriptISOString() = %q, want %q", got, want)
	}
}

type testLogger struct {
	mu      sync.Mutex
	entries []string
}

func (l *testLogger) Debug(message string, context map[string]any) { l.append(message) }
func (l *testLogger) Info(message string, context map[string]any)  { l.append(message) }
func (l *testLogger) Warn(message string, context map[string]any)  { l.append(message) }
func (l *testLogger) Error(message string, context map[string]any) { l.append(message) }

func (l *testLogger) append(message string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, message)
}

func (l *testLogger) containsMessage(want string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, entry := range l.entries {
		if entry == want {
			return true
		}
	}
	return false
}

func (l *testLogger) messages() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.entries...)
}

func int64Ptr(value int64) *int64 {
	return &value
}

func containsEventType(events []storage.EventLogRecord, want string) bool {
	for _, event := range events {
		if event.EventType == want {
			return true
		}
	}
	return false
}

func openMigratedCoordinator(t *testing.T, dbPath, backupDir string) *storage.SQLiteCoordinator {
	t.Helper()

	coordinator, err := storage.OpenSQLiteCoordinator(context.Background(), dbPath, storage.SQLiteCoordinatorOptions{
		BackupDir: backupDir,
	})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	if _, err := coordinator.MigrationRunner().RunPending(context.Background()); err != nil {
		t.Fatalf("MigrationRunner.RunPending() error = %v", err)
	}
	return coordinator
}

func seedLoopWithRun(t *testing.T, repos *storage.Repositories, projectID, loopID string, seq int64, loopStatus, runStatus, nowISO string) {
	t.Helper()
	prNumber := int64(99)
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{
		ID:         loopID,
		Seq:        seq,
		ProjectID:  projectID,
		Type:       "worker",
		TargetType: "pull_request",
		TargetID:   stringPtr("pr:acme/looper:99"),
		Repo:       stringPtr("acme/looper"),
		PRNumber:   &prNumber,
		Status:     loopStatus,
		NextRunAt:  stringPtr(nowISO),
		CreatedAt:  nowISO,
		UpdatedAt:  nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert(%s) error = %v", loopID, err)
	}
	if err := repos.Runs.Upsert(context.Background(), storage.RunRecord{
		ID:        loopID + "_run",
		LoopID:    loopID,
		Status:    runStatus,
		StartedAt: nowISO,
		EndedAt:   stringPtr(nowISO),
		CreatedAt: nowISO,
		UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Runs.Upsert(%s) error = %v", loopID, err)
	}
}

func seedFailedReviewerRecoveryLoop(t *testing.T, repos *storage.Repositories, projectID, loopID string, seq int64, nowISO string) {
	t.Helper()

	repo := "acme/looper"
	prNumber := int64(42 + seq)
	targetID := "pr:acme/looper:" + mustFormatInt64(prNumber)
	metadata := mustMarshalJSON(map[string]any{"loop": map[string]any{"enabled": true, "failureCount": 1, "consecutiveFailures": 1, "lastFailure": "PR head changed before publish"}})
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: seq, ProjectID: projectID, Type: "reviewer", TargetType: "pull_request", TargetID: &targetID, Repo: &repo, PRNumber: &prNumber, Status: "failed", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert(%s) error = %v", loopID, err)
	}
	step := "publish"
	errorMessage := "PR head changed before publish: expected old, got new"
	checkpoint := `{"resumePolicy":"restart_from_discover","detail":{"state":"OPEN","reviewDecision":"APPROVED","headSha":"abc123","currentLogin":"octocat","reviews":[{"author":{"login":"octocat"},"state":"APPROVED","commit":{"oid":"abc123"}}],"labels":[]}}`
	runID := "run_" + loopID
	if err := repos.Runs.Upsert(context.Background(), storage.RunRecord{ID: runID, LoopID: loopID, Status: "failed", CurrentStep: &step, CheckpointJSON: &checkpoint, Summary: &errorMessage, ErrorMessage: &errorMessage, StartedAt: nowISO, EndedAt: &nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Runs.Upsert(%s) error = %v", runID, err)
	}
	queueKind := "retryable_after_resume"
	queueID := "queue_" + loopID
	if err := repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: queueID, ProjectID: &projectID, LoopID: &loopID, Type: "reviewer", TargetType: "pull_request", TargetID: targetID, Repo: &repo, PRNumber: &prNumber, DedupeKey: "reviewer:" + projectID + ":" + loopID + ":acme/looper:" + mustFormatInt64(prNumber), Priority: storage.QueuePriorityReviewer, Status: "failed", AvailableAt: nowISO, Attempts: 1, MaxAttempts: 3, FinishedAt: &nowISO, LastError: &errorMessage, LastErrorKind: &queueKind, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Queue.Upsert(%s) error = %v", queueID, err)
	}
}

func mustFormatInt64(value int64) string {
	return strconv.FormatInt(value, 10)
}

func assertLoopStatus(t *testing.T, repos *storage.Repositories, loopID, want string) {
	t.Helper()
	loop, err := repos.Loops.GetByID(context.Background(), loopID)
	if err != nil {
		t.Fatalf("Loops.GetByID(%s) error = %v", loopID, err)
	}
	if loop == nil || loop.Status != want {
		t.Fatalf("Loops.GetByID(%s) = %#v, want status %q", loopID, loop, want)
	}
}

func waitForCondition(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("condition not satisfied before timeout")
}
