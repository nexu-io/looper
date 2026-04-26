package cliapp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/powerformer/looper/internal/agent"
	"github.com/powerformer/looper/internal/api"
	"github.com/powerformer/looper/internal/config"
	gitinfra "github.com/powerformer/looper/internal/infra/git"
	looperdruntime "github.com/powerformer/looper/internal/runtime"
	"github.com/powerformer/looper/internal/version"
	"github.com/powerformer/looper/internal/worker"
	pkgapi "github.com/powerformer/looper/pkg/api"
)

func runApp(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	return runAppWithContext(t, context.Background(), args...)
}

func runAppWithContext(t *testing.T, ctx context.Context, args ...string) (int, string, string) {
	t.Helper()

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := New(Deps{Stdout: stdout, Stderr: stderr})
	exitCode := app.Run(ctx, args)

	return exitCode, stdout.String(), stderr.String()
}

func TestCommandGroupHelpListsExpectedSubcommands(t *testing.T) {
	t.Parallel()

	tests := []struct {
		args        []string
		subcommands []string
	}{
		{args: []string{"project", "--help"}, subcommands: []string{"list  List projects", "add   Add a project"}},
		{args: []string{"config", "--help"}, subcommands: []string{"show  Show active config"}},
		{args: []string{"daemon", "--help"}, subcommands: []string{"install  Install the managed daemon binary", "status   Show daemon status", "start    Start the daemon", "stop     Stop the daemon", "restart  Restart the daemon", "logs     Show daemon logs"}},
		{args: []string{"loop", "--help"}, subcommands: []string{"list   List loops", "start  Start a loop", "pause  Pause a loop"}},
		{args: []string{"pr", "--help"}, subcommands: []string{"list    List pull requests", "show    Show a pull request", "status  Show pull request status"}},
		{args: []string{"run", "--help"}, subcommands: []string{"list  List runs"}},
	}

	for _, testCase := range tests {
		testCase := testCase
		t.Run(strings.Join(testCase.args, "_"), func(t *testing.T) {
			t.Parallel()

			exitCode, stdout, stderr := runApp(t, testCase.args...)
			if exitCode != 0 {
				t.Fatalf("Run(%v) exit code = %d, want 0", testCase.args, exitCode)
			}
			if stderr != "" {
				t.Fatalf("Run(%v) stderr = %q, want empty string", testCase.args, stderr)
			}
			if !strings.Contains(stdout, "Subcommands:") {
				t.Fatalf("Run(%v) stdout = %q, want Subcommands section", testCase.args, stdout)
			}

			for _, subcommand := range testCase.subcommands {
				if !strings.Contains(stdout, subcommand) {
					t.Fatalf("Run(%v) stdout = %q, want to contain %q", testCase.args, stdout, subcommand)
				}
			}
		})
	}
}

func TestRootHelpIncludesGlobalFlagsWithFrozenSyntax(t *testing.T) {
	t.Parallel()

	exitCode, stdout, stderr := runApp(t, "--help")
	if exitCode != 0 {
		t.Fatalf("Run([--help]) exit code = %d, want 0", exitCode)
	}
	if stderr != "" {
		t.Fatalf("Run([--help]) stderr = %q, want empty string", stderr)
	}
	if !strings.Contains(stdout, "Version:\n  "+version.Current().Version) {
		t.Fatalf("Run([--help]) stdout = %q, want version section", stdout)
	}

	for _, syntax := range []string{
		"--json",
		"--config <path>",
		"--host <host>",
		"--port <port>",
		"--db-path <path>",
		"--log-dir <path>",
		"--daemon-mode <mode>",
		"--git-path <path>",
		"--gh-path <path>",
		"--osascript-path <path>",
	} {
		if !strings.Contains(stdout, syntax) {
			t.Fatalf("Run([--help]) stdout = %q, want to contain %q", stdout, syntax)
		}
	}
}

func TestRootHelpIncludesFeedbackSubcommand(t *testing.T) {
	t.Parallel()

	exitCode, stdout, stderr := runApp(t, "--help")
	if exitCode != 0 {
		t.Fatalf("Run([--help]) exit code = %d, want 0", exitCode)
	}
	if stderr != "" {
		t.Fatalf("Run([--help]) stderr = %q, want empty string", stderr)
	}
	if !strings.Contains(stdout, "feedback") || !strings.Contains(stdout, "Submit feedback as a GitHub issue") {
		t.Fatalf("Run([--help]) stdout = %q, want feedback subcommand", stdout)
	}
}

