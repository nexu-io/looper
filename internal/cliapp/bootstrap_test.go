package cliapp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestBootstrapYesInstallsStartsAndPrintsNextSteps(t *testing.T) {
	t.Parallel()

	homeDir := t.TempDir()
	cwd := t.TempDir()
	projectPath := filepath.Join(cwd, "repo")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatalf("MkdirAll(projectPath) error = %v", err)
	}
	configPath := filepath.Join(t.TempDir(), "bootstrap.json")
	managedPath := filepath.Join(homeDir, ".looper", "bin", "looperd")

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	binary := []byte("looperd-binary")
	checksum := sha256.Sum256(binary)
	checksumText := hex.EncodeToString(checksum[:]) + "  looperd-darwin-arm64\n"

	var daemonStarted atomic.Bool
	var spawnCalls atomic.Int32
	var statusCalls atomic.Int32

	app := New(Deps{
		Stdout:   stdout,
		Stderr:   stderr,
		HomeDir:  homeDir,
		Platform: "darwin",
		Arch:     "arm64",
		Getwd: func() (string, error) {
			return cwd, nil
		},
		LookPath: func(file string) (string, error) {
			switch file {
			case "git", "gh", "osascript":
				return "/usr/bin/" + file, nil
			default:
				return "", fmt.Errorf("not found")
			}
		},
		HTTPClient: newTestHTTPClient(func(req *http.Request) (*http.Response, error) {
			switch req.URL.String() {
			case "https://api.github.com/repos/powerformer/looper/releases/latest":
				return jsonResponse(t, http.StatusOK, `{"tag_name":"v1.2.3","assets":[{"name":"looperd-darwin-arm64","browser_download_url":"https://example.invalid/looperd-darwin-arm64"},{"name":"looperd-darwin-arm64.sha256","browser_download_url":"https://example.invalid/looperd-darwin-arm64.sha256"}]}`), nil
			case "https://example.invalid/looperd-darwin-arm64":
				return binaryResponse(t, http.StatusOK, binary), nil
			case "https://example.invalid/looperd-darwin-arm64.sha256":
				return textResponse(t, http.StatusOK, checksumText), nil
			}

			switch req.URL.Path {
			case "/api/v1/status", "/api/v1/healthz":
				statusCalls.Add(1)
				if !daemonStarted.Load() {
					return nil, fmt.Errorf("daemon offline")
				}
				return jsonResponse(t, http.StatusOK, `{"ok":true,"requestId":"req_status","data":{"service":{"healthy":true,"binary":{"name":"looperd"}}}}`), nil
			default:
				t.Fatalf("unexpected request URL %q", req.URL.String())
				return nil, nil
			}
		}),
		RunCommand: func(ctx context.Context, command string, args []string, timeout time.Duration) (commandExecutionResult, error) {
			_ = ctx
			_ = timeout
			if command == "/usr/bin/gh" && strings.Join(args, " ") == "auth status" {
				return commandExecutionResult{ExitCode: 0}, nil
			}
			if command == managedPath && strings.Join(args, " ") == "--version" {
				if _, err := os.Stat(managedPath); err != nil {
					return commandExecutionResult{ExitCode: 1, Stderr: "not found"}, nil
				}
				return commandExecutionResult{ExitCode: 0, Stdout: "1.2.3\n"}, nil
			}
			if command == "ps" && len(args) == 4 && args[0] == "-p" && args[2] == "-o" && args[3] == "command=" {
				return commandExecutionResult{ExitCode: 0, Stdout: managedPath + "\n"}, nil
			}
			return commandExecutionResult{ExitCode: 1, Stderr: "not found"}, nil
		},
		SpawnDetached: func(command string, args []string, cwd string, env []string) (int, error) {
			_ = args
			_ = cwd
			_ = env
			if command != managedPath {
				return 0, fmt.Errorf("unexpected command %q", command)
			}
			spawnCalls.Add(1)
			daemonStarted.Store(true)
			return 4321, nil
		},
		KillProcess: func(pid int, signal int) error {
			if pid == 4321 && signal == 0 {
				return nil
			}
			return fmt.Errorf("unexpected kill(%d, %d)", pid, signal)
		},
		Sleep: func(duration time.Duration) {
			_ = duration
		},
	})

	exitCode := app.Run(context.Background(), []string{"bootstrap", "--yes", "--project-path", projectPath, "--config", configPath})
	if exitCode != 0 {
		t.Fatalf("Run([bootstrap --yes]) exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if !strings.Contains(stderr.String(), "Downloading looperd-darwin-arm64: 14 B / 14 B (100%)") {
		t.Fatalf("Run([bootstrap --yes]) stderr = %q, want daemon download progress", stderr.String())
	}
	if spawnCalls.Load() != 1 {
		t.Fatalf("spawnDetached calls = %d, want 1", spawnCalls.Load())
	}
	if statusCalls.Load() < 2 {
		t.Fatalf("status checks = %d, want at least 2", statusCalls.Load())
	}

	for _, rel := range []string{".looper/bin", ".looper/backups", ".looper/logs"} {
		path := filepath.Join(homeDir, rel)
		if info, err := os.Stat(path); err != nil || !info.IsDir() {
			t.Fatalf("expected runtime directory %q to exist, err=%v", path, err)
		}
	}

	rawConfig, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile(configPath) error = %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(rawConfig, &decoded); err != nil {
		t.Fatalf("Unmarshal(config) error = %v", err)
	}
	projects, ok := decoded["projects"].([]any)
	if !ok || len(projects) == 0 {
		t.Fatalf("config projects = %#v, want non-empty array", decoded["projects"])
	}

	out := stdout.String()
	for _, want := range []string{"Bootstrap complete", "configCreated", "yes", "projectAdded", "daemonInstallState", "installed", "apiReachable", "yes", "Next steps:", "- looper status"} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout = %q, want to contain %q", out, want)
		}
	}
	if strings.Contains(out, "looper project add /path/to/repo") {
		t.Fatalf("stdout = %q, did not expect manual project-add step", out)
	}
}

