package runtime

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/projects"
)

// Contract: project add schedules deferred webhook reconcile; Stop must cancel
// that pass (and not hang on wg) even while the GitHub tunnel boundary blocks.
func TestRuntimeStopCancelsBlockedScheduledWebhookReconcile(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	cfg.Webhook.Enabled = true
	cfg.Webhook.Mode = config.WebhookModeTunnel
	cfg.Webhook.ListenPort = 0
	cfg.Webhook.PublicBaseURL = "https://looper.example.test"
	ghPath := "/usr/bin/gh"
	cfg.Tools.GHPath = &ghPath

	rt := New(Options{
		Config:           cfg,
		Logger:           &testLogger{},
		RunSchedulerTick: func(context.Context, Services) error { return nil },
	})
	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	reconcileStarted := make(chan struct{})
	// Intentionally never release: Stop must cancel via life context.
	releaseReconcile := make(chan struct{})
	rt.webhook.bootstrapDone = true
	rt.webhook.tunnelClient = blockingWebhookTunnelGitHubClient{
		started: reconcileStarted,
		release: releaseReconcile,
	}

	projectService := rt.Services().Projects
	projectService.ListWorktrees = nil
	repo := "acme/live"
	if _, err := projectService.AddProject(context.Background(), projects.AddInput{
		ID: "live", Name: "Live", RepoPath: workingDir, Repo: &repo, SnapshotMode: projects.SnapshotModeOff,
	}); err != nil {
		t.Fatalf("AddProject() error = %v", err)
	}

	select {
	case <-reconcileStarted:
	case <-time.After(time.Second):
		t.Fatal("webhook reconciliation did not start")
	}

	stopDone := make(chan struct{})
	go func() {
		rt.Stop("test blocked reconcile stop")
		close(stopDone)
	}()

	select {
	case <-stopDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop() blocked on scheduled webhook reconciliation")
	}
}

// Contract: project mutation → admitted gh-forward launch (past execCommand /
// pipe setup) → Stop must still refuse cmd.Start when shutdown wins the
// start boundary, and must not hang on the in-flight launch.
func TestRuntimeStopRejectsScheduledGHForwardLaunch(t *testing.T) {
	// Mutates package-level execCommand / start gate / started hooks.
	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	cfg.Webhook.Enabled = true
	cfg.Webhook.Mode = config.WebhookModeGHForward
	ghPath := "/usr/bin/gh"
	cfg.Tools.GHPath = &ghPath

	testBin, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() error = %v", err)
	}

	var (
		startGate   = make(chan struct{})
		launchSeen  = make(chan struct{})
		startedHook = make(chan struct{}, 1)
	)
	originalCommand := execCommand
	originalStartedHook := webhookForwarderStartedHook
	originalStartGate := webhookForwarderStartGate
	execCommand = func(name string, args ...string) *exec.Cmd {
		cmd := exec.Command(testBin, "-test.run=TestWebhookRuntimeForwarderHelperProcess", "--")
		cmd.Env = append(os.Environ(), "GO_WANT_HELPER_PROCESS=1")
		cmd.Args[0] = name
		return cmd
	}
	webhookForwarderStartGate = func() {
		select {
		case <-launchSeen:
		default:
			close(launchSeen)
		}
		<-startGate
	}
	webhookForwarderStartedHook = func() {
		select {
		case startedHook <- struct{}{}:
		default:
		}
	}
	t.Cleanup(func() {
		execCommand = originalCommand
		webhookForwarderStartedHook = originalStartedHook
		webhookForwarderStartGate = originalStartGate
		select {
		case <-startGate:
		default:
			close(startGate)
		}
	})

	rt := New(Options{
		Config:           cfg,
		Logger:           &testLogger{},
		RunSchedulerTick: func(context.Context, Services) error { return nil },
	})
	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	rt.webhook.bootstrapDone = true

	projectService := rt.Services().Projects
	projectService.ListWorktrees = nil
	repo := "acme/gh-forward-live"
	if _, err := projectService.AddProject(context.Background(), projects.AddInput{
		ID: "ghfwd", Name: "GH Forward Live", RepoPath: workingDir, Repo: &repo, SnapshotMode: projects.SnapshotModeOff,
	}); err != nil {
		t.Fatalf("AddProject() error = %v", err)
	}

	select {
	case <-launchSeen:
	case <-time.After(2 * time.Second):
		rt.scheduleWebhookForwarderReconcile()
		select {
		case <-launchSeen:
		case <-time.After(2 * time.Second):
			t.Fatal("scheduled gh-forward reconcile did not attempt forwarder launch")
		}
	}

	stopDone := make(chan struct{})
	go func() {
		rt.Stop("test gh-forward launch stop")
		close(stopDone)
	}()

	select {
	case <-rt.webhook.stopCh:
	case <-time.After(3 * time.Second):
		t.Fatal("webhook runtime did not observe Stop before start gate release")
	}
	close(startGate)

	select {
	case <-stopDone:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop() blocked on scheduled gh-forward forwarder launch")
	}

	select {
	case <-startedHook:
		t.Fatal("forwarder process reached started hook after Stop began")
	default:
	}

	status := rt.WebhookStatus()
	for _, fwd := range status.Forwarders {
		if fwd.Running || fwd.PID != nil {
			t.Fatalf("forwarder still running after Stop: %#v", fwd)
		}
	}
}
