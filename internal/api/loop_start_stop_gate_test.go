package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nexu-io/looper/internal/agent"
	"github.com/nexu-io/looper/internal/storage"
)

// TestHandlerLoopStartClearsStopGateBeforeClaimable ensures start/unpause clears
// the sticky stop gate before the requeue TX commits claimable work (same race
// as retry: concurrent tick must not see running+queued with gate still closed).
func TestHandlerLoopStartClearsStopGateBeforeClaimable(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	h := NewHandler(Context{Config: cfg, Runtime: rt})
	services := rt.Services()
	nowISO := "2026-04-11T12:00:00.000Z"
	projectID := "project_start_clear_stop_gate"
	loopID := "loop_start_clear_stop_gate"
	targetID := projectID
	metadata := `{"worker":{"title":"Start gate","prompt":"go","repo":"acme/looper","baseBranch":"main"}}`

	if err := services.Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID: projectID, Name: "Looper", RepoPath: t.TempDir(), CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := services.Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{
		ID: loopID, Seq: 3150, ProjectID: projectID, Type: "worker", TargetType: "project",
		TargetID: &targetID, Status: "paused", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	if services.ActiveExecutions == nil {
		t.Fatal("ActiveExecutions is nil")
	}
	if _, err := services.ActiveExecutions.BeginLoopStop(loopID, "looper stop"); err != nil {
		t.Fatalf("BeginLoopStop: %v", err)
	}
	if !services.ActiveExecutions.LoopStopActive(loopID) {
		t.Fatal("LoopStopActive = false before start, want sticky closed")
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/loops/"+loopID+"/start", nil)
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("start status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	if services.ActiveExecutions.LoopStopActive(loopID) {
		t.Fatal("LoopStopActive = true after start, want ClearLoopStop before claimable queue publish")
	}
	if _, err := services.ActiveExecutions.AdmitSpawn(context.Background(), agent.SpawnMeta{
		LoopID: loopID, RunID: "run_start_clear", ExecutionID: "exec_start_clear",
	}); err != nil {
		t.Fatalf("AdmitSpawn after start error = %v, want success", err)
	}
}

// TestHandlerLoopStartRestoresStopGateOnTXFailure ensures a failed start does
// not leave AdmitSpawn open after a sticky looper-stop gate was pre-cleared.

// TestHandlerLoopStartRestoresStopGateOnTXFailure ensures a failed start does
// not leave AdmitSpawn open after a sticky looper-stop gate was pre-cleared.

// TestHandlerLoopStartRestoresStopGateOnTXFailure ensures a failed start does
// not leave AdmitSpawn open after a sticky looper-stop gate was pre-cleared.
func TestHandlerLoopStartRestoresStopGateOnTXFailure(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	h := NewHandler(Context{Config: cfg, Runtime: rt})
	services := rt.Services()
	nowISO := "2026-04-11T12:00:00.000Z"
	projectID := "project_start_restore_stop_gate"
	loopID := "loop_start_restore_stop_gate"
	otherLoopID := "loop_start_restore_other"
	targetID := "pr:acme/looper:77"
	repo := "acme/looper"
	prNumber := int64(77)
	metadata := `{"worker":{"title":"Start restore","prompt":"go","repo":"acme/looper","baseBranch":"main"}}`

	if err := services.Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID: projectID, Name: "Looper", RepoPath: t.TempDir(), CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := services.Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{
		ID: loopID, Seq: 3151, ProjectID: projectID, Type: "worker", TargetType: "pull_request",
		TargetID: &targetID, Repo: &repo, PRNumber: &prNumber, Status: "paused",
		MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert() loop error = %v", err)
	}
	// Conflicting active loop for the same PR target so start TX fails unique check.
	if err := services.Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{
		ID: otherLoopID, Seq: 3152, ProjectID: projectID, Type: "worker", TargetType: "pull_request",
		TargetID: &targetID, Repo: &repo, PRNumber: &prNumber, Status: "running",
		MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert() other error = %v", err)
	}
	if services.ActiveExecutions == nil {
		t.Fatal("ActiveExecutions is nil")
	}
	if _, err := services.ActiveExecutions.BeginLoopStop(loopID, "looper stop"); err != nil {
		t.Fatalf("BeginLoopStop: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/loops/"+loopID+"/start", nil)
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code == http.StatusOK {
		t.Fatalf("start status = 200, want conflict/error; body=%s", recorder.Body.String())
	}
	if !services.ActiveExecutions.LoopStopActive(loopID) {
		t.Fatal("LoopStopActive = false after failed start, want sticky gate restored")
	}
	if _, err := services.ActiveExecutions.AdmitSpawn(context.Background(), agent.SpawnMeta{
		LoopID: loopID, RunID: "run_start_restore", ExecutionID: "exec_start_restore",
	}); !errors.Is(err, agent.ErrSpawnLoopStopping) {
		t.Fatalf("AdmitSpawn after failed start error = %v, want ErrSpawnLoopStopping", err)
	}
}

// TestHandlerWorkersCreateReuseClearsStickyStopGate ensures issue-worker reuse
// (paused → queued) reopens the sticky stop spawn gate closed by looper stop.
// Without this, recreate-same-issue claims the queue then AgentExecutor.Start
// fails with ErrSpawnLoopStopping forever.

// TestHandlerWorkersCreateReuseClearsStickyStopGate ensures issue-worker reuse
// (paused → queued) reopens the sticky stop spawn gate closed by looper stop.
// Without this, recreate-same-issue claims the queue then AgentExecutor.Start
// fails with ErrSpawnLoopStopping forever.
