package cliapp

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/config"
)

func TestConfigMigrateDanglingSymlinkDestinationRequiresForce(t *testing.T) {
	t.Parallel()

	fromPath := writeEditableCLIConfigWithPayload(t, map[string]any{"defaults": map[string]any{"allowAutoApprove": false}, "notifications": map[string]any{"osascript": map[string]any{"enabled": false}}})
	toPath := filepath.Join(filepath.Dir(fromPath), "config.toml")
	if err := os.Symlink(filepath.Join(t.TempDir(), "missing.toml"), toPath); err != nil {
		t.Fatalf("Symlink(dest) error = %v", err)
	}

	exitCode, _, stderr := runAppWithLookPath(t, configLookPathForTests(), "config", "migrate", "--from", fromPath, "--to", toPath)
	if exitCode == 0 {
		t.Fatalf("Run([config migrate]) exit code = 0, want non-zero")
	}
	if !strings.Contains(stderr, "destination already exists") {
		t.Fatalf("stderr = %q, want destination exists error", stderr)
	}
}

func TestConfigMigrateForceBacksUpDanglingSymlinkDestination(t *testing.T) {
	t.Parallel()

	fromPath := writeEditableCLIConfigWithPayload(t, map[string]any{"defaults": map[string]any{"allowAutoApprove": false}, "notifications": map[string]any{"osascript": map[string]any{"enabled": false}}})
	toPath := filepath.Join(filepath.Dir(fromPath), "config.toml")
	linkTarget := filepath.Join(t.TempDir(), "missing.toml")
	if err := os.Symlink(linkTarget, toPath); err != nil {
		t.Fatalf("Symlink(dest) error = %v", err)
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
	backupTarget, err := os.Readlink(backupPath)
	if err != nil {
		t.Fatalf("Readlink(backup) error = %v", err)
	}
	if backupTarget != linkTarget {
		t.Fatalf("backup symlink target = %q, want %q", backupTarget, linkTarget)
	}
	if info, err := os.Lstat(toPath); err != nil {
		t.Fatalf("Lstat(dest after) error = %v", err)
	} else if info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("destination remained symlink after migration")
	}
}

func TestBackupConfigFileCopiesSymlinkTargetContents(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	targetPath := filepath.Join(dir, "real-config.toml")
	if err := os.WriteFile(targetPath, []byte("before = true\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(target) error = %v", err)
	}
	configPath := filepath.Join(dir, "config.toml")
	if err := os.Symlink(targetPath, configPath); err != nil {
		t.Fatalf("Symlink(config) error = %v", err)
	}

	if err := backupConfigFile(configPath); err != nil {
		t.Fatalf("backupConfigFile() error = %v", err)
	}

	backupPaths, err := filepath.Glob(configPath + ".*.bak")
	if err != nil {
		t.Fatalf("Glob(backup) error = %v", err)
	}
	if len(backupPaths) != 1 {
		t.Fatalf("len(backupPaths) = %d, want 1", len(backupPaths))
	}
	if info, err := os.Lstat(backupPaths[0]); err != nil {
		t.Fatalf("Lstat(backup) error = %v", err)
	} else if info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("backup remained symlink")
	}
	if err := os.WriteFile(targetPath, []byte("after = true\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(target after) error = %v", err)
	}
	backupRaw, err := os.ReadFile(backupPaths[0])
	if err != nil {
		t.Fatalf("ReadFile(backup) error = %v", err)
	}
	if string(backupRaw) != "before = true\n" {
		t.Fatalf("backup contents = %q, want original target contents", backupRaw)
	}
}