func TestFeedbackCommandRunsAgentAndPrintsIssueURL(t *testing.T) {
	t.Parallel()

	scriptPath := filepath.Join(t.TempDir(), "fake-agent.sh")
	script := strings.Join([]string{
		"#!/bin/sh",
		"printf 'agent started\\n'",
		"printf 'https://github.com/powerformer/looper/issues/42\\n'",
		"printf 'https://github.com/powerformer/looper/issues/321\\n'",
		"printf '%s{\"summary\":\"created issue\"}\\n' \"$LOOPER_COMPLETION_MARKER\"",
	}, "\n")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake agent script: %v", err)
	}

	configPath := writeCLIConfigWithAgent(t, "http://127.0.0.1:1", string(config.AgentVendorOpenCode), map[string]any{"command": scriptPath})

	exitCode, stdout, stderr := runApp(t, "feedback", "Great", "tool", "--title", "CLI Feedback", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run([feedback ...]) exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
	if stderr != "" {
		t.Fatalf("Run([feedback ...]) stderr = %q, want empty string", stderr)
	}
	if got, want := stdout, "https://github.com/powerformer/looper/issues/321\n"; got != want {
		t.Fatalf("Run([feedback ...]) stdout = %q, want %q", got, want)
	}

	exitCode, stdout, stderr = runApp(t, "feedback", "Great", "tool", "--title", "CLI Feedback", "--json", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run([feedback ... --json]) exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
	if stderr != "" {
		t.Fatalf("Run([feedback ... --json]) stderr = %q, want empty string", stderr)
	}
	assertJSONContains(t, stdout, "repo", "powerformer/looper")
	assertJSONContains(t, stdout, "titleHint", "CLI Feedback")
	assertJSONContains(t, stdout, "message", "Great tool")
	assertJSONContains(t, stdout, "issueUrl", "https://github.com/powerformer/looper/issues/321")
	assertJSONContains(t, stdout, "summary", "created issue")
}

func TestFeedbackCommandRequiresMessage(t *testing.T) {
	t.Parallel()

	configPath := writeCLIConfigWithAgent(t, "http://127.0.0.1:1", string(config.AgentVendorOpenCode), map[string]any{"command": "/bin/true"})
	exitCode, stdout, stderr := runApp(t, "feedback", "--title", "Only title", "--config", configPath)
	if exitCode == 0 {
		t.Fatalf("Run([feedback --title ...]) exit code = %d, want non-zero", exitCode)
	}
	if stdout != "" {
		t.Fatalf("Run([feedback --title ...]) stdout = %q, want empty string", stdout)
	}
	if !strings.Contains(stderr, "feedback message is required") {
		t.Fatalf("Run([feedback --title ...]) stderr = %q, want missing message error", stderr)
	}
}

func TestVersionCommandPrintsCurrentVersion(t *testing.T) {
	t.Parallel()

	exitCode, stdout, stderr := runApp(t, "version")
	if exitCode != 0 {
		t.Fatalf("Run([version]) exit code = %d, want 0", exitCode)
	}
	if stderr != "" {
		t.Fatalf("Run([version]) stderr = %q, want empty string", stderr)
	}
	if got, want := stdout, version.Current().Version+"\n"; got != want {
		t.Fatalf("Run([version]) stdout = %q, want %q", got, want)
	}
}

func TestVersionCommandJSONPrintsBuildMetadata(t *testing.T) {
	t.Parallel()

	exitCode, stdout, stderr := runApp(t, "version", "--json")
	if exitCode != 0 {
		t.Fatalf("Run([version --json]) exit code = %d, want 0", exitCode)
	}
	if stderr != "" {
		t.Fatalf("Run([version --json]) stderr = %q, want empty string", stderr)
	}
	assertJSONContains(t, stdout, "version", version.Current().Version)
	assertJSONContains(t, stdout, "metadata", map[string]any{
		"versionSource":   version.Current().Metadata.VersionSource,
		"channel":         version.Current().Metadata.Channel,
		"apiVersion":      version.Current().Metadata.APIVersion,
		"minCliForDaemon": nil,
		"minDaemonForCli": nil,
		"gitCommitSha":    nil,
		"buildTimestamp":  nil,
	})
}

func TestNestedCommandParsingReachesLeafCommands(t *testing.T) {
	t.Parallel()

	configPath := writeDaemonCLIConfig(t, "http://127.0.0.1:1")
	homeDir := t.TempDir()
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := New(Deps{
		Stdout:  stdout,
		Stderr:  stderr,
		HomeDir: homeDir,
		ReadFile: func(path string) ([]byte, error) {
			if strings.HasSuffix(path, filepath.Join("logs", "looperd.log")) {
				return []byte("one\ntwo\n"), nil
			}
			return nil, os.ErrNotExist
		},
	})

	exitCode := app.Run(context.Background(), []string{"daemon", "logs", "--lines", "1", "--json", "--config", configPath})
	if exitCode != 0 {
		t.Fatalf("Run([daemon logs --lines 1 --json]) exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("Run([daemon logs --lines 1 --json]) stderr = %q, want empty string", stderr.String())
	}
	assertJSONContains(t, stdout.String(), "lines", []any{"two"})
}

func TestExtractConfigArgsForwardsOnlyConfigFlags(t *testing.T) {
	t.Parallel()

	got := ExtractConfigArgs([]string{
		"daemon",
		"start",
		"--json",
		"--config",
		"/tmp/looper.json",
		"--host",
		"127.0.0.2",
		"--port",
		"9999",
		"--db-path=/tmp/looper.sqlite",
		"--log-dir",
		"/tmp/looper-logs",
		"--daemon-mode",
		"minimal",
		"--git-path",
		"/opt/git",
		"--gh-path",
		"/opt/gh",
		"--osascript-path",
		"/opt/osascript",
		"--force",
	})

	want := []string{
		"--config",
		"/tmp/looper.json",
		"--host",
		"127.0.0.2",
		"--port",
		"9999",
		"--db-path=/tmp/looper.sqlite",
		"--log-dir",
		"/tmp/looper-logs",
		"--daemon-mode",
		"minimal",
		"--git-path",
		"/opt/git",
		"--gh-path",
		"/opt/gh",
		"--osascript-path",
		"/opt/osascript",
	}

	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("ExtractConfigArgs() = %#v, want %#v", got, want)
	}
}

func TestStatusJSONPrintsDaemonPayload(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/status" {
			t.Fatalf("request path = %q, want %q", r.URL.Path, "/api/v1/status")
		}
		writeEnvelope(t, w, pkgapi.Success("req_status", map[string]any{"healthy": true, "version": "1.2.3"}))
	}))
	defer server.Close()

	configPath := writeCLIConfig(t, server.URL, "")
	exitCode, stdout, stderr := runApp(t, "status", "--json", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run([status --json]) exit code = %d, want 0", exitCode)
	}
	if stderr != "" {
		t.Fatalf("Run([status --json]) stderr = %q, want empty string", stderr)
	}
	assertJSONContains(t, stdout, "healthy", true)
	assertJSONContains(t, stdout, "version", "1.2.3")
}

func TestConfigShowJSONSendsLocalToken(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.Header.Get("Authorization"), "Bearer secret-token"; got != want {
			t.Fatalf("Authorization header = %q, want %q", got, want)
		}
		writeEnvelope(t, w, pkgapi.Success("req_config", map[string]any{"server": map[string]any{"authMode": "local-token"}}))
	}))
	defer server.Close()

	configPath := writeCLIConfig(t, server.URL, "secret-token")
	exitCode, stdout, stderr := runApp(t, "config", "show", "--json", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run([config show --json]) exit code = %d, want 0", exitCode)
	}
	if stderr != "" {
		t.Fatalf("Run([config show --json]) stderr = %q, want empty string", stderr)
	}
	assertJSONContains(t, stdout, "server", map[string]any{"authMode": "local-token"})
}

func TestProjectAddJSONPostsExpectedBody(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.Method, http.MethodPost; got != want {
			t.Fatalf("request method = %q, want %q", got, want)
		}
		if got, want := r.URL.Path, "/api/v1/projects"; got != want {
			t.Fatalf("request path = %q, want %q", got, want)
		}

		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		if got, want := body["repoPath"], "/tmp/repo"; got != want {
			t.Fatalf("body.repoPath = %#v, want %#v", got, want)
		}
		if got, want := body["id"], "project_1"; got != want {
			t.Fatalf("body.id = %#v, want %#v", got, want)
		}
		if got, want := body["repo"], "acme/looper"; got != want {
			t.Fatalf("body.repo = %#v, want %#v", got, want)
		}

		writeEnvelope(t, w, pkgapi.Success("req_project", map[string]any{"id": "project_1", "repoPath": "/tmp/repo"}))
	}))
	defer server.Close()

	configPath := writeCLIConfig(t, server.URL, "")
	exitCode, stdout, stderr := runApp(t, "project", "add", "/tmp/repo", "--id", "project_1", "--repo", "acme/looper", "--json", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run([project add ... --json]) exit code = %d, want 0", exitCode)
	}
	if stderr != "" {
		t.Fatalf("Run([project add ... --json]) stderr = %q, want empty string", stderr)
	}
	assertJSONContains(t, stdout, "id", "project_1")
}

