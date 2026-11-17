// Package collab hosts Looper's existing worker, queue, conversation transport
// and PR observer without booting the daemon or its other roles.
package collab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/nexu-io/looper/internal/agent"
	"github.com/nexu-io/looper/internal/bootstrap"
	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/disclosure"
	"github.com/nexu-io/looper/internal/eventlog"
	"github.com/nexu-io/looper/internal/executionhost"
	gitinfra "github.com/nexu-io/looper/internal/infra/git"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/infra/notify"
	"github.com/nexu-io/looper/internal/infra/planedoc"
	"github.com/nexu-io/looper/internal/loops"
	networkclient "github.com/nexu-io/looper/internal/network/client"
	"github.com/nexu-io/looper/internal/projects"
	"github.com/nexu-io/looper/internal/storage"
	"github.com/nexu-io/looper/internal/worker"
	"github.com/nexu-io/looper/internal/workerhost"
)

type Options struct {
	Config           config.Config
	StateDir         string
	Logger           bootstrap.Logger
	NetworkStatePath string
	BridgeEntry      string
}

func StateDir(cfg config.Config, override string) string {
	if override != "" {
		return override
	}
	return filepath.Join(filepath.Dir(cfg.Storage.DBPath), "collab")
}

func SocketPath(dir string) string { return filepath.Join(dir, "control.sock") }
func LogPath(dir string) string    { return filepath.Join(dir, "collab.log") }

