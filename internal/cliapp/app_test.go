package cliapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/agent"
	"github.com/nexu-io/looper/internal/api"
	"github.com/nexu-io/looper/internal/config"
	gitinfra "github.com/nexu-io/looper/internal/infra/git"
	"github.com/nexu-io/looper/internal/network/client"
	looperdruntime "github.com/nexu-io/looper/internal/runtime"
	"github.com/nexu-io/looper/internal/version"
	"github.com/nexu-io/looper/internal/worker"
	pkgapi "github.com/nexu-io/looper/pkg/api"
	"github.com/spf13/cobra"
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

func runAppWithLookPath(t *testing.T, lookPath config.LookPathFunc, args ...string) (int, string, string) {
	t.Helper()

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := New(Deps{Stdout: stdout, Stderr: stderr, LookPath: lookPath})
	exitCode := app.Run(context.Background(), args)

	return exitCode, stdout.String(), stderr.String()
}

func TestCommandGroupHelpListsExpectedSubcommands(t *testing.T) {
	t.Parallel()

	tests := []struct {
		args        []string
		subcommands []string
	}{
		{args: []string{"project", "--help"}, subcommands: []string{"list    List projects", "add     Add a project", "remove  Remove a project"}},
		{args: []string{"config", "--help"}, subcommands: []string{"get       Get a config value", "set       Set a config value", "unset     Unset a config value", "validate  Validate the active config file", "show      Show active config", "edit      Edit the active config file", "migrate   Migrate a config file to canonical format"}},
		{args: []string{"daemon", "--help"}, subcommands: []string{"install  Install the managed daemon binary", "status   Show daemon status", "start    Start the daemon", "stop     Stop the daemon", "restart  Restart the daemon", "logs     Show daemon logs"}},
		{args: []string{"labels", "--help"}, subcommands: []string{"init  Initialize standard Looper GitHub labels"}},
		{args: []string{"loop", "--help"}, subcommands: []string{"list   List loops", "start  Start a loop", "pause  Pause a loop"}},
		{args: []string{"pr", "--help"}, subcommands: []string{"list    List pull requests", "show    Show a pull request", "status  Show pull request status"}},
		{args: []string{"run", "--help"}, subcommands: []string{"list             List runs", "stats            Show recent run stats", "reconcile-stale  Reconcile stale running runs"}},
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

func TestFixCreateAcceptsNumericPRRefFromCurrentProject(t *testing.T) {
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
			if got, want := body["type"], "fixer"; got != want {
				t.Fatalf("body.type = %#v, want %#v", got, want)
			}
			if got, want := body["repo"], "acme/looper"; got != want {
				t.Fatalf("body.repo = %#v, want %#v", got, want)
			}
			if got, want := body["prNumber"], float64(123); got != want {
				t.Fatalf("body.prNumber = %#v, want %#v", got, want)
			}
			writeEnvelope(t, w, pkgapi.Success("req_loop", map[string]any{"id": "loop_fix_1", "projectId": "project_1", "repo": "acme/looper", "prNumber": 123, "status": "queued"}))
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

	exitCode := app.Run(context.Background(), []string{"fix", "123", "--config", configPath})
	if exitCode != 0 {
		t.Fatalf("Run([fix 123]) exit code = %d, want 0", exitCode)
	}
	if got := stderr.String(); got != "" {
		t.Fatalf("Run([fix 123]) stderr = %q, want empty string", got)
	}
	if got := stdout.String(); !strings.Contains(got, "Fixer started") || !strings.Contains(got, "acme/looper#123") {
		t.Fatalf("Run([fix 123]) stdout = %q, want fixer summary for %q", got, "acme/looper#123")
	}
}

func TestPullRequestListShowsMergeabilityAndBlocker(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.URL.Path, "/api/v1/pull-requests"; got != want {
			t.Fatalf("request path = %q, want %q", got, want)
		}
		writeEnvelope(t, w, pkgapi.Success("req_prs", map[string]any{"items": []map[string]any{{
			"repo":           "acme/looper",
			"prNumber":       42,
			"title":          "Fix queue visibility",
			"mergeability":   "blocked",
			"blockingReason": "checks",
			"reviewState":    "APPROVED",
			"checksSummary":  "FAILURE",
			"reviewer":       "completed",
		}}}))
	}))
	defer server.Close()

	configPath := writeCLIConfig(t, server.URL, "")
	exitCode, stdout, stderr := runApp(t, "pr", "list", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run([pr list]) exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
	for _, want := range []string{"mergeability", "blocker", "blocked", "checks"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout = %q, want to contain %q", stdout, want)
		}
	}
}

func TestRunReconcileStaleOutputsHumanAndJSON(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.URL.Path, "/api/v1/runs/reconcile-stale"; got != want {
			t.Fatalf("request path = %q, want %q", got, want)
		}
		if got, want := r.Method, http.MethodPost; got != want {
			t.Fatalf("request method = %q, want %q", got, want)
		}
		writeEnvelope(t, w, pkgapi.Success("req_reconcile", map[string]any{
			"mode":                 "manual",
			"candidateRuns":        2,
			"interruptedRuns":      1,
			"loopsRequeued":        1,
			"queueItemsRequeued":   1,
			"queueItemsCancelled":  0,
			"cleanedExecutions":    1,
			"skippedUncertainRuns": 0,
			"runIds":               []string{"run_1"},
			"loopIds":              []string{"loop_1"},
			"executionIds":         []string{"exec_1"},
		}))
	}))
	defer server.Close()

	configPath := writeCLIConfig(t, server.URL, "")
	exitCode, stdout, stderr := runApp(t, "run", "reconcile-stale", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run([run reconcile-stale]) exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
	if stderr != "" {
		t.Fatalf("Run([run reconcile-stale]) stderr = %q, want empty string", stderr)
	}
	for _, want := range []string{"Stale runs reconciled", "mode", "manual", "interruptedRuns", "run_1", "exec_1"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout = %q, want to contain %q", stdout, want)
		}
	}

	exitCode, stdout, stderr = runApp(t, "run", "reconcile-stale", "--json", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run([run reconcile-stale --json]) exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
	if stderr != "" {
		t.Fatalf("Run([run reconcile-stale --json]) stderr = %q, want empty string", stderr)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		t.Fatalf("json.Unmarshal(stdout) error = %v", err)
	}
	if got := payload["mode"]; got != "manual" {
		t.Fatalf("payload.mode = %#v, want manual", got)
	}
	if got := payload["loopsRequeued"]; got != float64(1) {
		t.Fatalf("payload.loopsRequeued = %#v, want 1", got)
	}
}

func TestFixCreateRequiresExplicitProjectWhenCurrentProjectIsAmbiguous(t *testing.T) {
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

	exitCode := app.Run(context.Background(), []string{"fix", "123", "--config", configPath})
	if exitCode == 0 {
		t.Fatalf("Run([fix 123]) exit code = 0, want non-zero")
	}
	if got := stdout.String(); got != "" {
		t.Fatalf("Run([fix 123]) stdout = %q, want empty string", got)
	}
	if got := stderr.String(); !strings.Contains(got, "--project is required") {
		t.Fatalf("Run([fix 123]) stderr = %q, want to contain %q", got, "--project is required")
	}
}

func TestLabelsInitDryRunPrintsPlannedChanges(t *testing.T) {
	t.Parallel()

	configPath := writeDaemonCLIConfig(t, "http://127.0.0.1:1")
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	var calls []string
	app := New(Deps{
		Stdout: stdout,
		Stderr: stderr,
		Getwd: func() (string, error) {
			return t.TempDir(), nil
		},
		RunCommand: func(ctx context.Context, command string, args []string, timeout time.Duration) (commandExecutionResult, error) {
			_ = ctx
			_ = timeout
			calls = append(calls, command+" "+strings.Join(args, " "))
			switch strings.Join(args, " ") {
			case "auth status --hostname github.com":
				return commandExecutionResult{}, nil
			case "label list --repo acme/looper --limit 1000 --json name,color,description":
				return commandExecutionResult{Stdout: `[{"name":"looper:plan","color":"5319e7","description":"Picked up automatically by planner"}]`}, nil
			default:
				t.Fatalf("unexpected command: %s %s", command, strings.Join(args, " "))
				return commandExecutionResult{}, nil
			}
		},
	})

	exitCode := app.Run(context.Background(), []string{"labels", "init", "--repo", "acme/looper", "--dry-run", "--gh-path", "/fake/gh", "--config", configPath})
	if exitCode != 0 {
		t.Fatalf("Run(labels init --dry-run) exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("Run(labels init --dry-run) stderr = %q, want empty string", stderr.String())
	}
	for _, want := range []string{"Previewing Looper labels for acme/looper", "skipped looper:plan", "created looper:spec-reviewing", "created looper:spec-ready", "Summary: created=3 updated=0 skipped=1 failed=0"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("stdout = %q, want to contain %q", stdout.String(), want)
		}
	}
	for _, call := range calls {
		if strings.Contains(call, "label create") || strings.Contains(call, "label edit") {
			t.Fatalf("dry run executed mutation command: %s", call)
		}
	}
}

func TestLabelsInitRequiresAuthenticatedGH(t *testing.T) {
	t.Parallel()

	configPath := writeDaemonCLIConfig(t, "http://127.0.0.1:1")
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := New(Deps{
		Stdout: stdout,
		Stderr: stderr,
		Getwd: func() (string, error) {
			return t.TempDir(), nil
		},
		RunCommand: func(ctx context.Context, command string, args []string, timeout time.Duration) (commandExecutionResult, error) {
			_ = ctx
			_ = command
			_ = timeout
			if strings.Join(args, " ") != "auth status --hostname github.com" {
				t.Fatalf("unexpected command args: %s", strings.Join(args, " "))
			}
			return commandExecutionResult{ExitCode: 1, Stderr: "not logged in"}, nil
		},
	})

	exitCode := app.Run(context.Background(), []string{"labels", "init", "--repo", "acme/looper", "--gh-path", "/fake/gh", "--config", configPath})
	if exitCode == 0 {
		t.Fatalf("Run(labels init unauthenticated) exit code = %d, want non-zero", exitCode)
	}
	if stdout.Len() != 0 {
		t.Fatalf("Run(labels init unauthenticated) stdout = %q, want empty string", stdout.String())
	}
	if !strings.Contains(stderr.String(), "gh is not authenticated; run `gh auth login` and retry") {
		t.Fatalf("stderr = %q, want actionable auth error", stderr.String())
	}
}

func TestLabelsInitFailsAndPrintsGHStderrWhenMutationFails(t *testing.T) {
	t.Parallel()

	configPath := writeDaemonCLIConfig(t, "http://127.0.0.1:1")
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := New(Deps{
		Stdout: stdout,
		Stderr: stderr,
		Getwd: func() (string, error) {
			return t.TempDir(), nil
		},
		RunCommand: func(ctx context.Context, command string, args []string, timeout time.Duration) (commandExecutionResult, error) {
			_ = ctx
			_ = command
			_ = timeout
			switch strings.Join(args, " ") {
			case "auth status --hostname github.com":
				return commandExecutionResult{}, nil
			case "label list --repo acme/looper --limit 1000 --json name,color,description":
				return commandExecutionResult{Stdout: `[{"name":"looper:plan","color":"5319e7","description":"Picked up automatically by planner"}]`}, nil
			case "label create looper:spec-reviewing --repo acme/looper --color 1d76db --description Spec PR is under review":
				return commandExecutionResult{ExitCode: 1, Stderr: "GraphQL: Resource not accessible by integration"}, nil
			case "label create looper:spec-ready --repo acme/looper --color 0e8a16 --description Spec PR is ready for implementation":
				return commandExecutionResult{Stdout: "{}"}, nil
			case "label create looper:needs-human --repo acme/looper --color d93f0b --description Looper requires manual intervention":
				return commandExecutionResult{Stdout: "{}"}, nil
			default:
				t.Fatalf("unexpected command args: %s", strings.Join(args, " "))
				return commandExecutionResult{}, nil
			}
		},
	})

	exitCode := app.Run(context.Background(), []string{"labels", "init", "--repo", "acme/looper", "--gh-path", "/fake/gh", "--config", configPath})
	if exitCode == 0 {
		t.Fatalf("Run(labels init) exit code = 0, want non-zero")
	}
	if !strings.Contains(stdout.String(), "failed looper:spec-reviewing: gh exited with code 1: GraphQL: Resource not accessible by integration") {
		t.Fatalf("stdout = %q, want failed label with gh stderr", stdout.String())
	}
	if !strings.Contains(stdout.String(), "Summary: created=2 updated=0 skipped=1 failed=1") {
		t.Fatalf("stdout = %q, want failed summary", stdout.String())
	}
	if !strings.Contains(stderr.String(), "initialize labels for acme/looper: 1 label mutation(s) failed") {
		t.Fatalf("stderr = %q, want command failure", stderr.String())
	}
}

func TestLabelsAuthHostname(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		repo string
		want string
	}{
		{name: "empty", repo: "", want: "github.com"},
		{name: "owner name", repo: "acme/looper", want: "github.com"},
		{name: "host owner name", repo: "github.example.com/acme/looper", want: "github.example.com"},
		{name: "trim host", repo: " github.example.com/acme/looper ", want: "github.example.com"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := labelsAuthHostname(tc.repo); got != tc.want {
				t.Fatalf("labelsAuthHostname(%q) = %q, want %q", tc.repo, got, tc.want)
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
	if !strings.Contains(stdout, "Config path (`~/.looper/config.toml` by default; also supports .yaml, .yml, and .json)") {
		t.Fatalf("Run([--help]) stdout = %q, want canonical config flag description", stdout)
	}
}

func TestConfigHelpUsesCanonicalReviewerExamples(t *testing.T) {
	t.Parallel()

	exitCode, stdout, stderr := runApp(t, "config", "--help")
	if exitCode != 0 {
		t.Fatalf("Run([config --help]) exit code = %d, want 0", exitCode)
	}
	if stderr != "" {
		t.Fatalf("Run([config --help]) stderr = %q, want empty string", stderr)
	}
	for _, want := range []string{
		"roles.reviewer.behavior.reviewEvents.clean",
		"$ looper config set roles.reviewer.behavior.reviewEvents.clean APPROVE",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("Run([config --help]) stdout = %q, want to contain %q", stdout, want)
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
		"printf 'https://github.com/nexu-io/looper/issues/42\\n'",
		"printf 'https://github.com/nexu-io/looper/issues/321\\n'",
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
	if got, want := stdout, "https://github.com/nexu-io/looper/issues/321\n"; got != want {
		t.Fatalf("Run([feedback ...]) stdout = %q, want %q", got, want)
	}

	exitCode, stdout, stderr = runApp(t, "feedback", "Great", "tool", "--title", "CLI Feedback", "--json", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run([feedback ... --json]) exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
	if stderr != "" {
		t.Fatalf("Run([feedback ... --json]) stderr = %q, want empty string", stderr)
	}
	assertJSONContains(t, stdout, "repo", "nexu-io/looper")
	assertJSONContains(t, stdout, "titleHint", "CLI Feedback")
	assertJSONContains(t, stdout, "message", "Great tool")
	assertJSONContains(t, stdout, "issueUrl", "https://github.com/nexu-io/looper/issues/321")
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
		"--planner-agent-timeout-seconds",
		"1200",
		"--worker-agent-timeout-seconds=3600",
		"--reviewer-agent-timeout-seconds",
		"1800",
		"--fixer-agent-timeout-seconds=900",
		"--reviewer-loop-enabled=false",
		"--reviewer-enable-self-review=true",
		"--no-custom-instructions",
		"false",
		"--no-custom-instructions=true",
		"--reviewer-quiet-period-seconds",
		"60",
		"--reviewer-min-publish-interval-seconds",
		"300",
		"--reviewer-max-iterations-per-pr",
		"7",
		"--reviewer-max-iterations-per-head=2",
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
		"--planner-agent-timeout-seconds",
		"1200",
		"--worker-agent-timeout-seconds=3600",
		"--reviewer-agent-timeout-seconds",
		"1800",
		"--fixer-agent-timeout-seconds=900",
		"--reviewer-loop-enabled=false",
		"--reviewer-enable-self-review=true",
		"--no-custom-instructions",
		"false",
		"--no-custom-instructions=true",
		"--reviewer-quiet-period-seconds",
		"60",
		"--reviewer-min-publish-interval-seconds",
		"300",
		"--reviewer-max-iterations-per-pr",
		"7",
		"--reviewer-max-iterations-per-head=2",
	}

	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("ExtractConfigArgs() = %#v, want %#v", got, want)
	}
}

func TestExtractConfigArgsForwardsCanonicalConfigFlags(t *testing.T) {
	t.Parallel()

	got := ExtractConfigArgs([]string{
		"status",
		"--package-auto-upgrade-enabled=false",
		"--roles-fixer-triggers-author-filter",
		"any",
		"--roles-reviewer-behavior-review-events-clean=APPROVE",
		"--roles-reviewer-discovery-triggers-enable-self-review",
		"true",
		"--roles-reviewer-behavior-loop-enabled-by-default=false",
		"--instructions-enabled=true",
		"--project",
		"demo",
	})

	want := []string{
		"--package-auto-upgrade-enabled=false",
		"--roles-fixer-triggers-author-filter",
		"any",
		"--roles-reviewer-behavior-review-events-clean=APPROVE",
		"--roles-reviewer-discovery-triggers-enable-self-review",
		"true",
		"--roles-reviewer-behavior-loop-enabled-by-default=false",
		"--instructions-enabled=true",
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

func TestStatusAcceptsReviewerLoopConfigOverrideFlag(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/status" {
			t.Fatalf("request path = %q, want %q", r.URL.Path, "/api/v1/status")
		}
		writeEnvelope(t, w, pkgapi.Success("req_status", map[string]any{"healthy": true}))
	}))
	defer server.Close()

	configPath := writeCLIConfig(t, server.URL, "")
	exitCode, _, stderr := runApp(t, "status", "--reviewer-loop-enabled=false", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run([status --reviewer-loop-enabled=false]) exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
	if !strings.Contains(stderr, `warning: deprecated CLI flag "--reviewer-loop-enabled" is accepted for now; use "--roles-reviewer-behavior-loop-enabled-by-default" instead`) {
		t.Fatalf("Run([status --reviewer-loop-enabled=false]) stderr = %q, want deprecation warning", stderr)
	}
}

func TestStatusAcceptsReviewerEnableSelfReviewConfigOverrideFlag(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/status" {
			t.Fatalf("request path = %q, want %q", r.URL.Path, "/api/v1/status")
		}
		writeEnvelope(t, w, pkgapi.Success("req_status", map[string]any{"healthy": true}))
	}))
	defer server.Close()

	configPath := writeCLIConfig(t, server.URL, "")
	exitCode, _, stderr := runApp(t, "status", "--reviewer-enable-self-review=true", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run([status --reviewer-enable-self-review=true]) exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
	if !strings.Contains(stderr, `warning: deprecated CLI flag "--reviewer-enable-self-review" is accepted for now; use "--roles-reviewer-discovery-triggers-enable-self-review" instead`) {
		t.Fatalf("Run([status --reviewer-enable-self-review=true]) stderr = %q, want deprecation warning", stderr)
	}
}