func TestStatusWithoutJSONPrintsHumanReadableSections(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/status" {
			t.Fatalf("request path = %q, want %q", r.URL.Path, "/api/v1/status")
		}
		writeEnvelope(t, w, pkgapi.Success("req_status", map[string]any{
			"service":   map[string]any{"healthy": true, "version": "1.2.3", "daemonMode": "full", "startedAt": "2026-04-20T10:00:00.000Z"},
			"storage":   map[string]any{"dbPath": "/tmp/looper.sqlite", "schemaVersion": "12", "healthy": true, "pendingMigrations": []string{}},
			"scheduler": map[string]any{"healthy": true, "queuedItems": 2, "runningItems": 1},
			"loops": map[string]any{
				"planner":  map[string]any{"running": 0, "paused": 1, "failed": 0},
				"reviewer": map[string]any{"running": 1, "paused": 0, "failed": 0},
				"worker":   map[string]any{"running": 2, "paused": 0, "failed": 1},
				"fixer":    map[string]any{"running": 0, "paused": 0, "failed": 0},
			},
			"tools":         map[string]any{"git": true, "gh": false, "osascript": true},
			"notifications": map[string]any{"inAppEnabled": true, "osascriptEnabled": false},
		}))
	}))
	defer server.Close()

	configPath := writeCLIConfig(t, server.URL, "")
	exitCode, stdout, stderr := runApp(t, "status", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run([status]) exit code = %d, want 0", exitCode)
	}
	if stderr != "" {
		t.Fatalf("Run([status]) stderr = %q, want empty string", stderr)
	}
	for _, want := range []string{"Service", "healthy    : yes", "version    : 1.2.3", "Storage", "Scheduler", "type", "reviewer", "Tools", "gh        : no", "Notifications"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("Run([status]) stdout = %q, want to contain %q", stdout, want)
		}
	}
}

func TestConfigShowWithoutJSONPrintsJSON(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeEnvelope(t, w, pkgapi.Success("req_config", map[string]any{"server": map[string]any{"authMode": "none"}}))
	}))
	defer server.Close()

	configPath := writeCLIConfig(t, server.URL, "")
	exitCode, stdout, stderr := runApp(t, "config", "show", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run([config show]) exit code = %d, want 0", exitCode)
	}
	if stderr != "" {
		t.Fatalf("Run([config show]) stderr = %q, want empty string", stderr)
	}
	assertJSONContains(t, stdout, "server", map[string]any{"authMode": "none"})
}

func TestProjectListWithoutJSONPrintsTable(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.URL.Path, "/api/v1/projects"; got != want {
			t.Fatalf("request path = %q, want %q", got, want)
		}
		writeEnvelope(t, w, pkgapi.Success("req_projects", map[string]any{"items": []map[string]any{{"id": "project_1", "name": "Looper", "repoPath": "/tmp/repo", "baseBranch": "main", "repo": "acme/looper", "updatedAt": "2026-04-20T10:00:00.000Z"}}}))
	}))
	defer server.Close()

	configPath := writeCLIConfig(t, server.URL, "")
	exitCode, stdout, stderr := runApp(t, "project", "list", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run([project list]) exit code = %d, want 0", exitCode)
	}
	if stderr != "" {
		t.Fatalf("Run([project list]) stderr = %q, want empty string", stderr)
	}
	for _, want := range []string{"id", "repoPath", "project_1", "/tmp/repo", "acme/looper"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("Run([project list]) stdout = %q, want to contain %q", stdout, want)
		}
	}
}

func TestPSWithoutJSONPrintsEmptyMessage(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.URL.Path, "/api/v1/runs/active"; got != want {
			t.Fatalf("request path = %q, want %q", got, want)
		}
		writeEnvelope(t, w, pkgapi.Success("req_active_runs", map[string]any{"items": []map[string]any{}}))
	}))
	defer server.Close()

	configPath := writeCLIConfig(t, server.URL, "")
	exitCode, stdout, stderr := runApp(t, "ps", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run([ps]) exit code = %d, want 0", exitCode)
	}
	if stderr != "" {
		t.Fatalf("Run([ps]) stderr = %q, want empty string", stderr)
	}
	if got, want := stdout, "No running or queued loops.\n"; got != want {
		t.Fatalf("Run([ps]) stdout = %q, want %q", got, want)
	}
}

func TestPSWithoutJSONShowsRunningLoopWithoutRunRow(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.URL.Path, "/api/v1/runs/active"; got != want {
			t.Fatalf("request path = %q, want %q", got, want)
		}
		writeEnvelope(t, w, pkgapi.Success("req_active_runs", map[string]any{"items": []map[string]any{{
			"seq":         42,
			"type":        "reviewer",
			"status":      "running",
			"currentStep": "review",
			"target":      map[string]any{"label": "acme/looper#123"},
		}}}))
	}))
	defer server.Close()

	configPath := writeCLIConfig(t, server.URL, "")
	exitCode, stdout, stderr := runApp(t, "ps", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run([ps]) exit code = %d, want 0", exitCode)
	}
	if stderr != "" {
		t.Fatalf("Run([ps]) stderr = %q, want empty string", stderr)
	}
	if strings.Contains(stdout, "No running or queued loops.") {
		t.Fatalf("Run([ps]) stdout = %q, did not expect empty-state message", stdout)
	}
	for _, want := range []string{"reviewer", "acme/looper#123", "running"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("Run([ps]) stdout = %q, want to contain %q", stdout, want)
		}
	}
}

func TestLogsWithoutJSONPrintsHeaderAndTail(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.URL.Path, "/api/v1/loops/loop_1/logs"; got != want {
			t.Fatalf("request path = %q, want %q", got, want)
		}
		writeEnvelope(t, w, pkgapi.Success("req_logs", map[string]any{"seq": 12, "loopId": "loop_1", "loopType": "reviewer", "loopStatus": "running", "run": map[string]any{"runId": "run_1", "currentStep": "review"}, "agent": map[string]any{"vendor": "openai", "pid": 1234, "status": "running", "stdout": "line1\nline2\nline3\n", "stderr": "err1\nerr2\n"}}))
	}))
	defer server.Close()

	configPath := writeCLIConfig(t, server.URL, "")
	exitCode, stdout, stderr := runApp(t, "logs", "loop_1", "--tail", "2", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run([logs loop_1 --tail 2]) exit code = %d, want 0", exitCode)
	}
	if stderr != "" {
		t.Fatalf("Run([logs loop_1 --tail 2]) stderr = %q, want empty string", stderr)
	}
	for _, want := range []string{"Loop #12 · reviewer · running", "Run run_1 · step: review", "Agent: openai · pid 1234 · running", "line2", "line3"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("Run([logs loop_1 --tail 2]) stdout = %q, want to contain %q", stdout, want)
		}
	}
	if strings.Contains(stdout, "line1") {
		t.Fatalf("Run([logs loop_1 --tail 2]) stdout = %q, did not expect trimmed line", stdout)
	}
}

func TestLogsWithoutJSONPrintsAgentErrorMessage(t *testing.T) {
	t.Parallel()

	errorMessage := "The 'gpt-5.5' model requires a newer version of Codex. Please upgrade to the latest app or CLI and try again."
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeEnvelope(t, w, pkgapi.Success("req_logs", map[string]any{"seq": 12, "loopId": "loop_1", "loopType": "fixer", "loopStatus": "failed", "run": map[string]any{"runId": "run_1", "currentStep": "repair", "status": "failed"}, "agent": map[string]any{"vendor": "codex", "status": "failed", "errorMessage": errorMessage, "stdout": "", "stderr": errorMessage}}))
	}))
	defer server.Close()

	configPath := writeCLIConfig(t, server.URL, "")
	exitCode, stdout, stderr := runApp(t, "logs", "loop_1", "--tail", "2", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run([logs loop_1 --tail 2]) exit code = %d, want 0", exitCode)
	}
	if stderr != "" {
		t.Fatalf("Run([logs loop_1 --tail 2]) stderr = %q, want empty string", stderr)
	}
	for _, want := range []string{"Agent: codex", "Error: " + errorMessage, errorMessage} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("Run([logs loop_1 --tail 2]) stdout = %q, want to contain %q", stdout, want)
		}
	}
}

