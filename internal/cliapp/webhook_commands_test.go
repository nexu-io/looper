package cliapp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/storage"
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
		"tools": map[string]any{"ghPath": "/usr/bin/gh"},
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
		"tools": map[string]any{"ghPath": "/usr/bin/gh"},
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

func TestWebhookStatusShowsCleanupHintWhenDegraded(t *testing.T) {
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
			"degraded":                    true,
			"degradedReasons":             []string{"forwarder for acme/looper exited: exit status 1"},
			"queue":                       map[string]any{"pending": 0, "capacity": 8, "activeWorkers": 0},
			"counters":                    map[string]any{"deliveriesReceived": 0, "coalesced": 0, "dropped": 0, "queued": 0, "processed": 0, "failed": 0},
			"recentOutcomes":              []map[string]any{},
			"forwarders":                  []map[string]any{{"repo": "acme/looper", "running": false, "restartCount": 2, "lastError": "exit status 1"}},
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
	exitCode, stdout, stderr := runApp(t, "webhook", "status", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run(webhook status) exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
	for _, needle := range []string{"Cleanup hint", "looper webhook cleanup acme/looper", "--confirm"} {
		if !strings.Contains(stdout, needle) {
			t.Fatalf("stdout = %q, want to contain %q", stdout, needle)
		}
	}
}

func TestWebhookCleanupDryRunListsMatchingCLIHooks(t *testing.T) {
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
			return commandExecutionResult{Stdout: `[
				[
					{"id":101,"name":"cli","type":"Repository","active":true,"events":["pull_request","issue_comment"],"config":{"url":"https://webhook-forwarder.github.com/hook"}}
				],
				[
					{"id":202,"name":"web","type":"Repository","active":true,"events":["push"],"config":{"url":"https://example.com/webhook"}}
				]
			]`, ExitCode: 0}, nil
		},
	})

	exitCode := app.Run(context.Background(), []string{"webhook", "cleanup", "acme/looper", "--config", configPath})
	if exitCode != 0 {
		t.Fatalf("Run(webhook cleanup) exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if len(commands) != 1 || !strings.HasSuffix(commands[0], " api --paginate --slurp repos/acme/looper/hooks") {
		t.Fatalf("commands = %q, want a single paginated+slurped gh api list call", commands)
	}
	for _, needle := range []string{"Found 1 GitHub CLI webhook hook(s)", "id=101", "Dry run only.", "looper webhook cleanup acme/looper --confirm"} {
		if !strings.Contains(stdout.String(), needle) {
			t.Fatalf("stdout = %q, want to contain %q", stdout.String(), needle)
		}
	}
}

func TestWebhookCleanupDryRunAcceptsHostQualifiedRepo(t *testing.T) {
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
			return commandExecutionResult{Stdout: `[[{"id":101,"name":"cli","type":"Repository","active":true,"events":["pull_request"],"config":{"url":"https://webhook-forwarder.github.com/hook"}}]]`, ExitCode: 0}, nil
		},
	})

	exitCode := app.Run(context.Background(), []string{"webhook", "cleanup", " github.example.com/acme/looper ", "--config", configPath})
	if exitCode != 0 {
		t.Fatalf("Run(webhook cleanup host-qualified) exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if len(commands) != 1 || !strings.HasSuffix(commands[0], " api --paginate --slurp repos/acme/looper/hooks --hostname github.example.com") {
		t.Fatalf("commands = %q, want a paginated+slurped gh api list call using owner/repo plus --hostname", commands)
	}
	if !strings.Contains(stdout.String(), "looper webhook cleanup github.example.com/acme/looper --confirm") {
		t.Fatalf("stdout = %q, want host-qualified cleanup rerun hint", stdout.String())
	}
}

