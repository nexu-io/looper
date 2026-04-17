package runtime

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/powerformer/looper/internal/bootstrap"
	"github.com/powerformer/looper/internal/config"
	"github.com/powerformer/looper/internal/storage"
)

type OpenSQLiteCoordinatorFunc func(context.Context, string, storage.SQLiteCoordinatorOptions) (*storage.SQLiteCoordinator, error)

type SyncConfiguredProjectsFunc func(context.Context, *storage.Repositories, config.Config, time.Time) error

type RunSchedulerTickFunc func(context.Context, Services) error

type RecoverySummary struct {
	StartedAt             string                     `json:"startedAt,omitempty"`
	CompletedAt           string                     `json:"completedAt,omitempty"`
	OrphanAgentCleanup    RecoveryOrphanAgentCleanup `json:"orphanAgentCleanup"`
	ExpiredLocksReleased  int64                      `json:"expiredLocksReleased"`
	InterruptedRunsMarked int64                      `json:"interruptedRunsMarked"`
	LoopsRequeued         int64                      `json:"loopsRequeued"`
	EventsWritten         int64                      `json:"eventsWritten"`
}

type RecoveryOrphanAgentCleanup struct {
	Attempted    bool   `json:"attempted"`
	CleanedCount int64  `json:"cleanedCount"`
	Warning      string `json:"warning,omitempty"`
}

type Options struct {
	Config                 config.Config
	Logger                 bootstrap.Logger
	Now                    func() time.Time
	OpenSQLiteCoordinator  OpenSQLiteCoordinatorFunc
	SyncConfiguredProjects SyncConfiguredProjectsFunc
	RunSchedulerTick       RunSchedulerTickFunc
}

type Services struct {
	Coordinator  *storage.SQLiteCoordinator
	Repositories *storage.Repositories
}

type Runtime struct {
	config config.Config
	logger bootstrap.Logger
	now    func() time.Time

	openSQLiteCoordinator  OpenSQLiteCoordinatorFunc
	syncConfiguredProjects SyncConfiguredProjectsFunc
	runSchedulerTick       RunSchedulerTickFunc

	mu            sync.RWMutex
	startedAt     *time.Time
	recovery      RecoverySummary
	stopped       bool
	services      Services
	startErr      error
	startOnce     sync.Once
	shutdownOnce  sync.Once
	shutdownCh    chan struct{}
	schedulerStop chan struct{}
	schedulerDone chan struct{}
	schedulerWake chan struct{}
}

func New(options Options) *Runtime {
	now := options.Now
	if now == nil {
		now = time.Now
	}

	openSQLiteCoordinator := options.OpenSQLiteCoordinator
	if openSQLiteCoordinator == nil {
		openSQLiteCoordinator = storage.OpenSQLiteCoordinator
	}

	syncConfiguredProjects := options.SyncConfiguredProjects
	if syncConfiguredProjects == nil {
		syncConfiguredProjects = defaultSyncConfiguredProjects
	}

	runSchedulerTick := options.RunSchedulerTick
	if runSchedulerTick == nil {
		runSchedulerTick = func(context.Context, Services) error {
			return nil
		}
	}

	return &Runtime{
		config:                 options.Config,
		logger:                 options.Logger,
		now:                    now,
		openSQLiteCoordinator:  openSQLiteCoordinator,
		syncConfiguredProjects: syncConfiguredProjects,
		runSchedulerTick:       runSchedulerTick,
		recovery:               createEmptyRecoverySummary(),
		shutdownCh:             make(chan struct{}),
	}
}

func Start(ctx context.Context, deps bootstrap.RuntimeDependencies) (bootstrap.Runtime, error) {
	rt := New(Options{
		Config: deps.Config,
		Logger: deps.Logger,
	})
	if err := rt.Start(ctx); err != nil {
		return nil, err
	}

	return rt, nil
}

func (r *Runtime) Start(ctx context.Context) error {
	r.startOnce.Do(func() {
		r.startErr = r.start(ctx)
	})

	return r.startErr
}

