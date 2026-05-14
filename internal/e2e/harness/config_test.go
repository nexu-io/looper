package harness

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/nexu-io/looper/internal/config"
)

func TestWriteConfigPreservesRawOverrides(t *testing.T) {
	home := NewTempHome(t)
	bins := MustBinaries(t)
	port := ReserveTCPPort(t)
	cfg := DefaultConfig(t, home, ConfigOptions{Port: port, ToolPaths: TestToolPaths{Git: "/usr/bin/git", GH: bins.FakeGHPath, Looper: bins.LooperPath}})
	WriteConfig(t, home.ConfigPath, cfg, map[string]any{"legacy": map[string]any{"enabled": true}})
	payload, err := os.ReadFile(home.ConfigPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(payload, &doc); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	legacy, ok := doc["legacy"].(map[string]any)
	if !ok || legacy["enabled"] != true {
		t.Fatalf("legacy override missing: %#v", doc["legacy"])
	}
	server, ok := doc["server"].(map[string]any)
	if !ok || int(server["port"].(float64)) != port {
		t.Fatalf("server.port = %#v, want %d", server["port"], port)
	}
	_ = config.Config{}
}
