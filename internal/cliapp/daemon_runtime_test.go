package cliapp

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	pkgapi "github.com/nexu-io/looper/pkg/api"
)

func TestDaemonStatusJSONFallsBackToBinaryVersion(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/status":
			w.WriteHeader(http.StatusServiceUnavailable)
			writeEnvelope(t, w, map[string]any{
				"ok":        false,
				"requestId": "req_status",
				"error":     map[string]any{"message": "offline"},
			})
		case "/api/v1/healthz":
			writeEnvelope(t, w, pkgapi.Success("req_health", map[string]any{"status": "ok"}))
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	defer server.Close()

	homeDir := t.TempDir()
	configPath := writeDaemonCLIConfig(t, server.URL)
	managedPath := filepath.Join(homeDir, ".looper", "bin", "looperd")
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := New(Deps{
		Stdout:  stdout,
		Stderr:  stderr,
		HomeDir: homeDir,
		RunCommand: func(ctx context.Context, command string, args []string, timeout time.Duration) (commandExecutionResult, error) {
			_ = ctx
			_ = timeout
			if command == managedPath && strings.Join(args, " ") == "--version" {
				return commandExecutionResult{Stdout: "0.4.0\n", ExitCode: 0}, nil
			}
			return commandExecutionResult{ExitCode: 1, Stderr: "not found"}, nil
		},
	})

	exitCode := app.Run(context.Background(), []string{"daemon", "status", "--json", "--config", configPath})
	if exitCode != 0 {
		t.Fatalf("Run([daemon status --json]) exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("Run([daemon status --json]) stderr = %q, want empty string", stderr.String())
	}
	assertJSONContains(t, stdout.String(), "apiReachable", true)
	assertJSONContains(t, stdout.String(), "daemonVersion", "0.4.0")
	assertJSONContains(t, stdout.String(), "daemonVersionSource", "binary")
	assertJSONContains(t, stdout.String(), "daemonBinaryPath", managedPath)
	assertJSONContains(t, stdout.String(), "health", map[string]any{"status": "ok"})
}

func TestDaemonStatusJSONUsesAPIVersionAndBinaryPath(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/status":
			writeEnvelope(t, w, pkgapi.Success("req_status", map[string]any{
				"service": map[string]any{
					"version": "0.6.0",
					"binary": map[string]any{
						"path": "/opt/looper/bin/looperd",
					},
				},
			}))
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	defer server.Close()

	configPath := writeDaemonCLIConfig(t, server.URL)
	homeDir := t.TempDir()
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := New(Deps{
		Stdout:  stdout,
		Stderr:  stderr,
		HomeDir: homeDir,
		RunCommand: func(ctx context.Context, command string, args []string, timeout time.Duration) (commandExecutionResult, error) {
			_ = ctx
			_ = command
			_ = args
			_ = timeout
			t.Fatal("RunCommand should not be called when API version is available")
			return commandExecutionResult{}, nil
		},
	})

	exitCode := app.Run(context.Background(), []string{"daemon", "status", "--json", "--config", configPath})
	if exitCode != 0 {
		t.Fatalf("Run([daemon status --json]) exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("Run([daemon status --json]) stderr = %q, want empty string", stderr.String())
	}
	assertJSONContains(t, stdout.String(), "apiReachable", true)
	assertJSONContains(t, stdout.String(), "daemonVersion", "0.6.0")
	assertJSONContains(t, stdout.String(), "daemonVersionSource", "api")
	assertJSONContains(t, stdout.String(), "daemonBinaryPath", "/opt/looper/bin/looperd")
}

func TestDaemonStatusJSONIgnoresManagedProbeFailureWhenAPIAlreadyReportedVersion(t *testing.T) {
	t.Parallel()

	homeDir := t.TempDir()
	managedPath := filepath.Join(homeDir, ".looper", "bin", "looperd")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/status":
			writeEnvelope(t, w, pkgapi.Success("req_status", map[string]any{
				"service": map[string]any{
					"version": "0.6.0",
					"binary": map[string]any{
						"path": managedPath,
					},
				},
			}))
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	defer server.Close()

	configPath := writeDaemonCLIConfig(t, server.URL)
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := New(Deps{
		Stdout:  stdout,
		Stderr:  stderr,
		HomeDir: homeDir,
		RunCommand: func(ctx context.Context, command string, args []string, timeout time.Duration) (commandExecutionResult, error) {
			_ = ctx
			_ = timeout
			if command == managedPath && strings.Join(args, " ") == "--version" {
				return commandExecutionResult{ExitCode: 1, Stderr: "permission denied"}, nil
			}
			return commandExecutionResult{ExitCode: 1, Stderr: "not found"}, nil
		},
	})

	exitCode := app.Run(context.Background(), []string{"daemon", "status", "--json", "--config", configPath})
	if exitCode != 0 {
		t.Fatalf("Run([daemon status --json]) exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("Run([daemon status --json]) stderr = %q, want empty string", stderr.String())
	}
	assertJSONContains(t, stdout.String(), "apiReachable", true)
	assertJSONContains(t, stdout.String(), "daemonVersion", "0.6.0")
	assertJSONContains(t, stdout.String(), "daemonVersionSource", "api")
	assertJSONContains(t, stdout.String(), "daemonBinaryPath", managedPath)

	var decoded map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
		t.Fatalf("unmarshal stdout JSON: %v\nraw=%q", err, stdout.String())
	}
	if _, ok := decoded["managedDaemonVersion"]; ok {
		t.Fatalf("stdout JSON unexpectedly included managedDaemonVersion: %#v", decoded)
	}
	if _, ok := decoded["managedDaemonBinaryPath"]; ok {
		t.Fatalf("stdout JSON unexpectedly included managedDaemonBinaryPath: %#v", decoded)
	}
}

func TestDaemonUpgradePendingRestart(t *testing.T) {
	t.Parallel()

	managedPath := "/tmp/.looper/bin/looperd"
	otherPath := "/usr/local/bin/looperd"
	for _, tc := range []struct {
		name        string
		lifecycle   daemonLifecycleStatus
		running     *daemonVersionState
		managed     *daemonVersionState
		wantPending bool
		wantVersion string
	}{
		{
			name:        "running with different managed version",
			lifecycle:   daemonLifecycleStatus{Process: "running"},
			running:     &daemonVersionState{Version: "0.2.0", BinaryPath: stringPtr(managedPath)},
			managed:     &daemonVersionState{Version: "0.3.0", BinaryPath: stringPtr(managedPath)},
			wantPending: true,
			wantVersion: "0.3.0",
		},
		{
			name:        "stopped process",
			lifecycle:   daemonLifecycleStatus{Process: "stopped"},
			running:     &daemonVersionState{Version: "0.2.0", BinaryPath: stringPtr(managedPath)},
			managed:     &daemonVersionState{Version: "0.3.0", BinaryPath: stringPtr(managedPath)},
			wantPending: false,
		},
		{
			name:        "mismatched binary paths",
			lifecycle:   daemonLifecycleStatus{Process: "running"},
			running:     &daemonVersionState{Version: "0.2.0", BinaryPath: stringPtr(otherPath)},
			managed:     &daemonVersionState{Version: "0.3.0", BinaryPath: stringPtr(managedPath)},
			wantPending: false,
		},
		{
			name:        "same version",
			lifecycle:   daemonLifecycleStatus{Process: "running"},
			running:     &daemonVersionState{Version: "0.3.0", BinaryPath: stringPtr(managedPath)},
			managed:     &daemonVersionState{Version: "0.3.0", BinaryPath: stringPtr(managedPath)},
			wantPending: false,
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotPending, gotVersion := daemonUpgradePendingRestart(tc.lifecycle, tc.running, tc.managed)
			if gotPending != tc.wantPending {
				t.Fatalf("daemonUpgradePendingRestart() pending = %v, want %v", gotPending, tc.wantPending)
			}
			if got := derefString(gotVersion); got != tc.wantVersion {
				t.Fatalf("daemonUpgradePendingRestart() version = %q, want %q", got, tc.wantVersion)
			}
		})
	}
}

func TestDaemonStartWritesPIDFileAndPassesConfigArgs(t *testing.T) {
	t.Parallel()

	homeDir := t.TempDir()
	managedPath := filepath.Join(homeDir, ".looper", "bin", "looperd")
	configPath := writeDaemonCLIConfig(t, "http://daemon.test")
	daemonWorkingDir := filepath.Join(filepath.Dir(configPath), "working")
	statusRequests := 0
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	spawned := struct {
		command string
		args    []string
		cwd     string
	}{}
	writes := map[string]string{}
	var mkdirPath string
	killCalls := make([]struct {
		pid    int
		signal int
	}, 0)

	app := New(Deps{
		Stdout:  stdout,
		Stderr:  stderr,
		HomeDir: homeDir,
		HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			statusRequests++
			if statusRequests == 1 {
				return nil, os.ErrNotExist
			}
			return jsonResponse(t, http.StatusOK, `{"ok":true,"requestId":"req_status","data":{"service":{"binary":{"name":"looperd"}}}}`), nil
		})},
		Getwd: func() (string, error) {
			return "/tmp/start-directory", nil
		},
		ReadFile: func(path string) ([]byte, error) {
			return nil, os.ErrNotExist
		},
		RunCommand: func(ctx context.Context, command string, args []string, timeout time.Duration) (commandExecutionResult, error) {
			_ = ctx
			_ = timeout
			if command == managedPath && strings.Join(args, " ") == "--version" {
				return commandExecutionResult{Stdout: "0.3.0\n", ExitCode: 0}, nil
			}
			if command == "ps" && len(args) >= 2 && args[1] == "4321" {
				return commandExecutionResult{Stdout: managedPath + "\n", ExitCode: 0}, nil
			}
			return commandExecutionResult{ExitCode: 1, Stderr: "not found"}, nil
		},
		SpawnDetached: func(command string, args []string, cwd string, env []string) (int, error) {
			_ = env
			spawned.command = command
			spawned.args = append([]string{}, args...)
			spawned.cwd = cwd
			return 4321, nil
		},
		KillProcess: func(pid int, signal int) error {
			killCalls = append(killCalls, struct {
				pid    int
				signal int
			}{pid: pid, signal: signal})
			return nil
		},
		MkdirAll: func(path string, perm os.FileMode) error {
			_ = perm
			mkdirPath = path
			return nil
		},
		WriteFile: func(path string, data []byte, perm os.FileMode) error {
			_ = perm
			writes[path] = string(data)
			return nil
		},
		Sleep: func(duration time.Duration) {
			_ = duration
		},
	})

	exitCode := app.Run(context.Background(), []string{"daemon", "start", "--config", configPath, "--db-path", "/tmp/looper.sqlite"})
	if exitCode != 0 {
		t.Fatalf("Run([daemon start]) exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("Run([daemon start]) stderr = %q, want empty string", stderr.String())
	}
	if spawned.command != managedPath {
		t.Fatalf("spawned.command = %q, want %q", spawned.command, managedPath)
	}
	if got, want := strings.Join(spawned.args, "\n"), strings.Join([]string{"--config", configPath, "--db-path", "/tmp/looper.sqlite"}, "\n"); got != want {
		t.Fatalf("spawned.args = %#v, want %#v", spawned.args, []string{"--config", configPath, "--db-path", "/tmp/looper.sqlite"})
	}
	if spawned.cwd != daemonWorkingDir {
		t.Fatalf("spawned.cwd = %q, want %q", spawned.cwd, daemonWorkingDir)
	}
	if got, want := mkdirPath, filepath.Join(homeDir, ".looper"); got != want {
		t.Fatalf("mkdirPath = %q, want %q", got, want)
	}
	pidPath := filepath.Join(homeDir, ".looper", "looperd.pid")
	if writes[pidPath] != "4321\n" {
		t.Fatalf("pid write = %q, want %q", writes[pidPath], "4321\n")
	}
	statePath := filepath.Join(homeDir, ".looper", "looperd.state.json")
	if !strings.Contains(writes[statePath], `"mode": "foreground"`) || !strings.Contains(writes[statePath], `"pid": 4321`) {
		t.Fatalf("state write = %q, want foreground state with pid", writes[statePath])
	}
	if len(killCalls) != 2 || killCalls[0].pid != 4321 || killCalls[0].signal != 0 || killCalls[1].pid != 4321 || killCalls[1].signal != 0 {
		t.Fatalf("killCalls = %#v, want readiness liveness probes for pid 4321", killCalls)
	}
	if !strings.Contains(stdout.String(), "Started looperd") {
		t.Fatalf("stdout = %q, want start confirmation", stdout.String())
	}
}