// Serve stays available across tasks. The local socket is private to this OS
// user; it is not a remotely accessible, unauthenticated Plane endpoint.
func Serve(parent context.Context, options Options) error {
	if options.BridgeEntry != "" {
		return serveBridge(parent, options)
	}
	cfg := privateChatConfig(options.Config)
	dir, err := filepath.Abs(StateDir(cfg, options.StateDir))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	lock, err := executionhost.AcquireServiceLock(filepath.Join(dir, "service.lock"), "collab", time.Now())
	if err != nil {
		return err
	}
	defer lock.Release()
	if err := os.MkdirAll(filepath.Join(dir, "logs"), 0700); err != nil {
		return err
	}
	cfg.Daemon.LogDir = filepath.Join(dir, "logs")
	cfg.Storage.DBPath = filepath.Join(dir, "looper.sqlite")
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	coordinator, err := storage.OpenSQLiteCoordinator(ctx, cfg.Storage.DBPath, storage.SQLiteCoordinatorOptions{Migrations: storage.EmbeddedMigrations, BackupDir: filepath.Join(dir, "backups")})
	if err != nil {
		return err
	}
	closed := false
	defer func() {
		if !closed {
			_ = coordinator.Close()
		}
	}()
	if _, err := coordinator.MigrationRunner().RunPending(ctx, storage.RunPendingOptions{}); err != nil {
		return err
	}
	repos := storage.NewRepositories(coordinator.DB())
	chatManager := newTaskChatManager(cfg, repos, dir)
	git := gitinfra.New(gitinfra.Options{GitPath: value(cfg.Tools.GitPath), Repos: repos})
	projectService := projects.Service{DB: coordinator.DB(), Repos: repos, Config: cfg, DetectRepo: func(ctx context.Context, path string) (projects.DetectedRepo, error) {
		repo, err := git.DetectGitHubRepo(ctx, path)
		return projects.DetectedRepo{Repo: repo, Provider: "github"}, err
	}}
	if err := projectService.SyncConfigured(ctx, cfg, time.Now()); err != nil {
		return err
	}
	owner := executionhost.NewActiveExecutionRegistry()
	owner.SetAllowSpawn(func() error { return ctx.Err() })
	owner.SetOnHardPersistFailure(func(error) { cancel() })
	recovery := executionhost.Recovery{Config: func() config.Config { return cfg }, Logger: options.Logger, ReadProcessCommand: executionhost.DefaultReadProcessCommand, ActiveExecutions: owner}
	now := time.Now().UTC()
	orphans, err := recovery.RecoverOrphanExecutions(ctx, repos, eventlog.FormatJavaScriptISOString(now))
	if err != nil {
		return err
	}
	if _, err := recovery.ReconcileStaleRunningRunsWithMode(ctx, repos, now, executionhost.StaleRunReconcileModeStartup); err != nil {
		return err
	}
	if err := recovery.RecoverQueueClaims(ctx, repos, eventlog.FormatJavaScriptISOString(now)); err != nil {
		return err
	}
	if orphans.Cleanup.QuarantinedCount > 0 && options.Logger != nil {
		options.Logger.Warn("tasks need recovery confirmation; unrelated tasks can continue", map[string]any{"quarantinedExecutions": orphans.Cleanup.QuarantinedCount})
	}
	wake := make(chan struct{}, 1)
	requestWake := func() {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
	gh := githubinfra.New(githubinfra.Options{GHPath: value(cfg.Tools.GHPath), GitPath: value(cfg.Tools.GitPath)})
	networkStatePath := options.NetworkStatePath
	if networkStatePath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		networkStatePath = networkclient.DefaultStatePath(home)
	}
	network := networkclient.NewCollaborationManager(networkStatePath, cfg, repos, gh)
	if err := network.Start(ctx); err != nil {
		return fmt.Errorf("start existing loopernet connection: %w", err)
	}
	defer network.Stop()
	gateway := notify.NewGateway(notify.Options{Config: cfg.Notifications, Repositories: repos, FeishuCLI: workerhost.PrivateFeishuClient(&cfg), LogFilePath: LogPath(dir), ResolveOwnerOpenID: func(project string) string { return config.ProjectOwner(cfg, project) }})
	resolved, ok := config.ResolveAgent(cfg, "", config.CodingRoleWorker)
	if !ok {
		return fmt.Errorf("configure an agent for the worker role before starting collab")
	}
	executor := agent.New(agent.ExecutorOptions{Config: agent.ExecutorConfig{Vendor: resolved.Vendor, Model: resolved.Model, Params: cfg.Agent.Params, Env: cfg.Agent.Env, NativeResumeEnabled: cfg.Agent.NativeResume.Enabled, LiveToolEvents: true, ClaudeJSONEvents: true}, ParamsOwnerVendor: cfg.Agent.Vendor, Repos: repos, LogDir: cfg.Daemon.LogDir, Owner: owner, OnHardPersistFailure: func(error) { cancel() }, OnProgress: func(ctx context.Context, update agent.ProgressUpdate) {
		gateway.RefreshThreadHeader(ctx, update.LoopID, update.TailLines, update.ElapsedSeconds)
	}})
	stamper := disclosure.FromConfig(cfg)
	runner := worker.New(worker.Options{
		DB: coordinator.DB(), Repos: repos, GitHub: workerhost.GitHub{Gateway: gh, Stamper: stamper, Config: &cfg}, Git: workerhost.Git{Gateway: git, Stamper: stamper}, AgentExecutor: workerhost.Executor{Executor: executor}, Logger: options.Logger,
		ContainmentTracker: owner, AllowAutoCommit: cfg.Defaults.AllowAutoCommit, AllowAutoPush: cfg.Defaults.AllowAutoPush, OpenPRStrategy: cfg.Defaults.OpenPRStrategy, ShepherdEnabled: true,
		CustomInstructions: &cfg, AgentRuntime: string(resolved.Vendor), AgentModel: resolved.Model, AgentProfileID: resolved.ProfileID, Disclosure: &cfg.Disclosure,
		AgentTimeout: time.Duration(cfg.Agent.Timeouts.WorkerMaxRuntimeSeconds) * time.Second, AgentIdleTimeout: time.Duration(cfg.Agent.Timeouts.WorkerIdleTimeoutSeconds) * time.Second,
		RetryBaseDelay: time.Duration(cfg.Scheduler.RetryBaseDelayMS) * time.Millisecond, RetryMaxAttempts: int64(cfg.Scheduler.RetryMaxAttempts), OnQueueItemEnqueued: requestWake,
		PlaneDoc: func(project string) (*planedoc.Gateway, string, bool) {
			return workerhost.PlaneDocForProject(&cfg, project)
		},
		HITLEnabled: cfg.HITL.Enabled, HITLAnswerTransport: cfg.HITL.AnswerTransport, HITLNotify: func(ctx context.Context, ask worker.HITLAskNotification) error {
			return workerhost.PostPrivateFeishuQuestion(ctx, &cfg, repos, gateway, ask.LoopID)
		},
		OnAgentExecutionStarted: func(ctx context.Context, input worker.AgentExecutionStartedInput) error {
			gateway.RefreshThreadHeader(ctx, input.LoopID, nil, 0)
			return nil
		},
		OnRunCompleted: func(ctx context.Context, input worker.RunCompletedInput) error {
			return workerhost.NotifyWorkerCompleted(ctx, cfg, repos, gateway, options.Logger, input)
		},
	})
	shepherd := workerhost.Shepherd{Config: cfg, Repos: repos, GitHub: gh, Notify: gateway, Logger: options.Logger, Wake: requestWake}
	sources, err := planeSources(cfg)
	if err != nil {
		return err
	}
	for _, source := range sources {
		if status := network.Status(); status.Configured && source.nodeID != status.NodeID {
			return fmt.Errorf("Plane project %s is bound to node %s, but loopernet is connected as %s; reconnect this project to the current device", source.project.ID, source.nodeID, status.NodeID)
		}
	}
	loopService := loops.Service{DB: coordinator.DB(), Repos: repos}
	var wg sync.WaitGroup
	workErr := make(chan error, 1)
	wg.Add(2)
	go func() {
		defer wg.Done()
		if err := work(ctx, runner, repos, owner, chatManager, wake, options.Logger); err != nil {
			workErr <- err
			cancel()
		}
	}()
	go func() {
		defer wg.Done()
		tick := time.NewTicker(10 * time.Second)
		defer tick.Stop()
		lastPR := time.Time{}
		for {
			if status := network.Status(); status.CloudReachable && !status.IdentityDrift {
				for _, source := range sources {
					if err := source.poll(ctx, runner, &loopService, repos, owner); err != nil && ctx.Err() == nil && options.Logger != nil {
						options.Logger.Warn("Plane dispatch poll failed", map[string]any{"project": source.project.ID, "error": err.Error()})
					}
				}
				requestWake()
			}
			if time.Since(lastPR) >= time.Minute {
				shepherd.Reconcile(ctx)
				lastPR = time.Now()
			}
			err := workerhost.PollPrivateFeishu(ctx, workerhost.FeishuPollInput{Config: &cfg, Repos: repos, NotificationGateway: gateway, Now: time.Now, AllowClaim: func() error { return ctx.Err() }, OnHITLAnswerDelivered: func(ctx context.Context, loop, answer string) {
				gateway.MarkAskAnswered(ctx, loop, answer)
				requestWake()
			}})
			if err != nil && ctx.Err() == nil && options.Logger != nil {
				options.Logger.Warn("Feishu reply poll failed", map[string]any{"error": err.Error()})
			}
			_, _ = gateway.RetryPendingCards(ctx, 20)
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	}()
	// The file lock above proves any prior socket belongs to a stopped host.
	_ = os.Remove(SocketPath(dir))
	listener, err := net.Listen("unix", SocketPath(dir))
	if err != nil {
		cancel()
		_ = owner.BeginShutdown("listen failed")
		wg.Wait()
		return err
	}
	if err := os.Chmod(SocketPath(dir), 0600); err != nil {
		_ = listener.Close()
		cancel()
		_ = owner.BeginShutdown("socket permissions failed")
		wg.Wait()
		return err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		items, err := repos.Loops.List(r.Context())
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		tasks := make([]Task, 0, len(items))
		for _, item := range items {
			tasks = append(tasks, taskStatus(item))
		}
		state := "running"
		if ctx.Err() != nil {
			state = "stopping"
		}
		writeJSON(w, Status{Service: "looper collab", PID: os.Getpid(), State: state, ActiveExecutions: owner.LiveCount(), Network: network.Status(), Tasks: tasks, PlaneTargets: configuredPlaneTargets(cfg)})
	})
	mux.HandleFunc("POST /run", func(w http.ResponseWriter, r *http.Request) {
		var input worker.EnqueueInput
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20)).Decode(&input); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if ctx.Err() != nil {
			http.Error(w, "collab is stopping", 503)
			return
		}
		item, err := runner.Enqueue(r.Context(), input)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		requestWake()
		writeJSON(w, taskStatus(item))
	})
	stopped := make(chan struct{})
	var stopErr error
	mux.HandleFunc("POST /stop", func(w http.ResponseWriter, r *http.Request) {
		cancel()
		select {
		case <-stopped:
			if stopErr != nil {
				http.Error(w, stopErr.Error(), http.StatusInternalServerError)
				return
			}
			writeJSON(w, map[string]string{"status": "stopped"})
		case <-r.Context().Done():
		}
	})
	server := http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	select {
	case <-ctx.Done():
	case err = <-serveErr:
		cancel()
	}
	drainErr := owner.BeginShutdown("collab stopped")
	wg.Wait()
	network.Stop()
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	select {
	case workerErr := <-workErr:
		err = errors.Join(err, workerErr)
	default:
	}
	stopErr = errors.Join(err, drainErr)
	close(stopped)
	shutdownCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	_ = server.Shutdown(shutdownCtx)
	stop()
	if drainErr != nil {
		closed = true
		return errors.Join(err, fmt.Errorf("execution drain failed; storage retained: %w", drainErr))
	}
	closed = true
	closeErr := coordinator.Close()
	return errors.Join(err, closeErr)
}

