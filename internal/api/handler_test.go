package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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
	"github.com/powerformer/looper/internal/domain"
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

func TestIsTerminalReviewerLoopRecordTreatsFailedAsTerminal(t *testing.T) {
	t.Parallel()
	metadata := `{"loop":{"status":"failed"}}`

	if !isTerminalReviewerLoopRecord(storage.LoopRecord{Type: "reviewer", Status: "failed"}) {
		t.Fatalf("failed record status was not terminal")
	}
	if !isTerminalReviewerLoopRecord(storage.LoopRecord{Type: "reviewer", Status: "running", MetadataJSON: &metadata}) {
		t.Fatalf("failed metadata status was not terminal")
	}
}

func TestHandlerStatusSuccessContainsExpectedSections(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	seedStatusData(t, rt)
	seedStatusLoopCounts(t, rt)

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
	binaryInfo := service["binary"].(map[string]any)
	storageInfo := data["storage"].(map[string]any)
	scheduler := data["scheduler"].(map[string]any)
	loops := data["loops"].(map[string]any)

	assertEqual(t, service["healthy"], true)
	assertEqual(t, service["daemonMode"], "foreground")
	binaryPath, ok := binaryInfo["path"].(string)
	if !ok || strings.TrimSpace(binaryPath) == "" {
		t.Fatalf("service.binary.path missing/invalid: %#v", binaryInfo["path"])
	}
	assertEqual(t, storageInfo["healthy"], true)
	queuedItems, queuedOK := scheduler["queuedItems"].(float64)
	runningItems, runningOK := scheduler["runningItems"].(float64)
	if !queuedOK || !runningOK {
		t.Fatalf("scheduler queue counters missing/invalid: %#v", scheduler)
	}
	if queuedItems+runningItems != float64(1) {
		t.Fatalf("scheduler queued+running = %v, want 1 (queued=%v running=%v)", queuedItems+runningItems, queuedItems, runningItems)
	}
	assertEqual(t, scheduler["totalRuns"], float64(1))
	assertEqual(t, scheduler["activeRuns"], float64(1))

	reviewer := loops["reviewer"].(map[string]any)
	assertEqual(t, reviewer["queued"], float64(1))
	assertEqual(t, reviewer["running"], float64(1))
	assertEqual(t, reviewer["waiting"], float64(1))
	assertEqual(t, reviewer["terminated"], float64(1))
	assertEqual(t, reviewer["stopped"], float64(1))
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
	reviewer := data["reviewer"].(map[string]any)
	reviewerLoop := reviewer["loop"].(map[string]any)

	assertEqual(t, server["host"], cfg.Server.Host)
	assertEqual(t, server["port"], float64(cfg.Server.Port))
	assertEqual(t, server["authMode"], string(cfg.Server.AuthMode))
	assertEqual(t, server["localTokenConfigured"], false)
	assertEqual(t, storageInfo["mode"], cfg.Storage.Mode)
	assertEqual(t, daemon["mode"], string(cfg.Daemon.Mode))
	assertEqual(t, daemon["workingDirectory"], cfg.Daemon.WorkingDirectory)
	assertEqual(t, reviewer["scope"], string(cfg.Reviewer.Scope))
	assertEqual(t, reviewer["publishMode"], string(cfg.Reviewer.PublishMode))
	assertEqual(t, reviewer["detectDuplicateFindings"], cfg.Reviewer.DetectDuplicateFindings)
	assertEqual(t, reviewerLoop["enabledByDefault"], cfg.Reviewer.Loop.EnabledByDefault)
	assertEqual(t, reviewerLoop["maxConsecutiveFailures"], float64(cfg.Reviewer.Loop.MaxConsecutiveFailures))
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

func TestHandlerPullRequestRouteReturnsInternalErrorWhenLoopLookupFails(t *testing.T) {
	fixture := newTestFixture(t)
	if err := fixture.runtime.Services().Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "project_1", Name: "Looper", RepoPath: "/tmp/repos/looper", CreatedAt: fixture.now.UTC().Format(javaScriptISOString), UpdatedAt: fixture.now.UTC().Format(javaScriptISOString)}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := fixture.runtime.Services().Repositories.PullRequestSnapshots.Upsert(context.Background(), storage.PullRequestSnapshotRecord{
		ID:         "prs_loops_fail_detail",
		ProjectID:  "project_1",
		Repo:       "acme/looper",
		PRNumber:   42,
		HeadSHA:    "abc123",
		CapturedAt: fixture.now.UTC().Format(javaScriptISOString),
		CreatedAt:  fixture.now.UTC().Format(javaScriptISOString),
	}); err != nil {
		t.Fatalf("PullRequestSnapshots.Upsert() error = %v", err)
	}

	services := fixture.runtime.Services()
	services.Repositories = storage.NewRepositories(errorInjectingQuerier{db: services.Coordinator.DB(), queryError: func(query string) error {
		if strings.Contains(query, "SELECT * FROM loops ORDER BY updated_at DESC, seq DESC") {
			return errors.New("database is locked")
		}
		return nil
	}})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/pull-requests/acme%2Flooper/42", nil)
	recorder := httptest.NewRecorder()

	NewHandler(Context{Config: fixture.config, Runtime: fixedRuntimeState{services: services}}).ServeHTTP(recorder, req)

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", recorder.Code)
	}
	body := parseJSONMap(t, recorder.Body.Bytes())
	errMap := body["error"].(map[string]any)
	assertEqual(t, errMap["code"], "INTERNAL_ERROR")
	assertEqual(t, errMap["message"], "list loops: database is locked")
}

func TestHandlerPullRequestStatusReturnsInternalErrorWhenLoopLookupFails(t *testing.T) {
	fixture := newTestFixture(t)
	if err := fixture.runtime.Services().Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "project_1", Name: "Looper", RepoPath: "/tmp/repos/looper", CreatedAt: fixture.now.UTC().Format(javaScriptISOString), UpdatedAt: fixture.now.UTC().Format(javaScriptISOString)}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := fixture.runtime.Services().Repositories.PullRequestSnapshots.Upsert(context.Background(), storage.PullRequestSnapshotRecord{
		ID:         "prs_loops_fail_status",
		ProjectID:  "project_1",
		Repo:       "acme/looper",
		PRNumber:   42,
		HeadSHA:    "abc123",
		CapturedAt: fixture.now.UTC().Format(javaScriptISOString),
		CreatedAt:  fixture.now.UTC().Format(javaScriptISOString),
	}); err != nil {
		t.Fatalf("PullRequestSnapshots.Upsert() error = %v", err)
	}

	services := fixture.runtime.Services()
	services.Repositories = storage.NewRepositories(errorInjectingQuerier{db: services.Coordinator.DB(), queryError: func(query string) error {
		if strings.Contains(query, "SELECT * FROM loops ORDER BY updated_at DESC, seq DESC") {
			return errors.New("database is locked")
		}
		return nil
	}})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/pull-requests/acme%2Flooper/42/status", nil)
	recorder := httptest.NewRecorder()

	NewHandler(Context{Config: fixture.config, Runtime: fixedRuntimeState{services: services}}).ServeHTTP(recorder, req)

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", recorder.Code)
	}
	body := parseJSONMap(t, recorder.Body.Bytes())
	errMap := body["error"].(map[string]any)
	assertEqual(t, errMap["code"], "INTERNAL_ERROR")
	assertEqual(t, errMap["message"], "list loops: database is locked")
}

func TestReadAgentOutputLogReadsTailToPreserveCompletionMarker(t *testing.T) {
	t.Parallel()

	logDir := t.TempDir()
	logPath := filepath.Join(logDir, "stdout.log")
	completionLine := "__LOOPER_RESULT__={\"summary\":\"done from tail\"}\n"
	headSize := maxPersistedAgentLogReadBytes - len(completionLine) + 1023
	fullLog := strings.Repeat("x", headSize) + "\n" + completionLine
	if err := os.WriteFile(logPath, []byte(fullLog), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	persisted, ok := readAgentOutputLog(logDir, logPath)
	if !ok {
		t.Fatal("readAgentOutputLog() = ok false, want true")
	}
	if len(persisted) != maxPersistedAgentLogReadBytes {
		t.Fatalf("len(persisted) = %d, want %d", len(persisted), maxPersistedAgentLogReadBytes)
	}
	if !strings.HasSuffix(persisted, completionLine) {
		t.Fatal("persisted log missing completion tail suffix")
	}
}

func TestHandlerPullRequestStatusReturnsInternalErrorWhenRunLookupFails(t *testing.T) {
	fixture := newTestFixture(t)
	seedEventAndPullRequestRouteData(t, fixture.runtime)

	services := fixture.runtime.Services()
	services.Repositories = storage.NewRepositories(errorInjectingQuerier{db: services.Coordinator.DB(), queryError: func(query string) error {
		if strings.Contains(query, "SELECT * FROM runs WHERE loop_id = ? ORDER BY started_at DESC") {
			return errors.New("database is locked")
		}
		return nil
	}})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/pull-requests/acme%2Flooper/42/status", nil)
	recorder := httptest.NewRecorder()

	NewHandler(Context{Config: fixture.config, Runtime: fixedRuntimeState{services: services}}).ServeHTTP(recorder, req)

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", recorder.Code)
	}
	body := parseJSONMap(t, recorder.Body.Bytes())
	errMap := body["error"].(map[string]any)
	assertEqual(t, errMap["code"], "INTERNAL_ERROR")
	assertEqual(t, errMap["message"], "list runs by loop: database is locked")
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
		{caseID: "route-not-found", method: http.MethodGet, path: "/api/v1/does-not-exist", cfg: cfg},
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
			assertErrorArtifactMatch(t, parseJSONMap(t, recorder.Body.Bytes()), want)
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

func TestHandlerEventAndPullRequestRoutesMatchFrozenSuccessArtifacts(t *testing.T) {
	routes := loadResponseArtifact(t)
	fixture := newTestFixture(t)
	seedEventAndPullRequestRouteData(t, fixture.runtime)

	h := NewHandler(Context{
		Config:  fixture.config,
		Runtime: fixture.runtime,
		Now:     func() time.Time { return fixture.now },
	})

	tests := []struct {
		routeID string
		method  string
		path    string
	}{
		{routeID: "events.list", method: http.MethodGet, path: "/api/v1/events?limit=1"},
		{routeID: "events.entity", method: http.MethodGet, path: "/api/v1/events/loop/loop_1"},
		{routeID: "pullRequests.list", method: http.MethodGet, path: "/api/v1/pull-requests"},
		{routeID: "pullRequests.detail", method: http.MethodGet, path: "/api/v1/pull-requests/acme%2Flooper/42"},
		{routeID: "pullRequests.status", method: http.MethodGet, path: "/api/v1/pull-requests/acme%2Flooper/42/status"},
	}

	for _, tt := range tests {
		t.Run(tt.routeID, func(t *testing.T) {
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

func TestHandlerEventAndPullRequestRouteErrorsMatchArtifactCases(t *testing.T) {
	fixture := newTestFixture(t)
	seedEventAndPullRequestRouteData(t, fixture.runtime)

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
	}{
		{caseID: "validation-failed", method: http.MethodGet, path: "/api/v1/events?limit=0"},
		{caseID: "pr-not-found", method: http.MethodGet, path: "/api/v1/pull-requests/acme%2Flooper/999"},
	}

	h := NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime})
	for _, tt := range tests {
		t.Run(tt.caseID, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, nil)
			req.Header.Set("x-request-id", "error-request-id")
			recorder := httptest.NewRecorder()
			h.ServeHTTP(recorder, req)

			want := findArtifactCase(t, artifact.Cases, tt.caseID)
			if recorder.Code != want.ExpectedStatus {
				t.Fatalf("status = %d, want %d", recorder.Code, want.ExpectedStatus)
			}
			assertErrorArtifactMatch(t, parseJSONMap(t, recorder.Body.Bytes()), want)
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
	nowISO := fixture.now.UTC().Format(javaScriptISOString)
	reqBody := []byte(`{"repoPath":"C:\\\\tmp/repos/Looper Repo"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects", bytes.NewReader(reqBody))
	req.Header.Set("x-request-id", "fixture-request-id")
	recorder := httptest.NewRecorder()

	NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime, ProjectsService: fakeProjectService{
		addProject: func(context.Context, projects.AddInput) (projects.AddResult, error) {
			metadataJSON := `{"repo":null,"worktreeRoot":null,"source":"api"}`
			return projects.AddResult{
				Project:                storage.ProjectRecord{ID: "looper-repo", Name: "looper-repo", RepoPath: `C:\\tmp/repos/Looper Repo`, BaseBranch: stringPtr(fixture.config.Defaults.BaseBranch), MetadataJSON: &metadataJSON, CreatedAt: nowISO, UpdatedAt: nowISO},
				DiscoveredPullRequests: 0,
				DiscoveredWorktrees:    0,
				Warnings:               nil,
			}, nil
		},
	}}).ServeHTTP(recorder, req)

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

func TestHandlerProjectsCreateRouteReturnsDiscoveryDetails(t *testing.T) {
	fixture := newTestFixture(t)
	nowISO := fixture.now.UTC().Format(javaScriptISOString)
	reqBody := []byte(`{"repoPath":"/tmp/repos/looper","name":"Looper"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects", bytes.NewReader(reqBody))
	recorder := httptest.NewRecorder()

	NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime, ProjectsService: fakeProjectService{
		addProject: func(context.Context, projects.AddInput) (projects.AddResult, error) {
			repo := "acme/looper"
			metadataJSON := `{"repo":"acme/looper","worktreeRoot":null,"source":"api"}`
			return projects.AddResult{
				Project:                storage.ProjectRecord{ID: "looper", Name: "Looper", RepoPath: "/tmp/repos/looper", BaseBranch: stringPtr("main"), MetadataJSON: &metadataJSON, CreatedAt: nowISO, UpdatedAt: nowISO},
				Repo:                   &repo,
				DiscoveredPullRequests: 2,
				DiscoveredWorktrees:    3,
				Warnings:               []string{"warn 1", "warn 2"},
			}, nil
		},
	}}).ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	body := parseJSONMap(t, recorder.Body.Bytes())
	data := body["data"].(map[string]any)
	assertEqual(t, data["id"], "looper")
	assertEqual(t, data["repo"], "acme/looper")
	assertEqual(t, data["discoveredPullRequests"], float64(2))
	assertEqual(t, data["discoveredWorktrees"], float64(3))
	warnings, ok := data["warnings"].([]any)
	if !ok || !reflect.DeepEqual(warnings, []any{"warn 1", "warn 2"}) {
		t.Fatalf("warnings = %#v, want [warn 1 warn 2]", data["warnings"])
	}
}

func TestHandlerProjectsRemoveRouteDeletesProject(t *testing.T) {
	fixture := newTestFixture(t)
	nowISO := fixture.now.UTC().Format(javaScriptISOString)
	if err := fixture.runtime.Services().Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "project_1", Name: "Looper", RepoPath: "/tmp/looper", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/projects/project_1", nil)
	req.Header.Set("x-request-id", "fixture-request-id")
	recorder := httptest.NewRecorder()
	NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime}).ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	body := parseJSONMap(t, recorder.Body.Bytes())
	data := body["data"].(map[string]any)
	assertEqual(t, data["id"], "project_1")
	assertEqual(t, data["name"], "Looper")
	project, err := fixture.runtime.Services().Repositories.Projects.GetByID(context.Background(), "project_1")
	if err != nil {
		t.Fatalf("Projects.GetByID() error = %v", err)
	}
	if project != nil {
		t.Fatalf("project after delete = %#v, want nil", project)
	}
}

func TestHandlerProjectsRemoveRouteDeletesProjectWithEscapedSlashInName(t *testing.T) {
	fixture := newTestFixture(t)
	nowISO := fixture.now.UTC().Format(javaScriptISOString)
	if err := fixture.runtime.Services().Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "project_1", Name: "Looper/Core", RepoPath: "/tmp/looper", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/projects/Looper%2FCore", nil)
	req.Header.Set("x-request-id", "fixture-request-id")
	recorder := httptest.NewRecorder()
	NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime}).ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	body := parseJSONMap(t, recorder.Body.Bytes())
	data := body["data"].(map[string]any)
	assertEqual(t, data["id"], "project_1")
	assertEqual(t, data["name"], "Looper/Core")
	project, err := fixture.runtime.Services().Repositories.Projects.GetByID(context.Background(), "project_1")
	if err != nil {
		t.Fatalf("Projects.GetByID() error = %v", err)
	}
	if project != nil {
		t.Fatalf("project after delete = %#v, want nil", project)
	}
}

func TestHandlerProjectsRemoveRouteReturnsNotFound(t *testing.T) {
	fixture := newTestFixture(t)
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/projects/missing", nil)
	req.Header.Set("x-request-id", "fixture-request-id")
	recorder := httptest.NewRecorder()
	NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime}).ServeHTTP(recorder, req)

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", recorder.Code)
	}
	body := parseJSONMap(t, recorder.Body.Bytes())
	errorMap := body["error"].(map[string]any)
	assertEqual(t, errorMap["code"], "PROJECT_NOT_FOUND")
	assertEqual(t, errorMap["message"], "Project not found: missing")
}

