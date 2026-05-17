package runtime

import (
	"context"
	"database/sql"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
)

func TestSchedulerFullPollIntervalUsesWebhookFallbackWhenEnabled(t *testing.T) {
	t.Parallel()

	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Scheduler.PollIntervalSeconds = 45
	cfg.Webhook.Enabled = true
	cfg.Webhook.FallbackPollIntervalSeconds = 300

	if got := schedulerFullPollInterval(cfg); got != 5*time.Minute {
		t.Fatalf("schedulerFullPollInterval() = %v, want %v", got, 5*time.Minute)
	}

	cfg.Webhook.Enabled = false
	if got := schedulerFullPollInterval(cfg); got != 45*time.Second {
		t.Fatalf("schedulerFullPollInterval() with webhook disabled = %v, want %v", got, 45*time.Second)
	}
}

func TestNewWebhookRuntimeDoesNotDegradeHealthyWebhookMode(t *testing.T) {
	t.Parallel()

	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Webhook.Enabled = true
	cfg.Webhook.FallbackPollIntervalSeconds = 300
	host := "127.0.0.1"
	ghPath := "/usr/bin/gh"
	cfg.Server.Host = host
	cfg.Tools.GHPath = &ghPath

	rt := newWebhookRuntime(cfg, &testLogger{}, func() time.Time { return time.Unix(0, 0) })
	status := rt.Status()
	if status.Degraded {
		t.Fatalf("Status().Degraded = true, want false; reasons=%v", status.DegradedReasons)
	}
	if len(status.DegradedReasons) != 0 {
		t.Fatalf("Status().DegradedReasons = %v, want empty", status.DegradedReasons)
	}
}

func TestNewWebhookRuntimeBracketsIPv6EndpointURL(t *testing.T) {
	t.Parallel()

	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Webhook.Enabled = true
	cfg.Server.Host = "::1"

	rt := newWebhookRuntime(cfg, &testLogger{}, func() time.Time { return time.Unix(0, 0) })
	status := rt.Status()
	if status.EndpointURL != "http://[::1]:17310/webhook/forward" {
		t.Fatalf("Status().EndpointURL = %q, want %q", status.EndpointURL, "http://[::1]:17310/webhook/forward")
	}
}

func TestWebhookRuntimeClearsForwarderDegradedReasonsAfterRecovery(t *testing.T) {
	t.Parallel()

	rt := &webhookRuntime{status: WebhookStatus{Degraded: true, DegradedReasons: []string{
		"forwarder for nexu-io/looper failed: temporary network error",
		"server.host is not loopback; webhook forwarders require a loopback daemon endpoint",
	}}}

	rt.clearForwarderDegradedReasons("nexu-io/looper")
	status := rt.Status()
	if !status.Degraded {
		t.Fatal("Status().Degraded = false, want true while non-forwarder reasons remain")
	}
	if len(status.DegradedReasons) != 1 || !strings.Contains(status.DegradedReasons[0], "server.host is not loopback") {
		t.Fatalf("Status().DegradedReasons = %v, want only non-forwarder reason", status.DegradedReasons)
	}

	rt.clearDegradedReasons(func(string) bool { return true })
	status = rt.Status()
	if status.Degraded {
		t.Fatalf("Status().Degraded = true, want false after clearing all reasons; reasons=%v", status.DegradedReasons)
	}
	if len(status.DegradedReasons) != 0 {
		t.Fatalf("Status().DegradedReasons = %v, want empty", status.DegradedReasons)
	}
}

