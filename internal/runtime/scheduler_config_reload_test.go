package runtime

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	coordinatorrole "github.com/nexu-io/looper/internal/coordinator"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/infra/shell"
	"github.com/nexu-io/looper/internal/projects"
	"github.com/nexu-io/looper/internal/storage"
)

func TestClaimPhaseRefreshesConfigAfterPublicationBoundary(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	coordinator := openMigratedCoordinator(t, filepath.Join(root, "claim-refresh.sqlite"), t.TempDir())
	repositories := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.July, 16, 10, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	insertSchedulerProject(t, repositories, root, nowISO)
	item := schedulerTestQueueItem("queue_worker_after_config_publish", "worker", nowISO)
	if err := repositories.Queue.Upsert(context.Background(), item); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	oldRunner := &stubWorkerScheduler{}
	newRunner := &stubWorkerScheduler{}
	latestRunner := workerScheduler(oldRunner)
	publicationBoundary := &sync.RWMutex{}
	publicationBoundary.Lock()
	refreshed := make(chan struct{})

	input := defaultSchedulerTickInput{
		Repos:             repositories,
		Now:               func() time.Time { return now },
		MaxConcurrentRuns: 0,
		ClaimBoundary:     publicationBoundary,
		AsyncRunner:       immediateSchedulerRunner{},
		Worker:            oldRunner,
	}
	input.RefreshForClaim = func() defaultSchedulerTickInput {
		close(refreshed)
		latest := input
		latest.MaxConcurrentRuns = 1
		latest.Worker = latestRunner
		latest.RefreshForClaim = nil
		return latest
	}

	done := make(chan error, 1)
	go func() {
		_, _, err := executeClaimPhase(context.Background(), "test", input, nil, false)
		done <- err
	}()
	refreshedEarly := false
	select {
	case <-refreshed:
		refreshedEarly = true
	case <-time.After(25 * time.Millisecond):
	}
	latestRunner = newRunner
	publicationBoundary.Unlock()

	if err := <-done; err != nil {
		t.Fatalf("executeClaimPhase() error = %v", err)
	}
	if refreshedEarly {
		t.Fatal("claim refreshed while config publication held the write boundary")
	}
	if oldRunner.processItemCount() != 0 || newRunner.processItemCount() != 1 {
		t.Fatalf("processed items: old=%d new=%d, want old=0 new=1", oldRunner.processItemCount(), newRunner.processItemCount())
	}
}

func TestCatalogSchedulerStartsUsingVendorPublishedAfterDaemonStartup(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	cfg, err := config.DefaultConfig(root)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Agent.Vendor = nil
	catalog := projects.NewCatalog(cfg)
	coordinator, err := storage.OpenSQLiteCoordinator(context.Background(), filepath.Join(root, "looper.sqlite"), storage.SQLiteCoordinatorOptions{})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	if _, err := coordinator.MigrationRunner().RunPending(context.Background()); err != nil {
		t.Fatalf("RunPending() error = %v", err)
	}
	repositories := storage.NewRepositories(coordinator.DB())
	logger := &capturingSchedulerLogger{}
	handlers := buildCatalogSchedulerHandlers(
		catalog,
		nil,
		"",
		logger,
		coordinator,
		repositories,
		nil,
		nil,
		NewActiveExecutionRegistry(),
		nil,
		nil,
		time.Now,
		nil,
		nil,
		nil,
	)
	if handlers.webhook != nil {
		t.Cleanup(handlers.webhook.Close)
	}

	// Without a live vendor, handlers still build so sticky snapshot retries can
	// claim, but discovery stays off, webhooks stay nil, and claimTypeSets keeps
	// coding roles sticky-snapshot-only (no unrestricted fresh claims).
	if err := handlers.tick(context.Background(), Services{Repositories: repositories}); err != nil {
		t.Fatalf("tick without vendor error = %v", err)
	}
	before := handlers.snapshot()
	if before.input == nil {
		t.Fatal("handlers.input = nil without vendor, want sticky-retry runners available")
	}
	beforeInput := before.input(Services{Repositories: repositories})
	if beforeInput.Planner == nil || beforeInput.Worker == nil || beforeInput.Reviewer == nil || beforeInput.Fixer == nil {
		t.Fatal("coding role runners nil without vendor, want present for sticky snapshot retries")
	}
	if discoveryEnabled(beforeInput.PlannerDiscoveryEnabled) || discoveryEnabled(beforeInput.WorkerDiscoveryEnabled) ||
		discoveryEnabled(beforeInput.ReviewerDiscoveryEnabled) || discoveryEnabled(beforeInput.FixerDiscoveryEnabled) {
		t.Fatal("discovery enabled without vendor, want gated until ResolveAgent succeeds")
	}
	if before.reviewer != nil || before.fixer != nil {
		t.Fatal("webhook reviewer/fixer non-nil without vendor, want nil to block new webhook discovery")
	}
	unrestricted, stickyOnly := claimTypeSetsFromInput(beforeInput)
	if len(unrestricted) != 0 {
		t.Fatalf("unrestricted claim types without vendor = %v, want empty", unrestricted)
	}
	if len(stickyOnly) != 4 {
		t.Fatalf("sticky-only claim types without vendor = %v, want planner/worker/reviewer/fixer", stickyOnly)
	}

	next := config.CloneConfig(cfg)
	vendor := config.AgentVendorCodex
	next.Agent.Vendor = &vendor
	catalog.PublishGlobals(next)
	if err := handlers.tick(context.Background(), Services{Repositories: repositories}); err != nil {
		t.Fatalf("tick after vendor publication error = %v", err)
	}
	if !schedulerLoggerContains(logger, "scheduler tick summary") {
		t.Fatal("scheduler did not rebuild handlers after agent.vendor was published")
	}
	after := handlers.snapshot()
	if after.reviewer == nil || after.fixer == nil {
		t.Fatal("webhook reviewer/fixer nil after vendor publication, want configured")
	}
}

