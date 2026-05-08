package cliapp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/version"
	"github.com/spf13/cobra"
)

const (
	autoUpgradeStateSchemaVersion = 1
	autoUpgradeCheckInterval      = 24 * time.Hour
)

type autoUpgradeState struct {
	SchemaVersion int        `json:"schemaVersion"`
	LastCheckedAt *time.Time `json:"lastCheckedAt,omitempty"`
}

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

type cliUpgradeOutput struct {
	Changed         bool    `json:"changed"`
	CurrentVersion  string  `json:"currentVersion"`
	LatestVersion   string  `json:"latestVersion"`
	BinaryPath      *string `json:"binaryPath,omitempty"`
	PreviousBinary  *string `json:"previousBinary,omitempty"`
	DownloadedFrom  *string `json:"downloadedFrom,omitempty"`
	InstallSource   string  `json:"installSource"`
	Skipped         *bool   `json:"skipped,omitempty"`
	Refused         *bool   `json:"refused,omitempty"`
	RefusedGuidance *string `json:"refusedGuidance,omitempty"`
}

type unifiedUpgradeOutput struct {
	CLI    cliUpgradeOutput    `json:"cli"`
	Daemon daemonUpgradeOutput `json:"daemon"`
}

type cliInstallSource string

const (
	cliInstallSourceRelease  cliInstallSource = "release-binary"
	cliInstallSourceHomebrew cliInstallSource = "homebrew"
	cliInstallSourceDev      cliInstallSource = "dev"
	cliInstallSourceUnknown  cliInstallSource = "unknown"
)

type cliUpgradeRefusedError struct {
	message string
}

func (e *cliUpgradeRefusedError) Error() string {
	return e.message
}

func (r *commandRuntime) maybeRunAutoUpgrade(cmd *cobra.Command, args []string) error {
	_ = args
	if shouldSkipAutoUpgrade(cmd) {
		return nil
	}
	execPath, err := r.executablePath()
	if err != nil {
		return nil
	}
	if detectCLIInstallSource(execPath) != cliInstallSourceRelease {
		return nil
	}

	loaded, err := r.loadConfig()
	if err != nil {
		return nil
	}
	if !loaded.Config.Package.AutoUpgradeEnabled {
		return nil
	}

	statePath, err := r.resolveAutoUpgradeStatePath()
	if err != nil {
		return nil
	}
	state, err := r.readAutoUpgradeState(statePath)
	if err != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Auto-upgrade skipped: read state: %v\n", err)
		return nil
	}
	if !shouldRunAutoUpgradeCheck(state, time.Now()) {
		return nil
	}

	autoCmd := newAutoUpgradeCommand(cmd)
	r.runAutoUpgrade(autoCmd)
	if err := r.writeAutoUpgradeState(statePath, autoUpgradeState{LastCheckedAt: timePtr(time.Now())}); err != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Auto-upgrade state update failed: %v\n", err)
	}
	return nil
}

func shouldSkipAutoUpgrade(cmd *cobra.Command) bool {
	if cmd == nil {
		return true
	}
	if cmd.Name() == "help" {
		return true
	}
	if cmd.RunE != nil {
		if reflect.ValueOf(cmd.RunE).Pointer() == reflect.ValueOf(helpCommand).Pointer() {
			return true
		}
	}
	path := strings.TrimSpace(cmd.CommandPath())
	return path == "looper" || path == "looper upgrade"
}

func newAutoUpgradeCommand(parent *cobra.Command) *cobra.Command {
	cmd := &cobra.Command{}
	if parent != nil {
		cmd.SetContext(parent.Context())
		cmd.SetOut(parent.ErrOrStderr())
		cmd.SetErr(parent.ErrOrStderr())
	}
	return cmd
}

