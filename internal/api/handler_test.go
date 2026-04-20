package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/powerformer/looper/internal/bootstrap"
	"github.com/powerformer/looper/internal/config"
	looperdruntime "github.com/powerformer/looper/internal/runtime"
	"github.com/powerformer/looper/internal/storage"
)

func TestHandlerHealthzSuccessAndRequestIDEcho(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	h := NewHandler(Context{Config: cfg, Runtime: rt})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/healthz", nil)
	req.Header.Set("x-request-id", "fixture-request-id")
	recorder := httptest.NewRecorder()

	h.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if got := recorder.Header().Get("content-type"); got != "application/json; charset=utf-8" {
		t.Fatalf("content-type = %q, want application/json; charset=utf-8", got)
	}

	body := parseJSONMap(t, recorder.Body.Bytes())
	assertEqual(t, body["ok"], true)
	assertEqual(t, body["requestId"], "fixture-request-id")

	data := body["data"].(map[string]any)
	assertEqual(t, data["healthy"], true)
	storageInfo := data["storage"].(map[string]any)
	assertEqual(t, storageInfo["ok"], true)
	assertEqual(t, storageInfo["mode"], "sqlite")
	if _, ok := storageInfo["dbPath"].(string); !ok {
		t.Fatalf("data.storage.dbPath missing/invalid: %#v", storageInfo["dbPath"])
	}
}

func TestHandlerStatusSuccessContainsExpectedSections(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	seedStatusData(t, rt)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
	req.Header.Set("x-request-id", "fixture-request-id")
	recorder := httptest.NewRecorder()

	NewHandler(Context{Config: cfg, Runtime: rt}).ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	body := parseJSONMap(t, recorder.Body.Bytes())
	assertEqual(t, body["ok"], true)
	assertEqual(t, body["requestId"], "fixture-request-id")

	data := body["data"].(map[string]any)
	service := data["service"].(map[string]any)
	storageInfo := data["storage"].(map[string]any)
	scheduler := data["scheduler"].(map[string]any)
	loops := data["loops"].(map[string]any)

	assertEqual(t, service["healthy"], true)
	assertEqual(t, service["daemonMode"], "foreground")
	assertEqual(t, storageInfo["healthy"], true)
	assertEqual(t, scheduler["queuedItems"], float64(1))
	assertEqual(t, scheduler["runningItems"], float64(0))
	assertEqual(t, scheduler["totalRuns"], float64(1))
	assertEqual(t, scheduler["activeRuns"], float64(1))

	reviewer := loops["reviewer"].(map[string]any)
	assertEqual(t, reviewer["running"], float64(1))
}

func TestHandlerAuthMisconfigured(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	cfg.Server.AuthMode = config.AuthModeLocalToken
	cfg.Server.LocalToken = nil

	req := httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
	req.Header.Set("x-request-id", "error-request-id")
	recorder := httptest.NewRecorder()

	NewHandler(Context{Config: cfg, Runtime: rt}).ServeHTTP(recorder, req)

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", recorder.Code)
	}
	body := parseJSONMap(t, recorder.Body.Bytes())
	errMap := body["error"].(map[string]any)
	assertEqual(t, errMap["code"], "AUTH_MISCONFIGURED")
	assertEqual(t, errMap["message"], "Local token auth is enabled but no token is configured")
	assertEqual(t, body["requestId"], "error-request-id")
}

func TestHandlerUnauthorized(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	token := "secret-token"
	cfg.Server.AuthMode = config.AuthModeLocalToken
	cfg.Server.LocalToken = &token

	req := httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
	req.Header.Set("x-request-id", "error-request-id")
	recorder := httptest.NewRecorder()

	NewHandler(Context{Config: cfg, Runtime: rt}).ServeHTTP(recorder, req)

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", recorder.Code)
	}
	body := parseJSONMap(t, recorder.Body.Bytes())
	errMap := body["error"].(map[string]any)
	assertEqual(t, errMap["code"], "UNAUTHORIZED")
	assertEqual(t, errMap["message"], "Authorization token is required")
}

