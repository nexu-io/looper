package config

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
)

// HostingIdentityKind selects daemon-owned code hosting authentication.
type HostingIdentityKind string

const (
	HostingIdentityGitHubApp    HostingIdentityKind = "github-app"
	HostingIdentityForgejoToken HostingIdentityKind = "forgejo-token"
)

// HostingCommitIdentity overrides the bot's default Git attribution. Empty
// fields inherit the account's name or email independently.
type HostingCommitIdentity struct {
	Name  string `json:"name,omitempty"`
	Email string `json:"email,omitempty"`
}

// HostingIdentityConfig contains credential references, never credential
// values. Tokens and private keys are resolved only inside the daemon.
type HostingIdentityConfig struct {
	Kind           HostingIdentityKind   `json:"kind"`
	BaseURL        string                `json:"baseUrl,omitempty"`
	AppID          int64                 `json:"appId,omitempty"`
	InstallationID int64                 `json:"installationId,omitempty"`
	PrivateKeyFile string                `json:"privateKeyFile,omitempty"`
	TokenEnv       string                `json:"tokenEnv,omitempty"`
	Commit         HostingCommitIdentity `json:"commit,omitempty"`
}

// ResolvedHostingIdentity is a detached binding captured before discovery or a
// role run. Refreshing credentials must retain this definition and target.
type ResolvedHostingIdentity struct {
	Name       string
	Definition HostingIdentityConfig
	Target     RepositoryIdentity
	ProjectID  string
	Role       string
}

var hostingIdentityRoles = []string{"planner", "reviewer", "worker", "fixer", "coordinator"}

// ResolveHostingIdentity applies effective role identity → project default →
// existing authentication. An empty project role override clears the global
// role identity and inherits the project default. A selected invalid identity
// returns selected=true with an error; callers must never retry legacy auth.
func ResolveHostingIdentity(cfg Config, projectID, role string) (resolved ResolvedHostingIdentity, selected bool, err error) {
	projectID = strings.TrimSpace(projectID)
	role = strings.TrimSpace(role)
	name, knownRole := roleHostingIdentity(ProjectRoleConfigs(cfg, projectID), role)
	project := findConfiguredProject(cfg.Projects, projectID)
	if name == "" && project != nil {
		name = strings.TrimSpace(project.Identity)
	}
	selected = name != ""
	if !knownRole {
		return resolved, selected, fmt.Errorf("hosting identity: unsupported role %q", role)
	}
	if !selected {
		return resolved, false, nil
	}
	resolved = ResolvedHostingIdentity{Name: name, ProjectID: projectID, Role: role}
	if project == nil {
		return resolved, true, fmt.Errorf("hosting identity %q: project %q is not configured", name, projectID)
	}
	definition, ok := cfg.Identities[name]
	if !ok {
		return resolved, true, fmt.Errorf("hosting identity %q for project %q role %q is not defined", name, projectID, role)
	}
	definition = normalizeHostingIdentity(definition)
	var issues []ValidationIssue
	validateHostingIdentityDefinition(definition, "identities."+name, &issues)
	if len(issues) > 0 {
		return resolved, true, &ConfigValidationError{Issues: issues}
	}
	target, ok := ProjectRepositoryIdentity(cfg, *project)
	if !ok {
		return resolved, true, fmt.Errorf("hosting identity %q for project %q role %q requires a configured provider and repository", name, projectID, role)
	}
	if message := hostingIdentityTargetError(definition, target); message != "" {
		return resolved, true, fmt.Errorf("hosting identity %q for project %q role %q: %s", name, projectID, role, message)
	}
	resolved.Definition = definition
	resolved.Target = target
	return resolved, true, nil
}

func roleHostingIdentity(roles RoleConfigs, role string) (string, bool) {
	var name string
	switch role {
	case "planner":
		name = roles.Planner.Identity
	case "reviewer":
		name = roles.Reviewer.Identity
	case "worker":
		name = roles.Worker.Identity
	case "fixer":
		name = roles.Fixer.Identity
	case "coordinator":
		name = roles.Coordinator.Identity
	default:
		return "", false
	}
	return strings.TrimSpace(name), true
}

