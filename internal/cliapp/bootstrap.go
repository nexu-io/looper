package cliapp

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/forge"
	"github.com/nexu-io/looper/internal/version"
	"github.com/spf13/cobra"
)

const bootstrapHealthCheckTimeout = 5 * time.Second

type bootstrapResult struct {
	ConfigPath         string   `json:"configPath"`
	ConfigCreated      bool     `json:"configCreated"`
	ProjectAdded       bool     `json:"projectAdded"`
	ManagedDaemonPath  string   `json:"managedDaemonPath"`
	DaemonInstalled    bool     `json:"daemonInstalled"`
	DaemonInstallState string   `json:"daemonInstallState"`
	DaemonRunning      bool     `json:"daemonRunning"`
	APIReachable       bool     `json:"apiReachable"`
	NextSteps          []string `json:"nextSteps"`
	Notes              []string `json:"notes,omitempty"`
	ProviderID         string   `json:"providerId,omitempty"`
	ProviderKind       string   `json:"providerKind,omitempty"`
	Repo               string   `json:"repo,omitempty"`
	Identity           string   `json:"identity,omitempty"`
}

type bootstrapOptions struct {
	Yes               bool
	Force             bool
	AgentVendor       string
	ProjectPath       string
	EnableLocalToken  bool
	DisableOsascript  bool
	Provider          string
	CodeRepo          string
	TriggerLabel      string
	PlaneBaseURL      string
	PlaneWorkspace    string
	PlaneProject      string
	PlaneTokenEnv     string
	FeishuWebhookEnv  string
	ForgejoURL        string
	ForgejoTokenEnv   string
	ForgejoAuth       string
	ForgejoTeaLogin   string
	ForgejoProviderID string
}

type bootstrapConfigPlan struct {
	AgentVendor      *config.AgentVendor
	EnableOsascript  bool
	EnableLocalToken bool
	ProjectPath      string
	// Provider is the task-source provider for the generated project:
	// "github" (default; unchanged behavior), "forgejo", or "plane".
	Provider          string
	CodeRepo          string
	TriggerLabel      string
	PlaneBaseURL      string
	PlaneWorkspace    string
	PlaneProject      string
	PlaneTokenEnv     string
	FeishuWebhookEnv  string
	ForgejoURL        string
	ForgejoTokenEnv   string
	ForgejoAuth       config.ProviderAuthMode
	ForgejoTeaLogin   string
	ForgejoProviderID string
	Repo              string
	Identity          string
}

const (
	bootstrapProviderGitHub     = "github"
	bootstrapProviderForgejo    = "forgejo"
	bootstrapProviderPlane      = "plane"
	defaultPlaneBootstrapBase   = "https://plane.powerformer.net/api/v1"
	defaultPlaneBootstrapToken  = "PLANE_API_KEY"
	defaultBootstrapTriggerText = "looper:plan"
)

func (r *commandRuntime) bootstrap(cmd *cobra.Command, args []string) error {
	_ = args

	ctx := cmd.Context()
	opts := bootstrapOptions{
		Yes:               getBoolFlag(cmd, "yes"),
		Force:             getBoolFlag(cmd, "force"),
		AgentVendor:       strings.TrimSpace(getStringFlag(cmd, "agent-vendor")),
		ProjectPath:       strings.TrimSpace(getStringFlag(cmd, "project-path")),
		EnableLocalToken:  getBoolFlag(cmd, "enable-local-token"),
		DisableOsascript:  getBoolFlag(cmd, "disable-osascript"),
		Provider:          strings.TrimSpace(getStringFlag(cmd, "provider")),
		CodeRepo:          strings.TrimSpace(getStringFlag(cmd, "code-repo")),
		TriggerLabel:      strings.TrimSpace(getStringFlag(cmd, "trigger-label")),
		PlaneBaseURL:      strings.TrimSpace(getStringFlag(cmd, "plane-base-url")),
		PlaneWorkspace:    strings.TrimSpace(getStringFlag(cmd, "plane-workspace")),
		PlaneProject:      strings.TrimSpace(getStringFlag(cmd, "plane-project")),
		PlaneTokenEnv:     strings.TrimSpace(getStringFlag(cmd, "plane-token-env")),
		FeishuWebhookEnv:  strings.TrimSpace(getStringFlag(cmd, "feishu-webhook-env")),
		ForgejoURL:        strings.TrimSpace(getStringFlag(cmd, "forgejo-url")),
		ForgejoTokenEnv:   strings.TrimSpace(getStringFlag(cmd, "forgejo-token-env")),
		ForgejoAuth:       strings.TrimSpace(getStringFlag(cmd, "auth")),
		ForgejoTeaLogin:   strings.TrimSpace(getStringFlag(cmd, "tea-login")),
		ForgejoProviderID: strings.TrimSpace(getStringFlag(cmd, "forgejo-provider-id")),
	}

	result, err := r.runBootstrap(ctx, cmd, opts)
	if err != nil {
		return err
	}

	if getBoolFlag(cmd, "json") {
		return writeJSON(cmd.OutOrStdout(), result)
	}

	return writeHumanBootstrapResult(cmd.OutOrStdout(), result)
}

func (r *commandRuntime) runBootstrap(ctx context.Context, cmd *cobra.Command, opts bootstrapOptions) (bootstrapResult, error) {
	cwd, err := r.getwd()
	if err != nil {
		return bootstrapResult{}, fmt.Errorf("determine current working directory: %w", err)
	}

	configPath, err := r.resolveBootstrapConfigPath(cwd)
	if err != nil {
		return bootstrapResult{}, err
	}
	managedDaemonPath, err := r.managedDaemonBinaryPath()
	if err != nil {
		return bootstrapResult{}, err
	}

	result := bootstrapResult{ConfigPath: configPath, ManagedDaemonPath: managedDaemonPath}
	planned, planNotes, err := r.planBootstrapConfig(cmd, cwd, opts)
	if err != nil {
		return bootstrapResult{}, err
	}
	result.Notes = append(result.Notes, planNotes...)

	preflightNotes, err := r.bootstrapPreflight(ctx, configPath, &planned)
	if err != nil {
		return bootstrapResult{}, err
	}
	result.Notes = append(result.Notes, preflightNotes...)

	if err := r.ensureBootstrapDirectories(); err != nil {
		return bootstrapResult{}, err
	}

	configCreated, projectAdded, err := r.ensureBootstrapConfig(configPath, cwd, planned)
	if err != nil {
		return bootstrapResult{}, err
	}
	result.ConfigCreated = configCreated
	result.ProjectAdded = projectAdded
	if planned.Provider == bootstrapProviderForgejo {
		result.ProviderID = planned.ForgejoProviderID
		result.ProviderKind = planned.Provider
		result.Repo = planned.Repo
		result.Identity = planned.Identity
	}

	installState, installed, err := r.ensureBootstrapDaemon(ctx, opts.Force)
	if err != nil {
		return bootstrapResult{}, err
	}
	result.DaemonInstallState = installState
	result.DaemonInstalled = installed

	loaded, err := r.loadConfig()
	if err != nil {
		return bootstrapResult{}, err
	}
	client := r.apiClientFromLoaded(loaded)

	var apiReachable bool
	if installed {
		apiReachable, err = r.bootstrapAPIReachableForInstalled(ctx, client)
	} else {
		apiReachable, err = r.bootstrapAPIReachable(ctx, client)
	}
	if err != nil {
		return bootstrapResult{}, err
	}
	if apiReachable && installed {
		expectedDaemon, err := r.readManagedDaemonVersion(ctx)
		if err != nil {
			return bootstrapResult{}, err
		}
		expectedVersion := ""
		if expectedDaemon != nil {
			expectedVersion = expectedDaemon.Version
		}
		matches, err := r.bootstrapReachableDaemonMatches(ctx, client, expectedVersion, managedDaemonPath)
		if err != nil {
			return bootstrapResult{}, err
		}
		if !matches {
			if err := r.bootstrapCanRestartReachableDaemon(ctx, client, loaded); err != nil {
				return bootstrapResult{}, err
			}
			if err := r.daemonRestartForBootstrap(cmd); err != nil {
				return bootstrapResult{}, err
			}
			apiReachable, err = r.waitForBootstrapMatchingDaemon(ctx, client, expectedVersion, managedDaemonPath)
			if err != nil {
				return bootstrapResult{}, err
			}
		}
	} else if !apiReachable {
		if err := r.daemonStartForBootstrap(cmd); err != nil {
			return bootstrapResult{}, err
		}
		apiReachable, err = r.waitForBootstrapHealth(ctx, client)
		if err != nil {
			return bootstrapResult{}, err
		}
	}

	result.APIReachable = apiReachable
	result.DaemonRunning = apiReachable
	restartRequired := planned.Provider == bootstrapProviderForgejo && projectAdded && !configCreated && apiReachable && !installed
	result.NextSteps = bootstrapNextStepsForPlan(planned, restartRequired)
	return result, nil
}

