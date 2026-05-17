package cliapp

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigMigrateDryRunForceRunsOverwriteBackupPreflight(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(root, "legacy.json")
	if err := os.WriteFile(sourcePath, []byte(`{"defaults":{"allowRiskyFixes":true}}`), 0o644); err != nil {
		t.Fatalf("os.WriteFile(sourcePath) error = %v", err)
	}
	destPath := filepath.Join(root, "config.toml")
	before := "[defaults]\nallowRiskyFixes = false\n"
	if err := os.WriteFile(destPath, []byte(before), 0o644); err != nil {
		t.Fatalf("os.WriteFile(destPath) error = %v", err)
	}

	originalBackup := backupConfigFileWithPath
	backupCalled := false
	backupConfigFileWithPath = func(path string) (string, error) {
		backupCalled = true
		if path != destPath {
			t.Fatalf("backup path = %q, want %q", path, destPath)
		}
		return "", errors.New("read config for backup: permission denied")
	}
	defer func() {
		backupConfigFileWithPath = originalBackup
	}()

	exitCode, stdout, stderr := runApp(t, "config", "migrate", "--from", sourcePath, "--to", destPath, "--dry-run", "--force")
	if exitCode == 0 {
		t.Fatalf("Run([config migrate --dry-run --force]) exit code = %d, want non-zero", exitCode)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "read config for backup: permission denied") {
		t.Fatalf("stderr = %q, want backup preflight error", stderr)
	}
	if !backupCalled {
		t.Fatal("expected dry-run --force to run backup preflight")
	}
	after, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatalf("os.ReadFile(destPath) error = %v", err)
	}
	if string(after) != before {
		t.Fatalf("destination changed during dry-run preflight: %q", after)
	}
}

func TestWriteMigratedConfigRemovesDestinationAfterWriteFailure(t *testing.T) {
	root := t.TempDir()
	destPath := filepath.Join(root, "config.toml")
	runtime := newCommandRuntime(New(Deps{}), nil)

	originalOpen := openExclusiveConfigWriteFile
	openExclusiveConfigWriteFile = func(path string) (configWriteFile, error) {
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err != nil {
			return nil, err
		}
		return failingConfigWriteFile{File: file, writeErr: errors.New("disk full")}, nil
	}
	defer func() {
		openExclusiveConfigWriteFile = originalOpen
	}()

	_, err := runtime.writeMigratedConfig(destPath, "title = 'preview'\n", false)
	if err == nil || err.Error() != "create config without overwrite: disk full" {
		t.Fatalf("writeMigratedConfig() error = %v, want disk-full failure", err)
	}
	if _, statErr := os.Stat(destPath); !os.IsNotExist(statErr) {
		t.Fatalf("os.Stat(%q) error = %v, want destination removed", destPath, statErr)
	}
}

func TestWriteMigratedConfigRemovesDestinationAfterCloseFailure(t *testing.T) {
	root := t.TempDir()
	destPath := filepath.Join(root, "config.toml")
	runtime := newCommandRuntime(New(Deps{}), nil)

	originalOpen := openExclusiveConfigWriteFile
	openExclusiveConfigWriteFile = func(path string) (configWriteFile, error) {
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err != nil {
			return nil, err
		}
		return failingConfigWriteFile{File: file, closeErr: errors.New("close failed")}, nil
	}
	defer func() {
		openExclusiveConfigWriteFile = originalOpen
	}()

	_, err := runtime.writeMigratedConfig(destPath, "title = 'preview'\n", false)
	if err == nil || err.Error() != "create config without overwrite: close failed" {
		t.Fatalf("writeMigratedConfig() error = %v, want close failure", err)
	}
	if _, statErr := os.Stat(destPath); !os.IsNotExist(statErr) {
		t.Fatalf("os.Stat(%q) error = %v, want destination removed", destPath, statErr)
	}
}

type failingConfigWriteFile struct {
	*os.File
	writeErr error
	closeErr error
}

func (f failingConfigWriteFile) WriteString(s string) (int, error) {
	if f.writeErr != nil {
		if _, err := f.File.WriteString(s[:min(len(s), 1)]); err != nil {
			return 0, err
		}
		return 1, f.writeErr
	}
	return f.File.WriteString(s)
}

func (f failingConfigWriteFile) Close() error {
	err := f.File.Close()
	if err != nil {
		return err
	}
	return f.closeErr
}

func min(a int, b int) int {
	if a < b {
		return a
	}
	return b
}
