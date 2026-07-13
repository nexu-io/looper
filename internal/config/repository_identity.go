package config

import (
	"fmt"
	"strings"
)

const (
	defaultGitHubProviderID  = "github"
	defaultGitHubProviderURL = "https://github.com"
)

// RepositoryIdentity is the forge-qualified identity of a configured project.
// The explicit project-to-provider binding is authoritative; the implicit
// GitHub values preserve legacy projects that predate provider configuration.
type RepositoryIdentity struct {
	ProviderID string
	Kind       ProviderKind
	BaseURL    string
	Repo       string
}

// Key returns a normalized, collision-safe key for in-process indexing.
func (identity RepositoryIdentity) Key() string {
	kind := string(identity.Kind)
	return fmt.Sprintf("%d:%s%d:%s%d:%s", len(kind), kind, len(identity.BaseURL), identity.BaseURL, len(identity.Repo), identity.Repo)
}

// ProjectRepositoryIdentity resolves a project through its configured provider
// binding. It returns false when the binding is unknown or the repository is
// empty, allowing validation to report those errors at their original paths.
func ProjectRepositoryIdentity(cfg Config, project ProjectRefConfig) (RepositoryIdentity, bool) {
	repo := strings.ToLower(strings.TrimSpace(project.Repo))
	if repo == "" {
		return RepositoryIdentity{}, false
	}

	providerID := strings.TrimSpace(project.Provider)
	if providerID == "" {
		return RepositoryIdentity{ProviderID: defaultGitHubProviderID, Kind: ProviderKindGitHub, BaseURL: defaultGitHubProviderURL, Repo: repo}, true
	}
	for _, provider := range cfg.Providers {
		if provider.ID != providerID {
			continue
		}
		if provider.Kind == ProviderKindPlane {
			return RepositoryIdentity{ProviderID: defaultGitHubProviderID, Kind: ProviderKindGitHub, BaseURL: defaultGitHubProviderURL, Repo: repo}, true
		}
		baseURL := normalizeBaseURL(provider.BaseURL)
		if provider.Kind == ProviderKindGitHub && baseURL == "" {
			baseURL = defaultGitHubProviderURL
		}
		return RepositoryIdentity{ProviderID: provider.ID, Kind: provider.Kind, BaseURL: baseURL, Repo: repo}, true
	}
	return RepositoryIdentity{}, false
}
