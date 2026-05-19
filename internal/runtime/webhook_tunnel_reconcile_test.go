package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
)

func TestReconcileTunnelHookMissingSecretDegradesWithoutMutation(t *testing.T) {
	t.Parallel()

	ctx, repos, cfg := setupWebhookTunnelTestRepos(t)
	record := storage.WebhookTunnelHookRecord{Repo: "acme/looper", HookID: 42, ManagedURL: webhookTunnelManagedURL(cfg, "acme/looper"), SecretRef: webhookTunnelSecretRef("acme/looper"), CreatedAt: 1, UpdatedAt: 1}
	if err := repos.WebhookTunnelHooks.Upsert(ctx, record); err != nil {
		t.Fatalf("WebhookTunnelHooks.Upsert() error = %v", err)
	}
	client := &fakeWebhookTunnelGitHubClient{}
	rt := newWebhookRuntime(cfg, &testLogger{}, func() time.Time { return time.Unix(10, 0) })
	rt.tunnelClient = client

	state := rt.reconcileTunnelHook(ctx, repos.WebhookTunnelHooks, record.Repo, record, true, time.Now().UnixNano())

	if state.LastError == "" || !strings.Contains(state.LastError, "read webhook secret") {
		t.Fatalf("state.LastError = %q, want read webhook secret failure", state.LastError)
	}
	if client.createCalls != 0 || client.updateCalls != 0 {
		t.Fatalf("client calls = create:%d update:%d, want no mutation", client.createCalls, client.updateCalls)
	}
	if _, err := os.Stat(webhookTunnelSecretPath(cfg.Storage.DBPath, record.SecretRef)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("secret path stat error = %v, want not exists", err)
	}
}

