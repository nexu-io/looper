package config

import (
	"net/url"
	"strings"
)

// MatchForgejoProviderByRemoteHost finds a configured forgejo provider whose
// baseUrl host is compatible with a git remote host.
//
// Matching is intentionally host-based (not full URL): git remotes often use
// ssh.<api-host> (for example code.example.com vs ssh.code.example.com) while
// provider baseUrl is the HTTPS API host.
func MatchForgejoProviderByRemoteHost(cfg Config, remoteHost string) (ProviderConfig, bool) {
	remoteHost = normalizeRemoteHost(remoteHost)
	if remoteHost == "" {
		return ProviderConfig{}, false
	}

	var matched ProviderConfig
	matches := 0
	for _, provider := range cfg.Providers {
		if provider.Kind != ProviderKindForgejo {
			continue
		}
		if !remoteHostMatchesProviderBaseURL(remoteHost, provider.BaseURL) {
			continue
		}
		matched = provider
		matches++
	}
	if matches == 1 {
		return matched, true
	}
	// Ambiguous multi-provider match: refuse to guess.
	return ProviderConfig{}, false
}

func remoteHostMatchesProviderBaseURL(remoteHost, baseURL string) bool {
	providerHost := hostFromBaseURL(baseURL)
	if providerHost == "" || remoteHost == "" {
		return false
	}
	if remoteHost == providerHost {
		return true
	}
	// Common Forgejo/Gitea SSH host convention: ssh.<web-host>
	if remoteHost == "ssh."+providerHost {
		return true
	}
	if strings.HasPrefix(remoteHost, "ssh.") && strings.TrimPrefix(remoteHost, "ssh.") == providerHost {
		return true
	}
	// Also accept web host when remote uses bare host and provider has www. prefix or vice versa.
	if strings.TrimPrefix(remoteHost, "www.") == strings.TrimPrefix(providerHost, "www.") {
		return true
	}
	return false
}

func hostFromBaseURL(baseURL string) string {
	trimmed := strings.TrimSpace(baseURL)
	if trimmed == "" {
		return ""
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return ""
	}
	return normalizeRemoteHost(parsed.Hostname())
}

func normalizeRemoteHost(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	// Strip optional trailing port from host:port forms that did not come from url.Parse.
	if i := strings.LastIndex(host, ":"); i > 0 && strings.Count(host, ":") == 1 {
		// Only strip if the suffix looks like a port.
		port := host[i+1:]
		if port != "" && isAllDigits(port) {
			host = host[:i]
		}
	}
	return host
}

func isAllDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
