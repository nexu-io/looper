package config

import (
	"encoding/json"
	"testing"
)

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
