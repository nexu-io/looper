package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nexu-io/looper/internal/storage"
)

// Compact recovery-surface contracts for the Dashboard manual-intervention card.
// Worktree dirty/clean/missing preflight matrix lives in loop_retry_discard_test.go;
// this file covers displayStatus projection and isolation only.
func TestHandlerLoopDetailManualInterventionRecoverySurface(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	h := NewHandler(Context{Config: cfg, Runtime: rt})
	services := rt.Services()
	nowISO := "2026-04-11T12:00:00.000Z"
	lastErrorKind := "manual_intervention"
	lastError := "dirty worker worktree: uncommitted local changes"
	projectID := "project_recovery_surface"
	loopID := "loop_recovery_surface"
	targetID := projectID

	if err := services.Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID: projectID, Name: "Recovery", RepoPath: "/tmp/repos/recovery", CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert error = %v", err)
	}
	if err := services.Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{
		ID: loopID, Seq: 6171, ProjectID: projectID, Type: "worker",
		TargetType: "project", TargetID: &targetID, Status: "paused",
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert error = %v", err)
	}
	if err := services.Repositories.Queue.Upsert(context.Background(), storage.QueueItemRecord{
		ID: "queue_recovery_surface", ProjectID: &projectID, LoopID: &loopID, Type: "worker",
		TargetType: "project", TargetID: targetID, DedupeKey: "worker:recovery_surface",
		Priority: storage.QueuePriorityWorker, Status: "manual_intervention", AvailableAt: nowISO,
		Attempts: 1, MaxAttempts: 3, LastError: &lastError, LastErrorKind: &lastErrorKind,
		FinishedAt: &nowISO, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Queue.Upsert error = %v", err)
	}

	preLoop, _ := services.Repositories.Loops.GetByID(context.Background(), loopID)
	preQueue, _ := services.Repositories.Queue.GetLatestByLoopID(context.Background(), loopID)

	detailReq := httptest.NewRequest(http.MethodGet, "/api/v1/loops/6171", nil)
	detailRec := httptest.NewRecorder()
	h.ServeHTTP(detailRec, detailReq)
	if detailRec.Code != http.StatusOK {
		t.Fatalf("detail status = %d, want 200; body=%s", detailRec.Code, detailRec.Body.String())
	}
	detail := parseJSONMap(t, detailRec.Body.Bytes())["data"].(map[string]any)
	assertEqual(t, detail["status"], "paused")
	assertEqual(t, detail["displayStatus"], "manual_intervention")
	assertEqual(t, detail["lastFailureKind"], "manual_intervention")
	assertEqual(t, detail["lastFailureReason"], lastError)

	postLoop, err := services.Repositories.Loops.GetByID(context.Background(), loopID)
	if err != nil || postLoop == nil || postLoop.Status != preLoop.Status || postLoop.UpdatedAt != preLoop.UpdatedAt {
		t.Fatalf("loop mutated by GET detail: before=%#v after=%#v err=%v", preLoop, postLoop, err)
	}
	postQueue, err := services.Repositories.Queue.GetLatestByLoopID(context.Background(), loopID)
	if err != nil || postQueue == nil || postQueue.Status != preQueue.Status || postQueue.ID != preQueue.ID {
		t.Fatalf("queue mutated by GET detail: before=%#v after=%#v err=%v", preQueue, postQueue, err)
	}

	// awaiting_human must not be re-projected as manual_intervention (decision card path).
	hitlProjectID := "project_recovery_hitl"
	hitlLoopID := "loop_recovery_hitl"
	hitlTarget := hitlProjectID
	if err := services.Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID: hitlProjectID, Name: "HITL", RepoPath: "/tmp/repos/hitl", CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert(hitl) error = %v", err)
	}
	meta := `{"hitl":{"question":"Ship it?","options":["yes","no"],"status":"awaiting","askedAt":"2026-04-11T12:00:00.000Z"}}`
	if err := services.Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{
		ID: hitlLoopID, Seq: 6174, ProjectID: hitlProjectID, Type: "fixer",
		TargetType: "project", TargetID: &hitlTarget, Status: "awaiting_human",
		MetadataJSON: &meta, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert(hitl) error = %v", err)
	}
	hitlReq := httptest.NewRequest(http.MethodGet, "/api/v1/loops/6174", nil)
	hitlRec := httptest.NewRecorder()
	h.ServeHTTP(hitlRec, hitlReq)
	if hitlRec.Code != http.StatusOK {
		t.Fatalf("hitl detail status = %d; body=%s", hitlRec.Code, hitlRec.Body.String())
	}
	hitlDetail := parseJSONMap(t, hitlRec.Body.Bytes())["data"].(map[string]any)
	assertEqual(t, hitlDetail["status"], "awaiting_human")
	assertEqual(t, hitlDetail["displayStatus"], "awaiting_human")
}

