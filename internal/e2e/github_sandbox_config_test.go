package e2e

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/config"
)

func TestGitHubSandboxRepoEnvCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{name: "preferred", env: map[string]string{envSandboxRepo: "acme/looper"}, want: "acme/looper"},
		{name: "legacy", env: map[string]string{envSandboxRepoLegacy: "legacy/looper"}, want: "legacy/looper"},
		{name: "same value", env: map[string]string{envSandboxRepo: "acme/looper", envSandboxRepoLegacy: "acme/looper"}, want: "acme/looper"},
		{name: "trims whitespace", env: map[string]string{envSandboxRepo: " acme/looper ", envSandboxRepoLegacy: "\tacme/looper\n"}, want: "acme/looper"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveGitHubSandboxRepoEnv(t, func(key string) string { return tc.env[key] })
			if got != tc.want {
				t.Fatalf("repo = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestGitHubSandboxAppCredentialReferences(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "not-read-by-parser.pem")
	valid := map[string]string{envSandboxAppID: "123", envSandboxInstallationID: "456", envSandboxAppPrivateKeyFile: keyPath}
	definition, err := parseSandboxAppIdentity(func(key string) string { return valid[key] })
	if err != nil || definition.Kind != config.HostingIdentityGitHubApp || definition.AppID != 123 || definition.InstallationID != 456 || definition.PrivateKeyFile != keyPath {
		t.Fatalf("App references = %+v, error = %v", definition, err)
	}
	for _, key := range []string{envSandboxAppID, envSandboxInstallationID, envSandboxAppPrivateKeyFile} {
		t.Run(key, func(t *testing.T) {
			_, err := parseSandboxAppIdentity(func(name string) string {
				if name == key {
					return "secret-shaped-invalid-value"
				}
				return valid[name]
			})
			if err == nil || !strings.Contains(err.Error(), key) || strings.Contains(err.Error(), "secret-shaped") {
				t.Fatalf("missing safe diagnostic for %s: %v", key, err)
			}
		})
	}
}

func TestGitHubSandboxRepoEnvConflictFailsFast(t *testing.T) {
	env := map[string]string{envSandboxRepo: "acme/looper", envSandboxRepoLegacy: "other/looper"}
	_, err := parseGitHubSandboxRepoEnv(func(key string) string { return env[key] })
	if err == nil || !strings.Contains(err.Error(), envSandboxRepo) || !strings.Contains(err.Error(), envSandboxRepoLegacy) {
		t.Fatalf("err = %v, want conflict naming both env vars", err)
	}
}