func (r *commandRuntime) resolveBootstrapConfigPath(cwd string) (string, error) {
	if override := strings.TrimSpace(extractConfigPathOverride(ExtractConfigArgs(r.argv))); override != "" {
		return config.ResolveConfigPath(override, cwd), nil
	}
	if override, ok := os.LookupEnv("LOOPER_CONFIG"); ok && strings.TrimSpace(override) != "" {
		return config.ResolveConfigPath(strings.TrimSpace(override), cwd), nil
	}
	defaultConfigPath, err := config.DiscoverDefaultConfigPath()
	if err != nil {
		return "", fmt.Errorf("determine default config path: %w", err)
	}
	return defaultConfigPath, nil
}

func extractConfigPathOverride(args []string) string {
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if !strings.HasPrefix(arg, "--config") {
			continue
		}
		if _, value, ok := strings.Cut(arg, "="); ok {
			return value
		}
		if index+1 < len(args) {
			return args[index+1]
		}
	}
	return ""
}

func (r *commandRuntime) planBootstrapConfig(cmd *cobra.Command, cwd string, opts bootstrapOptions) (bootstrapConfigPlan, []string, error) {
	plan := bootstrapConfigPlan{EnableOsascript: runtime.GOOS == "darwin", EnableLocalToken: opts.EnableLocalToken}
	if opts.DisableOsascript {
		plan.EnableOsascript = false
	}
	if opts.AgentVendor != "" {
		vendor := config.AgentVendor(opts.AgentVendor)
		if !isSupportedBootstrapVendor(vendor) {
			return bootstrapConfigPlan{}, nil, fmt.Errorf("unsupported --agent-vendor %q", opts.AgentVendor)
		}
		plan.AgentVendor = &vendor
	}
	if opts.ProjectPath != "" {
		resolved, err := filepath.Abs(opts.ProjectPath)
		if err != nil {
			return bootstrapConfigPlan{}, nil, fmt.Errorf("resolve --project-path: %w", err)
		}
		plan.ProjectPath = resolved
	}

	plan.FeishuWebhookEnv = opts.FeishuWebhookEnv
	planeNotes, err := r.resolveBootstrapProviderPlan(cmd, &plan, opts)
	if err != nil {
		return bootstrapConfigPlan{}, nil, err
	}

	configPath, err := r.resolveBootstrapConfigPath(cwd)
	if err != nil {
		return bootstrapConfigPlan{}, nil, err
	}
	if _, err := os.Stat(configPath); err == nil {
		if plan.Provider == bootstrapProviderPlane {
			return bootstrapConfigPlan{}, nil, fmt.Errorf("config already exists at %s; --provider plane generates a fresh config — remove it or pass --config <new-path> and rerun", configPath)
		}
		return plan, nil, nil
	} else if !os.IsNotExist(err) {
		return bootstrapConfigPlan{}, nil, fmt.Errorf("check config path %s: %w", configPath, err)
	}

	// The plane provider is fully specified by flags, so skip interactive prompts.
	if opts.Yes || plan.Provider == bootstrapProviderPlane {
		return plan, planeNotes, nil
	}

	reader := bufio.NewReader(cmd.InOrStdin())
	if plan.AgentVendor == nil {
		vendor, err := promptBootstrapVendor(reader, cmd.OutOrStdout())
		if err != nil {
			return bootstrapConfigPlan{}, nil, err
		}
		plan.AgentVendor = vendor
	}
	if !opts.DisableOsascript {
		enabled, err := promptBootstrapBool(reader, cmd.OutOrStdout(), "Enable osascript notifications", plan.EnableOsascript)
		if err != nil {
			return bootstrapConfigPlan{}, nil, err
		}
		plan.EnableOsascript = enabled
	}
	if !opts.EnableLocalToken {
		enabled, err := promptBootstrapBool(reader, cmd.OutOrStdout(), "Enable local-token API auth", false)
		if err != nil {
			return bootstrapConfigPlan{}, nil, err
		}
		plan.EnableLocalToken = enabled
	}
	if plan.ProjectPath == "" {
		projectPath, err := promptBootstrapString(reader, cmd.OutOrStdout(), "Default project path (optional)", "")
		if err != nil {
			return bootstrapConfigPlan{}, nil, err
		}
		if strings.TrimSpace(projectPath) != "" {
			resolved, err := filepath.Abs(strings.TrimSpace(projectPath))
			if err != nil {
				return bootstrapConfigPlan{}, nil, fmt.Errorf("resolve project path: %w", err)
			}
			plan.ProjectPath = resolved
		}
	}

	return plan, planeNotes, nil
}