func (r *commandRuntime) runAutoUpgrade(cmd *cobra.Command) {
	cliOutput, cliErr := r.upgradeCLIWithOutput(cmd, false)
	if cliErr != nil {
		var refused *cliUpgradeRefusedError
		if errors.As(cliErr, &refused) {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Auto-upgrade: CLI skipped: %s\n", refused.message)
		} else {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Auto-upgrade: CLI check failed: %v\n", cliErr)
		}
	} else if cliOutput.Changed {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Auto-upgrade: upgraded looper %s → %s\n", cliOutput.CurrentVersion, cliOutput.LatestVersion)
	}

	managedDaemon, err := r.readManagedUpgradeDaemonVersion(cmd.Context())
	if err != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Auto-upgrade: daemon check failed: %v\n", err)
		return
	}
	if managedDaemon == nil {
		return
	}

	statusPayload, statusErr := r.currentDaemonStatusPayload(cmd.Context())
	if statusErr != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Auto-upgrade: daemon status check failed: %v\n", statusErr)
		return
	}
	pathDaemon, pathErr := r.readPathUpgradeDaemonVersion(cmd.Context())
	if pathErr != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Auto-upgrade: daemon path version check failed: %v\n", pathErr)
		return
	}
	runningDaemon := selectUpgradeDaemonVersionState(statusPayload, managedDaemon, pathDaemon)

	daemonOutput, daemonErr := r.upgradeDaemonWithOutput(cmd, false)
	if daemonErr != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Auto-upgrade: daemon check failed: %v\n", daemonErr)
		return
	}
	if !daemonOutput.Changed {
		return
	}

	if daemonOutput.PreviousVersion != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Auto-upgrade: installed looperd %s → %s\n", *daemonOutput.PreviousVersion, daemonOutput.LatestVersion)
	} else {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Auto-upgrade: installed looperd %s\n", daemonOutput.LatestVersion)
	}
	if len(statusPayload) > 0 && runningDaemon != nil && runningDaemon.Source == "installed-binary" && runningDaemon.Version != daemonOutput.LatestVersion {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Auto-upgrade: looperd %s is installed on disk, but the running daemon is still %s. Run `looper daemon restart`.\n", daemonOutput.LatestVersion, runningDaemon.Version)
	}
}

func (r *commandRuntime) resolveAutoUpgradeStatePath() (string, error) {
	homeDir, err := r.homeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(homeDir, ".looper", "auto-upgrade.state.json"), nil
}

func (r *commandRuntime) readAutoUpgradeState(path string) (*autoUpgradeState, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var state autoUpgradeState
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, fmt.Errorf("decode auto-upgrade state: %w", err)
	}
	if state.SchemaVersion != 0 && state.SchemaVersion != autoUpgradeStateSchemaVersion {
		return nil, fmt.Errorf("unsupported auto-upgrade state schemaVersion %d", state.SchemaVersion)
	}
	return &state, nil
}

func (r *commandRuntime) writeAutoUpgradeState(path string, state autoUpgradeState) error {
	state.SchemaVersion = autoUpgradeStateSchemaVersion
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.tmp.%d", path, time.Now().UnixNano())
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func shouldRunAutoUpgradeCheck(state *autoUpgradeState, now time.Time) bool {
	if state == nil || state.LastCheckedAt == nil {
		return true
	}
	return now.Sub(*state.LastCheckedAt) >= autoUpgradeCheckInterval
}

func timePtr(value time.Time) *time.Time {
	return &value
}

func (r *commandRuntime) upgrade(cmd *cobra.Command, args []string) error {
	_ = args

	check := getBoolFlag(cmd, "check")
	cliOnly := getBoolFlag(cmd, "cli")
	daemonOnly := getBoolFlag(cmd, "daemon")

	selectedPaths := 0
	if check {
		selectedPaths++
	}
	if cliOnly {
		selectedPaths++
	}
	if daemonOnly {
		selectedPaths++
	}
	if selectedPaths > 1 {
		return fmt.Errorf("--check, --cli, and --daemon cannot be combined")
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

	if cliOnly {
		_, err := r.upgradeCLI(cmd)
		return err
	}

	return r.upgradeUnified(cmd)
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
			CurrentVersion: version.Current().Version,
			LatestVersion:  latestCLIVersion,
		},
		Daemon: upgradeDaemonSummary{LatestVersion: latestDaemonRelease.Version, Source: "not-installed"},
	}
	cliUpgradeAvailable, err := isSemverUpgradeAvailable(version.Current().Version, latestCLIVersion)
	if err != nil {
		return upgradeCheckSummary{}, fmt.Errorf("compare CLI versions: %w", err)
	}
	summary.CLI.UpdateAvailable = cliUpgradeAvailable

	if currentDaemon != nil {
		summary.Daemon.CurrentVersion = stringPtr(currentDaemon.Version)
		daemonUpgradeAvailable, err := isSemverUpgradeAvailable(currentDaemon.Version, latestDaemonRelease.Version)
		if err != nil {
			return upgradeCheckSummary{}, fmt.Errorf("compare daemon versions: %w", err)
		}
		summary.Daemon.UpdateAvailable = daemonUpgradeAvailable
		summary.Daemon.Installed = currentDaemon.Source == "installed-binary"
		summary.Daemon.Source = currentDaemon.Source
		summary.Daemon.BinaryPath = currentDaemon.BinaryPath
	} else {
		summary.Daemon.UpdateAvailable = true
	}

	return summary, nil
}

