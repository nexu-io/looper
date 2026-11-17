package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nexu-io/looper/internal/bootstrap"
	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/domain"
	"github.com/nexu-io/looper/internal/executionhost"
	"github.com/nexu-io/looper/internal/hostingidentity"
	gitinfra "github.com/nexu-io/looper/internal/infra/git"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/infra/specpr"
	"github.com/nexu-io/looper/internal/loops"
	networkclient "github.com/nexu-io/looper/internal/network/client"
	"github.com/nexu-io/looper/internal/projects"
	"github.com/nexu-io/looper/internal/runs"
	"github.com/nexu-io/looper/internal/storage"
	"github.com/nexu-io/looper/internal/webhookforward"
)

type OpenSQLiteCoordinatorFunc func(context.Context, string, storage.SQLiteCoordinatorOptions) (*storage.SQLiteCoordinator, error)

type SyncConfiguredProjectsFunc func(context.Context, *projects.Service, config.Config, time.Time) error

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

type StaleRunReconcileSummary = executionhost.StaleRunReconcileSummary

type staleRunReconcileMode = executionhost.StaleRunReconcileMode

const (
	staleRunReconcileModeStartup = executionhost.StaleRunReconcileModeStartup
	staleRunReconcileModeLive    = executionhost.StaleRunReconcileModeLive
	staleRunReconcileModeManual  = executionhost.StaleRunReconcileModeManual
)

type RecoveryOrphanAgentCleanup = executionhost.RecoveryOrphanAgentCleanup

type Options struct {
	Config config.Config
	// InitialConfig and ReloadConfig are supplied by bootstrap so hot reloads
	// replay the daemon's exact startup precedence (file, environment, and CLI).
	// Tests and embedders that omit ReloadConfig simply run without a watcher.
	InitialConfig        config.LoadedFileConfig
	ReloadConfig         func() (config.LoadedFileConfig, error)
	LoadConfigAt         func(string) (config.LoadedFileConfig, error)
	ConfigReloadInterval time.Duration
	// ConfigPath is the daemon-loaded config file path (from --config /
	// LOOPER_CONFIG resolution). Runtime config management patches this source;
	// trusted review-submit children receive a separate sanitized run snapshot.
	ConfigPath                  string
	Logger                      bootstrap.Logger
	Now                         func() time.Time
	ShutdownTimeout             time.Duration
	WorktreeCleanupInitialDelay time.Duration
	OpenSQLiteCoordinator       OpenSQLiteCoordinatorFunc
	SyncConfiguredProjects      SyncConfiguredProjectsFunc
	RunSchedulerTick            RunSchedulerTickFunc
	ReadProcessCommand          ReadProcessCommandFunc
	SignalProcess               SignalProcessFunc
	DeferRecovery               bool
}

type Services struct {
	Coordinator      *storage.SQLiteCoordinator
	Repositories     *storage.Repositories
	Projects         *projects.Service
	Loops            *loops.Service
	Runs             *runs.Service
	ActiveExecutions *ActiveExecutionRegistry
}

type WebhookForwarder interface {
	Forward(context.Context, webhookforward.DeliveryRequest) (webhookforward.ForwardResult, error)
	Stats() webhookforward.Stats
	// CancelExecute aborts in-flight webhook discovery without waiting for drain.
	// BeginShutdown and MarkDegraded call this so admission-closed races cannot
	// still enqueue after a one-time AllowExecute pass.
	CancelExecute()
	Close()
}

type Runtime struct {
	config     config.Config
	configPath string
	logger     bootstrap.Logger
	now        func() time.Time

	// Injectable only to exercise the full Plane reconciliation state machines
	// without a real CLI/network. Production uses workerhost.PlaneDocForProject.
	planeDocFactory planeDocFactory
	// Test seam for the approval classifier; production builds the configured agent.
	specApprovalJudge specApprovalJudgeFunc

	configReloadMu       sync.Mutex
	configBoundary       sync.RWMutex
	loadedConfig         config.LoadedFileConfig
	reloadConfig         func() (config.LoadedFileConfig, error)
	loadConfigAt         func(string) (config.LoadedFileConfig, error)
	configReloadInterval time.Duration
	configReloadStop     chan struct{}
	configReloadDone     chan struct{}
	configReloadStatus   ConfigReloadStatus

	openSQLiteCoordinator  OpenSQLiteCoordinatorFunc
	syncConfiguredProjects SyncConfiguredProjectsFunc
	runSchedulerTick       RunSchedulerTickFunc
	defaultSchedulerTick   RunSchedulerTickFunc
	defaultSchedulerClaim  RunSchedulerTickFunc
	customSchedulerTick    bool
	readProcessCommand     ReadProcessCommandFunc
	signalProcess          SignalProcessFunc
	shutdownTimeout        time.Duration
	deferRecovery          bool

	mu                          sync.RWMutex
	startedAt                   *time.Time
	recovery                    RecoverySummary
	stopped                     bool
	services                    Services
	startErr                    error
	startOnce                   sync.Once
	shutdownOnce                sync.Once
	shutdownCh                  chan struct{}
	schedulerStop               chan struct{}
	schedulerDone               chan struct{}
	schedulerWake               chan struct{}
	schedulerClaimWake          chan struct{}
	schedulerCancel             context.CancelFunc
	schedulerTasks              *schedulerTaskTracker
	worktreeCleanupStop         chan struct{}
	worktreeCleanupDone         chan struct{}
	worktreeCleanupCancel       context.CancelFunc
	worktreeCleanupRunning      bool
	worktreeCleanupInitialDelay time.Duration
	worktreeCleanupStatus       WorktreeCleanupStatus
	recoveryCancel              context.CancelFunc
	recoveryDone                chan struct{}
	// humanAttentionNotifyCancel/Ctx are the shared cancelable parent for all
	// best-effort human-attention delivery (post-claim + recovery rescan).
	// humanAttentionNotifyWG drains those goroutines before SQLite close so
	// notification transports and persistence cannot race coordinator.Close.
	// humanAttentionNotifyDone is the latest recovery-rescan generation only
	// (WaitForHumanAttentionRecoveryNotify / API fixtures).
	humanAttentionNotifyCancel context.CancelFunc
	humanAttentionNotifyCtx    context.Context
	humanAttentionNotifyWG     sync.WaitGroup
	humanAttentionNotifyDone   chan struct{}
	activeExecutions           *ActiveExecutionRegistry
	// beginShutdownHooks run during BeginShutdown after admission flips to
	// stopping and before non-agent drain waits (cancel producers first).
	beginShutdownHooks []func()
	projectCatalog     *projects.Catalog
	githubGateway      *githubinfra.Gateway
	webhook            *webhookRuntime
	webhookDaemonLock  *daemonLock
	webhookForwarder   WebhookForwarder
	networkManager     runtimeNetworkManager
	schedulerDisabled  bool
	startupReadyOnce   sync.Once
	startupReadyErr    error
	// ownershipAcquired remains true after CompleteStartup succeeds so stop
	// still writes looperd.stopped. Admission is the sole ready Authority;
	// this flag is not a mutation/claim gate.
	ownershipAcquired bool
	admission         *Admission

	// shutdownDrainErr is set by BeginShutdown when producer/handle drain fails.
	// Stop retains SQLite when non-nil (ADR-0015 / #577).
	shutdownDrainErr error
	// storageRetained is true when Stop skipped coordinator.Close after a
	// drain failure so undrained ownership is not closed under SQLite.
	storageRetained bool
}

type runtimeNetworkManager interface {
	Start(context.Context) error
	Stop()
	Status() networkclient.Status
	UpdateConfig(config.Config)
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

	projectCatalog := projects.NewCatalog(options.Config)
	loadedConfig := options.InitialConfig
	if reflect.DeepEqual(loadedConfig.Config, config.Config{}) {
		loadedConfig.Config = options.Config
		loadedConfig.Metadata.ConfigPath = strings.TrimSpace(options.ConfigPath)
	}
	reloadInterval := options.ConfigReloadInterval
	if reloadInterval <= 0 {
		reloadInterval = time.Second
	}
	rt := &Runtime{
		config:                      options.Config,
		configPath:                  strings.TrimSpace(options.ConfigPath),
		logger:                      options.Logger,
		now:                         now,
		loadedConfig:                loadedConfig,
		reloadConfig:                options.ReloadConfig,
		loadConfigAt:                options.LoadConfigAt,
		configReloadInterval:        reloadInterval,
		openSQLiteCoordinator:       openSQLiteCoordinator,
		syncConfiguredProjects:      syncConfiguredProjects,
		runSchedulerTick:            runSchedulerTick,
		customSchedulerTick:         customSchedulerTick,
		readProcessCommand:          readProcessCommand,
		signalProcess:               signalProcess,
		shutdownTimeout:             shutdownTimeout,
		worktreeCleanupInitialDelay: options.WorktreeCleanupInitialDelay,
		deferRecovery:               options.DeferRecovery,
		recovery:                    createEmptyRecoverySummary(),
		shutdownCh:                  make(chan struct{}),
		activeExecutions:            NewActiveExecutionRegistry(),
		projectCatalog:              projectCatalog,
		webhook:                     newWebhookRuntime(options.Config, options.Logger, now),
		admission:                   NewAdmission(),
	}
	// Project daemon Admission onto agent spawn leases so cmd.Start is refused
	// while starting/stopping/degraded (#576 + #575).
	rt.activeExecutions.SetAllowSpawn(rt.AllowClaim)
	// Hard agent_executions observation failures close admission until process
	// restart (#578 / ADR-0015 R5). Prefer split-brain stop over silent continue.
	rt.activeExecutions.SetOnHardPersistFailure(func(err error) {
		reason := "agent execution persistence hard failure"
		if err != nil {
			reason = reason + ": " + err.Error()
		}
		if markErr := rt.MarkDegraded(reason); markErr != nil && rt.logger != nil {
			rt.logger.Warn("failed to mark admission degraded after persistence failure", map[string]any{
				"error":  markErr.Error(),
				"reason": reason,
			})
		} else if rt.logger != nil {
			rt.logger.Error("daemon admission degraded after agent execution persistence failure", map[string]any{
				"reason": reason,
			})
		}
	})
	if rt.webhook != nil {
		rt.webhook.forwarder = rt.WebhookForwarder
		// Tunnel listener is not behind the API mutation gate; consult the same
		// admission Authority before accepting deliveries (#583). Worker-side
		// discovery rechecks via webhookforward.Options.AllowExecute.
		rt.webhook.allowForward = rt.AllowMutations
	}
	if !customSchedulerTick {
		rt.runSchedulerTick = rt.executeDefaultSchedulerTick
	}
	return rt
}