// resolveBootstrapProviderPlan validates the --provider selection and, for
// plane, fills in defaults + required Plane coordinates and resolves the GitHub
// code repo (from --code-repo or the --project-path git origin). It returns
// human-readable notes (e.g. which env vars must be exported).
func (r *commandRuntime) resolveBootstrapProviderPlan(cmd *cobra.Command, plan *bootstrapConfigPlan, opts bootstrapOptions) ([]string, error) {
	provider := strings.ToLower(strings.TrimSpace(opts.Provider))
	if provider == "" {
		provider = bootstrapProviderGitHub
	}
	switch provider {
	case bootstrapProviderGitHub:
		plan.Provider = bootstrapProviderGitHub
		return nil, nil
	case bootstrapProviderForgejo:
		plan.Provider = bootstrapProviderForgejo
		return r.resolveForgejoBootstrapPlan(cmd.Context(), plan, opts)
	case bootstrapProviderPlane:
		plan.Provider = bootstrapProviderPlane
	default:
		return nil, fmt.Errorf("unsupported --provider %q (supported: github, forgejo, plane)", opts.Provider)
	}

	if plan.ProjectPath == "" {
		return nil, fmt.Errorf("--provider plane requires --project-path (the local checkout of the GitHub code repo)")
	}
	if strings.TrimSpace(opts.PlaneWorkspace) == "" {
		return nil, fmt.Errorf("--provider plane requires --plane-workspace (the Plane workspace slug)")
	}
	if strings.TrimSpace(opts.PlaneProject) == "" {
		return nil, fmt.Errorf("--provider plane requires --plane-project (the Plane project UUID)")
	}
	plan.PlaneWorkspace = strings.TrimSpace(opts.PlaneWorkspace)
	plan.PlaneProject = strings.TrimSpace(opts.PlaneProject)
	plan.PlaneBaseURL = strings.TrimSpace(opts.PlaneBaseURL)
	if plan.PlaneBaseURL == "" {
		plan.PlaneBaseURL = defaultPlaneBootstrapBase
	}
	plan.PlaneTokenEnv = strings.TrimSpace(opts.PlaneTokenEnv)
	if plan.PlaneTokenEnv == "" {
		plan.PlaneTokenEnv = defaultPlaneBootstrapToken
	}
	plan.TriggerLabel = strings.TrimSpace(opts.TriggerLabel)
	if plan.TriggerLabel == "" {
		plan.TriggerLabel = defaultBootstrapTriggerText
	}

	codeRepo := strings.TrimSpace(opts.CodeRepo)
	if codeRepo == "" {
		detected := r.detectBootstrapOriginRepo(cmd.Context(), plan.ProjectPath)
		codeRepo = detected
	}
	if codeRepo == "" {
		return nil, fmt.Errorf("--provider plane requires the GitHub code repo: pass --code-repo owner/repo or ensure %s has a github.com origin remote", plan.ProjectPath)
	}
	plan.CodeRepo = codeRepo

	notes := []string{
		fmt.Sprintf("plane provider: export %s with your Plane API key before starting looperd", plan.PlaneTokenEnv),
	}
	if plan.FeishuWebhookEnv != "" {
		notes = append(notes, fmt.Sprintf("feishu notifications: export %s with your Feishu (or generic) webhook URL", plan.FeishuWebhookEnv))
	}
	return notes, nil
}

var environmentNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func (r *commandRuntime) resolveForgejoBootstrapPlan(ctx context.Context, plan *bootstrapConfigPlan, opts bootstrapOptions) ([]string, error) {
	if plan.ProjectPath == "" {
		return nil, fmt.Errorf("--provider forgejo requires --project-path")
	}
	baseURL, err := validateForgejoBaseURL(opts.ForgejoURL)
	if err != nil {
		return nil, err
	}
	auth, tokenEnv, teaLogin, err := resolveForgejoBootstrapAuth(ctx, opts, baseURL)
	if err != nil {
		return nil, err
	}
	providerID := strings.TrimSpace(opts.ForgejoProviderID)
	if providerID == "" {
		providerID = "forgejo"
	}
	if deriveBootstrapProjectID(providerID) != providerID {
		return nil, fmt.Errorf("--forgejo-provider-id must contain only lowercase letters, numbers, and hyphens")
	}
	remote, err := r.detectBootstrapOriginRemote(ctx, plan.ProjectPath)
	if err != nil {
		return nil, err
	}
	if !forgejoRemoteMatchesBaseURL(remote, baseURL) {
		return nil, fmt.Errorf("origin host %q does not match --forgejo-url %q; pass the URL for that remote or correct origin", remote.Host, baseURL)
	}
	if remote.Repo == "" {
		return nil, fmt.Errorf("could not detect owner/repo from origin for %s", plan.ProjectPath)
	}
	provider := forgejoProviderConfig(providerID, baseURL, auth, tokenEnv, teaLogin)
	client, err := forge.NewForgejoClientFromConfig(provider, remote.Repo, forge.WithHTTPClient(r.app.deps.HTTPClient))
	if err != nil {
		return nil, err
	}
	identity, err := client.CurrentUser(ctx)
	if err != nil {
		return nil, fmt.Errorf("validate Forgejo current identity: %w", err)
	}
	if strings.TrimSpace(identity.Login) == "" {
		return nil, fmt.Errorf("validate Forgejo current identity: server returned an empty login")
	}
	if err := client.CheckRepository(ctx); err != nil {
		return nil, fmt.Errorf("validate Forgejo repository %s: %w", remote.Repo, err)
	}
	plan.ForgejoURL = baseURL
	plan.ForgejoAuth = auth
	plan.ForgejoTokenEnv = tokenEnv
	plan.ForgejoTeaLogin = teaLogin
	plan.ForgejoProviderID = providerID
	plan.Repo = remote.Repo
	plan.Identity = identity.Login
	if auth == config.ProviderAuthTea {
		return []string{fmt.Sprintf("forgejo provider: tea login %q (no tokenEnv required)", teaLogin)}, nil
	}
	return []string{fmt.Sprintf("forgejo provider: export %s before starting looperd", tokenEnv)}, nil
}

func resolveForgejoBootstrapAuth(ctx context.Context, opts bootstrapOptions, baseURL string) (config.ProviderAuthMode, string, string, error) {
	authFlag := strings.TrimSpace(opts.ForgejoAuth)
	tokenEnv := strings.TrimSpace(opts.ForgejoTokenEnv)
	teaLogin := strings.TrimSpace(opts.ForgejoTeaLogin)
	// Fail closed on mixed strategies before any branch can silently drop a credential.
	if err := rejectMixedForgejoAuthFlags(authFlag, tokenEnv, teaLogin); err != nil {
		return "", "", "", err
	}
	switch {
	case authFlag == string(config.ProviderAuthTea) || (authFlag == "" && teaLogin != "" && tokenEnv == ""):
		if teaLogin == "" {
			return "", "", "", fmt.Errorf("--tea-login is required when auth is tea (bootstrap is non-interactive for multi-login hosts; pass the login explicitly)")
		}
		provider := config.ProviderConfig{
			ID: "probe", Kind: config.ProviderKindForgejo, BaseURL: baseURL,
			Auth: config.ProviderAuthTea, TeaLogin: stringPtr(teaLogin),
		}
		if _, _, err := forge.ValidateTeaLoginForProvider(ctx, provider, nil, nil); err != nil {
			return "", "", "", err
		}
		return config.ProviderAuthTea, "", teaLogin, nil
	case authFlag == string(config.ProviderAuthTokenEnv) || (authFlag == "" && tokenEnv != "" && teaLogin == ""):
		if !environmentNamePattern.MatchString(tokenEnv) {
			return "", "", "", fmt.Errorf("--forgejo-token-env must name a valid environment variable")
		}
		if strings.TrimSpace(os.Getenv(tokenEnv)) == "" {
			return "", "", "", fmt.Errorf("environment variable %s is not set; export the Forgejo token and rerun bootstrap", tokenEnv)
		}
		return config.ProviderAuthTokenEnv, tokenEnv, "", nil
	case authFlag == "" && tokenEnv == "" && teaLogin == "":
		return "", "", "", fmt.Errorf("provide --auth tea --tea-login <name> or --forgejo-token-env <ENV>")
	case authFlag != "" && authFlag != string(config.ProviderAuthTea) && authFlag != string(config.ProviderAuthTokenEnv):
		return "", "", "", fmt.Errorf("--auth must be %q or %q", config.ProviderAuthTea, config.ProviderAuthTokenEnv)
	default:
		return "", "", "", fmt.Errorf("choose one authentication strategy: --auth tea --tea-login <name> or --auth token-env --forgejo-token-env <ENV>")
	}
}

