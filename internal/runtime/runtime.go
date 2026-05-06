package runtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/powerformer/looper/internal/bootstrap"
	"github.com/powerformer/looper/internal/config"
	"github.com/powerformer/looper/internal/domain"
	gitinfra "github.com/powerformer/looper/internal/infra/git"
	githubinfra "github.com/powerformer/looper/internal/infra/github"
	"github.com/powerformer/looper/internal/infra/specpr"
	"github.com/powerformer/looper/internal/loops"
	"github.com/powerformer/looper/internal/projects"
	"github.com/powerformer/looper/internal/runs"
	"github.com/powerformer/looper/internal/storage"
)

type OpenSQLiteCoordinatorFunc func(context.Context, string, storage.SQLiteCoordinatorOptions) (*storage.SQLiteCoordinator, error)

type SyncConfiguredProjectsFunc func(context.Context, *storage.Repositories, config.Config, time.Time) error

type RunSchedulerTickFunc func(context.Context, Services) error

type ReadProcessCommandFunc func(context.Context, int) (string, error)

type SignalProcessFunc func(int, syscall.Signal) error

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
	ReadProcessCommand     ReadProcessCommandFunc
	SignalProcess          SignalProcessFunc
}

type Services struct {
	Coordinator      *storage.SQLiteCoordinator
	Repositories     *storage.Repositories
	Projects         *projects.Service
	Loops            *loops.Service
	Runs             *runs.Service
	ActiveExecutions *ActiveExecutionRegistry
}

type Runtime struct {
	config config.Config
	logger bootstrap.Logger
	now    func() time.Time

	openSQLiteCoordinator  OpenSQLiteCoordinatorFunc
	syncConfiguredProjects SyncConfiguredProjectsFunc
	runSchedulerTick       RunSchedulerTickFunc
	defaultSchedulerTick   RunSchedulerTickFunc
	customSchedulerTick    bool
	readProcessCommand     ReadProcessCommandFunc
	signalProcess          SignalProcessFunc
	shutdownTimeout        time.Duration

	mu               sync.RWMutex
	startedAt        *time.Time
	recovery         RecoverySummary
	stopped          bool
	services         Services
	startErr         error
	startOnce        sync.Once
	shutdownOnce     sync.Once
	shutdownCh       chan struct{}
	schedulerStop    chan struct{}
	schedulerDone    chan struct{}
	schedulerWake    chan struct{}
	schedulerCancel  context.CancelFunc
	schedulerTasks   *schedulerTaskTracker
	recoveryCancel   context.CancelFunc
	recoveryDone     chan struct{}
	activeExecutions *ActiveExecutionRegistry
}

const reviewerRecoveryLoginTimeout = 3 * time.Second

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
	customSchedulerTick := runSchedulerTick != nil

	readProcessCommand := options.ReadProcessCommand
	if readProcessCommand == nil {
		readProcessCommand = defaultReadProcessCommand
	}

	signalProcess := options.SignalProcess
	if signalProcess == nil {
		signalProcess = defaultSignalProcess
	}

	shutdownTimeout := options.ShutdownTimeout
	if shutdownTimeout <= 0 {
		shutdownTimeout = time.Duration(options.Config.Daemon.ShutdownTimeoutMS) * time.Millisecond
	}
	if shutdownTimeout <= 0 {
		shutdownTimeout = time.Second
	}

	rt := &Runtime{
		config:                 options.Config,
		logger:                 options.Logger,
		now:                    now,
		openSQLiteCoordinator:  openSQLiteCoordinator,
		syncConfiguredProjects: syncConfiguredProjects,
		runSchedulerTick:       runSchedulerTick,
		customSchedulerTick:    customSchedulerTick,
		readProcessCommand:     readProcessCommand,
		signalProcess:          signalProcess,
		shutdownTimeout:        shutdownTimeout,
		recovery:               createEmptyRecoverySummary(),
		shutdownCh:             make(chan struct{}),
		activeExecutions:       NewActiveExecutionRegistry(),
	}
	if !customSchedulerTick {
		rt.runSchedulerTick = rt.executeDefaultSchedulerTick
	}
	return rt
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

		r.stopDeferredReviewerRecovery()
		r.stopSchedulerLoop()

		r.mu.Lock()
		r.stopped = true
		coordinator := r.services.Coordinator
		repositories := r.services.Repositories
		r.mu.Unlock()

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
			pullRequests, err := githubGateway.ListOpenPullRequests(ctx, githubinfra.ListOpenPullRequestsInput{Repo: input.Repo, CWD: input.CWD, Limit: input.Limit, Timeout: input.Timeout})
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
		AsyncSnapshotQueueEnabled: func() bool {
			return r.customSchedulerTick || r.config.Agent.Vendor != nil
		},
	}
	loopService := &loops.Service{DB: coordinator.DB(), Repos: repositories, Now: r.now}
	runService := &runs.Service{DB: coordinator.DB(), Repos: repositories, Loops: loopService, Now: r.now}
	startedAt := r.now().UTC()
	if err := r.syncConfiguredProjects(ctx, repositories, r.config, startedAt); err != nil {
		return err
	}
	recoverySummary, err := r.runRecoveryPipeline(ctx, repositories, nil, startedAt)
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
		Coordinator:      coordinator,
		Repositories:     repositories,
		Projects:         projectService,
		Loops:            loopService,
		Runs:             runService,
		ActiveExecutions: r.activeExecutions,
	}
	schedulerDisabled := false
	if !r.customSchedulerTick {
		r.defaultSchedulerTick = buildDefaultSchedulerTick(r.config, r.logger, coordinator, repositories, gitGateway, githubGateway, r.activeExecutions, func() schedulerAsyncRunner {
			r.mu.RLock()
			defer r.mu.RUnlock()
			return r.schedulerTasks
		}, r.now)
		schedulerDisabled = r.config.Agent.Vendor == nil
	}
	r.mu.Unlock()
	if schedulerDisabled && r.logger != nil {
		r.logger.Warn("looperd scheduler disabled", map[string]any{"reason": "config.agent.vendor is not set"})
	}

	if err := r.appendStartedEvent(context.Background(), startedAt); err != nil {
		return err
	}
	if !schedulerDisabled {
		r.startSchedulerLoop()
	}
	r.startDeferredReviewerRecovery(githubGateway)

	started = true

	if r.logger != nil {
		r.logger.Info("looperd runtime assembled", map[string]any{
			"dbPath":                 r.config.Storage.DBPath,
			"projectCount":           len(r.config.Projects),
			"autoMigrate":            r.config.Package.AutoMigrateOnStartup,
			"backupRequired":         r.config.Package.RequireBackupBeforeMigrate,
			"recoverySummary":        recoverySummary,
			"schedulerDefaultActive": !r.customSchedulerTick && !schedulerDisabled,
		})
	}

	return nil
}