func TestLogsWithoutJSONDefaultsToCodexStderrWhenStdoutEmpty(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.URL.Path, "/api/v1/loops/loop_1/logs"; got != want {
			t.Fatalf("request path = %q, want %q", got, want)
		}
		writeEnvelope(t, w, pkgapi.Success("req_logs", map[string]any{
			"seq":        12,
			"loopId":     "loop_1",
			"loopType":   "reviewer",
			"loopStatus": "running",
			"run":        map[string]any{"runId": "run_1", "currentStep": "review"},
			"agent": map[string]any{
				"vendor": "codex",
				"pid":    1234,
				"status": "running",
				"stdout": "",
				"stderr": "codex line1\ncodex line2\n",
			},
		}))
	}))
	defer server.Close()

	configPath := writeCLIConfig(t, server.URL, "")
	exitCode, stdout, stderr := runApp(t, "logs", "loop_1", "--tail", "2", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run([logs loop_1 --tail 2]) exit code = %d, want 0", exitCode)
	}
	if stderr != "" {
		t.Fatalf("Run([logs loop_1 --tail 2]) stderr = %q, want empty string", stderr)
	}
	for _, want := range []string{"Loop #12 · reviewer · running", "Run run_1 · step: review", "Agent: codex · pid 1234 · running", "codex line1", "codex line2"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("Run([logs loop_1 --tail 2]) stdout = %q, want to contain %q", stdout, want)
		}
	}
}

func TestLogsFollowStreamsNewOutput(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.URL.Path, "/api/v1/loops/loop_1/logs"; got != want {
			t.Fatalf("request path = %q, want %q", got, want)
		}
		if got := r.URL.Query().Get("follow"); got != "1" {
			t.Fatalf("follow query = %q, want 1", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: snapshot\n")
		_, _ = io.WriteString(w, "data: {\"seq\":12,\"loopType\":\"reviewer\",\"loopStatus\":\"running\",\"run\":{\"runId\":\"run_1\",\"status\":\"running\",\"currentStep\":\"review\"},\"agent\":{\"executionId\":\"exec_1\",\"vendor\":\"openai\",\"pid\":1234,\"status\":\"running\",\"stdout\":\"line1\\n\",\"stderr\":\"\"}}\n\n")
		_, _ = io.WriteString(w, "event: chunk\n")
		_, _ = io.WriteString(w, "data: {\"runId\":\"run_1\",\"currentStep\":\"review\",\"executionId\":\"exec_1\",\"vendor\":\"openai\",\"pid\":1234,\"status\":\"running\",\"content\":\"line2\\n\"}\n\n")
		_, _ = io.WriteString(w, "event: end\n")
		_, _ = io.WriteString(w, "data: {\"reason\":\"run_completed\"}\n\n")
	}))
	defer server.Close()

	configPath := writeCLIConfig(t, server.URL, "")
	exitCode, stdout, stderr := runApp(t, "logs", "loop_1", "--follow", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run([logs loop_1 --follow]) exit code = %d, want 0", exitCode)
	}
	if stderr != "" {
		t.Fatalf("Run([logs loop_1 --follow]) stderr = %q, want empty string", stderr)
	}
	for _, want := range []string{"Loop #12 · reviewer · running", "Run run_1 · step: review", "Agent: openai · pid 1234 · running", "line1", "line2"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("Run([logs loop_1 --follow]) stdout = %q, want to contain %q", stdout, want)
		}
	}
}

func TestLogsFollowDefaultsToCodexStderrWhenStdoutEmpty(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.URL.Path, "/api/v1/loops/loop_1/logs"; got != want {
			t.Fatalf("request path = %q, want %q", got, want)
		}
		if got := r.URL.Query().Get("follow"); got != "1" {
			t.Fatalf("follow query = %q, want 1", got)
		}
		if got := r.URL.Query().Get("stderr"); got != "" {
			t.Fatalf("stderr query = %q, want empty string", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: snapshot\n")
		_, _ = io.WriteString(w, "data: {\"seq\":12,\"loopType\":\"reviewer\",\"loopStatus\":\"running\",\"run\":{\"runId\":\"run_1\",\"status\":\"running\",\"currentStep\":\"review\"},\"agent\":{\"executionId\":\"exec_1\",\"vendor\":\"codex\",\"pid\":1234,\"status\":\"running\",\"stdout\":\"\",\"stderr\":\"codex line1\\n\"}}\n\n")
		_, _ = io.WriteString(w, "event: chunk\n")
		_, _ = io.WriteString(w, "data: {\"runId\":\"run_1\",\"currentStep\":\"review\",\"executionId\":\"exec_1\",\"vendor\":\"codex\",\"pid\":1234,\"status\":\"running\",\"content\":\"codex line2\\n\"}\n\n")
		_, _ = io.WriteString(w, "event: end\n")
		_, _ = io.WriteString(w, "data: {\"reason\":\"run_completed\"}\n\n")
	}))
	defer server.Close()

	configPath := writeCLIConfig(t, server.URL, "")
	exitCode, stdout, stderr := runApp(t, "logs", "loop_1", "--follow", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run([logs loop_1 --follow]) exit code = %d, want 0", exitCode)
	}
	if stderr != "" {
		t.Fatalf("Run([logs loop_1 --follow]) stderr = %q, want empty string", stderr)
	}
	for _, want := range []string{"Loop #12 · reviewer · running", "Run run_1 · step: review", "Agent: codex · pid 1234 · running", "codex line1", "codex line2"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("Run([logs loop_1 --follow]) stdout = %q, want to contain %q", stdout, want)
		}
	}
}

func TestLogsFollowHandlesLargeSnapshotPayload(t *testing.T) {
	t.Parallel()

	largeOutput := strings.Repeat("x", 1024*1024+128) + "\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: snapshot\n")
		_, _ = fmt.Fprintf(w, "data: {\"seq\":12,\"loopType\":\"reviewer\",\"loopStatus\":\"success\",\"run\":{\"runId\":\"run_1\",\"status\":\"success\",\"currentStep\":\"review\"},\"agent\":{\"executionId\":\"exec_1\",\"vendor\":\"openai\",\"pid\":1234,\"status\":\"completed\",\"stdout\":%q,\"stderr\":\"\"}}\n\n", largeOutput)
		_, _ = io.WriteString(w, "event: end\n")
		_, _ = io.WriteString(w, "data: {\"reason\":\"run_completed\"}\n\n")
	}))
	defer server.Close()

	configPath := writeCLIConfig(t, server.URL, "")
	exitCode, stdout, stderr := runApp(t, "logs", "loop_1", "--follow", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run([logs loop_1 --follow]) exit code = %d, want 0", exitCode)
	}
	if stderr != "" {
		t.Fatalf("Run([logs loop_1 --follow]) stderr = %q, want empty string", stderr)
	}
	for _, want := range []string{"Loop #12 · reviewer · success", "Run run_1 · step: review", "Agent: openai · pid 1234 · completed", largeOutput[:64], largeOutput[len(largeOutput)-65 : len(largeOutput)-1]} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("Run([logs loop_1 --follow]) stdout missing expected content %q", want)
		}
	}
}