func TestDaemonLifecycleStateReadWriteAndStaleDetection(t *testing.T) {
	t.Parallel()

	homeDir := t.TempDir()
	statePath := filepath.Join(homeDir, ".looper", "looperd.state.json")
	startedAt := time.Date(2026, 5, 2, 10, 15, 30, 0, time.UTC)
	app := New(Deps{
		HomeDir: homeDir,
		KillProcess: func(pid int, signal int) error {
			_ = signal
			if pid == 9999 {
				return os.ErrProcessDone
			}
			return nil
		},
	})
	runtime := newCommandRuntime(app, nil)
	state := daemonLifecycleState{Mode: "foreground", PID: 9999, StartedAt: &startedAt, BinaryPath: "/tmp/looperd", Logs: daemonLifecycleLogState{Main: "/tmp/looperd.log"}}
	if err := runtime.writeDaemonLifecycleState(statePath, state); err != nil {
		t.Fatalf("writeDaemonLifecycleState() error = %v", err)
	}
	readState, err := runtime.readDaemonLifecycleState(statePath)
	if err != nil {
		t.Fatalf("readDaemonLifecycleState() error = %v", err)
	}
	if readState.SchemaVersion != daemonStateSchemaVersion || readState.PID != 9999 || readState.BinaryPath != "/tmp/looperd" {
		t.Fatalf("read state = %#v, want persisted schema/pid/binary", readState)
	}

	loaded := writeDaemonCLIConfig(t, "http://daemon.test")
	status, err := runtime.daemonLifecycleStatus(context.Background(), mustLoadConfig(t, loaded))
	if err != nil {
		t.Fatalf("daemonLifecycleStatus() error = %v", err)
	}
	if status.Process != "exited" || !status.StaleState || status.State.LastExit == nil || !strings.Contains(status.State.LastExit.Reason, "no longer running") {
		t.Fatalf("status = %#v, want inferred stale exit", status)
	}
}

func TestGenerateLaunchdPlistRestartPolicyMapping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		policy     string
		want       string
		wantAbsent string
	}{
		{name: "never", policy: "never", wantAbsent: "<key>KeepAlive</key>"},
		{name: "on failure", policy: "on-failure", want: "<key>SuccessfulExit</key>\n\t\t<false/>"},
		{name: "always", policy: "always", want: "<key>KeepAlive</key>\n\t<true/>"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			plist, err := generateLaunchdPlist(launchdPlistConfig{Label: launchdLooperdLabel, ProgramArguments: []string{"/bin/looperd", "--config", "/tmp/config.json"}, WorkingDirectory: "/tmp", Environment: map[string]string{"LOOPER_CONFIG": "/tmp/config.json"}, RunAtLoad: true, RestartPolicy: config.DaemonRestartPolicy(tt.policy), RestartThrottleSeconds: 10, StandardOutPath: "/tmp/stdout.log", StandardErrorPath: "/tmp/stderr.log"})
			if err != nil {
				t.Fatalf("generateLaunchdPlist() error = %v", err)
			}
			got := string(plist)
			for _, want := range []string{"<key>Label</key>", launchdLooperdLabel, "<key>ProgramArguments</key>", "<key>RunAtLoad</key>", "<key>ThrottleInterval</key>", "<integer>10</integer>", "<key>StandardOutPath</key>", "<key>StandardErrorPath</key>"} {
				if !strings.Contains(got, want) {
					t.Fatalf("plist missing %q:\n%s", want, got)
				}
			}
			if tt.want != "" && !strings.Contains(got, tt.want) {
				t.Fatalf("plist missing policy mapping %q:\n%s", tt.want, got)
			}
			if tt.wantAbsent != "" && strings.Contains(got, tt.wantAbsent) {
				t.Fatalf("plist contains %q unexpectedly:\n%s", tt.wantAbsent, got)
			}
		})
	}
}

func TestResolveLaunchdPlistPathNormalizesRelativeConfigPath(t *testing.T) {
	t.Parallel()

	callerDir := t.TempDir()
	plistPath := filepath.Join("agents", "looperd.plist")
	loaded := config.LoadedFileConfig{}
	loaded.Config.Daemon.PlistPath = &plistPath
	runtime := newCommandRuntime(New(Deps{Getwd: func() (string, error) { return callerDir, nil }}), nil)

	resolved, err := runtime.resolveLaunchdPlistPath(loaded)
	if err != nil {
		t.Fatalf("resolveLaunchdPlistPath() error = %v", err)
	}
	if got, want := resolved, filepath.Join(callerDir, "agents", "looperd.plist"); got != want {
		t.Fatalf("resolveLaunchdPlistPath() = %q, want %q", got, want)
	}
}