func TestBootstrapJSONSuppressesDaemonStartOutput(t *testing.T) {
	t.Parallel()

	homeDir := t.TempDir()
	cwd := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "bootstrap.json")
	managedPath := filepath.Join(homeDir, ".looper", "bin", "looperd")
	binary := []byte("looperd-binary")
	checksum := sha256.Sum256(binary)
	checksumText := hex.EncodeToString(checksum[:]) + "  looperd-darwin-arm64\n"

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	var daemonStarted atomic.Bool

	app := New(Deps{
		Stdout:   stdout,
		Stderr:   stderr,
		HomeDir:  homeDir,
		Platform: "darwin",
		Arch:     "arm64",
		Getwd: func() (string, error) {
			return cwd, nil
		},
		LookPath: func(file string) (string, error) {
			switch file {
			case "git", "gh", "osascript":
				return "/usr/bin/" + file, nil
			default:
				return "", fmt.Errorf("not found")
			}
		},
		HTTPClient: newTestHTTPClient(func(req *http.Request) (*http.Response, error) {
			switch req.URL.String() {
			case "https://api.github.com/repos/powerformer/looper/releases/latest":
				return jsonResponse(t, http.StatusOK, `{"tag_name":"v1.2.3","assets":[{"name":"looperd-darwin-arm64","browser_download_url":"https://example.invalid/looperd-darwin-arm64"},{"name":"looperd-darwin-arm64.sha256","browser_download_url":"https://example.invalid/looperd-darwin-arm64.sha256"}]}`), nil
			case "https://example.invalid/looperd-darwin-arm64":
				return binaryResponse(t, http.StatusOK, binary), nil
			case "https://example.invalid/looperd-darwin-arm64.sha256":
				return textResponse(t, http.StatusOK, checksumText), nil
			}
			switch req.URL.Path {
			case "/api/v1/status", "/api/v1/healthz":
				if !daemonStarted.Load() {
					return nil, fmt.Errorf("daemon offline")
				}
				return jsonResponse(t, http.StatusOK, `{"ok":true,"requestId":"req_status","data":{"service":{"healthy":true,"binary":{"name":"looperd"}}}}`), nil
			default:
				t.Fatalf("unexpected request URL %q", req.URL.String())
				return nil, nil
			}
		}),
		RunCommand: func(ctx context.Context, command string, args []string, timeout time.Duration) (commandExecutionResult, error) {
			_ = ctx
			_ = timeout
			if command == "/usr/bin/gh" && strings.Join(args, " ") == "auth status" {
				return commandExecutionResult{ExitCode: 0}, nil
			}
			if command == managedPath && strings.Join(args, " ") == "--version" {
				if _, err := os.Stat(managedPath); err != nil {
					return commandExecutionResult{ExitCode: 1, Stderr: "not found"}, nil
				}
				return commandExecutionResult{ExitCode: 0, Stdout: "1.2.3\n"}, nil
			}
			if command == "ps" && len(args) == 4 && args[0] == "-p" && args[2] == "-o" && args[3] == "command=" {
				return commandExecutionResult{ExitCode: 0, Stdout: managedPath + "\n"}, nil
			}
			return commandExecutionResult{ExitCode: 1, Stderr: "not found"}, nil
		},
		SpawnDetached: func(command string, args []string, cwd string, env []string) (int, error) {
			_ = args
			_ = cwd
			_ = env
			if command != managedPath {
				return 0, fmt.Errorf("unexpected command %q", command)
			}
			daemonStarted.Store(true)
			return 4321, nil
		},
		KillProcess: func(pid int, signal int) error {
			if pid == 4321 && signal == 0 {
				return nil
			}
			return fmt.Errorf("unexpected kill(%d, %d)", pid, signal)
		},
		Sleep: func(duration time.Duration) {
			_ = duration
		},
	})

	exitCode := app.Run(context.Background(), []string{"bootstrap", "--yes", "--json", "--config", configPath})
	if exitCode != 0 {
		t.Fatalf("Run([bootstrap --yes --json]) exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if !strings.Contains(stderr.String(), "Downloading looperd-darwin-arm64: 14 B / 14 B (100%)") {
		t.Fatalf("Run([bootstrap --yes --json]) stderr = %q, want daemon download progress", stderr.String())
	}
	var decoded bootstrapResult
	if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
		t.Fatalf("stdout is not valid bootstrap JSON: %v\nraw=%q", err, stdout.String())
	}
	if !decoded.DaemonRunning || !decoded.APIReachable {
		t.Fatalf("bootstrap JSON daemon state = running:%v reachable:%v, want true/true", decoded.DaemonRunning, decoded.APIReachable)
	}
	if strings.Contains(stdout.String(), "Started looperd") || strings.Contains(stdout.String(), "PID file:") {
		t.Fatalf("stdout contains daemon start chatter: %q", stdout.String())
	}
}

