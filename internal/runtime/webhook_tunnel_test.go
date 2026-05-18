package runtime

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
	"github.com/nexu-io/looper/internal/webhookforward"
)

func TestValidGitHubSignature(t *testing.T) {
	t.Parallel()

	secret := "top-secret"
	body := []byte(`{"repository":{"full_name":"acme/looper"}}`)
	signature := testGitHubSignature(secret, body)

	if !validGitHubSignature(secret, body, signature) {
		t.Fatal("validGitHubSignature() = false, want true for valid sha256 signature")
	}
	if validGitHubSignature(secret, []byte(`{"repository":{"full_name":"acme/other"}}`), signature) {
		t.Fatal("validGitHubSignature() = true, want false for tampered body")
	}
	if validGitHubSignature(secret, body, "") {
		t.Fatal("validGitHubSignature() = true, want false for missing signature")
	}
	if validGitHubSignature(secret, body, "sha1=deadbeef") {
		t.Fatal("validGitHubSignature() = true, want false for wrong signature algorithm")
	}
}

func TestRepoFromWebhookTunnelPath(t *testing.T) {
	t.Parallel()

	if got, ok := repoFromWebhookTunnelPath("/webhook/acme/looper"); !ok || got != "acme/looper" {
		t.Fatalf("repoFromWebhookTunnelPath() = (%q, %v), want (%q, true)", got, ok, "acme/looper")
	}

	for _, path := range []string{"", "/webhook", "/webhook/acme", "/hook/acme/looper", "/webhook//looper", "/webhook/acme/", "/webhook/acme/looper/extra"} {
		if got, ok := repoFromWebhookTunnelPath(path); ok || got != "" {
			t.Fatalf("repoFromWebhookTunnelPath(%q) = (%q, %v), want (\"\", false)", path, got, ok)
		}
	}
}

func TestWebhookTunnelManagedURLTrimsTrailingSlash(t *testing.T) {
	t.Parallel()

	cfg := config.Config{}
	cfg.Webhook.PublicBaseURL = " https://example.com/base// "

	if got := webhookTunnelManagedURL(cfg, "acme/looper"); got != "https://example.com/base/webhook/acme/looper" {
		t.Fatalf("webhookTunnelManagedURL() = %q, want %q", got, "https://example.com/base/webhook/acme/looper")
	}
}