func (r *Runtime) Stop(reason string) {
	r.shutdownOnce.Do(func() {
		if r.logger != nil {
			r.logger.Info("looperd runtime stopping", map[string]any{"reason": reason})
		}

		r.stopSchedulerLoop()

		r.mu.Lock()
		r.stopped = true
		coordinator := r.services.Coordinator
		repositories := r.services.Repositories
		r.services = Services{}
		r.mu.Unlock()

		if repositories != nil {
			if err := r.appendStoppedEvent(context.Background(), repositories, reason); err != nil && r.logger != nil {
				r.logger.Warn("looperd runtime stop event failed", map[string]any{"error": err.Error()})
			}
		}

		if coordinator != nil {
			if err := coordinator.Close(); err != nil && r.logger != nil {
				r.logger.Warn("looperd runtime close failed", map[string]any{"error": err.Error()})
			}
		}

		close(r.shutdownCh)

		if r.logger != nil {
			r.logger.Info("looperd runtime stopped", map[string]any{"reason": reason})
		}
	})
}

func (r *Runtime) WaitForShutdown() {
	<-r.shutdownCh
}

func (r *Runtime) Services() Services {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return r.services
}

func (r *Runtime) StartedAt() (time.Time, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if r.startedAt == nil {
		return time.Time{}, false
	}

	return *r.startedAt, true
}

func (r *Runtime) Config() config.Config {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return r.config
}

func (r *Runtime) RecoverySummary() RecoverySummary {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return r.recovery
}

func (r *Runtime) start(ctx context.Context) error {
	r.mu.RLock()
	if r.stopped {
		r.mu.RUnlock()
		return fmt.Errorf("runtime already stopped")
	}
	r.mu.RUnlock()

	backupDir := ""
	if r.config.Storage.BackupDir != nil {
		backupDir = *r.config.Storage.BackupDir
	}

	coordinator, err := r.openSQLiteCoordinator(ctx, r.config.Storage.DBPath, storage.SQLiteCoordinatorOptions{
		BackupDir: backupDir,
		Now:       r.now,
	})
	if err != nil {
		return err
	}

	started := false
	defer func() {
		if !started {
			_ = coordinator.Close()
		}
	}()

	if r.config.Package.AutoMigrateOnStartup {
		_, err = coordinator.MigrationRunner().RunPending(ctx, storage.RunPendingOptions{
			RequireBackup: r.config.Package.RequireBackupBeforeMigrate,
		})
		if err != nil {
			return err
		}
	}

	repositories := storage.NewRepositories(coordinator.DB())
	startedAt := r.now().UTC()
	if err := r.syncConfiguredProjects(ctx, repositories, r.config, startedAt); err != nil {
		return err
	}

	recoverySummary, err := r.runRecoveryPipeline(ctx, repositories, startedAt)
	if err != nil {
		return err
	}

	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return fmt.Errorf("runtime already stopped")
	}
	r.startedAt = &startedAt
	r.recovery = recoverySummary
	r.services = Services{
		Coordinator:  coordinator,
		Repositories: repositories,
	}
	r.mu.Unlock()

	if err := r.appendStartedEvent(context.Background(), startedAt); err != nil {
		return err
	}
	r.startSchedulerLoop()

	started = true

	if r.logger != nil {
		r.logger.Info("looperd runtime assembled", map[string]any{
			"dbPath":          r.config.Storage.DBPath,
			"projectCount":    len(r.config.Projects),
			"autoMigrate":     r.config.Package.AutoMigrateOnStartup,
			"backupRequired":  r.config.Package.RequireBackupBeforeMigrate,
			"recoverySummary": recoverySummary,
		})
	}

	return nil
}

func (r *Runtime) startSchedulerLoop() {
	pollInterval := time.Duration(r.config.Scheduler.PollIntervalSeconds) * time.Second
	stopCh := make(chan struct{})
	doneCh := make(chan struct{})
	wakeCh := make(chan struct{}, 1)

	r.mu.Lock()
	r.schedulerStop = stopCh
	r.schedulerDone = doneCh
	r.schedulerWake = wakeCh
	r.mu.Unlock()

	go func() {
		defer close(doneCh)

		r.executeSchedulerTick(context.Background())
		if pollInterval <= 0 {
			for {
				select {
				case <-stopCh:
					return
				case <-wakeCh:
					r.executeSchedulerTick(context.Background())
				}
			}
		}

		ticker := time.NewTicker(pollInterval)
		defer ticker.Stop()

		for {
			select {
			case <-stopCh:
				return
			case <-wakeCh:
				r.executeSchedulerTick(context.Background())
			case <-ticker.C:
				r.executeSchedulerTick(context.Background())
			}
		}
	}()
}