func TestHandlerProjectsRouteErrorsMatchArtifactCases(t *testing.T) {
	fixture := newTestFixture(t)

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
		caseID          string
		runtime         RuntimeState
		projectsService projectService
		body            string
		wantID          bool
	}{
		{
			caseID:  "projects-unavailable",
			runtime: stubUnavailableRuntime,
			body:    `{"repoPath":"/tmp/repos/looper","name":"Looper"}`,
			wantID:  true,
		},
		{
			caseID:          "invalid-project-id",
			runtime:         fixture.runtime,
			projectsService: fixture.runtime.Services().Projects,
			body:            `{"repoPath":"/tmp/repos/looper","id":"../../tmp","name":"Looper"}`,
			wantID:          true,
		},
		{
			caseID:  "project-id-conflict",
			runtime: fixture.runtime,
			projectsService: fakeProjectService{
				addProject: func(context.Context, projects.AddInput) (projects.AddResult, error) {
					return projects.AddResult{}, projects.ProjectIDCollisionError{ProjectID: "looper"}
				},
			},
			body:   `{"repoPath":"/tmp/repos/looper","id":"looper","name":"Looper"}`,
			wantID: true,
		},
		{
			caseID:  "internal-error",
			runtime: fixture.runtime,
			projectsService: fakeProjectService{
				addProject: func(context.Context, projects.AddInput) (projects.AddResult, error) {
					return projects.AddResult{}, errors.New("boom")
				},
			},
			body:   `{"repoPath":"/tmp/repos/looper","id":"looper","name":"Looper"}`,
			wantID: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.caseID, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/projects", bytes.NewReader([]byte(tt.body)))
			if tt.wantID {
				req.Header.Set("x-request-id", "error-request-id")
			}
			recorder := httptest.NewRecorder()
			NewHandler(Context{Config: fixture.config, Runtime: tt.runtime, ProjectsService: tt.projectsService}).ServeHTTP(recorder, req)

			want := findArtifactCase(t, artifact.Cases, tt.caseID)
			if recorder.Code != want.ExpectedStatus {
				t.Fatalf("status = %d, want %d", recorder.Code, want.ExpectedStatus)
			}
			assertErrorArtifactMatch(t, parseJSONMap(t, recorder.Body.Bytes()), want)
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
			assertErrorArtifactMatch(t, parseJSONMap(t, recorder.Body.Bytes()), want)
		})
	}
}

func TestHandlerLoopStartRejectsCodingLoopsWithoutAgentConfigured(t *testing.T) {
	fixture := newTestFixture(t)
	seedLoopRouteData(t, fixture.runtime)
	nowISO := fixture.now.UTC().Format(javaScriptISOString)

	configWithoutAgent := fixture.config
	configWithoutAgent.Agent.Vendor = nil
	tests := []struct {
		name       string
		loopID     string
		loopType   string
		targetType string
		targetID   *string
		repo       *string
		prNumber   *int64
		message    string
	}{
		{
			name:       "fixer",
			loopID:     "loop_fixer_no_agent",
			loopType:   "fixer",
			targetType: "pull_request",
			targetID:   stringPtr("pr:acme/looper:99"),
			repo:       stringPtr("acme/looper"),
			prNumber:   int64Ptr(99),
			message:    "Cannot start fixer loop without config.agent.vendor",
		},
		{
			name:       "reviewer",
			loopID:     "loop_reviewer_no_agent",
			loopType:   "reviewer",
			targetType: "pull_request",
			targetID:   stringPtr("pr:acme/looper:100"),
			repo:       stringPtr("acme/looper"),
			prNumber:   int64Ptr(100),
			message:    "Cannot start reviewer loop without config.agent.vendor",
		},
		{
			name:       "worker",
			loopID:     "loop_worker_no_agent",
			loopType:   "worker",
			targetType: "project",
			targetID:   stringPtr("project:project_1"),
			repo:       stringPtr("acme/looper"),
			message:    "Cannot start worker loop without config.agent.vendor",
		},
		{
			name:       "planner",
			loopID:     "loop_planner_no_agent",
			loopType:   "planner",
			targetType: "issue",
			targetID:   stringPtr("issue:acme/looper:101"),
			repo:       stringPtr("acme/looper"),
			message:    "Cannot start planner loop without config.agent.vendor",
		},
	}

	for i, tt := range tests {
		if err := fixture.runtime.Services().Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{
			ID:         tt.loopID,
			Seq:        int64(i + 4),
			ProjectID:  "project_1",
			Type:       tt.loopType,
			TargetType: tt.targetType,
			TargetID:   tt.targetID,
			Repo:       tt.repo,
			PRNumber:   tt.prNumber,
			Status:     "paused",
			CreatedAt:  nowISO,
			UpdatedAt:  nowISO,
		}); err != nil {
			t.Fatalf("Loops.Upsert() error = %v", err)
		}

		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/loops/"+tt.loopID+"/start", nil)
			req.Header.Set("x-request-id", "error-request-id")
			recorder := httptest.NewRecorder()

			NewHandler(Context{Config: configWithoutAgent, Runtime: fixture.runtime, Now: func() time.Time { return fixture.now.Add(time.Minute) }}).ServeHTTP(recorder, req)

			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", recorder.Code)
			}
			body := parseJSONMap(t, recorder.Body.Bytes())
			errorMap := body["error"].(map[string]any)
			assertEqual(t, errorMap["code"], "AGENT_NOT_CONFIGURED")
			assertEqual(t, errorMap["message"], tt.message)
		})
	}
}

func TestHandlerLoopStatusMutationsReconcileQueueItems(t *testing.T) {
	fixture := newTestFixture(t)
	services := fixture.runtime.Services()
	nowISO := fixture.now.UTC().Format(javaScriptISOString)
	projectID := "project_status"
	loopID := "loop_worker_status"
	targetID := "project:project_status"
	payload := `{"title":"Implement worker loop","prompt":"Do the thing","repo":"acme/looper","baseBranch":"main"}`
	metadata := `{"worker":{"title":"Implement worker loop","prompt":"Do the thing","repo":"acme/looper","baseBranch":"main"}}`

	if err := services.Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Looper", RepoPath: "/tmp/repos/looper", Archived: false, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := services.Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 2, ProjectID: projectID, Type: "worker", TargetType: "project", TargetID: &targetID, Repo: stringPtr("acme/looper"), Status: "running", MetadataJSON: &metadata, NextRunAt: &nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	if err := services.Repositories.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: "queue_worker_status", ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: targetID, Repo: stringPtr("acme/looper"), DedupeKey: "worker:loop_worker_status", Priority: storage.QueuePriorityWorker, Status: "queued", AvailableAt: nowISO, Attempts: 0, MaxAttempts: 3, PayloadJSON: &payload, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	h := NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime, Now: func() time.Time { return fixture.now.Add(time.Minute) }})

	pauseReq := httptest.NewRequest(http.MethodPost, "/api/v1/loops/"+loopID+"/pause", nil)
	pauseRecorder := httptest.NewRecorder()
	h.ServeHTTP(pauseRecorder, pauseReq)
	if pauseRecorder.Code != http.StatusOK {
		t.Fatalf("pause status = %d, want 200", pauseRecorder.Code)
	}

	pausedLoop, err := services.Repositories.Loops.GetByID(context.Background(), loopID)
	if err != nil {
		t.Fatalf("Loops.GetByID() after pause error = %v", err)
	}
	if pausedLoop == nil || pausedLoop.Status != "paused" || pausedLoop.NextRunAt != nil {
		t.Fatalf("paused loop = %#v, want paused with nil next run", pausedLoop)
	}
	pausedQueue, err := services.Repositories.Queue.GetByID(context.Background(), "queue_worker_status")
	if err != nil {
		t.Fatalf("Queue.GetByID() after pause error = %v", err)
	}
	if pausedQueue == nil || pausedQueue.Status != "cancelled" {
		t.Fatalf("paused queue = %#v, want cancelled", pausedQueue)
	}

	startReq := httptest.NewRequest(http.MethodPost, "/api/v1/loops/"+loopID+"/start", nil)
	startRecorder := httptest.NewRecorder()
	h.ServeHTTP(startRecorder, startReq)
	if startRecorder.Code != http.StatusOK {
		t.Fatalf("start status = %d, want 200", startRecorder.Code)
	}

	startedLoop, err := services.Repositories.Loops.GetByID(context.Background(), loopID)
	if err != nil {
		t.Fatalf("Loops.GetByID() after start error = %v", err)
	}
	if startedLoop == nil || startedLoop.Status != "running" || startedLoop.NextRunAt == nil {
		t.Fatalf("started loop = %#v, want running with next run", startedLoop)
	}
	startedQueue, err := services.Repositories.Queue.GetByID(context.Background(), "queue_worker_status")
	if err != nil {
		t.Fatalf("Queue.GetByID() after start error = %v", err)
	}
	if startedQueue == nil || startedQueue.Status != "queued" || startedQueue.FinishedAt != nil || startedQueue.LastError != nil {
		t.Fatalf("started queue = %#v, want requeued item", startedQueue)
	}
}

func TestHandlerLoopStartRejectsTerminalReviewerLoop(t *testing.T) {
	fixture := newTestFixture(t)
	services := fixture.runtime.Services()
	nowISO := fixture.now.UTC().Format(javaScriptISOString)
	repo := "acme/looper"
	prNumber := int64(42)
	if err := services.Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "project_1", Name: "Looper", RepoPath: "/tmp/repos/looper", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	metadataTerminated := `{"loop":{"status":"terminated"}}`
	loops := []storage.LoopRecord{
		{ID: "loop_reviewer_terminated", Seq: 10, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: "terminated", CreatedAt: nowISO, UpdatedAt: nowISO},
		{ID: "loop_reviewer_metadata_terminated", Seq: 11, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: "completed", MetadataJSON: &metadataTerminated, CreatedAt: nowISO, UpdatedAt: nowISO},
	}
	for _, loop := range loops {
		if err := services.Repositories.Loops.Upsert(context.Background(), loop); err != nil {
			t.Fatalf("Loops.Upsert(%s) error = %v", loop.ID, err)
		}
	}
	h := NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime, Now: func() time.Time { return fixture.now.Add(time.Minute) }})
	for _, loop := range loops {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/loops/"+loop.ID+"/start", nil)
		recorder := httptest.NewRecorder()
		h.ServeHTTP(recorder, req)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("start %s status = %d, want 400", loop.ID, recorder.Code)
		}
		updated, err := services.Repositories.Loops.GetByID(context.Background(), loop.ID)
		if err != nil || updated == nil {
			t.Fatalf("Loops.GetByID(%s) = (%#v, %v), want loop", loop.ID, updated, err)
		}
		if updated.Status != loop.Status {
			t.Fatalf("loop %s status = %q, want unchanged %q", loop.ID, updated.Status, loop.Status)
		}
	}
}

func TestHandlerLoopStartIsIdempotentWhenQueueItemAlreadyActive(t *testing.T) {
	fixture := newTestFixture(t)
	services := fixture.runtime.Services()
	nowISO := fixture.now.UTC().Format(javaScriptISOString)
	projectID := "project_status"
	loopID := "loop_worker_status"
	targetID := "project:project_status"
	payload := `{"title":"Implement worker loop","prompt":"Do the thing","repo":"acme/looper","baseBranch":"main"}`
	metadata := `{"worker":{"title":"Implement worker loop","prompt":"Do the thing","repo":"acme/looper","baseBranch":"main"}}`

	if err := services.Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Looper", RepoPath: "/tmp/repos/looper", Archived: false, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := services.Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 2, ProjectID: projectID, Type: "worker", TargetType: "project", TargetID: &targetID, Repo: stringPtr("acme/looper"), Status: "running", MetadataJSON: &metadata, NextRunAt: &nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	if err := services.Repositories.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: "queue_worker_status", ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: targetID, Repo: stringPtr("acme/looper"), DedupeKey: "worker:loop_worker_status", Priority: storage.QueuePriorityWorker, Status: "queued", AvailableAt: nowISO, Attempts: 0, MaxAttempts: 3, PayloadJSON: &payload, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	triggered := 0
	h := NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime, Now: func() time.Time { return fixture.now.Add(time.Minute) }, TriggerSchedulerTick: func() { triggered++ }})

	startReq := httptest.NewRequest(http.MethodPost, "/api/v1/loops/"+loopID+"/start", nil)
	startRecorder := httptest.NewRecorder()
	h.ServeHTTP(startRecorder, startReq)
	if startRecorder.Code != http.StatusOK {
		t.Fatalf("start status = %d, want 200", startRecorder.Code)
	}

	queueItems, err := services.Repositories.Queue.List(context.Background())
	if err != nil {
		t.Fatalf("Queue.List() error = %v", err)
	}
	matched := []storage.QueueItemRecord{}
	for _, item := range queueItems {
		if item.LoopID != nil && *item.LoopID == loopID {
			matched = append(matched, item)
		}
	}
	if len(matched) != 1 {
		t.Fatalf("queue items for loop = %d, want 1", len(matched))
	}
	assertEqual(t, matched[0].Status, "queued")
	assertEqual(t, triggered, 1)
}

func TestHandlerLoopStartDoesNotRequeueCancelledItemWhenAnotherQueueItemIsActive(t *testing.T) {
	fixture := newTestFixture(t)
	services := fixture.runtime.Services()
	now := fixture.now.UTC()
	nowISO := now.Format(javaScriptISOString)
	projectID := "project_status"
	loopID := "loop_worker_status"
	targetID := "project:project_status"
	payload := `{"title":"Implement worker loop","prompt":"Do the thing","repo":"acme/looper","baseBranch":"main"}`
	metadata := `{"worker":{"title":"Implement worker loop","prompt":"Do the thing","repo":"acme/looper","baseBranch":"main"}}`
	olderISO := now.Add(-time.Minute).Format(javaScriptISOString)
	newerISO := now.Add(time.Minute).Format(javaScriptISOString)
	finishedISO := now.Add(2 * time.Minute).Format(javaScriptISOString)

	if err := services.Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Looper", RepoPath: "/tmp/repos/looper", Archived: false, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := services.Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 2, ProjectID: projectID, Type: "worker", TargetType: "project", TargetID: &targetID, Repo: stringPtr("acme/looper"), Status: "paused", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	if err := services.Repositories.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: "queue_worker_active", ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: targetID, Repo: stringPtr("acme/looper"), DedupeKey: "worker:loop_worker_status", Priority: storage.QueuePriorityWorker, Status: "queued", AvailableAt: olderISO, Attempts: 0, MaxAttempts: 3, PayloadJSON: &payload, CreatedAt: olderISO, UpdatedAt: olderISO}); err != nil {
		t.Fatalf("Queue.Upsert(active) error = %v", err)
	}
	if err := services.Repositories.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: "queue_worker_cancelled", ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: targetID, Repo: stringPtr("acme/looper"), DedupeKey: "worker:loop_worker_status", Priority: storage.QueuePriorityWorker, Status: "cancelled", AvailableAt: newerISO, Attempts: 1, MaxAttempts: 3, PayloadJSON: &payload, FinishedAt: &finishedISO, CreatedAt: newerISO, UpdatedAt: newerISO}); err != nil {
		t.Fatalf("Queue.Upsert(cancelled) error = %v", err)
	}

	triggered := 0
	h := NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime, Now: func() time.Time { return now.Add(3 * time.Minute) }, TriggerSchedulerTick: func() { triggered++ }})

	startReq := httptest.NewRequest(http.MethodPost, "/api/v1/loops/"+loopID+"/start", nil)
	startRecorder := httptest.NewRecorder()
	h.ServeHTTP(startRecorder, startReq)
	if startRecorder.Code != http.StatusOK {
		t.Fatalf("start status = %d, want 200", startRecorder.Code)
	}

	activeQueue, err := services.Repositories.Queue.GetByID(context.Background(), "queue_worker_active")
	if err != nil {
		t.Fatalf("Queue.GetByID(active) error = %v", err)
	}
	if activeQueue == nil || activeQueue.Status != "queued" {
		t.Fatalf("active queue = %#v, want queued", activeQueue)
	}
	cancelledQueue, err := services.Repositories.Queue.GetByID(context.Background(), "queue_worker_cancelled")
	if err != nil {
		t.Fatalf("Queue.GetByID(cancelled) error = %v", err)
	}
	if cancelledQueue == nil || cancelledQueue.Status != "cancelled" {
		t.Fatalf("cancelled queue = %#v, want cancelled", cancelledQueue)
	}
	queueItems, err := services.Repositories.Queue.List(context.Background())
	if err != nil {
		t.Fatalf("Queue.List() error = %v", err)
	}
	matched := []storage.QueueItemRecord{}
	for _, item := range queueItems {
		if item.LoopID != nil && *item.LoopID == loopID {
			matched = append(matched, item)
		}
	}
	if len(matched) != 2 {
		t.Fatalf("queue items for loop = %d, want 2", len(matched))
	}
	assertEqual(t, triggered, 1)
}

func TestHandlerWorkerAndPlannerRoutesMatchFrozenSuccessArtifacts(t *testing.T) {
	routes := loadResponseArtifact(t)
	requestArtifact := loadRequestArtifact(t)

	fixture := newTestFixture(t)
	seedLoopRouteData(t, fixture.runtime)
	seedWorkerPlannerArtifactsData(t, fixture.runtime, fixture.now)

	h := NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime, Now: func() time.Time { return fixture.now.Add(time.Minute) }})

	bootstrapReq := httptest.NewRequest(http.MethodPost, "/api/v1/loops", bytes.NewReader([]byte(marshalArtifactRequestBody(t, requestArtifact, "loops.create"))))
	bootstrapReq.Header.Set("content-type", "application/json")
	bootstrapRecorder := httptest.NewRecorder()
	h.ServeHTTP(bootstrapRecorder, bootstrapReq)
	if bootstrapRecorder.Code != http.StatusOK {
		t.Fatalf("bootstrap loops.create status = %d, want 200", bootstrapRecorder.Code)
	}

	workerReq := httptest.NewRequest(http.MethodPost, "/api/v1/workers", bytes.NewReader([]byte(marshalArtifactRequestBody(t, requestArtifact, "workers.create"))))
	workerReq.Header.Set("x-request-id", "fixture-request-id")
	workerReq.Header.Set("content-type", "application/json")
	workerRecorder := httptest.NewRecorder()
	h.ServeHTTP(workerRecorder, workerReq)
	if workerRecorder.Code != http.StatusOK {
		t.Fatalf("workers.create status = %d, want 200", workerRecorder.Code)
	}
	workerActual := normalizeResponseValue(parseJSONValue(t, workerRecorder.Body.Bytes()), fixture.rootDir)
	workerWant := findResponseArtifactRoute(t, routes, "workers.create")
	if !responseFixtureMatches(workerActual, workerWant.Body) {
		actualJSON, _ := json.MarshalIndent(workerActual, "", "  ")
		wantJSON, _ := json.MarshalIndent(workerWant.Body, "", "  ")
		t.Fatalf("workers.create normalized body mismatch\nactual=%s\nwant=%s", actualJSON, wantJSON)
	}

	plannerReq := httptest.NewRequest(http.MethodPost, "/api/v1/planners", bytes.NewReader([]byte(marshalArtifactRequestBody(t, requestArtifact, "planners.create"))))
	plannerReq.Header.Set("x-request-id", "fixture-request-id")
	plannerReq.Header.Set("content-type", "application/json")
	plannerRecorder := httptest.NewRecorder()
	h.ServeHTTP(plannerRecorder, plannerReq)
	if plannerRecorder.Code != http.StatusOK {
		t.Fatalf("planners.create status = %d, want 200", plannerRecorder.Code)
	}
	plannerActual := normalizeResponseValue(parseJSONValue(t, plannerRecorder.Body.Bytes()), fixture.rootDir)
	plannerWant := findResponseArtifactRoute(t, routes, "planners.create")
	if !responseFixtureMatches(plannerActual, plannerWant.Body) {
		actualJSON, _ := json.MarshalIndent(plannerActual, "", "  ")
		wantJSON, _ := json.MarshalIndent(plannerWant.Body, "", "  ")
		t.Fatalf("planners.create normalized body mismatch\nactual=%s\nwant=%s", actualJSON, wantJSON)
	}
}

