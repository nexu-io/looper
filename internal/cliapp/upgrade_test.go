package cliapp

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
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
			case "https://registry.npmjs.org/%40powerformer%2Flooper/latest":
				return jsonResponse(t, http.StatusOK, `{"version":"0.2.1"}`), nil
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
