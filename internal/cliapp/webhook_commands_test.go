package cliapp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	pkgapi "github.com/nexu-io/looper/pkg/api"
)

func TestWebhookEnablePersistsConfigAndWarnsWithoutChangingScheduler(t *testing.T) {
	t.Parallel()

	configPath := writeEditableCLIConfigWithPayload(t, map[string]any{
		"server":    map[string]any{"host": "0.0.0.0"},
		"scheduler": map[string]any{"pollIntervalSeconds": 42},
		"notifications": map[string]any{
			"osascript": map[string]any{"enabled": false},
		},
	})
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	missingLookPath := func(string) (string, error) { return "", os.ErrNotExist }
	app := New(Deps{Stdout: stdout, Stderr: stderr, LookPath: missingLookPath})
	exitCode := app.Run(context.Background(), []string{"webhook", "enable", "--config", configPath})
	if exitCode != 0 {
		t.Fatalf("Run(webhook enable) exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Restart looperd") || !strings.Contains(stdout.String(), "Warning:") {
		t.Fatalf("stdout = %q, want restart instruction and warnings", stdout.String())
	}
	var updated map[string]any
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile(config) error = %v", err)
	}
	if err := json.Unmarshal(raw, &updated); err != nil {
		t.Fatalf("Unmarshal(config) error = %v", err)
	}
	webhook := updated["webhook"].(map[string]any)
	if got := webhook["enabled"]; got != true {
		t.Fatalf("webhook.enabled = %v, want true", got)
	}
	if got := int(webhook["fallbackPollIntervalSeconds"].(float64)); got != 300 {
		t.Fatalf("webhook.fallbackPollIntervalSeconds = %d, want 300", got)
	}
	scheduler := updated["scheduler"].(map[string]any)
	if got := int(scheduler["pollIntervalSeconds"].(float64)); got != 42 {
		t.Fatalf("scheduler.pollIntervalSeconds = %d, want 42", got)
	}
}