func TestStopLaunchdDaemonUsesPersistedStatePlistPath(t *testing.T) {
	t.Parallel()

	homeDir := t.TempDir()
	currentPlistPath := filepath.Join(homeDir, "current.plist")
	persistedPlistPath := filepath.Join(homeDir, "persisted.plist")
	loaded := config.LoadedFileConfig{}
	loaded.Config.Daemon.PlistPath = &currentPlistPath
	state := &daemonLifecycleState{Mode: config.DaemonModeLaunchd, PID: 4321, Logs: daemonLifecycleLogState{Main: filepath.Join(homeDir, "looperd.log")}, Supervisor: &daemonSupervisorState{Source: "launchd", Label: launchdLooperdLabel, PlistPath: persistedPlistPath}}
	var bootoutArgs []string
	var removed []string
	runtime := newCommandRuntime(New(Deps{
		HomeDir:  homeDir,
		Platform: "darwin",
		LookPath: func(file string) (string, error) {
			if file == "launchctl" {
				return "/bin/launchctl", nil
			}
			return "", os.ErrNotExist
		},
		RunCommand: func(ctx context.Context, command string, args []string, timeout time.Duration) (commandExecutionResult, error) {
			_ = ctx
			_ = command
			_ = timeout
			bootoutArgs = append([]string{}, args...)
			return commandExecutionResult{ExitCode: 0}, nil
		},
		RemoveFile: func(path string) error {
			removed = append(removed, path)
			return nil
		},
	}), nil)

	stopped, err := runtime.stopLaunchdDaemon(context.Background(), &bytes.Buffer{}, loaded, state, false)
	if err != nil {
		t.Fatalf("stopLaunchdDaemon() error = %v", err)
	}
	if !stopped {
		t.Fatal("stopLaunchdDaemon() stopped = false, want true")
	}
	if len(bootoutArgs) != 3 || bootoutArgs[0] != "bootout" || bootoutArgs[2] != persistedPlistPath {
		t.Fatalf("launchctl args = %#v, want bootout of persisted plist %q", bootoutArgs, persistedPlistPath)
	}
	if len(removed) == 0 || removed[len(removed)-1] != persistedPlistPath {
		t.Fatalf("removed files = %#v, want persisted plist removed", removed)
	}
}

func TestMigrateLegacyLaunchdPlistBootsOutAndRemovesWhenLegacyPresent(t *testing.T) {
	t.Parallel()

	homeDir := t.TempDir()
	legacyDir := filepath.Join(homeDir, "Library", "LaunchAgents")
	if err := os.MkdirAll(legacyDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(legacyDir) error = %v", err)
	}
	legacyPath := filepath.Join(legacyDir, launchdLooperdLegacyLabel+".plist")
	if err := os.WriteFile(legacyPath, []byte("<plist/>\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(legacy plist) error = %v", err)
	}

	var bootoutArgs []string
	var removed []string
	runtime := newCommandRuntime(New(Deps{
		HomeDir:  homeDir,
		Platform: "darwin",
		LookPath: func(file string) (string, error) {
			if file == "launchctl" {
				return "/bin/launchctl", nil
			}
			return "", os.ErrNotExist
		},
		RunCommand: func(ctx context.Context, command string, args []string, timeout time.Duration) (commandExecutionResult, error) {
			_ = ctx
			_ = command
			_ = timeout
			bootoutArgs = append([]string{}, args...)
			return commandExecutionResult{ExitCode: 0}, nil
		},
		RemoveFile: func(path string) error {
			removed = append(removed, path)
			return nil
		},
	}), nil)

	runtime.migrateLegacyLaunchdPlist(context.Background(), "/bin/launchctl", "gui/1000")

	if len(bootoutArgs) != 3 || bootoutArgs[0] != "bootout" || bootoutArgs[1] != "gui/1000" || bootoutArgs[2] != legacyPath {
		t.Fatalf("launchctl args = %#v, want bootout of legacy plist %q in domain gui/1000", bootoutArgs, legacyPath)
	}
	if len(removed) != 1 || removed[0] != legacyPath {
		t.Fatalf("removed files = %#v, want exactly the legacy plist %q", removed, legacyPath)
	}
}

func TestMigrateLegacyLaunchdPlistNoopWhenLegacyAbsent(t *testing.T) {
	t.Parallel()

	homeDir := t.TempDir()
	// Intentionally do not create the legacy plist on disk.

	var runCommandCalls int
	var removeCalls int
	runtime := newCommandRuntime(New(Deps{
		HomeDir:  homeDir,
		Platform: "darwin",
		LookPath: func(file string) (string, error) {
			if file == "launchctl" {
				return "/bin/launchctl", nil
			}
			return "", os.ErrNotExist
		},
		RunCommand: func(ctx context.Context, command string, args []string, timeout time.Duration) (commandExecutionResult, error) {
			_ = ctx
			_ = command
			_ = args
			_ = timeout
			runCommandCalls++
			return commandExecutionResult{ExitCode: 0}, nil
		},
		RemoveFile: func(path string) error {
			_ = path
			removeCalls++
			return nil
		},
	}), nil)

	runtime.migrateLegacyLaunchdPlist(context.Background(), "/bin/launchctl", "gui/1000")

	if runCommandCalls != 0 {
		t.Fatalf("runCommand invocations = %d, want 0 (no legacy plist to migrate)", runCommandCalls)
	}
	if removeCalls != 0 {
		t.Fatalf("removeFile invocations = %d, want 0 (no legacy plist to migrate)", removeCalls)
	}
}

func TestRefreshLaunchdLifecycleStateClearsStalePIDWhenServiceAbsent(t *testing.T) {
	t.Parallel()

	homeDir := t.TempDir()
	statePath := filepath.Join(homeDir, ".looper", "looperd.state.json")
	pidPath := filepath.Join(homeDir, ".looper", "looperd.pid")
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(pidPath, []byte("4321\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(pid) error = %v", err)
	}
	runtime := newCommandRuntime(New(Deps{
		HomeDir:  homeDir,
		Platform: "darwin",
		LookPath: func(file string) (string, error) {
			if file == "launchctl" {
				return "/bin/launchctl", nil
			}
			return "", os.ErrNotExist
		},
		RunCommand: func(ctx context.Context, command string, args []string, timeout time.Duration) (commandExecutionResult, error) {
			_ = ctx
			_ = command
			_ = args
			_ = timeout
			return commandExecutionResult{Stdout: "{ service = inactive; }\n", ExitCode: 0}, nil
		},
	}), nil)
	state := daemonLifecycleState{Mode: config.DaemonModeLaunchd, PID: 4321, Logs: daemonLifecycleLogState{Main: filepath.Join(homeDir, "looperd.log")}, Supervisor: &daemonSupervisorState{Source: "launchd", Label: launchdLooperdLabel}}
	if err := runtime.writeDaemonLifecycleState(statePath, state); err != nil {
		t.Fatalf("writeDaemonLifecycleState() error = %v", err)
	}

	updated, err := runtime.refreshLaunchdLifecycleState(context.Background(), loadedLaunchdTestConfig(homeDir), statePath, &state)
	if err != nil {
		t.Fatalf("refreshLaunchdLifecycleState() error = %v", err)
	}
	if updated == nil || updated.PID != 0 || updated.LastExit == nil || !strings.Contains(updated.LastExit.Reason, "clearing stale pid 4321") {
		t.Fatalf("updated state = %#v, want stale pid cleared with last exit reason", updated)
	}
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Fatalf("pid file stat error = %v, want not exist", err)
	}
	persisted, err := runtime.readDaemonLifecycleState(statePath)
	if err != nil {
		t.Fatalf("readDaemonLifecycleState() error = %v", err)
	}
	if persisted.PID != 0 {
		t.Fatalf("persisted PID = %d, want 0", persisted.PID)
	}
}

func TestDaemonStartLaunchdUnsupportedPlatform(t *testing.T) {
	t.Parallel()

	configPath := writeDaemonCLIConfig(t, "http://daemon.test")
	homeDir := t.TempDir()
	managedPath := filepath.Join(homeDir, ".looper", "bin", "looperd")
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := New(Deps{Stdout: stdout, Stderr: stderr, HomeDir: homeDir, Platform: "linux", HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) { return nil, os.ErrNotExist })}, RunCommand: func(ctx context.Context, command string, args []string, timeout time.Duration) (commandExecutionResult, error) {
		_ = ctx
		_ = timeout
		if command == managedPath && strings.Join(args, " ") == "--version" {
			return commandExecutionResult{Stdout: "0.6.0\n", ExitCode: 0}, nil
		}
		return commandExecutionResult{ExitCode: 1, Stderr: "not found"}, nil
	}})
	exitCode := app.Run(context.Background(), []string{"daemon", "start", "--daemon-mode", "launchd", "--config", configPath})
	if exitCode == 0 {
		t.Fatalf("Run([daemon start --daemon-mode launchd]) exit code = 0, want error")
	}
	if !strings.Contains(stderr.String(), "only supported on macOS") {
		t.Fatalf("stderr = %q, want unsupported platform guidance", stderr.String())
	}
}