func TestBootstrapPreflightHonorsConfigAndEnvToolOverrides(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "bootstrap.json")
	if err := os.WriteFile(configPath, []byte(`{"tools":{"gitPath":"/config/git","ghPath":"/config/gh"}}`), 0o644); err != nil {
		t.Fatalf("WriteFile(configPath) error = %v", err)
	}
	t.Setenv("LOOPER_GH_PATH", "/env/gh")

	app := New(Deps{
		Platform: "darwin",
		Arch:     "arm64",
		LookPath: func(file string) (string, error) {
			return "", fmt.Errorf("%s not on PATH", file)
		},
		RunCommand: func(ctx context.Context, command string, args []string, timeout time.Duration) (commandExecutionResult, error) {
			_ = ctx
			_ = timeout
			if command != "/env/gh" || strings.Join(args, " ") != "auth status" {
				t.Fatalf("RunCommand(%q, %#v), want env gh auth status", command, args)
			}
			return commandExecutionResult{ExitCode: 0}, nil
		},
	})
	runtime := newCommandRuntime(app, []string{"bootstrap", "--yes"})
	plan := bootstrapConfigPlan{}

	if _, err := runtime.bootstrapPreflight(context.Background(), configPath, &plan); err != nil {
		t.Fatalf("bootstrapPreflight() error = %v", err)
	}
}