func normalizeHostingIdentity(definition HostingIdentityConfig) HostingIdentityConfig {
	definition.Kind = HostingIdentityKind(strings.TrimSpace(string(definition.Kind)))
	definition.BaseURL = normalizeBaseURL(definition.BaseURL)
	if definition.Kind == HostingIdentityGitHubApp && definition.BaseURL == "" {
		definition.BaseURL = defaultGitHubProviderURL
	}
	definition.PrivateKeyFile = strings.TrimSpace(definition.PrivateKeyFile)
	definition.TokenEnv = strings.TrimSpace(definition.TokenEnv)
	definition.Commit.Name = strings.TrimSpace(definition.Commit.Name)
	definition.Commit.Email = strings.TrimSpace(definition.Commit.Email)
	return definition
}

func normalizeHostingIdentityPaths(cfg *Config, cwd string) error {
	for name, definition := range cfg.Identities {
		path := definition.PrivateKeyFile
		if path == "" {
			continue
		}
		if path == "~" || strings.HasPrefix(path, "~/") {
			homeDir, err := os.UserHomeDir()
			if err != nil {
				return fmt.Errorf("identities.%s.privateKeyFile: determine home directory: %w", name, err)
			}
			path = filepath.Join(homeDir, strings.TrimPrefix(path, "~"))
		}
		definition.PrivateKeyFile = ResolveConfigPath(path, cwd)
		cfg.Identities[name] = definition
	}
	return nil
}

func validateHostingIdentities(cfg Config, issues *[]ValidationIssue) {
	names := make([]string, 0, len(cfg.Identities))
	for name := range cfg.Identities {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		path := "identities." + name
		if !agentProfileIDPattern.MatchString(name) {
			*issues = append(*issues, ValidationIssue{Path: path, Message: "identity name must contain only letters, digits, underscores, or hyphens"})
		}
		validateHostingIdentityDefinition(normalizeHostingIdentity(cfg.Identities[name]), path, issues)
	}
	validateReference := func(name, path string, target *RepositoryIdentity) {
		name = strings.TrimSpace(name)
		if name == "" {
			return
		}
		definition, ok := cfg.Identities[name]
		if !ok {
			*issues = append(*issues, ValidationIssue{Path: path, Message: fmt.Sprintf("references unknown identity: %s", name)})
			return
		}
		if target != nil {
			if message := hostingIdentityTargetError(normalizeHostingIdentity(definition), *target); message != "" {
				*issues = append(*issues, ValidationIssue{Path: path, Message: message})
			}
		}
	}
	for _, role := range hostingIdentityRoles {
		name, _ := roleHostingIdentity(cfg.Roles, role)
		validateReference(name, "roles."+role+".identity", nil)
	}
	for index, project := range cfg.Projects {
		prefix := fmt.Sprintf("projects[%d]", index)
		target, ok := ProjectRepositoryIdentity(cfg, project)
		var targetRef *RepositoryIdentity
		if ok {
			targetRef = &target
		}
		validateReference(project.Identity, prefix+".identity", targetRef)
		roles := ProjectRoleConfigs(cfg, project.ID)
		reportedMissingTarget := false
		for _, role := range hostingIdentityRoles {
			name, _ := roleHostingIdentity(roles, role)
			validateReference(name, prefix+".roles."+role+".identity", targetRef)
			if (name != "" || strings.TrimSpace(project.Identity) != "") && !ok && !reportedMissingTarget {
				*issues = append(*issues, ValidationIssue{Path: prefix + ".repo", Message: "an explicit hosting identity requires a configured provider and repository"})
				reportedMissingTarget = true
			}
		}
	}
}

// ValidateHostingIdentities checks identity policy and the authentication
// coverage of bound providers without reading credential files, environment
// values, or contacting the hosting platform. Project materialization uses it
// before publishing a SQLite catalog mutation.
func ValidateHostingIdentities(cfg Config) error {
	var issues []ValidationIssue
	validateHostingIdentities(cfg, &issues)
	for index, provider := range cfg.Providers {
		if provider.Kind != ProviderKindForgejo {
			continue
		}
		for _, project := range cfg.Projects {
			if project.Provider == provider.ID {
				validateForgejoProviderAuth(cfg, provider, fmt.Sprintf("providers[%d]", index), &issues)
				break
			}
		}
	}
	if len(issues) > 0 {
		return &ConfigValidationError{Issues: issues}
	}
	return nil
}

