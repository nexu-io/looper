package cliapp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/nexu-io/looper/internal/planestrict"
)

func TestPersonalConnectDoesNotEnrollOrRequireNetworkIdentity(t *testing.T) {
	home := t.TempDir()
	const connectionID = "66666666-7777-4888-9999-aaaaaaaaaaaa"
	const deviceID = "55555555-6666-4777-8888-999999999999"
	const projectID = "33333333-4444-4555-8666-777777777777"
	calls := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls[r.URL.Path]++
		switch r.URL.Path {
		case "/api/looper/connections/exchange/":
			var input map[string]any
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input["pairing"] != "device_v1" {
				t.Errorf("exchange: %v %v", input, err)
			}
			writePlaneCommandTestJSON(t, w, map[string]any{"personal": true, "pairing": "device_v1", "relay_url": "https://relay.example/collab", "connection_id": connectionID, "status": "cli_connected", "workspace": "team", "project_id": projectID, "member_id": "44444444-5555-4666-8777-888888888888"})
		case "/api/looper/devices/pair/":
			if r.Header.Get("X-Looper-Connect-Code") != "code" || r.Header.Get("Looper-Signature") == "" {
				t.Error("missing pairing credentials")
			}
			writePlaneCommandTestJSON(t, w, map[string]any{"binding": map[string]string{"id": deviceID, "node_id": "collab-" + connectionID}})
		case "/api/looper/devices/projects/":
			if r.Header.Get("Looper-Signature") == "" {
				t.Error("missing device signature")
			}
			writePlaneCommandTestJSON(t, w, map[string]any{"state": "active", "projects": []any{map[string]string{"workspace": "team", "id": projectID}}})
		case "/api/workspaces/team/projects/" + projectID + "/looper/nodes/session/":
			var session map[string]any
			_ = json.NewDecoder(r.Body).Decode(&session)
			session["state"] = "active"
			session["node_id"] = "collab-" + connectionID
			writePlaneCommandTestJSON(t, w, session)
		case "/api/workspaces/team/projects/" + projectID + "/looper/dispatch/inbox/":
			writePlaneCommandTestJSON(t, w, map[string]any{"dispatches": []any{}})
		case "/api/looper/connections/complete/":
			writePlaneCommandTestJSON(t, w, map[string]any{"status": "completed"})
		default:
			t.Errorf("unexpected dependency %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	configPath := writeEditableCLIConfigWithPayload(t, map[string]any{"notifications": map[string]any{"osascript": map[string]any{"enabled": false}}})
	app := New(Deps{HomeDir: home, LookPath: configLookPathForTests()})
	code, stdout, stderr := runAppWithDeps(t, app, []string{"--config", configPath, "plane", "connect", server.URL, "--code", "code", "--collab", "--json"})
	if code != 0 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
	if _, err := os.Stat(filepath.Join(home, ".looper", "network.json")); !os.IsNotExist(err) {
		t.Fatalf("network state created: %v", err)
	}
	device, err := planestrict.LoadDeviceConnection(filepath.Join(home, ".looper", "plane-device.json"))
	if err != nil || device.Pairing != "device_v1" || device.DeviceID != deviceID {
		t.Fatalf("device=%+v err=%v", device, err)
	}
	if calls["/api/looper/connections/complete/"] != 1 {
		t.Fatalf("incomplete connection: %v", calls)
	}
}
