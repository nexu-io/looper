package version

import "testing"

func TestCurrentUsesSharedBuildMetadata(t *testing.T) {
	originalValue := Value
	originalSource := VersionSource
	originalCommit := GitCommitSHA
	originalTimestamp := BuildTimestamp

	t.Cleanup(func() {
		Value = originalValue
		VersionSource = originalSource
		GitCommitSHA = originalCommit
		BuildTimestamp = originalTimestamp
	})

	Value = "1.2.3"
	VersionSource = "apps/cli/package.json"
	GitCommitSHA = "abc123"
	BuildTimestamp = "2026-04-17T00:00:00Z"

	info := Current()

	if info.Version != "1.2.3" {
		t.Fatalf("Current().Version = %q, want %q", info.Version, "1.2.3")
	}

	if info.Metadata.VersionSource != "apps/cli/package.json" {
		t.Fatalf("Current().Metadata.VersionSource = %q, want %q", info.Metadata.VersionSource, "apps/cli/package.json")
	}

	if info.Metadata.GitCommitSHA != "abc123" {
		t.Fatalf("Current().Metadata.GitCommitSHA = %q, want %q", info.Metadata.GitCommitSHA, "abc123")
	}

	if info.Metadata.BuildTimestamp != "2026-04-17T00:00:00Z" {
		t.Fatalf("Current().Metadata.BuildTimestamp = %q, want %q", info.Metadata.BuildTimestamp, "2026-04-17T00:00:00Z")
	}
}

func TestCurrentDefaultsToDevelopmentMetadata(t *testing.T) {
	originalValue := Value
	originalSource := VersionSource
	originalCommit := GitCommitSHA
	originalTimestamp := BuildTimestamp

	t.Cleanup(func() {
		Value = originalValue
		VersionSource = originalSource
		GitCommitSHA = originalCommit
		BuildTimestamp = originalTimestamp
	})

	Value = defaultVersion
	VersionSource = defaultVersionSource
	GitCommitSHA = ""
	BuildTimestamp = ""

	info := Current()

	if info.Version != defaultVersion {
		t.Fatalf("Current().Version = %q, want %q", info.Version, defaultVersion)
	}

	if info.Metadata.VersionSource != defaultVersionSource {
		t.Fatalf("Current().Metadata.VersionSource = %q, want %q", info.Metadata.VersionSource, defaultVersionSource)
	}

	if info.Metadata.GitCommitSHA != "" {
		t.Fatalf("Current().Metadata.GitCommitSHA = %q, want empty", info.Metadata.GitCommitSHA)
	}

	if info.Metadata.BuildTimestamp != "" {
		t.Fatalf("Current().Metadata.BuildTimestamp = %q, want empty", info.Metadata.BuildTimestamp)
	}
}
