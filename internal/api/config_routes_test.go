package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	looperdruntime "github.com/nexu-io/looper/internal/runtime"
)

type configRouteRuntime struct {
	mu      sync.Mutex
	configs []config.Config
	calls   int
}

// configOverrideRuntime keeps legacy handler fixtures explicit: tests that
// intentionally alter a copied config after starting a runtime publish that
// copy as the runtime's request snapshot while retaining all embedded runtime
// services and optional control interfaces.
type configOverrideRuntime struct {
	*looperdruntime.Runtime
	config config.Config
}

func (r *configOverrideRuntime) Config() config.Config {
	return r.config
}

func runtimeWithConfig(r *looperdruntime.Runtime, cfg config.Config) RuntimeState {
	return &configOverrideRuntime{Runtime: r, config: cfg}
}

func (r *configRouteRuntime) Services() looperdruntime.Services {
	return looperdruntime.Services{}
}

func (r *configRouteRuntime) StartedAt() (time.Time, bool) {
	return time.Time{}, false
}

func (r *configRouteRuntime) Config() config.Config {
	r.mu.Lock()
	defer r.mu.Unlock()
	index := r.calls
	if index >= len(r.configs) {
		index = len(r.configs) - 1
	}
	r.calls++
	return r.configs[index]
}

func (r *configRouteRuntime) replaceConfig(cfg config.Config) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.configs = []config.Config{cfg}
}

func (r *configRouteRuntime) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func testConfigRouteConfig(t *testing.T) config.Config {
	t.Helper()
	cfg, err := config.DefaultConfig("")
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Server.AuthMode = config.AuthModeNone
	return cfg
}

func TestConfigPatchPassesRawValuesAndReturnsRefreshedProjection(t *testing.T) {
	initial := testConfigRouteConfig(t)
	initial.Scheduler.MaxConcurrentRuns = 3
	runtime := &configRouteRuntime{configs: []config.Config{initial}}
	appliedAt := time.Date(2026, time.July, 16, 10, 30, 0, 0, time.UTC)
	metadata := ConfigMetadata{ConfigPath: "/tmp/config.json", Format: "json", FilePresent: true, Revision: "sha256:test"}
	callbackCalls := 0

	handler := NewHandler(Context{
		Config:  initial,
		Runtime: runtime,
		ConfigMetadata: func() ConfigMetadata {
			return metadata
		},
		PatchConfig: func(_ context.Context, patch ConfigPatchRequest) error {
			callbackCalls++
			if got := string(patch.Set["scheduler.maxConcurrentRuns"]); got != "4" {
				t.Fatalf("raw set value = %q, want 4", got)
			}
			if got := string(patch.Set["agent.env.NEW_TOKEN"]); got != `"write-only-value"` {
				t.Fatalf("raw env value = %q, want quoted JSON string", got)
			}
			if len(patch.Unset) != 1 || patch.Unset[0] != "agent.env.OLD_TOKEN" {
				t.Fatalf("unset = %#v", patch.Unset)
			}
			next := initial
			next.Scheduler.MaxConcurrentRuns = 4
			next.Agent.Env = map[string]string{"NEW_TOKEN": "write-only-value"}
			runtime.replaceConfig(next)
			metadata.LastAttemptAt = &appliedAt
			metadata.LastAppliedAt = &appliedAt
			return nil
		},
	})

	body := `{"revision":"sha256:test","set":{"scheduler.maxConcurrentRuns":4,"agent.env.NEW_TOKEN":"write-only-value"},"unset":["agent.env.OLD_TOKEN"]}`
	recorder := httptest.NewRecorder()
	req := loopbackConfigPatchRequest(strings.NewReader(body))
	req.Header.Set("content-type", "application/json")
	handler.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	if callbackCalls != 1 {
		t.Fatalf("PatchConfig calls = %d, want 1", callbackCalls)
	}
	if got := runtime.callCount(); got != 2 {
		t.Fatalf("Runtime.Config() calls = %d, want initial and refreshed snapshots", got)
	}
	if strings.Contains(recorder.Body.String(), "write-only-value") {
		t.Fatalf("PATCH response exposed write-only value: %s", recorder.Body.String())
	}
	data := parseJSONMap(t, recorder.Body.Bytes())["data"].(map[string]any)
	assertEqual(t, data["scheduler"].(map[string]any)["maxConcurrentRuns"], float64(4))
	assertStringArray(t, data["agent"].(map[string]any)["envKeys"], []string{"NEW_TOKEN"})
	assertEqual(t, data["metadata"].(map[string]any)["lastAppliedAt"], appliedAt.Format(time.RFC3339))
}

