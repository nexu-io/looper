package release

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBuildManifestCollectsArtifactsAndDerivesDefaults(t *testing.T) {
	assetsDir := t.TempDir()
	writeFile(t, filepath.Join(assetsDir, "looper-darwin-arm64"), []byte("looper"))
	writeFile(t, filepath.Join(assetsDir, "looper-darwin-arm64.sha256"), []byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa  looper-darwin-arm64\n"))
	writeFile(t, filepath.Join(assetsDir, "looperd-darwin-arm64"), []byte("looperd"))
	writeFile(t, filepath.Join(assetsDir, "looperd-darwin-arm64.sha256"), []byte("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb  looperd-darwin-arm64\n"))

	manifest, err := BuildManifest(BuildManifestInput{
		Tag:               "v1.2.3-rc.1",
		Released:          time.Date(2026, 4, 22, 12, 0, 0, 0, time.UTC),
		APIVersion:        "v1",
		SchemaVersion:     "12",
		MinCliForDaemon:   "0.2.0",
		MinDaemonForCli:   "0.2.0",
		Repo:              "powerformer/looper",
		AssetsDir:         assetsDir,
		RequiredArtifacts: []string{"looper-darwin-arm64", "looperd-darwin-arm64"},
	})
	if err != nil {
		t.Fatalf("BuildManifest(...) error = %v", err)
	}

	if manifest.Version != "1.2.3-rc.1" {
		t.Fatalf("manifest.Version = %q, want %q", manifest.Version, "1.2.3-rc.1")
	}
	if manifest.Channel != "beta" {
		t.Fatalf("manifest.Channel = %q, want %q", manifest.Channel, "beta")
	}
	if manifest.Artifacts["looper-darwin-arm64"].SHA256 != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("looper sha = %q, want %q", manifest.Artifacts["looper-darwin-arm64"].SHA256, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	}
	if manifest.Artifacts["looperd-darwin-arm64"].URL != "https://github.com/powerformer/looper/releases/download/v1.2.3-rc.1/looperd-darwin-arm64" {
		t.Fatalf("looperd URL = %q", manifest.Artifacts["looperd-darwin-arm64"].URL)
	}
}

func TestBuildManifestFailsWhenRequiredArtifactMissing(t *testing.T) {
	assetsDir := t.TempDir()
	writeFile(t, filepath.Join(assetsDir, "looper-darwin-arm64"), []byte("looper"))
	writeFile(t, filepath.Join(assetsDir, "looper-darwin-arm64.sha256"), []byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa  looper-darwin-arm64\n"))

	_, err := BuildManifest(BuildManifestInput{
		Tag:               "v1.2.3",
		Released:          time.Date(2026, 4, 22, 12, 0, 0, 0, time.UTC),
		APIVersion:        "v1",
		SchemaVersion:     "12",
		Repo:              "powerformer/looper",
		AssetsDir:         assetsDir,
		RequiredArtifacts: []string{"looper-darwin-arm64", "looperd-darwin-arm64"},
	})
	if err == nil {
		t.Fatal("BuildManifest(...) error = nil, want missing required artifact")
	}
}

func TestBuildManifestRejectsInvalidTag(t *testing.T) {
	assetsDir := t.TempDir()
	writeFile(t, filepath.Join(assetsDir, "looper-darwin-arm64"), []byte("looper"))
	writeFile(t, filepath.Join(assetsDir, "looper-darwin-arm64.sha256"), []byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa  looper-darwin-arm64\n"))

	_, err := BuildManifest(BuildManifestInput{
		Tag:               "vfoo",
		Released:          time.Date(2026, 4, 22, 12, 0, 0, 0, time.UTC),
		APIVersion:        "v1",
		SchemaVersion:     "0007_agent_execution_run_index",
		MinCliForDaemon:   "0.2.0",
		MinDaemonForCli:   "0.2.0",
		Repo:              "powerformer/looper",
		AssetsDir:         assetsDir,
		RequiredArtifacts: []string{"looper-darwin-arm64"},
	})
	if err == nil {
		t.Fatal("BuildManifest(...) error = nil, want invalid tag error")
	}
}

func TestBuildManifestRejectsInvalidChecksum(t *testing.T) {
	assetsDir := t.TempDir()
	writeFile(t, filepath.Join(assetsDir, "looper-darwin-arm64"), []byte("looper"))
	writeFile(t, filepath.Join(assetsDir, "looper-darwin-arm64.sha256"), []byte("not-a-sha  looper-darwin-arm64\n"))

	_, err := BuildManifest(BuildManifestInput{
		Tag:               "v1.2.3",
		Released:          time.Date(2026, 4, 22, 12, 0, 0, 0, time.UTC),
		APIVersion:        "v1",
		SchemaVersion:     "0007_agent_execution_run_index",
		MinCliForDaemon:   "0.2.0",
		MinDaemonForCli:   "0.2.0",
		Repo:              "powerformer/looper",
		AssetsDir:         assetsDir,
		RequiredArtifacts: []string{"looper-darwin-arm64"},
	})
	if err == nil {
		t.Fatal("BuildManifest(...) error = nil, want invalid checksum error")
	}
}

func TestCurrentSchemaVersionUsesLatestEmbeddedMigration(t *testing.T) {
	if got, want := CurrentSchemaVersion(), "0010_agent_native_resume"; got != want {
		t.Fatalf("CurrentSchemaVersion() = %q, want %q", got, want)
	}
}

func TestEncodeManifestProducesStableJSONShape(t *testing.T) {
	manifest := Manifest{
		ManifestVersion: 1,
		Version:         "1.2.3",
		Tag:             "v1.2.3",
		Released:        "2026-04-22T12:00:00Z",
		Channel:         "stable",
		APIVersion:      "v1",
		SchemaVersion:   "12",
		MinCliForDaemon: "0.2.0",
		MinDaemonForCli: "0.2.0",
		Artifacts: map[string]Artifact{
			"looper-darwin-arm64": {
				URL:    "https://example.test/looper",
				SHA256: "abc",
				Size:   1,
			},
		},
	}

	encoded, err := EncodeManifest(manifest)
	if err != nil {
		t.Fatalf("EncodeManifest(...) error = %v", err)
	}

	decoded := map[string]any{}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("json.Unmarshal(...) error = %v", err)
	}

	if decoded["channel"] != "stable" {
		t.Fatalf("channel = %v, want stable", decoded["channel"])
	}
	if decoded["apiVersion"] != "v1" {
		t.Fatalf("apiVersion = %v, want v1", decoded["apiVersion"])
	}
}

func writeFile(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("os.WriteFile(%q) error = %v", path, err)
	}
}