func TestDaemonStartReportsNonExecutableManagedDaemon(t *testing.T) {
	t.Parallel()

	homeDir := t.TempDir()
	managedPath := filepath.Join(homeDir, ".looper", "bin", "looperd")
	if err := os.MkdirAll(filepath.Dir(managedPath), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(managedPath, []byte("looperd"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	stderr := &bytes.Buffer{}
	app := New(Deps{
		Stderr:  stderr,
		HomeDir: homeDir,
		HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return nil, os.ErrNotExist
		})},
	})

	exitCode := app.Run(context.Background(), []string{"daemon", "start", "--config", writeDaemonCLIConfig(t, "http://daemon.test")})
	if exitCode == 0 {
		t.Fatalf("Run([daemon start]) exit code = 0, want error")
	}
	for _, want := range []string{"not executable", "chmod +x " + managedPath, "looper daemon install --force"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr = %q, want to contain %q", stderr.String(), want)
		}
	}
}

func TestDaemonStartNormalizesForwardedRelativeConfigPathArgs(t *testing.T) {
	homeDir := t.TempDir()
	callerDir := t.TempDir()
	daemonWorkingDir := filepath.Join(t.TempDir(), "daemon-working")
	if err := os.MkdirAll(daemonWorkingDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(daemonWorkingDir) error = %v", err)
	}
	for _, dir := range []string{"logs", "storage"} {
		if err := os.MkdirAll(filepath.Join(callerDir, dir), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s) error = %v", dir, err)
		}
	}
	for _, file := range []string{"git", "gh", "osascript"} {
		if err := os.WriteFile(filepath.Join(callerDir, file), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", file, err)
		}
	}

	previousWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}
	if err := os.Chdir(callerDir); err != nil {
		t.Fatalf("Chdir(callerDir) error = %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(previousWD); err != nil {
			t.Fatalf("restore working directory error = %v", err)
		}
	})

	configPath := filepath.Join(callerDir, "looper.json")
	payload := map[string]any{
		"server": map[string]any{
			"baseUrl":  "http://daemon.test",
			"authMode": "none",
		},
		"daemon": map[string]any{
			"logDir":           filepath.Join(callerDir, "logs"),
			"workingDirectory": daemonWorkingDir,
		},
		"storage": map[string]any{
			"dbPath": filepath.Join(callerDir, "storage", "looper.sqlite"),
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Marshal(config) error = %v", err)
	}
	if err := os.WriteFile(configPath, raw, 0o644); err != nil {
		t.Fatalf("WriteFile(config) error = %v", err)
	}

	managedPath := filepath.Join(homeDir, ".looper", "bin", "looperd")
	statusRequests := 0
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	var spawnedArgs []string
	var spawnedCWD string
	app := New(Deps{
		Stdout:  stdout,
		Stderr:  stderr,
		HomeDir: homeDir,
		HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			statusRequests++
			if statusRequests == 1 {
				return nil, os.ErrNotExist
			}
			return jsonResponse(t, http.StatusOK, `{"ok":true,"requestId":"req_status","data":{"service":{"binary":{"name":"looperd"}}}}`), nil
		})},
		Getwd: func() (string, error) {
			return callerDir, nil
		},
		ReadFile: func(path string) ([]byte, error) {
			return nil, os.ErrNotExist
		},
		RunCommand: daemonVersionCommand(managedPath),
		SpawnDetached: func(command string, args []string, cwd string, env []string) (int, error) {
			_ = command
			_ = env
			spawnedArgs = append([]string{}, args...)
			spawnedCWD = cwd
			return 4321, nil
		},
		KillProcess: func(pid int, signal int) error {
			return nil
		},
		MkdirAll: func(path string, perm os.FileMode) error {
			return nil
		},
		WriteFile: func(path string, data []byte, perm os.FileMode) error {
			return nil
		},
		Sleep: func(duration time.Duration) {},
	})

	exitCode := app.Run(context.Background(), []string{"daemon", "start", "--config", "./looper.json", "--db-path=./looper.sqlite", "--log-dir", "./logs", "--git-path", "./git", "--gh-path=./gh", "--osascript-path", "./osascript", "--host", "127.0.0.1"})
	if exitCode != 0 {
		t.Fatalf("Run([daemon start]) exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if spawnedCWD != daemonWorkingDir {
		t.Fatalf("spawned.cwd = %q, want %q", spawnedCWD, daemonWorkingDir)
	}
	want := []string{"--config", filepath.Join(callerDir, "looper.json"), "--db-path=" + filepath.Join(callerDir, "looper.sqlite"), "--log-dir", filepath.Join(callerDir, "logs"), "--git-path", filepath.Join(callerDir, "git"), "--gh-path=" + filepath.Join(callerDir, "gh"), "--osascript-path", filepath.Join(callerDir, "osascript"), "--host", "127.0.0.1"}
	if got := strings.Join(spawnedArgs, "\n"); got != strings.Join(want, "\n") {
		t.Fatalf("spawned.args = %#v, want %#v", spawnedArgs, want)
	}
}

func TestDaemonStartNormalizesRelativeConfigEnvForSpawn(t *testing.T) {
	homeDir := t.TempDir()
	callerDir := t.TempDir()
	daemonWorkingDir := filepath.Join(t.TempDir(), "daemon-working")
	if err := os.MkdirAll(daemonWorkingDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(daemonWorkingDir) error = %v", err)
	}
	logDir := filepath.Join(callerDir, "logs")
	storageDir := filepath.Join(callerDir, "storage")
	for _, dir := range []string{logDir, storageDir, filepath.Join(callerDir, "env-logs"), filepath.Join(callerDir, "env-working")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll(%s) error = %v", dir, err)
		}
	}
	for _, file := range []string{"env-git", "env-gh", "env-osascript"} {
		if err := os.WriteFile(filepath.Join(callerDir, file), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", file, err)
		}
	}

	previousWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}
	if err := os.Chdir(callerDir); err != nil {
		t.Fatalf("Chdir(callerDir) error = %v", err)
	}
	resolvedCallerDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd(callerDir) error = %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(previousWD); err != nil {
			t.Fatalf("restore working directory error = %v", err)
		}
	})
	t.Setenv("LOOPER_CONFIG", "./looper.json")
	t.Setenv("LOOPER_DB_PATH", "./env-looper.sqlite")
	t.Setenv("LOOPER_LOG_DIR", "./env-logs")
	t.Setenv("LOOPER_WORKING_DIRECTORY", "./env-working")
	t.Setenv("LOOPER_GIT_PATH", "./env-git")
	t.Setenv("LOOPER_GH_PATH", "./env-gh")
	t.Setenv("LOOPER_OSASCRIPT_PATH", "./env-osascript")

	configPath := filepath.Join(callerDir, "looper.json")
	payload := map[string]any{
		"server": map[string]any{
			"baseUrl":  "http://daemon.test",
			"authMode": "none",
		},
		"daemon": map[string]any{
			"logDir":           logDir,
			"workingDirectory": daemonWorkingDir,
		},
		"storage": map[string]any{
			"dbPath": filepath.Join(storageDir, "looper.sqlite"),
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Marshal(config) error = %v", err)
	}
	if err := os.WriteFile(configPath, raw, 0o644); err != nil {
		t.Fatalf("WriteFile(config) error = %v", err)
	}

	managedPath := filepath.Join(homeDir, ".looper", "bin", "looperd")
	statusRequests := 0
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	var spawnedCWD string
	var spawnedEnv []string
	app := New(Deps{
		Stdout:  stdout,
		Stderr:  stderr,
		HomeDir: homeDir,
		HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			statusRequests++
			if statusRequests == 1 {
				return nil, os.ErrNotExist
			}
			return jsonResponse(t, http.StatusOK, `{"ok":true,"requestId":"req_status","data":{"service":{"binary":{"name":"looperd"}}}}`), nil
		})},
		ReadFile: func(path string) ([]byte, error) {
			return nil, os.ErrNotExist
		},
		RunCommand: daemonVersionCommand(managedPath),
		SpawnDetached: func(command string, args []string, cwd string, env []string) (int, error) {
			_ = command
			_ = args
			spawnedCWD = cwd
			spawnedEnv = append([]string{}, env...)
			return 4321, nil
		},
		KillProcess: func(pid int, signal int) error { return nil },
		MkdirAll:    func(path string, perm os.FileMode) error { return nil },
		WriteFile:   func(path string, data []byte, perm os.FileMode) error { return nil },
		Sleep:       func(duration time.Duration) {},
	})

	exitCode := app.Run(context.Background(), []string{"daemon", "start"})
	if exitCode != 0 {
		t.Fatalf("Run([daemon start]) exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if spawnedCWD != filepath.Join(resolvedCallerDir, "env-working") {
		t.Fatalf("spawned.cwd = %q, want %q", spawnedCWD, filepath.Join(resolvedCallerDir, "env-working"))
	}
	if got, want := envValue(spawnedEnv, "LOOPER_CONFIG"), filepath.Join(resolvedCallerDir, "looper.json"); got != want {
		t.Fatalf("spawned LOOPER_CONFIG = %q, want %q; env=%#v", got, want, spawnedEnv)
	}
	wantEnv := map[string]string{
		"LOOPER_DB_PATH":           filepath.Join(resolvedCallerDir, "env-looper.sqlite"),
		"LOOPER_LOG_DIR":           filepath.Join(resolvedCallerDir, "env-logs"),
		"LOOPER_WORKING_DIRECTORY": filepath.Join(resolvedCallerDir, "env-working"),
		"LOOPER_GIT_PATH":          filepath.Join(resolvedCallerDir, "env-git"),
		"LOOPER_GH_PATH":           filepath.Join(resolvedCallerDir, "env-gh"),
		"LOOPER_OSASCRIPT_PATH":    filepath.Join(resolvedCallerDir, "env-osascript"),
	}
	for name, want := range wantEnv {
		if got := envValue(spawnedEnv, name); got != want {
			t.Fatalf("spawned %s = %q, want %q; env=%#v", name, got, want, spawnedEnv)
		}
	}
}