func TestStatusAcceptsCanonicalReviewerConfigOverrideFlags(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/status" {
			t.Fatalf("request path = %q, want %q", r.URL.Path, "/api/v1/status")
		}
		writeEnvelope(t, w, pkgapi.Success("req_status", map[string]any{"healthy": true}))
	}))
	defer server.Close()

	configPath := writeCLIConfig(t, server.URL, "")
	exitCode, _, stderr := runApp(t,
		"status",
		"--roles-reviewer-discovery-triggers-enable-self-review=true",
		"--roles-reviewer-behavior-review-events-clean=APPROVE",
		"--roles-reviewer-behavior-loop-enabled-by-default=false",
		"--roles-fixer-triggers-author-filter=any",
		"--package-auto-upgrade-enabled=false",
		"--instructions-enabled=true",
		"--config", configPath,
	)
	if exitCode != 0 {
		t.Fatalf("Run([status canonical config flags]) exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
	if stderr != "" {
		t.Fatalf("Run([status canonical config flags]) stderr = %q, want empty string", stderr)
	}
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

func TestProjectAddResolvesRelativePathsBeforePosting(t *testing.T) {
	root := t.TempDir()
	originalCWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("get current working directory: %v", err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatalf("change working directory to temp root: %v", err)
	}
	root, err = os.Getwd()
	if err != nil {
		t.Fatalf("get temp working directory: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(originalCWD); err != nil {
			t.Fatalf("restore working directory: %v", err)
		}
	})

	wantRepoPath := filepath.Join(root, "repo")
	wantWorktreeRoot := filepath.Join(root, "worktrees")

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
		if got := body["repoPath"]; got != wantRepoPath {
			t.Fatalf("body.repoPath = %#v, want %#v", got, wantRepoPath)
		}
		if got := body["worktreeRoot"]; got != wantWorktreeRoot {
			t.Fatalf("body.worktreeRoot = %#v, want %#v", got, wantWorktreeRoot)
		}

		writeEnvelope(t, w, pkgapi.Success("req_project", map[string]any{"id": "project_1", "repoPath": wantRepoPath}))
	}))
	defer server.Close()

	configPath := writeCLIConfig(t, server.URL, "")
	exitCode, _, stderr := runApp(t, "project", "add", "repo", "--worktree-root", "worktrees", "--json", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run([project add relative paths --json]) exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
	if stderr != "" {
		t.Fatalf("Run([project add relative paths --json]) stderr = %q, want empty string", stderr)
	}
}

func TestProjectRemoveForceDeletesResolvedProject(t *testing.T) {
	t.Parallel()

	var seenList atomic.Bool
	var seenDelete atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects":
			seenList.Store(true)
			writeEnvelope(t, w, pkgapi.Success("req_projects", map[string]any{"items": []map[string]any{{"id": "project_1", "name": "Looper", "repoPath": "/tmp/repo", "baseBranch": "main", "updatedAt": "2026-04-20T10:00:00.000Z"}}}))
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v1/projects/project_1":
			seenDelete.Store(true)
			writeEnvelope(t, w, pkgapi.Success("req_project_remove", map[string]any{"id": "project_1", "name": "Looper", "repoPath": "/tmp/repo", "baseBranch": "main", "updatedAt": "2026-04-20T10:00:00.000Z"}))
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	configPath := writeCLIConfig(t, server.URL, "")
	exitCode, stdout, stderr := runApp(t, "project", "remove", "Looper", "--force", "--json", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run([project remove ... --force --json]) exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
	if stderr != "" {
		t.Fatalf("Run([project remove ... --force --json]) stderr = %q, want empty string", stderr)
	}
	if !seenList.Load() || !seenDelete.Load() {
		t.Fatalf("requests seen list=%v delete=%v, want both", seenList.Load(), seenDelete.Load())
	}
	assertJSONContains(t, stdout, "id", "project_1")
}

func TestProjectRemovePromptsForConfirmation(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects":
			writeEnvelope(t, w, pkgapi.Success("req_projects", map[string]any{"items": []map[string]any{{"id": "project_1", "name": "Looper", "repoPath": "/tmp/repo", "baseBranch": "main", "updatedAt": "2026-04-20T10:00:00.000Z"}}}))
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v1/projects/project_1":
			writeEnvelope(t, w, pkgapi.Success("req_project_remove", map[string]any{"id": "project_1", "name": "Looper", "repoPath": "/tmp/repo", "baseBranch": "main", "updatedAt": "2026-04-20T10:00:00.000Z"}))
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	configPath := writeCLIConfig(t, server.URL, "")
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := New(Deps{Stdin: strings.NewReader("project_1\n"), Stdout: stdout, Stderr: stderr})
	exitCode := app.Run(context.Background(), []string{"project", "remove", "project_1", "--config", configPath})
	if exitCode != 0 {
		t.Fatalf("Run([project remove ...]) exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if !strings.Contains(stderr.String(), "Type \"project_1\" to confirm") {
		t.Fatalf("stderr = %q, want confirmation prompt", stderr.String())
	}
	if !strings.Contains(stdout.String(), "Project removed") {
		t.Fatalf("stdout = %q, want removal summary", stdout.String())
	}
}

func TestProjectRemoveMissingProjectReturnsClearError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.Method, http.MethodGet; got != want {
			t.Fatalf("request method = %q, want %q", got, want)
		}
		writeEnvelope(t, w, pkgapi.Success("req_projects", map[string]any{"items": []map[string]any{{"id": "project_1", "name": "Looper", "repoPath": "/tmp/repo"}}}))
	}))
	defer server.Close()

	configPath := writeCLIConfig(t, server.URL, "")
	exitCode, stdout, stderr := runApp(t, "project", "remove", "missing", "--force", "--config", configPath)
	if exitCode == 0 {
		t.Fatalf("Run([project remove missing --force]) exit code = %d, want non-zero", exitCode)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "project not found: missing") {
		t.Fatalf("stderr = %q, want not found error", stderr)
	}
}

func TestProjectRemoveResolveIDDoesNotFallBackToName(t *testing.T) {
	t.Parallel()

	projects := []projectOutput{{ID: "project_1", Name: "missing"}}
	_, err := resolveProjectByIdentifier(projects, projectRemoveIdentifierValue{value: "missing", source: projectIdentifierSourceID})
	if err == nil {
		t.Fatal("resolveProjectByIdentifier(--id missing) error = nil, want project not found")
	}
	if !strings.Contains(err.Error(), "project not found: missing") {
		t.Fatalf("resolveProjectByIdentifier(--id missing) error = %q, want not found", err.Error())
	}
}

