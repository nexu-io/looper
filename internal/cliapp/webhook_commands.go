package cliapp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
	pkgapi "github.com/nexu-io/looper/pkg/api"
	"github.com/spf13/cobra"
)

const ghWebhookExtension = "cli/gh-webhook"

const ghWebhookForwarderHookURL = "https://webhook-forwarder.github.com/hook"

type webhookStatusOutput struct {
	ConfigPath       string              `json:"configPath"`
	Enabled          bool                `json:"enabled"`
	Mode             config.WebhookMode  `json:"mode"`
	ListenPort       int                 `json:"listenPort"`
	PublicBaseURL    string              `json:"publicBaseUrl"`
	FallbackPoll     int                 `json:"fallbackPollIntervalSeconds"`
	RestartRequired  bool                `json:"restartRequired"`
	Warnings         []string            `json:"warnings"`
	RuntimeAvailable bool                `json:"runtimeAvailable"`
	Runtime          *webhookRuntimeView `json:"runtime,omitempty"`
	configUsesTunnel bool
	configTunnelIDs  []string
}

type webhookRuntimeView struct {
	Enabled                     bool     `json:"enabled"`
	Mode                        string   `json:"mode"`
	ConfiguredTunnelProjectIDs  []string `json:"configuredTunnelProjectIds,omitempty"`
	ListenerPath                string   `json:"listenerPath"`
	EndpointURL                 string   `json:"endpointUrl"`
	TunnelListenerURL           string   `json:"tunnelListenerUrl"`
	TunnelPublicBaseURL         string   `json:"tunnelPublicBaseUrl"`
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
	TunnelHooks []struct {
		Repo                string  `json:"repo"`
		HookID              *int64  `json:"hookId,omitempty"`
		ManagedURL          string  `json:"managedUrl,omitempty"`
		LastPingAt          *string `json:"lastPingAt,omitempty"`
		ConsecutiveDisables int64   `json:"consecutiveDisables"`
		Latched             bool    `json:"latched"`
		Orphaned            bool    `json:"orphaned"`
		LastError           string  `json:"lastError,omitempty"`
	} `json:"tunnelHooks"`
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
		return writeJSON(cmd.OutOrStdout(), webhookStatusOutput{ConfigPath: updated.Metadata.ConfigPath, Enabled: true, Mode: updated.Config.Webhook.Mode, ListenPort: updated.Config.Webhook.ListenPort, PublicBaseURL: updated.Config.Webhook.PublicBaseURL, FallbackPoll: updated.Config.Webhook.FallbackPollIntervalSeconds, RestartRequired: true, Warnings: warnings})
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
		return writeJSON(cmd.OutOrStdout(), webhookStatusOutput{ConfigPath: loaded.Metadata.ConfigPath, Enabled: false, Mode: loaded.Config.Webhook.Mode, ListenPort: loaded.Config.Webhook.ListenPort, PublicBaseURL: loaded.Config.Webhook.PublicBaseURL, FallbackPoll: loaded.Config.Webhook.FallbackPollIntervalSeconds, RestartRequired: true})
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
		ConfigPath:       loaded.Metadata.ConfigPath,
		Enabled:          loaded.Config.Webhook.Enabled,
		Mode:             loaded.Config.Webhook.Mode,
		ListenPort:       loaded.Config.Webhook.ListenPort,
		PublicBaseURL:    loaded.Config.Webhook.PublicBaseURL,
		FallbackPoll:     loaded.Config.Webhook.FallbackPollIntervalSeconds,
		Warnings:         webhookWarnings(loaded.Config),
		configUsesTunnel: webhookConfigUsesTunnel(loaded.Config),
		configTunnelIDs:  webhookConfiguredTunnelProjectIDs(loaded.Config),
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

func (r *commandRuntime) webhookListOrphans(cmd *cobra.Command, args []string) error {
	_ = args
	return r.withWebhookRepositories(cmd.Context(), func(_ config.Config, repos *storage.Repositories) error {
		records, err := repos.WebhookTunnelHooks.List(cmd.Context())
		if err != nil {
			return err
		}
		orphans := make([]storage.WebhookTunnelHookRecord, 0)
		for _, record := range records {
			if record.Orphaned {
				orphans = append(orphans, record)
			}
		}
		if getBoolFlag(cmd, "json") {
			return writeJSON(cmd.OutOrStdout(), orphans)
		}
		if len(orphans) == 0 {
			_, err := fmt.Fprintln(cmd.OutOrStdout(), "No orphaned tunnel webhook records found.")
			return err
		}
		for _, record := range orphans {
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s hookId=%d managedUrl=%s cleanup=\"looper webhook delete %s --confirm\" forget=\"looper webhook delete %s --confirm --forget\"\n", record.Repo, record.HookID, record.ManagedURL, record.Repo, record.Repo); err != nil {
				return err
			}
		}
		return nil
	})
}

