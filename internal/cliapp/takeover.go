package cliapp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/spf13/cobra"
)

// takeoverProjectSyncTimeout bounds how long takeover waits for the daemon to
// register the scoped project after (re)starting.
const takeoverProjectSyncTimeout = 10 * time.Second

// takeoverVendorBinaries maps each supported agent vendor to the CLI binary
// looper spawns for it (see internal/agent/executor.go resolveCommand). Only
// vendors with an unambiguous binary name participate in auto-detection;
// cursor-cli spawns the generic "agent" binary and is intentionally excluded to
// avoid false positives, so cursor users pass --agent-vendor explicitly.
var takeoverVendorBinaries = []struct {
	vendor config.AgentVendor
	binary string
}{
	{config.AgentVendorClaudeCode, "claude"},
	{config.AgentVendorCodex, "codex"},
	{config.AgentVendorOpenCode, "opencode"},
	{config.AgentVendorGrokBuild, "grok"},
}

type takeoverOptions struct {
	PRRef       string
	AgentVendor string
	Merge       bool
	Yes         bool
	NoFix       bool
	JSON        bool
}

type takeoverLoopResult struct {
	ID     string `json:"id"`
	Seq    int64  `json:"seq"`
	Type   string `json:"type"`
	Status string `json:"status"`
}

type takeoverResult struct {
	Repo          string              `json:"repo"`
	PRNumber      int64               `json:"prNumber"`
	ProjectID     string              `json:"projectId"`
	AgentVendor   string              `json:"agentVendor"`
	AutoMerge     bool                `json:"autoMerge"`
	ConfigPath    string              `json:"configPath"`
	DaemonRunning bool                `json:"daemonRunning"`
	Reviewer      *takeoverLoopResult `json:"reviewer,omitempty"`
	Fixer         *takeoverLoopResult `json:"fixer,omitempty"`
	NextSteps     []string            `json:"nextSteps"`
	Notes         []string            `json:"notes,omitempty"`
}

func (r *commandRuntime) takeover(cmd *cobra.Command, args []string) error {
	opts := takeoverOptions{
		AgentVendor: strings.TrimSpace(getStringFlag(cmd, "agent-vendor")),
		Merge:       getBoolFlag(cmd, "merge"),
		Yes:         getBoolFlag(cmd, "yes"),
		NoFix:       getBoolFlag(cmd, "no-fix"),
		JSON:        getBoolFlag(cmd, "json"),
	}
	if len(args) > 0 {
		opts.PRRef = strings.TrimSpace(args[0])
	}

	result, err := r.runTakeover(cmd.Context(), cmd, opts)
	if err != nil {
		return err
	}
	if opts.JSON {
		return writeJSON(cmd.OutOrStdout(), result)
	}
	return writeHumanTakeoverResult(cmd.OutOrStdout(), result)
}