type bootstrapOriginRemote struct {
	Scheme string
	Host   string
	Path   string
	Repo   string
}

var bootstrapSCPRemotePattern = regexp.MustCompile(`^(?:[^@/:]+@)?(\[[^]]+\]|[^/:]+):(.+)$`)

func (r *commandRuntime) detectBootstrapOriginRemote(ctx context.Context, projectPath string) (bootstrapOriginRemote, error) {
	gitPath, err := r.lookPath()("git")
	if err != nil || strings.TrimSpace(gitPath) == "" {
		gitPath = "git"
	}
	result, err := r.runCommand(ctx, gitPath, []string{"-C", projectPath, "config", "--get", "remote.origin.url"}, 3*time.Second)
	if err != nil || result.ExitCode != 0 {
		return bootstrapOriginRemote{}, fmt.Errorf("read git origin for %s", projectPath)
	}
	remote, err := parseBootstrapRemote(strings.TrimSpace(result.Stdout))
	if err != nil {
		return bootstrapOriginRemote{}, err
	}
	return remote, nil
}

func parseBootstrapRemote(value string) (bootstrapOriginRemote, error) {
	trimmed := strings.TrimSpace(strings.TrimSuffix(value, ".git"))
	if trimmed == "" {
		return bootstrapOriginRemote{}, fmt.Errorf("git origin is empty")
	}
	var scheme, host, path string
	if match := bootstrapSCPRemotePattern.FindStringSubmatch(trimmed); !strings.Contains(trimmed, "://") && match != nil {
		scheme = "ssh"
		host, path = match[1], match[2]
	} else {
		parsed, err := url.Parse(trimmed)
		if err != nil || parsed.Hostname() == "" {
			return bootstrapOriginRemote{}, fmt.Errorf("unsupported git origin URL")
		}
		scheme, host, path = strings.ToLower(parsed.Scheme), parsed.Host, parsed.Path
	}
	remotePath := strings.Trim(path, "/")
	parts := strings.Split(remotePath, "/")
	if host == "" || len(parts) < 2 || parts[len(parts)-2] == "" || parts[len(parts)-1] == "" {
		return bootstrapOriginRemote{}, fmt.Errorf("git origin must identify owner/repo")
	}
	return bootstrapOriginRemote{Scheme: scheme, Host: strings.ToLower(host), Path: remotePath, Repo: parts[len(parts)-2] + "/" + parts[len(parts)-1]}, nil
}

func validateForgejoBaseURL(value string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("--forgejo-url must be an absolute http(s) URL without credentials, query, or fragment")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	return strings.TrimRight(parsed.String(), "/"), nil
}

func forgejoRemoteMatchesBaseURL(remote bootstrapOriginRemote, baseURL string) bool {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return false
	}
	remoteURL, err := url.Parse("//" + strings.TrimSpace(remote.Host))
	if err != nil || remoteURL.Hostname() == "" {
		return false
	}
	remoteHost := strings.TrimPrefix(strings.ToLower(remoteURL.Hostname()), "ssh.")
	providerHost := strings.TrimPrefix(strings.ToLower(parsed.Hostname()), "www.")
	if strings.TrimPrefix(remoteHost, "www.") != providerHost {
		return false
	}
	if remote.Scheme != "ssh" && forgejoURLPort(remote.Scheme, remoteURL.Port()) != forgejoURLPort(parsed.Scheme, parsed.Port()) {
		return false
	}
	if remote.Scheme == "ssh" {
		return len(strings.Split(remote.Path, "/")) == 2
	}
	basePath := strings.Trim(strings.TrimSpace(parsed.Path), "/")
	if basePath == "" {
		return len(strings.Split(remote.Path, "/")) == 2
	}
	return strings.HasPrefix(remote.Path, basePath+"/") && len(strings.Split(strings.TrimPrefix(remote.Path, basePath+"/"), "/")) == 2
}

func forgejoURLPort(scheme, port string) string {
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		return ""
	}
	return port
}

func forgejoBaseURLsMatch(first, second string) bool {
	firstURL, firstErr := url.Parse(first)
	secondURL, secondErr := url.Parse(second)
	if firstErr != nil || secondErr != nil || firstURL.User != nil || secondURL.User != nil {
		return false
	}
	return strings.EqualFold(firstURL.Scheme, secondURL.Scheme) &&
		strings.EqualFold(firstURL.Hostname(), secondURL.Hostname()) &&
		forgejoURLPort(strings.ToLower(firstURL.Scheme), firstURL.Port()) == forgejoURLPort(strings.ToLower(secondURL.Scheme), secondURL.Port()) &&
		strings.TrimRight(firstURL.Path, "/") == strings.TrimRight(secondURL.Path, "/") &&
		firstURL.RawQuery == secondURL.RawQuery && firstURL.Fragment == secondURL.Fragment
}

// detectBootstrapOriginRepo best-effort resolves owner/repo from the git origin
// remote of projectPath. Returns "" on any failure (caller falls back to
// requiring --code-repo).
func (r *commandRuntime) detectBootstrapOriginRepo(ctx context.Context, projectPath string) string {
	gitPath, err := r.lookPath()("git")
	if err != nil || strings.TrimSpace(gitPath) == "" {
		gitPath = "git"
	}
	result, err := r.runCommand(ctx, gitPath, []string{"-C", projectPath, "config", "--get", "remote.origin.url"}, 3*time.Second)
	if err != nil || result.ExitCode != 0 {
		return ""
	}
	return parseGitHubRepoSlug(strings.TrimSpace(result.Stdout))
}

// parseGitHubRepoSlug extracts "owner/repo" from a github.com remote URL
// (git@github.com:owner/repo.git, https://github.com/owner/repo.git, or ssh).
func parseGitHubRepoSlug(remoteURL string) string {
	remoteURL = strings.TrimSpace(remoteURL)
	if remoteURL == "" {
		return ""
	}
	remoteURL = strings.TrimSuffix(remoteURL, ".git")
	var path string
	switch {
	case strings.HasPrefix(remoteURL, "git@"):
		if _, after, ok := strings.Cut(remoteURL, ":"); ok {
			path = after
		}
	case strings.Contains(remoteURL, "github.com/"):
		if _, after, ok := strings.Cut(remoteURL, "github.com/"); ok {
			path = after
		}
	}
	path = strings.Trim(strings.TrimSpace(path), "/")
	parts := strings.Split(path, "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return ""
	}
	return parts[0] + "/" + parts[1]
}

