package runtime

import (
	"context"
	"database/sql"
	"errors"
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

func TestWebhookRuntimeLaunchForwarderClearsStaleTailsOnRestart(t *testing.T) {
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
			Enabled: true,
			Forwarders: []WebhookForwarderState{{
				Repo:       "nexu-io/looper",
				Command:    []string{"gh", "webhook", "forward"},
				StdoutTail: []string{"stale stdout"},
				StderrTail: []string{"HTTP 404"},
			}},
		},
		stopCh:          make(chan struct{}),
		forwarderStopCh: map[string]chan struct{}{"nexu-io/looper": make(chan struct{})},
		now:             time.Now,
	}
	t.Cleanup(rt.Stop)

	rt.launchForwarder("nexu-io/looper")
	<-startedCh

	waitForWebhookCondition(t, 5*time.Second, func() bool {
		status := rt.Status()
		return len(status.Forwarders) == 1 && status.Forwarders[0].Running
	})
	status := rt.Status()
	if len(status.Forwarders[0].StdoutTail) != 0 || len(status.Forwarders[0].StderrTail) != 0 {
		t.Fatalf("forwarder tails = stdout:%v stderr:%v, want cleared on restart", status.Forwarders[0].StdoutTail, status.Forwarders[0].StderrTail)
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
	if len(status.DegradedReasons) != 1 || !strings.Contains(status.DegradedReasons[0], "webhook forwarder bootstrap is incomplete") {
		t.Fatalf("Status().DegradedReasons = %v, want transient bootstrap failure reason", status.DegradedReasons)
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

func TestWebhookRuntimeTerminalForwarderExitLatchesWithoutRespawn(t *testing.T) {
	testBin, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() error = %v", err)
	}
	var mu sync.Mutex
	starts := 0
	originalCommand := execCommand
	execCommand = func(name string, args ...string) *exec.Cmd {
		mu.Lock()
		starts++
		mu.Unlock()
		cmd := exec.Command(testBin, "-test.run=TestWebhookRuntimeForwarderHelperProcess", "--")
		cmd.Env = append(os.Environ(), "GO_WANT_HELPER_PROCESS=1", "LOOPER_HELPER_STDERR_EXIT=Hook already exists on this repository")
		cmd.Args[0] = name
		return cmd
	}
	t.Cleanup(func() { execCommand = originalCommand })

	rt := &webhookRuntime{
		ghPath:          "/usr/bin/gh",
		status:          WebhookStatus{Enabled: true, EndpointURL: "http://127.0.0.1:7777/webhook/forward", FallbackPollIntervalSeconds: 300},
		stopCh:          make(chan struct{}),
		forwarderStopCh: map[string]chan struct{}{},
		now:             time.Now,
	}
	t.Cleanup(rt.Stop)
	rt.Reconcile(openWebhookRuntimeTestRepositoriesWithProject(t, "nexu-io/looper"))

	waitForWebhookCondition(t, 5*time.Second, func() bool {
		status := rt.Status()
		return len(status.Forwarders) == 1 && status.Forwarders[0].Latched
	})
	status := rt.Status()
	if !status.Degraded || len(status.DegradedReasons) == 0 || !strings.Contains(status.DegradedReasons[0], "polling fallback") {
		t.Fatalf("Status().DegradedReasons = %v, want latched polling fallback reason", status.DegradedReasons)
	}
	if status.Forwarders[0].RestartCount != 1 || status.Forwarders[0].LatchReason == nil || !strings.Contains(*status.Forwarders[0].LatchReason, "Hook already exists") {
		t.Fatalf("forwarder status = %#v, want terminal latch with one observed exit", status.Forwarders[0])
	}
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if starts != 1 {
		t.Fatalf("starts = %d, want no respawn after terminal latch", starts)
	}
}

func TestWebhookRuntimeBootstrapAdoptsMatchingForwarderRecord(t *testing.T) {
	repositories := openWebhookRuntimeTestRepositoriesWithProject(t, "nexu-io/looper")
	ghPath := "/usr/bin/gh"
	endpoint := "http://127.0.0.1:7777/webhook/forward"
	fingerprint, events := commandFingerprint(ghPath, "nexu-io/looper", webhookForwardEvents, endpoint)
	record := storage.WebhookForwarderRecord{Repo: "nexu-io/looper", PID: 4242, ProcessStart: 99, Fingerprint: fingerprint, Endpoint: endpoint, Events: events, GHPath: ghPath, DaemonID: "old", SpawnedAt: time.Date(2026, time.May, 17, 12, 0, 0, 0, time.UTC).UnixNano(), UpdatedAt: 1}
	if err := repositories.WebhookForwarders.Upsert(context.Background(), record); err != nil {
		t.Fatalf("WebhookForwarders.Upsert() error = %v", err)
	}

	probe := &testProcessProbe{alive: true, start: 99, exe: ghPath, argv: []string{ghPath, "webhook", "forward", "--repo", "nexu-io/looper", "--events", events, "--url", endpoint}}
	rt := &webhookRuntime{
		ghPath:          ghPath,
		status:          WebhookStatus{Enabled: true, EndpointURL: endpoint, FallbackPollIntervalSeconds: 300},
		stopCh:          make(chan struct{}),
		forwarderStopCh: map[string]chan struct{}{},
		probe:           probe,
		now:             time.Now,
	}
	t.Cleanup(func() { probe.alive = false; rt.Stop() })

	rt.Start(repositories)
	status := rt.Status()
	if len(status.Forwarders) != 1 || !status.Forwarders[0].Adopted || !status.Forwarders[0].Running || status.Forwarders[0].PID == nil || *status.Forwarders[0].PID != 4242 {
		t.Fatalf("Status().Forwarders = %#v, want adopted running forwarder", status.Forwarders)
	}
	if status.Forwarders[0].Fingerprint != fingerprint {
		t.Fatalf("Fingerprint = %q, want %q", status.Forwarders[0].Fingerprint, fingerprint)
	}
}

func TestWebhookRuntimeBootstrapRejectsStaleFingerprint(t *testing.T) {
	repositories := openWebhookRuntimeTestRepositoriesWithProject(t, "nexu-io/looper")
	ghPath := "/usr/bin/gh"
	endpoint := "http://127.0.0.1:7777/webhook/forward"
	_, events := commandFingerprint(ghPath, "nexu-io/looper", webhookForwardEvents, endpoint)
	record := storage.WebhookForwarderRecord{Repo: "nexu-io/looper", PID: 4242, ProcessStart: 99, Fingerprint: "stale", Endpoint: endpoint, Events: events, GHPath: ghPath, DaemonID: "old", SpawnedAt: 1, UpdatedAt: 1}
	if err := repositories.WebhookForwarders.Upsert(context.Background(), record); err != nil {
		t.Fatalf("WebhookForwarders.Upsert() error = %v", err)
	}
	originalFindProcess := osFindProcess
	osFindProcess = func(pid int) (*os.Process, error) { return nil, os.ErrNotExist }
	t.Cleanup(func() { osFindProcess = originalFindProcess })
	rt := &webhookRuntime{
		ghPath:          ghPath,
		status:          WebhookStatus{Enabled: true, EndpointURL: endpoint, FallbackPollIntervalSeconds: 300},
		stopCh:          make(chan struct{}),
		forwarderStopCh: map[string]chan struct{}{},
		probe:           &testProcessProbe{alive: true, start: 99, exe: ghPath, argv: []string{ghPath, "webhook", "forward", "--repo", "nexu-io/looper", "--events", events, "--url", endpoint}},
		now:             time.Now,
	}
	rt.Bootstrap(context.Background(), repositories)
	status := rt.Status()
	if len(status.Forwarders) != 0 {
		t.Fatalf("Status().Forwarders = %#v, want stale record rejected", status.Forwarders)
	}
}

func TestWebhookRuntimeBootstrapAdoptsDesiredForwarderWhenGHPathUnavailable(t *testing.T) {
	repositories := openWebhookRuntimeTestRepositoriesWithProject(t, "nexu-io/looper")
	ghPath := "/usr/bin/gh"
	endpoint := "http://127.0.0.1:7777/webhook/forward"
	fingerprint, events := commandFingerprint(ghPath, "nexu-io/looper", webhookForwardEvents, endpoint)
	record := storage.WebhookForwarderRecord{Repo: "nexu-io/looper", PID: 4242, ProcessStart: 99, Fingerprint: fingerprint, Endpoint: endpoint, Events: events, GHPath: ghPath, DaemonID: "old", SpawnedAt: 1, UpdatedAt: 1}
	if err := repositories.WebhookForwarders.Upsert(context.Background(), record); err != nil {
		t.Fatalf("WebhookForwarders.Upsert() error = %v", err)
	}
	probe := &testProcessProbe{alive: true, start: 99, exe: ghPath, argv: []string{ghPath, "webhook", "forward", "--repo", "nexu-io/looper", "--events", events, "--url", endpoint}}
	rt := &webhookRuntime{
		status:          WebhookStatus{Enabled: true, EndpointURL: endpoint, FallbackPollIntervalSeconds: 300},
		stopCh:          make(chan struct{}),
		forwarderStopCh: map[string]chan struct{}{},
		probe:           probe,
		now:             time.Now,
	}
	t.Cleanup(func() { probe.alive = false; rt.Stop() })

	rt.Bootstrap(context.Background(), repositories)
	status := rt.Status()
	if len(status.Forwarders) != 1 || !status.Forwarders[0].Adopted || !status.Forwarders[0].Running {
		t.Fatalf("Status().Forwarders = %#v, want adopted running forwarder when gh path is unavailable", status.Forwarders)
	}
	if status.Forwarders[0].PID == nil || *status.Forwarders[0].PID != 4242 {
		t.Fatalf("Status().Forwarders = %#v, want adopted pid 4242", status.Forwarders)
	}
	if status.Forwarders[0].Fingerprint != fingerprint {
		t.Fatalf("Fingerprint = %q, want %q", status.Forwarders[0].Fingerprint, fingerprint)
	}
	if !rt.bootstrapCompleted() {
		t.Fatal("bootstrapCompleted() = false, want true")
	}
	stored, err := repositories.WebhookForwarders.List(context.Background())
	if err != nil {
		t.Fatalf("WebhookForwarders.List() error = %v", err)
	}
	if len(stored) != 1 || stored[0].Repo != record.Repo {
		t.Fatalf("WebhookForwarders.List() = %#v, want retained adopted record", stored)
	}
}

func TestWebhookRuntimeAdoptedForwarderExitDoesNotRespawnWhenLaunchBlocked(t *testing.T) {
	startedCh := make(chan struct{}, 1)
	originalCommand := execCommand
	originalStartedHook := webhookForwarderStartedHook
	execCommand = func(name string, args ...string) *exec.Cmd {
		startedCh <- struct{}{}
		return exec.Command("/usr/bin/false")
	}
	webhookForwarderStartedHook = func() { startedCh <- struct{}{} }
	t.Cleanup(func() {
		execCommand = originalCommand
		webhookForwarderStartedHook = originalStartedHook
	})

	ghPath := "/usr/bin/gh"
	endpoint := "http://127.0.0.1:7777/webhook/forward"
	fingerprint, events := commandFingerprint(ghPath, "nexu-io/looper", webhookForwardEvents, endpoint)
	rt := &webhookRuntime{
		ghPath: ghPath,
		status: WebhookStatus{
			Enabled:         true,
			EndpointURL:     endpoint,
			Degraded:        true,
			DegradedReasons: []string{"server.host is not loopback; webhook forwarders require a loopback daemon endpoint"},
		},
		stopCh:          make(chan struct{}),
		forwarderStopCh: map[string]chan struct{}{},
		probe:           &testProcessProbe{alive: false},
		now:             time.Now,
	}
	t.Cleanup(rt.Stop)

	rt.adoptForwarder(storage.WebhookForwarderRecord{Repo: "nexu-io/looper", PID: 4242, ProcessStart: 99, Fingerprint: fingerprint, Endpoint: endpoint, Events: events, GHPath: ghPath}, []string{ghPath, "webhook", "forward", "--repo", "nexu-io/looper", "--events", events, "--url", endpoint})

	waitForWebhookCondition(t, 3*time.Second, func() bool {
		status := rt.Status()
		return len(status.Forwarders) == 0
	})

	select {
	case <-startedCh:
		t.Fatal("forwarder respawn started, want launch to remain blocked")
	case <-time.After(100 * time.Millisecond):
	}
	status := rt.Status()
	if len(status.Forwarders) != 0 {
		t.Fatalf("Status().Forwarders = %#v, want no replacement launch while launch is blocked", status.Forwarders)
	}
	if !status.Degraded || len(status.DegradedReasons) != 2 || !strings.Contains(status.DegradedReasons[1], "forwarder for nexu-io/looper exited:") {
		t.Fatalf("Status() = %#v, want preserved launch blocker plus exit degradation", status)
	}
}

func TestWebhookRuntimeStartDoesNotLaunchReplacementWhenBootstrapProbeIsInconclusive(t *testing.T) {
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

	repositories := openWebhookRuntimeTestRepositoriesWithProject(t, "nexu-io/looper")
	ghPath := "/usr/bin/gh"
	endpoint := "http://127.0.0.1:7777/webhook/forward"
	fingerprint, events := commandFingerprint(ghPath, "nexu-io/looper", webhookForwardEvents, endpoint)
	record := storage.WebhookForwarderRecord{Repo: "nexu-io/looper", PID: 4242, ProcessStart: 99, Fingerprint: fingerprint, Endpoint: endpoint, Events: events, GHPath: ghPath, DaemonID: "old", SpawnedAt: 1, UpdatedAt: 1}
	if err := repositories.WebhookForwarders.Upsert(context.Background(), record); err != nil {
		t.Fatalf("WebhookForwarders.Upsert() error = %v", err)
	}

	rt := &webhookRuntime{
		ghPath:          ghPath,
		status:          WebhookStatus{Enabled: true, EndpointURL: endpoint, FallbackPollIntervalSeconds: 300},
		stopCh:          make(chan struct{}),
		forwarderStopCh: map[string]chan struct{}{},
		probe:           &testProcessProbe{alive: true, startErr: errors.New("probe failed")},
		now:             time.Now,
	}
	t.Cleanup(rt.Stop)

	rt.Start(repositories)
	time.Sleep(50 * time.Millisecond)

	select {
	case <-startedCh:
		t.Fatal("forwarder launch started, want bootstrap retry without replacement launch")
	default:
	}
	if rt.bootstrapCompleted() {
		t.Fatal("bootstrapCompleted() = true, want false after inconclusive adoption probe")
	}
	status := rt.Status()
	if len(status.Forwarders) != 0 {
		t.Fatalf("Status().Forwarders = %#v, want no replacement launch during inconclusive bootstrap", status.Forwarders)
	}
	if !status.Degraded || len(status.DegradedReasons) != 1 || !strings.Contains(status.DegradedReasons[0], "webhook forwarder bootstrap is incomplete: adoption probe for nexu-io/looper failed") {
		t.Fatalf("Status() = %#v, want transient bootstrap degradation for inconclusive adoption probe", status)
	}
	stored, err := repositories.WebhookForwarders.List(context.Background())
	if err != nil {
		t.Fatalf("WebhookForwarders.List() error = %v", err)
	}
	if len(stored) != 1 || stored[0].Repo != record.Repo {
		t.Fatalf("WebhookForwarders.List() = %#v, want retained record during inconclusive bootstrap", stored)
	}
}

func TestWebhookRuntimeBootstrapRetryDoesNotDuplicateAdoptedForwarders(t *testing.T) {
	repositories := openWebhookRuntimeTestRepositories(t)
	nowISO := formatJavaScriptISOString(time.Date(2026, time.May, 16, 12, 0, 0, 0, time.UTC))
	metadata1 := `{"repo":"nexu-io/looper"}`
	metadata2 := `{"repo":"nexu-io/other"}`
	if err := repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "project_1", Name: "Looper", RepoPath: "/tmp/looper", MetadataJSON: &metadata1, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert(project_1) error = %v", err)
	}
	if err := repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "project_2", Name: "Other", RepoPath: "/tmp/other", MetadataJSON: &metadata2, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert(project_2) error = %v", err)
	}

	ghPath := "/usr/bin/gh"
	endpoint := "http://127.0.0.1:7777/webhook/forward"
	fingerprint1, events1 := commandFingerprint(ghPath, "nexu-io/looper", webhookForwardEvents, endpoint)
	fingerprint2, events2 := commandFingerprint(ghPath, "nexu-io/other", webhookForwardEvents, endpoint)
	record1 := storage.WebhookForwarderRecord{Repo: "nexu-io/looper", PID: 4242, ProcessStart: 99, Fingerprint: fingerprint1, Endpoint: endpoint, Events: events1, GHPath: ghPath, DaemonID: "old", SpawnedAt: 1, UpdatedAt: 1}
	record2 := storage.WebhookForwarderRecord{Repo: "nexu-io/other", PID: 4343, ProcessStart: 101, Fingerprint: fingerprint2, Endpoint: endpoint, Events: events2, GHPath: ghPath, DaemonID: "old", SpawnedAt: 1, UpdatedAt: 1}
	if err := repositories.WebhookForwarders.Upsert(context.Background(), record1); err != nil {
		t.Fatalf("WebhookForwarders.Upsert(record1) error = %v", err)
	}
	if err := repositories.WebhookForwarders.Upsert(context.Background(), record2); err != nil {
		t.Fatalf("WebhookForwarders.Upsert(record2) error = %v", err)
	}

	probe := &multiProcessProbe{probes: map[int]testProcessProbe{
		4242: {alive: true, start: 99, exe: ghPath, argv: []string{ghPath, "webhook", "forward", "--repo", "nexu-io/looper", "--events", events1, "--url", endpoint}},
		4343: {alive: true, startErr: errors.New("probe failed")},
	}}
	rt := &webhookRuntime{
		ghPath:          ghPath,
		status:          WebhookStatus{Enabled: true, EndpointURL: endpoint, FallbackPollIntervalSeconds: 300},
		stopCh:          make(chan struct{}),
		forwarderStopCh: map[string]chan struct{}{},
		probe:           probe,
		now:             time.Now,
	}
	t.Cleanup(func() { probe.probes[4242] = testProcessProbe{}; rt.Stop() })

	rt.Bootstrap(context.Background(), repositories)
	rt.Bootstrap(context.Background(), repositories)

	status := rt.Status()
	if len(status.Forwarders) != 1 {
		t.Fatalf("Status().Forwarders = %#v, want exactly one adopted forwarder after retry", status.Forwarders)
	}
	if !status.Forwarders[0].Adopted || status.Forwarders[0].Repo != record1.Repo {
		t.Fatalf("Status().Forwarders = %#v, want single adopted forwarder for %q", status.Forwarders, record1.Repo)
	}
	if !status.Degraded || len(status.DegradedReasons) != 1 || !strings.Contains(status.DegradedReasons[0], "webhook forwarder bootstrap is incomplete: adoption probe for nexu-io/other failed") {
		t.Fatalf("Status() = %#v, want transient bootstrap degradation for inconclusive record", status)
	}
	if rt.bootstrapCompleted() {
		t.Fatal("bootstrapCompleted() = true, want false while probe error remains inconclusive")
	}
}

