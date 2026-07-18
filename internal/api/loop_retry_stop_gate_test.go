package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/agent"
	"github.com/nexu-io/looper/internal/storage"
)

// Retry must restore the sticky stop gate when a later TX validation fails so
// a failed retry cannot reopen AdmitSpawn for stale pre-stop runners.
func TestHandlerLoopRetryRestoresStopGateOnTXConflict(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	h := NewHandler(Context{Config: cfg, Runtime: rt})
	services := rt.Services()
	nowISO := "2026-04-11T12:00:00.000Z"
	projectID := "project_retry_restore_stop_gate"
	loopID := "loop_retry_restore_stop_gate"
	targetID := projectID
	dedupeKey := "worker:restore_stop_gate"
	otherLoopID := "loop_retry_restore_stop_gate_other"

	if err := services.Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID: projectID, Name: "Looper", RepoPath: t.TempDir(), CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := services.Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{
		ID: loopID, Seq: 3141, ProjectID: projectID, Type: "worker", TargetType: "project",
		TargetID: &targetID, Status: "paused", CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	lastErrorKind := "manual_intervention"
	if err := services.Repositories.Queue.Upsert(context.Background(), storage.QueueItemRecord{
		ID: "queue_retry_restore_stop_gate_failed", ProjectID: &projectID, LoopID: &loopID, Type: "worker",
		TargetType: "project", TargetID: targetID, DedupeKey: dedupeKey,
		Priority: storage.QueuePriorityWorker, Status: "failed", AvailableAt: nowISO,
		Attempts: 1, MaxAttempts: 3, LastErrorKind: &lastErrorKind, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Queue.Upsert() failed item error = %v", err)
	}
	// Sibling loop exists but has no active queue yet so preflight dedupe passes.
	if err := services.Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{
		ID: otherLoopID, Seq: 3142, ProjectID: projectID, Type: "worker", TargetType: "project",
		TargetID: &targetID, Status: "paused", CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert() other loop error = %v", err)
	}
	if services.ActiveExecutions == nil {
		t.Fatal("ActiveExecutions is nil")
	}
	if _, err := services.ActiveExecutions.BeginLoopStop(loopID, "looper stop"); err != nil {
		t.Fatalf("BeginLoopStop: %v", err)
	}
	if !services.ActiveExecutions.LoopStopActive(loopID) {
		t.Fatal("LoopStopActive = false before retry, want sticky closed")
	}

	// After ClearLoopStop and before the requeue TX, inject an active dedupe
	// row so the TX fails the way a concurrent requeue would.
	h.retryAfterClearStopGateHook = func(id string) {
		if id != loopID {
			return
		}
		if services.ActiveExecutions.LoopStopActive(loopID) {
			t.Error("LoopStopActive still true in after-clear hook, want gate already cleared")
		}
		if err := services.Repositories.Queue.Upsert(context.Background(), storage.QueueItemRecord{
			ID: "queue_retry_restore_stop_gate_active", ProjectID: &projectID, LoopID: &otherLoopID, Type: "worker",
			TargetType: "project", TargetID: targetID, DedupeKey: dedupeKey,
			Priority: storage.QueuePriorityWorker, Status: "queued", AvailableAt: nowISO,
			Attempts: 0, MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO,
		}); err != nil {
			t.Errorf("inject active dedupe: %v", err)
		}
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/loops/3141/retry", strings.NewReader(`{"mode":"auto","resetAttempts":true}`))
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", recorder.Code, recorder.Body.String())
	}
	if !services.ActiveExecutions.LoopStopActive(loopID) {
		t.Fatal("LoopStopActive = false after failed retry, want sticky gate restored")
	}
	if _, err := services.ActiveExecutions.AdmitSpawn(context.Background(), agent.SpawnMeta{
		LoopID: loopID, RunID: "run_retry_restore", ExecutionID: "exec_retry_restore",
	}); !errors.Is(err, agent.ErrSpawnLoopStopping) {
		t.Fatalf("AdmitSpawn after failed retry error = %v, want ErrSpawnLoopStopping", err)
	}
}

// Retry must clear the sticky stop gate before publishing claimable queue work
// so a concurrent scheduler tick cannot claim then fail AdmitSpawn.

// Retry must clear the sticky stop gate before publishing claimable queue work
// so a concurrent scheduler tick cannot claim then fail AdmitSpawn.

// Retry must clear the sticky stop gate before publishing claimable queue work
// so a concurrent scheduler tick cannot claim then fail AdmitSpawn.
func TestHandlerLoopRetryClearsStopGateBeforeClaimable(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	h := NewHandler(Context{Config: cfg, Runtime: rt})
	services := rt.Services()
	nowISO := "2026-04-11T12:00:00.000Z"
	projectID := "project_retry_clear_stop_gate"
	loopID := "loop_retry_clear_stop_gate"
	targetID := projectID

	if err := services.Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID: projectID, Name: "Looper", RepoPath: t.TempDir(), CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := services.Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{
		ID: loopID, Seq: 3140, ProjectID: projectID, Type: "worker", TargetType: "project",
		TargetID: &targetID, Status: "paused", CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	lastErrorKind := "manual_intervention"
	if err := services.Repositories.Queue.Upsert(context.Background(), storage.QueueItemRecord{
		ID: "queue_retry_clear_stop_gate", ProjectID: &projectID, LoopID: &loopID, Type: "worker",
		TargetType: "project", TargetID: targetID, DedupeKey: "worker:clear_stop_gate",
		Priority: storage.QueuePriorityWorker, Status: "failed", AvailableAt: nowISO,
		Attempts: 1, MaxAttempts: 3, LastErrorKind: &lastErrorKind, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}
	if services.ActiveExecutions == nil {
		t.Fatal("ActiveExecutions is nil")
	}
	if _, err := services.ActiveExecutions.BeginLoopStop(loopID, "looper stop"); err != nil {
		t.Fatalf("BeginLoopStop: %v", err)
	}
	if !services.ActiveExecutions.LoopStopActive(loopID) {
		t.Fatal("LoopStopActive = false before retry, want sticky closed")
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/loops/3140/retry", strings.NewReader(`{"mode":"auto","resetAttempts":true}`))
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	if services.ActiveExecutions.LoopStopActive(loopID) {
		t.Fatal("LoopStopActive = true after retry, want ClearLoopStop before claimable queue publish")
	}
	if _, err := services.ActiveExecutions.AdmitSpawn(context.Background(), agent.SpawnMeta{
		LoopID: loopID, RunID: "run_retry_clear", ExecutionID: "exec_retry_clear",
	}); err != nil {
		t.Fatalf("AdmitSpawn after retry error = %v, want success", err)
	}
}

// TestHandlerLoopStartClearsStopGateBeforeClaimable ensures start/unpause clears
// the sticky stop gate before the requeue TX commits claimable work (same race
// as retry: concurrent tick must not see running+queued with gate still closed).
