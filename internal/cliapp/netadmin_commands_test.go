package cliapp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	githubinfra "github.com/nexu-io/looper/internal/infra/github"
)

func TestNetadminOnboardRepoInitializesLabelsAndCreatesLoopernetWebhook(t *testing.T) {
	labels := make([]map[string]any, 0, len(githubinfra.StandardLooperLabels()))
	for _, label := range githubinfra.StandardLooperLabels() {
		labels = append(labels, map[string]any{"name": label.Name, "color": label.Color, "description": label.Description})
	}
	labelJSON, _ := json.Marshal(labels)
	t.Setenv("LOOPERNET_ADMIN_TOKEN", "admin-token")
	secretServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/github/webhook-secret" {
			t.Fatalf("request path = %q, want /v1/github/webhook-secret", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer admin-token" {
			t.Fatalf("Authorization = %q, want Bearer admin-token", got)
		}
		_, _ = w.Write([]byte(`{"secret":"shared-secret"}`))
	}))
	defer secretServer.Close()
	configPath := writeEditableCLIConfigWithPayload(t, map[string]any{
		"network": map[string]any{"loopernetBaseUrl": secretServer.URL},
		"server":  map[string]any{"baseUrl": secretServer.URL},
		"tools":   map[string]any{"ghPath": "/usr/bin/gh"},
		"notifications": map[string]any{
			"osascript": map[string]any{"enabled": false},
		},
	})
	commands := []string{}
	app := New(Deps{RunCommand: func(ctx context.Context, command string, args []string, timeout time.Duration) (commandExecutionResult, error) {
		commands = append(commands, strings.Join(args, " "))
		switch {
		case strings.Join(args, " ") == "auth status --hostname github.com":
			return commandExecutionResult{ExitCode: 0}, nil
		case strings.Join(args, " ") == "label list --repo acme/looper --limit 1000 --json name,color,description":
			return commandExecutionResult{Stdout: string(labelJSON), ExitCode: 0}, nil
		case strings.Join(args[:3], " ") == "api --paginate --slurp":
			return commandExecutionResult{Stdout: `[]`, ExitCode: 0}, nil
		case len(args) >= 5 && args[0] == "api" && args[1] == "repos/acme/looper/hooks" && args[2] == "--method" && args[3] == "POST":
			return commandExecutionResult{Stdout: `{"id":7,"name":"web","config":{"url":"` + secretServer.URL + `/v1/github/webhook"}}`, ExitCode: 0}, nil
		default:
			t.Fatalf("unexpected gh command: %q", strings.Join(args, " "))
			return commandExecutionResult{}, nil
		}
	}})
	exitCode, _, stderr := runAppWithDeps(t, app, []string{"netadmin", "onboard-repo", "acme/looper", "--json", "--config", configPath})
	if exitCode != 0 {
		t.Fatalf("Run(netadmin onboard-repo) exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
	if len(commands) < 4 {
		t.Fatalf("commands = %v, want auth + label list + hook list + hook create", commands)
	}
}

func TestNetadminOffboardRepoDeletesMatchingLoopernetWebhook(t *testing.T) {
	t.Parallel()
	configPath := writeEditableCLIConfigWithPayload(t, map[string]any{
		"network": map[string]any{"loopernetBaseUrl": "https://loopernet.example.com"},
		"server":  map[string]any{"baseUrl": "https://loopernet.example.com"},
		"tools":   map[string]any{"ghPath": "/usr/bin/gh"},
		"notifications": map[string]any{
			"osascript": map[string]any{"enabled": false},
		},
	})
	deleted := []string{}
	app := New(Deps{RunCommand: func(ctx context.Context, command string, args []string, timeout time.Duration) (commandExecutionResult, error) {
		switch {
		case strings.Join(args, " ") == "auth status --hostname github.com":
			return commandExecutionResult{ExitCode: 0}, nil
		case strings.Join(args[:3], " ") == "api --paginate --slurp":
			return commandExecutionResult{Stdout: `[[{"id":7,"name":"web","config":{"url":"https://loopernet.example.com/v1/github/webhook"}},{"id":8,"name":"web","config":{"url":"https://other.example.com/hook"}}]]`, ExitCode: 0}, nil
		case len(args) >= 4 && args[0] == "api" && args[1] == "-X" && args[2] == "DELETE":
			deleted = append(deleted, args[3])
			return commandExecutionResult{ExitCode: 0}, nil
		default:
			t.Fatalf("unexpected gh command: %q", strings.Join(args, " "))
			return commandExecutionResult{}, nil
		}
	}})
	exitCode, _, stderr := runAppWithDeps(t, app, []string{"netadmin", "offboard-repo", "acme/looper", "--json", "--config", configPath})
	if exitCode != 0 {
		t.Fatalf("Run(netadmin offboard-repo) exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
	if len(deleted) != 1 || deleted[0] != "repos/acme/looper/hooks/7" {
		t.Fatalf("deleted = %v, want matching loopernet hook removed", deleted)
	}
}

func runAppWithDeps(t *testing.T, app *App, argv []string) (int, string, string) {
	t.Helper()
	stdout := &strings.Builder{}
	stderr := &strings.Builder{}
	app.deps.Stdout = stdout
	app.deps.Stderr = stderr
	exitCode := app.Run(context.Background(), argv)
	return exitCode, stdout.String(), stderr.String()
}