func TestWaitForBootstrapHealthPropagatesCancellation(t *testing.T) {
	t.Parallel()

	app := New(Deps{
		HTTPClient: newTestHTTPClient(func(req *http.Request) (*http.Response, error) {
			_ = req
			return nil, context.Canceled
		}),
		Sleep: func(duration time.Duration) {
			t.Fatalf("Sleep(%s) called after bootstrap probe cancellation", duration)
		},
	})
	runtime := newCommandRuntime(app, nil)
	client := NewDaemonAPIClient(DaemonAPIClientOptions{BaseURL: "http://127.0.0.1", HTTPClient: app.deps.HTTPClient})

	reachable, err := runtime.waitForBootstrapHealth(context.Background(), client)
	if reachable {
		t.Fatal("waitForBootstrapHealth(...) reachable = true, want false")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("waitForBootstrapHealth(...) error = %v, want context.Canceled", err)
	}
}

func TestWaitForBootstrapHealthPropagatesDaemonAPIProbeError(t *testing.T) {
	t.Parallel()

	app := New(Deps{
		HTTPClient: newTestHTTPClient(func(req *http.Request) (*http.Response, error) {
			switch req.URL.Path {
			case "/api/v1/status":
				return nil, fmt.Errorf("connection refused")
			case "/api/v1/healthz":
				return jsonResponse(t, http.StatusUnauthorized, `{"ok":false,"requestId":"req_401","error":{"code":"UNAUTHORIZED","message":"unauthorized"}}`), nil
			default:
				t.Fatalf("unexpected request path %q", req.URL.Path)
				return nil, nil
			}
		}),
		Sleep: func(duration time.Duration) {
			t.Fatalf("Sleep(%s) called after daemon API probe error", duration)
		},
	})
	runtime := newCommandRuntime(app, nil)
	client := NewDaemonAPIClient(DaemonAPIClientOptions{BaseURL: "http://127.0.0.1", HTTPClient: app.deps.HTTPClient})

	reachable, err := runtime.waitForBootstrapHealth(context.Background(), client)
	if reachable {
		t.Fatal("waitForBootstrapHealth(...) reachable = true, want false")
	}
	var apiErr *DaemonAPIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("waitForBootstrapHealth(...) error type = %T, want *DaemonAPIError", err)
	}
	if got, want := apiErr.Status, http.StatusUnauthorized; got != want {
		t.Fatalf("status = %d, want %d", got, want)
	}
	if got, want := apiErr.Message, "unauthorized"; got != want {
		t.Fatalf("message = %q, want %q", got, want)
	}
}

func TestBootstrapPreflightFailsWhenRequiredToolsMissing(t *testing.T) {
	t.Parallel()

	homeDir := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "bootstrap.json")
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}

	app := New(Deps{
		Stdout:   stdout,
		Stderr:   stderr,
		HomeDir:  homeDir,
		Platform: "darwin",
		Arch:     "arm64",
		Getwd: func() (string, error) {
			return t.TempDir(), nil
		},
		LookPath: func(file string) (string, error) {
			if file == "osascript" {
				return "/usr/bin/osascript", nil
			}
			return "", fmt.Errorf("not found")
		},
		HTTPClient: newTestHTTPClient(func(req *http.Request) (*http.Response, error) {
			t.Fatalf("unexpected network request %q", req.URL.String())
			return nil, nil
		}),
	})

	exitCode := app.Run(context.Background(), []string{"bootstrap", "--yes", "--config", configPath})
	if exitCode != 1 {
		t.Fatalf("Run([bootstrap --yes]) exit code = %d, want 1", exitCode)
	}
	if stdout.Len() != 0 {
		t.Fatalf("Run([bootstrap --yes]) stdout = %q, want empty string", stdout.String())
	}
	for _, want := range []string{"bootstrap preflight failed", "missing required tools: git, gh", "Install them manually", "brew install git gh"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr = %q, want to contain %q", stderr.String(), want)
		}
	}
	if _, err := os.Stat(configPath); !os.IsNotExist(err) {
		t.Fatalf("config path exists unexpectedly after preflight failure: %v", err)
	}
}

