package cliapp

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nexu-io/looper/internal/config"
	pkgapi "github.com/nexu-io/looper/pkg/api"
)

func TestWebhookStatusRestartRequiredTracksTunnelEndpointDrift(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/webhook/status" {
			t.Fatalf("request path = %q, want %q", r.URL.Path, "/api/v1/webhook/status")
		}
		writeEnvelope(t, w, pkgapi.Success("req_webhook", map[string]any{
			"enabled":                     true,
			"mode":                        "tunnel",
			"listenerPath":                "/webhook/forward",
			"endpointUrl":                 "http://127.0.0.1:17310/webhook/forward",
			"tunnelListenerUrl":           "http://127.0.0.1:8443",
			"tunnelPublicBaseUrl":         "https://runtime.example.com/base",
			"fallbackPollIntervalSeconds": 300,
			"degraded":                    false,
			"degradedReasons":             []string{},
			"queue":                       map[string]any{"pending": 0, "capacity": 8, "activeWorkers": 0},
			"counters":                    map[string]any{"deliveriesReceived": 0, "coalesced": 0, "dropped": 0, "queued": 0, "processed": 0, "failed": 0},
			"recentOutcomes":              []map[string]any{},
			"forwarders":                  []map[string]any{},
			"tunnelHooks":                 []map[string]any{},
		}))
	}))
	defer server.Close()

	configPath := writeEditableCLIConfigWithPayload(t, map[string]any{
		"webhook": map[string]any{"enabled": true, "mode": "tunnel", "listenPort": 9443, "publicBaseUrl": "https://config.example.com/base", "fallbackPollIntervalSeconds": 300},
		"server":  map[string]any{"baseUrl": server.URL, "authMode": "none"},
		"notifications": map[string]any{
			"osascript": map[string]any{"enabled": false},
		},
	})
	exitCode, stdout, stderr := runApp(t, "webhook", "status", "--json", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run(webhook status --json) exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
	assertJSONContains(t, stdout, "restartRequired", true)
}

func TestWebhookStatusRestartRequiredTracksTunnelEndpointDriftForMixedModeProjects(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/webhook/status" {
			t.Fatalf("request path = %q, want %q", r.URL.Path, "/api/v1/webhook/status")
		}
		writeEnvelope(t, w, pkgapi.Success("req_webhook", map[string]any{
			"enabled":                     true,
			"mode":                        "gh-forward",
			"listenerPath":                "/webhook/forward",
			"endpointUrl":                 "http://127.0.0.1:17310/webhook/forward",
			"tunnelListenerUrl":           "http://127.0.0.1:8443",
			"tunnelPublicBaseUrl":         "https://runtime.example.com/base",
			"fallbackPollIntervalSeconds": 300,
			"degraded":                    false,
			"degradedReasons":             []string{},
			"queue":                       map[string]any{"pending": 0, "capacity": 8, "activeWorkers": 0},
			"counters":                    map[string]any{"deliveriesReceived": 0, "coalesced": 0, "dropped": 0, "queued": 0, "processed": 0, "failed": 0},
			"recentOutcomes":              []map[string]any{},
			"forwarders":                  []map[string]any{},
			"tunnelHooks":                 []map[string]any{},
		}))
	}))
	defer server.Close()

	configPath := writeEditableCLIConfigWithPayload(t, map[string]any{
		"webhook":  map[string]any{"enabled": true, "mode": "gh-forward", "listenPort": 9443, "publicBaseUrl": "https://config.example.com/base", "fallbackPollIntervalSeconds": 300},
		"projects": []map[string]any{{"id": "proj-1", "name": "Looper", "repoPath": t.TempDir(), "webhook": map[string]any{"mode": "tunnel"}}},
		"server":   map[string]any{"baseUrl": server.URL, "authMode": "none"},
		"notifications": map[string]any{
			"osascript": map[string]any{"enabled": false},
		},
	})
	exitCode, stdout, stderr := runApp(t, "webhook", "status", "--json", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run(webhook status --json) exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
	assertJSONContains(t, stdout, "restartRequired", true)
}

