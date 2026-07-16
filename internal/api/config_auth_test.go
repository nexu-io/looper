package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/config"
)

func TestConfigPatchWithoutTokenRequiresDirectLoopback(t *testing.T) {
	cfg := testConfigRouteConfig(t)
	baseURL := "https://looper.example.test"
	cfg.Server.BaseURL = &baseURL
	callbackCalls := 0
	handler := NewHandler(Context{
		Config: cfg,
		PatchConfig: func(context.Context, ConfigPatchRequest) error {
			callbackCalls++
			return nil
		},
	})
	body := `{"revision":"sha256:test","set":{"scheduler.maxConcurrentRuns":4}}`

	tests := []struct {
		name      string
		configure func(*http.Request)
	}{
		{
			name: "remote client",
			configure: func(req *http.Request) {
				req.RemoteAddr = "203.0.113.10:54321"
			},
		},
		{
			name: "forwarded through local proxy",
			configure: func(req *http.Request) {
				req.Header.Set("X-Forwarded-For", "203.0.113.10")
			},
		},
		{
			name: "alternate forwarding metadata",
			configure: func(req *http.Request) {
				req.Header.Set("X-Forwarded-Proto", "https")
			},
		},
		{
			name: "public authority through stripped local proxy",
			configure: func(req *http.Request) {
				req.Host = "looper.example.test"
				req.Header.Set("Origin", baseURL)
			},
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			req := loopbackConfigPatchRequest(strings.NewReader(body))
			testCase.configure(req)
			handler.ServeHTTP(recorder, req)
			if recorder.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403; body=%s", recorder.Code, recorder.Body.String())
			}
		})
	}
	if callbackCalls != 0 {
		t.Fatalf("PatchConfig calls = %d, want 0", callbackCalls)
	}
}

func TestConfigPatchWithTokenAllowsAuthenticatedLocalProxy(t *testing.T) {
	cfg := testConfigRouteConfig(t)
	token := "local-token"
	cfg.Server.AuthMode = config.AuthModeLocalToken
	cfg.Server.LocalToken = &token
	callbackCalls := 0
	handler := NewHandler(Context{
		Config: cfg,
		PatchConfig: func(context.Context, ConfigPatchRequest) error {
			callbackCalls++
			return nil
		},
	})

	recorder := httptest.NewRecorder()
	req := loopbackConfigPatchRequest(strings.NewReader(`{"revision":"sha256:test","set":{"scheduler.maxConcurrentRuns":4}}`))
	req.Header.Set("X-Forwarded-For", "203.0.113.10")
	req.Header.Set("Authorization", "Bearer "+token)
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	if callbackCalls != 1 {
		t.Fatalf("PatchConfig calls = %d, want 1", callbackCalls)
	}
}