func TestBuildDefaultSchedulerHandlers_PerRoleAgentVendors(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	cfg, err := config.DefaultConfig(root)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	// No global vendor: only worker + reviewer resolve via role bindings.
	cfg.Agent.Vendor = nil
	workerVendor := config.AgentVendorCodex
	reviewerVendor := config.AgentVendorClaudeCode
	cfg.Roles.Worker.Agent = &config.RoleAgentConfig{Vendor: &workerVendor}
	cfg.Roles.Reviewer.Agent = &config.RoleAgentConfig{Vendor: &reviewerVendor}

	coordinator, err := storage.OpenSQLiteCoordinator(context.Background(), filepath.Join(root, "looper.sqlite"), storage.SQLiteCoordinatorOptions{})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	if _, err := coordinator.MigrationRunner().RunPending(context.Background()); err != nil {
		t.Fatalf("RunPending() error = %v", err)
	}
	repositories := storage.NewRepositories(coordinator.DB())

	handlers := buildDefaultSchedulerHandlersWithOptions(
		cfg,
		"",
		&capturingSchedulerLogger{},
		coordinator,
		repositories,
		nil,
		nil,
		NewActiveExecutionRegistry(),
		nil,
		nil,
		time.Now,
		nil,
		false,
		nil,
		nil,
		newSchedulerNotificationGatewayFactory(),
		coordinatorrole.NewRuntimeState(),
	)
	if handlers.input == nil {
		t.Fatal("handlers.input = nil")
	}
	input := handlers.input(Services{Repositories: repositories})
	if input.Worker == nil {
		t.Fatal("Worker runner = nil, want configured for role-only worker vendor")
	}
	if input.Reviewer == nil {
		t.Fatal("Reviewer runner = nil, want configured for role-only reviewer vendor")
	}
	// Unconfigured roles still get runners so sticky snapshot retries remain claimable.
	if input.Planner == nil {
		t.Fatal("Planner runner = nil, want present for sticky snapshot retries when planner agent not configured")
	}
	if input.Fixer == nil {
		t.Fatal("Fixer runner = nil, want present for sticky snapshot retries when fixer agent not configured")
	}
	if discoveryEnabled(input.PlannerDiscoveryEnabled) || discoveryEnabled(input.FixerDiscoveryEnabled) {
		t.Fatal("planner/fixer discovery enabled without role agent, want gated")
	}
	if discoveryEnabled(input.WorkerDiscoveryEnabled) != config.AnyProjectRoleAutoDiscoveryEnabled(cfg, "worker") {
		t.Fatalf("worker discovery enabled = %v, want auto-discovery when role agent configured", discoveryEnabled(input.WorkerDiscoveryEnabled))
	}
	// ResolveAgent identity must differ for the two configured roles.
	workerResolved, workerOK := config.ResolveAgent(cfg, "", config.CodingRoleWorker)
	reviewerResolved, reviewerOK := config.ResolveAgent(cfg, "", config.CodingRoleReviewer)
	if !workerOK || !reviewerOK {
		t.Fatalf("ResolveAgent ok worker=%v reviewer=%v", workerOK, reviewerOK)
	}
	if workerResolved.Vendor == reviewerResolved.Vendor {
		t.Fatalf("worker and reviewer vendors both %q, want different", workerResolved.Vendor)
	}
	// Live-configured roles are unrestricted; unconfigured roles are sticky-only.
	unrestricted, stickyOnly := claimTypeSetsFromInput(input)
	wantUnrestricted := map[string]bool{"worker": true, "reviewer": true}
	for _, got := range unrestricted {
		delete(wantUnrestricted, got)
	}
	if len(wantUnrestricted) != 0 {
		t.Fatalf("unrestricted claim types missing %v; got %v", wantUnrestricted, unrestricted)
	}
	for _, got := range unrestricted {
		if got == "planner" || got == "fixer" {
			t.Fatalf("unrestricted claim types = %v, must not include unconfigured planner/fixer", unrestricted)
		}
	}
	wantSticky := map[string]bool{"planner": true, "fixer": true}
	for _, got := range stickyOnly {
		delete(wantSticky, got)
	}
	if len(wantSticky) != 0 {
		t.Fatalf("sticky-only claim types missing %v; got %v", wantSticky, stickyOnly)
	}
}

