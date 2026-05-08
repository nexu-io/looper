package release

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/storage"
)

const ManifestVersion = 1

var releaseTagPattern = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?$`)

type Manifest struct {
	ManifestVersion int                 `json:"manifestVersion"`
	Version         string              `json:"version"`
	Tag             string              `json:"tag"`
	Released        string              `json:"released"`
	Channel         string              `json:"channel"`
	APIVersion      string              `json:"apiVersion"`
	SchemaVersion   string              `json:"schemaVersion"`
	MinCliForDaemon string              `json:"minCliForDaemon"`
	MinDaemonForCli string              `json:"minDaemonForCli"`
	Artifacts       map[string]Artifact `json:"artifacts"`
}

type Artifact struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type BuildManifestInput struct {
	Tag               string
	Version           string
	Released          time.Time
	Channel           string
	APIVersion        string
	SchemaVersion     string
	MinCliForDaemon   string
	MinDaemonForCli   string
	Repo              string
	AssetsDir         string
	RequiredArtifacts []string
}

func BuildManifest(input BuildManifestInput) (Manifest, error) {
	tag := strings.TrimSpace(input.Tag)
	if !releaseTagPattern.MatchString(tag) {
		return Manifest{}, fmt.Errorf("tag must match vMAJOR.MINOR.PATCH[-PRERELEASE]: %q", tag)
	}

	version := strings.TrimSpace(input.Version)
	if version == "" {
		version = strings.TrimPrefix(tag, "v")
	}

	if input.Released.IsZero() {
		return Manifest{}, fmt.Errorf("released time is required")
	}

	apiVersion := strings.TrimSpace(input.APIVersion)
	if apiVersion == "" {
		return Manifest{}, fmt.Errorf("apiVersion is required")
	}

	schemaVersion := strings.TrimSpace(input.SchemaVersion)
	if schemaVersion == "" {
		return Manifest{}, fmt.Errorf("schemaVersion is required")
	}

	minCliForDaemon := strings.TrimSpace(input.MinCliForDaemon)
	if minCliForDaemon == "" {
		return Manifest{}, fmt.Errorf("minCliForDaemon is required")
	}

	minDaemonForCli := strings.TrimSpace(input.MinDaemonForCli)
	if minDaemonForCli == "" {
		return Manifest{}, fmt.Errorf("minDaemonForCli is required")
	}

	repo := strings.TrimSpace(input.Repo)
	if repo == "" {
		return Manifest{}, fmt.Errorf("repo is required")
	}

	channel := strings.TrimSpace(input.Channel)
	if channel == "" {
		if strings.Contains(version, "-") {
			channel = "beta"
		} else {
			channel = "stable"
		}
	}

	assets, err := collectArtifacts(strings.TrimSpace(input.AssetsDir), repo, tag)
	if err != nil {
		return Manifest{}, err
	}
	if len(input.RequiredArtifacts) == 0 {
		return Manifest{}, fmt.Errorf("at least one required artifact is required")
	}

	for _, name := range input.RequiredArtifacts {
		if _, ok := assets[name]; !ok {
			return Manifest{}, fmt.Errorf("missing required artifact: %s", name)
		}
	}

	return Manifest{
		ManifestVersion: ManifestVersion,
		Version:         version,
		Tag:             tag,
		Released:        input.Released.UTC().Format(time.RFC3339),
		Channel:         channel,
		APIVersion:      apiVersion,
		SchemaVersion:   schemaVersion,
		MinCliForDaemon: minCliForDaemon,
		MinDaemonForCli: minDaemonForCli,
		Artifacts:       assets,
	}, nil
}

func CurrentSchemaVersion() string {
	if len(storage.EmbeddedMigrations) == 0 {
		return ""
	}
	return storage.EmbeddedMigrations[len(storage.EmbeddedMigrations)-1].ID
}

func EncodeManifest(manifest Manifest) ([]byte, error) {
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(encoded, '\n'), nil
}

func collectArtifacts(assetsDir, repo, tag string) (map[string]Artifact, error) {
	if assetsDir == "" {
		return nil, fmt.Errorf("assets directory is required")
	}
	entries, err := os.ReadDir(assetsDir)
	if err != nil {
		return nil, fmt.Errorf("read assets directory: %w", err)
	}

	artifactNames := make([]string, 0)
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || strings.HasSuffix(name, ".sha256") || strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".minisig") {
			continue
		}
		artifactNames = append(artifactNames, name)
	}
	sort.Strings(artifactNames)

	artifacts := make(map[string]Artifact, len(artifactNames))
	for _, artifactName := range artifactNames {
		path := filepath.Join(assetsDir, artifactName)
		info, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("stat artifact %s: %w", artifactName, err)
		}

		shaPath := path + ".sha256"
		shaBytes, err := os.ReadFile(shaPath)
		if err != nil {
			return nil, fmt.Errorf("read checksum for %s: %w", artifactName, err)
		}
		sha := parseSHA256(string(shaBytes))
		if sha == "" {
			return nil, fmt.Errorf("invalid checksum format for %s", artifactName)
		}

		artifacts[artifactName] = Artifact{
			URL:    fmt.Sprintf("https://github.com/%s/releases/download/%s/%s", repo, tag, artifactName),
			SHA256: sha,
			Size:   info.Size(),
		}
	}

	return artifacts, nil
}

func parseSHA256(raw string) string {
	fields := strings.Fields(raw)
	if len(fields) == 0 {
		return ""
	}
	hash := strings.ToLower(strings.TrimSpace(fields[0]))
	if len(hash) != 64 {
		return ""
	}
	for _, char := range hash {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return ""
		}
	}
	return hash
}
