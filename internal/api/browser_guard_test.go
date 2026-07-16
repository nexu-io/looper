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