func (r *commandRuntime) runTakeover(ctx context.Context, cmd *cobra.Command, opts takeoverOptions) (takeoverResult, error) {
	cwd, err := r.getwd()
	if err != nil {
		return takeoverResult{}, fmt.Errorf("determine current working directory: %w", err)
	}

	repoRoot, err := r.takeoverRepoRoot(ctx)
	if err != nil {
		return takeoverResult{}, err
	}

	repo, prNumber, err := r.resolveTakeoverTarget(ctx, opts.PRRef, repoRoot)
	if err != nil {
		return takeoverResult{}, err
	}

	vendor, vendorNote, err := r.resolveTakeoverVendor(cmd, opts)
	if err != nil {
		return takeoverResult{}, err
	}

	result := takeoverResult{
		Repo:        repo,
		PRNumber:    prNumber,
		AgentVendor: string(vendor),
		AutoMerge:   opts.Merge,
	}
	if vendorNote != "" {
		result.Notes = append(result.Notes, vendorNote)
	}

	configPath, err := r.resolveBootstrapConfigPath(cwd)
	if err != nil {
		return takeoverResult{}, err
	}
	result.ConfigPath = configPath

	plan := bootstrapConfigPlan{}
	preflightNotes, err := r.bootstrapPreflight(ctx, configPath, &plan)
	if err != nil {
		return takeoverResult{}, err
	}
	result.Notes = append(result.Notes, preflightNotes...)

	if err := r.ensureBootstrapDirectories(); err != nil {
		return takeoverResult{}, err
	}

	changed, err := r.ensureTakeoverConfig(configPath, cwd, repoRoot, vendor, opts.Merge)
	if err != nil {
		return takeoverResult{}, err
	}

	if _, _, err := r.ensureBootstrapDaemon(ctx, false); err != nil {
		return takeoverResult{}, err
	}

	daemonNote, err := r.ensureTakeoverDaemonRunning(ctx, cmd, changed)
	if err != nil {
		return takeoverResult{}, err
	}
	if daemonNote != "" {
		result.Notes = append(result.Notes, daemonNote)
	}
	result.DaemonRunning = true

	project, err := r.waitForTakeoverProject(ctx, repoRoot)
	if err != nil {
		return takeoverResult{}, err
	}
	result.ProjectID = project.ID

	reviewer, err := r.startTakeoverLoop(ctx, "reviewer", project.ID, repo, prNumber)
	if err != nil {
		return takeoverResult{}, err
	}
	result.Reviewer = reviewer

	if !opts.NoFix {
		fixer, err := r.startTakeoverLoop(ctx, "fixer", project.ID, repo, prNumber)
		if err != nil {
			return takeoverResult{}, err
		}
		result.Fixer = fixer
	}

	entry := takeoverStateEntry{
		Repo:        repo,
		PRNumber:    prNumber,
		ProjectID:   project.ID,
		AgentVendor: string(vendor),
		AutoMerge:   opts.Merge,
		StartedAt:   time.Now().UTC().Format(time.RFC3339),
	}
	if result.Reviewer != nil {
		entry.ReviewerLoopID = result.Reviewer.ID
	}
	if result.Fixer != nil {
		entry.FixerLoopID = result.Fixer.ID
	}
	if err := r.recordTakeover(entry); err != nil {
		result.Notes = append(result.Notes, fmt.Sprintf("could not record takeover state (%v); `looper takeover list`/`stop` may not see this run", err))
	}

	result.NextSteps = takeoverNextSteps(repo, prNumber)
	if !opts.Merge {
		result.Notes = append(result.Notes, "auto-merge is disabled; rerun with --merge to let the reviewer enable auto-merge once the PR is approved and green")
	}
	return result, nil
}

// takeoverRepoRoot resolves the git repository root for the current directory.
func (r *commandRuntime) takeoverRepoRoot(ctx context.Context) (string, error) {
	gitPath, err := r.lookPath()("git")
	if err != nil {
		return "", fmt.Errorf("git is required for `looper takeover`: %w", err)
	}
	out, err := r.runCommand(ctx, gitPath, []string{"rev-parse", "--show-toplevel"}, 5*time.Second)
	if err != nil {
		return "", fmt.Errorf("resolve git repository root: %w", err)
	}
	if out.ExitCode != 0 {
		return "", fmt.Errorf("`looper takeover` must run inside a git repository (git rev-parse failed: %s)", strings.TrimSpace(out.Stderr))
	}
	root := strings.TrimSpace(out.Stdout)
	if root == "" {
		return "", fmt.Errorf("`looper takeover` must run inside a git repository")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve repository root path: %w", err)
	}
	return abs, nil
}

// resolveTakeoverTarget determines the owner/repo slug and PR number to take
// over. An explicit "<owner/repo>#<n>" or "<n>" ref wins; otherwise the PR for
// the current branch is discovered via gh.
func (r *commandRuntime) resolveTakeoverTarget(ctx context.Context, prRef string, repoRoot string) (string, int64, error) {
	if prRef != "" {
		repo, prNumber, repoQualified, err := parseOptionalPullRequestRef(prRef)
		if err != nil {
			return "", 0, err
		}
		if repoQualified {
			return repo, prNumber, nil
		}
		repo, err = r.detectTakeoverRepoSlug(ctx)
		if err != nil {
			return "", 0, err
		}
		return repo, prNumber, nil
	}

	repo, err := r.detectTakeoverRepoSlug(ctx)
	if err != nil {
		return "", 0, err
	}
	prNumber, err := r.detectTakeoverPRNumber(ctx)
	if err != nil {
		return "", 0, err
	}
	return repo, prNumber, nil
}

