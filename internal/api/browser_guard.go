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

// validateBrowserRequest enforces Host allowlisting and Origin matching for
// browser requests, including safe methods (GET/HEAD) that expose dashboard
// state. CLI clients without an Origin header continue to work.
//
// DNS rebinding under authMode=none: when a browser sends Host and Origin for an
// attacker domain, both are checked against authorities derived from server
// config (bind host/port, loopback aliases, optional server.baseUrl) — not from
// the request Host itself. This applies to API/dashboard reads as well as
// mutations so rebinding cannot exfiltrate local state over GET.
func validateBrowserRequest(r *http.Request, cfg config.Config) error {
	host := strings.TrimSpace(r.Host)
	if host == "" {
		return apiError{
			code:    pkgapi.ErrorCodeUnauthorized,
			status:  http.StatusForbidden,
			message: "Host header is required",
		}
	}

	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		// Non-browser clients (CLI): Host is the dial target; unit tests often
		// leave httptest's default Host. DNS rebinding always includes Origin.
		return nil
	}

	allowed := allowedAuthorities(cfg)
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
