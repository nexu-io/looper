package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	looperdruntime "github.com/nexu-io/looper/internal/runtime"
	pkgapi "github.com/nexu-io/looper/pkg/api"
)

type admissionGateRuntime struct {
	err error
}

func (r admissionGateRuntime) Services() looperdruntime.Services {
	return looperdruntime.Services{}
}

func (r admissionGateRuntime) StartedAt() (time.Time, bool) {
	return time.Time{}, false
}

func (r admissionGateRuntime) AllowMutations() error {
	return r.err
}

func TestHandlerMutationAdmissionGate(t *testing.T) {
	t.Parallel()

	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}

	handler := NewHandler(Context{
		Config:  cfg,
		Runtime: admissionGateRuntime{err: looperdruntime.ErrAdmissionNotReady},
	})

	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/loops", nil)
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("POST while not ready status = %d body=%s, want 503", recorder.Code, recorder.Body.String())
	}
	var envelope pkgapi.Envelope[any]
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode body error = %v", err)
	}
	if envelope.Error == nil || envelope.Error.Code != pkgapi.ErrorCodeServiceUnavailable {
		t.Fatalf("error = %#v, want SERVICE_UNAVAILABLE", envelope.Error)
	}

	// GET is not gated by admission.
	if isMutatingHTTPMethod(http.MethodGet) {
		t.Fatal("GET classified as mutating")
	}

	// Ready admission admits mutations past the gate (route may still 4xx/5xx for other reasons).
	ready := NewHandler(Context{
		Config:  cfg,
		Runtime: admissionGateRuntime{err: nil},
	})
	readyRecorder := httptest.NewRecorder()
	readyReq := httptest.NewRequest(http.MethodPost, "/api/v1/loops", nil)
	ready.ServeHTTP(readyRecorder, readyReq)
	if readyRecorder.Code == http.StatusServiceUnavailable {
		t.Fatalf("POST while ready status = %d, want not 503", readyRecorder.Code)
	}

	// Degraded is also 503.
	degraded := NewHandler(Context{
		Config:  cfg,
		Runtime: admissionGateRuntime{err: looperdruntime.ErrAdmissionDegraded},
	})
	degradedRecorder := httptest.NewRecorder()
	degraded.ServeHTTP(degradedRecorder, httptest.NewRequest(http.MethodPost, "/api/v1/loops", nil))
	if degradedRecorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("POST while degraded status = %d, want 503", degradedRecorder.Code)
	}
}

// Contract: Feishu url_verification must echo the challenge while admission is
// closed; real card actions still require admission (#583).
func TestHandlerFeishuURLVerificationBypassesAdmission(t *testing.T) {
	t.Parallel()

	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	handler := NewHandler(Context{
		Config:  cfg,
		Runtime: admissionGateRuntime{err: looperdruntime.ErrAdmissionStopping},
	})

	challengeBody := `{"type":"url_verification","challenge":"abc123","token":"t"}`
	challengeRec := httptest.NewRecorder()
	handler.ServeHTTP(challengeRec, httptest.NewRequest(http.MethodPost, "/api/v1/hitl/feishu", strings.NewReader(challengeBody)))
	if challengeRec.Code != http.StatusOK {
		t.Fatalf("url_verification while stopping status = %d body=%s, want 200", challengeRec.Code, challengeRec.Body.String())
	}
	if !strings.Contains(challengeRec.Body.String(), `"challenge":"abc123"`) {
		t.Fatalf("challenge echo missing: %s", challengeRec.Body.String())
	}

	// Non-handshake Feishu callbacks remain gated.
	actionBody := `{"action":{"tag":"button","value":{"loopSeq":"1","answer":"yes"}}}`
	actionRec := httptest.NewRecorder()
	handler.ServeHTTP(actionRec, httptest.NewRequest(http.MethodPost, "/api/v1/hitl/feishu", strings.NewReader(actionBody)))
	if actionRec.Code != http.StatusServiceUnavailable {
		t.Fatalf("card action while stopping status = %d body=%s, want 503", actionRec.Code, actionRec.Body.String())
	}
}