func (r *commandRuntime) upgradeDaemon(cmd *cobra.Command) error {
	_, err := r.upgradeDaemonWithOutput(cmd, true)
	return err
}

func (r *commandRuntime) upgradeDaemonWithOutput(cmd *cobra.Command, emitOutput bool) (daemonUpgradeOutput, error) {
	ctx := cmd.Context()
	statusPayload, err := r.currentDaemonStatusPayload(ctx)
	if err != nil {
		return daemonUpgradeOutput{}, err
	}
	managedDaemon, err := r.readManagedUpgradeDaemonVersion(ctx)
	if err != nil {
		return daemonUpgradeOutput{}, err
	}
	pathDaemon, err := r.readPathUpgradeDaemonVersion(ctx)
	if err != nil {
		return daemonUpgradeOutput{}, err
	}
	current := selectUpgradeDaemonVersionState(statusPayload, managedDaemon, pathDaemon)
	latestRelease, err := r.fetchLatestDaemonRelease(ctx)
	if err != nil {
		return daemonUpgradeOutput{}, err
	}

	var currentVersion *string
	if managedDaemon != nil {
		currentVersion = stringPtr(managedDaemon.Version)
	} else if current != nil {
		currentVersion = stringPtr(current.Version)
	}

	needsInstall := managedDaemon == nil
	needsUpgrade := needsInstall || currentVersion == nil
	if !needsUpgrade {
		available, err := isSemverUpgradeAvailable(*currentVersion, latestRelease.Version)
		if err != nil {
			return daemonUpgradeOutput{}, fmt.Errorf("compare daemon versions: %w", err)
		}
		needsUpgrade = available
	}
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
		if emitOutput && getBoolFlag(cmd, "json") {
			return output, writeJSON(cmd.OutOrStdout(), output)
		}
		if !emitOutput {
			return output, nil
		}

		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "looperd is already up to date (%s)\n", *currentVersion); err != nil {
			return daemonUpgradeOutput{}, err
		}
		if output.BinaryPath != nil {
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Managed binary: %s\n", *output.BinaryPath)
			return output, err
		}
		return output, nil
	}

	result, err := r.installManagedDaemon(ctx, true, latestRelease.Tag, cmd.ErrOrStderr())
	if err != nil {
		return daemonUpgradeOutput{}, fmt.Errorf("Failed to upgrade looperd: %w", err)
	}

	output := daemonUpgradeOutput{
		Changed:         true,
		PreviousVersion: daemonVersionPointer(current),
		LatestVersion:   latestRelease.Version,
		InstallPath:     stringPtr(result.InstallPath),
		DownloadedFrom:  result.DownloadedFrom,
		Skipped:         boolPtr(result.Skipped),
	}
	if emitOutput && getBoolFlag(cmd, "json") {
		return output, writeJSON(cmd.OutOrStdout(), output)
	}
	if !emitOutput {
		return output, nil
	}

	if managedDaemon == nil && pathDaemon != nil {
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Installed managed looperd %s to %s (previously using %s)\n", latestRelease.Version, result.InstallPath, *pathDaemon.BinaryPath); err != nil {
			return daemonUpgradeOutput{}, err
		}
	} else if managedDaemon == nil {
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Installed looperd %s to %s\n", latestRelease.Version, result.InstallPath); err != nil {
			return daemonUpgradeOutput{}, err
		}
	} else {
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Upgraded looperd %s → %s at %s\n", managedDaemon.Version, latestRelease.Version, result.InstallPath); err != nil {
			return daemonUpgradeOutput{}, err
		}
	}
	if result.DownloadedFrom != nil {
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Downloaded from %s\n", *result.DownloadedFrom); err != nil {
			return daemonUpgradeOutput{}, err
		}
	}
	if _, err := fmt.Fprintln(cmd.OutOrStdout(), "Restart the daemon to use the new version:"); err != nil {
		return daemonUpgradeOutput{}, err
	}
	_, err = fmt.Fprintln(cmd.OutOrStdout(), "  looper daemon restart")
	return output, err
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