func TestHandlerLoopStartCreatesReplacementQueueItemWhenLatestWorkIsTerminal(t *testing.T) {
	fixture := newTestFixture(t)
	services := fixture.runtime.Services()
	nowISO := fixture.now.UTC().Format(javaScriptISOString)
	projectID := "project_restart"
	loopID := "loop_worker_restart"
	targetID := "project:project_restart"
	payload := `{"title":"Implement worker loop","prompt":"Do the thing","repo":"acme/looper","baseBranch":"main"}`
	metadata := `{"worker":{"title":"Implement worker loop","prompt":"Do the thing","repo":"acme/looper","baseBranch":"main"}}`
	finishedAt := fixture.now.Add(-time.Minute).UTC().Format(javaScriptISOString)

	if err := services.Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Looper", RepoPath: "/tmp/repos/looper", Archived: false, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := services.Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 3, ProjectID: projectID, Type: "worker", TargetType: "project", TargetID: &targetID, Repo: stringPtr("acme/looper"), Status: "paused", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	if err := services.Repositories.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: "queue_worker_terminal", ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: targetID, Repo: stringPtr("acme/looper"), DedupeKey: "worker:loop_worker_restart", Priority: storage.QueuePriorityWorker, Status: "failed", AvailableAt: nowISO, Attempts: 2, MaxAttempts: 3, FinishedAt: &finishedAt, PayloadJSON: &payload, LastError: stringPtr("boom"), LastErrorKind: stringPtr("non_retryable"), CreatedAt: nowISO, UpdatedAt: finishedAt}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	h := NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime, Now: func() time.Time { return fixture.now.Add(time.Minute) }})
	startReq := httptest.NewRequest(http.MethodPost, "/api/v1/loops/"+loopID+"/start", nil)
	startRecorder := httptest.NewRecorder()
	h.ServeHTTP(startRecorder, startReq)
	if startRecorder.Code != http.StatusOK {
		t.Fatalf("start status = %d, want 200", startRecorder.Code)
	}

	items, err := services.Repositories.Queue.List(context.Background())
	if err != nil {
		t.Fatalf("Queue.List() error = %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("len(Queue.List()) = %d, want 2", len(items))
	}

	var replacement *storage.QueueItemRecord
	for index := range items {
		if items[index].ID != "queue_worker_terminal" {
			replacement = &items[index]
			break
		}
	}
	if replacement == nil {
		t.Fatal("replacement queue item = nil, want new queued item")
	}
	if replacement.Status != "queued" || replacement.Attempts != 0 || replacement.FinishedAt != nil || replacement.LastError != nil || replacement.LastErrorKind != nil {
		t.Fatalf("replacement queue item = %#v, want clean queued item", replacement)
	}
	if replacement.PayloadJSON == nil || *replacement.PayloadJSON != payload {
		t.Fatalf("replacement.PayloadJSON = %v, want %q", replacement.PayloadJSON, payload)
	}
}

func TestHandlerLoopStartCreatesQueueItemWhenLoopHasNoQueueHistory(t *testing.T) {
	fixture := newTestFixture(t)
	services := fixture.runtime.Services()
	nowISO := fixture.now.UTC().Format(javaScriptISOString)
	projectID := "project_no_history"
	loopID := "loop_worker_no_history"
	targetID := "project:project_no_history"
	metadata := `{"worker":{"title":"Implement worker loop","prompt":"Do the thing","repo":"acme/looper","baseBranch":"main"}}`

	if err := services.Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Looper", RepoPath: "/tmp/repos/looper", Archived: false, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := services.Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 4, ProjectID: projectID, Type: "worker", TargetType: "project", TargetID: &targetID, Repo: stringPtr("acme/looper"), Status: "paused", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	h := NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime, Now: func() time.Time { return fixture.now.Add(time.Minute) }})
	startReq := httptest.NewRequest(http.MethodPost, "/api/v1/loops/"+loopID+"/start", nil)
	startRecorder := httptest.NewRecorder()
	h.ServeHTTP(startRecorder, startReq)
	if startRecorder.Code != http.StatusOK {
		t.Fatalf("start status = %d, want 200", startRecorder.Code)
	}

	items, err := services.Repositories.Queue.List(context.Background())
	if err != nil {
		t.Fatalf("Queue.List() error = %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("len(Queue.List()) = %d, want 1", len(items))
	}
	assertEqual(t, items[0].LoopID != nil && *items[0].LoopID == loopID, true)
	assertEqual(t, items[0].Status, "queued")
	assertEqual(t, items[0].TargetID, targetID)
	if items[0].PayloadJSON == nil || *items[0].PayloadJSON == "" {
		t.Fatalf("queue payload = %v, want worker payload", items[0].PayloadJSON)
	}
}

func TestHandlerLoopStartRejectsConflictingActiveLoop(t *testing.T) {
	fixture := newTestFixture(t)
	services := fixture.runtime.Services()
	nowISO := fixture.now.UTC().Format(javaScriptISOString)
	projectID := "project_conflict"
	targetID := "pr:acme/looper:43"
	activeLoopID := "loop_active"
	pausedLoopID := "loop_paused"
	prNumber := int64(43)

	if err := services.Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Looper", RepoPath: "/tmp/repos/looper", Archived: false, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	for _, loop := range []storage.LoopRecord{
		{ID: activeLoopID, Seq: 5, ProjectID: projectID, Type: "reviewer", TargetType: "pull_request", TargetID: &targetID, Repo: stringPtr("acme/looper"), PRNumber: &prNumber, Status: "queued", CreatedAt: nowISO, UpdatedAt: nowISO},
		{ID: pausedLoopID, Seq: 6, ProjectID: projectID, Type: "reviewer", TargetType: "pull_request", TargetID: &targetID, Repo: stringPtr("acme/looper"), PRNumber: &prNumber, Status: "paused", CreatedAt: nowISO, UpdatedAt: nowISO},
	} {
		if err := services.Repositories.Loops.Upsert(context.Background(), loop); err != nil {
			t.Fatalf("Loops.Upsert(%s) error = %v", loop.ID, err)
		}
	}

	h := NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime, Now: func() time.Time { return fixture.now.Add(time.Minute) }})
	startReq := httptest.NewRequest(http.MethodPost, "/api/v1/loops/"+pausedLoopID+"/start", nil)
	startRecorder := httptest.NewRecorder()
	h.ServeHTTP(startRecorder, startReq)
	if startRecorder.Code != http.StatusConflict {
		t.Fatalf("start status = %d, want 409", startRecorder.Code)
	}
	body := parseJSONMap(t, startRecorder.Body.Bytes())
	errorMap := body["error"].(map[string]any)
	assertEqual(t, errorMap["code"], "LOOP_CONFLICT")

	pausedLoop, err := services.Repositories.Loops.GetByID(context.Background(), pausedLoopID)
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if pausedLoop == nil || pausedLoop.Status != "paused" {
		t.Fatalf("paused loop = %#v, want paused", pausedLoop)
	}
}

func TestHandlerWorkerRouteErrorsMatchArtifactCases(t *testing.T) {
	fixture := newTestFixture(t)
	seedLoopRouteData(t, fixture.runtime)
	seedWorkerPlannerArtifactsData(t, fixture.runtime, fixture.now)

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

	if err := fixture.runtime.Services().Repositories.PullRequestSnapshots.Upsert(context.Background(), storage.PullRequestSnapshotRecord{
		ID:         "prs_1",
		ProjectID:  "project_1",
		Repo:       "acme/looper",
		PRNumber:   42,
		HeadSHA:    "abc123",
		CapturedAt: fixture.now.UTC().Format(javaScriptISOString),
		CreatedAt:  fixture.now.UTC().Format(javaScriptISOString),
	}); err != nil {
		t.Fatalf("PullRequestSnapshots.Upsert(prs_1) error = %v", err)
	}

	tests := []struct {
		caseID string
		cfg    config.Config
		body   string
		setup  func(*testing.T)
	}{
		{
			caseID: "agent-not-configured",
			cfg: func() config.Config {
				cfg := fixture.config
				cfg.Agent.Vendor = nil
				return cfg
			}(),
			body: `{"projectId":"project_1","title":"Wire runtime","prompt":"Wire runtime","repo":"acme/looper","baseBranch":"main"}`,
		},
		{
			caseID: "project-ambiguous",
			cfg:    fixture.config,
			body:   `{"repo":"acme/looper","prompt":"Wire runtime","baseBranch":"main"}`,
			setup: func(t *testing.T) {
				t.Helper()
				nowISO := fixture.now.UTC().Format(javaScriptISOString)
				metadata := `{"repo":"acme/looper","worktreeRoot":null,"source":"api"}`
				baseBranch := "main"
				if err := fixture.runtime.Services().Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{
					ID:           "project_2",
					Name:         "Looper 2",
					RepoPath:     "/tmp/repos/looper-2",
					BaseBranch:   &baseBranch,
					Archived:     false,
					MetadataJSON: &metadata,
					CreatedAt:    nowISO,
					UpdatedAt:    nowISO,
				}); err != nil {
					t.Fatalf("Projects.Upsert(project_2) error = %v", err)
				}
			},
		},
		{
			caseID: "pull-request-not-found",
			cfg:    fixture.config,
			body:   `{"projectId":"project_1","repo":"acme/looper","prNumber":999,"baseBranch":"main"}`,
		},
		{
			caseID: "pull-request-project-mismatch",
			cfg:    fixture.config,
			body:   `{"projectId":"project_2","repo":"acme/looper","prNumber":42,"baseBranch":"main"}`,
			setup: func(t *testing.T) {
				t.Helper()
				nowISO := fixture.now.UTC().Format(javaScriptISOString)
				metadata := `{"repo":"other/repo","worktreeRoot":null,"source":"api"}`
				baseBranch := "main"
				if err := fixture.runtime.Services().Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{
					ID:           "project_2",
					Name:         "Mismatch",
					RepoPath:     "/tmp/repos/mismatch",
					BaseBranch:   &baseBranch,
					Archived:     false,
					MetadataJSON: &metadata,
					CreatedAt:    nowISO,
					UpdatedAt:    nowISO,
				}); err != nil {
					t.Fatalf("Projects.Upsert(project_2 mismatch) error = %v", err)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.caseID, func(t *testing.T) {
			if tt.setup != nil {
				tt.setup(t)
			}
			req := httptest.NewRequest(http.MethodPost, "/api/v1/workers", bytes.NewReader([]byte(tt.body)))
			req.Header.Set("x-request-id", "error-request-id")
			req.Header.Set("content-type", "application/json")
			recorder := httptest.NewRecorder()
			NewHandler(Context{Config: tt.cfg, Runtime: fixture.runtime, Now: func() time.Time { return fixture.now.Add(time.Minute) }}).ServeHTTP(recorder, req)

			want := findArtifactCase(t, artifact.Cases, tt.caseID)
			if recorder.Code != want.ExpectedStatus {
				t.Fatalf("status = %d, want %d", recorder.Code, want.ExpectedStatus)
			}
			assertErrorArtifactMatch(t, parseJSONMap(t, recorder.Body.Bytes()), want)
		})
	}
}

func TestHandlerWorkersCreateStoresUnwrappedQueuePayloadJSON(t *testing.T) {
	fixture := newTestFixture(t)
	seedWorkerPlannerArtifactsData(t, fixture.runtime, fixture.now)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/workers", bytes.NewReader([]byte(`{"projectId":"project_1","title":"Wire runtime","prompt":"Wire runtime","repo":"acme/looper","baseBranch":"main"}`)))
	req.Header.Set("x-request-id", "fixture-request-id")
	req.Header.Set("content-type", "application/json")
	recorder := httptest.NewRecorder()

	NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime, Now: func() time.Time { return fixture.now.Add(time.Minute) }}).ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	responseBody := parseJSONMap(t, recorder.Body.Bytes())
	loopID := responseBody["data"].(map[string]any)["id"].(string)

	queueItems, err := fixture.runtime.Services().Repositories.Queue.List(context.Background())
	if err != nil {
		t.Fatalf("Queue.List() error = %v", err)
	}
	var queueItem *storage.QueueItemRecord
	for i := range queueItems {
		if queueItems[i].LoopID != nil && *queueItems[i].LoopID == loopID {
			queueItem = &queueItems[i]
			break
		}
	}
	if queueItem == nil || queueItem.PayloadJSON == nil {
		t.Fatalf("worker queue payload missing for loop %s", loopID)
	}
	payload := parseJSONMap(t, []byte(*queueItem.PayloadJSON))
	if _, ok := payload["worker"]; ok {
		t.Fatalf("queue payload should be unwrapped: %#v", payload)
	}
	assertEqual(t, payload["title"], "Wire runtime")
	assertEqual(t, payload["prompt"], "Wire runtime")
	assertEqual(t, payload["specPath"], nil)
	assertEqual(t, payload["repo"], "acme/looper")
	assertEqual(t, payload["baseBranch"], "main")
}

func TestHandlerWorkerAndPlannerCreateRejectActiveLoopConflicts(t *testing.T) {
	fixture := newTestFixture(t)
	seedWorkerPlannerArtifactsData(t, fixture.runtime, fixture.now)
	nowISO := fixture.now.UTC().Format(javaScriptISOString)
	if err := fixture.runtime.Services().Repositories.PullRequestSnapshots.Upsert(context.Background(), storage.PullRequestSnapshotRecord{
		ID:         "prs_conflict_1",
		ProjectID:  "project_1",
		Repo:       "acme/looper",
		PRNumber:   42,
		HeadSHA:    "abc123",
		CapturedAt: nowISO,
		CreatedAt:  nowISO,
	}); err != nil {
		t.Fatalf("PullRequestSnapshots.Upsert(prs_conflict_1) error = %v", err)
	}

	if err := fixture.runtime.Services().Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{
		ID:         "loop_existing_worker",
		Seq:        1,
		ProjectID:  "project_1",
		Type:       "worker",
		TargetType: "pull_request",
		TargetID:   stringPtr("pr:acme/looper:42"),
		Repo:       stringPtr("acme/looper"),
		PRNumber:   int64Ptr(42),
		Status:     "queued",
		CreatedAt:  nowISO,
		UpdatedAt:  nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert(loop_existing_worker) error = %v", err)
	}
	if err := fixture.runtime.Services().Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{
		ID:         "loop_existing_planner",
		Seq:        2,
		ProjectID:  "project_1",
		Type:       "planner",
		TargetType: "issue",
		TargetID:   stringPtr("issue:acme/looper:77"),
		Repo:       stringPtr("acme/looper"),
		Status:     "running",
		CreatedAt:  nowISO,
		UpdatedAt:  nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert(loop_existing_planner) error = %v", err)
	}

	h := NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime, Now: func() time.Time { return fixture.now.Add(time.Minute) }})

	workerReq := httptest.NewRequest(http.MethodPost, "/api/v1/workers", bytes.NewReader([]byte(`{"projectId":"project_1","repo":"acme/looper","prNumber":42,"baseBranch":"main"}`)))
	workerReq.Header.Set("x-request-id", "error-request-id")
	workerReq.Header.Set("content-type", "application/json")
	workerRecorder := httptest.NewRecorder()
	h.ServeHTTP(workerRecorder, workerReq)
	if workerRecorder.Code != http.StatusConflict {
		t.Fatalf("worker status = %d, want 409", workerRecorder.Code)
	}
	workerBody := parseJSONMap(t, workerRecorder.Body.Bytes())
	workerError := workerBody["error"].(map[string]any)
	assertEqual(t, workerError["code"], "LOOP_CONFLICT")

	plannerReq := httptest.NewRequest(http.MethodPost, "/api/v1/planners", bytes.NewReader([]byte(`{"projectId":"project_1","issueNumber":77}`)))
	plannerReq.Header.Set("x-request-id", "error-request-id")
	plannerReq.Header.Set("content-type", "application/json")
	plannerRecorder := httptest.NewRecorder()
	h.ServeHTTP(plannerRecorder, plannerReq)
	if plannerRecorder.Code != http.StatusConflict {
		t.Fatalf("planner status = %d, want 409", plannerRecorder.Code)
	}
	plannerBody := parseJSONMap(t, plannerRecorder.Body.Bytes())
	plannerError := plannerBody["error"].(map[string]any)
	assertEqual(t, plannerError["code"], "LOOP_CONFLICT")
}

func TestAssertUniqueActiveLoopCompatAllowsWaitingReviewerRerun(t *testing.T) {
	existing := []storage.LoopRecord{{
		ID:         "loop_waiting",
		ProjectID:  "project_1",
		Type:       string(domain.LoopTypeReviewer),
		TargetType: string(domain.LoopTargetTypePullRequest),
		Repo:       stringPtr("acme/looper"),
		PRNumber:   int64Ptr(42),
		Status:     string(domain.LoopStatusWaiting),
	}}
	target := domain.LoopTarget{TargetType: domain.LoopTargetTypePullRequest, Repo: "acme/looper", PRNumber: 42}
	if err := assertUniqueActiveLoopCompat(existing, "loop_new", "project_1", domain.LoopTypeReviewer, target, domain.LoopStatusQueued); err != nil {
		t.Fatalf("assertUniqueActiveLoopCompat() error = %v, want waiting loop ignored for conflict checks", err)
	}
}

