package cliapp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/version"
)

func TestStableMetadataRejectsOtherChannels(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ channel, tag string }{
		{"beta", "v1.2.3-beta.1"}, {"stable", "v1.2.3-beta.1"}, {"beta", "v1.2.3"}, {"", "v1.2.3"},
	} {
		t.Run(tc.channel+tc.tag, func(t *testing.T) {
			t.Parallel()
			var seen []string
			app := New(Deps{HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				seen = append(seen, req.URL.String())
				if isCDNReleaseMetadataURL(req.URL.String()) {
					return jsonResponse(t, 200, fmt.Sprintf(`{"manifestVersion":1,"channel":%q,"tag":%q,"artifacts":{"looper-darwin-arm64":{}}}`, tc.channel, tc.tag)), nil
				}
				return jsonResponse(t, 200, `{"tag_name":"v1.2.2","assets":[]}`), nil
			})}})
			runtime := newCommandRuntime(app, nil)
			got, err := runtime.fetchLatestCLIVersion(context.Background())
			if err != nil || got != "1.2.2" || len(seen) != 2 {
				t.Fatalf("version=%q err=%v requests=%v", got, err, seen)
			}
		})
	}
}

func TestVersionedMetadataStillAcceptsBeta(t *testing.T) {
	t.Parallel()
	app := New(Deps{HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != defaultReleaseManifestBaseURL+"/v1.2.3-beta.1/manifest.json" {
			t.Fatalf("unexpected URL %s", req.URL)
		}
		return jsonResponse(t, 200, `{"manifestVersion":1,"channel":"beta","tag":"v1.2.3-beta.1","artifacts":{"looperd-darwin-arm64":{}}}`), nil
	})}})
	got, err := newCommandRuntime(app, nil).fetchReleaseMetadata(context.Background(), "v1.2.3-beta.1")
	if err != nil || got.TagName != "v1.2.3-beta.1" {
		t.Fatalf("metadata=%v err=%v", got, err)
	}
}

func TestUpgradeFallsBackAfterCDNSelectedDownloadFails(t *testing.T) {
	t.Parallel()
	for _, binary := range []string{"looper", "looperd"} {
		for _, failure := range []string{"binary missing", "checksum missing", "checksum mismatch", "bad archive", "already current", "both fail"} {
			t.Run(binary+"/"+failure, func(t *testing.T) {
				t.Parallel()
				home := t.TempDir()
				installPath := filepath.Join(home, ".looper", "bin", binary)
				if err := os.MkdirAll(filepath.Dir(installPath), 0755); err != nil {
					t.Fatal(err)
				}
				old := []byte("original binary")
				if err := os.WriteFile(installPath, old, 0755); err != nil {
					t.Fatal(err)
				}
				next := []byte("verified fallback binary")
				checksum := fmt.Sprintf("%x", sha256.Sum256(next))
				fallbackTag := "v998.0.0"
				if failure == "already current" {
					fallbackTag = "v" + version.Current().Version
				}
				name := binary + "-darwin-arm64"
				cdnName := name
				if failure == "bad archive" {
					cdnName += ".tar.gz"
				}
				var seen []string
				stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
				app := New(Deps{HomeDir: home, Platform: "darwin", Arch: "arm64", CLIChannel: cliInstallChannelStable, ExecutablePath: filepath.Join(home, ".looper", "bin", "looper"), Stdout: stdout, Stderr: stderr,
					RunCommand: func(ctx context.Context, command string, args []string, timeout time.Duration) (commandExecutionResult, error) {
						if command == installPath && binary == "looperd" {
							return commandExecutionResult{Stdout: version.Current().Version + "\n", ExitCode: 0}, nil
						}
						return commandExecutionResult{ExitCode: 1}, nil
					},
					HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
						u := req.URL.String()
						seen = append(seen, u)
						if req.URL.Path == "/api/v1/status" {
							return nil, fmt.Errorf("daemon offline")
						}
						if isCDNReleaseMetadataURL(u) {
							return jsonResponse(t, 200, fmt.Sprintf(`{"manifestVersion":1,"channel":"stable","tag":"v999.0.0","artifacts":{%q:{}}}`, cdnName)), nil
						}
						if strings.HasPrefix(u, "https://api.github.com/") {
							if u != buildGitHubReleaseAPIURL(defaultReleaseOwner, defaultReleaseRepo, "") {
								return jsonResponse(t, 404, `{}`), nil
							}
							return jsonResponse(t, 200, fmt.Sprintf(`{"tag_name":%q,"assets":[{"name":%q,"browser_download_url":"https://example.invalid/binary"},{"name":%q,"browser_download_url":"https://example.invalid/checksum"}]}`, fallbackTag, name, name+".sha256")), nil
						}
						if strings.HasPrefix(u, "https://github.com/") {
							if strings.HasSuffix(u, ".sha256") {
								if failure == "bad archive" {
									return textResponse(t, 200, checksum), nil
								}
								if failure == "checksum missing" {
									return textResponse(t, 404, "missing"), nil
								}
								return textResponse(t, 200, strings.Repeat("0", 64)), nil
							}
							if failure == "checksum missing" || failure == "checksum mismatch" || failure == "bad archive" {
								return binaryResponse(t, 200, next), nil
							}
							return textResponse(t, 404, "missing"), nil
						}
						if failure == "both fail" {
							return textResponse(t, 404, "missing"), nil
						}
						if u == "https://example.invalid/binary" {
							return binaryResponse(t, 200, next), nil
						}
						if u == "https://example.invalid/checksum" {
							return textResponse(t, 200, checksum), nil
						}
						t.Fatalf("unexpected request %s", u)
						return nil, nil
					})},
				})
				flag := "--cli"
				if binary == "looperd" {
					flag = "--daemon"
				}
				configPath := writeCLIConfig(t, "http://127.0.0.1:4321", "")
				exit := app.Run(context.Background(), []string{"upgrade", flag, "--json", "--config", configPath})
				if (exit != 0) != (failure == "both fail") {
					t.Fatalf("exit=%d stderr=%s requests=%v", exit, stderr, seen)
				}
				got, err := os.ReadFile(installPath)
				if err != nil {
					t.Fatal(err)
				}
				want := next
				if failure == "both fail" || failure == "already current" {
					want = old
				}
				if !bytes.Equal(got, want) {
					t.Fatalf("installed=%q want=%q", got, want)
				}
				if !strings.Contains(strings.Join(seen, "\n"), buildGitHubReleaseAPIURL(defaultReleaseOwner, defaultReleaseRepo, "")) {
					t.Fatalf("no GitHub latest fallback: %v", seen)
				}
				if failure != "both fail" {
					var result struct {
						LatestVersion string `json:"latestVersion"`
						Changed       bool   `json:"changed"`
					}
					if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
						t.Fatalf("output=%s error=%v", stdout, err)
					}
					if result.LatestVersion != normalizeVersion(fallbackTag) || result.Changed != (failure != "already current") {
						t.Fatalf("result=%+v", result)
					}
				}
			})
		}
	}
}

