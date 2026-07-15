package cliapp

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	pkgapi "github.com/nexu-io/looper/pkg/api"
)

func TestDashboardNoOpenPrintsURL(t *testing.T) {
	t.Parallel()

	var opened atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/status":
			writeEnvelope(t, w, pkgapi.Success("req", map[string]any{
				"service": map[string]any{
					"version": "1.0.0",
					"binary":  map[string]any{"name": "looperd", "path": "/usr/bin/looperd"},
				},
			}))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	configPath := writeCLIConfig(t, server.URL, "")
	stdout := &strings.Builder{}
	stderr := &strings.Builder{}
	app := New(Deps{
		Stdout: stdout,
		Stderr: stderr,
		OpenURL: func(u string) error {
			opened.Add(1)
			return nil
		},
	})
	exitCode := app.Run(context.Background(), []string{"dashboard", "--no-open", "--config", configPath})
	if exitCode != 0 {
		t.Fatalf("exit = %d stderr=%q", exitCode, stderr.String())
	}
	if opened.Load() != 0 {
		t.Fatalf("OpenURL called with --no-open")
	}
	got := strings.TrimSpace(stdout.String())
	want := strings.TrimRight(server.URL, "/") + "/dashboard/"
	if got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}

func TestDashboardMintsBootstrapCode(t *testing.T) {
	t.Parallel()

	var openedURL string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/status":
			writeEnvelope(t, w, pkgapi.Success("req", map[string]any{
				"service": map[string]any{
					"version": "1.0.0",
					"binary":  map[string]any{"name": "looperd", "path": "/usr/bin/looperd"},
				},
			}))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/dashboard/bootstrap/code":
			if got := r.Header.Get("Authorization"); got != "Bearer secret-token" {
				t.Fatalf("Authorization = %q", got)
			}
			writeEnvelope(t, w, pkgapi.Success("req", map[string]any{
				"code":      "abc123",
				"expiresAt": "2026-07-15T12:00:00.000Z",
			}))
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	configPath := writeCLIConfig(t, server.URL, "secret-token")
	stdout := &strings.Builder{}
	stderr := &strings.Builder{}
	app := New(Deps{
		Stdout: stdout,
		Stderr: stderr,
		OpenURL: func(u string) error {
			openedURL = u
			return nil
		},
	})
	exitCode := app.Run(context.Background(), []string{"dashboard", "--config", configPath})
	if exitCode != 0 {
		t.Fatalf("exit = %d stderr=%q", exitCode, stderr.String())
	}
	printed := strings.TrimSpace(stdout.String())
	wantPrefix := strings.TrimRight(server.URL, "/") + "/dashboard/?code="
	if !strings.HasPrefix(printed, wantPrefix) {
		t.Fatalf("printed URL = %q, want prefix %q", printed, wantPrefix)
	}
	if openedURL != printed {
		t.Fatalf("opened %q, printed %q", openedURL, printed)
	}
	parsed, err := url.Parse(printed)
	if err != nil {
		t.Fatalf("parse URL: %v", err)
	}
	if parsed.Query().Get("code") != "abc123" {
		t.Fatalf("code = %q", parsed.Query().Get("code"))
	}
}

func TestDashboardRemoteDownDoesNotStart(t *testing.T) {
	t.Parallel()

	// Unreachable baseURL with configured remote — must not attempt local start.
	configPath := writeCLIConfig(t, "http://127.0.0.1:1", "")
	stdout := &strings.Builder{}
	stderr := &strings.Builder{}
	var startAttempted atomic.Int32
	app := New(Deps{
		Stdout: stdout,
		Stderr: stderr,
		SpawnDetached: func(command string, args []string, cwd string, env []string) (int, error) {
			startAttempted.Add(1)
			return 0, nil
		},
		OpenURL: func(string) error { return nil },
	})
	exitCode := app.Run(context.Background(), []string{"dashboard", "--no-open", "--config", configPath})
	if exitCode == 0 {
		t.Fatalf("expected non-zero exit when remote daemon down")
	}
	if startAttempted.Load() != 0 {
		t.Fatalf("must not spawn daemon for remote baseURL")
	}
	if !strings.Contains(stderr.String(), "not reachable") && !strings.Contains(stderr.String(), "remote") {
		// Error is printed as "looper: ..."
		if !strings.Contains(stderr.String(), "looperd is not reachable") {
			t.Fatalf("stderr = %q, want unreachable message", stderr.String())
		}
	}
}