func TestConfigPatchMapsTypedRequestErrors(t *testing.T) {
	cfg := testConfigRouteConfig(t)
	tests := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{
			name: "validation",
			err: &ConfigRequestError{
				Kind:    ConfigRequestErrorKindValidation,
				Message: "Configuration validation failed",
				Issues:  []ConfigPatchIssue{{Path: "scheduler.pollIntervalSeconds", Code: "too_small", Message: "must be at least 10"}},
			},
			status: http.StatusBadRequest,
			code:   "VALIDATION_FAILED",
		},
		{
			name: "conflict",
			err: fmt.Errorf("persist: %w", ConfigRequestError{
				Kind:    ConfigRequestErrorKindConflict,
				Message: "Configuration changed on disk",
				Issues:  []ConfigPatchIssue{{Path: "scheduler.maxConcurrentRuns", Code: "file_changed", Message: "reload and retry"}},
			}),
			status: http.StatusConflict,
			code:   "CONFIG_CONFLICT",
		},
		{
			name: "unsupported",
			err: ConfigRequestError{
				Kind:    ConfigRequestErrorKindUnsupported,
				Message: "Field is startup-only",
				Issues:  []ConfigPatchIssue{{Path: "server.port", Code: "startup_only", Message: "restart required"}},
			},
			status: http.StatusBadRequest,
			code:   "VALIDATION_FAILED",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := NewHandler(Context{
				Config: cfg,
				PatchConfig: func(context.Context, ConfigPatchRequest) error {
					return tt.err
				},
			})
			recorder := httptest.NewRecorder()
			req := loopbackConfigPatchRequest(strings.NewReader(`{"revision":"sha256:test","set":{"scheduler.maxConcurrentRuns":4}}`))
			handler.ServeHTTP(recorder, req)

			if recorder.Code != tt.status {
				t.Fatalf("status = %d, want %d; body=%s", recorder.Code, tt.status, recorder.Body.String())
			}
			if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
				t.Fatalf("Cache-Control = %q, want no-store", got)
			}
			errorResponse := parseJSONMap(t, recorder.Body.Bytes())["error"].(map[string]any)
			assertEqual(t, errorResponse["code"], tt.code)
			details := errorResponse["details"].(map[string]any)
			issue := details["issues"].([]any)[0].(map[string]any)
			requestError, _ := asConfigRequestError(tt.err)
			assertEqual(t, issue["path"], requestError.Issues[0].Path)
			assertEqual(t, issue["code"], requestError.Issues[0].Code)
			assertEqual(t, issue["message"], requestError.Issues[0].Message)
		})
	}
}

func TestConfigResponseNeverReturnsSecretValues(t *testing.T) {
	cfg := testConfigRouteConfig(t)
	localToken := "server-token-value"
	cfg.Server.LocalToken = &localToken
	cfg.Agent.Env = map[string]string{"OPENAI_API_KEY": "agent-secret-value"}
	cfg.Agent.Params = map[string]any{"apiKey": "agent-param-secret"}
	cfg.Daemon.Environment = map[string]string{"PRIVATE_TOKEN": "daemon-secret-value"}

	handler := NewHandler(Context{
		Config: cfg,
		ConfigMetadata: func() ConfigMetadata {
			return ConfigMetadata{Fields: map[string]ConfigFieldMetadata{
				"agent.env.OPENAI_API_KEY": {Source: "config-file", Editable: true, ApplyMode: "hot"},
			}}
		},
	})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/config", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	for _, secret := range []string{localToken, "agent-secret-value", "agent-param-secret", "daemon-secret-value"} {
		if strings.Contains(body, secret) {
			t.Fatalf("config response exposed secret %q: %s", secret, body)
		}
	}
	data := parseJSONMap(t, recorder.Body.Bytes())["data"].(map[string]any)
	if _, exists := data["server"].(map[string]any)["localTokenConfigured"]; exists {
		t.Fatal("localTokenConfigured must not be returned")
	}
	assertStringArray(t, data["agent"].(map[string]any)["envKeys"], []string{"OPENAI_API_KEY"})
	assertEqual(t, len(data["agent"].(map[string]any)["params"].(map[string]any)), 0)
	assertEqual(t, len(data["daemon"].(map[string]any)["environment"].(map[string]any)), 0)
}

func assertStringArray(t *testing.T, value any, want []string) {
	t.Helper()
	items, ok := value.([]any)
	if !ok {
		t.Fatalf("value = %#v, want string array", value)
	}
	if len(items) != len(want) {
		t.Fatalf("len(value) = %d, want %d: %#v", len(items), len(want), value)
	}
	for index, expected := range want {
		assertEqual(t, items[index], expected)
	}
}
