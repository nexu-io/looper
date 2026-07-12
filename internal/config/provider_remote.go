package config

import (
	"net/url"
	"strings"
)

// UpsertRuntimeProjectBinding mirrors an API-discovered project provider binding
// into the live config project list so forgejo client resolution and role
// profiles work for projects that were not declared in the config file.
//
// Config-file projects keep authority: an existing config entry with the same
// id is left unchanged. Runtime-added entries are replaced on repeat API adds so
// provider/repo metadata stays aligned with the project database.
func UpsertRuntimeProjectBinding(cfg *Config, projectID, name, providerID, repo, repoPath string) {
	if cfg == nil {
		return
	}
	projectID = strings.TrimSpace(projectID)
	providerID = strings.TrimSpace(providerID)
	repo = strings.TrimSpace(repo)
	repoPath = strings.TrimSpace(repoPath)
	if projectID == "" {
		return
	}
	existingIndex := -1
	for index, existing := range cfg.Projects {
		if existing.ID == projectID {
			existingIndex = index
			break
		}
	}
	if providerID == "" || repo == "" || repoPath == "" {
		if existingIndex >= 0 && cfg.hasRuntimeProjectBinding(projectID) {
			cfg.Projects = append(cfg.Projects[:existingIndex], cfg.Projects[existingIndex+1:]...)
			delete(cfg.runtimeProjectBindingIDs, projectID)
		}
		return
	}
	if existingIndex >= 0 && !cfg.hasRuntimeProjectBinding(projectID) {
		return
	}
	if name = strings.TrimSpace(name); name == "" {
		name = projectID
	}
	project := ProjectRefConfig{
		ID:       projectID,
		Name:     name,
		Provider: providerID,
		Repo:     repo,
		RepoPath: repoPath,
	}
	if resolvedProjectProviderKind(*cfg, project) == ProviderKindForgejo {
		applyForgejoProjectProfile(&project)
	}
	if existingIndex >= 0 {
		cfg.Projects[existingIndex] = project
		cfg.markRuntimeProjectBinding(projectID)
		return
	}
	cfg.Projects = append(cfg.Projects, project)
	cfg.markRuntimeProjectBinding(projectID)
}

func (cfg *Config) hasRuntimeProjectBinding(projectID string) bool {
	if cfg == nil || cfg.runtimeProjectBindingIDs == nil {
		return false
	}
	_, ok := cfg.runtimeProjectBindingIDs[projectID]
	return ok
}

func (cfg *Config) markRuntimeProjectBinding(projectID string) {
	if cfg.runtimeProjectBindingIDs == nil {
		cfg.runtimeProjectBindingIDs = map[string]struct{}{}
	}
	cfg.runtimeProjectBindingIDs[projectID] = struct{}{}
}

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
	if i := strings.LastIndex(host, ":"); i > 0 && !strings.Contains(host, "]") {
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
