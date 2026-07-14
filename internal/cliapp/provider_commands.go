package cliapp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"sort"
	"strings"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/forge"
	"github.com/nexu-io/looper/internal/storage"
	"github.com/spf13/cobra"
)

type providerOutput struct {
	ID              string              `json:"id"`
	Kind            config.ProviderKind `json:"kind"`
	BaseURL         string              `json:"baseUrl,omitempty"`
	TokenEnv        string              `json:"tokenEnv,omitempty"`
	Identity        string              `json:"identity,omitempty"`
	Repo            string              `json:"repo,omitempty"`
	ConfigPath      string              `json:"configPath,omitempty"`
	RestartRequired bool                `json:"restartRequired,omitempty"`
}

func (r *commandRuntime) providerAdd(cmd *cobra.Command, _ []string) error {
	baseURL, err := validateForgejoBaseURL(getStringFlag(cmd, "forgejo-url"))
	if err != nil {
		return err
	}
	tokenEnv := strings.TrimSpace(getStringFlag(cmd, "forgejo-token-env"))
	if !environmentNamePattern.MatchString(tokenEnv) {
		return fmt.Errorf("--forgejo-token-env must name a valid environment variable")
	}
	if strings.TrimSpace(os.Getenv(tokenEnv)) == "" {
		return fmt.Errorf("environment variable %s is not set", tokenEnv)
	}
	id := strings.TrimSpace(getStringFlag(cmd, "id"))
	if id == "" {
		id = forgejoProviderID(baseURL)
	}
	if deriveBootstrapProjectID(id) != id {
		return fmt.Errorf("--id must contain only lowercase letters, numbers, and hyphens")
	}
	provider := config.ProviderConfig{ID: id, Kind: config.ProviderKindForgejo, BaseURL: baseURL, TokenEnv: stringPtr(tokenEnv)}
	identity, err := r.testForgejoIdentity(cmd.Context(), provider)
	if err != nil {
		return err
	}

	loaded, err := r.loadConfigForEdit()
	if err != nil {
		return err
	}
	for _, existing := range loaded.Config.Providers {
		if existing.ID == id {
			return fmt.Errorf("provider id %q already exists", id)
		}
	}
	providers := append([]config.PartialProviderConfig(nil), partialProviders(loaded)...)
	kind := config.ProviderKindForgejo
	providers = append(providers, config.PartialProviderConfig{ID: id, Kind: &kind, BaseURL: stringPtr(baseURL), TokenEnv: stringPtr(tokenEnv)})
	partial := loaded.Partial
	partial.Providers = &providers
	if err := r.writeConfigFile(loaded.Metadata.ConfigPath, partial); err != nil {
		return err
	}
	return writeProviderResult(cmd, providerOutput{ID: id, Kind: kind, BaseURL: baseURL, TokenEnv: tokenEnv, Identity: identity, ConfigPath: loaded.Metadata.ConfigPath, RestartRequired: true}, "Provider added")
}

