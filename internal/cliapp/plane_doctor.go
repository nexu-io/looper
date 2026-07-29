package cliapp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/config"
	networkclient "github.com/nexu-io/looper/internal/network/client"
	"github.com/nexu-io/looper/internal/planestrict"
	"github.com/spf13/cobra"
)

type planeDoctorCheck struct {
	Name        string `json:"name"`
	Status      string `json:"status"`
	Detail      string `json:"detail"`
	Remediation string `json:"remediation,omitempty"`
}

type planeDoctorResult struct {
	ProviderID string             `json:"providerId,omitempty"`
	Ready      bool               `json:"ready"`
	Checks     []planeDoctorCheck `json:"checks"`
}

func (r *commandRuntime) planeDoctor(cmd *cobra.Command, args []string) error {
	result := r.runPlaneDoctor(cmd.Context(), args)
	if getBoolFlag(cmd, "json") {
		if err := writeJSON(cmd.OutOrStdout(), result); err != nil {
			return err
		}
	} else if err := writeHumanPlaneDoctor(cmd.OutOrStdout(), result); err != nil {
		return err
	}
	if !result.Ready {
		return errors.New("Plane readiness checks failed; follow the actions above and rerun `looper plane doctor`")
	}
	return nil
}

func (r *commandRuntime) runPlaneDoctor(ctx context.Context, args []string) planeDoctorResult {
	result := planeDoctorResult{Ready: true}
	add := func(name, status, detail, remediation string) {
		result.Checks = append(result.Checks, planeDoctorCheck{Name: name, Status: status, Detail: detail, Remediation: remediation})
		if status == "failed" {
			result.Ready = false
		}
	}

	loaded, err := r.loadConfig()
	if err != nil {
		add("config", "failed", err.Error(), "Run `looper bootstrap`, or use Plane's Connect my Looper command from inside the local repository.")
		return result
	}
	if !loaded.Metadata.ConfigFilePresent {
		add("config", "failed", "no Looper config exists at "+loaded.Metadata.ConfigPath, "Run `looper bootstrap`, or use Plane's Connect my Looper command from inside the local repository.")
		return result
	}
	add("config", "passed", loaded.Metadata.ConfigPath, "")

	wanted := ""
	if len(args) == 1 {
		wanted = strings.TrimSpace(args[0])
	}
	provider, providerErr := selectPlaneDoctorProvider(loaded.Config, wanted)
	if providerErr != nil {
		add("plane-provider", "failed", providerErr.Error(), "Open the Plane connection page and run its `looper plane connect` command.")
		return result
	}
	result.ProviderID = provider.ID
	add("plane-provider", "passed", fmt.Sprintf("%s · %s · %s", provider.ID, stringPointerValue(provider.Workspace), stringPointerValue(provider.ProjectID)), "")

	project := findPlaneDoctorProject(loaded.Config, provider.ID)
	if project == nil {
		add("repository", "failed", "no local project uses this Plane provider", "Reconnect from inside the GitHub checkout, or pass --project-path and --code-repo to `looper plane connect`.")
	} else if info, statErr := os.Stat(project.RepoPath); statErr != nil || !info.IsDir() {
		add("repository", "failed", fmt.Sprintf("%s is unavailable", project.RepoPath), "Restore the checkout or reconnect with the correct --project-path.")
	} else {
		add("repository", "passed", fmt.Sprintf("%s · GitHub %s", project.RepoPath, project.Repo), "")
	}

	r.planeDoctorToolChecks(ctx, loaded.Config, add)

	tokenEnv := stringPointerValue(provider.TokenEnv)
	token := strings.TrimSpace(os.Getenv(tokenEnv))
	if tokenEnv == "" {
		add("plane-api-key", "failed", "provider tokenEnv is not configured", "Reconnect the provider with --plane-token-env PLANE_API_KEY.")
	} else if token == "" {
		add("plane-api-key", "failed", tokenEnv+" is empty", "Export the Plane API key in the daemon environment, then restart looperd.")
	} else {
		var identity planeLinkIdentity
		apiCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := r.planeJSONRequest(apiCtx, provider.BaseURL, token, "GET", "users/me", "", nil, &identity)
		cancel()
		if err != nil {
			add("plane-api-key", "failed", err.Error(), "Refresh the Plane API key and restart looperd.")
		} else {
			add("plane-api-key", "passed", "authenticated as member "+identity.ID, "")
		}
	}

	homeDir, homeErr := r.homeDir()
	if homeErr != nil {
		add("loopernet", "failed", homeErr.Error(), "Fix the local home directory configuration.")
	} else if state, stateErr := networkclient.LoadState(networkclient.DefaultStatePath(homeDir)); stateErr != nil {
		add("loopernet", "failed", stateErr.Error(), "Run the loopernet join command provided by your team setup.")
	} else {
		add("loopernet", "passed", fmt.Sprintf("%s · %s", state.NodeName, state.NodeID), "")
	}

	strict := provider.StrictDispatch
	if strict == nil || !strict.Enabled {
		add("node-binding", "failed", "strict Plane dispatch is not connected or enabled", "Use Plane's Connect my Looper page and repeat its one-time command.")
	} else if credentials, credentialsErr := planestrict.LoadCredentials(strict.BindingID, strict.KeyRevision, strict.NodeID, strict.PrivateKeyFile); credentialsErr != nil {
		add("node-binding", "failed", credentialsErr.Error(), "Reconnect this computer from Plane; never copy the private key from another machine.")
	} else {
		add("node-binding", "passed", fmt.Sprintf("node %s · key %s", strict.NodeID, filepath.Base(strict.PrivateKeyFile)), "")
		strictClient, clientErr := planestrict.NewClient(strict.BaseURL, stringPointerValue(provider.Workspace), stringPointerValue(provider.ProjectID), credentials, planestrict.WithHTTPClient(r.httpClient()))
		if clientErr != nil {
			add("signed-inbox", "failed", clientErr.Error(), "Reconnect this Plane provider.")
		} else {
			inboxCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
			inbox, inboxErr := strictClient.Inbox(inboxCtx, "")
			cancel()
			if inboxErr != nil {
				add("signed-inbox", "failed", inboxErr.Error(), "Check Plane reachability and whether this Node binding is still active.")
			} else if inbox.IntegrationState != "active" {
				add("signed-inbox", "failed", "project integration is "+inbox.IntegrationState, "Ask a Plane project admin to activate the Looper integration.")
			} else {
				add("signed-inbox", "passed", fmt.Sprintf("reachable · %d queued dispatches", len(inbox.Dispatches)), "")
			}
		}
	}

	daemonCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	probe, probeErr := r.probeDaemonStatus(daemonCtx, r.apiClientFromLoaded(loaded))
	cancel()
	if probeErr != nil || !probe.reachable || !probe.isLooperd {
		detail := "looperd is not reachable"
		if probeErr != nil {
			detail = probeErr.Error()
		}
		add("daemon", "failed", detail, "Run `looper daemon restart`; if it fails, run `looper upgrade --daemon`.")
	} else {
		add("daemon", "passed", "looperd API is reachable", "")
	}

	return result
}