func TestLogsFollowRejectsJSON(t *testing.T) {
	t.Parallel()

	exitCode, _, stderr := runApp(t, "logs", "loop_1", "--follow", "--json")
	if exitCode == 0 {
		t.Fatal("Run([logs loop_1 --follow --json]) exit code = 0, want non-zero")
	}
	if !strings.Contains(stderr, "--json cannot be combined with --follow") {
		t.Fatalf("Run([logs loop_1 --follow --json]) stderr = %q, want follow/json error", stderr)
	}
}

func TestLogsFollowStopsOnContextCancellation(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: snapshot\n")
		_, _ = io.WriteString(w, "data: {\"seq\":12,\"loopType\":\"reviewer\",\"loopStatus\":\"running\",\"run\":{\"runId\":\"run_1\",\"status\":\"running\",\"currentStep\":\"review\"},\"agent\":{\"executionId\":\"exec_1\",\"vendor\":\"openai\",\"pid\":1234,\"status\":\"running\",\"stdout\":\"\",\"stderr\":\"\"}}\n\n")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-r.Context().Done()
	}))
	defer server.Close()

	configPath := writeCLIConfig(t, server.URL, "")
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	exitCode, stdout, stderr := runAppWithContext(t, ctx, "logs", "loop_1", "--follow", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run([logs loop_1 --follow]) exit code = %d, want 0", exitCode)
	}
	if stderr != "" {
		t.Fatalf("Run([logs loop_1 --follow]) stderr = %q, want empty string", stderr)
	}
	if !strings.Contains(stdout, "Waiting for log output...") {
		t.Fatalf("Run([logs loop_1 --follow]) stdout = %q, want waiting message", stdout)
	}
}

func TestLogsHelpIncludesFollowFlag(t *testing.T) {
	t.Parallel()

	exitCode, stdout, stderr := runApp(t, "logs", "--help")
	if exitCode != 0 {
		t.Fatalf("Run([logs --help]) exit code = %d, want 0", exitCode)
	}
	if stderr != "" {
		t.Fatalf("Run([logs --help]) stderr = %q, want empty string", stderr)
	}
	if !strings.Contains(stdout, "--follow") {
		t.Fatalf("Run([logs --help]) stdout = %q, want --follow flag", stdout)
	}
}

func TestJumpWithoutJSONPrintsShellChangeDir(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.URL.Path, "/api/v1/runs/active/12"; got != want {
			t.Fatalf("request path = %q, want %q", got, want)
		}
		writeEnvelope(t, w, pkgapi.Success("req_active_run", map[string]any{"seq": 12, "loopId": "loop_12", "projectId": "project_1", "worktree": map[string]any{"path": "/tmp/worktree"}}))
	}))
	defer server.Close()

	configPath := writeCLIConfig(t, server.URL, "")
	exitCode, stdout, stderr := runApp(t, "jump", "12", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run([jump 12]) exit code = %d, want 0", exitCode)
	}
	if stderr != "" {
		t.Fatalf("Run([jump 12]) stderr = %q, want empty string", stderr)
	}
	if got, want := stdout, "cd -- '/tmp/worktree'\n"; got != want {
		t.Fatalf("Run([jump 12]) stdout = %q, want %q", got, want)
	}
}

func TestJumpWithPrintPathPrintsWorktreePath(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.URL.Path, "/api/v1/runs/active/12"; got != want {
			t.Fatalf("request path = %q, want %q", got, want)
		}
		writeEnvelope(t, w, pkgapi.Success("req_active_run", map[string]any{"seq": 12, "loopId": "loop_12", "projectId": "project_1", "worktree": map[string]any{"path": "/tmp/worktree"}}))
	}))
	defer server.Close()

	configPath := writeCLIConfig(t, server.URL, "")
	exitCode, stdout, stderr := runApp(t, "jump", "12", "--print-path", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run([jump 12 --print-path]) exit code = %d, want 0", exitCode)
	}
	if stderr != "" {
		t.Fatalf("Run([jump 12 --print-path]) stderr = %q, want empty string", stderr)
	}
	if got, want := stdout, "/tmp/worktree\n"; got != want {
		t.Fatalf("Run([jump 12 --print-path]) stdout = %q, want %q", got, want)
	}
}

func TestJumpWithShellIntegrationPrintsHelper(t *testing.T) {
	t.Parallel()

	exitCode, stdout, stderr := runApp(t, "jump", "--shell-integration", "bash")
	if exitCode != 0 {
		t.Fatalf("Run([jump --shell-integration bash]) exit code = %d, want 0", exitCode)
	}
	if stderr != "" {
		t.Fatalf("Run([jump --shell-integration bash]) stderr = %q, want empty string", stderr)
	}
	if got, want := stdout, "lj() { eval \"$(looper jump \"$@\")\"; }\n"; got != want {
		t.Fatalf("Run([jump --shell-integration bash]) stdout = %q, want %q", got, want)
	}
}

func TestReviewCreateAcceptsNumericPRRefFromCurrentProject(t *testing.T) {
	t.Parallel()

	repoPath := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repoPath, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s) error = %v", repoPath, err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/projects":
			writeEnvelope(t, w, pkgapi.Success("req_projects", map[string]any{"items": []map[string]any{{"id": "project_1", "name": "Looper", "repoPath": repoPath, "repo": "acme/looper", "updatedAt": "2026-04-20T10:00:00.000Z"}}}))
		case "/api/v1/loops":
			if got, want := r.Method, http.MethodPost; got != want {
				t.Fatalf("request method = %q, want %q", got, want)
			}
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode request body: %v", err)
			}
			if got, want := body["projectId"], "project_1"; got != want {
				t.Fatalf("body.projectId = %#v, want %#v", got, want)
			}
			if got, want := body["repo"], "acme/looper"; got != want {
				t.Fatalf("body.repo = %#v, want %#v", got, want)
			}
			if got, want := body["prNumber"], float64(123); got != want {
				t.Fatalf("body.prNumber = %#v, want %#v", got, want)
			}
			writeEnvelope(t, w, pkgapi.Success("req_loop", map[string]any{"id": "loop_1", "projectId": "project_1", "repo": "acme/looper", "prNumber": 123, "status": "running"}))
		default:
			t.Fatalf("unexpected request path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	configPath := writeCLIConfig(t, server.URL, "")
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := New(Deps{
		Stdout: stdout,
		Stderr: stderr,
		Getwd: func() (string, error) {
			return repoPath, nil
		},
	})

	exitCode := app.Run(context.Background(), []string{"review", "123", "--config", configPath})
	if exitCode != 0 {
		t.Fatalf("Run([review 123]) exit code = %d, want 0", exitCode)
	}
	if got := stderr.String(); got != "" {
		t.Fatalf("Run([review 123]) stderr = %q, want empty string", got)
	}
	if got := stdout.String(); !strings.Contains(got, "acme/looper#123") {
		t.Fatalf("Run([review 123]) stdout = %q, want to contain %q", got, "acme/looper#123")
	}
}

