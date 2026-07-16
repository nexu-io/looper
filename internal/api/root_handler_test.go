package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewRootHandlerDashboardMethodNotAllowed(t *testing.T) {
	t.Parallel()

	root := NewRootHandler(http.NotFoundHandler(), http.NotFoundHandler())
	rec := httptest.NewRecorder()
	root.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/dashboard", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != "GET, HEAD" {
		t.Fatalf("Allow = %q, want GET, HEAD", got)
	}
}

func TestNewRootHandlerDashboardRedirectAndAPI(t *testing.T) {
	t.Parallel()

	apiHits := 0
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiHits++
		w.WriteHeader(http.StatusTeapot)
	})
	dash := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("dash"))
	})
	root := NewRootHandler(api, dash)

	rec := httptest.NewRecorder()
	root.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/dashboard", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("redirect status = %d, want 302", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/dashboard/" {
		t.Fatalf("Location = %q, want /dashboard/", loc)
	}

	// Bootstrap codes arrive as /dashboard?code=...; redirect must keep the query.
	rec = httptest.NewRecorder()
	root.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/dashboard?code=bootstrap-one-shot", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("query redirect status = %d, want 302", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/dashboard/?code=bootstrap-one-shot" {
		t.Fatalf("Location = %q, want /dashboard/?code=bootstrap-one-shot", loc)
	}

	rec = httptest.NewRecorder()
	root.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/dashboard/", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "dash" {
		t.Fatalf("dashboard serve = %d %q", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	root.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/status", nil))
	if rec.Code != http.StatusTeapot || apiHits != 1 {
		t.Fatalf("api status = %d hits=%d", rec.Code, apiHits)
	}

	// Host-root favicon is served from embedded dashboard assets (not the API).
	rec = httptest.NewRecorder()
	root.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/favicon.ico", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("favicon status = %d, want 200", rec.Code)
	}
	if rec.Body.Len() == 0 {
		t.Fatal("favicon body empty")
	}
	if apiHits != 1 {
		t.Fatalf("favicon must not hit API handler, apiHits=%d", apiHits)
	}
}
