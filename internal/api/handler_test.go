package api

import (
	"bytes"
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
	"github.com/powerformer/looper/internal/projects"
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

func TestHandlerConfigSuccessContainsExpectedSections(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/config", nil)
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
	server := data["server"].(map[string]any)
	storageInfo := data["storage"].(map[string]any)
	daemon := data["daemon"].(map[string]any)

	assertEqual(t, server["host"], cfg.Server.Host)
	assertEqual(t, server["port"], float64(cfg.Server.Port))
	assertEqual(t, server["authMode"], string(cfg.Server.AuthMode))
	assertEqual(t, server["localTokenConfigured"], false)
	assertEqual(t, storageInfo["mode"], cfg.Storage.Mode)
	assertEqual(t, daemon["mode"], string(cfg.Daemon.Mode))
	assertEqual(t, daemon["workingDirectory"], cfg.Daemon.WorkingDirectory)
	if _, ok := daemon["shutdownTimeoutMs"]; ok {
		t.Fatalf("daemon.shutdownTimeoutMs should be omitted from config response: %#v", daemon)
	}
	if _, ok := server["localToken"]; ok {
		t.Fatalf("server.localToken should be omitted from config response: %#v", server)
	}
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

func TestHandlerMatchesFrozenSuccessArtifactsForCoreRoutes(t *testing.T) {
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

	for _, routeID := range []string{"healthz.get", "status.get", "config.get"} {
		t.Run(routeID, func(t *testing.T) {
			path := "/api/v1/healthz"
			switch routeID {
			case "status.get":
				path = "/api/v1/status"
			case "config.get":
				path = "/api/v1/config"
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

func TestHandlerProjectsListRouteSuccess(t *testing.T) {
	fixture := newTestFixture(t)
	nowISO := fixture.now.UTC().Format(javaScriptISOString)
	metadata := `{"repo":"acme/looper","worktreeRoot":null,"source":"api"}`
	baseBranch := "main"

	err := fixture.runtime.Services().Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID:           "project_1",
		Name:         "Looper",
		RepoPath:     "/tmp/looper",
		BaseBranch:   &baseBranch,
		Archived:     false,
		MetadataJSON: &metadata,
		CreatedAt:    nowISO,
		UpdatedAt:    nowISO,
	})
	if err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	req.Header.Set("x-request-id", "fixture-request-id")
	recorder := httptest.NewRecorder()

	NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime}).ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}

	body := parseJSONMap(t, recorder.Body.Bytes())
	assertEqual(t, body["ok"], true)
	assertEqual(t, body["requestId"], "fixture-request-id")
	items := body["data"].(map[string]any)["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("len(items) = %d, want 1", len(items))
	}

	project := items[0].(map[string]any)
	assertEqual(t, project["id"], "project_1")
	assertEqual(t, project["name"], "Looper")
	assertEqual(t, project["repoPath"], "/tmp/looper")
	assertEqual(t, project["baseBranch"], "main")
	assertEqual(t, project["archived"], false)
	assertEqual(t, project["repo"], "acme/looper")
	if project["worktreeRoot"] != nil {
		t.Fatalf("worktreeRoot = %#v, want nil", project["worktreeRoot"])
	}
	assertEqual(t, project["createdAt"], nowISO)
	assertEqual(t, project["updatedAt"], nowISO)
}

func TestHandlerProjectsCreateRouteSuccessDerivesDefaults(t *testing.T) {
	fixture := newTestFixture(t)
	reqBody := []byte(`{"repoPath":"C:\\\\tmp/repos/Looper Repo"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects", bytes.NewReader(reqBody))
	req.Header.Set("x-request-id", "fixture-request-id")
	recorder := httptest.NewRecorder()

	NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime}).ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	body := parseJSONMap(t, recorder.Body.Bytes())
	assertEqual(t, body["ok"], true)
	assertEqual(t, body["requestId"], "fixture-request-id")
	data := body["data"].(map[string]any)
	assertEqual(t, data["id"], "looper-repo")
	assertEqual(t, data["name"], "looper-repo")
	assertEqual(t, data["baseBranch"], fixture.config.Defaults.BaseBranch)
	assertEqual(t, data["archived"], false)
	if data["repo"] != nil {
		t.Fatalf("repo = %#v, want nil", data["repo"])
	}
	if data["worktreeRoot"] != nil {
		t.Fatalf("worktreeRoot = %#v, want nil", data["worktreeRoot"])
	}
	assertEqual(t, data["discoveredPullRequests"], float64(0))
	assertEqual(t, data["discoveredWorktrees"], float64(0))
	warnings, ok := data["warnings"].([]any)
	if !ok || len(warnings) != 0 {
		t.Fatalf("warnings = %#v, want empty array", data["warnings"])
	}
}

func TestHandlerProjectsRouteErrorsMatchArtifactCases(t *testing.T) {
	fixture := newTestFixture(t)
	projectService := fixture.runtime.Services().Projects

	artifactPath := filepath.Join("..", "..", "specs", "2026-04-17-go-port-plan", "artifacts", "daemon-http.errors.compat.json")
	raw, err := os.ReadFile(artifactPath)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", artifactPath, err)
	}
	var artifact struct {
		Cases []errorArtifactCase `json:"cases"`
	}
	if err := json.Unmarshal(raw, &artifact); err != nil {
		t.Fatalf("json.Unmarshal(%s) error = %v", artifactPath, err)
	}

	stubUnavailableRuntime := fixedRuntimeState{services: looperdruntime.Services{Projects: nil}}
	tests := []struct {
		caseID  string
		runtime RuntimeState
		body    string
		wantID  bool
		custom  *errorArtifactCase
	}{
		{
			caseID:  "projects-unavailable",
			runtime: stubUnavailableRuntime,
			body:    `{"repoPath":"/tmp/repos/looper","name":"Looper"}`,
			wantID:  true,
		},
		{
			caseID:  "invalid-project-id",
			runtime: fixture.runtime,
			body:    `{"repoPath":"/tmp/repos/looper","id":"../../tmp","name":"Looper"}`,
			wantID:  true,
		},
		{
			caseID:  "internal-error",
			runtime: fixedRuntimeState{services: looperdruntime.Services{Projects: &projects.Service{Repos: nil}}},
			body:    `{"repoPath":"/tmp/repos/looper","id":"looper","name":"Looper"}`,
			wantID:  false,
			custom: &errorArtifactCase{ExpectedStatus: http.StatusInternalServerError, Body: struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}{Error: struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			}{Code: "INTERNAL_ERROR", Message: "projects repository is not configured"}}},
		},
	}

	seedConflictProject(t, projectService)

	for _, tt := range tests {
		t.Run(tt.caseID, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/projects", bytes.NewReader([]byte(tt.body)))
			if tt.wantID {
				req.Header.Set("x-request-id", "error-request-id")
			}
			recorder := httptest.NewRecorder()
			NewHandler(Context{Config: fixture.config, Runtime: tt.runtime}).ServeHTTP(recorder, req)

			want := findArtifactCase(t, artifact.Cases, tt.caseID)
			if tt.custom != nil {
				want = *tt.custom
			}
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

func TestHandlerProjectsCreateRouteMapsProjectIDConflict(t *testing.T) {
	fixture := newTestFixture(t)
	h := NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects", bytes.NewReader([]byte(`{"repoPath":"/tmp/repos/looper","id":"looper"}`)))

	_, err := h.buildCreateProjectResponse(req, fakeProjectService{
		addProject: func(context.Context, projects.AddInput) (projects.AddResult, error) {
			return projects.AddResult{}, projects.ProjectIDCollisionError{ProjectID: "looper"}
		},
	})

	if err == nil {
		t.Fatal("buildCreateProjectResponse() error = nil, want conflict error")
	}
	typed, ok := err.(apiError)
	if !ok {
		t.Fatalf("error type = %T, want apiError", err)
	}
	assertEqual(t, string(typed.code), "PROJECT_ID_CONFLICT")
	assertEqual(t, typed.status, http.StatusConflict)
	assertEqual(t, typed.message, "Derived project id collides with an existing explicit project: looper")
}

func TestHandlerLoopRoutesMatchFrozenSuccessArtifacts(t *testing.T) {
	routes := loadResponseArtifact(t)
	requestArtifact := loadRequestArtifact(t)

	tests := []struct {
		routeID string
		method  string
		path    string
		body    string
		prepare func(*testing.T, *Handler)
	}{{routeID: "loops.list", method: http.MethodGet, path: "/api/v1/loops"}, {routeID: "loop.detail", method: http.MethodGet, path: "/api/v1/loops/loop_1"}, {routeID: "loop.logs", method: http.MethodGet, path: "/api/v1/loops/loop_1/logs"}, {routeID: "loop.start", method: http.MethodPost, path: "/api/v1/loops/loop_1/start"}, {routeID: "loop.pause", method: http.MethodPost, path: "/api/v1/loops/loop_1/pause", prepare: func(t *testing.T, h *Handler) {
		t.Helper()
		startReq := httptest.NewRequest(http.MethodPost, "/api/v1/loops/loop_1/start", nil)
		startRecorder := httptest.NewRecorder()
		h.ServeHTTP(startRecorder, startReq)
		if startRecorder.Code != http.StatusOK {
			t.Fatalf("pre-start status = %d, want 200", startRecorder.Code)
		}
	}}, {routeID: "loops.create", method: http.MethodPost, path: "/api/v1/loops", body: marshalArtifactRequestBody(t, requestArtifact, "loops.create")}}

	for _, tt := range tests {
		t.Run(tt.routeID, func(t *testing.T) {
			fixture := newTestFixture(t)
			seedLoopRouteData(t, fixture.runtime)
			h := NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime, Now: func() time.Time { return fixture.now.Add(time.Minute) }})
			if tt.prepare != nil {
				tt.prepare(t, h)
			}

			var body io.Reader
			if tt.body != "" {
				body = bytes.NewReader([]byte(tt.body))
			}
			req := httptest.NewRequest(tt.method, tt.path, body)
			req.Header.Set("x-request-id", "fixture-request-id")
			if tt.body != "" {
				req.Header.Set("content-type", "application/json")
			}
			recorder := httptest.NewRecorder()
			h.ServeHTTP(recorder, req)

			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", recorder.Code)
			}

			actual := normalizeResponseValue(parseJSONValue(t, recorder.Body.Bytes()), fixture.rootDir)
			want := findResponseArtifactRoute(t, routes, tt.routeID)
			if !responseFixtureMatches(actual, want.Body) {
				actualJSON, _ := json.MarshalIndent(actual, "", "  ")
				wantJSON, _ := json.MarshalIndent(want.Body, "", "  ")
				t.Fatalf("normalized body mismatch\nactual=%s\nwant=%s", actualJSON, wantJSON)
			}
		})
	}
}

func TestHandlerLoopRouteErrorsMatchArtifactCases(t *testing.T) {
	fixture := newTestFixture(t)
	seedLoopRouteData(t, fixture.runtime)

	artifactPath := filepath.Join("..", "..", "specs", "2026-04-17-go-port-plan", "artifacts", "daemon-http.errors.compat.json")
	raw, err := os.ReadFile(artifactPath)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", artifactPath, err)
	}
	var artifact struct {
		Cases []errorArtifactCase `json:"cases"`
	}
	if err := json.Unmarshal(raw, &artifact); err != nil {
		t.Fatalf("json.Unmarshal(%s) error = %v", artifactPath, err)
	}

	tests := []struct {
		caseID string
		method string
		path   string
		body   string
	}{{caseID: "loop-not-found", method: http.MethodGet, path: "/api/v1/loops/missing-loop"}, {caseID: "project-not-found", method: http.MethodPost, path: "/api/v1/loops", body: `{"projectId":"missing-project","type":"worker","targetType":"project","targetId":"missing-project"}`}, {caseID: "loop-conflict", method: http.MethodPost, path: "/api/v1/loops", body: `{"projectId":"project_1","type":"reviewer","targetType":"pull_request","repo":"acme/looper","prNumber":42}`}}

	h := NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime, Now: func() time.Time { return fixture.now.Add(time.Minute) }})
	for _, tt := range tests {
		t.Run(tt.caseID, func(t *testing.T) {
			var body io.Reader
			if tt.body != "" {
				body = bytes.NewReader([]byte(tt.body))
			}
			req := httptest.NewRequest(tt.method, tt.path, body)
			req.Header.Set("x-request-id", "error-request-id")
			if tt.body != "" {
				req.Header.Set("content-type", "application/json")
			}
			recorder := httptest.NewRecorder()
			h.ServeHTTP(recorder, req)

			want := findArtifactCase(t, artifact.Cases, tt.caseID)
			if recorder.Code != want.ExpectedStatus {
				t.Fatalf("status = %d, want %d", recorder.Code, want.ExpectedStatus)
			}
			responseBody := parseJSONMap(t, recorder.Body.Bytes())
			errorMap := responseBody["error"].(map[string]any)
			assertEqual(t, errorMap["code"], want.Body.Error.Code)
			assertEqual(t, errorMap["message"], want.Body.Error.Message)
		})
	}
}

func TestHandlerLoopStartRejectsFixerWithoutAgentConfigured(t *testing.T) {
	fixture := newTestFixture(t)
	seedLoopRouteData(t, fixture.runtime)
	nowISO := fixture.now.UTC().Format(javaScriptISOString)
	if err := fixture.runtime.Services().Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{
		ID:         "loop_fixer_no_agent",
		Seq:        4,
		ProjectID:  "project_1",
		Type:       "fixer",
		TargetType: "pull_request",
		TargetID:   stringPtr("pr:acme/looper:99"),
		Repo:       stringPtr("acme/looper"),
		PRNumber:   int64Ptr(99),
		Status:     "paused",
		CreatedAt:  nowISO,
		UpdatedAt:  nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	configWithoutAgent := fixture.config
	configWithoutAgent.Agent.Vendor = nil
	req := httptest.NewRequest(http.MethodPost, "/api/v1/loops/loop_fixer_no_agent/start", nil)
	req.Header.Set("x-request-id", "error-request-id")
	recorder := httptest.NewRecorder()

	NewHandler(Context{Config: configWithoutAgent, Runtime: fixture.runtime, Now: func() time.Time { return fixture.now.Add(time.Minute) }}).ServeHTTP(recorder, req)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}
	body := parseJSONMap(t, recorder.Body.Bytes())
	errorMap := body["error"].(map[string]any)
	assertEqual(t, errorMap["code"], "AGENT_NOT_CONFIGURED")
	assertEqual(t, errorMap["message"], "Cannot start fixer loop without config.agent.vendor")
}

func TestHandlerRunRoutesMatchFrozenSuccessArtifacts(t *testing.T) {
	routes := loadResponseArtifact(t)

	tests := []struct {
		routeID string
		method  string
		path    string
		setup   func(testFixture) Context
	}{
		{routeID: "runs.list", method: http.MethodGet, path: "/api/v1/runs?loopId=loop_1"},
		{routeID: "runs.active.list", method: http.MethodGet, path: "/api/v1/runs/active"},
		{routeID: "runs.active.detail", method: http.MethodGet, path: "/api/v1/runs/active/1"},
		{routeID: "runs.active.stop", method: http.MethodPost, path: "/api/v1/runs/active/1/stop", setup: func(fixture testFixture) Context {
			return Context{
				Config:  fixture.config,
				Runtime: fixture.runtime,
				Now:     func() time.Time { return fixture.now },
				StopLoop: func(_ context.Context, loopID, _ string) (any, error) {
					return stopLoopResponse{Stopped: true, LoopID: loopID}, nil
				},
			}
		}},
	}

	for _, tt := range tests {
		t.Run(tt.routeID, func(t *testing.T) {
			fixture := newTestFixture(t)
			seedRunRouteData(t, fixture.runtime)
			ctx := Context{Config: fixture.config, Runtime: fixture.runtime, Now: func() time.Time { return fixture.now }}
			if tt.setup != nil {
				ctx = tt.setup(fixture)
			}
			h := NewHandler(ctx)

			req := httptest.NewRequest(tt.method, tt.path, nil)
			req.Header.Set("x-request-id", "fixture-request-id")
			recorder := httptest.NewRecorder()
			h.ServeHTTP(recorder, req)

			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", recorder.Code)
			}

			actual := normalizeResponseValue(parseJSONValue(t, recorder.Body.Bytes()), fixture.rootDir)
			want := findResponseArtifactRoute(t, routes, tt.routeID)
			if !responseFixtureMatches(actual, want.Body) {
				actualJSON, _ := json.MarshalIndent(actual, "", "  ")
				wantJSON, _ := json.MarshalIndent(want.Body, "", "  ")
				t.Fatalf("normalized body mismatch\nactual=%s\nwant=%s", actualJSON, wantJSON)
			}
		})
	}
}

func TestHandlerRunRouteErrorsMatchArtifactCases(t *testing.T) {
	fixture := newTestFixture(t)
	seedRunRouteData(t, fixture.runtime)

	artifactPath := filepath.Join("..", "..", "specs", "2026-04-17-go-port-plan", "artifacts", "daemon-http.errors.compat.json")
	raw, err := os.ReadFile(artifactPath)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", artifactPath, err)
	}
	var artifact struct {
		Cases []errorArtifactCase `json:"cases"`
	}
	if err := json.Unmarshal(raw, &artifact); err != nil {
		t.Fatalf("json.Unmarshal(%s) error = %v", artifactPath, err)
	}

	existingRun, err := fixture.runtime.Services().Repositories.Runs.GetByID(context.Background(), "run_1")
	if err != nil {
		t.Fatalf("Runs.GetByID(run_1) error = %v", err)
	}
	if existingRun == nil {
		t.Fatal("run_1 missing from fixture")
	}
	completedRun := *existingRun
	completedRun.Status = "completed"
	completedAt := fixture.now.Add(10 * time.Minute).UTC().Format(javaScriptISOString)
	completedRun.EndedAt = &completedAt
	completedRun.UpdatedAt = completedAt
	if err := fixture.runtime.Services().Repositories.Runs.Upsert(context.Background(), completedRun); err != nil {
		t.Fatalf("Runs.Upsert(completed) error = %v", err)
	}

	tests := []struct {
		caseID  string
		runtime RuntimeState
		method  string
		path    string
	}{
		{caseID: "runtime-control-unavailable", runtime: fixture.runtime, method: http.MethodPost, path: "/api/v1/runs/active/1/stop"},
		{caseID: "active-run-not-found", runtime: fixture.runtime, method: http.MethodGet, path: "/api/v1/runs/active/1"},
	}

	for _, tt := range tests {
		t.Run(tt.caseID, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, nil)
			req.Header.Set("x-request-id", "error-request-id")
			recorder := httptest.NewRecorder()
			NewHandler(Context{Config: fixture.config, Runtime: tt.runtime}).ServeHTTP(recorder, req)

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

func TestHandlerActiveRunsSupportFiltersAgentsAndWorktrees(t *testing.T) {
	fixture := newTestFixture(t)
	seedRunRouteData(t, fixture.runtime)
	nowISO := fixture.now.UTC().Format(javaScriptISOString)

	if err := fixture.runtime.Services().Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{
		ID:         "loop_worker_1",
		Seq:        5,
		ProjectID:  "project_1",
		Type:       "worker",
		TargetType: "project",
		TargetID:   stringPtr("project_1"),
		Status:     "running",
		CreatedAt:  nowISO,
		UpdatedAt:  nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert(loop_worker_1) error = %v", err)
	}

	checkpoint := `{"worktree":{"id":"wt_1","path":"/tmp/worktrees/loop-1","branch":"feature/loop-1"}}`
	existingRun, err := fixture.runtime.Services().Repositories.Runs.GetByID(context.Background(), "run_1")
	if err != nil {
		t.Fatalf("Runs.GetByID(run_1) error = %v", err)
	}
	if existingRun == nil {
		t.Fatal("run_1 missing from fixture")
	}
	runWithWorktree := *existingRun
	runWithWorktree.CheckpointJSON = &checkpoint
	if err := fixture.runtime.Services().Repositories.Runs.Upsert(context.Background(), runWithWorktree); err != nil {
		t.Fatalf("Runs.Upsert(run_1 worktree) error = %v", err)
	}

	if err := fixture.runtime.Services().Repositories.Runs.Upsert(context.Background(), storage.RunRecord{
		ID:              "run_worker_1",
		LoopID:          "loop_worker_1",
		Status:          "running",
		CurrentStep:     stringPtr("execute"),
		StartedAt:       fixture.now.Add(2 * time.Minute).UTC().Format(javaScriptISOString),
		LastHeartbeatAt: stringPtr(fixture.now.Add(2*time.Minute + 30*time.Second).UTC().Format(javaScriptISOString)),
		CreatedAt:       fixture.now.Add(2 * time.Minute).UTC().Format(javaScriptISOString),
		UpdatedAt:       fixture.now.Add(2*time.Minute + 30*time.Second).UTC().Format(javaScriptISOString),
	}); err != nil {
		t.Fatalf("Runs.Upsert(run_worker_1) error = %v", err)
	}

	if err := fixture.runtime.Services().Repositories.AgentExecutions.Upsert(context.Background(), storage.AgentExecutionRecord{
		ID:              "agent_exec_worker_old",
		ProjectID:       stringPtr("project_1"),
		LoopID:          stringPtr("loop_worker_1"),
		RunID:           stringPtr("run_worker_1"),
		Vendor:          "opencode",
		Status:          "running",
		PID:             int64Ptr(11111),
		HeartbeatCount:  2,
		LastHeartbeatAt: stringPtr(fixture.now.Add(2*time.Minute + 10*time.Second).UTC().Format(javaScriptISOString)),
		StartedAt:       fixture.now.Add(2*time.Minute + 10*time.Second).UTC().Format(javaScriptISOString),
		CreatedAt:       fixture.now.Add(2*time.Minute + 10*time.Second).UTC().Format(javaScriptISOString),
		UpdatedAt:       fixture.now.Add(2*time.Minute + 10*time.Second).UTC().Format(javaScriptISOString),
	}); err != nil {
		t.Fatalf("AgentExecutions.Upsert(old) error = %v", err)
	}
	if err := fixture.runtime.Services().Repositories.AgentExecutions.Upsert(context.Background(), storage.AgentExecutionRecord{
		ID:              "agent_exec_worker_new",
		ProjectID:       stringPtr("project_1"),
		LoopID:          stringPtr("loop_worker_1"),
		RunID:           stringPtr("run_worker_1"),
		Vendor:          "opencode",
		Status:          "running",
		PID:             int64Ptr(22222),
		HeartbeatCount:  5,
		LastHeartbeatAt: stringPtr(fixture.now.Add(2*time.Minute + 20*time.Second).UTC().Format(javaScriptISOString)),
		StartedAt:       fixture.now.Add(2*time.Minute + 20*time.Second).UTC().Format(javaScriptISOString),
		CreatedAt:       fixture.now.Add(2*time.Minute + 20*time.Second).UTC().Format(javaScriptISOString),
		UpdatedAt:       fixture.now.Add(2*time.Minute + 20*time.Second).UTC().Format(javaScriptISOString),
	}); err != nil {
		t.Fatalf("AgentExecutions.Upsert(new) error = %v", err)
	}

	h := NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime})

	workerReq := httptest.NewRequest(http.MethodGet, "/api/v1/runs/active?type=worker", nil)
	workerRecorder := httptest.NewRecorder()
	h.ServeHTTP(workerRecorder, workerReq)
	if workerRecorder.Code != http.StatusOK {
		t.Fatalf("worker filter status = %d, want 200", workerRecorder.Code)
	}
	workerBody := parseJSONMap(t, workerRecorder.Body.Bytes())
	items := workerBody["data"].(map[string]any)["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("len(worker items) = %d, want 2", len(items))
	}
	first := items[0].(map[string]any)
	assertEqual(t, first["runId"], "run_worker_1")
	assertEqual(t, first["type"], "worker")
	target := first["target"].(map[string]any)
	assertEqual(t, target["label"], "Looper")
	agent := first["agent"].(map[string]any)
	assertEqual(t, agent["executionId"], "agent_exec_worker_new")
	assertEqual(t, agent["activeCount"], float64(2))

	detailReq := httptest.NewRequest(http.MethodGet, "/api/v1/runs/active/1", nil)
	detailRecorder := httptest.NewRecorder()
	h.ServeHTTP(detailRecorder, detailReq)
	if detailRecorder.Code != http.StatusOK {
		t.Fatalf("detail status = %d, want 200", detailRecorder.Code)
	}
	detailBody := parseJSONMap(t, detailRecorder.Body.Bytes())
	detail := detailBody["data"].(map[string]any)
	worktree := detail["worktree"].(map[string]any)
	assertEqual(t, worktree["path"], "/tmp/worktrees/loop-1")
	assertEqual(t, worktree["branch"], "feature/loop-1")

	validationReq := httptest.NewRequest(http.MethodGet, "/api/v1/runs/active?repo=acme/looper", nil)
	validationRecorder := httptest.NewRecorder()
	h.ServeHTTP(validationRecorder, validationReq)
	if validationRecorder.Code != http.StatusBadRequest {
		t.Fatalf("validation status = %d, want 400", validationRecorder.Code)
	}
	validationBody := parseJSONMap(t, validationRecorder.Body.Bytes())
	validationError := validationBody["error"].(map[string]any)
	assertEqual(t, validationError["code"], "VALIDATION_FAILED")
	assertEqual(t, validationError["message"], "repo and prNumber must be provided together")
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
		LastRunAt:  stringPtr(nowISO),
		NextRunAt:  stringPtr(nowISO),
		CreatedAt:  nowISO,
		UpdatedAt:  nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	if err := services.Repositories.Runs.Upsert(context.Background(), storage.RunRecord{
		ID:                "run_1",
		LoopID:            loopID,
		Status:            "running",
		CurrentStep:       stringPtr("review"),
		LastCompletedStep: stringPtr("snapshot"),
		StartedAt:         nowISO,
		LastHeartbeatAt:   stringPtr(nowISO),
		CreatedAt:         nowISO,
		UpdatedAt:         nowISO,
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

func seedLoopRouteData(t *testing.T, rt *looperdruntime.Runtime) {
	t.Helper()
	seedStatusData(t, rt)
}

func seedRunRouteData(t *testing.T, rt *looperdruntime.Runtime) {
	t.Helper()
	services := rt.Services()
	now := time.Date(2026, time.April, 11, 12, 0, 0, 0, time.UTC)
	nowISO := now.Format(javaScriptISOString)
	queuedAt := now.Add(3 * time.Minute).Format(javaScriptISOString)

	if err := services.Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID:        "project_1",
		Name:      "Looper",
		RepoPath:  "/tmp/repos/looper",
		Archived:  false,
		CreatedAt: nowISO,
		UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	if err := services.Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{
		ID:         "loop_1",
		Seq:        1,
		ProjectID:  "project_1",
		Type:       "reviewer",
		TargetType: "pull_request",
		TargetID:   stringPtr("pr:acme/looper:42"),
		Repo:       stringPtr("acme/looper"),
		PRNumber:   int64Ptr(42),
		Status:     "running",
		LastRunAt:  stringPtr(nowISO),
		NextRunAt:  stringPtr(nowISO),
		CreatedAt:  nowISO,
		UpdatedAt:  nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert(loop_1) error = %v", err)
	}

	if err := services.Repositories.Runs.Upsert(context.Background(), storage.RunRecord{
		ID:                "run_1",
		LoopID:            "loop_1",
		Status:            "running",
		CurrentStep:       stringPtr("review"),
		LastCompletedStep: stringPtr("snapshot"),
		StartedAt:         nowISO,
		LastHeartbeatAt:   stringPtr(nowISO),
		CreatedAt:         nowISO,
		UpdatedAt:         nowISO,
	}); err != nil {
		t.Fatalf("Runs.Upsert(run_1) error = %v", err)
	}

	loop3ID := "11111111-1111-1111-1111-111111111111"
	if err := services.Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{
		ID:         loop3ID,
		Seq:        3,
		ProjectID:  "project_1",
		Type:       "worker",
		TargetType: "project",
		TargetID:   stringPtr("project_1"),
		Status:     "queued",
		NextRunAt:  stringPtr(queuedAt),
		CreatedAt:  queuedAt,
		UpdatedAt:  queuedAt,
	}); err != nil {
		t.Fatalf("Loops.Upsert(loop_3) error = %v", err)
	}

	if err := services.Repositories.Queue.Upsert(context.Background(), storage.QueueItemRecord{
		ID:          "queue_worker_1",
		ProjectID:   stringPtr("project_1"),
		LoopID:      &loop3ID,
		Type:        "worker",
		TargetType:  "project",
		TargetID:    "project_1",
		DedupeKey:   "worker:loop_3",
		Priority:    3,
		Status:      "running",
		AvailableAt: queuedAt,
		Attempts:    0,
		MaxAttempts: 3,
		ClaimedBy:   stringPtr("executor_1"),
		ClaimedAt:   stringPtr(queuedAt),
		StartedAt:   stringPtr(queuedAt),
		CreatedAt:   queuedAt,
		UpdatedAt:   queuedAt,
	}); err != nil {
		t.Fatalf("Queue.Upsert(queue_worker_1) error = %v", err)
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

func loadRequestArtifact(t *testing.T) []requestArtifactRoute {
	t.Helper()

	artifactPath := filepath.Join("..", "..", "specs", "2026-04-17-go-port-plan", "artifacts", "daemon-http.requests.compat.json")
	raw, err := os.ReadFile(artifactPath)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", artifactPath, err)
	}

	var artifact struct {
		Routes []requestArtifactRoute `json:"routes"`
	}
	if err := json.Unmarshal(raw, &artifact); err != nil {
		t.Fatalf("json.Unmarshal(%s) error = %v", artifactPath, err)
	}

	return artifact.Routes
}

func marshalArtifactRequestBody(t *testing.T, routes []requestArtifactRoute, routeID string) string {
	t.Helper()
	for _, route := range routes {
		if route.ID != routeID {
			continue
		}
		if route.Request.Body == nil {
			return ""
		}
		encoded, err := json.Marshal(route.Request.Body)
		if err != nil {
			t.Fatalf("json.Marshal(%s) error = %v", routeID, err)
		}
		return string(encoded)
	}
	t.Fatalf("request artifact route %q not found", routeID)
	return ""
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
		case "<uuid>":
			got, ok := actual.(string)
			return ok && strings.Count(got, "-") == 4 && strings.TrimSpace(got) != ""
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

type fixedRuntimeState struct {
	services looperdruntime.Services
}

func (s fixedRuntimeState) Services() looperdruntime.Services {
	return s.services
}

func (s fixedRuntimeState) StartedAt() (time.Time, bool) {
	return time.Time{}, false
}

func seedConflictProject(t *testing.T, service *projects.Service) {
	t.Helper()
	if service == nil || service.Repos == nil || service.Repos.Projects == nil {
		t.Fatal("projects service is not configured for conflict seed")
	}
	nowISO := time.Date(2026, time.April, 11, 12, 0, 0, 0, time.UTC).Format(javaScriptISOString)
	metadata := `{"repo":null,"worktreeRoot":null,"source":"api"}`
	if err := service.Repos.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID:           "looper",
		Name:         "Looper",
		RepoPath:     "/tmp/repos/looper",
		BaseBranch:   stringPtr("main"),
		Archived:     false,
		MetadataJSON: &metadata,
		CreatedAt:    nowISO,
		UpdatedAt:    nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert(conflict) error = %v", err)
	}
}

type fakeProjectService struct {
	list       func(context.Context) ([]storage.ProjectRecord, error)
	addProject func(context.Context, projects.AddInput) (projects.AddResult, error)
}

func (f fakeProjectService) List(ctx context.Context) ([]storage.ProjectRecord, error) {
	if f.list != nil {
		return f.list(ctx)
	}
	return nil, nil
}

func (f fakeProjectService) AddProject(ctx context.Context, input projects.AddInput) (projects.AddResult, error) {
	if f.addProject != nil {
		return f.addProject(ctx, input)
	}
	return projects.AddResult{}, nil
}

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

type requestArtifactRoute struct {
	ID      string `json:"id"`
	Request struct {
		Body any `json:"body"`
	} `json:"request"`
}