func TestProjectRemoveResolveNameDoesNotMatchIDFirst(t *testing.T) {
	t.Parallel()

	projects := []projectOutput{{ID: "Looper", Name: "Other"}, {ID: "project_1", Name: "Looper"}}
	project, err := resolveProjectByIdentifier(projects, projectRemoveIdentifierValue{value: "Looper", source: projectIdentifierSourceName})
	if err != nil {
		t.Fatalf("resolveProjectByIdentifier(--name Looper) error = %v, want nil", err)
	}
	if got, want := project.ID, "project_1"; got != want {
		t.Fatalf("resolveProjectByIdentifier(--name Looper) ID = %q, want %q", got, want)
	}
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

func TestConfigSetGetUnsetAllowRiskyFixes(t *testing.T) {
	configPath := writeEditableCLIConfig(t)

	exitCode, stdout, stderr := runApp(t, "config", "set", "defaults.allowRiskyFixes", "true", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run([config set ...]) exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
	if !strings.Contains(stdout, "Set defaults.allowRiskyFixes") {
		t.Fatalf("stdout = %q, want set confirmation", stdout)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q, want empty", stderr)
	}

	exitCode, stdout, stderr = runApp(t, "config", "get", "defaults.allowRiskyFixes", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run([config get ...]) exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
	if strings.TrimSpace(stdout) != "true" {
		t.Fatalf("stdout = %q, want true", stdout)
	}

	exitCode, stdout, stderr = runApp(t, "config", "unset", "defaults.allowRiskyFixes", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run([config unset ...]) exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
	if !strings.Contains(stdout, "Unset defaults.allowRiskyFixes") {
		t.Fatalf("stdout = %q, want unset confirmation", stdout)
	}

	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if strings.Contains(string(raw), "allowRiskyFixes") {
		t.Fatalf("config = %s, want allowRiskyFixes removed", raw)
	}
}

func TestConfigGetReadsConfigFileLayer(t *testing.T) {
	configPath := writeEditableCLIConfig(t)
	t.Setenv("LOOPER_FIX_ALL_PULL_REQUESTS", "true")

	exitCode, stdout, stderr := runApp(t, "config", "get", "defaults.fixAllPullRequests", "--fix-all-pull-requests", "true", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run([config get ...]) exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
	if strings.TrimSpace(stdout) != "false" {
		t.Fatalf("stdout = %q, want config file value false", stdout)
	}
}

func TestConfigGetSupportsInstructionKeys(t *testing.T) {
	configPath := writeEditableCLIConfigWithPayload(t, map[string]any{
		"notifications": map[string]any{
			"osascript": map[string]any{"enabled": false},
		},
		"instructions": map[string]any{
			"enabled":  false,
			"maxBytes": 4096,
		},
		"roles": map[string]any{
			"planner":  map[string]any{"instructions": "plan carefully"},
			"reviewer": map[string]any{"instructions": "review carefully"},
			"fixer":    map[string]any{"instructions": "fix carefully"},
			"worker":   map[string]any{"instructions": "work carefully"},
		},
	})

	tests := map[string]string{
		"instructions.enabled":        "false",
		"instructions.maxBytes":       "4096",
		"roles.planner.instructions":  "plan carefully",
		"roles.reviewer.instructions": "review carefully",
		"roles.fixer.instructions":    "fix carefully",
		"roles.worker.instructions":   "work carefully",
	}
	for key, want := range tests {
		key, want := key, want
		t.Run(key, func(t *testing.T) {
			exitCode, stdout, stderr := runApp(t, "config", "get", key, "--config", configPath)
			if exitCode != 0 {
				t.Fatalf("Run([config get %s]) exit code = %d, want 0; stderr=%q", key, exitCode, stderr)
			}
			if strings.TrimSpace(stdout) != want {
				t.Fatalf("stdout = %q, want %q", stdout, want)
			}
		})
	}
}

func TestConfigSetUnsetInstructionKeys(t *testing.T) {
	configPath := writeEditableCLIConfig(t)

	exitCode, stdout, stderr := runApp(t, "config", "set", "roles.reviewer.instructions", "review carefully", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run([config set reviewer instructions]) exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
	if !strings.Contains(stdout, "Set roles.reviewer.instructions") {
		t.Fatalf("stdout = %q, want set confirmation", stdout)
	}

	exitCode, stdout, stderr = runApp(t, "config", "get", "roles.reviewer.instructions", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run([config get reviewer instructions]) exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
	if strings.TrimSpace(stdout) != "review carefully" {
		t.Fatalf("stdout = %q, want reviewer instructions", stdout)
	}

	exitCode, stdout, stderr = runApp(t, "config", "set", "instructions.maxBytes", "4096", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run([config set instructions.maxBytes]) exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
	if !strings.Contains(stdout, "Set instructions.maxBytes") {
		t.Fatalf("stdout = %q, want set confirmation", stdout)
	}

	exitCode, stdout, stderr = runApp(t, "config", "unset", "roles.reviewer.instructions", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run([config unset reviewer instructions]) exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
	if !strings.Contains(stdout, "Unset roles.reviewer.instructions") {
		t.Fatalf("stdout = %q, want unset confirmation", stdout)
	}

	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if strings.Contains(string(raw), "review carefully") {
		t.Fatalf("config = %s, want reviewer instructions removed", raw)
	}
}

func TestConfigSetSupportsCanonicalReviewerDiscoveryKeys(t *testing.T) {
	configPath := writeEditableCLIConfigWithPayload(t, map[string]any{
		"notifications": map[string]any{
			"osascript": map[string]any{"enabled": false},
		},
		"roles": map[string]any{
			"reviewer": map[string]any{
				"autoDiscovery": false,
			},
		},
	})

	tests := []struct {
		key    string
		value  string
		assert func(t *testing.T, partial config.PartialConfig)
	}{
		{
			key:   "roles.reviewer.discovery.autoDiscovery",
			value: "true",
			assert: func(t *testing.T, partial config.PartialConfig) {
				t.Helper()
				if partial.Roles == nil || partial.Roles.Reviewer == nil || partial.Roles.Reviewer.Discovery == nil || partial.Roles.Reviewer.Discovery.AutoDiscovery == nil || !*partial.Roles.Reviewer.Discovery.AutoDiscovery {
					t.Fatalf("canonical reviewer autoDiscovery missing: %#v", partial.Roles)
				}
				if partial.Roles.Reviewer.AutoDiscovery != nil {
					t.Fatalf("legacy reviewer autoDiscovery should be cleared: %#v", partial.Roles.Reviewer)
				}
			},
		},
		{
			key:   "roles.reviewer.discovery.triggers.requireReviewRequest",
			value: "true",
			assert: func(t *testing.T, partial config.PartialConfig) {
				t.Helper()
				if partial.Roles == nil || partial.Roles.Reviewer == nil || partial.Roles.Reviewer.Discovery == nil || partial.Roles.Reviewer.Discovery.Triggers == nil || partial.Roles.Reviewer.Discovery.Triggers.RequireReviewRequest == nil || !*partial.Roles.Reviewer.Discovery.Triggers.RequireReviewRequest {
					t.Fatalf("canonical reviewer trigger missing: %#v", partial.Roles)
				}
				if partial.Roles.Reviewer.Triggers != nil && partial.Roles.Reviewer.Triggers.RequireReviewRequest != nil {
					t.Fatalf("legacy reviewer trigger should be cleared: %#v", partial.Roles.Reviewer.Triggers)
				}
			},
		},
		{
			key:   "roles.reviewer.discovery.triggers.includeDrafts",
			value: "true",
			assert: func(t *testing.T, partial config.PartialConfig) {
				t.Helper()
				if partial.Roles == nil || partial.Roles.Reviewer == nil || partial.Roles.Reviewer.Discovery == nil || partial.Roles.Reviewer.Discovery.Triggers == nil || partial.Roles.Reviewer.Discovery.Triggers.IncludeDrafts == nil || !*partial.Roles.Reviewer.Discovery.Triggers.IncludeDrafts {
					t.Fatalf("canonical reviewer trigger missing: %#v", partial.Roles)
				}
				if partial.Roles.Reviewer.Triggers != nil && partial.Roles.Reviewer.Triggers.IncludeDrafts != nil {
					t.Fatalf("legacy reviewer trigger should be cleared: %#v", partial.Roles.Reviewer.Triggers)
				}
			},
		},
		{
			key:   "roles.reviewer.discovery.triggers.enableSelfReview",
			value: "true",
			assert: func(t *testing.T, partial config.PartialConfig) {
				t.Helper()
				if partial.Roles == nil || partial.Roles.Reviewer == nil || partial.Roles.Reviewer.Discovery == nil || partial.Roles.Reviewer.Discovery.Triggers == nil || partial.Roles.Reviewer.Discovery.Triggers.EnableSelfReview == nil || !*partial.Roles.Reviewer.Discovery.Triggers.EnableSelfReview {
					t.Fatalf("canonical reviewer trigger missing: %#v", partial.Roles)
				}
				if partial.Roles.Reviewer.Triggers != nil && partial.Roles.Reviewer.Triggers.EnableSelfReview != nil {
					t.Fatalf("legacy reviewer trigger should be cleared: %#v", partial.Roles.Reviewer.Triggers)
				}
			},
		},
		{
			key:   "roles.reviewer.discovery.triggers.labels",
			value: "looper:review,looper:review-extra",
			assert: func(t *testing.T, partial config.PartialConfig) {
				t.Helper()
				if partial.Roles == nil || partial.Roles.Reviewer == nil || partial.Roles.Reviewer.Discovery == nil || partial.Roles.Reviewer.Discovery.Triggers == nil || partial.Roles.Reviewer.Discovery.Triggers.Labels == nil {
					t.Fatalf("canonical reviewer trigger labels missing: %#v", partial.Roles)
				}
				if got := *partial.Roles.Reviewer.Discovery.Triggers.Labels; !reflect.DeepEqual(got, []string{"looper:review", "looper:review-extra"}) {
					t.Fatalf("canonical reviewer trigger labels = %#v, want %#v", got, []string{"looper:review", "looper:review-extra"})
				}
				if partial.Roles.Reviewer.Triggers != nil && partial.Roles.Reviewer.Triggers.Labels != nil {
					t.Fatalf("legacy reviewer trigger labels should be cleared: %#v", partial.Roles.Reviewer.Triggers)
				}
			},
		},
		{
			key:   "roles.reviewer.discovery.triggers.labelMode",
			value: "any",
			assert: func(t *testing.T, partial config.PartialConfig) {
				t.Helper()
				if partial.Roles == nil || partial.Roles.Reviewer == nil || partial.Roles.Reviewer.Discovery == nil || partial.Roles.Reviewer.Discovery.Triggers == nil || partial.Roles.Reviewer.Discovery.Triggers.LabelMode == nil || *partial.Roles.Reviewer.Discovery.Triggers.LabelMode != config.LabelModeAny {
					t.Fatalf("canonical reviewer trigger labelMode missing: %#v", partial.Roles)
				}
				if partial.Roles.Reviewer.Triggers != nil && partial.Roles.Reviewer.Triggers.LabelMode != nil {
					t.Fatalf("legacy reviewer trigger labelMode should be cleared: %#v", partial.Roles.Reviewer.Triggers)
				}
			},
		},
		{
			key:   "roles.reviewer.discovery.specReview.includeReviewingLabel",
			value: "false",
			assert: func(t *testing.T, partial config.PartialConfig) {
				t.Helper()
				if partial.Roles == nil || partial.Roles.Reviewer == nil || partial.Roles.Reviewer.Discovery == nil || partial.Roles.Reviewer.Discovery.SpecReview == nil || partial.Roles.Reviewer.Discovery.SpecReview.IncludeReviewingLabel == nil || *partial.Roles.Reviewer.Discovery.SpecReview.IncludeReviewingLabel {
					t.Fatalf("canonical reviewer specReview missing: %#v", partial.Roles)
				}
				if partial.Roles.Reviewer.SpecReview != nil && partial.Roles.Reviewer.SpecReview.IncludeReviewingLabel != nil {
					t.Fatalf("legacy reviewer specReview should be cleared: %#v", partial.Roles.Reviewer.SpecReview)
				}
			},
		},
		{
			key:   "roles.reviewer.discovery.specReview.reviewingLabel",
			value: "looper:spec-reviewing",
			assert: func(t *testing.T, partial config.PartialConfig) {
				t.Helper()
				if partial.Roles == nil || partial.Roles.Reviewer == nil || partial.Roles.Reviewer.Discovery == nil || partial.Roles.Reviewer.Discovery.SpecReview == nil || partial.Roles.Reviewer.Discovery.SpecReview.ReviewingLabel == nil || *partial.Roles.Reviewer.Discovery.SpecReview.ReviewingLabel != "looper:spec-reviewing" {
					t.Fatalf("canonical reviewer specReview missing: %#v", partial.Roles)
				}
				if partial.Roles.Reviewer.SpecReview != nil && partial.Roles.Reviewer.SpecReview.ReviewingLabel != nil {
					t.Fatalf("legacy reviewer specReview should be cleared: %#v", partial.Roles.Reviewer.SpecReview)
				}
			},
		},
	}

	for _, testCase := range tests {
		testCase := testCase
		t.Run(testCase.key, func(t *testing.T) {
			exitCode, stdout, stderr := runApp(t, "config", "set", testCase.key, testCase.value, "--config", configPath)
			if exitCode != 0 {
				t.Fatalf("Run([config set %s]) exit code = %d, want 0; stderr=%q", testCase.key, exitCode, stderr)
			}
			if !strings.Contains(stdout, "Set "+testCase.key) {
				t.Fatalf("stdout = %q, want set confirmation for %s", stdout, testCase.key)
			}

			partial, present, err := config.ReadPartialConfigFile(configPath)
			if err != nil {
				t.Fatalf("ReadPartialConfigFile() error = %v", err)
			}
			if !present {
				t.Fatalf("ReadPartialConfigFile() present = false, want true")
			}
			testCase.assert(t, partial)
		})
	}
}

func TestConfigUnsetCanonicalReviewerDiscoveryKeysClearsCanonicalFields(t *testing.T) {
	configPath := writeEditableCLIConfigWithPayload(t, map[string]any{
		"notifications": map[string]any{
			"osascript": map[string]any{"enabled": false},
		},
		"roles": map[string]any{
			"reviewer": map[string]any{
				"discovery": map[string]any{
					"autoDiscovery": true,
					"triggers": map[string]any{
						"includeDrafts":        true,
						"requireReviewRequest": true,
						"labels":               []string{"looper:review", "looper:review-extra"},
						"labelMode":            "any",
					},
					"specReview": map[string]any{
						"includeReviewingLabel": true,
						"reviewingLabel":        "looper:spec-reviewing",
					},
				},
			},
		},
	})

	for _, key := range []string{
		"roles.reviewer.discovery.autoDiscovery",
		"roles.reviewer.discovery.triggers.includeDrafts",
		"roles.reviewer.discovery.triggers.requireReviewRequest",
		"roles.reviewer.discovery.triggers.enableSelfReview",
		"roles.reviewer.discovery.triggers.labels",
		"roles.reviewer.discovery.triggers.labelMode",
		"roles.reviewer.discovery.specReview.includeReviewingLabel",
		"roles.reviewer.discovery.specReview.reviewingLabel",
	} {
		exitCode, stdout, stderr := runApp(t, "config", "unset", key, "--config", configPath)
		if exitCode != 0 {
			t.Fatalf("Run([config unset %s]) exit code = %d, want 0; stderr=%q", key, exitCode, stderr)
		}
		if !strings.Contains(stdout, "Unset "+key) {
			t.Fatalf("stdout = %q, want unset confirmation for %s", stdout, key)
		}
	}

	partial, present, err := config.ReadPartialConfigFile(configPath)
	if err != nil {
		t.Fatalf("ReadPartialConfigFile() error = %v", err)
	}
	if !present {
		t.Fatalf("ReadPartialConfigFile() present = false, want true")
	}
	if partial.Roles == nil || partial.Roles.Reviewer == nil || partial.Roles.Reviewer.Discovery == nil {
		t.Fatalf("reviewer discovery missing after unset: %#v", partial.Roles)
	}
	if partial.Roles.Reviewer.Discovery.AutoDiscovery != nil {
		t.Fatalf("canonical reviewer autoDiscovery still set: %#v", partial.Roles.Reviewer.Discovery)
	}
	if partial.Roles.Reviewer.Discovery.Triggers != nil && partial.Roles.Reviewer.Discovery.Triggers.RequireReviewRequest != nil {
		t.Fatalf("canonical reviewer trigger still set: %#v", partial.Roles.Reviewer.Discovery.Triggers)
	}
	if partial.Roles.Reviewer.Discovery.Triggers != nil && partial.Roles.Reviewer.Discovery.Triggers.IncludeDrafts != nil {
		t.Fatalf("canonical reviewer includeDrafts still set: %#v", partial.Roles.Reviewer.Discovery.Triggers)
	}
	if partial.Roles.Reviewer.Discovery.Triggers != nil && partial.Roles.Reviewer.Discovery.Triggers.EnableSelfReview != nil {
		t.Fatalf("canonical reviewer enableSelfReview still set: %#v", partial.Roles.Reviewer.Discovery.Triggers)
	}
	if partial.Roles.Reviewer.Discovery.Triggers != nil && partial.Roles.Reviewer.Discovery.Triggers.Labels != nil {
		t.Fatalf("canonical reviewer labels still set: %#v", partial.Roles.Reviewer.Discovery.Triggers)
	}
	if partial.Roles.Reviewer.Discovery.Triggers != nil && partial.Roles.Reviewer.Discovery.Triggers.LabelMode != nil {
		t.Fatalf("canonical reviewer labelMode still set: %#v", partial.Roles.Reviewer.Discovery.Triggers)
	}
	if partial.Roles.Reviewer.Discovery.SpecReview != nil && partial.Roles.Reviewer.Discovery.SpecReview.IncludeReviewingLabel != nil {
		t.Fatalf("canonical reviewer includeReviewingLabel still set: %#v", partial.Roles.Reviewer.Discovery.SpecReview)
	}
	if partial.Roles.Reviewer.Discovery.SpecReview != nil && partial.Roles.Reviewer.Discovery.SpecReview.ReviewingLabel != nil {
		t.Fatalf("canonical reviewer specReview still set: %#v", partial.Roles.Reviewer.Discovery.SpecReview)
	}
}

func TestConfigSetPreservesLegacyReviewerRootWhenWritingUnrelatedKey(t *testing.T) {
	configPath := writeEditableCLIConfigWithPayload(t, map[string]any{
		"notifications": map[string]any{
			"osascript": map[string]any{"enabled": false},
		},
		"reviewer": map[string]any{
			"loop":  map[string]any{"enabledByDefault": false},
			"scope": "changed_files",
		},
	})

	exitCode, _, stderr := runApp(t, "config", "set", "defaults.allowRiskyFixes", "true", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run([config set defaults.allowRiskyFixes]) exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}

	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile(configPath) error = %v", err)
	}
	if !strings.Contains(string(raw), `"reviewer"`) {
		t.Fatalf("config = %s, want legacy reviewer root preserved", raw)
	}

	partial, present, err := config.ReadPartialConfigFile(configPath)
	if err != nil {
		t.Fatalf("ReadPartialConfigFile() error = %v", err)
	}
	if !present {
		t.Fatalf("ReadPartialConfigFile() present = false, want true")
	}
	if partial.LegacyReviewer == nil || partial.LegacyReviewer.Loop == nil || partial.LegacyReviewer.Loop.EnabledByDefault == nil || *partial.LegacyReviewer.Loop.EnabledByDefault {
		t.Fatalf("legacy reviewer loop missing after write: %#v", partial.LegacyReviewer)
	}
	if partial.LegacyReviewer.Scope == nil || *partial.LegacyReviewer.Scope != config.ReviewerScopeChangedFiles {
		t.Fatalf("legacy reviewer scope missing after write: %#v", partial.LegacyReviewer)
	}
}

func TestConfigSetRejectsInvalidKeyAndValue(t *testing.T) {
	configPath := writeEditableCLIConfig(t)

	exitCode, _, stderr := runApp(t, "config", "set", "defaults.missing", "true", "--config", configPath)
	if exitCode == 0 {
		t.Fatalf("Run([config set invalid key]) exit code = %d, want non-zero", exitCode)
	}
	if !strings.Contains(stderr, "unsupported config key") {
		t.Fatalf("stderr = %q, want unsupported key", stderr)
	}

	exitCode, _, stderr = runApp(t, "config", "set", "defaults.allowRiskyFixes", "maybe", "--config", configPath)
	if exitCode == 0 {
		t.Fatalf("Run([config set invalid value]) exit code = %d, want non-zero", exitCode)
	}
	if !strings.Contains(stderr, "not a boolean") {
		t.Fatalf("stderr = %q, want boolean error", stderr)
	}

	exitCode, _, stderr = runApp(t, "config", "set", "reviewer.reviewEvents.clean", "REQUEST_CHANGES", "--config", configPath)
	if exitCode == 0 {
		t.Fatalf("Run([config set invalid clean review event]) exit code = %d, want non-zero", exitCode)
	}
	if !strings.Contains(stderr, "reviewer.reviewEvents.clean") || !strings.Contains(stderr, "COMMENT, APPROVE") {
		t.Fatalf("stderr = %q, want clean review event enum error", stderr)
	}

	exitCode, _, stderr = runApp(t, "config", "set", "reviewer.reviewEvents.blocking", "APPROVE", "--config", configPath)
	if exitCode == 0 {
		t.Fatalf("Run([config set invalid blocking review event]) exit code = %d, want non-zero", exitCode)
	}
	if !strings.Contains(stderr, "reviewer.reviewEvents.blocking") || !strings.Contains(stderr, "COMMENT, REQUEST_CHANGES") {
		t.Fatalf("stderr = %q, want blocking review event enum error", stderr)
	}
}

func TestConfigUnsetLegacyReviewerReviewEventKeysClearsLegacyAndCanonicalFields(t *testing.T) {
	configPath := writeEditableCLIConfigWithPayload(t, map[string]any{
		"notifications": map[string]any{
			"osascript": map[string]any{"enabled": false},
		},
		"reviewer": map[string]any{
			"loop": map[string]any{"enabledByDefault": false},
			"reviewEvents": map[string]any{
				"clean":    "APPROVE",
				"blocking": "REQUEST_CHANGES",
			},
		},
		"roles": map[string]any{
			"reviewer": map[string]any{
				"behavior": map[string]any{
					"reviewEvents": map[string]any{
						"clean":    "COMMENT",
						"blocking": "COMMENT",
					},
				},
			},
		},
	})

	for _, key := range []string{"reviewer.reviewEvents.clean", "reviewer.reviewEvents.blocking"} {
		exitCode, stdout, stderr := runApp(t, "config", "unset", key, "--config", configPath)
		if exitCode != 0 {
			t.Fatalf("Run([config unset %s]) exit code = %d, want 0; stderr=%q", key, exitCode, stderr)
		}
		if !strings.Contains(stdout, "Unset "+key) {
			t.Fatalf("stdout = %q, want unset confirmation for %s", stdout, key)
		}
	}

	partial, present, err := config.ReadPartialConfigFile(configPath)
	if err != nil {
		t.Fatalf("ReadPartialConfigFile() error = %v", err)
	}
	if !present {
		t.Fatalf("ReadPartialConfigFile() present = false, want true")
	}
	if partial.LegacyReviewer == nil || partial.LegacyReviewer.Loop == nil || partial.LegacyReviewer.Loop.EnabledByDefault == nil || *partial.LegacyReviewer.Loop.EnabledByDefault {
		t.Fatalf("legacy reviewer loop missing after unset: %#v", partial.LegacyReviewer)
	}
	if partial.LegacyReviewer.ReviewEvents != nil {
		if partial.LegacyReviewer.ReviewEvents.Clean != nil {
			t.Fatalf("legacy reviewer clean event still set: %#v", partial.LegacyReviewer.ReviewEvents)
		}
		if partial.LegacyReviewer.ReviewEvents.Blocking != nil {
			t.Fatalf("legacy reviewer blocking event still set: %#v", partial.LegacyReviewer.ReviewEvents)
		}
	}
	if partial.Roles == nil || partial.Roles.Reviewer == nil || partial.Roles.Reviewer.Behavior == nil || partial.Roles.Reviewer.Behavior.ReviewEvents == nil {
		t.Fatalf("canonical reviewer reviewEvents missing after unset: %#v", partial.Roles)
	}
	if partial.Roles.Reviewer.Behavior.ReviewEvents.Clean != nil {
		t.Fatalf("canonical reviewer clean event still set: %#v", partial.Roles.Reviewer.Behavior.ReviewEvents)
	}
	if partial.Roles.Reviewer.Behavior.ReviewEvents.Blocking != nil {
		t.Fatalf("canonical reviewer blocking event still set: %#v", partial.Roles.Reviewer.Behavior.ReviewEvents)
	}
}

func TestConfigSetCanonicalReviewerReviewEventClearsLegacyField(t *testing.T) {
	tests := []struct {
		name        string
		key         string
		value       string
		assertClean bool
	}{
		{name: "clean", key: "roles.reviewer.behavior.reviewEvents.clean", value: "APPROVE", assertClean: true},
		{name: "blocking", key: "roles.reviewer.behavior.reviewEvents.blocking", value: "REQUEST_CHANGES", assertClean: false},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			configPath := writeEditableCLIConfigWithPayload(t, map[string]any{
				"notifications": map[string]any{
					"osascript": map[string]any{"enabled": false},
				},
				"reviewer": map[string]any{
					"loop": map[string]any{"enabledByDefault": false},
					"reviewEvents": map[string]any{
						"clean":    "COMMENT",
						"blocking": "COMMENT",
					},
				},
			})

			exitCode, stdout, stderr := runApp(t, "config", "set", tc.key, tc.value, "--config", configPath)
			if exitCode != 0 {
				t.Fatalf("Run([config set %s %s]) exit code = %d, want 0; stderr=%q", tc.key, tc.value, exitCode, stderr)
			}
			if !strings.Contains(stdout, "Set "+tc.key) {
				t.Fatalf("stdout = %q, want set confirmation for %s", stdout, tc.key)
			}

			partial, present, err := config.ReadPartialConfigFile(configPath)
			if err != nil {
				t.Fatalf("ReadPartialConfigFile() error = %v", err)
			}
			if !present {
				t.Fatalf("ReadPartialConfigFile() present = false, want true")
			}
			if partial.LegacyReviewer == nil || partial.LegacyReviewer.Loop == nil || partial.LegacyReviewer.Loop.EnabledByDefault == nil || *partial.LegacyReviewer.Loop.EnabledByDefault {
				t.Fatalf("legacy reviewer loop missing after set: %#v", partial.LegacyReviewer)
			}
			if partial.LegacyReviewer.ReviewEvents == nil {
				t.Fatalf("legacy reviewer reviewEvents missing after set: %#v", partial.LegacyReviewer)
			}
			if partial.Roles == nil || partial.Roles.Reviewer == nil || partial.Roles.Reviewer.Behavior == nil || partial.Roles.Reviewer.Behavior.ReviewEvents == nil {
				t.Fatalf("canonical reviewer reviewEvents missing after set: %#v", partial.Roles)
			}

			if tc.assertClean {
				if partial.LegacyReviewer.ReviewEvents.Clean != nil {
					t.Fatalf("legacy reviewer clean event still set: %#v", partial.LegacyReviewer.ReviewEvents)
				}
				if got := partial.Roles.Reviewer.Behavior.ReviewEvents.Clean; got == nil || *got != config.ReviewerReviewEventApprove {
					t.Fatalf("canonical reviewer clean event = %#v, want %q", got, config.ReviewerReviewEventApprove)
				}
			} else {
				if partial.LegacyReviewer.ReviewEvents.Blocking != nil {
					t.Fatalf("legacy reviewer blocking event still set: %#v", partial.LegacyReviewer.ReviewEvents)
				}
				if got := partial.Roles.Reviewer.Behavior.ReviewEvents.Blocking; got == nil || *got != config.ReviewerReviewEventRequestChanges {
					t.Fatalf("canonical reviewer blocking event = %#v, want %q", got, config.ReviewerReviewEventRequestChanges)
				}
			}
		})
	}
}

func TestConfigValidateAndShowSource(t *testing.T) {
	configPath := writeEditableCLIConfigWithPayload(t, map[string]any{
		"notifications": map[string]any{
			"osascript": map[string]any{"enabled": false},
		},
		"defaults": map[string]any{
			"allowRiskyFixes":    false,
			"fixAllPullRequests": false,
		},
		"package": map[string]any{
			"autoUpgradeEnabled": false,
		},
	})

	exitCode, stdout, stderr := runApp(t, "config", "validate", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run([config validate]) exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
	if !strings.Contains(stdout, "Config valid") {
		t.Fatalf("stdout = %q, want validation confirmation", stdout)
	}

	exitCode, stdout, stderr = runApp(t, "config", "show", "--source", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run([config show --source]) exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(stdout), &decoded); err != nil {
		t.Fatalf("unmarshal source output: %v", err)
	}
	fields, ok := decoded["fields"].(map[string]any)
	if !ok {
		t.Fatalf("fields = %#v, want object", decoded["fields"])
	}
	allowRisky, ok := fields["defaults.allowRiskyFixes"].(map[string]any)
	if !ok {
		t.Fatalf("defaults.allowRiskyFixes = %#v, want object", fields["defaults.allowRiskyFixes"])
	}
	if got, want := allowRisky["source"], "config-file"; got != want {
		t.Fatalf("source = %#v, want %#v", got, want)
	}
	if got, want := allowRisky["value"], false; got != want {
		t.Fatalf("value = %#v, want %#v", got, want)
	}
	autoUpgrade, ok := fields["package.autoUpgradeEnabled"].(map[string]any)
	if !ok {
		t.Fatalf("package.autoUpgradeEnabled = %#v, want object", fields["package.autoUpgradeEnabled"])
	}
	if got, want := autoUpgrade["source"], "config-file"; got != want {
		t.Fatalf("package.autoUpgradeEnabled source = %#v, want %#v", got, want)
	}
	if got, want := autoUpgrade["value"], false; got != want {
		t.Fatalf("package.autoUpgradeEnabled value = %#v, want %#v", got, want)
	}

	exitCode, stdout, stderr = runApp(t, "config", "show", "--source", "--no-custom-instructions=false", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run([config show --source --no-custom-instructions=false]) exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
	decoded = map[string]any{}
	if err := json.Unmarshal([]byte(stdout), &decoded); err != nil {
		t.Fatalf("unmarshal source output with CLI instructions override: %v", err)
	}
	fields, ok = decoded["fields"].(map[string]any)
	if !ok {
		t.Fatalf("fields = %#v, want object", decoded["fields"])
	}
	instructionsEnabled, ok := fields["instructions.enabled"].(map[string]any)
	if !ok {
		t.Fatalf("instructions.enabled = %#v, want object", fields["instructions.enabled"])
	}
	if got, want := instructionsEnabled["source"], "cli"; got != want {
		t.Fatalf("instructions.enabled source = %#v, want %#v", got, want)
	}
	if got, want := instructionsEnabled["value"], true; got != want {
		t.Fatalf("instructions.enabled value = %#v, want %#v", got, want)
	}
}

func TestConfigShowSourceDetectsCanonicalReviewerBehaviorOverrides(t *testing.T) {
	configPath := writeEditableCLIConfigWithPayload(t, map[string]any{
		"notifications": map[string]any{
			"osascript": map[string]any{"enabled": false},
		},
		"roles": map[string]any{
			"reviewer": map[string]any{
				"behavior": map[string]any{
					"reviewEvents": map[string]any{"clean": "COMMENT"},
				},
			},
		},
	})

	t.Setenv("LOOPER_ROLES_REVIEWER_BEHAVIOR_REVIEW_EVENTS_CLEAN", "APPROVE")
	exitCode, stdout, stderr := runApp(t, "config", "show", "--source", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run([config show --source]) exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
	assertConfigFieldSource(t, stdout, "roles.reviewer.behavior.reviewEvents.clean", "env")

	os.Unsetenv("LOOPER_ROLES_REVIEWER_BEHAVIOR_REVIEW_EVENTS_CLEAN")
	exitCode, stdout, stderr = runApp(t, "config", "show", "--source", "--roles-reviewer-behavior-review-events-clean=APPROVE", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run([config show --source --roles-reviewer-behavior-review-events-clean]) exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
	assertConfigFieldSource(t, stdout, "roles.reviewer.behavior.reviewEvents.clean", "cli")
}

func TestConfigShowSourceDetectsLegacyReviewerReviewEventsAsConfigFileSources(t *testing.T) {
	configPath := writeEditableCLIConfigWithPayload(t, map[string]any{
		"notifications": map[string]any{
			"osascript": map[string]any{"enabled": false},
		},
		"reviewer": map[string]any{
			"reviewEvents": map[string]any{
				"clean":    "APPROVE",
				"blocking": "REQUEST_CHANGES",
			},
		},
	})

	exitCode, stdout, stderr := runApp(t, "config", "show", "--source", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run([config show --source]) exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
	assertConfigFieldSource(t, stdout, "roles.reviewer.behavior.reviewEvents.clean", "config-file")
	assertConfigFieldSource(t, stdout, "roles.reviewer.behavior.reviewEvents.blocking", "config-file")
}

func TestConfigShowSourceDetectsCanonicalReviewerDiscoveryEnvOverride(t *testing.T) {
	configPath := writeEditableCLIConfigWithPayload(t, map[string]any{
		"notifications": map[string]any{
			"osascript": map[string]any{"enabled": false},
		},
		"roles": map[string]any{
			"reviewer": map[string]any{
				"discovery": map[string]any{"autoDiscovery": false},
			},
		},
	})

	t.Setenv("LOOPER_ROLES_REVIEWER_DISCOVERY_AUTO_DISCOVERY", "true")
	exitCode, stdout, stderr := runApp(t, "config", "show", "--source", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run([config show --source]) exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
	assertConfigFieldSource(t, stdout, "roles.reviewer.discovery.autoDiscovery", "env")
}

func TestConfigShowSourceDetectsCanonicalEnableFlagOverrides(t *testing.T) {
	configPath := writeEditableCLIConfigWithPayload(t, map[string]any{
		"notifications": map[string]any{
			"osascript": map[string]any{"enabled": false},
		},
		"instructions": map[string]any{"enabled": false},
		"package":      map[string]any{"autoUpgradeEnabled": true},
	})

	exitCode, stdout, stderr := runApp(t, "config", "show", "--source", "--instructions-enabled=true", "--package-auto-upgrade-enabled=false", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run([config show --source --instructions-enabled --package-auto-upgrade-enabled]) exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
	assertConfigFieldSource(t, stdout, "instructions.enabled", "cli")
	assertConfigFieldSource(t, stdout, "package.autoUpgradeEnabled", "cli")
}

func TestConfigShowSourceDetectsFixerAuthorFilterCLIOverride(t *testing.T) {
	configPath := writeEditableCLIConfigWithPayload(t, map[string]any{
		"notifications": map[string]any{
			"osascript": map[string]any{"enabled": false},
		},
		"roles": map[string]any{
			"fixer": map[string]any{
				"triggers": map[string]any{"authorFilter": "current_user"},
			},
		},
	})

	exitCode, stdout, stderr := runApp(t, "config", "show", "--source", "--roles-fixer-triggers-author-filter=any", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run([config show --source --roles-fixer-triggers-author-filter]) exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
	assertConfigFieldSource(t, stdout, "roles.fixer.triggers.authorFilter", "cli")
}

func TestConfigShowSourceDetectsCanonicalReviewerDiscoveryEnvOverrides(t *testing.T) {
	configPath := writeEditableCLIConfigWithPayload(t, map[string]any{
		"notifications": map[string]any{
			"osascript": map[string]any{"enabled": false},
		},
		"roles": map[string]any{
			"reviewer": map[string]any{
				"discovery": map[string]any{
					"triggers": map[string]any{
						"enableSelfReview":     false,
						"includeDrafts":        false,
						"requireReviewRequest": false,
						"labels":               []string{"looper:review"},
						"labelMode":            "all",
					},
					"specReview": map[string]any{
						"includeReviewingLabel": false,
						"reviewingLabel":        "looper:reviewing",
					},
				},
			},
		},
	})

	t.Setenv("LOOPER_ROLES_REVIEWER_DISCOVERY_TRIGGERS_ENABLE_SELF_REVIEW", "true")
	t.Setenv("LOOPER_ROLES_REVIEWER_DISCOVERY_TRIGGERS_INCLUDE_DRAFTS", "true")
	t.Setenv("LOOPER_ROLES_REVIEWER_DISCOVERY_TRIGGERS_REQUIRE_REVIEW_REQUEST", "true")
	t.Setenv("LOOPER_ROLES_REVIEWER_DISCOVERY_TRIGGERS_LABELS", "looper:review,looper:extra")
	t.Setenv("LOOPER_ROLES_REVIEWER_DISCOVERY_TRIGGERS_LABEL_MODE", "any")
	t.Setenv("LOOPER_ROLES_REVIEWER_DISCOVERY_SPEC_REVIEW_INCLUDE_REVIEWING_LABEL", "true")
	t.Setenv("LOOPER_ROLES_REVIEWER_DISCOVERY_SPEC_REVIEW_REVIEWING_LABEL", "looper:canonical")

	exitCode, stdout, stderr := runApp(t, "config", "show", "--source", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run([config show --source]) exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
	assertConfigFieldSource(t, stdout, "roles.reviewer.discovery.triggers.enableSelfReview", "env")
	assertConfigFieldSource(t, stdout, "roles.reviewer.discovery.triggers.includeDrafts", "env")
	assertConfigFieldSource(t, stdout, "roles.reviewer.discovery.triggers.requireReviewRequest", "env")
	assertConfigFieldSource(t, stdout, "roles.reviewer.discovery.triggers.labels", "env")
	assertConfigFieldSource(t, stdout, "roles.reviewer.discovery.triggers.labelMode", "env")
	assertConfigFieldSource(t, stdout, "roles.reviewer.discovery.specReview.includeReviewingLabel", "env")
	assertConfigFieldSource(t, stdout, "roles.reviewer.discovery.specReview.reviewingLabel", "env")
}

func TestConfigShowSourceDetectsCanonicalReviewerEnableSelfReviewCLIOverride(t *testing.T) {
	configPath := writeEditableCLIConfigWithPayload(t, map[string]any{
		"notifications": map[string]any{
			"osascript": map[string]any{"enabled": false},
		},
		"roles": map[string]any{
			"reviewer": map[string]any{
				"discovery": map[string]any{
					"triggers": map[string]any{"enableSelfReview": false},
				},
			},
		},
	})

	exitCode, stdout, stderr := runApp(t, "config", "show", "--source", "--roles-reviewer-discovery-triggers-enable-self-review=true", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run([config show --source --roles-reviewer-discovery-triggers-enable-self-review]) exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
	assertConfigFieldSource(t, stdout, "roles.reviewer.discovery.triggers.enableSelfReview", "cli")
}

func TestConfigValidateRejectsEnabledOsascriptNotificationsWithoutResolvedPath(t *testing.T) {
	t.Parallel()

	configPath := writeEditableCLIConfigWithPayload(t, map[string]any{
		"notifications": map[string]any{
			"osascript": map[string]any{"enabled": true, "throttleWindowSeconds": 60},
		},
		"defaults": map[string]any{
			"allowRiskyFixes": false,
		},
	})

	exitCode, stdout, stderr := runAppWithLookPath(t, func(file string) (string, error) {
		return "", exec.ErrNotFound
	}, "config", "validate", "--config", configPath)
	if exitCode == 0 {
		t.Fatalf("Run([config validate]) exit code = %d, want non-zero", exitCode)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "tools.osascriptPath") || !strings.Contains(stderr, "notifications.osascript.enabled is true") {
		t.Fatalf("stderr = %q, want osascript validation error", stderr)
	}
}

func TestConfigSetRejectsWriteWhenEnabledOsascriptNotificationsLackResolvedPath(t *testing.T) {
	configPath := writeEditableCLIConfigWithPayload(t, invalidOsascriptNotificationConfigPayload(false))
	t.Setenv("LOOPER_CONFIG", writeEditableCLIConfig(t))

	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config before set: %v", err)
	}

	exitCode, stdout, stderr := runAppWithLookPath(t, func(file string) (string, error) {
		return "", exec.ErrNotFound
	}, "config", "set", "defaults.allowRiskyFixes", "true", "--config", configPath)
	if exitCode == 0 {
		t.Fatalf("Run([config set ...]) exit code = %d, want non-zero", exitCode)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "tools.osascriptPath") || !strings.Contains(stderr, "notifications.osascript.enabled is true") {
		t.Fatalf("stderr = %q, want osascript validation error", stderr)
	}

	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config after set: %v", err)
	}
	if string(after) != string(before) {
		t.Fatalf("config changed after failed set\nbefore=%s\nafter=%s", before, after)
	}
}

func TestConfigUnsetRejectsWriteWhenEnabledOsascriptNotificationsLackResolvedPath(t *testing.T) {
	t.Parallel()

	configPath := writeEditableCLIConfigWithPayload(t, invalidOsascriptNotificationConfigPayload(true))
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config before unset: %v", err)
	}

	exitCode, stdout, stderr := runAppWithLookPath(t, func(file string) (string, error) {
		return "", exec.ErrNotFound
	}, "config", "unset", "defaults.allowRiskyFixes", "--config", configPath)
	if exitCode == 0 {
		t.Fatalf("Run([config unset ...]) exit code = %d, want non-zero", exitCode)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "tools.osascriptPath") || !strings.Contains(stderr, "notifications.osascript.enabled is true") {
		t.Fatalf("stderr = %q, want osascript validation error", stderr)
	}

	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config after unset: %v", err)
	}
	if string(after) != string(before) {
		t.Fatalf("config changed after failed unset\nbefore=%s\nafter=%s", before, after)
	}
}

func TestConfigEditRejectsEnabledOsascriptNotificationsWithoutResolvedPath(t *testing.T) {
	configPath := writeEditableCLIConfig(t)
	editorPath := filepath.Join(t.TempDir(), "editor.sh")
	editorScript := `#!/bin/sh
cat > "$1" <<'EOF'
{"notifications":{"osascript":{"enabled":true,"throttleWindowSeconds":60}},"defaults":{"allowRiskyFixes":false}}
EOF
`
	if err := os.WriteFile(editorPath, []byte(editorScript), 0o755); err != nil {
		t.Fatalf("write editor script: %v", err)
	}
	t.Setenv("EDITOR", editorPath)

	exitCode, stdout, stderr := runAppWithLookPath(t, func(file string) (string, error) {
		return "", exec.ErrNotFound
	}, "config", "edit", "--config", configPath)
	if exitCode == 0 {
		t.Fatalf("Run([config edit]) exit code = %d, want non-zero", exitCode)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "tools.osascriptPath") || !strings.Contains(stderr, "notifications.osascript.enabled is true") {
		t.Fatalf("stderr = %q, want osascript validation error", stderr)
	}
}

func TestConfigEditCreatesCanonicalTemplateAtSelectedTOMLPath(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "generated.toml")
	editorPath := filepath.Join(t.TempDir(), "editor.sh")
	if err := os.WriteFile(editorPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write editor script: %v", err)
	}
	t.Setenv("EDITOR", editorPath)

	exitCode, stdout, stderr := runApp(t, "config", "edit", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run([config edit --config generated.toml]) exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
	if !strings.Contains(stdout, "Config valid: "+configPath) {
		t.Fatalf("stdout = %q, want config-valid output for %q", stdout, configPath)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q, want empty", stderr)
	}

	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile(configPath) error = %v", err)
	}
	text := string(raw)
	for _, want := range []string{"[server]", "[daemon]", "[storage]", "[defaults]", "[instructions]", "[roles.planner]"} {
		if !strings.Contains(text, want) {
			t.Fatalf("generated TOML = %q, want to contain %q", text, want)
		}
	}
	if strings.Contains(text, "[reviewer]") {
		t.Fatalf("generated TOML = %q, did not expect legacy reviewer root", text)
	}
	if strings.Contains(text, "allowAutoApprove") {
		t.Fatalf("generated TOML = %q, did not expect deprecated defaults.allowAutoApprove", text)
	}
	if strings.Contains(text, "fixAllPullRequests") {
		t.Fatalf("generated TOML = %q, did not expect deprecated defaults.fixAllPullRequests", text)
	}

	loaded, err := config.LoadFile(config.LoadFileOptions{Args: []string{"--config", configPath}})
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}
	if !loaded.Metadata.ConfigFilePresent {
		t.Fatal("LoadFile().Metadata.ConfigFilePresent = false, want true")
	}
	if len(loaded.Warnings) != 0 {
		t.Fatalf("LoadFile().Warnings = %#v, want none", loaded.Warnings)
	}
}

func TestConfigSetPreservesSelectedYAMLFormat(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "selected.yaml")
	exitCode, stdout, stderr := runApp(t, "config", "set", "defaults.allowRiskyFixes", "true", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run([config set ... --config selected.yaml]) exit code = %d, want 0; stdout=%q stderr=%q", exitCode, stdout, stderr)
	}
	if !strings.Contains(stdout, "Set defaults.allowRiskyFixes in "+configPath) {
		t.Fatalf("stdout = %q, want set confirmation for %q", stdout, configPath)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q, want empty", stderr)
	}

	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile(configPath) error = %v", err)
	}
	text := string(raw)
	if !strings.Contains(text, "defaults:") || !strings.Contains(text, "allowRiskyFixes: true") {
		t.Fatalf("generated YAML = %q, want defaults.allowRiskyFixes field", text)
	}
	if strings.Contains(text, "\"defaults\"") {
		t.Fatalf("generated YAML = %q, did not expect JSON formatting", text)
	}

	loaded, err := config.LoadFile(config.LoadFileOptions{Args: []string{"--config", configPath}})
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}
	if !loaded.Config.Defaults.AllowRiskyFixes {
		t.Fatalf("loaded config allowRiskyFixes = %t, want true", loaded.Config.Defaults.AllowRiskyFixes)
	}
}

func invalidOsascriptNotificationConfigPayload(allowRiskyFixes bool) map[string]any {
	return map[string]any{
		"notifications": map[string]any{
			"osascript": map[string]any{"enabled": true, "throttleWindowSeconds": 60},
		},
		"defaults": map[string]any{
			"allowRiskyFixes": allowRiskyFixes,
		},
	}
}

func TestConfigSetWarnsWhenFlagOverridesWrittenValue(t *testing.T) {
	configPath := writeEditableCLIConfig(t)

	exitCode, _, stderr := runApp(t, "config", "set", "defaults.fixAllPullRequests", "false", "--fix-all-pull-requests", "true", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run([config set with override]) exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
	if !strings.Contains(stderr, "warning: --fix-all-pull-requests is set") {
		t.Fatalf("stderr = %q, want override warning", stderr)
	}

	exitCode, _, stderr = runApp(t, "config", "set", "instructions.enabled", "true", "--no-custom-instructions", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run([config set instructions.enabled with override]) exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
	if !strings.Contains(stderr, "warning: --no-custom-instructions is set") {
		t.Fatalf("stderr = %q, want instructions override warning", stderr)
	}

	t.Setenv("LOOPER_ROLES_REVIEWER_BEHAVIOR_REVIEW_EVENTS_CLEAN", "APPROVE")
	exitCode, _, stderr = runApp(t, "config", "set", "roles.reviewer.behavior.reviewEvents.clean", "COMMENT", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run([config set roles.reviewer.behavior.reviewEvents.clean with env override]) exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
	if !strings.Contains(stderr, "warning: LOOPER_ROLES_REVIEWER_BEHAVIOR_REVIEW_EVENTS_CLEAN is set") {
		t.Fatalf("stderr = %q, want canonical env override warning", stderr)
	}

	exitCode, _, stderr = runApp(t, "config", "set", "roles.reviewer.behavior.reviewEvents.clean", "COMMENT", "--roles-reviewer-behavior-review-events-clean=APPROVE", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run([config set roles.reviewer.behavior.reviewEvents.clean with canonical flag override]) exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
	if !strings.Contains(stderr, "warning: --roles-reviewer-behavior-review-events-clean is set") {
		t.Fatalf("stderr = %q, want canonical flag override warning", stderr)
	}

	t.Setenv("LOOPER_ROLES_REVIEWER_DISCOVERY_AUTO_DISCOVERY", "true")
	exitCode, _, stderr = runApp(t, "config", "set", "roles.reviewer.discovery.autoDiscovery", "false", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run([config set roles.reviewer.discovery.autoDiscovery with env override]) exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
	if !strings.Contains(stderr, "warning: LOOPER_ROLES_REVIEWER_DISCOVERY_AUTO_DISCOVERY is set") {
		t.Fatalf("stderr = %q, want canonical reviewer discovery env override warning", stderr)
	}

	exitCode, _, stderr = runApp(t, "config", "set", "instructions.enabled", "false", "--instructions-enabled=true", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run([config set instructions.enabled with canonical override]) exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
	if !strings.Contains(stderr, "warning: --instructions-enabled is set") {
		t.Fatalf("stderr = %q, want canonical instructions flag override warning", stderr)
	}

	exitCode, _, stderr = runApp(t, "config", "set", "package.autoUpgradeEnabled", "true", "--package-auto-upgrade-enabled=false", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run([config set package.autoUpgradeEnabled with canonical override]) exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
	if !strings.Contains(stderr, "warning: --package-auto-upgrade-enabled is set") {
		t.Fatalf("stderr = %q, want canonical package flag override warning", stderr)
	}
}

func assertConfigFieldSource(t *testing.T, stdout, key, wantSource string) {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal([]byte(stdout), &decoded); err != nil {
		t.Fatalf("unmarshal source output: %v", err)
	}
	fields, ok := decoded["fields"].(map[string]any)
	if !ok {
		t.Fatalf("fields = %#v, want object", decoded["fields"])
	}
	field, ok := fields[key].(map[string]any)
	if !ok {
		t.Fatalf("%s = %#v, want object", key, fields[key])
	}
	if got := field["source"]; got != wantSource {
		t.Fatalf("%s source = %#v, want %#v", key, got, wantSource)
	}
}

func TestConfigValidatePrintsLegacyDefaultConfigMigrationNote(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("LOOPER_CONFIG", "")
	looperHome := filepath.Join(homeDir, ".looper")
	if err := os.MkdirAll(looperHome, 0o755); err != nil {
		t.Fatalf("os.MkdirAll() error = %v", err)
	}
	legacyDefaultPath := filepath.Join(looperHome, "config.json")
	if err := os.WriteFile(legacyDefaultPath, []byte(`{"server":{"port":7400}}`), 0o644); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}

	exitCode, stdout, stderr := runApp(t, "config", "validate")
	if exitCode != 0 {
		t.Fatalf("Run([config validate]) exit code = %d, want 0; stdout=%q stderr=%q", exitCode, stdout, stderr)
	}
	if !strings.Contains(stdout, "Config valid: "+legacyDefaultPath) {
		t.Fatalf("stdout = %q, want config-valid output for %q", stdout, legacyDefaultPath)
	}
	if !strings.Contains(stderr, "note: legacy default config file ") || !strings.Contains(stderr, legacyDefaultPath) || !strings.Contains(stderr, filepath.Join(looperHome, "config.toml")) || !strings.Contains(stderr, "docs/configuration.md") {
		t.Fatalf("stderr = %q, want migration note", stderr)
	}
	if _, err := os.Stat(legacyDefaultPath); err != nil {
		t.Fatalf("os.Stat(%q) error = %v, want legacy config file preserved", legacyDefaultPath, err)
	}
}

func TestConfigCommandPathSkipsExplicitBoolLiteralForConfigFlag(t *testing.T) {
	command, subcommand := configCommandPath([]string{"config", "--no-custom-instructions", "false", "validate"})
	if got, want := command, "config"; got != want {
		t.Fatalf("command = %q, want %q", got, want)
	}
	if got, want := subcommand, "validate"; got != want {
		t.Fatalf("subcommand = %q, want %q", got, want)
	}
}

func TestConfigShowSourceSuppressesLegacyDefaultConfigMigrationNote(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	looperHome := filepath.Join(homeDir, ".looper")
	if err := os.MkdirAll(looperHome, 0o755); err != nil {
		t.Fatalf("os.MkdirAll() error = %v", err)
	}
	legacyDefaultPath := filepath.Join(looperHome, "config.json")
	if err := os.WriteFile(legacyDefaultPath, []byte(`{"server":{"port":7400}}`), 0o644); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}

	exitCode, stdout, stderr := runApp(t, "config", "show", "--source")
	if exitCode != 0 {
		t.Fatalf("Run([config show --source]) exit code = %d, want 0; stdout=%q stderr=%q", exitCode, stdout, stderr)
	}
	if strings.Contains(stderr, "note: legacy default config file ") {
		t.Fatalf("stderr = %q, want migration note suppressed", stderr)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(stdout), &decoded); err != nil {
		t.Fatalf("unmarshal source output: %v", err)
	}
	fields, ok := decoded["fields"].(map[string]any)
	if !ok {
		t.Fatalf("fields = %#v, want object", decoded["fields"])
	}
	if len(fields) == 0 {
		t.Fatalf("fields = %#v, want non-empty source map", fields)
	}
}

func TestConfigMigrateDryRunCanonicalizesLegacyJSONWithoutWritingDestination(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "legacy.json")
	if err := os.WriteFile(configPath, []byte(`{"reviewer":{"reviewEvents":{"clean":"COMMENT"}},"defaults":{"allowAutoApprove":true},"projects":[{"id":"repo","name":"Repo","path":"/tmp/repo","instructions":{"reviewer":"check carefully"}}]}`), 0o644); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}
	destPath := filepath.Join(filepath.Dir(configPath), "canonical.toml")

	exitCode, stdout, stderr := runApp(t, "config", "migrate", "--from", configPath, "--to", destPath, "--dry-run")
	if exitCode != 0 {
		t.Fatalf("Run([config migrate --dry-run]) exit code = %d, want 0; stdout=%q stderr=%q", exitCode, stdout, stderr)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q, want empty", stderr)
	}
	for _, want := range []string{
		"Dry run: would migrate config ",
		"rewrite config format from json to toml",
		"move legacy top-level reviewer.* settings to roles.reviewer.behavior.*",
		"[roles.reviewer.behavior.reviewEvents]",
		"clean = 'COMMENT'",
		"[projects.roles.reviewer]",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout = %q, want %q", stdout, want)
		}
	}
	for _, unwanted := range []string{"[reviewer]", "path = '/tmp/repo'", "[projects.instructions]", "allowAutoApprove ="} {
		if strings.Contains(stdout, unwanted) {
			t.Fatalf("stdout = %q, did not expect %q", stdout, unwanted)
		}
	}
	if _, err := os.Stat(destPath); !os.IsNotExist(err) {
		t.Fatalf("os.Stat(%q) error = %v, want destination not created", destPath, err)
	}
	if _, err := os.Stat(configPath); err != nil {
		t.Fatalf("os.Stat(%q) error = %v, want source preserved", configPath, err)
	}
}

func TestConfigMigrateFailsWhenDestinationExistsWithoutForce(t *testing.T) {
	sourcePath := filepath.Join(t.TempDir(), "legacy.json")
	if err := os.WriteFile(sourcePath, []byte(`{"defaults":{"allowRiskyFixes":true}}`), 0o644); err != nil {
		t.Fatalf("os.WriteFile(sourcePath) error = %v", err)
	}
	destPath := filepath.Join(filepath.Dir(sourcePath), "config.toml")
	before := "[defaults]\nallowRiskyFixes = false\n"
	if err := os.WriteFile(destPath, []byte(before), 0o644); err != nil {
		t.Fatalf("os.WriteFile(destPath) error = %v", err)
	}

	exitCode, stdout, stderr := runApp(t, "config", "migrate", "--from", sourcePath, "--to", destPath)
	if exitCode == 0 {
		t.Fatalf("Run([config migrate]) exit code = %d, want non-zero", exitCode)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "destination config file already exists") || !strings.Contains(stderr, "--force") {
		t.Fatalf("stderr = %q, want overwrite guidance", stderr)
	}
	after, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatalf("os.ReadFile(destPath) error = %v", err)
	}
	if string(after) != before {
		t.Fatalf("destination changed unexpectedly\nbefore=%s\nafter=%s", before, after)
	}
}

func TestConfigMigrateForceOverwritesExistingDestinationAndCreatesBackup(t *testing.T) {
	sourcePath := filepath.Join(t.TempDir(), "legacy.json")
	if err := os.WriteFile(sourcePath, []byte(`{"reviewer":{"reviewEvents":{"clean":"APPROVE"}},"defaults":{"fixAllPullRequests":true}}`), 0o644); err != nil {
		t.Fatalf("os.WriteFile(sourcePath) error = %v", err)
	}
	destPath := filepath.Join(filepath.Dir(sourcePath), "canonical.toml")
	before := "[defaults]\nallowRiskyFixes = false\n"
	if err := os.WriteFile(destPath, []byte(before), 0o644); err != nil {
		t.Fatalf("os.WriteFile(destPath) error = %v", err)
	}

	exitCode, stdout, stderr := runApp(t, "config", "migrate", "--from", sourcePath, "--to", destPath, "--force")
	if exitCode != 0 {
		t.Fatalf("Run([config migrate --force]) exit code = %d, want 0; stdout=%q stderr=%q", exitCode, stdout, stderr)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q, want empty", stderr)
	}
	if !strings.Contains(stdout, "Backup created at ") || !strings.Contains(stdout, "Source preserved at "+sourcePath) {
		t.Fatalf("stdout = %q, want backup and source-preserved messages", stdout)
	}
	raw, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatalf("os.ReadFile(destPath) error = %v", err)
	}
	text := string(raw)
	if !strings.Contains(text, "[roles.reviewer.behavior.reviewEvents]") || !strings.Contains(text, "[roles.fixer.triggers]") {
		t.Fatalf("migrated TOML = %q, want canonical reviewer/fixer sections", text)
	}
	if strings.Contains(text, "fixAllPullRequests") || strings.Contains(text, "[reviewer]") {
		t.Fatalf("migrated TOML = %q, did not expect legacy surfaces", text)
	}
	entries, err := os.ReadDir(filepath.Dir(destPath))
	if err != nil {
		t.Fatalf("os.ReadDir() error = %v", err)
	}
	backupFound := false
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), filepath.Base(destPath)+".") && strings.HasSuffix(entry.Name(), ".bak") {
			backupFound = true
			backupRaw, err := os.ReadFile(filepath.Join(filepath.Dir(destPath), entry.Name()))
			if err != nil {
				t.Fatalf("os.ReadFile(backup) error = %v", err)
			}
			if string(backupRaw) != before {
				t.Fatalf("backup contents = %q, want original destination contents %q", backupRaw, before)
			}
		}
	}
	if !backupFound {
		t.Fatal("expected destination backup to be created")
	}
}

func TestConfigMigrateDefaultPathCreatesCanonicalTOMLAndPreservesLegacyJSON(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("LOOPER_CONFIG", "")
	looperHome := filepath.Join(homeDir, ".looper")
	if err := os.MkdirAll(looperHome, 0o755); err != nil {
		t.Fatalf("os.MkdirAll() error = %v", err)
	}
	legacyPath := filepath.Join(looperHome, "config.json")
	canonicalPath := filepath.Join(looperHome, "config.toml")
	if err := os.WriteFile(legacyPath, []byte(`{"defaults":{"allowRiskyFixes":true}}`), 0o644); err != nil {
		t.Fatalf("os.WriteFile(legacyPath) error = %v", err)
	}

	exitCode, stdout, stderr := runApp(t, "config", "migrate")
	if exitCode != 0 {
		t.Fatalf("Run([config migrate]) exit code = %d, want 0; stdout=%q stderr=%q", exitCode, stdout, stderr)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q, want empty", stderr)
	}
	if !strings.Contains(stdout, "Migrated config "+legacyPath+" -> "+canonicalPath) {
		t.Fatalf("stdout = %q, want default migration message", stdout)
	}
	if _, err := os.Stat(legacyPath); err != nil {
		t.Fatalf("os.Stat(%q) error = %v, want source preserved", legacyPath, err)
	}
	raw, err := os.ReadFile(canonicalPath)
	if err != nil {
		t.Fatalf("os.ReadFile(%q) error = %v", canonicalPath, err)
	}
	if !strings.Contains(string(raw), "allowRiskyFixes = true") {
		t.Fatalf("canonical TOML = %q, want migrated defaults.allowRiskyFixes", raw)
	}
}

func TestConfigMigrateRejectsPositionalArguments(t *testing.T) {
	exitCode, stdout, stderr := runApp(t, "config", "migrate", "./legacy.json")
	if exitCode == 0 {
		t.Fatalf("Run([config migrate ./legacy.json]) exit code = %d, want non-zero", exitCode)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "unknown command \"./legacy.json\" for \"looper config migrate\"") {
		t.Fatalf("stderr = %q, want positional-args rejection", stderr)
	}
}

func TestConfigMigrateDefaultPathFailsClearlyWhenCanonicalDefaultAlreadyExists(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("LOOPER_CONFIG", "")
	looperHome := filepath.Join(homeDir, ".looper")
	if err := os.MkdirAll(looperHome, 0o755); err != nil {
		t.Fatalf("os.MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(looperHome, "config.json"), []byte(`{"defaults":{"allowRiskyFixes":true}}`), 0o644); err != nil {
		t.Fatalf("os.WriteFile(config.json) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(looperHome, "config.toml"), []byte("[defaults]\nallowRiskyFixes = false\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile(config.toml) error = %v", err)
	}

	exitCode, stdout, stderr := runApp(t, "config", "migrate")
	if exitCode == 0 {
		t.Fatalf("Run([config migrate]) exit code = %d, want non-zero", exitCode)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "default migration target already exists") || !strings.Contains(stderr, "--force") {
		t.Fatalf("stderr = %q, want default-target conflict guidance", stderr)
	}
}

func TestConfigMigrateDryRunAllowsExistingDestinationWithoutForce(t *testing.T) {
	sourcePath := filepath.Join(t.TempDir(), "legacy.yaml")
	if err := os.WriteFile(sourcePath, []byte("defaults:\n  allowRiskyFixes: true\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile(sourcePath) error = %v", err)
	}
	destPath := filepath.Join(filepath.Dir(sourcePath), "canonical.toml")
	if err := os.WriteFile(destPath, []byte("[defaults]\nallowRiskyFixes = false\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile(destPath) error = %v", err)
	}

	exitCode, stdout, stderr := runApp(t, "config", "migrate", "--from", sourcePath, "--to", destPath, "--dry-run")
	if exitCode != 0 {
		t.Fatalf("Run([config migrate --dry-run]) exit code = %d, want 0; stdout=%q stderr=%q", exitCode, stdout, stderr)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q, want empty", stderr)
	}
	if !strings.Contains(stdout, "Dry run: would migrate config") || !strings.Contains(stdout, "allowRiskyFixes = true") {
		t.Fatalf("stdout = %q, want dry-run preview", stdout)
	}
	raw, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatalf("os.ReadFile(destPath) error = %v", err)
	}
	if string(raw) != "[defaults]\nallowRiskyFixes = false\n" {
		t.Fatalf("destination changed during dry-run: %q", raw)
	}
}

func TestConfigMigrateDryRunFailsWhenDestinationDirectoryCannotBePrepared(t *testing.T) {
	sourcePath := filepath.Join(t.TempDir(), "legacy.json")
	if err := os.WriteFile(sourcePath, []byte(`{"defaults":{"allowRiskyFixes":true}}`), 0o644); err != nil {
		t.Fatalf("os.WriteFile(sourcePath) error = %v", err)
	}
	destDir := filepath.Join(t.TempDir(), "locked")
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		t.Fatalf("os.MkdirAll(destDir) error = %v", err)
	}
	if err := os.Chmod(destDir, 0o555); err != nil {
		t.Fatalf("os.Chmod(destDir) error = %v", err)
	}
	defer func() {
		_ = os.Chmod(destDir, 0o755)
	}()

	destPath := filepath.Join(destDir, "canonical.toml")
	exitCode, stdout, stderr := runApp(t, "config", "migrate", "--from", sourcePath, "--to", destPath, "--dry-run")
	if exitCode == 0 {
		t.Fatalf("Run([config migrate --dry-run]) exit code = %d, want non-zero", exitCode)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "create config directory") {
		t.Fatalf("stderr = %q, want destination preparation error", stderr)
	}
	if _, err := os.Stat(destPath); !os.IsNotExist(err) {
		t.Fatalf("os.Stat(%q) error = %v, want destination not created", destPath, err)
	}
}

func TestConfigMigrateDryRunDoesNotCreateMissingDestinationDirectories(t *testing.T) {
	sourcePath := filepath.Join(t.TempDir(), "legacy.json")
	if err := os.WriteFile(sourcePath, []byte(`{"defaults":{"allowRiskyFixes":true}}`), 0o644); err != nil {
		t.Fatalf("os.WriteFile(sourcePath) error = %v", err)
	}
	root := t.TempDir()
	destDir := filepath.Join(root, "nested", "config")
	destPath := filepath.Join(destDir, "canonical.toml")

	exitCode, stdout, stderr := runApp(t, "config", "migrate", "--from", sourcePath, "--to", destPath, "--dry-run")
	if exitCode != 0 {
		t.Fatalf("Run([config migrate --dry-run]) exit code = %d, want 0; stdout=%q stderr=%q", exitCode, stdout, stderr)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q, want empty", stderr)
	}
	if !strings.Contains(stdout, "Dry run: would migrate config") || !strings.Contains(stdout, "allowRiskyFixes = true") {
		t.Fatalf("stdout = %q, want dry-run preview", stdout)
	}
	if _, err := os.Stat(destDir); !os.IsNotExist(err) {
		t.Fatalf("os.Stat(%q) error = %v, want dry-run to avoid creating destination directories", destDir, err)
	}
	if _, err := os.Stat(destPath); !os.IsNotExist(err) {
		t.Fatalf("os.Stat(%q) error = %v, want dry-run to avoid creating destination file", destPath, err)
	}
}

func TestConfigMigrateDryRunRejectsExistingDestinationDirectory(t *testing.T) {
	sourcePath := filepath.Join(t.TempDir(), "legacy.json")
	if err := os.WriteFile(sourcePath, []byte(`{"defaults":{"allowRiskyFixes":true}}`), 0o644); err != nil {
		t.Fatalf("os.WriteFile(sourcePath) error = %v", err)
	}
	destDir := t.TempDir()

	exitCode, stdout, stderr := runApp(t, "config", "migrate", "--from", sourcePath, "--to", destDir, "--dry-run")
	if exitCode == 0 {
		t.Fatalf("Run([config migrate --dry-run --to dir]) exit code = %d, want non-zero", exitCode)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "destination config path points to a directory") {
		t.Fatalf("stderr = %q, want directory-destination guidance", stderr)
	}
	if info, err := os.Stat(destDir); err != nil || !info.IsDir() {
		t.Fatalf("os.Stat(%q) = (%v, %v), want existing directory preserved", destDir, info, err)
	}
}

func TestConfigMigrateReportsMissingSourceBeforeOverwriteGuidance(t *testing.T) {
	sourcePath := filepath.Join(t.TempDir(), "missing.json")
	destPath := filepath.Join(t.TempDir(), "config.toml")
	before := "[defaults]\nallowRiskyFixes = false\n"
	if err := os.WriteFile(destPath, []byte(before), 0o644); err != nil {
		t.Fatalf("os.WriteFile(destPath) error = %v", err)
	}

	exitCode, stdout, stderr := runApp(t, "config", "migrate", "--from", sourcePath, "--to", destPath)
	if exitCode == 0 {
		t.Fatalf("Run([config migrate missing source]) exit code = %d, want non-zero", exitCode)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "source config file not found") {
		t.Fatalf("stderr = %q, want missing-source guidance", stderr)
	}
	if strings.Contains(stderr, "--force") {
		t.Fatalf("stderr = %q, did not expect overwrite guidance", stderr)
	}
	after, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatalf("os.ReadFile(destPath) error = %v", err)
	}
	if string(after) != before {
		t.Fatalf("destination changed unexpectedly\nbefore=%s\nafter=%s", before, after)
	}
}

func TestConfigMigrateRejectsSameSourceAndDestinationPath(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(configPath, []byte("[defaults]\nallowRiskyFixes = true\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile(configPath) error = %v", err)
	}

	exitCode, stdout, stderr := runApp(t, "config", "migrate", "--from", configPath)
	if exitCode == 0 {
		t.Fatalf("Run([config migrate --from config.toml]) exit code = %d, want non-zero", exitCode)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "source and destination config paths must differ") {
		t.Fatalf("stderr = %q, want same-path guidance", stderr)
	}
}

func TestConfigMigrateRejectsInvalidLegacyProjectInstructionRole(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "legacy.json")
	if err := os.WriteFile(configPath, []byte(`{"projects":[{"id":"repo","name":"Repo","path":"/tmp/repo","instructions":{"bad-role":"nope"}}]}`), 0o644); err != nil {
		t.Fatalf("os.WriteFile(configPath) error = %v", err)
	}

	exitCode, stdout, stderr := runApp(t, "config", "migrate", "--from", configPath, "--to", filepath.Join(filepath.Dir(configPath), "canonical.toml"))
	if exitCode == 0 {
		t.Fatalf("Run([config migrate invalid project role]) exit code = %d, want non-zero", exitCode)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "projects[0].instructions.bad-role") {
		t.Fatalf("stderr = %q, want legacy role validation error", stderr)
	}
}

func TestConfigMigrateIgnoresUnrelatedEnvironmentOverridesDuringWriteValidation(t *testing.T) {
	sourcePath := filepath.Join(t.TempDir(), "legacy.json")
	if err := os.WriteFile(sourcePath, []byte(`{"defaults":{"allowRiskyFixes":true}}`), 0o644); err != nil {
		t.Fatalf("os.WriteFile(sourcePath) error = %v", err)
	}
	destPath := filepath.Join(filepath.Dir(sourcePath), "canonical.toml")
	t.Setenv("LOOPER_PORT", "not-an-int")

	exitCode, stdout, stderr := runApp(t, "config", "migrate", "--from", sourcePath, "--to", destPath)
	if exitCode != 0 {
		t.Fatalf("Run([config migrate with invalid env override]) exit code = %d, want 0; stdout=%q stderr=%q", exitCode, stdout, stderr)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q, want empty", stderr)
	}
	if !strings.Contains(stdout, "Migrated config "+sourcePath+" -> "+destPath) {
		t.Fatalf("stdout = %q, want success migration message", stdout)
	}
}

func TestEmitConfigLoadNoticesPrintsEachNoticeOncePerRuntime(t *testing.T) {
	stderr := &bytes.Buffer{}
	runtime := newCommandRuntime(New(Deps{Stderr: stderr}), nil)
	loaded := config.LoadedFileConfig{Notices: []string{"migrate me"}}

	runtime.emitConfigLoadNotices(loaded)
	runtime.emitConfigLoadNotices(loaded)

	if got := stderr.String(); got != "note: migrate me\n" {
		t.Fatalf("stderr = %q, want one emitted notice", got)
	}
}

func TestEmitConfigLoadNoticesPrintsWarningsAndDeduplicatesByLevel(t *testing.T) {
	stderr := &bytes.Buffer{}
	runtime := newCommandRuntime(New(Deps{Stderr: stderr}), nil)
	loaded := config.LoadedFileConfig{Warnings: []string{"deprecated reviewer path"}, Notices: []string{"legacy default config"}}

	runtime.emitConfigLoadNotices(loaded)
	runtime.emitConfigLoadNotices(loaded)

	if got := stderr.String(); got != "warning: deprecated reviewer path\nnote: legacy default config\n" {
		t.Fatalf("stderr = %q, want one emitted warning and notice", got)
	}

	stderr.Reset()
	runtime.emitConfigLoadNotices(config.LoadedFileConfig{Warnings: []string{"same text"}, Notices: []string{"same text"}})
	if got := stderr.String(); got != "warning: same text\nnote: same text\n" {
		t.Fatalf("stderr = %q, want warning and note tracked separately", got)
	}
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

func TestPSSuppressesConfigFileDeprecationNotices(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.URL.Path, "/api/v1/runs/active"; got != want {
			t.Fatalf("request path = %q, want %q", got, want)
		}
		writeEnvelope(t, w, pkgapi.Success("req_active_runs", map[string]any{"items": []map[string]any{}}))
	}))
	defer server.Close()

	configPath := writeEditableCLIConfigWithPayload(t, map[string]any{
		"server": map[string]any{
			"baseUrl":  server.URL,
			"authMode": "none",
		},
		"reviewer": map[string]any{"reviewEvents": map[string]any{"clean": "COMMENT"}},
		"defaults": map[string]any{"allowAutoApprove": true},
		"roles":    map[string]any{"reviewer": map[string]any{"autoDiscovery": true}},
	})

	exitCode, stdout, stderr := runApp(t, "ps", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run([ps]) exit code = %d, want 0; stdout=%q stderr=%q", exitCode, stdout, stderr)
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

func TestPSStatusAndTypeFiltersUseActiveRunsQueryAndShowInactiveCompletedLoop(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.URL.Path, "/api/v1/runs/active"; got != want {
			t.Fatalf("request path = %q, want %q", got, want)
		}
		if got, want := r.URL.Query().Get("status"), "completed"; got != want {
			t.Fatalf("status query = %q, want %q", got, want)
		}
		if got, want := r.URL.Query().Get("type"), "worker"; got != want {
			t.Fatalf("type query = %q, want %q", got, want)
		}
		if got := r.URL.Query().Get("all"); got != "" {
			t.Fatalf("all query = %q, want empty", got)
		}
		writeEnvelope(t, w, pkgapi.Success("req_active_runs", map[string]any{"items": []map[string]any{{
			"seq":    12,
			"type":   "worker",
			"status": "completed",
			"target": map[string]any{"label": "Issue #42"},
		}}}))
	}))
	defer server.Close()

	configPath := writeCLIConfig(t, server.URL, "")
	exitCode, stdout, stderr := runApp(t, "ps", "--status", "completed", "--type", "worker", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run([ps --status completed --type worker]) exit code = %d, want 0", exitCode)
	}
	if stderr != "" {
		t.Fatalf("Run([ps --status completed --type worker]) stderr = %q, want empty string", stderr)
	}
	for _, want := range []string{"worker", "completed", "Issue #42"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("Run([ps --status completed --type worker]) stdout = %q, want to contain %q", stdout, want)
		}
	}
}

