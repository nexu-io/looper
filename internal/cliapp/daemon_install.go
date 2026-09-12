package cliapp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/release"
	"github.com/spf13/cobra"
)

const (
	defaultReleaseOwner           = "nexu-io"
	defaultReleaseRepo            = "looper"
	defaultReleaseManifestBaseURL = "https://releases.looper.powerformer.com"
	looperdBinaryName             = "looperd"
	looperdUserAgent              = "looper-cli"
	releaseMetadataTimeout        = 5 * time.Second
	maxReleaseMetadataBytes       = 1 << 20
)

type daemonInstallResult struct {
	Target         string  `json:"target"`
	InstallPath    string  `json:"installPath"`
	DownloadedFrom *string `json:"downloadedFrom"`
	Skipped        bool    `json:"skipped"`
}

type preparedDaemonInstall struct {
	result      daemonInstallResult
	binaryBytes []byte
}

type githubReleasePayload struct {
	TagName string               `json:"tag_name"`
	Assets  []githubReleaseAsset `json:"assets"`
}

type githubReleaseAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

func (r *commandRuntime) daemonInstall(cmd *cobra.Command, args []string) error {
	_ = args

	result, err := r.installManagedDaemon(cmd.Context(), getBoolFlag(cmd, "force"), "", cmd.ErrOrStderr())
	if err != nil {
		return fmt.Errorf("Failed to install looperd: %w", err)
	}

	if getBoolFlag(cmd, "json") {
		return writeJSON(cmd.OutOrStdout(), result)
	}

	if result.Skipped {
		_, err := fmt.Fprintf(cmd.OutOrStdout(), "looperd is already installed at %s (use --force to overwrite)\n", result.InstallPath)
		return err
	}

	if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Installed looperd (%s) to %s\n", result.Target, result.InstallPath); err != nil {
		return err
	}
	if result.DownloadedFrom != nil {
		_, err := fmt.Fprintf(cmd.OutOrStdout(), "Downloaded from %s\n", *result.DownloadedFrom)
		return err
	}

	return nil
}

func (r *commandRuntime) installManagedDaemon(ctx context.Context, force bool, tag string, progress io.Writer) (daemonInstallResult, error) {
	prepared, err := r.prepareManagedDaemonInstall(ctx, force, tag, progress)
	if err != nil {
		return daemonInstallResult{}, err
	}
	if err := commitPreparedDaemonInstall(prepared); err != nil {
		return daemonInstallResult{}, err
	}
	return prepared.result, nil
}

func (r *commandRuntime) prepareManagedDaemonInstall(ctx context.Context, force bool, tag string, progress io.Writer) (preparedDaemonInstall, error) {
	homeDir, err := r.homeDir()
	if err != nil {
		return preparedDaemonInstall{}, err
	}

	target, err := resolveLooperdTarget(r.platform(), r.arch())
	if err != nil {
		return preparedDaemonInstall{}, err
	}

	installDir := filepath.Join(homeDir, ".looper", "bin")
	installPath := filepath.Join(installDir, looperdBinaryName)
	if !force {
		state, err := r.checkManagedDaemonBinary(ctx)
		if err != nil {
			return preparedDaemonInstall{}, err
		}
		if state.Exists {
			return preparedDaemonInstall{result: daemonInstallResult{Target: target, InstallPath: installPath, Skipped: true}}, nil
		}
	}

	release, err := r.fetchReleaseMetadataMatching(ctx, tag, func(payload githubReleasePayload) error {
		_, err := findReleaseAssetSet(payload, looperdBinaryName+"-"+target)
		return err
	})
	if err != nil {
		return preparedDaemonInstall{}, err
	}

	asset, err := findReleaseAssetSet(release, looperdBinaryName+"-"+target)
	if err != nil {
		return preparedDaemonInstall{}, fmt.Errorf("looperd release: %w", err)
	}

	binaryBytes, err := r.fetchAndExtractBinary(ctx, asset, progress)
	if err != nil {
		return preparedDaemonInstall{}, err
	}

	return preparedDaemonInstall{
		result: daemonInstallResult{
			Target:         target,
			InstallPath:    installPath,
			DownloadedFrom: stringPtr(asset.PreferredURL),
			Skipped:        false,
		},
		binaryBytes: binaryBytes,
	}, nil
}

