package cliapp

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func buildTarGzArchive(t *testing.T, entryName string, contents []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	header := &tar.Header{
		Name:     entryName,
		Mode:     0o755,
		Size:     int64(len(contents)),
		Typeflag: tar.TypeReg,
	}
	if err := tw.WriteHeader(header); err != nil {
		t.Fatalf("tar write header: %v", err)
	}
	if _, err := tw.Write(contents); err != nil {
		t.Fatalf("tar write contents: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

func buildTarGzArchiveWithHeaderOnly(t *testing.T, entryName string, size int64) []byte {
	t.Helper()
	var raw bytes.Buffer
	header := make([]byte, 512)
	copy(header[0:100], []byte(entryName))
	copy(header[100:108], []byte(fmt.Sprintf("%07o\x00", 0o755)))
	copy(header[108:116], []byte("0000000\x00"))
	copy(header[116:124], []byte("0000000\x00"))
	copy(header[124:136], []byte(fmt.Sprintf("%011o\x00", size)))
	copy(header[136:148], []byte("00000000000\x00"))
	for i := 148; i < 156; i++ {
		header[i] = ' '
	}
	header[156] = tar.TypeReg
	copy(header[257:263], []byte("ustar\x00"))
	copy(header[263:265], []byte("00"))
	checksum := 0
	for _, b := range header {
		checksum += int(b)
	}
	copy(header[148:156], []byte(fmt.Sprintf("%06o\x00 ", checksum)))
	if _, err := raw.Write(header); err != nil {
		t.Fatalf("write tar header: %v", err)
	}
	if _, err := raw.Write(make([]byte, 1024)); err != nil {
		t.Fatalf("write tar trailer: %v", err)
	}

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(raw.Bytes()); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

func TestExtractBinaryFromTarGzReturnsBinaryBytes(t *testing.T) {
	t.Parallel()

	want := []byte("real-binary-bytes")
	archive := buildTarGzArchive(t, "looperd-darwin-arm64", want)

	got, err := extractBinaryFromTarGz(archive, "looperd-darwin-arm64")
	if err != nil {
		t.Fatalf("extractBinaryFromTarGz() error = %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("extractBinaryFromTarGz() bytes = %q, want %q", got, want)
	}
}

func TestExtractBinaryFromTarGzRejectsUnsafePaths(t *testing.T) {
	t.Parallel()

	archive := buildTarGzArchive(t, "../escape-binary", []byte("payload"))
	_, err := extractBinaryFromTarGz(archive, "escape-binary")
	if err == nil {
		t.Fatal("extractBinaryFromTarGz() error = nil, want unsafe-path error")
	}
	if !strings.Contains(err.Error(), "unsafe path") {
		t.Fatalf("error = %q, want unsafe-path message", err.Error())
	}
}

func TestExtractBinaryFromTarGzReportsMissingEntry(t *testing.T) {
	t.Parallel()

	archive := buildTarGzArchive(t, "looperd-darwin-arm64", []byte("ok"))
	_, err := extractBinaryFromTarGz(archive, "different-name")
	if err == nil {
		t.Fatal("extractBinaryFromTarGz() error = nil, want missing-entry error")
	}
	if !strings.Contains(err.Error(), "does not contain entry") {
		t.Fatalf("error = %q, want missing-entry message", err.Error())
	}
}

func TestExtractBinaryFromTarGzRejectsOversizedEntry(t *testing.T) {
	t.Parallel()

	archive := buildTarGzArchiveWithHeaderOnly(t, "looperd-darwin-arm64", maxArchiveBinaryBytes+1)
	_, err := extractBinaryFromTarGz(archive, "looperd-darwin-arm64")
	if err == nil {
		t.Fatal("extractBinaryFromTarGz() error = nil, want oversized-entry error")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("error = %q, want oversized-entry message", err.Error())
	}
}

func TestFindReleaseAssetSetPrefersArchive(t *testing.T) {
	t.Parallel()

	release := githubReleasePayload{
		Assets: []githubReleaseAsset{
			{Name: "looperd-darwin-arm64", BrowserDownloadURL: "https://example.invalid/raw"},
			{Name: "looperd-darwin-arm64.sha256", BrowserDownloadURL: "https://example.invalid/raw.sha256"},
			{Name: "looperd-darwin-arm64.tar.gz", BrowserDownloadURL: "https://example.invalid/archive.tar.gz"},
			{Name: "looperd-darwin-arm64.tar.gz.sha256", BrowserDownloadURL: "https://example.invalid/archive.tar.gz.sha256"},
		},
	}

	asset, err := findReleaseAssetSet(release, "looperd-darwin-arm64")
	if err != nil {
		t.Fatalf("findReleaseAssetSet() error = %v", err)
	}
	if !asset.IsArchive {
		t.Fatal("asset.IsArchive = false, want archive preferred")
	}
	if asset.PreferredURL != "https://example.invalid/archive.tar.gz" {
		t.Fatalf("asset.PreferredURL = %q, want archive URL", asset.PreferredURL)
	}
	if asset.PreferredName != "looperd-darwin-arm64.tar.gz" {
		t.Fatalf("asset.PreferredName = %q", asset.PreferredName)
	}
	if asset.BinaryName != "looperd-darwin-arm64" {
		t.Fatalf("asset.BinaryName = %q", asset.BinaryName)
	}
}

func TestFindReleaseAssetSetFallsBackToRawBinary(t *testing.T) {
	t.Parallel()

	release := githubReleasePayload{
		Assets: []githubReleaseAsset{
			{Name: "looperd-darwin-arm64", BrowserDownloadURL: "https://example.invalid/raw"},
			{Name: "looperd-darwin-arm64.sha256", BrowserDownloadURL: "https://example.invalid/raw.sha256"},
		},
	}

	asset, err := findReleaseAssetSet(release, "looperd-darwin-arm64")
	if err != nil {
		t.Fatalf("findReleaseAssetSet() error = %v", err)
	}
	if asset.IsArchive {
		t.Fatal("asset.IsArchive = true, want raw binary fallback")
	}
	if asset.PreferredURL != "https://example.invalid/raw" {
		t.Fatalf("asset.PreferredURL = %q", asset.PreferredURL)
	}
}

func TestFindReleaseAssetSetReportsMissingAssets(t *testing.T) {
	t.Parallel()

	release := githubReleasePayload{Assets: []githubReleaseAsset{}}
	_, err := findReleaseAssetSet(release, "looperd-darwin-arm64")
	if err == nil {
		t.Fatal("findReleaseAssetSet() error = nil, want missing-asset error")
	}
	if !strings.Contains(err.Error(), "looperd-darwin-arm64") {
		t.Fatalf("error = %q, want to mention missing asset", err.Error())
	}
}

func TestInstallManagedDaemonExtractsArchiveWhenAvailable(t *testing.T) {
	t.Parallel()

	homeDir := t.TempDir()
	binary := []byte("daemon-binary-payload")
	archive := buildTarGzArchive(t, "looperd-darwin-arm64", binary)
	archiveChecksum := sha256.Sum256(archive)
	checksumText := hex.EncodeToString(archiveChecksum[:]) + "  looperd-darwin-arm64.tar.gz\n"

	app := New(Deps{
		HTTPClient: newTestHTTPClient(func(req *http.Request) (*http.Response, error) {
			switch req.URL.String() {
			case "https://api.github.com/repos/nexu-io/looper/releases/latest":
				return jsonResponse(t, http.StatusOK, `{"assets":[
					{"name":"looperd-darwin-arm64","browser_download_url":"https://example.invalid/looperd-darwin-arm64"},
					{"name":"looperd-darwin-arm64.sha256","browser_download_url":"https://example.invalid/looperd-darwin-arm64.sha256"},
					{"name":"looperd-darwin-arm64.tar.gz","browser_download_url":"https://example.invalid/looperd-darwin-arm64.tar.gz"},
					{"name":"looperd-darwin-arm64.tar.gz.sha256","browser_download_url":"https://example.invalid/looperd-darwin-arm64.tar.gz.sha256"}
				]}`), nil
			case "https://example.invalid/looperd-darwin-arm64.tar.gz":
				return binaryResponse(t, http.StatusOK, archive), nil
			case "https://example.invalid/looperd-darwin-arm64.tar.gz.sha256":
				return textResponse(t, http.StatusOK, checksumText), nil
			case "https://example.invalid/looperd-darwin-arm64", "https://example.invalid/looperd-darwin-arm64.sha256":
				t.Fatalf("raw binary URL hit while archive is available: %q", req.URL.String())
				return nil, nil
			default:
				t.Fatalf("unexpected request URL %q", req.URL.String())
				return nil, nil
			}
		}),
		HomeDir:  homeDir,
		Platform: "darwin",
		Arch:     "arm64",
	})

	runtime := newCommandRuntime(app, nil)
	result, err := runtime.installManagedDaemon(context.Background(), false, "", nil)
	if err != nil {
		t.Fatalf("installManagedDaemon() error = %v", err)
	}

	installPath := filepath.Join(homeDir, ".looper", "bin", "looperd")
	if result.InstallPath != installPath {
		t.Fatalf("result.InstallPath = %q, want %q", result.InstallPath, installPath)
	}
	if result.DownloadedFrom == nil || *result.DownloadedFrom != "https://example.invalid/looperd-darwin-arm64.tar.gz" {
		t.Fatalf("result.DownloadedFrom = %#v, want archive URL", result.DownloadedFrom)
	}

	installedBytes, err := os.ReadFile(installPath)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", installPath, err)
	}
	if !bytes.Equal(installedBytes, binary) {
		t.Fatalf("installed bytes = %q, want %q", installedBytes, binary)
	}
}

func TestUpgradeUnifiedDownloadsCLIAndDaemonConcurrently(t *testing.T) {
	homeDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(homeDir, ".looper", "worktrees"), 0o755); err != nil {
		t.Fatalf("create test worktree root: %v", err)
	}
	t.Setenv("HOME", homeDir)

	execPath := filepath.Join(homeDir, ".looper", "bin", "looper")
	if err := os.MkdirAll(filepath.Dir(execPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(exec dir): %v", err)
	}
	if err := os.WriteFile(execPath, []byte("old-cli"), 0o755); err != nil {
		t.Fatalf("WriteFile(execPath): %v", err)
	}

	cliBinary := []byte("new-cli-binary")
	daemonBinary := []byte("new-daemon-binary")
	cliChecksum := sha256.Sum256(cliBinary)
	daemonChecksum := sha256.Sum256(daemonBinary)

	configPath := writeCLIConfig(t, "http://daemon.test", "")

	var inflight atomic.Int64
	var maxInflight atomic.Int64
	releaseGate := make(chan struct{})

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}

	app := New(Deps{
		Stdout:         stdout,
		Stderr:         stderr,
		HomeDir:        homeDir,
		Platform:       "darwin",
		Arch:           "arm64",
		ExecutablePath: execPath,
		CLIChannel:     cliInstallChannelStable,
		HTTPClient: newTestHTTPClient(func(req *http.Request) (*http.Response, error) {
			switch req.URL.String() {
			case "http://daemon.test/api/v1/status":
				return nil, errDaemonOffline
			case "https://api.github.com/repos/nexu-io/looper/releases/latest",
				"https://api.github.com/repos/nexu-io/looper/releases/tags/v9.9.9":
				return jsonResponse(t, http.StatusOK, `{"tag_name":"v9.9.9","assets":[
					{"name":"looper-darwin-arm64","browser_download_url":"https://example.invalid/looper-darwin-arm64"},
					{"name":"looper-darwin-arm64.sha256","browser_download_url":"https://example.invalid/looper-darwin-arm64.sha256"},
					{"name":"looperd-darwin-arm64","browser_download_url":"https://example.invalid/looperd-darwin-arm64"},
					{"name":"looperd-darwin-arm64.sha256","browser_download_url":"https://example.invalid/looperd-darwin-arm64.sha256"}
				]}`), nil
			case "https://example.invalid/looper-darwin-arm64",
				"https://example.invalid/looperd-darwin-arm64":
				current := inflight.Add(1)
				for {
					prev := maxInflight.Load()
					if current <= prev || maxInflight.CompareAndSwap(prev, current) {
						break
					}
				}
				// Once both downloads have arrived, release them together so
				// we can verify they were genuinely in flight at the same
				// time. If only one arrives, the test fails by timing out
				// rather than incorrectly passing on serial execution.
				if current >= 2 {
					select {
					case <-releaseGate:
					default:
						close(releaseGate)
					}
				}
				<-releaseGate
				inflight.Add(-1)
				if strings.HasSuffix(req.URL.String(), "looper-darwin-arm64") {
					return binaryResponse(t, http.StatusOK, cliBinary), nil
				}
				return binaryResponse(t, http.StatusOK, daemonBinary), nil
			case "https://example.invalid/looper-darwin-arm64.sha256":
				return textResponse(t, http.StatusOK, hex.EncodeToString(cliChecksum[:])+"  looper-darwin-arm64\n"), nil
			case "https://example.invalid/looperd-darwin-arm64.sha256":
				return textResponse(t, http.StatusOK, hex.EncodeToString(daemonChecksum[:])+"  looperd-darwin-arm64\n"), nil
			default:
				t.Fatalf("unexpected request URL %q", req.URL.String())
				return nil, nil
			}
		}),
	})

	exitCode := app.Run(context.Background(), []string{"upgrade", "--config", configPath})
	if exitCode != 0 {
		t.Fatalf("Run([upgrade]) exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if got := maxInflight.Load(); got < 2 {
		t.Fatalf("max in-flight downloads = %d, want >=2 (concurrent)", got)
	}
	if !strings.Contains(stdout.String(), "Upgraded looper") {
		t.Fatalf("stdout = %q, want CLI upgrade confirmation", stdout.String())
	}
	if !strings.Contains(stdout.String(), "Installed looperd 9.9.9") &&
		!strings.Contains(stdout.String(), "looperd 9.9.9") {
		t.Fatalf("stdout = %q, want daemon install confirmation", stdout.String())
	}
}

// errDaemonOffline mimics the real "daemon offline" error returned by the
// transport when the daemon is not reachable, which is what the upgrade
// status preflight expects to see.
var errDaemonOffline = &daemonOfflineError{}

type daemonOfflineError struct{}

func (e *daemonOfflineError) Error() string { return "looperd is not reachable" }
