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
		case buildGitSHAEnvVar:
			return "  abc123  "
		case buildTimestampEnvVar:
			return "  2026-04-17T00:00:00Z  "
		default:
			return ""
		}
	})

	if overrides.Version != defaultVersion {
		t.Fatalf("BuildOverridesFromEnv(...).Version = %q, want %q", overrides.Version, defaultVersion)
	}

	if overrides.VersionSource != defaultVersionSource {
		t.Fatalf("BuildOverridesFromEnv(...).VersionSource = %q, want %q", overrides.VersionSource, defaultVersionSource)
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
		Version:        "1.2.3",
		VersionSource:  "internal/version/version.go",
		GitCommitSHA:   "abc123",
		BuildTimestamp: "2026-04-17T00:00:00Z",
	})

	const want = "-X github.com/powerformer/looper/internal/version.Value=1.2.3 -X github.com/powerformer/looper/internal/version.VersionSource=internal/version/version.go -X github.com/powerformer/looper/internal/version.GitCommitSHA=abc123 -X github.com/powerformer/looper/internal/version.BuildTimestamp=2026-04-17T00:00:00Z"
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
			Version:        "9.9.9",
			VersionSource:  "internal/version/version.go",
			GitCommitSHA:   "deadbeef",
			BuildTimestamp: "2026-04-17T12:34:56Z",
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
	const want = `{"version":"9.9.9","metadata":{"versionSource":"internal/version/version.go","gitCommitSha":"deadbeef","buildTimestamp":"2026-04-17T12:34:56Z"}}`
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