func envValue(env []string, name string) string {
	prefix := name + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			return strings.TrimPrefix(entry, prefix)
		}
	}
	return ""
}

func TestDaemonStartMissingPIDFileUsesReachableLooperdAPI(t *testing.T) {
	t.Parallel()

	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/api/v1/status" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		writeLooperdStatusEnvelope(t, w)
	}))
	defer server.Close()

	configPath := writeDaemonCLIConfigForBindEndpoint(t, server.URL, nil)
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := New(Deps{
		Stdout: stdout,
		Stderr: stderr,
		ReadFile: func(path string) ([]byte, error) {
			return nil, os.ErrNotExist
		},
		RunCommand: daemonVersionCommand(t.TempDir()),
		SpawnDetached: func(command string, args []string, cwd string, env []string) (int, error) {
			t.Fatal("SpawnDetached() called, want existing API reused")
			return 0, nil
		},
	})

	exitCode := app.Run(context.Background(), []string{"daemon", "start", "--config", configPath})
	if exitCode != 0 {
		t.Fatalf("Run([daemon start]) exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "pid file missing") || !strings.Contains(stdout.String(), server.URL) {
		t.Fatalf("stdout = %q, want already running message with URL and missing pid", stdout.String())
	}
	if requests == 0 {
		t.Fatal("status endpoint was not probed")
	}
}

func TestDaemonStartStalePIDFileUsesReachableLooperdAPI(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeLooperdStatusEnvelope(t, w)
	}))
	defer server.Close()

	homeDir := t.TempDir()
	pidFilePath := filepath.Join(homeDir, ".looper", "looperd.pid")
	configPath := writeDaemonCLIConfigForBindEndpoint(t, server.URL, nil)
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	removed := false
	app := New(Deps{
		Stdout:  stdout,
		Stderr:  stderr,
		HomeDir: homeDir,
		ReadFile: func(path string) ([]byte, error) {
			if path == pidFilePath {
				return []byte("9876\n"), nil
			}
			return nil, os.ErrNotExist
		},
		RunCommand: daemonVersionCommand(filepath.Join(homeDir, ".looper", "bin", "looperd")),
		KillProcess: func(pid int, signal int) error {
			if pid == 9876 && signal == 0 {
				return os.ErrProcessDone
			}
			return nil
		},
		RemoveFile: func(path string) error {
			if path == pidFilePath {
				removed = true
			}
			return nil
		},
		SpawnDetached: func(command string, args []string, cwd string, env []string) (int, error) {
			t.Fatal("SpawnDetached() called, want existing API reused")
			return 0, nil
		},
	})

	exitCode := app.Run(context.Background(), []string{"daemon", "start", "--config", configPath})
	if exitCode != 0 {
		t.Fatalf("Run([daemon start]) exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if !removed {
		t.Fatal("stale pid file was not removed")
	}
	if !strings.Contains(stdout.String(), "Removed stale daemon pid file") || !strings.Contains(stdout.String(), "pid file missing") {
		t.Fatalf("stdout = %q, want stale removal and API success messages", stdout.String())
	}
}

func TestDaemonStartAliveNonLooperdPIDFileFailsEvenWithReachableLooperdAPI(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeLooperdStatusEnvelope(t, w)
	}))
	defer server.Close()

	homeDir := t.TempDir()
	pidFilePath := filepath.Join(homeDir, ".looper", "looperd.pid")
	configPath := writeDaemonCLIConfigForBindEndpoint(t, server.URL, nil)
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := New(Deps{
		Stdout:  stdout,
		Stderr:  stderr,
		HomeDir: homeDir,
		ReadFile: func(path string) ([]byte, error) {
			if path == pidFilePath {
				return []byte("9876\n"), nil
			}
			return nil, os.ErrNotExist
		},
		RunCommand: func(ctx context.Context, command string, args []string, timeout time.Duration) (commandExecutionResult, error) {
			_ = ctx
			_ = timeout
			if command == "ps" && len(args) >= 2 && args[1] == "9876" {
				return commandExecutionResult{Stdout: "/usr/bin/sleep\n", ExitCode: 0}, nil
			}
			return commandExecutionResult{ExitCode: 1, Stderr: "not found"}, nil
		},
		KillProcess: func(pid int, signal int) error {
			if pid != 9876 || signal != 0 {
				t.Fatalf("KillProcess(%d, %d), want liveness probe for pid 9876", pid, signal)
			}
			return nil
		},
		RemoveFile: func(path string) error { return nil },
		SpawnDetached: func(command string, args []string, cwd string, env []string) (int, error) {
			t.Fatal("SpawnDetached() called, want existing API reused")
			return 0, nil
		},
	})

	exitCode := app.Run(context.Background(), []string{"daemon", "start", "--config", configPath})
	if exitCode == 0 {
		t.Fatal("Run([daemon start]) exit code = 0, want non-zero")
	}
	if !strings.Contains(stderr.String(), "alive non-looperd process 9876") {
		t.Fatalf("stderr = %q, want non-looperd pid refusal", stderr.String())
	}
}

func TestDaemonStartNonLooperdAPIServiceFailsBeforeSpawn(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeEnvelope(t, w, pkgapi.Success("req_status", map[string]any{
			"service": map[string]any{"binary": map[string]any{"name": "not-looperd"}},
		}))
	}))
	defer server.Close()

	configPath := writeDaemonCLIConfigForBindEndpoint(t, server.URL, nil)
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := New(Deps{
		Stdout: stdout,
		Stderr: stderr,
		ReadFile: func(path string) ([]byte, error) {
			return nil, os.ErrNotExist
		},
		RunCommand: daemonVersionCommand(t.TempDir()),
		SpawnDetached: func(command string, args []string, cwd string, env []string) (int, error) {
			t.Fatal("SpawnDetached() called, want port occupation failure")
			return 0, nil
		},
	})

	exitCode := app.Run(context.Background(), []string{"daemon", "start", "--config", configPath})
	if exitCode == 0 {
		t.Fatal("Run([daemon start]) exit code = 0, want non-zero")
	}
	if !strings.Contains(stderr.String(), "occupied by another service") {
		t.Fatalf("stderr = %q, want occupied-by-another-service diagnostic", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
}

func TestDaemonStartExitedProcessIncludesStartupLogTail(t *testing.T) {
	t.Parallel()

	configPath := writeDaemonCLIConfigForBindEndpoint(t, "http://127.0.0.1:1", nil)
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := New(Deps{
		Stdout: stdout,
		Stderr: stderr,
		ReadFile: func(path string) ([]byte, error) {
			if strings.Contains(path, filepath.Join("startup", "looperd-")) {
				return []byte("line one\nconfig validation failed: missing storage.dbPath\n"), nil
			}
			return nil, os.ErrNotExist
		},
		RunCommand: daemonVersionCommand(t.TempDir()),
		Getwd: func() (string, error) {
			return "/tmp/workspace", nil
		},
		SpawnDetached: func(command string, args []string, cwd string, env []string) (int, error) {
			return 4321, nil
		},
		KillProcess: func(pid int, signal int) error {
			if pid == 4321 && signal == 0 {
				return os.ErrProcessDone
			}
			return nil
		},
		MkdirAll: func(path string, perm os.FileMode) error {
			return nil
		},
		RemoveFile: func(path string) error {
			return nil
		},
	})

	exitCode := app.Run(context.Background(), []string{"daemon", "start", "--config", configPath})
	if exitCode == 0 {
		t.Fatal("Run([daemon start]) exit code = 0, want non-zero")
	}
	if !strings.Contains(stderr.String(), "startup log:") || !strings.Contains(stderr.String(), "config validation failed") {
		t.Fatalf("stderr = %q, want startup log path and tail", stderr.String())
	}
}

func TestDaemonStartReadinessLoopWaitsForStatus(t *testing.T) {
	t.Parallel()

	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			writeEnvelope(t, w, map[string]any{"ok": false, "requestId": "req_not_ready", "error": map[string]any{"message": "not ready"}})
			return
		}
		writeLooperdStatusEnvelope(t, w)
	}))
	defer server.Close()

	configPath := writeDaemonCLIConfigForBindEndpoint(t, server.URL, nil)
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	clientRequests := 0
	app := New(Deps{
		Stdout: stdout,
		Stderr: stderr,
		HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			clientRequests++
			if clientRequests == 1 {
				return nil, os.ErrNotExist
			}
			return http.DefaultTransport.RoundTrip(req)
		})},
		ReadFile: func(path string) ([]byte, error) {
			return nil, os.ErrNotExist
		},
		RunCommand: func(ctx context.Context, command string, args []string, timeout time.Duration) (commandExecutionResult, error) {
			_ = ctx
			_ = timeout
			if strings.Join(args, " ") == "--version" {
				return commandExecutionResult{Stdout: "0.7.0\n", ExitCode: 0}, nil
			}
			if command == "ps" && len(args) >= 2 && args[1] == "4321" {
				return commandExecutionResult{Stdout: filepath.Join(t.TempDir(), "looperd") + "\n", ExitCode: 0}, nil
			}
			return commandExecutionResult{ExitCode: 1}, nil
		},
		Getwd: func() (string, error) { return "/tmp/workspace", nil },
		SpawnDetached: func(command string, args []string, cwd string, env []string) (int, error) {
			return 4321, nil
		},
		KillProcess: func(pid int, signal int) error { return nil },
		MkdirAll:    func(path string, perm os.FileMode) error { return nil },
		WriteFile:   func(path string, data []byte, perm os.FileMode) error { return nil },
		Sleep:       func(duration time.Duration) {},
	})

	exitCode := app.Run(context.Background(), []string{"daemon", "start", "--config", configPath})
	if exitCode != 0 {
		t.Fatalf("Run([daemon start]) exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if requests < 3 {
		t.Fatalf("status requests = %d, want readiness loop to retry until ready", requests)
	}
}