func (r *commandRuntime) detectTakeoverRepoSlug(ctx context.Context) (string, error) {
	ghPath, err := r.lookPath()("gh")
	if err != nil {
		return "", fmt.Errorf("gh is required to detect the repository: %w", err)
	}
	out, err := r.runCommand(ctx, ghPath, []string{"repo", "view", "--json", "nameWithOwner", "--jq", ".nameWithOwner"}, 15*time.Second)
	if err != nil {
		return "", fmt.Errorf("detect repository via gh: %w", err)
	}
	if out.ExitCode != 0 {
		return "", fmt.Errorf("could not detect the GitHub repository (gh repo view failed: %s). Pass an explicit <owner/repo>#<number>", strings.TrimSpace(out.Stderr))
	}
	repo := strings.TrimSpace(out.Stdout)
	if repo == "" {
		return "", fmt.Errorf("could not detect the GitHub repository; pass an explicit <owner/repo>#<number>")
	}
	return repo, nil
}

func (r *commandRuntime) detectTakeoverPRNumber(ctx context.Context) (int64, error) {
	ghPath, err := r.lookPath()("gh")
	if err != nil {
		return 0, fmt.Errorf("gh is required to detect the current pull request: %w", err)
	}
	out, err := r.runCommand(ctx, ghPath, []string{"pr", "view", "--json", "number", "--jq", ".number"}, 15*time.Second)
	if err != nil {
		return 0, fmt.Errorf("detect pull request via gh: %w", err)
	}
	if out.ExitCode != 0 {
		return 0, fmt.Errorf("no pull request found for the current branch (gh pr view failed: %s). Push the branch and open a PR, or pass an explicit <owner/repo>#<number>", strings.TrimSpace(out.Stderr))
	}
	prNumber, err := parsePositiveInt(strings.TrimSpace(out.Stdout), "pull request number")
	if err != nil {
		return 0, fmt.Errorf("parse detected pull request number: %w", err)
	}
	return prNumber, nil
}

// resolveTakeoverVendor decides which agent vendor to configure. An explicit
// --agent-vendor wins; otherwise it auto-detects installed agent CLIs and, when
// interactive, prompts. In non-interactive mode (--yes) an ambiguous or empty
// detection is a hard error so the run never silently picks the wrong agent.
func (r *commandRuntime) resolveTakeoverVendor(cmd *cobra.Command, opts takeoverOptions) (config.AgentVendor, string, error) {
	if opts.AgentVendor != "" {
		vendor := config.AgentVendor(opts.AgentVendor)
		if !isSupportedBootstrapVendor(vendor) {
			return "", "", fmt.Errorf("unsupported --agent-vendor %q (supported: claude-code, codex, opencode, cursor-cli, grok-build)", opts.AgentVendor)
		}
		return vendor, "", nil
	}

	if existing := r.takeoverConfiguredVendor(cmd); existing != "" {
		return existing, fmt.Sprintf("reusing agent vendor %q from existing config", existing), nil
	}

	detected := detectInstalledVendors(r.lookPath())
	if len(detected) == 1 {
		return detected[0], fmt.Sprintf("detected installed agent CLI: %s", detected[0]), nil
	}

	if opts.Yes {
		if len(detected) == 0 {
			return "", "", fmt.Errorf("no supported agent CLI detected on PATH; install one (claude, codex, opencode, or grok) or pass --agent-vendor")
		}
		return "", "", fmt.Errorf("multiple agent CLIs detected (%s); pass --agent-vendor to choose one", joinVendors(detected))
	}

	vendor, err := r.promptTakeoverVendor(cmd, detected)
	if err != nil {
		return "", "", err
	}
	return vendor, "", nil
}

// takeoverConfiguredVendor returns the agent vendor already present in the
// loaded config, if any, so an existing setup is reused rather than re-prompted.
func (r *commandRuntime) takeoverConfiguredVendor(cmd *cobra.Command) config.AgentVendor {
	_ = cmd
	loaded, err := r.loadConfig()
	if err != nil {
		return ""
	}
	if loaded.Config.Agent.Vendor == nil {
		return ""
	}
	return *loaded.Config.Agent.Vendor
}

