package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/release"
)

func main() {
	var (
		tag             = flag.String("tag", "", "git tag (for example: v0.3.0)")
		version         = flag.String("version", "", "release version without v prefix; defaults from tag")
		released        = flag.String("released", "", "RFC3339 UTC release timestamp")
		channel         = flag.String("channel", "", "release channel (stable|beta), defaults from version")
		apiVersion      = flag.String("api-version", "v1", "management API version")
		schemaVersion   = flag.String("schema-version", release.CurrentSchemaVersion(), "storage schema version")
		minCliForDaemon = flag.String("min-cli-for-daemon", "", "minimum CLI required by daemon")
		minDaemonForCli = flag.String("min-daemon-for-cli", "", "minimum daemon required by CLI")
		repo            = flag.String("repo", "nexu-io/looper", "GitHub repo owner/name")
		assetsDir       = flag.String("assets-dir", "", "release assets directory")
		output          = flag.String("output", "", "output manifest path")
		requiredAssets  = flag.String("required-assets", "", "comma-separated required artifact names")
	)
	flag.Parse()

	if strings.TrimSpace(*tag) == "" || strings.TrimSpace(*released) == "" || strings.TrimSpace(*assetsDir) == "" || strings.TrimSpace(*output) == "" {
		_, _ = fmt.Fprintln(os.Stderr, "required flags: -tag, -released, -assets-dir, -output")
		os.Exit(2)
	}

	releasedAt, err := time.Parse(time.RFC3339, strings.TrimSpace(*released))
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "parse -released: %v\n", err)
		os.Exit(1)
	}

	required := splitCSV(*requiredAssets)
	manifest, err := release.BuildManifest(release.BuildManifestInput{
		Tag:               *tag,
		Version:           *version,
		Released:          releasedAt,
		Channel:           *channel,
		APIVersion:        *apiVersion,
		SchemaVersion:     *schemaVersion,
		MinCliForDaemon:   *minCliForDaemon,
		MinDaemonForCli:   *minDaemonForCli,
		Repo:              *repo,
		AssetsDir:         *assetsDir,
		RequiredArtifacts: required,
	})
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "build manifest: %v\n", err)
		os.Exit(1)
	}

	encoded, err := release.EncodeManifest(manifest)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "encode manifest: %v\n", err)
		os.Exit(1)
	}

	if err := os.WriteFile(strings.TrimSpace(*output), encoded, 0o644); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "write manifest: %v\n", err)
		os.Exit(1)
	}
}

func splitCSV(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
