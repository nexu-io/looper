package cliapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/config"
	pkgapi "github.com/nexu-io/looper/pkg/api"
	"github.com/spf13/cobra"
)

const ghWebhookExtension = "cli/gh-webhook"

const ghWebhookForwarderHookURL = "https://webhook-forwarder.github.com/hook"

type webhookStatusOutput struct {
	ConfigPath       string              `json:"configPath"`
	Enabled          bool                `json:"enabled"`
	FallbackPoll     int                 `json:"fallbackPollIntervalSeconds"`
	RestartRequired  bool                `json:"restartRequired"`
	Warnings         []string            `json:"warnings"`
	RuntimeAvailable bool                `json:"runtimeAvailable"`
	Runtime          *webhookRuntimeView `json:"runtime,omitempty"`
}

type webhookRuntimeView struct {
	Enabled                     bool     `json:"enabled"`
	ListenerPath                string   `json:"listenerPath"`
	EndpointURL                 string   `json:"endpointUrl"`
	FallbackPollIntervalSeconds int      `json:"fallbackPollIntervalSeconds"`
	Degraded                    bool     `json:"degraded"`
	DegradedReasons             []string `json:"degradedReasons"`
	Queue                       struct {
		Pending       int `json:"pending"`
		Capacity      int `json:"capacity"`
		ActiveWorkers int `json:"activeWorkers"`
	} `json:"queue"`
	Counters struct {
		DeliveriesReceived int `json:"deliveriesReceived"`
		Coalesced          int `json:"coalesced"`
		Dropped            int `json:"dropped"`
		Queued             int `json:"queued"`
		Processed          int `json:"processed"`
		Failed             int `json:"failed"`
	} `json:"counters"`
	RecentOutcomes []struct {
		At      string `json:"at"`
		Outcome string `json:"outcome"`
		Message string `json:"message"`
	} `json:"recentOutcomes"`
	Forwarders []struct {
		Repo          string   `json:"repo"`
		Running       bool     `json:"running"`
		PID           *int     `json:"pid,omitempty"`
		Adopted       bool     `json:"adopted"`
		Latched       bool     `json:"latched"`
		LatchReason   *string  `json:"latchReason,omitempty"`
		Fingerprint   string   `json:"fingerprint,omitempty"`
		SpawnedAt     *string  `json:"spawnedAt,omitempty"`
		Command       []string `json:"command"`
		RestartCount  int      `json:"restartCount"`
		LastStartedAt *string  `json:"lastStartedAt,omitempty"`
		LastExitAt    *string  `json:"lastExitAt,omitempty"`
		LastError     string   `json:"lastError,omitempty"`
		StdoutTail    []string `json:"stdoutTail,omitempty"`
		StderrTail    []string `json:"stderrTail,omitempty"`
	} `json:"forwarders"`
}

type webhookHook struct {
	ID     int64    `json:"id"`
	Name   string   `json:"name"`
	Type   string   `json:"type"`
	Active bool     `json:"active"`
	Events []string `json:"events"`
	Config struct {
		URL string `json:"url"`
	} `json:"config"`
}

type webhookCleanupCandidate struct {
	ID     int64
	Events string
	Active bool
}