func (r *commandRuntime) bootstrapPreflight(ctx context.Context, configPath string, plan *bootstrapConfigPlan) ([]string, error) {
	if _, err := resolveLooperdTarget(r.platform(), r.arch()); err != nil {
		return nil, err
	}

	configured, err := r.bootstrapConfiguredToolPaths(configPath)
	if err != nil {
		return nil, err
	}
	detected := config.DetectToolPaths(configured, r.lookPath())
	missing := make([]string, 0)
	if detected.Paths.GitPath == nil || strings.TrimSpace(*detected.Paths.GitPath) == "" {
		missing = append(missing, "git")
	}
	if plan.Provider != bootstrapProviderForgejo && (detected.Paths.GHPath == nil || strings.TrimSpace(*detected.Paths.GHPath) == "") {
		missing = append(missing, "gh")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("bootstrap preflight failed: missing required tools: %s. Install them manually (for example: brew install git gh) and rerun `looper bootstrap`", strings.Join(missing, ", "))
	}

	notes := make([]string, 0)
	if plan.EnableOsascript && (detected.Paths.OsascriptPath == nil || strings.TrimSpace(*detected.Paths.OsascriptPath) == "") {
		plan.EnableOsascript = false
		notes = append(notes, "osascript was not detected; notifications.osascript.enabled will remain disabled")
	}
	if plan.Provider != bootstrapProviderForgejo && detected.Paths.GHPath != nil {
		result, err := r.runCommand(ctx, *detected.Paths.GHPath, []string{"auth", "status"}, 3*time.Second)
		if err != nil || result.ExitCode != 0 {
			notes = append(notes, "gh auth status is not ready yet; run `gh auth login` if you plan to use GitHub integration")
		}
	}
	return notes, nil
}

func (r *commandRuntime) daemonStartForBootstrap(cmd *cobra.Command) error {
	if !getBoolFlag(cmd, "json") {
		return r.daemonStart(cmd, nil)
	}

	originalOut := cmd.OutOrStdout()
	cmd.SetOut(io.Discard)
	defer cmd.SetOut(originalOut)
	return r.daemonStart(cmd, nil)
}

func (r *commandRuntime) daemonRestartForBootstrap(cmd *cobra.Command) error {
	if !getBoolFlag(cmd, "json") {
		return r.daemonRestart(cmd, nil)
	}

	originalOut := cmd.OutOrStdout()
	cmd.SetOut(io.Discard)
	defer cmd.SetOut(originalOut)
	return r.daemonRestart(cmd, nil)
}

func (r *commandRuntime) bootstrapConfiguredToolPaths(configPath string) (config.ToolPathsConfig, error) {
	configured := config.ToolPathsConfig{}
	if partial, err := readBootstrapPartialConfigIfPresent(configPath); err != nil {
		return config.ToolPathsConfig{}, err
	} else if partial.Tools != nil {
		configured.GitPath = cloneBootstrapStringPtr(partial.Tools.GitPath)
		configured.GHPath = cloneBootstrapStringPtr(partial.Tools.GHPath)
		configured.OsascriptPath = cloneBootstrapStringPtr(partial.Tools.OsascriptPath)
	}

	if value, ok := os.LookupEnv("LOOPER_GIT_PATH"); ok {
		configured.GitPath = stringPtr(value)
	}
	if value, ok := os.LookupEnv("LOOPER_GH_PATH"); ok {
		configured.GHPath = stringPtr(value)
	}
	if value, ok := os.LookupEnv("LOOPER_OSASCRIPT_PATH"); ok {
		configured.OsascriptPath = stringPtr(value)
	}

	args := ExtractConfigArgs(r.argv)
	if value := strings.TrimSpace(extractToolPathOverride(args, "git-path")); value != "" {
		configured.GitPath = stringPtr(value)
	}
	if value := strings.TrimSpace(extractToolPathOverride(args, "gh-path")); value != "" {
		configured.GHPath = stringPtr(value)
	}
	if value := strings.TrimSpace(extractToolPathOverride(args, "osascript-path")); value != "" {
		configured.OsascriptPath = stringPtr(value)
	}
	return configured, nil
}

func readBootstrapPartialConfigIfPresent(path string) (config.PartialConfig, error) {
	partial, err := readBootstrapPartialConfig(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return config.PartialConfig{}, nil
		}
		return config.PartialConfig{}, err
	}
	return partial, nil
}

func cloneBootstrapStringPtr(value *string) *string {
	if value == nil {
		return nil
	}
	return stringPtr(*value)
}

func extractToolPathOverride(args []string, flag string) string {
	prefix := "--" + flag
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if !strings.HasPrefix(arg, prefix) {
			continue
		}
		if _, value, ok := strings.Cut(arg, "="); ok {
			return value
		}
		if index+1 < len(args) {
			return args[index+1]
		}
	}
	return ""
}

func (r *commandRuntime) lookPath() config.LookPathFunc {
	if r.app.deps.LookPath != nil {
		return config.LookPathFunc(r.app.deps.LookPath)
	}
	return config.LookPathFunc(execLookPath)
}

var execLookPath = func(file string) (string, error) {
	return exec.LookPath(file)
}

func (r *commandRuntime) ensureBootstrapDirectories() error {
	homeDir, err := r.homeDir()
	if err != nil {
		return err
	}
	for _, path := range []string{
		filepath.Join(homeDir, ".looper", "bin"),
		filepath.Join(homeDir, ".looper", "backups"),
		filepath.Join(homeDir, ".looper", "logs"),
	} {
		if err := r.mkdirAll(path, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", path, err)
		}
	}
	return nil
}

func (r *commandRuntime) ensureBootstrapConfig(configPath string, cwd string, plan bootstrapConfigPlan) (bool, bool, error) {
	if err := r.mkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		return false, false, fmt.Errorf("create config directory: %w", err)
	}
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		cfg, err := config.DefaultConfig(cwd)
		if err != nil {
			return false, false, fmt.Errorf("build default config: %w", err)
		}
		applyBootstrapPlan(&cfg, plan)
		if err := writeBootstrapConfig(configPath, cfg); err != nil {
			return false, false, err
		}
		return true, plan.ProjectPath != "", nil
	} else if err != nil {
		return false, false, fmt.Errorf("check config path %s: %w", configPath, err)
	}

	partial, err := readBootstrapPartialConfig(configPath)
	if err != nil {
		return false, false, err
	}

	if plan.ProjectPath == "" {
		return false, false, nil
	}
	normalized, err := config.Normalize(cwd, partial)
	if err != nil {
		return false, false, err
	}
	if err := config.Validate(normalized); err != nil {
		return false, false, err
	}
	if plan.Provider == bootstrapProviderForgejo {
		providerExists := false
		for _, provider := range normalized.Providers {
			if provider.ID == plan.ForgejoProviderID {
				providerExists = true
				candidate := forgejoProviderConfig(plan.ForgejoProviderID, plan.ForgejoURL, plan.ForgejoAuth, plan.ForgejoTokenEnv, plan.ForgejoTeaLogin)
				if !forgejoProvidersEquivalent(provider, candidate) {
					return false, false, fmt.Errorf("provider id %q already exists with different settings; choose a different --forgejo-provider-id", plan.ForgejoProviderID)
				}
				break
			}
		}
		for _, project := range normalized.Projects {
			if !samePath(project.RepoPath, plan.ProjectPath) {
				continue
			}
			if project.Provider != plan.ForgejoProviderID || project.Repo != plan.Repo {
				return false, false, fmt.Errorf("project %q already exists for %s but is not bound to Forgejo provider %q repository %q; remove or rebind the project first", project.ID, plan.ProjectPath, plan.ForgejoProviderID, plan.Repo)
			}
			return false, false, nil
		}
		if !providerExists {
			providers := []config.PartialProviderConfig{}
			if partial.Providers != nil {
				providers = append(providers, (*partial.Providers)...)
			}
			providers = append(providers, partialForgejoProvider(plan.ForgejoProviderID, plan.ForgejoURL, plan.ForgejoAuth, plan.ForgejoTokenEnv, plan.ForgejoTeaLogin))
			partial.Providers = &providers
		}
	} else if hasBootstrapProject(normalized.Projects, plan.ProjectPath) {
		return false, false, nil
	}
	projects := []config.PartialProjectRefConfig{}
	if partial.Projects != nil {
		projects = append(projects, (*partial.Projects)...)
	}
	project := buildBootstrapProject(plan.ProjectPath, normalized.Defaults.BaseBranch)
	if plan.Provider == bootstrapProviderForgejo {
		project.Provider = plan.ForgejoProviderID
		project.Repo = plan.Repo
	}
	projects = append(projects, partialProjectFromConfig(project))
	partial.Projects = &projects
	updated, err := config.Normalize(cwd, partial)
	if err != nil {
		return false, false, err
	}
	if err := config.Validate(updated); err != nil {
		return false, false, err
	}
	if err := writeBootstrapPartialConfig(configPath, partial); err != nil {
		return false, false, err
	}
	return false, true, nil
}

