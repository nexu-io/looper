package runtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/powerformer/looper/internal/bootstrap"
	"github.com/powerformer/looper/internal/config"
	gitinfra "github.com/powerformer/looper/internal/infra/git"
	githubinfra "github.com/powerformer/looper/internal/infra/github"
	"github.com/powerformer/looper/internal/loops"
	"github.com/powerformer/looper/internal/projects"
	"github.com/powerformer/looper/internal/runs"
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
	ShutdownTimeout        time.Duration
	OpenSQLiteCoordinator  OpenSQLiteCoordinatorFunc
	SyncConfiguredProjects SyncConfiguredProjectsFunc
	RunSchedulerTick       RunSchedulerTickFunc
}

type Services struct {
	Coordinator  *storage.SQLiteCoordinator
	Repositories *storage.Repositories
	Projects     *projects.Service
	Loops        *loops.Service
	Runs         *runs.Service
}

type Runtime struct {
	config config.Config
	logger bootstrap.Logger
	now    func() time.Time

	openSQLiteCoordinator  OpenSQLiteCoordinatorFunc
	syncConfiguredProjects SyncConfiguredProjectsFunc
	runSchedulerTick       RunSchedulerTickFunc
	shutdownTimeout        time.Duration

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
	inFlightWork  map[string]chan struct{}
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

	shutdownTimeout := options.ShutdownTimeout
	if shutdownTimeout <= 0 {
		shutdownTimeout = time.Duration(options.Config.Daemon.ShutdownTimeoutMS) * time.Millisecond
	}
	if shutdownTimeout <= 0 {
		shutdownTimeout = time.Second
	}

	return &Runtime{
		config:                 options.Config,
		logger:                 options.Logger,
		now:                    now,
		openSQLiteCoordinator:  openSQLiteCoordinator,
		syncConfiguredProjects: syncConfiguredProjects,
		runSchedulerTick:       runSchedulerTick,
		shutdownTimeout:        shutdownTimeout,
		recovery:               createEmptyRecoverySummary(),
		shutdownCh:             make(chan struct{}),
		inFlightWork:           make(map[string]chan struct{}),
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
		r.mu.Unlock()

		r.waitForInFlightSchedulerWorkOnStop()

		if repositories != nil {
			if err := r.appendStoppedEvent(context.Background(), repositories, reason); err != nil && r.logger != nil {
				r.logger.Warn("looperd runtime stop event failed", map[string]any{"error": err.Error()})
			}
		}

		r.mu.Lock()
		r.services = Services{}
		r.mu.Unlock()

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

// trackInFlightWork registers a unit of scheduler-owned work so shutdown can
// wait for it to finish before tearing down shared runtime services.
func (r *Runtime) trackInFlightWork(id string) func() {
	if id == "" {
		return func() {}
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	doneCh := make(chan struct{})
	r.inFlightWork[id] = doneCh

	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			current, ok := r.inFlightWork[id]
			if ok && current == doneCh {
				delete(r.inFlightWork, id)
			}
			r.mu.Unlock()
			close(doneCh)
		})
	}
}

func (r *Runtime) WaitForShutdown() {
	<-r.shutdownCh
}

func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
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
	gitGateway := gitinfra.New(gitinfra.Options{GitPath: derefString(r.config.Tools.GitPath), Repos: repositories, Now: r.now})
	githubGateway := githubinfra.New(githubinfra.Options{GHPath: derefString(r.config.Tools.GHPath), Now: r.now})
	projectService := &projects.Service{
		DB:     coordinator.DB(),
		Repos:  repositories,
		Logger: r.logger,
		Now:    r.now,
		DetectRepo: func(ctx context.Context, repoPath string) (string, error) {
			return gitGateway.DetectGitHubRepo(ctx, repoPath)
		},
		ListWorktrees: func(ctx context.Context, repoPath string) ([]projects.WorktreeListEntry, error) {
			worktrees, err := gitGateway.ListWorktrees(ctx, repoPath)
			if err != nil {
				return nil, err
			}
			items := make([]projects.WorktreeListEntry, 0, len(worktrees))
			for _, worktree := range worktrees {
				items = append(items, projects.WorktreeListEntry{Path: worktree.Path, Branch: worktree.Branch, HeadSHA: worktree.HeadSHA, Bare: worktree.Bare})
			}
			return items, nil
		},
		ListOpenPullRequests: func(ctx context.Context, input projects.ListOpenPullRequestsInput) ([]projects.PullRequestSummary, error) {
			pullRequests, err := githubGateway.ListOpenPullRequests(ctx, githubinfra.ListOpenPullRequestsInput{Repo: input.Repo, CWD: input.CWD, Limit: input.Limit})
			if err != nil {
				return nil, err
			}
			items := make([]projects.PullRequestSummary, 0, len(pullRequests))
			for _, pullRequest := range pullRequests {
				items = append(items, projects.PullRequestSummary{Number: pullRequest.Number, State: pullRequest.State, IsDraft: pullRequest.IsDraft})
			}
			return items, nil
		},
		CapturePullRequestSnapshot: func(ctx context.Context, input projects.CapturePullRequestSnapshotInput) (storage.PullRequestSnapshotRecord, error) {
			return githubGateway.CapturePullRequestSnapshot(ctx, githubinfra.CapturePullRequestSnapshotInput{ProjectID: input.ProjectID, Repo: input.Repo, PRNumber: input.PRNumber, CWD: input.CWD, CapturedAt: input.CapturedAt})
		},
	}
	loopService := &loops.Service{DB: coordinator.DB(), Repos: repositories, Now: r.now}
	runService := &runs.Service{DB: coordinator.DB(), Repos: repositories, Loops: loopService, Now: r.now}
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
		Projects:     projectService,
		Loops:        loopService,
		Runs:         runService,
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