func commitPreparedDaemonInstall(prepared preparedDaemonInstall) error {
	if prepared.result.Skipped {
		return nil
	}
	installDir := filepath.Dir(prepared.result.InstallPath)
	if err := os.MkdirAll(installDir, 0o755); err != nil {
		return fmt.Errorf("create install directory: %w", err)
	}

	tempInstallPath := prepared.result.InstallPath + ".new"
	if err := os.WriteFile(tempInstallPath, prepared.binaryBytes, 0o755); err != nil {
		_ = removeTempInstallFile(tempInstallPath)
		return err
	}
	if err := os.Chmod(tempInstallPath, 0o755); err != nil {
		_ = removeTempInstallFile(tempInstallPath)
		return err
	}
	if err := os.Rename(tempInstallPath, prepared.result.InstallPath); err != nil {
		_ = removeTempInstallFile(tempInstallPath)
		return err
	}
	return nil
}

func (r *commandRuntime) fetchReleaseMetadata(ctx context.Context, tag string) (githubReleasePayload, error) {
	return r.fetchReleaseMetadataMatching(ctx, tag, nil)
}

func (r *commandRuntime) fetchReleaseMetadataMatching(ctx context.Context, tag string, accept func(githubReleasePayload) error) (githubReleasePayload, error) {
	var lastErr error
	for _, releaseURL := range releaseMetadataURLs(tag) {
		if err := ctx.Err(); err != nil {
			return githubReleasePayload{}, err
		}
		payload, err := r.fetchReleaseMetadataFromURL(ctx, releaseURL)
		if err != nil {
			lastErr = err
			continue
		}
		if err := requireMatchingReleaseTag(payload, tag); err != nil {
			lastErr = err
			continue
		}
		if accept != nil {
			if err := accept(payload); err != nil {
				lastErr = err
				continue
			}
		}
		return payload, nil
	}
	if lastErr == nil {
		return githubReleasePayload{}, fmt.Errorf("Failed to fetch GitHub release metadata from %s (status missing)", buildGitHubReleaseAPIURL(defaultReleaseOwner, defaultReleaseRepo, tag))
	}
	return githubReleasePayload{}, lastErr
}

func requireMatchingReleaseTag(payload githubReleasePayload, requested string) error {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return nil
	}
	got := strings.TrimSpace(payload.TagName)
	if got == "" {
		return fmt.Errorf("release metadata is missing tag_name")
	}
	if normalizeVersion(got) != normalizeVersion(requested) {
		return fmt.Errorf("release metadata tag %q does not match requested %q", got, requested)
	}
	return nil
}