func TestDaemonStartIgnoresRemoteBaseURLForLocalReadiness(t *testing.T) {
	t.Parallel()

	remoteRequests := 0
	remoteServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		remoteRequests++
		writeLooperdStatusEnvelope(t, w)
	}))
	defer remoteServer.Close()

	localRequests := 0
	localServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		localRequests++
		writeLooperdStatusEnvelope(t, w)
	}))
	defer localServer.Close()

	homeDir := t.TempDir()
	configPath := writeDaemonCLIConfigForBindEndpoint(t, localServer.URL, &remoteServer.URL)
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	spawned := false
	clientRequests := 0
	app := New(Deps{
		Stdout:  stdout,
		Stderr:  stderr,
		HomeDir: homeDir,
		HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			clientRequests++
			if clientRequests == 1 {
				return nil, os.ErrNotExist
			}
			return http.DefaultTransport.RoundTrip(req)
		})},
		ReadFile:   func(path string) ([]byte, error) { return nil, os.ErrNotExist },
		RunCommand: daemonVersionCommand(filepath.Join(homeDir, ".looper", "bin", "looperd")),
		Getwd:      func() (string, error) { return "/tmp/workspace", nil },
		SpawnDetached: func(command string, args []string, cwd string, env []string) (int, error) {
			spawned = true
			return 4321, nil
		},
		KillProcess: func(pid int, signal int) error { return nil },
		MkdirAll:    func(path string, perm os.FileMode) error { return nil },
		WriteFile:   func(path string, data []byte, perm os.FileMode) error { return nil },
		Sleep:       func(duration time.Duration) {},
	})

	exitCode := app.Run(context.Background(), []string{"daemon", "start", "--config", configPath})
	if exitCode != 0 {
		t.Fatalf("Run([daemon start]) exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if !spawned {
		t.Fatal("SpawnDetached() was not called; remote baseUrl appears to have short-circuited startup")
	}
	if remoteRequests != 0 {
		t.Fatalf("remote baseUrl requests = %d, want 0", remoteRequests)
	}
	if localRequests == 0 {
		t.Fatalf("local bind endpoint requests = %d, want readiness poll", localRequests)
	}
	if !strings.Contains(stdout.String(), "Started looperd") || !strings.Contains(stdout.String(), "pid 4321") {
		t.Fatalf("stdout = %q, want start confirmation", stdout.String())
	}
}

func TestDaemonStartReadinessTimeoutTerminatesSpawnedProcessAndRemovesPIDFile(t *testing.T) {
	t.Parallel()

	homeDir := t.TempDir()
	pidFilePath := filepath.Join(homeDir, ".looper", "looperd.pid")
	configPath := writeDaemonCLIConfigForBindEndpoint(t, "http://127.0.0.1:1", nil)
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	removeCalls := make([]string, 0, 1)
	killCalls := make([]struct {
		pid    int
		signal int
	}, 0, 8)
	app := New(Deps{
		Stdout:             stdout,
		Stderr:             stderr,
		HomeDir:            homeDir,
		DaemonStartTimeout: 200 * time.Millisecond,
		ReadFile: func(path string) ([]byte, error) {
			return nil, os.ErrNotExist
		},
		RunCommand: daemonVersionCommand(filepath.Join(homeDir, ".looper", "bin", "looperd")),
		Getwd: func() (string, error) {
			return "/tmp/workspace", nil
		},
		SpawnDetached: func(command string, args []string, cwd string, env []string) (int, error) {
			return 4321, nil
		},
		KillProcess: func(pid int, signal int) error {
			killCalls = append(killCalls, struct {
				pid    int
				signal int
			}{pid: pid, signal: signal})
			return nil
		},
		MkdirAll: func(path string, perm os.FileMode) error {
			return nil
		},
		RemoveFile: func(path string) error {
			removeCalls = append(removeCalls, path)
			return nil
		},
		WriteFile: func(path string, data []byte, perm os.FileMode) error {
			if !strings.HasSuffix(path, "looperd.state.json") {
				t.Fatalf("WriteFile(%q) called, want only lifecycle failure state write", path)
			}
			if !strings.Contains(string(data), "detached looperd exited or failed readiness during startup") {
				t.Fatalf("state write = %s, want startup failure diagnostics", string(data))
			}
			return nil
		},
		Sleep: func(duration time.Duration) {},
	})

	exitCode := app.Run(context.Background(), []string{"daemon", "start", "--config", configPath})
	if exitCode == 0 {
		t.Fatal("Run([daemon start]) exit code = 0, want non-zero")
	}
	if got := len(removeCalls); got != 1 || removeCalls[0] != pidFilePath {
		t.Fatalf("removeCalls = %#v, want [%q]", removeCalls, pidFilePath)
	}
	if got := len(killCalls); got < 3 {
		t.Fatalf("killCalls = %#v, want readiness liveness probes plus SIGTERM cleanup", killCalls)
	}
	lastKill := killCalls[len(killCalls)-1]
	if lastKill.pid != 4321 || lastKill.signal != int(syscall.SIGTERM) {
		t.Fatalf("last kill call = %#v, want pid 4321 SIGTERM", lastKill)
	}
	for i, call := range killCalls[:len(killCalls)-1] {
		if call.pid != 4321 || call.signal != 0 {
			t.Fatalf("killCalls[%d] = %#v, want pid 4321 signal 0 readiness probe", i, call)
		}
	}
	if !strings.Contains(stderr.String(), "Timed out waiting for looperd pid 4321 to become ready") {
		t.Fatalf("stderr = %q, want readiness timeout", stderr.String())
	}
	if !strings.Contains(stderr.String(), "last readiness error") {
		t.Fatalf("stderr = %q, want last readiness error details", stderr.String())
	}
}

