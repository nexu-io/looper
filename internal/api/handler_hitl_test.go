package api

import (
	"context"
	"fmt"
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
	repo := "acme/looper"
	prNumber := int64(42)
	targetID := "pr:acme/looper:42"
	metadata := `{"hitl":{"question":"Which direction?","options":["continue","redirect"],"sessionId":"sess-abc","executionId":"agent-1","vendor":"codex","status":"awaiting","askedAt":"2026-04-11T11:59:00.000Z"}}`

	if err := services.Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Looper", RepoPath: "/tmp/repos/looper", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := services.Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 71, ProjectID: projectID, Type: "fixer", TargetType: "pull_request", TargetID: &targetID, Repo: &repo, PRNumber: &prNumber, Status: "awaiting_human", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	// A cancelled queue item (as a suspend leaves behind) so the resume requeues it.
	cancelReason := "loop suspended awaiting human"
	if err := services.Repositories.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: "queue_hitl", ProjectID: &projectID, LoopID: &loopID, Type: "fixer", TargetType: "pull_request", TargetID: targetID, Repo: &repo, PRNumber: &prNumber, DedupeKey: "fixer:hitl", Priority: storage.QueuePriorityFixer, Status: "cancelled", AvailableAt: nowISO, Attempts: 0, MaxAttempts: 3, LastError: &cancelReason, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
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

// HITL resume re-enters mutateLoopStatus(...Running). When live worker/global
// vendor was removed while the loop waited, sticky agent_snapshot_json on the
// interrupted predecessor must still allow answer requeue (same rule as retry).
func TestHandlerRespondAllowsStickySnapshotWhenAgentNotConfigured(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	cfg.Agent.Vendor = nil
	h := NewHandler(Context{Config: cfg, Runtime: runtimeWithConfig(rt, cfg)})
	services := rt.Services()
	nowISO := "2026-04-11T12:00:00.000Z"
	projectID := "project_hitl_sticky_snapshot"
	loopID := "loop_hitl_sticky_snapshot"
	targetID := projectID
	metadata := `{"hitl":{"question":"Continue?","options":["yes","no"],"sessionId":"sess-sticky","status":"awaiting","askedAt":"2026-04-11T11:59:00.000Z"}}`

	if err := services.Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Looper", RepoPath: "/tmp/repos/looper", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := services.Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 79, ProjectID: projectID, Type: "worker", TargetType: "project", TargetID: &targetID, Status: "awaiting_human", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	cancelReason := "worker suspended awaiting human decision"
	if err := services.Repositories.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: "queue_hitl_sticky", ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: targetID, DedupeKey: "worker:hitl-sticky", Priority: storage.QueuePriorityWorker, Status: "cancelled", AvailableAt: nowISO, Attempts: 0, MaxAttempts: 3, LastError: &cancelReason, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}
	snapshot := `{"vendor":"codex","model":"frozen-hitl","profileId":"worker-profile"}`
	if err := services.Repositories.Runs.Upsert(context.Background(), storage.RunRecord{
		ID: "run_" + loopID + "_hitl", LoopID: loopID, Status: "interrupted",
		StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO, AgentSnapshotJSON: &snapshot,
	}); err != nil {
		t.Fatalf("Runs.Upsert(snapshot) error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/loops/79/respond", strings.NewReader(`{"answer":"yes continue"}`))
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 sticky HITL resume with predecessor snapshot; body=%s", recorder.Code, recorder.Body.String())
	}
	loop, err := services.Repositories.Loops.GetByID(context.Background(), loopID)
	if err != nil || loop == nil || loop.Status != "running" {
		t.Fatalf("loop after sticky HITL respond = %#v, %v, want running", loop, err)
	}
	ask, ok := loops.ReadHITLAsk(loop.MetadataJSON)
	if !ok || ask.Answer != "yes continue" || ask.Status != "answered" {
		t.Fatalf("HITL ask after sticky respond = %#v, ok=%v", ask, ok)
	}
}

