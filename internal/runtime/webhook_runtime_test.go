package runtime

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
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
		stopCh: make(chan struct{}),
		now:    time.Now,
	}
	t.Cleanup(rt.Stop)

	rt.launchForwarder(0)
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
		stopCh: make(chan struct{}),
		now:    time.Now,
	}
	rt.launchForwarder(0)
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

func TestWebhookRuntimeForwarderHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	select {}
}
