package api

import (
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/nexu-io/looper/internal/config"
	pkgapi "github.com/nexu-io/looper/pkg/api"
)

// rewriteHTTPtestDefaultHost is enabled only by TestMain during `go test`.
// httptest.NewRequest uses the synthetic Host "example.com" for path-only URLs;
// when this flag is set, that Host is evaluated as the configured server
// authority so unit tests need not set Host on every request. Production
// binaries leave this false so a real Host: example.com is not rewritten.
var rewriteHTTPtestDefaultHost bool

// validateBrowserRequest enforces Host allowlisting and Origin matching for
// browser requests, including safe methods (GET/HEAD) that expose dashboard
// state.
//
// DNS rebinding under authMode=none: same-origin navigation to a rebound
// daemon URL may omit Origin entirely. Host is therefore always validated —
// never skipped solely because Origin is absent. Origin matching runs only when
// the header is present. Authorities come from server config (bind host/port,
// loopback aliases, optional server.baseUrl), not from the request Host alone.
//
// When Origin is absent (CLI / non-browser), Host must be allowlisted or a
// loopback authority. Loopback is accepted without requiring an exact config
// port match because the client already dialed the process; this still rejects
// attacker Hosts such as evil.example that DNS rebinding would present.
//
// Exception: non-browser external callbacks (see isNonBrowserCallbackPath)
// authenticate with their own shared secret (e.g. Feishu verification token).
// When Origin is absent, Host allowlisting is skipped for those paths so a
// public Host on server.host=0.0.0.0 without server.baseUrl still reaches the
// token-verified handler.
func validateBrowserRequest(r *http.Request, cfg config.Config) error {
	return validateBrowserRequestForPath(r, cfg, r.URL.Path)
}

func validateBrowserRequestForPath(r *http.Request, cfg config.Config, path string) error {
	host := effectiveRequestHost(r, cfg)
	if host == "" {
		return apiError{
			code:    pkgapi.ErrorCodeUnauthorized,
			status:  http.StatusForbidden,
			message: "Host header is required",
		}
	}

	allowed := allowedAuthorities(cfg)
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		if authorityAllowed(host, allowed) || isLoopbackAuthority(host) {
			return nil
		}
		// Token-verified inbound callbacks are not browser reads; do not require
		// server.baseUrl just so a public Host survives the Host allowlist.
		if isNonBrowserCallbackPath(path) {
			return nil
		}
		return apiError{
			code:    pkgapi.ErrorCodeUnauthorized,
			status:  http.StatusForbidden,
			message: "Host is not allowed",
		}
	}

	if !authorityAllowed(host, allowed) {
		return apiError{
			code:    pkgapi.ErrorCodeUnauthorized,
			status:  http.StatusForbidden,
			message: "Host is not allowed",
		}
	}
	if !originAllowed(origin, allowed) {
		return apiError{
			code:    pkgapi.ErrorCodeUnauthorized,
			status:  http.StatusForbidden,
			message: "Origin is not allowed",
		}
	}
	return nil
}

// effectiveRequestHost returns the Host used for allowlisting. Under tests only,
// httptest's synthetic "example.com" maps to the configured bind authority.
func effectiveRequestHost(r *http.Request, cfg config.Config) string {
	host := strings.TrimSpace(r.Host)
	if rewriteHTTPtestDefaultHost && strings.EqualFold(host, "example.com") {
		return configuredRequestAuthority(cfg)
	}
	return host
}

func configuredRequestAuthority(cfg config.Config) string {
	port := cfg.Server.Port
	if port <= 0 {
		port = config.DefaultServerPort
	}
	portStr := strconv.Itoa(port)
	host := normalizeAuthorityHost(cfg.Server.Host)
	switch {
	case host == "" || isWildcardBindHost(host) || isLoopbackHostname(host):
		return net.JoinHostPort("127.0.0.1", portStr)
	default:
		return net.JoinHostPort(host, portStr)
	}
}

// isLoopbackAuthority reports whether hostport is a loopback hostname/IP,
// optionally with any port (CLI and httptest.Server dial targets).
func isLoopbackAuthority(hostport string) bool {
	host, _, err := splitHostPort(hostport)
	if err != nil {
		return false
	}
	return isLoopbackHostname(host)
}