func applyBootstrapPlan(cfg *config.Config, plan bootstrapConfigPlan) {
	if plan.AgentVendor != nil {
		vendor := *plan.AgentVendor
		cfg.Agent.Vendor = &vendor
	}
	cfg.Notifications.Osascript.Enabled = plan.EnableOsascript
	if plan.EnableLocalToken {
		authMode := config.AuthModeLocalToken
		cfg.Server.AuthMode = authMode
		token := bootstrapLocalToken()
		cfg.Server.LocalToken = &token
	}
	if strings.TrimSpace(plan.FeishuWebhookEnv) != "" {
		cfg.Notifications.Webhook = config.WebhookNotificationConfig{
			Enabled: true,
			URLEnv:  plan.FeishuWebhookEnv,
			Format:  "feishu",
			Levels: []config.NotificationSoundLevel{
				config.NotificationSoundLevelActionRequired,
				config.NotificationSoundLevelFailure,
			},
			ThrottleWindowSeconds: cfg.Notifications.Webhook.ThrottleWindowSeconds,
		}
	}
	if plan.Provider == bootstrapProviderPlane {
		applyPlaneBootstrapPlan(cfg, plan)
		return
	}
	if plan.Provider == bootstrapProviderForgejo {
		applyForgejoBootstrapPlan(cfg, plan)
		return
	}
	if plan.ProjectPath != "" {
		cfg.Projects = append(cfg.Projects, buildBootstrapProject(plan.ProjectPath, cfg.Defaults.BaseBranch))
	}
}

func applyForgejoBootstrapPlan(cfg *config.Config, plan bootstrapConfigPlan) {
	cfg.Providers = append(cfg.Providers, forgejoProviderConfig(plan.ForgejoProviderID, plan.ForgejoURL, plan.ForgejoAuth, plan.ForgejoTokenEnv, plan.ForgejoTeaLogin))
	project := buildBootstrapProject(plan.ProjectPath, cfg.Defaults.BaseBranch)
	project.Provider = plan.ForgejoProviderID
	project.Repo = plan.Repo
	config.ApplyForgejoProjectProfile(&project)
	cfg.Projects = append(cfg.Projects, project)
}

// applyPlaneBootstrapPlan wires a Plane task-source provider + a project bound to
// it (with the GitHub code repo for PRs) + planner/worker discovery on the
// configured trigger label. Plane assignees are UUIDs (not GitHub logins), so
// discovery keys on the label only (requireAssigneeCurrentUser=false).
func applyPlaneBootstrapPlan(cfg *config.Config, plan bootstrapConfigPlan) {
	providerID := planeBootstrapProviderID(plan.PlaneWorkspace)
	cfg.Providers = append(cfg.Providers, config.ProviderConfig{
		ID:        providerID,
		Kind:      config.ProviderKindPlane,
		BaseURL:   plan.PlaneBaseURL,
		TokenEnv:  stringPtr(plan.PlaneTokenEnv),
		Workspace: stringPtr(plan.PlaneWorkspace),
		ProjectID: stringPtr(plan.PlaneProject),
	})

	projectName := filepath.Base(plan.ProjectPath)
	if strings.TrimSpace(projectName) == "" || projectName == "." || projectName == string(filepath.Separator) {
		projectName = providerID
	}
	cfg.Projects = append(cfg.Projects, config.ProjectRefConfig{
		ID:         deriveBootstrapProjectID(plan.ProjectPath),
		Name:       projectName,
		Provider:   providerID,
		Repo:       plan.CodeRepo,
		RepoPath:   plan.ProjectPath,
		BaseBranch: stringPtr(cfg.Defaults.BaseBranch),
	})

	label := plan.TriggerLabel
	if strings.TrimSpace(label) == "" {
		label = defaultBootstrapTriggerText
	}
	cfg.Roles.Planner.Triggers.Labels = []string{label}
	cfg.Roles.Planner.Triggers.LabelMode = config.LabelModeAll
	cfg.Roles.Planner.Triggers.RequireAssigneeCurrentUser = false
	cfg.Roles.Worker.Triggers.Labels = []string{label}
	cfg.Roles.Worker.Triggers.LabelMode = config.LabelModeAll
	cfg.Roles.Worker.Triggers.RequireAssigneeCurrentUser = false
}

func planeBootstrapProviderID(workspace string) string {
	slug := deriveBootstrapProjectID(workspace)
	if slug == "" || slug == "project" {
		return "plane"
	}
	return "plane-" + slug
}

func writeBootstrapConfig(path string, cfg config.Config) error {
	if err := config.Validate(cfg); err != nil {
		return err
	}
	raw, err := config.MarshalConfigFile(path, cfg)
	if err != nil {
		return fmt.Errorf("write bootstrap config: %w", err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return fmt.Errorf("write bootstrap config: %w", err)
	}
	return nil
}

func readBootstrapPartialConfig(path string) (config.PartialConfig, error) {
	partial, present, err := config.ReadPartialConfigFile(path)
	if err != nil {
		return config.PartialConfig{}, fmt.Errorf("read bootstrap config: %w", err)
	}
	if !present {
		return config.PartialConfig{}, fmt.Errorf("read bootstrap config: %w", os.ErrNotExist)
	}
	return partial, nil
}

func partialProjectFromConfig(project config.ProjectRefConfig) config.PartialProjectRefConfig {
	return config.PartialProjectRefConfig{
		ID:           project.ID,
		Name:         project.Name,
		Provider:     stringPtrIfSet(project.Provider),
		Repo:         stringPtrIfSet(project.Repo),
		RepoPath:     project.RepoPath,
		Path:         project.Path,
		BaseBranch:   project.BaseBranch,
		WorktreeRoot: project.WorktreeRoot,
		Roles:        project.Roles,
	}
}

func stringPtrIfSet(value string) *string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return stringPtr(value)
}