func TestPSAllUsesActiveRunsAllQueryAndShowsInactiveCompletedLoop(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.URL.Path, "/api/v1/runs/active"; got != want {
			t.Fatalf("request path = %q, want %q", got, want)
		}
		if got, want := r.URL.Query().Get("all"), "true"; got != want {
			t.Fatalf("all query = %q, want %q", got, want)
		}
		if got := r.URL.Query().Get("status"); got != "" {
			t.Fatalf("status query = %q, want empty", got)
		}
		writeEnvelope(t, w, pkgapi.Success("req_active_runs", map[string]any{"items": []map[string]any{{
			"seq":     15,
			"runId":   "run_15",
			"type":    "worker",
			"status":  "completed",
			"endedAt": "2026-04-11T12:01:00.000Z",
			"target":  map[string]any{"label": "Issue #15"},
		}}}))
	}))
	defer server.Close()

	configPath := writeCLIConfig(t, server.URL, "")
	exitCode, stdout, stderr := runApp(t, "ps", "--all", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run([ps --all]) exit code = %d, want 0", exitCode)
	}
	if stderr != "" {
		t.Fatalf("Run([ps --all]) stderr = %q, want empty string", stderr)
	}
	for _, want := range []string{"worker", "completed", "Issue #15"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("Run([ps --all]) stdout = %q, want to contain %q", stdout, want)
		}
	}
}