func TestHandlerWorkerCreateUsesProjectScopedPullRequestSnapshot(t *testing.T) {
	fixture := newTestFixture(t)
	seedWorkerPlannerArtifactsData(t, fixture.runtime, fixture.now)
	nowISO := fixture.now.UTC().Format(javaScriptISOString)
	baseBranch := "main"
	metadata := `{"repo":"acme/looper","worktreeRoot":null,"source":"api"}`
	if err := fixture.runtime.Services().Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID:           "project_2",
		Name:         "Looper Duplicate",
		RepoPath:     "/tmp/repos/looper-duplicate",
		BaseBranch:   &baseBranch,
		Archived:     false,
		MetadataJSON: &metadata,
		CreatedAt:    nowISO,
		UpdatedAt:    nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert(project_2) error = %v", err)
	}
	if err := fixture.runtime.Services().Repositories.PullRequestSnapshots.Upsert(context.Background(), storage.PullRequestSnapshotRecord{
		ID:         "prs_project_1_latest",
		ProjectID:  "project_1",
		Repo:       "acme/looper",
		PRNumber:   42,
		HeadSHA:    "head-project-1",
		CapturedAt: fixture.now.UTC().Format(javaScriptISOString),
		CreatedAt:  nowISO,
	}); err != nil {
		t.Fatalf("PullRequestSnapshots.Upsert(project_1) error = %v", err)
	}
	if err := fixture.runtime.Services().Repositories.PullRequestSnapshots.Upsert(context.Background(), storage.PullRequestSnapshotRecord{
		ID:         "prs_project_2_latest",
		ProjectID:  "project_2",
		Repo:       "acme/looper",
		PRNumber:   42,
		HeadSHA:    "head-project-2",
		CapturedAt: fixture.now.Add(time.Minute).UTC().Format(javaScriptISOString),
		CreatedAt:  nowISO,
	}); err != nil {
		t.Fatalf("PullRequestSnapshots.Upsert(project_2) error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/workers", bytes.NewReader([]byte(`{"projectId":"project_1","repo":"acme/looper","prNumber":42,"baseBranch":"main"}`)))
	req.Header.Set("x-request-id", "error-request-id")
	req.Header.Set("content-type", "application/json")
	recorder := httptest.NewRecorder()

	NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime, Now: func() time.Time { return fixture.now.Add(2 * time.Minute) }}).ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	body := parseJSONMap(t, recorder.Body.Bytes())
	data := body["data"].(map[string]any)
	assertEqual(t, data["status"], "queued")
	queueItems, err := fixture.runtime.Services().Repositories.Queue.List(context.Background())
	if err != nil {
		t.Fatalf("Queue.List() error = %v", err)
	}
	if len(queueItems) != 1 {
		t.Fatalf("Queue.List() = %#v, want one enqueued worker", queueItems)
	}
	if queueItems[0].ProjectID == nil || *queueItems[0].ProjectID != "project_1" {
		t.Fatalf("queueItems[0].ProjectID = %#v, want project_1", queueItems[0].ProjectID)
	}
}

func TestHandlerWorkerCreateRejectsRepoMismatchForExplicitProject(t *testing.T) {
	fixture := newTestFixture(t)
	seedWorkerPlannerArtifactsData(t, fixture.runtime, fixture.now)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/workers", bytes.NewReader([]byte(`{"projectId":"project_1","title":"Wire runtime","prompt":"Wire runtime","repo":"other/repo","baseBranch":"main"}`)))
	req.Header.Set("x-request-id", "error-request-id")
	req.Header.Set("content-type", "application/json")
	recorder := httptest.NewRecorder()

	NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime, Now: func() time.Time { return fixture.now.Add(time.Minute) }}).ServeHTTP(recorder, req)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}
	body := parseJSONMap(t, recorder.Body.Bytes())
	errMap := body["error"].(map[string]any)
	assertEqual(t, errMap["code"], "VALIDATION_FAILED")
	assertEqual(t, errMap["message"], "project project_1 is configured for repo acme/looper, not other/repo")
	queueItems, err := fixture.runtime.Services().Repositories.Queue.List(context.Background())
	if err != nil {
		t.Fatalf("Queue.List() error = %v", err)
	}
	if len(queueItems) != 0 {
		t.Fatalf("Queue.List() = %#v, want no enqueued worker", queueItems)
	}
}

func TestHandlerCreateLoopRejectsPullRequestRepoMismatchForExplicitProject(t *testing.T) {
	fixture := newTestFixture(t)
	seedWorkerPlannerArtifactsData(t, fixture.runtime, fixture.now)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/loops", bytes.NewReader([]byte(`{"projectId":"project_1","type":"reviewer","targetType":"pull_request","repo":"other/repo","prNumber":42}`)))
	req.Header.Set("x-request-id", "error-request-id")
	req.Header.Set("content-type", "application/json")
	recorder := httptest.NewRecorder()

	NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime, Now: func() time.Time { return fixture.now.Add(time.Minute) }}).ServeHTTP(recorder, req)

	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", recorder.Code)
	}
	body := parseJSONMap(t, recorder.Body.Bytes())
	errMap := body["error"].(map[string]any)
	assertEqual(t, errMap["code"], "PULL_REQUEST_PROJECT_MISMATCH")
	assertEqual(t, errMap["message"], "Pull request other/repo#42 does not belong to project project_1")
	queueItems, err := fixture.runtime.Services().Repositories.Queue.List(context.Background())
	if err != nil {
		t.Fatalf("Queue.List() error = %v", err)
	}
	if len(queueItems) != 0 {
		t.Fatalf("Queue.List() = %#v, want no enqueued loop", queueItems)
	}
}

func TestHandlerCreateLoopRejectsIssueRepoMismatchForExplicitProject(t *testing.T) {
	fixture := newTestFixture(t)
	seedWorkerPlannerArtifactsData(t, fixture.runtime, fixture.now)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/loops", bytes.NewReader([]byte(`{"projectId":"project_1","type":"planner","targetType":"issue","repo":"other/repo","issueNumber":77}`)))
	req.Header.Set("x-request-id", "error-request-id")
	req.Header.Set("content-type", "application/json")
	recorder := httptest.NewRecorder()

	NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime, Now: func() time.Time { return fixture.now.Add(time.Minute) }}).ServeHTTP(recorder, req)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}
	body := parseJSONMap(t, recorder.Body.Bytes())
	errMap := body["error"].(map[string]any)
	assertEqual(t, errMap["code"], "VALIDATION_FAILED")
	assertEqual(t, errMap["message"], "project project_1 is configured for repo acme/looper, not other/repo")
	queueItems, err := fixture.runtime.Services().Repositories.Queue.List(context.Background())
	if err != nil {
		t.Fatalf("Queue.List() error = %v", err)
	}
	if len(queueItems) != 0 {
		t.Fatalf("Queue.List() = %#v, want no enqueued loop", queueItems)
	}
}

func TestHandlerCreateLoopRejectsUnsupportedLoopType(t *testing.T) {
	fixture := newTestFixture(t)
	seedWorkerPlannerArtifactsData(t, fixture.runtime, fixture.now)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/loops", bytes.NewReader([]byte(`{"projectId":"project_1","type":"reveiwer","targetType":"pull_request","repo":"acme/looper","prNumber":42}`)))
	req.Header.Set("x-request-id", "error-request-id")
	req.Header.Set("content-type", "application/json")
	recorder := httptest.NewRecorder()

	NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime, Now: func() time.Time { return fixture.now.Add(time.Minute) }}).ServeHTTP(recorder, req)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}
	body := parseJSONMap(t, recorder.Body.Bytes())
	errMap := body["error"].(map[string]any)
	assertEqual(t, errMap["code"], "VALIDATION_FAILED")
	assertEqual(t, errMap["message"], "loop.type must be one of: planner, reviewer, worker, fixer")

	loops, err := fixture.runtime.Services().Repositories.Loops.List(context.Background())
	if err != nil {
		t.Fatalf("Loops.List() error = %v", err)
	}
	for _, loop := range loops {
		if loop.Type == "reveiwer" {
			t.Fatalf("persisted unsupported loop type: %#v", loop)
		}
	}
}

func TestHandlerCreateLoopRejectsUnsupportedLoopStatus(t *testing.T) {
	fixture := newTestFixture(t)
	seedWorkerPlannerArtifactsData(t, fixture.runtime, fixture.now)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/loops", bytes.NewReader([]byte(`{"projectId":"project_1","type":"planner","targetType":"issue","repo":"acme/looper","issueNumber":42,"status":"bogus"}`)))
	req.Header.Set("x-request-id", "error-request-id")
	req.Header.Set("content-type", "application/json")
	recorder := httptest.NewRecorder()

	NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime, Now: func() time.Time { return fixture.now.Add(time.Minute) }}).ServeHTTP(recorder, req)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}
	body := parseJSONMap(t, recorder.Body.Bytes())
	errMap := body["error"].(map[string]any)
	assertEqual(t, errMap["code"], "VALIDATION_FAILED")
	assertEqual(t, errMap["message"], "loop.status must be one of: idle, queued, running, paused, waiting, stopped, terminated, completed, failed, interrupted")

	loops, err := fixture.runtime.Services().Repositories.Loops.List(context.Background())
	if err != nil {
		t.Fatalf("Loops.List() error = %v", err)
	}
	for _, loop := range loops {
		if loop.Type == "planner" && loop.Status == "bogus" {
			t.Fatalf("persisted unsupported loop status: %#v", loop)
		}
	}
}

func TestHandlerCreateLoopRejectsIncompatibleLoopTypeAndTarget(t *testing.T) {
	fixture := newTestFixture(t)
	seedWorkerPlannerArtifactsData(t, fixture.runtime, fixture.now)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/loops", bytes.NewReader([]byte(`{"projectId":"project_1","type":"reviewer","targetType":"project","targetId":"project_1","status":"paused"}`)))
	req.Header.Set("x-request-id", "error-request-id")
	req.Header.Set("content-type", "application/json")
	recorder := httptest.NewRecorder()

	NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime, Now: func() time.Time { return fixture.now.Add(time.Minute) }}).ServeHTTP(recorder, req)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}
	body := parseJSONMap(t, recorder.Body.Bytes())
	errMap := body["error"].(map[string]any)
	assertEqual(t, errMap["code"], "VALIDATION_FAILED")
	assertEqual(t, errMap["message"], "reviewer loops must target a pull request")

	loops, err := fixture.runtime.Services().Repositories.Loops.List(context.Background())
	if err != nil {
		t.Fatalf("Loops.List() error = %v", err)
	}
	for _, loop := range loops {
		if loop.ProjectID == "project_1" && loop.Type == "reviewer" && loop.TargetType == "project" {
			t.Fatalf("persisted incompatible loop: %#v", loop)
		}
	}

	queueItems, err := fixture.runtime.Services().Repositories.Queue.List(context.Background())
	if err != nil {
		t.Fatalf("Queue.List() error = %v", err)
	}
	for _, item := range queueItems {
		if item.Type == "reviewer" && item.TargetType == "project" {
			t.Fatalf("persisted incompatible queue item: %#v", item)
		}
	}
}

func TestHandlerCreateLoopRejectsWorkerAndPlannerWithoutAgentConfigured(t *testing.T) {
	fixture := newTestFixture(t)
	seedWorkerPlannerArtifactsData(t, fixture.runtime, fixture.now)
	configWithoutAgent := fixture.config
	configWithoutAgent.Agent.Vendor = nil

	tests := []struct {
		name    string
		body    string
		message string
	}{
		{
			name:    "worker",
			body:    `{"projectId":"project_1","type":"worker","targetType":"project","targetId":"project_1","metadata":{"worker":{"title":"Implement worker loop","prompt":"Do the thing","repo":"acme/looper","baseBranch":"main"}}}`,
			message: "Cannot create worker loop without config.agent.vendor",
		},
		{
			name:    "planner",
			body:    `{"projectId":"project_1","type":"planner","targetType":"issue","repo":"acme/looper","issueNumber":123}`,
			message: "Cannot create planner loop without config.agent.vendor",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/loops", bytes.NewReader([]byte(tt.body)))
			req.Header.Set("x-request-id", "error-request-id")
			req.Header.Set("content-type", "application/json")
			recorder := httptest.NewRecorder()

			NewHandler(Context{Config: configWithoutAgent, Runtime: fixture.runtime, Now: func() time.Time { return fixture.now.Add(time.Minute) }}).ServeHTTP(recorder, req)

			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", recorder.Code)
			}
			body := parseJSONMap(t, recorder.Body.Bytes())
			errMap := body["error"].(map[string]any)
			assertEqual(t, errMap["code"], "AGENT_NOT_CONFIGURED")
			assertEqual(t, errMap["message"], tt.message)
		})
	}
}

func TestHandlerCreateLoopReviewerEnqueuesSchedulableManualLoop(t *testing.T) {
	fixture := newTestFixture(t)
	seedWorkerPlannerArtifactsData(t, fixture.runtime, fixture.now)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/loops", bytes.NewReader([]byte(`{"projectId":"project_1","type":"reviewer","targetType":"pull_request","repo":"acme/looper","prNumber":99,"metadata":{"manual":true,"followUpdates":false}}`)))
	req.Header.Set("x-request-id", "fixture-request-id")
	req.Header.Set("content-type", "application/json")
	recorder := httptest.NewRecorder()

	NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime, Now: func() time.Time { return fixture.now.Add(time.Minute) }}).ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}

	resp := parseJSONMap(t, recorder.Body.Bytes())
	data := resp["data"].(map[string]any)
	loopID := data["id"].(string)
	assertEqual(t, data["status"], "queued")

	loop, err := fixture.runtime.Services().Repositories.Loops.GetByID(context.Background(), loopID)
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if loop == nil {
		t.Fatal("Loops.GetByID() = nil, want created loop")
	}
	if loop.MetadataJSON == nil {
		t.Fatal("loop.MetadataJSON = nil, want manual metadata")
	}
	metadata := parseJSONObject(loop.MetadataJSON)
	assertEqual(t, metadata["manual"], true)
	assertEqual(t, metadata["followUpdates"], false)
	loopMeta := metadata["loop"].(map[string]any)
	assertEqual(t, loopMeta["enabled"], false)
	assertEqual(t, loopMeta["startTime"], fixture.now.Add(time.Minute).UTC().Format(javaScriptISOString))

	queueItems, err := fixture.runtime.Services().Repositories.Queue.List(context.Background())
	if err != nil {
		t.Fatalf("Queue.List() error = %v", err)
	}
	matched := []storage.QueueItemRecord{}
	for _, item := range queueItems {
		if item.LoopID != nil && *item.LoopID == loopID {
			matched = append(matched, item)
		}
	}
	if len(matched) != 1 {
		t.Fatalf("queue items for loop = %d, want 1", len(matched))
	}
	queue := matched[0]
	assertEqual(t, queue.Type, "reviewer")
	assertEqual(t, queue.Status, "queued")
	assertEqual(t, queue.TargetType, "pull_request")
	assertEqual(t, queue.TargetID, "pr:acme/looper:99")
	assertEqual(t, queue.DedupeKey, "reviewer:project_1:"+loopID+":acme/looper:99")
}

func TestHandlerCreateLoopReviewerBackfillsFollowUpdatesFromLoopEnabled(t *testing.T) {
	fixture := newTestFixture(t)
	seedWorkerPlannerArtifactsData(t, fixture.runtime, fixture.now)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/loops", bytes.NewReader([]byte(`{"projectId":"project_1","type":"reviewer","targetType":"pull_request","repo":"acme/looper","prNumber":99,"metadata":{"loop":{"enabled":false}}}`)))
	req.Header.Set("content-type", "application/json")
	recorder := httptest.NewRecorder()

	NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime, Now: func() time.Time { return fixture.now }}).ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	data := parseJSONMap(t, recorder.Body.Bytes())["data"].(map[string]any)
	loopID := data["id"].(string)
	loop, err := fixture.runtime.Services().Repositories.Loops.GetByID(context.Background(), loopID)
	if err != nil || loop == nil {
		t.Fatalf("Loops.GetByID() = (%#v, %v), want loop", loop, err)
	}
	metadata := parseJSONObject(loop.MetadataJSON)
	assertEqual(t, metadata["followUpdates"], false)
	loopMeta := metadata["loop"].(map[string]any)
	assertEqual(t, loopMeta["enabled"], false)
	assertEqual(t, loopMeta["startTime"], fixture.now.UTC().Format(javaScriptISOString))
}

