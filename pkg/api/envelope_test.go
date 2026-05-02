package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

func TestEnvelopeJSONShape(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		envelope := Success("request-1", map[string]string{"status": "ok"})

		payload, err := json.Marshal(envelope)
		if err != nil {
			t.Fatalf("json.Marshal() error = %v", err)
		}

		want := `{"ok":true,"data":{"status":"ok"},"requestId":"request-1"}`
		if string(payload) != want {
			t.Fatalf("json.Marshal() = %s, want %s", payload, want)
		}
	})

	t.Run("error omits details when nil", func(t *testing.T) {
		envelope := Failure("request-2", ErrorCodeValidationFailed, "bad input", nil)

		payload, err := json.Marshal(envelope)
		if err != nil {
			t.Fatalf("json.Marshal() error = %v", err)
		}

		want := `{"ok":false,"error":{"code":"VALIDATION_FAILED","message":"bad input"},"requestId":"request-2"}`
		if string(payload) != want {
			t.Fatalf("json.Marshal() = %s, want %s", payload, want)
		}
	})

	t.Run("error includes details when present", func(t *testing.T) {
		envelope := Failure("request-3", ErrorCodeInternalError, "boom", map[string]any{"retryable": true})

		payload, err := json.Marshal(envelope)
		if err != nil {
			t.Fatalf("json.Marshal() error = %v", err)
		}

		want := `{"ok":false,"error":{"code":"INTERNAL_ERROR","message":"boom","details":{"retryable":true}},"requestId":"request-3"}`
		if string(payload) != want {
			t.Fatalf("json.Marshal() = %s, want %s", payload, want)
		}
	})
}

func TestAllErrorCodesMatchFrozenArtifact(t *testing.T) {
	type frozenErrorCode struct {
		Code   string `json:"code"`
		Status int    `json:"status"`
	}

	type frozenArtifact struct {
		ErrorCodes []frozenErrorCode `json:"errorCodes"`
	}

	artifactPath := filepath.Join("..", "..", "internal", "api", "testdata", "contracts", "daemon-http.errors.compat.json")
	contents, err := os.ReadFile(artifactPath)
	if err != nil {
		t.Fatalf("os.ReadFile(%q) error = %v", artifactPath, err)
	}

	var artifact frozenArtifact
	if err := json.Unmarshal(contents, &artifact); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}

	got := make([]frozenErrorCode, 0, len(AllErrorCodes()))
	for _, code := range AllErrorCodes() {
		got = append(got, frozenErrorCode{Code: code.String(), Status: code.Status()})
	}

	sort.Slice(got, func(i, j int) bool {
		return got[i].Code < got[j].Code
	})
	sort.Slice(artifact.ErrorCodes, func(i, j int) bool {
		return artifact.ErrorCodes[i].Code < artifact.ErrorCodes[j].Code
	})

	if !reflect.DeepEqual(got, artifact.ErrorCodes) {
		t.Fatalf("AllErrorCodes() mismatch (-got +want):\ngot:  %#v\nwant: %#v", got, artifact.ErrorCodes)
	}
}
