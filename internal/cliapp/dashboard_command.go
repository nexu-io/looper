package cliapp

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/url"
	"os/exec"
	"runtime"
	"strings"

	"github.com/nexu-io/looper/internal/config"
	"github.com/spf13/cobra"
)

func (r *commandRuntime) dashboard(cmd *cobra.Command, args []string) error {
	_ = args
	ctx := cmd.Context()

	loaded, err := r.loadConfig()
	if err != nil {
		return err
	}

	browserBase, err := resolveDashboardBrowserBaseURL(loaded)
	if err != nil {
		return err
	}
	if err := allowDashboardOpen(browserBase, loaded.Config.Server.AuthMode); err != nil {
		return err
	}

	client := r.dashboardAPIClient(loaded)
	if err := r.ensureDashboardDaemonReady(ctx, cmd, loaded, client); err != nil {
		return err
	}

	dashboardURL := strings.TrimRight(browserBase, "/") + "/dashboard/"
	if loaded.Config.Server.AuthMode == config.AuthModeLocalToken {
		var mint struct {
			Code      string `json:"code"`
			ExpiresAt string `json:"expiresAt"`
		}
		if err := client.Post(ctx, "/api/v1/dashboard/bootstrap/code", map[string]any{}, &mint); err != nil {
			return fmt.Errorf("mint dashboard bootstrap code: %w", err)
		}
		if strings.TrimSpace(mint.Code) == "" {
			return fmt.Errorf("mint dashboard bootstrap code: empty code returned")
		}
		dashboardURL += "?code=" + url.QueryEscape(mint.Code)
	}

	// Print URL first so operators always have it (SSH/headless, opener failure).
	if _, err := fmt.Fprintln(cmd.OutOrStdout(), dashboardURL); err != nil {
		return err
	}

	if getBoolFlag(cmd, "no-open") {
		return nil
	}

	if err := r.openURL(dashboardURL); err != nil {
		return fmt.Errorf("open browser: %w", err)
	}
	return nil
}

func (r *commandRuntime) ensureDashboardDaemonReady(ctx context.Context, cmd *cobra.Command, loaded config.LoadedFileConfig, client *DaemonAPIClient) error {
	probe, err := r.probeDaemonStatus(ctx, client)
	if err != nil {
		return err
	}
	if probe.isLooperd {
		return nil
	}

	// Remote baseURL: never start a daemon; only probe.
	if usesConfiguredBaseURL(loaded) {
		return fmt.Errorf("looperd is not reachable at %s; start the remote daemon before opening the dashboard", client.baseURL)
	}

	// Local target: start then re-verify. Suppress daemon lifecycle chatter so
	// `looper dashboard --no-open` (and headless/SSH scripts) keep stdout as
	// the dashboard URL only.
	originalOut := cmd.OutOrStdout()
	cmd.SetOut(io.Discard)
	err = r.daemonStart(cmd, nil)
	cmd.SetOut(originalOut)
	if err != nil {
		return err
	}

	probe, err = r.probeDaemonStatus(ctx, client)
	if err != nil {
		return err
	}
	if !probe.isLooperd {
		return fmt.Errorf("looperd is not ready at %s after start", client.baseURL)
	}
	return nil
}

func usesConfiguredBaseURL(loaded config.LoadedFileConfig) bool {
	return loaded.Config.Server.BaseURL != nil && strings.TrimSpace(*loaded.Config.Server.BaseURL) != ""
}

// dashboardAPIClient returns an API client for dashboard readiness/mint.
// When server.host is a wildcard bind (0.0.0.0 / ::), probe via loopback —
// matching browserHostForDashboard — not the unroutable wildcard address.
func (r *commandRuntime) dashboardAPIClient(loaded config.LoadedFileConfig) *DaemonAPIClient {
	if usesConfiguredBaseURL(loaded) {
		return r.apiClientFromLoaded(loaded)
	}
	host := browserHostForDashboard(loaded.Config.Server.Host)
	baseURL := dashboardHTTPBaseURL(host, loaded.Config.Server.Port)
	return r.newAPIClientForLoaded(loaded, baseURL)
}

func resolveDashboardBrowserBaseURL(loaded config.LoadedFileConfig) (string, error) {
	if usesConfiguredBaseURL(loaded) {
		base := strings.TrimRight(strings.TrimSpace(*loaded.Config.Server.BaseURL), "/")
		parsed, err := url.Parse(base)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			return "", fmt.Errorf("invalid server.baseUrl %q", base)
		}
		// SPA is rooted at /dashboard/ with root-relative /api/v1/... calls.
		// A path prefix (e.g. https://host/base) would open /base/dashboard/ while
		// assets and API still target site root, so dashboard would break after mint.
		if path := strings.Trim(parsed.Path, "/"); path != "" {
			return "", fmt.Errorf("server.baseUrl must not include a path prefix for the dashboard (got %s); the SPA is served at /dashboard/ with root-relative API paths", base)
		}
		host := parsed.Hostname()
		if !isLoopbackHostname(host) && !strings.EqualFold(parsed.Scheme, "https") {
			return "", fmt.Errorf("server.baseUrl must use https for non-loopback hosts (got %s)", base)
		}
		return base, nil
	}

	host := browserHostForDashboard(loaded.Config.Server.Host)
	return dashboardHTTPBaseURL(host, loaded.Config.Server.Port), nil
}

// dashboardHTTPBaseURL builds an http:// URL authority with IPv6 hosts bracketed
// (net.JoinHostPort), matching how looperd listens.
func dashboardHTTPBaseURL(host string, port int) string {
	return "http://" + net.JoinHostPort(host, fmt.Sprintf("%d", port))
}

func browserHostForDashboard(host string) string {
	h := strings.TrimSpace(host)
	switch h {
	case "0.0.0.0", "::", "[::]":
		return "127.0.0.1"
	default:
		return h
	}
}

func allowDashboardOpen(browserBase string, authMode config.AuthMode) error {
	parsed, err := url.Parse(browserBase)
	if err != nil || parsed.Host == "" {
		return fmt.Errorf("invalid dashboard base URL %q", browserBase)
	}
	host := parsed.Hostname()
	if isLoopbackHostname(host) {
		return nil
	}
	if authMode == config.AuthModeLocalToken && strings.EqualFold(parsed.Scheme, "https") {
		return nil
	}
	return fmt.Errorf("opening dashboard on non-loopback host %q requires server.authMode=local-token and an https server.baseUrl", host)
}

func isLoopbackHostname(host string) bool {
	host = strings.Trim(strings.TrimSpace(host), "[]")
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (r *commandRuntime) openURL(target string) error {
	if r.app != nil && r.app.deps.OpenURL != nil {
		return r.app.deps.OpenURL(target)
	}
	return defaultOpenURL(target)
}

func defaultOpenURL(target string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", target)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	default:
		cmd = exec.Command("xdg-open", target)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	// Do not wait for the browser process; detach best-effort.
	_ = cmd.Process.Release()
	return nil
}
