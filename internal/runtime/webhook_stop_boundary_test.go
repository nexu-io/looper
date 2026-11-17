package runtime

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/storage"
)

// Focused stop/reconcile boundary contracts for the redesigned life-context
// lifecycle. Kept out of the broad webhook_runtime_test.go suite.

func TestWebhookRuntimeEnsureTunnelServerRefusesAfterStop(t *testing.T) {
	t.Parallel()

	_, _, cfg := setupWebhookTunnelTestRepos(t)
	cfg.Webhook.ListenPort = 0
	rt := newWebhookRuntime(cfg, &testLogger{}, time.Now)
	rt.Stop()

	if err := rt.ensureTunnelServer(); !errors.Is(err, errWebhookRuntimeStopped) {
		t.Fatalf("ensureTunnelServer() after Stop error = %v, want %v", err, errWebhookRuntimeStopped)
	}
	if rt.tunnelServer != nil {
		t.Fatal("tunnelServer started after Stop, want nil")
	}
}

func TestWebhookRuntimeLaunchForwarderRefusesAfterStop(t *testing.T) {
	// Mutates package-level execCommand; must not run in parallel.
	var starts atomic.Int32
	originalCommand := execCommand
	execCommand = func(name string, args ...string) *exec.Cmd {
		starts.Add(1)
		return exec.Command("false")
	}
	t.Cleanup(func() { execCommand = originalCommand })

	rt := &webhookRuntime{
		status: WebhookStatus{
			Enabled:    true,
			Forwarders: []WebhookForwarderState{{Repo: "nexu-io/looper", Command: []string{"gh", "webhook", "forward"}}},
		},
		stopCh:          make(chan struct{}),
		forwarderStopCh: map[string]chan struct{}{"nexu-io/looper": make(chan struct{})},
		now:             time.Now,
	}
	rt.Stop()
	rt.launchForwarder("nexu-io/looper")

	time.Sleep(50 * time.Millisecond)
	if got := starts.Load(); got != 0 {
		t.Fatalf("execCommand starts after Stop = %d, want 0", got)
	}
}

