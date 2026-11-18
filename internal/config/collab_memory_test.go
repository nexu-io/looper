package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestPersonalCollabMemoryLoadsFromConfigFiles(t *testing.T) {
	for _, format := range []string{"json", "yaml", "toml"} {
		t.Run(format, func(t *testing.T) {
			for _, enabled := range []bool{true, false} {
				dir := t.TempDir()
				path := filepath.Join(dir, "config."+format)
				raw, err := MarshalConfigFile(path, PartialConfig{Collab: &CollabConfig{MemoryEnabled: enabled}})
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
				loaded, err := LoadFile(LoadFileOptions{CWD: dir, ConfigPath: path, LookupEnv: emptyEnvLookup,
					LookPath: fakeLookPath(map[string]string{"git": "/git", "gh": "/gh", "osascript": "/osascript"})})
				if err != nil {
					t.Fatal(err)
				}
				if loaded.Config.Collab == nil || loaded.Config.Collab.MemoryEnabled != enabled {
					t.Fatalf("file memory opt-in was dropped: got %#v, want %v", loaded.Config.Collab, enabled)
				}
			}
		})
	}
}

func TestPersonalCollabMemoryConfigurationIsExplicitAndLayered(t *testing.T) {
	var enabled, disabled PartialConfig
	if err := json.Unmarshal([]byte(`{"collab":{"memoryEnabled":true}}`), &enabled); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(`{"collab":{"memoryEnabled":false}}`), &disabled); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		layers []PartialConfig
		want   bool
	}{{nil, false}, {[]PartialConfig{enabled}, true}, {[]PartialConfig{enabled, disabled}, false}} {
		cfg, err := Normalize(t.TempDir(), test.layers...)
		if err != nil {
			t.Fatal(err)
		}
		if got := cfg.Collab != nil && cfg.Collab.MemoryEnabled; got != test.want {
			t.Fatalf("memory opt-in=%v, want %v", got, test.want)
		}
	}
	copy := clonePartialConfig(enabled)
	copy.Collab.MemoryEnabled = false
	if !enabled.Collab.MemoryEnabled {
		t.Fatal("mutating a configuration snapshot changed the source")
	}
}