func TestWorkCreateIssueResolvesProjectFromCurrentProject(t *testing.T) {
	t.Parallel()

	repoPath := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repoPath, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s) error = %v", repoPath, err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/projects":
			writeEnvelope(t, w, pkgapi.Success("req_projects", map[string]any{"items": []map[string]any{{"id": "project_1", "name": "Looper", "repoPath": repoPath, "repo": "acme/looper", "updatedAt": "2026-04-20T10:00:00.000Z"}}}))
		case "/api/v1/workers":
			if got, want := r.Method, http.MethodPost; got != want {
				t.Fatalf("request method = %q, want %q", got, want)
			}
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode request body: %v", err)
			}
			if got, want := body["projectId"], "project_1"; got != want {
				t.Fatalf("body.projectId = %#v, want %#v", got, want)
			}
			if _, ok := body["repo"]; ok {
				t.Fatalf("body.repo = %#v, want omitted when inferred from current project", body["repo"])
			}
			if got, want := body["issueNumber"], float64(54); got != want {
				t.Fatalf("body.issueNumber = %#v, want %#v", got, want)
			}
			writeEnvelope(t, w, pkgapi.Success("req_worker", map[string]any{"id": "loop_1", "projectId": "project_1", "repo": "acme/looper", "status": "queued", "issueNumber": 54, "title": "Implement issue #54", "baseBranch": "main"}))
		default:
			t.Fatalf("unexpected request path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	configPath := writeCLIConfig(t, server.URL, "")
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := New(Deps{
		Stdout: stdout,
		Stderr: stderr,
		Getwd: func() (string, error) {
			return repoPath, nil
		},
	})

	exitCode := app.Run(context.Background(), []string{"work", "--issue", "54", "--config", configPath})
	if exitCode != 0 {
		t.Fatalf("Run([work --issue 54]) exit code = %d, want 0", exitCode)
	}
	if got := stderr.String(); got != "" {
		t.Fatalf("Run([work --issue 54]) stderr = %q, want empty string", got)
	}
	if got := stdout.String(); !strings.Contains(got, "Worker started") {
		t.Fatalf("Run([work --issue 54]) stdout = %q, want to contain %q", got, "Worker started")
	}
}