func TestStopAllWithoutJSONUsesStopAllRoute(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.Method, http.MethodPost; got != want {
			t.Fatalf("method = %q, want %q", got, want)
		}
		if got, want := r.URL.Path, "/api/v1/runs/active/stop-all"; got != want {
			t.Fatalf("path = %q, want %q", got, want)
		}
		writeEnvelope(t, w, pkgapi.Success("req_stop_all", map[string]any{"summary": map[string]any{"total": 1, "stopped": 1, "alreadyFinished": 0, "alreadyStopping": 0, "failed": 0}, "items": []map[string]any{{"seq": 12, "type": "worker", "loopId": "loop_12", "runId": "run_12", "executionId": "exec_12", "result": "stopped"}}}))
	}))
	defer server.Close()

	configPath := writeCLIConfig(t, server.URL, "")
	exitCode, stdout, stderr := runApp(t, "stop", "all", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run([stop all]) exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
	if stderr != "" {
		t.Fatalf("Run([stop all]) stderr = %q, want empty string", stderr)
	}
	for _, want := range []string{"Stopped running tasks", "loop_12", "worker", "stopped"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("Run([stop all]) stdout = %q, want to contain %q", stdout, want)
		}
	}
}

func TestStopAllWithoutJSONPrintsNoRunningWorkMessage(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeEnvelope(t, w, pkgapi.Success("req_stop_all_empty", map[string]any{"summary": map[string]any{"total": 0, "stopped": 0, "alreadyFinished": 0, "alreadyStopping": 0, "failed": 0}, "items": []map[string]any{}}))
	}))
	defer server.Close()

	configPath := writeCLIConfig(t, server.URL, "")
	exitCode, stdout, stderr := runApp(t, "stop", "all", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run([stop all]) exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
	if stderr != "" {
		t.Fatalf("Run([stop all]) stderr = %q, want empty string", stderr)
	}
	if got, want := stdout, "No running tasks to stop.\n"; got != want {
		t.Fatalf("Run([stop all]) stdout = %q, want %q", got, want)
	}
}