func (r *commandRuntime) webhookEnable(cmd *cobra.Command, args []string) error {
	_ = args
	loaded, err := r.loadRawConfigForEdit()
	if err != nil {
		return err
	}
	ghWebhookInstalled := false
	ghWebhookWarning := ""
	ghPath := webhookGHPath(loaded.Config)
	if ghPath == "" {
		if resolved, resolveErr := r.lookPath()("gh"); resolveErr == nil {
			ghPath = strings.TrimSpace(resolved)
		}
	}
	if ghPath != "" {
		available, checkErr := r.ghWebhookCommandAvailable(cmd.Context(), ghPath)
		if checkErr != nil {
			ghWebhookWarning = fmt.Sprintf("could not check gh webhook command: %v", checkErr)
		} else if !available {
			if getBoolFlag(cmd, "install-gh-webhook") {
				if err := r.installGHWebhookExtension(cmd.Context(), ghPath); err != nil {
					return err
				}
				ghWebhookInstalled = true
			} else {
				ghWebhookWarning = "gh webhook command is unavailable; install the GitHub CLI extension with: gh extension install cli/gh-webhook, or rerun: looper webhook enable --install-gh-webhook"
			}
		}
	}
	partial := loaded.Partial
	if partial.Webhook == nil {
		partial.Webhook = &config.PartialWebhookConfig{}
	}
	partial.Webhook.Enabled = webhookBoolPtr(true)
	if partial.Webhook.FallbackPollIntervalSeconds == nil {
		partial.Webhook.FallbackPollIntervalSeconds = webhookIntPtr(loaded.Config.Webhook.FallbackPollIntervalSeconds)
	}
	if err := r.writeConfigFile(loaded.Metadata.ConfigPath, partial); err != nil {
		return err
	}
	updated, err := r.loadConfigForEdit()
	if err != nil {
		return err
	}
	warnings := webhookWarnings(updated.Config)
	if ghWebhookWarning != "" {
		warnings = append(warnings, ghWebhookWarning)
	}
	if getBoolFlag(cmd, "json") {
		return writeJSON(cmd.OutOrStdout(), webhookStatusOutput{ConfigPath: updated.Metadata.ConfigPath, Enabled: true, FallbackPoll: updated.Config.Webhook.FallbackPollIntervalSeconds, RestartRequired: true, Warnings: warnings})
	}
	if ghWebhookInstalled {
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Installed GitHub CLI webhook extension %s\n", ghWebhookExtension); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Enabled webhook mode in %s\n", updated.Metadata.ConfigPath); err != nil {
		return err
	}
	for _, warning := range warnings {
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Warning: %s\n", warning); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintln(cmd.OutOrStdout(), "Restart looperd to apply webhook changes.")
	return err
}

func (r *commandRuntime) ghWebhookCommandAvailable(ctx context.Context, ghPath string) (bool, error) {
	result, err := r.runCommand(ctx, ghPath, []string{"webhook", "forward", "--help"}, 10*time.Second)
	if err != nil {
		return false, err
	}
	return result.ExitCode == 0, nil
}

func (r *commandRuntime) installGHWebhookExtension(ctx context.Context, ghPath string) error {
	result, err := r.runCommand(ctx, ghPath, []string{"extension", "install", ghWebhookExtension}, 60*time.Second)
	if err != nil {
		return fmt.Errorf("install gh webhook extension: %w", err)
	}
	if result.ExitCode != 0 {
		output := strings.TrimSpace(strings.Join([]string{result.Stderr, result.Stdout}, "\n"))
		if output == "" {
			output = fmt.Sprintf("exit code %d", result.ExitCode)
		}
		return fmt.Errorf("install gh webhook extension: %s", output)
	}
	available, err := r.ghWebhookCommandAvailable(ctx, ghPath)
	if err != nil {
		return fmt.Errorf("verify gh webhook extension: %w", err)
	}
	if !available {
		return errors.New("install gh webhook extension: gh webhook command is still unavailable after install")
	}
	return nil
}

func (r *commandRuntime) webhookDisable(cmd *cobra.Command, args []string) error {
	_ = args
	loaded, err := r.loadRawConfigForEdit()
	if err != nil {
		return err
	}
	partial := loaded.Partial
	if partial.Webhook == nil {
		partial.Webhook = &config.PartialWebhookConfig{}
	}
	partial.Webhook.Enabled = webhookBoolPtr(false)
	if err := r.writeConfigFile(loaded.Metadata.ConfigPath, partial); err != nil {
		return err
	}
	if getBoolFlag(cmd, "json") {
		return writeJSON(cmd.OutOrStdout(), webhookStatusOutput{ConfigPath: loaded.Metadata.ConfigPath, Enabled: false, FallbackPoll: loaded.Config.Webhook.FallbackPollIntervalSeconds, RestartRequired: true})
	}
	if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Disabled webhook mode in %s\n", loaded.Metadata.ConfigPath); err != nil {
		return err
	}
	_, err = fmt.Fprintln(cmd.OutOrStdout(), "Restart looperd to apply webhook changes.")
	return err
}

