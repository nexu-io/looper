package config

import "fmt"

type deprecatedSurfaceKind string

const (
	deprecatedSurfaceConfigPath deprecatedSurfaceKind = "config path"
	deprecatedSurfaceEnvVar     deprecatedSurfaceKind = "environment variable"
	deprecatedSurfaceCLIFlag    deprecatedSurfaceKind = "CLI flag"
)

type deprecatedSurface struct {
	kind        deprecatedSurfaceKind
	legacy      string
	replacement string
}

func deprecationWarning(kind deprecatedSurfaceKind, legacy string, replacement string) string {
	return fmt.Sprintf("deprecated %s %q is accepted for now; use %q instead", kind, legacy, replacement)
}

func removedLegacySurfaceError(kind deprecatedSurfaceKind, legacy string, replacement string) string {
	return fmt.Sprintf("legacy %s %q is no longer supported; use %q instead", kind, legacy, replacement)
}

func legacyDefaultConfigMigrationNote(legacyPath string, canonicalPath string) string {
	return fmt.Sprintf("legacy default config file %q is still supported for now. TOML is now the preferred default format/path; move this config to %q, keep only one default config file in place, and update any legacy keys to the canonical taxonomy described in docs/configuration.md", legacyPath, canonicalPath)
}

func dedupeDeprecationWarnings(surfaces []deprecatedSurface) []string {
	warnings := make([]string, 0, len(surfaces))
	seen := map[deprecatedSurface]struct{}{}
	for _, surface := range surfaces {
		if _, ok := seen[surface]; ok {
			continue
		}
		seen[surface] = struct{}{}
		warnings = append(warnings, deprecationWarning(surface.kind, surface.legacy, surface.replacement))
	}
	return warnings
}