func (r *Runtime) stopSchedulerLoop() {
	r.mu.Lock()
	stopCh := r.schedulerStop
	doneCh := r.schedulerDone
	r.schedulerStop = nil
	r.schedulerDone = nil
	r.schedulerWake = nil
	r.mu.Unlock()

	if stopCh == nil || doneCh == nil {
		return
	}

	close(stopCh)
	<-doneCh
}

func (r *Runtime) TriggerSchedulerTick() {
	r.mu.RLock()
	if r.stopped {
		r.mu.RUnlock()
		return
	}
	wakeCh := r.schedulerWake
	r.mu.RUnlock()

	if wakeCh == nil {
		return
	}

	select {
	case wakeCh <- struct{}{}:
	default:
	}
}

func (r *Runtime) executeSchedulerTick(ctx context.Context) {
	services := r.Services()
	if services.Repositories == nil {
		return
	}

	if err := r.runSchedulerTick(ctx, services); err != nil && r.logger != nil {
		r.logger.Warn("looperd scheduler tick failed", map[string]any{"error": err.Error()})
	}
}

func (r *Runtime) runRecoveryPipeline(ctx context.Context, repositories *storage.Repositories, now time.Time) (RecoverySummary, error) {
	nowISO := formatJavaScriptISOString(now)
	eventsWritten := int64(0)
	summary := createEmptyRecoverySummary()
	summary.StartedAt = nowISO
	summary.OrphanAgentCleanup.Warning = "agent execution recovery has not been ported yet"

	expiredLocks, err := repositories.Locks.ListExpired(ctx, nowISO)
	if err != nil {
		return RecoverySummary{}, err
	}
	for _, lock := range expiredLocks {
		if err := repositories.Locks.Release(ctx, lock.Key); err != nil {
			return RecoverySummary{}, err
		}
		summary.ExpiredLocksReleased += 1
		if err := appendSystemEvent(ctx, repositories, storage.EventLogRecord{
			ID:         newRuntimeEventID(),
			EventType:  "looperd.recovery.lock_released",
			EntityType: stringPtr("lock"),
			EntityID:   stringPtr(lock.Key),
			PayloadJSON: mustMarshalJSON(map[string]any{
				"owner":       lock.Owner,
				"expiredAt":   lock.ExpiresAt,
				"recoveredAt": nowISO,
			}),
			CreatedAt: nowISO,
		}); err != nil {
			return RecoverySummary{}, err
		}
		eventsWritten += 1
	}

	loops, err := repositories.Loops.List(ctx)
	if err != nil {
		return RecoverySummary{}, err
	}
	requeuedLoopIDs := make(map[string]struct{})
	for _, loop := range loops {
		latestRun, err := repositories.Runs.GetLatestByLoopID(ctx, loop.ID)
		if err != nil {
			return RecoverySummary{}, err
		}
		if latestRun == nil {
			continue
		}

		if latestRun.Status == "running" {
			interrupted := *latestRun
			interrupted.Status = "interrupted"
			if interrupted.ErrorMessage == nil {
				interrupted.ErrorMessage = stringPtr("Interrupted during looperd recovery")
			}
			interrupted.EndedAt = stringPtr(nowISO)
			interrupted.UpdatedAt = nowISO
			if err := repositories.Runs.Upsert(ctx, interrupted); err != nil {
				return RecoverySummary{}, err
			}
			*latestRun = interrupted
			summary.InterruptedRunsMarked += 1
			if err := appendSystemEvent(ctx, repositories, storage.EventLogRecord{
				ID:         newRuntimeEventID(),
				EventType:  "looperd.recovery.run_interrupted",
				LoopID:     stringPtr(loop.ID),
				RunID:      stringPtr(latestRun.ID),
				EntityType: stringPtr("run"),
				EntityID:   stringPtr(latestRun.ID),
				PayloadJSON: mustMarshalJSON(map[string]any{
					"previousStatus":  "running",
					"recoveredStatus": "interrupted",
				}),
				CreatedAt: nowISO,
			}); err != nil {
				return RecoverySummary{}, err
			}
			eventsWritten += 1
		}

		if shouldRequeueLoop(loop, *latestRun) {
			requeuedLoop := loop
			requeuedLoop.Status = "queued"
			requeuedLoop.NextRunAt = stringPtr(nowISO)
			requeuedLoop.LastRunAt = coalesceString(latestRun.EndedAt, stringPtr(latestRun.StartedAt), loop.LastRunAt)
			requeuedLoop.UpdatedAt = nowISO
			if err := repositories.Loops.Upsert(ctx, requeuedLoop); err != nil {
				return RecoverySummary{}, err
			}
			requeuedLoopIDs[loop.ID] = struct{}{}
			recoveredQueueItems, err := repositories.Queue.RequeueRunningByLoop(ctx, loop.ID, nowISO)
			if err != nil {
				return RecoverySummary{}, err
			}
			summary.LoopsRequeued += 1
			if err := appendSystemEvent(ctx, repositories, storage.EventLogRecord{
				ID:         newRuntimeEventID(),
				EventType:  "looperd.recovery.loop_requeued",
				LoopID:     stringPtr(loop.ID),
				EntityType: stringPtr("loop"),
				EntityID:   stringPtr(loop.ID),
				PayloadJSON: mustMarshalJSON(map[string]any{
					"previousStatus":      loop.Status,
					"nextRunAt":           nowISO,
					"recoveredQueueItems": recoveredQueueItems,
				}),
				CreatedAt: nowISO,
			}); err != nil {
				return RecoverySummary{}, err
			}
			eventsWritten += 1
		}
	}

	queueItems, err := repositories.Queue.List(ctx)
	if err != nil {
		return RecoverySummary{}, err
	}
	queuedLoopIDs := make(map[string]struct{})
	for _, item := range queueItems {
		if item.LoopID == nil {
			continue
		}
		if item.Status == "queued" || item.Status == "running" {
			queuedLoopIDs[*item.LoopID] = struct{}{}
		}
	}

	for _, loop := range loops {
		if loop.Status != "queued" {
			continue
		}
		if _, wasRequeued := requeuedLoopIDs[loop.ID]; wasRequeued {
			continue
		}
		if _, exists := queuedLoopIDs[loop.ID]; exists {
			continue
		}

		latestRun, err := repositories.Runs.GetLatestByLoopID(ctx, loop.ID)
		if err != nil {
			return RecoverySummary{}, err
		}
		if latestRun == nil {
			continue
		}

		normalizedLoop := loop
		normalizedLoop.Status = normalizeStaleQueuedLoopStatus(*latestRun)
		normalizedLoop.NextRunAt = nil
		normalizedLoop.LastRunAt = coalesceString(latestRun.EndedAt, stringPtr(latestRun.StartedAt), loop.LastRunAt)
		normalizedLoop.UpdatedAt = nowISO
		if err := repositories.Loops.Upsert(ctx, normalizedLoop); err != nil {
			return RecoverySummary{}, err
		}
		if err := appendSystemEvent(ctx, repositories, storage.EventLogRecord{
			ID:         newRuntimeEventID(),
			EventType:  "looperd.recovery.loop_queue_normalized",
			LoopID:     stringPtr(loop.ID),
			EntityType: stringPtr("loop"),
			EntityID:   stringPtr(loop.ID),
			PayloadJSON: mustMarshalJSON(map[string]any{
				"previousStatus":  loop.Status,
				"recoveredStatus": normalizedLoop.Status,
				"latestRunStatus": latestRun.Status,
			}),
			CreatedAt: nowISO,
		}); err != nil {
			return RecoverySummary{}, err
		}
		eventsWritten += 1
	}

	summary.CompletedAt = nowISO
	if err := appendSystemEvent(ctx, repositories, storage.EventLogRecord{
		ID:         newRuntimeEventID(),
		EventType:  "looperd.recovery.completed",
		EntityType: stringPtr("notification"),
		EntityID:   stringPtr("looperd-recovery"),
		PayloadJSON: mustMarshalJSON(map[string]any{
			"expiredLocksReleased":  summary.ExpiredLocksReleased,
			"interruptedRunsMarked": summary.InterruptedRunsMarked,
			"loopsRequeued":         summary.LoopsRequeued,
			"orphanAgentCleanup":    summary.OrphanAgentCleanup,
		}),
		CreatedAt: nowISO,
	}); err != nil {
		return RecoverySummary{}, err
	}
	eventsWritten += 1
	summary.EventsWritten = eventsWritten

	return summary, nil
}