func (r *commandRuntime) upgradeUnified(cmd *cobra.Command) error {
	jsonOutput := getBoolFlag(cmd, "json")
	cliOutput, cliErr := r.upgradeCLIWithOutput(cmd, !jsonOutput)
	if cliErr != nil {
		var refused *cliUpgradeRefusedError
		if !errors.As(cliErr, &refused) {
			return cliErr
		}
		if !jsonOutput {
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "CLI self-upgrade skipped: %s\n", refused.message); err != nil {
				return err
			}
		}
	}

	if !jsonOutput {
		if _, err := fmt.Fprintln(cmd.OutOrStdout(), "Proceeding with daemon upgrade..."); err != nil {
			return err
		}
	}

	daemonOutput, err := r.upgradeDaemonWithOutput(cmd, !jsonOutput)
	if err != nil {
		return err
	}
	if jsonOutput {
		return writeJSON(cmd.OutOrStdout(), unifiedUpgradeOutput{CLI: cliOutput, Daemon: daemonOutput})
	}
	return nil
}

func (r *commandRuntime) upgradeCLI(cmd *cobra.Command) (cliUpgradeOutput, error) {
	return r.upgradeCLIWithOutput(cmd, true)
}

func (r *commandRuntime) upgradeCLIWithOutput(cmd *cobra.Command, emitOutput bool) (cliUpgradeOutput, error) {
	ctx := cmd.Context()
	execPath, err := r.executablePath()
	if err != nil {
		return cliUpgradeOutput{}, fmt.Errorf("resolve current looper path: %w", err)
	}
	installSource := detectCLIInstallSource(execPath)
	guidance := cliRefusalGuidance(installSource, execPath)
	if installSource != cliInstallSourceRelease {
		refused := true
		result := cliUpgradeOutput{Changed: false, CurrentVersion: version.Current().Version, BinaryPath: stringPtr(execPath), InstallSource: string(installSource), Refused: &refused, RefusedGuidance: &guidance}
		if emitOutput && getBoolFlag(cmd, "json") {
			if err := writeJSON(cmd.OutOrStdout(), result); err != nil {
				return cliUpgradeOutput{}, err
			}
		}
		return result, &cliUpgradeRefusedError{message: guidance}
	}

	latestRelease, err := r.fetchReleaseMetadata(ctx, "")
	if err != nil {
		return cliUpgradeOutput{}, err
	}
	latestVersion := normalizeVersion(latestRelease.TagName)
	if latestVersion == "" {
		return cliUpgradeOutput{}, fmt.Errorf("latest looper release metadata is missing tag_name")
	}

	available, err := isSemverUpgradeAvailable(version.Current().Version, latestVersion)
	if err != nil {
		return cliUpgradeOutput{}, fmt.Errorf("compare CLI versions: %w", err)
	}
	if !available {
		skipped := true
		result := cliUpgradeOutput{Changed: false, CurrentVersion: version.Current().Version, LatestVersion: latestVersion, BinaryPath: stringPtr(execPath), InstallSource: string(installSource), Skipped: &skipped}
		if emitOutput && getBoolFlag(cmd, "json") {
			if err := writeJSON(cmd.OutOrStdout(), result); err != nil {
				return cliUpgradeOutput{}, err
			}
		}
		if emitOutput && !getBoolFlag(cmd, "json") {
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "looper is already up to date (%s)\n", version.Current().Version); err != nil {
				return cliUpgradeOutput{}, err
			}
		}
		return result, nil
	}
	if err := preflightSelfUpgradeReplace(execPath); err != nil {
		refused := true
		guidance := cliSelfUpgradeWriteGuidance(execPath, err)
		result := cliUpgradeOutput{Changed: false, CurrentVersion: version.Current().Version, LatestVersion: latestVersion, BinaryPath: stringPtr(execPath), InstallSource: string(installSource), Refused: &refused, RefusedGuidance: &guidance}
		if emitOutput && getBoolFlag(cmd, "json") {
			if err := writeJSON(cmd.OutOrStdout(), result); err != nil {
				return cliUpgradeOutput{}, err
			}
		}
		return result, &cliUpgradeRefusedError{message: guidance}
	}

	target, err := resolveLooperTarget(r.platform(), r.arch())
	if err != nil {
		return cliUpgradeOutput{}, err
	}
	binaryAsset, checksumAsset, err := findLooperReleaseAssets(latestRelease, target)
	if err != nil {
		return cliUpgradeOutput{}, err
	}
	binaryBytes, err := r.downloadBinary(ctx, binaryAsset.BrowserDownloadURL, binaryAsset.Name, cmd.ErrOrStderr())
	if err != nil {
		return cliUpgradeOutput{}, fmt.Errorf("failed to download looper binary: %w", err)
	}
	checksumText, err := r.downloadChecksum(ctx, checksumAsset.BrowserDownloadURL)
	if err != nil {
		return cliUpgradeOutput{}, fmt.Errorf("failed to download looper checksum: %w", err)
	}
	expectedChecksum, err := parseChecksum(checksumText)
	if err != nil {
		return cliUpgradeOutput{}, err
	}
	actualChecksum := sha256.Sum256(binaryBytes)
	if hex.EncodeToString(actualChecksum[:]) != expectedChecksum {
		return cliUpgradeOutput{}, fmt.Errorf("downloaded looper checksum mismatch: expected %s, received %s", expectedChecksum, hex.EncodeToString(actualChecksum[:]))
	}
	if err := replaceBinaryAtomically(execPath, binaryBytes); err != nil {
		return cliUpgradeOutput{}, err
	}

	prevPath := execPath + ".prev"
	result := cliUpgradeOutput{Changed: true, CurrentVersion: version.Current().Version, LatestVersion: latestVersion, BinaryPath: stringPtr(execPath), PreviousBinary: stringPtr(prevPath), DownloadedFrom: stringPtr(binaryAsset.BrowserDownloadURL), InstallSource: string(installSource)}
	if emitOutput && getBoolFlag(cmd, "json") {
		if err := writeJSON(cmd.OutOrStdout(), result); err != nil {
			return cliUpgradeOutput{}, err
		}
		return result, nil
	}
	if !emitOutput {
		return result, nil
	}
	if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Upgraded looper %s → %s at %s\n", version.Current().Version, latestVersion, execPath); err != nil {
		return cliUpgradeOutput{}, err
	}
	if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Previous binary kept at %s\n", prevPath); err != nil {
		return cliUpgradeOutput{}, err
	}
	if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Downloaded from %s\n", binaryAsset.BrowserDownloadURL); err != nil {
		return cliUpgradeOutput{}, err
	}
	return result, nil
}

