package cliapp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResolveLooperdTarget(t *testing.T) {
	t.Parallel()

	target, err := resolveLooperdTarget("darwin", "arm64")
	if err != nil {
		t.Fatalf("resolveLooperdTarget(darwin, arm64) error = %v", err)
	}
	if target != "darwin-arm64" {
		t.Fatalf("resolveLooperdTarget(darwin, arm64) = %q, want %q", target, "darwin-arm64")
	}

	_, err = resolveLooperdTarget("linux", "amd64")
	if err == nil || err.Error() != "Unsupported platform/arch for looperd install: linux-amd64. Supported targets: darwin-arm64" {
		t.Fatalf("resolveLooperdTarget(linux, amd64) error = %v", err)
	}

	for _, arch := range []string{"amd64", "x64"} {
		_, err = resolveLooperdTarget("darwin", arch)
		if err == nil {
			t.Fatalf("resolveLooperdTarget(darwin, %s) error = nil, want unsupported", arch)
		}
	}
}

func TestInstallManagedDaemonInstallsBinary(t *testing.T) {
	t.Parallel()

	homeDir := t.TempDir()
	binary := []byte{1, 2, 3, 4}
	checksum := sha256.Sum256(binary)
	checksumText := hex.EncodeToString(checksum[:]) + "  looperd-darwin-arm64\n"

	app := New(Deps{
		HTTPClient: newTestHTTPClient(func(req *http.Request) (*http.Response, error) {
			switch req.URL.String() {
			case "https://api.github.com/repos/nexu-io/looper/releases/latest":
				return jsonResponse(t, http.StatusOK, `{"assets":[{"name":"looperd-darwin-arm64","browser_download_url":"https://example.invalid/looperd-darwin-arm64"},{"name":"looperd-darwin-arm64.sha256","browser_download_url":"https://example.invalid/looperd-darwin-arm64.sha256"}]}`), nil
			case "https://example.invalid/looperd-darwin-arm64":
				return binaryResponse(t, http.StatusOK, binary), nil
			case "https://example.invalid/looperd-darwin-arm64.sha256":
				return textResponse(t, http.StatusOK, checksumText), nil
			default:
				t.Fatalf("unexpected request URL %q", req.URL.String())
				return nil, nil
			}
		}),
		HomeDir:  homeDir,
		Platform: "darwin",
		Arch:     "arm64",
	})

	runtime := newCommandRuntime(app, nil)
	result, err := runtime.installManagedDaemon(context.Background(), false, "", nil)
	if err != nil {
		t.Fatalf("installManagedDaemon() error = %v", err)
	}

	installPath := filepath.Join(homeDir, ".looper", "bin", "looperd")
	if result.Target != "darwin-arm64" {
		t.Fatalf("result.Target = %q, want %q", result.Target, "darwin-arm64")
	}
	if result.InstallPath != installPath {
		t.Fatalf("result.InstallPath = %q, want %q", result.InstallPath, installPath)
	}
	if result.DownloadedFrom == nil || *result.DownloadedFrom != "https://example.invalid/looperd-darwin-arm64" {
		t.Fatalf("result.DownloadedFrom = %#v, want asset URL", result.DownloadedFrom)
	}
	if result.Skipped {
		t.Fatalf("result.Skipped = true, want false")
	}

	installedBytes, err := os.ReadFile(installPath)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", installPath, err)
	}
	if !bytes.Equal(installedBytes, binary) {
		t.Fatalf("installed bytes = %v, want %v", installedBytes, binary)
	}
	if _, err := os.Stat(installPath + ".new"); !os.IsNotExist(err) {
		t.Fatalf("temp install file exists unexpectedly: %v", err)
	}
}