func TestWorkCreateIssueRequiresExplicitProjectWhenCurrentProjectIsAmbiguous(t *testing.T) {
	t.Parallel()

	rootPath := t.TempDir()
	repoPath := filepath.Join(rootPath, "repo")
	if err := os.MkdirAll(repoPath, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s) error = %v", repoPath, err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/projects":
			writeEnvelope(t, w, pkgapi.Success("req_projects", map[string]any{"items": []map[string]any{{"id": "project_1", "name": "Looper A", "repoPath": repoPath, "repo": "acme/looper", "updatedAt": "2026-04-20T10:00:00.000Z"}, {"id": "project_2", "name": "Looper B", "repoPath": repoPath, "repo": "acme/looper", "updatedAt": "2026-04-20T10:01:00.000Z"}}}))
		default:
			t.Fatalf("unexpected request path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	configPath := writeCLIConfig(t, server.URL, "")
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := New(Deps{
		Stdout: stdout,
		Stderr: stderr,
		Getwd: func() (string, error) {
			return repoPath, nil
		},
	})

	exitCode := app.Run(context.Background(), []string{"work", "--issue", "54", "--config", configPath})
	if exitCode == 0 {
		t.Fatalf("Run([work --issue 54]) exit code = 0, want non-zero")
	}
	if got := stdout.String(); got != "" {
		t.Fatalf("Run([work --issue 54]) stdout = %q, want empty string", got)
	}
	if got := stderr.String(); !strings.Contains(got, "--project is required") {
		t.Fatalf("Run([work --issue 54]) stderr = %q, want to contain %q", got, "--project is required")
	}
}

func TestResolveProjectForCWDPrefersMostSpecificRepoPath(t *testing.T) {
	t.Parallel()

	projects := []projectOutput{
		{ID: "project_parent", RepoPath: "/tmp/repos/looper", Repo: stringPtr("acme/looper")},
		{ID: "project_child", RepoPath: "/tmp/repos/looper/submodule", Repo: stringPtr("acme/looper-submodule")},
	}

	project, err := resolveProjectForCWD(projects, "/tmp/repos/looper/submodule/internal")
	if err != nil {
		t.Fatalf("resolveProjectForCWD() error = %v", err)
	}
	if got, want := project.ID, "project_child"; got != want {
		t.Fatalf("resolveProjectForCWD().ID = %q, want %q", got, want)
	}
}

func TestLoopStartUsesExplicitProjectForPullRequestTarget(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/projects":
			writeEnvelope(t, w, pkgapi.Success("req_projects", map[string]any{"items": []map[string]any{{"id": "project_1", "name": "Looper A", "repoPath": "/tmp/repos/looper-a", "repo": "acme/looper", "updatedAt": "2026-04-20T10:00:00.000Z"}, {"id": "project_2", "name": "Looper B", "repoPath": "/tmp/repos/looper-b", "repo": "acme/looper", "updatedAt": "2026-04-20T10:01:00.000Z"}}}))
		case "/api/v1/loops":
			if got, want := r.Method, http.MethodPost; got != want {
				t.Fatalf("request method = %q, want %q", got, want)
			}
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode request body: %v", err)
			}
			if got, want := body["projectId"], "project_2"; got != want {
				t.Fatalf("body.projectId = %#v, want %#v", got, want)
			}
			writeEnvelope(t, w, pkgapi.Success("req_loop", map[string]any{"id": "loop_2", "projectId": "project_2", "repo": "acme/looper", "prNumber": 42, "status": "running"}))
		default:
			t.Fatalf("unexpected request path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	configPath := writeCLIConfig(t, server.URL, "")
	exitCode, stdout, stderr := runApp(t, "loop", "start", "--type", "reviewer", "--pr", "acme/looper#42", "--project", "project_2", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run([loop start ...]) exit code = %d, want 0", exitCode)
	}
	if stderr != "" {
		t.Fatalf("Run([loop start ...]) stderr = %q, want empty string", stderr)
	}
	if !strings.Contains(stdout, "Loop started") {
		t.Fatalf("Run([loop start ...]) stdout = %q, want to contain %q", stdout, "Loop started")
	}
}

func TestLoopStartRequiresExplicitProjectWhenRepoMatchesMultipleProjects(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/projects":
			writeEnvelope(t, w, pkgapi.Success("req_projects", map[string]any{"items": []map[string]any{{"id": "project_1", "name": "Looper A", "repoPath": "/tmp/repos/looper-a", "repo": "acme/looper", "updatedAt": "2026-04-20T10:00:00.000Z"}, {"id": "project_2", "name": "Looper B", "repoPath": "/tmp/repos/looper-b", "repo": "acme/looper", "updatedAt": "2026-04-20T10:01:00.000Z"}}}))
		default:
			t.Fatalf("unexpected request path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	configPath := writeCLIConfig(t, server.URL, "")
	exitCode, stdout, stderr := runApp(t, "loop", "start", "--type", "reviewer", "--pr", "acme/looper#42", "--config", configPath)
	if exitCode == 0 {
		t.Fatalf("Run([loop start ...]) exit code = 0, want non-zero")
	}
	if stdout != "" {
		t.Fatalf("Run([loop start ...]) stdout = %q, want empty string", stdout)
	}
	if !strings.Contains(stderr, "--project is required") {
		t.Fatalf("Run([loop start ...]) stderr = %q, want to contain %q", stderr, "--project is required")
	}
}

func TestInProcessSmokeWorkerWorkflowSucceedsWithManualPROpeningAndMutatesWorktree(t *testing.T) {
	repoPath := initSampleGitRepo(t)
	runtimeRoot := t.TempDir()

	cfg, err := config.DefaultConfig(runtimeRoot)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	backupDir := filepath.Join(runtimeRoot, "backups")
	cfg.Storage.DBPath = filepath.Join(runtimeRoot, "state", "looper.sqlite")
	cfg.Storage.BackupDir = &backupDir
	cfg.Daemon.WorkingDirectory = runtimeRoot
	vendor := config.AgentVendor("custom")
	cfg.Agent.Vendor = &vendor
	cfg.Agent.Params = map[string]any{
		"command": "/bin/sh",
		"args": []any{
			"-c",
			`printf 'smoke-change\n' >> smoke-output.txt; printf '__LOOPER_RESULT__={"summary":"smoke complete","changedFiles":["smoke-output.txt"]}\n'`,
		},
	}

	rt := looperdruntime.New(looperdruntime.Options{
		Config: cfg,
		RunSchedulerTick: func(ctx context.Context, services looperdruntime.Services) error {
			runner := worker.New(worker.Options{
				DB:    services.Coordinator.DB(),
				Repos: services.Repositories,
				Git: smokeGitGateway{gateway: gitinfra.New(gitinfra.Options{
					GitPath: "git",
					Repos:   services.Repositories,
				})},
				AgentExecutor: smokeAgentExecutor{executor: agent.New(agent.ExecutorOptions{
					Config: agent.ExecutorConfig{
						Vendor: *cfg.Agent.Vendor,
						Params: cfg.Agent.Params,
						Env:    cfg.Agent.Env,
					},
					Repos: services.Repositories,
				})},
				AllowAutoCommit: true,
				OpenPRStrategy:  config.OpenPRStrategyManual,
			})
			_, err := runner.ProcessNext(ctx, "smoke-worker")
			return err
		},
	})
	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Runtime.Start() error = %v", err)
	}
	t.Cleanup(func() {
		rt.Stop("test cleanup")
	})

	server := httptest.NewServer(api.NewHandler(api.Context{
		Config:               cfg,
		Runtime:              rt,
		TriggerSchedulerTick: rt.TriggerSchedulerTick,
	}))
	t.Cleanup(server.Close)

	configPath := writeCLIConfig(t, server.URL, "")

	exitCode, statusJSON, stderr := runApp(t, "status", "--json", "--config", configPath)
	if exitCode != 0 || stderr != "" {
		t.Fatalf("Run([status --json]) = (%d, %q), want (0, empty)", exitCode, stderr)
	}
	var statusPayload map[string]any
	if err := json.Unmarshal([]byte(statusJSON), &statusPayload); err != nil {
		t.Fatalf("json.Unmarshal(statusJSON) error = %v", err)
	}
	service, _ := statusPayload["service"].(map[string]any)
	if service == nil {
		t.Fatalf("status payload service = %#v, want object", statusPayload["service"])
	}
	if got := service["healthy"]; got != true {
		t.Fatalf("status payload service.healthy = %#v, want true", got)
	}
	if got := service["daemonMode"]; got != "foreground" {
		t.Fatalf("status payload service.daemonMode = %#v, want foreground", got)
	}

	exitCode, configJSON, stderr := runApp(t, "config", "show", "--json", "--config", configPath)
	if exitCode != 0 || stderr != "" {
		t.Fatalf("Run([config show --json]) = (%d, %q), want (0, empty)", exitCode, stderr)
	}
	var configPayload map[string]any
	if err := json.Unmarshal([]byte(configJSON), &configPayload); err != nil {
		t.Fatalf("json.Unmarshal(configJSON) error = %v", err)
	}
	daemonConfig, _ := configPayload["daemon"].(map[string]any)
	if daemonConfig == nil {
		t.Fatalf("config payload daemon = %#v, want object", configPayload["daemon"])
	}
	if got := daemonConfig["mode"]; got != string(cfg.Daemon.Mode) {
		t.Fatalf("config payload daemon.mode = %#v, want %q", got, cfg.Daemon.Mode)
	}
	if got := daemonConfig["workingDirectory"]; got != cfg.Daemon.WorkingDirectory {
		t.Fatalf("config payload daemon.workingDirectory = %#v, want %q", got, cfg.Daemon.WorkingDirectory)
	}

	exitCode, addJSON, stderr := runApp(t, "project", "add", repoPath, "--id", "smoke-project", "--repo", "acme/looper", "--base-branch", "main", "--json", "--config", configPath)
	if exitCode != 0 || stderr != "" {
		t.Fatalf("Run([project add ... --json]) = (%d, %q), want (0, empty)", exitCode, stderr)
	}
	assertJSONContains(t, addJSON, "id", "smoke-project")

	exitCode, projectListJSON, stderr := runApp(t, "project", "list", "--json", "--config", configPath)
	if exitCode != 0 || stderr != "" {
		t.Fatalf("Run([project list --json]) = (%d, %q), want (0, empty)", exitCode, stderr)
	}
	if !strings.Contains(projectListJSON, "smoke-project") {
		t.Fatalf("project list JSON = %q, want smoke-project", projectListJSON)
	}

	exitCode, workJSON, stderr := runApp(t, "work", "--project", "smoke-project", "--repo", "acme/looper", "--base-branch", "main", "--title", "Smoke run", "--prompt", "Create smoke output", "--json", "--config", configPath)
	if exitCode != 0 || stderr != "" {
		t.Fatalf("Run([work ... --json]) = (%d, %q), want (0, empty)", exitCode, stderr)
	}
	var workResult map[string]any
	if err := json.Unmarshal([]byte(workJSON), &workResult); err != nil {
		t.Fatalf("json.Unmarshal(workJSON) error = %v", err)
	}
	loopID, _ := workResult["id"].(string)
	if strings.TrimSpace(loopID) == "" {
		t.Fatalf("work result id = %#v, want non-empty loop id", workResult["id"])
	}

	deadline := time.Now().Add(20 * time.Second)
	for {
		exitCode, runListJSON, runStderr := runApp(t, "run", "list", "--loop", loopID, "--json", "--config", configPath)
		if exitCode != 0 || runStderr != "" {
			t.Fatalf("Run([run list --loop ... --json]) = (%d, %q), want (0, empty)", exitCode, runStderr)
		}
		var runList struct {
			Items []struct {
				Status string `json:"status"`
			} `json:"items"`
		}
		if err := json.Unmarshal([]byte(runListJSON), &runList); err != nil {
			t.Fatalf("json.Unmarshal(runListJSON) error = %v", err)
		}
		hasSuccess := false
		for _, item := range runList.Items {
			if item.Status == "success" {
				hasSuccess = true
				break
			}
		}
		if hasSuccess {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for successful run; last run list payload: %s", runListJSON)
		}
		time.Sleep(100 * time.Millisecond)
	}

	runRecord, err := rt.Services().Repositories.Runs.GetLatestByLoopID(context.Background(), loopID)
	if err != nil {
		t.Fatalf("Runs.GetLatestByLoopID() error = %v", err)
	}
	if runRecord == nil || runRecord.Status != "success" {
		t.Fatalf("run record = %#v, want success", runRecord)
	}

	worktrees, err := rt.Services().Repositories.Worktrees.ListByProject(context.Background(), "smoke-project")
	if err != nil {
		t.Fatalf("Worktrees.ListByProject() error = %v", err)
	}
	if len(worktrees) == 0 {
		t.Fatalf("Worktrees.ListByProject() = %#v, want at least one worktree", worktrees)
	}
	changedPath := filepath.Join(worktrees[0].WorktreePath, "smoke-output.txt")
	changed, err := os.ReadFile(changedPath)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", changedPath, err)
	}
	if !strings.Contains(string(changed), "smoke-change") {
		t.Fatalf("%s content = %q, want smoke-change", changedPath, string(changed))
	}
}