func TestWebhookStatusRestartRequiredTracksTunnelProjectSetDrift(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/webhook/status" {
			t.Fatalf("request path = %q, want %q", r.URL.Path, "/api/v1/webhook/status")
		}
		writeEnvelope(t, w, pkgapi.Success("req_webhook", map[string]any{
			"enabled":                     true,
			"mode":                        "gh-forward",
			"configuredTunnelProjectIds":  []string{},
			"listenerPath":                "/webhook/forward",
			"endpointUrl":                 "http://127.0.0.1:17310/webhook/forward",
			"tunnelListenerUrl":           "http://127.0.0.1:9443",
			"tunnelPublicBaseUrl":         "https://config.example.com/base",
			"fallbackPollIntervalSeconds": 300,
			"degraded":                    false,
			"degradedReasons":             []string{},
			"queue":                       map[string]any{"pending": 0, "capacity": 8, "activeWorkers": 0},
			"counters":                    map[string]any{"deliveriesReceived": 0, "coalesced": 0, "dropped": 0, "queued": 0, "processed": 0, "failed": 0},
			"recentOutcomes":              []map[string]any{},
			"forwarders":                  []map[string]any{},
			"tunnelHooks":                 []map[string]any{},
		}))
	}))
	defer server.Close()

	configPath := writeEditableCLIConfigWithPayload(t, map[string]any{
		"webhook":  map[string]any{"enabled": true, "mode": "gh-forward", "listenPort": 9443, "publicBaseUrl": "https://config.example.com/base", "fallbackPollIntervalSeconds": 300},
		"projects": []map[string]any{{"id": "proj-1", "name": "Looper", "repoPath": t.TempDir(), "webhook": map[string]any{"mode": "tunnel"}}},
		"server":   map[string]any{"baseUrl": server.URL, "authMode": "none"},
		"notifications": map[string]any{
			"osascript": map[string]any{"enabled": false},
		},
	})
	exitCode, stdout, stderr := runApp(t, "webhook", "status", "--json", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run(webhook status --json) exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
	assertJSONContains(t, stdout, "restartRequired", true)
}

func TestWebhookWarningsIncludesLoopbackWarningForMixedModeGHForwardOverrides(t *testing.T) {
	t.Parallel()

	warnings := webhookWarnings(config.Config{
		Server:  config.ServerConfig{Host: "0.0.0.0"},
		Webhook: config.WebhookConfig{Mode: config.WebhookModeTunnel},
		Projects: []config.ProjectRefConfig{{
			ID:       "proj-1",
			Name:     "Looper",
			RepoPath: t.TempDir(),
			Webhook:  config.ProjectWebhookConfig{Mode: config.WebhookModeGHForward},
		}},
	})

	if len(warnings) == 0 || warnings[0] != "server.host is not loopback; looperd will degrade webhook mode to poll fallback" {
		t.Fatalf("webhookWarnings() = %v, want loopback warning for mixed-mode gh-forward overrides", warnings)
	}
}

func TestWebhookWarningsSkipsLoopbackWarningForPureTunnelConfigs(t *testing.T) {
	t.Parallel()

	warnings := webhookWarnings(config.Config{
		Server:  config.ServerConfig{Host: "0.0.0.0"},
		Webhook: config.WebhookConfig{Mode: config.WebhookModeTunnel},
		Projects: []config.ProjectRefConfig{{
			ID:       "proj-1",
			Name:     "Looper",
			RepoPath: t.TempDir(),
		}},
	})

	for _, warning := range warnings {
		if warning == "server.host is not loopback; looperd will degrade webhook mode to poll fallback" {
			t.Fatalf("webhookWarnings() = %v, want no loopback warning when every project uses tunnel", warnings)
		}
	}
}
