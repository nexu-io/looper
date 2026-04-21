package cliapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/powerformer/looper/internal/version"
	"github.com/spf13/cobra"
)

type upgradeCheckSummary struct {
	CLI    upgradeCLISummary    `json:"cli"`
	Daemon upgradeDaemonSummary `json:"daemon"`
}

type upgradeCLISummary struct {
	CurrentVersion  string `json:"currentVersion"`
	LatestVersion   string `json:"latestVersion"`
	UpdateAvailable bool   `json:"updateAvailable"`
}

type upgradeDaemonSummary struct {
	CurrentVersion  *string `json:"currentVersion"`
	LatestVersion   string  `json:"latestVersion"`
	UpdateAvailable bool    `json:"updateAvailable"`
	Installed       bool    `json:"installed"`
	Source          string  `json:"source"`
	BinaryPath      *string `json:"binaryPath"`
}

type latestDaemonReleaseInfo struct {
	Version string `json:"version"`
	Tag     string `json:"tag"`
}

type upgradeDaemonVersionState struct {
	Version    string
	Source     string
	BinaryPath *string
}

type daemonUpgradeOutput struct {
	Changed         bool    `json:"changed"`
	CurrentVersion  *string `json:"currentVersion,omitempty"`
	PreviousVersion *string `json:"previousVersion,omitempty"`
	LatestVersion   string  `json:"latestVersion"`
	BinaryPath      *string `json:"binaryPath,omitempty"`
	InstallPath     *string `json:"installPath,omitempty"`
	DownloadedFrom  *string `json:"downloadedFrom,omitempty"`
	Skipped         *bool   `json:"skipped,omitempty"`
}

func (r *commandRuntime) upgrade(cmd *cobra.Command, args []string) error {
	_ = args

	check := getBoolFlag(cmd, "check")
	daemonOnly := getBoolFlag(cmd, "daemon")

	if check && daemonOnly {
		return fmt.Errorf("--check and --daemon cannot be combined")
	}

	if check {
		summary, err := r.collectUpgradeCheckSummary(cmd.Context())
		if err != nil {
			return err
		}
		if getBoolFlag(cmd, "json") {
			return writeJSON(cmd.OutOrStdout(), summary)
		}
		return writeHumanUpgradeSummary(cmd.OutOrStdout(), summary)
	}

	if daemonOnly {
		return r.upgradeDaemon(cmd)
	}

	return fmt.Errorf("Full `looper upgrade` (CLI + daemon) is not implemented yet. Use `looper upgrade --check` or `looper upgrade --daemon`.")
}

func (r *commandRuntime) collectUpgradeCheckSummary(ctx context.Context) (upgradeCheckSummary, error) {
	latestCLIVersion, err := r.fetchLatestCLIVersion(ctx)
	if err != nil {
		return upgradeCheckSummary{}, err
	}

	latestDaemonRelease, err := r.fetchLatestDaemonRelease(ctx)
	if err != nil {
		return upgradeCheckSummary{}, err
	}

	statusPayload, err := r.currentDaemonStatusPayload(ctx)
	if err != nil {
		return upgradeCheckSummary{}, err
	}
	managedDaemon, err := r.readManagedUpgradeDaemonVersion(ctx)
	if err != nil {
		return upgradeCheckSummary{}, err
	}
	pathDaemon, err := r.readPathUpgradeDaemonVersion(ctx)
	if err != nil {
		return upgradeCheckSummary{}, err
	}
	currentDaemon := selectUpgradeDaemonVersionState(statusPayload, managedDaemon, pathDaemon)

	summary := upgradeCheckSummary{
		CLI: upgradeCLISummary{
			CurrentVersion:  version.Current().Version,
			LatestVersion:   latestCLIVersion,
			UpdateAvailable: normalizeVersion(version.Current().Version) != normalizeVersion(latestCLIVersion),
		},
		Daemon: upgradeDaemonSummary{LatestVersion: latestDaemonRelease.Version, Source: "not-installed"},
	}

	if currentDaemon != nil {
		summary.Daemon.CurrentVersion = stringPtr(currentDaemon.Version)
		summary.Daemon.UpdateAvailable = normalizeVersion(currentDaemon.Version) != normalizeVersion(latestDaemonRelease.Version)
		summary.Daemon.Installed = currentDaemon.Source == "installed-binary"
		summary.Daemon.Source = currentDaemon.Source
		summary.Daemon.BinaryPath = currentDaemon.BinaryPath
	} else {
		summary.Daemon.UpdateAvailable = true
	}

	return summary, nil
}