// The host only invokes the existing queue and worker. Operation leases cover
// claim-through-finalization just as they do in the daemon scheduler.
func work(ctx context.Context, runner *worker.Runner, repos *storage.Repositories, owner *executionhost.ActiveExecutionRegistry, chats *taskChatManager, wake <-chan struct{}, logger bootstrap.Logger) error {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for ctx.Err() == nil {
		lease, err := owner.AdmitOperation(ctx, executionhost.OperationMeta{ClaimedBy: "collab"})
		if err != nil {
			return nil
		}
		nowISO := eventlog.FormatJavaScriptISOString(time.Now().UTC())
		claimCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		item, err := repos.Queue.ClaimNextOfType(claimCtx, nowISO, "collab", "worker")
		cancel()
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			var recoveryErr error
			item, recoveryErr = executionhost.RecoverCancelledClaim(ctx, repos, "collab", nowISO, owner.OwnsQueueClaim)
			if recoveryErr != nil {
				// An unobserved durable claim may exist. Retain its pending
				// lease, stop admission and report the persistence failure.
				owner.ReportHardPersistFailure(recoveryErr)
				return recoveryErr
			}
			if item != nil {
				err = nil
			}
		}
		if err == nil && item != nil {
			permit, bindErr := lease.BindClaim(*item)
			var finalizeErr error
			if bindErr == nil && permit.Valid() && lease.Context().Err() == nil {
				// Prepare routing here, after claiming and before any worker
				// notification. Intake and background polling must not race
				// execution or move an active conversation to another group.
				if chatErr := chats.prepare(lease.Context(), value(item.LoopID)); chatErr != nil && logger != nil {
					logger.Warn("Feishu task chat preparation failed; using existing or fallback chat", map[string]any{"loopId": value(item.LoopID), "error": chatErr.Error()})
				}
				_, err = runner.ProcessClaimedQueueItem(lease.Context(), *item)
				if lease.Context().Err() != nil {
					finalizeErr = executionhost.FinalizeCancelledClaim(ctx, repos, *item, time.Now)
				} else {
					finalizeErr = executionhost.EnsureClaimFinalized(ctx, repos, *item, err, time.Now)
				}
			} else {
				finalizeErr = executionhost.FinalizeCancelledClaim(ctx, repos, *item, time.Now)
			}
			if finalizeErr != nil {
				// A running claim must never outlive a released owner.
				owner.ReportHardPersistFailure(finalizeErr)
				return finalizeErr
			}
		}
		lease.Release()
		if err != nil && ctx.Err() == nil && logger != nil {
			logger.Warn("worker pass failed", map[string]any{"error": err.Error()})
		}
		if item != nil && err == nil {
			continue
		}
		select {
		case <-ctx.Done():
			return nil
		case <-wake:
		case <-tick.C:
		}
	}
	return nil
}

func value(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
func writeJSON(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(data)
}

// The explicit private HITL chat is also the destination for progress cards.
// CLI authentication is reused; no webhook or second chat configuration is needed.
func privateChatConfig(cfg config.Config) config.Config {
	client := workerhost.PrivateFeishuClient(&cfg)
	if client == nil {
		return cfg
	}
	cfg.Notifications.Webhook.Enabled = true
	cfg.Notifications.Webhook.Mode = "app"
	cfg.Notifications.Webhook.ChatID = client.Config.ChatID
	cfg.Projects = append([]config.ProjectRefConfig(nil), cfg.Projects...)
	for i := range cfg.Projects {
		if cfg.Projects[i].Owner == nil {
			cfg.Projects[i].Owner = &config.FeishuActorConfig{FeishuOpenID: client.Config.OwnerOpenID}
		}
	}
	return cfg
}
