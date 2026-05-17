package cliapp

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/config"
)

func TestConfigMigrateDryRunPreviewsCanonicalTOMLWithoutWritingDestination(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	fromPath := filepath.Join(homeDir, ".looper", "config.json")
	toPath := filepath.Join(homeDir, ".looper", "config.toml")
	if err := os.MkdirAll(filepath.Dir(fromPath), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	legacy := `{
		"defaults": {"allowAutoApprove": true, "fixAllPullRequests": true},
		"reviewer": {"reviewEvents": {"blocking": "REQUEST_CHANGES"}},
		"roles": {"reviewer": {"autoDiscovery": true, "triggers": {"requireReviewRequest": true}, "specReview": {"includeReviewingLabel": true}}},
		"projects": [{"id": "project_1", "name": "Repo", "path": "/tmp/repo", "instructions": {"reviewer": "be careful"}, "roles": {"reviewer": {"autoDiscovery": true, "triggers": {"requireReviewRequest": true}}}}],
		"notifications": {"osascript": {"enabled": false}}
	}`
	if err := os.WriteFile(fromPath, []byte(legacy), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	exitCode, stdout, stderr := runAppWithLookPath(t, configLookPathForTests(), "config", "migrate", "--dry-run", "--from", fromPath)
	if exitCode != 0 {
		t.Fatalf("Run([config migrate --dry-run]) exit code = %d; stdout=%q stderr=%q", exitCode, stdout, stderr)
	}
	if stderr != "" {
		t.Fatalf("Run([config migrate --dry-run]) stderr = %q, want empty", stderr)
	}
	if _, err := os.Stat(toPath); !os.IsNotExist(err) {
		t.Fatalf("Stat(%s) err = %v, want not exists", toPath, err)
	}
	for _, notWant := range []string{"reviewer =", "[roles.reviewer.specReview]", "path = \"/tmp/repo\"", "instructions = {"} {
		if strings.Contains(stdout, notWant) {
			t.Fatalf("dry-run preview unexpectedly contained %q:\n%s", notWant, stdout)
		}
	}
	for _, want := range []string{"Preview migration:", "[roles.reviewer.behavior.reviewEvents]", "clean = 'APPROVE'", "authorFilter = 'any'", "repoPath = '/tmp/repo'", "[projects.roles.reviewer.discovery]"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("dry-run preview missing %q:\n%s", want, stdout)
		}
	}

	previewPath := filepath.Join(t.TempDir(), "preview.toml")
	preview := stdout[strings.Index(stdout, "[roles.reviewer.behavior.reviewEvents]"):]
	if err := os.WriteFile(previewPath, []byte(preview), 0o644); err != nil {
		t.Fatalf("WriteFile(preview) error = %v", err)
	}
	loaded, err := config.LoadFile(config.LoadFileOptions{CWD: t.TempDir(), ConfigPath: previewPath, LookupEnv: emptyConfigEnvLookup, LookPath: configLookPathForTests()})
	if err != nil {
		t.Fatalf("LoadFile(preview) error = %v", err)
	}
	if len(loaded.Warnings) != 0 {
		t.Fatalf("LoadFile(preview).Warnings = %#v, want none", loaded.Warnings)
	}
	if len(loaded.Notices) != 0 {
		t.Fatalf("LoadFile(preview).Notices = %#v, want none", loaded.Notices)
	}
}

func TestConfigMigrateWritesDestinationAndPreservesSource(t *testing.T) {
	t.Parallel()

	fromPath := writeEditableCLIConfigWithPayload(t, map[string]any{"defaults": map[string]any{"allowAutoApprove": false}, "notifications": map[string]any{"osascript": map[string]any{"enabled": false}}})
	toPath := filepath.Join(filepath.Dir(fromPath), "config.toml")
	beforeSource, err := os.ReadFile(fromPath)
	if err != nil {
		t.Fatalf("ReadFile(source) error = %v", err)
	}

	exitCode, stdout, stderr := runAppWithLookPath(t, configLookPathForTests(), "config", "migrate", "--from", fromPath, "--to", toPath)
	if exitCode != 0 {
		t.Fatalf("Run([config migrate]) exit code = %d; stdout=%q stderr=%q", exitCode, stdout, stderr)
	}
	if stderr != "" {
		t.Fatalf("Run([config migrate]) stderr = %q, want empty", stderr)
	}
	if !strings.Contains(stdout, "Migrated config:") {
		t.Fatalf("stdout = %q, want migrated summary", stdout)
	}
	afterSource, err := os.ReadFile(fromPath)
	if err != nil {
		t.Fatalf("ReadFile(source after) error = %v", err)
	}
	if !bytes.Equal(beforeSource, afterSource) {
		t.Fatal("source config changed during migration")
	}
	rawDest, err := os.ReadFile(toPath)
	if err != nil {
		t.Fatalf("ReadFile(dest) error = %v", err)
	}
	if strings.Contains(string(rawDest), "allowAutoApprove") {
		t.Fatalf("destination still contained legacy key:\n%s", rawDest)
	}
}

func TestConfigMigrateDryRunJSONReportsLegacyChanges(t *testing.T) {
	t.Parallel()

	fromPath := writeEditableCLIConfigWithPayload(t, map[string]any{
		"defaults": map[string]any{"allowAutoApprove": true},
		"projects": []map[string]any{{
			"id":   "project_1",
			"name": "Repo",
			"path": "/tmp/repo",
			"instructions": map[string]any{
				"worker": "legacy worker instructions",
			},
		}},
		"notifications": map[string]any{"osascript": map[string]any{"enabled": false}},
	})

	exitCode, stdout, stderr := runAppWithLookPath(t, configLookPathForTests(), "config", "migrate", "--dry-run", "--json", "--from", fromPath)
	if exitCode != 0 {
		t.Fatalf("Run([config migrate --dry-run --json]) exit code = %d; stdout=%q stderr=%q", exitCode, stdout, stderr)
	}
	if stderr != "" {
		t.Fatalf("Run([config migrate --dry-run --json]) stderr = %q, want empty", stderr)
	}

	var decoded map[string]any
	if err := json.Unmarshal([]byte(stdout), &decoded); err != nil {
		t.Fatalf("Unmarshal(stdout) error = %v\nraw=%q", err, stdout)
	}
	changes, ok := decoded["changes"].([]any)
	if !ok {
		t.Fatalf("changes type = %T, want []any", decoded["changes"])
	}
	got := make([]string, 0, len(changes))
	for _, change := range changes {
		text, ok := change.(string)
		if !ok {
			t.Fatalf("change type = %T, want string", change)
		}
		got = append(got, text)
	}
	for _, want := range []string{
		"defaults.allowAutoApprove -> roles.reviewer.behavior.reviewEvents.clean",
		"projects[0].instructions -> projects[0].roles.<role>.instructions",
	} {
		if !slices.Contains(got, want) {
			t.Fatalf("changes = %#v, want %q", got, want)
		}
	}
}

func TestConfigMigrateExistingDestinationRequiresForce(t *testing.T) {
	t.Parallel()

	fromPath := writeEditableCLIConfigWithPayload(t, map[string]any{"defaults": map[string]any{"allowAutoApprove": false}, "notifications": map[string]any{"osascript": map[string]any{"enabled": false}}})
	toPath := filepath.Join(filepath.Dir(fromPath), "config.toml")
	if err := os.WriteFile(toPath, []byte("existing = true\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(dest) error = %v", err)
	}

	exitCode, _, stderr := runAppWithLookPath(t, configLookPathForTests(), "config", "migrate", "--from", fromPath, "--to", toPath)
	if exitCode == 0 {
		t.Fatalf("Run([config migrate]) exit code = 0, want non-zero")
	}
	if !strings.Contains(stderr, "destination already exists") {
		t.Fatalf("stderr = %q, want destination exists error", stderr)
	}
}

func TestConfigMigrateForceCreatesBackup(t *testing.T) {
	t.Parallel()

	fromPath := writeEditableCLIConfigWithPayload(t, map[string]any{"defaults": map[string]any{"allowAutoApprove": false}, "notifications": map[string]any{"osascript": map[string]any{"enabled": false}}})
	toPath := filepath.Join(filepath.Dir(fromPath), "config.toml")
	if err := os.WriteFile(toPath, []byte("existing = true\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(dest) error = %v", err)
	}

	exitCode, stdout, stderr := runAppWithLookPath(t, configLookPathForTests(), "config", "migrate", "--force", "--json", "--from", fromPath, "--to", toPath)
	if exitCode != 0 {
		t.Fatalf("Run([config migrate --force --json]) exit code = %d; stdout=%q stderr=%q", exitCode, stdout, stderr)
	}
	if stderr != "" {
		t.Fatalf("Run([config migrate --force --json]) stderr = %q, want empty", stderr)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(stdout), &decoded); err != nil {
		t.Fatalf("Unmarshal(stdout) error = %v\nraw=%q", err, stdout)
	}
	backupPath, _ := decoded["backupPath"].(string)
	if backupPath == "" {
		t.Fatalf("backupPath missing from JSON output: %#v", decoded)
	}
	backupRaw, err := os.ReadFile(backupPath)
	if err != nil {
		t.Fatalf("ReadFile(backup) error = %v", err)
	}
	if string(backupRaw) != "existing = true\n" {
		t.Fatalf("backup contents = %q, want original destination", backupRaw)
	}
}