func TestCatalogSchedulerPreservesCoordinatorThrottleAcrossConfigSnapshots(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	cfg, err := config.DefaultConfig(root)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	vendor := config.AgentVendorCodex
	cfg.Agent.Vendor = &vendor
	cfg.Roles.Coordinator.Enabled = true
	cfg.Roles.Coordinator.PollInterval = "5m"
	cfg.Roles.Reviewer.Discovery.AutoDiscovery = false
	catalog := projects.NewCatalog(cfg)
	coordinator := openMigratedCoordinator(t, filepath.Join(root, "coordinator-throttle.sqlite"), t.TempDir())
	repositories := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.July, 16, 10, 0, 0, 0, time.UTC)
	insertSchedulerProject(t, repositories, root, formatJavaScriptISOString(now))
	githubGateway := githubinfra.New(githubinfra.Options{GHRun: func(context.Context, shell.Options) (shell.Result, error) {
		return shell.Result{Stdout: "[]"}, nil
	}})
	handlers := buildCatalogSchedulerHandlers(
		catalog,
		nil,
		"",
		&capturingSchedulerLogger{},
		coordinator,
		repositories,
		nil,
		githubGateway,
		NewActiveExecutionRegistry(),
		nil,
		nil,
		func() time.Time { return now },
		nil,
		nil,
		nil,
	)
	if handlers.webhook != nil {
		t.Cleanup(handlers.webhook.Close)
	}

	discover := func() coordinatorrole.DiscoveryResult {
		t.Helper()
		snapshot := handlers.snapshot()
		if snapshot.input == nil {
			t.Fatal("catalog scheduler snapshot has no input builder")
		}
		result, err := snapshot.input(Services{Repositories: repositories}).Coordinator.DiscoverIssues(context.Background(), coordinatorrole.DiscoveryInput{ProjectID: "looper", Repo: "nexu-io/looper"})
		if err != nil {
			t.Fatalf("DiscoverIssues() error = %v", err)
		}
		return result
	}

	if result := discover(); !result.Ticked || result.Skipped {
		t.Fatalf("first discovery result = %#v, want ticked", result)
	}
	now = now.Add(2 * time.Minute)
	if result := discover(); !result.Skipped || result.Ticked {
		t.Fatalf("second discovery result = %#v, want shared 5m throttle to skip", result)
	}

	next := config.CloneConfig(cfg)
	next.Roles.Coordinator.PollInterval = "1m"
	catalog.PublishGlobals(next)
	if result := discover(); !result.Ticked || result.Skipped {
		t.Fatalf("discovery after interval reload = %#v, want latest 1m policy to tick", result)
	}
}

func schedulerLoggerContains(logger *capturingSchedulerLogger, message string) bool {
	logger.mu.Lock()
	defer logger.mu.Unlock()
	for _, entry := range logger.entries {
		if entry.message == message {
			return true
		}
	}
	return false
}