func TestDashboardColdStartStdoutIsURLOnly(t *testing.T) {
	t.Parallel()

	// Local host/port without baseURL: daemon is down → dashboard starts it, but
	// stdout must remain a single URL line (no "Started looperd" / PID chatter).
	homeDir := t.TempDir()
	managedPath := filepath.Join(homeDir, ".looper", "bin", "looperd")
	configPath := writeDaemonCLIConfigForBindEndpoint(t, "http://127.0.0.1:17310", nil)
	var daemonStarted atomic.Bool
	stdout := &strings.Builder{}
	stderr := &strings.Builder{}

	app := New(Deps{
		Stdout:  stdout,
		Stderr:  stderr,
		HomeDir: homeDir,
		HTTPClient: newTestHTTPClient(func(req *http.Request) (*http.Response, error) {
			switch req.URL.Path {
			case "/api/v1/status", "/api/v1/healthz":
				if !daemonStarted.Load() {
					return nil, fmt.Errorf("daemon offline")
				}
				return jsonResponse(t, http.StatusOK, `{"ok":true,"requestId":"req_status","data":{"service":{"healthy":true,"binary":{"name":"looperd","path":"/usr/bin/looperd"},"version":"1.0.0"}}}`), nil
			default:
				t.Fatalf("unexpected request %s %s", req.Method, req.URL.String())
				return nil, nil
			}
		}),
		RunCommand: func(ctx context.Context, command string, args []string, timeout time.Duration) (commandExecutionResult, error) {
			_ = ctx
			_ = timeout
			if command == managedPath && strings.Join(args, " ") == "--version" {
				return commandExecutionResult{ExitCode: 0, Stdout: "1.0.0\n"}, nil
			}
			if command == "ps" && len(args) >= 2 {
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
		ReadFile: func(path string) ([]byte, error) {
			return nil, os.ErrNotExist
		},
		Sleep:   func(time.Duration) {},
		OpenURL: func(string) error { return nil },
	})

	exitCode := app.Run(context.Background(), []string{"dashboard", "--no-open", "--config", configPath})
	if exitCode != 0 {
		t.Fatalf("exit = %d stderr=%q", exitCode, stderr.String())
	}
	if !daemonStarted.Load() {
		t.Fatal("expected cold-start SpawnDetached")
	}
	got := strings.TrimSpace(stdout.String())
	want := "http://127.0.0.1:17310/dashboard/"
	if got != want {
		t.Fatalf("stdout = %q, want URL-only %q", stdout.String(), want)
	}
	for _, chatter := range []string{"Started looperd", "PID file:", "State file:", "Startup log:"} {
		if strings.Contains(stdout.String(), chatter) {
			t.Fatalf("stdout contains daemon-start chatter %q: %q", chatter, stdout.String())
		}
	}
}

func TestDashboardNonLoopbackPolicy(t *testing.T) {
	t.Parallel()

	configPath := writeEditableCLIConfigWithPayload(t, map[string]any{
		"server": map[string]any{
			"baseUrl":  "http://example.com:8443",
			"authMode": "none",
		},
	})
	stdout := &strings.Builder{}
	stderr := &strings.Builder{}
	app := New(Deps{Stdout: stdout, Stderr: stderr, OpenURL: func(string) error { return nil }})
	exitCode := app.Run(context.Background(), []string{"dashboard", "--no-open", "--config", configPath})
	if exitCode == 0 {
		t.Fatalf("expected failure for non-loopback http without local-token")
	}
	if !strings.Contains(stderr.String(), "https") && !strings.Contains(stderr.String(), "non-loopback") {
		t.Fatalf("stderr = %q, want policy error", stderr.String())
	}
}

func TestResolveDashboardBrowserBaseURLMapsWildcard(t *testing.T) {
	t.Parallel()

	loaded := config.LoadedFileConfig{
		Config: config.Config{
			Server: config.ServerConfig{
				Host: "0.0.0.0",
				Port: 17310,
			},
		},
	}
	got, err := resolveDashboardBrowserBaseURL(loaded)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != "http://127.0.0.1:17310" {
		t.Fatalf("got %q", got)
	}
}

func TestResolveDashboardBrowserBaseURLBracketsIPv6(t *testing.T) {
	t.Parallel()

	loaded := config.LoadedFileConfig{
		Config: config.Config{
			Server: config.ServerConfig{
				Host: "::1",
				Port: 17310,
			},
		},
	}
	got, err := resolveDashboardBrowserBaseURL(loaded)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != "http://[::1]:17310" {
		t.Fatalf("got %q, want http://[::1]:17310", got)
	}
}

func TestDashboardHTTPBaseURLBracketsIPv6(t *testing.T) {
	t.Parallel()

	if got := dashboardHTTPBaseURL("::1", 17310); got != "http://[::1]:17310" {
		t.Fatalf("got %q", got)
	}
	if got := dashboardHTTPBaseURL("127.0.0.1", 17310); got != "http://127.0.0.1:17310" {
		t.Fatalf("got %q", got)
	}
}

// Cold-start via daemonStart rebuilds its probe client with localAPIClientFromLoaded;
// that path must bracket IPv6 the same way as dashboardAPIClient / dashboardHTTPBaseURL.
func TestLocalAPIClientFromLoadedBracketsIPv6(t *testing.T) {
	t.Parallel()

	r := newCommandRuntime(New(Deps{}), nil)
	loaded := config.LoadedFileConfig{
		Config: config.Config{
			Server: config.ServerConfig{
				Host: "::1",
				Port: 17310,
			},
		},
	}
	client := r.localAPIClientFromLoaded(loaded)
	if client == nil {
		t.Fatal("expected client")
	}
	if client.baseURL != "http://[::1]:17310" {
		t.Fatalf("localAPIClientFromLoaded baseURL = %q, want http://[::1]:17310", client.baseURL)
	}
	// apiClientFromLoaded without baseUrl must match (used by non-dashboard paths).
	apiClient := r.apiClientFromLoaded(loaded)
	if apiClient.baseURL != "http://[::1]:17310" {
		t.Fatalf("apiClientFromLoaded baseURL = %q, want http://[::1]:17310", apiClient.baseURL)
	}
}

func TestDashboardWildcardHostProbesLoopback(t *testing.T) {
	t.Parallel()

	var seenHost string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenHost = r.Host
		switch r.URL.Path {
		case "/api/v1/status":
			writeEnvelope(t, w, pkgapi.Success("req", map[string]any{
				"service": map[string]any{
					"version": "1.0.0",
					"binary":  map[string]any{"name": "looperd", "path": "/usr/bin/looperd"},
				},
			}))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	// Parse the test server's port; configure bind host as 0.0.0.0 without baseUrl
	// so dashboard must map probe client to 127.0.0.1 (not the wildcard).
	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	port := u.Port()
	if port == "" {
		t.Fatal("missing server port")
	}

	// Point host/port at the real listener via 0.0.0.0 + port, no baseUrl.
	// The test server listens on 127.0.0.1; probing 0.0.0.0 may fail or be wrong.
	// dashboardAPIClient must rewrite to 127.0.0.1.
	configPath := writeEditableCLIConfigWithPayload(t, map[string]any{
		"server": map[string]any{
			"host":     "0.0.0.0",
			"port":     mustAtoi(t, port),
			"authMode": "none",
		},
		"notifications": map[string]any{
			"osascript": map[string]any{"enabled": false},
		},
	})

	// The httptest server is on 127.0.0.1:port. Config says 0.0.0.0:port.
	// Client must dial 127.0.0.1:port.
	stdout := &strings.Builder{}
	stderr := &strings.Builder{}
	app := New(Deps{
		Stdout:  stdout,
		Stderr:  stderr,
		OpenURL: func(string) error { return nil },
	})
	exitCode := app.Run(context.Background(), []string{"dashboard", "--no-open", "--config", configPath})
	if exitCode != 0 {
		t.Fatalf("exit = %d stderr=%q", exitCode, stderr.String())
	}
	if !strings.HasPrefix(seenHost, "127.0.0.1:") {
		t.Fatalf("probe Host = %q, want 127.0.0.1:<port>", seenHost)
	}
	wantURL := "http://127.0.0.1:" + port + "/dashboard/"
	if strings.TrimSpace(stdout.String()) != wantURL {
		t.Fatalf("stdout = %q, want %q", stdout.String(), wantURL)
	}
}

func mustAtoi(t *testing.T, s string) int {
	t.Helper()
	var n int
	for _, c := range s {
		if c < '0' || c > '9' {
			t.Fatalf("not an int: %q", s)
		}
		n = n*10 + int(c-'0')
	}
	return n
}

func TestDashboardOpenerFailureStillPrintsURL(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeEnvelope(t, w, pkgapi.Success("req", map[string]any{
			"service": map[string]any{
				"version": "1.0.0",
				"binary":  map[string]any{"name": "looperd", "path": "/usr/bin/looperd"},
			},
		}))
	}))
	defer server.Close()

	configPath := writeCLIConfig(t, server.URL, "")
	stdout := &strings.Builder{}
	stderr := &strings.Builder{}
	app := New(Deps{
		Stdout: stdout,
		Stderr: stderr,
		OpenURL: func(string) error {
			return errOpenFailed
		},
	})
	exitCode := app.Run(context.Background(), []string{"dashboard", "--config", configPath})
	if exitCode == 0 {
		t.Fatalf("expected non-zero exit on opener failure")
	}
	want := strings.TrimRight(server.URL, "/") + "/dashboard/"
	if strings.TrimSpace(stdout.String()) != want {
		t.Fatalf("stdout = %q, want URL printed first %q", stdout.String(), want)
	}
}

var errOpenFailed = &openFailedError{}

type openFailedError struct{}

func (e *openFailedError) Error() string { return "open failed" }
