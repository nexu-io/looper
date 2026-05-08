package cliapp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

const (
	defaultReleaseOwner = "nexu-io"
	defaultReleaseRepo  = "looper"
	looperdBinaryName   = "looperd"
	looperdUserAgent    = "looper-cli"
)

type daemonInstallResult struct {
	Target         string  `json:"target"`
	InstallPath    string  `json:"installPath"`
	DownloadedFrom *string `json:"downloadedFrom"`
	Skipped        bool    `json:"skipped"`
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
	homeDir, err := r.homeDir()
	if err != nil {
		return daemonInstallResult{}, err
	}

	target, err := resolveLooperdTarget(r.platform(), r.arch())
	if err != nil {
		return daemonInstallResult{}, err
	}

	installDir := filepath.Join(homeDir, ".looper", "bin")
	installPath := filepath.Join(installDir, looperdBinaryName)
	if !force {
		state, err := r.checkManagedDaemonBinary(ctx)
		if err != nil {
			return daemonInstallResult{}, err
		}
		if state.Exists {
			return daemonInstallResult{Target: target, InstallPath: installPath, Skipped: true}, nil
		}
	}

	release, err := r.fetchReleaseMetadata(ctx, tag)
	if err != nil {
		return daemonInstallResult{}, err
	}

	binaryAsset, checksumAsset, err := findLooperdReleaseAssets(release, target)
	if err != nil {
		return daemonInstallResult{}, err
	}

	binaryBytes, err := r.downloadBinary(ctx, binaryAsset.BrowserDownloadURL, binaryAsset.Name, progress)
	if err != nil {
		return daemonInstallResult{}, err
	}

	checksumText, err := r.downloadChecksum(ctx, checksumAsset.BrowserDownloadURL)
	if err != nil {
		return daemonInstallResult{}, err
	}

	expectedChecksum, err := parseChecksum(checksumText)
	if err != nil {
		return daemonInstallResult{}, err
	}
	actualChecksum := sha256.Sum256(binaryBytes)
	if hex.EncodeToString(actualChecksum[:]) != expectedChecksum {
		return daemonInstallResult{}, fmt.Errorf("Downloaded looperd checksum mismatch: expected %s, received %s", expectedChecksum, hex.EncodeToString(actualChecksum[:]))
	}

	if err := os.MkdirAll(installDir, 0o755); err != nil {
		return daemonInstallResult{}, fmt.Errorf("create install directory: %w", err)
	}

	tempInstallPath := installPath + ".new"
	if err := os.WriteFile(tempInstallPath, binaryBytes, 0o755); err != nil {
		_ = removeTempInstallFile(tempInstallPath)
		return daemonInstallResult{}, err
	}
	if err := os.Chmod(tempInstallPath, 0o755); err != nil {
		_ = removeTempInstallFile(tempInstallPath)
		return daemonInstallResult{}, err
	}
	if err := os.Rename(tempInstallPath, installPath); err != nil {
		_ = removeTempInstallFile(tempInstallPath)
		return daemonInstallResult{}, err
	}

	return daemonInstallResult{
		Target:         target,
		InstallPath:    installPath,
		DownloadedFrom: stringPtr(binaryAsset.BrowserDownloadURL),
		Skipped:        false,
	}, nil
}

func (r *commandRuntime) fetchReleaseMetadata(ctx context.Context, tag string) (githubReleasePayload, error) {
	releaseURL := buildGitHubReleaseAPIURL(defaultReleaseOwner, defaultReleaseRepo, tag)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, releaseURL, nil)
	if err != nil {
		return githubReleasePayload{}, fmt.Errorf("build release metadata request: %w", err)
	}
	req.Header.Set("User-Agent", looperdUserAgent)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := r.httpClient().Do(req)
	if err != nil {
		return githubReleasePayload{}, fmt.Errorf("fetch GitHub release metadata: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return githubReleasePayload{}, fmt.Errorf("Failed to fetch GitHub release metadata from %s (status %s)", releaseURL, resp.Status)
	}

	var payload githubReleasePayload
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return githubReleasePayload{}, fmt.Errorf("decode GitHub release payload: %w", err)
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
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, resp, fmt.Errorf("status %s", resp.Status)
	}

	reader := resp.Body
	if progress != nil && strings.TrimSpace(progressName) != "" {
		tracker := newDownloadProgress(progress, progressName, resp.ContentLength)
		defer tracker.finish()
		reader = struct {
			io.Reader
			io.Closer
		}{Reader: io.TeeReader(resp.Body, tracker), Closer: resp.Body}
	}

	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, nil, err
	}
	return data, resp, nil
}