func TestWebhookTunnelServerServeHTTP(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	cfg, err := config.DefaultConfig(tempDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	dbPath := filepath.Join(tempDir, "looper.sqlite")
	cfg.Storage.DBPath = dbPath
	coordinator := openMigratedCoordinator(t, dbPath, filepath.Join(tempDir, "backups"))
	t.Cleanup(func() {
		if err := coordinator.Close(); err != nil {
			t.Fatalf("coordinator.Close() error = %v", err)
		}
	})
	repos := storage.NewRepositories(coordinator.DB())

	const (
		repoName  = "acme/looper"
		secret    = "top-secret"
		secretRef = "webhook_acme_looper.key"
	)
	if err := repos.WebhookTunnelHooks.Upsert(context.Background(), storage.WebhookTunnelHookRecord{
		Repo:       repoName,
		HookID:     42,
		ManagedURL: "https://example.com/webhook/acme/looper",
		SecretRef:  secretRef,
		CreatedAt:  time.Now().UnixNano(),
		UpdatedAt:  time.Now().UnixNano(),
	}); err != nil {
		t.Fatalf("WebhookTunnelHooks.Upsert() error = %v", err)
	}
	secretPath := webhookTunnelSecretPath(dbPath, secretRef)
	if err := os.MkdirAll(filepath.Dir(secretPath), 0o700); err != nil {
		t.Fatalf("os.MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(secretPath, []byte(secret), 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}
	if info, err := os.Stat(secretPath); err != nil {
		t.Fatalf("os.Stat() error = %v", err)
	} else if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("secret file mode = %o, want 600", got)
	}

	forwarder := &testTunnelForwarder{result: webhookforward.ForwardResult{Status: "accepted", WorkItems: 1}}
	rt := newWebhookRuntime(cfg, &testLogger{}, func() time.Time { return time.Unix(1, 0) })
	rt.tunnelStore = repos.WebhookTunnelHooks
	rt.forwarder = func() WebhookForwarder { return forwarder }
	server := &webhookTunnelServer{runtime: rt}

	t.Run("ping skips forwarder", func(t *testing.T) {
		forwarder.reset()
		req := httptest.NewRequest(http.MethodPost, "/webhook/acme/looper", http.NoBody)
		req.Header.Set("X-GitHub-Event", "ping")
		req.Header.Set("X-GitHub-Delivery", "delivery-ping")
		req.Header.Set("X-Hub-Signature-256", testGitHubSignature(secret, nil))
		resp := httptest.NewRecorder()

		server.ServeHTTP(resp, req)

		if resp.Code != http.StatusOK {
			t.Fatalf("ServeHTTP() status = %d, want %d", resp.Code, http.StatusOK)
		}
		if forwarder.calls != 0 {
			t.Fatalf("forwarder calls = %d, want 0", forwarder.calls)
		}
	})

	t.Run("valid delivery forwards", func(t *testing.T) {
		forwarder.reset()
		body := []byte(`{"repository":{"full_name":"acme/looper"}}`)
		req := httptest.NewRequest(http.MethodPost, "/webhook/acme/looper", bytes.NewReader(body))
		req.Header.Set("X-GitHub-Event", "pull_request")
		req.Header.Set("X-GitHub-Delivery", "delivery-accepted")
		req.Header.Set("X-Hub-Signature-256", testGitHubSignature(secret, body))
		resp := httptest.NewRecorder()

		server.ServeHTTP(resp, req)

		if resp.Code != http.StatusAccepted {
			t.Fatalf("ServeHTTP() status = %d, want %d", resp.Code, http.StatusAccepted)
		}
		if forwarder.calls != 1 {
			t.Fatalf("forwarder calls = %d, want 1", forwarder.calls)
		}
		if forwarder.lastRequest.DeliveryID != "delivery-accepted" || forwarder.lastRequest.EventType != "pull_request" || string(forwarder.lastRequest.Payload) != string(body) {
			t.Fatalf("forwarder request = %#v, want delivery payload forwarded", forwarder.lastRequest)
		}
	})

	t.Run("bad hmac rejected", func(t *testing.T) {
		forwarder.reset()
		body := []byte(`{"repository":{"full_name":"acme/looper"}}`)
		req := httptest.NewRequest(http.MethodPost, "/webhook/acme/looper", bytes.NewReader(body))
		req.Header.Set("X-GitHub-Event", "pull_request")
		req.Header.Set("X-GitHub-Delivery", "delivery-bad-hmac")
		req.Header.Set("X-Hub-Signature-256", testGitHubSignature("wrong-secret", body))
		resp := httptest.NewRecorder()

		server.ServeHTTP(resp, req)

		if resp.Code != http.StatusUnauthorized {
			t.Fatalf("ServeHTTP() status = %d, want %d", resp.Code, http.StatusUnauthorized)
		}
		if forwarder.calls != 0 {
			t.Fatalf("forwarder calls = %d, want 0", forwarder.calls)
		}
	})

	t.Run("oversized payload rejected before hmac validation", func(t *testing.T) {
		forwarder.reset()
		body := bytes.Repeat([]byte("a"), maxWebhookTunnelPayloadBytes+1)
		req := httptest.NewRequest(http.MethodPost, "/webhook/acme/looper", bytes.NewReader(body))
		req.Header.Set("X-GitHub-Event", "push")
		req.Header.Set("X-GitHub-Delivery", "delivery-too-large")
		req.Header.Set("X-Hub-Signature-256", testGitHubSignature(secret, body))
		resp := httptest.NewRecorder()

		server.ServeHTTP(resp, req)

		if resp.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("ServeHTTP() status = %d, want %d", resp.Code, http.StatusRequestEntityTooLarge)
		}
		if forwarder.calls != 0 {
			t.Fatalf("forwarder calls = %d, want 0", forwarder.calls)
		}
	})

	t.Run("repository mismatch rejected", func(t *testing.T) {
		forwarder.reset()
		body := []byte(`{"repository":{"full_name":"acme/other"}}`)
		req := httptest.NewRequest(http.MethodPost, "/webhook/acme/looper", bytes.NewReader(body))
		req.Header.Set("X-GitHub-Event", "pull_request")
		req.Header.Set("X-GitHub-Delivery", "delivery-mismatch")
		req.Header.Set("X-Hub-Signature-256", testGitHubSignature(secret, body))
		resp := httptest.NewRecorder()

		server.ServeHTTP(resp, req)

		if resp.Code != http.StatusBadRequest {
			t.Fatalf("ServeHTTP() status = %d, want %d", resp.Code, http.StatusBadRequest)
		}
		if forwarder.calls != 0 {
			t.Fatalf("forwarder calls = %d, want 0", forwarder.calls)
		}
	})
}

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

func setupWebhookTunnelTestRepos(t *testing.T) (context.Context, *storage.Repositories, config.Config) {
	t.Helper()

	tempDir := t.TempDir()
	cfg, err := config.DefaultConfig(tempDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Storage.DBPath = filepath.Join(tempDir, "looper.sqlite")
	cfg.Webhook.Enabled = true
	cfg.Webhook.Mode = config.WebhookModeTunnel
	cfg.Webhook.ListenPort = 0
	ghPath := "/usr/bin/gh"
	cfg.Tools.GHPath = &ghPath
	coordinator := openMigratedCoordinator(t, cfg.Storage.DBPath, filepath.Join(tempDir, "backups"))
	t.Cleanup(func() {
		if err := coordinator.Close(); err != nil {
			t.Fatalf("coordinator.Close() error = %v", err)
		}
	})
	return context.Background(), storage.NewRepositories(coordinator.DB()), cfg
}

type fakeWebhookTunnelGitHubClient struct {
	getHook      webhookTunnelGitHubHook
	getFound     bool
	createHook   webhookTunnelGitHubHook
	createErr    error
	updateHook   webhookTunnelGitHubHook
	updateErr    error
	deleteErr    error
	getCalls     int
	createCalls  int
	updateCalls  int
	deleteCalls  int
	lastUpdate   fakeWebhookTunnelUpdateCall
	deletedHooks []int64
}

type fakeWebhookTunnelUpdateCall struct {
	repo   string
	id     int64
	url    string
	secret string
	active bool
}

func (f *fakeWebhookTunnelGitHubClient) GetHook(context.Context, string, int64) (webhookTunnelGitHubHook, bool, error) {
	f.getCalls++
	return f.getHook, f.getFound, nil
}

func (f *fakeWebhookTunnelGitHubClient) CreateHook(context.Context, string, string, string, []string) (webhookTunnelGitHubHook, error) {
	f.createCalls++
	if f.createHook.ID == 0 {
		f.createHook.ID = 999
	}
	return f.createHook, f.createErr
}

func (f *fakeWebhookTunnelGitHubClient) UpdateHook(_ context.Context, repo string, id int64, url string, secret string, _ []string, active bool) (webhookTunnelGitHubHook, error) {
	f.updateCalls++
	f.lastUpdate = fakeWebhookTunnelUpdateCall{repo: repo, id: id, url: url, secret: secret, active: active}
	return f.updateHook, f.updateErr
}

func (f *fakeWebhookTunnelGitHubClient) DeleteHook(_ context.Context, _ string, id int64) error {
	f.deleteCalls++
	f.deletedHooks = append(f.deletedHooks, id)
	return f.deleteErr
}

type testTunnelForwarder struct {
	result      webhookforward.ForwardResult
	err         error
	calls       int
	lastRequest webhookforward.DeliveryRequest
}

func (f *testTunnelForwarder) Forward(_ context.Context, req webhookforward.DeliveryRequest) (webhookforward.ForwardResult, error) {
	f.calls++
	f.lastRequest = req
	if f.err != nil {
		return webhookforward.ForwardResult{}, f.err
	}
	return f.result, nil
}

func (f *testTunnelForwarder) Stats() webhookforward.Stats { return webhookforward.Stats{} }

func (f *testTunnelForwarder) Close() {}

func (f *testTunnelForwarder) reset() {
	f.calls = 0
	f.lastRequest = webhookforward.DeliveryRequest{}
	f.err = nil
}

func testGitHubSignature(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}
