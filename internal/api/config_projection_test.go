package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	looperdruntime "github.com/nexu-io/looper/internal/runtime"
	"github.com/nexu-io/looper/internal/webhookforward"
)

func TestConfigGetRedactsSecretsAndIncludesMetadata(t *testing.T) {
	cfg := testConfigRouteConfig(t)
	localToken := "local-token-secret"
	cfg.Server.LocalToken = &localToken
	cfg.Agent.Env = map[string]string{"Z_TOKEN": "agent-env-secret", "A_TOKEN": "another-agent-secret"}
	cfg.Agent.Params = map[string]any{"apiKey": "agent-param-secret", "nested": map[string]any{"token": "nested-param-secret"}}
	cfg.Daemon.Environment = map[string]string{"SECOND": "daemon-env-secret", "FIRST": "another-daemon-secret"}

	lastAttempt := time.Date(2026, time.July, 16, 10, 0, 0, 0, time.UTC)
	lastApplied := lastAttempt.Add(-time.Minute)
	lastError := "invalid scheduler value"
	metadata := ConfigMetadata{
		ConfigPath: "/tmp/looper/config.json", Format: "json", FilePresent: true, Revision: "sha256:test",
		LastAttemptAt: &lastAttempt, LastAppliedAt: &lastApplied, LastError: &lastError,
		RejectedPaths: []string{"server.port"},
		Fields: map[string]ConfigFieldMetadata{
			"scheduler.pollIntervalSeconds": {Source: "config-file", Editable: true, ApplyMode: "hot"},
		},
	}

	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/config", nil)
	NewHandler(Context{Config: cfg, ConfigMetadata: func() ConfigMetadata { return metadata }}).ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	for _, secret := range []string{localToken, "agent-env-secret", "another-agent-secret", "agent-param-secret", "nested-param-secret", "daemon-env-secret", "another-daemon-secret"} {
		if strings.Contains(recorder.Body.String(), secret) {
			t.Fatalf("config response exposed secret %q: %s", secret, recorder.Body.String())
		}
	}

	data := parseJSONMap(t, recorder.Body.Bytes())["data"].(map[string]any)
	server := data["server"].(map[string]any)
	if _, exists := server["localTokenConfigured"]; exists {
		t.Fatalf("server.localTokenConfigured must be omitted: %#v", server)
	}
	if _, exists := server["localToken"]; exists {
		t.Fatalf("server.localToken must be omitted: %#v", server)
	}
	agent := data["agent"].(map[string]any)
	assertStringArray(t, agent["envKeys"], []string{"A_TOKEN", "Z_TOKEN"})
	if len(agent["env"].(map[string]any)) != 0 || len(agent["params"].(map[string]any)) != 0 {
		t.Fatalf("agent secret fields must be redacted to empty objects: %#v", agent)
	}
	daemon := data["daemon"].(map[string]any)
	if len(daemon["environment"].(map[string]any)) != 0 {
		t.Fatalf("daemon.environment must be redacted: %#v", daemon)
	}

	metadataResponse := data["metadata"].(map[string]any)
	assertEqual(t, metadataResponse["configPath"], metadata.ConfigPath)
	assertEqual(t, metadataResponse["format"], metadata.Format)
	assertEqual(t, metadataResponse["filePresent"], true)
	assertEqual(t, metadataResponse["revision"], "sha256:test")
	assertEqual(t, metadataResponse["lastAttemptAt"], lastAttempt.Format(time.RFC3339))
	assertEqual(t, metadataResponse["lastAppliedAt"], lastApplied.Format(time.RFC3339))
	assertEqual(t, metadataResponse["lastError"], lastError)
	assertStringArray(t, metadataResponse["rejectedPaths"], []string{"server.port"})
	field := metadataResponse["fields"].(map[string]any)["scheduler.pollIntervalSeconds"].(map[string]any)
	assertEqual(t, field["source"], "config-file")
	assertEqual(t, field["editable"], true)
	assertEqual(t, field["applyMode"], "hot")
}

func TestConfigGetUsesOneRuntimeSnapshotPerRequest(t *testing.T) {
	first := testConfigRouteConfig(t)
	first.Server.Port = 18001
	second := first
	second.Server.Port = 18002
	runtime := &configRouteRuntime{configs: []config.Config{first, second}}
	recorder := httptest.NewRecorder()
	NewHandler(Context{Runtime: runtime}).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/config", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	if got := runtime.callCount(); got != 1 {
		t.Fatalf("Runtime.Config() calls = %d, want 1", got)
	}
	data := parseJSONMap(t, recorder.Body.Bytes())["data"].(map[string]any)
	assertEqual(t, data["server"].(map[string]any)["port"], float64(first.Server.Port))
}

type configRouteForwarderRuntime struct {
	*configRouteRuntime
	mu        sync.Mutex
	forwarder looperdruntime.WebhookForwarder
}

func (r *configRouteForwarderRuntime) WebhookForwarder() looperdruntime.WebhookForwarder {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.forwarder
}

type recordingConfigRouteForwarder struct {
	mu    sync.Mutex
	calls int
}

func (f *recordingConfigRouteForwarder) Forward(context.Context, webhookforward.DeliveryRequest) (webhookforward.ForwardResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return webhookforward.ForwardResult{Status: "accepted", WorkItems: 1}, nil
}

func (f *recordingConfigRouteForwarder) Stats() webhookforward.Stats { return webhookforward.Stats{} }
func (f *recordingConfigRouteForwarder) CancelExecute()              {}
func (f *recordingConfigRouteForwarder) Close()                      {}
func (f *recordingConfigRouteForwarder) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func TestWebhookForwardUsesCurrentRuntimeForwarder(t *testing.T) {
	cfg := testConfigRouteConfig(t)
	cfg.Webhook.Enabled = true
	staleForwarder := &recordingConfigRouteForwarder{}
	currentForwarder := &recordingConfigRouteForwarder{}
	runtime := &configRouteForwarderRuntime{
		configRouteRuntime: &configRouteRuntime{configs: []config.Config{cfg}},
		forwarder:          currentForwarder,
	}
	handler := NewHandler(Context{Config: cfg, Runtime: runtime, WebhookForwarder: staleForwarder})
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/webhook/forward", strings.NewReader(`{}`))
	req.RemoteAddr = "127.0.0.1:17310"
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", recorder.Code, recorder.Body.String())
	}
	if got := staleForwarder.callCount(); got != 0 {
		t.Fatalf("constructor forwarder calls = %d, want 0", got)
	}
	if got := currentForwarder.callCount(); got != 1 {
		t.Fatalf("runtime forwarder calls = %d, want 1", got)
	}
}
