package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/loops"
	looperdruntime "github.com/nexu-io/looper/internal/runtime"
	"github.com/nexu-io/looper/internal/storage"
)

func TestHandlerRespondResumesAwaitingHumanLoop(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	h := NewHandler(Context{Config: cfg, Runtime: rt})
	services := rt.Services()
	nowISO := "2026-04-11T12:00:00.000Z"
	projectID := "project_hitl"
	loopID := "loop_hitl"
	targetID := projectID
	metadata := `{"hitl":{"question":"Which direction?","options":["continue","redirect"],"sessionId":"sess-abc","executionId":"agent-1","vendor":"codex","status":"awaiting","askedAt":"2026-04-11T11:59:00.000Z"}}`

	if err := services.Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Looper", RepoPath: "/tmp/repos/looper", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := services.Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 71, ProjectID: projectID, Type: "worker", TargetType: "project", TargetID: &targetID, Status: "awaiting_human", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	// A cancelled queue item (as a suspend leaves behind) so the resume requeues it.
	cancelReason := "loop suspended awaiting human"
	if err := services.Repositories.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: "queue_hitl", ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: targetID, DedupeKey: "worker:hitl", Priority: storage.QueuePriorityWorker, Status: "cancelled", AvailableAt: nowISO, Attempts: 0, MaxAttempts: 3, LastError: &cancelReason, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/loops/71/respond", strings.NewReader(`{"answer":"continue with the redis approach"}`))
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}

	loop, err := services.Repositories.Loops.GetByID(context.Background(), loopID)
	if err != nil || loop == nil {
		t.Fatalf("Loops.GetByID() = %#v, %v", loop, err)
	}
	if loop.Status != "running" {
		t.Fatalf("loop.Status = %q, want running", loop.Status)
	}
	ask, ok := loops.ReadHITLAsk(loop.MetadataJSON)
	if !ok {
		t.Fatalf("HITL ask metadata missing after respond")
	}
	if ask.Answer != "continue with the redis approach" {
		t.Fatalf("ask.Answer = %q, want the posted answer", ask.Answer)
	}
	if ask.Status != "answered" {
		t.Fatalf("ask.Status = %q, want answered", ask.Status)
	}
	if ask.SessionID != "sess-abc" {
		t.Fatalf("ask.SessionID = %q, want preserved sess-abc", ask.SessionID)
	}

	// The loop must be requeued so the scheduler resumes it.
	items, err := services.Repositories.Queue.List(context.Background())
	if err != nil {
		t.Fatalf("Queue.List() error = %v", err)
	}
	queued := false
	for _, item := range items {
		if item.LoopID != nil && *item.LoopID == loopID && item.Status == "queued" {
			queued = true
		}
	}
	if !queued {
		t.Fatalf("expected a queued queue item for the resumed loop; items=%#v", items)
	}
}

func TestHandlerRespondRejectsNonAwaitingLoop(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	h := NewHandler(Context{Config: cfg, Runtime: rt})
	services := rt.Services()
	nowISO := "2026-04-11T12:00:00.000Z"
	projectID := "project_hitl_reject"
	loopID := "loop_hitl_reject"
	targetID := projectID

	if err := services.Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Looper", RepoPath: "/tmp/repos/looper", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := services.Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 72, ProjectID: projectID, Type: "worker", TargetType: "project", TargetID: &targetID, Status: "paused", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/loops/72/respond", strings.NewReader(`{"answer":"continue"}`))
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", recorder.Code, recorder.Body.String())
	}
	loop, err := services.Repositories.Loops.GetByID(context.Background(), loopID)
	if err != nil || loop == nil {
		t.Fatalf("Loops.GetByID() = %#v, %v", loop, err)
	}
	if loop.Status != "paused" {
		t.Fatalf("loop.Status = %q, want unchanged paused", loop.Status)
	}
}

