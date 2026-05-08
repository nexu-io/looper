package version

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultBuildOverrides(t *testing.T) {
	overrides := DefaultBuildOverrides()

	if overrides.Version != defaultVersion {
		t.Fatalf("DefaultBuildOverrides().Version = %q, want %q", overrides.Version, defaultVersion)
	}

	if overrides.VersionSource != defaultVersionSource {
		t.Fatalf("DefaultBuildOverrides().VersionSource = %q, want %q", overrides.VersionSource, defaultVersionSource)
	}

	if overrides.Channel != defaultChannel {
		t.Fatalf("DefaultBuildOverrides().Channel = %q, want %q", overrides.Channel, defaultChannel)
	}

	if overrides.APIVersion != defaultAPIVersion {
		t.Fatalf("DefaultBuildOverrides().APIVersion = %q, want %q", overrides.APIVersion, defaultAPIVersion)
	}

	if overrides.MinCliForDaemon != "" {
		t.Fatalf("DefaultBuildOverrides().MinCliForDaemon = %q, want empty string", overrides.MinCliForDaemon)
	}

	if overrides.MinDaemonForCli != "" {
		t.Fatalf("DefaultBuildOverrides().MinDaemonForCli = %q, want empty string", overrides.MinDaemonForCli)
	}

	if overrides.GitCommitSHA != "" {
		t.Fatalf("DefaultBuildOverrides().GitCommitSHA = %q, want empty string", overrides.GitCommitSHA)
	}

	if overrides.BuildTimestamp != "" {
		t.Fatalf("DefaultBuildOverrides().BuildTimestamp = %q, want empty string", overrides.BuildTimestamp)
	}
}

func TestBuildOverridesFromEnvUsesOptionalBuildMetadata(t *testing.T) {
	overrides := BuildOverridesFromEnv(func(name string) string {
		switch name {
		case buildVersionEnvVar:
			return "  1.2.3  "
		case buildVersionSourceEnvVar:
			return "  git-tag:v1.2.3  "
		case buildChannelEnvVar:
			return "  stable  "
		case buildAPIVersionEnvVar:
			return "  v1  "
		case buildMinCliForDaemonEnvVar:
			return "  0.2.0  "
		case buildMinDaemonForCliEnvVar:
			return "  0.2.0  "
		case buildGitSHAEnvVar:
			return "  abc123  "
		case buildTimestampEnvVar:
			return "  2026-04-17T00:00:00Z  "
		default:
			return ""
		}
	})

	if overrides.Version != "1.2.3" {
		t.Fatalf("BuildOverridesFromEnv(...).Version = %q, want %q", overrides.Version, "1.2.3")
	}

	if overrides.VersionSource != "git-tag:v1.2.3" {
		t.Fatalf("BuildOverridesFromEnv(...).VersionSource = %q, want %q", overrides.VersionSource, "git-tag:v1.2.3")
	}

	if overrides.Channel != "stable" {
		t.Fatalf("BuildOverridesFromEnv(...).Channel = %q, want %q", overrides.Channel, "stable")
	}

	if overrides.APIVersion != "v1" {
		t.Fatalf("BuildOverridesFromEnv(...).APIVersion = %q, want %q", overrides.APIVersion, "v1")
	}

	if overrides.MinCliForDaemon != "0.2.0" {
		t.Fatalf("BuildOverridesFromEnv(...).MinCliForDaemon = %q, want %q", overrides.MinCliForDaemon, "0.2.0")
	}

	if overrides.MinDaemonForCli != "0.2.0" {
		t.Fatalf("BuildOverridesFromEnv(...).MinDaemonForCli = %q, want %q", overrides.MinDaemonForCli, "0.2.0")
	}

	if overrides.GitCommitSHA != "abc123" {
		t.Fatalf("BuildOverridesFromEnv(...).GitCommitSHA = %q, want %q", overrides.GitCommitSHA, "abc123")
	}

	if overrides.BuildTimestamp != "2026-04-17T00:00:00Z" {
		t.Fatalf("BuildOverridesFromEnv(...).BuildTimestamp = %q, want %q", overrides.BuildTimestamp, "2026-04-17T00:00:00Z")
	}
}

func TestLDFlagsMatchesVersionVariables(t *testing.T) {
	ldflags := LDFlags(BuildOverrides{
		Version:         "1.2.3",
		VersionSource:   "internal/version/version.go",
		Channel:         "stable",
		APIVersion:      "v1",
		MinCliForDaemon: "0.2.0",
		MinDaemonForCli: "0.2.0",
		GitCommitSHA:    "abc123",
		BuildTimestamp:  "2026-04-17T00:00:00Z",
	})

	const want = "-X github.com/nexu-io/looper/internal/version.Value=1.2.3 -X github.com/nexu-io/looper/internal/version.VersionSource=internal/version/version.go -X github.com/nexu-io/looper/internal/version.Channel=stable -X github.com/nexu-io/looper/internal/version.APIVersion=v1 -X github.com/nexu-io/looper/internal/version.MinCliForDaemon=0.2.0 -X github.com/nexu-io/looper/internal/version.MinDaemonForCli=0.2.0 -X github.com/nexu-io/looper/internal/version.GitCommitSHA=abc123 -X github.com/nexu-io/looper/internal/version.BuildTimestamp=2026-04-17T00:00:00Z"
	if ldflags != want {
		t.Fatalf("LDFlags(...) = %q, want %q", ldflags, want)
	}
}

func TestLDFlagsInjectIntoBuiltBinary(t *testing.T) {
	tempDir := t.TempDir()
	outputPath := filepath.Join(tempDir, "print-version")

	command := exec.Command(
		"go",
		"build",
		"-o",
		outputPath,
		"-ldflags",
		LDFlags(BuildOverrides{
			Version:         "9.9.9",
			VersionSource:   "internal/version/version.go",
			Channel:         "stable",
			APIVersion:      "v1",
			MinCliForDaemon: "0.2.0",
			MinDaemonForCli: "0.2.0",
			GitCommitSHA:    "deadbeef",
			BuildTimestamp:  "2026-04-17T12:34:56Z",
		}),
		"./tools/print-version-json",
	)
	command.Dir = repoRoot(t)
	command.Env = os.Environ()
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("go build test helper failed: %v\n%s", err, output)
	}

	runCommand := exec.Command(outputPath)
	runCommand.Env = os.Environ()
	runOutput, err := runCommand.CombinedOutput()
	if err != nil {
		t.Fatalf("running built helper failed: %v\n%s", err, runOutput)
	}

	got := strings.TrimSpace(string(runOutput))
	const want = `{"version":"9.9.9","metadata":{"versionSource":"internal/version/version.go","channel":"stable","apiVersion":"v1","minCliForDaemon":"0.2.0","minDaemonForCli":"0.2.0","gitCommitSha":"deadbeef","buildTimestamp":"2026-04-17T12:34:56Z"}}`
	if got != want {
		t.Fatalf("built helper output = %s, want %s", got, want)
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()

	workingDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd() error = %v", err)
	}

	return filepath.Clean(filepath.Join(workingDir, "..", ".."))
}