func detectCLIInstallSource(execPath string) cliInstallSource {
	path := strings.ToLower(execPath)
	if strings.Contains(path, "/cellar/") || strings.Contains(path, "/homebrew/") {
		return cliInstallSourceHomebrew
	}
	base := strings.ToLower(filepath.Base(path))
	if base != "looper" {
		return cliInstallSourceUnknown
	}
	if strings.HasSuffix(path, "/.local/bin/looper") || strings.HasSuffix(path, "/usr/local/bin/looper") || strings.HasSuffix(path, "/.looper/bin/looper") {
		return cliInstallSourceRelease
	}
	if strings.Contains(path, "/go/bin/") || strings.Contains(path, "/go-build/") || strings.Contains(path, "/tmp/") {
		return cliInstallSourceDev
	}
	if isInstallerSelectedUserBinPath(execPath) {
		return cliInstallSourceRelease
	}
	return cliInstallSourceUnknown
}

func isInstallerSelectedUserBinPath(execPath string) bool {
	homeDir, err := os.UserHomeDir()
	if err != nil || homeDir == "" {
		return false
	}
	execDir := filepath.Clean(filepath.Dir(execPath))
	homeDir = filepath.Clean(homeDir)
	if execDir == homeDir || !strings.HasPrefix(execDir, homeDir+string(os.PathSeparator)) {
		return false
	}

	for _, entry := range filepath.SplitList(os.Getenv("PATH")) {
		if filepath.Clean(entry) == execDir {
			return true
		}
	}
	return false
}

