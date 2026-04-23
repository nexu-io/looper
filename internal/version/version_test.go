package version

import (
	"encoding/json"
	"testing"
)

func TestCurrentUsesSharedBuildMetadata(t *testing.T) {
	originalValue := Value
	originalSource := VersionSource
	originalChannel := Channel
	originalAPIVersion := APIVersion
	originalMinCli := MinCliForDaemon
	originalMinDaemon := MinDaemonForCli
	originalCommit := GitCommitSHA
	originalTimestamp := BuildTimestamp

	t.Cleanup(func() {
		Value = originalValue
		VersionSource = originalSource
		Channel = originalChannel
		APIVersion = originalAPIVersion
		MinCliForDaemon = originalMinCli
		MinDaemonForCli = originalMinDaemon
		GitCommitSHA = originalCommit
		BuildTimestamp = originalTimestamp
	})

	Value = "1.2.3"
	VersionSource = "internal/version/version.go"
	Channel = "stable"
	APIVersion = "v1"
	MinCliForDaemon = "0.2.0"
	MinDaemonForCli = "0.2.0"
	GitCommitSHA = "abc123"
	BuildTimestamp = "2026-04-17T00:00:00Z"

	info := Current()

	if info.Version != "1.2.3" {
		t.Fatalf("Current().Version = %q, want %q", info.Version, "1.2.3")
	}

	if info.Metadata.VersionSource != "internal/version/version.go" {
		t.Fatalf("Current().Metadata.VersionSource = %q, want %q", info.Metadata.VersionSource, "internal/version/version.go")
	}

	if info.Metadata.Channel != "stable" {
		t.Fatalf("Current().Metadata.Channel = %q, want %q", info.Metadata.Channel, "stable")
	}

	if info.Metadata.APIVersion != "v1" {
		t.Fatalf("Current().Metadata.APIVersion = %q, want %q", info.Metadata.APIVersion, "v1")
	}

	if info.Metadata.MinCliForDaemon == nil || *info.Metadata.MinCliForDaemon != "0.2.0" {
		t.Fatalf("Current().Metadata.MinCliForDaemon = %v, want %q", info.Metadata.MinCliForDaemon, "0.2.0")
	}

	if info.Metadata.MinDaemonForCli == nil || *info.Metadata.MinDaemonForCli != "0.2.0" {
		t.Fatalf("Current().Metadata.MinDaemonForCli = %v, want %q", info.Metadata.MinDaemonForCli, "0.2.0")
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
	originalChannel := Channel
	originalAPIVersion := APIVersion
	originalMinCli := MinCliForDaemon
	originalMinDaemon := MinDaemonForCli
	originalCommit := GitCommitSHA
	originalTimestamp := BuildTimestamp

	t.Cleanup(func() {
		Value = originalValue
		VersionSource = originalSource
		Channel = originalChannel
		APIVersion = originalAPIVersion
		MinCliForDaemon = originalMinCli
		MinDaemonForCli = originalMinDaemon
		GitCommitSHA = originalCommit
		BuildTimestamp = originalTimestamp
	})

	Value = defaultVersion
	VersionSource = defaultVersionSource
	Channel = defaultChannel
	APIVersion = defaultAPIVersion
	MinCliForDaemon = ""
	MinDaemonForCli = ""
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

	if info.Metadata.Channel != defaultChannel {
		t.Fatalf("Current().Metadata.Channel = %q, want %q", info.Metadata.Channel, defaultChannel)
	}

	if info.Metadata.APIVersion != defaultAPIVersion {
		t.Fatalf("Current().Metadata.APIVersion = %q, want %q", info.Metadata.APIVersion, defaultAPIVersion)
	}

	if info.Metadata.MinCliForDaemon != nil {
		t.Fatalf("Current().Metadata.MinCliForDaemon = %v, want nil", info.Metadata.MinCliForDaemon)
	}

	if info.Metadata.MinDaemonForCli != nil {
		t.Fatalf("Current().Metadata.MinDaemonForCli = %v, want nil", info.Metadata.MinDaemonForCli)
	}
}

func TestCurrentJSONMatchesStatusMetadataShape(t *testing.T) {
	originalValue := Value
	originalSource := VersionSource
	originalChannel := Channel
	originalAPIVersion := APIVersion
	originalMinCli := MinCliForDaemon
	originalMinDaemon := MinDaemonForCli
	originalCommit := GitCommitSHA
	originalTimestamp := BuildTimestamp

	t.Cleanup(func() {
		Value = originalValue
		VersionSource = originalSource
		Channel = originalChannel
		APIVersion = originalAPIVersion
		MinCliForDaemon = originalMinCli
		MinDaemonForCli = originalMinDaemon
		GitCommitSHA = originalCommit
		BuildTimestamp = originalTimestamp
	})

	Value = defaultVersion
	VersionSource = defaultVersionSource
	Channel = defaultChannel
	APIVersion = defaultAPIVersion
	MinCliForDaemon = ""
	MinDaemonForCli = ""
	GitCommitSHA = ""
	BuildTimestamp = ""

	encoded, err := json.Marshal(Current())
	if err != nil {
		t.Fatalf("json.Marshal(Current()) error = %v", err)
	}

	const want = `{"version":"0.0.0-dev","metadata":{"versionSource":"internal/version/version.go","channel":"dev","apiVersion":"v1","minCliForDaemon":null,"minDaemonForCli":null,"gitCommitSha":null,"buildTimestamp":null}}`
	if string(encoded) != want {
		t.Fatalf("json.Marshal(Current()) = %s, want %s", encoded, want)
	}
}