func (r *commandRuntime) downloadBinary(ctx context.Context, url string, name string, progress io.Writer) ([]byte, error) {
	data, _, err := r.download(ctx, url, "application/octet-stream", name, progress)
	if err != nil {
		return nil, fmt.Errorf("Failed to download release binary from %s (%v)", url, err)
	}
	return data, nil
}

type downloadProgress struct {
	w        io.Writer
	name     string
	total    int64
	read     int64
	last     time.Time
	finished bool
}

func newDownloadProgress(w io.Writer, name string, total int64) *downloadProgress {
	p := &downloadProgress{w: w, name: name, total: total}
	p.print(true)
	return p
}

func (p *downloadProgress) Write(data []byte) (int, error) {
	p.read += int64(len(data))
	if time.Since(p.last) >= 200*time.Millisecond {
		p.print(false)
	}
	return len(data), nil
}

func (p *downloadProgress) finish() {
	if p.finished {
		return
	}
	p.finished = true
	p.print(false)
	_, _ = fmt.Fprintln(p.w)
}

func (p *downloadProgress) print(initial bool) {
	p.last = time.Now()
	var line string
	if p.total > 0 {
		percent := 0
		if p.total > 0 {
			percent = int((p.read * 100) / p.total)
		}
		line = fmt.Sprintf("Downloading %s: %s / %s (%d%%)", p.name, formatDownloadBytes(p.read), formatDownloadBytes(p.total), percent)
	} else if p.read > 0 {
		line = fmt.Sprintf("Downloading %s: %s downloaded", p.name, formatDownloadBytes(p.read))
	} else {
		line = fmt.Sprintf("Downloading %s...", p.name)
	}
	if initial {
		_, _ = fmt.Fprint(p.w, line)
		return
	}
	_, _ = fmt.Fprintf(p.w, "\r%s", line)
}

func formatDownloadBytes(value int64) string {
	const unit = 1024
	if value < unit {
		return fmt.Sprintf("%d B", value)
	}
	divisor := int64(unit)
	unitIndex := 0
	for n := value / unit; n >= unit && unitIndex < 3; n /= unit {
		divisor *= unit
		unitIndex++
	}
	number := strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.1f", float64(value)/float64(divisor)), "0"), ".")
	return fmt.Sprintf("%s %ciB", number, "KMGT"[unitIndex])
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

	return "", fmt.Errorf("Unsupported platform/arch for looperd install: %s-%s. Supported targets: darwin-arm64", platform, arch)
}

func buildGitHubReleaseAPIURL(owner, repo, tag string) string {
	base := fmt.Sprintf("https://api.github.com/repos/%s/%s/releases", owner, repo)
	if strings.TrimSpace(tag) != "" {
		return base + "/tags/" + tag
	}
	return base + "/latest"
}

func findLooperdReleaseAssets(release githubReleasePayload, target string) (githubReleaseAsset, githubReleaseAsset, error) {
	binaryName := looperdBinaryName + "-" + target
	checksumName := binaryName + ".sha256"

	var binary githubReleaseAsset
	var checksum githubReleaseAsset
	for _, asset := range release.Assets {
		switch asset.Name {
		case binaryName:
			binary = asset
		case checksumName:
			checksum = asset
		}
	}

	missing := make([]string, 0, 2)
	if strings.TrimSpace(binary.Name) == "" {
		missing = append(missing, binaryName)
	}
	if strings.TrimSpace(checksum.Name) == "" {
		missing = append(missing, checksumName)
	}
	if len(missing) > 0 {
		return githubReleaseAsset{}, githubReleaseAsset{}, fmt.Errorf("GitHub release is missing required looperd asset(s): %s", strings.Join(missing, ", "))
	}

	return binary, checksum, nil
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