func selectPlaneDoctorProvider(cfg config.Config, wanted string) (config.ProviderConfig, error) {
	var selected *config.ProviderConfig
	for index := range cfg.Providers {
		provider := &cfg.Providers[index]
		if provider.Kind != config.ProviderKindPlane || (wanted != "" && provider.ID != wanted) {
			continue
		}
		if selected != nil && wanted == "" {
			return config.ProviderConfig{}, errors.New("multiple Plane providers exist; pass a provider id")
		}
		selected = provider
	}
	if selected == nil {
		return config.ProviderConfig{}, errors.New("Plane provider was not found")
	}
	return *selected, nil
}

func findPlaneDoctorProject(cfg config.Config, providerID string) *config.ProjectRefConfig {
	for index := range cfg.Projects {
		if cfg.Projects[index].Provider == providerID {
			return &cfg.Projects[index]
		}
	}
	return nil
}

func (r *commandRuntime) planeDoctorToolChecks(ctx context.Context, cfg config.Config, add func(string, string, string, string)) {
	checkTool := func(name, configured, fallback, remediation string) string {
		candidate := strings.TrimSpace(configured)
		if candidate == "" {
			candidate = fallback
		}
		path, err := r.lookPath()(candidate)
		if err != nil || strings.TrimSpace(path) == "" {
			add(name, "failed", fallback+" was not found", remediation)
			return ""
		}
		add(name, "passed", path, "")
		return path
	}
	gitPath, ghPath, planePath := "", "", ""
	if cfg.Tools.GitPath != nil {
		gitPath = *cfg.Tools.GitPath
	}
	if cfg.Tools.GHPath != nil {
		ghPath = *cfg.Tools.GHPath
	}
	if cfg.Tools.PlanePath != nil {
		planePath = *cfg.Tools.PlanePath
	}
	checkTool("git", gitPath, "git", "Install Git, for example `brew install git`.")
	ghPath = checkTool("github-cli", ghPath, "gh", "Install GitHub CLI and run `gh auth login`.")
	planePath = checkTool("plane-cli", planePath, "plane", "Install and authenticate the Plane CLI; Looper currently uses it for spec pages and links.")
	if ghPath != "" {
		probe, err := r.runCommand(ctx, ghPath, []string{"auth", "status"}, 5*time.Second)
		if err != nil || probe.ExitCode != 0 {
			add("github-auth", "failed", strings.TrimSpace(probe.Stderr), "Run `gh auth login`.")
		} else {
			add("github-auth", "passed", "GitHub CLI is authenticated", "")
		}
	}
	if planePath != "" {
		probe, err := r.runCommand(ctx, planePath, []string{"api", "me"}, 5*time.Second)
		if err != nil || probe.ExitCode != 0 {
			add("plane-cli-auth", "failed", strings.TrimSpace(probe.Stderr), "Log in with the Plane CLI and rerun this check.")
		} else {
			add("plane-cli-auth", "passed", "Plane CLI is authenticated", "")
		}
	}
	agentCommand := ""
	if command, ok := cfg.Agent.Params["command"].(string); ok {
		agentCommand = strings.TrimSpace(command)
	}
	if agentCommand == "" && cfg.Agent.Vendor != nil {
		switch *cfg.Agent.Vendor {
		case config.AgentVendorClaudeCode:
			agentCommand = "claude"
		case config.AgentVendorCursorCLI:
			agentCommand = "agent"
		case config.AgentVendorGrokBuild:
			agentCommand = "grok"
		default:
			agentCommand = string(*cfg.Agent.Vendor)
		}
	}
	if agentCommand == "" {
		add("coding-agent", "failed", "no coding agent vendor is configured", "Run `looper bootstrap` and choose Claude Code, Codex, or OpenCode.")
	} else {
		checkTool("coding-agent", "", agentCommand, "Install and log in to the configured coding agent: "+agentCommand+".")
	}
}

func writeHumanPlaneDoctor(writer io.Writer, result planeDoctorResult) error {
	if _, err := fmt.Fprintln(writer, "Plane Looper readiness"); err != nil {
		return err
	}
	for _, check := range result.Checks {
		mark := "✓"
		if check.Status == "failed" {
			mark = "✗"
		}
		if _, err := fmt.Fprintf(writer, "%s %-16s %s\n", mark, check.Name, check.Detail); err != nil {
			return err
		}
		if check.Remediation != "" {
			if _, err := fmt.Fprintf(writer, "  → %s\n", check.Remediation); err != nil {
				return err
			}
		}
	}
	if result.Ready {
		_, err := fmt.Fprintln(writer, "\nReady: this computer can receive Plane work.")
		return err
	}
	_, err := fmt.Fprintln(writer, "\nNot ready: fix the failed checks above.")
	return err
}