func (r *commandRuntime) webhookStatus(cmd *cobra.Command, args []string) error {
	_ = args
	loaded, err := r.loadConfigForEdit()
	if err != nil {
		return err
	}
	output := webhookStatusOutput{
		ConfigPath:   loaded.Metadata.ConfigPath,
		Enabled:      loaded.Config.Webhook.Enabled,
		FallbackPoll: loaded.Config.Webhook.FallbackPollIntervalSeconds,
		Warnings:     webhookWarnings(loaded.Config),
	}
	client := r.apiClientFromLoaded(loaded)
	payload, err := r.getJSONWithClient(cmd.Context(), client, "/api/v1/webhook/status")
	if err != nil {
		if !isWebhookRuntimeUnavailableError(err) {
			return err
		}
	} else {
		var runtimeView webhookRuntimeView
		if decodeErr := json.Unmarshal(payload, &runtimeView); decodeErr != nil {
			return fmt.Errorf("decode webhook status response: %w", decodeErr)
		}
		output.RuntimeAvailable = true
		output.Runtime = &runtimeView
	}
	output.RestartRequired = webhookRuntimeRestartRequired(output)
	if getBoolFlag(cmd, "json") {
		return writeJSON(cmd.OutOrStdout(), output)
	}
	return writeHumanWebhookStatus(cmd.OutOrStdout(), output, getBoolFlag(cmd, "verbose"))
}

func (r *commandRuntime) webhookCleanup(cmd *cobra.Command, args []string) error {
	repo, err := normalizeWebhookRepo(args[0])
	if err != nil {
		return err
	}
	loaded, err := r.loadConfig()
	if err != nil {
		return err
	}
	ghPath, err := r.resolveGHPath(loaded.Config)
	if err != nil {
		return err
	}
	hooks, err := r.listWebhookHooks(cmd.Context(), ghPath, repo)
	if err != nil {
		return err
	}
	candidates := webhookCleanupCandidates(hooks)
	if len(candidates) == 0 {
		_, err := fmt.Fprintf(cmd.OutOrStdout(), "No stale GitHub CLI webhook hooks found for %s.\n", repo)
		return err
	}
	if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Found %d GitHub CLI webhook hook(s) for %s:\n", len(candidates), repo); err != nil {
		return err
	}
	for _, candidate := range candidates {
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "- id=%d active=%t events=%s\n", candidate.ID, candidate.Active, candidate.Events); err != nil {
			return err
		}
	}
	if !getBoolFlag(cmd, "confirm") {
		_, err := fmt.Fprintf(cmd.OutOrStdout(), "Dry run only. Rerun with: looper webhook cleanup %s --confirm\n", repo)
		return err
	}
	for _, candidate := range candidates {
		if err := r.deleteWebhookHook(cmd.Context(), ghPath, repo, candidate.ID); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "Deleted %d GitHub CLI webhook hook(s) for %s.\n", len(candidates), repo)
	return err
}

func webhookRuntimeRestartRequired(output webhookStatusOutput) bool {
	if output.Runtime == nil {
		return false
	}
	if output.Runtime.Enabled != output.Enabled {
		return true
	}
	return output.Runtime.FallbackPollIntervalSeconds != output.FallbackPoll
}