// allowedAuthorities returns host:port authorities the daemon accepts for
// browser Host/Origin checks.
func allowedAuthorities(cfg config.Config) map[string]struct{} {
	port := cfg.Server.Port
	if port <= 0 {
		port = config.DefaultServerPort
	}
	portStr := strconv.Itoa(port)

	out := make(map[string]struct{})
	add := func(host string, p string) {
		host = normalizeAuthorityHost(host)
		if host == "" {
			return
		}
		if p == "" {
			out[host] = struct{}{}
			return
		}
		out[net.JoinHostPort(host, p)] = struct{}{}
	}

	bindHost := normalizeAuthorityHost(cfg.Server.Host)
	switch {
	case bindHost == "" || isWildcardBindHost(bindHost):
		for _, alias := range loopbackHostAliases() {
			add(alias, portStr)
		}
	case isLoopbackHostname(bindHost):
		for _, alias := range loopbackHostAliases() {
			add(alias, portStr)
		}
		add(bindHost, portStr)
	default:
		add(bindHost, portStr)
	}

	if cfg.Server.BaseURL != nil {
		base := strings.TrimSpace(*cfg.Server.BaseURL)
		if parsed, err := url.Parse(base); err == nil && parsed.Host != "" {
			h := parsed.Hostname()
			p := parsed.Port()
			if p == "" {
				switch strings.ToLower(parsed.Scheme) {
				case "https":
					p = "443"
				case "http":
					p = "80"
				default:
					p = portStr
				}
			}
			add(h, p)
			if isLoopbackHostname(h) {
				for _, alias := range loopbackHostAliases() {
					add(alias, p)
				}
			}
		}
	}

	return out
}

func loopbackHostAliases() []string {
	return []string{"127.0.0.1", "localhost", "::1"}
}

func isWildcardBindHost(host string) bool {
	host = normalizeAuthorityHost(host)
	switch host {
	case "0.0.0.0", "::", "0:0:0:0:0:0:0:0":
		return true
	default:
		return false
	}
}

func normalizeAuthorityHost(host string) string {
	host = strings.TrimSpace(host)
	host = strings.Trim(host, "[]")
	if host == "" {
		return ""
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() {
			if ip.To4() != nil {
				return "127.0.0.1"
			}
			return "::1"
		}
		// Canonicalize IPv6 textual form.
		return ip.String()
	}
	return strings.ToLower(host)
}

func authorityAllowed(hostport string, allowed map[string]struct{}) bool {
	host, port, err := splitHostPort(hostport)
	if err != nil {
		return false
	}
	host = normalizeAuthorityHost(host)
	if host == "" {
		return false
	}
	if port != "" {
		_, ok := allowed[net.JoinHostPort(host, port)]
		return ok
	}
	// Browsers omit default ports on Host (https → no :443). Allowlist entries
	// from server.baseURL always include an explicit port, so try 80/443.
	if _, ok := allowed[host]; ok {
		return true
	}
	for _, p := range []string{"443", "80"} {
		if _, ok := allowed[net.JoinHostPort(host, p)]; ok {
			return true
		}
	}
	return false
}

func originAllowed(origin string, allowed map[string]struct{}) bool {
	origin = strings.TrimRight(strings.TrimSpace(origin), "/")
	if origin == "" {
		return false
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return false
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return false
	}
	host := normalizeAuthorityHost(parsed.Hostname())
	port := parsed.Port()
	if port == "" {
		if scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	if host == "" {
		return false
	}
	_, ok := allowed[net.JoinHostPort(host, port)]
	return ok
}

func splitHostPort(hostport string) (host, port string, err error) {
	hostport = strings.TrimSpace(hostport)
	if hostport == "" {
		return "", "", net.InvalidAddrError("empty host")
	}
	// net.SplitHostPort requires a port; accept bare hosts too.
	if _, _, splitErr := net.SplitHostPort(hostport); splitErr != nil {
		// Bare hostname / IP (including bracketed IPv6 without port).
		if strings.HasPrefix(hostport, "[") && strings.HasSuffix(hostport, "]") {
			inner := strings.TrimSuffix(strings.TrimPrefix(hostport, "["), "]")
			if net.ParseIP(inner) != nil {
				return inner, "", nil
			}
		}
		if strings.Count(hostport, ":") == 0 || net.ParseIP(hostport) != nil {
			return hostport, "", nil
		}
		// host:port without brackets for IPv6 is ambiguous; try SplitHostPort after all.
		return net.SplitHostPort(hostport)
	}
	return net.SplitHostPort(hostport)
}

func isLoopbackHostname(host string) bool {
	host = normalizeAuthorityHost(host)
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func isDashboardBootstrapPath(path string) bool {
	return path == dashboardBootstrapCodePath || path == dashboardBootstrapExchangePath
}

// isNonBrowserCallbackPath reports paths that receive external non-browser
// webhooks/callbacks and authenticate via their own shared secret rather than
// browser Host/Origin. Host allowlisting still applies when Origin is present
// (browser-initiated requests).
func isNonBrowserCallbackPath(path string) bool {
	switch path {
	case apiBasePath + "/hitl/feishu":
		return true
	default:
		return false
	}
}