func (r *commandRuntime) promptTakeoverVendor(cmd *cobra.Command, detected []config.AgentVendor) (config.AgentVendor, error) {
	reader := bufio.NewReader(cmd.InOrStdin())
	defaultValue := ""
	if len(detected) > 0 {
		defaultValue = string(detected[0])
	}
	label := "Agent vendor [claude-code/codex/opencode/cursor-cli/grok-build]"
	if len(detected) > 0 {
		label = fmt.Sprintf("Agent vendor (detected: %s) [claude-code/codex/opencode/cursor-cli/grok-build]", joinVendors(detected))
	}
	answer, err := promptBootstrapString(reader, cmd.OutOrStdout(), label, defaultValue)
	if err != nil {
		return "", err
	}
	answer = strings.TrimSpace(answer)
	if answer == "" {
		return "", fmt.Errorf("an agent vendor is required to run loops; pass --agent-vendor or answer the prompt")
	}
	vendor := config.AgentVendor(answer)
	if !isSupportedBootstrapVendor(vendor) {
		return "", fmt.Errorf("unsupported agent vendor %q (supported: claude-code, codex, opencode, cursor-cli, grok-build)", answer)
	}
	return vendor, nil
}

func detectInstalledVendors(look config.LookPathFunc) []config.AgentVendor {
	found := make([]config.AgentVendor, 0, len(takeoverVendorBinaries))
	for _, vb := range takeoverVendorBinaries {
		if _, err := look(vb.binary); err == nil {
			found = append(found, vb.vendor)
		}
	}
	return found
}

func joinVendors(vendors []config.AgentVendor) string {
	parts := make([]string, 0, len(vendors))
	for _, v := range vendors {
		parts = append(parts, string(v))
	}
	return strings.Join(parts, ", ")
}

// takeoverScopingRoles builds a fresh set of per-project role overrides that
// scope the daemon to the single targeted PR.
func takeoverScopingRoles(merge bool) *config.PartialRoleConfigs {
	return applyTakeoverScoping(nil, merge)
}

// applyTakeoverScoping enforces the single-PR scoping on top of an existing role
// tree (or a fresh one when roles is nil): every autonomous discovery loop is
// disabled so only the manually started reviewer/fixer loops run, and the
// reviewer auto-merge flag is set explicitly to match --merge. Setting
// auto-merge explicitly (instead of only when merge is true) is required so a
// takeover without --merge overrides any global roles.reviewer.autoMerge.enabled
// rather than silently inheriting it. Existing role sub-fields (instructions,
// reviewer behavior, triggers, …) are preserved — only the scoping leaves are
// overwritten.
func applyTakeoverScoping(roles *config.PartialRoleConfigs, merge bool) *config.PartialRoleConfigs {
	disabled := false
	autoMerge := merge
	if roles == nil {
		roles = &config.PartialRoleConfigs{}
	}
	if roles.Planner == nil {
		roles.Planner = &config.PartialPlannerRoleConfig{}
	}
	roles.Planner.AutoDiscovery = &disabled
	if roles.Worker == nil {
		roles.Worker = &config.PartialWorkerRoleConfig{}
	}
	roles.Worker.AutoDiscovery = &disabled
	if roles.Fixer == nil {
		roles.Fixer = &config.PartialFixerRoleConfig{}
	}
	roles.Fixer.AutoDiscovery = &disabled
	if roles.Reviewer == nil {
		roles.Reviewer = &config.PartialReviewerRoleConfig{}
	}
	if roles.Reviewer.Discovery == nil {
		roles.Reviewer.Discovery = &config.PartialReviewerRoleDiscoveryConfig{}
	}
	roles.Reviewer.Discovery.AutoDiscovery = &disabled
	if roles.Reviewer.AutoMerge == nil {
		roles.Reviewer.AutoMerge = &config.PartialReviewerAutoMergeConfig{}
	}
	roles.Reviewer.AutoMerge.Enabled = &autoMerge
	return roles
}

// clonePartialRoleConfigs deep-copies a role tree via JSON so scoping can be
// applied to a copy without mutating the config read from disk (which would
// break change detection).
func clonePartialRoleConfigs(in *config.PartialRoleConfigs) *config.PartialRoleConfigs {
	if in == nil {
		return nil
	}
	data, err := json.Marshal(in)
	if err != nil {
		return nil
	}
	var out config.PartialRoleConfigs
	if err := json.Unmarshal(data, &out); err != nil {
		return nil
	}
	return &out
}