func writeHumanWebhookStatus(w io.Writer, data webhookStatusOutput, verbose bool) error {
	printSection(w, "Webhook config", [][2]any{{"configPath", data.ConfigPath}, {"enabled", data.Enabled}, {"fallbackPollIntervalSeconds", data.FallbackPoll}, {"restartRequired", data.RestartRequired}, {"warnings", joinOrNone(data.Warnings)}})
	if data.Runtime == nil {
		if _, err := fmt.Fprintln(w); err != nil {
			return err
		}
		printSection(w, "Webhook runtime", [][2]any{{"available", false}})
		return nil
	}
	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}
	printSection(w, "Webhook runtime", [][2]any{{"available", true}, {"enabled", data.Runtime.Enabled}, {"listenerPath", data.Runtime.ListenerPath}, {"endpointUrl", data.Runtime.EndpointURL}, {"fallbackPollIntervalSeconds", data.Runtime.FallbackPollIntervalSeconds}, {"degraded", data.Runtime.Degraded}, {"degradedReasons", joinOrNone(data.Runtime.DegradedReasons)}})
	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}
	printSection(w, "Queue", [][2]any{{"pending", data.Runtime.Queue.Pending}, {"capacity", data.Runtime.Queue.Capacity}, {"activeWorkers", data.Runtime.Queue.ActiveWorkers}})
	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}
	printSection(w, "Counters", [][2]any{{"deliveriesReceived", data.Runtime.Counters.DeliveriesReceived}, {"coalesced", data.Runtime.Counters.Coalesced}, {"dropped", data.Runtime.Counters.Dropped}, {"queued", data.Runtime.Counters.Queued}, {"processed", data.Runtime.Counters.Processed}, {"failed", data.Runtime.Counters.Failed}})
	if data.Runtime.Degraded {
		commands := webhookCleanupSuggestions(data.Runtime)
		if len(commands) > 0 {
			if _, err := fmt.Fprintln(w); err != nil {
				return err
			}
			printSection(w, "Cleanup hint", [][2]any{{"staleCliWebhookCleanup", strings.Join(commands, "\n")}, {"note", "Run the dry-run command first; add --confirm to delete matching GitHub CLI webhook hooks."}})
		}
	}
	if !verbose {
		return nil
	}
	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}
	rows := make([]tableRow, 0, len(data.Runtime.Forwarders))
	for _, forwarder := range data.Runtime.Forwarders {
		rows = append(rows, tableRow{"repo": forwarder.Repo, "running": forwarder.Running, "pid": forwarder.PID, "adopted": forwarder.Adopted, "latched": forwarder.Latched, "restarts": forwarder.RestartCount, "lastError": forwarder.LastError})
	}
	printTable(w, []string{"repo", "running", "pid", "adopted", "latched", "restarts", "lastError"}, rows)
	for _, forwarder := range data.Runtime.Forwarders {
		if _, err := fmt.Fprintln(w); err != nil {
			return err
		}
		stdoutTail := joinOrNone(forwarder.StdoutTail)
		stderrTail := joinOrNone(forwarder.StderrTail)
		if forwarder.Adopted && len(forwarder.StdoutTail) == 0 && len(forwarder.StderrTail) == 0 {
			stdoutTail = "(adopted process: stdout/stderr not captured)"
			stderrTail = "(adopted process: stdout/stderr not captured)"
		}
		printSection(w, fmt.Sprintf("Forwarder %s", forwarder.Repo), [][2]any{{"command", strings.Join(forwarder.Command, " ")}, {"adopted", forwarder.Adopted}, {"latched", forwarder.Latched}, {"latchReason", forwarder.LatchReason}, {"fingerprint", forwarder.Fingerprint}, {"spawnedAt", forwarder.SpawnedAt}, {"lastStartedAt", forwarder.LastStartedAt}, {"lastExitAt", forwarder.LastExitAt}, {"stdoutTail", stdoutTail}, {"stderrTail", stderrTail}})
	}
	return nil
}

func webhookWarnings(cfg config.Config) []string {
	warnings := make([]string, 0, 2)
	if !isWebhookLoopbackHost(cfg.Server.Host) {
		warnings = append(warnings, "server.host is not loopback; looperd will degrade webhook mode to poll fallback")
	}
	if cfg.Tools.GHPath == nil || strings.TrimSpace(*cfg.Tools.GHPath) == "" {
		warnings = append(warnings, "gh could not be resolved; looperd will degrade webhook mode to poll fallback")
	}
	return warnings
}

func (r *commandRuntime) resolveGHPath(cfg config.Config) (string, error) {
	ghPath := webhookGHPath(cfg)
	if ghPath != "" {
		return ghPath, nil
	}
	resolved, err := r.lookPath()("gh")
	if err != nil {
		return "", errors.New("gh is not configured or could not be resolved")
	}
	resolved = strings.TrimSpace(resolved)
	if resolved == "" {
		return "", errors.New("gh is not configured or could not be resolved")
	}
	return resolved, nil
}

func (r *commandRuntime) listWebhookHooks(ctx context.Context, ghPath, repo string) ([]webhookHook, error) {
	hostname, repoPath := splitWebhookRepoHostname(repo)
	args := []string{"api", "--paginate", "--slurp", fmt.Sprintf("repos/%s/hooks", repoPath)}
	if hostname != "" {
		args = append(args, "--hostname", hostname)
	}
	result, err := r.runCommand(ctx, ghPath, args, 15*time.Second)
	if err != nil {
		return nil, fmt.Errorf("list webhook hooks for %s: %w", repo, err)
	}
	if result.ExitCode != 0 {
		output := strings.TrimSpace(strings.Join([]string{result.Stderr, result.Stdout}, "\n"))
		if output == "" {
			output = fmt.Sprintf("exit code %d", result.ExitCode)
		}
		return nil, fmt.Errorf("list webhook hooks for %s: %s", repo, output)
	}
	var pages [][]webhookHook
	if err := json.Unmarshal([]byte(result.Stdout), &pages); err != nil {
		return nil, fmt.Errorf("decode webhook hooks for %s: %w", repo, err)
	}
	hooks := make([]webhookHook, 0)
	for _, page := range pages {
		hooks = append(hooks, page...)
	}
	return hooks, nil
}

