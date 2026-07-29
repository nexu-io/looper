package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nexu-io/looper/internal/storage"
)

const recoverySurfaceNow = "2026-04-11T12:00:00.000Z"

// seedRecoverySurfaceLoop inserts a project+loop and optional run/queue rows.
// seq is the public loop number used by GET /api/v1/loops/{seq}.
func seedRecoverySurfaceLoop(t *testing.T, repos *storage.Repositories, seq int64, loop storage.LoopRecord, run *storage.RunRecord, queue *storage.QueueItemRecord) {
	t.Helper()
	now := recoverySurfaceNow
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID: loop.ProjectID, Name: loop.ProjectID, RepoPath: "/tmp/repos/" + loop.ProjectID,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("Projects.Upsert error = %v", err)
	}
	loop.Seq = seq
	if loop.CreatedAt == "" {
		loop.CreatedAt = now
	}
	if loop.UpdatedAt == "" {
		loop.UpdatedAt = now
	}
	if err := repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert error = %v", err)
	}
	if run != nil {
		if run.StartedAt == "" {
			run.StartedAt = now
		}
		if run.CreatedAt == "" {
			run.CreatedAt = now
		}
		if run.UpdatedAt == "" {
			run.UpdatedAt = now
		}
		if run.EndedAt == nil {
			run.EndedAt = &now
		}
		if err := repos.Runs.Upsert(context.Background(), *run); err != nil {
			t.Fatalf("Runs.Upsert error = %v", err)
		}
	}
	if queue != nil {
		if queue.AvailableAt == "" {
			queue.AvailableAt = now
		}
		if queue.CreatedAt == "" {
			queue.CreatedAt = now
		}
		if queue.UpdatedAt == "" {
			queue.UpdatedAt = now
		}
		if err := repos.Queue.Upsert(context.Background(), *queue); err != nil {
			t.Fatalf("Queue.Upsert error = %v", err)
		}
	}
}

func getLoopDetail(t *testing.T, h http.Handler, seq int64) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/loops/%d", seq), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("detail status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	return parseJSONMap(t, rec.Body.Bytes())["data"].(map[string]any)
}

// Compact recovery-surface contracts for Dashboard manual-intervention gating.
// Worktree dirty/clean/missing preflight matrix lives in loop_retry_discard_test.go.
func TestHandlerLoopDetailManualInterventionRecoverySurface(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	h := NewHandler(Context{Config: cfg, Runtime: rt})
	services := rt.Services()
	now := recoverySurfaceNow
	kind, reason := "manual_intervention", "dirty worker worktree: uncommitted local changes"
	projectID, loopID, target := "project_recovery_surface", "loop_recovery_surface", "project_recovery_surface"

	repos := services.Repositories
	seedRecoverySurfaceLoop(t, repos, 6171, storage.LoopRecord{
		ID: loopID, ProjectID: projectID, Type: "worker", TargetType: "project", TargetID: &target, Status: "paused",
	}, nil, &storage.QueueItemRecord{
		ID: "queue_recovery_surface", ProjectID: &projectID, LoopID: &loopID, Type: "worker",
		TargetType: "project", TargetID: target, DedupeKey: "worker:recovery_surface",
		Priority: storage.QueuePriorityWorker, Status: "manual_intervention",
		Attempts: 1, MaxAttempts: 3, LastError: &reason, LastErrorKind: &kind, FinishedAt: &now,
	})

	preLoop, _ := repos.Loops.GetByID(context.Background(), loopID)
	preQueue, _ := repos.Queue.GetLatestByLoopID(context.Background(), loopID)
	detail := getLoopDetail(t, h, 6171)
	assertEqual(t, detail["status"], "paused")
	assertEqual(t, detail["displayStatus"], "manual_intervention")
	assertEqual(t, detail["lastFailureKind"], "manual_intervention")
	assertEqual(t, detail["lastFailureReason"], reason)

	postLoop, err := repos.Loops.GetByID(context.Background(), loopID)
	if err != nil || postLoop == nil || postLoop.Status != preLoop.Status || postLoop.UpdatedAt != preLoop.UpdatedAt {
		t.Fatalf("loop mutated by GET detail: before=%#v after=%#v err=%v", preLoop, postLoop, err)
	}
	postQueue, err := repos.Queue.GetLatestByLoopID(context.Background(), loopID)
	if err != nil || postQueue == nil || postQueue.Status != preQueue.Status || postQueue.ID != preQueue.ID {
		t.Fatalf("queue mutated by GET detail: before=%#v after=%#v err=%v", preQueue, postQueue, err)
	}

	// awaiting_human must not be re-projected as manual_intervention (decision card path).
	hitlMeta := `{"hitl":{"question":"Ship it?","options":["yes","no"],"status":"awaiting","askedAt":"2026-04-11T12:00:00.000Z"}}`
	hitlProject, hitlLoop, hitlTarget := "project_recovery_hitl", "loop_recovery_hitl", "project_recovery_hitl"
	seedRecoverySurfaceLoop(t, repos, 6174, storage.LoopRecord{
		ID: hitlLoop, ProjectID: hitlProject, Type: "fixer", TargetType: "project", TargetID: &hitlTarget,
		Status: "awaiting_human", MetadataJSON: &hitlMeta,
	}, nil, nil)
	hitl := getLoopDetail(t, h, 6174)
	assertEqual(t, hitl["status"], "awaiting_human")
	assertEqual(t, hitl["displayStatus"], "awaiting_human")
}

