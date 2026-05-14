package cliapp

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

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