func TestWebhookEnableWarnsWhenGHWebhookCommandIsUnavailable(t *testing.T) {
	t.Parallel()

	configPath := writeEditableCLIConfigWithPayload(t, map[string]any{
		"notifications": map[string]any{
			"osascript": map[string]any{"enabled": false},
		},
	})
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := New(Deps{
		Stdout: stdout,
		Stderr: stderr,
		LookPath: func(command string) (string, error) {
			if command == "gh" {
				return "/usr/bin/gh", nil
			}
			return command, nil
		},
		RunCommand: func(ctx context.Context, command string, args []string, timeout time.Duration) (commandExecutionResult, error) {
			if command != "/usr/bin/gh" || strings.Join(args, " ") != "webhook forward --help" {
				t.Fatalf("RunCommand(%q, %q), want gh webhook forward --help", command, strings.Join(args, " "))
			}
			return commandExecutionResult{Stderr: "unknown command \"webhook\" for \"gh\"", ExitCode: 1}, nil
		},
	})

	exitCode := app.Run(context.Background(), []string{"webhook", "enable", "--config", configPath})
	if exitCode != 0 {
		t.Fatalf("Run(webhook enable) exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "gh webhook command is unavailable") || !strings.Contains(stdout.String(), "--install-gh-webhook") {
		t.Fatalf("stdout = %q, want gh webhook install warning", stdout.String())
	}
}

func TestWebhookEnableCanInstallGHWebhookExtension(t *testing.T) {
	t.Parallel()

	configPath := writeEditableCLIConfigWithPayload(t, map[string]any{
		"notifications": map[string]any{
			"osascript": map[string]any{"enabled": false},
		},
	})
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	commands := []string{}
	app := New(Deps{
		Stdout: stdout,
		Stderr: stderr,
		LookPath: func(command string) (string, error) {
			if command == "gh" {
				return "/usr/bin/gh", nil
			}
			return command, nil
		},
		RunCommand: func(ctx context.Context, command string, args []string, timeout time.Duration) (commandExecutionResult, error) {
			commands = append(commands, command+" "+strings.Join(args, " "))
			switch len(commands) {
			case 1:
				return commandExecutionResult{Stderr: "unknown command \"webhook\" for \"gh\"", ExitCode: 1}, nil
			case 2:
				return commandExecutionResult{ExitCode: 0}, nil
			case 3:
				return commandExecutionResult{Stdout: "Forward GitHub webhooks", ExitCode: 0}, nil
			default:
				t.Fatalf("unexpected RunCommand call %d: %s %q", len(commands), command, args)
				return commandExecutionResult{}, nil
			}
		},
	})

	exitCode := app.Run(context.Background(), []string{"webhook", "enable", "--install-gh-webhook", "--config", configPath})
	if exitCode != 0 {
		t.Fatalf("Run(webhook enable --install-gh-webhook) exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	wantCommands := []string{
		"/usr/bin/gh webhook forward --help",
		"/usr/bin/gh extension install cli/gh-webhook",
		"/usr/bin/gh webhook forward --help",
	}
	if strings.Join(commands, "\n") != strings.Join(wantCommands, "\n") {
		t.Fatalf("commands = %q, want %q", commands, wantCommands)
	}
	if !strings.Contains(stdout.String(), "Installed GitHub CLI webhook extension cli/gh-webhook") {
		t.Fatalf("stdout = %q, want install confirmation", stdout.String())
	}
}

func TestWebhookDisablePersistsDisabledState(t *testing.T) {
	t.Parallel()

	configPath := writeEditableCLIConfigWithPayload(t, map[string]any{
		"webhook": map[string]any{"enabled": true, "fallbackPollIntervalSeconds": 300},
		"notifications": map[string]any{
			"osascript": map[string]any{"enabled": false},
		},
	})
	exitCode, stdout, stderr := runApp(t, "webhook", "disable", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run(webhook disable) exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
	if !strings.Contains(stdout, "Disabled webhook mode") {
		t.Fatalf("stdout = %q, want disable confirmation", stdout)
	}
	var updated map[string]any
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile(config) error = %v", err)
	}
	if err := json.Unmarshal(raw, &updated); err != nil {
		t.Fatalf("Unmarshal(config) error = %v", err)
	}
	if got := updated["webhook"].(map[string]any)["enabled"]; got != false {
		t.Fatalf("webhook.enabled = %v, want false", got)
	}
}

func TestWebhookStatusShowsConfigIntentWithoutDaemonRuntime(t *testing.T) {
	t.Parallel()

	configPath := writeEditableCLIConfigWithPayload(t, map[string]any{
		"webhook": map[string]any{"enabled": true, "fallbackPollIntervalSeconds": 300},
		"server":  map[string]any{"baseUrl": "http://127.0.0.1:1", "authMode": "none"},
		"notifications": map[string]any{
			"osascript": map[string]any{"enabled": false},
		},
	})
	exitCode, stdout, stderr := runApp(t, "webhook", "status", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run(webhook status) exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
	if !strings.Contains(stdout, "Webhook config") || !strings.Contains(stdout, "available : no") {
		t.Fatalf("stdout = %q, want config section and unavailable runtime", stdout)
	}
}

func TestWebhookStatusTreatsMissingStatusRouteAsRuntimeUnavailable(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/webhook/status" {
			t.Fatalf("request path = %q, want %q", r.URL.Path, "/api/v1/webhook/status")
		}
		w.WriteHeader(http.StatusNotFound)
		writeEnvelope(t, w, pkgapi.Failure("req_missing", pkgapi.ErrorCodeRouteNotFound, "route not found", nil))
	}))
	defer server.Close()

	configPath := writeEditableCLIConfigWithPayload(t, map[string]any{
		"webhook": map[string]any{"enabled": true, "fallbackPollIntervalSeconds": 300},
		"server":  map[string]any{"baseUrl": server.URL, "authMode": "none"},
		"notifications": map[string]any{
			"osascript": map[string]any{"enabled": false},
		},
	})
	exitCode, stdout, stderr := runApp(t, "webhook", "status", "--json", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run(webhook status --json) exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
	assertJSONContains(t, stdout, "runtimeAvailable", false)
	assertJSONContains(t, stdout, "restartRequired", false)
	if strings.Contains(stdout, "\"runtime\"") {
		t.Fatalf("stdout = %q, want config-only output when webhook status route is unavailable", stdout)
	}
}