func TestWebhookRuntimeRunForwarderClearsRecoveredForwarderReason(t *testing.T) {
	testBin, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() error = %v", err)
	}
	startedCh := make(chan struct{})
	originalCommand := execCommand
	originalStartedHook := webhookForwarderStartedHook
	execCommand = func(name string, args ...string) *exec.Cmd {
		cmd := exec.Command(testBin, "-test.run=TestWebhookRuntimeForwarderHelperProcess", "--")
		cmd.Env = append(os.Environ(), "GO_WANT_HELPER_PROCESS=1")
		cmd.Args[0] = name
		return cmd
	}
	webhookForwarderStartedHook = func() {
		close(startedCh)
	}
	t.Cleanup(func() {
		execCommand = originalCommand
		webhookForwarderStartedHook = originalStartedHook
	})

	rt := &webhookRuntime{
		status: WebhookStatus{
			Enabled:  true,
			Degraded: true,
			DegradedReasons: []string{
				"forwarder for nexu-io/looper failed: temporary network error",
				"server.host is not loopback; webhook forwarders require a loopback daemon endpoint",
			},
			Forwarders: []WebhookForwarderState{{Repo: "nexu-io/looper", Command: []string{"gh", "webhook", "forward"}}},
		},
		stopCh:          make(chan struct{}),
		forwarderStopCh: map[string]chan struct{}{"nexu-io/looper": make(chan struct{})},
		now:             time.Now,
	}
	t.Cleanup(rt.Stop)

	rt.launchForwarder("nexu-io/looper")
	<-startedCh

	deadline := time.After(5 * time.Second)
	for {
		status := rt.Status()
		if status.Forwarders[0].Running {
			if !status.Degraded {
				t.Fatal("Status().Degraded = false, want true while unrelated degraded reason remains")
			}
			if len(status.DegradedReasons) != 1 || !strings.Contains(status.DegradedReasons[0], "server.host is not loopback") {
				t.Fatalf("Status().DegradedReasons = %v, want only non-forwarder reason after recovery", status.DegradedReasons)
			}
			return
		}
		select {
		case <-deadline:
			t.Fatal("forwarder did not reach running state")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func TestWebhookRuntimeStopKillsForwarderStartedBeforePIDPublication(t *testing.T) {
	testBin, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() error = %v", err)
	}
	startedCh := make(chan struct{})
	releaseCh := make(chan struct{})
	originalCommand := execCommand
	originalStartedHook := webhookForwarderStartedHook
	execCommand = func(name string, args ...string) *exec.Cmd {
		cmd := exec.Command(testBin, "-test.run=TestWebhookRuntimeForwarderHelperProcess", "--")
		cmd.Env = append(os.Environ(), "GO_WANT_HELPER_PROCESS=1")
		cmd.Args[0] = name
		return cmd
	}
	webhookForwarderStartedHook = func() {
		close(startedCh)
		<-releaseCh
	}
	t.Cleanup(func() {
		execCommand = originalCommand
		webhookForwarderStartedHook = originalStartedHook
	})

	rt := &webhookRuntime{
		status: WebhookStatus{
			Enabled:    true,
			Forwarders: []WebhookForwarderState{{Repo: "nexu-io/looper", Command: []string{"gh", "webhook", "forward"}}},
		},
		stopCh:          make(chan struct{}),
		forwarderStopCh: map[string]chan struct{}{"nexu-io/looper": make(chan struct{})},
		now:             time.Now,
	}
	rt.launchForwarder("nexu-io/looper")
	<-startedCh

	stopDone := make(chan struct{})
	go func() {
		rt.Stop()
		close(stopDone)
	}()
	close(releaseCh)

	select {
	case <-stopDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop() did not return after forwarder started before PID publication")
	}

	status := rt.Status()
	if status.Forwarders[0].Running {
		t.Fatal("Status().Forwarders[0].Running = true, want false after Stop()")
	}
	if status.Forwarders[0].PID != nil {
		t.Fatalf("Status().Forwarders[0].PID = %v, want nil after Stop()", *status.Forwarders[0].PID)
	}
}

func TestWebhookRuntimeReconcileAddsMissingForwardersWithoutDuplicates(t *testing.T) {
	t.Parallel()

	repositories := openWebhookRuntimeTestRepositories(t)
	nowISO := formatJavaScriptISOString(time.Date(2026, time.May, 16, 12, 0, 0, 0, time.UTC))
	metadataOne := `{"repo":"nexu-io/looper"}`
	metadataTwo := `{"repo":"nexu-io/other"}`
	if err := repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "project_1", Name: "Looper", RepoPath: "/tmp/looper", MetadataJSON: &metadataOne, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert(project_1) error = %v", err)
	}
	if err := repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "project_2", Name: "Other", RepoPath: "/tmp/other", MetadataJSON: &metadataTwo, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert(project_2) error = %v", err)
	}

	rt := &webhookRuntime{
		ghPath: "/usr/bin/gh",
		status: WebhookStatus{
			Enabled:         true,
			EndpointURL:     "http://127.0.0.1:7777/webhook/forward",
			Degraded:        true,
			DegradedReasons: []string{noConfiguredWebhookReposReason},
			Forwarders: []WebhookForwarderState{{
				Repo:    "nexu-io/looper",
				Command: []string{"/usr/bin/gh", "webhook", "forward", "--repo", "nexu-io/looper", "--events", strings.Join(webhookForwardEvents, ","), "--url", "http://127.0.0.1:7777/webhook/forward"},
			}},
		},
		stopCh:          make(chan struct{}),
		forwarderStopCh: map[string]chan struct{}{"nexu-io/looper": make(chan struct{})},
		now:             time.Now,
	}
	t.Cleanup(rt.Stop)

	rt.Reconcile(repositories)
	status := rt.Status()
	if status.Degraded {
		t.Fatalf("Status().Degraded = true, want false after repos become available; reasons=%v", status.DegradedReasons)
	}
	if len(status.Forwarders) != 2 {
		t.Fatalf("len(Status().Forwarders) = %d, want 2", len(status.Forwarders))
	}
	if status.Forwarders[0].Repo != "nexu-io/looper" {
		t.Fatalf("Status().Forwarders[0].Repo = %q, want nexu-io/looper", status.Forwarders[0].Repo)
	}
	if status.Forwarders[1].Repo != "nexu-io/other" {
		t.Fatalf("Status().Forwarders[1].Repo = %q, want nexu-io/other", status.Forwarders[1].Repo)
	}

	rt.Reconcile(repositories)
	status = rt.Status()
	if len(status.Forwarders) != 2 {
		t.Fatalf("len(Status().Forwarders) after second reconcile = %d, want 2", len(status.Forwarders))
	}
}