func TestHandlerCreateLoopReviewerEnqueuesAcrossProjectsForSamePullRequest(t *testing.T) {
	fixture := newTestFixture(t)
	seedWorkerPlannerArtifactsData(t, fixture.runtime, fixture.now)
	nowISO := fixture.now.UTC().Format(javaScriptISOString)
	baseBranch := "main"
	metadata := `{"repo":"acme/looper","worktreeRoot":null,"source":"api"}`
	if err := fixture.runtime.Services().Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID:           "project_2",
		Name:         "Looper Duplicate",
		RepoPath:     "/tmp/repos/looper-duplicate",
		BaseBranch:   &baseBranch,
		Archived:     false,
		MetadataJSON: &metadata,
		CreatedAt:    nowISO,
		UpdatedAt:    nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert(project_2) error = %v", err)
	}
	if err := fixture.runtime.Services().Repositories.PullRequestSnapshots.Upsert(context.Background(), storage.PullRequestSnapshotRecord{
		ID:         "prs_project_2_99",
		ProjectID:  "project_2",
		Repo:       "acme/looper",
		PRNumber:   99,
		HeadSHA:    "head-project-2",
		CapturedAt: fixture.now.Add(time.Minute).UTC().Format(javaScriptISOString),
		CreatedAt:  nowISO,
	}); err != nil {
		t.Fatalf("PullRequestSnapshots.Upsert(project_2) error = %v", err)
	}
	project1ID := "project_1"
	loop1ID := "loop_existing"
	prNumber := int64(99)
	if err := fixture.runtime.Services().Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{
		ID:         loop1ID,
		Seq:        1,
		ProjectID:  project1ID,
		Type:       "reviewer",
		TargetType: "pull_request",
		TargetID:   stringPtr("pr:acme/looper:99"),
		Status:     "queued",
		CreatedAt:  nowISO,
		UpdatedAt:  nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert(existing) error = %v", err)
	}
	if err := fixture.runtime.Services().Repositories.Queue.Upsert(context.Background(), storage.QueueItemRecord{
		ID:          "queue_existing",
		ProjectID:   &project1ID,
		LoopID:      &loop1ID,
		Type:        "reviewer",
		TargetType:  "pull_request",
		TargetID:    "pr:acme/looper:99",
		Repo:        stringPtr("acme/looper"),
		PRNumber:    &prNumber,
		DedupeKey:   "reviewer:project_1:loop_existing:acme/looper:99",
		Priority:    storage.QueuePriorityReviewer,
		Status:      "queued",
		AvailableAt: nowISO,
		Attempts:    0,
		MaxAttempts: 3,
		CreatedAt:   nowISO,
		UpdatedAt:   nowISO,
	}); err != nil {
		t.Fatalf("Queue.Upsert(existing) error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/loops", bytes.NewReader([]byte(`{"projectId":"project_2","type":"reviewer","targetType":"pull_request","repo":"acme/looper","prNumber":99,"metadata":{"manual":true,"followUpdates":false}}`)))
	req.Header.Set("x-request-id", "fixture-request-id")
	req.Header.Set("content-type", "application/json")
	recorder := httptest.NewRecorder()

	NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime, Now: func() time.Time { return fixture.now.Add(2 * time.Minute) }}).ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}

	resp := parseJSONMap(t, recorder.Body.Bytes())
	data := resp["data"].(map[string]any)
	loopID := data["id"].(string)
	queueItems, err := fixture.runtime.Services().Repositories.Queue.List(context.Background())
	if err != nil {
		t.Fatalf("Queue.List() error = %v", err)
	}
	matched := []storage.QueueItemRecord{}
	for _, item := range queueItems {
		if item.LoopID != nil && *item.LoopID == loopID {
			matched = append(matched, item)
		}
	}
	if len(matched) != 1 {
		t.Fatalf("queue items for loop = %d, want 1", len(matched))
	}
	assertEqual(t, matched[0].DedupeKey, "reviewer:project_2:"+loopID+":acme/looper:99")
}

func TestHandlerCreateLoopReviewerTriggersSchedulerTickHook(t *testing.T) {
	fixture := newTestFixture(t)
	seedWorkerPlannerArtifactsData(t, fixture.runtime, fixture.now)

	triggered := 0
	h := NewHandler(Context{
		Config:  fixture.config,
		Runtime: fixture.runtime,
		Now:     func() time.Time { return fixture.now.Add(time.Minute) },
		TriggerSchedulerTick: func() {
			triggered++
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/loops", bytes.NewReader([]byte(`{"projectId":"project_1","type":"reviewer","targetType":"pull_request","repo":"acme/looper","prNumber":99,"metadata":{"manual":true,"followUpdates":false}}`)))
	req.Header.Set("x-request-id", "fixture-request-id")
	req.Header.Set("content-type", "application/json")
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	assertEqual(t, triggered, 1)
}

func TestHandlerCreateLoopPlannerEnqueuesSchedulableLoop(t *testing.T) {
	fixture := newTestFixture(t)
	seedWorkerPlannerArtifactsData(t, fixture.runtime, fixture.now)

	triggered := 0
	h := NewHandler(Context{
		Config:  fixture.config,
		Runtime: fixture.runtime,
		Now:     func() time.Time { return fixture.now.Add(time.Minute) },
		TriggerSchedulerTick: func() {
			triggered++
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/loops", bytes.NewReader([]byte(`{"projectId":"project_1","type":"planner","targetType":"issue","repo":"acme/looper","issueNumber":123}`)))
	req.Header.Set("x-request-id", "fixture-request-id")
	req.Header.Set("content-type", "application/json")
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}

	resp := parseJSONMap(t, recorder.Body.Bytes())
	data := resp["data"].(map[string]any)
	loopID := data["id"].(string)
	assertEqual(t, data["status"], "running")
	metadata := parseJSONMap(t, []byte(data["metadataJson"].(string)))
	assertEqual(t, metadata["manual"], true)
	assertEqual(t, metadata["issueNumber"], float64(123))

	queueItems, err := fixture.runtime.Services().Repositories.Queue.List(context.Background())
	if err != nil {
		t.Fatalf("Queue.List() error = %v", err)
	}
	matched := []storage.QueueItemRecord{}
	for _, item := range queueItems {
		if item.LoopID != nil && *item.LoopID == loopID {
			matched = append(matched, item)
		}
	}
	if len(matched) != 1 {
		t.Fatalf("queue items for loop = %d, want 1", len(matched))
	}
	queue := matched[0]
	assertEqual(t, queue.Type, "planner")
	assertEqual(t, queue.Status, "queued")
	assertEqual(t, queue.TargetType, "issue")
	assertEqual(t, queue.TargetID, "issue:acme/looper:123")
	assertEqual(t, queue.DedupeKey, "planner:project_1:"+loopID+":acme/looper:123")
	payload := parseJSONMap(t, []byte(*queue.PayloadJSON))
	assertEqual(t, payload["manual"], true)
	assertEqual(t, payload["issueNumber"], float64(123))
	assertEqual(t, triggered, 1)
}

func TestHandlerCreateLoopNormalizesProjectTargetID(t *testing.T) {
	fixture := newTestFixture(t)
	seedWorkerPlannerArtifactsData(t, fixture.runtime, fixture.now)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/loops", bytes.NewReader([]byte(`{"projectId":"project_1","type":"worker","targetType":"project","targetId":"project:project:project_1","metadata":{"worker":{"title":"Implement worker loop","prompt":"Do the thing","repo":"acme/looper","baseBranch":"main"}},"status":"paused"}`)))
	req.Header.Set("x-request-id", "fixture-request-id")
	req.Header.Set("content-type", "application/json")
	recorder := httptest.NewRecorder()

	NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime, Now: func() time.Time { return fixture.now.Add(time.Minute) }}).ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}

	resp := parseJSONMap(t, recorder.Body.Bytes())
	data := resp["data"].(map[string]any)
	assertEqual(t, data["targetId"], "project:project_1")
	loopID := data["id"].(string)

	loop, err := fixture.runtime.Services().Repositories.Loops.GetByID(context.Background(), loopID)
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if loop == nil || loop.TargetID == nil {
		t.Fatalf("loop = %#v, want project target id", loop)
	}
	assertEqual(t, *loop.TargetID, "project:project_1")
}

func TestHandlerLoopStartPlannerIgnoresOtherProjectsScopedDedupe(t *testing.T) {
	fixture := newTestFixture(t)
	services := fixture.runtime.Services()
	now := fixture.now.UTC()
	nowISO := now.Format(javaScriptISOString)
	targetID := "issue:acme/looper:77"
	projectID := "project_planner_a"
	loopID := "loop_planner_a"
	otherProjectID := "project_planner_b"
	otherLoopID := "loop_planner_b"
	payload := `{"issueNumber":77}`
	finishedAt := now.Add(-time.Minute).Format(javaScriptISOString)
	otherAvailableAt := now.Add(-2 * time.Minute).Format(javaScriptISOString)

	for _, project := range []storage.ProjectRecord{
		{ID: projectID, Name: "Planner A", RepoPath: "/tmp/repos/planner-a", Archived: false, CreatedAt: nowISO, UpdatedAt: nowISO},
		{ID: otherProjectID, Name: "Planner B", RepoPath: "/tmp/repos/planner-b", Archived: false, CreatedAt: nowISO, UpdatedAt: nowISO},
	} {
		if err := services.Repositories.Projects.Upsert(context.Background(), project); err != nil {
			t.Fatalf("Projects.Upsert(%s) error = %v", project.ID, err)
		}
	}
	if err := services.Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 1, ProjectID: projectID, Type: "planner", TargetType: "issue", TargetID: &targetID, Repo: stringPtr("acme/looper"), Status: "paused", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert(primary) error = %v", err)
	}
	if err := services.Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{ID: otherLoopID, Seq: 2, ProjectID: otherProjectID, Type: "planner", TargetType: "issue", TargetID: &targetID, Repo: stringPtr("acme/looper"), Status: "running", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert(other) error = %v", err)
	}
	if err := services.Repositories.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: "queue_planner_terminal", ProjectID: &projectID, LoopID: &loopID, Type: "planner", TargetType: "issue", TargetID: targetID, Repo: stringPtr("acme/looper"), DedupeKey: "planner:project_planner_a:loop_planner_a:acme/looper:77", Priority: storage.QueuePriorityPlanner, Status: "failed", AvailableAt: nowISO, Attempts: 1, MaxAttempts: 3, LockKey: stringPtr(targetID), PayloadJSON: &payload, FinishedAt: &finishedAt, LastError: stringPtr("boom"), LastErrorKind: stringPtr("non_retryable"), CreatedAt: nowISO, UpdatedAt: finishedAt}); err != nil {
		t.Fatalf("Queue.Upsert(primary terminal) error = %v", err)
	}
	if err := services.Repositories.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: "queue_planner_other_active", ProjectID: &otherProjectID, LoopID: &otherLoopID, Type: "planner", TargetType: "issue", TargetID: targetID, Repo: stringPtr("acme/looper"), DedupeKey: "planner:project_planner_b:loop_planner_b:acme/looper:77", Priority: storage.QueuePriorityPlanner, Status: "queued", AvailableAt: otherAvailableAt, Attempts: 0, MaxAttempts: 3, LockKey: stringPtr(targetID), PayloadJSON: &payload, CreatedAt: otherAvailableAt, UpdatedAt: otherAvailableAt}); err != nil {
		t.Fatalf("Queue.Upsert(other active) error = %v", err)
	}

	h := NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime, Now: func() time.Time { return now.Add(time.Minute) }})
	startReq := httptest.NewRequest(http.MethodPost, "/api/v1/loops/"+loopID+"/start", nil)
	startRecorder := httptest.NewRecorder()
	h.ServeHTTP(startRecorder, startReq)
	if startRecorder.Code != http.StatusOK {
		t.Fatalf("start status = %d, want 200", startRecorder.Code)
	}

	items, err := services.Repositories.Queue.List(context.Background())
	if err != nil {
		t.Fatalf("Queue.List() error = %v", err)
	}
	matched := []storage.QueueItemRecord{}
	for _, item := range items {
		if item.LoopID != nil && *item.LoopID == loopID {
			matched = append(matched, item)
		}
	}
	if len(matched) != 2 {
		t.Fatalf("queue items for loop = %d, want 2", len(matched))
	}

	replacements := 0
	for _, item := range matched {
		if item.ID == "queue_planner_terminal" {
			continue
		}
		replacements++
		assertEqual(t, item.Status, "queued")
		assertEqual(t, item.DedupeKey, "planner:project_planner_a:loop_planner_a:acme/looper:77")
	}
	assertEqual(t, replacements, 1)
}