func (r *commandRuntime) prepareProjectAddProvider(cmd *cobra.Command, repoPath string) (string, string, error) {
	providerID := strings.TrimSpace(getStringFlag(cmd, "provider"))
	repo := strings.TrimSpace(getStringFlag(cmd, "repo"))
	forgejoURL := strings.TrimSpace(getStringFlag(cmd, "forgejo-url"))
	explicitForgejoBaseURL := ""
	if forgejoURL == "" {
		if providerID != "" {
			loaded, err := r.loadConfigForEdit()
			if err != nil {
				return "", "", err
			}
			for _, provider := range loaded.Config.Providers {
				if provider.ID != providerID {
					continue
				}
				if provider.Kind != config.ProviderKindForgejo || repo != "" {
					return providerID, repo, nil
				}
				explicitForgejoBaseURL = provider.BaseURL
				break
			}
		}
		if explicitForgejoBaseURL == "" && providerID != "forgejo" {
			return providerID, repo, nil
		}
	}
	absPath, err := absolutePathIfSet(repoPath)
	if err != nil || absPath == "" {
		return "", "", fmt.Errorf("Forgejo project add requires a repository path")
	}
	remote, err := r.detectBootstrapOriginRemote(cmd.Context(), absPath)
	if err != nil {
		return "", "", err
	}
	if repo == "" {
		repo = remote.Repo
	} else if repo != remote.Repo {
		return "", "", fmt.Errorf("--repo %q does not match origin repo %q", repo, remote.Repo)
	}
	if explicitForgejoBaseURL != "" {
		if !forgejoRemoteMatchesBaseURL(remote, explicitForgejoBaseURL) {
			return "", "", fmt.Errorf("origin host %q does not match provider %q base URL %q", remote.Host, providerID, explicitForgejoBaseURL)
		}
		return providerID, repo, nil
	}
	loaded, err := r.loadConfigForEdit()
	if err != nil {
		return "", "", err
	}
	if forgejoURL == "" {
		matches := make([]string, 0)
		for _, provider := range loaded.Config.Providers {
			if provider.Kind == config.ProviderKindForgejo && forgejoRemoteMatchesBaseURL(remote, provider.BaseURL) {
				matches = append(matches, provider.ID)
			}
		}
		if len(matches) == 1 {
			return matches[0], repo, nil
		}
		if len(matches) == 0 {
			return "", "", fmt.Errorf("no Forgejo provider matches origin host %q; pass --forgejo-url and --forgejo-token-env to create one", remote.Host)
		}
		return "", "", fmt.Errorf("multiple Forgejo providers match origin host %q (%s); pass an explicit provider id", remote.Host, strings.Join(matches, ", "))
	}

	baseURL, err := validateForgejoBaseURL(forgejoURL)
	if err != nil {
		return "", "", err
	}
	if !forgejoRemoteMatchesBaseURL(remote, baseURL) {
		return "", "", fmt.Errorf("origin host %q does not match --forgejo-url %q", remote.Host, baseURL)
	}
	tokenEnv := strings.TrimSpace(getStringFlag(cmd, "forgejo-token-env"))
	if !environmentNamePattern.MatchString(tokenEnv) || strings.TrimSpace(os.Getenv(tokenEnv)) == "" {
		return "", "", fmt.Errorf("--forgejo-token-env must name a set environment variable")
	}
	if providerID == "" {
		providerID = forgejoProviderID(baseURL)
	}
	if deriveBootstrapProjectID(providerID) != providerID {
		return "", "", fmt.Errorf("--provider must contain only lowercase letters, numbers, and hyphens when creating a Forgejo provider")
	}
	provider := config.ProviderConfig{ID: providerID, Kind: config.ProviderKindForgejo, BaseURL: baseURL, TokenEnv: stringPtr(tokenEnv)}
	client, err := forge.NewForgejoClientFromConfig(provider, repo, forge.WithHTTPClient(r.app.deps.HTTPClient))
	if err != nil {
		return "", "", err
	}
	if _, err := client.CurrentUser(cmd.Context()); err != nil {
		return "", "", fmt.Errorf("validate Forgejo current identity: %w", err)
	}
	if err := client.CheckRepository(cmd.Context()); err != nil {
		return "", "", fmt.Errorf("validate Forgejo repository %s: %w", repo, err)
	}
	for _, existing := range loaded.Config.Providers {
		if existing.ID == providerID {
			if existing.Kind == provider.Kind && forgejoBaseURLsMatch(existing.BaseURL, provider.BaseURL) && dereferenceString(existing.TokenEnv) == tokenEnv {
				return providerID, repo, nil
			}
			return "", "", fmt.Errorf("provider id %q already exists with different settings", providerID)
		}
	}
	providers := partialProviders(loaded)
	kind := config.ProviderKindForgejo
	providers = append(providers, config.PartialProviderConfig{ID: providerID, Kind: &kind, BaseURL: stringPtr(baseURL), TokenEnv: stringPtr(tokenEnv)})
	partial := loaded.Partial
	partial.Providers = &providers
	if err := r.writeConfigFile(loaded.Metadata.ConfigPath, partial); err != nil {
		return "", "", err
	}
	if err := r.daemonRestartForBootstrap(cmd); err != nil {
		return "", "", fmt.Errorf("provider %q was added, but looperd could not reload it: %w", providerID, err)
	}
	return providerID, repo, nil
}

