package version

import (
	"encoding/json"
	"testing"
)

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
	VersionSource = "internal/version/version.go"
	GitCommitSHA = "abc123"
	BuildTimestamp = "2026-04-17T00:00:00Z"

	info := Current()

	if info.Version != "1.2.3" {
		t.Fatalf("Current().Version = %q, want %q", info.Version, "1.2.3")
	}

	if info.Metadata.VersionSource != "internal/version/version.go" {
		t.Fatalf("Current().Metadata.VersionSource = %q, want %q", info.Metadata.VersionSource, "internal/version/version.go")
	}

	if info.Metadata.GitCommitSHA == nil || *info.Metadata.GitCommitSHA != "abc123" {
		t.Fatalf("Current().Metadata.GitCommitSHA = %v, want %q", info.Metadata.GitCommitSHA, "abc123")
	}

	if info.Metadata.BuildTimestamp == nil || *info.Metadata.BuildTimestamp != "2026-04-17T00:00:00Z" {
		t.Fatalf("Current().Metadata.BuildTimestamp = %v, want %q", info.Metadata.BuildTimestamp, "2026-04-17T00:00:00Z")
	}
}

func TestCurrentDefaultsToPackageVersionMetadata(t *testing.T) {
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

	if info.Metadata.GitCommitSHA != nil {
		t.Fatalf("Current().Metadata.GitCommitSHA = %v, want nil", info.Metadata.GitCommitSHA)
	}

	if info.Metadata.BuildTimestamp != nil {
		t.Fatalf("Current().Metadata.BuildTimestamp = %v, want nil", info.Metadata.BuildTimestamp)
	}
}

func TestCurrentJSONMatchesStatusMetadataShape(t *testing.T) {
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

	encoded, err := json.Marshal(Current())
	if err != nil {
		t.Fatalf("json.Marshal(Current()) error = %v", err)
	}

	const want = `{"version":"0.2.1","metadata":{"versionSource":"internal/version/version.go","gitCommitSha":null,"buildTimestamp":null}}`
	if string(encoded) != want {
		t.Fatalf("json.Marshal(Current()) = %s, want %s", encoded, want)
	}
}