func (r *commandRuntime) webhookDelete(cmd *cobra.Command, args []string) error {
	repo, err := normalizeWebhookRepo(args[0])
	if err != nil {
		return err
	}
	return r.withWebhookRepositories(cmd.Context(), func(cfg config.Config, repos *storage.Repositories) error {
		record, ok, err := repos.WebhookTunnelHooks.Get(cmd.Context(), repo)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("no managed tunnel webhook record found for %s", repo)
		}
		if !getBoolFlag(cmd, "confirm") {
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "Dry run only. Rerun with: looper webhook delete %s --confirm\n", repo)
			return err
		}
		if getBoolFlag(cmd, "forget") {
			if !record.Orphaned {
				return fmt.Errorf("refusing to forget active tunnel webhook record for %s; remove the repo from tunnel mode first so the record becomes orphaned", repo)
			}
			if err := repos.WebhookTunnelHooks.Delete(cmd.Context(), repo); err != nil {
				return err
			}
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "Forgot local tunnel webhook record for %s (hookId=%d); remote hook was not deleted.\n", repo, record.HookID)
			return err
		}
		ghPath, err := r.resolveGHPath(cfg)
		if err != nil {
			return err
		}
		hook, found, err := r.getWebhookHook(cmd.Context(), ghPath, repo, record.HookID)
		if err != nil {
			return err
		}
		if found && strings.TrimSpace(hook.Config.URL) != strings.TrimSpace(record.ManagedURL) {
			return fmt.Errorf("refusing to delete hook %d for %s: remote URL %q does not match managed URL %q", record.HookID, repo, hook.Config.URL, record.ManagedURL)
		}
		if found {
			if err := r.deleteWebhookHook(cmd.Context(), ghPath, repo, record.HookID); err != nil {
				return err
			}
		}
		if err := repos.WebhookTunnelHooks.Delete(cmd.Context(), repo); err != nil {
			return err
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "Deleted managed tunnel webhook for %s (hookId=%d).\n", repo, record.HookID)
		return err
	})
}

func (r *commandRuntime) webhookRotate(cmd *cobra.Command, args []string) error {
	repo, err := normalizeWebhookRepo(args[0])
	if err != nil {
		return err
	}
	return r.withWebhookRepositories(cmd.Context(), func(cfg config.Config, repos *storage.Repositories) error {
		record, ok, err := repos.WebhookTunnelHooks.Get(cmd.Context(), repo)
		if err != nil {
			return err
		}
		if !ok || record.Orphaned {
			return fmt.Errorf("no active managed tunnel webhook record found for %s", repo)
		}
		ghPath, err := r.resolveGHPath(cfg)
		if err != nil {
			return err
		}
		hook, found, err := r.getWebhookHook(cmd.Context(), ghPath, repo, record.HookID)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("managed tunnel webhook %d for %s no longer exists", record.HookID, repo)
		}
		if strings.TrimSpace(hook.Config.URL) != strings.TrimSpace(record.ManagedURL) {
			return fmt.Errorf("refusing to rotate hook %d for %s: remote URL %q does not match managed URL %q", record.HookID, repo, hook.Config.URL, record.ManagedURL)
		}
		secret, err := generateWebhookTunnelSecret()
		if err != nil {
			return err
		}
		path := webhookTunnelSecretPathForCLI(cfg.Storage.DBPath, record.SecretRef)
		oldSecret, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read existing tunnel webhook secret: %w", err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(secret), 0o600); err != nil {
			return err
		}
		if err := r.updateWebhookHookSecret(cmd.Context(), ghPath, repo, record.HookID, record.ManagedURL, secret); err != nil {
			if len(oldSecret) > 0 {
				_ = os.WriteFile(path, oldSecret, 0o600)
			}
			return err
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "Rotated managed tunnel webhook secret for %s (hookId=%d).\n", repo, record.HookID)
		return err
	})
}