func (r *Runtime) appendStartedEvent(ctx context.Context, startedAt time.Time) error {
	services := r.Services()
	if services.Repositories == nil {
		return nil
	}

	return appendSystemEvent(ctx, services.Repositories, storage.EventLogRecord{
		ID:         newRuntimeEventID(),
		EventType:  "looperd.started",
		EntityType: stringPtr("notification"),
		EntityID:   stringPtr("looperd"),
		PayloadJSON: mustMarshalJSON(map[string]any{
			"daemonMode": r.config.Daemon.Mode,
			"host":       r.config.Server.Host,
			"port":       r.config.Server.Port,
			"recovery":   r.RecoverySummary(),
		}),
		CreatedAt: formatJavaScriptISOString(startedAt),
	})
}

func (r *Runtime) appendStoppedEvent(ctx context.Context, repositories *storage.Repositories, reason string) error {
	return appendSystemEvent(ctx, repositories, storage.EventLogRecord{
		ID:         newRuntimeEventID(),
		EventType:  "looperd.stopped",
		EntityType: stringPtr("notification"),
		EntityID:   stringPtr("looperd"),
		PayloadJSON: mustMarshalJSON(map[string]any{
			"reason": reason,
		}),
		CreatedAt: formatJavaScriptISOString(r.now()),
	})
}