func TestHandlerCreateLoopFixerEnqueuesSchedulableManualLoop(t *testing.T) {
	fixture := newTestFixture(t)
	seedWorkerPlannerArtifactsData(t, fixture.runtime, fixture.now)

	triggered := 0
	h := NewHandler(Context{
		Config:  fixture.config,
		Runtime: fixture.runtime,
		Now:     func() time.Time { return fixture.now.Add(time.Minute) },
		TriggerSchedulerTick: func() {
			triggered++
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/loops", bytes.NewReader([]byte(`{"projectId":"project_1","type":"fixer","targetType":"pull_request","repo":"acme/looper","prNumber":99}`)))
	req.Header.Set("x-request-id", "fixture-request-id")
	req.Header.Set("content-type", "application/json")
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}

	resp := parseJSONMap(t, recorder.Body.Bytes())
	data := resp["data"].(map[string]any)
	loopID := data["id"].(string)
	assertEqual(t, data["status"], "queued")

	queueItems, err := fixture.runtime.Services().Repositories.Queue.List(context.Background())
	if err != nil {
		t.Fatalf("Queue.List() error = %v", err)
	}
	matched := []storage.QueueItemRecord{}
	for _, item := range queueItems {
		if item.LoopID != nil && *item.LoopID == loopID {
			matched = append(matched, item)
		}
	}
	if len(matched) != 1 {
		t.Fatalf("queue items for loop = %d, want 1", len(matched))
	}
	queue := matched[0]
	assertEqual(t, queue.Type, "fixer")
	assertEqual(t, queue.Status, "queued")
	assertEqual(t, queue.TargetType, "pull_request")
	assertEqual(t, queue.TargetID, "pr:acme/looper:99")
	assertEqual(t, queue.DedupeKey, "fixer:"+loopID)
	assertEqual(t, triggered, 1)
}

func TestHandlerCreateLoopWorkerEnqueuesSchedulableManualLoop(t *testing.T) {
	fixture := newTestFixture(t)
	seedWorkerPlannerArtifactsData(t, fixture.runtime, fixture.now)

	triggered := 0
	h := NewHandler(Context{
		Config:  fixture.config,
		Runtime: fixture.runtime,
		Now:     func() time.Time { return fixture.now.Add(time.Minute) },
		TriggerSchedulerTick: func() {
			triggered++
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/loops", bytes.NewReader([]byte(`{"projectId":"project_1","type":"worker","targetType":"pull_request","repo":"acme/looper","prNumber":99,"metadata":{"worker":{"title":"Implement worker loop","prompt":"Do the thing","repo":"acme/looper","baseBranch":"main"}}}`)))
	req.Header.Set("x-request-id", "fixture-request-id")
	req.Header.Set("content-type", "application/json")
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}

	resp := parseJSONMap(t, recorder.Body.Bytes())
	data := resp["data"].(map[string]any)
	loopID := data["id"].(string)
	assertEqual(t, data["status"], "queued")

	queueItems, err := fixture.runtime.Services().Repositories.Queue.List(context.Background())
	if err != nil {
		t.Fatalf("Queue.List() error = %v", err)
	}
	matched := []storage.QueueItemRecord{}
	for _, item := range queueItems {
		if item.LoopID != nil && *item.LoopID == loopID {
			matched = append(matched, item)
		}
	}
	if len(matched) != 1 {
		t.Fatalf("queue items for loop = %d, want 1", len(matched))
	}
	queue := matched[0]
	assertEqual(t, queue.Type, "worker")
	assertEqual(t, queue.Status, "queued")
	assertEqual(t, queue.TargetType, "pull_request")
	assertEqual(t, queue.TargetID, "pr:acme/looper:99")
	assertEqual(t, queue.DedupeKey, "worker:project_1:acme/looper:99")
	if queue.PayloadJSON == nil {
		t.Fatal("queue.PayloadJSON = nil, want worker payload")
	}
	payload := parseJSONObject(queue.PayloadJSON)
	assertEqual(t, payload["title"], "Implement worker loop")
	assertEqual(t, payload["prompt"], "Do the thing")
	assertEqual(t, payload["baseBranch"], "main")
	assertEqual(t, triggered, 1)
}

func TestHandlerWorkerCreateUsesOnlyNewestMatchingPlannerLoop(t *testing.T) {
	fixture := newTestFixture(t)
	seedWorkerPlannerArtifactsData(t, fixture.runtime, fixture.now)
	nowISO := fixture.now.UTC().Format(javaScriptISOString)
	olderISO := fixture.now.Add(-time.Minute).UTC().Format(javaScriptISOString)
	newerISO := fixture.now.Add(time.Minute).UTC().Format(javaScriptISOString)
	issueTargetID := "issue:acme/looper:77"
	openPayloadBytes, err := json.Marshal(map[string]any{
		"detail": map[string]any{
			"state": "OPEN",
		},
	})
	if err != nil {
		t.Fatalf("json.Marshal(open payload) error = %v", err)
	}
	openPayloadJSON := string(openPayloadBytes)
	closedPayloadBytes, err := json.Marshal(map[string]any{
		"detail": map[string]any{
			"state": "CLOSED",
		},
	})
	if err != nil {
		t.Fatalf("json.Marshal(closed payload) error = %v", err)
	}
	closedPayloadJSON := string(closedPayloadBytes)
	olderPRNumber := int64(42)
	newerPRNumber := int64(43)
	olderMetadata := `{"prNumber":42,"specPath":"specs/older-open.md"}`
	newerMetadata := `{"prNumber":43,"specPath":"specs/newer-closed.md"}`
	if err := fixture.runtime.Services().Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{
		ID:           "loop_planner_older_open",
		Seq:          1,
		ProjectID:    "project_1",
		Type:         "planner",
		TargetType:   "issue",
		TargetID:     &issueTargetID,
		Repo:         stringPtr("acme/looper"),
		PRNumber:     &olderPRNumber,
		Status:       "completed",
		MetadataJSON: &olderMetadata,
		CreatedAt:    olderISO,
		UpdatedAt:    olderISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert(loop_planner_older_open) error = %v", err)
	}
	if err := fixture.runtime.Services().Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{
		ID:           "loop_planner_newer_closed",
		Seq:          2,
		ProjectID:    "project_1",
		Type:         "planner",
		TargetType:   "issue",
		TargetID:     &issueTargetID,
		Repo:         stringPtr("acme/looper"),
		PRNumber:     &newerPRNumber,
		Status:       "completed",
		MetadataJSON: &newerMetadata,
		CreatedAt:    newerISO,
		UpdatedAt:    newerISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert(loop_planner_newer_closed) error = %v", err)
	}
	if err := fixture.runtime.Services().Repositories.PullRequestSnapshots.Upsert(context.Background(), storage.PullRequestSnapshotRecord{
		ID:          "prs_planner_older_open",
		ProjectID:   "project_1",
		Repo:        "acme/looper",
		PRNumber:    olderPRNumber,
		HeadSHA:     "head-older-open",
		PayloadJSON: &openPayloadJSON,
		CapturedAt:  olderISO,
		CreatedAt:   nowISO,
	}); err != nil {
		t.Fatalf("PullRequestSnapshots.Upsert(prs_planner_older_open) error = %v", err)
	}
	if err := fixture.runtime.Services().Repositories.PullRequestSnapshots.Upsert(context.Background(), storage.PullRequestSnapshotRecord{
		ID:          "prs_planner_newer_closed",
		ProjectID:   "project_1",
		Repo:        "acme/looper",
		PRNumber:    newerPRNumber,
		HeadSHA:     "head-newer-closed",
		PayloadJSON: &closedPayloadJSON,
		CapturedAt:  newerISO,
		CreatedAt:   nowISO,
	}); err != nil {
		t.Fatalf("PullRequestSnapshots.Upsert(prs_planner_newer_closed) error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/workers", bytes.NewReader([]byte(`{"projectId":"project_1","repo":"acme/looper","issueNumber":77,"baseBranch":"main"}`)))
	req.Header.Set("x-request-id", "fixture-request-id")
	req.Header.Set("content-type", "application/json")
	recorder := httptest.NewRecorder()

	NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime, Now: func() time.Time { return fixture.now.Add(2 * time.Minute) }}).ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	body := parseJSONMap(t, recorder.Body.Bytes())
	data := body["data"].(map[string]any)
	loopID := data["id"].(string)

	loop, err := fixture.runtime.Services().Repositories.Loops.GetByID(context.Background(), loopID)
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if loop == nil {
		t.Fatal("Loops.GetByID() = nil, want created worker loop")
	}
	assertEqual(t, loop.TargetType, "issue")
	if loop.PRNumber != nil {
		t.Fatalf("loop.PRNumber = %#v, want nil", loop.PRNumber)
	}
	assertEqual(t, derefString(loop.TargetID), "issue:acme/looper:77")

	queueItems, err := fixture.runtime.Services().Repositories.Queue.List(context.Background())
	if err != nil {
		t.Fatalf("Queue.List() error = %v", err)
	}
	var queueItem *storage.QueueItemRecord
	for i := range queueItems {
		if queueItems[i].LoopID != nil && *queueItems[i].LoopID == loopID {
			queueItem = &queueItems[i]
			break
		}
	}
	if queueItem == nil || queueItem.PayloadJSON == nil {
		t.Fatalf("worker queue payload missing for loop %s", loopID)
	}
	assertEqual(t, queueItem.TargetType, "issue")
	assertEqual(t, queueItem.TargetID, "issue:acme/looper:77")
	assertEqual(t, derefString(queueItem.LockKey), "issue:acme/looper:77")
	assertEqual(t, queueItem.DedupeKey, "worker:project_1:acme/looper:77")
	if queueItem.PRNumber != nil {
		t.Fatalf("queueItem.PRNumber = %#v, want nil", queueItem.PRNumber)
	}
	if queueItem.LockKey == nil || *queueItem.LockKey != "issue:acme/looper:77" {
		t.Fatalf("queueItem.LockKey = %#v, want issue lock key", queueItem.LockKey)
	}
	payload := parseJSONMap(t, []byte(*queueItem.PayloadJSON))
	assertEqual(t, payload["specPath"], "specs/newer-closed.md")
	if _, ok := payload["prNumber"]; ok {
		t.Fatalf("payload[\"prNumber\"] present = %#v, want omitted", payload["prNumber"])
	}
}

func TestHandlerWorkerCreatePreservesPlannerPRWhenSnapshotIsMissing(t *testing.T) {
	fixture := newTestFixture(t)
	seedWorkerPlannerArtifactsData(t, fixture.runtime, fixture.now)
	nowISO := fixture.now.UTC().Format(javaScriptISOString)
	issueTargetID := "issue:acme/looper:77"
	prNumber := int64(42)
	metadata := `{"prNumber":42,"specPath":"specs/planner.md"}`
	if err := fixture.runtime.Services().Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{
		ID:           "loop_planner_missing_snapshot",
		Seq:          1,
		ProjectID:    "project_1",
		Type:         "planner",
		TargetType:   "issue",
		TargetID:     &issueTargetID,
		Repo:         stringPtr("acme/looper"),
		PRNumber:     &prNumber,
		Status:       "running",
		MetadataJSON: &metadata,
		CreatedAt:    nowISO,
		UpdatedAt:    nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert(loop_planner_missing_snapshot) error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/workers", bytes.NewReader([]byte(`{"projectId":"project_1","repo":"acme/looper","issueNumber":77,"baseBranch":"main"}`)))
	req.Header.Set("x-request-id", "fixture-request-id")
	req.Header.Set("content-type", "application/json")
	recorder := httptest.NewRecorder()

	NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime, Now: func() time.Time { return fixture.now.Add(time.Minute) }}).ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	body := parseJSONMap(t, recorder.Body.Bytes())
	data := body["data"].(map[string]any)
	loopID := data["id"].(string)

	loop, err := fixture.runtime.Services().Repositories.Loops.GetByID(context.Background(), loopID)
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if loop == nil {
		t.Fatal("Loops.GetByID() = nil, want created worker loop")
	}
	assertEqual(t, loop.TargetType, "pull_request")
	if loop.PRNumber == nil || *loop.PRNumber != prNumber {
		t.Fatalf("loop.PRNumber = %#v, want %d", loop.PRNumber, prNumber)
	}

	queueItems, err := fixture.runtime.Services().Repositories.Queue.List(context.Background())
	if err != nil {
		t.Fatalf("Queue.List() error = %v", err)
	}
	var queueItem *storage.QueueItemRecord
	for i := range queueItems {
		if queueItems[i].LoopID != nil && *queueItems[i].LoopID == loopID {
			queueItem = &queueItems[i]
			break
		}
	}
	if queueItem == nil || queueItem.PayloadJSON == nil {
		t.Fatalf("worker queue payload missing for loop %s", loopID)
	}
	assertEqual(t, queueItem.TargetType, "pull_request")
	assertEqual(t, queueItem.TargetID, "pr:acme/looper:42")
	if queueItem.PRNumber == nil || *queueItem.PRNumber != prNumber {
		t.Fatalf("queueItem.PRNumber = %#v, want %d", queueItem.PRNumber, prNumber)
	}
	payload := parseJSONMap(t, []byte(*queueItem.PayloadJSON))
	assertEqual(t, payload["specPath"], "specs/planner.md")
	assertEqual(t, payload["prNumber"], float64(prNumber))
}

func TestHandlerPullRequestStatusUsesLatestRunAcrossLoops(t *testing.T) {
	fixture := newTestFixture(t)
	seedEventAndPullRequestRouteData(t, fixture.runtime)
	services := fixture.runtime.Services()

	if err := services.Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{
		ID:         "loop_fixer_1",
		Seq:        2,
		ProjectID:  "project_1",
		Type:       "fixer",
		TargetType: "pull_request",
		TargetID:   stringPtr("pr:acme/looper:42"),
		Repo:       stringPtr("acme/looper"),
		PRNumber:   int64Ptr(42),
		Status:     "queued",
		CreatedAt:  "2026-04-11T12:01:00.000Z",
		UpdatedAt:  "2026-04-11T12:01:00.000Z",
	}); err != nil {
		t.Fatalf("Loops.Upsert(loop_fixer_1) error = %v", err)
	}
	if err := services.Repositories.Runs.Upsert(context.Background(), storage.RunRecord{
		ID:        "run_reviewer_old",
		LoopID:    "loop_1",
		Status:    "running",
		StartedAt: "2026-04-11T12:00:00.000Z",
		CreatedAt: "2026-04-11T12:00:00.000Z",
		UpdatedAt: "2026-04-11T12:00:00.000Z",
	}); err != nil {
		t.Fatalf("Runs.Upsert(run_reviewer_old) error = %v", err)
	}
	if err := services.Repositories.Runs.Upsert(context.Background(), storage.RunRecord{
		ID:        "run_fixer_new",
		LoopID:    "loop_fixer_1",
		Status:    "failed",
		StartedAt: "2026-04-11T12:05:00.000Z",
		CreatedAt: "2026-04-11T12:05:00.000Z",
		UpdatedAt: "2026-04-11T12:06:00.000Z",
	}); err != nil {
		t.Fatalf("Runs.Upsert(run_fixer_new) error = %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/pull-requests/acme%2Flooper/42/status", nil)
	recorder := httptest.NewRecorder()
	NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime}).ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	body := parseJSONMap(t, recorder.Body.Bytes())
	data := body["data"].(map[string]any)
	loopStatus := data["loopStatus"].(map[string]any)
	assertEqual(t, loopStatus["latestRunStatus"], "failed")
	assertEqual(t, loopStatus["runningRunCount"], float64(2))
}

func TestSerializePullRequestListItemUsesProvidedLoopMatches(t *testing.T) {
	h := NewHandler(Context{})
	item := h.serializePullRequestListItem("acme/looper", 42, nil, []storage.LoopRecord{{
		ID:         "loop_reviewer_1",
		ProjectID:  "project_1",
		Type:       "reviewer",
		TargetType: "pull_request",
		Repo:       stringPtr("acme/looper"),
		PRNumber:   int64Ptr(42),
		Status:     "running",
		CreatedAt:  "2026-04-11T12:00:00.000Z",
		UpdatedAt:  "2026-04-11T12:00:00.000Z",
	}})

	if item.ProjectID == nil || *item.ProjectID != "project_1" {
		t.Fatalf("ProjectID = %v, want project_1", item.ProjectID)
	}
	if item.Reviewer == nil || *item.Reviewer != "running" {
		t.Fatalf("Reviewer = %v, want running", item.Reviewer)
	}
}

func TestIsPlannerPullRequestOpenReadsStructMarshaledStateKey(t *testing.T) {
	fixture := newTestFixture(t)
	nowISO := fixture.now.UTC().Format(javaScriptISOString)

	if err := fixture.runtime.Services().Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID:        "project_1",
		Name:      "Looper",
		RepoPath:  "/tmp/repos/looper",
		Archived:  false,
		CreatedAt: nowISO,
		UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	payloadBytes, err := json.Marshal(map[string]any{
		"detail": struct {
			State string
		}{
			State: "OPEN",
		},
	})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	payloadJSON := string(payloadBytes)

	if err := fixture.runtime.Services().Repositories.PullRequestSnapshots.Upsert(context.Background(), storage.PullRequestSnapshotRecord{
		ID:          "prs_planner_open",
		ProjectID:   "project_1",
		Repo:        "acme/looper",
		PRNumber:    42,
		HeadSHA:     "abc123",
		PayloadJSON: &payloadJSON,
		CapturedAt:  nowISO,
		CreatedAt:   nowISO,
	}); err != nil {
		t.Fatalf("PullRequestSnapshots.Upsert() error = %v", err)
	}

	h := NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime})
	if !h.isPlannerPullRequestOpen(context.Background(), "project_1", "acme/looper", 42) {
		t.Fatal("isPlannerPullRequestOpen() = false, want true")
	}
}

func TestHandlerWorkersCreateAllowsConcurrentProjectWorkers(t *testing.T) {
	fixture := newTestFixture(t)
	seedWorkerPlannerArtifactsData(t, fixture.runtime, fixture.now)
	nowISO := fixture.now.UTC().Format(javaScriptISOString)

	if err := fixture.runtime.Services().Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{
		ID:         "loop_existing_worker",
		Seq:        1,
		ProjectID:  "project_1",
		Type:       "worker",
		TargetType: "project",
		TargetID:   stringPtr("project:project_1"),
		Status:     "queued",
		CreatedAt:  nowISO,
		UpdatedAt:  nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert(loop_existing_worker) error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/workers", bytes.NewReader([]byte(`{"projectId":"project_1","prompt":"Second worker","repo":"acme/looper","baseBranch":"main"}`)))
	req.Header.Set("x-request-id", "fixture-request-id")
	req.Header.Set("content-type", "application/json")
	recorder := httptest.NewRecorder()

	NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime, Now: func() time.Time { return fixture.now.Add(time.Minute) }}).ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
}

func TestHandlerWorkersCreateRejectsDuplicateIssueWorkers(t *testing.T) {
	fixture := newTestFixture(t)
	seedWorkerPlannerArtifactsData(t, fixture.runtime, fixture.now)
	nowISO := fixture.now.UTC().Format(javaScriptISOString)

	if err := fixture.runtime.Services().Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{
		ID:         "loop_existing_issue_worker",
		Seq:        1,
		ProjectID:  "project_1",
		Type:       "worker",
		TargetType: "issue",
		TargetID:   stringPtr("issue:acme/looper:77"),
		Repo:       stringPtr("acme/looper"),
		Status:     "queued",
		CreatedAt:  nowISO,
		UpdatedAt:  nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert(loop_existing_issue_worker) error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/workers", bytes.NewReader([]byte(`{"projectId":"project_1","repo":"acme/looper","issueNumber":77,"baseBranch":"main"}`)))
	req.Header.Set("x-request-id", "fixture-request-id")
	req.Header.Set("content-type", "application/json")
	recorder := httptest.NewRecorder()

	NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime, Now: func() time.Time { return fixture.now.Add(time.Minute) }}).ServeHTTP(recorder, req)

	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", recorder.Code)
	}
	body := parseJSONMap(t, recorder.Body.Bytes())
	apiErr := body["error"].(map[string]any)
	assertEqual(t, apiErr["code"], "LOOP_CONFLICT")
}