func TestWebhookRuntimeCleanupForwarderRecordRetainsUnverifiableRows(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repositories := openWebhookRuntimeTestRepositories(t)
	record := storage.WebhookForwarderRecord{
		Repo:         "nexu-io/looper",
		PID:          4242,
		ProcessStart: 99,
		Fingerprint:  "fingerprint",
		Endpoint:     "http://127.0.0.1:7777/webhook/forward",
		Events:       strings.Join(webhookForwardEvents, ","),
		GHPath:       "/usr/bin/gh",
		DaemonID:     "daemon",
		SpawnedAt:    1,
		UpdatedAt:    1,
	}

	t.Run("probe error keeps row", func(t *testing.T) {
		if err := repositories.WebhookForwarders.Upsert(ctx, record); err != nil {
			t.Fatalf("WebhookForwarders.Upsert() error = %v", err)
		}

		rt := &webhookRuntime{
			forwarderStore: repositories.WebhookForwarders,
			probe:          &testProcessProbe{aliveErr: errors.New("probe failed")},
		}
		rt.cleanupForwarderRecord(ctx, record, "adoption_rejected")

		records, err := repositories.WebhookForwarders.List(ctx)
		if err != nil {
			t.Fatalf("WebhookForwarders.List() error = %v", err)
		}
		if len(records) != 1 || records[0].Repo != record.Repo {
			t.Fatalf("WebhookForwarders.List() = %#v, want retained record for %q", records, record.Repo)
		}
	})

	t.Run("dead pid deletes row", func(t *testing.T) {
		if err := repositories.WebhookForwarders.Upsert(ctx, record); err != nil {
			t.Fatalf("WebhookForwarders.Upsert() error = %v", err)
		}

		rt := &webhookRuntime{
			forwarderStore: repositories.WebhookForwarders,
			probe:          &testProcessProbe{alive: false},
		}
		rt.cleanupForwarderRecord(ctx, record, "adoption_rejected")

		records, err := repositories.WebhookForwarders.List(ctx)
		if err != nil {
			t.Fatalf("WebhookForwarders.List() error = %v", err)
		}
		if len(records) != 0 {
			t.Fatalf("WebhookForwarders.List() = %#v, want row deleted after definitive mismatch", records)
		}
	})
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

func openWebhookRuntimeTestRepositoriesWithProject(t *testing.T, repo string) *storage.Repositories {
	t.Helper()
	repositories := openWebhookRuntimeTestRepositories(t)
	nowISO := formatJavaScriptISOString(time.Date(2026, time.May, 16, 12, 0, 0, 0, time.UTC))
	metadata := `{"repo":"` + repo + `"}`
	if err := repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "project_1", Name: "Project", RepoPath: "/tmp/project", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	return repositories
}

type flakyProjectListQuerier struct {
	db                *sql.DB
	mu                sync.Mutex
	failuresRemaining int
}

type testProcessProbe struct {
	alive    bool
	aliveErr error
	start    int64
	startErr error
	exe      string
	exeErr   error
	argv     []string
	argvErr  error
}

type multiProcessProbe struct {
	probes map[int]testProcessProbe
}

func (p *multiProcessProbe) IsAlive(pid int) (bool, error) {
	probe := p.probes[pid]
	return probe.alive, probe.aliveErr
}
func (p *multiProcessProbe) StartTime(pid int) (int64, error) {
	probe := p.probes[pid]
	return probe.start, probe.startErr
}
func (p *multiProcessProbe) ExecutablePath(pid int) (string, error) {
	probe := p.probes[pid]
	return probe.exe, probe.exeErr
}
func (p *multiProcessProbe) Argv(pid int) ([]string, error) {
	probe := p.probes[pid]
	return append([]string{}, probe.argv...), probe.argvErr
}

func (p *testProcessProbe) IsAlive(pid int) (bool, error)    { return p.alive, p.aliveErr }
func (p *testProcessProbe) StartTime(pid int) (int64, error) { return p.start, p.startErr }
func (p *testProcessProbe) Argv(pid int) ([]string, error) {
	return append([]string{}, p.argv...), p.argvErr
}
func (p *testProcessProbe) ExecutablePath(pid int) (string, error) { return p.exe, p.exeErr }

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
	if message := os.Getenv("LOOPER_HELPER_STDERR_EXIT"); message != "" {
		_, _ = os.Stderr.WriteString(message + "\n")
		os.Exit(1)
	}
	select {}
}
