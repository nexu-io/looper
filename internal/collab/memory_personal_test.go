package collab

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/planestrict"
	"github.com/nexu-io/looper/internal/storage"
)

// Real device credential, project discovery and SQLite, without a provider,
// repository binding, API token or legacy loopernet configuration.
func TestMemoryPersonalDeviceDiscoveryHonorsOptIn(t *testing.T) {
	ctx := context.Background()
	cfg, dir := hostFixture(t, "#!/bin/sh\nexit 1\n")
	const project = "33333333-4444-4555-8666-777777777777"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Looper-Signature") == "" {
			t.Error("project discovery was not signed")
		}
		if r.URL.Path != "/api/looper/devices/projects/" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(planestrict.DeviceProjects{State: "active", Projects: []planestrict.DeviceProject{{ID: project, Workspace: "fixture", Name: "Personal project"}}})
	}))
	defer server.Close()
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	keyPath := filepath.Join(dir, "device.pem")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	connection := planestrict.DeviceConnection{Pairing: "device_v1", BaseURL: server.URL, DeviceID: "77777777-7777-4777-8777-777777777777", NodeID: "device", Workspace: "fixture", ProjectID: project, PrivateKeyFile: keyPath}
	if err := connection.Save(filepath.Join(dir, "plane-device.json")); err != nil {
		t.Fatal(err)
	}
	db, err := storage.OpenSQLiteCoordinator(ctx, filepath.Join(dir, "state.sqlite"), storage.SQLiteCoordinatorOptions{Migrations: storage.EmbeddedMigrations})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.MigrationRunner().RunPending(ctx, storage.RunPendingOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, enabled := range []bool{false, true} {
		cfg.Collab = &config.CollabConfig{MemoryEnabled: enabled}
		h := &bridgeHost{cfg: cfg, dir: dir, db: db.DB(), repos: storage.NewRepositories(db.DB())}
		if err := h.loadPersonalPlane(filepath.Join(dir, "unused-network.json")); err != nil {
			t.Fatal(err)
		}
		// MCP registration happens before the first network discovery completes.
		socket, script := h.memoryToolPaths()
		if (socket != "" && script != "") != enabled {
			t.Fatalf("personal device memory MCP opt-in=%v: %q, %q", enabled, socket, script)
		}
		if err := h.refreshPersonalPlane(ctx); err != nil {
			t.Fatal(err)
		}
		if (h.memoryClient("personal-plane-"+project) != nil) != enabled {
			t.Fatalf("discovered personal project memory opt-in=%v was lost", enabled)
		}
		if h.memoryClient("personal-plane-other") != nil {
			t.Fatal("memory service exposed outside the discovered project")
		}
	}
}