func (r *commandRuntime) fetchReleaseMetadataFromURL(ctx context.Context, releaseURL string) (githubReleasePayload, error) {
	// Bound each source independently, including reading its response body,
	// so a stalled CDN leaves the caller's context usable for GitHub.
	ctx, cancel := context.WithTimeout(ctx, releaseMetadataTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, releaseURL, nil)
	if err != nil {
		return githubReleasePayload{}, fmt.Errorf("build release metadata request: %w", err)
	}
	req.Header.Set("User-Agent", looperdUserAgent)
	req.Header.Set("Accept", "application/json, application/vnd.github+json")

	resp, err := r.httpClient().Do(req)
	if err != nil {
		return githubReleasePayload{}, fmt.Errorf("fetch release metadata from %s: %w", releaseURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return githubReleasePayload{}, fmt.Errorf("Failed to fetch GitHub release metadata from %s (status %s)", releaseURL, resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxReleaseMetadataBytes+1))
	if err != nil {
		return githubReleasePayload{}, fmt.Errorf("read release metadata from %s: %w", releaseURL, err)
	}
	if len(body) > maxReleaseMetadataBytes {
		return githubReleasePayload{}, fmt.Errorf("release metadata from %s exceeds %d bytes", releaseURL, maxReleaseMetadataBytes)
	}
	payload, err := decodeReleaseMetadata(body, isCDNReleaseMetadataURL(releaseURL))
	if err != nil {
		return githubReleasePayload{}, fmt.Errorf("decode release metadata from %s: %w", releaseURL, err)
	}
	if payload.Assets == nil {
		return githubReleasePayload{}, fmt.Errorf("GitHub release payload is missing assets array: %s", releaseURL)
	}

	return payload, nil
}

func (r *commandRuntime) downloadChecksum(ctx context.Context, url string) (string, error) {
	data, _, err := r.download(ctx, url, "text/plain", "", nil)
	if err != nil {
		return "", fmt.Errorf("Failed to download looperd checksum from %s (%v)", url, err)
	}
	return string(data), nil
}

func (r *commandRuntime) download(ctx context.Context, url string, accept string, progressName string, progress io.Writer) ([]byte, *http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("User-Agent", looperdUserAgent)
	req.Header.Set("Accept", accept)

	resp, err := r.httpClient().Do(req)
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		resp.Body.Close()
		return nil, resp, fmt.Errorf("status %s", resp.Status)
	}

	reader := io.ReadCloser(resp.Body)
	downloadSucceeded := false
	if progress != nil && strings.TrimSpace(progressName) != "" {
		factory, owned := ensureDownloadProgressFactory(progress)
		if owned {
			defer factory.close()
		}
		tracker := factory.newTracker(progressName, resp.ContentLength)
		defer func() { tracker.finish(downloadSucceeded) }()
		reader = tracker.wrap(resp.Body)
	}
	defer reader.Close()

	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, nil, err
	}
	downloadSucceeded = true
	return data, resp, nil
}

func (r *commandRuntime) downloadBinary(ctx context.Context, url string, name string, progress io.Writer) ([]byte, error) {
	data, _, err := r.download(ctx, url, "application/octet-stream", name, progress)
	if err != nil {
		return nil, fmt.Errorf("Failed to download release binary from %s (%v)", url, err)
	}
	return data, nil
}

func (r *commandRuntime) httpClient() *http.Client {
	if r.app.deps.HTTPClient != nil {
		return r.app.deps.HTTPClient
	}
	return http.DefaultClient
}

func (r *commandRuntime) homeDir() (string, error) {
	if trimmed := strings.TrimSpace(r.app.deps.HomeDir); trimmed != "" {
		return trimmed, nil
	}
	return os.UserHomeDir()
}

func (r *commandRuntime) platform() string {
	if trimmed := strings.TrimSpace(r.app.deps.Platform); trimmed != "" {
		return trimmed
	}
	return goruntime.GOOS
}

func (r *commandRuntime) arch() string {
	if trimmed := strings.TrimSpace(r.app.deps.Arch); trimmed != "" {
		return trimmed
	}
	return goruntime.GOARCH
}

func resolveLooperdTarget(platform string, arch string) (string, error) {
	if platform == "darwin" && arch == "arm64" {
		return "darwin-arm64", nil
	}
	if platform == "linux" && arch == "amd64" {
		return "linux-amd64", nil
	}

	return "", fmt.Errorf("Unsupported platform/arch for looperd install: %s-%s. Supported targets: darwin-arm64, linux-amd64", platform, arch)
}

func releaseMetadataURLs(tag string) []string {
	return []string{
		buildReleaseManifestURL(defaultReleaseManifestBaseURL, tag),
		buildGitHubReleaseAPIURL(defaultReleaseOwner, defaultReleaseRepo, tag),
	}
}

func buildReleaseManifestURL(base, tag string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	tag = strings.TrimSpace(tag)
	if tag == "" {
		return base + "/channels/stable.json"
	}
	return base + "/" + tag + "/manifest.json"
}