func TestHandlerRespondRejectsWhenAgentNotConfiguredWithoutSnapshot(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	cfg.Agent.Vendor = nil
	h := NewHandler(Context{Config: cfg, Runtime: runtimeWithConfig(rt, cfg)})
	services := rt.Services()
	nowISO := "2026-04-11T12:00:00.000Z"
	projectID := "project_hitl_no_snapshot"
	loopID := "loop_hitl_no_snapshot"
	targetID := projectID
	metadata := `{"hitl":{"question":"Continue?","sessionId":"sess-none","status":"awaiting"}}`

	if err := services.Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Looper", RepoPath: "/tmp/repos/looper", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := services.Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 80, ProjectID: projectID, Type: "worker", TargetType: "project", TargetID: &targetID, Status: "awaiting_human", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/loops/80/respond", strings.NewReader(`{"answer":"yes"}`))
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 without live vendor or snapshot; body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "without config.agent.vendor") {
		t.Fatalf("body = %s, want agent not configured rejection", recorder.Body.String())
	}
	loop, err := services.Repositories.Loops.GetByID(context.Background(), loopID)
	// Answer is stored before mutateLoopStatus; loop may stay awaiting_human if
	// requeue fails after metadata write — either way respond must not succeed.
	if err != nil || loop == nil {
		t.Fatalf("Loops.GetByID() = %#v, %v", loop, err)
	}
	if loop.Status == "running" {
		t.Fatalf("loop.Status = running, want not requeued without agent identity")
	}
}

func TestHandlerFeishuCardActionDeliversAnswer(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	cfg.HITL.Enabled = true
	// The card-action route is fail-closed: it requires a configured, matching
	// Feishu verification token before it will deliver an answer.
	t.Setenv("LOOPER_TEST_FEISHU_VTOKEN", "verify-tok-123")
	cfg.Notifications.Webhook.VerificationTokenEnv = "LOOPER_TEST_FEISHU_VTOKEN"
	h := NewHandler(Context{Config: cfg, Runtime: runtimeWithConfig(rt, cfg)})
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
	h := NewHandler(Context{Config: cfg, Runtime: runtimeWithConfig(rt, cfg)})
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

func TestHandlerRespondReviewFixBudgetContinueTriggersScheduler(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	triggered := 0
	h := NewHandler(Context{Config: cfg, Runtime: rt, TriggerSchedulerTick: func() { triggered++ }})
	seedReviewFixBudgetAwaitingLoop(t, rt, "project_budget_continue", "loop_budget_continue", 641)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/loops/641/respond", strings.NewReader(`{"answer":"Continue"}`))
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	if triggered != 1 {
		t.Fatalf("TriggerSchedulerTick called %d times, want 1 after budget Continue", triggered)
	}

	loop, err := rt.Services().Repositories.Loops.GetByID(context.Background(), "loop_budget_continue")
	if err != nil || loop == nil || loop.Status != "queued" {
		t.Fatalf("loop after budget Continue = %#v, %v, want queued", loop, err)
	}
	active, err := rt.Services().Repositories.Queue.FindActiveByLoopID(context.Background(), "loop_budget_continue")
	if err != nil || active == nil || active.Status != "queued" {
		t.Fatalf("queue after budget Continue = %#v, %v, want queued", active, err)
	}
}

func TestHandlerRespondReviewFixBudgetInvalidAnswerIsValidationError(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	triggered := 0
	h := NewHandler(Context{Config: cfg, Runtime: rt, TriggerSchedulerTick: func() { triggered++ }})
	seedReviewFixBudgetAwaitingLoop(t, rt, "project_budget_invalid", "loop_budget_invalid", 642)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/loops/642/respond", strings.NewReader(`{"answer":"maybe later"}`))
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", recorder.Code, recorder.Body.String())
	}
	body := parseJSONMap(t, recorder.Body.Bytes())
	errMap := body["error"].(map[string]any)
	if errMap["code"] != "VALIDATION_FAILED" {
		t.Fatalf("error.code = %#v, want VALIDATION_FAILED", errMap["code"])
	}
	if !strings.Contains(fmt.Sprint(errMap["message"]), "Continue") {
		t.Fatalf("error.message = %#v, want invalid-option text", errMap["message"])
	}
	if triggered != 0 {
		t.Fatalf("TriggerSchedulerTick called %d times, want 0 for invalid budget answer", triggered)
	}
	loop, err := rt.Services().Repositories.Loops.GetByID(context.Background(), "loop_budget_invalid")
	if err != nil || loop == nil || loop.Status != "awaiting_human" {
		t.Fatalf("loop after invalid budget answer = %#v, %v, want awaiting_human", loop, err)
	}
}

