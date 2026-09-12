package cliapp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/release"
)

// Complete a fixture's published binaries without changing the fields under
// test. Individual tests can then remove an artifact to model partial uploads.
func withPublishedArtifacts(t *testing.T, body string) string {
	t.Helper()
	var manifest release.Manifest
	if err := json.Unmarshal([]byte(body), &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Artifacts == nil {
		manifest.Artifacts = map[string]release.Artifact{}
	}
	for _, name := range []string{"looper-darwin-arm64", "looper-linux-amd64", "looperd-darwin-arm64", "looperd-linux-amd64"} {
		if _, raw := manifest.Artifacts[name]; raw {
			continue
		}
		if _, archive := manifest.Artifacts[name+".tar.gz"]; archive {
			continue
		}
		manifest.Artifacts[name] = release.Artifact{}
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func TestUpgradeCheckRequiresCompletePublishedManifest(t *testing.T) {
	t.Parallel()
	for _, missing := range []string{"none", "unrelated only", "looper-darwin-arm64", "looper-linux-amd64", "looperd-darwin-arm64", "looperd-linux-amd64"} {
		t.Run(missing, func(t *testing.T) {
			t.Parallel()
			body := withPublishedArtifacts(t, `{"manifestVersion":1,"channel":"stable","tag":"v1.2.3"}`)
			var manifest release.Manifest
			if err := json.Unmarshal([]byte(body), &manifest); err != nil {
				t.Fatal(err)
			}
			if missing == "unrelated only" {
				manifest.Artifacts = map[string]release.Artifact{"README.md": {}}
			} else {
				delete(manifest.Artifacts, missing)
			}
			encoded, err := json.Marshal(manifest)
			if err != nil {
				t.Fatal(err)
			}
			stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
			githubCalls := 0
			app := New(Deps{HomeDir: t.TempDir(), Platform: "darwin", Arch: "arm64", Stdout: stdout, Stderr: stderr, RunCommand: missingDaemonVersionCommand,
				HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					switch {
					case req.URL.Path == "/api/v1/status":
						return nil, fmt.Errorf("daemon offline")
					case isCDNReleaseMetadataURL(req.URL.String()):
						return jsonResponse(t, 200, string(encoded)), nil
					case req.URL.String() == buildGitHubReleaseAPIURL(defaultReleaseOwner, defaultReleaseRepo, ""):
						githubCalls++
						return jsonResponse(t, 200, `{"tag_name":"v1.2.4","assets":[]}`), nil
					default:
						t.Fatalf("check must not download assets: %s", req.URL)
						return nil, nil
					}
				})},
			})
			configPath := writeCLIConfig(t, "http://127.0.0.1:4321", "")
			if code := app.Run(context.Background(), []string{"upgrade", "--check", "--json", "--config", configPath}); code != 0 {
				t.Fatalf("exit=%d stderr=%s", code, stderr)
			}
			var result upgradeCheckSummary
			if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			want := "1.2.4"
			wantCalls := 2
			if missing == "none" {
				want = "1.2.3"
				wantCalls = 0
			}
			if result.CLI.LatestVersion != want || result.Daemon.LatestVersion != want || githubCalls != wantCalls {
				t.Fatalf("summary=%+v GitHub calls=%d; want both versions=%s calls=%d", result, githubCalls, want, wantCalls)
			}
		})
	}
}

func TestCDNPublishedContractAcceptsRawAndArchivedBinaries(t *testing.T) {
	t.Parallel()
	for _, suffix := range []string{"", ".tar.gz"} {
		t.Run("suffix="+suffix, func(t *testing.T) {
			t.Parallel()
			body := withPublishedArtifacts(t, `{"manifestVersion":1,"channel":"stable","tag":"v1.2.3"}`)
			if suffix != "" {
				body = strings.ReplaceAll(body, `-arm64"`, `-arm64.tar.gz"`)
				body = strings.ReplaceAll(body, `-amd64"`, `-amd64.tar.gz"`)
			}
			_, err := decodeReleaseMetadata([]byte(body), buildReleaseManifestURL(defaultReleaseManifestBaseURL, ""))
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}
