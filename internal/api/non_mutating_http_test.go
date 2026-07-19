package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	looperdruntime "github.com/nexu-io/looper/internal/runtime"
	pkgapi "github.com/nexu-io/looper/pkg/api"
)

// Contract (#580): under starting and degraded, mutating HTTP methods return
// explicit 503 SERVICE_UNAVAILABLE across work-producing surfaces; reads stay open.
func TestNonMutatingHTTPCoverageUnderStartingAndDegraded(t *testing.T) {
	t.Parallel()

	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}

	mutationPaths := []struct {
		method string
		path   string
		body   string
	}{
		{method: http.MethodPost, path: "/api/v1/loops", body: `{}`},
		{method: http.MethodPost, path: "/api/v1/projects", body: `{}`},
		{method: http.MethodPost, path: "/api/v1/workers", body: `{}`},
		{method: http.MethodPost, path: "/api/v1/planners", body: `{}`},
		{method: http.MethodPatch, path: "/api/v1/config", body: `{"revision":"x","set":{},"unset":[]}`},
		{method: http.MethodPost, path: "/api/v1/runs/reconcile-stale", body: `{}`},
		{method: http.MethodPost, path: "/webhook/forward", body: `{}`},
		{method: http.MethodDelete, path: "/api/v1/projects/some-id", body: ``},
	}

	for _, state := range []struct {
		name string
		err  error
	}{
		{name: "starting", err: looperdruntime.ErrAdmissionNotReady},
		{name: "degraded", err: looperdruntime.ErrAdmissionDegraded},
	} {
		state := state
		t.Run(state.name, func(t *testing.T) {
			t.Parallel()
			handler := NewHandler(Context{
				Config:  cfg,
				Runtime: admissionGateRuntime{err: state.err},
			})

			for _, route := range mutationPaths {
				route := route
				t.Run(route.method+" "+route.path, func(t *testing.T) {
					t.Parallel()
					rec := httptest.NewRecorder()
					req := httptest.NewRequest(route.method, route.path, strings.NewReader(route.body))
					if route.body != "" {
						req.Header.Set("Content-Type", "application/json")
					}
					// Loopback so webhook forward auth does not mask admission 503.
					req.RemoteAddr = "127.0.0.1:54321"
					handler.ServeHTTP(rec, req)
					if rec.Code != http.StatusServiceUnavailable {
						t.Fatalf("status = %d body=%s, want 503 under %s", rec.Code, rec.Body.String(), state.name)
					}
					var envelope pkgapi.Envelope[any]
					if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
						t.Fatalf("decode body error = %v body=%s", err, rec.Body.String())
					}
					if envelope.Error == nil || envelope.Error.Code != pkgapi.ErrorCodeServiceUnavailable {
						t.Fatalf("error = %#v, want SERVICE_UNAVAILABLE", envelope.Error)
					}
				})
			}

			// GET is never classified as mutating; gate must not invent 503.
			if isMutatingHTTPMethod(http.MethodGet) {
				t.Fatal("GET classified as mutating")
			}
			// Version and config GET do not need storage; prove reads are not gated.
			for _, path := range []string{"/api/v1/version", "/api/v1/config"} {
				path := path
				t.Run("GET "+path, func(t *testing.T) {
					t.Parallel()
					rec := httptest.NewRecorder()
					req := httptest.NewRequest(http.MethodGet, path, nil)
					handler.ServeHTTP(rec, req)
					if rec.Code == http.StatusServiceUnavailable {
						t.Fatalf("GET %s status = 503 under %s, reads must remain available", path, state.name)
					}
				})
			}
		})
	}
}

// Contract (#580): full runtime under starting then degraded keeps GET status
// available while mutations return 503.
func TestNonMutatingHTTPCoverageReadsOnLiveRuntime(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	backupDir := filepath.Join(workingDir, "backups")
	cfg.Storage.BackupDir = &backupDir

	rt := looperdruntime.New(looperdruntime.Options{
		Config:        cfg,
		Logger:        nil,
		DeferRecovery: true,
	})
	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { rt.Stop("test cleanup") })

	handler := NewHandler(Context{Config: cfg, Runtime: rt})

	// Starting: mutations 503, status/healthz not 503.
	assertMutation503(t, handler, "starting")
	assertReadNot503(t, handler, "/api/v1/status")
	assertReadNot503(t, handler, "/api/v1/healthz")

	if err := rt.CompleteStartup(context.Background()); err != nil {
		t.Fatalf("CompleteStartup() error = %v", err)
	}
	if err := rt.MarkDegraded("test degrade"); err != nil {
		t.Fatalf("MarkDegraded() error = %v", err)
	}

	assertMutation503(t, handler, "degraded")
	assertReadNot503(t, handler, "/api/v1/status")
	assertReadNot503(t, handler, "/api/v1/healthz")
	assertReadNot503(t, handler, "/api/v1/loops")
}

// Contract (#580): ready admission does not invent 503 for mutations.
func TestNonMutatingHTTPCoverageReadyAllowsMutationPastGate(t *testing.T) {
	t.Parallel()

	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	handler := NewHandler(Context{
		Config:  cfg,
		Runtime: admissionGateRuntime{err: nil},
		Now:     func() time.Time { return time.Date(2026, time.July, 19, 12, 0, 0, 0, time.UTC) },
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/loops", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(rec, req)
	if rec.Code == http.StatusServiceUnavailable {
		t.Fatalf("POST while ready status = 503, want gate open (got body %s)", rec.Body.String())
	}
}

func assertMutation503(t *testing.T, handler http.Handler, state string) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/loops", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("POST /api/v1/loops under %s status = %d body=%s, want 503", state, rec.Code, rec.Body.String())
	}
}

func assertReadNot503(t *testing.T, handler http.Handler, path string) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	handler.ServeHTTP(rec, req)
	if rec.Code == http.StatusServiceUnavailable {
		t.Fatalf("GET %s status = 503, reads must remain available (body=%s)", path, rec.Body.String())
	}
}