func TestHandlerRouteAndMethodErrors(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	h := NewHandler(Context{Config: cfg, Runtime: rt})

	methodReq := httptest.NewRequest(http.MethodDelete, "/api/v1/status", nil)
	methodReq.Header.Set("x-request-id", "error-request-id")
	methodRecorder := httptest.NewRecorder()
	h.ServeHTTP(methodRecorder, methodReq)
	if methodRecorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("method status = %d, want 405", methodRecorder.Code)
	}
	methodBody := parseJSONMap(t, methodRecorder.Body.Bytes())
	assertEqual(t, methodBody["requestId"], "error-request-id")
	assertEqual(t, methodBody["error"].(map[string]any)["code"], "METHOD_NOT_ALLOWED")

	routeReq := httptest.NewRequest(http.MethodGet, "/api/v1/does-not-exist", nil)
	routeRecorder := httptest.NewRecorder()
	h.ServeHTTP(routeRecorder, routeReq)
	if routeRecorder.Code != http.StatusNotFound {
		t.Fatalf("route status = %d, want 404", routeRecorder.Code)
	}
	routeBody := parseJSONMap(t, routeRecorder.Body.Bytes())
	assertEqual(t, routeBody["error"].(map[string]any)["code"], "ROUTE_NOT_FOUND")
	if got := routeBody["requestId"].(string); got == "" {
		t.Fatal("generated requestId is empty")
	}
}

func TestHandlerHealthzReturnsUnhealthyEnvelopeWhenStorageCheckFails(t *testing.T) {
	fixture := newTestFixture(t)
	if err := fixture.runtime.Services().Coordinator.Close(); err != nil {
		t.Fatalf("Coordinator.Close() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/healthz", nil)
	recorder := httptest.NewRecorder()

	NewHandler(Context{
		Config:          fixture.config,
		Runtime:         fixture.runtime,
		Now:             func() time.Time { return fixture.now },
		RecoverySummary: func() any { return map[string]any{"expiredLocksReleased": 1} },
	}).ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	body := parseJSONMap(t, recorder.Body.Bytes())
	data := body["data"].(map[string]any)
	assertEqual(t, data["healthy"], false)
	storageInfo := data["storage"].(map[string]any)
	assertEqual(t, storageInfo["ok"], false)
	if details, ok := storageInfo["details"].(string); !ok || strings.TrimSpace(details) == "" {
		t.Fatalf("storage details = %#v, want non-empty string", storageInfo["details"])
	}
}

func TestHandlerMatchesFrozenErrorArtifactForStatusRoutes(t *testing.T) {
	rt, cfg := startTestRuntime(t)

	token := "secret-token"
	authCfg := cfg
	authCfg.Server.AuthMode = config.AuthModeLocalToken
	authCfg.Server.LocalToken = &token

	misconfiguredCfg := cfg
	misconfiguredCfg.Server.AuthMode = config.AuthModeLocalToken
	misconfiguredCfg.Server.LocalToken = nil

	var artifact struct {
		Cases []errorArtifactCase `json:"cases"`
	}

	artifactPath := filepath.Join("..", "..", "specs", "2026-04-17-go-port-plan", "artifacts", "daemon-http.errors.compat.json")
	raw, err := os.ReadFile(artifactPath)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", artifactPath, err)
	}
	if err := json.Unmarshal(raw, &artifact); err != nil {
		t.Fatalf("json.Unmarshal(%s) error = %v", artifactPath, err)
	}

	tests := []struct {
		caseID  string
		method  string
		path    string
		headers map[string]string
		cfg     config.Config
	}{
		{caseID: "auth-misconfigured", method: http.MethodGet, path: "/api/v1/status", headers: map[string]string{"x-request-id": "error-request-id"}, cfg: misconfiguredCfg},
		{caseID: "unauthorized", method: http.MethodGet, path: "/api/v1/status", headers: map[string]string{"x-request-id": "error-request-id"}, cfg: authCfg},
		{caseID: "method-not-allowed", method: http.MethodDelete, path: "/api/v1/status", headers: map[string]string{"x-request-id": "error-request-id"}, cfg: cfg},
	}

	for _, tt := range tests {
		t.Run(tt.caseID, func(t *testing.T) {
			h := NewHandler(Context{Config: tt.cfg, Runtime: rt})
			req := httptest.NewRequest(tt.method, tt.path, nil)
			for key, value := range tt.headers {
				req.Header.Set(key, value)
			}

			recorder := httptest.NewRecorder()
			h.ServeHTTP(recorder, req)

			want := findArtifactCase(t, artifact.Cases, tt.caseID)
			if recorder.Code != want.ExpectedStatus {
				t.Fatalf("status = %d, want %d", recorder.Code, want.ExpectedStatus)
			}
			body := parseJSONMap(t, recorder.Body.Bytes())
			errorMap := body["error"].(map[string]any)
			assertEqual(t, errorMap["code"], want.Body.Error.Code)
			assertEqual(t, errorMap["message"], want.Body.Error.Message)
		})
	}
}