func writeBootstrapPartialConfig(path string, partial config.PartialConfig) error {
	raw, err := config.MarshalConfigFile(path, partial)
	if err != nil {
		return fmt.Errorf("write bootstrap config: %w", err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return fmt.Errorf("write bootstrap config: %w", err)
	}
	return nil
}

func hasBootstrapProject(projects []config.ProjectRefConfig, projectPath string) bool {
	for _, project := range projects {
		if samePath(project.RepoPath, projectPath) {
			return true
		}
	}
	return false
}

func samePath(left string, right string) bool {
	leftClean := filepath.Clean(left)
	rightClean := filepath.Clean(right)
	return leftClean == rightClean
}

func buildBootstrapProject(projectPath string, baseBranch string) config.ProjectRefConfig {
	projectID := deriveBootstrapProjectID(projectPath)
	projectName := filepath.Base(projectPath)
	if projectName == "." || projectName == string(filepath.Separator) || strings.TrimSpace(projectName) == "" {
		projectName = projectID
	}
	return config.ProjectRefConfig{
		ID:         projectID,
		Name:       projectName,
		RepoPath:   projectPath,
		BaseBranch: stringPtr(baseBranch),
	}
}

func deriveBootstrapProjectID(projectPath string) string {
	base := strings.ToLower(filepath.Base(projectPath))
	var builder strings.Builder
	lastHyphen := false
	for _, r := range base {
		allowed := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if allowed {
			builder.WriteRune(r)
			lastHyphen = false
			continue
		}
		if !lastHyphen {
			builder.WriteRune('-')
			lastHyphen = true
		}
	}
	derived := strings.Trim(builder.String(), "-")
	if derived == "" {
		return "project"
	}
	return derived
}

func bootstrapLocalToken() string {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return fmt.Sprintf("bootstrap-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(raw)
}

func (r *commandRuntime) ensureBootstrapDaemon(ctx context.Context, force bool) (string, bool, error) {
	matchingTag := bootstrapDaemonReleaseTag()
	if force {
		result, err := r.installManagedDaemon(ctx, true, matchingTag, r.app.stderr())
		if err != nil {
			return "", false, fmt.Errorf("install managed daemon: %w", err)
		}
		if result.Skipped {
			return "already-installed", false, nil
		}
		return "reinstalled", true, nil
	}
	installed, err := r.readManagedDaemonVersion(ctx)
	if err != nil {
		return "", false, err
	}
	if !force && installed != nil && bootstrapDaemonVersionMatches(installed.Version) {
		return "already-installed", false, nil
	}
	reinstall := force || installed != nil
	result, err := r.installManagedDaemon(ctx, reinstall, matchingTag, r.app.stderr())
	if err != nil {
		return "", false, fmt.Errorf("install managed daemon: %w", err)
	}
	if result.Skipped {
		return "already-installed", false, nil
	}
	if reinstall {
		return "reinstalled", true, nil
	}
	return "installed", true, nil
}

func bootstrapDaemonVersionMatches(daemonVersion string) bool {
	cliVersion := strings.TrimSpace(version.Current().Version)
	if cliVersion == "" || cliVersion == "0.0.0-dev" || strings.Contains(cliVersion, "dev") {
		return strings.TrimSpace(daemonVersion) != ""
	}
	return strings.TrimPrefix(strings.TrimSpace(daemonVersion), "v") == strings.TrimPrefix(cliVersion, "v")
}

func bootstrapDaemonReleaseTag() string {
	cliVersion := strings.TrimSpace(version.Current().Version)
	if cliVersion == "" || cliVersion == "0.0.0-dev" || strings.Contains(cliVersion, "dev") {
		return ""
	}
	if strings.HasPrefix(cliVersion, "v") {
		return cliVersion
	}
	return "v" + cliVersion
}

func (r *commandRuntime) bootstrapAPIReachable(ctx context.Context, client *DaemonAPIClient) (bool, error) {
	_, err := r.getJSONWithClient(ctx, client, "/api/v1/status")
	if err == nil {
		return true, nil
	}
	if isBootstrapProbeContextError(err) {
		return false, err
	}
	_, healthErr := r.getJSONWithClient(ctx, client, "/api/v1/healthz")
	if healthErr == nil {
		return true, nil
	}
	if isBootstrapProbeContextError(healthErr) {
		return false, healthErr
	}
	if !isBootstrapProbeReachabilityError(healthErr) {
		return false, healthErr
	}
	return false, nil
}

func (r *commandRuntime) bootstrapAPIReachableForInstalled(ctx context.Context, client *DaemonAPIClient) (bool, error) {
	_, err := r.getJSONWithClient(ctx, client, "/api/v1/status")
	if err == nil {
		return true, nil
	}
	if isBootstrapProbeContextError(err) {
		return false, err
	}
	if !isBootstrapProbeReachabilityError(err) {
		return false, err
	}

	_, healthErr := r.getJSONWithClient(ctx, client, "/api/v1/healthz")
	if healthErr == nil {
		return true, nil
	}
	if isBootstrapProbeContextError(healthErr) {
		return false, healthErr
	}
	if !isBootstrapProbeReachabilityError(healthErr) {
		return true, nil
	}
	return false, nil
}

func (r *commandRuntime) bootstrapReachableDaemonMatches(ctx context.Context, client *DaemonAPIClient, expectedVersion string, managedDaemonPath string) (bool, error) {
	payload, err := r.getJSONWithClient(ctx, client, "/api/v1/status")
	if err != nil {
		if isBootstrapProbeContextError(err) {
			return false, err
		}
		return false, nil
	}
	return bootstrapDaemonPayloadMatchesManaged(payload, expectedVersion, managedDaemonPath), nil
}

func bootstrapDaemonPayloadMatchesManaged(payload json.RawMessage, expectedVersion string, managedDaemonPath string) bool {
	binary := extractDaemonServiceBinary(payload)
	if !bootstrapDaemonVersionMatchesExpected(binary.Version, expectedVersion) {
		return false
	}
	if strings.TrimSpace(binary.Path) == "" {
		return false
	}
	return bootstrapDaemonPathMatchesManaged(binary.Path, managedDaemonPath)
}

func bootstrapDaemonPathMatchesManaged(binaryPath string, managedDaemonPath string) bool {
	return canonicalBootstrapPath(binaryPath) == canonicalBootstrapPath(managedDaemonPath)
}

func canonicalBootstrapPath(path string) string {
	cleanPath := filepath.Clean(path)
	resolvedPath, err := filepath.EvalSymlinks(cleanPath)
	if err != nil {
		return cleanPath
	}
	return filepath.Clean(resolvedPath)
}

func bootstrapDaemonVersionMatchesExpected(daemonVersion string, expectedVersion string) bool {
	if strings.TrimSpace(expectedVersion) == "" {
		return bootstrapDaemonVersionMatches(daemonVersion)
	}
	return strings.TrimPrefix(strings.TrimSpace(daemonVersion), "v") == strings.TrimPrefix(strings.TrimSpace(expectedVersion), "v")
}

func (r *commandRuntime) bootstrapCanRestartReachableDaemon(ctx context.Context, client *DaemonAPIClient, loaded config.LoadedFileConfig) error {
	localClient := r.localAPIClientFromLoaded(loaded)
	if normalizeBootstrapBaseURL(client.baseURL) != normalizeBootstrapBaseURL(localClient.baseURL) {
		return fmt.Errorf("installed managed looperd, but the configured API endpoint is not the local daemon endpoint; stop the stale looperd process manually and rerun `looper bootstrap`")
	}

	pidFilePath, err := r.resolveDaemonPIDFilePath()
	if err != nil {
		return err
	}
	existingPID, ok := r.readPIDFile(pidFilePath)
	if !ok {
		return fmt.Errorf("installed managed looperd, but a stale looperd API is already reachable and no daemon pid file was found; stop the stale looperd process manually and rerun `looper bootstrap`")
	}
	if !r.isProcessAlive(existingPID) {
		return fmt.Errorf("installed managed looperd, but a stale looperd API is already reachable and the daemon pid file points to a stopped process; stop the stale looperd process manually and rerun `looper bootstrap`")
	}
	isLooperd, err := r.isLooperdProcess(ctx, existingPID)
	if err != nil {
		return err
	}
	if !isLooperd {
		return fmt.Errorf("installed managed looperd, but a stale looperd API is already reachable and the daemon pid file does not point to looperd; stop the stale looperd process manually and rerun `looper bootstrap`")
	}
	return nil
}

func normalizeBootstrapBaseURL(baseURL string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return trimmed
	}

	host := strings.ToLower(parsed.Hostname())
	if isBootstrapLoopbackHost(host) {
		host = "localhost"
	}
	if port := parsed.Port(); port != "" {
		host = net.JoinHostPort(host, port)
	}

	parsed.Scheme = strings.ToLower(parsed.Scheme)
	parsed.Host = host
	parsed.User = nil
	return strings.TrimRight(parsed.String(), "/")
}

func isBootstrapLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	parsedIP := net.ParseIP(host)
	return parsedIP != nil && parsedIP.IsLoopback()
}