func TestHandlerWorkersCreateTriggersSchedulerTickHook(t *testing.T) {
	fixture := newTestFixture(t)
	seedWorkerPlannerArtifactsData(t, fixture.runtime, fixture.now)

	triggered := 0
	h := NewHandler(Context{
		Config:  fixture.config,
		Runtime: fixture.runtime,
		Now:     func() time.Time { return fixture.now.Add(time.Minute) },
		TriggerSchedulerTick: func() {
			triggered++
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/workers", bytes.NewReader([]byte(`{"projectId":"project_1","prompt":"Wire runtime","repo":"acme/looper","baseBranch":"main"}`)))
	req.Header.Set("x-request-id", "fixture-request-id")
	req.Header.Set("content-type", "application/json")
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	assertEqual(t, triggered, 1)
}

func TestHandlerPlannersCreateTriggersSchedulerTickHook(t *testing.T) {
	fixture := newTestFixture(t)
	seedWorkerPlannerArtifactsData(t, fixture.runtime, fixture.now)

	triggered := 0
	h := NewHandler(Context{
		Config:  fixture.config,
		Runtime: fixture.runtime,
		Now:     func() time.Time { return fixture.now.Add(time.Minute) },
		TriggerSchedulerTick: func() {
			triggered++
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/planners", bytes.NewReader([]byte(`{"projectId":"project_1","issueNumber":77}`)))
	req.Header.Set("x-request-id", "fixture-request-id")
	req.Header.Set("content-type", "application/json")
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	assertEqual(t, triggered, 1)
}

func TestHandlerPlannersCreateChecksProjectBeforeIssueValidation(t *testing.T) {
	fixture := newTestFixture(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/planners", bytes.NewReader([]byte(`{"projectId":"missing-project","issueNumber":0}`)))
	req.Header.Set("x-request-id", "error-request-id")
	req.Header.Set("content-type", "application/json")
	recorder := httptest.NewRecorder()

	NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime, Now: func() time.Time { return fixture.now.Add(time.Minute) }}).ServeHTTP(recorder, req)

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", recorder.Code)
	}
	body := parseJSONMap(t, recorder.Body.Bytes())
	errorMap := body["error"].(map[string]any)
	assertEqual(t, errorMap["code"], "PROJECT_NOT_FOUND")
	assertEqual(t, errorMap["message"], "Project not found: missing-project")
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

func TestHandlerActiveRunsStopAllUsesContextStopAll(t *testing.T) {
	fixture := newTestFixture(t)
	seedRunRouteData(t, fixture.runtime)

	called := 0
	var gotReason string
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runs/active/stop-all", nil)
	recorder := httptest.NewRecorder()

	NewHandler(Context{
		Config:  fixture.config,
		Runtime: fixture.runtime,
		StopAll: func(_ context.Context, reason string) (any, error) {
			called++
			gotReason = reason
			return map[string]any{"summary": map[string]any{"total": 0, "stopped": 0, "alreadyFinished": 0, "alreadyStopping": 0, "failed": 0}, "items": []map[string]any{}}, nil
		},
		StopLoop: func(context.Context, string, string) (any, error) {
			t.Fatal("StopLoop should not be called for stop-all route")
			return nil, nil
		},
	}).ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	assertEqual(t, called, 1)
	assertEqual(t, gotReason, "Stopped by user via selector all")
	body := parseJSONMap(t, recorder.Body.Bytes())
	assertEqual(t, body["ok"], true)
}

func TestHandlerLoopLogsFollowStreamsSnapshotAndChunk(t *testing.T) {
	fixture := newTestFixture(t)
	seedRunRouteData(t, fixture.runtime)

	nowISO := fixture.now.UTC().Format(javaScriptISOString)
	if err := fixture.runtime.Services().Repositories.AgentExecutions.Upsert(context.Background(), storage.AgentExecutionRecord{
		ID:              "agent_exec_1",
		ProjectID:       stringPtr("project_1"),
		LoopID:          stringPtr("loop_1"),
		RunID:           stringPtr("run_1"),
		Vendor:          "openai",
		Status:          "running",
		PID:             int64Ptr(1234),
		HeartbeatCount:  1,
		LastHeartbeatAt: stringPtr(nowISO),
		StartedAt:       nowISO,
		OutputJSON:      stringPtr(`{"stdout":"line1\n","stderr":""}`),
		CreatedAt:       nowISO,
		UpdatedAt:       nowISO,
	}); err != nil {
		t.Fatalf("AgentExecutions.Upsert(agent_exec_1) error = %v", err)
	}

	server := httptest.NewServer(NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime}))
	defer server.Close()

	response, err := http.Get(server.URL + "/api/v1/loops/loop_1/logs?follow=1")
	if err != nil {
		t.Fatalf("http.Get() error = %v", err)
	}
	defer response.Body.Close()

	go func() {
		time.Sleep(250 * time.Millisecond)
		updatedRun, getRunErr := fixture.runtime.Services().Repositories.Runs.GetByID(context.Background(), "run_1")
		if getRunErr != nil || updatedRun == nil {
			return
		}
		run := *updatedRun
		completedAt := fixture.now.Add(time.Minute).UTC().Format(javaScriptISOString)
		run.Status = "success"
		run.EndedAt = &completedAt
		run.UpdatedAt = completedAt
		_ = fixture.runtime.Services().Repositories.Runs.Upsert(context.Background(), run)

		updatedExec, getExecErr := fixture.runtime.Services().Repositories.AgentExecutions.GetLatestByRunID(context.Background(), "run_1")
		if getExecErr != nil || updatedExec == nil {
			return
		}
		exec := *updatedExec
		exec.Status = "completed"
		exec.EndedAt = &completedAt
		exec.OutputJSON = stringPtr(`{"stdout":"line1\nline2\n","stderr":""}`)
		exec.UpdatedAt = completedAt
		_ = fixture.runtime.Services().Repositories.AgentExecutions.Upsert(context.Background(), exec)
	}()

	bodyCh := make(chan []byte, 1)
	errCh := make(chan error, 1)
	go func() {
		body, readErr := io.ReadAll(response.Body)
		if readErr != nil {
			errCh <- readErr
			return
		}
		bodyCh <- body
	}()

	var body []byte
	select {
	case body = <-bodyCh:
	case err := <-errCh:
		t.Fatalf("io.ReadAll() error = %v", err)
	case <-time.After(5 * time.Second):
		_ = response.Body.Close()
		t.Fatal("timed out waiting for loop logs follow stream")
	}
	text := string(body)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	if got := response.Header.Get("Content-Type"); !strings.Contains(got, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", got)
	}
	if !strings.Contains(text, "event: snapshot") {
		t.Fatalf("stream body = %q, want snapshot event", text)
	}
	if !strings.Contains(text, "event: chunk") || !strings.Contains(text, "\"content\":\"line2\\n\"") {
		t.Fatalf("stream body = %q, want chunk with appended output", text)
	}
	if !strings.Contains(text, "event: end") {
		t.Fatalf("stream body = %q, want end event", text)
	}
}

func TestHandlerLoopLogsReturnsPersistedHistoricalAgentOutput(t *testing.T) {
	fixture := newTestFixture(t)
	seedRunRouteData(t, fixture.runtime)

	stdoutPath := filepath.Join(fixture.config.Daemon.LogDir, "loops", "loop_1", "run_1", "agent_exec_persisted.stdout.log")
	stderrPath := filepath.Join(fixture.config.Daemon.LogDir, "loops", "loop_1", "run_1", "agent_exec_persisted.stderr.log")
	fullStdout := strings.Repeat("stdout-line\n", 4)
	fullStderr := strings.Repeat("stderr-line\n", 4)
	if err := os.MkdirAll(filepath.Dir(stdoutPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(log dir) error = %v", err)
	}
	if err := os.WriteFile(stdoutPath, []byte(fullStdout), 0o644); err != nil {
		t.Fatalf("WriteFile(stdoutPath) error = %v", err)
	}
	if err := os.WriteFile(stderrPath, []byte(fullStderr), 0o644); err != nil {
		t.Fatalf("WriteFile(stderrPath) error = %v", err)
	}

	nowISO := fixture.now.UTC().Format(javaScriptISOString)
	if err := fixture.runtime.Services().Repositories.AgentExecutions.Upsert(context.Background(), storage.AgentExecutionRecord{
		ID:              "agent_exec_persisted",
		ProjectID:       stringPtr("project_1"),
		LoopID:          stringPtr("loop_1"),
		RunID:           stringPtr("run_1"),
		Vendor:          "openai",
		Status:          "completed",
		PID:             int64Ptr(1234),
		HeartbeatCount:  1,
		LastHeartbeatAt: stringPtr(nowISO),
		StartedAt:       nowISO,
		OutputJSON:      stringPtr(`{"stdout":"tail-out","stderr":"tail-err","stdoutLogPath":"` + stdoutPath + `","stderrLogPath":"` + stderrPath + `"}`),
		CreatedAt:       nowISO,
		UpdatedAt:       nowISO,
	}); err != nil {
		t.Fatalf("AgentExecutions.Upsert(agent_exec_persisted) error = %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/loops/loop_1/logs", nil)
	recorder := httptest.NewRecorder()
	NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime}).ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	body := parseJSONMap(t, recorder.Body.Bytes())
	data := body["data"].(map[string]any)
	agent := data["agent"].(map[string]any)
	assertEqual(t, agent["stdout"], fullStdout)
	assertEqual(t, agent["stderr"], fullStderr)
}

func TestHandlerLoopLogsFollowDefaultsToCodexStderrWhenStdoutEmpty(t *testing.T) {
	fixture := newTestFixture(t)
	seedRunRouteData(t, fixture.runtime)

	nowISO := fixture.now.UTC().Format(javaScriptISOString)
	if err := fixture.runtime.Services().Repositories.AgentExecutions.Upsert(context.Background(), storage.AgentExecutionRecord{
		ID:              "agent_exec_1",
		ProjectID:       stringPtr("project_1"),
		LoopID:          stringPtr("loop_1"),
		RunID:           stringPtr("run_1"),
		Vendor:          "codex",
		Status:          "running",
		PID:             int64Ptr(1234),
		HeartbeatCount:  1,
		LastHeartbeatAt: stringPtr(nowISO),
		StartedAt:       nowISO,
		OutputJSON:      stringPtr(`{"stdout":"","stderr":"codex line1\n"}`),
		CreatedAt:       nowISO,
		UpdatedAt:       nowISO,
	}); err != nil {
		t.Fatalf("AgentExecutions.Upsert(agent_exec_1) error = %v", err)
	}

	server := httptest.NewServer(NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime}))
	defer server.Close()

	response, err := http.Get(server.URL + "/api/v1/loops/loop_1/logs?follow=1")
	if err != nil {
		t.Fatalf("http.Get() error = %v", err)
	}
	defer response.Body.Close()

	go func() {
		time.Sleep(250 * time.Millisecond)
		updatedRun, getRunErr := fixture.runtime.Services().Repositories.Runs.GetByID(context.Background(), "run_1")
		if getRunErr != nil || updatedRun == nil {
			return
		}
		run := *updatedRun
		completedAt := fixture.now.Add(time.Minute).UTC().Format(javaScriptISOString)
		run.Status = "success"
		run.EndedAt = &completedAt
		run.UpdatedAt = completedAt
		_ = fixture.runtime.Services().Repositories.Runs.Upsert(context.Background(), run)

		updatedExec, getExecErr := fixture.runtime.Services().Repositories.AgentExecutions.GetLatestByRunID(context.Background(), "run_1")
		if getExecErr != nil || updatedExec == nil {
			return
		}
		exec := *updatedExec
		exec.Status = "completed"
		exec.EndedAt = &completedAt
		exec.OutputJSON = stringPtr(`{"stdout":"","stderr":"codex line1\ncodex line2\n"}`)
		exec.UpdatedAt = completedAt
		_ = fixture.runtime.Services().Repositories.AgentExecutions.Upsert(context.Background(), exec)
	}()

	bodyCh := make(chan []byte, 1)
	errCh := make(chan error, 1)
	go func() {
		body, readErr := io.ReadAll(response.Body)
		if readErr != nil {
			errCh <- readErr
			return
		}
		bodyCh <- body
	}()

	var body []byte
	select {
	case body = <-bodyCh:
	case err := <-errCh:
		t.Fatalf("io.ReadAll() error = %v", err)
	case <-time.After(5 * time.Second):
		_ = response.Body.Close()
		t.Fatal("timed out waiting for loop logs follow stream")
	}
	text := string(body)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	if !strings.Contains(text, "event: snapshot") {
		t.Fatalf("stream body = %q, want snapshot event", text)
	}
	if !strings.Contains(text, "\"stderr\":\"codex line1\\n\"") {
		t.Fatalf("stream body = %q, want codex stderr in snapshot", text)
	}
	if !strings.Contains(text, "event: chunk") || !strings.Contains(text, "\"content\":\"codex line2\\n\"") {
		t.Fatalf("stream body = %q, want chunk with appended codex stderr output", text)
	}
	if !strings.Contains(text, "event: end") {
		t.Fatalf("stream body = %q, want end event", text)
	}
}

func TestHandlerLoopLogsFollowEmitsEndForTerminalSnapshot(t *testing.T) {
	fixture := newTestFixture(t)
	seedRunRouteData(t, fixture.runtime)

	completedAt := fixture.now.Add(time.Minute).UTC().Format(javaScriptISOString)
	run, err := fixture.runtime.Services().Repositories.Runs.GetByID(context.Background(), "run_1")
	if err != nil {
		t.Fatalf("Runs.GetByID(run_1) error = %v", err)
	}
	if run == nil {
		t.Fatal("run_1 missing from fixture")
	}
	run.Status = "success"
	run.EndedAt = &completedAt
	run.UpdatedAt = completedAt
	if err := fixture.runtime.Services().Repositories.Runs.Upsert(context.Background(), *run); err != nil {
		t.Fatalf("Runs.Upsert(run_1) error = %v", err)
	}

	server := httptest.NewServer(NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime}))
	defer server.Close()

	response, err := http.Get(server.URL + "/api/v1/loops/loop_1/logs?follow=1")
	if err != nil {
		t.Fatalf("http.Get() error = %v", err)
	}
	defer response.Body.Close()

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("io.ReadAll() error = %v", err)
	}
	text := string(body)
	if !strings.Contains(text, "event: snapshot") {
		t.Fatalf("stream body = %q, want snapshot event", text)
	}
	if !strings.Contains(text, "event: end") {
		t.Fatalf("stream body = %q, want end event", text)
	}
	if strings.Contains(text, "event: chunk") {
		t.Fatalf("stream body = %q, did not expect chunk event for terminal snapshot", text)
	}
}

func TestHandlerLoopLogsFollowDoesNotStreamNextRunChunks(t *testing.T) {
	fixture := newTestFixture(t)
	seedRunRouteData(t, fixture.runtime)

	nowISO := fixture.now.UTC().Format(javaScriptISOString)
	if err := fixture.runtime.Services().Repositories.AgentExecutions.Upsert(context.Background(), storage.AgentExecutionRecord{
		ID:              "agent_exec_1",
		ProjectID:       stringPtr("project_1"),
		LoopID:          stringPtr("loop_1"),
		RunID:           stringPtr("run_1"),
		Vendor:          "openai",
		Status:          "running",
		PID:             int64Ptr(1234),
		HeartbeatCount:  1,
		LastHeartbeatAt: stringPtr(nowISO),
		StartedAt:       nowISO,
		OutputJSON:      stringPtr(`{"stdout":"line1\n","stderr":""}`),
		CreatedAt:       nowISO,
		UpdatedAt:       nowISO,
	}); err != nil {
		t.Fatalf("AgentExecutions.Upsert(agent_exec_1) error = %v", err)
	}

	server := httptest.NewServer(NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime}))
	defer server.Close()

	response, err := http.Get(server.URL + "/api/v1/loops/loop_1/logs?follow=1")
	if err != nil {
		t.Fatalf("http.Get() error = %v", err)
	}
	defer response.Body.Close()

	go func() {
		time.Sleep(250 * time.Millisecond)
		completedAt := fixture.now.Add(time.Minute).UTC().Format(javaScriptISOString)

		updatedRun, getRunErr := fixture.runtime.Services().Repositories.Runs.GetByID(context.Background(), "run_1")
		if getRunErr != nil || updatedRun == nil {
			return
		}
		run := *updatedRun
		run.Status = "success"
		run.EndedAt = &completedAt
		run.UpdatedAt = completedAt
		_ = fixture.runtime.Services().Repositories.Runs.Upsert(context.Background(), run)

		nextStartedAt := fixture.now.Add(2 * time.Minute).UTC().Format(javaScriptISOString)
		_ = fixture.runtime.Services().Repositories.Runs.Upsert(context.Background(), storage.RunRecord{
			ID:                "run_2",
			LoopID:            "loop_1",
			Status:            "running",
			CurrentStep:       stringPtr("review"),
			LastCompletedStep: stringPtr("snapshot"),
			StartedAt:         nextStartedAt,
			LastHeartbeatAt:   stringPtr(nextStartedAt),
			CreatedAt:         nextStartedAt,
			UpdatedAt:         nextStartedAt,
		})
		_ = fixture.runtime.Services().Repositories.AgentExecutions.Upsert(context.Background(), storage.AgentExecutionRecord{
			ID:              "agent_exec_2",
			ProjectID:       stringPtr("project_1"),
			LoopID:          stringPtr("loop_1"),
			RunID:           stringPtr("run_2"),
			Vendor:          "openai",
			Status:          "running",
			PID:             int64Ptr(5678),
			HeartbeatCount:  1,
			LastHeartbeatAt: stringPtr(nextStartedAt),
			StartedAt:       nextStartedAt,
			OutputJSON:      stringPtr(`{"stdout":"run2-line1\n","stderr":""}`),
			CreatedAt:       nextStartedAt,
			UpdatedAt:       nextStartedAt,
		})
	}()

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("io.ReadAll() error = %v", err)
	}
	text := string(body)
	if !strings.Contains(text, "event: snapshot") {
		t.Fatalf("stream body = %q, want snapshot event", text)
	}
	if !strings.Contains(text, "event: end") {
		t.Fatalf("stream body = %q, want end event", text)
	}
	if strings.Contains(text, "run2-line1\\n") {
		t.Fatalf("stream body = %q, did not expect next run chunk", text)
	}
	if strings.Contains(text, "event: chunk") {
		t.Fatalf("stream body = %q, did not expect chunk event from next run", text)
	}
}

func TestHandlerLoopLogsFollowEmitsEndWhenLoopTerminatesBeforeRunStarts(t *testing.T) {
	fixture := newTestFixture(t)
	seedRunRouteData(t, fixture.runtime)

	nowISO := fixture.now.UTC().Format(javaScriptISOString)
	if err := fixture.runtime.Services().Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{
		ID:         "loop_no_run",
		Seq:        2,
		ProjectID:  "project_1",
		Type:       "reviewer",
		TargetType: "pull_request",
		TargetID:   stringPtr("pr:acme/looper:99"),
		Repo:       stringPtr("acme/looper"),
		PRNumber:   int64Ptr(99),
		Status:     "running",
		CreatedAt:  nowISO,
		UpdatedAt:  nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert(loop_no_run) error = %v", err)
	}

	server := httptest.NewServer(NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime}))
	defer server.Close()

	response, err := http.Get(server.URL + "/api/v1/loops/loop_no_run/logs?follow=1")
	if err != nil {
		t.Fatalf("http.Get() error = %v", err)
	}
	defer response.Body.Close()

	go func() {
		time.Sleep(250 * time.Millisecond)
		loop, getLoopErr := fixture.runtime.Services().Repositories.Loops.GetByID(context.Background(), "loop_no_run")
		if getLoopErr != nil || loop == nil {
			return
		}
		updatedAt := fixture.now.Add(time.Minute).UTC().Format(javaScriptISOString)
		updatedLoop := *loop
		updatedLoop.Status = "completed"
		updatedLoop.UpdatedAt = updatedAt
		_ = fixture.runtime.Services().Repositories.Loops.Upsert(context.Background(), updatedLoop)
	}()

	bodyCh := make(chan []byte, 1)
	errCh := make(chan error, 1)
	go func() {
		body, readErr := io.ReadAll(response.Body)
		if readErr != nil {
			errCh <- readErr
			return
		}
		bodyCh <- body
	}()

	var body []byte
	select {
	case body = <-bodyCh:
	case err := <-errCh:
		t.Fatalf("io.ReadAll() error = %v", err)
	case <-time.After(5 * time.Second):
		_ = response.Body.Close()
		t.Fatal("timed out waiting for loop logs follow stream")
	}

	text := string(body)
	if !strings.Contains(text, "event: snapshot") {
		t.Fatalf("stream body = %q, want snapshot event", text)
	}
	if !strings.Contains(text, "event: end") {
		t.Fatalf("stream body = %q, want end event", text)
	}
	if strings.Contains(text, "event: chunk") {
		t.Fatalf("stream body = %q, did not expect chunk event", text)
	}
}