func (r *commandRuntime) executablePath() (string, error) {
	resolve := func(path string) string {
		resolvedPath, err := filepath.EvalSymlinks(path)
		if err != nil {
			return path
		}
		resolvedPath = strings.TrimSpace(resolvedPath)
		if resolvedPath == "" {
			return path
		}
		return resolvedPath
	}

	if value := strings.TrimSpace(r.app.deps.ExecutablePath); value != "" {
		return resolve(value), nil
	}
	path, err := os.Executable()
	if err != nil {
		return "", err
	}
	path = strings.TrimSpace(path)
	if path == "" {
		return path, nil
	}
	return resolve(path), nil
}

func cliRefusalGuidance(source cliInstallSource, execPath string) string {
	switch source {
	case cliInstallSourceHomebrew:
		return "this looper binary looks Homebrew-managed. Upgrade with `brew upgrade looper` (or your tap formula)."
	case cliInstallSourceDev:
		return "this looper binary looks like a dev/go-install build. Reinstall with `go install ./cmd/looper` (or rebuild locally)."
	default:
		return fmt.Sprintf("cannot safely self-upgrade looper from %q. Reinstall from a release binary or use your package manager.", execPath)
	}
}

func cliSelfUpgradeWriteGuidance(execPath string, err error) string {
	return fmt.Sprintf("cannot self-upgrade looper at %q because the install location is not writable: %v. Reinstall looper into a user-writable directory or use your package manager for this installation.", execPath, err)
}

func preflightSelfUpgradeReplace(installPath string) error {
	installDir := filepath.Dir(installPath)
	if err := os.MkdirAll(installDir, 0o755); err != nil {
		return fmt.Errorf("prepare install directory %q: %w", installDir, err)
	}
	tempFile, err := os.CreateTemp(installDir, ".looper-upgrade-check-*")
	if err != nil {
		return fmt.Errorf("create test file in %q: %w", installDir, err)
	}
	tempPath := tempFile.Name()
	if err := tempFile.Close(); err != nil {
		_ = removeTempInstallFile(tempPath)
		return fmt.Errorf("close test file in %q: %w", installDir, err)
	}
	renamedPath := tempPath + ".rename"
	_ = os.Remove(renamedPath)
	if err := os.Rename(tempPath, renamedPath); err != nil {
		_ = removeTempInstallFile(tempPath)
		return fmt.Errorf("rename test file in %q: %w", installDir, err)
	}
	if err := removeTempInstallFile(renamedPath); err != nil {
		return fmt.Errorf("remove test file in %q: %w", installDir, err)
	}
	return nil
}

