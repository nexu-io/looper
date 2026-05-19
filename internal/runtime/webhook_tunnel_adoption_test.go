package runtime

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/storage"
)

func TestReconcileTunnelHookPatchPreservesWebhookSecret(t *testing.T) {
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
	if err := os.WriteFile(secretPath, []byte("top-secret"), 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}
	hook := webhookTunnelGitHubHook{ID: 42, Active: false, Events: webhookForwardEvents}
	hook.Config.URL = record.ManagedURL
	hook.Config.ContentType = "json"
	hook.Config.InsecureSSL = "0"
	client := &fakeWebhookTunnelGitHubClient{getHook: hook, getFound: true}
	rt := newWebhookRuntime(cfg, &testLogger{}, func() time.Time { return time.Unix(10, 0) })
	rt.tunnelClient = client

	state := rt.reconcileTunnelHook(ctx, repos.WebhookTunnelHooks, record.Repo, record, true, time.Now().UnixNano())

	if state.LastError != "" {
		t.Fatalf("state.LastError = %q, want empty", state.LastError)
	}
	if client.updateCalls != 1 {
		t.Fatalf("UpdateHook calls = %d, want 1", client.updateCalls)
	}
	if client.lastUpdate.secret != "top-secret" {
		t.Fatalf("UpdateHook secret = %q, want preserved secret", client.lastUpdate.secret)
	}
}

func TestReconcileTunnelHookCreatesManagedHookWithoutAdoptingURLMatch(t *testing.T) {
	t.Parallel()

	ctx, repos, cfg := setupWebhookTunnelTestRepos(t)
	const repo = "acme/looper"
	client := &fakeWebhookTunnelGitHubClient{createHook: webhookTunnelGitHubHook{ID: 91, Active: true, Events: webhookForwardEvents}}
	rt := newWebhookRuntime(cfg, &testLogger{}, func() time.Time { return time.Unix(10, 0) })
	rt.tunnelClient = client
	rt.tunnelStore = repos.WebhookTunnelHooks
	rt.setAllowedTunnelRepos(map[string]struct{}{repo: {}})
	server := &webhookTunnelServer{runtime: rt}

	state := rt.reconcileTunnelHook(ctx, repos.WebhookTunnelHooks, repo, storage.WebhookTunnelHookRecord{}, false, time.Now().UnixNano())

	if state.LastError != "" {
		t.Fatalf("state.LastError = %q, want empty", state.LastError)
	}
	if client.createCalls != 1 {
		t.Fatalf("CreateHook calls = %d, want 1", client.createCalls)
	}
	if client.updateCalls != 0 {
		t.Fatalf("UpdateHook calls = %d, want 0", client.updateCalls)
	}
	record, ok, err := repos.WebhookTunnelHooks.Get(ctx, repo)
	if err != nil {
		t.Fatalf("WebhookTunnelHooks.Get() error = %v", err)
	}
	if !ok || record.HookID != 91 {
		t.Fatalf("record = %#v found=%v, want created hook id 91", record, ok)
	}
	secret, err := readWebhookTunnelSecret(cfg.Storage.DBPath, record.SecretRef)
	if err != nil {
		t.Fatalf("readWebhookTunnelSecret() error = %v", err)
	}
	if secret == "" {
		t.Fatal("readWebhookTunnelSecret() = empty, want persisted local secret")
	}
	req := httptest.NewRequest(http.MethodPost, "/webhook/acme/looper", http.NoBody)
	req.Header.Set("X-GitHub-Event", "ping")
	req.Header.Set("X-GitHub-Delivery", "delivery-ping")
	req.Header.Set("X-Hub-Signature-256", testGitHubSignature(secret, nil))
	resp := httptest.NewRecorder()

	server.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("ServeHTTP() status = %d, want %d", resp.Code, http.StatusOK)
	}
}

func TestReconcileTunnelHookCreateErrorDoesNotAdoptURLMatch(t *testing.T) {
	t.Parallel()

	ctx, repos, cfg := setupWebhookTunnelTestRepos(t)
	const repo = "acme/looper"
	client := &fakeWebhookTunnelGitHubClient{createErr: errors.New("create timed out")}
	rt := newWebhookRuntime(cfg, &testLogger{}, func() time.Time { return time.Unix(10, 0) })
	rt.tunnelClient = client
	rt.tunnelStore = repos.WebhookTunnelHooks

	state := rt.reconcileTunnelHook(ctx, repos.WebhookTunnelHooks, repo, storage.WebhookTunnelHookRecord{}, false, time.Now().UnixNano())

	if state.LastError != "create timed out" {
		t.Fatalf("state.LastError = %q, want create failure", state.LastError)
	}
	if client.createCalls != 1 {
		t.Fatalf("CreateHook calls = %d, want 1", client.createCalls)
	}
	if client.updateCalls != 0 {
		t.Fatalf("UpdateHook calls = %d, want 0", client.updateCalls)
	}
	record, ok, err := repos.WebhookTunnelHooks.Get(ctx, repo)
	if err != nil {
		t.Fatalf("WebhookTunnelHooks.Get() error = %v", err)
	}
	if ok {
		t.Fatalf("record = %#v found=%v, want no persisted record", record, ok)
	}
}

