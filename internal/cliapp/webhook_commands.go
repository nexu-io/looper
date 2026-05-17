package cliapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/config"
	pkgapi "github.com/nexu-io/looper/pkg/api"
	"github.com/spf13/cobra"
)

const ghWebhookExtension = "cli/gh-webhook"

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