func TestStopAllWithoutJSONReturnsNonZeroForPartialFailure(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeEnvelope(t, w, pkgapi.Success("req_stop_all_partial", map[string]any{"summary": map[string]any{"total": 2, "stopped": 1, "alreadyFinished": 0, "alreadyStopping": 0, "failed": 1}, "items": []map[string]any{{"seq": 1, "type": "planner", "loopId": "loop_1", "runId": "run_1", "executionId": "exec_1", "result": "stopped"}, {"seq": 2, "type": "reviewer", "loopId": "loop_2", "runId": "run_2", "executionId": "exec_2", "result": "failed", "error": "signal failed"}}}))
	}))
	defer server.Close()

	configPath := writeCLIConfig(t, server.URL, "")
	exitCode, stdout, stderr := runApp(t, "stop", "all", "--config", configPath)
	if exitCode == 0 {
		t.Fatal("Run([stop all]) exit code = 0, want non-zero")
	}
	if !strings.Contains(stdout, "Stopped running tasks") || !strings.Contains(stdout, "signal failed") {
		t.Fatalf("Run([stop all]) stdout = %q, want summary and failure row", stdout)
	}
	if !strings.Contains(stderr, "failed to stop 1 running task(s)") {
		t.Fatalf("Run([stop all]) stderr = %q, want partial failure message", stderr)
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

func TestLogsWithoutJSONPrintsRunFailureWhenNoAgentOutput(t *testing.T) {
	t.Parallel()

	failureMessage := "Command exited with code 1: GraphQL: Could not resolve to a PullRequest with the number of 345. (repository.pullRequest)"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.URL.Path, "/api/v1/loops/loop_1/logs"; got != want {
			t.Fatalf("request path = %q, want %q", got, want)
		}
		writeEnvelope(t, w, pkgapi.Success("req_logs", map[string]any{
			"seq":        12,
			"loopId":     "loop_1",
			"loopType":   "reviewer",
			"loopStatus": "failed",
			"run": map[string]any{
				"runId":        "run_1",
				"currentStep":  "discover",
				"status":       "failed",
				"errorMessage": failureMessage,
			},
			"agent": nil,
		}))
	}))
	defer server.Close()

	configPath := writeCLIConfig(t, server.URL, "")
	exitCode, stdout, stderr := runApp(t, "logs", "loop_1", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run([logs loop_1]) exit code = %d, want 0", exitCode)
	}
	if stderr != "" {
		t.Fatalf("Run([logs loop_1]) stderr = %q, want empty string", stderr)
	}
	for _, want := range []string{"Loop #12 · reviewer · failed", "Run run_1 · step: discover", "Failure: " + failureMessage} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("Run([logs loop_1]) stdout = %q, want to contain %q", stdout, want)
		}
	}
	if strings.Contains(stdout, "No agent output for the current step.") {
		t.Fatalf("Run([logs loop_1]) stdout = %q, did not expect no-output message", stdout)
	}
}

