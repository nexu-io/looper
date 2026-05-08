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
	"runtime"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/config"
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
}

type bootstrapOptions struct {
	Yes              bool
	Force            bool
	AgentVendor      string
	ProjectPath      string
	EnableLocalToken bool
	DisableOsascript bool
}

type bootstrapConfigPlan struct {
	AgentVendor      *config.AgentVendor
	EnableOsascript  bool
	EnableLocalToken bool
	ProjectPath      string
}

func (r *commandRuntime) bootstrap(cmd *cobra.Command, args []string) error {
	_ = args

	ctx := cmd.Context()
	opts := bootstrapOptions{
		Yes:              getBoolFlag(cmd, "yes"),
		Force:            getBoolFlag(cmd, "force"),
		AgentVendor:      strings.TrimSpace(getStringFlag(cmd, "agent-vendor")),
		ProjectPath:      strings.TrimSpace(getStringFlag(cmd, "project-path")),
		EnableLocalToken: getBoolFlag(cmd, "enable-local-token"),
		DisableOsascript: getBoolFlag(cmd, "disable-osascript"),
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
	result.NextSteps = bootstrapNextSteps(planned.ProjectPath)
	return result, nil
}

func (r *commandRuntime) resolveBootstrapConfigPath(cwd string) (string, error) {
	if override := strings.TrimSpace(extractConfigPathOverride(ExtractConfigArgs(r.argv))); override != "" {
		return config.ResolveConfigPath(override, cwd), nil
	}
	if override, ok := os.LookupEnv("LOOPER_CONFIG"); ok && strings.TrimSpace(override) != "" {
		return config.ResolveConfigPath(strings.TrimSpace(override), cwd), nil
	}
	defaultConfigPath, err := config.DefaultConfigPath()
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

	configPath, err := r.resolveBootstrapConfigPath(cwd)
	if err != nil {
		return bootstrapConfigPlan{}, nil, err
	}
	if _, err := os.Stat(configPath); err == nil {
		return plan, nil, nil
	} else if !os.IsNotExist(err) {
		return bootstrapConfigPlan{}, nil, fmt.Errorf("check config path %s: %w", configPath, err)
	}

	if opts.Yes {
		return plan, nil, nil
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

	return plan, nil, nil
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
	if detected.Paths.GHPath == nil || strings.TrimSpace(*detected.Paths.GHPath) == "" {
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
	if detected.Paths.GHPath != nil {
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
	if hasBootstrapProject(normalized.Projects, plan.ProjectPath) {
		return false, false, nil
	}
	projects := []config.ProjectRefConfig{}
	if partial.Projects != nil {
		projects = append(projects, (*partial.Projects)...)
	}
	projects = append(projects, buildBootstrapProject(plan.ProjectPath, normalized.Defaults.BaseBranch))
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
	if plan.ProjectPath != "" {
		cfg.Projects = append(cfg.Projects, buildBootstrapProject(plan.ProjectPath, cfg.Defaults.BaseBranch))
	}
}

func writeBootstrapConfig(path string, cfg config.Config) error {
	if err := config.Validate(cfg); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal bootstrap config: %w", err)
	}
	raw = append(raw, '\n')
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return fmt.Errorf("write bootstrap config: %w", err)
	}
	return nil
}

func readBootstrapPartialConfig(path string) (config.PartialConfig, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return config.PartialConfig{}, fmt.Errorf("read bootstrap config: %w", err)
	}
	var partial config.PartialConfig
	if err := json.Unmarshal(raw, &partial); err != nil {
		return config.PartialConfig{}, fmt.Errorf("parse bootstrap config: %w", err)
	}
	return partial, nil
}

func writeBootstrapPartialConfig(path string, partial config.PartialConfig) error {
	raw, err := json.MarshalIndent(partial, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal bootstrap config: %w", err)
	}
	raw = append(raw, '\n')
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

func writeHumanBootstrapResult(w io.Writer, result bootstrapResult) error {
	printSection(w, "Bootstrap complete", [][2]any{{"configPath", result.ConfigPath}, {"configCreated", result.ConfigCreated}, {"projectAdded", result.ProjectAdded}, {"managedDaemonPath", result.ManagedDaemonPath}, {"daemonInstallState", result.DaemonInstallState}, {"apiReachable", result.APIReachable}})
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
	answer, err := promptBootstrapString(reader, w, "Agent vendor [claude-code/codex/opencode/cursor-cli]", "")
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
	case config.AgentVendorClaudeCode, config.AgentVendorCodex, config.AgentVendorOpenCode, config.AgentVendorCursorCLI:
		return true
	default:
		return false
	}
}
