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

	pkgapi "github.com/powerformer/looper/pkg/api"
)

func runApp(t *testing.T, args ...string) (int, string, string) {
	t.Helper()

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := New(Deps{Stdout: stdout, Stderr: stderr})
	exitCode := app.Run(context.Background(), args)

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
		{args: []string{"daemon", "--help"}, subcommands: []string{"install  Install the managed daemon binary", "status   Show daemon status", "start    Start the daemon", "restart  Restart the daemon", "logs     Show daemon logs"}},
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

	for _, syntax := range []string{
		"--json",
		"--config <path>",
		"--host <host>",
		"--port <port>",
		"--db-path <path>",
		"--log-dir <path>",
		"--daemon-mode <mode>",
		"--bun-path <path>",
		"--git-path <path>",
		"--gh-path <path>",
		"--osascript-path <path>",
	} {
		if !strings.Contains(stdout, syntax) {
			t.Fatalf("Run([--help]) stdout = %q, want to contain %q", stdout, syntax)
		}
	}
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
		"--bun-path",
		"/opt/bun",
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
		"--bun-path",
		"/opt/bun",
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
			"tools":         map[string]any{"bun": true, "git": true, "gh": false, "osascript": true},
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
