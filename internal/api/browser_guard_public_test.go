package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nexu-io/looper/internal/config"
)

func TestHTTPSBaseURLBareHostAllowed(t *testing.T) {
	t.Parallel()

	// Remote HTTPS baseURL: browser sends Host without :443 and Origin without port.
	baseURL := "https://dashboard.example.com"
	token := "secret-token"
	h := NewHandler(Context{Config: config.Config{
		Server: config.ServerConfig{
			Host:       "0.0.0.0",
			Port:       17310,
			AuthMode:   config.AuthModeLocalToken,
			LocalToken: &token,
			BaseURL:    &baseURL,
		},
	}})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/does-not-exist", bytes.NewReader([]byte(`{}`)))
	req.Host = "dashboard.example.com"
	req.Header.Set("Origin", "https://dashboard.example.com")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code == http.StatusForbidden {
		t.Fatalf("bare Host with https baseURL must not get 403: %s", rec.Body.String())
	}
}

func TestPublicHostWithoutOriginRejectedOnNonCallback(t *testing.T) {
	t.Parallel()

	// 0.0.0.0 bind + no baseUrl: public Host without Origin is rejected for
	// ordinary API paths (CLI still uses loopback Host).
	h := NewHandler(Context{Config: config.Config{
		Server: config.ServerConfig{
			Host:     "0.0.0.0",
			Port:     17310,
			AuthMode: config.AuthModeNone,
		},
	}})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
	req.Host = "daemon.example.com:17310"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("public Host without Origin on non-callback = %d, want 403 body=%s", rec.Code, rec.Body.String())
	}
}

func TestFeishuCallbackPublicHostWithoutOriginNotHostRejected(t *testing.T) {
	t.Parallel()

	// Documented Feishu callback: public Host, no Origin, authMode=none, no
	// server.baseUrl. Host allowlist must not 403 before verification-token logic.
	h := NewHandler(Context{Config: config.Config{
		Server: config.ServerConfig{
			Host:     "0.0.0.0",
			Port:     17310,
			AuthMode: config.AuthModeNone,
		},
	}})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/hitl/feishu", bytes.NewReader([]byte(`{}`)))
	req.Host = "daemon.example.com:17310"
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code == http.StatusForbidden {
		// Host guard uses ErrorCodeUnauthorized; Feishu unconfigured token uses
		// validation_failed. Either way Host rejection would be 403 with
		// "Host is not allowed".
		if bytes.Contains(rec.Body.Bytes(), []byte("Host is not allowed")) {
			t.Fatalf("Feishu callback must not be Host-rejected; body=%s", rec.Body.String())
		}
	}
}

func TestFeishuCallbackWithOriginStillHostChecked(t *testing.T) {
	t.Parallel()

	// Browser-initiated request with Origin still requires Host allowlist.
	h := NewHandler(Context{Config: config.Config{
		Server: config.ServerConfig{
			Host:     "0.0.0.0",
			Port:     17310,
			AuthMode: config.AuthModeNone,
		},
	}})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/hitl/feishu", bytes.NewReader([]byte(`{}`)))
	req.Host = "evil.example"
	req.Header.Set("Origin", "http://evil.example")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("Feishu path with attacker Origin must still 403, got %d body=%s", rec.Code, rec.Body.String())
	}
}