func TestInstallManagedDaemonSkipsExistingBinary(t *testing.T) {
	t.Parallel()

	homeDir := t.TempDir()
	installPath := filepath.Join(homeDir, ".looper", "bin", "looperd")
	if err := os.MkdirAll(filepath.Dir(installPath), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(installPath, []byte("existing"), 0o755); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	app := New(Deps{
		HTTPClient: newTestHTTPClient(func(req *http.Request) (*http.Response, error) {
			t.Fatalf("unexpected network request %q", req.URL.String())
			return nil, nil
		}),
		HomeDir:  homeDir,
		Platform: "darwin",
		Arch:     "arm64",
		RunCommand: func(ctx context.Context, command string, args []string, timeout time.Duration) (commandExecutionResult, error) {
			_ = ctx
			_ = timeout
			if command == installPath && strings.Join(args, " ") == "--version" {
				return commandExecutionResult{ExitCode: 0, Stdout: "1.2.3\n"}, nil
			}
			return commandExecutionResult{ExitCode: 1, Stderr: "not found"}, nil
		},
	})

	runtime := newCommandRuntime(app, nil)
	result, err := runtime.installManagedDaemon(context.Background(), false, "", nil)
	if err != nil {
		t.Fatalf("installManagedDaemon() error = %v", err)
	}
	if !result.Skipped {
		t.Fatalf("result.Skipped = false, want true")
	}
	if result.DownloadedFrom != nil {
		t.Fatalf("result.DownloadedFrom = %#v, want nil", result.DownloadedFrom)
	}
	if result.Target != "darwin-arm64" {
		t.Fatalf("result.Target = %q, want %q", result.Target, "darwin-arm64")
	}
}

func TestInstallManagedDaemonReportsNonExecutableExistingBinary(t *testing.T) {
	t.Parallel()

	homeDir := t.TempDir()
	installPath := filepath.Join(homeDir, ".looper", "bin", "looperd")
	if err := os.MkdirAll(filepath.Dir(installPath), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(installPath, []byte("existing"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	app := New(Deps{HomeDir: homeDir, Platform: "darwin", Arch: "arm64"})
	runtime := newCommandRuntime(app, nil)
	_, err := runtime.installManagedDaemon(context.Background(), false, "", nil)
	if err == nil {
		t.Fatalf("installManagedDaemon() error = nil, want permission error")
	}
	for _, want := range []string{"not executable", "chmod +x " + installPath, "looper daemon install --force"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want to contain %q", err.Error(), want)
		}
	}
}

func TestInstallManagedDaemonReportsInvalidExistingBinary(t *testing.T) {
	t.Parallel()

	homeDir := t.TempDir()
	installPath := filepath.Join(homeDir, ".looper", "bin", "looperd")
	if err := os.MkdirAll(filepath.Dir(installPath), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(installPath, []byte("corrupt"), 0o755); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	app := New(Deps{
		HomeDir:  homeDir,
		Platform: "darwin",
		Arch:     "arm64",
		RunCommand: func(ctx context.Context, command string, args []string, timeout time.Duration) (commandExecutionResult, error) {
			_ = ctx
			_ = timeout
			if command == installPath && strings.Join(args, " ") == "--version" {
				return commandExecutionResult{ExitCode: 1, Stderr: "exec format error"}, nil
			}
			return commandExecutionResult{ExitCode: 1, Stderr: "not found"}, nil
		},
	})
	runtime := newCommandRuntime(app, nil)
	_, err := runtime.installManagedDaemon(context.Background(), false, "", nil)
	if err == nil {
		t.Fatalf("installManagedDaemon() error = nil, want invalid binary error")
	}
	for _, want := range []string{"version check failed", "exec format error", "looper daemon install --force"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want to contain %q", err.Error(), want)
		}
	}
}

func TestDaemonInstallCommandPrintsHumanOutput(t *testing.T) {
	t.Parallel()

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	binary := []byte{9, 8, 7}
	checksum := sha256.Sum256(binary)
	checksumText := hex.EncodeToString(checksum[:]) + "  looperd-darwin-arm64\n"

	app := New(Deps{
		Stdout: stdout,
		Stderr: stderr,
		HTTPClient: newTestHTTPClient(func(req *http.Request) (*http.Response, error) {
			switch req.URL.String() {
			case "https://api.github.com/repos/nexu-io/looper/releases/latest":
				return jsonResponse(t, http.StatusOK, `{"assets":[{"name":"looperd-darwin-arm64","browser_download_url":"https://example.invalid/looperd-darwin-arm64"},{"name":"looperd-darwin-arm64.sha256","browser_download_url":"https://example.invalid/looperd-darwin-arm64.sha256"}]}`), nil
			case "https://example.invalid/looperd-darwin-arm64":
				return binaryResponse(t, http.StatusOK, binary), nil
			case "https://example.invalid/looperd-darwin-arm64.sha256":
				return textResponse(t, http.StatusOK, checksumText), nil
			default:
				t.Fatalf("unexpected request URL %q", req.URL.String())
				return nil, nil
			}
		}),
		HomeDir:  t.TempDir(),
		Platform: "darwin",
		Arch:     "arm64",
	})

	exitCode := app.Run(context.Background(), []string{"daemon", "install"})
	if exitCode != 0 {
		t.Fatalf("Run([daemon install]) exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if !strings.Contains(stderr.String(), "Downloading looperd-darwin-arm64: 3 B / 3 B (100%)") {
		t.Fatalf("Run([daemon install]) stderr = %q, want download progress", stderr.String())
	}
	if !strings.Contains(stdout.String(), "Installed looperd (darwin-arm64) to ") {
		t.Fatalf("Run([daemon install]) stdout = %q, want install confirmation", stdout.String())
	}
	if !strings.Contains(stdout.String(), "Downloaded from https://example.invalid/looperd-darwin-arm64") {
		t.Fatalf("Run([daemon install]) stdout = %q, want download URL", stdout.String())
	}
}

func TestDownloadBinaryProgressFallsBackWhenLengthUnknown(t *testing.T) {
	t.Parallel()

	stderr := &bytes.Buffer{}
	app := New(Deps{
		HTTPClient: newTestHTTPClient(func(req *http.Request) (*http.Response, error) {
			if req.URL.String() != "https://example.invalid/looperd-darwin-arm64" {
				t.Fatalf("unexpected request URL %q", req.URL.String())
			}
			return &http.Response{
				StatusCode:    http.StatusOK,
				Status:        http.StatusText(http.StatusOK),
				Header:        http.Header{"Content-Type": []string{"application/octet-stream"}},
				Body:          io.NopCloser(strings.NewReader("abcd")),
				ContentLength: -1,
			}, nil
		}),
	})
	runtime := newCommandRuntime(app, nil)

	data, err := runtime.downloadBinary(context.Background(), "https://example.invalid/looperd-darwin-arm64", "looperd-darwin-arm64", stderr)
	if err != nil {
		t.Fatalf("downloadBinary() error = %v", err)
	}
	if string(data) != "abcd" {
		t.Fatalf("downloadBinary() data = %q, want abcd", string(data))
	}
	if !strings.Contains(stderr.String(), "Downloading looperd-darwin-arm64: 4 B downloaded") {
		t.Fatalf("progress = %q, want unknown-size fallback", stderr.String())
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func newTestHTTPClient(fn roundTripFunc) *http.Client {
	return &http.Client{Transport: fn}
}

func jsonResponse(t *testing.T, status int, body string) *http.Response {
	t.Helper()
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func textResponse(t *testing.T, status int, body string) *http.Response {
	t.Helper()
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     http.Header{"Content-Type": []string{"text/plain"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func binaryResponse(t *testing.T, status int, body []byte) *http.Response {
	t.Helper()
	return &http.Response{
		StatusCode:    status,
		Status:        http.StatusText(status),
		Header:        http.Header{"Content-Type": []string{"application/octet-stream"}},
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
	}
}