func TestHandlerMatchesFrozenSuccessArtifactsForStatusRoutes(t *testing.T) {
	fixture := newTestFixture(t)
	seedStatusData(t, fixture.runtime)

	routes := loadResponseArtifact(t)
	h := NewHandler(Context{
		Config:  fixture.config,
		Runtime: fixture.runtime,
		Now:     func() time.Time { return fixture.now },
		RecoverySummary: func() any {
			return map[string]any{"expiredLocksReleased": 1}
		},
	})

	for _, routeID := range []string{"healthz.get", "status.get"} {
		t.Run(routeID, func(t *testing.T) {
			path := "/api/v1/healthz"
			if routeID == "status.get" {
				path = "/api/v1/status"
			}

			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.Header.Set("x-request-id", "fixture-request-id")
			recorder := httptest.NewRecorder()
			h.ServeHTTP(recorder, req)

			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", recorder.Code)
			}

			actual := parseJSONValue(t, recorder.Body.Bytes())
			normalized := normalizeResponseValue(actual, fixture.rootDir)
			want := findResponseArtifactRoute(t, routes, routeID)

			if !responseFixtureMatches(normalized, want.Body) {
				actualJSON, _ := json.MarshalIndent(normalized, "", "  ")
				wantJSON, _ := json.MarshalIndent(want.Body, "", "  ")
				t.Fatalf("normalized body mismatch\nactual=%s\nwant=%s", actualJSON, wantJSON)
			}
		})
	}
}

