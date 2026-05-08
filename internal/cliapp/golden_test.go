package cliapp

import (
	"bytes"
	"context"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pkgapi "github.com/nexu-io/looper/pkg/api"
)

var updateGolden = flag.Bool("update", false, "update CLI golden fixtures")

func TestCLIGoldenOutputs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		fixture    string
		exitCode   int
		run        func(t *testing.T) (int, string, string)
		stderrFile string
	}{
		{
			name:     "root_help",
			fixture:  "root-help.stdout.golden",
			exitCode: 0,
			run: func(t *testing.T) (int, string, string) {
				exitCode, stdout, stderr := runApp(t, "--help")
				return exitCode, stdout, stderr
			},
		},
		{
			name:     "daemon_help",
			fixture:  "daemon-help.stdout.golden",
			exitCode: 0,
			run: func(t *testing.T) (int, string, string) {
				exitCode, stdout, stderr := runApp(t, "daemon", "--help")
				return exitCode, stdout, stderr
			},
		},
		{
			name:     "status_human",
			fixture:  "status-human.stdout.golden",
			exitCode: 0,
			run: func(t *testing.T) (int, string, string) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != "/api/v1/status" {
						t.Fatalf("request path = %q, want %q", r.URL.Path, "/api/v1/status")
					}
					writeEnvelope(t, w, pkgapi.Success("req_status", map[string]any{
						"service":   map[string]any{"healthy": true, "version": "1.2.3", "daemonMode": "full", "startedAt": "2026-04-20T10:00:00.000Z"},
						"storage":   map[string]any{"dbPath": "/tmp/looper.sqlite", "schemaVersion": "12", "healthy": true, "pendingMigrations": []string{}},
						"scheduler": map[string]any{"healthy": true, "queuedItems": 2, "runningItems": 1},
						"loops": map[string]any{
							"planner":  map[string]any{"queued": 0, "running": 0, "waiting": 0, "paused": 1, "failed": 0, "terminated": 0, "stopped": 0},
							"reviewer": map[string]any{"queued": 1, "running": 1, "waiting": 1, "paused": 0, "failed": 0, "terminated": 0, "stopped": 0},
							"worker":   map[string]any{"queued": 0, "running": 2, "waiting": 0, "paused": 0, "failed": 1, "terminated": 1, "stopped": 0},
							"fixer":    map[string]any{"queued": 0, "running": 0, "waiting": 0, "paused": 0, "failed": 0, "terminated": 0, "stopped": 1},
						},
						"tools":         map[string]any{"git": true, "gh": false, "osascript": true},
						"notifications": map[string]any{"inAppEnabled": true, "osascriptEnabled": false},
					}))
				}))
				defer server.Close()

				configPath := writeCLIConfig(t, server.URL, "")
				exitCode, stdout, stderr := runApp(t, "status", "--config", configPath)
				return exitCode, stdout, stderr
			},
		},
		{
			name:     "project_list_human",
			fixture:  "project-list.stdout.golden",
			exitCode: 0,
			run: func(t *testing.T) (int, string, string) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if got, want := r.URL.Path, "/api/v1/projects"; got != want {
						t.Fatalf("request path = %q, want %q", got, want)
					}
					writeEnvelope(t, w, pkgapi.Success("req_projects", map[string]any{"items": []map[string]any{{"id": "project_1", "name": "Looper", "repoPath": "/tmp/repo", "baseBranch": "main", "repo": "acme/looper", "updatedAt": "2026-04-20T10:00:00.000Z"}, {"id": "project_2", "name": "CLI", "repoPath": "/tmp/cli", "baseBranch": "develop", "repo": nil, "updatedAt": "2026-04-20T11:30:00.000Z"}}}))
				}))
				defer server.Close()

				configPath := writeCLIConfig(t, server.URL, "")
				exitCode, stdout, stderr := runApp(t, "project", "list", "--config", configPath)
				return exitCode, stdout, stderr
			},
		},
		{
			name:     "logs_tail_human",
			fixture:  "logs-tail.stdout.golden",
			exitCode: 0,
			run: func(t *testing.T) (int, string, string) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if got, want := r.URL.Path, "/api/v1/loops/loop_1/logs"; got != want {
						t.Fatalf("request path = %q, want %q", got, want)
					}
					writeEnvelope(t, w, pkgapi.Success("req_logs", map[string]any{"seq": 12, "loopId": "loop_1", "loopType": "reviewer", "loopStatus": "running", "run": map[string]any{"runId": "run_1", "currentStep": "review"}, "agent": map[string]any{"vendor": "openai", "pid": 1234, "status": "running", "stdout": "line1\nline2\nline3\n", "stderr": "err1\nerr2\n"}}))
				}))
				defer server.Close()

				configPath := writeCLIConfig(t, server.URL, "")
				exitCode, stdout, stderr := runApp(t, "logs", "loop_1", "--tail", "2", "--config", configPath)
				return exitCode, stdout, stderr
			},
		},
		{
			name:     "upgrade_check",
			fixture:  "upgrade-check.stdout.golden",
			exitCode: 0,
			run: func(t *testing.T) (int, string, string) {
				configPath := writeCLIConfig(t, "http://127.0.0.1:4321", "")
				homeDir := t.TempDir()
				stdout := &bytes.Buffer{}
				stderr := &bytes.Buffer{}
				managedPath := filepath.Join(homeDir, ".looper", "bin", "looperd")

				app := New(Deps{
					Stdout:  stdout,
					Stderr:  stderr,
					HomeDir: homeDir,
					HTTPClient: newTestHTTPClient(func(req *http.Request) (*http.Response, error) {
						switch req.URL.String() {
						case "http://127.0.0.1:4321/api/v1/status":
							return nil, os.ErrNotExist
						case "https://api.github.com/repos/nexu-io/looper/releases/latest":
							return jsonResponse(t, http.StatusOK, `{"tag_name":"v0.3.0","assets":[]}`), nil
						default:
							t.Fatalf("unexpected request URL %q", req.URL.String())
							return nil, nil
						}
					}),
					RunCommand: func(ctx context.Context, command string, args []string, timeoutDuration time.Duration) (commandExecutionResult, error) {
						_ = ctx
						_ = timeoutDuration
						if command == managedPath {
							return commandExecutionResult{Stdout: "0.2.1\n", ExitCode: 0}, nil
						}
						return commandExecutionResult{ExitCode: 1, Stderr: "not found"}, nil
					},
				})

				exitCode := app.Run(context.Background(), []string{"upgrade", "--check", "--config", configPath})
				if exitCode != 0 {
					t.Fatalf("Run([upgrade --check]) exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
				}
				normalizedStdout := strings.ReplaceAll(stdout.String(), homeDir, "__HOME__")
				return exitCode, normalizedStdout, stderr.String()
			},
		},
	}

	for _, testCase := range tests {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			exitCode, stdout, stderr := testCase.run(t)
			if exitCode != testCase.exitCode {
				t.Fatalf("exitCode = %d, want %d", exitCode, testCase.exitCode)
			}
			assertGoldenFile(t, testCase.fixture, stdout)
			if testCase.stderrFile != "" {
				assertGoldenFile(t, testCase.stderrFile, stderr)
			} else if stderr != "" {
				t.Fatalf("stderr = %q, want empty string", stderr)
			}
		})
	}
}

func assertGoldenFile(t *testing.T, fixture string, got string) {
	t.Helper()

	path := filepath.Join("testdata", "golden", fixture)
	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("os.MkdirAll(%q) error = %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("os.WriteFile(%q) error = %v", path, err)
		}
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("os.ReadFile(%q) error = %v", path, err)
	}
	if got != string(want) {
		t.Fatalf("golden mismatch for %s\n--- got ---\n%s\n--- want ---\n%s", fixture, got, string(want))
	}
}