func TestDaemonRestartStopsExistingPIDAndStartsReplacement(t *testing.T) {
	t.Parallel()

	homeDir := t.TempDir()
	configPath := writeCLIConfig(t, "http://127.0.0.1:4321", "")
	managedPath := filepath.Join(homeDir, ".looper", "bin", "looperd")
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	removeCalls := make([]string, 0)
	writeCalls := make([]string, 0)
	killCalls := make([]struct {
		pid    int
		signal int
	}, 0)
	spawnCalls := make([]string, 0)
	pidReads := 0
	alive1234 := true
	alive2233 := false

	app := New(Deps{
		Stdout:  stdout,
		Stderr:  stderr,
		HomeDir: homeDir,
		HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return jsonResponse(t, http.StatusOK, `{"ok":true,"requestId":"req_status","data":{"service":{"binary":{"name":"looperd"}}}}`), nil
		})},
		Getwd: func() (string, error) {
			return "/tmp/workspace", nil
		},
		ReadFile: func(path string) ([]byte, error) {
			if strings.HasSuffix(path, filepath.Join(".looper", "looperd.pid")) {
				pidReads += 1
				if pidReads == 1 {
					return []byte("1234\n"), nil
				}
			}
			return nil, os.ErrNotExist
		},
		RunCommand: func(ctx context.Context, command string, args []string, timeout time.Duration) (commandExecutionResult, error) {
			_ = ctx
			_ = timeout
			if command == managedPath && strings.Join(args, " ") == "--version" {
				return commandExecutionResult{Stdout: "0.5.0\n", ExitCode: 0}, nil
			}
			if command == "ps" && len(args) >= 2 && args[1] == "1234" {
				return commandExecutionResult{Stdout: managedPath + "\n", ExitCode: 0}, nil
			}
			if command == "ps" && len(args) >= 2 && args[1] == "2233" {
				return commandExecutionResult{Stdout: managedPath + "\n", ExitCode: 0}, nil
			}
			return commandExecutionResult{ExitCode: 1}, nil
		},
		KillProcess: func(pid int, signal int) error {
			killCalls = append(killCalls, struct {
				pid    int
				signal int
			}{pid: pid, signal: signal})
			if pid == 1234 && signal == 0 {
				if !alive1234 {
					return os.ErrProcessDone
				}
				return nil
			}
			if pid == 1234 && signal == 15 {
				alive1234 = false
				return nil
			}
			if pid == 2233 && signal == 0 {
				if !alive2233 {
					return os.ErrProcessDone
				}
				return nil
			}
			return nil
		},
		SpawnDetached: func(command string, args []string, cwd string, env []string) (int, error) {
			_ = args
			_ = cwd
			_ = env
			spawnCalls = append(spawnCalls, command)
			alive2233 = true
			return 2233, nil
		},
		MkdirAll: func(path string, perm os.FileMode) error {
			_ = path
			_ = perm
			return nil
		},
		WriteFile: func(path string, data []byte, perm os.FileMode) error {
			_ = data
			_ = perm
			writeCalls = append(writeCalls, path)
			return nil
		},
		RemoveFile: func(path string) error {
			removeCalls = append(removeCalls, path)
			return nil
		},
		Sleep: func(duration time.Duration) {
			_ = duration
		},
	})

	exitCode := app.Run(context.Background(), []string{"daemon", "restart", "--config", configPath})
	if exitCode != 0 {
		t.Fatalf("Run([daemon restart]) exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("Run([daemon restart]) stderr = %q, want empty string", stderr.String())
	}
	if len(spawnCalls) != 1 || spawnCalls[0] != managedPath {
		t.Fatalf("spawnCalls = %#v, want [%q]", spawnCalls, managedPath)
	}
	if len(removeCalls) == 0 || !strings.HasSuffix(removeCalls[0], filepath.Join(".looper", "looperd.pid")) {
		t.Fatalf("removeCalls = %#v, want pid file removal", removeCalls)
	}
	if len(writeCalls) == 0 || !strings.HasSuffix(writeCalls[0], filepath.Join(".looper", "looperd.pid")) {
		t.Fatalf("writeCalls = %#v, want pid file write", writeCalls)
	}
	if !strings.Contains(stdout.String(), "Stopped looperd pid 1234") {
		t.Fatalf("stdout = %q, want stop confirmation", stdout.String())
	}
	if len(killCalls) < 3 {
		t.Fatalf("killCalls = %#v, want restart probes and SIGTERM", killCalls)
	}
	if killCalls[1].pid != 1234 || killCalls[1].signal != 15 {
		t.Fatalf("killCalls = %#v, want SIGTERM for pid 1234", killCalls)
	}
}