func TestWebhookRuntimeStartForwarderCmdRefusesAfterStop(t *testing.T) {
	testBin, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() error = %v", err)
	}

	startGate := make(chan struct{})
	atBoundary := make(chan struct{})
	originalGate := webhookForwarderStartGate
	webhookForwarderStartGate = func() {
		close(atBoundary)
		<-startGate
	}
	t.Cleanup(func() {
		webhookForwarderStartGate = originalGate
		select {
		case <-startGate:
		default:
			close(startGate)
		}
	})

	rt := &webhookRuntime{
		stopCh: make(chan struct{}),
		now:    time.Now,
	}
	cmd := exec.Command(testBin, "-test.run=TestWebhookRuntimeForwarderHelperProcess", "--")
	cmd.Env = append(os.Environ(), "GO_WANT_HELPER_PROCESS=1")

	errCh := make(chan error, 1)
	go func() {
		errCh <- rt.startForwarderCmd(cmd)
	}()

	select {
	case <-atBoundary:
	case <-time.After(2 * time.Second):
		t.Fatal("startForwarderCmd did not reach start gate")
	}

	rt.Stop()
	close(startGate)

	select {
	case err := <-errCh:
		if !errors.Is(err, errWebhookRuntimeStopped) {
			t.Fatalf("startForwarderCmd() error = %v, want errWebhookRuntimeStopped", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("startForwarderCmd did not return after Stop")
	}
	if cmd.Process != nil {
		t.Fatal("cmd.Process started after Stop won start boundary")
	}
}

func TestWebhookRuntimePersistForwarderRecordRefusesAfterStop(t *testing.T) {
	repositories := openWebhookRuntimeTestRepositories(t)
	rt := &webhookRuntime{
		stopCh:         make(chan struct{}),
		now:            time.Now,
		forwarderStore: repositories.WebhookForwarders,
	}
	rt.Stop()
	record := storage.WebhookForwarderRecord{
		Repo: "nexu-io/looper", PID: 1, ProcessStart: 1, Fingerprint: "fp",
		Endpoint: "http://127.0.0.1/webhook/forward", Events: "push", GHPath: "/usr/bin/gh",
		DaemonID: "daemon", SpawnedAt: 1, UpdatedAt: 1,
	}
	if err := rt.persistForwarderRecord(repositories.WebhookForwarders, record); !errors.Is(err, errWebhookRuntimeStopped) {
		t.Fatalf("persistForwarderRecord() after Stop error = %v, want errWebhookRuntimeStopped", err)
	}
	rows, err := repositories.WebhookForwarders.List(context.Background())
	if err != nil {
		t.Fatalf("WebhookForwarders.List() error = %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("persisted %d forwarder rows after Stop, want 0", len(rows))
	}
}

// Contract: persist gate is outside w.mu, so Stop can finish while a slow
// write is in flight; a row completed after Stop is rolled back.
func TestWebhookRuntimeStopUnblockedDuringForwarderPersist(t *testing.T) {
	repositories := openWebhookRuntimeTestRepositories(t)
	persistGate := make(chan struct{})
	atGate := make(chan struct{})
	originalGate := webhookForwarderPersistGate
	webhookForwarderPersistGate = func() {
		close(atGate)
		<-persistGate
	}
	t.Cleanup(func() {
		webhookForwarderPersistGate = originalGate
		select {
		case <-persistGate:
		default:
			close(persistGate)
		}
	})

	rt := &webhookRuntime{
		stopCh:         make(chan struct{}),
		now:            time.Now,
		forwarderStore: repositories.WebhookForwarders,
	}
	record := storage.WebhookForwarderRecord{
		Repo: "nexu-io/looper", PID: 1, ProcessStart: 1, Fingerprint: "fp",
		Endpoint: "http://127.0.0.1/webhook/forward", Events: "push", GHPath: "/usr/bin/gh",
		DaemonID: "daemon", SpawnedAt: 1, UpdatedAt: 1,
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- rt.persistForwarderRecord(repositories.WebhookForwarders, record)
	}()

	select {
	case <-atGate:
	case <-time.After(2 * time.Second):
		t.Fatal("persistForwarderRecord did not reach persist gate")
	}

	stopDone := make(chan struct{})
	go func() {
		rt.Stop()
		close(stopDone)
	}()

	select {
	case <-stopDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop blocked while forwarder persist held only the DB path")
	}

	close(persistGate)

	select {
	case err := <-errCh:
		if !errors.Is(err, errWebhookRuntimeStopped) {
			t.Fatalf("persistForwarderRecord() error = %v, want errWebhookRuntimeStopped after Stop", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("persistForwarderRecord did not return after Stop released gate")
	}

	rows, err := repositories.WebhookForwarders.List(context.Background())
	if err != nil {
		t.Fatalf("WebhookForwarders.List() error = %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("persisted %d forwarder rows after write followed by Stop, want 0", len(rows))
	}
}

func TestWebhookRuntimeReconcileDoesNotLaunchForwarderAfterStop(t *testing.T) {
	// Mutates package-level execCommand; must not run in parallel.
	repositories := openWebhookRuntimeTestRepositories(t)
	nowISO := formatJavaScriptISOString(time.Date(2026, time.May, 16, 12, 0, 0, 0, time.UTC))
	metadata := `{"repo":"nexu-io/looper"}`
	if err := repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "project_1", Name: "Looper", RepoPath: "/tmp/looper", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	var starts atomic.Int32
	originalCommand := execCommand
	execCommand = func(name string, args ...string) *exec.Cmd {
		starts.Add(1)
		return exec.Command("false")
	}
	t.Cleanup(func() { execCommand = originalCommand })

	rt := &webhookRuntime{
		cfg:    webhookRuntimeTestConfig("nexu-io/looper"),
		ghPath: "/usr/bin/gh",
		status: WebhookStatus{
			Enabled:     true,
			EndpointURL: "http://127.0.0.1:7777/webhook/forward",
		},
		stopCh:          make(chan struct{}),
		forwarderStopCh: map[string]chan struct{}{},
		now:             time.Now,
		bootstrapDone:   true,
	}
	rt.Stop()

	if err := rt.Reconcile(repositories); err != nil {
		t.Fatalf("Reconcile() after Stop error = %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if got := starts.Load(); got != 0 {
		t.Fatalf("execCommand starts after stopped Reconcile = %d, want 0", got)
	}
	if status := rt.Status(); len(status.Forwarders) != 0 {
		for _, fwd := range status.Forwarders {
			if fwd.Running || fwd.PID != nil {
				t.Fatalf("forwarder started after Stop: %#v", fwd)
			}
		}
	}
}
