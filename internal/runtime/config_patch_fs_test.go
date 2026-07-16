package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/config"
)

func TestPatchConfigRejectsDanglingSymlinkWithoutReplacingIt(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.Symlink("missing-target.json", path); err != nil {
		t.Skipf("create symlink: %v", err)
	}
	rt := newConfigPatchRuntime(t, path, func(string) (string, bool) { return "", false })
	err := rt.PatchConfig(context.Background(), ConfigPatch{
		Revision: config.ConfigFileRevision(nil, false),
		Set:      map[string]json.RawMessage{"scheduler.maxConcurrentRuns": json.RawMessage("4")},
	})
	var patchErr *ConfigPatchError
	if !errors.As(err, &patchErr) || patchErr.Kind != "unsupported" {
		t.Fatalf("PatchConfig() error = %#v, want unsupported symlink", err)
	}
	info, statErr := os.Lstat(path)
	if statErr != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("config path was not preserved as symlink: info=%v err=%v", info, statErr)
	}
	if target, readErr := os.Readlink(path); readErr != nil || target != "missing-target.json" {
		t.Fatalf("Readlink() = %q, %v", target, readErr)
	}
}

func TestPatchConfigCASRejectsGenerationChangedDuringValidation(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	original := []byte(`{"notifications":{"osascript":{"enabled":false}},"scheduler":{"maxConcurrentRuns":2}}`)
	external := []byte(`{"notifications":{"osascript":{"enabled":false}},"scheduler":{"maxConcurrentRuns":7}}`)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	baseLoad := func(selected string) (config.LoadedFileConfig, error) {
		return config.LoadFile(config.LoadFileOptions{ConfigPathOverride: selected, LookupEnv: func(string) (string, bool) { return "", false }})
	}
	initial, err := baseLoad(path)
	if err != nil {
		t.Fatal(err)
	}
	swapped := false
	loadAt := func(selected string) (config.LoadedFileConfig, error) {
		loaded, loadErr := baseLoad(selected)
		if loadErr == nil && strings.HasPrefix(filepath.Base(selected), ".config-reload-") && !swapped {
			swapped = true
			if writeErr := os.WriteFile(path, external, 0o600); writeErr != nil {
				t.Fatalf("external WriteFile() error = %v", writeErr)
			}
		}
		return loaded, loadErr
	}
	rt := New(Options{
		Config:        initial.Config,
		ConfigPath:    path,
		InitialConfig: initial,
		ReloadConfig:  func() (config.LoadedFileConfig, error) { return baseLoad(path) },
		LoadConfigAt:  loadAt,
	})

	err = rt.PatchConfig(context.Background(), ConfigPatch{
		Revision: config.ConfigFileRevision(original, true),
		Set:      map[string]json.RawMessage{"scheduler.maxConcurrentRuns": json.RawMessage("4")},
	})
	var patchErr *ConfigPatchError
	if !errors.As(err, &patchErr) || patchErr.Kind != "conflict" {
		t.Fatalf("PatchConfig() error = %#v, want conflict", err)
	}
	if got, readErr := os.ReadFile(path); readErr != nil || !bytes.Equal(got, external) {
		t.Fatalf("external generation was not restored: got=%q err=%v", got, readErr)
	}
}

func TestPatchConfigCASRejectsSameByteSymlinkIntroducedDuringValidation(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	target := filepath.Join(dir, "operator-config.json")
	original := []byte(`{"notifications":{"osascript":{"enabled":false}},"scheduler":{"maxConcurrentRuns":2}}`)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, original, 0o600); err != nil {
		t.Fatal(err)
	}
	baseLoad := func(selected string) (config.LoadedFileConfig, error) {
		return config.LoadFile(config.LoadFileOptions{ConfigPathOverride: selected, LookupEnv: func(string) (string, bool) { return "", false }})
	}
	initial, err := baseLoad(path)
	if err != nil {
		t.Fatal(err)
	}
	swapped := false
	loadAt := func(selected string) (config.LoadedFileConfig, error) {
		loaded, loadErr := baseLoad(selected)
		if loadErr == nil && strings.HasPrefix(filepath.Base(selected), ".config-reload-") && !swapped {
			swapped = true
			if removeErr := os.Remove(path); removeErr != nil {
				t.Fatalf("remove config for external symlink: %v", removeErr)
			}
			if linkErr := os.Symlink(target, path); linkErr != nil {
				t.Fatalf("create external symlink: %v", linkErr)
			}
		}
		return loaded, loadErr
	}
	rt := New(Options{
		Config:        initial.Config,
		ConfigPath:    path,
		InitialConfig: initial,
		ReloadConfig:  func() (config.LoadedFileConfig, error) { return baseLoad(path) },
		LoadConfigAt:  loadAt,
	})

	err = rt.PatchConfig(context.Background(), ConfigPatch{
		Revision: config.ConfigFileRevision(original, true),
		Set:      map[string]json.RawMessage{"scheduler.maxConcurrentRuns": json.RawMessage("4")},
	})
	var patchErr *ConfigPatchError
	if !errors.As(err, &patchErr) || patchErr.Kind != "unsupported" {
		t.Fatalf("PatchConfig() error = %#v, want unsupported symlink", err)
	}
	info, statErr := os.Lstat(path)
	if statErr != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("external symlink was not preserved: info=%v err=%v", info, statErr)
	}
	if got, readErr := os.ReadFile(target); readErr != nil || !bytes.Equal(got, original) {
		t.Fatalf("symlink target changed: got=%q err=%v", got, readErr)
	}
}