func (r *commandRuntime) upgradeDaemon(cmd *cobra.Command) error {
	ctx := cmd.Context()
	statusPayload, err := r.currentDaemonStatusPayload(ctx)
	if err != nil {
		return err
	}
	managedDaemon, err := r.readManagedUpgradeDaemonVersion(ctx)
	if err != nil {
		return err
	}
	pathDaemon, err := r.readPathUpgradeDaemonVersion(ctx)
	if err != nil {
		return err
	}
	current := selectUpgradeDaemonVersionState(statusPayload, managedDaemon, pathDaemon)
	latestRelease, err := r.fetchLatestDaemonRelease(ctx)
	if err != nil {
		return err
	}

	var currentVersion *string
	if managedDaemon != nil {
		currentVersion = stringPtr(managedDaemon.Version)
	} else if current != nil {
		currentVersion = stringPtr(current.Version)
	}

	needsInstall := managedDaemon == nil
	needsUpgrade := needsInstall || currentVersion == nil || normalizeVersion(*currentVersion) != normalizeVersion(latestRelease.Version)
	if !needsUpgrade {
		output := daemonUpgradeOutput{
			Changed:        false,
			CurrentVersion: currentVersion,
			LatestVersion:  latestRelease.Version,
		}
		if managedDaemon != nil {
			output.BinaryPath = managedDaemon.BinaryPath
		} else if current != nil {
			output.BinaryPath = current.BinaryPath
		}
		if getBoolFlag(cmd, "json") {
			return writeJSON(cmd.OutOrStdout(), output)
		}

		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "looperd is already up to date (%s)\n", *currentVersion); err != nil {
			return err
		}
		if output.BinaryPath != nil {
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Managed binary: %s\n", *output.BinaryPath)
			return err
		}
		return nil
	}

	result, err := r.installManagedDaemon(ctx, true, latestRelease.Tag)
	if err != nil {
		return fmt.Errorf("Failed to upgrade looperd: %w", err)
	}

	output := daemonUpgradeOutput{
		Changed:         true,
		PreviousVersion: daemonVersionPointer(current),
		LatestVersion:   latestRelease.Version,
		InstallPath:     stringPtr(result.InstallPath),
		DownloadedFrom:  result.DownloadedFrom,
		Skipped:         boolPtr(result.Skipped),
	}
	if getBoolFlag(cmd, "json") {
		return writeJSON(cmd.OutOrStdout(), output)
	}

	if managedDaemon == nil && pathDaemon != nil {
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Installed managed looperd %s to %s (previously using %s)\n", latestRelease.Version, result.InstallPath, *pathDaemon.BinaryPath); err != nil {
			return err
		}
	} else if managedDaemon == nil {
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Installed looperd %s to %s\n", latestRelease.Version, result.InstallPath); err != nil {
			return err
		}
	} else {
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Upgraded looperd %s → %s at %s\n", managedDaemon.Version, latestRelease.Version, result.InstallPath); err != nil {
			return err
		}
	}
	if result.DownloadedFrom != nil {
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Downloaded from %s\n", *result.DownloadedFrom); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(cmd.OutOrStdout(), "Restart the daemon to use the new version:"); err != nil {
		return err
	}
	_, err = fmt.Fprintln(cmd.OutOrStdout(), "  looper daemon restart")
	return err
}

func (r *commandRuntime) fetchLatestCLIVersion(ctx context.Context) (string, error) {
	release, err := r.fetchReleaseMetadata(ctx, "")
	if err != nil {
		return "", err
	}

	versionText := normalizeVersion(release.TagName)
	if versionText == "" {
		return "", fmt.Errorf("latest looper release metadata is missing tag_name")
	}

	return versionText, nil
}

