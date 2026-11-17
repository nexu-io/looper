package cliapp

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"github.com/nexu-io/looper/internal/collab"
	"github.com/nexu-io/looper/internal/config"
	networkclient "github.com/nexu-io/looper/internal/network/client"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPlaneDoctorCollabUsesIndependentSocketWithoutDaemonOrPlaneCLI(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "doctor-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	listener, err := net.Listen("unix", collab.SocketPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	reads := 0
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reads++
		if r.URL.Path != "/status" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"service":"looper collab","state":"running"}`))
	})}
	go server.Serve(listener)
	defer server.Close()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/users/me/" || r.Header.Get("X-API-Key") != "doctor-fixture" {
			t.Errorf("wrong Plane identity request %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"id":"owner"}`))
	}))
	defer api.Close()
	configPath := writeEditableCLIConfigWithPayload(t, map[string]any{
		"providers":     []any{map[string]any{"id": "plane", "kind": "plane", "baseUrl": api.URL + "/api/v1", "workspace": "fixture", "projectId": "project", "tokenEnv": "DOCTOR_UNUSED_TOKEN"}},
		"notifications": map[string]any{"osascript": map[string]any{"enabled": false}},
	})
	t.Setenv("DOCTOR_UNUSED_TOKEN", "doctor-fixture")
	app := New(Deps{HomeDir: filepath.Join(dir, "home"), LookPath: configLookPathForTests(), RunCommand: func(_ context.Context, command string, args []string, _ time.Duration) (commandExecutionResult, error) {
		if filepath.Base(command) == "plane" {
			t.Error("independent collab does not require the Plane CLI")
		}
		return commandExecutionResult{ExitCode: 0}, nil
	}})
	_, stdout, stderr := runAppWithDeps(t, app, []string{"--config", configPath, "plane", "doctor", "--collab", "--state-dir", dir, "--json"})
	var result planeDoctorResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("output %s stderr %s: %v", stdout, stderr, err)
	}
	found := false
	for _, check := range result.Checks {
		if check.Name == "daemon" || check.Name == "plane-cli" {
			t.Fatalf("unrelated dependency: %+v", check)
		}
		if check.Name == "collab" {
			found = check.Status == "failed" // This transport-only fixture has no active node binding.
		}
	}
	if !found || reads != 2 {
		t.Fatalf("collab not checked: %+v reads=%d", result.Checks, reads)
	}
}