func resolveLooperTarget(platform string, arch string) (string, error) {
	if platform == "darwin" && arch == "arm64" {
		return "darwin-arm64", nil
	}
	return "", fmt.Errorf("unsupported platform/arch for looper upgrade: %s-%s. Supported targets: darwin-arm64", platform, arch)
}

func findLooperReleaseAssets(release githubReleasePayload, target string) (githubReleaseAsset, githubReleaseAsset, error) {
	binaryName := "looper-" + target
	checksumName := binaryName + ".sha256"
	var binaryAsset githubReleaseAsset
	var checksumAsset githubReleaseAsset
	for _, asset := range release.Assets {
		if asset.Name == binaryName {
			binaryAsset = asset
		}
		if asset.Name == checksumName {
			checksumAsset = asset
		}
	}
	if strings.TrimSpace(binaryAsset.BrowserDownloadURL) == "" {
		return githubReleaseAsset{}, githubReleaseAsset{}, fmt.Errorf("release is missing asset %q", binaryName)
	}
	if strings.TrimSpace(checksumAsset.BrowserDownloadURL) == "" {
		return githubReleaseAsset{}, githubReleaseAsset{}, fmt.Errorf("release is missing asset %q", checksumName)
	}
	return binaryAsset, checksumAsset, nil
}

func replaceBinaryAtomically(installPath string, binaryBytes []byte) error {
	return replaceBinaryAtomicallyWithRename(installPath, binaryBytes, os.Rename)
}

func replaceBinaryAtomicallyWithRename(installPath string, binaryBytes []byte, rename func(string, string) error) error {
	installDir := filepath.Dir(installPath)
	tempInstallPath := installPath + ".new"
	prevPath := installPath + ".prev"
	previousPreserved := false
	if err := os.MkdirAll(installDir, 0o755); err != nil {
		return fmt.Errorf("create install directory: %w", err)
	}
	if err := os.WriteFile(tempInstallPath, binaryBytes, 0o755); err != nil {
		_ = removeTempInstallFile(tempInstallPath)
		return fmt.Errorf("write staged binary: %w", err)
	}
	if err := os.Chmod(tempInstallPath, 0o755); err != nil {
		_ = removeTempInstallFile(tempInstallPath)
		return fmt.Errorf("chmod staged binary: %w", err)
	}
	if _, err := os.Stat(installPath); err == nil {
		_ = os.Remove(prevPath)
		if err := rename(installPath, prevPath); err != nil {
			_ = removeTempInstallFile(tempInstallPath)
			return fmt.Errorf("preserve previous looper binary: %w", err)
		}
		previousPreserved = true
	}
	if err := rename(tempInstallPath, installPath); err != nil {
		_ = removeTempInstallFile(tempInstallPath)
		if previousPreserved {
			if restoreErr := rename(prevPath, installPath); restoreErr != nil {
				return fmt.Errorf("replace looper binary: %w (failed to restore previous binary from %s: %v)", err, prevPath, restoreErr)
			}
		}
		return fmt.Errorf("replace looper binary: %w", err)
	}
	return nil
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
	serviceBinary := extractDaemonServiceBinary(statusPayload)
	if serviceBinary.Version != "" {
		state := &upgradeDaemonVersionState{Version: serviceBinary.Version, Source: "api"}
		if serviceBinary.Path != "" {
			state.BinaryPath = stringPtr(serviceBinary.Path)
			if managedDaemon != nil && managedDaemon.BinaryPath != nil && serviceBinary.Path == *managedDaemon.BinaryPath {
				state.Source = managedDaemon.Source
			} else if pathDaemon != nil && pathDaemon.BinaryPath != nil && serviceBinary.Path == *pathDaemon.BinaryPath {
				state.Source = pathDaemon.Source
			}
		}
		return state
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