func TestHandlerRespondReviewFixBudgetStorageFailureIsInternalError(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	triggered := 0
	h := NewHandler(Context{Config: cfg, Runtime: rt, TriggerSchedulerTick: func() { triggered++ }})
	seedReviewFixBudgetAwaitingLoop(t, rt, "project_budget_storage", "loop_budget_storage", 643)

	if _, err := rt.Services().Coordinator.DB().ExecContext(context.Background(), `
		CREATE TRIGGER fail_budget_loop_update
		BEFORE UPDATE ON loops
		BEGIN
			SELECT RAISE(ABORT, 'database is locked');
		END
	`); err != nil {
		t.Fatalf("create storage-failure trigger: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/loops/643/respond", strings.NewReader(`{"answer":"Continue"}`))
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", recorder.Code, recorder.Body.String())
	}
	body := parseJSONMap(t, recorder.Body.Bytes())
	errMap := body["error"].(map[string]any)
	if errMap["code"] != "INTERNAL_ERROR" {
		t.Fatalf("error.code = %#v, want INTERNAL_ERROR", errMap["code"])
	}
	if triggered != 0 {
		t.Fatalf("TriggerSchedulerTick called %d times, want 0 after storage failure", triggered)
	}
	loop, err := rt.Services().Repositories.Loops.GetByID(context.Background(), "loop_budget_storage")
	if err != nil || loop == nil || loop.Status != "awaiting_human" {
		t.Fatalf("loop after storage failure = %#v, %v, want awaiting_human", loop, err)
	}
}