func TestWebhookCleanupConfirmUsesHostnameForHostQualifiedRepo(t *testing.T) {
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
				return commandExecutionResult{Stdout: `[[{"id":101,"name":"cli","type":"Repository","active":true,"events":["push","pull_request"],"config":{"url":"https://webhook-forwarder.github.com/hook"}}]]`, ExitCode: 0}, nil
			case 2:
				return commandExecutionResult{ExitCode: 0}, nil
			default:
				t.Fatalf("unexpected RunCommand call %d: %s %q", len(commands), command, args)
				return commandExecutionResult{}, nil
			}
		},
	})

	exitCode := app.Run(context.Background(), []string{"webhook", "cleanup", "github.example.com/acme/looper", "--confirm", "--config", configPath})
	if exitCode != 0 {
		t.Fatalf("Run(webhook cleanup --confirm host-qualified) exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if len(commands) != 2 || !strings.HasSuffix(commands[0], " api --paginate --slurp repos/acme/looper/hooks --hostname github.example.com") || !strings.HasSuffix(commands[1], " api -X DELETE repos/acme/looper/hooks/101 --hostname github.example.com") {
		t.Fatalf("commands = %q, want host-qualified cleanup to split owner/repo from --hostname for list and delete", commands)
	}
	if !strings.Contains(stdout.String(), "Deleted 1 GitHub CLI webhook hook(s) for github.example.com/acme/looper.") {
		t.Fatalf("stdout = %q, want delete confirmation for host-qualified repo", stdout.String())
	}
}

func TestWebhookCleanupConfirmDeletesMatchingCLIHooks(t *testing.T) {
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
				return commandExecutionResult{Stdout: `[[{"id":101,"name":"cli","type":"Repository","active":true,"events":["push","pull_request"],"config":{"url":"https://webhook-forwarder.github.com/hook"}}]]`, ExitCode: 0}, nil
			case 2:
				return commandExecutionResult{ExitCode: 0}, nil
			default:
				t.Fatalf("unexpected RunCommand call %d: %s %q", len(commands), command, args)
				return commandExecutionResult{}, nil
			}
		},
	})

	exitCode := app.Run(context.Background(), []string{"webhook", "cleanup", "acme/looper", "--confirm", "--config", configPath})
	if exitCode != 0 {
		t.Fatalf("Run(webhook cleanup --confirm) exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if len(commands) != 2 || !strings.HasSuffix(commands[0], " api --paginate --slurp repos/acme/looper/hooks") || !strings.HasSuffix(commands[1], " api -X DELETE repos/acme/looper/hooks/101") {
		t.Fatalf("commands = %q, want one paginated+slurped gh api list call followed by deleting the shown hook id", commands)
	}
	if !strings.Contains(stdout.String(), "Deleted 1 GitHub CLI webhook hook(s) for acme/looper.") {
		t.Fatalf("stdout = %q, want delete confirmation", stdout.String())
	}
}

func TestWebhookCleanupConfirmContinuesPastMissingShownHook(t *testing.T) {
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
				return commandExecutionResult{Stdout: `[[
					{"id":101,"name":"cli","type":"Repository","active":true,"events":["push"],"config":{"url":"https://webhook-forwarder.github.com/hook"}},
					{"id":202,"name":"cli","type":"Repository","active":true,"events":["pull_request"],"config":{"url":"https://webhook-forwarder.github.com/hook"}}
				]]`, ExitCode: 0}, nil
			case 2:
				return commandExecutionResult{ExitCode: 1, Stderr: "gh: HTTP 404: Not Found (https://api.github.com/repos/acme/looper/hooks/101)"}, nil
			case 3:
				return commandExecutionResult{ExitCode: 0}, nil
			default:
				t.Fatalf("unexpected RunCommand call %d: %s %q", len(commands), command, args)
				return commandExecutionResult{}, nil
			}
		},
	})

	exitCode := app.Run(context.Background(), []string{"webhook", "cleanup", "acme/looper", "--confirm", "--config", configPath})
	if exitCode != 0 {
		t.Fatalf("Run(webhook cleanup --confirm) exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if len(commands) != 3 || !strings.HasSuffix(commands[1], " api -X DELETE repos/acme/looper/hooks/101") || !strings.HasSuffix(commands[2], " api -X DELETE repos/acme/looper/hooks/202") {
		t.Fatalf("commands = %q, want cleanup to continue deleting the remaining shown hook ids after a 404", commands)
	}
	if !strings.Contains(stdout.String(), "Deleted 2 GitHub CLI webhook hook(s) for acme/looper.") {
		t.Fatalf("stdout = %q, want delete confirmation after continuing past a missing hook", stdout.String())
	}
}

