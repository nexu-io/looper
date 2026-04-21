package cliapp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestUpgradeCheckPrintsSummary(t *testing.T) {
	t.Parallel()

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
				return nil, fmt.Errorf("daemon offline")
			case "https://api.github.com/repos/powerformer/looper/releases/latest":
				return jsonResponse(t, http.StatusOK, `{"tag_name":"v0.3.0","assets":[]}`), nil
			default:
				t.Fatalf("unexpected request URL %q", req.URL.String())
				return nil, nil
			}
		}),
		RunCommand: func(ctx context.Context, command string, args []string, timeout time.Duration) (commandExecutionResult, error) {
			_ = ctx
			_ = timeout
			if command == managedPath && strings.Join(args, " ") == "--version" {
				return commandExecutionResult{Stdout: "0.2.1\n", ExitCode: 0}, nil
			}
			return commandExecutionResult{ExitCode: 1, Stderr: "not found"}, nil
		},
	})

	exitCode := app.Run(context.Background(), []string{"upgrade", "--check", "--config", configPath})
	if exitCode != 0 {
		t.Fatalf("Run([upgrade --check]) exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("Run([upgrade --check]) stderr = %q, want empty string", stderr.String())
	}
	if !strings.Contains(stdout.String(), "Upgrade check") {
		t.Fatalf("stdout = %q, want Upgrade check section", stdout.String())
	}
	for _, want := range []string{"cliCurrent", "0.2.1", "cliLatest", "0.3.0", "installed-binary"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("stdout = %q, want to contain %q", stdout.String(), want)
		}
	}
}

func TestUpgradeRejectsCombiningCheckAndDaemon(t *testing.T) {
	t.Parallel()

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := New(Deps{Stdout: stdout, Stderr: stderr})

	exitCode := app.Run(context.Background(), []string{"upgrade", "--check", "--daemon"})
	if exitCode != 1 {
		t.Fatalf("Run([upgrade --check --daemon]) exit code = %d, want 1", exitCode)
	}
	if !strings.Contains(stderr.String(), "--check and --daemon cannot be combined") {
		t.Fatalf("stderr = %q, want combination error", stderr.String())
	}
}

func TestUpgradeWithoutFlagsExplainsNotImplemented(t *testing.T) {
	t.Parallel()

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := New(Deps{Stdout: stdout, Stderr: stderr})

	exitCode := app.Run(context.Background(), []string{"upgrade"})
	if exitCode != 1 {
		t.Fatalf("Run([upgrade]) exit code = %d, want 1", exitCode)
	}
	if !strings.Contains(stderr.String(), "Full `looper upgrade` (CLI + daemon) is not implemented yet") {
		t.Fatalf("stderr = %q, want bare-upgrade guidance", stderr.String())
	}
}