// Table-driven supersession contracts for displayStatus projection.
func TestHandlerLoopDetailRecoveryDisplayStatusSupersession(t *testing.T) {
	kind, reason := "manual_intervention", "dirty worker worktree: uncommitted local changes"
	checkpoint := `{"resumePolicy":"manual_intervention"}`
	runError := "checkpoint hold: operator must inspect worktree"
	hitlMeta := `{"hitl":{"question":"Ship it?","options":["yes","no"],"status":"awaiting","askedAt":"2026-04-11T12:00:00.000Z"}}`
	now := recoverySurfaceNow

	cases := []struct {
		name            string
		seq             int64
		loopStatus      string
		meta            *string
		queueStatus     string // empty = no queue
		queueKind       *string
		wantStatus      string
		wantDisplay     string
		wantFailureKind any
	}{
		// CancelByLoop after suspendForHuman retains MI kind; HITL wins.
		{
			name: "awaiting_human supersedes stale MI kind",
			seq:  6177, loopStatus: "awaiting_human", meta: &hitlMeta,
			queueStatus: "cancelled", queueKind: &kind,
			wantStatus: "awaiting_human", wantDisplay: "awaiting_human", wantFailureKind: "manual_intervention",
		},
		// Safety-floor quarantine may retain MI queue while loop is human_takeover.
		{
			name: "human_takeover supersedes retained MI queue",
			seq:  6178, loopStatus: "human_takeover",
			queueStatus: "manual_intervention", queueKind: &kind,
			wantStatus: "human_takeover", wantDisplay: "human_takeover", wantFailureKind: "manual_intervention",
		},
		// Startup recovery requeues running→queued while retaining last_error_kind.
		{
			name: "active queued supersedes stale MI kind",
			seq:  6176, loopStatus: "queued",
			queueStatus: "queued", queueKind: &kind,
			wantStatus: "queued", wantDisplay: "queued", wantFailureKind: "manual_intervention",
		},
		// retryLoop leaves resumePolicy; Pause cancels the clean replacement queue.
		{
			name: "clean cancelled supersedes stale resumePolicy",
			seq:  6179, loopStatus: "paused",
			queueStatus: "cancelled", queueKind: nil,
			wantStatus: "paused", wantDisplay: "paused", wantFailureKind: nil,
		},
		// Fresh post-retry queued item supersedes stale resumePolicy.
		{
			name: "queued retry supersedes stale resumePolicy",
			seq:  6175, loopStatus: "queued",
			queueStatus: "queued", queueKind: nil,
			wantStatus: "queued", wantDisplay: "queued", wantFailureKind: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt, cfg := startTestRuntime(t)
			h := NewHandler(Context{Config: cfg, Runtime: rt})
			services := rt.Services()
			projectID := "project_" + tc.name
			loopID := "loop_" + tc.name
			target := projectID

			var queue *storage.QueueItemRecord
			if tc.queueStatus != "" {
				q := storage.QueueItemRecord{
					ID: "queue_" + tc.name, ProjectID: &projectID, LoopID: &loopID, Type: "worker",
					TargetType: "project", TargetID: target, DedupeKey: "worker:" + tc.name,
					Priority: storage.QueuePriorityWorker, Status: tc.queueStatus,
					Attempts: 1, MaxAttempts: 3, LastErrorKind: tc.queueKind,
				}
				if tc.queueKind != nil {
					q.LastError = &reason
					if tc.queueStatus != "queued" && tc.queueStatus != "running" {
						q.FinishedAt = &now
					}
				} else {
					q.Attempts = 0
				}
				queue = &q
			}
			seedRecoverySurfaceLoop(t, services.Repositories, tc.seq, storage.LoopRecord{
				ID: loopID, ProjectID: projectID, Type: "worker", TargetType: "project",
				TargetID: &target, Status: tc.loopStatus, MetadataJSON: tc.meta,
			}, &storage.RunRecord{
				ID: "run_" + tc.name, LoopID: loopID, Status: "failed",
				CheckpointJSON: &checkpoint, ErrorMessage: &runError,
			}, queue)

			detail := getLoopDetail(t, h, tc.seq)
			assertEqual(t, detail["status"], tc.wantStatus)
			assertEqual(t, detail["displayStatus"], tc.wantDisplay)
			assertEqual(t, detail["resumePolicy"], "manual_intervention")
			if tc.wantFailureKind == nil {
				if detail["lastFailureKind"] != nil {
					t.Fatalf("lastFailureKind = %#v, want nil", detail["lastFailureKind"])
				}
			} else {
				assertEqual(t, detail["lastFailureKind"], tc.wantFailureKind)
			}
		})
	}
}

