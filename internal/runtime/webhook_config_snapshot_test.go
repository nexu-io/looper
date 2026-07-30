package runtime

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
)

func TestWebhookRuntimeTunnelReconcileUsesCapturedConfigAcrossUpdateConfig(t *testing.T) {
	t.Parallel()

	_, repos, cfg := setupWebhookTunnelTestRepos(t)
	cfg.Webhook.Mode = config.WebhookModeTunnel
	cfg.Webhook.PublicBaseURL = "https://captured.example.test"
	cfg.Projects = []config.ProjectRefConfig{{ID: "first", Repo: "acme/first"}}
	rt := newWebhookRuntime(cfg, &testLogger{}, func() time.Time { return time.Unix(10, 0) })
	rt.ghPath = "/usr/bin/gh"
	rt.bootstrapDone = true
	t.Cleanup(rt.Stop)

	started := make(chan struct{})
	release := make(chan struct{})
	var (
		createdRepo atomic.Value
		createdURL  atomic.Value
	)
	rt.tunnelClient = &callbackTunnelCreateClient{
		onCreate: func(_ context.Context, repo, url string) (webhookTunnelGitHubHook, error) {
			createdRepo.Store(repo)
			createdURL.Store(url)
			close(started)
			<-release
			return webhookTunnelGitHubHook{ID: 11, Active: true, Events: webhookForwardEvents}, nil
		},
	}

	captured := rt.configSnapshot()
	done := make(chan error, 1)
	go func() {
		done <- rt.reconcileSnapshot(context.Background(), repos, captured)
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("tunnel CreateHook did not start")
	}

	// Publish a second project while the first pass is blocked inside tunnel helpers.
	published := captured
	published.Projects = append(append([]config.ProjectRefConfig(nil), captured.Projects...), config.ProjectRefConfig{ID: "second", Repo: "acme/second"})
	for i := 0; i < 100; i++ {
		rt.updateConfig(published)
	}

	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("reconcileSnapshot() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reconcileSnapshot did not finish after release")
	}

	if got, _ := createdRepo.Load().(string); got != "acme/first" {
		t.Fatalf("CreateHook repo = %q, want acme/first from captured snapshot", got)
	}
	wantURL := webhookTunnelManagedURL(captured, "acme/first")
	if got, _ := createdURL.Load().(string); got != wantURL {
		t.Fatalf("CreateHook URL = %q, want captured snapshot URL %q", got, wantURL)
	}
	status := rt.Status()
	if len(status.TunnelHooks) != 1 || status.TunnelHooks[0].Repo != "acme/first" {
		t.Fatalf("status.TunnelHooks = %#v, want only captured repo acme/first", status.TunnelHooks)
	}
}

type callbackTunnelCreateClient struct {
	onCreate func(ctx context.Context, repo, url string) (webhookTunnelGitHubHook, error)
}

func (c *callbackTunnelCreateClient) GetHook(context.Context, string, int64) (webhookTunnelGitHubHook, bool, error) {
	return webhookTunnelGitHubHook{}, false, nil
}

func (c *callbackTunnelCreateClient) CreateHook(ctx context.Context, repo, url, _ string, events []string) (webhookTunnelGitHubHook, error) {
	if c.onCreate != nil {
		return c.onCreate(ctx, repo, url)
	}
	return webhookTunnelGitHubHook{ID: 1, Active: true, Events: events}, nil
}

func (c *callbackTunnelCreateClient) UpdateHook(context.Context, string, int64, string, string, []string, bool) (webhookTunnelGitHubHook, error) {
	return webhookTunnelGitHubHook{}, nil
}

func (c *callbackTunnelCreateClient) DeleteHook(context.Context, string, int64) error {
	return nil
}
