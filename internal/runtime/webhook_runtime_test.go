package runtime

import (
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
