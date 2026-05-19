package runtime

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
	"github.com/nexu-io/looper/internal/webhookforward"
)

func TestWebhookTunnelServerServeHTTP(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	cfg, err := config.DefaultConfig(tempDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Webhook.PublicBaseURL = "https://example.com/base"
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
	rt.setAllowedTunnelRepos(map[string]struct{}{repoName: {}})
	rt.forwarder = func() WebhookForwarder { return forwarder }
	server := &webhookTunnelServer{runtime: rt}

	t.Run("ping skips forwarder", func(t *testing.T) {
		forwarder.reset()
		req := httptest.NewRequest(http.MethodPost, "/base/webhook/acme/looper", http.NoBody)
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
		req := httptest.NewRequest(http.MethodPost, "/base/webhook/acme/looper", bytes.NewReader(body))
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
		req := httptest.NewRequest(http.MethodPost, "/base/webhook/acme/looper", bytes.NewReader(body))
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
		req := httptest.NewRequest(http.MethodPost, "/base/webhook/acme/looper", bytes.NewReader(body))
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
		req := httptest.NewRequest(http.MethodPost, "/base/webhook/acme/looper", bytes.NewReader(body))
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

	t.Run("repo outside desired tunnel set is rejected even if persisted hook remains active", func(t *testing.T) {
		forwarder.reset()
		rt.setAllowedTunnelRepos(nil)
		body := []byte(`{"repository":{"full_name":"acme/looper"}}`)
		req := httptest.NewRequest(http.MethodPost, "/base/webhook/acme/looper", bytes.NewReader(body))
		req.Header.Set("X-GitHub-Event", "pull_request")
		req.Header.Set("X-GitHub-Delivery", "delivery-blocked")
		req.Header.Set("X-Hub-Signature-256", testGitHubSignature(secret, body))
		resp := httptest.NewRecorder()

		server.ServeHTTP(resp, req)

		if resp.Code != http.StatusNotFound {
			t.Fatalf("ServeHTTP() status = %d, want %d", resp.Code, http.StatusNotFound)
		}
		if forwarder.calls != 0 {
			t.Fatalf("forwarder calls = %d, want 0", forwarder.calls)
		}
	})
}