// Claimed/running with retained MI kind stays active (not recovery card).
func TestHandlerLoopDetailActiveRunningSupersedesStaleManualErrorKind(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	h := NewHandler(Context{Config: cfg, Runtime: rt})
	services := rt.Services()
	now := recoverySurfaceNow
	kind, reason := "manual_intervention", "dirty worker worktree: uncommitted local changes"
	checkpoint := `{"resumePolicy":"manual_intervention"}`
	projectID, loopID, target := "project_recovery_running", "loop_recovery_running", "project_recovery_running"

	seedRecoverySurfaceLoop(t, services.Repositories, 6180, storage.LoopRecord{
		ID: loopID, ProjectID: projectID, Type: "worker", TargetType: "project", TargetID: &target, Status: "running",
	}, &storage.RunRecord{
		ID: "run_recovery_running", LoopID: loopID, Status: "failed", CheckpointJSON: &checkpoint, ErrorMessage: &reason,
	}, &storage.QueueItemRecord{
		ID: "queue_recovery_running", ProjectID: &projectID, LoopID: &loopID, Type: "worker",
		TargetType: "project", TargetID: target, DedupeKey: "worker:recovery_running",
		Priority: storage.QueuePriorityWorker, Status: "running",
		Attempts: 1, MaxAttempts: 3, LastError: &reason, LastErrorKind: &kind,
		ClaimedBy: stringPtr("scheduler"), ClaimedAt: &now, StartedAt: &now,
	})

	detail := getLoopDetail(t, h, 6180)
	assertEqual(t, detail["status"], "running")
	assertEqual(t, detail["displayStatus"], "running")

	activeReq := httptest.NewRequest(http.MethodGet, "/api/v1/runs/active", nil)
	activeRec := httptest.NewRecorder()
	h.ServeHTTP(activeRec, activeReq)
	if activeRec.Code != http.StatusOK {
		t.Fatalf("active status = %d; body=%s", activeRec.Code, activeRec.Body.String())
	}
	for _, raw := range parseJSONMap(t, activeRec.Body.Bytes())["data"].(map[string]any)["items"].([]any) {
		item := raw.(map[string]any)
		if item["loopId"] == loopID {
			assertEqual(t, item["displayStatus"], "running")
			return
		}
	}
	t.Fatalf("active items missing loop %s", loopID)
}