func TestServerServesStatusEndpoint(t *testing.T) {
	fixture := newTestFixture(t)
	seedStatusData(t, fixture.runtime)
	fixture.config.Server.Port = freeTCPPort(t)

	server := NewServer(fixture.config, NewHandler(Context{
		Config:  fixture.config,
		Runtime: fixture.runtime,
		Now:     func() time.Time { return fixture.now },
		RecoverySummary: func() any {
			return map[string]any{"expiredLocksReleased": 1}
		},
	}))
	if err := server.Start(); err != nil {
		t.Fatalf("Server.Start() error = %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := server.Stop(ctx); err != nil {
			t.Fatalf("Server.Stop() error = %v", err)
		}
	}()

	response, err := (&http.Client{Timeout: time.Second}).Get(fmt.Sprintf("http://%s/api/v1/status", server.Addr().String()))
	if err != nil {
		t.Fatalf("GET /api/v1/status error = %v", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	if got := response.Header.Get("content-type"); got != "application/json; charset=utf-8" {
		t.Fatalf("content-type = %q, want application/json; charset=utf-8", got)
	}
	if _, err := io.ReadAll(response.Body); err != nil {
		t.Fatalf("ReadAll(response.Body) error = %v", err)
	}
}

type testFixture struct {
	rootDir string
	now     time.Time
	config  config.Config
	runtime *looperdruntime.Runtime
}

func newTestFixture(t *testing.T) testFixture {
	t.Helper()

	rootDir := t.TempDir()
	cfg, err := config.DefaultConfig(rootDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}

	backupDir := filepath.Join(rootDir, "backups")
	cfg.Storage.DBPath = filepath.Join(rootDir, "state", "looper.sqlite")
	cfg.Storage.BackupDir = &backupDir
	cfg.Daemon.LogDir = filepath.Join(rootDir, "logs")
	cfg.Daemon.WorkingDirectory = rootDir
	cfg.Tools.BunPath = stringPtr("/usr/bin/bun")
	cfg.Tools.GitPath = stringPtr("/usr/bin/git")
	cfg.Tools.GHPath = stringPtr("/usr/bin/gh")
	cfg.Tools.OsascriptPath = stringPtr("/usr/bin/osascript")
	vendor := config.AgentVendorOpenCode
	cfg.Agent.Vendor = &vendor

	now := time.Date(2026, time.April, 11, 12, 0, 0, 0, time.UTC)
	rt := looperdruntime.New(looperdruntime.Options{
		Config: cfg,
		Logger: noopLogger{},
		Now: func() time.Time {
			return now
		},
	})

	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Runtime.Start() error = %v", err)
	}

	t.Cleanup(func() {
		rt.Stop("test cleanup")
	})

	return testFixture{rootDir: rootDir, now: now, config: cfg, runtime: rt}
}

func startTestRuntime(t *testing.T) (*looperdruntime.Runtime, config.Config) {
	fixture := newTestFixture(t)
	return fixture.runtime, fixture.config
}

func seedStatusData(t *testing.T, rt *looperdruntime.Runtime) {
	t.Helper()

	services := rt.Services()
	nowISO := "2026-04-11T12:00:00.000Z"
	projectID := "project_1"
	loopID := "loop_1"

	if err := services.Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID:        projectID,
		Name:      "Looper",
		RepoPath:  "/tmp/repos/looper",
		Archived:  false,
		CreatedAt: nowISO,
		UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	if err := services.Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{
		ID:         loopID,
		Seq:        1,
		ProjectID:  projectID,
		Type:       "reviewer",
		TargetType: "pull_request",
		TargetID:   stringPtr("pr:acme/looper:42"),
		Repo:       stringPtr("acme/looper"),
		PRNumber:   int64Ptr(42),
		Status:     "running",
		CreatedAt:  nowISO,
		UpdatedAt:  nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	if err := services.Repositories.Runs.Upsert(context.Background(), storage.RunRecord{
		ID:        "run_1",
		LoopID:    loopID,
		Status:    "running",
		StartedAt: nowISO,
		CreatedAt: nowISO,
		UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}

	if err := services.Repositories.Queue.Upsert(context.Background(), storage.QueueItemRecord{
		ID:          "queue_1",
		ProjectID:   &projectID,
		LoopID:      &loopID,
		Type:        "reviewer",
		TargetType:  "pull_request",
		TargetID:    "pr:acme/looper:42",
		Repo:        stringPtr("acme/looper"),
		PRNumber:    int64Ptr(42),
		DedupeKey:   "reviewer:acme/looper:42",
		Priority:    2,
		Status:      "queued",
		AvailableAt: nowISO,
		Attempts:    0,
		MaxAttempts: 3,
		CreatedAt:   nowISO,
		UpdatedAt:   nowISO,
	}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}
}

func parseJSONMap(t *testing.T, body []byte) map[string]any {
	t.Helper()

	var value map[string]any
	if err := json.Unmarshal(body, &value); err != nil {
		t.Fatalf("json.Unmarshal() error = %v\nbody=%s", err, string(body))
	}

	return value
}

func parseJSONValue(t *testing.T, body []byte) any {
	t.Helper()

	var value any
	if err := json.Unmarshal(body, &value); err != nil {
		t.Fatalf("json.Unmarshal() error = %v\nbody=%s", err, string(body))
	}

	return value
}

func loadResponseArtifact(t *testing.T) []responseArtifactRoute {
	t.Helper()

	artifactPath := filepath.Join("..", "..", "specs", "2026-04-17-go-port-plan", "artifacts", "daemon-http.responses.compat.json")
	raw, err := os.ReadFile(artifactPath)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", artifactPath, err)
	}

	var artifact struct {
		Routes []responseArtifactRoute `json:"routes"`
	}
	if err := json.Unmarshal(raw, &artifact); err != nil {
		t.Fatalf("json.Unmarshal(%s) error = %v", artifactPath, err)
	}

	return artifact.Routes
}

func findResponseArtifactRoute(t *testing.T, routes []responseArtifactRoute, routeID string) responseArtifactRoute {
	t.Helper()
	for _, route := range routes {
		if route.ID == routeID {
			return route
		}
	}

	t.Fatalf("response artifact route %q not found", routeID)
	return responseArtifactRoute{}
}

func normalizeResponseValue(value any, rootDir string) any {
	switch typed := value.(type) {
	case map[string]any:
		normalized := make(map[string]any, len(typed))
		for key, item := range typed {
			normalized[key] = normalizeResponseValue(item, rootDir)
		}
		return normalized
	case []any:
		normalized := make([]any, len(typed))
		for i, item := range typed {
			normalized[i] = normalizeResponseValue(item, rootDir)
		}
		return normalized
	case string:
		homeDir, _ := os.UserHomeDir()
		return strings.ReplaceAll(strings.ReplaceAll(typed, rootDir, "<tmp-root>"), homeDir, "<home>")
	default:
		return value
	}
}

func responseFixtureMatches(actual, expected any) bool {
	switch want := expected.(type) {
	case map[string]any:
		got, ok := actual.(map[string]any)
		if !ok || len(got) != len(want) {
			return false
		}
		for key, wantValue := range want {
			gotValue, ok := got[key]
			if !ok || !responseFixtureMatches(gotValue, wantValue) {
				return false
			}
		}
		return true
	case []any:
		got, ok := actual.([]any)
		if !ok || len(got) != len(want) {
			return false
		}
		for i := range want {
			if !responseFixtureMatches(got[i], want[i]) {
				return false
			}
		}
		return true
	case string:
		switch want {
		case "<generated-timestamp>", "<current-target>":
			got, ok := actual.(string)
			return ok && strings.TrimSpace(got) != ""
		case "<artifact-name>":
			if actual == nil {
				return true
			}
			got, ok := actual.(string)
			return ok && strings.TrimSpace(got) != ""
		default:
			got, ok := actual.(string)
			return ok && got == want
		}
	default:
		return reflect.DeepEqual(actual, expected)
	}
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen(:0) error = %v", err)
	}
	defer listener.Close()

	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener addr type = %T, want *net.TCPAddr", listener.Addr())
	}

	return addr.Port
}

func assertEqual(t *testing.T, got, want any) {
	t.Helper()
	if got != want {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func findArtifactCase(t *testing.T, cases []errorArtifactCase, caseID string) errorArtifactCase {
	t.Helper()
	for _, item := range cases {
		if item.ID == caseID {
			return item
		}
	}
	t.Fatalf("artifact case %q not found", caseID)
	return errorArtifactCase{}
}

func stringPtr(value string) *string {
	return &value
}

func int64Ptr(value int64) *int64 {
	return &value
}

type noopLogger struct{}

func (noopLogger) Debug(string, map[string]any) {}
func (noopLogger) Info(string, map[string]any)  {}
func (noopLogger) Warn(string, map[string]any)  {}
func (noopLogger) Error(string, map[string]any) {}

var _ bootstrap.Logger = noopLogger{}

type errorArtifactCase struct {
	ID             string `json:"id"`
	ExpectedStatus int    `json:"expectedStatus"`
	Body           struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	} `json:"body"`
}

type responseArtifactRoute struct {
	ID   string `json:"id"`
	Body any    `json:"body"`
}
