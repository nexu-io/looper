package cliapp

import (
	"context"
	"encoding/json"
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