// ensureTakeoverConfig creates or patches the config file so it carries the
// agent vendor and a project entry (scoped to the single PR) for repoRoot. It
// returns whether the file content changed.
func (r *commandRuntime) ensureTakeoverConfig(configPath string, cwd string, repoRoot string, vendor config.AgentVendor, merge bool) (bool, error) {
	if err := r.mkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		return false, fmt.Errorf("create config directory: %w", err)
	}

	roles := takeoverScopingRoles(merge)

	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		cfg, err := config.DefaultConfig(cwd)
		if err != nil {
			return false, fmt.Errorf("build default config: %w", err)
		}
		v := vendor
		cfg.Agent.Vendor = &v
		cfg.Notifications.Osascript.Enabled = false
		project := buildBootstrapProject(repoRoot, cfg.Defaults.BaseBranch)
		project.Roles = roles
		cfg.Projects = append(cfg.Projects, project)
		if err := writeBootstrapConfig(configPath, cfg); err != nil {
			return false, err
		}
		return true, nil
	} else if err != nil {
		return false, fmt.Errorf("check config path %s: %w", configPath, err)
	}

	partial, err := readBootstrapPartialConfig(configPath)
	if err != nil {
		return false, err
	}

	changed := false
	// Always honor the resolved vendor. resolveTakeoverVendor already reuses the
	// configured vendor when no explicit --agent-vendor was given, so this is a
	// no-op in the reuse case but applies an explicit override that would
	// otherwise be silently ignored (leaving the daemon on the old vendor).
	if partial.Agent == nil {
		partial.Agent = &config.PartialAgentConfig{}
	}
	if partial.Agent.Vendor == nil || *partial.Agent.Vendor != vendor {
		v := vendor
		partial.Agent.Vendor = &v
		changed = true
	}

	normalized, err := config.Normalize(cwd, partial)
	if err != nil {
		return false, err
	}
	project := buildBootstrapProject(repoRoot, normalized.Defaults.BaseBranch)
	project.Roles = roles
	partialProject := partialProjectFromConfig(project)

	projects := []config.PartialProjectRefConfig{}
	replaced := false
	if partial.Projects != nil {
		for _, existing := range *partial.Projects {
			if samePath(existing.RepoPath, repoRoot) {
				// Preserve the user's existing project fields AND existing role
				// sub-config (instructions, reviewer behavior, …); only overwrite
				// the single-PR scoping leaves. Apply scoping to a deep copy so
				// the original is untouched for change detection.
				merged := existing
				merged.Roles = applyTakeoverScoping(clonePartialRoleConfigs(existing.Roles), merge)
				if !sameTakeoverProject(existing, merged) {
					changed = true
				}
				projects = append(projects, merged)
				replaced = true
				continue
			}
			projects = append(projects, existing)
		}
	}
	if !replaced {
		projects = append(projects, partialProject)
		changed = true
	}
	partial.Projects = &projects

	updated, err := config.Normalize(cwd, partial)
	if err != nil {
		return false, err
	}
	if err := config.Validate(updated); err != nil {
		return false, err
	}

	if !changed {
		return false, nil
	}

	newRaw, err := config.MarshalConfigFile(configPath, partial)
	if err != nil {
		return false, fmt.Errorf("marshal config: %w", err)
	}
	if err := os.WriteFile(configPath, newRaw, 0o644); err != nil {
		return false, fmt.Errorf("write config: %w", err)
	}
	return true, nil
}

// sameTakeoverProject reports whether two project entries are equivalent for
// takeover purposes, so an idempotent re-run does not rewrite the config (and
// therefore does not trigger a needless daemon restart).
func sameTakeoverProject(a, b config.PartialProjectRefConfig) bool {
	left, err := json.Marshal(a)
	if err != nil {
		return false
	}
	right, err := json.Marshal(b)
	if err != nil {
		return false
	}
	return string(left) == string(right)
}

// ensureTakeoverDaemonRunning starts the daemon when it is down, or restarts it
// to reload changed config when it is already running. Daemon lifecycle output
// is suppressed so takeover prints a single coherent summary.
func (r *commandRuntime) ensureTakeoverDaemonRunning(ctx context.Context, cmd *cobra.Command, configChanged bool) (string, error) {
	loaded, err := r.loadConfig()
	if err != nil {
		return "", err
	}
	client := r.apiClientFromLoaded(loaded)
	reachable, err := r.bootstrapAPIReachable(ctx, client)
	if err != nil {
		return "", err
	}

	if !reachable {
		if err := r.takeoverDaemonLifecycle(cmd, r.daemonStart); err != nil {
			return "", err
		}
		if _, err := r.waitForBootstrapHealth(ctx, client); err != nil {
			return "", err
		}
		return "started looperd in detached mode; it will not survive logout or reboot — run `looper daemon start --daemon-mode launchd` for supervised lifecycle", nil
	}

	if configChanged {
		if err := r.takeoverDaemonLifecycle(cmd, r.daemonRestart); err != nil {
			return "", err
		}
		if _, err := r.waitForBootstrapHealth(ctx, client); err != nil {
			return "", err
		}
		return "restarted looperd to load the updated configuration", nil
	}

	return "", nil
}