func TestWebhookRuntimeReconcileClearsTransientListFailureAfterRecovery(t *testing.T) {
	testBin, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() error = %v", err)
	}
	startedCh := make(chan struct{})
	originalCommand := execCommand
	originalStartedHook := webhookForwarderStartedHook
	execCommand = func(name string, args ...string) *exec.Cmd {
		cmd := exec.Command(testBin, "-test.run=TestWebhookRuntimeForwarderHelperProcess", "--")
		cmd.Env = append(os.Environ(), "GO_WANT_HELPER_PROCESS=1")
		cmd.Args[0] = name
		return cmd
	}
	webhookForwarderStartedHook = func() {
		close(startedCh)
	}
	t.Cleanup(func() {
		execCommand = originalCommand
		webhookForwarderStartedHook = originalStartedHook
	})

	failingDBPath := t.TempDir() + "/failing-runtime.sqlite"
	coordinator, err := storage.OpenSQLiteCoordinator(context.Background(), failingDBPath, storage.SQLiteCoordinatorOptions{})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	if _, err := coordinator.MigrationRunner().RunPending(context.Background()); err != nil {
		t.Fatalf("RunPending() error = %v", err)
	}
	failingRepositories := storage.NewRepositories(coordinator.DB())
	if err := coordinator.Close(); err != nil {
		t.Fatalf("Coordinator.Close() error = %v", err)
	}

	healthyRepositories := openWebhookRuntimeTestRepositories(t)
	nowISO := formatJavaScriptISOString(time.Date(2026, time.May, 16, 12, 0, 0, 0, time.UTC))
	metadata := `{"repo":"nexu-io/looper"}`
	if err := healthyRepositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "project_1", Name: "Looper", RepoPath: "/tmp/looper", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert(project_1) error = %v", err)
	}

	rt := &webhookRuntime{
		ghPath: "/usr/bin/gh",
		status: WebhookStatus{
			Enabled:     true,
			EndpointURL: "http://127.0.0.1:7777/webhook/forward",
		},
		stopCh:          make(chan struct{}),
		forwarderStopCh: map[string]chan struct{}{},
		now:             time.Now,
	}
	t.Cleanup(rt.Stop)

	rt.Reconcile(failingRepositories)
	status := rt.Status()
	if !status.Degraded {
		t.Fatal("Status().Degraded = false, want true after temporary project list failure")
	}
	if len(status.DegradedReasons) != 1 || !strings.Contains(status.DegradedReasons[0], "list configured projects") {
		t.Fatalf("Status().DegradedReasons = %v, want transient list failure reason", status.DegradedReasons)
	}

	rt.Reconcile(healthyRepositories)
	select {
	case <-startedCh:
	case <-time.After(5 * time.Second):
		t.Fatal("forwarder did not launch after reconcile recovery")
	}
	status = rt.Status()
	if status.Degraded {
		t.Fatalf("Status().Degraded = true, want false after reconcile recovery; reasons=%v", status.DegradedReasons)
	}
	if len(status.DegradedReasons) != 0 {
		t.Fatalf("Status().DegradedReasons = %v, want empty after reconcile recovery", status.DegradedReasons)
	}
	if len(status.Forwarders) != 1 || status.Forwarders[0].Repo != "nexu-io/looper" {
		t.Fatalf("Status().Forwarders = %v, want launched forwarder for nexu-io/looper", status.Forwarders)
	}
}

