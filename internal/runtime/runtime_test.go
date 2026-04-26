package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/powerformer/looper/internal/config"
	"github.com/powerformer/looper/internal/storage"
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
	if err := seedRepos.AgentExecutions.Upsert(context.Background(), storage.AgentExecutionRecord{
		ID:             "agent_orphan_1",
		Vendor:         "codex",
		Status:         "running",
		PID:            &pid,
		CommandJSON:    stringPtr(`{"command":"codex","args":["exec","fix failing tests"]}`),
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
			if gotPID != int(pid) || signal != syscall.SIGTERM {
				t.Fatalf("SignalProcess(%d, %v), want (%d, SIGTERM)", gotPID, signal, pid)
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
	recovery := rt.RecoverySummary()
	if recovery.OrphanAgentCleanup.CleanedCount != 0 {
		t.Fatalf("RecoverySummary().OrphanAgentCleanup = %#v, want cleanedCount=0", recovery.OrphanAgentCleanup)
	}
	if !logger.containsMessage("skipped orphan agent cleanup for mismatched pid") {
		t.Fatalf("logger entries = %#v, want mismatched pid warning", logger.messages())
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
	defer close(blockCh)
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

	started := time.Now()
	rt.Stop("SIGTERM")
	elapsed := time.Since(started)
	if elapsed > 250*time.Millisecond {
		t.Fatalf("Stop() elapsed = %v, want timeout-bounded shutdown", elapsed)
	}
	if !logger.containsMessage("looperd stop timed out waiting for scheduler loop") {
		t.Fatalf("logger entries = %#v, want scheduler timeout warning", logger.messages())
	}
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
	existingMetadata := `{"repo":"powerformer/looper","worktreeRoot":"/tmp/old","source":"config"}`
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
	if err := defaultSyncConfiguredProjects(context.Background(), repositories, cfg, now); err != nil {
		t.Fatalf("defaultSyncConfiguredProjects() error = %v", err)
	}

	project, err := repositories.Projects.GetByID(context.Background(), "project_1")
	if err != nil {
		t.Fatalf("Projects.GetByID() error = %v", err)
	}
	if project == nil || project.MetadataJSON == nil {
		t.Fatal("project metadata missing after sync")
	}
	const want = `{"repo":"powerformer/looper","worktreeRoot":null,"source":"config"}`
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

func TestDefaultSyncConfiguredProjectsPreservesUnknownMetadataFields(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	coordinator := openMigratedCoordinator(t, filepath.Join(workingDir, "runtime.sqlite"), filepath.Join(workingDir, "backups"))
	ctx := context.Background()
	repos := storage.NewRepositories(coordinator.DB())
	existingMetadata := `{"extra":"value","repo":"powerformer/looper","worktreeRoot":"/tmp/old","source":"config"}`
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
	if err := defaultSyncConfiguredProjects(ctx, repos, cfg, now); err != nil {
		t.Fatalf("defaultSyncConfiguredProjects() error = %v", err)
	}
	stored, err := repos.Projects.GetByID(ctx, project.ID)
	if err != nil {
		t.Fatalf("Projects.GetByID() error = %v", err)
	}
	if stored == nil || stored.MetadataJSON == nil {
		t.Fatalf("Projects.GetByID() = %#v, want metadata", stored)
	}

	const want = `{"extra":"value","repo":"powerformer/looper","worktreeRoot":null,"source":"config"}`
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