func (r *commandRuntime) deleteWebhookHook(ctx context.Context, ghPath, repo string, id int64) error {
	hostname, repoPath := splitWebhookRepoHostname(repo)
	args := []string{"api", "-X", "DELETE", fmt.Sprintf("repos/%s/hooks/%d", repoPath, id)}
	if hostname != "" {
		args = append(args, "--hostname", hostname)
	}
	result, err := r.runCommand(ctx, ghPath, args, 15*time.Second)
	if err != nil {
		return fmt.Errorf("delete webhook hook %d for %s: %w", id, repo, err)
	}
	if result.ExitCode != 0 {
		output := strings.TrimSpace(strings.Join([]string{result.Stderr, result.Stdout}, "\n"))
		if webhookHookDeleteNotFound(output) {
			return nil
		}
		if output == "" {
			output = fmt.Sprintf("exit code %d", result.ExitCode)
		}
		return fmt.Errorf("delete webhook hook %d for %s: %s", id, repo, output)
	}
	return nil
}

func webhookHookDeleteNotFound(output string) bool {
	lower := strings.ToLower(strings.TrimSpace(output))
	return strings.Contains(lower, "404") && strings.Contains(lower, "not found")
}

func webhookCleanupCandidates(hooks []webhookHook) []webhookCleanupCandidate {
	candidates := make([]webhookCleanupCandidate, 0, len(hooks))
	for _, hook := range hooks {
		if !strings.EqualFold(strings.TrimSpace(hook.Name), "cli") {
			continue
		}
		if strings.TrimSpace(hook.Config.URL) != ghWebhookForwarderHookURL {
			continue
		}
		candidates = append(candidates, webhookCleanupCandidate{ID: hook.ID, Active: hook.Active, Events: strings.Join(sortedLowercase(hook.Events), ",")})
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].ID < candidates[j].ID })
	return candidates
}

func webhookCleanupSuggestions(runtime *webhookRuntimeView) []string {
	if runtime == nil {
		return nil
	}
	repos := map[string]struct{}{}
	for _, forwarder := range runtime.Forwarders {
		repo := strings.TrimSpace(forwarder.Repo)
		if repo == "" {
			continue
		}
		if forwarder.Running && !forwarder.Latched && strings.TrimSpace(forwarder.LastError) == "" {
			continue
		}
		repos[repo] = struct{}{}
	}
	if len(repos) == 0 {
		return []string{"looper webhook cleanup <owner/repo>"}
	}
	commands := make([]string, 0, len(repos))
	for repo := range repos {
		commands = append(commands, fmt.Sprintf("looper webhook cleanup %s", repo))
	}
	sort.Strings(commands)
	return commands
}

func sortedLowercase(values []string) []string {
	canon := make([]string, 0, len(values))
	for _, value := range values {
		trimmed := strings.ToLower(strings.TrimSpace(value))
		if trimmed == "" {
			continue
		}
		canon = append(canon, trimmed)
	}
	sort.Strings(canon)
	return canon
}

func normalizeWebhookRepo(value string) (string, error) {
	repo := strings.TrimSpace(value)
	parts := strings.Split(repo, "/")
	if len(parts) != 2 && len(parts) != 3 {
		return "", errors.New("repo must be in owner/repo or host/owner/repo form")
	}
	trimmed := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			return "", errors.New("repo must be in owner/repo or host/owner/repo form")
		}
		trimmed = append(trimmed, part)
	}
	return strings.Join(trimmed, "/"), nil
}

func splitWebhookRepoHostname(repo string) (string, string) {
	parts := strings.Split(strings.TrimSpace(repo), "/")
	if len(parts) == 3 {
		return parts[0], parts[1] + "/" + parts[2]
	}
	return "", strings.TrimSpace(repo)
}

func webhookGHPath(cfg config.Config) string {
	if cfg.Tools.GHPath == nil {
		return ""
	}
	return strings.TrimSpace(*cfg.Tools.GHPath)
}

func isWebhookRuntimeUnavailableError(err error) bool {
	if err == nil {
		return false
	}
	var apiErr *DaemonAPIError
	if errors.As(err, &apiErr) {
		return apiErr.Code == pkgapi.ErrorCodeRouteNotFound
	}
	return strings.Contains(err.Error(), "looperd is not reachable:")
}

func isWebhookLoopbackHost(host string) bool {
	host = strings.TrimSpace(host)
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func webhookBoolPtr(value bool) *bool { return &value }

func webhookIntPtr(value int) *int { return &value }