func TestBootstrapIdempotentWhenConfigProjectAndDaemonAlreadyHealthy(t *testing.T) {
	t.Parallel()

	homeDir := t.TempDir()
	cwd := t.TempDir()
	projectPath := filepath.Join(cwd, "repo")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatalf("MkdirAll(projectPath) error = %v", err)
	}
	configPath := filepath.Join(t.TempDir(), "bootstrap-existing.json")
	managedPath := filepath.Join(homeDir, ".looper", "bin", "looperd")

	existingConfig := map[string]any{
		"server": map[string]any{"baseUrl": "http://daemon.test", "authMode": "none"},
		"projects": []map[string]any{{
			"id":         "repo",
			"name":       "repo",
			"repoPath":   projectPath,
			"baseBranch": "main",
		}},
	}
	raw, err := json.Marshal(existingConfig)
	if err != nil {
		t.Fatalf("Marshal(existingConfig) error = %v", err)
	}
	if err := os.WriteFile(configPath, raw, 0o644); err != nil {
		t.Fatalf("WriteFile(configPath) error = %v", err)
	}
	before := string(raw)

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	var spawnCalls atomic.Int32
	var installAssetCalls atomic.Int32

	app := New(Deps{
		Stdout:   stdout,
		Stderr:   stderr,
		HomeDir:  homeDir,
		Platform: "darwin",
		Arch:     "arm64",
		Getwd: func() (string, error) {
			return cwd, nil
		},
		LookPath: func(file string) (string, error) {
			switch file {
			case "git", "gh", "osascript":
				return "/usr/bin/" + file, nil
			default:
				return "", fmt.Errorf("not found")
			}
		},
		HTTPClient: newTestHTTPClient(func(req *http.Request) (*http.Response, error) {
			switch req.URL.String() {
			case "https://api.github.com/repos/powerformer/looper/releases/latest":
				return jsonResponse(t, http.StatusOK, `{"tag_name":"v1.2.3","assets":[]}`), nil
			case "https://example.invalid/looperd-darwin-arm64", "https://example.invalid/looperd-darwin-arm64.sha256":
				installAssetCalls.Add(1)
				t.Fatalf("unexpected daemon asset download request %q", req.URL.String())
				return nil, nil
			}
			if req.URL.String() == "http://daemon.test/api/v1/status" {
				return jsonResponse(t, http.StatusOK, `{"ok":true,"requestId":"req_status","data":{"service":{"healthy":true,"binary":{"name":"looperd"}}}}`), nil
			}
			if req.URL.String() == "http://daemon.test/api/v1/healthz" {
				return jsonResponse(t, http.StatusOK, `{"ok":true,"requestId":"req_health","data":{"healthy":true}}}`), nil
			}
			t.Fatalf("unexpected request URL %q", req.URL.String())
			return nil, nil
		}),
		RunCommand: func(ctx context.Context, command string, args []string, timeout time.Duration) (commandExecutionResult, error) {
			_ = ctx
			_ = timeout
			if command == "/usr/bin/gh" && strings.Join(args, " ") == "auth status" {
				return commandExecutionResult{ExitCode: 0}, nil
			}
			if command == managedPath && strings.Join(args, " ") == "--version" {
				return commandExecutionResult{ExitCode: 0, Stdout: "1.2.3\n"}, nil
			}
			return commandExecutionResult{ExitCode: 1, Stderr: "not found"}, nil
		},
		SpawnDetached: func(command string, args []string, cwd string, env []string) (int, error) {
			_ = command
			_ = args
			_ = cwd
			_ = env
			spawnCalls.Add(1)
			return 0, fmt.Errorf("spawn should not be called in idempotent flow")
		},
	})

	exitCode := app.Run(context.Background(), []string{"bootstrap", "--yes", "--project-path", projectPath, "--json", "--config", configPath})
	if exitCode != 0 {
		t.Fatalf("Run([bootstrap --yes --json]) exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("Run([bootstrap --yes --json]) stderr = %q, want empty string", stderr.String())
	}
	if spawnCalls.Load() != 0 {
		t.Fatalf("spawnDetached calls = %d, want 0", spawnCalls.Load())
	}
	if installAssetCalls.Load() != 0 {
		t.Fatalf("daemon asset download calls = %d, want 0", installAssetCalls.Load())
	}

	assertJSONContains(t, stdout.String(), "configCreated", false)
	assertJSONContains(t, stdout.String(), "projectAdded", false)
	assertJSONContains(t, stdout.String(), "daemonInstallState", "already-installed")
	assertJSONContains(t, stdout.String(), "daemonInstalled", false)
	assertJSONContains(t, stdout.String(), "daemonRunning", true)
	assertJSONContains(t, stdout.String(), "apiReachable", true)
	assertJSONContains(t, stdout.String(), "nextSteps", []any{"looper status"})

	afterRaw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile(configPath) error = %v", err)
	}
	if string(afterRaw) != before {
		t.Fatalf("config changed unexpectedly\nbefore=%s\nafter=%s", before, string(afterRaw))
	}
}

