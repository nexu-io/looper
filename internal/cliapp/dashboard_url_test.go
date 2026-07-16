package cliapp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/config"
	pkgapi "github.com/nexu-io/looper/pkg/api"
)

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

func TestResolveDashboardBrowserBaseURLRejectsPathPrefix(t *testing.T) {
	t.Parallel()

	base := "https://daemon.example.test/base"
	loaded := config.LoadedFileConfig{
		Config: config.Config{
			Server: config.ServerConfig{
				BaseURL:  &base,
				AuthMode: config.AuthModeLocalToken,
			},
		},
	}
	_, err := resolveDashboardBrowserBaseURL(loaded)
	if err == nil {
		t.Fatal("expected error for pathful server.baseUrl")
	}
	if !strings.Contains(err.Error(), "path prefix") {
		t.Fatalf("error = %q, want path prefix rejection", err)
	}
}

func TestResolveDashboardBrowserBaseURLAllowsOriginOnlyBaseURL(t *testing.T) {
	t.Parallel()

	base := "https://daemon.example.test"
	loaded := config.LoadedFileConfig{
		Config: config.Config{
			Server: config.ServerConfig{
				BaseURL:  &base,
				AuthMode: config.AuthModeLocalToken,
			},
		},
	}
	got, err := resolveDashboardBrowserBaseURL(loaded)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != "https://daemon.example.test" {
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

// Wildcard binds are not valid request Host values under the browser Host guard.
// CLI clients (daemon status, config, etc.) must dial loopback like dashboardAPIClient.
func TestAPIClientsMapWildcardBindToLoopback(t *testing.T) {
	t.Parallel()

	r := newCommandRuntime(New(Deps{}), nil)
	for _, host := range []string{"0.0.0.0", "::", "[::]"} {
		loaded := config.LoadedFileConfig{
			Config: config.Config{
				Server: config.ServerConfig{
					Host: host,
					Port: 17310,
				},
			},
		}
		want := "http://127.0.0.1:17310"
		if got := r.localAPIClientFromLoaded(loaded).baseURL; got != want {
			t.Fatalf("localAPIClientFromLoaded(%q) baseURL = %q, want %q", host, got, want)
		}
		if got := r.apiClientFromLoaded(loaded).baseURL; got != want {
			t.Fatalf("apiClientFromLoaded(%q) baseURL = %q, want %q", host, got, want)
		}
		if got := r.dashboardAPIClient(loaded).baseURL; got != want {
			t.Fatalf("dashboardAPIClient(%q) baseURL = %q, want %q", host, got, want)
		}
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