func seedWorkerPlannerArtifactsData(t *testing.T, rt *looperdruntime.Runtime, now time.Time) {
	t.Helper()
	nowISO := now.UTC().Format(javaScriptISOString)
	baseBranch := "main"
	metadata := `{"repo":"acme/looper","worktreeRoot":null,"source":"api"}`
	if err := rt.Services().Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID:           "project_1",
		Name:         "Looper",
		RepoPath:     "/tmp/repos/looper",
		BaseBranch:   &baseBranch,
		Archived:     false,
		MetadataJSON: &metadata,
		CreatedAt:    nowISO,
		UpdatedAt:    nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert(project_1 metadata) error = %v", err)
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
	existingLoop, err := fixture.runtime.Services().Repositories.Loops.GetByID(context.Background(), "loop_1")
	if err != nil {
		t.Fatalf("Loops.GetByID(loop_1) error = %v", err)
	}
	if existingLoop == nil {
		t.Fatal("loop_1 missing from fixture")
	}
	completedLoop := *existingLoop
	completedLoop.Status = "completed"
	completedLoop.UpdatedAt = completedAt
	if err := fixture.runtime.Services().Repositories.Loops.Upsert(context.Background(), completedLoop); err != nil {
		t.Fatalf("Loops.Upsert(completed) error = %v", err)
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
			assertErrorArtifactMatch(t, parseJSONMap(t, recorder.Body.Bytes()), want)
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

func TestActiveRunsIncludesRunningLoopWithoutRun(t *testing.T) {
	fixture := newTestFixture(t)
	nowISO := fixture.now.UTC().Format(javaScriptISOString)

	if err := fixture.runtime.Services().Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID:        "project_1",
		Name:      "Looper",
		RepoPath:  "/tmp/repos/looper",
		Archived:  false,
		CreatedAt: nowISO,
		UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	if err := fixture.runtime.Services().Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{
		ID:         "loop_reviewer_running_only",
		Seq:        7,
		ProjectID:  "project_1",
		Type:       "reviewer",
		TargetType: "pull_request",
		TargetID:   stringPtr("pr:acme/looper:43"),
		Repo:       stringPtr("acme/looper"),
		PRNumber:   int64Ptr(43),
		Status:     "running",
		LastRunAt:  stringPtr(nowISO),
		NextRunAt:  stringPtr(nowISO),
		CreatedAt:  nowISO,
		UpdatedAt:  nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	h := NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/runs/active", nil)
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	body := parseJSONMap(t, recorder.Body.Bytes())
	items := body["data"].(map[string]any)["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("len(items) = %d, want 1", len(items))
	}
	item := items[0].(map[string]any)
	assertEqual(t, item["loopId"], "loop_reviewer_running_only")
	assertEqual(t, item["runId"], nil)
	assertEqual(t, item["type"], "reviewer")
	assertEqual(t, item["status"], "running")
	target := item["target"].(map[string]any)
	assertEqual(t, target["label"], "acme/looper#43")
}

func TestActiveRunsIncludesPausedLoopWithRunningRun(t *testing.T) {
	fixture := newTestFixture(t)
	nowISO := fixture.now.UTC().Format(javaScriptISOString)

	if err := fixture.runtime.Services().Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID:        "project_1",
		Name:      "Looper",
		RepoPath:  "/tmp/repos/looper",
		Archived:  false,
		CreatedAt: nowISO,
		UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	if err := fixture.runtime.Services().Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{
		ID:         "loop_paused",
		Seq:        8,
		ProjectID:  "project_1",
		Type:       "reviewer",
		TargetType: "pull_request",
		TargetID:   stringPtr("pr:acme/looper:52"),
		Repo:       stringPtr("acme/looper"),
		PRNumber:   int64Ptr(52),
		Status:     "paused",
		LastRunAt:  stringPtr(nowISO),
		CreatedAt:  nowISO,
		UpdatedAt:  nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	if err := fixture.runtime.Services().Repositories.Runs.Upsert(context.Background(), storage.RunRecord{
		ID:              "run_stale",
		LoopID:          "loop_paused",
		Status:          "running",
		CurrentStep:     stringPtr("review"),
		StartedAt:       nowISO,
		LastHeartbeatAt: stringPtr(nowISO),
		CreatedAt:       nowISO,
		UpdatedAt:       nowISO,
	}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}

	h := NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/runs/active", nil)
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	body := parseJSONMap(t, recorder.Body.Bytes())
	items := body["data"].(map[string]any)["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("len(items) = %d, want 1", len(items))
	}
	item := items[0].(map[string]any)
	assertEqual(t, item["loopId"], "loop_paused")
	assertEqual(t, item["runId"], "run_stale")
	assertEqual(t, item["status"], "running")
}

func TestActiveRunsStatusAndTypeFiltersIncludeInactiveCompletedWorkerLoops(t *testing.T) {
	fixture := newTestFixture(t)
	nowISO := fixture.now.UTC().Format(javaScriptISOString)
	completedAt := fixture.now.Add(time.Minute).UTC().Format(javaScriptISOString)

	if err := fixture.runtime.Services().Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID:        "project_1",
		Name:      "Looper",
		RepoPath:  "/tmp/repos/looper",
		Archived:  false,
		CreatedAt: nowISO,
		UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	for _, loop := range []storage.LoopRecord{
		{
			ID:         "loop_worker_completed",
			Seq:        12,
			ProjectID:  "project_1",
			Type:       "worker",
			TargetType: "issue",
			TargetID:   stringPtr("issue:acme/looper:42"),
			Repo:       stringPtr("acme/looper"),
			Status:     "completed",
			LastRunAt:  stringPtr(completedAt),
			CreatedAt:  nowISO,
			UpdatedAt:  completedAt,
		},
		{
			ID:         "loop_worker_running",
			Seq:        13,
			ProjectID:  "project_1",
			Type:       "worker",
			TargetType: "issue",
			TargetID:   stringPtr("issue:acme/looper:43"),
			Repo:       stringPtr("acme/looper"),
			Status:     "running",
			LastRunAt:  stringPtr(nowISO),
			CreatedAt:  nowISO,
			UpdatedAt:  nowISO,
		},
		{
			ID:         "loop_reviewer_completed",
			Seq:        14,
			ProjectID:  "project_1",
			Type:       "reviewer",
			TargetType: "pull_request",
			TargetID:   stringPtr("pr:acme/looper:44"),
			Repo:       stringPtr("acme/looper"),
			PRNumber:   int64Ptr(44),
			Status:     "completed",
			LastRunAt:  stringPtr(completedAt),
			CreatedAt:  nowISO,
			UpdatedAt:  completedAt,
		},
	} {
		if err := fixture.runtime.Services().Repositories.Loops.Upsert(context.Background(), loop); err != nil {
			t.Fatalf("Loops.Upsert(%s) error = %v", loop.ID, err)
		}
	}

	h := NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime})

	defaultReq := httptest.NewRequest(http.MethodGet, "/api/v1/runs/active", nil)
	defaultRecorder := httptest.NewRecorder()
	h.ServeHTTP(defaultRecorder, defaultReq)
	if defaultRecorder.Code != http.StatusOK {
		t.Fatalf("default status = %d, want 200", defaultRecorder.Code)
	}
	defaultBody := parseJSONMap(t, defaultRecorder.Body.Bytes())
	defaultItems := defaultBody["data"].(map[string]any)["items"].([]any)
	if len(defaultItems) != 1 {
		t.Fatalf("len(default items) = %d, want 1", len(defaultItems))
	}
	defaultItem := defaultItems[0].(map[string]any)
	assertEqual(t, defaultItem["loopId"], "loop_worker_running")

	filteredReq := httptest.NewRequest(http.MethodGet, "/api/v1/runs/active?status=completed&type=worker", nil)
	filteredRecorder := httptest.NewRecorder()
	h.ServeHTTP(filteredRecorder, filteredReq)
	if filteredRecorder.Code != http.StatusOK {
		t.Fatalf("filtered status = %d, want 200", filteredRecorder.Code)
	}
	filteredBody := parseJSONMap(t, filteredRecorder.Body.Bytes())
	filteredItems := filteredBody["data"].(map[string]any)["items"].([]any)
	if len(filteredItems) != 1 {
		t.Fatalf("len(filtered items) = %d, want 1", len(filteredItems))
	}
	filteredItem := filteredItems[0].(map[string]any)
	assertEqual(t, filteredItem["loopId"], "loop_worker_completed")
	assertEqual(t, filteredItem["type"], "worker")
	assertEqual(t, filteredItem["status"], "completed")
	assertEqual(t, filteredItem["runId"], nil)
	assertEqual(t, filteredItem["agent"], nil)
}

func TestActiveRunsAllIncludesInactiveCompletedLoopWithLatestRunFields(t *testing.T) {
	fixture := newTestFixture(t)
	nowISO := fixture.now.UTC().Format(javaScriptISOString)
	startedAt := fixture.now.Add(-2 * time.Minute).UTC().Format(javaScriptISOString)
	completedAt := fixture.now.Add(time.Minute).UTC().Format(javaScriptISOString)

	if err := fixture.runtime.Services().Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID:        "project_1",
		Name:      "Looper",
		RepoPath:  "/tmp/repos/looper",
		Archived:  false,
		CreatedAt: nowISO,
		UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	if err := fixture.runtime.Services().Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{
		ID:           "loop_worker_completed",
		Seq:          15,
		ProjectID:    "project_1",
		Type:         "worker",
		TargetType:   "issue",
		TargetID:     stringPtr("issue:acme/looper:15"),
		Repo:         stringPtr("acme/looper"),
		Status:       "completed",
		LastRunAt:    stringPtr(completedAt),
		MetadataJSON: stringPtr(`{"worktreePath":"/tmp/worktrees/loop-15","branch":"feature/loop-15"}`),
		CreatedAt:    nowISO,
		UpdatedAt:    completedAt,
	}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	if err := fixture.runtime.Services().Repositories.Runs.Upsert(context.Background(), storage.RunRecord{
		ID:             "run_15",
		LoopID:         "loop_worker_completed",
		Status:         "completed",
		StartedAt:      startedAt,
		EndedAt:        stringPtr(completedAt),
		CheckpointJSON: stringPtr(`{"worktree":{"path":"/tmp/worktrees/loop-15","branch":"feature/loop-15"}}`),
		CreatedAt:      startedAt,
		UpdatedAt:      completedAt,
	}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}

	h := NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/runs/active?all=true", nil)
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	body := parseJSONMap(t, recorder.Body.Bytes())
	items := body["data"].(map[string]any)["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("len(items) = %d, want 1", len(items))
	}
	item := items[0].(map[string]any)
	assertEqual(t, item["loopId"], "loop_worker_completed")
	assertEqual(t, item["runId"], "run_15")
	assertEqual(t, item["type"], "worker")
	assertEqual(t, item["status"], "completed")
	assertEqual(t, item["startedAt"], startedAt)
	assertEqual(t, item["endedAt"], completedAt)
	worktree := item["worktree"].(map[string]any)
	assertEqual(t, worktree["path"], "/tmp/worktrees/loop-15")
	assertEqual(t, worktree["branch"], "feature/loop-15")
	assertEqual(t, item["agent"], nil)
}

func TestActiveRunDetailIncludesPausedLoopWithRunningRun(t *testing.T) {
	fixture := newTestFixture(t)
	nowISO := fixture.now.UTC().Format(javaScriptISOString)

	if err := fixture.runtime.Services().Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID:        "project_1",
		Name:      "Looper",
		RepoPath:  "/tmp/repos/looper",
		Archived:  false,
		CreatedAt: nowISO,
		UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	if err := fixture.runtime.Services().Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{
		ID:         "loop_paused",
		Seq:        8,
		ProjectID:  "project_1",
		Type:       "reviewer",
		TargetType: "pull_request",
		TargetID:   stringPtr("pr:acme/looper:52"),
		Repo:       stringPtr("acme/looper"),
		PRNumber:   int64Ptr(52),
		Status:     "paused",
		LastRunAt:  stringPtr(nowISO),
		CreatedAt:  nowISO,
		UpdatedAt:  nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	if err := fixture.runtime.Services().Repositories.Runs.Upsert(context.Background(), storage.RunRecord{
		ID:              "run_stale",
		LoopID:          "loop_paused",
		Status:          "running",
		CurrentStep:     stringPtr("review"),
		StartedAt:       nowISO,
		LastHeartbeatAt: stringPtr(nowISO),
		CreatedAt:       nowISO,
		UpdatedAt:       nowISO,
	}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}

	h := NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/runs/active/8", nil)
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	body := parseJSONMap(t, recorder.Body.Bytes())
	data := body["data"].(map[string]any)
	assertEqual(t, data["loopId"], "loop_paused")
	assertEqual(t, data["runId"], "run_stale")
	assertEqual(t, data["status"], "running")
}

func TestActiveRunDetailIncludesRunningLoopWithoutRun(t *testing.T) {
	fixture := newTestFixture(t)
	nowISO := fixture.now.UTC().Format(javaScriptISOString)

	if err := fixture.runtime.Services().Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID:        "project_1",
		Name:      "Looper",
		RepoPath:  "/tmp/repos/looper",
		Archived:  false,
		CreatedAt: nowISO,
		UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	if err := fixture.runtime.Services().Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{
		ID:         "loop_reviewer_running_only",
		Seq:        7,
		ProjectID:  "project_1",
		Type:       "reviewer",
		TargetType: "pull_request",
		TargetID:   stringPtr("pr:acme/looper:43"),
		Repo:       stringPtr("acme/looper"),
		PRNumber:   int64Ptr(43),
		Status:     "running",
		LastRunAt:  stringPtr(nowISO),
		NextRunAt:  stringPtr(nowISO),
		CreatedAt:  nowISO,
		UpdatedAt:  nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	h := NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/runs/active/loop_reviewer_running_only", nil)
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	body := parseJSONMap(t, recorder.Body.Bytes())
	item := body["data"].(map[string]any)
	assertEqual(t, item["loopId"], "loop_reviewer_running_only")
	assertEqual(t, item["runId"], nil)
	assertEqual(t, item["type"], "reviewer")
	assertEqual(t, item["status"], "running")
	target := item["target"].(map[string]any)
	assertEqual(t, target["label"], "acme/looper#43")
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
	cfg.Notifications.Osascript.Enabled = true
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
		RunSchedulerTick: func(context.Context, looperdruntime.Services) error {
			return nil
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

func seedStatusLoopCounts(t *testing.T, rt *looperdruntime.Runtime) {
	t.Helper()

	services := rt.Services()
	nowISO := "2026-04-11T12:00:00.000Z"
	projectID := "project_1"
	for _, seededLoop := range []storage.LoopRecord{
		{ID: "loop_queued", Seq: 2, ProjectID: projectID, Type: "reviewer", TargetType: "pull_request", TargetID: stringPtr("pr:acme/looper:43"), Repo: stringPtr("acme/looper"), PRNumber: int64Ptr(43), Status: "queued", CreatedAt: nowISO, UpdatedAt: nowISO},
		{ID: "loop_waiting", Seq: 3, ProjectID: projectID, Type: "reviewer", TargetType: "pull_request", TargetID: stringPtr("pr:acme/looper:44"), Repo: stringPtr("acme/looper"), PRNumber: int64Ptr(44), Status: "waiting", CreatedAt: nowISO, UpdatedAt: nowISO},
		{ID: "loop_terminated", Seq: 4, ProjectID: projectID, Type: "reviewer", TargetType: "pull_request", TargetID: stringPtr("pr:acme/looper:45"), Repo: stringPtr("acme/looper"), PRNumber: int64Ptr(45), Status: "terminated", CreatedAt: nowISO, UpdatedAt: nowISO},
		{ID: "loop_stopped", Seq: 5, ProjectID: projectID, Type: "reviewer", TargetType: "pull_request", TargetID: stringPtr("pr:acme/looper:46"), Repo: stringPtr("acme/looper"), PRNumber: int64Ptr(46), Status: "stopped", CreatedAt: nowISO, UpdatedAt: nowISO},
	} {
		if err := services.Repositories.Loops.Upsert(context.Background(), seededLoop); err != nil {
			t.Fatalf("Loops.Upsert(%s) error = %v", seededLoop.ID, err)
		}
	}
}

func seedLoopRouteData(t *testing.T, rt *looperdruntime.Runtime) {
	t.Helper()
	seedStatusData(t, rt)
}

func seedEventAndPullRequestRouteData(t *testing.T, rt *looperdruntime.Runtime) {
	t.Helper()
	seedStatusData(t, rt)
	nowISO := "2026-04-11T12:00:00.000Z"

	if _, err := rt.Services().Coordinator.DB().ExecContext(context.Background(), `DELETE FROM event_logs`); err != nil {
		t.Fatalf("DELETE FROM event_logs error = %v", err)
	}

	if err := rt.Services().Repositories.Events.Append(context.Background(), storage.EventLogRecord{
		ID:               "event_1",
		EventType:        "loop.created",
		ProjectID:        stringPtr("project_1"),
		LoopID:           stringPtr("loop_1"),
		EntityType:       stringPtr("loop"),
		EntityID:         stringPtr("loop_1"),
		ActorType:        stringPtr("system"),
		ActorID:          stringPtr("looperd"),
		ActorDisplayName: stringPtr("looperd"),
		PayloadJSON:      `{"status":"running"}`,
		CreatedAt:        nowISO,
	}); err != nil {
		t.Fatalf("Events.Append(event_1) error = %v", err)
	}

	if err := rt.Services().Repositories.PullRequestSnapshots.Upsert(context.Background(), storage.PullRequestSnapshotRecord{
		ID:                    "prs_1",
		ProjectID:             "project_1",
		Repo:                  "acme/looper",
		PRNumber:              42,
		HeadSHA:               "abc123",
		BaseSHA:               stringPtr("base123"),
		Title:                 stringPtr("Runtime foundation"),
		Body:                  stringPtr("Adds recovery and API"),
		Author:                stringPtr("octocat"),
		ChecksSummary:         stringPtr("green"),
		UnresolvedThreadCount: int64Ptr(1),
		ReviewState:           stringPtr("changes_requested"),
		CapturedAt:            nowISO,
		CreatedAt:             nowISO,
	}); err != nil {
		t.Fatalf("PullRequestSnapshots.Upsert(prs_1) error = %v", err)
	}
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
		case "<generated-timestamp>", "<current-target>", "<daemon-executable-path>":
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

func assertErrorArtifactMatch(t *testing.T, body map[string]any, want errorArtifactCase) {
	t.Helper()

	assertEqual(t, body["ok"], false)

	requestID, ok := body["requestId"].(string)
	if !ok || strings.TrimSpace(requestID) == "" {
		t.Fatalf("requestId = %#v, want non-empty string", body["requestId"])
	}

	wantBytes, err := json.Marshal(want.Body)
	if err != nil {
		t.Fatalf("json.Marshal(want.Body) error = %v", err)
	}
	wantBody := parseJSONValue(t, wantBytes)
	if !responseFixtureMatches(body, wantBody) {
		actualJSON, _ := json.MarshalIndent(body, "", "  ")
		wantJSON, _ := json.MarshalIndent(wantBody, "", "  ")
		t.Fatalf("error artifact mismatch\nactual=%s\nwant=%s", actualJSON, wantJSON)
	}
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

type errorInjectingQuerier struct {
	db         *sql.DB
	queryError func(query string) error
}

func (q errorInjectingQuerier) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return q.db.ExecContext(ctx, query, args...)
}

func (q errorInjectingQuerier) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if q.queryError != nil {
		if err := q.queryError(query); err != nil {
			return nil, err
		}
	}
	return q.db.QueryContext(ctx, query, args...)
}

func (q errorInjectingQuerier) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return q.db.QueryRowContext(ctx, query, args...)
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
	list          func(context.Context) ([]storage.ProjectRecord, error)
	addProject    func(context.Context, projects.AddInput) (projects.AddResult, error)
	removeProject func(context.Context, string) (storage.ProjectRecord, error)
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

func (f fakeProjectService) RemoveProject(ctx context.Context, identifier string) (storage.ProjectRecord, error) {
	if f.removeProject != nil {
		return f.removeProject(ctx, identifier)
	}
	return storage.ProjectRecord{}, nil
}

type errorArtifactCase struct {
	ID             string `json:"id"`
	ExpectedStatus int    `json:"expectedStatus"`
	Body           struct {
		OK        bool   `json:"ok"`
		RequestID string `json:"requestId"`
		Error     struct {
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
