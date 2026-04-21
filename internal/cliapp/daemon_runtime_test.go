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

	pkgapi "github.com/powerformer/looper/pkg/api"
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

func TestDaemonStartWritesPIDFileAndPassesConfigArgs(t *testing.T) {
	t.Parallel()

	homeDir := t.TempDir()
	managedPath := filepath.Join(homeDir, ".looper", "bin", "looperd")
	configPath := filepath.Join(t.TempDir(), "config.json")
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	spawned := struct {
		command string
		args    []string
		cwd     string
	}{}
	var wrotePath string
	var wroteBody string
	var mkdirPath string
	killCalls := make([]struct {
		pid    int
		signal int
	}, 0)

	app := New(Deps{
		Stdout:  stdout,
		Stderr:  stderr,
		HomeDir: homeDir,
		Getwd: func() (string, error) {
			return "/tmp/workspace", nil
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
			wrotePath = path
			wroteBody = string(data)
			return nil
		},
		Sleep: func(duration time.Duration) {
			_ = duration
		},
	})

	exitCode := app.Run(context.Background(), []string{"daemon", "start", "--config", configPath, "--port", "9999", "--db-path", "/tmp/looper.sqlite"})
	if exitCode != 0 {
		t.Fatalf("Run([daemon start]) exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("Run([daemon start]) stderr = %q, want empty string", stderr.String())
	}
	if spawned.command != managedPath {
		t.Fatalf("spawned.command = %q, want %q", spawned.command, managedPath)
	}
	if got, want := strings.Join(spawned.args, "\n"), strings.Join([]string{"--config", configPath, "--port", "9999", "--db-path", "/tmp/looper.sqlite"}, "\n"); got != want {
		t.Fatalf("spawned.args = %#v, want %#v", spawned.args, []string{"--config", configPath, "--port", "9999", "--db-path", "/tmp/looper.sqlite"})
	}
	if spawned.cwd != "/tmp/workspace" {
		t.Fatalf("spawned.cwd = %q, want %q", spawned.cwd, "/tmp/workspace")
	}
	if got, want := mkdirPath, filepath.Join(homeDir, ".looper"); got != want {
		t.Fatalf("mkdirPath = %q, want %q", got, want)
	}
	if got, want := wrotePath, filepath.Join(homeDir, ".looper", "looperd.pid"); got != want {
		t.Fatalf("wrotePath = %q, want %q", got, want)
	}
	if wroteBody != "4321\n" {
		t.Fatalf("wroteBody = %q, want %q", wroteBody, "4321\n")
	}
	if len(killCalls) != 1 || killCalls[0].pid != 4321 || killCalls[0].signal != 0 {
		t.Fatalf("killCalls = %#v, want only signal 0 probe for pid 4321", killCalls)
	}
	if !strings.Contains(stdout.String(), "Started looperd") {
		t.Fatalf("stdout = %q, want start confirmation", stdout.String())
	}
}

func TestDaemonRestartStopsExistingPIDAndStartsReplacement(t *testing.T) {
	t.Parallel()

	homeDir := t.TempDir()
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

	exitCode := app.Run(context.Background(), []string{"daemon", "restart"})
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