func TestLogsWithoutJSONPrintsRunSummaryWhenAgentOutputEmpty(t *testing.T) {
	t.Parallel()

	summary := "discover failed before agent output was captured"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeEnvelope(t, w, pkgapi.Success("req_logs", map[string]any{
			"seq":        12,
			"loopId":     "loop_1",
			"loopType":   "reviewer",
			"loopStatus": "failed",
			"run":        map[string]any{"runId": "run_1", "currentStep": "discover", "status": "failed", "summary": summary},
			"agent":      map[string]any{"vendor": "codex", "status": "failed", "stdout": "", "stderr": ""},
		}))
	}))
	defer server.Close()

	configPath := writeCLIConfig(t, server.URL, "")
	exitCode, stdout, stderr := runApp(t, "logs", "loop_1", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run([logs loop_1]) exit code = %d, want 0", exitCode)
	}
	if stderr != "" {
		t.Fatalf("Run([logs loop_1]) stderr = %q, want empty string", stderr)
	}
	for _, want := range []string{"Agent: codex", "Failure: " + summary} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("Run([logs loop_1]) stdout = %q, want to contain %q", stdout, want)
		}
	}
	if strings.Contains(stdout, "No output captured.") {
		t.Fatalf("Run([logs loop_1]) stdout = %q, did not expect no-output message", stdout)
	}
}

func TestLogsWithoutJSONDefaultsToStderrWhenStdoutEmpty(t *testing.T) {
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
				"vendor": "opencode",
				"pid":    1234,
				"status": "running",
				"stdout": "",
				"stderr": "stderr line1\nstderr line2\n",
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
	for _, want := range []string{"Loop #12 · reviewer · running", "Run run_1 · step: review", "Agent: opencode · pid 1234 · running", "stderr line1", "stderr line2"} {
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

func TestLogsFollowDefaultsToStderrWhenStdoutEmpty(t *testing.T) {
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
		_, _ = io.WriteString(w, "data: {\"seq\":12,\"loopType\":\"reviewer\",\"loopStatus\":\"running\",\"run\":{\"runId\":\"run_1\",\"status\":\"running\",\"currentStep\":\"review\"},\"agent\":{\"executionId\":\"exec_1\",\"vendor\":\"opencode\",\"pid\":1234,\"status\":\"running\",\"stdout\":\"\",\"stderr\":\"stderr line1\\n\"}}\n\n")
		_, _ = io.WriteString(w, "event: chunk\n")
		_, _ = io.WriteString(w, "data: {\"runId\":\"run_1\",\"currentStep\":\"review\",\"executionId\":\"exec_1\",\"vendor\":\"opencode\",\"pid\":1234,\"status\":\"running\",\"content\":\"stderr line2\\n\"}\n\n")
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
	for _, want := range []string{"Loop #12 · reviewer · running", "Run run_1 · step: review", "Agent: opencode · pid 1234 · running", "stderr line1", "stderr line2"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("Run([logs loop_1 --follow]) stdout = %q, want to contain %q", stdout, want)
		}
	}
}

func TestLogsFollowPrintsFailureAfterEmptyOutputRunFails(t *testing.T) {
	t.Parallel()

	failureMessage := "Command exited with code 1: GraphQL: Could not resolve to a PullRequest with the number of 345. (repository.pullRequest)"
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.URL.Path, "/api/v1/loops/loop_1/logs"; got != want {
			t.Fatalf("request path = %q, want %q", got, want)
		}
		requests++
		if r.URL.Query().Get("follow") == "1" {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: snapshot\n")
			_, _ = io.WriteString(w, "data: {\"seq\":12,\"loopType\":\"reviewer\",\"loopStatus\":\"running\",\"run\":{\"runId\":\"run_1\",\"status\":\"running\",\"currentStep\":\"discover\"},\"agent\":{\"executionId\":\"exec_1\",\"vendor\":\"codex\",\"pid\":1234,\"status\":\"running\",\"stdout\":\"\",\"stderr\":\"\"}}\n\n")
			_, _ = io.WriteString(w, "event: end\n")
			_, _ = io.WriteString(w, "data: {\"reason\":\"run_completed\"}\n\n")
			return
		}
		writeEnvelope(t, w, pkgapi.Success("req_logs", map[string]any{
			"seq":        12,
			"loopId":     "loop_1",
			"loopType":   "reviewer",
			"loopStatus": "failed",
			"run":        map[string]any{"runId": "run_1", "currentStep": "discover", "status": "failed", "errorMessage": failureMessage},
			"agent":      map[string]any{"executionId": "exec_1", "vendor": "codex", "pid": 1234, "status": "failed", "stdout": "", "stderr": ""},
		}))
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
	if requests != 2 {
		t.Fatalf("requests = %d, want follow stream plus final snapshot fetch", requests)
	}
	for _, want := range []string{"Waiting for log output...", "Loop #12 · reviewer · failed", "Failure: " + failureMessage} {
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

	snapshotFlushed := make(chan struct{})
	var flushOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: snapshot\n")
		_, _ = io.WriteString(w, "data: {\"seq\":12,\"loopType\":\"reviewer\",\"loopStatus\":\"running\",\"run\":{\"runId\":\"run_1\",\"status\":\"running\",\"currentStep\":\"review\"},\"agent\":{\"executionId\":\"exec_1\",\"vendor\":\"openai\",\"pid\":1234,\"status\":\"running\",\"stdout\":\"\",\"stderr\":\"\"}}\n\n")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		flushOnce.Do(func() { close(snapshotFlushed) })
		<-r.Context().Done()
	}))
	defer server.Close()

	configPath := writeCLIConfig(t, server.URL, "")
	ctx, cancel := context.WithCancel(context.Background())
	cancelErr := make(chan error, 1)
	go func() {
		select {
		case <-snapshotFlushed:
		case <-time.After(2 * time.Second):
			cancelErr <- errors.New("timed out waiting for snapshot flush")
		}
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
	select {
	case err := <-cancelErr:
		t.Fatal(err)
	default:
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
			metadata, ok := body["metadata"].(map[string]any)
			if !ok {
				t.Fatalf("body.metadata = %#v, want object", body["metadata"])
			}
			if got, want := metadata["manual"], true; got != want {
				t.Fatalf("body.metadata.manual = %#v, want %#v", got, want)
			}
			if got, want := metadata["followUpdates"], false; got != want {
				t.Fatalf("body.metadata.followUpdates = %#v, want %#v", got, want)
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

func TestWorkCreateIssuePrintsReusedWorkerMessage(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/workers":
			if got, want := r.Method, http.MethodPost; got != want {
				t.Fatalf("request method = %q, want %q", got, want)
			}
			writeEnvelope(t, w, pkgapi.Success("req_worker", map[string]any{"id": "loop_existing", "projectId": "project_1", "repo": "acme/looper", "status": "queued", "issueNumber": 54, "title": "Implement issue #54", "baseBranch": "main", "reused": true}))
		default:
			t.Fatalf("unexpected request path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	configPath := writeCLIConfig(t, server.URL, "")
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := New(Deps{Stdout: stdout, Stderr: stderr})

	exitCode := app.Run(context.Background(), []string{"work", "--issue", "54", "--project", "project_1", "--config", configPath})
	if exitCode != 0 {
		t.Fatalf("Run([work --issue 54]) exit code = %d, want 0", exitCode)
	}
	if got := stderr.String(); got != "" {
		t.Fatalf("Run([work --issue 54]) stderr = %q, want empty string", got)
	}
	if got := stdout.String(); !strings.Contains(got, "Existing worker reused") {
		t.Fatalf("Run([work --issue 54]) stdout = %q, want to contain %q", got, "Existing worker reused")
	}
}

func TestPlanCreateIssueResolvesProjectFromCurrentProject(t *testing.T) {
	t.Parallel()

	repoPath := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repoPath, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s) error = %v", repoPath, err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/projects":
			writeEnvelope(t, w, pkgapi.Success("req_projects", map[string]any{"items": []map[string]any{{"id": "project_1", "name": "Looper", "repoPath": repoPath, "repo": "acme/looper", "updatedAt": "2026-04-20T10:00:00.000Z"}}}))
		case "/api/v1/planners":
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
			if got, want := body["issueNumber"], float64(54); got != want {
				t.Fatalf("body.issueNumber = %#v, want %#v", got, want)
			}
			writeEnvelope(t, w, pkgapi.Success("req_planner", map[string]any{"id": "planner_1", "projectId": "project_1", "issueNumber": 54, "status": "queued"}))
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

	exitCode := app.Run(context.Background(), []string{"plan", "--issue", "54", "--config", configPath})
	if exitCode != 0 {
		t.Fatalf("Run([plan --issue 54]) exit code = %d, want 0", exitCode)
	}
	if got := stderr.String(); got != "" {
		t.Fatalf("Run([plan --issue 54]) stderr = %q, want empty string", got)
	}
	if got := stdout.String(); !strings.Contains(got, "Planner started") {
		t.Fatalf("Run([plan --issue 54]) stdout = %q, want to contain %q", got, "Planner started")
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

func TestLoopPauseHelpOnlyShowsSupportedIDForms(t *testing.T) {
	t.Parallel()

	exitCode, stdout, stderr := runApp(t, "loop", "pause", "--help")
	if exitCode != 0 {
		t.Fatalf("Run([loop pause --help]) exit code = %d, want 0", exitCode)
	}
	if stderr != "" {
		t.Fatalf("Run([loop pause --help]) stderr = %q, want empty string", stderr)
	}
	for _, want := range []string{"Usage:\n  looper loop pause [id]", "--id <id>"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("Run([loop pause --help]) stdout = %q, want to contain %q", stdout, want)
		}
	}
	for _, unwanted := range []string{"--type <type>", "--pr <repo#number>"} {
		if strings.Contains(stdout, unwanted) {
			t.Fatalf("Run([loop pause --help]) stdout = %q, want not to contain %q", stdout, unwanted)
		}
	}
}

func TestLoopPauseRequiresID(t *testing.T) {
	t.Parallel()

	exitCode, stdout, stderr := runApp(t, "loop", "pause")
	if exitCode == 0 {
		t.Fatalf("Run([loop pause]) exit code = %d, want non-zero", exitCode)
	}
	if stdout != "" {
		t.Fatalf("Run([loop pause]) stdout = %q, want empty string", stdout)
	}
	if !strings.Contains(stderr, "loop pause requires [id] or --id <id>") {
		t.Fatalf("Run([loop pause]) stderr = %q, want missing id error", stderr)
	}
}

func TestLoopPauseAcceptsPositionalAndFlagID(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		args     []string
		wantPath string
	}{
		{name: "positional", args: []string{"loop", "pause", "loop_123"}, wantPath: "/api/v1/loops/loop_123/pause"},
		{name: "flag", args: []string{"loop", "pause", "--id", "loop_456"}, wantPath: "/api/v1/loops/loop_456/pause"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got, want := r.Method, http.MethodPost; got != want {
					t.Fatalf("request method = %q, want %q", got, want)
				}
				if got, want := r.URL.Path, tc.wantPath; got != want {
					t.Fatalf("request path = %q, want %q", got, want)
				}
				writeEnvelope(t, w, pkgapi.Success("req_loop_pause", map[string]any{"id": strings.TrimSuffix(strings.TrimPrefix(tc.wantPath, "/api/v1/loops/"), "/pause"), "status": "paused"}))
			}))
			defer server.Close()

			configPath := writeCLIConfig(t, server.URL, "")
			args := append(tc.args, "--config", configPath)
			exitCode, stdout, stderr := runApp(t, args...)
			if exitCode != 0 {
				t.Fatalf("Run(%v) exit code = %d, want 0", args, exitCode)
			}
			if stderr != "" {
				t.Fatalf("Run(%v) stderr = %q, want empty string", args, stderr)
			}
			if !strings.Contains(stdout, "Loop paused") {
				t.Fatalf("Run(%v) stdout = %q, want pause success output", args, stdout)
			}
		})
	}
}

func TestTopLevelPauseAndUnpauseUseLoopSequence(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		args       []string
		wantMethod string
		wantPath   string
		wantStatus string
		wantOutput string
	}{
		{name: "pause", args: []string{"pause", "12"}, wantMethod: http.MethodPost, wantPath: "/api/v1/loops/12/pause", wantStatus: "paused", wantOutput: "Loop paused"},
		{name: "unpause", args: []string{"unpause", "12"}, wantMethod: http.MethodPost, wantPath: "/api/v1/loops/12/start", wantStatus: "running", wantOutput: "Loop unpaused"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got := r.Method; got != tc.wantMethod {
					t.Fatalf("request method = %q, want %q", got, tc.wantMethod)
				}
				if got := r.URL.Path; got != tc.wantPath {
					t.Fatalf("request path = %q, want %q", got, tc.wantPath)
				}
				writeEnvelope(t, w, pkgapi.Success("req_loop_status", map[string]any{"id": "loop_seq_12", "seq": 12, "status": tc.wantStatus}))
			}))
			defer server.Close()

			configPath := writeCLIConfig(t, server.URL, "")
			args := append(tc.args, "--config", configPath)
			exitCode, stdout, stderr := runApp(t, args...)
			if exitCode != 0 {
				t.Fatalf("Run(%v) exit code = %d, want 0", args, exitCode)
			}
			if stderr != "" {
				t.Fatalf("Run(%v) stderr = %q, want empty string", args, stderr)
			}
			if !strings.Contains(stdout, tc.wantOutput) || !strings.Contains(stdout, tc.wantStatus) {
				t.Fatalf("Run(%v) stdout = %q, want %q and status %q", args, stdout, tc.wantOutput, tc.wantStatus)
			}
		})
	}
}