func Start(ctx context.Context, deps bootstrap.RuntimeDependencies) (bootstrap.Runtime, error) {
	rt := New(Options{
		Config:        deps.Config,
		InitialConfig: deps.InitialConfig,
		ReloadConfig:  deps.ReloadConfig,
		LoadConfigAt:  deps.LoadConfigAt,
		Logger:        deps.Logger,
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

		// Close admission and cancel work-producing contexts before draining
		// producers (ADR-0015 shutdown order). Use BeginShutdown so direct
		// Runtime.Stop matches the daemon path: scheduler, deferred recovery,
		// and in-flight webhook discovery are canceled before any waits.
		r.BeginShutdown(reason)

		r.stopConfigReloadLoop()
		r.stopDeferredReviewerRecovery()
		r.stopHumanAttentionRecoveryNotify()
		r.stopWorktreeCleanupLoop()
		r.stopSchedulerLoop()
		r.stopWebhookRuntime()

		// Re-collect non-agent containment drain failures reported while
		// producers finished after BeginShutdown (shell validation / trusted
		// review cancel paths). Agent failures were already joined above.
		if r.activeExecutions != nil {
			if err := r.activeExecutions.NonAgentDrainErr(); err != nil {
				r.mu.Lock()
				r.shutdownDrainErr = errors.Join(r.shutdownDrainErr, err)
				r.mu.Unlock()
			}
		}

		r.mu.Lock()
		r.stopped = true
		forwarder := r.webhookForwarder
		r.webhookForwarder = nil
		networkManager := r.networkManager
		r.networkManager = nil
		coordinator := r.services.Coordinator
		repositories := r.services.Repositories
		ownershipAcquired := r.ownershipAcquired
		drainErr := r.shutdownDrainErr
		r.mu.Unlock()

		if ownershipAcquired && repositories != nil {
			if err := r.appendStoppedEvent(context.Background(), repositories, reason); err != nil && r.logger != nil {
				r.logger.Warn("looperd runtime stop event failed", map[string]any{"error": err.Error()})
			}
		}

		// Independent infra (forwarder/network) still stop on drain failure;
		// they are not Supervisor domain and must not block retain-storage.
		if forwarder != nil {
			forwarder.Close()
		}
		if networkManager != nil {
			networkManager.Stop()
		}

		// #577: retain SQLite when containment drain failed. Never report
		// graceful success with undrained ownership under a closed DB.
		if drainErr != nil {
			r.mu.Lock()
			r.storageRetained = true
			r.mu.Unlock()
			close(r.shutdownCh)
			if r.logger != nil {
				r.logger.Error("looperd runtime stop retained storage after drain failure", map[string]any{
					"reason": reason,
					"error":  drainErr.Error(),
				})
			}
			return
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

// BeginShutdown transitions admission to stopping without closing storage.
// Daemon stop drains HTTP ingress after this so mutations/claims stop first.
// Also cancels the scheduler context so an in-flight full tick observes
// cancellation during the HTTP drain window before Runtime.Stop closes the
// loop; work-producing lanes still recheck AllowClaim as the Authority.
// Cancels deferred reviewer recovery so a post-ready recovery goroutine cannot
// still requeue loops/queue items after admission is already stopping; the
// wait for recovery exit remains in Runtime.Stop via stopDeferredReviewerRecovery.
// Cancels webhook-forward discovery so process exit can abort in-flight
// CreateOrGetActiveByDedupe promptly (sticky MarkDegraded does not cancel
// webhook execute — accepted/202 deliveries must still complete).
//
// Shutdown order (ADR-0015 / #577): close admission → cancel producers →
// confirmed-drain handles (agents + tracked non-agent shell/trusted-review).
// Producer cancel must run before ActiveExecutionRegistry.BeginShutdown waits
// on tracked non-agent handles: validation shell.Run only enters Kill after its
// owner ctx is canceled. Waiting first would burn the full kill budget then
// force-kill instead of cancel/drain promptly.
// SQLite close happens only in Stop after drain succeeds; drain failure is
// recorded for retain-storage. Non-agent Kill/Drain failures that finish after
// this returns are re-collected in Stop via NonAgentDrainErr.
func (r *Runtime) BeginShutdown(reason string) {
	if r == nil {
		return
	}
	// Snapshot cancel targets under r.mu first, then flip stopping + invoke
	// cancels under admission.mu. Do not take r.mu while holding admission.mu
	// (other paths lock r.mu then read admission — that order would deadlock).
	cancels := r.snapshotWorkProducerCancels()
	hooks := r.snapshotBeginShutdownHooks()
	_ = r.admission.BeginShutdownThen(reason, func() {
		cancels.invokeForShutdown()
		for _, hook := range hooks {
			if hook != nil {
				hook()
			}
		}
	})
	// Close agent spawn admission and confirmed-drain live handles — agents and
	// tracked Supervisor-owned non-agents (#576/#577). Agent leases cancel via
	// registry; non-agent owners were canceled above.
	if r.activeExecutions != nil {
		if err := r.activeExecutions.BeginShutdown(reason); err != nil {
			r.mu.Lock()
			r.shutdownDrainErr = errors.Join(r.shutdownDrainErr, err)
			r.mu.Unlock()
		}
	}
}

// OnBeginShutdown registers a hook invoked during BeginShutdown after admission
// flips to stopping and work-producer cancels run, but before the registry
// waits on tracked non-agent handles. Used for Supervisor-owned non-agent
// work that is not scheduler/recovery (e.g. model catalog shared probes).
// Hooks must be safe to call more than once (idempotent cancel).
func (r *Runtime) OnBeginShutdown(hook func()) {
	if r == nil || hook == nil {
		return
	}
	r.mu.Lock()
	r.beginShutdownHooks = append(r.beginShutdownHooks, hook)
	r.mu.Unlock()
}

func (r *Runtime) snapshotBeginShutdownHooks() []func() {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.beginShutdownHooks) == 0 {
		return nil
	}
	out := make([]func(), len(r.beginShutdownHooks))
	copy(out, r.beginShutdownHooks)
	return out
}

// workProducerCancels is a lock-free snapshot of cancel targets so
// MarkDegraded/BeginShutdown can invoke them while holding admission.mu
// without re-entering r.mu (lock-order safety).
type workProducerCancels struct {
	scheduler      context.CancelFunc
	recovery       context.CancelFunc
	cleanup        context.CancelFunc
	humanAttention context.CancelFunc
	forwarder      interface{ CancelExecute() }
}

// invokeForDegrade cancels sticky-degrade producers but leaves webhook execute
// live: Forward may already have returned accepted/202 for queued discovery,
// and GitHub will not retry that delivery while the daemon stays up.
// Human-attention recovery rescan is left running on sticky degrade so an
// already-scheduled post-recovery alert can still deliver while the daemon stays up.
func (c workProducerCancels) invokeForDegrade() {
	if c.scheduler != nil {
		c.scheduler()
	}
	if c.recovery != nil {
		c.recovery()
	}
	if c.cleanup != nil {
		c.cleanup()
	}
}

// invokeForShutdown cancels all work producers including webhook discovery so
// process exit can abort in-flight CreateOrGetActiveByDedupe promptly.
// Also cancels human-attention delivery (recovery rescan + post-claim) so
// a slow notification transport cannot block process exit beyond
// stopHumanAttentionRecoveryNotify's wait budget.
func (c workProducerCancels) invokeForShutdown() {
	c.invokeForDegrade()
	if c.humanAttention != nil {
		c.humanAttention()
	}
	if c.forwarder != nil {
		c.forwarder.CancelExecute()
	}
}

func (r *Runtime) snapshotWorkProducerCancels() workProducerCancels {
	if r == nil {
		return workProducerCancels{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return workProducerCancels{
		scheduler:      r.schedulerCancel,
		recovery:       r.recoveryCancel,
		cleanup:        r.worktreeCleanupCancel,
		humanAttention: r.humanAttentionNotifyCancel,
		forwarder:      r.webhookForwarder,
	}
}

// cancelWorkProducers aborts in-flight scheduler ticks, deferred recovery,
// worktree cleanup, and (for process-exit paths) webhook discovery. Prefer
// invokeForDegrade vs invokeForShutdown at the call site: sticky degrade must
// not CancelExecute accepted webhook work.
func (r *Runtime) cancelWorkProducers() {
	r.snapshotWorkProducerCancels().invokeForShutdown()
}

// StorageRetained reports whether Stop skipped SQLite close after a drain
// failure (ADR-0015 / #577). Operators must not treat stop as graceful success.
func (r *Runtime) StorageRetained() bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.storageRetained
}

// ShutdownDrainError returns the drain failure recorded during BeginShutdown,
// if any.
func (r *Runtime) ShutdownDrainError() error {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.shutdownDrainErr
}

// AdmissionState returns the authoritative live admission state.
func (r *Runtime) AdmissionState() AdmissionState {
	if r == nil || r.admission == nil {
		return AdmissionStopping
	}
	return r.admission.State()
}

// AllowMutations is the HTTP mutation readiness projection of admission.
func (r *Runtime) AllowMutations() error {
	if r == nil || r.admission == nil {
		return ErrAdmissionStopping
	}
	return r.admission.AllowMutations()
}

// AllowClaim is the scheduler work-producing projection of admission
// (full tick + durable claims).
func (r *Runtime) AllowClaim() error {
	if r == nil || r.admission == nil {
		return ErrAdmissionStopping
	}
	return r.admission.AllowClaim()
}

// WithAllowClaim runs fn only while claim admission is open, holding the
// admission mutex for the full duration of fn so MarkDegraded/BeginShutdown
// cannot interleave with the critical section (webhook accept + enqueue).
func (r *Runtime) WithAllowClaim(fn func()) error {
	if r == nil || r.admission == nil {
		return ErrAdmissionStopping
	}
	return r.admission.WithAllowWork(fn)
}

// MarkDegraded sticks admission until process restart and cancels work-producing
// contexts (scheduler, recovery, cleanup) so new discovery/claims/cleanup that
// already passed AllowClaim cannot complete after the transition. Unlike
// BeginShutdown, this does not drain active agent handles and does not
// CancelExecute webhook workers: Forward may already have returned accepted/202
// for in-memory queue entries, and sticky degrade leaves the daemon up with no
// GitHub retry. New webhook accepts are still refused via AllowExecute /
// AllowExecuteWhile. There is no clear-and-resume path: canceled producer
// contexts are not recreated; operators must restart looperd.
func (r *Runtime) MarkDegraded(reason string) error {
	if r == nil || r.admission == nil {
		return ErrAdmissionStopping
	}
	// Snapshot cancel targets under r.mu, then hold admission.mu across the
	// degraded transition + cancel invoke so there is no window where admission
	// is already closed but cleanup/scheduler contexts are still live. A
	// point-in-time AllowClaim winner must either start remove while still
	// ready, or observe cancel after close — never start git worktree remove
	// after close with a live context. Webhook execCtx stays live so accepted
	// deliveries can still finish CreateOrGetActiveByDedupe.
	cancels := r.snapshotWorkProducerCancels()
	return r.admission.TransitionThen(AdmissionDegraded, reason, cancels.invokeForDegrade)
}

func (r *Runtime) WaitForShutdown() {
	<-r.shutdownCh
}

func derefString(value *string) string {
	return executionhost.DerefString(value)
}

func (r *Runtime) Services() Services {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return r.services
}

// Config returns the current runtime configuration with Projects materialized
// from the authoritative Project Catalog.
func (r *Runtime) Config() config.Config {
	if r.projectCatalog != nil {
		return r.projectCatalog.Snapshot()
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.config
}

func (r *Runtime) StartedAt() (time.Time, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if r.startedAt == nil {
		return time.Time{}, false
	}

	return *r.startedAt, true
}

func (r *Runtime) WebhookForwarder() WebhookForwarder {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.webhookForwarder
}

func (r *Runtime) NetworkStatus() networkclient.Status {
	r.mu.RLock()
	manager := r.networkManager
	r.mu.RUnlock()
	if manager == nil {
		return networkclient.Status{}
	}
	return manager.Status()
}

func runtimeHomeDirOrEmpty() string {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return homeDir
}

func (r *Runtime) RecoverySummary() RecoverySummary {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return r.recovery
}

func (r *Runtime) WebhookStatus() WebhookStatus {
	r.mu.RLock()
	webhook := r.webhook
	forwarder := r.webhookForwarder
	r.mu.RUnlock()
	if webhook == nil {
		return WebhookStatus{}
	}
	status := webhook.Status()
	if forwarder == nil {
		return status
	}
	stats := forwarder.Stats()
	status.Queue.Pending = stats.Queued
	status.Queue.Capacity = stats.QueueCapacity
	status.Queue.ActiveWorkers = stats.InFlight
	status.Counters.DeliveriesReceived = int(stats.DeliveriesReceived)
	status.Counters.Coalesced = int(stats.QueueCoalesced)
	status.Counters.Dropped = int(stats.DeliveriesDeduped + stats.DeliveriesIgnored + stats.QueueRejected)
	status.Counters.Queued = int(stats.QueueEnqueued)
	status.Counters.Processed = int(stats.ExecutionsSucceeded)
	status.Counters.Failed = int(stats.ExecutionsFailed)
	status.RecentOutcomes = webhookRecentOutcomesFromStats(stats.RecentOutcomes)
	return status
}

func (r *Runtime) RecordWebhookDelivery(eventType, deliveryID string) {
	r.mu.RLock()
	webhook := r.webhook
	r.mu.RUnlock()
	if webhook != nil {
		webhook.RecordDelivery(eventType, deliveryID)
		r.TriggerSchedulerTick()
	}
}

func webhookRecentOutcomesFromStats(outcomes []webhookforward.Outcome) []WebhookRecentOutcome {
	if len(outcomes) == 0 {
		return []WebhookRecentOutcome{}
	}
	recent := make([]WebhookRecentOutcome, 0, len(outcomes))
	for _, outcome := range outcomes {
		recent = append(recent, WebhookRecentOutcome{
			At:      outcome.At,
			Outcome: outcome.Status,
			Message: formatWebhookOutcomeMessage(outcome),
		})
	}
	return recent
}

func formatWebhookOutcomeMessage(outcome webhookforward.Outcome) string {
	parts := make([]string, 0, 4)
	if repo := strings.TrimSpace(outcome.Repo); repo != "" {
		parts = append(parts, repo)
	}
	if objectType := strings.TrimSpace(outcome.ObjectType); objectType != "" && outcome.Number > 0 {
		parts = append(parts, fmt.Sprintf("%s #%d", objectType, outcome.Number))
	}
	if eventType := strings.TrimSpace(outcome.EventType); eventType != "" {
		parts = append(parts, eventType)
	}
	message := strings.Join(parts, " · ")
	if err := strings.TrimSpace(outcome.Error); err != "" {
		if message == "" {
			return err
		}
		return message + ": " + err
	}
	return message
}

func (r *Runtime) ReconcileWebhookForwarders() {
	r.mu.RLock()
	webhook := r.webhook
	repositories := r.services.Repositories
	r.mu.RUnlock()
	if webhook != nil {
		if err := webhook.Reconcile(repositories); err != nil && r.logger != nil {
			r.logger.Warn("webhook.reconcile_failed", map[string]any{"error": err.Error()})
		}
	}
}

func (r *Runtime) scheduleWebhookForwarderReconcile() {
	r.mu.RLock()
	webhook := r.webhook
	repositories := r.services.Repositories
	r.mu.RUnlock()
	if webhook != nil {
		webhook.ScheduleReconcile(repositories)
	}
}

func (r *Runtime) RefreshWebhookForwarders() error {
	r.ReconcileWebhookForwarders()
	return nil
}

func (r *Runtime) stopWebhookRuntime() {
	r.mu.RLock()
	webhook := r.webhook
	lock := r.webhookDaemonLock
	r.mu.RUnlock()
	if webhook != nil {
		webhook.Stop()
	}
	if lock != nil {
		_ = lock.Release()
		r.mu.Lock()
		if r.webhookDaemonLock == lock {
			r.webhookDaemonLock = nil
		}
		r.mu.Unlock()
	}
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

	var lock *daemonLock
	var err error
	if r.config.Webhook.Enabled {
		lockPath := webhookForwarderLockPath(r.config.Storage.DBPath)
		lock, err = acquireDaemonLock(lockPath, r.webhook.daemonID, r.now())
		if err != nil {
			if r.logger != nil {
				holder, _ := os.ReadFile(lockPath)
				r.logger.Warn("webhook.daemon.lock_failed", map[string]any{"path": lockPath, "existing_holder": strings.TrimSpace(string(holder)), "error": err.Error()})
			}
			return err
		}
		if r.logger != nil {
			r.logger.Info("webhook.daemon.lock_acquired", map[string]any{"path": lockPath})
		}
	}

	coordinator, err := r.openSQLiteCoordinator(ctx, r.config.Storage.DBPath, storage.SQLiteCoordinatorOptions{
		BackupDir: backupDir,
		Now:       r.now,
	})
	if err != nil {
		if lock != nil {
			_ = lock.Release()
		}
		return err
	}

	started := false
	defer func() {
		if !started {
			if lock != nil {
				_ = lock.Release()
			}
			r.mu.Lock()
			forwarder := r.webhookForwarder
			r.webhookForwarder = nil
			r.mu.Unlock()
			if forwarder != nil {
				forwarder.Close()
			}
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
	var githubGateway *githubinfra.Gateway
	if strings.TrimSpace(derefString(r.config.Tools.GHPath)) != "" || runtimeConfigHasGitHubProjects(r.config) {
		githubGateway = githubinfra.New(githubinfra.Options{GHPath: derefString(r.config.Tools.GHPath), Now: r.now, DiscoveryCacheTTL: time.Duration(r.config.Scheduler.DiscoveryCacheTTLSeconds) * time.Second})
	}
	projectService := &projects.Service{
		DB:             coordinator.DB(),
		Repos:          repositories,
		Logger:         r.logger,
		Config:         r.config,
		ConfigSource:   r.projectCatalog,
		ConfigBoundary: &r.configBoundary,
		Now:            r.now,
		DetectRepo: func(ctx context.Context, repoPath string) (projects.DetectedRepo, error) {
			return detectProjectRepo(ctx, gitGateway, r.projectCatalog.Snapshot(), repoPath)
		},
		GetRepositorySettings: func(ctx context.Context, input githubinfra.RepositorySettingsInput) (githubinfra.RepositorySettings, error) {
			bound, err := bindProjectHostingOperation(ctx, r.projectCatalog.Snapshot(), "", input.Repo, "", "worker")
			if err != nil {
				return githubinfra.RepositorySettings{}, err
			}
			ctx = bound
			if githubGateway == nil {
				return githubinfra.RepositorySettings{}, fmt.Errorf("github gateway is not configured")
			}
			return githubGateway.GetRepositorySettings(ctx, input)
		},
		GetBranchProtection: func(ctx context.Context, input githubinfra.BranchProtectionInput) (githubinfra.BranchProtection, error) {
			bound, err := bindProjectHostingOperation(ctx, r.projectCatalog.Snapshot(), "", input.Repo, "", "worker")
			if err != nil {
				return githubinfra.BranchProtection{}, err
			}
			ctx = bound
			if githubGateway == nil {
				return githubinfra.BranchProtection{}, fmt.Errorf("github gateway is not configured")
			}
			return githubGateway.GetBranchProtection(ctx, input)
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
			bound, err := bindProjectHostingOperation(ctx, r.projectCatalog.Snapshot(), "", input.Repo, input.CWD, "worker")
			if err != nil {
				return nil, err
			}
			ctx = bound
			if githubGateway == nil {
				return nil, fmt.Errorf("github gateway is not configured")
			}
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
			bound, err := bindProjectHostingOperation(ctx, r.projectCatalog.Snapshot(), input.ProjectID, input.Repo, input.CWD, "reviewer")
			if err != nil {
				return storage.PullRequestSnapshotRecord{}, err
			}
			ctx = bound
			if githubGateway == nil {
				return storage.PullRequestSnapshotRecord{}, fmt.Errorf("github gateway is not configured")
			}
			return githubGateway.CapturePullRequestSnapshot(ctx, githubinfra.CapturePullRequestSnapshotInput{ProjectID: input.ProjectID, Repo: input.Repo, PRNumber: input.PRNumber, CWD: input.CWD, CapturedAt: input.CapturedAt})
		},
		AsyncSnapshotQueueEnabled: func() bool {
			return asyncSnapshotQueueEnabled(r.customSchedulerTick, r.Config())
		},
		PublishProjects: func(projects []config.ProjectRefConfig) {
			r.publishProjectsSnapshot(projects)
		},
		AfterPublishProjects: r.afterProjectsPublished,
	}
	loopService := &loops.Service{DB: coordinator.DB(), Repos: repositories, Now: r.now}
	runService := &runs.Service{DB: coordinator.DB(), Repos: repositories, Loops: loopService, Now: r.now}
	startedAt := r.now().UTC()
	if err := r.syncConfiguredProjects(ctx, projectService, r.config, startedAt); err != nil {
		return err
	}
	// Project import is already committed. Materialize that durable state even
	// when the startup request is canceled; CompleteStartup still observes ctx.
	if err := r.reloadProjectCatalog(context.Background(), repositories); err != nil {
		return err
	}
	r.config = r.projectCatalog.Snapshot()
	if strings.TrimSpace(derefString(r.config.Tools.GHPath)) != "" || runtimeConfigHasGitHubProjects(r.config) {
		if githubGateway == nil {
			githubGateway = githubinfra.New(githubinfra.Options{GHPath: derefString(r.config.Tools.GHPath), Now: r.now, DiscoveryCacheTTL: time.Duration(r.config.Scheduler.DiscoveryCacheTTLSeconds) * time.Second})
		}
	} else {
		githubGateway = nil
	}
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return fmt.Errorf("runtime already stopped")
	}
	r.startedAt = &startedAt
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
		handlers := buildCatalogSchedulerHandlers(r.projectCatalog, &r.configBoundary, r.configPath, r.logger, coordinator, repositories, gitGateway, githubGateway, r.activeExecutions, func() schedulerAsyncRunner {
			r.mu.RLock()
			defer r.mu.RUnlock()
			return r.schedulerTasks
		}, r.TriggerSchedulerClaim, r.now, r.reconcileLiveStaleRunningRuns, r.AllowClaim, r.WithAllowClaim, r.notifyHumanAttentionPostClaim, r)
		r.defaultSchedulerTick = handlers.tick
		r.defaultSchedulerClaim = handlers.claim
		r.webhookForwarder = handlers.webhook
		schedulerDisabled = !defaultSchedulerAgentsConfigured(r.config)
	}
	r.githubGateway = githubGateway
	r.networkManager = networkclient.NewManager(filepath.Join(runtimeHomeDirOrEmpty(), ".looper", "network.json"), r.config, repositories, githubGateway)
	r.webhookDaemonLock = lock
	r.schedulerDisabled = schedulerDisabled
	r.mu.Unlock()

	if r.deferRecovery {
		if r.networkManager != nil {
			_ = r.networkManager.Start(context.Background())
		}
		started = true
		return nil
	}

	if err := r.CompleteStartup(ctx); err != nil {
		return err
	}
	started = true
	return nil
}

func (r *Runtime) CompleteStartup(ctx context.Context) error {
	r.startupReadyOnce.Do(func() {
		r.mu.RLock()
		if r.stopped {
			r.mu.RUnlock()
			r.startupReadyErr = fmt.Errorf("runtime already stopped")
			return
		}
		startedAt := r.startedAt
		repositories := r.services.Repositories
		githubGateway := r.githubGateway
		schedulerDisabled := r.schedulerDisabled
		r.mu.RUnlock()

		if startedAt == nil {
			r.startupReadyErr = fmt.Errorf("runtime has not been started")
			return
		}
		if repositories == nil {
			r.startupReadyErr = fmt.Errorf("runtime repositories are not configured")
			return
		}
		if err := r.validateCoordinatorDependencyGates(ctx, repositories, githubGateway); err != nil {
			r.startupReadyErr = err
			return
		}
		recoverySummary, err := r.runRecoveryPipeline(ctx, repositories, githubGateway, *startedAt)
		if err != nil {
			r.startupReadyErr = err
			return
		}
		if err := r.appendStartedEvent(context.Background(), *startedAt, recoverySummary); err != nil {
			r.startupReadyErr = err
			return
		}

		r.mu.Lock()
		r.recovery = recoverySummary
		r.ownershipAcquired = true
		r.mu.Unlock()
		if r.networkManager != nil {
			if err := r.networkManager.Start(ctx); err != nil {
				r.startupReadyErr = err
				return
			}
		}

		if r.webhook != nil {
			if err := r.webhook.Start(repositories); err != nil {
				r.startupReadyErr = err
				return
			}
		}
		if schedulerDisabled && r.logger != nil {
			r.logger.Warn("looperd scheduler waiting for agent configuration", map[string]any{"reason": "no coding role agent is configured"})
		}
		// The scheduler's handlers snapshot the current config for each operation.
		// Keep the loop alive even without an initial vendor so configuring one can
		// take effect without restarting looperd.
		r.startSchedulerLoop()
		if r.config.Daemon.WorktreeCleanup.Enabled {
			r.startWorktreeCleanupLoop()
		}
		r.startConfigReloadLoop()

		// Open admission only after recovery and producer loops are assembled.
		// HTTP mutations and scheduler claims are projections of this state.
		if err := r.admission.MarkReady("complete startup"); err != nil {
			r.startupReadyErr = err
			return
		}
		// Deferred reviewer recovery requeues failed loops without the scheduler
		// claim path; start it only after admission is ready, and only while
		// admission remains ready (startDeferredReviewerRecovery rechecks under
		// the shutdown race where BeginShutdown may have already missed a nil
		// recoveryCancel between MarkReady and registration).
		r.startDeferredReviewerRecovery(githubGateway)
		// Rescan durable human-attention parks only after startup is committed
		// (admission ready). Launching earlier races Start's failure cleanup,
		// which closes SQLite while this goroutine still queries/persists or
		// waits on a notification transport. Delivery stays async so notification
		// dispatch cannot delay MarkReady / admission.
		r.scheduleHumanAttentionRecoveryNotify(repositories)
		// startSchedulerLoop already fired an immediate full tick while admission
		// was still starting (gate no-op). Wake full + claim pumps now that
		// admission is ready so discovery/HITL do not wait a full poll interval.
		r.TriggerSchedulerTick()

		if r.logger != nil {
			catalog := r.Config()
			r.logger.Info("looperd runtime assembled", map[string]any{
				"dbPath":                 r.config.Storage.DBPath,
				"projectCount":           len(catalog.Projects),
				"autoMigrate":            r.config.Package.AutoMigrateOnStartup,
				"backupRequired":         r.config.Package.RequireBackupBeforeMigrate,
				"recoverySummary":        recoverySummary,
				"schedulerDefaultActive": !r.customSchedulerTick && !schedulerDisabled,
				"admissionState":         string(r.admission.State()),
			})
		}
	})

	return r.startupReadyErr
}

func (r *Runtime) validateCoordinatorDependencyGates(ctx context.Context, repositories *storage.Repositories, githubGateway *githubinfra.Gateway) error {
	if repositories == nil || repositories.Projects == nil || githubGateway == nil {
		return nil
	}
	projectsList, err := repositories.Projects.List(ctx)
	if err != nil {
		return err
	}
	catalog := r.Config()
	botProbeCtx, cancelBotProbes := context.WithTimeout(ctx, 3*time.Second)
	defer cancelBotProbes()
	for _, project := range projectsList {
		if project.Archived {
			continue
		}
		roleCfg := config.ProjectRoleConfigs(catalog, project.ID).Coordinator
		if !roleCfg.Enabled || !roleCfg.Dependencies.Enabled {
			continue
		}
		repo := strings.TrimSpace(runtimeProjectRepo(project.MetadataJSON))
		if repo == "" {
			return fmt.Errorf("coordinator dependency gate enabled but repository metadata unavailable for project %s", project.ID)
		}
		probeCtx := ctx
		if _, selected, resolveErr := config.ResolveHostingIdentity(catalog, project.ID, "coordinator"); resolveErr != nil {
			return resolveErr
		} else if selected {
			probeCtx = botProbeCtx
		}
		bound, err := hostingidentity.Bind(probeCtx, catalog, project.ID, "coordinator")
		if err != nil {
			return err
		}
		issueNumber, err := r.firstDependencyProbeIssue(bound, githubGateway, repo, project.RepoPath)
		if err != nil {
			if session, selected := hostingidentity.FromContext(bound); selected {
				if r.logger != nil {
					r.logger.Warn("coordinator hosting identity probe failed", map[string]any{"identity": session.Name(), "projectId": project.ID, "error": err.Error()})
				}
				continue
			}
			return err
		}
		if issueNumber == 0 {
			continue
		}
		if err := r.probeDependencyAPI(bound, githubGateway, repo, project.RepoPath, issueNumber, roleCfg.Dependencies); err != nil {
			if session, selected := hostingidentity.FromContext(bound); selected {
				if r.logger != nil {
					r.logger.Warn("coordinator hosting identity capability probe failed", map[string]any{"identity": session.Name(), "projectId": project.ID, "error": err.Error()})
				}
				continue
			}
			return err
		}
	}
	return nil
}

func (r *Runtime) firstDependencyProbeIssue(ctx context.Context, githubGateway *githubinfra.Gateway, repo, cwd string) (int64, error) {
	return githubGateway.FindAnyIssueNumber(ctx, repo, cwd)
}

func (r *Runtime) probeDependencyAPI(ctx context.Context, githubGateway *githubinfra.Gateway, repo, cwd string, issueNumber int64, depsCfg config.CoordinatorDependenciesConfig) error {
	var lastErr error
	for attempt := 0; attempt < runtimeMaxDependencyAttempts(depsCfg.APIRetryAttempts); attempt++ {
		callCtx, cancel := context.WithTimeout(ctx, runtimeDependencyTimeout(depsCfg.APITimeoutSeconds))
		_, err := githubGateway.ListIssueBlockedBy(callCtx, githubinfra.ListIssueBlockedByInput{Repo: repo, IssueNumber: issueNumber, CWD: cwd})
		cancel()
		if err == nil {
			return nil
		}
		lastErr = err
		if githubinfra.IsNotFoundError(err) {
			return fmt.Errorf("coordinator dependency gate enabled but dependencies API unavailable on %s; disable roles.coordinator.dependencies.enabled or upgrade GHES", repo)
		}
		if !runtimeShouldRetryDependencyError(err) {
			return err
		}
	}
	return lastErr
}

func runtimeProjectRepo(metadataJSON *string) string {
	if metadataJSON == nil || strings.TrimSpace(*metadataJSON) == "" {
		return ""
	}
	var metadata map[string]any
	if err := json.Unmarshal([]byte(*metadataJSON), &metadata); err != nil {
		return ""
	}
	value, _ := metadata["repo"].(string)
	return strings.TrimSpace(value)
}

// defaultSchedulerAgentsConfigured reports whether the default scheduler has at
// least one coding-role agent vendor (global or role/profile). This is the same
// gate as buildDefaultSchedulerHandlersWithOptions.
func defaultSchedulerAgentsConfigured(cfg config.Config) bool {
	return config.AnyCodingRoleAgentConfigured(cfg)
}

// asyncSnapshotQueueEnabled reports whether project import can enqueue async PR
// snapshots for the scheduler to process. Role-only agent installs count as
// scheduler-enabled so import does not fall back to full synchronous capture.
func asyncSnapshotQueueEnabled(customSchedulerTick bool, cfg config.Config) bool {
	return customSchedulerTick || defaultSchedulerAgentsConfigured(cfg)
}

func runtimeConfigHasGitHubProjects(cfg config.Config) bool {
	for _, project := range cfg.Projects {
		switch config.ResolvedProjectProviderKind(cfg, project) {
		case config.ProviderKindGitHub:
			return true
		case config.ProviderKindPlane:
			// Plane is a task-source only: its pull requests live on the
			// project's GitHub code repo, so the GitHub gateway is required.
			return true
		}
	}
	return len(cfg.Projects) == 0
}

func runtimeProjectProviderKind(cfg config.Config, projectID string) config.ProviderKind {
	project, ok := runtimeProjectBinding(cfg, projectID)
	if ok {
		return config.ResolvedProjectProviderKind(cfg, project)
	}
	return config.ProviderKindGitHub
}

func runtimeProjectBinding(cfg config.Config, projectID string) (config.ProjectRefConfig, bool) {
	for _, project := range cfg.Projects {
		if project.ID == projectID {
			return project, true
		}
	}
	return config.ProjectRefConfig{}, false
}

func detectProjectRepo(ctx context.Context, gitGateway *gitinfra.Gateway, cfg config.Config, repoPath string) (projects.DetectedRepo, error) {
	if gitGateway == nil {
		return projects.DetectedRepo{}, fmt.Errorf("git gateway is not configured")
	}
	remote, err := gitGateway.DetectOriginRemote(ctx, repoPath)
	if err != nil {
		return projects.DetectedRepo{}, err
	}
	if strings.TrimSpace(remote.Repo) == "" {
		return projects.DetectedRepo{}, nil
	}
	if provider, ok := config.MatchForgejoProviderByRemoteHost(cfg, remote.Host); ok {
		return projects.DetectedRepo{Repo: remote.Repo, Provider: provider.ID}, nil
	}
	if remote.Host == "github.com" || strings.HasSuffix(remote.Host, ".github.com") {
		return projects.DetectedRepo{Repo: remote.Repo}, nil
	}
	return projects.DetectedRepo{}, nil
}

func (r *Runtime) reloadProjectCatalog(ctx context.Context, repos *storage.Repositories) error {
	if r.projectCatalog == nil || repos == nil || repos.Projects == nil {
		return fmt.Errorf("project catalog dependencies are not configured")
	}
	records, err := repos.Projects.List(ctx)
	if err != nil {
		return fmt.Errorf("list projects for runtime catalog: %w", err)
	}
	global := r.projectCatalog.Snapshot()
	materialized, err := projects.MaterializeCatalog(global, records)
	if err != nil {
		return fmt.Errorf("materialize runtime project catalog: %w", err)
	}
	r.publishProjects(materialized)
	return nil
}

func (r *Runtime) publishProjects(materialized []config.ProjectRefConfig) {
	r.publishProjectsSnapshot(materialized)
	r.afterProjectsPublished()
}

// publishProjectsSnapshot is the atomic in-memory half of project
// publication. Project mutations call it while holding configBoundary so a
// concurrent config reload cannot validate or publish a mixed catalog.
func (r *Runtime) publishProjectsSnapshot(materialized []config.ProjectRefConfig) {
	if r == nil || r.projectCatalog == nil {
		return
	}
	r.projectCatalog.Publish(materialized)
	r.publishCatalogConsumers(r.projectCatalog.Snapshot())
}

// publishCatalogConsumers keeps long-lived consumers on the same coherent
// globals-plus-projects snapshot used for new scheduler claims.
func (r *Runtime) publishCatalogConsumers(next config.Config) {
	r.mu.RLock()
	webhook := r.webhook
	networkManager := r.networkManager
	r.mu.RUnlock()
	if webhook != nil {
		webhook.updateConfig(next)
	}
	if networkManager != nil {
		networkManager.UpdateConfig(next)
	}
}

// afterProjectsPublished deliberately runs outside configBoundary. Webhook
// reconciliation can invoke gh and other external operations; those must not
// block config publication or new claim snapshots.
func (r *Runtime) afterProjectsPublished() {
	if r == nil {
		return
	}
	r.mu.RLock()
	started := r.startedAt != nil
	r.mu.RUnlock()
	if started {
		r.TriggerSchedulerTick()
		r.TriggerSchedulerClaim()
		r.scheduleWebhookForwarderReconcile()
	}
}

func runtimeDependencyTimeout(seconds int) time.Duration {
	if seconds <= 0 {
		return 10 * time.Second
	}
	return time.Duration(seconds) * time.Second
}

func runtimeMaxDependencyAttempts(attempts int) int {
	if attempts <= 0 {
		return 1
	}
	return attempts
}

func runtimeShouldRetryDependencyError(err error) bool {
	if githubinfra.IsTransientError(err) {
		return true
	}
	message := strings.ToLower(githubinfra.ErrorMessage(err))
	return strings.Contains(message, "timed out") || strings.Contains(message, "context deadline exceeded")
}

func (r *Runtime) startSchedulerLoop() {
	pollInterval := schedulerFullPollInterval(r.config)
	stopCh := make(chan struct{})
	doneCh := make(chan struct{})
	wakeCh := make(chan struct{}, 1)
	claimWakeCh := make(chan struct{}, 1)
	schedulerCtx, schedulerCancel := context.WithCancel(context.Background())
	taskTracker := &schedulerTaskTracker{}

	r.mu.Lock()
	r.schedulerStop = stopCh
	r.schedulerDone = doneCh
	r.schedulerWake = wakeCh
	r.schedulerClaimWake = claimWakeCh
	r.schedulerCancel = schedulerCancel
	r.schedulerTasks = taskTracker
	r.mu.Unlock()

	if len(r.config.Identities) > 0 {
		probeConfig := r.Config()
		taskTracker.Go(func() { r.probeHostingIdentities(schedulerCtx, probeConfig) })
	}

	if r.defaultSchedulerClaim != nil {
		taskTracker.Go(func() {
			r.runSchedulerClaimLoop(schedulerCtx, stopCh, claimWakeCh)
		})
		// Dedicated shepherd poll: drive worker loops shepherding their PR to merge
		// on a prompt cadence (no per-PR webhook seam), so a re-review / CI change /
		// merge is picked up in ~60s rather than the slower wake-reconcile.
		taskTracker.Go(func() {
			ticker := time.NewTicker(shepherdReconcileInterval)
			defer ticker.Stop()
			for {
				select {
				case <-stopCh:
					return
				case <-ticker.C:
					r.reconcileWorkerShepherd(schedulerCtx)
				}
			}
		})
	}

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

func schedulerFullPollInterval(cfg config.Config) time.Duration {
	if cfg.Webhook.Enabled {
		return time.Duration(cfg.Webhook.FallbackPollIntervalSeconds) * time.Second
	}
	return time.Duration(cfg.Scheduler.PollIntervalSeconds) * time.Second
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
	r.schedulerClaimWake = nil
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
	// Refuse to arm recovery once shutdown has begun or admission is no longer
	// ready. BeginShutdown may already have observed recoveryCancel==nil; starting
	// a live goroutine afterward would let requeue persist work while stopping
	// (and stopDeferredReviewerRecovery may already have passed).
	if r.stopped || r.admission == nil || r.admission.State() != AdmissionReady {
		r.mu.Unlock()
		cancel()
		return
	}
	r.recoveryCancel = cancel
	r.recoveryDone = done
	r.mu.Unlock()

	// Publish-then-recheck: if BeginShutdown raced between the ready check and
	// assigning recoveryCancel, it missed cancel. If admission is no longer
	// ready, cancel immediately so the goroutine is born canceled.
	if err := r.AllowClaim(); err != nil {
		cancel()
	}

	go func(repositories *storage.Repositories) {
		defer close(done)
		if err := ctx.Err(); err != nil {
			return
		}
		// Deferred recovery requeues without AllowClaim on the scheduler path;
		// refuse when admission already closed after MarkReady/shutdown race.
		if err := r.AllowClaim(); err != nil {
			return
		}
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

// admissionRefusesDeferredRequeue reports whether deferred recovery must not
// persist queue/loop requeues. Stopping and degraded are hard refusals;
// starting is allowed only for direct unit-test helpers (production arms
// deferred recovery only after MarkReady via startDeferredReviewerRecovery).
func (r *Runtime) admissionRefusesDeferredRequeue() bool {
	if r == nil || r.admission == nil {
		return true
	}
	switch r.admission.State() {
	case AdmissionStopping, AdmissionDegraded:
		return true
	default:
		return false
	}
}

// WaitForDeferredReviewerRecovery blocks until the post-ready deferred
// reviewer recovery goroutine exits, or until ctx is canceled. It returns
// immediately when deferred recovery was never started (for example when no
// GitHub gateway is configured). Test fixtures call this after CompleteStartup
// so later inserts of terminal reviewer metadata cannot race
// normalizeTerminalReviewerLoopForRecovery.
func (r *Runtime) WaitForDeferredReviewerRecovery(ctx context.Context) error {
	r.mu.RLock()
	done := r.recoveryDone
	r.mu.RUnlock()
	if done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *Runtime) TriggerSchedulerTick() {
	r.mu.RLock()
	if r.stopped {
		r.mu.RUnlock()
		return
	}
	wakeCh := r.schedulerWake
	claimWakeCh := r.schedulerClaimWake
	r.mu.RUnlock()

	if wakeCh != nil {
		select {
		case wakeCh <- struct{}{}:
		default:
		}
	}
	if claimWakeCh != nil {
		select {
		case claimWakeCh <- struct{}{}:
		default:
		}
	}
}

func (r *Runtime) TriggerSchedulerClaim() {
	r.mu.RLock()
	if r.stopped {
		r.mu.RUnlock()
		return
	}
	claimWakeCh := r.schedulerClaimWake
	r.mu.RUnlock()
	if claimWakeCh != nil {
		select {
		case claimWakeCh <- struct{}{}:
		default:
		}
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

func (r *Runtime) executeSchedulerClaimPass(ctx context.Context) {
	// Claim is a projection of admission — refuse before any durable claim.
	if err := r.AllowClaim(); err != nil {
		return
	}
	r.mu.RLock()
	services := r.services
	claim := r.defaultSchedulerClaim
	r.mu.RUnlock()
	if services.Repositories == nil || claim == nil {
		return
	}
	if err := claim(ctx, services); err != nil && r.logger != nil {
		r.logger.Warn("looperd scheduler claim pump failed", map[string]any{"error": err.Error()})
	}
}

// releaseExpiredLocks releases every lock past its TTL and records a recovery
// event for each. Shared by startup recovery and the sleep/wake reconcile so a
// laptop that suspended mid-run doesn't stay wedged behind a dead owner's lock.
func (r *Runtime) releaseExpiredLocks(ctx context.Context, repositories *storage.Repositories, nowISO string) (released int64, eventsWritten int64, err error) {
	expiredLocks, err := repositories.Locks.ListExpired(ctx, nowISO)
	if err != nil {
		return 0, 0, err
	}
	for _, lock := range expiredLocks {
		if err := repositories.Locks.Release(ctx, lock.Key); err != nil {
			return released, eventsWritten, err
		}
		released++
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
			return released, eventsWritten, err
		}
		eventsWritten++
	}
	return released, eventsWritten, nil
}

// wakeReconcileThreshold: a gap between 1s claim passes this much larger than the
// tick means the host suspended (laptop closed) rather than merely idled — the cue
// to run recovery so work resumes on wake. periodicReconcileInterval bounds how
// often the live reconcile runs even without a wake or a full queue, so a wedged
// run doesn't wait for availableSlots==0.
const (
	wakeReconcileThreshold    = 90 * time.Second
	periodicReconcileInterval = 5 * time.Minute
	// Plane has no inbound webhook seam for work-item/page comments in this
	// runtime. Human decisions and spec approvals therefore need a much tighter
	// lightweight poll than the full recovery/auto-intake sweep.
	humanGateReconcileInterval = 30 * time.Second
)

// shouldWakeReconcile decides, from the time since the last claim pass and since
// the last reconcile, whether to run the wake/periodic reconcile and why. A gap
// far larger than the 1s tick means a suspend/resume; otherwise a periodic sweep.
func shouldWakeReconcile(sinceLastPass, sinceLastReconcile time.Duration) (bool, string) {
	switch {
	case sinceLastPass > wakeReconcileThreshold:
		return true, "wall_clock_jump"
	case sinceLastReconcile >= periodicReconcileInterval:
		return true, "periodic"
	default:
		return false, ""
	}
}

// runWakeReconcile is the lightweight recovery run on sleep/wake (and periodically):
// release expired locks + reconcile stale running runs (which also repairs the
// queue). Unlike startup recovery it does NOT force-interrupt live runs — after a
// suspend the agent subprocesses are usually still alive — it only touches what is
// provably dead (expired locks, superseded / heartbeat-dead runs).
func (r *Runtime) runWakeReconcile(ctx context.Context, reason string) {
	r.mu.RLock()
	repositories := r.services.Repositories
	now := r.now
	logger := r.logger
	r.mu.RUnlock()
	if repositories == nil {
		return
	}
	if now == nil {
		now = time.Now
	}
	nowISO := formatJavaScriptISOString(now().UTC())
	released, _, err := r.releaseExpiredLocks(ctx, repositories, nowISO)
	if err != nil && logger != nil {
		logger.Warn("wake reconcile: release expired locks failed", map[string]any{"error": err.Error(), "reason": reason})
	}
	stale, err := r.reconcileLiveStaleRunningRuns(ctx)
	if err != nil && logger != nil {
		logger.Warn("wake reconcile: stale-run reconcile failed", map[string]any{"error": err.Error(), "reason": reason})
	}
	// Classify + route freshly-labelled looper:auto Plane items into the pipeline
	// (flowchart top: bug/feature → product-spec gate → looper:plan / worker-ready).
	// Env-gated (LOOPER_PLANE_AUTO_INTAKE=1); a no-op otherwise.
	r.reconcileAutoIntake(ctx)
	// Resume loops whose named, externally-observed blocking condition cleared.
	r.reconcileBlockedConditions(ctx)
	// Resume V2 planner loops only after the configured Plane role authorities have
	// answered the current decision revision. Feishu is notification-only.
	r.reconcileAwaitingRoleDecisions(ctx)
	// Dispatch the worker for tech specs a human approved on the Plane page (node H→I).
	r.reconcileSpecApproval(ctx)
	// Drive worker loops shepherding their impl PR toward merge (looper:auto): wake
	// a pass on an actionable PR change, terminate once a human merges. Polling
	// cadence (no per-PR webhook seam) — best-effort.
	r.reconcileWorkerShepherd(ctx)
	if logger != nil && (released > 0 || stale.InterruptedRuns > 0 || stale.LoopsRequeued > 0) {
		logger.Info("wake reconcile recovered work", map[string]any{"reason": reason, "locksReleased": released, "runsInterrupted": stale.InterruptedRuns, "loopsRequeued": stale.LoopsRequeued})
	}
}

func (r *Runtime) runSchedulerClaimLoop(ctx context.Context, stopCh <-chan struct{}, wakeCh <-chan struct{}) {
	const claimPumpInterval = time.Second
	nowFn := r.now
	if nowFn == nil {
		nowFn = time.Now
	}
	lastPass := nowFn()
	lastReconcile := lastPass
	lastHumanGateReconcile := time.Time{}
	// maybeReconcile detects a suspend/resume (or a periodic due time) before each
	// claim pass and runs the wake reconcile so a laptop that was closed mid-run
	// resumes its work rather than waiting for a full queue.
	maybeReconcile := func() {
		now := nowFn()
		if lastHumanGateReconcile.IsZero() || now.Sub(lastHumanGateReconcile) >= humanGateReconcileInterval {
			r.reconcileBlockedConditions(ctx)
			r.reconcileAwaitingRoleDecisions(ctx)
			r.reconcileSpecApproval(ctx)
			lastHumanGateReconcile = now
		}
		if run, reason := shouldWakeReconcile(now.Sub(lastPass), now.Sub(lastReconcile)); run {
			r.runWakeReconcile(ctx, reason)
			lastReconcile = now
		}
		lastPass = now
	}
	// Reconcile named holds once at boot before claiming work. This closes the
	// restart gap where an external answer/spec arrived while looperd was down.
	r.reconcileBlockedConditions(ctx)
	r.executeSchedulerClaimPass(ctx)
	ticker := time.NewTicker(claimPumpInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stopCh:
			return
		case <-wakeCh:
			maybeReconcile()
			r.executeSchedulerClaimPass(ctx)
		case <-ticker.C:
			maybeReconcile()
			r.executeSchedulerClaimPass(ctx)
		}
	}
}

func (r *Runtime) runRecoveryPipeline(ctx context.Context, repositories *storage.Repositories, githubGateway *githubinfra.Gateway, now time.Time) (RecoverySummary, error) {
	nowISO := formatJavaScriptISOString(now)
	eventsWritten := int64(0)
	summary := createEmptyRecoverySummary()
	summary.StartedAt = nowISO
	orphans, err := r.recoveryHost().RecoverOrphanExecutions(ctx, repositories, nowISO)
	if err != nil {
		return RecoverySummary{}, err
	}
	summary.OrphanAgentCleanup = orphans.Cleanup
	eventsWritten += orphans.EventsWritten
	uncertainAgentRunIDs := orphans.UncertainRunIDs
	quarantinedLoopIDs := orphans.QuarantinedLoopIDs

	releasedLocks, lockEvents, err := r.releaseExpiredLocks(ctx, repositories, nowISO)
	if err != nil {
		return RecoverySummary{}, err
	}
	summary.ExpiredLocksReleased += releasedLocks
	eventsWritten += lockEvents

	staleSummary, err := r.reconcileStaleRunningRunsWithMode(ctx, repositories, now, staleRunReconcileModeStartup)
	if err != nil {
		return RecoverySummary{}, err
	}
	summary.InterruptedRunsMarked += staleSummary.InterruptedRuns
	summary.LoopsRequeued += staleSummary.LoopsRequeued
	eventsWritten += staleSummary.EventsWritten
	if err := r.recoveryHost().RecoverQueueClaims(ctx, repositories, nowISO); err != nil {
		return RecoverySummary{}, err
	}

	loops, err := repositories.Loops.List(ctx)
	if err != nil {
		return RecoverySummary{}, err
	}
	loopsByID := make(map[string]storage.LoopRecord, len(loops))
	for _, loop := range loops {
		loopsByID[loop.ID] = loop
	}
	requeuedLoopIDs := make(map[string]struct{})
	terminalNormalizedLoopIDs := make(map[string]struct{})
	if staleSummary.LoopsRequeued > 0 {
		for _, loopID := range staleSummary.LoopIDs {
			loop, ok := loopsByID[loopID]
			if ok && loop.Status == "queued" {
				requeuedLoopIDs[loopID] = struct{}{}
			}
		}
	}
	activeAgentRunIDs := make(map[string]struct{})
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
			pid := int(*execution.PID)
			matches, running, err := r.executionMatchesProcess(ctx, execution, pid)
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
	for _, loop := range loops {
		if _, wasRequeued := requeuedLoopIDs[loop.ID]; wasRequeued {
			continue
		}
		// Quarantined work must not requeue or auto-recover as if cleaned.
		if _, quarantined := quarantinedLoopIDs[loop.ID]; quarantined {
			continue
		}
		latestRun, err := repositories.Runs.GetLatestByLoopID(ctx, loop.ID)
		if err != nil {
			return RecoverySummary{}, err
		}
		latestQueue, err := repositories.Queue.GetLatestByLoopID(ctx, loop.ID)
		if err != nil {
			return RecoverySummary{}, err
		}
		if handled, wroteEvent, err := normalizeTerminalReviewerLoopForRecovery(ctx, repositories, loop, latestRun, nowISO); err != nil {
			return RecoverySummary{}, err
		} else if handled {
			if wroteEvent {
				eventsWritten += 1
			}
			terminalNormalizedLoopIDs[loop.ID] = struct{}{}
			continue
		}
		policy := r.reviewerRecoveryPolicyForProject(loop.ProjectID)
		if reviewerRecoveryNeedsFreshLogin(loop, latestRun, policy) {
			continue
		}
		if shouldAutoRecoverFailedReviewerLoop(loop, latestRun, latestQueue, policy) {
			// Share discard/retry exclusion with deferred recovery and discovery.
			didRequeue, recoveredQueueItems, err := requeueFailedReviewerWithSharedGuards(ctx, repositories, loop, latestQueue, nowISO, policy, latestRun)
			if err != nil {
				return RecoverySummary{}, err
			}
			if !didRequeue {
				continue
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
			terminalNormalizedLoopIDs[loop.ID] = struct{}{}
			continue
		}

		_, latestRunHasActiveAgent := activeAgentRunIDs[derefRunID(latestRun)]
		_, latestRunHasUncertainAgent := uncertainAgentRunIDs[derefRunID(latestRun)]
		if latestQueueIsManualIntervention(latestQueue) && (loop.Status == "running" || loop.Status == "queued") {
			if latestQueue.Status == "manual_intervention" {
				normalizedLoop := loop
				normalizedLoop.Status = normalizeStaleQueuedLoopStatus(loop, derefRunOrZero(latestRun))
				normalizedLoop.NextRunAt = nil
				if latestRun != nil {
					normalizedLoop.LastRunAt = coalesceString(latestRun.EndedAt, stringPtr(latestRun.StartedAt), loop.LastRunAt)
				}
				normalizedLoop.UpdatedAt = nowISO
				if err := repositories.Loops.Upsert(ctx, normalizedLoop); err != nil {
					return RecoverySummary{}, err
				}
				terminalNormalizedLoopIDs[loop.ID] = struct{}{}
				latestRunStatus := ""
				if latestRun != nil {
					latestRunStatus = latestRun.Status
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
						"latestRunStatus": latestRunStatus,
					}),
					CreatedAt: nowISO,
				}); err != nil {
					return RecoverySummary{}, err
				}
				eventsWritten += 1
				continue
			}

			requeuedLoop := loop
			requeuedLoop.Status = "queued"
			requeuedLoop.NextRunAt = stringPtr(nowISO)
			if latestRun != nil {
				requeuedLoop.LastRunAt = coalesceString(latestRun.EndedAt, stringPtr(latestRun.StartedAt), loop.LastRunAt)
			}
			requeuedLoop.UpdatedAt = nowISO
			if _, err := repositories.Queue.RequeueRunningByLoop(ctx, loop.ID, nowISO); err != nil {
				return RecoverySummary{}, err
			}
			if err := repositories.Loops.Upsert(ctx, requeuedLoop); err != nil {
				return RecoverySummary{}, err
			}
			requeuedLoopIDs[loop.ID] = struct{}{}
			if err := appendSystemEvent(ctx, repositories, storage.EventLogRecord{
				ID:         newRuntimeEventID(),
				EventType:  "looperd.recovery.loop_manual_intervention_requeued",
				LoopID:     stringPtr(loop.ID),
				EntityType: stringPtr("loop"),
				EntityID:   stringPtr(loop.ID),
				PayloadJSON: mustMarshalJSON(map[string]any{
					"previousStatus":  loop.Status,
					"recoveredStatus": requeuedLoop.Status,
					"queueItemId":     latestQueue.ID,
					"lastErrorKind":   derefString(latestQueue.LastErrorKind),
				}),
				CreatedAt: nowISO,
			}); err != nil {
				return RecoverySummary{}, err
			}
			eventsWritten += 1
			continue
		}
		if shouldRequeueLoop(loop, latestRun, latestRunHasActiveAgent || latestRunHasUncertainAgent) {
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
				if err := ensureRecoveryQueueItem(ctx, repositories, requeuedLoop, nowISO, int64(r.Config().Scheduler.RetryMaxAttempts)); err != nil {
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
		if _, normalized := terminalNormalizedLoopIDs[loop.ID]; normalized {
			continue
		}
		if _, exists := queuedLoopIDs[loop.ID]; exists {
			continue
		}

		if loop.Status != "queued" {
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
		normalizedLoop.Status = normalizeStaleQueuedLoopStatus(loop, *latestRun)
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
	// Refuse up front when admission already closed for shutdown/degraded.
	// (startDeferredReviewerRecovery only arms after ready; unit tests may call
	// this helper while still starting.)
	if r.admissionRefusesDeferredRequeue() {
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
		// Recheck before each durable requeue so BeginShutdown cannot leave a
		// still-running recovery path persisting queued loop/queue state.
		if r.admissionRefusesDeferredRequeue() {
			return requeued, nil
		}
		latestRun, err := repositories.Runs.GetLatestByLoopID(ctx, loop.ID)
		if err != nil {
			return requeued, err
		}
		latestQueue, err := repositories.Queue.GetLatestByLoopID(ctx, loop.ID)
		if err != nil {
			return requeued, err
		}
		if handled, _, err := normalizeTerminalReviewerLoopForRecovery(ctx, repositories, loop, latestRun, nowISO); err != nil {
			return requeued, err
		} else if handled {
			continue
		}
		policy := r.reviewerRecoveryPolicyForProject(loop.ProjectID)
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
		// Final admission gate immediately before lock+persist: matches the
		// shutdown race where cancel registration was missed after MarkReady.
		if r.admissionRefusesDeferredRequeue() {
			return requeued, nil
		}
		// Share discard/retry exclusion: recovery requeue of a PR reviewer must
		// not interleave with operator discard of a sibling loop on the same
		// managed worktree between preflight and git reset.
		didRequeue, recoveredQueueItems, err := requeueFailedReviewerWithSharedGuards(ctx, repositories, *currentLoop, latestQueue, nowISO, policy, latestRun)
		if err != nil {
			return requeued, err
		}
		if !didRequeue {
			continue
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

func (r *Runtime) appendStartedEvent(ctx context.Context, startedAt time.Time, recoverySummary RecoverySummary) error {
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
			"recovery":   recoverySummary,
		}),
		CreatedAt: formatJavaScriptISOString(startedAt.Add(time.Millisecond)),
	})
}

func (r *Runtime) ExecutionMatchesProcess(ctx context.Context, execution storage.AgentExecutionRecord, pid int) (matches bool, running bool, err error) {
	return r.executionMatchesProcess(ctx, execution, pid)
}

func (r *Runtime) ReconcileStaleRunningRuns(ctx context.Context) (StaleRunReconcileSummary, error) {
	r.mu.RLock()
	repositories := r.services.Repositories
	now := r.now
	r.mu.RUnlock()
	if repositories == nil {
		return StaleRunReconcileSummary{}, fmt.Errorf("storage is not configured")
	}
	if now == nil {
		now = time.Now
	}
	return r.reconcileStaleRunningRunsWithMode(ctx, repositories, now().UTC(), staleRunReconcileModeManual)
}

func (r *Runtime) reconcileLiveStaleRunningRuns(ctx context.Context) (StaleRunReconcileSummary, error) {
	r.mu.RLock()
	repositories := r.services.Repositories
	now := r.now
	r.mu.RUnlock()
	if repositories == nil {
		return StaleRunReconcileSummary{}, fmt.Errorf("storage is not configured")
	}
	if now == nil {
		now = time.Now
	}
	return r.reconcileStaleRunningRunsWithMode(ctx, repositories, now().UTC(), staleRunReconcileModeLive)
}

func (r *Runtime) reconcileStaleRunningRunsWithMode(ctx context.Context, repositories *storage.Repositories, now time.Time, mode staleRunReconcileMode) (StaleRunReconcileSummary, error) {
	return r.recoveryHost().ReconcileStaleRunningRunsWithMode(ctx, repositories, now, mode)
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

func defaultSyncConfiguredProjects(ctx context.Context, service *projects.Service, cfg config.Config, now time.Time) error {
	if service == nil {
		return fmt.Errorf("projects service is not configured")
	}
	return service.SyncConfigured(ctx, cfg, now)
}

type staleRunCandidateDecision = executionhost.StaleRunCandidateDecision

type staleRunQueueRepairSummary = executionhost.StaleRunQueueRepairSummary

func (r *Runtime) evaluateStaleRunCandidate(ctx context.Context, repositories *storage.Repositories, run storage.RunRecord, loop storage.LoopRecord, latestRun *storage.RunRecord, activeExecutions []storage.AgentExecutionRecord, now time.Time, mode staleRunReconcileMode) (staleRunCandidateDecision, error) {
	return r.recoveryHost().EvaluateStaleRunCandidate(ctx, repositories, run, loop, latestRun, activeExecutions, now, mode)
}

type executionLivenessResult = executionhost.ExecutionLivenessResult

func (r *Runtime) verifyRunExecutionLiveness(ctx context.Context, repositories *storage.Repositories, executions []storage.AgentExecutionRecord, now time.Time, scope string) (executionLivenessResult, error) {
	return r.recoveryHost().VerifyRunExecutionLiveness(ctx, repositories, executions, now, scope)
}

func (r *Runtime) repairStaleRunQueueState(ctx context.Context, repositories *storage.Repositories, loop storage.LoopRecord, latestRun *storage.RunRecord, latestRunHasLiveAgent bool, nowISO string) (staleRunQueueRepairSummary, error) {
	return r.recoveryHost().RepairStaleRunQueueState(ctx, repositories, loop, latestRun, latestRunHasLiveAgent, nowISO)
}

func (r *Runtime) repairInterruptedLoopQueueIfNeeded(ctx context.Context, repositories *storage.Repositories, loop storage.LoopRecord, nowISO string) (staleRunQueueRepairSummary, error) {
	return r.recoveryHost().RepairInterruptedLoopQueueIfNeeded(ctx, repositories, loop, nowISO)
}

func isAgentBackedRunStep(loop storage.LoopRecord, run storage.RunRecord) bool {
	return executionhost.IsAgentBackedRunStep(loop, run)
}

func runHeartbeatIsRecent(run storage.RunRecord, now time.Time, ttl time.Duration) bool {
	return executionhost.RunHeartbeatIsRecent(run, now, ttl)
}

func (r *Runtime) appendUncertainProcessIdentityEvent(ctx context.Context, repositories *storage.Repositories, execution storage.AgentExecutionRecord, pid int, scope string, nowISO string) (bool, error) {
	return r.recoveryHost().AppendUncertainProcessIdentityEvent(ctx, repositories, execution, pid, scope, nowISO)
}

func (r *Runtime) appendContainmentClassificationEvent(ctx context.Context, repositories *storage.Repositories, execution storage.AgentExecutionRecord, classification ContainmentClassification, scope, nowISO string) (bool, error) {
	return r.recoveryHost().AppendContainmentClassificationEvent(ctx, repositories, execution, classification, scope, nowISO)
}

func (r *Runtime) markRecoveredExecutionTerminal(ctx context.Context, repositories *storage.Repositories, execution storage.AgentExecutionRecord, pid int, nowISO string, message string) error {
	// Retained for any non-recovery callers; startup recovery must not use this
	// to invent cleanliness from raw PID probes (#575).
	cleaned := execution
	cleaned.Status = "killed"
	if cleaned.ErrorMessage == nil {
		cleaned.ErrorMessage = stringPtr(message)
	}
	if r.Config().Agent.NativeResume.Enabled && runtimeNativeResumeSupported(cleaned.Vendor) && cleaned.NativeSessionID != nil && strings.TrimSpace(*cleaned.NativeSessionID) != "" {
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
		return err
	}
	payload := map[string]any{"recoveredAt": nowISO}
	if pid > 0 {
		payload["pid"] = pid
	}
	return appendSystemEvent(ctx, repositories, storage.EventLogRecord{
		ID:          newRuntimeEventID(),
		EventType:   "agent.killed",
		ProjectID:   execution.ProjectID,
		LoopID:      execution.LoopID,
		RunID:       execution.RunID,
		EntityType:  stringPtr("agent_execution"),
		EntityID:    stringPtr(execution.ID),
		PayloadJSON: mustMarshalJSON(payload),
		CreatedAt:   nowISO,
	})
}

func (r *Runtime) quarantineRecoveryEvidence(ctx context.Context, repositories *storage.Repositories, execution storage.AgentExecutionRecord, nowISO, reason string) (bool, bool, error) {
	return r.recoveryHost().QuarantineRecoveryEvidence(ctx, repositories, execution, nowISO, reason)
}

func ensureRecoveryQueueItem(ctx context.Context, repositories *storage.Repositories, loop storage.LoopRecord, nowISO string, maxAttempts int64) error {
	return executionhost.EnsureRecoveryQueueItem(ctx, repositories, loop, nowISO, maxAttempts)
}

func shouldInterruptStaleRunningRun(run storage.RunRecord, latestRun *storage.RunRecord, hasActiveAgent bool, hasUncertainAgent bool) bool {
	if run.Status != string(domain.RunStatusRunning) {
		return false
	}
	if latestRun == nil || latestRun.ID != run.ID {
		return true
	}
	if hasActiveAgent || hasUncertainAgent {
		return false
	}
	// Recovery runs during daemon startup, so a persisted running run without an
	// active agent execution is orphaned regardless of loop status or heartbeat.
	return true
}

func interruptRecoveryRun(ctx context.Context, repositories *storage.Repositories, run storage.RunRecord, loop storage.LoopRecord, nowISO string, message string) error {
	return executionhost.InterruptRecoveryRun(ctx, repositories, run, loop, nowISO, message)
}

func buildRecoveryQueueItem(loop storage.LoopRecord, nowISO string, maxAttempts int64) (storage.QueueItemRecord, bool, error) {
	return executionhost.BuildRecoveryQueueItem(loop, nowISO, maxAttempts)
}

func buildRecoveryWorkerPayloadJSON(metadataJSON *string) *string {
	return executionhost.BuildRecoveryWorkerPayloadJSON(metadataJSON)
}

func parseIssueNumberFromTargetID(targetID string) (int64, error) {
	return executionhost.ParseIssueNumberFromTargetID(targetID)
}

func formatJavaScriptISOString(value time.Time) string {
	return executionhost.FormatJavaScriptISOString(value)
}

func appendSystemEvent(ctx context.Context, repositories *storage.Repositories, record storage.EventLogRecord) error {
	return executionhost.AppendSystemEvent(ctx, repositories, record)
}

func (r *Runtime) executionMatchesProcess(ctx context.Context, execution storage.AgentExecutionRecord, pid int) (matches bool, running bool, err error) {
	return r.recoveryHost().ExecutionMatchesProcess(ctx, execution, pid)
}

func expectedExecutionCommandTokens(execution storage.AgentExecutionRecord) ([]string, error) {
	return executionhost.ExpectedExecutionCommandTokens(execution)
}

func commandPrefixMatches(expected, actual []string) bool {
	return executionhost.CommandPrefixMatches(expected, actual)
}

func processCommandTokenMatches(expected, actual string) bool {
	return executionhost.ProcessCommandTokenMatches(expected, actual)
}

func splitProcessCommand(command string) []string {
	return executionhost.SplitProcessCommand(command)
}

func defaultReadProcessCommand(ctx context.Context, pid int) (string, error) {
	return executionhost.DefaultReadProcessCommand(ctx, pid)
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
	retry            config.ReviewerRetryConfig
}

func (r *Runtime) reviewerRecoveryPolicyForProject(projectID string) runtimeReviewerRecoveryPolicy {
	roles := config.ProjectRoleConfigs(r.Config(), projectID)
	return runtimeReviewerRecoveryPolicy{
		includeDrafts:    roles.Reviewer.Discovery.Triggers.IncludeDrafts,
		stopOnApproved:   roles.Reviewer.Behavior.Loop.StopOnApproved,
		stopOnReadyLabel: roles.Reviewer.Behavior.Loop.StopOnReadyLabel,
		retry:            config.NormalizeReviewerRetryConfig(roles.Reviewer.Behavior.Retry),
	}
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
	bound, bindErr := hostingidentity.Bind(ctx, r.Config(), loop.ProjectID, "reviewer")
	if bindErr != nil {
		return "", false
	}
	loginCtx, cancel := context.WithTimeout(bound, reviewerRecoveryLoginTimeout)
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
	policy.retry = config.NormalizeReviewerRetryConfig(policy.retry)
	if runtimeIntFromAny(loopMeta["autoRecoveryAttempts"]) >= policy.retry.AutoRecoveryMaxAttempts {
		return false
	}
	checkpoint := parseRuntimeReviewerCheckpoint(latestRun.CheckpointJSON)
	queueKind := derefString(latestQueue.LastErrorKind)
	queueMessage := derefString(latestQueue.LastError)
	resumePolicy := loops.NormalizeResumePolicy(queueKind, checkpoint.ResumePolicy)
	if loops.SuppressesAutonomousRecovery(queueKind, resumePolicy) {
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
	return (queueKind == loops.FailureKindRetryableAfterResume && (resumePolicy == loops.ResumePolicyRestartFromDiscover || resumePolicy == "rerun_review")) || isRuntimeRetryableTransientWithRemainingAttempts(*latestQueue) || runtimeRecoverableEnhancedTransient(policy.retry, *latestQueue, failureSummary) || (isKnownReviewerRediscoveryGuardrail(failureSummary) && isRuntimeReviewerRediscoveryRunStep(latestRun))
}

func requeueFailedReviewerQueueItemForRecovery(ctx context.Context, repositories *storage.Repositories, loopID string, latestQueue *storage.QueueItemRecord, queuedAt string, policy runtimeReviewerRecoveryPolicy, matchedMessage string) (int64, error) {
	if latestQueue != nil && (isRuntimeRetryableTransientWithRemainingAttempts(*latestQueue) || runtimeRecoverableEnhancedTransient(policy.retry, *latestQueue, matchedMessage)) {
		return repositories.Queue.RequeueFailedByIDWithAttempts(ctx, loopID, latestQueue.ID, queuedAt, latestQueue.Attempts)
	}
	if latestQueue == nil {
		return 0, nil
	}
	return repositories.Queue.RequeueFailedByID(ctx, loopID, latestQueue.ID, queuedAt)
}

// requeueFailedReviewerWithSharedGuards requeues a failed reviewer under the
// same per-loop + same-target locks as API discard+retry, so deferred/startup
// recovery cannot activate a PR worktree sibling between discard preflight and
// git reset/clean. didRequeue is false when eligibility no longer holds under
// the lock (no error).
func requeueFailedReviewerWithSharedGuards(ctx context.Context, repositories *storage.Repositories, loop storage.LoopRecord, latestQueue *storage.QueueItemRecord, nowISO string, policy runtimeReviewerRecoveryPolicy, latestRun *storage.RunRecord) (didRequeue bool, recoveredQueueItems int64, err error) {
	unlock := LockLoopRequeue(loop.ID)
	defer unlock()
	unlockTarget := LockLoopTarget(LoopTargetGuardKeyFromRecord(loop))
	defer unlockTarget()

	// Re-check eligibility under the lock: discard/retry may have already
	// requeued or otherwise changed status while we waited.
	current, err := repositories.Loops.GetByID(ctx, loop.ID)
	if err != nil {
		return false, 0, err
	}
	if current == nil || !shouldAutoRecoverFailedReviewerLoop(*current, latestRun, latestQueue, policy) {
		return false, 0, nil
	}
	failureSummary := firstNonEmpty(derefString(latestRun.Summary), derefString(latestRun.ErrorMessage), derefString(latestQueue.LastError))
	recoveredQueueItems, err = requeueFailedReviewerQueueItemForRecovery(ctx, repositories, loop.ID, latestQueue, nowISO, policy, failureSummary)
	if err != nil {
		return false, 0, err
	}
	if recoveredQueueItems == 0 {
		active, activeErr := repositories.Queue.FindActiveByLoopID(ctx, loop.ID)
		if activeErr != nil {
			return false, 0, activeErr
		}
		if active == nil {
			return false, 0, fmt.Errorf("reviewer recovery did not requeue failed queue item %s for loop %s", latestQueue.ID, loop.ID)
		}
	}
	requeuedLoop := autoRecoveredReviewerLoop(*current, nowISO)
	if err := repositories.Loops.Upsert(ctx, requeuedLoop); err != nil {
		return false, 0, err
	}
	return true, recoveredQueueItems, nil
}

func isRuntimeRetryableTransientWithRemainingAttempts(queue storage.QueueItemRecord) bool {
	if derefString(queue.LastErrorKind) != "retryable_transient" {
		return false
	}
	return runtimeQueueHasRemainingAttempts(queue)
}

func runtimeRecoverableEnhancedTransient(policy config.ReviewerRetryConfig, queue storage.QueueItemRecord, message string) bool {
	policy = config.NormalizeReviewerRetryConfig(policy)
	return policy.RecoverExistingMatchedFailures && runtimeQueueHasRemainingAttempts(queue) && config.ReviewerRetryMessageMatches(policy, message)
}

func runtimeQueueHasRemainingAttempts(queue storage.QueueItemRecord) bool {
	return true
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
	return executionhost.ParseRuntimeJSONObject(value)
}

func runtimeReviewerLoopMetadata(meta map[string]any) map[string]any {
	return executionhost.RuntimeReviewerLoopMetadata(meta)
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
	return executionhost.RuntimeStringFromAny(value)
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
	return executionhost.IsDeprecatedReviewerLoopBudgetReason(reason)
}

func removeDeprecatedReviewerLoopBudgetMetadata(loopMeta map[string]any) {
	executionhost.RemoveDeprecatedReviewerLoopBudgetMetadata(loopMeta)
}

var deprecatedReviewerLoopBudgetMetadataKeys = executionhost.DeprecatedReviewerLoopBudgetMetadataKeys

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
	return executionhost.FirstNonEmpty(values...)
}

func shouldRequeueLoop(loop storage.LoopRecord, latestRun *storage.RunRecord, latestRunHasLiveAgent bool) bool {
	return executionhost.ShouldRequeueLoop(loop, latestRun, latestRunHasLiveAgent)
}

func derefRunID(run *storage.RunRecord) string {
	if run == nil {
		return ""
	}
	return run.ID
}

func terminalReviewerRecoveryMetadataStatus(loop storage.LoopRecord) string {
	return executionhost.TerminalReviewerRecoveryMetadataStatus(loop)
}

func normalizeTerminalReviewerLoopForRecovery(ctx context.Context, repositories *storage.Repositories, loop storage.LoopRecord, latestRun *storage.RunRecord, nowISO string) (bool, bool, error) {
	terminalReviewerStatus := terminalReviewerRecoveryMetadataStatus(loop)
	if terminalReviewerStatus == "" {
		return false, false, nil
	}
	cancelReason := "reviewer terminal metadata recovered during runtime recovery"
	cancelledQueueItems, err := repositories.Queue.CancelByLoop(ctx, loop.ID, nowISO, &cancelReason)
	if err != nil {
		return true, false, err
	}
	if loop.Status != terminalReviewerStatus || loop.NextRunAt != nil || cancelledQueueItems > 0 {
		normalizedLoop := loop
		normalizedLoop.Status = terminalReviewerStatus
		normalizedLoop.NextRunAt = nil
		if latestRun != nil {
			normalizedLoop.LastRunAt = coalesceString(latestRun.EndedAt, stringPtr(latestRun.StartedAt), loop.LastRunAt)
		}
		normalizedLoop.UpdatedAt = nowISO
		if err := repositories.Loops.Upsert(ctx, normalizedLoop); err != nil {
			return true, false, err
		}
		latestRunStatus := ""
		if latestRun != nil {
			latestRunStatus = latestRun.Status
		}
		if err := appendSystemEvent(ctx, repositories, storage.EventLogRecord{
			ID:         newRuntimeEventID(),
			EventType:  "looperd.recovery.reviewer_terminal_metadata_normalized",
			LoopID:     stringPtr(loop.ID),
			EntityType: stringPtr("loop"),
			EntityID:   stringPtr(loop.ID),
			PayloadJSON: mustMarshalJSON(map[string]any{
				"previousStatus":      loop.Status,
				"recoveredStatus":     normalizedLoop.Status,
				"latestRunStatus":     latestRunStatus,
				"cancelledQueueItems": cancelledQueueItems,
			}),
			CreatedAt: nowISO,
		}); err != nil {
			return true, false, err
		}
		return true, true, nil
	}
	return true, false, nil
}

func normalizeStaleQueuedLoopStatus(loop storage.LoopRecord, latestRun storage.RunRecord) string {
	return executionhost.NormalizeStaleQueuedLoopStatus(loop, latestRun)
}

func latestQueueIsManualIntervention(queue *storage.QueueItemRecord) bool {
	return executionhost.LatestQueueIsManualIntervention(queue)
}

func derefRunOrZero(run *storage.RunRecord) storage.RunRecord {
	return executionhost.DerefRunOrZero(run)
}

func mustMarshalJSON(value any) string {
	return executionhost.MustMarshalJSON(value)
}

func stringPtr(value string) *string {
	return executionhost.StringPtr(value)
}

func coalesceString(values ...*string) *string {
	return executionhost.CoalesceString(values...)
}

func stringOrEmpty(value *string) string {
	return executionhost.StringOrEmpty(value)
}

func newRuntimeEventID() string {
	return executionhost.NewRuntimeEventID()
}