func TestWebhookRotateRefusesURLMismatchBeforePatch(t *testing.T) {
	t.Parallel()

	configPath, repo := writeWebhookCommandConfigWithRecord(t, storage.WebhookTunnelHookRecord{Repo: "acme/looper", HookID: 42, ManagedURL: "https://example.com/webhook/acme/looper", SecretRef: "webhook_acme_looper.key", CreatedAt: 1, UpdatedAt: 1})
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
			if len(commands) != 1 {
				t.Fatalf("unexpected RunCommand call %d: %s %q", len(commands), command, args)
			}
			return commandExecutionResult{Stdout: `{"id":42,"active":true,"events":["pull_request"],"config":{"url":"https://example.com/other","content_type":"json","insecure_ssl":"0"}}`, ExitCode: 0}, nil
		},
	})

	exitCode := app.Run(context.Background(), []string{"webhook", "rotate", repo, "--config", configPath})
	if exitCode == 0 {
		t.Fatalf("Run(webhook rotate) exit code = %d, want non-zero", exitCode)
	}
	if len(commands) != 1 || !strings.HasSuffix(commands[0], " api repos/acme/looper/hooks/42") {
		t.Fatalf("commands = %q, want a single GET preflight", commands)
	}
	if !strings.Contains(stderr.String(), "refusing to rotate hook 42 for acme/looper") {
		t.Fatalf("stderr = %q, want URL mismatch refusal", stderr.String())
	}
}

func TestWebhookDeleteConfirmForgetRemovesLocalRecordWithoutRunningGH(t *testing.T) {
	t.Parallel()

	configPath, repoStoreKey := writeWebhookCommandConfigWithRecord(t, storage.WebhookTunnelHookRecord{Repo: "acme/looper", HookID: 42, ManagedURL: "https://example.com/webhook/acme/looper", SecretRef: "webhook_acme_looper.key", Orphaned: true, CreatedAt: 1, UpdatedAt: 1})
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	runCalled := false
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
			runCalled = true
			return commandExecutionResult{}, nil
		},
	})

	exitCode := app.Run(context.Background(), []string{"webhook", "delete", repoStoreKey, "--confirm", "--forget", "--config", configPath})
	if exitCode != 0 {
		t.Fatalf("Run(webhook delete --confirm --forget) exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if runCalled {
		t.Fatal("RunCommand() called, want no gh invocation")
	}
	if !strings.Contains(stdout.String(), "Forgot local tunnel webhook record for acme/looper") {
		t.Fatalf("stdout = %q, want forget confirmation", stdout.String())
	}
	db, err := storage.OpenSQLiteDB(context.Background(), filepath.Join(filepath.Dir(configPath), "looper.sqlite"))
	if err != nil {
		t.Fatalf("OpenSQLiteDB() error = %v", err)
	}
	defer func() { _ = db.Close() }()
	_, ok, err := storage.NewRepositories(db).WebhookTunnelHooks.Get(context.Background(), "acme/looper")
	if err != nil {
		t.Fatalf("WebhookTunnelHooks.Get() error = %v", err)
	}
	if ok {
		t.Fatal("WebhookTunnelHooks.Get() found record, want deleted")
	}
}

func writeWebhookCommandConfigWithRecord(t *testing.T, record storage.WebhookTunnelHookRecord) (string, string) {
	t.Helper()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	dbPath := filepath.Join(dir, "looper.sqlite")
	coordinator := openMigratedCLIWebhookCoordinator(t, dbPath)
	defer func() { _ = coordinator.Close() }()
	if err := storage.NewRepositories(coordinator.DB()).WebhookTunnelHooks.Upsert(context.Background(), record); err != nil {
		t.Fatalf("WebhookTunnelHooks.Upsert() error = %v", err)
	}
	raw, err := json.Marshal(map[string]any{
		"storage": map[string]any{"dbPath": dbPath},
		"tools":   map[string]any{"ghPath": "/usr/bin/gh"},
		"notifications": map[string]any{
			"osascript": map[string]any{"enabled": false},
		},
	})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if err := os.WriteFile(configPath, raw, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return configPath, record.Repo
}

func openMigratedCLIWebhookCoordinator(t *testing.T, dbPath string) *storage.SQLiteCoordinator {
	t.Helper()

	coordinator, err := storage.OpenSQLiteCoordinator(context.Background(), dbPath, storage.SQLiteCoordinatorOptions{BackupDir: filepath.Join(filepath.Dir(dbPath), "backups")})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	if _, err := coordinator.MigrationRunner().RunPending(context.Background()); err != nil {
		t.Fatalf("MigrationRunner.RunPending() error = %v", err)
	}
	return coordinator
}
