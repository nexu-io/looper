package cliapp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestResolveLooperTarget(t *testing.T) {
	t.Parallel()

	target, err := resolveLooperTarget("darwin", "arm64")
	if err != nil {
		t.Fatalf("resolveLooperTarget(darwin, arm64) error = %v", err)
	}
	if target != "darwin-arm64" {
		t.Fatalf("resolveLooperTarget(darwin, arm64) = %q, want %q", target, "darwin-arm64")
	}

	for _, arch := range []string{"amd64", "x64"} {
		_, err = resolveLooperTarget("darwin", arch)
		if err == nil {
			t.Fatalf("resolveLooperTarget(darwin, %s) error = nil, want unsupported", arch)
		}
	}
}

func TestShouldRunAutoUpgradeCheck(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	recent := now.Add(-autoUpgradeCheckInterval + time.Minute)
	old := now.Add(-autoUpgradeCheckInterval - time.Minute)

	for _, tc := range []struct {
		name  string
		state *autoUpgradeState
		want  bool
	}{
		{name: "nil state", state: nil, want: true},
		{name: "missing timestamp", state: &autoUpgradeState{}, want: true},
		{name: "recent timestamp", state: &autoUpgradeState{LastCheckedAt: &recent}, want: false},
		{name: "expired timestamp", state: &autoUpgradeState{LastCheckedAt: &old}, want: true},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := shouldRunAutoUpgradeCheck(tc.state, now); got != tc.want {
				t.Fatalf("shouldRunAutoUpgradeCheck() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRootPreRunSkipsAutoUpgradeForUnsupportedInstallSource(t *testing.T) {
	t.Parallel()

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := New(Deps{
		Stdout:         stdout,
		Stderr:         stderr,
		ExecutablePath: "/opt/homebrew/Cellar/looper/0.2.1/bin/looper",
		HTTPClient: newTestHTTPClient(func(req *http.Request) (*http.Response, error) {
			t.Fatalf("unexpected auto-upgrade request to %q", req.URL.String())
			return nil, nil
		}),
	})

	exitCode := app.Run(context.Background(), []string{"version"})
	if exitCode != 0 {
		t.Fatalf("Run([version]) exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("Run([version]) stderr = %q, want empty string", stderr.String())
	}
	if got, want := stdout.String(), "0.0.0-dev\n"; got != want {
		t.Fatalf("Run([version]) stdout = %q, want %q", got, want)
	}
}

func TestRootPreRunSkipsAutoUpgradeForBareRootHelp(t *testing.T) {
	t.Parallel()

	homeDir := t.TempDir()
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := New(Deps{
		Stdout:         stdout,
		Stderr:         stderr,
		HomeDir:        homeDir,
		ExecutablePath: filepath.Join(homeDir, ".looper", "bin", "looper"),
		HTTPClient: newTestHTTPClient(func(req *http.Request) (*http.Response, error) {
			t.Fatalf("unexpected auto-upgrade request to %q", req.URL.String())
			return nil, nil
		}),
	})

	exitCode := app.Run(context.Background(), nil)
	if exitCode != 0 {
		t.Fatalf("Run([]) exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("Run([]) stderr = %q, want empty string", stderr.String())
	}
	if !strings.Contains(stdout.String(), "Usage:\n  looper") {
		t.Fatalf("Run([]) stdout = %q, want root help output", stdout.String())
	}
}

func TestRootPreRunSkipsAutoUpgradeForHelpOnlySubcommand(t *testing.T) {
	t.Parallel()

	homeDir := t.TempDir()
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := New(Deps{
		Stdout:         stdout,
		Stderr:         stderr,
		HomeDir:        homeDir,
		ExecutablePath: filepath.Join(homeDir, ".looper", "bin", "looper"),
		HTTPClient: newTestHTTPClient(func(req *http.Request) (*http.Response, error) {
			t.Fatalf("unexpected auto-upgrade request to %q", req.URL.String())
			return nil, nil
		}),
	})

	exitCode := app.Run(context.Background(), []string{"daemon"})
	if exitCode != 0 {
		t.Fatalf("Run([daemon]) exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("Run([daemon]) stderr = %q, want empty string", stderr.String())
	}
	if !strings.Contains(stdout.String(), "Usage:\n  looper daemon") {
		t.Fatalf("Run([daemon]) stdout = %q, want daemon help output", stdout.String())
	}
}

func TestVersionNoAutoUpgradeFlagDisablesAutoUpgrade(t *testing.T) {
	t.Parallel()

	homeDir := t.TempDir()
	configPath := writeCLIConfig(t, "http://127.0.0.1:4321", "")
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := New(Deps{
		Stdout:         stdout,
		Stderr:         stderr,
		HomeDir:        homeDir,
		ExecutablePath: filepath.Join(homeDir, ".looper", "bin", "looper"),
		HTTPClient: newTestHTTPClient(func(req *http.Request) (*http.Response, error) {
			t.Fatalf("unexpected auto-upgrade request to %q", req.URL.String())
			return nil, nil
		}),
	})

	exitCode := app.Run(context.Background(), []string{"version", "--no-auto-upgrade", "--config", configPath})
	if exitCode != 0 {
		t.Fatalf("Run([version --no-auto-upgrade]) exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("Run([version --no-auto-upgrade]) stderr = %q, want empty string", stderr.String())
	}
}

func TestEnvDisablesAutoUpgrade(t *testing.T) {
	homeDir := t.TempDir()
	configPath := writeCLIConfig(t, "http://127.0.0.1:4321", "")
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	t.Setenv("LOOPER_AUTO_UPGRADE_ENABLED", "false")
	app := New(Deps{
		Stdout:         stdout,
		Stderr:         stderr,
		HomeDir:        homeDir,
		ExecutablePath: filepath.Join(homeDir, ".looper", "bin", "looper"),
		HTTPClient: newTestHTTPClient(func(req *http.Request) (*http.Response, error) {
			t.Fatalf("unexpected auto-upgrade request to %q", req.URL.String())
			return nil, nil
		}),
	})

	exitCode := app.Run(context.Background(), []string{"version", "--config", configPath})
	if exitCode != 0 {
		t.Fatalf("Run([version --config]) exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("Run([version --config]) stderr = %q, want empty string", stderr.String())
	}
}

func TestAutoUpgradeCachesLatestReleaseCheck(t *testing.T) {
	t.Parallel()

	homeDir := t.TempDir()
	configPath := writeCLIConfig(t, "http://127.0.0.1:4321", "")
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	statePath := filepath.Join(homeDir, ".looper", "auto-upgrade.state.json")
	var latestCalls int
	app := New(Deps{
		Stdout:         stdout,
		Stderr:         stderr,
		HomeDir:        homeDir,
		ExecutablePath: filepath.Join(homeDir, ".looper", "bin", "looper"),
		HTTPClient: newTestHTTPClient(func(req *http.Request) (*http.Response, error) {
			switch req.URL.String() {
			case "https://api.github.com/repos/nexu-io/looper/releases/latest":
				latestCalls++
				return jsonResponse(t, http.StatusOK, `{"tag_name":"v0.0.0-dev","assets":[]}`), nil
			default:
				t.Fatalf("unexpected request URL %q", req.URL.String())
				return nil, nil
			}
		}),
		RunCommand: func(ctx context.Context, command string, args []string, timeout time.Duration) (commandExecutionResult, error) {
			_ = ctx
			_ = timeout
			return commandExecutionResult{ExitCode: 1, Stderr: "not found"}, nil
		},
	})

	for i := 0; i < 2; i++ {
		stdout.Reset()
		stderr.Reset()
		exitCode := app.Run(context.Background(), []string{"version", "--config", configPath})
		if exitCode != 0 {
			t.Fatalf("run %d Run([version --config]) exit code = %d, want 0; stderr=%q", i+1, exitCode, stderr.String())
		}
	}
	if latestCalls != 1 {
		t.Fatalf("latest release calls = %d, want 1", latestCalls)
	}
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("Stat(auto-upgrade state) error = %v, want file to exist", err)
	}
}

func TestCorruptAutoUpgradeStateSkipsNetworkAndPreservesFile(t *testing.T) {
	t.Parallel()

	homeDir := t.TempDir()
	configPath := writeCLIConfig(t, "http://127.0.0.1:4321", "")
	statePath := filepath.Join(homeDir, ".looper", "auto-upgrade.state.json")
	if err := os.MkdirAll(filepath.Dir(statePath), 0o755); err != nil {
		t.Fatalf("MkdirAll(state dir) error = %v", err)
	}
	const corrupt = "{invalid json\n"
	if err := os.WriteFile(statePath, []byte(corrupt), 0o644); err != nil {
		t.Fatalf("WriteFile(statePath) error = %v", err)
	}
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := New(Deps{
		Stdout:         stdout,
		Stderr:         stderr,
		HomeDir:        homeDir,
		ExecutablePath: filepath.Join(homeDir, ".looper", "bin", "looper"),
		HTTPClient: newTestHTTPClient(func(req *http.Request) (*http.Response, error) {
			t.Fatalf("unexpected auto-upgrade request to %q", req.URL.String())
			return nil, nil
		}),
	})

	exitCode := app.Run(context.Background(), []string{"version", "--config", configPath})
	if exitCode != 0 {
		t.Fatalf("Run([version --config]) exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if !strings.Contains(stderr.String(), "Auto-upgrade skipped: read state") {
		t.Fatalf("stderr = %q, want corrupt state warning", stderr.String())
	}
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("ReadFile(statePath) error = %v", err)
	}
	if string(raw) != corrupt {
		t.Fatalf("state file content = %q, want %q", string(raw), corrupt)
	}
}

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
			case "https://api.github.com/repos/nexu-io/looper/releases/latest":
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

func TestUpgradeCheckUsesManagedProvenanceFromStatusAPI(t *testing.T) {
	t.Parallel()

	homeDir := t.TempDir()
	managedPath := filepath.Join(homeDir, ".looper", "bin", "looperd")
	configPath := writeCLIConfig(t, "http://127.0.0.1:4321", "")
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}

	app := New(Deps{
		Stdout:  stdout,
		Stderr:  stderr,
		HomeDir: homeDir,
		HTTPClient: newTestHTTPClient(func(req *http.Request) (*http.Response, error) {
			switch req.URL.String() {
			case "http://127.0.0.1:4321/api/v1/status":
				return jsonResponse(t, http.StatusOK, `{"success":true,"data":{"service":{"version":"0.2.1","binary":{"path":"`+managedPath+`"}}}}`), nil
			case "https://api.github.com/repos/nexu-io/looper/releases/latest":
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

	exitCode := app.Run(context.Background(), []string{"upgrade", "--check", "--json", "--config", configPath})
	if exitCode != 0 {
		t.Fatalf("Run([upgrade --check --json]) exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("Run([upgrade --check --json]) stderr = %q, want empty string", stderr.String())
	}
	var decoded struct {
		Daemon struct {
			CurrentVersion string `json:"currentVersion"`
			Installed      bool   `json:"installed"`
			Source         string `json:"source"`
			BinaryPath     string `json:"binaryPath"`
		} `json:"daemon"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
		t.Fatalf("unmarshal stdout JSON: %v\nraw=%q", err, stdout.String())
	}
	if decoded.Daemon.CurrentVersion != "0.2.1" {
		t.Fatalf("daemon.currentVersion = %q, want 0.2.1", decoded.Daemon.CurrentVersion)
	}
	if !decoded.Daemon.Installed {
		t.Fatal("daemon.installed = false, want true")
	}
	if decoded.Daemon.Source != "installed-binary" {
		t.Fatalf("daemon.source = %q, want installed-binary", decoded.Daemon.Source)
	}
	if decoded.Daemon.BinaryPath != managedPath {
		t.Fatalf("daemon.binaryPath = %q, want %q", decoded.Daemon.BinaryPath, managedPath)
	}
}

func TestSelectUpgradeDaemonVersionStatePreservesAPIBinaryPath(t *testing.T) {
	t.Parallel()

	managedPath := "/tmp/.looper/bin/looperd"
	state := selectUpgradeDaemonVersionState(
		json.RawMessage(`{"service":{"version":"0.2.1","binary":{"path":"`+managedPath+`"}}}`),
		&upgradeDaemonVersionState{Version: "0.2.1", Source: "installed-binary", BinaryPath: stringPtr(managedPath)},
		nil,
	)
	if state == nil {
		t.Fatal("selectUpgradeDaemonVersionState() = nil, want state")
	}
	if state.Source != "installed-binary" {
		t.Fatalf("state.Source = %q, want installed-binary", state.Source)
	}
	if state.BinaryPath == nil || *state.BinaryPath != managedPath {
		t.Fatalf("state.BinaryPath = %v, want %q", state.BinaryPath, managedPath)
	}
}

func TestReplaceBinaryAtomicallyRestoresPreviousOnFinalRenameFailure(t *testing.T) {
	dir := t.TempDir()
	installPath := filepath.Join(dir, "looper")
	original := []byte("original")
	if err := os.WriteFile(installPath, original, 0o755); err != nil {
		t.Fatalf("WriteFile(installPath) error = %v", err)
	}

	var renameCalls int
	err := replaceBinaryAtomicallyWithRename(installPath, []byte("new"), func(oldPath string, newPath string) error {
		renameCalls++
		if renameCalls == 2 {
			return fmt.Errorf("injected final rename failure")
		}
		return os.Rename(oldPath, newPath)
	})
	if err == nil {
		t.Fatal("replaceBinaryAtomicallyWithRename() error = nil, want failure")
	}
	if !strings.Contains(err.Error(), "replace looper binary") {
		t.Fatalf("error = %v, want replace context", err)
	}
	restored, readErr := os.ReadFile(installPath)
	if readErr != nil {
		t.Fatalf("ReadFile(installPath) error = %v", readErr)
	}
	if string(restored) != string(original) {
		t.Fatalf("restored binary = %q, want %q", string(restored), string(original))
	}
	if _, statErr := os.Stat(installPath + ".new"); !os.IsNotExist(statErr) {
		t.Fatalf("staged binary still exists or stat failed: %v", statErr)
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
	if !strings.Contains(stderr.String(), "--check, --cli, and --daemon cannot be combined") {
		t.Fatalf("stderr = %q, want combination error", stderr.String())
	}
}

func TestUpgradeWithoutFlagsContinuesWithDaemonWhenCLISelfUpgradeRefused(t *testing.T) {
	t.Parallel()

	homeDir := t.TempDir()
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	binary := []byte{1, 2, 3, 4}
	checksumText := "9f64a747e1b97f131fabb6b447296c9b6f0201e79fb3c5356e6c77e89b6a806a  looperd-darwin-arm64\n"
	configPath := writeCLIConfig(t, "http://127.0.0.1:4321", "")
	managedPath := filepath.Join(homeDir, ".looper", "bin", "looperd")

	app := New(Deps{
		Stdout:         stdout,
		Stderr:         stderr,
		HomeDir:        homeDir,
		Platform:       "darwin",
		Arch:           "arm64",
		ExecutablePath: "/opt/homebrew/Cellar/looper/0.2.1/bin/looper",
		HTTPClient: newTestHTTPClient(func(req *http.Request) (*http.Response, error) {
			switch req.URL.String() {
			case "http://127.0.0.1:4321/api/v1/status":
				return nil, fmt.Errorf("daemon offline")
			case "https://api.github.com/repos/nexu-io/looper/releases/latest":
				return jsonResponse(t, http.StatusOK, `{"tag_name":"v0.3.0","assets":[{"name":"looperd-darwin-arm64","browser_download_url":"https://example.invalid/looperd-darwin-arm64"},{"name":"looperd-darwin-arm64.sha256","browser_download_url":"https://example.invalid/looperd-darwin-arm64.sha256"}]}`), nil
			case "https://api.github.com/repos/nexu-io/looper/releases/tags/v0.3.0":
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
				return commandExecutionResult{ExitCode: 1, Stderr: "not found"}, nil
			}
			if command == looperdBinaryName && strings.Join(args, " ") == "--version" {
				return commandExecutionResult{ExitCode: 1, Stderr: "not found"}, nil
			}
			return commandExecutionResult{ExitCode: 1, Stderr: "not found"}, nil
		},
	})

	exitCode := app.Run(context.Background(), []string{"upgrade", "--config", configPath})
	if exitCode != 0 {
		t.Fatalf("Run([upgrade]) exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "CLI self-upgrade skipped") {
		t.Fatalf("stdout = %q, want CLI refusal guidance", stdout.String())
	}
	if !strings.Contains(stdout.String(), "Proceeding with daemon upgrade") {
		t.Fatalf("stdout = %q, want daemon continuation note", stdout.String())
	}
	if !strings.Contains(stdout.String(), "Installed looperd 0.3.0") {
		t.Fatalf("stdout = %q, want daemon install message", stdout.String())
	}
}

func TestUpgradeWithoutFlagsWritesSingleJSONDocument(t *testing.T) {
	homeDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(homeDir, ".looper", "worktrees"), 0o755); err != nil {
		t.Fatalf("create test worktree root: %v", err)
	}
	t.Setenv("HOME", homeDir)

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	binary := []byte{1, 2, 3, 4}
	checksumText := "9f64a747e1b97f131fabb6b447296c9b6f0201e79fb3c5356e6c77e89b6a806a  looperd-darwin-arm64\n"
	configPath := writeCLIConfig(t, "http://127.0.0.1:4321", "")
	managedPath := filepath.Join(homeDir, ".looper", "bin", "looperd")

	app := New(Deps{
		Stdout:         stdout,
		Stderr:         stderr,
		HomeDir:        homeDir,
		Platform:       "darwin",
		Arch:           "arm64",
		ExecutablePath: "/opt/homebrew/Cellar/looper/0.2.1/bin/looper",
		HTTPClient: newTestHTTPClient(func(req *http.Request) (*http.Response, error) {
			switch req.URL.String() {
			case "http://127.0.0.1:4321/api/v1/status":
				return nil, fmt.Errorf("daemon offline")
			case "https://api.github.com/repos/nexu-io/looper/releases/latest":
				return jsonResponse(t, http.StatusOK, `{"tag_name":"v0.3.0","assets":[{"name":"looperd-darwin-arm64","browser_download_url":"https://example.invalid/looperd-darwin-arm64"},{"name":"looperd-darwin-arm64.sha256","browser_download_url":"https://example.invalid/looperd-darwin-arm64.sha256"}]}`), nil
			case "https://api.github.com/repos/nexu-io/looper/releases/tags/v0.3.0":
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
				return commandExecutionResult{ExitCode: 1, Stderr: "not found"}, nil
			}
			if command == looperdBinaryName && strings.Join(args, " ") == "--version" {
				return commandExecutionResult{ExitCode: 1, Stderr: "not found"}, nil
			}
			return commandExecutionResult{ExitCode: 1, Stderr: "not found"}, nil
		},
	})

	exitCode := app.Run(context.Background(), []string{"upgrade", "--json", "--config", configPath})
	if exitCode != 0 {
		t.Fatalf("Run([upgrade --json]) exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if !strings.Contains(stderr.String(), "Downloading looperd-darwin-arm64: 4 B / 4 B (100%)") {
		t.Fatalf("Run([upgrade --json]) stderr = %q, want daemon download progress", stderr.String())
	}
	if strings.Contains(stdout.String(), "Proceeding with daemon upgrade") {
		t.Fatalf("stdout = %q, want JSON without human progress text", stdout.String())
	}

	var decoded map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
		t.Fatalf("unmarshal stdout JSON: %v\nraw=%q", err, stdout.String())
	}
	if _, ok := decoded["cli"].(map[string]any); !ok {
		t.Fatalf("stdout JSON missing cli object: %#v", decoded)
	}
	if _, ok := decoded["daemon"].(map[string]any); !ok {
		t.Fatalf("stdout JSON missing daemon object: %#v", decoded)
	}
}

func TestUpgradeCLIRefusesHomebrewInstallWithGuidance(t *testing.T) {
	t.Parallel()

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := New(Deps{
		Stdout:         stdout,
		Stderr:         stderr,
		ExecutablePath: "/opt/homebrew/Cellar/looper/0.2.1/bin/looper",
		HTTPClient: newTestHTTPClient(func(req *http.Request) (*http.Response, error) {
			t.Fatalf("unexpected request before Homebrew refusal: %q", req.URL.String())
			return nil, nil
		}),
	})

	exitCode := app.Run(context.Background(), []string{"upgrade", "--cli"})
	if exitCode != 1 {
		t.Fatalf("Run([upgrade --cli]) exit code = %d, want 1", exitCode)
	}
	if !strings.Contains(stderr.String(), "brew upgrade looper") {
		t.Fatalf("stderr = %q, want brew guidance", stderr.String())
	}
}

func TestUpgradeCLIRefusesHomebrewSymlinkWithGuidance(t *testing.T) {
	t.Parallel()

	homebrewRoot := t.TempDir()
	targetPath := filepath.Join(homebrewRoot, "usr", "local", "Cellar", "looper", "0.2.1", "bin", "looper")
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
		t.Fatalf("os.MkdirAll(target dir) error = %v", err)
	}
	if err := os.WriteFile(targetPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("os.WriteFile(target) error = %v", err)
	}

	symlinkPath := filepath.Join(homebrewRoot, "usr", "local", "bin", "looper")
	if err := os.MkdirAll(filepath.Dir(symlinkPath), 0o755); err != nil {
		t.Fatalf("os.MkdirAll(symlink dir) error = %v", err)
	}
	if err := os.Symlink(targetPath, symlinkPath); err != nil {
		t.Skipf("os.Symlink() unavailable: %v", err)
	}

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := New(Deps{
		Stdout:         stdout,
		Stderr:         stderr,
		ExecutablePath: symlinkPath,
		HTTPClient: newTestHTTPClient(func(req *http.Request) (*http.Response, error) {
			t.Fatalf("unexpected request before Homebrew refusal: %q", req.URL.String())
			return nil, nil
		}),
	})

	exitCode := app.Run(context.Background(), []string{"upgrade", "--cli"})
	if exitCode != 1 {
		t.Fatalf("Run([upgrade --cli]) exit code = %d, want 1", exitCode)
	}
	if !strings.Contains(stderr.String(), "brew upgrade looper") {
		t.Fatalf("stderr = %q, want brew guidance", stderr.String())
	}
}

func TestUpgradeCLIPreflightsInstallPathBeforeDownload(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	blockingPath := filepath.Join(root, ".looper")
	if err := os.WriteFile(blockingPath, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("WriteFile(blockingPath) error = %v", err)
	}
	execPath := filepath.Join(root, ".looper", "bin", "looper")

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := New(Deps{
		Stdout:         stdout,
		Stderr:         stderr,
		ExecutablePath: execPath,
		HTTPClient: newTestHTTPClient(func(req *http.Request) (*http.Response, error) {
			switch req.URL.String() {
			case "https://api.github.com/repos/nexu-io/looper/releases/latest":
				return jsonResponse(t, http.StatusOK, `{"tag_name":"v0.3.0","assets":[{"name":"looper-darwin-arm64","browser_download_url":"https://example.invalid/looper-darwin-arm64"},{"name":"looper-darwin-arm64.sha256","browser_download_url":"https://example.invalid/looper-darwin-arm64.sha256"}]}`), nil
			default:
				t.Fatalf("unexpected request URL %q", req.URL.String())
				return nil, nil
			}
		}),
	})

	exitCode := app.Run(context.Background(), []string{"upgrade", "--cli"})
	if exitCode != 1 {
		t.Fatalf("Run([upgrade --cli]) exit code = %d, want 1", exitCode)
	}
	if !strings.Contains(stderr.String(), "install location is not writable") {
		t.Fatalf("stderr = %q, want writable guidance", stderr.String())
	}
	if strings.Contains(stderr.String(), "download") {
		t.Fatalf("stderr = %q, did not expect download failure", stderr.String())
	}
}

func TestUpgradeCLIPrintsDownloadProgressToStderr(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	execPath := filepath.Join(root, ".looper", "bin", "looper")
	if err := os.MkdirAll(filepath.Dir(execPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(exec dir) error = %v", err)
	}
	if err := os.WriteFile(execPath, []byte("old looper"), 0o755); err != nil {
		t.Fatalf("WriteFile(execPath) error = %v", err)
	}

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	binary := []byte("new looper")
	checksum := sha256.Sum256(binary)
	checksumText := hex.EncodeToString(checksum[:]) + "  looper-darwin-arm64\n"
	app := New(Deps{
		Stdout:         stdout,
		Stderr:         stderr,
		Platform:       "darwin",
		Arch:           "arm64",
		ExecutablePath: execPath,
		HTTPClient: newTestHTTPClient(func(req *http.Request) (*http.Response, error) {
			switch req.URL.String() {
			case "https://api.github.com/repos/nexu-io/looper/releases/latest":
				return jsonResponse(t, http.StatusOK, `{"tag_name":"v999.0.0","assets":[{"name":"looper-darwin-arm64","browser_download_url":"https://example.invalid/looper-darwin-arm64"},{"name":"looper-darwin-arm64.sha256","browser_download_url":"https://example.invalid/looper-darwin-arm64.sha256"}]}`), nil
			case "https://example.invalid/looper-darwin-arm64":
				return binaryResponse(t, http.StatusOK, binary), nil
			case "https://example.invalid/looper-darwin-arm64.sha256":
				return textResponse(t, http.StatusOK, checksumText), nil
			default:
				t.Fatalf("unexpected request URL %q", req.URL.String())
				return nil, nil
			}
		}),
	})

	exitCode := app.Run(context.Background(), []string{"upgrade", "--cli"})
	if exitCode != 0 {
		t.Fatalf("Run([upgrade --cli]) exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if !strings.Contains(stderr.String(), "Downloading looper-darwin-arm64: 10 B / 10 B (100%)") {
		t.Fatalf("stderr = %q, want CLI download progress", stderr.String())
	}
	if strings.Contains(stdout.String(), "Downloading looper-darwin-arm64") {
		t.Fatalf("stdout = %q, did not expect download progress", stdout.String())
	}
}

func TestDetectCLIInstallSourceTreatsInstallerSelectedUserBinAsRelease(t *testing.T) {
	homeDir, err := os.UserHomeDir()
	if err != nil || homeDir == "" {
		t.Skipf("cannot resolve user home directory: %v", err)
	}
	userBin := filepath.Join(homeDir, "bin")
	t.Setenv("PATH", userBin)

	got := detectCLIInstallSource(filepath.Join(userBin, "looper"))
	if got != cliInstallSourceRelease {
		t.Fatalf("detectCLIInstallSource(user PATH bin) = %q, want %q", got, cliInstallSourceRelease)
	}
}

func TestDetectCLIInstallSourceTreatsGoBinAsDevBeforeUserBinRelease(t *testing.T) {
	homeDir, err := os.UserHomeDir()
	if err != nil || homeDir == "" {
		t.Skipf("cannot resolve user home directory: %v", err)
	}
	goBin := filepath.Join(homeDir, "go", "bin")
	t.Setenv("PATH", goBin)

	got := detectCLIInstallSource(filepath.Join(goBin, "looper"))
	if got != cliInstallSourceDev {
		t.Fatalf("detectCLIInstallSource(go PATH bin) = %q, want %q", got, cliInstallSourceDev)
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
			case "https://api.github.com/repos/nexu-io/looper/releases/latest":
				return jsonResponse(t, http.StatusOK, `{"tag_name":"v0.3.0","assets":[{"name":"looperd-darwin-arm64","browser_download_url":"https://example.invalid/looperd-darwin-arm64"},{"name":"looperd-darwin-arm64.sha256","browser_download_url":"https://example.invalid/looperd-darwin-arm64.sha256"}]}`), nil
			case "https://api.github.com/repos/nexu-io/looper/releases/tags/v0.3.0":
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
	if !strings.Contains(stderr.String(), "Downloading looperd-darwin-arm64: 4 B / 4 B (100%)") {
		t.Fatalf("Run([upgrade --daemon]) stderr = %q, want daemon download progress", stderr.String())
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
			case "https://api.github.com/repos/nexu-io/looper/releases/latest":
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
			case "https://api.github.com/repos/nexu-io/looper/releases/latest":
				return jsonResponse(t, http.StatusOK, `{"tag_name":"v0.4.0","assets":[{"name":"looperd-darwin-arm64","browser_download_url":"https://example.invalid/looperd-darwin-arm64"},{"name":"looperd-darwin-arm64.sha256","browser_download_url":"https://example.invalid/looperd-darwin-arm64.sha256"}]}`), nil
			case "https://api.github.com/repos/nexu-io/looper/releases/tags/v0.4.0":
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
	if !strings.Contains(stderr.String(), "Downloading looperd-darwin-arm64: 4 B / 4 B (100%)") {
		t.Fatalf("Run([upgrade --daemon]) stderr = %q, want daemon download progress", stderr.String())
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
			case "https://api.github.com/repos/nexu-io/looper/releases/latest":
				latestCalls += 1
				if latestCalls == 1 {
					return jsonResponse(t, http.StatusOK, `{"tag_name":"v0.2.0","assets":[{"name":"looperd-darwin-arm64","browser_download_url":"https://example.invalid/looperd-darwin-arm64-v0.2.0"},{"name":"looperd-darwin-arm64.sha256","browser_download_url":"https://example.invalid/looperd-darwin-arm64-v0.2.0.sha256"}]}`), nil
				}
				return jsonResponse(t, http.StatusOK, `{"tag_name":"v0.3.0","assets":[{"name":"looperd-darwin-arm64","browser_download_url":"https://example.invalid/looperd-darwin-arm64-v0.3.0"},{"name":"looperd-darwin-arm64.sha256","browser_download_url":"https://example.invalid/looperd-darwin-arm64-v0.3.0.sha256"}]}`), nil
			case "https://api.github.com/repos/nexu-io/looper/releases/tags/v0.3.0":
				return jsonResponse(t, http.StatusOK, `{"tag_name":"v0.3.0","assets":[{"name":"looperd-darwin-arm64","browser_download_url":"https://example.invalid/looperd-darwin-arm64-v0.3.0"},{"name":"looperd-darwin-arm64.sha256","browser_download_url":"https://example.invalid/looperd-darwin-arm64-v0.3.0.sha256"}]}`), nil
			case "https://api.github.com/repos/nexu-io/looper/releases/tags/v0.2.0":
				return jsonResponse(t, http.StatusOK, `{"tag_name":"v0.2.0","assets":[{"name":"looperd-darwin-arm64","browser_download_url":"https://example.invalid/looperd-darwin-arm64-v0.2.0"},{"name":"looperd-darwin-arm64.sha256","browser_download_url":"https://example.invalid/looperd-darwin-arm64-v0.2.0.sha256"}]}`), nil
			case "https://example.invalid/looperd-darwin-arm64-v0.2.0":
				return binaryResponse(t, http.StatusOK, oldBinary), nil
			case "https://example.invalid/looperd-darwin-arm64-v0.2.0.sha256":
				return textResponse(t, http.StatusOK, hex.EncodeToString(oldChecksum[:])+"  looperd-darwin-arm64\n"), nil
			case "https://example.invalid/looperd-darwin-arm64-v0.3.0":
				return binaryResponse(t, http.StatusOK, newBinary), nil
			case "https://example.invalid/looperd-darwin-arm64-v0.3.0.sha256":
				return textResponse(t, http.StatusOK, hex.EncodeToString(newChecksum[:])+"  looperd-darwin-arm64\n"), nil
			case "http://daemon.test/api/v1/status", "http://127.0.0.1:17310/api/v1/status":
				mu.Lock()
				defer mu.Unlock()
				if runningPID == 0 {
					return nil, fmt.Errorf("daemon offline")
				}
				return jsonResponse(t, http.StatusOK, fmt.Sprintf(`{"ok":true,"requestId":"req_status","data":{"service":{"version":%q,"binary":{"name":"looperd"}}}}`, runningVersion)), nil
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
