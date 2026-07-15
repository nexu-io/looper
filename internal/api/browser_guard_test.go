package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nexu-io/looper/internal/config"
)

func TestOriginMismatchRejectedOnUnsafeMethod(t *testing.T) {
	t.Parallel()

	h := NewHandler(Context{Config: config.Config{
		Server: config.ServerConfig{
			Host:     "127.0.0.1",
			Port:     17310,
			AuthMode: config.AuthModeNone,
		},
	}})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/loops", bytes.NewReader([]byte(`{}`)))
	req.Host = "127.0.0.1:17310"
	req.Header.Set("Origin", "http://evil.example")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestOriginMatchAllowedOnUnsafeMethod(t *testing.T) {
	t.Parallel()

	h := NewHandler(Context{Config: config.Config{
		Server: config.ServerConfig{
			Host:     "127.0.0.1",
			Port:     17310,
			AuthMode: config.AuthModeNone,
		},
	}})

	// Unknown route still proves auth/origin passed (not 403).
	req := httptest.NewRequest(http.MethodPost, "/api/v1/does-not-exist", bytes.NewReader([]byte(`{}`)))
	req.Host = "127.0.0.1:17310"
	req.Header.Set("Origin", "http://127.0.0.1:17310")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code == http.StatusForbidden {
		t.Fatalf("status = 403, want non-forbidden (got body %s)", rec.Body.String())
	}
}

func TestCLIWithoutOriginAllowed(t *testing.T) {
	t.Parallel()

	h := NewHandler(Context{Config: config.Config{
		Server: config.ServerConfig{
			Host:     "127.0.0.1",
			Port:     17310,
			AuthMode: config.AuthModeNone,
		},
	}})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/does-not-exist", bytes.NewReader([]byte(`{}`)))
	req.Host = "127.0.0.1:17310"
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code == http.StatusForbidden {
		t.Fatalf("CLI without Origin must not get 403")
	}
}

func TestAttackerHostAndOriginRejectedAuthNone(t *testing.T) {
	t.Parallel()

	// DNS rebinding: Host and Origin both match the attacker domain — must still 403.
	h := NewHandler(Context{Config: config.Config{
		Server: config.ServerConfig{
			Host:     "127.0.0.1",
			Port:     17310,
			AuthMode: config.AuthModeNone,
		},
	}})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/does-not-exist", bytes.NewReader([]byte(`{}`)))
	req.Host = "evil.example"
	req.Header.Set("Origin", "http://evil.example")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 body=%s", rec.Code, rec.Body.String())
	}
}

func TestAttackerHostAndOriginRejectedOnSafeGetAuthNone(t *testing.T) {
	t.Parallel()

	// DNS rebinding via browser-readable GET (config / logs / status): Host+Origin
	// from the attacker domain must 403 even when authMode=none. Auth runs before
	// route dispatch, so unknown paths still prove the guard.
	h := NewHandler(Context{Config: config.Config{
		Server: config.ServerConfig{
			Host:     "127.0.0.1",
			Port:     17310,
			AuthMode: config.AuthModeNone,
		},
	}})

	for _, path := range []string{"/api/v1/config", "/api/v1/status", "/api/v1/does-not-exist"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Host = "evil.example"
		req.Header.Set("Origin", "http://evil.example")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("GET %s status = %d, want 403 body=%s", path, rec.Code, rec.Body.String())
		}
	}
}

func TestAttackerHostWithoutOriginRejectedOnSafeGetAuthNone(t *testing.T) {
	t.Parallel()

	// Same-origin DNS rebinding omits Origin; Host allowlist must still reject.
	h := NewHandler(Context{Config: config.Config{
		Server: config.ServerConfig{
			Host:     "127.0.0.1",
			Port:     17310,
			AuthMode: config.AuthModeNone,
		},
	}})

	for _, path := range []string{"/api/v1/config", "/api/v1/status", "/api/v1/does-not-exist"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Host = "evil.example"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("GET %s without Origin status = %d, want 403 body=%s", path, rec.Code, rec.Body.String())
		}
	}
}

func TestCLIGetWithoutOriginAllowed(t *testing.T) {
	t.Parallel()

	h := NewHandler(Context{Config: config.Config{
		Server: config.ServerConfig{
			Host:     "127.0.0.1",
			Port:     17310,
			AuthMode: config.AuthModeNone,
		},
	}})

	// Unknown route still proves auth/origin passed (not 403).
	req := httptest.NewRequest(http.MethodGet, "/api/v1/does-not-exist", nil)
	req.Host = "127.0.0.1:17310"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code == http.StatusForbidden {
		t.Fatalf("CLI GET without Origin must not get 403")
	}
}

func TestLegitimateHostOriginAllowedOnSafeGet(t *testing.T) {
	t.Parallel()

	h := NewHandler(Context{Config: config.Config{
		Server: config.ServerConfig{
			Host:     "127.0.0.1",
			Port:     17310,
			AuthMode: config.AuthModeNone,
		},
	}})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/does-not-exist", nil)
	req.Host = "127.0.0.1:17310"
	req.Header.Set("Origin", "http://127.0.0.1:17310")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code == http.StatusForbidden {
		t.Fatalf("legitimate loopback GET Host/Origin must not get 403: %s", rec.Body.String())
	}
}

func TestLegitimateHostOriginAllowed(t *testing.T) {
	t.Parallel()

	h := NewHandler(Context{Config: config.Config{
		Server: config.ServerConfig{
			Host:     "127.0.0.1",
			Port:     17310,
			AuthMode: config.AuthModeNone,
		},
	}})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/does-not-exist", bytes.NewReader([]byte(`{}`)))
	req.Host = "localhost:17310"
	req.Header.Set("Origin", "http://127.0.0.1:17310")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code == http.StatusForbidden {
		t.Fatalf("legitimate loopback Host/Origin must not get 403: %s", rec.Body.String())
	}
}

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