func TestPlaneDoctorCollabRejectsUnavailableAndMismatchedHosts(t *testing.T) {
	t.Parallel()
	dir, err := os.MkdirTemp("", "lc-doctor-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	cfg := config.Config{Projects: []config.ProjectRefConfig{{Provider: "plane", Repo: "acme/local"}}}
	provider := config.ProviderConfig{ID: "plane", Workspace: stringPtr("fixture"), ProjectID: stringPtr("project"), StrictDispatch: &config.PlaneStrictDispatchConfig{NodeID: "node"}}
	if check := checkPlaneCollabHost(context.Background(), cfg, dir, provider); check.Status != "failed" {
		t.Fatal("unavailable host passed readiness")
	}
	for _, tc := range []struct {
		name string
		edit func(*collab.Status)
	}{
		{"stopping", func(s *collab.Status) { s.State = "stopping" }},
		{"wrong service", func(s *collab.Status) { s.Service = "other" }},
		{"unbound", func(s *collab.Status) { s.Network.Configured = false }},
		{"offline", func(s *collab.Status) { s.Network.CloudReachable = false }},
		{"identity drift", func(s *collab.Status) { s.Network.IdentityDrift = true }},
		{"wrong node", func(s *collab.Status) { s.Network.NodeID = "other" }},
		{"stale config", func(s *collab.Status) { s.PlaneTargets = nil }},
		{"wrong repo", func(s *collab.Status) { s.PlaneTargets[0].Repo = "acme/other" }},
		{"wrong project", func(s *collab.Status) { s.PlaneTargets[0].ProjectID = "other" }},
		{"wrong workspace", func(s *collab.Status) { s.PlaneTargets[0].Workspace = "other" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status := doctorCollabStatus()
			tc.edit(&status)
			serveDoctorCollabStatus(t, dir, status, func() {
				check := checkPlaneCollabHost(context.Background(), cfg, dir, provider)
				if check.Status != "failed" || check.Detail == "" || check.Remediation == "" {
					t.Fatalf("bad host accepted or unexplained: %+v", check)
				}
			})
		})
	}
	cfg.Projects = append(cfg.Projects, config.ProjectRefConfig{Provider: "plane", Repo: "acme/second"})
	serveDoctorCollabStatus(t, dir, doctorCollabStatus(), func() {
		if check := checkPlaneCollabHost(context.Background(), cfg, dir, provider); check.Status != "failed" || !strings.Contains(check.Detail, "acme/second") {
			t.Fatalf("partially loaded provider passed: %+v", check)
		}
	})
}

// Use a real Unix control socket and the signed Plane client. External APIs and
// tool authentication are deterministic fixtures; no daemon or agent is started.
func TestPlaneDoctorCollabUsesLoadedTargetsWithoutDaemon(t *testing.T) {
	homeDir := t.TempDir()
	dir, err := os.MkdirTemp("", "lc-doctor-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	projectID := "33333333-4444-4555-8666-777777777777"
	daemonProbes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/users/me"):
			http.Redirect(w, r, r.URL.Path+"/", http.StatusMovedPermanently)
		case strings.HasSuffix(r.URL.Path, "/users/me/"):
			writePlaneCommandTestJSON(t, w, map[string]any{"id": "owner"})
		case strings.HasSuffix(r.URL.Path, "/nodes/session/"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			writePlaneCommandTestJSON(t, w, map[string]any{"session_id": body["session_id"], "state": "active"})
		case strings.HasSuffix(r.URL.Path, "/dispatch/inbox/"):
			if r.Header.Get("Looper-Signature") == "" {
				t.Error("unsigned inbox request")
			}
			writePlaneCommandTestJSON(t, w, map[string]any{"integration_state": "active", "dispatches": []any{}})
		case r.URL.Path == "/api/v1/status":
			daemonProbes++
			writePlaneCommandTestJSON(t, w, map[string]any{"ok": true, "data": map[string]any{"service": map[string]any{"binary": map[string]any{"name": "looperd"}}}})
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(homeDir, "node.pem")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := networkclient.SaveState(networkclient.DefaultStatePath(homeDir), networkclient.LocalState{URL: server.URL, NodeID: "node", NodeName: "fixture", NetworkID: "network", NodeToken: "fixture"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COLLAB_DOCTOR_TEST_TOKEN", "fixture")
	configPath := writeEditableCLIConfigWithPayload(t, map[string]any{
		"server":    map[string]any{"baseUrl": server.URL},
		"roles":     map[string]any{"worker": map[string]any{"agent": map[string]any{"vendor": "pi"}}},
		"projects":  []any{map[string]any{"id": "local", "name": "local", "repo": "acme/local", "repoPath": homeDir, "provider": "plane"}},
		"providers": []any{map[string]any{"id": "plane", "kind": "plane", "baseUrl": server.URL + "/api/v1", "workspace": "fixture", "projectId": projectID, "tokenEnv": "COLLAB_DOCTOR_TEST_TOKEN", "strictDispatch": map[string]any{"enabled": true, "baseUrl": server.URL, "nodeId": "node", "bindingId": "55555555-6666-4777-8888-999999999999", "keyRevision": 1, "privateKeyFile": keyPath}}},
	})
	app := New(Deps{HomeDir: homeDir, HTTPClient: server.Client(), LookPath: configLookPathForTests(), RunCommand: func(context.Context, string, []string, time.Duration) (commandExecutionResult, error) {
		return commandExecutionResult{ExitCode: 0}, nil
	}})
	status := doctorCollabStatus()
	status.Service = "looper collab · feishu-codex-bridge"
	status.PlaneTargets[0].ProjectID = projectID
	serveDoctorCollabStatus(t, dir, status, func() {
		code, stdout, stderr := runAppWithDeps(t, app, []string{"plane", "doctor", "--collab", "--state-dir", dir, "--json", "--config", configPath})
		var result planeDoctorResult
		if err := json.Unmarshal([]byte(stdout), &result); err != nil {
			t.Fatal(err)
		}
		if code != 0 || !result.Ready || daemonProbes != 0 || !strings.Contains(stdout, "codex") {
			t.Fatalf("exit=%d result=%+v stderr=%s daemon probes=%d", code, result, stderr, daemonProbes)
		}
	})
	code, stdout, _ := runAppWithDeps(t, app, []string{"plane", "doctor", "--json", "--config", configPath})
	if code == 0 || daemonProbes != 1 || !strings.Contains(stdout, `"name": "daemon"`) {
		t.Fatalf("legacy doctor changed: exit=%d output=%s probes=%d", code, stdout, daemonProbes)
	}
	if code, _, _ := runAppWithDeps(t, app, []string{"plane", "doctor", "--state-dir", dir, "--config", configPath}); code == 0 {
		t.Fatal("accepted --state-dir without --collab")
	}
}

func doctorCollabStatus() collab.Status {
	return collab.Status{Service: "looper collab", State: "running", Network: networkclient.Status{Configured: true, CloudReachable: true, NodeID: "node"}, PlaneTargets: []collab.PlaneTarget{{Workspace: "fixture", ProjectID: "project", Repo: "acme/local"}}}
}

func serveDoctorCollabStatus(t *testing.T, dir string, status collab.Status, check func()) {
	t.Helper()
	listener, err := net.Listen("unix", collab.SocketPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/status" {
			t.Errorf("unexpected collab request: %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(status)
	})}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()
	check()
}