func (r *commandRuntime) withWebhookRepositories(ctx context.Context, fn func(config.Config, *storage.Repositories) error) error {
	loaded, err := r.loadConfig()
	if err != nil {
		return err
	}
	db, err := storage.OpenSQLiteDB(ctx, loaded.Config.Storage.DBPath)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	return fn(loaded.Config, storage.NewRepositories(db))
}

func webhookRuntimeRestartRequired(output webhookStatusOutput) bool {
	if output.Runtime == nil {
		return false
	}
	if output.Runtime.Enabled != output.Enabled {
		return true
	}
	runtimeMode := config.WebhookMode(output.Runtime.Mode)
	if runtimeMode == "" {
		runtimeMode = config.WebhookModeGHForward
	}
	configMode := output.Mode
	if configMode == "" {
		configMode = config.WebhookModeGHForward
	}
	if runtimeMode != configMode {
		return true
	}
	if !sameWebhookProjectSet(output.configTunnelIDs, output.Runtime.ConfiguredTunnelProjectIDs) {
		return true
	}
	if output.configUsesTunnel || webhookRuntimeHasActiveTunnelHooks(output.Runtime) {
		if output.Runtime.TunnelListenerURL != webhookStatusTunnelListenerURL(output.ListenPort) {
			return true
		}
		if normalizeWebhookStatusPublicBaseURL(output.Runtime.TunnelPublicBaseURL) != normalizeWebhookStatusPublicBaseURL(output.PublicBaseURL) {
			return true
		}
	}
	return output.Runtime.FallbackPollIntervalSeconds != output.FallbackPoll
}

func webhookConfigUsesTunnel(cfg config.Config) bool {
	if cfg.Webhook.Mode == config.WebhookModeTunnel {
		return true
	}
	for _, project := range cfg.Projects {
		if project.Webhook.Mode == config.WebhookModeTunnel {
			return true
		}
	}
	return false
}

func webhookConfigNeedsGHForward(cfg config.Config) bool {
	if cfg.Webhook.Mode == "" || cfg.Webhook.Mode == config.WebhookModeGHForward {
		return true
	}
	for _, project := range cfg.Projects {
		if project.Webhook.Mode == config.WebhookModeGHForward {
			return true
		}
	}
	return false
}

func webhookRuntimeHasActiveTunnelHooks(runtime *webhookRuntimeView) bool {
	if runtime == nil {
		return false
	}
	for _, hook := range runtime.TunnelHooks {
		if !hook.Orphaned {
			return true
		}
	}
	return false
}

func webhookConfiguredTunnelProjectIDs(cfg config.Config) []string {
	ids := make([]string, 0, len(cfg.Projects))
	for _, project := range cfg.Projects {
		mode := cfg.Webhook.Mode
		if project.Webhook.Mode != "" {
			mode = project.Webhook.Mode
		}
		if mode == "" {
			mode = config.WebhookModeGHForward
		}
		if mode != config.WebhookModeTunnel {
			continue
		}
		ids = append(ids, project.ID)
	}
	sort.Strings(ids)
	return ids
}