func TestUpgradeDaemonPrintsRestartHint(t *testing.T) {
	t.Parallel()

	homeDir := t.TempDir()
	managedPath := filepath.Join(homeDir, ".looper", "bin", "looperd")
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	binary := []byte{1, 2, 3, 4}
	checksumText := "9f64a747e1b97f131fabb6b447296c9b6f0201e79fb3c5356e6c77e89b6a806a  looperd-darwin-arm64\n"
	configPath := writeCLIConfig(t, "http://127.0.0.1:4321", "")

	app := New(Deps{
		Stdout:   stdout,
		Stderr:   stderr,
		HomeDir:  homeDir,
		Platform: "darwin",
		Arch:     "arm64",
		HTTPClient: newTestHTTPClient(func(req *http.Request) (*http.Response, error) {
			switch req.URL.String() {
			case "http://127.0.0.1:4321/api/v1/status":
				return nil, fmt.Errorf("daemon offline")
			case "https://api.github.com/repos/powerformer/looper/releases/latest":
				return jsonResponse(t, http.StatusOK, `{"tag_name":"v0.3.0","assets":[{"name":"looperd-darwin-arm64","browser_download_url":"https://example.invalid/looperd-darwin-arm64"},{"name":"looperd-darwin-arm64.sha256","browser_download_url":"https://example.invalid/looperd-darwin-arm64.sha256"}]}`), nil
			case "https://api.github.com/repos/powerformer/looper/releases/tags/v0.3.0":
				return jsonResponse(t, http.StatusOK, `{"tag_name":"v0.3.0","assets":[{"name":"looperd-darwin-arm64","browser_download_url":"https://example.invalid/looperd-darwin-arm64"},{"name":"looperd-darwin-arm64.sha256","browser_download_url":"https://example.invalid/looperd-darwin-arm64.sha256"}]}`), nil
			case "https://example.invalid/looperd-darwin-arm64":
				return binaryResponse(t, http.StatusOK, binary), nil
			case "https://example.invalid/looperd-darwin-arm64.sha256":
				return textResponse(t, http.StatusOK, checksumText), nil
			default:
				t.Fatalf("unexpected request URL %q", req.URL.String())
				return nil, nil
			}
		}),
		RunCommand: func(ctx context.Context, command string, args []string, timeout time.Duration) (commandExecutionResult, error) {
			_ = ctx
			_ = timeout
			if command == managedPath && strings.Join(args, " ") == "--version" {
				return commandExecutionResult{Stdout: "0.2.1\n", ExitCode: 0}, nil
			}
			return commandExecutionResult{ExitCode: 1, Stderr: "not found"}, nil
		},
	})

	exitCode := app.Run(context.Background(), []string{"upgrade", "--daemon", "--config", configPath})
	if exitCode != 0 {
		t.Fatalf("Run([upgrade --daemon]) exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("Run([upgrade --daemon]) stderr = %q, want empty string", stderr.String())
	}
	if !strings.Contains(stdout.String(), "Upgraded looperd 0.2.1 → 0.3.0") {
		t.Fatalf("stdout = %q, want upgrade confirmation", stdout.String())
	}
	if !strings.Contains(stdout.String(), "looper daemon restart") {
		t.Fatalf("stdout = %q, want restart hint", stdout.String())
	}
}

func TestUpgradeDaemonSkipsCurrentManagedBinary(t *testing.T) {
	t.Parallel()

	homeDir := t.TempDir()
	managedPath := filepath.Join(homeDir, ".looper", "bin", "looperd")
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	configPath := writeCLIConfig(t, "http://127.0.0.1:4321", "")

	app := New(Deps{
		Stdout:  stdout,
		Stderr:  stderr,
		HomeDir: homeDir,
		HTTPClient: newTestHTTPClient(func(req *http.Request) (*http.Response, error) {
			switch req.URL.String() {
			case "http://127.0.0.1:4321/api/v1/status":
				return nil, fmt.Errorf("daemon offline")
			case "https://api.github.com/repos/powerformer/looper/releases/latest":
				return jsonResponse(t, http.StatusOK, `{"tag_name":"v0.2.1","assets":[]}`), nil
			default:
				t.Fatalf("unexpected request URL %q", req.URL.String())
				return nil, nil
			}
		}),
		RunCommand: func(ctx context.Context, command string, args []string, timeout time.Duration) (commandExecutionResult, error) {
			_ = ctx
			_ = timeout
			if command == managedPath && strings.Join(args, " ") == "--version" {
				return commandExecutionResult{Stdout: "0.2.1\n", ExitCode: 0}, nil
			}
			return commandExecutionResult{ExitCode: 1, Stderr: "not found"}, nil
		},
	})

	exitCode := app.Run(context.Background(), []string{"upgrade", "--daemon", "--config", configPath})
	if exitCode != 0 {
		t.Fatalf("Run([upgrade --daemon]) exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("Run([upgrade --daemon]) stderr = %q, want empty string", stderr.String())
	}
	if !strings.Contains(stdout.String(), "looperd is already up to date (0.2.1)") {
		t.Fatalf("stdout = %q, want current-version message", stdout.String())
	}
	if !strings.Contains(stdout.String(), managedPath) {
		t.Fatalf("stdout = %q, want managed binary path", stdout.String())
	}
}

func TestUpgradeDaemonInstallsManagedBinaryWhenOnlyPathBinaryExists(t *testing.T) {
	t.Parallel()

	homeDir := t.TempDir()
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	binary := []byte{1, 2, 3, 4}
	checksumText := "9f64a747e1b97f131fabb6b447296c9b6f0201e79fb3c5356e6c77e89b6a806a  looperd-darwin-arm64\n"
	configPath := writeCLIConfig(t, "http://127.0.0.1:4321", "")

	app := New(Deps{
		Stdout:   stdout,
		Stderr:   stderr,
		HomeDir:  homeDir,
		Platform: "darwin",
		Arch:     "arm64",
		HTTPClient: newTestHTTPClient(func(req *http.Request) (*http.Response, error) {
			switch req.URL.String() {
			case "http://127.0.0.1:4321/api/v1/status":
				return nil, fmt.Errorf("daemon offline")
			case "https://api.github.com/repos/powerformer/looper/releases/latest":
				return jsonResponse(t, http.StatusOK, `{"tag_name":"v0.4.0","assets":[{"name":"looperd-darwin-arm64","browser_download_url":"https://example.invalid/looperd-darwin-arm64"},{"name":"looperd-darwin-arm64.sha256","browser_download_url":"https://example.invalid/looperd-darwin-arm64.sha256"}]}`), nil
			case "https://api.github.com/repos/powerformer/looper/releases/tags/v0.4.0":
				return jsonResponse(t, http.StatusOK, `{"tag_name":"v0.4.0","assets":[{"name":"looperd-darwin-arm64","browser_download_url":"https://example.invalid/looperd-darwin-arm64"},{"name":"looperd-darwin-arm64.sha256","browser_download_url":"https://example.invalid/looperd-darwin-arm64.sha256"}]}`), nil
			case "https://example.invalid/looperd-darwin-arm64":
				return binaryResponse(t, http.StatusOK, binary), nil
			case "https://example.invalid/looperd-darwin-arm64.sha256":
				return textResponse(t, http.StatusOK, checksumText), nil
			default:
				t.Fatalf("unexpected request URL %q", req.URL.String())
				return nil, nil
			}
		}),
		RunCommand: func(ctx context.Context, command string, args []string, timeout time.Duration) (commandExecutionResult, error) {
			_ = ctx
			_ = timeout
			if command == filepath.Join(homeDir, ".looper", "bin", "looperd") && strings.Join(args, " ") == "--version" {
				return commandExecutionResult{ExitCode: 1, Stderr: "not found"}, nil
			}
			if command == looperdBinaryName && strings.Join(args, " ") == "--version" {
				return commandExecutionResult{Stdout: "0.4.0\n", ExitCode: 0}, nil
			}
			return commandExecutionResult{ExitCode: 1, Stderr: "not found"}, nil
		},
	})

	exitCode := app.Run(context.Background(), []string{"upgrade", "--daemon", "--config", configPath})
	if exitCode != 0 {
		t.Fatalf("Run([upgrade --daemon]) exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("Run([upgrade --daemon]) stderr = %q, want empty string", stderr.String())
	}
	if !strings.Contains(stdout.String(), "Installed managed looperd 0.4.0") {
		t.Fatalf("stdout = %q, want managed install message", stdout.String())
	}
	if !strings.Contains(stdout.String(), "previously using looperd") {
		t.Fatalf("stdout = %q, want PATH fallback note", stdout.String())
	}
}

func TestManagedDaemonInstallUpgradeLifecycleEndToEnd(t *testing.T) {
	homeDir := t.TempDir()
	managedPath := filepath.Join(homeDir, ".looper", "bin", "looperd")
	configPath := writeCLIConfig(t, "http://daemon.test", "")
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}

	oldBinary := []byte("looperd-v0.2.0")
	newBinary := []byte("looperd-v0.3.0")
	oldChecksum := sha256.Sum256(oldBinary)
	newChecksum := sha256.Sum256(newBinary)

	type processState struct {
		version string
		alive   bool
	}
	var (
		mu             sync.Mutex
		nextPID        = 2000
		processes      = map[int]processState{}
		runningPID     int
		runningVersion string
		latestCalls    int
	)

	app := New(Deps{
		Stdout:   stdout,
		Stderr:   stderr,
		HomeDir:  homeDir,
		Platform: "darwin",
		Arch:     "arm64",
		HTTPClient: newTestHTTPClient(func(req *http.Request) (*http.Response, error) {
			switch req.URL.String() {
			case "https://api.github.com/repos/powerformer/looper/releases/latest":
				latestCalls += 1
				if latestCalls == 1 {
					return jsonResponse(t, http.StatusOK, `{"tag_name":"v0.2.0","assets":[{"name":"looperd-darwin-arm64","browser_download_url":"https://example.invalid/looperd-darwin-arm64-v0.2.0"},{"name":"looperd-darwin-arm64.sha256","browser_download_url":"https://example.invalid/looperd-darwin-arm64-v0.2.0.sha256"}]}`), nil
				}
				return jsonResponse(t, http.StatusOK, `{"tag_name":"v0.3.0","assets":[{"name":"looperd-darwin-arm64","browser_download_url":"https://example.invalid/looperd-darwin-arm64-v0.3.0"},{"name":"looperd-darwin-arm64.sha256","browser_download_url":"https://example.invalid/looperd-darwin-arm64-v0.3.0.sha256"}]}`), nil
			case "https://api.github.com/repos/powerformer/looper/releases/tags/v0.3.0":
				return jsonResponse(t, http.StatusOK, `{"tag_name":"v0.3.0","assets":[{"name":"looperd-darwin-arm64","browser_download_url":"https://example.invalid/looperd-darwin-arm64-v0.3.0"},{"name":"looperd-darwin-arm64.sha256","browser_download_url":"https://example.invalid/looperd-darwin-arm64-v0.3.0.sha256"}]}`), nil
			case "https://api.github.com/repos/powerformer/looper/releases/tags/v0.2.0":
				return jsonResponse(t, http.StatusOK, `{"tag_name":"v0.2.0","assets":[{"name":"looperd-darwin-arm64","browser_download_url":"https://example.invalid/looperd-darwin-arm64-v0.2.0"},{"name":"looperd-darwin-arm64.sha256","browser_download_url":"https://example.invalid/looperd-darwin-arm64-v0.2.0.sha256"}]}`), nil
			case "https://example.invalid/looperd-darwin-arm64-v0.2.0":
				return binaryResponse(t, http.StatusOK, oldBinary), nil
			case "https://example.invalid/looperd-darwin-arm64-v0.2.0.sha256":
				return textResponse(t, http.StatusOK, hex.EncodeToString(oldChecksum[:])+"  looperd-darwin-arm64\n"), nil
			case "https://example.invalid/looperd-darwin-arm64-v0.3.0":
				return binaryResponse(t, http.StatusOK, newBinary), nil
			case "https://example.invalid/looperd-darwin-arm64-v0.3.0.sha256":
				return textResponse(t, http.StatusOK, hex.EncodeToString(newChecksum[:])+"  looperd-darwin-arm64\n"), nil
			case "http://daemon.test/api/v1/status":
				mu.Lock()
				defer mu.Unlock()
				if runningPID == 0 {
					return nil, fmt.Errorf("daemon offline")
				}
				return jsonResponse(t, http.StatusOK, fmt.Sprintf(`{"ok":true,"requestId":"req_status","data":{"service":{"version":%q}}}`, runningVersion)), nil
			default:
				t.Fatalf("unexpected request URL %q", req.URL.String())
				return nil, nil
			}
		}),
		RunCommand: func(ctx context.Context, command string, args []string, timeout time.Duration) (commandExecutionResult, error) {
			_ = ctx
			_ = timeout
			if command == managedPath && strings.Join(args, " ") == "--version" {
				raw, err := os.ReadFile(managedPath)
				if err != nil {
					return commandExecutionResult{ExitCode: 1, Stderr: "not found"}, nil
				}
				switch string(raw) {
				case string(oldBinary):
					return commandExecutionResult{Stdout: "0.2.0\n", ExitCode: 0}, nil
				case string(newBinary):
					return commandExecutionResult{Stdout: "0.3.0\n", ExitCode: 0}, nil
				default:
					return commandExecutionResult{ExitCode: 1, Stderr: "unknown binary"}, nil
				}
			}
			if command == "ps" && len(args) == 4 && args[0] == "-p" && args[2] == "-o" && args[3] == "command=" {
				return commandExecutionResult{Stdout: managedPath + "\n", ExitCode: 0}, nil
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
			raw, err := os.ReadFile(managedPath)
			if err != nil {
				return 0, err
			}
			version := ""
			switch string(raw) {
			case string(oldBinary):
				version = "0.2.0"
			case string(newBinary):
				version = "0.3.0"
			default:
				return 0, fmt.Errorf("unknown binary bytes")
			}

			mu.Lock()
			defer mu.Unlock()
			nextPID += 1
			processes[nextPID] = processState{version: version, alive: true}
			runningPID = nextPID
			runningVersion = version
			return nextPID, nil
		},
		KillProcess: func(pid int, signal int) error {
			mu.Lock()
			defer mu.Unlock()
			proc, ok := processes[pid]
			if !ok || !proc.alive {
				return os.ErrProcessDone
			}
			if signal == 15 {
				proc.alive = false
				processes[pid] = proc
				if runningPID == pid {
					runningPID = 0
					runningVersion = ""
				}
			}
			return nil
		},
		Sleep: func(duration time.Duration) {
			_ = duration
		},
		Getwd: func() (string, error) {
			return t.TempDir(), nil
		},
	})

	if exitCode := app.Run(context.Background(), []string{"daemon", "install", "--force", "--config", configPath}); exitCode != 0 {
		t.Fatalf("Run([daemon install --force]) exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Installed looperd (darwin-arm64)") {
		t.Fatalf("stdout = %q, want install confirmation", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if exitCode := app.Run(context.Background(), []string{"daemon", "start", "--config", configPath}); exitCode != 0 {
		t.Fatalf("Run([daemon start]) exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if exitCode := app.Run(context.Background(), []string{"daemon", "status", "--json", "--config", configPath}); exitCode != 0 {
		t.Fatalf("Run([daemon status --json]) exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	assertJSONContains(t, stdout.String(), "apiReachable", true)
	assertJSONContains(t, stdout.String(), "daemonVersion", "0.2.0")
	assertJSONContains(t, stdout.String(), "daemonVersionSource", "api")

	stdout.Reset()
	stderr.Reset()
	if exitCode := app.Run(context.Background(), []string{"upgrade", "--daemon", "--config", configPath}); exitCode != 0 {
		t.Fatalf("Run([upgrade --daemon]) exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Upgraded looperd 0.2.0 → 0.3.0") {
		t.Fatalf("stdout = %q, want upgrade confirmation", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if exitCode := app.Run(context.Background(), []string{"daemon", "restart", "--config", configPath}); exitCode != 0 {
		t.Fatalf("Run([daemon restart]) exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if exitCode := app.Run(context.Background(), []string{"daemon", "status", "--json", "--config", configPath}); exitCode != 0 {
		t.Fatalf("Run([daemon status --json]) exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	assertJSONContains(t, stdout.String(), "apiReachable", true)
	assertJSONContains(t, stdout.String(), "daemonVersion", "0.3.0")
	assertJSONContains(t, stdout.String(), "daemonVersionSource", "api")
}