func (r *Runtime) waitForInFlightSchedulerWorkOnStop() {
	r.mu.RLock()
	if len(r.inFlightWork) == 0 {
		r.mu.RUnlock()
		return
	}

	ids := make([]string, 0, len(r.inFlightWork))
	for id := range r.inFlightWork {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	work := make([]chan struct{}, 0, len(ids))
	for _, id := range ids {
		work = append(work, r.inFlightWork[id])
	}
	r.mu.RUnlock()

	timer := time.NewTimer(r.shutdownTimeout)
	defer timer.Stop()

	for _, doneCh := range work {
		select {
		case <-doneCh:
		case <-timer.C:
			if r.logger != nil {
				r.logger.Warn("looperd stop timed out waiting for in-flight scheduler work", map[string]any{
					"timeoutMs":    r.shutdownTimeout.Milliseconds(),
					"queueItemIds": ids,
				})
			}
			return
		}
	}
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
	finishWork := r.trackInFlightWork("scheduler-tick")
	defer finishWork()

	if err := r.runSchedulerTick(ctx, services); err != nil && r.logger != nil {
		r.logger.Warn("looperd scheduler tick failed", map[string]any{"error": err.Error()})
	}
}

func (r *Runtime) runRecoveryPipeline(ctx context.Context, repositories *storage.Repositories, now time.Time) (RecoverySummary, error) {
	nowISO := formatJavaScriptISOString(now)
	eventsWritten := int64(0)
	summary := createEmptyRecoverySummary()
	summary.StartedAt = nowISO
	summary.OrphanAgentCleanup.Attempted = true
	if repositories.AgentExecutions != nil {
		activeExecutions, err := repositories.AgentExecutions.ListActive(ctx)
		if err != nil {
			return RecoverySummary{}, err
		}
		for _, execution := range activeExecutions {
			if execution.PID == nil || *execution.PID <= 0 {
				continue
			}
			pid := int(*execution.PID)
			if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
				if r.logger != nil {
					r.logger.Warn("failed to cleanup orphan agent execution", map[string]any{"executionId": execution.ID, "pid": pid, "error": err.Error()})
				}
				continue
			}

			cleaned := execution
			cleaned.Status = "killed"
			if cleaned.ErrorMessage == nil {
				cleaned.ErrorMessage = stringPtr("Killed during looperd recovery")
			}
			cleaned.EndedAt = stringPtr(nowISO)
			cleaned.UpdatedAt = nowISO
			if err := repositories.AgentExecutions.Upsert(ctx, cleaned); err != nil {
				return RecoverySummary{}, err
			}
			summary.OrphanAgentCleanup.CleanedCount += 1
			if err := appendSystemEvent(ctx, repositories, storage.EventLogRecord{
				ID:         newRuntimeEventID(),
				EventType:  "agent.killed",
				ProjectID:  execution.ProjectID,
				LoopID:     execution.LoopID,
				RunID:      execution.RunID,
				EntityType: stringPtr("agent_execution"),
				EntityID:   stringPtr(execution.ID),
				PayloadJSON: mustMarshalJSON(map[string]any{
					"pid":         pid,
					"recoveredAt": nowISO,
				}),
				CreatedAt: nowISO,
			}); err != nil {
				return RecoverySummary{}, err
			}
			eventsWritten += 1
		}
	}

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
	service := &projects.Service{Repos: repositories, Now: func() time.Time { return now }}
	return service.SyncConfigured(ctx, cfg, now)
}

func formatJavaScriptISOString(value time.Time) string {
	value = value.UTC()
	return fmt.Sprintf("%s.%03dZ", value.Format("2006-01-02T15:04:05"), value.Nanosecond()/int(time.Millisecond))
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