func TestWebhookRuntimeReconcileLaunchesNewForwarderDespiteExistingForwarderDegradation(t *testing.T) {
	testBin, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() error = %v", err)
	}
	startedCh := make(chan struct{}, 1)
	originalCommand := execCommand
	originalStartedHook := webhookForwarderStartedHook
	execCommand = func(name string, args ...string) *exec.Cmd {
		cmd := exec.Command(testBin, "-test.run=TestWebhookRuntimeForwarderHelperProcess", "--")
		cmd.Env = append(os.Environ(), "GO_WANT_HELPER_PROCESS=1")
		cmd.Args[0] = name
		return cmd
	}
	webhookForwarderStartedHook = func() { startedCh <- struct{}{} }
	t.Cleanup(func() {
		execCommand = originalCommand
		webhookForwarderStartedHook = originalStartedHook
	})

	repositories := openWebhookRuntimeTestRepositories(t)
	nowISO := formatJavaScriptISOString(time.Date(2026, time.May, 16, 12, 0, 0, 0, time.UTC))
	metadataOne := `{"repo":"nexu-io/looper"}`
	metadataTwo := `{"repo":"nexu-io/other"}`
	if err := repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "project_1", Name: "Looper", RepoPath: "/tmp/looper", MetadataJSON: &metadataOne, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert(project_1) error = %v", err)
	}
	if err := repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "project_2", Name: "Other", RepoPath: "/tmp/other", MetadataJSON: &metadataTwo, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert(project_2) error = %v", err)
	}

	rt := &webhookRuntime{
		ghPath: "/usr/bin/gh",
		status: WebhookStatus{
			Enabled:     true,
			EndpointURL: "http://127.0.0.1:7777/webhook/forward",
			Degraded:    true,
			DegradedReasons: []string{
				"forwarder for nexu-io/looper exited: exit status 1",
			},
			Forwarders: []WebhookForwarderState{{
				Repo:    "nexu-io/looper",
				Command: []string{"/usr/bin/gh", "webhook", "forward", "--repo", "nexu-io/looper", "--events", strings.Join(webhookForwardEvents, ","), "--url", "http://127.0.0.1:7777/webhook/forward"},
			}},
		},
		stopCh: make(chan struct{}),
		forwarderStopCh: map[string]chan struct{}{
			"nexu-io/looper": make(chan struct{}),
		},
		now: time.Now,
	}
	t.Cleanup(rt.Stop)

	rt.Reconcile(repositories)

	select {
	case <-startedCh:
	case <-time.After(5 * time.Second):
		t.Fatal("new forwarder did not launch while only existing forwarder degradation remained")
	}

	var status WebhookStatus
	var launched *WebhookForwarderState
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		status = rt.Status()
		for i := range status.Forwarders {
			if status.Forwarders[i].Repo == "nexu-io/other" {
				launched = &status.Forwarders[i]
				break
			}
		}
		if launched != nil && launched.Running {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(status.Forwarders) != 2 {
		t.Fatalf("len(Status().Forwarders) = %d, want 2", len(status.Forwarders))
	}
	if launched == nil {
		t.Fatalf("Status().Forwarders = %v, want launched forwarder for nexu-io/other", status.Forwarders)
	}
	if !launched.Running {
		t.Fatal("Status().Forwarders[nexu-io/other].Running = false, want true after launching new repo forwarder")
	}
	if !status.Degraded {
		t.Fatal("Status().Degraded = false, want true while existing forwarder degradation remains")
	}
	if len(status.DegradedReasons) != 1 || !strings.Contains(status.DegradedReasons[0], "forwarder for nexu-io/looper") {
		t.Fatalf("Status().DegradedReasons = %v, want original forwarder degradation to remain", status.DegradedReasons)
	}
}