func (r *commandRuntime) takeoverDaemonLifecycle(cmd *cobra.Command, action func(*cobra.Command, []string) error) error {
	originalOut := cmd.OutOrStdout()
	cmd.SetOut(io.Discard)
	defer cmd.SetOut(originalOut)
	return action(cmd, nil)
}

// waitForTakeoverProject waits for the daemon to register the scoped project
// (synced from config on startup) and returns it.
func (r *commandRuntime) waitForTakeoverProject(ctx context.Context, repoRoot string) (projectOutput, error) {
	deadline := time.Now().Add(takeoverProjectSyncTimeout)
	var lastErr error
	for {
		projects, err := r.listProjects(ctx)
		if err != nil {
			lastErr = err
		} else {
			for _, project := range projects {
				if samePath(project.RepoPath, repoRoot) {
					return project, nil
				}
			}
			lastErr = fmt.Errorf("project for %s was not registered by looperd", repoRoot)
		}
		if !time.Now().Before(deadline) {
			return projectOutput{}, lastErr
		}
		r.sleep(250 * time.Millisecond)
	}
}

func (r *commandRuntime) startTakeoverLoop(ctx context.Context, loopType string, projectID string, repo string, prNumber int64) (*takeoverLoopResult, error) {
	body := map[string]any{
		"projectId":  projectID,
		"type":       loopType,
		"targetType": "pull_request",
		"repo":       repo,
		"prNumber":   prNumber,
		"status":     "running",
		"metadata": map[string]any{
			"manual":        true,
			"followUpdates": true,
		},
	}
	payload, err := r.postJSON(ctx, "/api/v1/loops", body)
	if err != nil {
		return nil, fmt.Errorf("start %s loop: %w", loopType, err)
	}
	var data loopOutput
	if err := json.Unmarshal(payload, &data); err != nil {
		return nil, fmt.Errorf("decode %s loop response: %w", loopType, err)
	}
	return &takeoverLoopResult{ID: data.ID, Seq: data.Seq, Type: data.Type, Status: data.Status}, nil
}

func takeoverNextSteps(repo string, prNumber int64) []string {
	return []string{
		"looper ps",
		"looper logs <id> --follow",
		fmt.Sprintf("looper pr status %s#%d", repo, prNumber),
		"looper stop <id>",
	}
}

// takeoverStateFile is the local index, under ~/.looper, that ties each
// takeover to the loops it started so `takeover list` / `takeover stop` can
// manage them without the daemon having to model "a takeover" as a first-class
// concept.
const takeoverStateFile = "takeovers.json"

type takeoverStateEntry struct {
	Repo           string `json:"repo"`
	PRNumber       int64  `json:"prNumber"`
	ProjectID      string `json:"projectId"`
	AgentVendor    string `json:"agentVendor"`
	AutoMerge      bool   `json:"autoMerge"`
	ReviewerLoopID string `json:"reviewerLoopId,omitempty"`
	FixerLoopID    string `json:"fixerLoopId,omitempty"`
	StartedAt      string `json:"startedAt"`
}

type takeoverState struct {
	Version   int                  `json:"version"`
	Takeovers []takeoverStateEntry `json:"takeovers"`
}

func (r *commandRuntime) takeoverStatePath() (string, error) {
	home, err := r.homeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".looper", takeoverStateFile), nil
}

