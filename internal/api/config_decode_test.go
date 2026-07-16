package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestConfigPatchStrictDecodeAndSizeLimit(t *testing.T) {
	cfg := testConfigRouteConfig(t)
	tooLarge := `{"revision":"sha256:test","set":{"agent.env.BIG":"` + strings.Repeat("x", maxConfigPatchBodyBytes) + `"}}`
	tests := []struct {
		name, body, wantCode string
		wantStatus           int
	}{
		{name: "body required", wantCode: "request_body_required", wantStatus: http.StatusBadRequest},
		{name: "object required", body: "null", wantCode: "invalid_json", wantStatus: http.StatusBadRequest},
		{name: "unknown field", body: `{"revision":"sha256:test","set":{"scheduler.maxConcurrentRuns":4},"extra":true}`, wantCode: "invalid_json", wantStatus: http.StatusBadRequest},
		{name: "trailing json", body: `{"revision":"sha256:test","set":{"scheduler.maxConcurrentRuns":4}} {}`, wantCode: "trailing_json", wantStatus: http.StatusBadRequest},
		{name: "empty patch", body: `{"revision":"sha256:test"}`, wantCode: "empty_patch", wantStatus: http.StatusBadRequest},
		{name: "conflicting operations", body: `{"revision":"sha256:test","set":{"scheduler.maxConcurrentRuns":4},"unset":["scheduler.maxConcurrentRuns"]}`, wantCode: "conflicting_operation", wantStatus: http.StatusBadRequest},
		{name: "duplicate json name", body: `{"revision":"sha256:test","set":{"scheduler.maxConcurrentRuns":3,"scheduler.maxConcurrentRuns":4}}`, wantCode: "duplicate_json_name", wantStatus: http.StatusBadRequest},
		{name: "too large", body: tooLarge, wantCode: "request_too_large", wantStatus: http.StatusRequestEntityTooLarge},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			callbackCalls := 0
			handler := NewHandler(Context{Config: cfg, PatchConfig: func(context.Context, ConfigPatchRequest) error {
				callbackCalls++
				return nil
			}})
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, loopbackConfigPatchRequest(strings.NewReader(tt.body)))
			if recorder.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", recorder.Code, tt.wantStatus, recorder.Body.String())
			}
			if callbackCalls != 0 {
				t.Fatalf("PatchConfig calls = %d, want 0", callbackCalls)
			}
			issues := parseJSONMap(t, recorder.Body.Bytes())["error"].(map[string]any)["details"].(map[string]any)["issues"].([]any)
			found := false
			for _, item := range issues {
				found = found || item.(map[string]any)["code"] == tt.wantCode
			}
			if !found {
				t.Fatalf("issues = %#v, want code %q", issues, tt.wantCode)
			}
		})
	}
}

func TestConfigPatchWithoutTokenRequiresLoopbackClient(t *testing.T) {
	callbackCalls := 0
	handler := NewHandler(Context{Config: testConfigRouteConfig(t), PatchConfig: func(context.Context, ConfigPatchRequest) error {
		callbackCalls++
		return nil
	}})
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/config", strings.NewReader(`{"revision":"sha256:test","set":{"scheduler.maxConcurrentRuns":4}}`))
	req.Host = "localhost:17310"
	req.RemoteAddr = "203.0.113.20:54321"
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", recorder.Code, recorder.Body.String())
	}
	if callbackCalls != 0 {
		t.Fatalf("PatchConfig calls = %d, want 0", callbackCalls)
	}
}

func TestConfigPatchWithoutCallbackIsUnsupported(t *testing.T) {
	recorder := httptest.NewRecorder()
	req := loopbackConfigPatchRequest(strings.NewReader(`{"revision":"sha256:test","set":{"scheduler.maxConcurrentRuns":4}}`))
	NewHandler(Context{Config: testConfigRouteConfig(t)}).ServeHTTP(recorder, req)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", recorder.Code, recorder.Body.String())
	}
	issue := parseJSONMap(t, recorder.Body.Bytes())["error"].(map[string]any)["details"].(map[string]any)["issues"].([]any)[0].(map[string]any)
	assertEqual(t, issue["code"], "config_patch_unsupported")
}

func loopbackConfigPatchRequest(body io.Reader) *http.Request {
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/config", body)
	req.RemoteAddr = "127.0.0.1:54321"
	return req
}
