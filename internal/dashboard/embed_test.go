package dashboard

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandlerServesIndex(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/dashboard/", nil)
	Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Looper") {
		t.Fatalf("body missing Looper marker: %q", body)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	assertSecurityHeaders(t, rec)
}

func TestHandlerSPAFallbackForNavigation(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/dashboard/loops/42", nil)
	Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Looper") {
		t.Fatalf("expected SPA fallback index.html body, got %q", body)
	}
}

func TestHandlerMissingAssetIs404(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/dashboard/assets/app.deadbeef.js", nil)
	Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	// Must not SPA-fallback to index.html for missing static assets.
	if strings.Contains(rec.Body.String(), "<!DOCTYPE html") || strings.Contains(rec.Body.String(), "<!doctype html") {
		t.Fatalf("missing hashed asset must not fall back to index.html: %q", rec.Body.String())
	}
}

func TestHandlerRejectsNonGET(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/dashboard/", nil)
	Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != "GET, HEAD" {
		t.Fatalf("Allow = %q, want GET, HEAD", got)
	}
}

func TestHandlerAssetTrailingSlashIs404(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/dashboard/assets/missing.js/", nil)
	Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if strings.Contains(strings.ToLower(rec.Body.String()), "<!doctype html") {
		t.Fatalf("asset trailing-slash must not SPA-fallback: %q", rec.Body.String())
	}
}

func TestHandlerHEADIndex(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodHead, "/dashboard/", nil)
	Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("HEAD body length = %d, want 0", rec.Body.Len())
	}
	if _, err := io.Copy(io.Discard, rec.Result().Body); err != nil {
		t.Fatalf("drain body: %v", err)
	}
}

func TestLooksHashedAsset(t *testing.T) {
	t.Parallel()

	if !looksHashedAsset("assets/index-a1b2c3d4.js") {
		t.Fatal("expected hashed js asset")
	}
	// Realistic Vite filename: non-hex content hash.
	if !looksHashedAsset("assets/index-w7rK2NGL.js") {
		t.Fatal("expected Vite-style hashed asset under assets/")
	}
	if !looksHashedAsset("assets/index-w7rK2NGL.css") {
		t.Fatal("expected Vite-style hashed css under assets/")
	}
	if looksHashedAsset("index.html") {
		t.Fatal("index.html should not look hashed")
	}
	// Public icons must not be treated as content-hashed (false positive via "-touch-icon").
	if looksHashedAsset("apple-touch-icon.png") {
		t.Fatal("apple-touch-icon.png should not look hashed")
	}
	if looksHashedAsset("favicon-32x32.png") {
		t.Fatal("favicon-32x32.png should not look hashed")
	}
}

func TestServeNamedFavicon(t *testing.T) {
	t.Parallel()

	// Production assets may or may not be present in the test binary; fallback
	// always embeds favicon.ico after the dashboard favicon work.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/favicon.ico", nil)
	if !ServeNamed(rec, req, "favicon.ico") {
		t.Fatal("ServeNamed(favicon.ico) = false, want true when embedded")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Body.Len() == 0 {
		t.Fatal("favicon body empty")
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "image/") {
		t.Fatalf("Content-Type = %q, want image/*", ct)
	}
}

func assertSecurityHeaders(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	checks := map[string]string{
		"Content-Security-Policy": cspHeader,
		"Referrer-Policy":         "no-referrer",
		"X-Content-Type-Options":  "nosniff",
		"X-Frame-Options":         "DENY",
	}
	for name, want := range checks {
		if got := rec.Header().Get(name); got != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
	}
}