func TestHandlerRespondBudgetContinueReturnsPromotedScopeHold(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	triggered := 0
	h := NewHandler(Context{Config: cfg, Runtime: rt, TriggerSchedulerTick: func() { triggered++ }})
	seedReviewFixBudgetAwaitingLoop(t, rt, "project_budget_promote_scope", "loop_budget_promote_scope", 644)

	services := rt.Services()
	loop, err := services.Repositories.Loops.GetByID(context.Background(), "loop_budget_promote_scope")
	if err != nil || loop == nil {
		t.Fatalf("GetByID() = (%#v, %v)", loop, err)
	}
	encoded, err := loops.PersistPendingReviewScopeHumanEvidence(loop.MetadataJSON, "Clarify AGENTS.md rule X before unpause", "PR non-goals exclude API expansion", true)
	if err != nil {
		t.Fatalf("PersistPendingReviewScopeHumanEvidence: %v", err)
	}
	loop.MetadataJSON = &encoded
	if err := services.Repositories.Loops.Upsert(context.Background(), *loop); err != nil {
		t.Fatalf("Loops.Upsert pending: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/loops/644/respond", strings.NewReader(`{"answer":"Continue"}`))
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	if triggered != 1 {
		t.Fatalf("TriggerSchedulerTick called %d times, want 1 after budget Continue promote", triggered)
	}

	updated, err := services.Repositories.Loops.GetByID(context.Background(), "loop_budget_promote_scope")
	if err != nil || updated == nil {
		t.Fatalf("loop after promote = (%#v, %v)", updated, err)
	}
	if updated.Status != "awaiting_human" {
		t.Fatalf("status = %q, want awaiting_human promoted scope hold (not running/queued)", updated.Status)
	}
	if !loops.IsReviewScopeHumanHold(*updated) || loops.IsReviewFixBudgetHold(*updated) {
		t.Fatalf("hold = scope=%v budget=%v, want scope-only", loops.IsReviewScopeHumanHold(*updated), loops.IsReviewFixBudgetHold(*updated))
	}
	ask, ok := loops.ReadHITLAsk(updated.MetadataJSON)
	if !ok || !loops.IsReviewScopeHumanAsk(ask) {
		t.Fatalf("ask = (%#v, %v), want promoted scope ask", ask, ok)
	}
}

func TestHandlerRespondOrdinaryAskDoesNotReleaseScopeOverlay(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	h := NewHandler(Context{Config: cfg, Runtime: rt})
	services := rt.Services()
	nowISO := "2026-04-11T12:00:00.000Z"
	projectID := "project_scope_overlay"
	repo := "acme/looper"
	prNumber := int64(42)
	targetID := "pr:acme/looper:42"
	if err := services.Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Looper", RepoPath: "/tmp/repos/looper", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	reviewer := storage.LoopRecord{
		ID: "loop_scope_overlay_reviewer", Seq: 645, ProjectID: projectID, Type: "reviewer",
		TargetType: "pull_request", TargetID: &targetID, Repo: &repo, PRNumber: &prNumber,
		Status: "running", CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	if err := services.Repositories.Loops.Upsert(context.Background(), reviewer); err != nil {
		t.Fatalf("Loops.Upsert(reviewer): %v", err)
	}
	fixer := storage.LoopRecord{
		ID: "loop_scope_overlay_fixer", Seq: 646, ProjectID: projectID, Type: "fixer",
		TargetType: "pull_request", TargetID: &targetID, Repo: &repo, PRNumber: &prNumber,
		Status: "awaiting_human", CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	midAsk := loops.HITLAsk{
		Kind: "agent_question", Question: "Which approach should Fixer take?",
		Options: []string{"A", "B"}, Status: "awaiting", AskedAt: nowISO, PRNumber: prNumber,
	}
	meta, err := loops.WriteHITLAsk(fixer.MetadataJSON, midAsk)
	if err != nil {
		t.Fatalf("WriteHITLAsk: %v", err)
	}
	fixer.MetadataJSON = &meta
	if err := services.Repositories.Loops.Upsert(context.Background(), fixer); err != nil {
		t.Fatalf("Loops.Upsert(fixer): %v", err)
	}
	if _, err := loops.ParkReviewScopeHuman(context.Background(), services.Repositories, loops.ParkReviewScopeHumanInput{
		Held: reviewer, Role: "reviewer", Repo: repo, PRNumber: prNumber,
		NowISO: nowISO, HITLEnabled: true,
		Question: "Clarify AGENTS.md rule X before unpause",
	}); err != nil {
		t.Fatalf("ParkReviewScopeHuman: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/loops/646/respond", strings.NewReader(`{"answer":"A"}`))
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for ordinary overlay answer; body=%s", recorder.Code, recorder.Body.String())
	}

	freshFixer, err := services.Repositories.Loops.GetByID(context.Background(), fixer.ID)
	if err != nil || freshFixer == nil || freshFixer.Status != "awaiting_human" {
		t.Fatalf("fixer after ordinary answer = (%#v, %v), want awaiting_human", freshFixer, err)
	}
	ask, ok := loops.ReadHITLAsk(freshFixer.MetadataJSON)
	if !ok || ask.Question != midAsk.Question || ask.Status != "awaiting" {
		t.Fatalf("mid-run ask mutated: ok=%v ask=%#v", ok, ask)
	}
	if !loops.IsReviewScopeHumanHold(*freshFixer) {
		t.Fatalf("fixer overlay released by ordinary answer: %#v", freshFixer)
	}
	freshReviewer, err := services.Repositories.Loops.GetByID(context.Background(), reviewer.ID)
	if err != nil || freshReviewer == nil || !loops.IsReviewScopeHumanHold(*freshReviewer) {
		t.Fatalf("reviewer hold released by sibling ordinary answer: (%#v, %v)", freshReviewer, err)
	}
}

func TestHandlerRespondScopeStopDrainsLiveSibling(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	services := rt.Services()
	var drained []string
	h := NewHandler(Context{
		Config:  cfg,
		Runtime: rt,
		StopLoop: func(ctx context.Context, loopID, _ string) (any, error) {
			rec, err := services.Repositories.Loops.GetByID(ctx, loopID)
			if err != nil || rec == nil {
				t.Fatalf("StopLoop GetByID(%s) = (%v, %v)", loopID, rec, err)
			}
			if rec.Status == "terminated" || rec.Status == "stopped" {
				t.Fatalf("StopLoop(%s) after terminalize: status=%s", loopID, rec.Status)
			}
			drained = append(drained, loopID)
			if services.ActiveExecutions != nil {
				if _, stopErr := services.ActiveExecutions.BeginLoopStop(loopID, "test drain"); stopErr != nil {
					return nil, stopErr
				}
			}
			return map[string]any{"stopped": true, "loopId": loopID}, nil
		},
	})
	nowISO := "2026-04-11T12:00:00.000Z"
	projectID := "project_scope_respond_stop"
	repo := "acme/looper"
	prNumber := int64(42)
	targetID := "pr:acme/looper:42"
	if err := services.Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID: projectID, Name: "Looper", RepoPath: "/tmp/repos/looper", CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	reviewer := storage.LoopRecord{
		ID: "loop_scope_respond_stop_rev", Seq: 647, ProjectID: projectID, Type: "reviewer",
		TargetType: "pull_request", TargetID: &targetID, Repo: &repo, PRNumber: &prNumber,
		Status: "running", CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	fixer := storage.LoopRecord{
		ID: "loop_scope_respond_stop_fix", Seq: 648, ProjectID: projectID, Type: "fixer",
		TargetType: "pull_request", TargetID: &targetID, Repo: &repo, PRNumber: &prNumber,
		Status: "running", CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	if err := services.Repositories.Loops.Upsert(context.Background(), reviewer); err != nil {
		t.Fatalf("Loops.Upsert(reviewer): %v", err)
	}
	if err := services.Repositories.Loops.Upsert(context.Background(), fixer); err != nil {
		t.Fatalf("Loops.Upsert(fixer): %v", err)
	}
	if _, err := loops.ParkReviewScopeHuman(context.Background(), services.Repositories, loops.ParkReviewScopeHumanInput{
		Held: reviewer, Role: "reviewer", Repo: repo, PRNumber: prNumber,
		NowISO: nowISO, HITLEnabled: true,
		Question: "Clarify AGENTS.md rule X before unpause",
	}); err != nil {
		t.Fatalf("ParkReviewScopeHuman: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/loops/647/respond", strings.NewReader(`{"answer":"Stop"}`))
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	reviewerAfter, _ := services.Repositories.Loops.GetByID(context.Background(), reviewer.ID)
	fixerAfter, _ := services.Repositories.Loops.GetByID(context.Background(), fixer.ID)
	if reviewerAfter == nil || reviewerAfter.Status != "terminated" || fixerAfter == nil || fixerAfter.Status != "terminated" {
		t.Fatalf("pair after Stop = reviewer %#v fixer %#v, want both terminated", reviewerAfter, fixerAfter)
	}
	if len(drained) != 2 {
		t.Fatalf("StopLoop calls = %v, want both pair members before terminalize", drained)
	}
	seen := map[string]bool{drained[0]: true, drained[1]: true}
	if !seen[reviewer.ID] || !seen[fixer.ID] {
		t.Fatalf("drained = %v, want %s and %s", drained, reviewer.ID, fixer.ID)
	}
}

func seedReviewFixBudgetAwaitingLoop(t *testing.T, rt *looperdruntime.Runtime, projectID, loopID string, seq int64) {
	t.Helper()
	services := rt.Services()
	nowISO := "2026-04-11T12:00:00.000Z"
	repo := "acme/looper"
	prNumber := int64(42)
	targetID := "pr:acme/looper:42"
	if err := services.Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Looper", RepoPath: "/tmp/repos/looper", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	ask := loops.NewReviewFixBudgetAsk("fixer", repo, prNumber, 8, 8, nowISO)
	metadata, err := loops.WriteHITLAsk(nil, ask)
	if err != nil {
		t.Fatalf("WriteHITLAsk() error = %v", err)
	}
	if err := services.Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{
		ID: loopID, Seq: seq, ProjectID: projectID, Type: "fixer", TargetType: "pull_request",
		TargetID: &targetID, Repo: &repo, PRNumber: &prNumber, Status: "awaiting_human",
		MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	cancelReason := loops.ReviewFixBudgetPauseReason
	if err := services.Repositories.Queue.Upsert(context.Background(), storage.QueueItemRecord{
		ID: "queue_" + loopID, ProjectID: &projectID, LoopID: &loopID, Type: "fixer",
		TargetType: "pull_request", TargetID: targetID, Repo: &repo, PRNumber: &prNumber,
		DedupeKey: "fixer:budget:" + loopID, Priority: storage.QueuePriorityFixer, Status: "cancelled",
		AvailableAt: nowISO, Attempts: 0, MaxAttempts: 3, LastError: &cancelReason,
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}
}