func TestConfigMigrateUsesActiveConfigPathFromFlagWhenFromOmitted(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	envPath := writeEditableCLIConfigWithPayload(t, map[string]any{"defaults": map[string]any{"allowAutoApprove": false}, "notifications": map[string]any{"osascript": map[string]any{"enabled": false}}})
	t.Setenv("LOOPER_CONFIG", envPath)

	rootDir := t.TempDir()
	fromPath := filepath.Join(rootDir, "custom", "active.json")
	if err := os.MkdirAll(filepath.Dir(fromPath), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(fromPath, []byte(`{"notifications":{"osascript":{"enabled":false}}}`), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	exitCode, stdout, stderr := runAppWithLookPath(t, configLookPathForTests(), "config", "migrate", "--dry-run", "--config", fromPath)
	if exitCode != 0 {
		t.Fatalf("Run([config migrate --dry-run --config]) exit code = %d; stdout=%q stderr=%q", exitCode, stdout, stderr)
	}
	if stderr != "" {
		t.Fatalf("Run([config migrate --dry-run --config]) stderr = %q, want empty", stderr)
	}
	want := "Preview migration: " + fromPath + " -> " + strings.TrimSuffix(fromPath, filepath.Ext(fromPath)) + ".toml"
	if !strings.Contains(stdout, want) {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
}

func TestConfigMigrateUsesActiveConfigPathFromEnvWhenFromOmitted(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	fromPath := filepath.Join(t.TempDir(), "env", "active.json")
	if err := os.MkdirAll(filepath.Dir(fromPath), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(fromPath, []byte(`{"notifications":{"osascript":{"enabled":false}}}`), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	t.Setenv("LOOPER_CONFIG", fromPath)

	exitCode, stdout, stderr := runAppWithLookPath(t, configLookPathForTests(), "config", "migrate", "--dry-run")
	if exitCode != 0 {
		t.Fatalf("Run([config migrate --dry-run]) exit code = %d; stdout=%q stderr=%q", exitCode, stdout, stderr)
	}
	if stderr != "" {
		t.Fatalf("Run([config migrate --dry-run]) stderr = %q, want empty", stderr)
	}
	want := "Preview migration: " + fromPath + " -> " + strings.TrimSuffix(fromPath, filepath.Ext(fromPath)) + ".toml"
	if !strings.Contains(stdout, want) {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
}

func TestConfigMigrateRejectsSameFileAcrossAbsoluteAndRelativePaths(t *testing.T) {
	fromPath := writeEditableCLIConfigWithPayload(t, map[string]any{"notifications": map[string]any{"osascript": map[string]any{"enabled": false}}})
	fromDir := filepath.Dir(fromPath)
	fromBase := filepath.Base(fromPath)
	beforeSource, err := os.ReadFile(fromPath)
	if err != nil {
		t.Fatalf("ReadFile(source) error = %v", err)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}
	defer func() {
		if err := os.Chdir(wd); err != nil {
			t.Fatalf("Chdir(restore) error = %v", err)
		}
	}()
	if err := os.Chdir(fromDir); err != nil {
		t.Fatalf("Chdir(%s) error = %v", fromDir, err)
	}

	exitCode, _, stderr := runAppWithLookPath(t, configLookPathForTests(), "config", "migrate", "--force", "--from", fromPath, "--to", fromBase)
	if exitCode == 0 {
		t.Fatalf("Run([config migrate --force]) exit code = 0, want non-zero")
	}
	if !strings.Contains(stderr, "source and destination must differ") {
		t.Fatalf("stderr = %q, want same-file error", stderr)
	}
	afterSource, err := os.ReadFile(fromPath)
	if err != nil {
		t.Fatalf("ReadFile(source after) error = %v", err)
	}
	if !bytes.Equal(beforeSource, afterSource) {
		t.Fatal("source config changed during rejected migration")
	}
}

func TestConfigMigrateRejectsSameFileThroughSymlinkAlias(t *testing.T) {
	fromPath := writeEditableCLIConfigWithPayload(t, map[string]any{"notifications": map[string]any{"osascript": map[string]any{"enabled": false}}})
	aliasPath := filepath.Join(filepath.Dir(fromPath), "config-alias.json")
	if err := os.Symlink(fromPath, aliasPath); err != nil {
		t.Fatalf("Symlink(%s, %s) error = %v", fromPath, aliasPath, err)
	}
	beforeSource, err := os.ReadFile(fromPath)
	if err != nil {
		t.Fatalf("ReadFile(source) error = %v", err)
	}

	exitCode, _, stderr := runAppWithLookPath(t, configLookPathForTests(), "config", "migrate", "--force", "--from", fromPath, "--to", aliasPath)
	if exitCode == 0 {
		t.Fatalf("Run([config migrate --force]) exit code = 0, want non-zero")
	}
	if !strings.Contains(stderr, "source and destination must differ") {
		t.Fatalf("stderr = %q, want same-file error", stderr)
	}
	afterSource, err := os.ReadFile(fromPath)
	if err != nil {
		t.Fatalf("ReadFile(source after) error = %v", err)
	}
	if !bytes.Equal(beforeSource, afterSource) {
		t.Fatal("source config changed during rejected migration")
	}
}

func TestConfigMigrateRejectsNonTOMLDestination(t *testing.T) {
	t.Parallel()

	fromPath := writeEditableCLIConfigWithPayload(t, map[string]any{"notifications": map[string]any{"osascript": map[string]any{"enabled": false}}})
	toPath := filepath.Join(filepath.Dir(fromPath), "config.yaml")

	exitCode, _, stderr := runAppWithLookPath(t, configLookPathForTests(), "config", "migrate", "--from", fromPath, "--to", toPath)
	if exitCode == 0 {
		t.Fatalf("Run([config migrate]) exit code = 0, want non-zero")
	}
	if !strings.Contains(stderr, "destination config must use .toml extension") {
		t.Fatalf("stderr = %q, want non-TOML destination error", stderr)
	}
}

func configLookPathForTests() config.LookPathFunc {
	return func(name string) (string, error) {
		return filepath.Join(string(os.PathSeparator), "detected", name), nil
	}
}

func emptyConfigEnvLookup(string) (string, bool) {
	return "", false
}