func (r *Runtime) startSchedulerLoop() {
	pollInterval := time.Duration(r.config.Scheduler.PollIntervalSeconds) * time.Second
	stopCh := make(chan struct{})
	doneCh := make(chan struct{})
	wakeCh := make(chan struct{}, 1)
	schedulerCtx, schedulerCancel := context.WithCancel(context.Background())
	taskTracker := &schedulerTaskTracker{}

	r.mu.Lock()
	r.schedulerStop = stopCh
	r.schedulerDone = doneCh
	r.schedulerWake = wakeCh
	r.schedulerCancel = schedulerCancel
	r.schedulerTasks = taskTracker
	r.mu.Unlock()

	go func() {
		defer close(doneCh)
		defer taskTracker.Wait()

		r.executeSchedulerTick(schedulerCtx)
		if pollInterval <= 0 {
			for {
				select {
				case <-stopCh:
					return
				case <-wakeCh:
					r.executeSchedulerTick(schedulerCtx)
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
				r.executeSchedulerTick(schedulerCtx)
			case <-ticker.C:
				r.executeSchedulerTick(schedulerCtx)
			}
		}
	}()
}

func (r *Runtime) stopSchedulerLoop() {
	r.mu.Lock()
	stopCh := r.schedulerStop
	doneCh := r.schedulerDone
	cancel := r.schedulerCancel
	taskTracker := r.schedulerTasks
	r.schedulerStop = nil
	r.schedulerDone = nil
	r.schedulerWake = nil
	r.schedulerCancel = nil
	r.mu.Unlock()

	if stopCh == nil || doneCh == nil {
		if cancel != nil {
			cancel()
		}
		return
	}

	if cancel != nil {
		cancel()
	}
	close(stopCh)
	timer := time.NewTimer(r.shutdownTimeout)
	defer timer.Stop()
	select {
	case <-doneCh:
	case <-timer.C:
		if r.logger != nil {
			r.logger.Warn("looperd stop timed out waiting for scheduler loop", map[string]any{"timeoutMs": r.shutdownTimeout.Milliseconds()})
		}
	}

	r.mu.Lock()
	if r.schedulerTasks == taskTracker {
		r.schedulerTasks = nil
	}
	r.mu.Unlock()
}

func (r *Runtime) startDeferredReviewerRecovery(githubGateway *githubinfra.Gateway) {
	if githubGateway == nil {
		return
	}
	services := r.Services()
	if services.Repositories == nil {
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	r.mu.Lock()
	r.recoveryCancel = cancel
	r.recoveryDone = done
	r.mu.Unlock()

	go func(repositories *storage.Repositories) {
		defer close(done)
		requeued, err := r.runDeferredReviewerRecovery(ctx, repositories, githubGateway, r.now().UTC())
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			if r.logger != nil {
				r.logger.Warn("looperd deferred reviewer recovery failed", map[string]any{"error": err.Error()})
			}
			return
		}
		if requeued > 0 && r.logger != nil {
			r.logger.Info("looperd deferred reviewer recovery completed", map[string]any{"loopsRequeued": requeued})
		}
	}(services.Repositories)
}

func (r *Runtime) stopDeferredReviewerRecovery() {
	r.mu.Lock()
	cancel := r.recoveryCancel
	done := r.recoveryDone
	r.recoveryCancel = nil
	r.recoveryDone = nil
	r.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if done == nil {
		return
	}
	timer := time.NewTimer(r.shutdownTimeout)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		if r.logger != nil {
			r.logger.Warn("looperd stop timed out waiting for deferred reviewer recovery", map[string]any{"timeoutMs": r.shutdownTimeout.Milliseconds()})
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
	r.mu.RLock()
	services := r.services
	tick := r.runSchedulerTick
	r.mu.RUnlock()
	if services.Repositories == nil {
		return
	}
	if tick == nil {
		return
	}

	if err := tick(ctx, services); err != nil && r.logger != nil {
		r.logger.Warn("looperd scheduler tick failed", map[string]any{"error": err.Error()})
	}
}

func (r *Runtime) executeDefaultSchedulerTick(ctx context.Context, services Services) error {
	r.mu.RLock()
	tick := r.defaultSchedulerTick
	r.mu.RUnlock()
	if tick == nil {
		return fmt.Errorf("default scheduler tick is not configured")
	}
	return tick(ctx, services)
}

func (r *Runtime) runRecoveryPipeline(ctx context.Context, repositories *storage.Repositories, githubGateway *githubinfra.Gateway, now time.Time) (RecoverySummary, error) {
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
			matches, running, err := r.executionMatchesProcess(ctx, execution, pid)
			if err != nil {
				if r.logger != nil {
					r.logger.Warn("failed to verify orphan agent execution identity", map[string]any{"executionId": execution.ID, "pid": pid, "error": err.Error()})
				}
				continue
			}
			if running && !matches {
				if r.logger != nil {
					r.logger.Warn("skipped orphan agent cleanup for mismatched pid", map[string]any{"executionId": execution.ID, "pid": pid})
				}
				continue
			}
			if running {
				if err := r.signalAgentProcessGroup(pid, syscall.SIGTERM); err != nil {
					if errors.Is(err, syscall.ESRCH) {
						running = false
					} else {
						if r.logger != nil {
							r.logger.Warn("failed to cleanup orphan agent execution", map[string]any{"executionId": execution.ID, "pid": pid, "error": err.Error()})
						}
						continue
					}
				}
				if running {
					go func(pid int) {
						timer := time.NewTimer(5 * time.Second)
						defer timer.Stop()
						<-timer.C
						_ = r.signalAgentProcessGroup(pid, syscall.SIGKILL)
					}(pid)
				}
			}

			cleaned := execution
			cleaned.Status = "killed"
			if cleaned.ErrorMessage == nil {
				cleaned.ErrorMessage = stringPtr("Killed during looperd recovery")
			}
			if r.config.Agent.NativeResume.Enabled && runtimeNativeResumeSupported(cleaned.Vendor) && cleaned.NativeSessionID != nil && strings.TrimSpace(*cleaned.NativeSessionID) != "" {
				cleaned.NativeResumeMode = stringPtr("native_resume")
				cleaned.NativeResumeStatus = stringPtr("pending")
				if r.logger != nil {
					r.logger.Info("agent execution eligible for native resume", map[string]any{"executionId": execution.ID, "runId": execution.RunID, "vendor": execution.Vendor})
				}
			} else if r.logger != nil {
				r.logger.Info("agent execution will restart from checkpoint", map[string]any{"executionId": execution.ID, "runId": execution.RunID, "vendor": execution.Vendor})
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
	loopsByID := make(map[string]storage.LoopRecord, len(loops))
	for _, loop := range loops {
		loopsByID[loop.ID] = loop
	}
	activeAgentRunIDs := make(map[string]struct{})
	if repositories.Runs != nil {
		runningRuns, err := repositories.Runs.ListByStatus(ctx, string(domain.RunStatusRunning))
		if err != nil {
			return RecoverySummary{}, err
		}
		if repositories.AgentExecutions != nil {
			activeExecutions, err := repositories.AgentExecutions.ListActive(ctx)
			if err != nil {
				return RecoverySummary{}, err
			}
			activeAgentRunIDs = make(map[string]struct{}, len(activeExecutions))
			for _, execution := range activeExecutions {
				if execution.RunID == nil || strings.TrimSpace(*execution.RunID) == "" || execution.PID == nil || *execution.PID <= 0 {
					continue
				}
				matches, running, err := r.executionMatchesProcess(ctx, execution, int(*execution.PID))
				if err != nil {
					if r.logger != nil {
						r.logger.Warn("failed to verify active agent execution identity", map[string]any{"executionId": execution.ID, "pid": *execution.PID, "error": err.Error()})
					}
					continue
				}
				if running && matches {
					activeAgentRunIDs[*execution.RunID] = struct{}{}
				}
			}
		}
		for _, run := range runningRuns {
			loop, ok := loopsByID[run.LoopID]
			if !ok {
				continue
			}
			latestRun, err := repositories.Runs.GetLatestByLoopID(ctx, run.LoopID)
			if err != nil {
				return RecoverySummary{}, err
			}
			_, hasActiveAgent := activeAgentRunIDs[run.ID]
			if !shouldInterruptStaleRunningRun(run, latestRun, hasActiveAgent) {
				continue
			}
			if err := interruptRecoveryRun(ctx, repositories, run, loop, nowISO, "Interrupted stale/orphaned running run during looperd recovery"); err != nil {
				return RecoverySummary{}, err
			}
			summary.InterruptedRunsMarked += 1
			eventsWritten += 1
		}
	}
	requeuedLoopIDs := make(map[string]struct{})
	for _, loop := range loops {
		latestRun, err := repositories.Runs.GetLatestByLoopID(ctx, loop.ID)
		if err != nil {
			return RecoverySummary{}, err
		}
		latestQueue, err := repositories.Queue.GetLatestByLoopID(ctx, loop.ID)
		if err != nil {
			return RecoverySummary{}, err
		}
		policy := runtimeReviewerRecoveryPolicy{
			includeDrafts:    r.config.Roles.Reviewer.Triggers.IncludeDrafts,
			stopOnApproved:   r.config.Reviewer.Loop.StopOnApproved,
			stopOnReadyLabel: r.config.Reviewer.Loop.StopOnReadyLabel,
		}
		if reviewerRecoveryNeedsFreshLogin(loop, latestRun, policy) {
			continue
		}
		if shouldAutoRecoverFailedReviewerLoop(loop, latestRun, latestQueue, policy) {
			recoveredQueueItems, err := requeueFailedReviewerQueueItemForRecovery(ctx, repositories, loop.ID, latestQueue, nowISO)
			if err != nil {
				return RecoverySummary{}, err
			}
			if recoveredQueueItems == 0 {
				active, activeErr := repositories.Queue.FindActiveByLoopID(ctx, loop.ID)
				if activeErr != nil {
					return RecoverySummary{}, activeErr
				}
				if active == nil {
					return RecoverySummary{}, fmt.Errorf("reviewer recovery did not requeue failed queue item %s for loop %s", latestQueue.ID, loop.ID)
				}
			}
			requeuedLoop := autoRecoveredReviewerLoop(loop, nowISO)
			if err := repositories.Loops.Upsert(ctx, requeuedLoop); err != nil {
				return RecoverySummary{}, err
			}
			requeuedLoopIDs[loop.ID] = struct{}{}
			summary.LoopsRequeued += 1
			if err := appendSystemEvent(ctx, repositories, storage.EventLogRecord{
				ID:         newRuntimeEventID(),
				EventType:  "looperd.recovery.reviewer_auto_recovered",
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
			continue
		}

		_, latestRunHasActiveAgent := activeAgentRunIDs[derefRunID(latestRun)]
		if shouldRequeueLoop(loop, latestRun, latestRunHasActiveAgent) {
			requeuedLoop := loop
			requeuedLoop.Status = "queued"
			requeuedLoop.NextRunAt = stringPtr(nowISO)
			if latestRun != nil {
				requeuedLoop.LastRunAt = coalesceString(latestRun.EndedAt, stringPtr(latestRun.StartedAt), loop.LastRunAt)
			}
			requeuedLoop.UpdatedAt = nowISO
			if err := repositories.Loops.Upsert(ctx, requeuedLoop); err != nil {
				return RecoverySummary{}, err
			}
			requeuedLoopIDs[loop.ID] = struct{}{}
			recoveredQueueItems, err := repositories.Queue.RequeueRunningByLoop(ctx, loop.ID, nowISO)
			if err != nil {
				return RecoverySummary{}, err
			}
			if recoveredQueueItems == 0 {
				if err := ensureRecoveryQueueItem(ctx, repositories, requeuedLoop, nowISO, int64(r.config.Scheduler.RetryMaxAttempts)); err != nil {
					return RecoverySummary{}, err
				}
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

func runtimeNativeResumeSupported(vendor string) bool {
	switch config.AgentVendor(vendor) {
	case config.AgentVendorClaudeCode, config.AgentVendorCodex, config.AgentVendorOpenCode, config.AgentVendorCursorCLI:
		return true
	default:
		return false
	}
}

func (r *Runtime) runDeferredReviewerRecovery(ctx context.Context, repositories *storage.Repositories, githubGateway *githubinfra.Gateway, now time.Time) (int64, error) {
	if repositories == nil || githubGateway == nil {
		return 0, nil
	}
	nowISO := formatJavaScriptISOString(now)
	loops, err := repositories.Loops.List(ctx)
	if err != nil {
		return 0, err
	}
	reviewerLoginByProjectID := make(map[string]string)
	requeued := int64(0)
	for _, loop := range loops {
		if err := ctx.Err(); err != nil {
			return requeued, err
		}
		latestRun, err := repositories.Runs.GetLatestByLoopID(ctx, loop.ID)
		if err != nil {
			return requeued, err
		}
		latestQueue, err := repositories.Queue.GetLatestByLoopID(ctx, loop.ID)
		if err != nil {
			return requeued, err
		}
		policy := runtimeReviewerRecoveryPolicy{
			includeDrafts:    r.config.Roles.Reviewer.Triggers.IncludeDrafts,
			stopOnApproved:   r.config.Reviewer.Loop.StopOnApproved,
			stopOnReadyLabel: r.config.Reviewer.Loop.StopOnReadyLabel,
		}
		if !reviewerRecoveryNeedsFreshLogin(loop, latestRun, policy) {
			continue
		}
		cachedLogin, cached := reviewerLoginByProjectID[loop.ProjectID]
		if !cached {
			login, ok := r.currentReviewerLoginForRecovery(ctx, repositories, githubGateway, loop, latestRun, policy)
			if !ok {
				continue
			}
			cachedLogin = login
			reviewerLoginByProjectID[loop.ProjectID] = cachedLogin
		}
		policy.currentLogin = cachedLogin
		if !shouldAutoRecoverFailedReviewerLoop(loop, latestRun, latestQueue, policy) {
			continue
		}
		currentLoop, err := repositories.Loops.GetByID(ctx, loop.ID)
		if err != nil {
			return requeued, err
		}
		if currentLoop == nil || !shouldAutoRecoverFailedReviewerLoop(*currentLoop, latestRun, latestQueue, policy) {
			continue
		}
		recoveredQueueItems, err := requeueFailedReviewerQueueItemForRecovery(ctx, repositories, loop.ID, latestQueue, nowISO)
		if err != nil {
			return requeued, err
		}
		if recoveredQueueItems == 0 {
			active, activeErr := repositories.Queue.FindActiveByLoopID(ctx, loop.ID)
			if activeErr != nil {
				return requeued, activeErr
			}
			if active == nil {
				return requeued, fmt.Errorf("reviewer deferred recovery did not requeue failed queue item %s for loop %s", latestQueue.ID, loop.ID)
			}
		}
		requeuedLoop := autoRecoveredReviewerLoop(*currentLoop, nowISO)
		if err := repositories.Loops.Upsert(ctx, requeuedLoop); err != nil {
			return requeued, err
		}
		requeued += 1
		if err := appendSystemEvent(ctx, repositories, storage.EventLogRecord{
			ID:         newRuntimeEventID(),
			EventType:  "looperd.recovery.reviewer_auto_recovered",
			LoopID:     stringPtr(loop.ID),
			EntityType: stringPtr("loop"),
			EntityID:   stringPtr(loop.ID),
			PayloadJSON: mustMarshalJSON(map[string]any{
				"previousStatus":      loop.Status,
				"nextRunAt":           nowISO,
				"recoveredQueueItems": recoveredQueueItems,
				"deferred":            true,
			}),
			CreatedAt: nowISO,
		}); err != nil {
			return requeued, err
		}
	}
	return requeued, nil
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

func (r *Runtime) ExecutionMatchesProcess(ctx context.Context, execution storage.AgentExecutionRecord, pid int) (matches bool, running bool, err error) {
	return r.executionMatchesProcess(ctx, execution, pid)
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

func ensureRecoveryQueueItem(ctx context.Context, repositories *storage.Repositories, loop storage.LoopRecord, nowISO string, maxAttempts int64) error {
	activeQueue, err := repositories.Queue.FindActiveByLoopID(ctx, loop.ID)
	if err != nil {
		return err
	}
	if activeQueue != nil {
		return nil
	}

	latestQueue, err := repositories.Queue.GetLatestByLoopID(ctx, loop.ID)
	if err != nil {
		return err
	}
	if latestQueue != nil {
		if latestQueue.Status == "queued" || latestQueue.Status == "running" {
			return nil
		}
		if latestQueue.DedupeKey != "" {
			activeByDedupe, err := repositories.Queue.FindActiveByDedupe(ctx, latestQueue.DedupeKey)
			if err != nil {
				return err
			}
			if activeByDedupe != nil {
				return nil
			}
		}

		replacement := *latestQueue
		replacement.ID = newRuntimeEventID()
		replacement.Status = "queued"
		replacement.AvailableAt = nowISO
		replacement.Attempts = 0
		replacement.ClaimedBy = nil
		replacement.ClaimedAt = nil
		replacement.StartedAt = nil
		replacement.FinishedAt = nil
		replacement.LastError = nil
		replacement.LastErrorKind = nil
		replacement.CreatedAt = nowISO
		replacement.UpdatedAt = nowISO
		return repositories.Queue.Upsert(ctx, replacement)
	}

	queueRecord, ok, err := buildRecoveryQueueItem(loop, nowISO, maxAttempts)
	if err != nil || !ok {
		return err
	}
	return repositories.Queue.Upsert(ctx, queueRecord)
}

func shouldInterruptStaleRunningRun(run storage.RunRecord, latestRun *storage.RunRecord, hasActiveAgent bool) bool {
	if run.Status != string(domain.RunStatusRunning) {
		return false
	}
	if latestRun == nil || latestRun.ID != run.ID {
		return true
	}
	if hasActiveAgent {
		return false
	}
	// Recovery runs during daemon startup, so a persisted running run without an
	// active agent execution is orphaned regardless of loop status or heartbeat.
	return true
}

func interruptRecoveryRun(ctx context.Context, repositories *storage.Repositories, run storage.RunRecord, loop storage.LoopRecord, nowISO string, message string) error {
	interrupted := run
	interrupted.Status = string(domain.RunStatusInterrupted)
	if interrupted.ErrorMessage == nil {
		interrupted.ErrorMessage = stringPtr(message)
	}
	interrupted.EndedAt = stringPtr(nowISO)
	interrupted.LastHeartbeatAt = stringPtr(nowISO)
	interrupted.UpdatedAt = nowISO
	if err := repositories.Runs.Upsert(ctx, interrupted); err != nil {
		return err
	}
	return appendSystemEvent(ctx, repositories, storage.EventLogRecord{
		ID:         newRuntimeEventID(),
		EventType:  "looperd.recovery.run_interrupted",
		ProjectID:  stringPtr(loop.ProjectID),
		LoopID:     stringPtr(loop.ID),
		RunID:      stringPtr(run.ID),
		EntityType: stringPtr("run"),
		EntityID:   stringPtr(run.ID),
		PayloadJSON: mustMarshalJSON(map[string]any{
			"previousStatus":  "running",
			"recoveredStatus": "interrupted",
		}),
		CreatedAt: nowISO,
	})
}

func buildRecoveryQueueItem(loop storage.LoopRecord, nowISO string, maxAttempts int64) (storage.QueueItemRecord, bool, error) {
	queueType := domain.LoopType(loop.Type)
	if queueType != domain.LoopTypePlanner && queueType != domain.LoopTypeReviewer && queueType != domain.LoopTypeFixer && queueType != domain.LoopTypeWorker {
		return storage.QueueItemRecord{}, false, nil
	}

	projectID := loop.ProjectID
	loopID := loop.ID
	queueRecord := storage.QueueItemRecord{
		ID:          newRuntimeEventID(),
		ProjectID:   &projectID,
		LoopID:      &loopID,
		Type:        loop.Type,
		TargetType:  loop.TargetType,
		TargetID:    strings.TrimSpace(derefString(loop.TargetID)),
		Repo:        loop.Repo,
		PRNumber:    loop.PRNumber,
		Status:      "queued",
		AvailableAt: nowISO,
		Attempts:    0,
		MaxAttempts: maxAttempts,
		CreatedAt:   nowISO,
		UpdatedAt:   nowISO,
	}

	switch queueType {
	case domain.LoopTypePlanner:
		repo := strings.TrimSpace(derefString(loop.Repo))
		issueNumber, err := parseIssueNumberFromTargetID(queueRecord.TargetID)
		if err != nil || repo == "" || loop.TargetType != string(domain.LoopTargetTypeIssue) {
			if err == nil {
				err = fmt.Errorf("planner loop requires repo and issue target")
			}
			return storage.QueueItemRecord{}, false, err
		}
		lockKey := fmt.Sprintf("issue:%s:%d", repo, issueNumber)
		payload := map[string]any{"issueNumber": issueNumber}
		payloadJSON := mustMarshalJSON(payload)
		queueRecord.TargetType = string(domain.LoopTargetTypeIssue)
		queueRecord.TargetID = lockKey
		queueRecord.Repo = &repo
		queueRecord.PRNumber = nil
		queueRecord.DedupeKey = fmt.Sprintf("planner:%s:%s:%s:%d", loop.ProjectID, loop.ID, repo, issueNumber)
		queueRecord.Priority = storage.QueuePriorityPlanner
		queueRecord.LockKey = &lockKey
		queueRecord.PayloadJSON = &payloadJSON
	case domain.LoopTypeReviewer:
		repo := strings.TrimSpace(derefString(loop.Repo))
		if repo == "" || loop.PRNumber == nil || loop.TargetType != string(domain.LoopTargetTypePullRequest) {
			return storage.QueueItemRecord{}, false, fmt.Errorf("reviewer loop requires repo and pull request target")
		}
		prNumber := *loop.PRNumber
		lockKey := fmt.Sprintf("pr:%s:%d", repo, prNumber)
		queueRecord.TargetType = string(domain.LoopTargetTypePullRequest)
		queueRecord.TargetID = lockKey
		queueRecord.Repo = &repo
		queueRecord.PRNumber = &prNumber
		queueRecord.DedupeKey = fmt.Sprintf("reviewer:%s:%s:%s:%d", loop.ProjectID, loop.ID, repo, prNumber)
		queueRecord.Priority = storage.QueuePriorityReviewer
		queueRecord.LockKey = &lockKey
	case domain.LoopTypeFixer:
		repo := strings.TrimSpace(derefString(loop.Repo))
		if repo == "" || loop.PRNumber == nil || loop.TargetType != string(domain.LoopTargetTypePullRequest) {
			return storage.QueueItemRecord{}, false, fmt.Errorf("fixer loop requires repo and pull request target")
		}
		prNumber := *loop.PRNumber
		lockKey := fmt.Sprintf("pr:%s:%d", repo, prNumber)
		queueRecord.TargetType = string(domain.LoopTargetTypePullRequest)
		queueRecord.TargetID = lockKey
		queueRecord.Repo = &repo
		queueRecord.PRNumber = &prNumber
		queueRecord.DedupeKey = fmt.Sprintf("fixer:%s", loop.ID)
		queueRecord.Priority = storage.QueuePriorityFixer
		queueRecord.LockKey = &lockKey
	case domain.LoopTypeWorker:
		payloadJSON := buildRecoveryWorkerPayloadJSON(loop.MetadataJSON)
		if payloadJSON != nil {
			queueRecord.PayloadJSON = payloadJSON
		}
		queueRecord.Priority = storage.QueuePriorityWorker
		lockKey := fmt.Sprintf("worker:%s", loop.ID)
		queueRecord.DedupeKey = fmt.Sprintf("worker:%s", loop.ID)
		if loop.TargetType == string(domain.LoopTargetTypeIssue) {
			repo := strings.TrimSpace(derefString(loop.Repo))
			issueNumber, err := parseIssueNumberFromTargetID(queueRecord.TargetID)
			if err != nil || repo == "" {
				if err == nil {
					err = fmt.Errorf("worker loop requires repo and issue target")
				}
				return storage.QueueItemRecord{}, false, err
			}
			lockKey = fmt.Sprintf("issue:%s:%d", repo, issueNumber)
			queueRecord.TargetType = string(domain.LoopTargetTypeIssue)
			queueRecord.TargetID = lockKey
			queueRecord.Repo = &repo
			queueRecord.PRNumber = nil
			queueRecord.DedupeKey = fmt.Sprintf("worker:%s:%s:%d", loop.ProjectID, repo, issueNumber)
		} else if loop.TargetType == string(domain.LoopTargetTypePullRequest) {
			repo := strings.TrimSpace(derefString(loop.Repo))
			if repo == "" || loop.PRNumber == nil {
				return storage.QueueItemRecord{}, false, fmt.Errorf("worker loop requires repo and prNumber")
			}
			prNumber := *loop.PRNumber
			lockKey = fmt.Sprintf("pr:%s:%d", repo, prNumber)
			queueRecord.TargetType = string(domain.LoopTargetTypePullRequest)
			queueRecord.TargetID = lockKey
			queueRecord.Repo = &repo
			queueRecord.PRNumber = &prNumber
			queueRecord.DedupeKey = fmt.Sprintf("worker:%s:%s:%d", loop.ProjectID, repo, prNumber)
		}
		queueRecord.LockKey = &lockKey
	}

	return queueRecord, true, nil
}

func buildRecoveryWorkerPayloadJSON(metadataJSON *string) *string {
	if metadataJSON == nil || strings.TrimSpace(*metadataJSON) == "" {
		return nil
	}
	metadata := map[string]any{}
	if err := json.Unmarshal([]byte(*metadataJSON), &metadata); err != nil {
		return nil
	}
	workerMeta, ok := metadata["worker"].(map[string]any)
	if !ok || len(workerMeta) == 0 {
		return nil
	}
	encoded, err := json.Marshal(workerMeta)
	if err != nil {
		return nil
	}
	text := string(encoded)
	return &text
}

func parseIssueNumberFromTargetID(targetID string) (int64, error) {
	parts := strings.Split(strings.TrimSpace(targetID), ":")
	if len(parts) != 3 || parts[0] != "issue" {
		return 0, fmt.Errorf("invalid issue target id %q", targetID)
	}
	var issueNumber int64
	if _, err := fmt.Sscanf(parts[2], "%d", &issueNumber); err != nil || issueNumber <= 0 {
		return 0, fmt.Errorf("invalid issue target id %q", targetID)
	}
	return issueNumber, nil
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

func (r *Runtime) executionMatchesProcess(ctx context.Context, execution storage.AgentExecutionRecord, pid int) (matches bool, running bool, err error) {
	processCommand, err := r.readProcessCommand(ctx, pid)
	if err != nil {
		return false, false, err
	}
	processCommand = strings.TrimSpace(processCommand)
	if processCommand == "" {
		return false, false, nil
	}
	expectedTokens, err := expectedExecutionCommandTokens(execution)
	if err != nil {
		return false, true, err
	}
	actualTokens := splitProcessCommand(processCommand)
	return commandPrefixMatches(expectedTokens, actualTokens), true, nil
}

func expectedExecutionCommandTokens(execution storage.AgentExecutionRecord) ([]string, error) {
	if execution.CommandJSON == nil || strings.TrimSpace(*execution.CommandJSON) == "" {
		return nil, fmt.Errorf("missing execution command metadata")
	}
	var payload struct {
		Command string   `json:"command"`
		Args    []string `json:"args"`
	}
	if err := json.Unmarshal([]byte(*execution.CommandJSON), &payload); err != nil {
		return nil, fmt.Errorf("parse execution command metadata: %w", err)
	}
	if strings.TrimSpace(payload.Command) == "" {
		return nil, fmt.Errorf("missing execution command")
	}
	tokens := make([]string, 0, len(payload.Args)+1)
	tokens = append(tokens, payload.Command)
	tokens = append(tokens, payload.Args...)
	return tokens, nil
}

func commandPrefixMatches(expected, actual []string) bool {
	if len(expected) == 0 || len(actual) == 0 {
		return false
	}
	if filepath.Base(expected[0]) != filepath.Base(actual[0]) {
		return false
	}
	if len(expected) == 1 {
		return true
	}
	if len(actual) < len(expected)-1 {
		return false
	}
	for i := 1; i < len(expected)-1; i++ {
		if expected[i] != actual[i] {
			return false
		}
	}
	return strings.Join(actual[len(expected)-1:], " ") == expected[len(expected)-1]
}

func splitProcessCommand(command string) []string {
	command = strings.TrimSpace(command)
	if command == "" {
		return nil
	}

	tokens := make([]string, 0)
	var current strings.Builder
	var quote rune
	escaped := false

	flush := func() {
		if current.Len() == 0 {
			return
		}
		tokens = append(tokens, current.String())
		current.Reset()
	}

	for _, r := range command {
		if escaped {
			current.WriteRune(r)
			escaped = false
			continue
		}

		switch {
		case r == '\\' && quote != 0:
			escaped = true
		case quote != 0:
			if r == quote {
				quote = 0
				continue
			}
			current.WriteRune(r)
		case r == '\'' || r == '"':
			quote = r
		case r == ' ' || r == '\t' || r == '\n':
			flush()
		default:
			current.WriteRune(r)
		}
	}

	flush()
	return tokens
}

func defaultReadProcessCommand(ctx context.Context, pid int) (string, error) {
	cmd := exec.CommandContext(ctx, "ps", "-p", fmt.Sprintf("%d", pid), "-o", "command=")
	output, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return "", nil
		}
		return "", fmt.Errorf("inspect process %d with ps: %w", pid, err)
	}
	return strings.TrimSpace(string(output)), nil
}

func defaultSignalProcess(pid int, signal syscall.Signal) error {
	return syscall.Kill(pid, signal)
}

func (r *Runtime) signalAgentProcessGroup(pid int, signal syscall.Signal) error {
	if r.signalProcess == nil {
		return nil
	}
	if err := r.signalProcess(-pid, signal); err != nil {
		if !errors.Is(err, syscall.ESRCH) {
			return err
		}
		return r.signalProcess(pid, signal)
	}
	return nil
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

const maxReviewerAutoRecoveryAttempts = 3

type runtimeReviewerCheckpoint struct {
	ResumePolicy string `json:"resumePolicy,omitempty"`
	Detail       *struct {
		State          string           `json:"state,omitempty"`
		IsDraft        bool             `json:"isDraft,omitempty"`
		ReviewDecision string           `json:"reviewDecision,omitempty"`
		Labels         []string         `json:"labels,omitempty"`
		HeadSHA        string           `json:"headSha,omitempty"`
		CurrentLogin   string           `json:"currentLogin,omitempty"`
		Reviews        []map[string]any `json:"reviews,omitempty"`
	} `json:"detail,omitempty"`
}

type runtimeReviewerRecoveryPolicy struct {
	includeDrafts    bool
	stopOnApproved   bool
	stopOnReadyLabel bool
	currentLogin     string
}

func (r *Runtime) currentReviewerLoginForRecovery(ctx context.Context, repositories *storage.Repositories, githubGateway *githubinfra.Gateway, loop storage.LoopRecord, latestRun *storage.RunRecord, policy runtimeReviewerRecoveryPolicy) (string, bool) {
	if githubGateway == nil || !policy.stopOnApproved || latestRun == nil || strings.TrimSpace(loop.ProjectID) == "" {
		return "", false
	}
	checkpoint := parseRuntimeReviewerCheckpoint(latestRun.CheckpointJSON)
	if checkpoint.Detail == nil || len(checkpoint.Detail.Reviews) == 0 {
		return "", false
	}
	project, err := repositories.Projects.GetByID(ctx, loop.ProjectID)
	if err != nil {
		if r.logger != nil {
			r.logger.Warn("failed to load project for reviewer recovery login refresh", map[string]any{"loopId": loop.ID, "projectId": loop.ProjectID, "error": err.Error()})
		}
		return "", false
	}
	if project == nil || strings.TrimSpace(project.RepoPath) == "" {
		return "", false
	}
	loginCtx, cancel := context.WithTimeout(ctx, reviewerRecoveryLoginTimeout)
	defer cancel()
	login, err := githubGateway.GetCurrentUserLogin(loginCtx, project.RepoPath)
	if err != nil {
		if r.logger != nil {
			r.logger.Warn("failed to refresh reviewer login during recovery", map[string]any{"loopId": loop.ID, "projectId": loop.ProjectID, "error": err.Error()})
		}
		return "", false
	}
	login = strings.ToLower(strings.TrimSpace(login))
	if login == "" {
		return "", false
	}
	return login, true
}

func canRefreshReviewerLoginForRecovery(loop storage.LoopRecord, latestRun *storage.RunRecord) bool {
	return loop.Type == string(domain.LoopTypeReviewer) && loop.Status == "failed" && latestRun != nil && latestRun.Status == "failed"
}

func reviewerRecoveryNeedsFreshLogin(loop storage.LoopRecord, latestRun *storage.RunRecord, policy runtimeReviewerRecoveryPolicy) bool {
	if !canRefreshReviewerLoginForRecovery(loop, latestRun) || !policy.stopOnApproved || latestRun == nil {
		return false
	}
	checkpoint := parseRuntimeReviewerCheckpoint(latestRun.CheckpointJSON)
	return checkpoint.Detail != nil && len(checkpoint.Detail.Reviews) > 0
}

func shouldAutoRecoverFailedReviewerLoop(loop storage.LoopRecord, latestRun *storage.RunRecord, latestQueue *storage.QueueItemRecord, policy runtimeReviewerRecoveryPolicy) bool {
	if loop.Type != string(domain.LoopTypeReviewer) || loop.Status != "failed" || latestRun == nil || latestRun.Status != "failed" || latestQueue == nil || latestQueue.Status != "failed" {
		return false
	}
	meta := parseRuntimeJSONObject(loop.MetadataJSON)
	if manual, _ := meta["manual"].(bool); manual {
		return false
	}
	if !runtimeReviewerLoopEnabled(meta) {
		return false
	}
	loopMeta := runtimeReviewerLoopMetadata(meta)
	if reason, _ := runtimeStringFromAny(loopMeta["terminationReason"]); reason != "" && !isDeprecatedReviewerLoopBudgetReason(reason) {
		return false
	}
	if runtimeIntFromAny(loopMeta["autoRecoveryAttempts"]) >= maxReviewerAutoRecoveryAttempts {
		return false
	}
	checkpoint := parseRuntimeReviewerCheckpoint(latestRun.CheckpointJSON)
	if checkpoint.ResumePolicy == "manual_intervention" {
		return false
	}
	queueKind := derefString(latestQueue.LastErrorKind)
	queueMessage := derefString(latestQueue.LastError)
	if queueKind == "manual_intervention" {
		return false
	}
	if checkpoint.Detail == nil {
		return false
	}
	if strings.ToLower(strings.TrimSpace(checkpoint.Detail.State)) != "open" {
		return false
	}
	if !policy.includeDrafts && checkpoint.Detail.IsDraft {
		return false
	}
	currentLogin := checkpoint.Detail.CurrentLogin
	if strings.TrimSpace(policy.currentLogin) != "" {
		currentLogin = policy.currentLogin
	}
	if policy.stopOnApproved && runtimeReviewerCheckpointApprovedForRecovery(checkpoint.Detail.Reviews, currentLogin, checkpoint.Detail.HeadSHA, checkpoint.Detail.ReviewDecision) {
		return false
	}
	if policy.stopOnReadyLabel && specpr.HasLabel(checkpoint.Detail.Labels, specpr.ReadyLabel) {
		return false
	}
	failureSummary := firstNonEmpty(derefString(latestRun.Summary), derefString(latestRun.ErrorMessage), queueMessage)
	return (queueKind == "retryable_after_resume" && (checkpoint.ResumePolicy == "restart_from_discover" || checkpoint.ResumePolicy == "rerun_review")) || isRuntimeRetryableTransientWithRemainingAttempts(*latestQueue) || (isKnownReviewerRediscoveryGuardrail(failureSummary) && isRuntimeReviewerRediscoveryRunStep(latestRun))
}

func requeueFailedReviewerQueueItemForRecovery(ctx context.Context, repositories *storage.Repositories, loopID string, latestQueue *storage.QueueItemRecord, queuedAt string) (int64, error) {
	if latestQueue != nil && isRuntimeRetryableTransientWithRemainingAttempts(*latestQueue) {
		return repositories.Queue.RequeueFailedByIDWithAttempts(ctx, loopID, latestQueue.ID, queuedAt, latestQueue.Attempts)
	}
	if latestQueue == nil {
		return 0, nil
	}
	return repositories.Queue.RequeueFailedByID(ctx, loopID, latestQueue.ID, queuedAt)
}

func isRuntimeRetryableTransientWithRemainingAttempts(queue storage.QueueItemRecord) bool {
	if derefString(queue.LastErrorKind) != "retryable_transient" {
		return false
	}
	nextAttempts := queue.Attempts + 1
	return queue.MaxAttempts > 0 && nextAttempts < queue.MaxAttempts
}

func autoRecoveredReviewerLoop(loop storage.LoopRecord, nowISO string) storage.LoopRecord {
	updated := loop
	updated.Status = "queued"
	updated.NextRunAt = stringPtr(nowISO)
	updated.UpdatedAt = nowISO
	meta := parseRuntimeJSONObject(updated.MetadataJSON)
	loopMeta := runtimeReviewerLoopMetadata(meta)
	loopMeta["status"] = "active"
	loopMeta["lastStatus"] = "auto_recovered"
	loopMeta["autoRecoveryAttempts"] = runtimeIntFromAny(loopMeta["autoRecoveryAttempts"]) + 1
	delete(loopMeta, "terminationReason")
	removeDeprecatedReviewerLoopBudgetMetadata(loopMeta)
	meta["loop"] = loopMeta
	encoded, err := json.Marshal(meta)
	if err == nil {
		text := string(encoded)
		updated.MetadataJSON = &text
	}
	return updated
}

func parseRuntimeReviewerCheckpoint(value *string) runtimeReviewerCheckpoint {
	if value == nil || strings.TrimSpace(*value) == "" {
		return runtimeReviewerCheckpoint{}
	}
	var checkpoint runtimeReviewerCheckpoint
	_ = json.Unmarshal([]byte(*value), &checkpoint)
	return checkpoint
}

func parseRuntimeJSONObject(value *string) map[string]any {
	if value == nil || strings.TrimSpace(*value) == "" {
		return map[string]any{}
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(*value), &decoded); err != nil || decoded == nil {
		return map[string]any{}
	}
	return decoded
}

func runtimeReviewerLoopMetadata(meta map[string]any) map[string]any {
	loopMeta, _ := meta["loop"].(map[string]any)
	if loopMeta == nil {
		loopMeta = map[string]any{}
	}
	return loopMeta
}

func runtimeReviewerLoopEnabled(meta map[string]any) bool {
	if enabled, ok := meta["followUpdates"].(bool); ok {
		return enabled
	}
	if loopMeta, ok := meta["loop"].(map[string]any); ok {
		if enabled, ok := loopMeta["enabled"].(bool); ok {
			return enabled
		}
	}
	return false
}

func runtimeIntFromAny(value any) int {
	switch typed := value.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	default:
		return 0
	}
}

func runtimeStringFromAny(value any) (string, bool) {
	text, ok := value.(string)
	return text, ok
}

func runtimeHasApprovedReviewByAuthorForHead(reviews []map[string]any, login string, headSHA string) bool {
	login = strings.ToLower(strings.TrimSpace(login))
	headSHA = strings.TrimSpace(headSHA)
	if login == "" || headSHA == "" {
		return false
	}
	for _, review := range reviews {
		author, ok := review["author"].(map[string]any)
		if !ok {
			continue
		}
		authorLogin, ok := runtimeStringFromAny(author["login"])
		if !ok || strings.ToLower(strings.TrimSpace(authorLogin)) != login {
			continue
		}
		state, _ := runtimeStringFromAny(review["state"])
		if !strings.EqualFold(strings.TrimSpace(state), "APPROVED") {
			continue
		}
		commit, ok := review["commit"].(map[string]any)
		if !ok {
			continue
		}
		if oid, ok := runtimeStringFromAny(commit["oid"]); ok && strings.TrimSpace(oid) == headSHA {
			return true
		}
	}
	return false
}

func runtimeReviewerCheckpointApprovedForRecovery(reviews []map[string]any, login string, headSHA string, reviewDecision string) bool {
	if runtimeHasApprovedReviewByAuthorForHead(reviews, login, headSHA) {
		return true
	}
	return false
}

func isDeprecatedReviewerLoopBudgetReason(reason string) bool {
	switch strings.TrimSpace(reason) {
	case "max_iterations_per_pr", "max_iterations_per_head", "max_wall_clock", "max_consecutive_failures", "max_agent_executions_per_pr":
		return true
	default:
		return false
	}
}

func removeDeprecatedReviewerLoopBudgetMetadata(loopMeta map[string]any) {
	for _, key := range deprecatedReviewerLoopBudgetMetadataKeys {
		delete(loopMeta, key)
	}
}

var deprecatedReviewerLoopBudgetMetadataKeys = []string{
	"maxIterationsPerPR",
	"maxIterationsPerHead",
	"maxWallClockSeconds",
	"maxConsecutiveFailures",
	"maxAgentExecutionsPerPR",
}

func isKnownReviewerRediscoveryGuardrail(message string) bool {
	return strings.Contains(message, "PR head changed before publish") || strings.Contains(message, "review request removed before publish")
}

func isRuntimeReviewerRediscoveryRunStep(run *storage.RunRecord) bool {
	if run == nil || run.CurrentStep == nil {
		return false
	}
	switch strings.TrimSpace(*run.CurrentStep) {
	case "publish", "review", "thread_resolution":
		return true
	default:
		return false
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func shouldRequeueLoop(loop storage.LoopRecord, latestRun *storage.RunRecord, latestRunHasActiveAgent bool) bool {
	if loop.Status == "paused" {
		return false
	}
	if loop.Status == "completed" || loop.Status == "failed" || loop.Status == "terminated" || loop.Status == "stopped" {
		return false
	}
	if latestRun == nil {
		return loop.Status == "running"
	}
	if latestRun.Status == string(domain.RunStatusRunning) && latestRunHasActiveAgent {
		return false
	}

	return loop.Status == "running" || latestRun.Status == "interrupted"
}

func derefRunID(run *storage.RunRecord) string {
	if run == nil {
		return ""
	}
	return run.ID
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
