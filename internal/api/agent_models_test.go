package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/agent/modelcatalog"
	"github.com/nexu-io/looper/internal/config"
	pkgapi "github.com/nexu-io/looper/pkg/api"
)

func TestHandlerAgentModelsMissingVendor(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	h := NewHandler(Context{Config: cfg, Runtime: rt})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/agent/models", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	body := parseJSONMap(t, rec.Body.Bytes())
	assertEqual(t, body["ok"], false)
	errObj := body["error"].(map[string]any)
	assertEqual(t, errObj["code"], string(pkgapi.ErrorCodeValidationFailed))
}

func TestHandlerAgentModelsUnknownVendor(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	h := NewHandler(Context{Config: cfg, Runtime: rt})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/agent/models?vendor=not-a-vendor", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	body := parseJSONMap(t, rec.Body.Bytes())
	errObj := body["error"].(map[string]any)
	assertEqual(t, errObj["code"], string(pkgapi.ErrorCodeValidationFailed))
}

func TestHandlerAgentModelsClaudeCodeStaticUnsupported(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	h := NewHandler(Context{Config: cfg, Runtime: rt})
	h.modelCatalog = modelcatalog.NewService(modelcatalog.Options{
		Runner: modelCatalogRunnerFunc(func(context.Context, string, ...string) ([]byte, error) {
			t.Fatal("claude-code must not probe")
			return nil, nil
		}),
		LookPath: func(s string) (string, error) { return "/bin/" + s, nil },
		Now:      func() time.Time { return time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC) },
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/agent/models?vendor=claude-code", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	data := parseJSONMap(t, rec.Body.Bytes())["data"].(map[string]any)
	assertEqual(t, data["vendor"], "claude-code")
	sources := data["sources"].(map[string]any)
	assertEqual(t, sources["static"], true)
	assertEqual(t, sources["probe"], "unsupported")
	models := data["models"].([]any)
	if len(models) == 0 {
		t.Fatal("expected static models")
	}
	ids := map[string]bool{}
	for _, raw := range models {
		m := raw.(map[string]any)
		ids[m["id"].(string)] = true
		assertEqual(t, m["source"], "static")
	}
	for _, want := range []string{"sonnet", "opus", "haiku"} {
		if !ids[want] {
			t.Fatalf("missing static alias %q in %#v", want, ids)
		}
	}
}

func TestHandlerAgentModelsProbeFailureStill200(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	h := NewHandler(Context{Config: cfg, Runtime: rt})
	h.modelCatalog = modelcatalog.NewService(modelcatalog.Options{
		Runner: modelCatalogRunnerFunc(func(context.Context, string, ...string) ([]byte, error) {
			return nil, errors.New("codex: command failed")
		}),
		LookPath: func(s string) (string, error) { return "/usr/bin/" + s, nil },
		Now:      func() time.Time { return time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC) },
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/agent/models?vendor=codex", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	data := parseJSONMap(t, rec.Body.Bytes())["data"].(map[string]any)
	sources := data["sources"].(map[string]any)
	assertEqual(t, sources["static"], true)
	assertEqual(t, sources["probe"], "error")
	if sources["probeError"] == nil || sources["probeError"] == "" {
		t.Fatalf("expected probeError, got %#v", sources)
	}
	models := data["models"].([]any)
	if len(models) == 0 {
		t.Fatal("expected static models on probe error")
	}
	assertEqual(t, data["probedAt"], "2026-08-01T12:00:00Z")
}

func TestHandlerAgentModelsProbeOK(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	h := NewHandler(Context{Config: cfg, Runtime: rt})
	h.modelCatalog = modelcatalog.NewService(modelcatalog.Options{
		Runner: modelCatalogRunnerFunc(func(_ context.Context, name string, args ...string) ([]byte, error) {
			if name != "/usr/bin/opencode" {
				t.Fatalf("name = %q", name)
			}
			if len(args) != 1 || args[0] != "models" {
				t.Fatalf("args = %#v", args)
			}
			return []byte("openai/gpt-4.1\nanthropic/claude-sonnet-4-5\n"), nil
		}),
		LookPath: func(s string) (string, error) {
			if s == "opencode" {
				return "/usr/bin/opencode", nil
			}
			return s, nil
		},
		Now: func() time.Time { return time.Date(2026, 8, 1, 15, 0, 0, 0, time.UTC) },
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/agent/models?vendor=opencode", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	data := parseJSONMap(t, rec.Body.Bytes())["data"].(map[string]any)
	sources := data["sources"].(map[string]any)
	assertEqual(t, sources["probe"], "ok")
	assertEqual(t, data["probedAt"], "2026-08-01T15:00:00Z")

	// Ensure response JSON shape matches envelope + payload fields.
	var envelope struct {
		OK   bool `json:"ok"`
		Data struct {
			Vendor string `json:"vendor"`
			Models []struct {
				ID     string `json:"id"`
				Label  string `json:"label"`
				Source string `json:"source"`
			} `json:"models"`
			Sources struct {
				Static bool   `json:"static"`
				Probe  string `json:"probe"`
			} `json:"sources"`
			ProbedAt string `json:"probedAt"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !envelope.OK || envelope.Data.Vendor != "opencode" || len(envelope.Data.Models) == 0 {
		t.Fatalf("envelope = %#v", envelope)
	}
	foundProbeOnly := false
	for _, m := range envelope.Data.Models {
		if m.ID == "openai/gpt-4.1" && m.Source == "probe" {
			foundProbeOnly = true
		}
	}
	if !foundProbeOnly {
		t.Fatalf("expected probe-only model in %#v", envelope.Data.Models)
	}
}

func TestHandlerAgentModelsMethodNotAllowed(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	h := NewHandler(Context{Config: cfg, Runtime: rt})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/models?vendor=codex", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestHandlerAgentModelsSameVendorUsesParamsCommand(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	vendor := config.AgentVendorCodex
	cfg.Agent.Vendor = &vendor
	cfg.Agent.Params = map[string]any{"command": "/opt/custom-codex"}
	h := NewHandler(Context{Config: cfg, Runtime: rt})

	var saw string
	h.modelCatalog = modelcatalog.NewService(modelcatalog.Options{
		Runner: modelCatalogRunnerFunc(func(_ context.Context, name string, args ...string) ([]byte, error) {
			saw = name
			if len(args) < 1 || args[0] != "debug" {
				t.Fatalf("args = %#v, want codex debug models", args)
			}
			return []byte(`{"models":[{"id":"gpt-5.4","name":"GPT-5.4","visibility":"list"}]}`), nil
		}),
		LookPath: func(s string) (string, error) { return s, nil },
		Now:      func() time.Time { return time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC) },
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/agent/models?vendor=codex", nil)
	rec := httptest.NewRecorder()
	// serveHTTP (not ServeHTTP): avoid Runtime.Config() overwriting the mutated
	// fixture agent.params used for spawn-equivalent command resolution.
	h.serveHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if saw != "/opt/custom-codex" {
		t.Fatalf("probed command = %q, want /opt/custom-codex (same-vendor wrapper kept)", saw)
	}
}

func TestHandlerAgentModelsCrossVendorStripsParamsCommand(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	global := config.AgentVendorCodex
	cfg.Agent.Vendor = &global
	cfg.Agent.Params = map[string]any{"command": "/opt/custom-codex-wrapper"}
	h := NewHandler(Context{Config: cfg, Runtime: rt})

	var saw string
	h.modelCatalog = modelcatalog.NewService(modelcatalog.Options{
		Runner: modelCatalogRunnerFunc(func(_ context.Context, name string, args ...string) ([]byte, error) {
			saw = name
			if len(args) != 1 || args[0] != "models" {
				t.Fatalf("args = %#v, want opencode models", args)
			}
			return []byte("openai/gpt-4.1\n"), nil
		}),
		LookPath: func(s string) (string, error) {
			if s == "opencode" {
				return "/usr/bin/opencode", nil
			}
			// If wrapper leaked through, LookPath would return it as-is below.
			return s, nil
		},
		Now: func() time.Time { return time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC) },
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/agent/models?vendor=opencode", nil)
	rec := httptest.NewRecorder()
	h.serveHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if saw == "/opt/custom-codex-wrapper" {
		t.Fatal("cross-vendor probe used global codex wrapper command")
	}
	if saw != "/usr/bin/opencode" {
		t.Fatalf("probed command = %q, want /usr/bin/opencode (default after strip)", saw)
	}
}

type modelCatalogRunnerFunc func(ctx context.Context, name string, args ...string) ([]byte, error)

func (f modelCatalogRunnerFunc) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return f(ctx, name, args...)
}
