package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
)

func TestBootstrapMintRequiresAuth(t *testing.T) {
	t.Parallel()

	token := "secret-token"
	h := NewHandler(Context{Config: config.Config{
		Server: config.ServerConfig{
			AuthMode:   config.AuthModeLocalToken,
			LocalToken: &token,
		},
	}})

	req := httptest.NewRequest(http.MethodPost, dashboardBootstrapCodePath, nil)
	req.Header.Set("x-request-id", "boot-mint-unauth")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestBootstrapExchangeOnceWorksSecondFails(t *testing.T) {
	t.Parallel()

	token := "secret-token"
	h := NewHandler(Context{Config: config.Config{
		Server: config.ServerConfig{
			AuthMode:   config.AuthModeLocalToken,
			LocalToken: &token,
		},
	}})

	code := mintBootstrapCode(t, h, token)

	// First exchange succeeds without Bearer.
	rec1 := exchangeBootstrapCode(t, h, code, "")
	if rec1.Code != http.StatusOK {
		t.Fatalf("first exchange status = %d, want 200 body=%s", rec1.Code, rec1.Body.String())
	}
	if got := rec1.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	data := parseEnvelopeData(t, rec1)
	if data["token"] != token {
		t.Fatalf("token = %#v, want %q", data["token"], token)
	}

	// Second exchange fails with generic message.
	rec2 := exchangeBootstrapCode(t, h, code, "")
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("second exchange status = %d, want 401", rec2.Code)
	}
	errMap := parseEnvelopeError(t, rec2)
	if errMap["message"] != bootstrapInvalidMsg {
		t.Fatalf("message = %#v, want %q", errMap["message"], bootstrapInvalidMsg)
	}
}

func TestBootstrapExpiredFails(t *testing.T) {
	t.Parallel()

	token := "secret-token"
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	h := NewHandler(Context{
		Config: config.Config{
			Server: config.ServerConfig{
				AuthMode:   config.AuthModeLocalToken,
				LocalToken: &token,
			},
		},
		Now: func() time.Time { return now },
	})
	// Align bootstrap store clock with handler now.
	h.bootstrap.now = func() time.Time { return now }

	code, _, err := h.bootstrap.Mint(now)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	// Advance past TTL.
	later := now.Add(bootstrapCodeTTL + time.Second)
	h.now = func() time.Time { return later }
	h.bootstrap.now = func() time.Time { return later }

	rec := exchangeBootstrapCode(t, h, code, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	errMap := parseEnvelopeError(t, rec)
	if errMap["message"] != bootstrapInvalidMsg {
		t.Fatalf("message = %#v, want %q", errMap["message"], bootstrapInvalidMsg)
	}
}

func TestBootstrapNoneMode404(t *testing.T) {
	t.Parallel()

	h := NewHandler(Context{Config: config.Config{
		Server: config.ServerConfig{AuthMode: config.AuthModeNone},
	}})

	for _, path := range []string{dashboardBootstrapCodePath, dashboardBootstrapExchangePath} {
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader([]byte(`{"code":"x"}`)))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s status = %d, want 404", path, rec.Code)
		}
	}
}

func TestBootstrapConcurrentExchangeOnlyOneWins(t *testing.T) {
	t.Parallel()

	token := "secret-token"
	h := NewHandler(Context{Config: config.Config{
		Server: config.ServerConfig{
			AuthMode:   config.AuthModeLocalToken,
			LocalToken: &token,
		},
	}})
	code := mintBootstrapCode(t, h, token)

	const workers = 16
	var success atomic.Int32
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			rec := exchangeBootstrapCode(t, h, code, "")
			if rec.Code == http.StatusOK {
				success.Add(1)
			}
		}()
	}
	wg.Wait()

	if got := success.Load(); got != 1 {
		t.Fatalf("successful exchanges = %d, want 1", got)
	}
}

func TestBootstrapAuthFailureSetsNoStore(t *testing.T) {
	t.Parallel()

	token := "secret-token"
	h := NewHandler(Context{Config: config.Config{
		Server: config.ServerConfig{
			Host:       "127.0.0.1",
			Port:       17310,
			AuthMode:   config.AuthModeLocalToken,
			LocalToken: &token,
		},
	}})

	// Unauthenticated mint.
	req := httptest.NewRequest(http.MethodPost, dashboardBootstrapCodePath, nil)
	req.Host = "127.0.0.1:17310"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("mint unauth status = %d, want 401", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("mint unauth Cache-Control = %q, want no-store", got)
	}

	// Origin/Host-rejected exchange.
	req = httptest.NewRequest(http.MethodPost, dashboardBootstrapExchangePath, bytes.NewReader([]byte(`{"code":"x"}`)))
	req.Host = "evil.example"
	req.Header.Set("Origin", "http://evil.example")
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("exchange host reject status = %d, want 403", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("exchange host reject Cache-Control = %q, want no-store", got)
	}
}

func mintBootstrapCode(t *testing.T, h *Handler, token string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, dashboardBootstrapCodePath, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("x-request-id", "boot-mint")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("mint status = %d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("mint Cache-Control = %q, want no-store", got)
	}
	data := parseEnvelopeData(t, rec)
	code, _ := data["code"].(string)
	if code == "" {
		t.Fatalf("mint missing code: %#v", data)
	}
	return code
}

func exchangeBootstrapCode(t *testing.T, h *Handler, code, bearer string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"code": code})
	req := httptest.NewRequest(http.MethodPost, dashboardBootstrapExchangePath, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-request-id", "boot-exchange")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func parseEnvelopeData(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var envelope map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode envelope: %v body=%s", err, rec.Body.String())
	}
	data, _ := envelope["data"].(map[string]any)
	if data == nil {
		t.Fatalf("missing data: %#v", envelope)
	}
	return data
}

func parseEnvelopeError(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var envelope map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode envelope: %v body=%s", err, rec.Body.String())
	}
	errMap, _ := envelope["error"].(map[string]any)
	if errMap == nil {
		t.Fatalf("missing error: %#v", envelope)
	}
	return errMap
}