// After MI recovery, suspendForHuman sets loop=awaiting_human and CancelByLoop
// marks the queue cancelled without clearing last_error_kind. HITL must win so
// Loop Detail keeps displayStatus=awaiting_human (decision card), not MI recovery.
func TestHandlerLoopDetailAwaitingHumanSupersedesStaleManualErrorKind(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	h := NewHandler(Context{Config: cfg, Runtime: rt})
	services := rt.Services()
	nowISO := "2026-04-11T12:00:00.000Z"
	lastErrorKind := "manual_intervention"
	lastError := "dirty worker worktree: uncommitted local changes"
	projectID := "project_recovery_hitl_stale_kind"
	loopID := "loop_recovery_hitl_stale_kind"
	targetID := projectID
	checkpoint := `{"resumePolicy":"manual_intervention"}`
	runError := "checkpoint hold: operator must inspect worktree"
	meta := `{"hitl":{"question":"Ship it?","options":["yes","no"],"status":"awaiting","askedAt":"2026-04-11T12:00:00.000Z"}}`

	if err := services.Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID: projectID, Name: "HITL supersedes stale MI", RepoPath: "/tmp/repos/hitl-stale-mi",
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert error = %v", err)
	}
	if err := services.Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{
		ID: loopID, Seq: 6177, ProjectID: projectID, Type: "fixer",
		TargetType: "project", TargetID: &targetID, Status: "awaiting_human",
		MetadataJSON: &meta, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert error = %v", err)
	}
	if err := services.Repositories.Runs.Upsert(context.Background(), storage.RunRecord{
		ID: "run_recovery_hitl_stale_kind", LoopID: loopID, Status: "failed",
		CheckpointJSON: &checkpoint, ErrorMessage: &runError, StartedAt: nowISO,
		EndedAt: &nowISO, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Runs.Upsert error = %v", err)
	}
	// Mirrors CancelByLoop after suspendForHuman: cancelled queue retains
	// last_error_kind=manual_intervention from the prior recovery hold.
	if err := services.Repositories.Queue.Upsert(context.Background(), storage.QueueItemRecord{
		ID: "queue_recovery_hitl_stale_kind", ProjectID: &projectID, LoopID: &loopID, Type: "fixer",
		TargetType: "project", TargetID: targetID, DedupeKey: "fixer:recovery_hitl_stale_kind",
		Priority: storage.QueuePriorityWorker, Status: "cancelled", AvailableAt: nowISO,
		Attempts: 1, MaxAttempts: 3, LastError: &lastError, LastErrorKind: &lastErrorKind,
		FinishedAt: &nowISO, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Queue.Upsert error = %v", err)
	}

	detailReq := httptest.NewRequest(http.MethodGet, "/api/v1/loops/6177", nil)
	detailRec := httptest.NewRecorder()
	h.ServeHTTP(detailRec, detailReq)
	if detailRec.Code != http.StatusOK {
		t.Fatalf("detail status = %d, want 200; body=%s", detailRec.Code, detailRec.Body.String())
	}
	detail := parseJSONMap(t, detailRec.Body.Bytes())["data"].(map[string]any)
	assertEqual(t, detail["status"], "awaiting_human")
	assertEqual(t, detail["displayStatus"], "awaiting_human")
	// Historical MI diagnostics may still be present for operators, but must not
	// override HITL displayStatus / recovery-card gating.
	assertEqual(t, detail["lastFailureKind"], "manual_intervention")
	assertEqual(t, detail["resumePolicy"], "manual_intervention")

	listReq := httptest.NewRequest(http.MethodGet, "/api/v1/loops?limit=100", nil)
	listRec := httptest.NewRecorder()
	h.ServeHTTP(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200; body=%s", listRec.Code, listRec.Body.String())
	}
	items := parseJSONMap(t, listRec.Body.Bytes())["data"].(map[string]any)["items"].([]any)
	found := false
	for _, raw := range items {
		item := raw.(map[string]any)
		if item["id"] != loopID {
			continue
		}
		found = true
		assertEqual(t, item["status"], "awaiting_human")
		assertEqual(t, item["displayStatus"], "awaiting_human")
	}
	if !found {
		t.Fatalf("list items missing loop %s: %#v", loopID, items)
	}
}

// Startup recovery requeues running→queued while intentionally retaining
// last_error_kind=manual_intervention. Active queue status must supersede that
// stale kind so Loop Detail does not show an action-required recovery card.
func TestHandlerLoopDetailActiveQueueSupersedesStaleManualErrorKind(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	h := NewHandler(Context{Config: cfg, Runtime: rt})
	services := rt.Services()
	nowISO := "2026-04-11T12:00:00.000Z"
	lastErrorKind := "manual_intervention"
	lastError := "dirty worker worktree: uncommitted local changes"
	projectID := "project_recovery_stale_kind"
	loopID := "loop_recovery_stale_kind"
	targetID := projectID
	checkpoint := `{"resumePolicy":"manual_intervention"}`
	runError := "checkpoint hold: operator must inspect worktree"

	if err := services.Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID: projectID, Name: "Stale kind supersede", RepoPath: "/tmp/repos/stale-kind",
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert error = %v", err)
	}
	if err := services.Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{
		ID: loopID, Seq: 6176, ProjectID: projectID, Type: "worker",
		TargetType: "project", TargetID: &targetID, Status: "queued",
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert error = %v", err)
	}
	if err := services.Repositories.Runs.Upsert(context.Background(), storage.RunRecord{
		ID: "run_recovery_stale_kind", LoopID: loopID, Status: "failed",
		CheckpointJSON: &checkpoint, ErrorMessage: &runError, StartedAt: nowISO,
		EndedAt: &nowISO, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Runs.Upsert error = %v", err)
	}
	// Mirrors RequeueRunningByLoop after startup recovery: status=queued but
	// last_error_kind still manual_intervention.
	if err := services.Repositories.Queue.Upsert(context.Background(), storage.QueueItemRecord{
		ID: "queue_recovery_stale_kind", ProjectID: &projectID, LoopID: &loopID, Type: "worker",
		TargetType: "project", TargetID: targetID, DedupeKey: "worker:recovery_stale_kind",
		Priority: storage.QueuePriorityWorker, Status: "queued", AvailableAt: nowISO,
		Attempts: 1, MaxAttempts: 3, LastError: &lastError, LastErrorKind: &lastErrorKind,
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Queue.Upsert error = %v", err)
	}

	detailReq := httptest.NewRequest(http.MethodGet, "/api/v1/loops/6176", nil)
	detailRec := httptest.NewRecorder()
	h.ServeHTTP(detailRec, detailReq)
	if detailRec.Code != http.StatusOK {
		t.Fatalf("detail status = %d, want 200; body=%s", detailRec.Code, detailRec.Body.String())
	}
	detail := parseJSONMap(t, detailRec.Body.Bytes())["data"].(map[string]any)
	assertEqual(t, detail["status"], "queued")
	assertEqual(t, detail["displayStatus"], "queued")
	// Historical failure fields may still be present for diagnostics, but must
	// not force the recovery card via displayStatus.
	assertEqual(t, detail["lastFailureKind"], "manual_intervention")
	assertEqual(t, detail["resumePolicy"], "manual_intervention")

	// Claimed/running with the same retained kind must also stay active.
	if err := services.Repositories.Queue.Upsert(context.Background(), storage.QueueItemRecord{
		ID: "queue_recovery_stale_kind", ProjectID: &projectID, LoopID: &loopID, Type: "worker",
		TargetType: "project", TargetID: targetID, DedupeKey: "worker:recovery_stale_kind",
		Priority: storage.QueuePriorityWorker, Status: "running", AvailableAt: nowISO,
		Attempts: 1, MaxAttempts: 3, LastError: &lastError, LastErrorKind: &lastErrorKind,
		ClaimedBy: stringPtr("scheduler"), ClaimedAt: &nowISO, StartedAt: &nowISO,
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Queue.Upsert(running) error = %v", err)
	}
	if err := services.Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{
		ID: loopID, Seq: 6176, ProjectID: projectID, Type: "worker",
		TargetType: "project", TargetID: &targetID, Status: "running",
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert(running) error = %v", err)
	}
	runDetailReq := httptest.NewRequest(http.MethodGet, "/api/v1/loops/6176", nil)
	runDetailRec := httptest.NewRecorder()
	h.ServeHTTP(runDetailRec, runDetailReq)
	if runDetailRec.Code != http.StatusOK {
		t.Fatalf("running detail status = %d, want 200; body=%s", runDetailRec.Code, runDetailRec.Body.String())
	}
	runDetail := parseJSONMap(t, runDetailRec.Body.Bytes())["data"].(map[string]any)
	assertEqual(t, runDetail["status"], "running")
	assertEqual(t, runDetail["displayStatus"], "running")

	activeReq := httptest.NewRequest(http.MethodGet, "/api/v1/runs/active", nil)
	activeRec := httptest.NewRecorder()
	h.ServeHTTP(activeRec, activeReq)
	if activeRec.Code != http.StatusOK {
		t.Fatalf("active status = %d, want 200; body=%s", activeRec.Code, activeRec.Body.String())
	}
	items := parseJSONMap(t, activeRec.Body.Bytes())["data"].(map[string]any)["items"].([]any)
	found := false
	for _, raw := range items {
		item := raw.(map[string]any)
		if item["loopId"] != loopID {
			continue
		}
		found = true
		assertEqual(t, item["displayStatus"], "running")
	}
	if !found {
		t.Fatalf("active items missing loop %s: %#v", loopID, items)
	}
}

// After retryLoop, a fresh queued item must supersede a stale run resumePolicy so
// the dashboard does not keep showing the recovery card for an already-queued loop.
func TestHandlerLoopDetailQueuedRetrySupersedesStaleResumePolicy(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	h := NewHandler(Context{Config: cfg, Runtime: rt})
	services := rt.Services()
	nowISO := "2026-04-11T12:00:00.000Z"
	projectID := "project_recovery_retry_supersede"
	loopID := "loop_recovery_retry_supersede"
	targetID := projectID
	checkpoint := `{"resumePolicy":"manual_intervention"}`
	runError := "checkpoint hold: operator must inspect worktree"

	if err := services.Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID: projectID, Name: "Retry supersede", RepoPath: "/tmp/repos/retry-supersede",
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert error = %v", err)
	}
	if err := services.Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{
		ID: loopID, Seq: 6175, ProjectID: projectID, Type: "worker",
		TargetType: "project", TargetID: &targetID, Status: "queued",
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert error = %v", err)
	}
	if err := services.Repositories.Runs.Upsert(context.Background(), storage.RunRecord{
		ID: "run_recovery_retry_supersede", LoopID: loopID, Status: "failed",
		CheckpointJSON: &checkpoint, ErrorMessage: &runError, StartedAt: nowISO,
		EndedAt: &nowISO, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Runs.Upsert error = %v", err)
	}
	// Fresh post-retry queue item: no error fields (supersedes the hold).
	if err := services.Repositories.Queue.Upsert(context.Background(), storage.QueueItemRecord{
		ID: "queue_recovery_retry_supersede", ProjectID: &projectID, LoopID: &loopID, Type: "worker",
		TargetType: "project", TargetID: targetID, DedupeKey: "worker:recovery_retry_supersede",
		Priority: storage.QueuePriorityWorker, Status: "queued", AvailableAt: nowISO,
		Attempts: 0, MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Queue.Upsert error = %v", err)
	}

	detailReq := httptest.NewRequest(http.MethodGet, "/api/v1/loops/6175", nil)
	detailRec := httptest.NewRecorder()
	h.ServeHTTP(detailRec, detailReq)
	if detailRec.Code != http.StatusOK {
		t.Fatalf("detail status = %d, want 200; body=%s", detailRec.Code, detailRec.Body.String())
	}
	detail := parseJSONMap(t, detailRec.Body.Bytes())["data"].(map[string]any)
	assertEqual(t, detail["status"], "queued")
	assertEqual(t, detail["displayStatus"], "queued")
	// resumePolicy may still be reported from the latest completed run, but must
	// not force displayStatus back to manual_intervention after re-queue.
	assertEqual(t, detail["resumePolicy"], "manual_intervention")

	activeReq := httptest.NewRequest(http.MethodGet, "/api/v1/runs/active", nil)
	activeRec := httptest.NewRecorder()
	h.ServeHTTP(activeRec, activeReq)
	if activeRec.Code != http.StatusOK {
		t.Fatalf("active status = %d, want 200; body=%s", activeRec.Code, activeRec.Body.String())
	}
	items := parseJSONMap(t, activeRec.Body.Bytes())["data"].(map[string]any)["items"].([]any)
	found := false
	for _, raw := range items {
		item := raw.(map[string]any)
		if item["loopId"] != loopID {
			continue
		}
		found = true
		assertEqual(t, item["displayStatus"], "queued")
		assertEqual(t, item["resumePolicy"], "manual_intervention")
	}
	if !found {
		t.Fatalf("active items missing loop %s: %#v", loopID, items)
	}
}