func defaultSyncConfiguredProjects(ctx context.Context, repositories *storage.Repositories, cfg config.Config, now time.Time) error {
	if repositories == nil || repositories.Projects == nil {
		return fmt.Errorf("projects repository is not configured")
	}

	nowISO := formatJavaScriptISOString(now)

	for _, project := range cfg.Projects {
		existing, err := repositories.Projects.GetByID(ctx, project.ID)
		if err != nil {
			return err
		}

		metadataJSONValue, err := buildProjectMetadataJSON(existing, project)
		if err != nil {
			return fmt.Errorf("build project metadata for %s: %w", project.ID, err)
		}

		baseBranch := cfg.Defaults.BaseBranch
		if project.BaseBranch != nil {
			baseBranch = *project.BaseBranch
		}

		createdAt := nowISO
		if existing != nil {
			createdAt = existing.CreatedAt
		}

		record := storage.ProjectRecord{
			ID:           project.ID,
			Name:         project.Name,
			RepoPath:     project.RepoPath,
			BaseBranch:   &baseBranch,
			Archived:     false,
			MetadataJSON: &metadataJSONValue,
			CreatedAt:    createdAt,
			UpdatedAt:    nowISO,
		}
		if err := repositories.Projects.Upsert(ctx, record); err != nil {
			return err
		}
	}

	return nil
}

func buildProjectMetadataJSON(existing *storage.ProjectRecord, project config.ProjectRefConfig) (string, error) {
	extras := make([]orderedJSONEntry, 0)
	repoRaw := []byte("null")

	if existing != nil {
		existingMetadata, err := parseProjectMetadata(existing.MetadataJSON)
		if err != nil {
			return "", err
		}
		for _, entry := range existingMetadata {
			switch entry.Key {
			case "repo":
				if existing.RepoPath == project.RepoPath {
					var existingRepo string
					if err := json.Unmarshal(entry.Raw, &existingRepo); err == nil && existingRepo != "" {
						repoRaw, err = json.Marshal(existingRepo)
						if err != nil {
							return "", err
						}
					}
				}
			case "worktreeRoot", "source":
				continue
			default:
				extras = append(extras, entry)
			}
		}
	}

	worktreeRootRaw := []byte("null")
	if project.WorktreeRoot != nil {
		encodedWorktreeRoot, err := json.Marshal(*project.WorktreeRoot)
		if err != nil {
			return "", err
		}
		worktreeRootRaw = encodedWorktreeRoot
	}

	entries := append(extras,
		orderedJSONEntry{Key: "repo", Raw: repoRaw},
		orderedJSONEntry{Key: "worktreeRoot", Raw: worktreeRootRaw},
		orderedJSONEntry{Key: "source", Raw: json.RawMessage(`"config"`)},
	)

	return marshalOrderedJSONObject(entries)
}