func TestDaemonStopStopsExistingPIDAndRemovesPIDFile(t *testing.T) {
	t.Parallel()

	homeDir := t.TempDir()
	managedPath := filepath.Join(homeDir, ".looper", "bin", "looperd")
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	removeCalls := make([]string, 0)
	killCalls := make([]struct {
		pid    int
		signal int
	}, 0)
	alive1234 := true

	app := New(Deps{
		Stdout:  stdout,
		Stderr:  stderr,
		HomeDir: homeDir,
		ReadFile: func(path string) ([]byte, error) {
			if strings.HasSuffix(path, filepath.Join(".looper", "looperd.pid")) {
				return []byte("1234\n"), nil
			}
			return nil, os.ErrNotExist
		},
		RunCommand: func(ctx context.Context, command string, args []string, timeout time.Duration) (commandExecutionResult, error) {
			_ = ctx
			_ = timeout
			if command == "ps" && len(args) >= 2 && args[1] == "1234" {
				return commandExecutionResult{Stdout: managedPath + "\n", ExitCode: 0}, nil
			}
			return commandExecutionResult{ExitCode: 1}, nil
		},
		KillProcess: func(pid int, signal int) error {
			killCalls = append(killCalls, struct {
				pid    int
				signal int
			}{pid: pid, signal: signal})
			if pid == 1234 && signal == 0 {
				if !alive1234 {
					return os.ErrProcessDone
				}
				return nil
			}
			if pid == 1234 && signal == 15 {
				alive1234 = false
				return nil
			}
			return nil
		},
		RemoveFile: func(path string) error {
			removeCalls = append(removeCalls, path)
			return nil
		},
		Sleep: func(duration time.Duration) {
			_ = duration
		},
	})

	exitCode := app.Run(context.Background(), []string{"daemon", "stop"})
	if exitCode != 0 {
		t.Fatalf("Run([daemon stop]) exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("Run([daemon stop]) stderr = %q, want empty string", stderr.String())
	}
	if len(removeCalls) == 0 || !strings.HasSuffix(removeCalls[0], filepath.Join(".looper", "looperd.pid")) {
		t.Fatalf("removeCalls = %#v, want pid file removal", removeCalls)
	}
	if !strings.Contains(stdout.String(), "Stopped looperd pid 1234") {
		t.Fatalf("stdout = %q, want stop confirmation", stdout.String())
	}
	if len(killCalls) < 3 {
		t.Fatalf("killCalls = %#v, want liveness probe, SIGTERM, and exit probe", killCalls)
	}
	if killCalls[1].pid != 1234 || killCalls[1].signal != 15 {
		t.Fatalf("killCalls = %#v, want SIGTERM for pid 1234", killCalls)
	}
}

func TestDaemonStopWithoutPIDFileReportsNothingToStop(t *testing.T) {
	t.Parallel()

	configPath := writeCLIConfig(t, "http://127.0.0.1:4321", "")
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := New(Deps{
		Stdout:  stdout,
		Stderr:  stderr,
		HomeDir: t.TempDir(),
		ReadFile: func(path string) ([]byte, error) {
			return nil, os.ErrNotExist
		},
	})

	exitCode := app.Run(context.Background(), []string{"daemon", "stop", "--config", configPath})
	if exitCode != 0 {
		t.Fatalf("Run([daemon stop]) exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("Run([daemon stop]) stderr = %q, want empty string", stderr.String())
	}
	if got, want := stdout.String(), "No daemon pid file found; nothing to stop.\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}

func TestDaemonStartReturnsProcessInspectionFailureForExistingPID(t *testing.T) {
	t.Parallel()

	homeDir := t.TempDir()
	pidFilePath := filepath.Join(homeDir, ".looper", "looperd.pid")
	managedPath := filepath.Join(homeDir, ".looper", "bin", "looperd")
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	spawned := false
	removed := false

	app := New(Deps{
		Stdout:  stdout,
		Stderr:  stderr,
		HomeDir: homeDir,
		ReadFile: func(path string) ([]byte, error) {
			if path == pidFilePath {
				return []byte("1234\n"), nil
			}
			return nil, os.ErrNotExist
		},
		RunCommand: func(ctx context.Context, command string, args []string, timeout time.Duration) (commandExecutionResult, error) {
			_ = ctx
			_ = timeout
			if command == managedPath && strings.Join(args, " ") == "--version" {
				return commandExecutionResult{Stdout: "0.5.0\n", ExitCode: 0}, nil
			}
			if command == "ps" && len(args) >= 2 && args[1] == "1234" {
				return commandExecutionResult{}, context.DeadlineExceeded
			}
			return commandExecutionResult{ExitCode: 1}, nil
		},
		KillProcess: func(pid int, signal int) error {
			if pid == 1234 && signal == 0 {
				return nil
			}
			return nil
		},
		SpawnDetached: func(command string, args []string, cwd string, env []string) (int, error) {
			_ = command
			_ = args
			_ = cwd
			_ = env
			spawned = true
			return 4321, nil
		},
		RemoveFile: func(path string) error {
			if path == pidFilePath {
				removed = true
			}
			return nil
		},
	})

	exitCode := app.Run(context.Background(), []string{"daemon", "start"})
	if exitCode == 0 {
		t.Fatalf("Run([daemon start]) exit code = %d, want non-zero", exitCode)
	}
	if !strings.Contains(stderr.String(), "inspect process 1234 with ps") {
		t.Fatalf("stderr = %q, want process inspection failure", stderr.String())
	}
	if spawned {
		t.Fatal("SpawnDetached() called, want existing daemon start aborted")
	}
	if removed {
		t.Fatal("RemoveFile() called, want pid file preserved on ps failure")
	}
}

func TestIsLooperdProcessAcceptsQuotedExecutablePathWithSpaces(t *testing.T) {
	t.Parallel()

	runtime := &commandRuntime{app: &App{deps: Deps{RunCommand: func(ctx context.Context, command string, args []string, timeout time.Duration) (commandExecutionResult, error) {
		_ = ctx
		_ = timeout
		if command != "ps" || len(args) < 2 || args[1] != "1234" {
			return commandExecutionResult{ExitCode: 1}, nil
		}
		return commandExecutionResult{Stdout: `"/Applications/Looper Tools/looperd" --config "/tmp/looper config.json"` + "\n", ExitCode: 0}, nil
	}}}}

	isLooperd, err := runtime.isLooperdProcess(context.Background(), 1234)
	if err != nil {
		t.Fatalf("isLooperdProcess() error = %v", err)
	}
	if !isLooperd {
		t.Fatal("isLooperdProcess() = false, want true")
	}
}

func TestDaemonLogsJSONReturnsTail(t *testing.T) {
	t.Parallel()

	configPath := writeDaemonCLIConfig(t, "http://127.0.0.1:1")
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := New(Deps{
		Stdout: stdout,
		Stderr: stderr,
		ReadFile: func(path string) ([]byte, error) {
			if strings.HasSuffix(path, filepath.Join("logs", "looperd.log")) {
				return []byte("one\ntwo\nthree\n"), nil
			}
			return nil, os.ErrNotExist
		},
	})

	exitCode := app.Run(context.Background(), []string{"daemon", "logs", "--lines", "2", "--json", "--config", configPath})
	if exitCode != 0 {
		t.Fatalf("Run([daemon logs --lines 2 --json]) exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("Run([daemon logs --lines 2 --json]) stderr = %q, want empty string", stderr.String())
	}
	assertJSONContains(t, stdout.String(), "lines", []any{"two", "three"})
}

func TestDaemonLogsFullJSONIncludesRetainedHistoryBeforeActiveLog(t *testing.T) {
	t.Parallel()

	configPath := writeDaemonCLIConfig(t, "http://127.0.0.1:1")
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := New(Deps{
		Stdout: stdout,
		Stderr: stderr,
		ReadFile: func(path string) ([]byte, error) {
			switch {
			case strings.HasSuffix(path, filepath.Join("logs", "looperd.log.4")):
				return []byte("oldest"), nil
			case strings.HasSuffix(path, filepath.Join("logs", "looperd.log.3")):
				return []byte("older"), nil
			case strings.HasSuffix(path, filepath.Join("logs", "looperd.log.2")):
				return []byte("old"), nil
			case strings.HasSuffix(path, filepath.Join("logs", "looperd.log.1")):
				return []byte("recent-rotated"), nil
			case strings.HasSuffix(path, filepath.Join("logs", "looperd.log")):
				return []byte("current-1\ncurrent-2\n"), nil
			default:
				return nil, os.ErrNotExist
			}
		},
	})

	exitCode := app.Run(context.Background(), []string{"daemon", "logs", "--full", "--json", "--config", configPath})
	if exitCode != 0 {
		t.Fatalf("Run([daemon logs --full --json]) exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("Run([daemon logs --full --json]) stderr = %q, want empty string", stderr.String())
	}
	assertJSONContains(t, stdout.String(), "full", true)
	assertJSONContains(t, stdout.String(), "lines", []any{"oldest", "older", "old", "recent-rotated", "current-1", "current-2"})
}

func TestDaemonLogsRejectsFullWithLines(t *testing.T) {
	t.Parallel()

	configPath := writeDaemonCLIConfig(t, "http://127.0.0.1:1")
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := New(Deps{
		Stdout: stdout,
		Stderr: stderr,
	})

	exitCode := app.Run(context.Background(), []string{"daemon", "logs", "--full", "--lines", "2", "--config", configPath})
	if exitCode != 1 {
		t.Fatalf("Run([daemon logs --full --lines 2]) exit code = %d, want 1", exitCode)
	}
	if stdout.Len() != 0 {
		t.Fatalf("Run([daemon logs --full --lines 2]) stdout = %q, want empty string", stdout.String())
	}
	if !strings.Contains(stderr.String(), "--full cannot be combined with --lines") {
		t.Fatalf("stderr = %q, want full/lines conflict", stderr.String())
	}
}

func writeDaemonCLIConfig(t *testing.T, baseURL string) string {
	t.Helper()

	root := t.TempDir()
	logDir := filepath.Join(root, "logs")
	workingDir := filepath.Join(root, "working")
	storageDir := filepath.Join(root, "storage")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(logDir) error = %v", err)
	}
	if err := os.MkdirAll(workingDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(workingDir) error = %v", err)
	}
	if err := os.MkdirAll(storageDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(storageDir) error = %v", err)
	}

	configPath := filepath.Join(root, "config.json")
	payload := map[string]any{
		"server": map[string]any{
			"baseUrl":  baseURL,
			"authMode": "none",
		},
		"daemon": map[string]any{
			"logDir":           logDir,
			"workingDirectory": workingDir,
		},
		"storage": map[string]any{
			"dbPath": filepath.Join(storageDir, "looper.sqlite"),
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Marshal(config) error = %v", err)
	}
	if err := os.WriteFile(configPath, raw, 0o644); err != nil {
		t.Fatalf("WriteFile(config) error = %v", err)
	}
	return configPath
}

func mustLoadConfig(t *testing.T, path string) config.LoadedFileConfig {
	t.Helper()
	loaded, err := config.LoadFile(config.LoadFileOptions{Args: []string{"--config", path}})
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}
	return loaded
}

func writeDaemonCLIConfigForBindEndpoint(t *testing.T, bindURL string, baseURL *string) string {
	t.Helper()

	parsed, err := url.Parse(bindURL)
	if err != nil {
		t.Fatalf("Parse(bindURL) error = %v", err)
	}
	host, portText, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		t.Fatalf("SplitHostPort(%q) error = %v", parsed.Host, err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("Atoi(%q) error = %v", portText, err)
	}

	root := t.TempDir()
	logDir := filepath.Join(root, "logs")
	workingDir := filepath.Join(root, "working")
	storageDir := filepath.Join(root, "storage")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(logDir) error = %v", err)
	}
	if err := os.MkdirAll(workingDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(workingDir) error = %v", err)
	}
	if err := os.MkdirAll(storageDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(storageDir) error = %v", err)
	}

	serverConfig := map[string]any{
		"host":     host,
		"port":     port,
		"authMode": "none",
	}
	if baseURL != nil {
		serverConfig["baseUrl"] = *baseURL
	}
	payload := map[string]any{
		"server": serverConfig,
		"daemon": map[string]any{
			"logDir":           logDir,
			"workingDirectory": workingDir,
		},
		"storage": map[string]any{
			"dbPath": filepath.Join(storageDir, "looper.sqlite"),
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Marshal(config) error = %v", err)
	}
	configPath := filepath.Join(root, "config.json")
	if err := os.WriteFile(configPath, raw, 0o644); err != nil {
		t.Fatalf("WriteFile(config) error = %v", err)
	}
	return configPath
}

func writeLooperdStatusEnvelope(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	writeEnvelope(t, w, pkgapi.Success("req_status", map[string]any{
		"service": map[string]any{
			"version": "0.7.0",
			"binary": map[string]any{
				"name": "looperd",
			},
		},
	}))
}

func loadedLaunchdTestConfig(root string) config.LoadedFileConfig {
	loaded := config.LoadedFileConfig{}
	loaded.Config.Daemon.LogDir = filepath.Join(root, "logs")
	return loaded
}

func daemonVersionCommand(managedPath string) runCommandFunc {
	return func(ctx context.Context, command string, args []string, timeout time.Duration) (commandExecutionResult, error) {
		_ = ctx
		_ = timeout
		_ = managedPath
		if strings.Join(args, " ") == "--version" {
			return commandExecutionResult{Stdout: "0.7.0\n", ExitCode: 0}, nil
		}
		return commandExecutionResult{ExitCode: 1, Stderr: "not found"}, nil
	}
}