func TestHandlerFeishuCardActionDeliversAnswer(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	cfg.HITL.Enabled = true
	// The card-action route is fail-closed: it requires a configured, matching
	// Feishu verification token before it will deliver an answer.
	t.Setenv("LOOPER_TEST_FEISHU_VTOKEN", "verify-tok-123")
	cfg.Notifications.Webhook.VerificationTokenEnv = "LOOPER_TEST_FEISHU_VTOKEN"
	h := NewHandler(Context{Config: cfg, Runtime: rt})
	services := rt.Services()
	nowISO := "2026-04-11T12:00:00.000Z"
	projectID := "project_card"
	loopID := "loop_card"
	targetID := projectID
	metadata := `{"hitl":{"question":"q","sessionId":"sess-1","status":"awaiting"}}`

	if err := services.Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Looper", RepoPath: "/tmp/repos/looper", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := services.Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 81, ProjectID: projectID, Type: "worker", TargetType: "project", TargetID: &targetID, Status: "awaiting_human", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	body := `{"token":"verify-tok-123","action":{"tag":"button","value":{"loopSeq":"81","answer":"redis"}}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/hitl/feishu", strings.NewReader(body))
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	loop, err := services.Repositories.Loops.GetByID(context.Background(), loopID)
	if err != nil || loop == nil {
		t.Fatalf("Loops.GetByID() = %#v, %v", loop, err)
	}
	if loop.Status != "running" {
		t.Fatalf("loop.Status = %q, want running", loop.Status)
	}
	ask, ok := loops.ReadHITLAsk(loop.MetadataJSON)
	if !ok || ask.Answer != "redis" || ask.Status != "answered" {
		t.Fatalf("ask = %#v (ok=%v), want answer redis + answered", ask, ok)
	}
}

// setupAwaitingCardLoop seeds a project + awaiting_human loop and returns the
// handler + services for card-action security tests.
func setupAwaitingCardLoop(t *testing.T, cfg config.Config, rt *looperdruntime.Runtime, projectID, loopID string, seq int64) *Handler {
	t.Helper()
	h := NewHandler(Context{Config: cfg, Runtime: rt})
	services := rt.Services()
	nowISO := "2026-04-11T12:00:00.000Z"
	targetID := projectID
	metadata := `{"hitl":{"question":"q","sessionId":"sess-1","status":"awaiting"}}`
	if err := services.Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Looper", RepoPath: "/tmp/repos/looper", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := services.Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: seq, ProjectID: projectID, Type: "worker", TargetType: "project", TargetID: &targetID, Status: "awaiting_human", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	return h
}

func TestHandlerFeishuCardActionRejectsWhenTokenNotConfigured(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	cfg.HITL.Enabled = true
	// No verificationTokenEnv configured -> the injection route must fail closed.
	h := setupAwaitingCardLoop(t, cfg, rt, "project_card_notok", "loop_card_notok", 82)

	body := `{"token":"anything","action":{"tag":"button","value":{"loopSeq":"82","answer":"redis"}}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/hitl/feishu", strings.NewReader(body))
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 when verification token unconfigured; body=%s", recorder.Code, recorder.Body.String())
	}
	loop, err := rt.Services().Repositories.Loops.GetByID(context.Background(), "loop_card_notok")
	if err != nil || loop == nil {
		t.Fatalf("Loops.GetByID() = %#v, %v", loop, err)
	}
	if loop.Status != "awaiting_human" {
		t.Fatalf("loop.Status = %q, want unchanged awaiting_human (no answer delivered)", loop.Status)
	}
}

func TestHandlerFeishuCardActionRejectsTokenMismatch(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	cfg.HITL.Enabled = true
	t.Setenv("LOOPER_TEST_FEISHU_VTOKEN2", "the-real-token")
	cfg.Notifications.Webhook.VerificationTokenEnv = "LOOPER_TEST_FEISHU_VTOKEN2"
	h := setupAwaitingCardLoop(t, cfg, rt, "project_card_bad", "loop_card_bad", 83)

	body := `{"token":"wrong-token","action":{"tag":"button","value":{"loopSeq":"83","answer":"redis"}}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/hitl/feishu", strings.NewReader(body))
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 on token mismatch; body=%s", recorder.Code, recorder.Body.String())
	}
	loop, err := rt.Services().Repositories.Loops.GetByID(context.Background(), "loop_card_bad")
	if err != nil || loop == nil {
		t.Fatalf("Loops.GetByID() = %#v, %v", loop, err)
	}
	if loop.Status != "awaiting_human" {
		t.Fatalf("loop.Status = %q, want unchanged awaiting_human (no answer delivered)", loop.Status)
	}
}

func TestHandlerFeishuCardActionAnswersChallenge(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	h := NewHandler(Context{Config: cfg, Runtime: rt})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/hitl/feishu", strings.NewReader(`{"type":"url_verification","challenge":"abc123","token":"t"}`))
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), `"challenge":"abc123"`) {
		t.Fatalf("challenge echo missing: %s", recorder.Body.String())
	}
}