func sameWebhookProjectSet(left []string, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func webhookStatusTunnelListenerURL(listenPort int) string {
	if listenPort <= 0 {
		return ""
	}
	return "http://" + net.JoinHostPort("127.0.0.1", fmt.Sprintf("%d", listenPort))
}

func normalizeWebhookStatusPublicBaseURL(baseURL string) string {
	return strings.TrimRight(strings.TrimSpace(baseURL), "/")
}

func writeHumanWebhookStatus(w io.Writer, data webhookStatusOutput, verbose bool) error {
	printSection(w, "Webhook config", [][2]any{{"configPath", data.ConfigPath}, {"enabled", data.Enabled}, {"mode", data.Mode}, {"listenPort", data.ListenPort}, {"publicBaseUrl", data.PublicBaseURL}, {"fallbackPollIntervalSeconds", data.FallbackPoll}, {"restartRequired", data.RestartRequired}, {"warnings", joinOrNone(data.Warnings)}})
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
	printSection(w, "Webhook runtime", [][2]any{{"available", true}, {"enabled", data.Runtime.Enabled}, {"mode", data.Runtime.Mode}, {"listenerPath", data.Runtime.ListenerPath}, {"endpointUrl", data.Runtime.EndpointURL}, {"tunnelListenerUrl", data.Runtime.TunnelListenerURL}, {"fallbackPollIntervalSeconds", data.Runtime.FallbackPollIntervalSeconds}, {"degraded", data.Runtime.Degraded}, {"degradedReasons", joinOrNone(data.Runtime.DegradedReasons)}})
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
	if len(data.Runtime.TunnelHooks) > 0 {
		if _, err := fmt.Fprintln(w); err != nil {
			return err
		}
		tunnelRows := make([]tableRow, 0, len(data.Runtime.TunnelHooks))
		for _, hook := range data.Runtime.TunnelHooks {
			tunnelRows = append(tunnelRows, tableRow{"repo": hook.Repo, "hookId": hook.HookID, "latched": hook.Latched, "orphaned": hook.Orphaned, "consecutiveDisables": hook.ConsecutiveDisables, "lastError": hook.LastError})
		}
		printTable(w, []string{"repo", "hookId", "latched", "orphaned", "consecutiveDisables", "lastError"}, tunnelRows)
	}
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
	if webhookConfigNeedsGHForward(cfg) && !isWebhookLoopbackHost(cfg.Server.Host) {
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

func (r *commandRuntime) getWebhookHook(ctx context.Context, ghPath, repo string, id int64) (webhookHook, bool, error) {
	hostname, repoPath := splitWebhookRepoHostname(repo)
	args := []string{"api", fmt.Sprintf("repos/%s/hooks/%d", repoPath, id)}
	if hostname != "" {
		args = append(args, "--hostname", hostname)
	}
	result, err := r.runCommand(ctx, ghPath, args, 15*time.Second)
	if err != nil {
		return webhookHook{}, false, fmt.Errorf("get webhook hook %d for %s: %w", id, repo, err)
	}
	if result.ExitCode != 0 {
		output := strings.TrimSpace(strings.Join([]string{result.Stderr, result.Stdout}, "\n"))
		if strings.Contains(output, "HTTP 404") || strings.Contains(output, "Not Found") {
			return webhookHook{}, false, nil
		}
		if output == "" {
			output = fmt.Sprintf("exit code %d", result.ExitCode)
		}
		return webhookHook{}, false, fmt.Errorf("get webhook hook %d for %s: %s", id, repo, output)
	}
	var hook webhookHook
	if err := json.Unmarshal([]byte(result.Stdout), &hook); err != nil {
		return webhookHook{}, false, fmt.Errorf("decode webhook hook %d for %s: %w", id, repo, err)
	}
	return hook, true, nil
}

func (r *commandRuntime) updateWebhookHookSecret(ctx context.Context, ghPath, repo string, id int64, managedURL string, secret string) error {
	hostname, repoPath := splitWebhookRepoHostname(repo)
	body := webhookHookMutationBodyForCLI(managedURL, secret)
	tmp, err := os.CreateTemp("", "looper-webhook-*.json")
	if err != nil {
		return err
	}
	path := tmp.Name()
	defer func() { _ = os.Remove(path) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	args := []string{"api", "-X", "PATCH", fmt.Sprintf("repos/%s/hooks/%d", repoPath, id), "--input", path}
	if hostname != "" {
		args = append(args, "--hostname", hostname)
	}
	result, err := r.runCommand(ctx, ghPath, args, 15*time.Second)
	if err != nil {
		return fmt.Errorf("rotate webhook hook secret for %s: %w", repo, err)
	}
	if result.ExitCode != 0 {
		output := strings.TrimSpace(strings.Join([]string{result.Stderr, result.Stdout}, "\n"))
		if output == "" {
			output = fmt.Sprintf("exit code %d", result.ExitCode)
		}
		return fmt.Errorf("rotate webhook hook secret for %s: %s", repo, output)
	}
	return nil
}

func webhookHookMutationBodyForCLI(managedURL string, secret string) []byte {
	body := map[string]any{
		"name":   "web",
		"active": true,
		"events": []string{"check_run", "issue_comment", "pull_request", "pull_request_review", "pull_request_review_comment", "push"},
		"config": map[string]string{"url": managedURL, "content_type": "json", "insecure_ssl": "0", "secret": secret},
	}
	encoded, _ := json.Marshal(body)
	return encoded
}

func generateWebhookTunnelSecret() (string, error) {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}

func webhookTunnelSecretPathForCLI(dbPath, ref string) string {
	dir := filepath.Dir(strings.TrimSpace(dbPath))
	if dir == "." || dir == "" || strings.HasPrefix(dbPath, "file:") || dbPath == ":memory:" {
		if home, err := os.UserHomeDir(); err == nil {
			dir = filepath.Join(home, ".looper")
		}
	}
	return filepath.Join(dir, "secrets", filepath.Base(ref))
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