// Bootstrap installs an explicit tag; fallback must keep that tag rather than
// switch to GitHub latest, even though unpinned upgrade uses latest.
func TestVersionedDaemonInstallDownloadFallbackKeepsRequestedTag(t *testing.T) {
	t.Parallel()
	const tag = "v1.2.3-beta.1"
	const name = "looperd-darwin-arm64"
	next := []byte("pinned daemon")
	checksum := fmt.Sprintf("%x", sha256.Sum256(next))
	home := t.TempDir()
	installPath := filepath.Join(home, ".looper", "bin", "looperd")
	var seen []string
	app := New(Deps{HomeDir: home, Platform: "darwin", Arch: "arm64", HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		u := req.URL.String()
		seen = append(seen, u)
		if _, err := os.Stat(installPath); !os.IsNotExist(err) {
			t.Fatalf("installation happened before source completed: %v", err)
		}
		switch u {
		case buildReleaseManifestURL(defaultReleaseManifestBaseURL, tag):
			return jsonResponse(t, 200, fmt.Sprintf(`{"manifestVersion":1,"channel":"beta","tag":%q,"artifacts":{%q:{}}}`, tag, name)), nil
		case githubReleaseDownloadURL(defaultReleaseOwner, defaultReleaseRepo, tag, name):
			return textResponse(t, 404, "missing"), nil
		case buildGitHubReleaseAPIURL(defaultReleaseOwner, defaultReleaseRepo, tag):
			return jsonResponse(t, 200, fmt.Sprintf(`{"tag_name":%q,"assets":[{"name":%q,"browser_download_url":"https://example.invalid/binary"},{"name":%q,"browser_download_url":"https://example.invalid/checksum"}]}`, tag, name, name+".sha256")), nil
		case "https://example.invalid/binary":
			return binaryResponse(t, 200, next), nil
		case "https://example.invalid/checksum":
			return textResponse(t, 200, checksum), nil
		default:
			t.Fatalf("unexpected request %s", u)
			return nil, nil
		}
	})}})
	result, err := newCommandRuntime(app, nil).installManagedDaemon(context.Background(), true, tag, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.DownloadedFrom == nil || *result.DownloadedFrom != "https://example.invalid/binary" || len(seen) != 5 {
		t.Fatalf("result=%+v requests=%v", result, seen)
	}
	got, err := os.ReadFile(installPath)
	if err != nil || !bytes.Equal(got, next) {
		t.Fatalf("installed=%q err=%v", got, err)
	}
}