func TestBootstrapIdempotentSkipsGitHubWhenManagedDaemonInstalled(t *testing.T) {
	t.Parallel()

	homeDir := t.TempDir()
	cwd := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "bootstrap-offline.json")
	managedPath := filepath.Join(homeDir, ".looper", "bin", "looperd")
	if err := os.WriteFile(configPath, []byte(`{"server":{"baseUrl":"http://daemon.test","authMode":"none"}}`), 0o644); err != nil {
		t.Fatalf("WriteFile(configPath) error = %v", err)
	}

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := New(Deps{
		Stdout:   stdout,
		Stderr:   stderr,
		HomeDir:  homeDir,
		Platform: "darwin",
		Arch:     "arm64",
		Getwd: func() (string, error) {
			return cwd, nil
		},
		LookPath: func(file string) (string, error) {
			switch file {
			case "git", "gh":
				return "/usr/bin/" + file, nil
			default:
				return "", fmt.Errorf("not found")
			}
		},
		HTTPClient: newTestHTTPClient(func(req *http.Request) (*http.Response, error) {
			switch req.URL.String() {
			case "http://daemon.test/api/v1/status":
				return jsonResponse(t, http.StatusOK, `{"ok":true,"requestId":"req_status","data":{"service":{"healthy":true,"binary":{"name":"looperd"}}}}`), nil
			case "http://daemon.test/api/v1/healthz":
				return jsonResponse(t, http.StatusOK, `{"ok":true,"requestId":"req_health","data":{"healthy":true}}}`), nil
			default:
				t.Fatalf("unexpected request URL %q", req.URL.String())
				return nil, nil
			}
		}),
		RunCommand: func(ctx context.Context, command string, args []string, timeout time.Duration) (commandExecutionResult, error) {
			_ = ctx
			_ = timeout
			if command == "/usr/bin/gh" && strings.Join(args, " ") == "auth status" {
				return commandExecutionResult{ExitCode: 0}, nil
			}
			if command == managedPath && strings.Join(args, " ") == "--version" {
				return commandExecutionResult{ExitCode: 0, Stdout: "0.1.0\n"}, nil
			}
			return commandExecutionResult{ExitCode: 1, Stderr: "not found"}, nil
		},
		SpawnDetached: func(command string, args []string, cwd string, env []string) (int, error) {
			_ = command
			_ = args
			_ = cwd
			_ = env
			return 0, fmt.Errorf("spawn should not be called")
		},
	})

	exitCode := app.Run(context.Background(), []string{"bootstrap", "--yes", "--json", "--config", configPath})
	if exitCode != 0 {
		t.Fatalf("Run([bootstrap --yes --json]) exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("Run([bootstrap --yes --json]) stderr = %q, want empty string", stderr.String())
	}
	assertJSONContains(t, stdout.String(), "daemonInstallState", "already-installed")
	assertJSONContains(t, stdout.String(), "daemonInstalled", false)
}