type orderedJSONEntry struct {
	Key string
	Raw json.RawMessage
}

func parseProjectMetadata(raw *string) ([]orderedJSONEntry, error) {
	if raw == nil || *raw == "" {
		return []orderedJSONEntry{}, nil
	}

	decoder := json.NewDecoder(strings.NewReader(*raw))
	startToken, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	startDelimiter, ok := startToken.(json.Delim)
	if !ok || startDelimiter != '{' {
		return nil, fmt.Errorf("project metadata must be a JSON object")
	}

	entries := make([]orderedJSONEntry, 0)
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, err
		}

		key, ok := keyToken.(string)
		if !ok {
			return nil, fmt.Errorf("project metadata key must be a string")
		}

		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}

		entries = append(entries, orderedJSONEntry{Key: key, Raw: value})
	}

	endToken, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	endDelimiter, ok := endToken.(json.Delim)
	if !ok || endDelimiter != '}' {
		return nil, fmt.Errorf("project metadata must end with a JSON object delimiter")
	}

	return entries, nil
}

func formatJavaScriptISOString(value time.Time) string {
	value = value.UTC()
	return fmt.Sprintf("%s.%03dZ", value.Format("2006-01-02T15:04:05"), value.Nanosecond()/int(time.Millisecond))
}

func marshalOrderedJSONObject(entries []orderedJSONEntry) (string, error) {
	buffer := &bytes.Buffer{}
	buffer.WriteByte('{')

	for index, entry := range entries {
		if index > 0 {
			buffer.WriteByte(',')
		}

		keyJSON, err := json.Marshal(entry.Key)
		if err != nil {
			return "", err
		}
		buffer.Write(keyJSON)
		buffer.WriteByte(':')
		buffer.Write(entry.Raw)
	}

	buffer.WriteByte('}')
	return buffer.String(), nil
}

func appendSystemEvent(ctx context.Context, repositories *storage.Repositories, record storage.EventLogRecord) error {
	if repositories == nil || repositories.Events == nil {
		return fmt.Errorf("events repository is not configured")
	}

	record.ActorType = stringPtr("system")
	record.ActorID = stringPtr("looperd")
	record.ActorDisplayName = stringPtr("looperd")
	return repositories.Events.Append(ctx, record)
}

func createEmptyRecoverySummary() RecoverySummary {
	return RecoverySummary{
		OrphanAgentCleanup: RecoveryOrphanAgentCleanup{
			Attempted:    false,
			CleanedCount: 0,
		},
		ExpiredLocksReleased:  0,
		InterruptedRunsMarked: 0,
		LoopsRequeued:         0,
		EventsWritten:         0,
	}
}

func shouldRequeueLoop(loop storage.LoopRecord, latestRun storage.RunRecord) bool {
	if loop.Status == "paused" {
		return false
	}
	if loop.Status == "completed" || loop.Status == "failed" {
		return false
	}

	return loop.Status == "running" || latestRun.Status == "interrupted"
}

func normalizeStaleQueuedLoopStatus(latestRun storage.RunRecord) string {
	switch latestRun.Status {
	case "success":
		return "completed"
	case "interrupted", "running":
		return "interrupted"
	default:
		return "failed"
	}
}

func mustMarshalJSON(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

func stringPtr(value string) *string {
	return &value
}

func coalesceString(values ...*string) *string {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func newRuntimeEventID() string {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return fmt.Sprintf("runtime_%d", time.Now().UTC().UnixNano())
	}
	return "runtime_" + hex.EncodeToString(raw)
}