func TestWebhookStatusRestartRequiredTracksConfigRuntimeDrift(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/webhook/status" {
			t.Fatalf("request path = %q, want %q", r.URL.Path, "/api/v1/webhook/status")
		}
		writeEnvelope(t, w, pkgapi.Success("req_webhook", map[string]any{
			"enabled":                     false,
			"listenerPath":                "/webhook/forward",
			"endpointUrl":                 "http://127.0.0.1:17310/webhook/forward",
			"fallbackPollIntervalSeconds": 300,
			"degraded":                    false,
			"degradedReasons":             []string{},
			"queue":                       map[string]any{"pending": 0, "capacity": 8, "activeWorkers": 0},
			"counters":                    map[string]any{"deliveriesReceived": 0, "coalesced": 0, "dropped": 0, "queued": 0, "processed": 0, "failed": 0},
			"recentOutcomes":              []map[string]any{},
			"forwarders":                  []map[string]any{},
		}))
	}))
	defer server.Close()

	configPath := writeEditableCLIConfigWithPayload(t, map[string]any{
		"webhook": map[string]any{"enabled": true, "fallbackPollIntervalSeconds": 300},
		"server":  map[string]any{"baseUrl": server.URL, "authMode": "none"},
		"notifications": map[string]any{
			"osascript": map[string]any{"enabled": false},
		},
	})
	exitCode, stdout, stderr := runApp(t, "webhook", "status", "--json", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run(webhook status --json) exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
	assertJSONContains(t, stdout, "runtimeAvailable", true)
	assertJSONContains(t, stdout, "restartRequired", true)
}

func TestWebhookStatusRestartRequiredFalseWhenConfigMatchesRuntime(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/webhook/status" {
			t.Fatalf("request path = %q, want %q", r.URL.Path, "/api/v1/webhook/status")
		}
		writeEnvelope(t, w, pkgapi.Success("req_webhook", map[string]any{
			"enabled":                     true,
			"listenerPath":                "/webhook/forward",
			"endpointUrl":                 "http://127.0.0.1:17310/webhook/forward",
			"fallbackPollIntervalSeconds": 300,
			"degraded":                    false,
			"degradedReasons":             []string{},
			"queue":                       map[string]any{"pending": 0, "capacity": 8, "activeWorkers": 0},
			"counters":                    map[string]any{"deliveriesReceived": 0, "coalesced": 0, "dropped": 0, "queued": 0, "processed": 0, "failed": 0},
			"recentOutcomes":              []map[string]any{},
			"forwarders":                  []map[string]any{},
		}))
	}))
	defer server.Close()

	configPath := writeEditableCLIConfigWithPayload(t, map[string]any{
		"webhook": map[string]any{"enabled": true, "fallbackPollIntervalSeconds": 300},
		"server":  map[string]any{"baseUrl": server.URL, "authMode": "none"},
		"notifications": map[string]any{
			"osascript": map[string]any{"enabled": false},
		},
	})
	exitCode, stdout, stderr := runApp(t, "webhook", "status", "--json", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run(webhook status --json) exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
	assertJSONContains(t, stdout, "restartRequired", false)
}

func TestWebhookStatusVerboseShowsRuntimeDetails(t *testing.T) {
	t.Parallel()

	pid := 4242
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/webhook/status" {
			t.Fatalf("request path = %q, want %q", r.URL.Path, "/api/v1/webhook/status")
		}
		writeEnvelope(t, w, pkgapi.Success("req_webhook", map[string]any{
			"enabled":                     true,
			"listenerPath":                "/webhook/forward",
			"endpointUrl":                 "http://127.0.0.1:17310/webhook/forward",
			"fallbackPollIntervalSeconds": 300,
			"degraded":                    true,
			"degradedReasons":             []string{"gh missing"},
			"queue":                       map[string]any{"pending": 1, "capacity": 8, "activeWorkers": 0},
			"counters":                    map[string]any{"deliveriesReceived": 2, "coalesced": 0, "dropped": 0, "queued": 1, "processed": 0, "failed": 0},
			"recentOutcomes":              []map[string]any{{"at": "2026-04-20T10:00:00.000Z", "outcome": "degraded", "message": "gh missing"}},
			"forwarders":                  []map[string]any{{"repo": "acme/looper", "running": true, "pid": pid, "restartCount": 1, "lastError": "gh missing", "stdoutTail": []string{"line1"}, "stderrTail": []string{"line2"}}},
		}))
	}))
	defer server.Close()

	configPath := writeEditableCLIConfigWithPayload(t, map[string]any{
		"webhook": map[string]any{"enabled": true, "fallbackPollIntervalSeconds": 300},
		"server":  map[string]any{"baseUrl": server.URL, "authMode": "none"},
		"notifications": map[string]any{
			"osascript": map[string]any{"enabled": false},
		},
	})
	exitCode, stdout, stderr := runApp(t, "webhook", "status", "--verbose", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run(webhook status --verbose) exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
	for _, needle := range []string{"Webhook runtime", "Forwarder acme/looper", "stdoutTail", "line1", "line2", "4242"} {
		if !strings.Contains(stdout, needle) {
			t.Fatalf("stdout = %q, want to contain %q", stdout, needle)
		}
	}
	if strings.Contains(stdout, "0x") {
		t.Fatalf("stdout = %q, want pid value instead of pointer address", stdout)
	}
}