func validateHostingIdentityDefinition(definition HostingIdentityConfig, path string, issues *[]ValidationIssue) {
	add := func(field, message string) {
		*issues = append(*issues, ValidationIssue{Path: path + "." + field, Message: message})
	}
	if !validHostingIdentityBaseURL(definition.BaseURL) {
		add("baseUrl", "must be an absolute http(s) instance URL without credentials, query, or fragment")
	}
	switch definition.Kind {
	case HostingIdentityGitHubApp:
		if parsed, err := url.Parse(definition.BaseURL); err == nil && strings.Trim(parsed.Path, "/") != "" {
			add("baseUrl", "must be the GitHub instance origin without an API path")
		}
		if definition.AppID <= 0 {
			add("appId", "must be a positive integer for github-app identities")
		}
		if definition.InstallationID <= 0 {
			add("installationId", "must be a positive integer for github-app identities")
		}
		if definition.PrivateKeyFile == "" {
			add("privateKeyFile", "is required for github-app identities")
		} else if strings.ContainsAny(definition.PrivateKeyFile, "\x00\r\n") {
			add("privateKeyFile", "must be a file path without control characters")
		}
		if definition.TokenEnv != "" {
			add("tokenEnv", "must be omitted for github-app identities")
		}
	case HostingIdentityForgejoToken:
		if !environmentNamePattern.MatchString(definition.TokenEnv) {
			add("tokenEnv", "must name the environment variable holding the Forgejo account token")
		}
		if definition.AppID != 0 {
			add("appId", "must be omitted for forgejo-token identities")
		}
		if definition.InstallationID != 0 {
			add("installationId", "must be omitted for forgejo-token identities")
		}
		if definition.PrivateKeyFile != "" {
			add("privateKeyFile", "must be omitted for forgejo-token identities")
		}
	default:
		add("kind", "must be one of: github-app, forgejo-token")
	}
	for _, field := range []struct{ name, value string }{{"commit.name", definition.Commit.Name}, {"commit.email", definition.Commit.Email}} {
		if strings.ContainsAny(field.value, "<>") || strings.ContainsFunc(field.value, unicode.IsControl) {
			add(field.name, "must not contain control characters or angle brackets")
		}
	}
	if definition.Commit.Email != "" {
		at := strings.LastIndexByte(definition.Commit.Email, '@')
		if at <= 0 || at == len(definition.Commit.Email)-1 || strings.ContainsFunc(definition.Commit.Email, unicode.IsSpace) {
			add("commit.email", "must be an email address without whitespace")
		}
	}
}

func validHostingIdentityBaseURL(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Hostname() != "" && parsed.User == nil && parsed.RawQuery == "" && !parsed.ForceQuery && parsed.Fragment == "" && parsed.Opaque == ""
}

func hostingIdentityTargetError(definition HostingIdentityConfig, target RepositoryIdentity) string {
	wantKind := ProviderKindGitHub
	if definition.Kind == HostingIdentityForgejoToken {
		wantKind = ProviderKindForgejo
	}
	if target.Kind != wantKind {
		return "identity kind does not match the project's code hosting provider"
	}
	if !validHostingIdentityBaseURL(target.BaseURL) || normalizeBaseURL(definition.BaseURL) != normalizeBaseURL(target.BaseURL) {
		return "identity baseUrl must match the project's code hosting instance"
	}
	parts := strings.Split(target.Repo, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || parts[0] == "." || parts[0] == ".." || parts[1] == "." || parts[1] == ".." || strings.ContainsAny(target.Repo, "\\?#%") || strings.ContainsFunc(target.Repo, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return "an explicit hosting identity requires an owner/repository target"
	}
	return ""
}

// Providers whose coding roles all select bots do not need legacy credentials.
// Before SQLite materialization, a provider without file-bound projects may
// omit legacy credentials if a valid matching bot definition is available.
// Materialization then validates the actual projects with this same rule.
func forgejoProviderUsesOnlyHostingIdentities(cfg Config, provider ProviderConfig) bool {
	found := false
	for _, project := range cfg.Projects {
		if project.Provider != provider.ID {
			continue
		}
		found = true
		if strings.TrimSpace(project.Identity) != "" {
			continue
		}
		roles := ProjectRoleConfigs(cfg, project.ID)
		for _, role := range []string{"planner", "reviewer", "worker", "fixer"} {
			if name, _ := roleHostingIdentity(roles, role); name == "" {
				return false
			}
		}
	}
	if found {
		return true
	}
	for name, definition := range cfg.Identities {
		definition = normalizeHostingIdentity(definition)
		if !agentProfileIDPattern.MatchString(name) || definition.Kind != HostingIdentityForgejoToken || definition.BaseURL != normalizeBaseURL(provider.BaseURL) {
			continue
		}
		var issues []ValidationIssue
		validateHostingIdentityDefinition(definition, "identities."+name, &issues)
		if len(issues) == 0 {
			return true
		}
	}
	return false
}