func TestReconcileTunnelHookRecreateErrorDoesNotAdoptURLMatch(t *testing.T) {
	t.Parallel()

	ctx, repos, cfg := setupWebhookTunnelTestRepos(t)
	const repo = "acme/looper"
	url := webhookTunnelManagedURL(cfg, repo)
	record := storage.WebhookTunnelHookRecord{Repo: repo, HookID: 42, ManagedURL: url, SecretRef: webhookTunnelSecretRef(repo), CreatedAt: 1, UpdatedAt: 1}
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
	client := &fakeWebhookTunnelGitHubClient{createErr: errors.New("hook already exists")}
	rt := newWebhookRuntime(cfg, &testLogger{}, func() time.Time { return time.Unix(10, 0) })
	rt.tunnelClient = client

	state := rt.reconcileTunnelHook(ctx, repos.WebhookTunnelHooks, repo, record, true, time.Now().UnixNano())

	if state.LastError != "recreate missing hook: hook already exists" {
		t.Fatalf("state.LastError = %q, want recreate failure", state.LastError)
	}
	if client.getCalls != 1 {
		t.Fatalf("GetHook calls = %d, want 1", client.getCalls)
	}
	if client.createCalls != 1 {
		t.Fatalf("CreateHook calls = %d, want 1", client.createCalls)
	}
	if client.updateCalls != 0 {
		t.Fatalf("UpdateHook calls = %d, want 0", client.updateCalls)
	}
	updated, ok, err := repos.WebhookTunnelHooks.Get(ctx, repo)
	if err != nil {
		t.Fatalf("WebhookTunnelHooks.Get() error = %v", err)
	}
	if !ok || updated.HookID != record.HookID {
		t.Fatalf("record = %#v found=%v, want original hook record unchanged", updated, ok)
	}
	secret, err := readWebhookTunnelSecret(cfg.Storage.DBPath, updated.SecretRef)
	if err != nil {
		t.Fatalf("readWebhookTunnelSecret() error = %v", err)
	}
	if secret != "top-secret" {
		t.Fatalf("readWebhookTunnelSecret() = %q, want preserved local secret", secret)
	}
}

func TestReconcileTunnelHookTreatsDesiredURLAsManagedInsteadOfOrphaning(t *testing.T) {
	t.Parallel()

	ctx, repos, cfg := setupWebhookTunnelTestRepos(t)
	const repo = "acme/looper"
	record := storage.WebhookTunnelHookRecord{Repo: repo, HookID: 42, ManagedURL: "https://old.example/webhook/acme/looper", SecretRef: webhookTunnelSecretRef(repo), CreatedAt: 1, UpdatedAt: 1}
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
	desiredURL := webhookTunnelManagedURL(cfg, repo)
	hook := webhookTunnelGitHubHook{ID: 42, Active: true, Events: webhookForwardEvents}
	hook.Config.URL = desiredURL
	hook.Config.ContentType = "json"
	hook.Config.InsecureSSL = "0"
	client := &fakeWebhookTunnelGitHubClient{getHook: hook, getFound: true}
	rt := newWebhookRuntime(cfg, &testLogger{}, func() time.Time { return time.Unix(10, 0) })
	rt.tunnelClient = client

	state := rt.reconcileTunnelHook(ctx, repos.WebhookTunnelHooks, repo, record, true, time.Now().UnixNano())

	if state.LastError != "" {
		t.Fatalf("state.LastError = %q, want empty", state.LastError)
	}
	if client.updateCalls != 0 {
		t.Fatalf("UpdateHook calls = %d, want 0", client.updateCalls)
	}
	updated, ok, err := repos.WebhookTunnelHooks.Get(ctx, repo)
	if err != nil {
		t.Fatalf("WebhookTunnelHooks.Get() error = %v", err)
	}
	if !ok {
		t.Fatal("WebhookTunnelHooks.Get() found = false, want true")
	}
	if updated.Orphaned {
		t.Fatalf("updated record = %#v, want non-orphaned", updated)
	}
	if updated.ManagedURL != desiredURL {
		t.Fatalf("updated.ManagedURL = %q, want desired URL %q", updated.ManagedURL, desiredURL)
	}
}

func TestReconcileTunnelHooksHostQualifiedRepoReturnsLastErrorWithoutGitHubCall(t *testing.T) {
	t.Parallel()

	ctx, repos, cfg := setupWebhookTunnelTestRepos(t)
	client := &fakeWebhookTunnelGitHubClient{}
	rt := newWebhookRuntime(cfg, &testLogger{}, func() time.Time { return time.Unix(10, 0) })
	rt.ghPath = "/usr/bin/gh"
	rt.tunnelClient = client

	rt.reconcileTunnelHooks(ctx, repos, map[string]struct{}{"github.example.com/acme/looper": {}})
	defer rt.stopTunnelServer()

	status := rt.Status()
	if len(status.TunnelHooks) != 1 || status.TunnelHooks[0].LastError != "tunnel mode does not support host-qualified repo names" {
		t.Fatalf("status.TunnelHooks = %#v, want host-qualified LastError", status.TunnelHooks)
	}
	if client.getCalls != 0 || client.createCalls != 0 || client.updateCalls != 0 || client.deleteCalls != 0 {
		t.Fatalf("client calls = %#v, want none", client)
	}
}