func TestWebhookRuntimeReconcileRetriesTransientListFailure(t *testing.T) {
	testBin, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() error = %v", err)
	}
	startedCh := make(chan struct{}, 1)
	originalCommand := execCommand
	originalStartedHook := webhookForwarderStartedHook
	execCommand = func(name string, args ...string) *exec.Cmd {
		cmd := exec.Command(testBin, "-test.run=TestWebhookRuntimeForwarderHelperProcess", "--")
		cmd.Env = append(os.Environ(), "GO_WANT_HELPER_PROCESS=1")
		cmd.Args[0] = name
		return cmd
	}
	webhookForwarderStartedHook = func() { startedCh <- struct{}{} }
	t.Cleanup(func() {
		execCommand = originalCommand
		webhookForwarderStartedHook = originalStartedHook
	})
	originalRetryDelay := webhookReconcileRetryDelay
	webhookReconcileRetryDelay = 10 * time.Millisecond
	t.Cleanup(func() { webhookReconcileRetryDelay = originalRetryDelay })

	dbPath := t.TempDir() + "/runtime.sqlite"
	coordinator, err := storage.OpenSQLiteCoordinator(context.Background(), dbPath, storage.SQLiteCoordinatorOptions{})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	t.Cleanup(func() {
		if err := coordinator.Close(); err != nil {
			t.Fatalf("Coordinator.Close() error = %v", err)
		}
	})
	if _, err := coordinator.MigrationRunner().RunPending(context.Background()); err != nil {
		t.Fatalf("RunPending() error = %v", err)
	}
	repositories := storage.NewRepositories(coordinator.DB())
	nowISO := formatJavaScriptISOString(time.Date(2026, time.May, 16, 12, 0, 0, 0, time.UTC))
	metadata := `{"repo":"nexu-io/looper"}`
	if err := repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "project_1", Name: "Looper", RepoPath: "/tmp/looper", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert(project_1) error = %v", err)
	}

	flaky := &flakyProjectListQuerier{db: coordinator.DB(), failuresRemaining: 1}
	retryRepositories := storage.NewRepositories(flaky)
	rt := &webhookRuntime{
		ghPath:          "/usr/bin/gh",
		status:          WebhookStatus{Enabled: true, EndpointURL: "http://127.0.0.1:7777/webhook/forward"},
		stopCh:          make(chan struct{}),
		forwarderStopCh: map[string]chan struct{}{},
		now:             time.Now,
	}
	t.Cleanup(rt.Stop)

	rt.Start(retryRepositories)

	select {
	case <-startedCh:
	case <-time.After(5 * time.Second):
		t.Fatal("forwarder did not launch after automatic retry")
	}
	status := rt.Status()
	if status.Degraded {
		t.Fatalf("Status().Degraded = true, want false after automatic retry; reasons=%v", status.DegradedReasons)
	}
	if len(status.Forwarders) != 1 || status.Forwarders[0].Repo != "nexu-io/looper" {
		t.Fatalf("Status().Forwarders = %v, want launched forwarder for nexu-io/looper", status.Forwarders)
	}
}