func (r *commandRuntime) providerList(cmd *cobra.Command, _ []string) error {
	loaded, err := r.loadConfigForEdit()
	if err != nil {
		return err
	}
	items := make([]providerOutput, 0, len(loaded.Config.Providers))
	for _, provider := range loaded.Config.Providers {
		item := providerOutput{ID: provider.ID, Kind: provider.Kind, BaseURL: provider.BaseURL, ConfigPath: loaded.Metadata.ConfigPath}
		if provider.TokenEnv != nil {
			item.TokenEnv = *provider.TokenEnv
		}
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	if getBoolFlag(cmd, "json") {
		return writeJSON(cmd.OutOrStdout(), map[string]any{"items": items, "configPath": loaded.Metadata.ConfigPath})
	}
	rows := make([]tableRow, 0, len(items))
	for _, item := range items {
		rows = append(rows, tableRow{"id": item.ID, "kind": item.Kind, "baseUrl": item.BaseURL, "tokenEnv": item.TokenEnv})
	}
	printTable(cmd.OutOrStdout(), []string{"id", "kind", "baseUrl", "tokenEnv"}, rows)
	return nil
}

func (r *commandRuntime) providerTest(cmd *cobra.Command, args []string) error {
	loaded, err := r.loadConfigForEdit()
	if err != nil {
		return err
	}
	id := ""
	if len(args) > 0 {
		id = strings.TrimSpace(args[0])
	}
	provider, err := selectForgejoProvider(loaded.Config, id)
	if err != nil {
		return err
	}
	repo := strings.TrimSpace(getStringFlag(cmd, "repo"))
	if repo == "" {
		for _, project := range loaded.Config.Projects {
			if project.Provider == provider.ID && strings.TrimSpace(project.Repo) != "" {
				repo = project.Repo
				break
			}
		}
	}
	if repo == "" {
		_, repo, err = runtimeProjectBindingForProvider(cmd.Context(), loaded.Config.Storage.DBPath, provider.ID)
		if err != nil {
			return err
		}
	}
	if repo == "" {
		return fmt.Errorf("provider test requires --repo owner/name when no project is bound to %q", provider.ID)
	}
	client, err := forge.NewForgejoClientFromConfig(provider, repo, forge.WithHTTPClient(r.app.deps.HTTPClient))
	if err != nil {
		return err
	}
	identity, err := client.CurrentUser(cmd.Context())
	if err != nil {
		return fmt.Errorf("validate Forgejo current identity: %w", err)
	}
	if err := client.CheckRepository(cmd.Context()); err != nil {
		return fmt.Errorf("validate Forgejo repository %s: %w", repo, err)
	}
	return writeProviderResult(cmd, providerOutput{ID: provider.ID, Kind: provider.Kind, BaseURL: provider.BaseURL, TokenEnv: dereferenceString(provider.TokenEnv), Identity: identity.Login, Repo: repo, ConfigPath: loaded.Metadata.ConfigPath}, "Provider test passed")
}

func (r *commandRuntime) providerRemove(cmd *cobra.Command, args []string) error {
	id := strings.TrimSpace(args[0])
	loaded, err := r.loadConfigForEdit()
	if err != nil {
		return err
	}
	for _, project := range loaded.Config.Projects {
		if project.Provider == id {
			return fmt.Errorf("provider %q is bound to project %q; remove or rebind the project first", id, project.ID)
		}
	}
	index := -1
	for i, provider := range loaded.Config.Providers {
		if provider.ID == id {
			index = i
			break
		}
	}
	if index < 0 {
		return fmt.Errorf("provider %q not found", id)
	}
	runtimeProjectID, _, err := runtimeProjectBindingForProvider(cmd.Context(), loaded.Config.Storage.DBPath, id)
	if err != nil {
		return err
	}
	if runtimeProjectID != "" {
		return fmt.Errorf("provider %q is bound to project %q; remove or rebind the project first", id, runtimeProjectID)
	}
	if !getBoolFlag(cmd, "force") {
		confirmed, err := promptBootstrapBool(bufio.NewReader(cmd.InOrStdin()), cmd.OutOrStdout(), fmt.Sprintf("Remove provider %s", id), false)
		if err != nil {
			return err
		}
		if !confirmed {
			return fmt.Errorf("provider removal cancelled")
		}
	}
	providers := partialProviders(loaded)
	providers = append(providers[:index:index], providers[index+1:]...)
	partial := loaded.Partial
	partial.Providers = &providers
	if err := r.writeConfigFile(loaded.Metadata.ConfigPath, partial); err != nil {
		return err
	}
	return writeProviderResult(cmd, providerOutput{ID: id, ConfigPath: loaded.Metadata.ConfigPath, RestartRequired: true}, "Provider removed")
}

func runtimeProjectBindingForProvider(ctx context.Context, dbPath, providerID string) (string, string, error) {
	if _, err := os.Stat(dbPath); err != nil {
		if os.IsNotExist(err) {
			return "", "", nil
		}
		return "", "", fmt.Errorf("check runtime project database: %w", err)
	}
	db, err := storage.OpenSQLiteDB(ctx, dbPath)
	if err != nil {
		return "", "", fmt.Errorf("open runtime project database: %w", err)
	}
	defer func() { _ = db.Close() }()
	projects, err := storage.NewRepositories(db).Projects.List(ctx)
	if err != nil {
		return "", "", err
	}
	boundProjectID := ""
	for _, project := range projects {
		if project.Archived || project.MetadataJSON == nil {
			continue
		}
		var metadata struct {
			Provider string `json:"provider"`
			Repo     string `json:"repo"`
		}
		if err := json.Unmarshal([]byte(*project.MetadataJSON), &metadata); err != nil {
			return "", "", fmt.Errorf("decode project %q metadata: %w", project.ID, err)
		}
		if strings.TrimSpace(metadata.Provider) == providerID {
			repo := strings.TrimSpace(metadata.Repo)
			if repo != "" {
				return project.ID, repo, nil
			}
			if boundProjectID == "" {
				boundProjectID = project.ID
			}
		}
	}
	return boundProjectID, "", nil
}

func partialProviders(loaded config.LoadedFileConfig) []config.PartialProviderConfig {
	if loaded.Partial.Providers != nil {
		return append([]config.PartialProviderConfig(nil), (*loaded.Partial.Providers)...)
	}
	providers := make([]config.PartialProviderConfig, 0, len(loaded.Config.Providers))
	for _, provider := range loaded.Config.Providers {
		kind := provider.Kind
		providers = append(providers, config.PartialProviderConfig{ID: provider.ID, Kind: &kind, BaseURL: stringPtr(provider.BaseURL), GHPath: provider.GHPath, TokenEnv: provider.TokenEnv, Workspace: provider.Workspace, ProjectID: provider.ProjectID})
	}
	return providers
}

func (r *commandRuntime) testForgejoIdentity(ctx context.Context, provider config.ProviderConfig) (string, error) {
	client, err := forge.NewForgejoClientFromConfig(provider, "looper/provider-test", forge.WithHTTPClient(r.app.deps.HTTPClient))
	if err != nil {
		return "", err
	}
	identity, err := client.CurrentUser(ctx)
	if err != nil {
		return "", fmt.Errorf("validate Forgejo current identity: %w", err)
	}
	if strings.TrimSpace(identity.Login) == "" {
		return "", fmt.Errorf("validate Forgejo current identity: server returned an empty login")
	}
	return identity.Login, nil
}

func selectForgejoProvider(cfg config.Config, id string) (config.ProviderConfig, error) {
	matches := make([]config.ProviderConfig, 0)
	for _, provider := range cfg.Providers {
		if provider.Kind == config.ProviderKindForgejo && (id == "" || provider.ID == id) {
			matches = append(matches, provider)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) == 0 {
		return config.ProviderConfig{}, fmt.Errorf("Forgejo provider %q not found", id)
	}
	return config.ProviderConfig{}, fmt.Errorf("multiple Forgejo providers are configured; pass an explicit provider id")
}

func forgejoProviderID(baseURL string) string {
	parsed, _ := url.Parse(baseURL)
	id := deriveBootstrapProjectID(parsed.Hostname())
	if id == "" || id == "project" {
		return "forgejo"
	}
	return "forgejo-" + id
}

func dereferenceString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func writeProviderResult(cmd *cobra.Command, result providerOutput, heading string) error {
	if getBoolFlag(cmd, "json") {
		return writeJSON(cmd.OutOrStdout(), result)
	}
	writeHumanProvider(cmd.OutOrStdout(), heading, result)
	return nil
}

func writeHumanProvider(w io.Writer, heading string, result providerOutput) {
	printSection(w, heading, [][2]any{{"id", result.ID}, {"kind", result.Kind}, {"baseUrl", result.BaseURL}, {"tokenEnv", result.TokenEnv}, {"identity", result.Identity}, {"repo", result.Repo}, {"configPath", result.ConfigPath}, {"restartRequired", result.RestartRequired}})
	if result.RestartRequired {
		_, _ = fmt.Fprintln(w, "\nNext step: looper daemon restart")
	}
}
