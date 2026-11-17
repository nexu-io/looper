package cliapp

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"github.com/nexu-io/looper/internal/planestrict"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/collab"
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
			found = check.Status == "passed"
		}
	}
	if !found || reads != 1 {
		t.Fatalf("collab not checked: %+v reads=%d", result.Checks, reads)
	}
}

func TestPlaneDoctorPersonalDeviceNeedsNoRepositoryTokenOrDaemon(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "personal-doctor-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	home := filepath.Join(dir, "home")
	if err := os.MkdirAll(filepath.Join(home, ".looper"), 0700); err != nil {
		t.Fatal(err)
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, "device.pem")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded}), 0600); err != nil {
		t.Fatal(err)
	}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/looper/devices/projects/" || r.Header.Get("Looper-Signature") == "" || r.Header.Get("X-API-Key") != "" {
			t.Errorf("wrong personal request: %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"state":"active","projects":[{"workspace":"team","id":"33333333-4444-4555-8666-777777777777","name":"Test"}]}`))
	}))
	defer api.Close()
	connection := planestrict.DeviceConnection{BaseURL: api.URL, DeviceID: "55555555-6666-4777-8888-999999999999", NodeID: "personal-node", Workspace: "team", ProjectID: "33333333-4444-4555-8666-777777777777", PrivateKeyFile: keyPath}
	if err := connection.Save(filepath.Join(home, ".looper", "plane-device.json")); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", collab.SocketPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"network":{"configured":true,"cloudReachable":true,"nodeId":"personal-node"}}`))
	})}
	go server.Serve(listener)
	defer server.Close()
	configPath := writeEditableCLIConfigWithPayload(t, map[string]any{"agent": map[string]any{"vendor": "codex"}, "providers": []any{}, "projects": []any{}, "notifications": map[string]any{"osascript": map[string]any{"enabled": false}}})
	app := New(Deps{HomeDir: home, LookPath: configLookPathForTests(), RunCommand: func(_ context.Context, command string, args []string, _ time.Duration) (commandExecutionResult, error) {
		if filepath.Base(command) == "plane" {
			t.Error("personal doctor invoked Plane CLI")
		}
		return commandExecutionResult{ExitCode: 0}, nil
	}})
	_, stdout, stderr := runAppWithDeps(t, app, []string{"--config", configPath, "plane", "doctor", "--state-dir", dir, "--json"})
	var result planeDoctorResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("%s %s: %v", stdout, stderr, err)
	}
	if !result.Ready {
		t.Fatalf("personal device incorrectly blocked: %+v", result.Checks)
	}
	for _, c := range result.Checks {
		switch c.Name {
		case "repository", "plane-api-key", "plane-provider", "daemon", "plane-cli":
			t.Fatalf("legacy dependency leaked into personal doctor: %+v", c)
		}
	}
}