func TestWebhookRuntimeReconcilePrunesForwardersForRemovedRepos(t *testing.T) {
	t.Parallel()

	repositories := openWebhookRuntimeTestRepositories(t)
	nowISO := formatJavaScriptISOString(time.Date(2026, time.May, 16, 12, 0, 0, 0, time.UTC))
	metadata := `{"repo":"nexu-io/other"}`
	if err := repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "project_2", Name: "Other", RepoPath: "/tmp/other", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert(project_2) error = %v", err)
	}

	rt := &webhookRuntime{
		ghPath: "/usr/bin/gh",
		status: WebhookStatus{
			Enabled:     true,
			EndpointURL: "http://127.0.0.1:7777/webhook/forward",
			Forwarders: []WebhookForwarderState{
				{Repo: "nexu-io/looper", Command: []string{"/usr/bin/gh", "webhook", "forward", "--repo", "nexu-io/looper", "--events", strings.Join(webhookForwardEvents, ","), "--url", "http://127.0.0.1:7777/webhook/forward"}},
				{Repo: "nexu-io/other", Command: []string{"/usr/bin/gh", "webhook", "forward", "--repo", "nexu-io/other", "--events", strings.Join(webhookForwardEvents, ","), "--url", "http://127.0.0.1:7777/webhook/forward"}},
			},
			Degraded: true,
			DegradedReasons: []string{
				"forwarder for nexu-io/looper exited: exit status 1",
			},
		},
		stopCh: make(chan struct{}),
		forwarderStopCh: map[string]chan struct{}{
			"nexu-io/looper": make(chan struct{}),
			"nexu-io/other":  make(chan struct{}),
		},
		now: time.Now,
	}
	t.Cleanup(rt.Stop)

	rt.Reconcile(repositories)
	status := rt.Status()
	if len(status.Forwarders) != 1 {
		t.Fatalf("len(Status().Forwarders) = %d, want 1 after prune", len(status.Forwarders))
	}
	if status.Forwarders[0].Repo != "nexu-io/other" {
		t.Fatalf("Status().Forwarders[0].Repo = %q, want nexu-io/other", status.Forwarders[0].Repo)
	}
	if status.Degraded {
		t.Fatalf("Status().Degraded = true, want false after pruning stale repo; reasons=%v", status.DegradedReasons)
	}
}

func openWebhookRuntimeTestRepositories(t *testing.T) *storage.Repositories {
	t.Helper()

	dbPath := t.TempDir() + "/runtime.sqlite"
	coordinator, err := storage.OpenSQLiteCoordinator(context.Background(), dbPath, storage.SQLiteCoordinatorOptions{})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	t.Cleanup(func() {
		if err := coordinator.Close(); err != nil {
			t.Fatalf("Coordinator.Close() error = %v", err)
		}
	})
	if _, err := coordinator.MigrationRunner().RunPending(context.Background()); err != nil {
		t.Fatalf("RunPending() error = %v", err)
	}
	return storage.NewRepositories(coordinator.DB())
}

type flakyProjectListQuerier struct {
	db                *sql.DB
	mu                sync.Mutex
	failuresRemaining int
}

func (q *flakyProjectListQuerier) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return q.db.ExecContext(ctx, query, args...)
}

func (q *flakyProjectListQuerier) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	q.mu.Lock()
	if q.failuresRemaining > 0 && strings.Contains(query, "FROM projects") {
		q.failuresRemaining--
		q.mu.Unlock()
		return nil, context.DeadlineExceeded
	}
	q.mu.Unlock()
	return q.db.QueryContext(ctx, query, args...)
}

func (q *flakyProjectListQuerier) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return q.db.QueryRowContext(ctx, query, args...)
}

func TestWebhookRuntimeForwarderHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	select {}
}