func TestReconcileTunnelHookEmptySecretDegradesWithoutMutation(t *testing.T) {
	t.Parallel()

	ctx, repos, cfg := setupWebhookTunnelTestRepos(t)
	record := storage.WebhookTunnelHookRecord{Repo: "acme/looper", HookID: 42, ManagedURL: webhookTunnelManagedURL(cfg, "acme/looper"), SecretRef: webhookTunnelSecretRef("acme/looper"), CreatedAt: 1, UpdatedAt: 1}
	if err := repos.WebhookTunnelHooks.Upsert(ctx, record); err != nil {
		t.Fatalf("WebhookTunnelHooks.Upsert() error = %v", err)
	}
	secretPath := webhookTunnelSecretPath(cfg.Storage.DBPath, record.SecretRef)
	if err := os.MkdirAll(filepath.Dir(secretPath), 0o700); err != nil {
		t.Fatalf("os.MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(secretPath, []byte(" \n\t "), 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}
	client := &fakeWebhookTunnelGitHubClient{}
	rt := newWebhookRuntime(cfg, &testLogger{}, func() time.Time { return time.Unix(10, 0) })
	rt.tunnelClient = client

	state := rt.reconcileTunnelHook(ctx, repos.WebhookTunnelHooks, record.Repo, record, true, time.Now().UnixNano())

	if state.LastError == "" || !strings.Contains(state.LastError, "read webhook secret") || !strings.Contains(state.LastError, "is empty") {
		t.Fatalf("state.LastError = %q, want empty-secret read failure", state.LastError)
	}
	if client.createCalls != 0 || client.updateCalls != 0 {
		t.Fatalf("client calls = create:%d update:%d, want no mutation", client.createCalls, client.updateCalls)
	}
}

func TestReconcileTunnelHooksMarksExistingRecordsOrphanedWhenRepoSetBecomesEmpty(t *testing.T) {
	t.Parallel()

	ctx, repos, cfg := setupWebhookTunnelTestRepos(t)
	record := storage.WebhookTunnelHookRecord{Repo: "acme/looper", HookID: 42, ManagedURL: webhookTunnelManagedURL(cfg, "acme/looper"), SecretRef: webhookTunnelSecretRef("acme/looper"), CreatedAt: 1, UpdatedAt: 1}
	if err := repos.WebhookTunnelHooks.Upsert(ctx, record); err != nil {
		t.Fatalf("WebhookTunnelHooks.Upsert() error = %v", err)
	}
	rt := newWebhookRuntime(cfg, &testLogger{}, func() time.Time { return time.Unix(10, 0) })

	rt.reconcileTunnelHooks(ctx, repos, map[string]struct{}{})

	updated, ok, err := repos.WebhookTunnelHooks.Get(ctx, record.Repo)
	if err != nil {
		t.Fatalf("WebhookTunnelHooks.Get() error = %v", err)
	}
	if !ok || !updated.Orphaned {
		t.Fatalf("updated record = %#v, want orphaned", updated)
	}
	status := rt.Status()
	if len(status.TunnelHooks) != 1 || !status.TunnelHooks[0].Orphaned {
		t.Fatalf("status.TunnelHooks = %#v, want orphaned state", status.TunnelHooks)
	}
}

func TestReconcileTunnelHooksReactivatesOrphanedRecordWhenRepoIsReadded(t *testing.T) {
	t.Parallel()

	ctx, repos, cfg := setupWebhookTunnelTestRepos(t)
	record := storage.WebhookTunnelHookRecord{Repo: "acme/looper", HookID: 42, ManagedURL: webhookTunnelManagedURL(cfg, "acme/looper"), SecretRef: webhookTunnelSecretRef("acme/looper"), Orphaned: true, CreatedAt: 1, UpdatedAt: 1}
	if err := repos.WebhookTunnelHooks.Upsert(ctx, record); err != nil {
		t.Fatalf("WebhookTunnelHooks.Upsert() error = %v", err)
	}
	secretPath := webhookTunnelSecretPath(cfg.Storage.DBPath, record.SecretRef)
	if err := os.MkdirAll(filepath.Dir(secretPath), 0o700); err != nil {
		t.Fatalf("os.MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(secretPath, []byte("top-secret"), 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}
	hook := webhookTunnelGitHubHook{ID: 42, Active: true, Events: webhookForwardEvents}
	hook.Config.URL = record.ManagedURL
	hook.Config.ContentType = "json"
	hook.Config.InsecureSSL = "0"
	client := &fakeWebhookTunnelGitHubClient{getHook: hook, getFound: true}
	rt := newWebhookRuntime(cfg, &testLogger{}, func() time.Time { return time.Unix(10, 0) })
	rt.ghPath = "/usr/bin/gh"
	rt.tunnelClient = client

	rt.reconcileTunnelHooks(ctx, repos, map[string]struct{}{"acme/looper": {}})
	defer rt.stopTunnelServer()

	updated, ok, err := repos.WebhookTunnelHooks.Get(ctx, record.Repo)
	if err != nil {
		t.Fatalf("WebhookTunnelHooks.Get() error = %v", err)
	}
	if !ok || updated.Orphaned {
		t.Fatalf("updated record = %#v, want active non-orphaned", updated)
	}
	status := rt.Status()
	if len(status.TunnelHooks) != 1 || status.TunnelHooks[0].Orphaned {
		t.Fatalf("status.TunnelHooks = %#v, want one active non-orphaned state", status.TunnelHooks)
	}
	if client.updateCalls != 0 {
		t.Fatalf("UpdateHook calls = %d, want no patch for matching hook", client.updateCalls)
	}
}

func TestReconcileTunnelHooksDisabledHookAtThresholdLatchesAndDegrades(t *testing.T) {
	t.Parallel()

	ctx, repos, cfg := setupWebhookTunnelTestRepos(t)
	lastDisableAt := time.Unix(9, 0).UnixNano()
	record := storage.WebhookTunnelHookRecord{Repo: "acme/looper", HookID: 42, ManagedURL: webhookTunnelManagedURL(cfg, "acme/looper"), SecretRef: webhookTunnelSecretRef("acme/looper"), ConsecutiveDisables: webhookTunnelDisableLatchThreshold - 1, LastDisableAt: &lastDisableAt, CreatedAt: 1, UpdatedAt: 1}
	if err := repos.WebhookTunnelHooks.Upsert(ctx, record); err != nil {
		t.Fatalf("WebhookTunnelHooks.Upsert() error = %v", err)
	}
	secretPath := webhookTunnelSecretPath(cfg.Storage.DBPath, record.SecretRef)
	if err := os.MkdirAll(filepath.Dir(secretPath), 0o700); err != nil {
		t.Fatalf("os.MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(secretPath, []byte("top-secret"), 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}
	client := &fakeWebhookTunnelGitHubClient{getHook: webhookTunnelGitHubHook{ID: 42, Active: false, Events: webhookForwardEvents}, getFound: true}
	rt := newWebhookRuntime(cfg, &testLogger{}, func() time.Time { return time.Unix(10, 0) })
	rt.ghPath = "/usr/bin/gh"
	rt.tunnelClient = client

	rt.reconcileTunnelHooks(ctx, repos, map[string]struct{}{"acme/looper": {}})
	defer rt.stopTunnelServer()

	status := rt.Status()
	if len(status.TunnelHooks) != 1 || !status.TunnelHooks[0].Latched {
		t.Fatalf("status.TunnelHooks = %#v, want one latched state", status.TunnelHooks)
	}
	if !status.Degraded || len(status.DegradedReasons) == 0 || !strings.Contains(status.DegradedReasons[0], "remote hook disabled repeatedly; not re-enabling") {
		t.Fatalf("status degraded = %v reasons=%v, want latched degraded reason", status.Degraded, status.DegradedReasons)
	}
	if client.updateCalls != 0 {
		t.Fatalf("UpdateHook calls = %d, want 0 once latched", client.updateCalls)
	}
}

func TestReconcileTunnelHooksDisabledHookAtThresholdReportsPersistFailure(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	cfg, err := config.DefaultConfig(tempDir)
	if err != nil {
		t.Fatalf("config.DefaultConfig() error = %v", err)
	}
	cfg.Storage.DBPath = filepath.Join(tempDir, "state", "looper.sqlite")
	cfg.Webhook.PublicBaseURL = "https://example.test"
	cfg.Webhook.ListenPort = 0
	ghPath := "/usr/bin/gh"
	cfg.Tools.GHPath = &ghPath
	coordinator := openMigratedCoordinator(t, cfg.Storage.DBPath, filepath.Join(tempDir, "backups"))
	repos := storage.NewRepositories(coordinator.DB())
	ctx := context.Background()
	lastDisableAt := time.Unix(9, 0).UnixNano()
	record := storage.WebhookTunnelHookRecord{Repo: "acme/looper", HookID: 42, ManagedURL: webhookTunnelManagedURL(cfg, "acme/looper"), SecretRef: webhookTunnelSecretRef("acme/looper"), ConsecutiveDisables: webhookTunnelDisableLatchThreshold - 1, LastDisableAt: &lastDisableAt, CreatedAt: 1, UpdatedAt: 1}
	if err := repos.WebhookTunnelHooks.Upsert(ctx, record); err != nil {
		t.Fatalf("WebhookTunnelHooks.Upsert() error = %v", err)
	}
	secretPath := webhookTunnelSecretPath(cfg.Storage.DBPath, record.SecretRef)
	if err := os.MkdirAll(filepath.Dir(secretPath), 0o700); err != nil {
		t.Fatalf("os.MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(secretPath, []byte("top-secret"), 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}
	if err := coordinator.Close(); err != nil {
		t.Fatalf("coordinator.Close() error = %v", err)
	}
	client := &fakeWebhookTunnelGitHubClient{getHook: webhookTunnelGitHubHook{ID: 42, Active: false, Events: webhookForwardEvents}, getFound: true}
	rt := newWebhookRuntime(cfg, &testLogger{}, func() time.Time { return time.Unix(10, 0) })
	rt.ghPath = "/usr/bin/gh"
	rt.tunnelClient = client

	state := rt.reconcileTunnelHook(ctx, repos.WebhookTunnelHooks, record.Repo, record, true, time.Unix(10, 0).UnixNano())

	if !strings.Contains(state.LastError, "persist latch state") {
		t.Fatalf("state.LastError = %q, want persist latch state failure", state.LastError)
	}
	if state.Latched {
		t.Fatalf("state.Latched = %v, want false when latch persistence fails", state.Latched)
	}
	if client.updateCalls != 0 {
		t.Fatalf("UpdateHook calls = %d, want 0 when latch persistence fails", client.updateCalls)
	}
}

func TestWebhookRuntimeReconcileConflictedRepoMarksTunnelHookOrphaned(t *testing.T) {
	t.Parallel()

	ctx, repos, cfg := setupWebhookTunnelTestRepos(t)
	nowISO := formatJavaScriptISOString(time.Date(2026, time.May, 16, 12, 0, 0, 0, time.UTC))
	metadata := `{"repo":"acme/looper"}`
	if err := repos.Projects.Upsert(ctx, storage.ProjectRecord{ID: "tunnel", Name: "Tunnel", RepoPath: "/tmp/tunnel", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert(tunnel) error = %v", err)
	}
	if err := repos.Projects.Upsert(ctx, storage.ProjectRecord{ID: "forward", Name: "Forward", RepoPath: "/tmp/forward", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert(forward) error = %v", err)
	}
	if err := repos.WebhookTunnelHooks.Upsert(ctx, storage.WebhookTunnelHookRecord{Repo: "acme/looper", HookID: 42, ManagedURL: webhookTunnelManagedURL(cfg, "acme/looper"), SecretRef: webhookTunnelSecretRef("acme/looper"), CreatedAt: 1, UpdatedAt: 1}); err != nil {
		t.Fatalf("WebhookTunnelHooks.Upsert() error = %v", err)
	}
	cfg.Webhook.Mode = config.WebhookModeTunnel
	cfg.Projects = []config.ProjectRefConfig{{ID: "forward", Webhook: config.ProjectWebhookConfig{Mode: config.WebhookModeGHForward}}}
	rt := newWebhookRuntime(cfg, &testLogger{}, func() time.Time { return time.Unix(10, 0) })
	rt.bootstrapDone = true

	rt.Reconcile(repos)

	updated, ok, err := repos.WebhookTunnelHooks.Get(ctx, "acme/looper")
	if err != nil {
		t.Fatalf("WebhookTunnelHooks.Get() error = %v", err)
	}
	if !ok || !updated.Orphaned {
		t.Fatalf("updated record = %#v, want conflicted hook orphaned", updated)
	}
	status := rt.Status()
	if len(status.TunnelHooks) != 1 || !status.TunnelHooks[0].Orphaned {
		t.Fatalf("status.TunnelHooks = %#v, want orphaned conflicted hook", status.TunnelHooks)
	}
	if !status.Degraded || len(status.DegradedReasons) == 0 || !strings.Contains(status.DegradedReasons[0], "webhook mode conflict") {
		t.Fatalf("status degraded=%v reasons=%v, want conflict degraded reason", status.Degraded, status.DegradedReasons)
	}
	if rt.isAllowedTunnelRepo("acme/looper") {
		t.Fatal("conflicted repo remained authorized for tunnel delivery")
	}
	defer rt.stopTunnelServer()
}