func (r *commandRuntime) loadTakeoverState() (takeoverState, error) {
	path, err := r.takeoverStatePath()
	if err != nil {
		return takeoverState{}, err
	}
	data, err := r.readFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return takeoverState{Version: 1}, nil
		}
		return takeoverState{}, err
	}
	var st takeoverState
	if err := json.Unmarshal(data, &st); err != nil {
		return takeoverState{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if st.Version == 0 {
		st.Version = 1
	}
	return st, nil
}

func (r *commandRuntime) saveTakeoverState(st takeoverState) error {
	path, err := r.takeoverStatePath()
	if err != nil {
		return err
	}
	if err := r.mkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	st.Version = 1
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return r.writeFile(path, append(data, '\n'), 0o644)
}

func (r *commandRuntime) recordTakeover(entry takeoverStateEntry) error {
	st, err := r.loadTakeoverState()
	if err != nil {
		return err
	}
	st.Takeovers = upsertTakeoverEntry(st.Takeovers, entry)
	return r.saveTakeoverState(st)
}

func upsertTakeoverEntry(entries []takeoverStateEntry, entry takeoverStateEntry) []takeoverStateEntry {
	for i := range entries {
		if entries[i].Repo == entry.Repo && entries[i].PRNumber == entry.PRNumber {
			entries[i] = entry
			return entries
		}
	}
	return append(entries, entry)
}

// matchTakeovers selects the takeover entries targeted by a stop request. With
// all=true it returns everything; an explicit fully-qualified ref matches
// repo+number; a bare number matches a unique PR number; an empty ref matches
// the only takeover when exactly one is recorded.
func matchTakeovers(entries []takeoverStateEntry, ref string, all bool) ([]takeoverStateEntry, error) {
	if all {
		if len(entries) == 0 {
			return nil, fmt.Errorf("no active takeovers to stop")
		}
		return entries, nil
	}

	if strings.TrimSpace(ref) == "" {
		switch len(entries) {
		case 0:
			return nil, fmt.Errorf("no active takeovers to stop")
		case 1:
			return entries, nil
		default:
			return nil, fmt.Errorf("multiple takeovers active; pass <owner/repo>#<number> or --all")
		}
	}

	repo, prNumber, repoQualified, err := parseOptionalPullRequestRef(ref)
	if err != nil {
		return nil, err
	}
	matches := make([]takeoverStateEntry, 0, 1)
	for _, entry := range entries {
		if entry.PRNumber != prNumber {
			continue
		}
		if repoQualified && entry.Repo != repo {
			continue
		}
		matches = append(matches, entry)
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("no active takeover matches %s", ref)
	}
	if len(matches) > 1 {
		return nil, fmt.Errorf("%s is ambiguous across multiple repos; pass <owner/repo>#<number>", ref)
	}
	return matches, nil
}

func (r *commandRuntime) takeoverList(cmd *cobra.Command, args []string) error {
	_ = args
	ctx := cmd.Context()
	st, err := r.loadTakeoverState()
	if err != nil {
		return err
	}

	statusByID := map[string]string{}
	if payload, err := r.getJSON(ctx, "/api/v1/loops"); err == nil {
		var data loopsListOutput
		if json.Unmarshal(payload, &data) == nil {
			for _, loop := range data.Items {
				statusByID[loop.ID] = loop.Status
			}
		}
	}

	if getBoolFlag(cmd, "json") {
		return writeJSON(cmd.OutOrStdout(), takeoverListPayload(st, statusByID))
	}
	return writeHumanTakeoverList(cmd.OutOrStdout(), st, statusByID)
}

func loopLiveStatus(statusByID map[string]string, loopID string) string {
	if loopID == "" {
		return "-"
	}
	if status, ok := statusByID[loopID]; ok {
		return status
	}
	return "waiting"
}

func takeoverListPayload(st takeoverState, statusByID map[string]string) map[string]any {
	items := make([]map[string]any, 0, len(st.Takeovers))
	for _, entry := range st.Takeovers {
		items = append(items, map[string]any{
			"repo":           entry.Repo,
			"prNumber":       entry.PRNumber,
			"projectId":      entry.ProjectID,
			"agentVendor":    entry.AgentVendor,
			"autoMerge":      entry.AutoMerge,
			"startedAt":      entry.StartedAt,
			"reviewerLoopId": entry.ReviewerLoopID,
			"fixerLoopId":    entry.FixerLoopID,
			"reviewerStatus": loopLiveStatus(statusByID, entry.ReviewerLoopID),
			"fixerStatus":    loopLiveStatus(statusByID, entry.FixerLoopID),
		})
	}
	return map[string]any{"items": items}
}

func writeHumanTakeoverList(w io.Writer, st takeoverState, statusByID map[string]string) error {
	if len(st.Takeovers) == 0 {
		_, err := fmt.Fprintln(w, "No active takeovers.")
		return err
	}
	rows := make([]tableRow, 0, len(st.Takeovers))
	for _, entry := range st.Takeovers {
		fixer := loopLiveStatus(statusByID, entry.FixerLoopID)
		if entry.FixerLoopID == "" {
			fixer = "(none)"
		}
		rows = append(rows, tableRow{
			"pr":        fmt.Sprintf("%s#%d", entry.Repo, entry.PRNumber),
			"agent":     entry.AgentVendor,
			"autoMerge": fmt.Sprintf("%t", entry.AutoMerge),
			"reviewer":  loopLiveStatus(statusByID, entry.ReviewerLoopID),
			"fixer":     fixer,
			"startedAt": entry.StartedAt,
		})
	}
	printTable(w, []string{"pr", "agent", "autoMerge", "reviewer", "fixer", "startedAt"}, rows)
	return nil
}

type takeoverStopResult struct {
	Stopped []takeoverStopEntry `json:"stopped"`
}

type takeoverStopEntry struct {
	Repo        string   `json:"repo"`
	PRNumber    int64    `json:"prNumber"`
	ClosedLoops []string `json:"closedLoops"`
	Warnings    []string `json:"warnings,omitempty"`
}

func (r *commandRuntime) takeoverStop(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	ref := ""
	if len(args) > 0 {
		ref = strings.TrimSpace(args[0])
	}

	st, err := r.loadTakeoverState()
	if err != nil {
		return err
	}
	targets, err := matchTakeovers(st.Takeovers, ref, getBoolFlag(cmd, "all"))
	if err != nil {
		return err
	}

	result := takeoverStopResult{}
	stopped := map[string]struct{}{}
	for _, target := range targets {
		entry := takeoverStopEntry{Repo: target.Repo, PRNumber: target.PRNumber}
		for _, loopID := range []string{target.ReviewerLoopID, target.FixerLoopID} {
			if loopID == "" {
				continue
			}
			if _, err := r.postJSON(ctx, "/api/v1/runs/active/"+url.PathEscape(loopID)+"/close", nil); err != nil {
				entry.Warnings = append(entry.Warnings, fmt.Sprintf("close %s: %v", loopID, err))
				continue
			}
			entry.ClosedLoops = append(entry.ClosedLoops, loopID)
		}
		result.Stopped = append(result.Stopped, entry)
		stopped[takeoverKey(target.Repo, target.PRNumber)] = struct{}{}
	}

	remaining := make([]takeoverStateEntry, 0, len(st.Takeovers))
	for _, entry := range st.Takeovers {
		if _, ok := stopped[takeoverKey(entry.Repo, entry.PRNumber)]; ok {
			continue
		}
		remaining = append(remaining, entry)
	}
	st.Takeovers = remaining
	if err := r.saveTakeoverState(st); err != nil {
		return err
	}

	if getBoolFlag(cmd, "json") {
		return writeJSON(cmd.OutOrStdout(), result)
	}
	return writeHumanTakeoverStop(cmd.OutOrStdout(), result)
}

func takeoverKey(repo string, prNumber int64) string {
	return fmt.Sprintf("%s#%d", repo, prNumber)
}

func writeHumanTakeoverStop(w io.Writer, result takeoverStopResult) error {
	for _, entry := range result.Stopped {
		printSection(w, "Takeover stopped", [][2]any{
			{"pr", takeoverKey(entry.Repo, entry.PRNumber)},
			{"closedLoops", strings.Join(entry.ClosedLoops, ", ")},
		})
		for _, warning := range entry.Warnings {
			_, _ = fmt.Fprintf(w, "- note: %s\n", warning)
		}
	}
	return nil
}

func writeHumanTakeoverResult(w io.Writer, result takeoverResult) error {
	rows := [][2]any{
		{"pr", fmt.Sprintf("%s#%d", result.Repo, result.PRNumber)},
		{"projectId", result.ProjectID},
		{"agentVendor", result.AgentVendor},
		{"autoMerge", result.AutoMerge},
		{"daemonRunning", result.DaemonRunning},
	}
	if result.Reviewer != nil {
		rows = append(rows, [2]any{"reviewerLoop", fmt.Sprintf("%s (%s)", result.Reviewer.ID, result.Reviewer.Status)})
	}
	if result.Fixer != nil {
		rows = append(rows, [2]any{"fixerLoop", fmt.Sprintf("%s (%s)", result.Fixer.ID, result.Fixer.Status)})
	}
	printSection(w, "Takeover started", rows)

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