func (r *commandRuntime) waitForBootstrapMatchingDaemon(ctx context.Context, client *DaemonAPIClient, expectedVersion string, managedDaemonPath string) (bool, error) {
	deadline := time.Now().Add(bootstrapHealthCheckTimeout)
	for time.Now().Before(deadline) {
		matches, err := r.bootstrapReachableDaemonMatches(ctx, client, expectedVersion, managedDaemonPath)
		if err != nil {
			return false, err
		}
		if matches {
			return true, nil
		}
		r.sleep(250 * time.Millisecond)
	}
	return false, fmt.Errorf("looperd is reachable but does not report the managed daemon version/path; stop the stale looperd process manually and rerun `looper bootstrap`")
}

func isBootstrapProbeContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func isBootstrapProbeReachabilityError(err error) bool {
	return strings.HasPrefix(err.Error(), "looperd is not reachable:")
}

func (r *commandRuntime) waitForBootstrapHealth(ctx context.Context, client *DaemonAPIClient) (bool, error) {
	deadline := time.Now().Add(bootstrapHealthCheckTimeout)
	for time.Now().Before(deadline) {
		reachable, err := r.bootstrapAPIReachable(ctx, client)
		if err != nil {
			return false, err
		}
		if reachable {
			return true, nil
		}
		r.sleep(250 * time.Millisecond)
	}
	return false, fmt.Errorf("looperd did not become healthy after bootstrap; rerun `looper bootstrap` to retry startup")
}

func bootstrapNextSteps(projectPath string) []string {
	steps := []string{"looper status"}
	if strings.TrimSpace(projectPath) == "" {
		steps = append(steps, "looper project add /path/to/repo")
	}
	return steps
}

func bootstrapNextStepsForPlan(plan bootstrapConfigPlan, restartRequired bool) []string {
	steps := bootstrapNextSteps(plan.ProjectPath)
	if plan.Provider == bootstrapProviderForgejo {
		if restartRequired {
			steps = append([]string{"looper daemon restart"}, steps...)
		}
		if plan.ForgejoAuth == config.ProviderAuthTea {
			steps = append([]string{fmt.Sprintf("ensure tea login %q remains valid", plan.ForgejoTeaLogin)}, steps...)
		} else if plan.ForgejoTokenEnv != "" {
			steps = append([]string{fmt.Sprintf("export %s=<forgejo-token>", plan.ForgejoTokenEnv)}, steps...)
		}
	}
	return steps
}

func writeHumanBootstrapResult(w io.Writer, result bootstrapResult) error {
	entries := [][2]any{{"configPath", result.ConfigPath}, {"configCreated", result.ConfigCreated}, {"projectAdded", result.ProjectAdded}}
	if result.ProviderID != "" {
		entries = append(entries, [2]any{"provider", result.ProviderID}, [2]any{"providerKind", result.ProviderKind}, [2]any{"repo", result.Repo}, [2]any{"identity", result.Identity})
	}
	entries = append(entries, [2]any{"managedDaemonPath", result.ManagedDaemonPath}, [2]any{"daemonInstallState", result.DaemonInstallState}, [2]any{"apiReachable", result.APIReachable})
	printSection(w, "Bootstrap complete", entries)
	if len(result.Notes) > 0 {
		_, _ = fmt.Fprintln(w)
		_, _ = fmt.Fprintln(w, "Notes:")
		for _, note := range result.Notes {
			_, _ = fmt.Fprintf(w, "- %s\n", note)
		}
	}
	if len(result.NextSteps) > 0 {
		_, _ = fmt.Fprintln(w)
		_, _ = fmt.Fprintln(w, "Next steps:")
		for _, step := range result.NextSteps {
			_, _ = fmt.Fprintf(w, "- %s\n", step)
		}
	}
	return nil
}

func promptBootstrapVendor(reader *bufio.Reader, w io.Writer) (*config.AgentVendor, error) {
	answer, err := promptBootstrapString(reader, w, "Agent vendor [claude-code/codex/opencode/cursor-cli/grok-build]", "")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(answer) == "" {
		return nil, nil
	}
	vendor := config.AgentVendor(strings.TrimSpace(answer))
	if !isSupportedBootstrapVendor(vendor) {
		return nil, fmt.Errorf("unsupported agent vendor %q", answer)
	}
	return &vendor, nil
}

func promptBootstrapBool(reader *bufio.Reader, w io.Writer, label string, defaultValue bool) (bool, error) {
	defaultText := "y/N"
	if defaultValue {
		defaultText = "Y/n"
	}
	answer, err := promptBootstrapString(reader, w, label+" ["+defaultText+"]", "")
	if err != nil {
		return false, err
	}
	trimmed := strings.ToLower(strings.TrimSpace(answer))
	if trimmed == "" {
		return defaultValue, nil
	}
	if trimmed == "y" || trimmed == "yes" {
		return true, nil
	}
	if trimmed == "n" || trimmed == "no" {
		return false, nil
	}
	return false, fmt.Errorf("invalid answer %q", answer)
}

func promptBootstrapString(reader *bufio.Reader, w io.Writer, label string, defaultValue string) (string, error) {
	if defaultValue != "" {
		if _, err := fmt.Fprintf(w, "%s [%s]: ", label, defaultValue); err != nil {
			return "", err
		}
	} else {
		if _, err := fmt.Fprintf(w, "%s: ", label); err != nil {
			return "", err
		}
	}
	line, err := reader.ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return defaultValue, nil
	}
	return trimmed, nil
}

func isSupportedBootstrapVendor(vendor config.AgentVendor) bool {
	switch vendor {
	case config.AgentVendorClaudeCode, config.AgentVendorCodex, config.AgentVendorOpenCode, config.AgentVendorCursorCLI, config.AgentVendorGrokBuild:
		return true
	default:
		return false
	}
}