func (r *commandRuntime) fetchLatestDaemonRelease(ctx context.Context) (latestDaemonReleaseInfo, error) {
	release, err := r.fetchReleaseMetadata(ctx, "")
	if err != nil {
		return latestDaemonReleaseInfo{}, err
	}

	versionText := normalizeVersion(release.TagName)
	if versionText == "" {
		return latestDaemonReleaseInfo{}, fmt.Errorf("latest looperd release metadata is missing tag_name")
	}

	tag := strings.TrimSpace(release.TagName)
	if tag == "" {
		tag = "v" + versionText
	}

	return latestDaemonReleaseInfo{Version: versionText, Tag: tag}, nil
}

func (r *commandRuntime) currentDaemonStatusPayload(ctx context.Context) (json.RawMessage, error) {
	client, err := r.apiClient()
	if err != nil {
		return nil, err
	}
	statusPayload, err := r.getJSONWithClient(ctx, client, "/api/v1/status")
	if err != nil {
		var apiError *DaemonAPIError
		if strings.Contains(err.Error(), "looperd is not reachable") || errors.As(err, &apiError) {
			return nil, nil
		}
		return nil, err
	}
	return statusPayload, nil
}

func (r *commandRuntime) readManagedUpgradeDaemonVersion(ctx context.Context) (*upgradeDaemonVersionState, error) {
	binaryPath, err := r.managedDaemonBinaryPath()
	if err != nil {
		return nil, err
	}
	return r.readUpgradeDaemonVersion(ctx, binaryPath, "installed-binary")
}

func (r *commandRuntime) readPathUpgradeDaemonVersion(ctx context.Context) (*upgradeDaemonVersionState, error) {
	return r.readUpgradeDaemonVersion(ctx, looperdBinaryName, "path-binary")
}

func (r *commandRuntime) readUpgradeDaemonVersion(ctx context.Context, command string, source string) (*upgradeDaemonVersionState, error) {
	versionText, err := r.runVersionCommand(ctx, command)
	if err != nil {
		return nil, err
	}
	if versionText == "" {
		return nil, nil
	}
	return &upgradeDaemonVersionState{Version: versionText, Source: source, BinaryPath: stringPtr(command)}, nil
}

func normalizeVersion(value string) string {
	return strings.TrimPrefix(strings.TrimSpace(value), "v")
}

func writeHumanUpgradeSummary(w io.Writer, summary upgradeCheckSummary) error {
	daemonCurrent := any("not installed")
	if summary.Daemon.CurrentVersion != nil {
		daemonCurrent = *summary.Daemon.CurrentVersion
	}
	daemonBinaryPath := any("-")
	if summary.Daemon.BinaryPath != nil {
		daemonBinaryPath = *summary.Daemon.BinaryPath
	}
	printSection(w, "Upgrade check", [][2]any{{"cliCurrent", summary.CLI.CurrentVersion}, {"cliLatest", summary.CLI.LatestVersion}, {"cliUpdateAvailable", summary.CLI.UpdateAvailable}, {"daemonCurrent", daemonCurrent}, {"daemonLatest", summary.Daemon.LatestVersion}, {"daemonUpdateAvailable", summary.Daemon.UpdateAvailable}, {"daemonSource", summary.Daemon.Source}, {"daemonBinaryPath", daemonBinaryPath}})
	return nil
}

func selectUpgradeDaemonVersionState(statusPayload json.RawMessage, managedDaemon *upgradeDaemonVersionState, pathDaemon *upgradeDaemonVersionState) *upgradeDaemonVersionState {
	if versionText := extractDaemonVersion(statusPayload); versionText != "" {
		return &upgradeDaemonVersionState{Version: versionText, Source: "api"}
	}
	if managedDaemon != nil {
		return managedDaemon
	}
	return pathDaemon
}

func daemonVersionPointer(state *upgradeDaemonVersionState) *string {
	if state == nil {
		return nil
	}
	return stringPtr(state.Version)
}

func boolPtr(value bool) *bool {
	return &value
}