func TestHandlerFeishuCardActionGatedWhenHITLDisabled(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	// cfg.HITL.Enabled defaults to false.
	h := NewHandler(Context{Config: cfg, Runtime: rt})

	body := `{"action":{"tag":"button","value":{"loopSeq":"81","answer":"redis"}}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/hitl/feishu", strings.NewReader(body))
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 when hitl disabled; body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestHandlerFeishuThreadReplyDeliversTypedAnswer(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	cfg.HITL.Enabled = true
	t.Setenv("LOOPER_TEST_FEISHU_VTOKEN3", "verify-tok-123")
	cfg.Notifications.Webhook.VerificationTokenEnv = "LOOPER_TEST_FEISHU_VTOKEN3"
	h := setupAwaitingCardLoop(t, cfg, rt, "project_thread", "loop_thread", 91)
	services := rt.Services()
	// The gateway would have recorded this when it created the thread root.
	if err := services.Repositories.FeishuThreads.Upsert(context.Background(), "om_root_91", "loop_thread", "oc_group", "2026-04-11T12:00:00.000Z"); err != nil {
		t.Fatalf("FeishuThreads.Upsert() error = %v", err)
	}

	// A human types a free-text reply in the ask thread (im.message.receive_v1).
	body := `{"schema":"2.0","header":{"event_type":"im.message.receive_v1","token":"verify-tok-123"},"event":{"message":{"message_id":"om_reply","root_id":"om_root_91","chat_id":"oc_group","message_type":"text","content":"{\"text\":\"用 A 改 resize handle\"}"},"sender":{"sender_type":"user","sender_id":{"open_id":"ou_user"}}}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/hitl/feishu", strings.NewReader(body))
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}

	loop, err := services.Repositories.Loops.GetByID(context.Background(), "loop_thread")
	if err != nil || loop == nil {
		t.Fatalf("Loops.GetByID() = %#v, %v", loop, err)
	}
	if loop.Status != "running" {
		t.Fatalf("loop.Status = %q, want running (resumed by typed reply)", loop.Status)
	}
	ask, ok := loops.ReadHITLAsk(loop.MetadataJSON)
	if !ok || ask.Answer != "用 A 改 resize handle" || ask.Status != "answered" {
		t.Fatalf("ask = %#v (ok=%v), want the typed free-text answer", ask, ok)
	}
}

func TestHandlerFeishuThreadReplyIgnoresUnknownThread(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	cfg.HITL.Enabled = true
	t.Setenv("LOOPER_TEST_FEISHU_VTOKEN4", "verify-tok-123")
	cfg.Notifications.Webhook.VerificationTokenEnv = "LOOPER_TEST_FEISHU_VTOKEN4"
	h := setupAwaitingCardLoop(t, cfg, rt, "project_thread2", "loop_thread2", 92)

	// A reply in a thread with no mapped loop must be ignored (200, not delivered),
	// so ordinary group chatter doesn't error or touch any loop.
	body := `{"schema":"2.0","header":{"event_type":"im.message.receive_v1","token":"verify-tok-123"},"event":{"message":{"message_id":"om_x","root_id":"om_unknown","chat_id":"oc_group","message_type":"text","content":"{\"text\":\"just chatting\"}"},"sender":{"sender_type":"user","sender_id":{"open_id":"ou_user"}}}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/hitl/feishu", strings.NewReader(body))
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (ignored); body=%s", recorder.Code, recorder.Body.String())
	}
	loop, err := rt.Services().Repositories.Loops.GetByID(context.Background(), "loop_thread2")
	if err != nil || loop == nil {
		t.Fatalf("Loops.GetByID() = %#v, %v", loop, err)
	}
	if loop.Status != "awaiting_human" {
		t.Fatalf("loop.Status = %q, want unchanged awaiting_human", loop.Status)
	}
}

func TestHandlerRespondRequiresAnswer(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	h := NewHandler(Context{Config: cfg, Runtime: rt})
	services := rt.Services()
	nowISO := "2026-04-11T12:00:00.000Z"
	projectID := "project_hitl_empty"
	loopID := "loop_hitl_empty"
	targetID := projectID

	if err := services.Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Looper", RepoPath: "/tmp/repos/looper", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := services.Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 73, ProjectID: projectID, Type: "worker", TargetType: "project", TargetID: &targetID, Status: "awaiting_human", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/loops/73/respond", strings.NewReader(`{"answer":"   "}`))
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for empty answer; body=%s", recorder.Code, recorder.Body.String())
	}
}