func TestTopLevelPauseRequiresNumericSequence(t *testing.T) {
	t.Parallel()

	exitCode, stdout, stderr := runApp(t, "pause", "loop_123")
	if exitCode == 0 {
		t.Fatalf("Run([pause loop_123]) exit code = 0, want non-zero")
	}
	if stdout != "" {
		t.Fatalf("Run([pause loop_123]) stdout = %q, want empty string", stdout)
	}
	if !strings.Contains(stderr, "loop sequence number must be numeric") {
		t.Fatalf("Run([pause loop_123]) stderr = %q, want numeric sequence error", stderr)
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

func (g smokeGitGateway) RestoreWorktree(ctx context.Context, input worker.RestoreWorktreeInput) (*worker.RestoreWorktreeResult, error) {
	record, err := g.gateway.RestoreWorktree(ctx, gitinfra.RestoreWorktreeInput{ProjectID: input.ProjectID, RepoPath: input.RepoPath, Branch: input.Branch, WorktreeRoot: input.WorktreeRoot, CheckoutMode: gitinfra.CheckoutMode(input.CheckoutMode), ExpectedWorktreePath: input.ExpectedWorktreePath})
	if err != nil || record == nil {
		return nil, err
	}
	return &worker.RestoreWorktreeResult{WorktreePath: record.WorktreePath, Branch: record.Branch, BaseBranch: strings.TrimSpace(derefString(record.BaseBranch)), HeadSHA: strings.TrimSpace(derefString(record.HeadSHA)), WorktreeID: record.ID}, nil
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

func (g smokeGitGateway) InspectHead(ctx context.Context, input worker.InspectHeadInput) (worker.InspectHeadResult, error) {
	result, err := g.gateway.InspectHead(ctx, gitinfra.InspectHeadInput{WorktreePath: input.WorktreePath, BaseRef: input.BaseRef})
	if err != nil {
		return worker.InspectHeadResult{}, err
	}
	return worker.InspectHeadResult{HeadSHA: result.HeadSHA, NewCommitSHAs: result.NewCommitSHAs, HasUncommittedChanges: result.HasUncommittedChanges, ChangedFiles: result.ChangedFiles}, nil
}

func (g smokeGitGateway) Commit(ctx context.Context, input worker.CommitInput) (worker.CommitResult, error) {
	result, err := g.gateway.Commit(ctx, gitinfra.CommitInput{WorktreePath: input.WorktreePath, Message: input.Message})
	if err != nil {
		return worker.CommitResult{}, err
	}
	return worker.CommitResult{CommitSHA: result.CommitSHA}, nil
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

func (e smokeAgentExecution) Kill(reason string) error {
	return e.execution.Kill(reason)
}

func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func TestNetworkJoinRemovesLocalStateWhenProjectEnrollmentRollbackFails(t *testing.T) {
	homeDir := t.TempDir()
	configDir := t.TempDir()
	configPath := filepath.Join(configDir, "config.json")
	ghPath := filepath.Join(t.TempDir(), "gh")
	if err := os.WriteFile(ghPath, []byte("#!/bin/sh\nif [ \"$1\" = \"api\" ] && [ \"$2\" = \"user\" ]; then\n  printf '{\"login\":\"worker-1\",\"id\":101}'\n  exit 0\nfi\nprintf 'unexpected gh invocation: %s\\n' \"$*\" >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
	configPayload := map[string]any{
		"tools":    map[string]any{"ghPath": ghPath},
		"roles":    map[string]any{"planner": map[string]any{"autoDiscovery": false}, "fixer": map[string]any{"autoDiscovery": false}},
		"projects": []map[string]any{{"id": "project-1", "name": "Repo", "path": t.TempDir()}},
	}
	raw, err := json.Marshal(configPayload)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if err := os.WriteFile(configPath, raw, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	var leaveCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/join":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"networkId":"net-1","nodeId":"node-1","nodeToken":"node-token"}`))
		case "/v1/leave":
			leaveCalls++
			if got, want := r.Header.Get("Authorization"), "Bearer node-token"; got != want {
				t.Fatalf("leave auth header = %q, want %q", got, want)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`))
		default:
			t.Fatalf("unexpected request path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	runtime := newCommandRuntime(New(Deps{
		Stdout:  stdout,
		Stderr:  stderr,
		HomeDir: homeDir,
		Getwd:   func() (string, error) { return configDir, nil },
		WriteFile: func(path string, data []byte, perm os.FileMode) error {
			if strings.HasSuffix(path, ".tmp") {
				return errors.New("write config temp: permission denied")
			}
			return os.WriteFile(path, data, perm)
		},
	}), []string{"--config", configPath})
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	cmd.Flags().String("key", "", "")
	cmd.Flags().String("name", "", "")
	cmd.Flags().Bool("no-enroll-projects", false, "")
	cmd.Flags().Bool("json", false, "")
	if err := cmd.Flags().Set("key", "join-1"); err != nil {
		t.Fatalf("set key flag: %v", err)
	}
	if err := cmd.Flags().Set("name", "worker-1"); err != nil {
		t.Fatalf("set name flag: %v", err)
	}

	err = runtime.networkJoin(cmd, []string{server.URL})
	if err == nil {
		t.Fatal("networkJoin() error = nil, want temp config write failure")
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	if !strings.Contains(err.Error(), "write config temp: permission denied") {
		t.Fatalf("error = %q, want temp config write failure", err)
	}
	if leaveCalls != 1 {
		t.Fatalf("leave calls = %d, want 1", leaveCalls)
	}
	if _, err := os.Stat(filepath.Join(homeDir, ".looper", "network.json")); !os.IsNotExist(err) {
		t.Fatalf("network state file still present: %v", err)
	}
}

func TestNetworkJoinRejectsAutoEnrollmentWhenPlannerOrFixerAutoDiscoveryIsEnabled(t *testing.T) {
	homeDir := t.TempDir()
	configDir := t.TempDir()
	configPath := filepath.Join(configDir, "config.toml")
	projectPath := t.TempDir()
	raw, err := config.MarshalConfigFile(configPath, config.PartialConfig{
		Roles: &config.PartialRoleConfigs{
			Planner: &config.PartialPlannerRoleConfig{AutoDiscovery: boolPtr(true)},
			Fixer:   &config.PartialFixerRoleConfig{AutoDiscovery: boolPtr(true)},
		},
		Projects: &[]config.PartialProjectRefConfig{{ID: "project-1", Name: "Repo", Path: projectPath}},
	})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if err := os.WriteFile(configPath, raw, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	joinCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		joinCalls++
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"networkId":"net-1","nodeId":"node-1","nodeToken":"node-token"}`))
	}))
	defer server.Close()

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	runtime := newCommandRuntime(New(Deps{Stdout: stdout, Stderr: stderr, HomeDir: homeDir, Getwd: func() (string, error) { return configDir, nil }}), []string{"--config", configPath})
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	cmd.Flags().String("key", "", "")
	cmd.Flags().String("name", "", "")
	cmd.Flags().Bool("no-enroll-projects", false, "")
	cmd.Flags().Bool("json", false, "")
	if err := cmd.Flags().Set("key", "join-1"); err != nil {
		t.Fatalf("set key flag: %v", err)
	}
	if err := cmd.Flags().Set("name", "worker-1"); err != nil {
		t.Fatalf("set name flag: %v", err)
	}

	err = runtime.networkJoin(cmd, []string{server.URL})
	if err == nil {
		t.Fatal("networkJoin() error = nil, want auto-enrollment validation failure")
	}
	if !strings.Contains(err.Error(), "cannot auto-enroll projects in network.mode=routed") || !strings.Contains(err.Error(), "--no-enroll-projects") {
		t.Fatalf("error = %q, want actionable routed enrollment remediation", err)
	}
	if joinCalls != 0 {
		t.Fatalf("join calls = %d, want no remote join attempt", joinCalls)
	}
	if _, err := os.Stat(filepath.Join(homeDir, ".looper", "network.json")); !os.IsNotExist(err) {
		t.Fatalf("network state file present after validation failure: %v", err)
	}
}

func TestNetworkJoinWithNoEnrollProjectsSkipsRoutedEnrollmentValidation(t *testing.T) {
	homeDir := t.TempDir()
	configDir := t.TempDir()
	configPath := filepath.Join(configDir, "config.toml")
	ghPath := filepath.Join(t.TempDir(), "gh")
	projectPath := t.TempDir()
	if err := os.WriteFile(ghPath, []byte("#!/bin/sh\nif [ \"$1\" = \"api\" ] && [ \"$2\" = \"user\" ]; then\n  printf '{\"login\":\"worker-1\",\"id\":101}'\n  exit 0\nfi\nprintf 'unexpected gh invocation: %s\\n' \"$*\" >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
	raw, err := config.MarshalConfigFile(configPath, config.PartialConfig{
		Tools: &config.PartialToolPathsConfig{GHPath: stringPtr(ghPath)},
		Roles: &config.PartialRoleConfigs{
			Planner: &config.PartialPlannerRoleConfig{AutoDiscovery: boolPtr(true)},
			Fixer:   &config.PartialFixerRoleConfig{AutoDiscovery: boolPtr(true)},
		},
		Projects: &[]config.PartialProjectRefConfig{{ID: "project-1", Name: "Repo", Path: projectPath}},
	})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if err := os.WriteFile(configPath, raw, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	joinCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/join" {
			t.Fatalf("unexpected request path: %s", r.URL.Path)
		}
		joinCalls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"networkId":"net-1","nodeId":"node-1","nodeToken":"node-token"}`))
	}))
	defer server.Close()

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	runtime := newCommandRuntime(New(Deps{Stdout: stdout, Stderr: stderr, HomeDir: homeDir, Getwd: func() (string, error) { return configDir, nil }}), []string{"--config", configPath})
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	cmd.Flags().String("key", "", "")
	cmd.Flags().String("name", "", "")
	cmd.Flags().Bool("no-enroll-projects", false, "")
	cmd.Flags().Bool("json", false, "")
	if err := cmd.Flags().Set("key", "join-1"); err != nil {
		t.Fatalf("set key flag: %v", err)
	}
	if err := cmd.Flags().Set("name", "worker-1"); err != nil {
		t.Fatalf("set name flag: %v", err)
	}
	if err := cmd.Flags().Set("no-enroll-projects", "true"); err != nil {
		t.Fatalf("set no-enroll-projects flag: %v", err)
	}

	if err := runtime.networkJoin(cmd, []string{server.URL}); err != nil {
		t.Fatalf("networkJoin() error = %v", err)
	}
	if joinCalls != 1 {
		t.Fatalf("join calls = %d, want 1", joinCalls)
	}
	state, err := client.LoadState(filepath.Join(homeDir, ".looper", "network.json"))
	if err != nil {
		t.Fatalf("LoadState() error = %v", err)
	}
	if state.URL != server.URL || state.NodeName != "worker-1" {
		t.Fatalf("state = %#v, want saved routed membership", state)
	}
	updated, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if strings.Contains(string(updated), "network") {
		t.Fatalf("config = %s, want project network mode unchanged when --no-enroll-projects is used", string(updated))
	}
}

func TestNetworkJoinIgnoresCLIOnlyFlagsWhenLoadingConfig(t *testing.T) {
	t.Parallel()

	homeDir := t.TempDir()
	configDir := t.TempDir()
	configPath := filepath.Join(configDir, "config.toml")
	ghPath := filepath.Join(t.TempDir(), "gh")
	projectPath := t.TempDir()
	if err := os.WriteFile(ghPath, []byte("#!/bin/sh\nif [ \"$1\" = \"api\" ] && [ \"$2\" = \"user\" ]; then\n  printf '{\"login\":\"worker-1\",\"id\":101}'\n  exit 0\nfi\nprintf 'unexpected gh invocation: %s\\n' \"$*\" >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
	raw, err := config.MarshalConfigFile(configPath, config.PartialConfig{
		Tools: &config.PartialToolPathsConfig{GHPath: stringPtr(ghPath)},
		Roles: &config.PartialRoleConfigs{
			Planner: &config.PartialPlannerRoleConfig{AutoDiscovery: boolPtr(false)},
			Fixer:   &config.PartialFixerRoleConfig{AutoDiscovery: boolPtr(false)},
		},
		Projects: &[]config.PartialProjectRefConfig{{ID: "project-1", Name: "Repo", Path: projectPath}},
	})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if err := os.WriteFile(configPath, raw, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/join" {
			t.Fatalf("unexpected request path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"networkId":"net-1","nodeId":"node-1","nodeToken":"node-token"}`))
	}))
	defer server.Close()

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	runtime := newCommandRuntime(New(Deps{Stdout: stdout, Stderr: stderr, HomeDir: homeDir, Getwd: func() (string, error) { return configDir, nil }}), []string{"network", "join", server.URL, "--key", "join-1", "--name", "worker-1", "--json", "--config", configPath})
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	cmd.Flags().String("key", "", "")
	cmd.Flags().String("name", "", "")
	cmd.Flags().Bool("no-enroll-projects", false, "")
	cmd.Flags().Bool("json", false, "")
	if err := cmd.Flags().Set("key", "join-1"); err != nil {
		t.Fatalf("set key flag: %v", err)
	}
	if err := cmd.Flags().Set("name", "worker-1"); err != nil {
		t.Fatalf("set name flag: %v", err)
	}
	if err := cmd.Flags().Set("json", "true"); err != nil {
		t.Fatalf("set json flag: %v", err)
	}

	if err := runtime.networkJoin(cmd, []string{server.URL}); err != nil {
		t.Fatalf("networkJoin() error = %v", err)
	}
	assertJSONContains(t, stdout.String(), "networkId", "net-1")
	updated, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if !strings.Contains(string(updated), "mode = 'routed'") {
		t.Fatalf("config = %s, want routed project mode written", string(updated))
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestNetworkJoinPreservesLocalStateWhenProjectEnrollmentRollbackLeaveFails(t *testing.T) {
	homeDir := t.TempDir()
	configDir := t.TempDir()
	configPath := filepath.Join(configDir, "config.json")
	ghPath := filepath.Join(t.TempDir(), "gh")
	if err := os.WriteFile(ghPath, []byte("#!/bin/sh\nif [ \"$1\" = \"api\" ] && [ \"$2\" = \"user\" ]; then\n  printf '{\"login\":\"worker-1\",\"id\":101}'\n  exit 0\nfi\nprintf 'unexpected gh invocation: %s\\n' \"$*\" >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
	configPayload := map[string]any{
		"tools":    map[string]any{"ghPath": ghPath},
		"roles":    map[string]any{"planner": map[string]any{"autoDiscovery": false}, "fixer": map[string]any{"autoDiscovery": false}},
		"projects": []map[string]any{{"id": "project-1", "name": "Repo", "path": t.TempDir()}},
	}
	raw, err := json.Marshal(configPayload)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if err := os.WriteFile(configPath, raw, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	var leaveCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/join":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"networkId":"net-1","nodeId":"node-1","nodeToken":"node-token"}`))
		case "/v1/leave":
			leaveCalls++
			if got, want := r.Header.Get("Authorization"), "Bearer node-token"; got != want {
				t.Fatalf("leave auth header = %q, want %q", got, want)
			}
			http.Error(w, "rollback unavailable", http.StatusBadGateway)
		default:
			t.Fatalf("unexpected request path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	runtime := newCommandRuntime(New(Deps{
		Stdout:  stdout,
		Stderr:  stderr,
		HomeDir: homeDir,
		Getwd:   func() (string, error) { return configDir, nil },
		WriteFile: func(path string, data []byte, perm os.FileMode) error {
			if strings.HasSuffix(path, ".tmp") {
				return errors.New("write config temp: permission denied")
			}
			return os.WriteFile(path, data, perm)
		},
	}), []string{"--config", configPath})
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	cmd.Flags().String("key", "", "")
	cmd.Flags().String("name", "", "")
	cmd.Flags().Bool("no-enroll-projects", false, "")
	cmd.Flags().Bool("json", false, "")
	if err := cmd.Flags().Set("key", "join-1"); err != nil {
		t.Fatalf("set key flag: %v", err)
	}
	if err := cmd.Flags().Set("name", "worker-1"); err != nil {
		t.Fatalf("set name flag: %v", err)
	}

	err = runtime.networkJoin(cmd, []string{server.URL})
	if err == nil {
		t.Fatal("networkJoin() error = nil, want rollback leave failure")
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	if !strings.Contains(err.Error(), "write config temp: permission denied") {
		t.Fatalf("error = %q, want temp config write failure", err)
	}
	if !strings.Contains(err.Error(), "status 502") {
		t.Fatalf("error = %q, want rollback leave failure", err)
	}
	if leaveCalls != 1 {
		t.Fatalf("leave calls = %d, want 1", leaveCalls)
	}
	state, loadErr := client.LoadState(filepath.Join(homeDir, ".looper", "network.json"))
	if loadErr != nil {
		t.Fatalf("LoadState() error = %v, want preserved local state", loadErr)
	}
	if state.NodeToken != "node-token" {
		t.Fatalf("saved node token = %q, want preserved token", state.NodeToken)
	}
	if state.URL != server.URL {
		t.Fatalf("saved url = %q, want %q", state.URL, server.URL)
	}
}

func TestNetworkLeaveRemovesLocalStateWhenProjectModeUpdateFails(t *testing.T) {
	homeDir := t.TempDir()
	configDir := t.TempDir()
	configPath := filepath.Join(configDir, "config.json")
	projectPath := t.TempDir()
	raw, err := config.MarshalConfigFile(configPath, config.PartialConfig{
		Projects: &[]config.PartialProjectRefConfig{{ID: "project-1", Name: "Repo", Path: projectPath}},
	})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if err := os.WriteFile(configPath, raw, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := client.SaveState(client.DefaultStatePath(homeDir), client.LocalState{URL: "http://127.0.0.1", NetworkID: "net-1", NodeID: "node-1", NodeName: "worker-1", NodeToken: "node-token"}); err != nil {
		t.Fatalf("save state: %v", err)
	}

	var leaveCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/leave" {
			t.Fatalf("unexpected request path: %s", r.URL.Path)
		}
		leaveCalls++
		if got, want := r.Header.Get("Authorization"), "Bearer node-token"; got != want {
			t.Fatalf("leave auth header = %q, want %q", got, want)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	if err := client.SaveState(client.DefaultStatePath(homeDir), client.LocalState{URL: server.URL, NetworkID: "net-1", NodeID: "node-1", NodeName: "worker-1", NodeToken: "node-token"}); err != nil {
		t.Fatalf("save state with server url: %v", err)
	}

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	runtime := newCommandRuntime(New(Deps{
		Stdout:  stdout,
		Stderr:  stderr,
		HomeDir: homeDir,
		Getwd:   func() (string, error) { return configDir, nil },
		WriteFile: func(path string, data []byte, perm os.FileMode) error {
			if strings.HasSuffix(path, ".tmp") {
				return errors.New("write config temp: permission denied")
			}
			return os.WriteFile(path, data, perm)
		},
	}), []string{"--config", configPath})
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	cmd.Flags().Bool("json", false, "")

	err = runtime.networkLeave(cmd, nil)
	if err == nil {
		t.Fatal("networkLeave() error = nil, want temp config write failure")
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	if !strings.Contains(err.Error(), "write config temp: permission denied") {
		t.Fatalf("error = %q, want temp config write failure", err)
	}
	if leaveCalls != 1 {
		t.Fatalf("leave calls = %d, want 1", leaveCalls)
	}
	if _, err := os.Stat(filepath.Join(homeDir, ".looper", "network.json")); !os.IsNotExist(err) {
		t.Fatalf("network state file still present: %v", err)
	}
}

func TestResolveNetworkStatusIgnoresCLIOnlyFlagsWhenLoadingConfig(t *testing.T) {
	t.Parallel()

	homeDir := t.TempDir()
	configDir := t.TempDir()
	configPath := filepath.Join(configDir, "config.toml")
	ghPath := filepath.Join(t.TempDir(), "gh")
	projectPath := t.TempDir()
	if err := os.WriteFile(ghPath, []byte("#!/bin/sh\nif [ \"$1\" = \"api\" ] && [ \"$2\" = \"user\" ]; then\n  printf '{\"login\":\"worker-1\",\"id\":101}'\n  exit 0\nfi\nprintf 'unexpected gh invocation: %s\\n' \"$*\" >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
	raw, err := config.MarshalConfigFile(configPath, config.PartialConfig{
		Tools:    &config.PartialToolPathsConfig{GHPath: stringPtr(ghPath)},
		Projects: &[]config.PartialProjectRefConfig{{ID: "project-1", Name: "Repo", Path: projectPath}},
	})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if err := os.WriteFile(configPath, raw, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	runtime := newCommandRuntime(New(Deps{HomeDir: homeDir, Getwd: func() (string, error) { return configDir, nil }}), []string{"network", "status", "--verbose", "--json", "--config", configPath})
	status, err := runtime.resolveNetworkStatus(context.Background())
	if err != nil {
		t.Fatalf("resolveNetworkStatus() error = %v", err)
	}
	if status.Configured {
		t.Fatalf("status.Configured = true, want false without saved network state")
	}
	if status.CurrentGitHub.Login != "worker-1" || status.CurrentGitHub.NumericID != 101 {
		t.Fatalf("status.CurrentGitHub = %#v, want fake gh identity", status.CurrentGitHub)
	}
	if status.LocalProjects != 1 || status.RoutedProjects != 0 {
		t.Fatalf("project counts = (%d, %d), want (1, 0)", status.LocalProjects, status.RoutedProjects)
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

func writeEditableCLIConfig(t *testing.T) string {
	t.Helper()

	return writeEditableCLIConfigWithPayload(t, map[string]any{
		"notifications": map[string]any{
			"osascript": map[string]any{"enabled": false},
		},
		"defaults": map[string]any{
			"allowRiskyFixes":    false,
			"fixAllPullRequests": false,
		},
	})
}

func writeEditableCLIConfigWithPayload(t *testing.T, configPayload map[string]any) string {
	t.Helper()

	configPath := filepath.Join(t.TempDir(), "config.json")
	raw, err := json.Marshal(configPayload)
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
