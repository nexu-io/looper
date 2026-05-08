package cliapp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nexu-io/looper/pkg/api"
)

func TestDaemonAPIClientPostSuccessSendsHeadersAndBody(t *testing.T) {
	t.Parallel()

	type requestPayload struct {
		Name string `json:"name"`
	}

	type responsePayload struct {
		ID string `json:"id"`
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.Method, http.MethodPost; got != want {
			t.Fatalf("method = %q, want %q", got, want)
		}
		if got, want := r.URL.Path, "/v1/loops"; got != want {
			t.Fatalf("path = %q, want %q", got, want)
		}
		if got, want := r.Header.Get("Authorization"), "Bearer abc123"; got != want {
			t.Fatalf("authorization = %q, want %q", got, want)
		}
		if got, want := r.Header.Get("Content-Type"), "application/json"; got != want {
			t.Fatalf("content-type = %q, want %q", got, want)
		}

		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}

		var payload requestPayload
		if err := json.Unmarshal(bodyBytes, &payload); err != nil {
			t.Fatalf("unmarshal body: %v", err)
		}
		if got, want := payload.Name, "demo"; got != want {
			t.Fatalf("payload.Name = %q, want %q", got, want)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(api.Success("req_123", responsePayload{ID: "loop_1"}))
	}))
	defer server.Close()

	client := NewDaemonAPIClient(DaemonAPIClientOptions{
		BaseURL: server.URL + "/",
		Token:   "abc123",
	})

	var result responsePayload
	err := client.Post(context.Background(), "/v1/loops", requestPayload{Name: "demo"}, &result)
	if err != nil {
		t.Fatalf("Post() error = %v, want nil", err)
	}
	if got, want := result.ID, "loop_1"; got != want {
		t.Fatalf("result.ID = %q, want %q", got, want)
	}
}

func TestDaemonAPIClientGetSuccess(t *testing.T) {
	t.Parallel()

	type responsePayload struct {
		Status string `json:"status"`
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.Method, http.MethodGet; got != want {
			t.Fatalf("method = %q, want %q", got, want)
		}
		if got, want := r.URL.Path, "/v1/status"; got != want {
			t.Fatalf("path = %q, want %q", got, want)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(api.Success("req_456", responsePayload{Status: "ok"}))
	}))
	defer server.Close()

	client := NewDaemonAPIClient(DaemonAPIClientOptions{BaseURL: server.URL + "///"})

	var result responsePayload
	err := client.Get(context.Background(), "/v1/status", &result)
	if err != nil {
		t.Fatalf("Get() error = %v, want nil", err)
	}
	if got, want := result.Status, "ok"; got != want {
		t.Fatalf("result.Status = %q, want %q", got, want)
	}
}

func TestDaemonAPIClientUnreachableReturnsHelpfulError(t *testing.T) {
	t.Parallel()

	client := NewDaemonAPIClient(DaemonAPIClientOptions{BaseURL: "http://127.0.0.1:1"})

	var out map[string]any
	err := client.Get(context.Background(), "/v1/status", &out)
	if err == nil {
		t.Fatalf("Get() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "looperd is not reachable:") {
		t.Fatalf("error = %q, want reachable prefix", err.Error())
	}
}

func TestDaemonAPIClientEnvelopeFailureReturnsTypedError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(api.Failure("req_789", api.ErrorCodeValidationFailed, "bad input", map[string]any{"field": "name"}))
	}))
	defer server.Close()

	client := NewDaemonAPIClient(DaemonAPIClientOptions{BaseURL: server.URL})

	var out map[string]any
	err := client.Get(context.Background(), "/v1/things", &out)
	if err == nil {
		t.Fatalf("Get() error = nil, want error")
	}

	var apiErr *DaemonAPIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error type = %T, want *DaemonAPIError", err)
	}
	if got, want := apiErr.Message, "bad input"; got != want {
		t.Fatalf("message = %q, want %q", got, want)
	}
	if got, want := apiErr.Code, api.ErrorCodeValidationFailed; got != want {
		t.Fatalf("code = %q, want %q", got, want)
	}
	if got, want := apiErr.Status, http.StatusOK; got != want {
		t.Fatalf("status = %d, want %d", got, want)
	}
	if got, want := apiErr.RequestID, "req_789"; got != want {
		t.Fatalf("requestID = %q, want %q", got, want)
	}
}

func TestDaemonAPIClientNon2xxWithErrorEnvelopeReturnsTypedError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(api.Failure("req_401", api.ErrorCodeUnauthorized, "unauthorized", nil))
	}))
	defer server.Close()

	client := NewDaemonAPIClient(DaemonAPIClientOptions{BaseURL: server.URL})

	var out map[string]any
	err := client.Get(context.Background(), "/v1/private", &out)
	if err == nil {
		t.Fatalf("Get() error = nil, want error")
	}

	var apiErr *DaemonAPIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error type = %T, want *DaemonAPIError", err)
	}
	if got, want := apiErr.Message, "unauthorized"; got != want {
		t.Fatalf("message = %q, want %q", got, want)
	}
	if got, want := apiErr.Code, api.ErrorCodeUnauthorized; got != want {
		t.Fatalf("code = %q, want %q", got, want)
	}
	if got, want := apiErr.Status, http.StatusUnauthorized; got != want {
		t.Fatalf("status = %d, want %d", got, want)
	}
	if got, want := apiErr.RequestID, "req_401"; got != want {
		t.Fatalf("requestID = %q, want %q", got, want)
	}
}

func TestDaemonAPIClientSuccessMissingDataReturnsTypedError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(api.Envelope[any]{OK: true, RequestID: "req_nil"})
	}))
	defer server.Close()

	client := NewDaemonAPIClient(DaemonAPIClientOptions{BaseURL: server.URL})

	var out map[string]any
	err := client.Get(context.Background(), "/v1/status", &out)
	if err == nil {
		t.Fatalf("Get() error = nil, want error")
	}

	var apiErr *DaemonAPIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error type = %T, want *DaemonAPIError", err)
	}
	if got, want := apiErr.Message, "Request failed with status 200"; got != want {
		t.Fatalf("message = %q, want %q", got, want)
	}
	if got, want := apiErr.Status, http.StatusOK; got != want {
		t.Fatalf("status = %d, want %d", got, want)
	}
	if got, want := apiErr.RequestID, "req_nil"; got != want {
		t.Fatalf("requestID = %q, want %q", got, want)
	}
}