func buildGitHubReleaseAPIURL(owner, repo, tag string) string {
	base := fmt.Sprintf("https://api.github.com/repos/%s/%s/releases", owner, repo)
	if strings.TrimSpace(tag) != "" {
		return base + "/tags/" + tag
	}
	return base + "/latest"
}

func decodeReleaseMetadata(body []byte, fromCDN bool) (githubReleasePayload, error) {
	// The endpoint determines the schema. Never interpret CDN content as a
	// GitHub API response, whose download URLs are trusted by callers.
	if fromCDN {
		var manifest release.Manifest
		if err := json.Unmarshal(body, &manifest); err != nil {
			return githubReleasePayload{}, err
		}
		if manifest.ManifestVersion != release.ManifestVersion {
			return githubReleasePayload{}, fmt.Errorf("unsupported release manifest version %d", manifest.ManifestVersion)
		}
		tag := canonicalReleaseTag(manifest.Tag, manifest.Version)
		if err := release.ValidateTag(tag); err != nil {
			return githubReleasePayload{}, err
		}
		if _, err := parseSemver(tag); err != nil {
			return githubReleasePayload{}, err
		}
		payload := githubReleaseFromManifest(manifest)
		if len(payload.Assets) == 0 {
			return githubReleasePayload{}, fmt.Errorf("release manifest is missing usable artifacts")
		}
		return payload, nil
	}
	var payload githubReleasePayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return githubReleasePayload{}, err
	}
	return payload, nil
}

func isCDNReleaseMetadataURL(raw string) bool {
	return strings.HasPrefix(raw, defaultReleaseManifestBaseURL+"/")
}

func githubReleaseFromManifest(manifest release.Manifest) githubReleasePayload {
	tag := canonicalReleaseTag(manifest.Tag, manifest.Version)
	assets := make([]githubReleaseAsset, 0, len(manifest.Artifacts)*2)
	for name := range manifest.Artifacts {
		name = strings.TrimSpace(name)
		if tag == "" || !isGitHubReleaseAssetName(name) {
			continue
		}
		assets = append(assets, githubReleaseAsset{
			Name:               name,
			BrowserDownloadURL: githubReleaseDownloadURL(defaultReleaseOwner, defaultReleaseRepo, tag, name),
		})
		if strings.HasSuffix(name, ".sha256") {
			continue
		}
		assets = append(assets, githubReleaseAsset{
			Name:               name + ".sha256",
			BrowserDownloadURL: githubReleaseDownloadURL(defaultReleaseOwner, defaultReleaseRepo, tag, name+".sha256"),
		})
	}
	return githubReleasePayload{TagName: tag, Assets: assets}
}

func githubReleaseDownloadURL(owner, repo, tag, name string) string {
	return fmt.Sprintf("https://github.com/%s/%s/releases/download/%s/%s", owner, repo, tag, name)
}

func isGitHubReleaseAssetName(name string) bool {
	return name != "" && !strings.ContainsAny(name, "/\\") && !strings.Contains(name, "..")
}

func canonicalReleaseTag(tag, version string) string {
	tag = strings.TrimSpace(tag)
	if tag == "" {
		tag = strings.TrimSpace(version)
	}
	tag = strings.TrimPrefix(tag, "v")
	if tag == "" {
		return ""
	}
	return "v" + tag
}

func parseChecksum(value string) (string, error) {
	hash := strings.ToLower(strings.TrimSpace(strings.Split(strings.TrimSpace(value), " ")[0]))
	if len(hash) != 64 {
		fields := strings.Fields(strings.TrimSpace(value))
		if len(fields) > 0 {
			hash = strings.ToLower(fields[0])
		}
	}
	if len(hash) != 64 {
		return "", fmt.Errorf("Downloaded looperd checksum is invalid")
	}
	for _, char := range hash {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return "", fmt.Errorf("Downloaded looperd checksum is invalid")
		}
	}
	return hash, nil
}

func removeTempInstallFile(path string) error {
	err := os.Remove(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func stringPtr(value string) *string {
	return &value
}