func TestBootstrapAddsProjectWithoutPersistingRuntimeOverrides(t *testing.T) {
	homeDir := t.TempDir()
	cwd := t.TempDir()
	projectPath := filepath.Join(cwd, "repo")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatalf("MkdirAll(projectPath) error = %v", err)
	}
	configPath := filepath.Join(t.TempDir(), "bootstrap-partial.json")
	managedPath := filepath.Join(homeDir, ".looper", "bin", "looperd")

	before := `{"server":{"baseUrl":"http://daemon.test","authMode":"none"},"defaults":{"baseBranch":"trunk"}}`
	if err := os.WriteFile(configPath, []byte(before), 0o644); err != nil {
		t.Fatalf("WriteFile(configPath) error = %v", err)
	}

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}

	app := New(Deps{
		Stdout:   stdout,
		Stderr:   stderr,
		HomeDir:  homeDir,
		Platform: "darwin",
		Arch:     "arm64",
		Getwd: func() (string, error) {
			return cwd, nil
		},
		LookPath: func(file string) (string, error) {
			switch file {
			case "git", "gh", "osascript":
				return "/detected/" + file, nil
			default:
				return "", fmt.Errorf("not found")
			}
		},
		HTTPClient: newTestHTTPClient(func(req *http.Request) (*http.Response, error) {
			switch req.URL.String() {
			case "https://api.github.com/repos/powerformer/looper/releases/latest":
				return jsonResponse(t, http.StatusOK, `{"tag_name":"v1.2.3","assets":[]}`), nil
			case "http://daemon.test/api/v1/status":
				return jsonResponse(t, http.StatusOK, `{"ok":true,"requestId":"req_status","data":{"service":{"healthy":true,"binary":{"name":"looperd"}}}}`), nil
			case "http://daemon.test/api/v1/healthz":
				return jsonResponse(t, http.StatusOK, `{"ok":true,"requestId":"req_health","data":{"healthy":true}}}`), nil
			default:
				t.Fatalf("unexpected request URL %q", req.URL.String())
				return nil, nil
			}
		}),
		RunCommand: func(ctx context.Context, command string, args []string, timeout time.Duration) (commandExecutionResult, error) {
			_ = ctx
			_ = timeout
			if command == "/detected/gh" && strings.Join(args, " ") == "auth status" {
				return commandExecutionResult{ExitCode: 0}, nil
			}
			if command == managedPath && strings.Join(args, " ") == "--version" {
				return commandExecutionResult{ExitCode: 0, Stdout: "1.2.3\n"}, nil
			}
			return commandExecutionResult{ExitCode: 1, Stderr: "not found"}, nil
		},
		SpawnDetached: func(command string, args []string, cwd string, env []string) (int, error) {
			_ = command
			_ = args
			_ = cwd
			_ = env
			return 0, fmt.Errorf("spawn should not be called")
		},
	})

	t.Setenv("LOOPER_PORT", "9999")
	exitCode := app.Run(context.Background(), []string{"--host", "0.0.0.0", "--git-path", "/override/git", "bootstrap", "--yes", "--project-path", projectPath, "--config", configPath})
	if exitCode != 0 {
		t.Fatalf("Run([bootstrap]) exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("Run([bootstrap]) stderr = %q, want empty string", stderr.String())
	}

	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile(configPath) error = %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("Unmarshal(config) error = %v\nraw=%s", err, string(raw))
	}
	server := decoded["server"].(map[string]any)
	if _, ok := server["host"]; ok {
		t.Fatalf("persisted server.host from CLI override: %s", string(raw))
	}
	if _, ok := server["port"]; ok {
		t.Fatalf("persisted server.port from env override: %s", string(raw))
	}
	if _, ok := decoded["tools"]; ok {
		t.Fatalf("persisted detected or overridden tools: %s", string(raw))
	}
	if _, ok := decoded["storage"]; ok {
		t.Fatalf("persisted default-only storage config: %s", string(raw))
	}
	projects, ok := decoded["projects"].([]any)
	if !ok || len(projects) != 1 {
		t.Fatalf("projects = %#v, want one project", decoded["projects"])
	}
	project := projects[0].(map[string]any)
	if project["baseBranch"] != "trunk" {
		t.Fatalf("project.baseBranch = %v, want trunk", project["baseBranch"])
	}
}