func initSampleGitRepo(t *testing.T) string {
	t.Helper()

	repoPath := filepath.Join(t.TempDir(), "sample-repo")
	if err := os.MkdirAll(repoPath, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s) error = %v", repoPath, err)
	}
	runGit(t, repoPath, "init", "-b", "main")
	runGit(t, repoPath, "config", "user.name", "Looper Smoke")
	runGit(t, repoPath, "config", "user.email", "smoke@looper.dev")
	if err := os.WriteFile(filepath.Join(repoPath, "README.md"), []byte("# smoke\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(README.md) error = %v", err)
	}
	runGit(t, repoPath, "add", "README.md")
	runGit(t, repoPath, "commit", "-m", "initial")
	return repoPath
}

func runGit(t *testing.T, repoPath string, args ...string) {
	t.Helper()

	commandArgs := append([]string{"-C", repoPath}, args...)
	out, err := exec.Command("git", commandArgs...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, string(out))
	}
}

type smokeGitGateway struct {
	gateway *gitinfra.Gateway
}

func (g smokeGitGateway) CreateWorktree(ctx context.Context, input worker.CreateWorktreeInput) (worker.CreateWorktreeResult, error) {
	record, err := g.gateway.CreateWorktree(ctx, gitinfra.CreateWorktreeInput{
		ProjectID:         input.ProjectID,
		RepoPath:          input.RepoPath,
		WorktreeRoot:      input.WorktreeRoot,
		Branch:            input.Branch,
		BaseBranch:        input.BaseBranch,
		PRNumber:          input.PRNumber,
		ProtectedBranches: append([]string{}, input.ProtectedBranches...),
		CheckoutMode:      gitinfra.CheckoutMode(input.CheckoutMode),
	})
	if err != nil {
		return worker.CreateWorktreeResult{}, err
	}
	return worker.CreateWorktreeResult{
		WorktreePath: record.WorktreePath,
		Branch:       record.Branch,
		BaseBranch:   strings.TrimSpace(derefString(record.BaseBranch)),
		HeadSHA:      strings.TrimSpace(derefString(record.HeadSHA)),
		WorktreeID:   record.ID,
	}, nil
}

func (g smokeGitGateway) PrepareWorktree(ctx context.Context, input worker.PrepareWorktreeInput) (worker.PrepareWorktreeResult, error) {
	prepared, err := g.gateway.PrepareWorktree(ctx, gitinfra.PrepareWorktreeInput{
		WorktreePath:    input.WorktreePath,
		Branch:          input.Branch,
		ExpectedHeadSHA: input.ExpectedHeadSHA,
		Remote:          input.Remote,
	})
	if err != nil {
		return worker.PrepareWorktreeResult{}, err
	}
	return worker.PrepareWorktreeResult{HeadSHA: prepared.HeadSHA, Clean: prepared.Clean}, nil
}

func (g smokeGitGateway) Push(ctx context.Context, input worker.PushInput) error {
	return g.gateway.Push(ctx, gitinfra.PushInput{
		WorktreePath:      input.WorktreePath,
		Branch:            input.Branch,
		Remote:            input.Remote,
		ProtectedBranches: append([]string{}, input.ProtectedBranches...),
	})
}

type smokeAgentExecutor struct {
	executor *agent.ConfiguredExecutor
}

func (e smokeAgentExecutor) Start(ctx context.Context, input worker.AgentRunInput) (worker.AgentExecution, error) {
	execution, err := e.executor.Start(ctx, agent.RunInput{
		ExecutionID:      input.ExecutionID,
		ProjectID:        input.ProjectID,
		LoopID:           input.LoopID,
		RunID:            input.RunID,
		Prompt:           input.Prompt,
		WorkingDirectory: input.WorkingDirectory,
		Timeout:          input.Timeout,
		Metadata:         input.Metadata,
		IdempotencyKey:   input.IdempotencyKey,
	})
	if err != nil {
		return nil, err
	}
	return smokeAgentExecution{execution: execution}, nil
}

type smokeAgentExecution struct {
	execution agent.Execution
}

func (e smokeAgentExecution) Wait(ctx context.Context) (worker.AgentResult, error) {
	result, err := e.execution.Wait(ctx)
	if err != nil {
		return worker.AgentResult{}, err
	}
	return worker.AgentResult{
		Status:       result.Status,
		Summary:      result.Summary,
		Stdout:       result.Stdout,
		ParseStatus:  result.ParseStatus,
		ChangedFiles: append([]string{}, result.ChangedFiles...),
		Commits:      append([]string{}, result.Commits...),
	}, nil
}

func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func writeCLIConfig(t *testing.T, baseURL string, localToken string) string {
	t.Helper()

	configPath := filepath.Join(t.TempDir(), "config.json")
	config := map[string]any{
		"server": map[string]any{
			"baseUrl":  baseURL,
			"authMode": "none",
		},
	}
	if localToken != "" {
		config["server"] = map[string]any{
			"baseUrl":    baseURL,
			"authMode":   "local-token",
			"localToken": localToken,
		}
	}

	raw, err := json.Marshal(config)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if err := os.WriteFile(configPath, raw, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	return configPath
}

func writeCLIConfigWithAgent(t *testing.T, baseURL string, vendor string, params map[string]any) string {
	t.Helper()

	configPath := filepath.Join(t.TempDir(), "config.json")
	configPayload := map[string]any{
		"server": map[string]any{
			"baseUrl":  baseURL,
			"authMode": "none",
		},
		"agent": map[string]any{
			"vendor": vendor,
			"params": params,
		},
	}

	raw, err := json.Marshal(configPayload)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if err := os.WriteFile(configPath, raw, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	return configPath
}

func writeEnvelope(t *testing.T, w http.ResponseWriter, payload any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		t.Fatalf("encode envelope: %v", err)
	}
}

func assertJSONContains(t *testing.T, raw string, key string, want any) {
	t.Helper()

	var decoded map[string]any
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		t.Fatalf("unmarshal stdout JSON: %v\nraw=%q", err, raw)
	}

	got, ok := decoded[key]
	if !ok {
		t.Fatalf("stdout JSON missing key %q: %#v", key, decoded)
	}

	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal got value: %v", err)
	}
	wantJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal want value: %v", err)
	}
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("stdout JSON %q = %s, want %s", key, gotJSON, wantJSON)
	}
}
