package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	gitinfra "github.com/nexu-io/looper/internal/infra/git"
	"github.com/nexu-io/looper/internal/storage"
)

func TestHandlerLoopRetryDiscardWorktreeChangesDirtyFixer(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	h := NewHandler(Context{Config: cfg, Runtime: rt})
	services := rt.Services()
	nowISO := "2026-04-11T12:00:00.000Z"

	fixture := seedManagedWorktreeFixture(t, services.Repositories, managedWorktreeSeed{
		ProjectID: "project_retry_discard_fixer",
		LoopID:    "loop_retry_discard_fixer",
		LoopSeq:   3108,
		LoopType:  "fixer",
		Branch:    "feature/discard-fixer",
		NowISO:    nowISO,
		Dirty:     true,
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/loops/3108/retry", strings.NewReader(`{"mode":"auto","resetAttempts":true,"discardWorktreeChanges":true}`))
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}

	body := parseJSONMap(t, recorder.Body.Bytes())
	data := body["data"].(map[string]any)
	assertEqual(t, data["discardWorktreeChanges"], true)
	discard := data["worktreeDiscard"].(map[string]any)
	assertEqual(t, discard["discarded"], true)
	assertEqual(t, discard["noOp"], false)
	assertEqual(t, discard["worktreePath"], fixture.WorktreePath)
	assertEqual(t, discard["reason"], "discarded")

	loop, err := services.Repositories.Loops.GetByID(context.Background(), fixture.LoopID)
	if err != nil || loop == nil || loop.Status != "queued" {
		t.Fatalf("loop after retry = %#v, %v, want queued", loop, err)
	}
	items, err := services.Repositories.Queue.List(context.Background())
	if err != nil {
		t.Fatalf("Queue.List() error = %v", err)
	}
	var replacement *storage.QueueItemRecord
	for i := range items {
		if items[i].LoopID != nil && *items[i].LoopID == fixture.LoopID && items[i].Status == "queued" {
			replacement = &items[i]
		}
	}
	if replacement == nil || replacement.ID == fixture.FailedQueueID || replacement.Attempts != 0 {
		t.Fatalf("replacement queue = %#v, want new queued item", replacement)
	}

	if _, err := os.Stat(filepath.Join(fixture.WorktreePath, "dirty.txt")); !os.IsNotExist(err) {
		t.Fatalf("dirty.txt still present after discard: %v", err)
	}
	if got := readTestFile(t, filepath.Join(fixture.WorktreePath, "README.md")); got != "hello\n" {
		t.Fatalf("README.md after discard = %q, want restored contents", got)
	}
	clean, err := gitinfra.New(gitinfra.Options{GitPath: "git"}).WorktreeClean(context.Background(), fixture.WorktreePath)
	if err != nil || !clean {
		t.Fatalf("worktree clean after discard = %v, %v", clean, err)
	}

	events, err := services.Repositories.Events.List(context.Background(), 50)
	if err != nil {
		t.Fatalf("Events.List() error = %v", err)
	}
	found := false
	for _, event := range events {
		if event.EventType == "looper.worktree.changes_discarded" {
			found = true
			if event.LoopID == nil || *event.LoopID != fixture.LoopID {
				t.Fatalf("discard event loop id = %#v, want %s", event.LoopID, fixture.LoopID)
			}
			break
		}
	}
	if !found {
		t.Fatal("expected looper.worktree.changes_discarded event")
	}
}

func TestHandlerLoopRetryDiscardWorktreeChangesResolvesBranchOnlyCheckpoint(t *testing.T) {
	// Worker dirty prepare leaves work.branch without checkpoint.worktree.
	rt, cfg := startTestRuntime(t)
	h := NewHandler(Context{Config: cfg, Runtime: rt})
	services := rt.Services()
	nowISO := "2026-04-11T12:00:00.000Z"

	fixture := seedManagedWorktreeFixture(t, services.Repositories, managedWorktreeSeed{
		ProjectID: "project_retry_discard_branch_only",
		LoopID:    "loop_retry_discard_branch_only",
		LoopSeq:   3119,
		LoopType:  "worker",
		Branch:    "feature/discard-branch-only",
		NowISO:    nowISO,
		Dirty:     true,
	})

	// Overwrite the run checkpoint to mimic prepare-worktree dirty failure:
	// work.branch present, worktree absent.
	branchOnly := fmt.Sprintf(`{"work":{"branch":%q,"executionMode":"push-existing"}}`, "feature/discard-branch-only")
	if err := services.Repositories.Runs.Upsert(context.Background(), storage.RunRecord{
		ID: "run_" + fixture.LoopID, LoopID: fixture.LoopID, Status: "failed", CheckpointJSON: &branchOnly,
		StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Runs.Upsert(branch-only) error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/loops/3119/retry", strings.NewReader(`{"mode":"auto","discardWorktreeChanges":true}`))
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	body := parseJSONMap(t, recorder.Body.Bytes())
	data := body["data"].(map[string]any)
	discard := data["worktreeDiscard"].(map[string]any)
	assertEqual(t, discard["discarded"], true)
	assertEqual(t, discard["reason"], "discarded")
	assertEqual(t, discard["worktreePath"], fixture.WorktreePath)
	if _, err := os.Stat(filepath.Join(fixture.WorktreePath, "dirty.txt")); !os.IsNotExist(err) {
		t.Fatalf("dirty.txt still present after branch-only discard: %v", err)
	}
}

func TestHandlerLoopRetryDiscardWorktreeChangesResolvesDetailHeadRef(t *testing.T) {
	// Fixer dirty prepare leaves detail.headRefName without checkpoint.worktree.
	rt, cfg := startTestRuntime(t)
	h := NewHandler(Context{Config: cfg, Runtime: rt})
	services := rt.Services()
	nowISO := "2026-04-11T12:00:00.000Z"

	fixture := seedManagedWorktreeFixture(t, services.Repositories, managedWorktreeSeed{
		ProjectID: "project_retry_discard_detail_head",
		LoopID:    "loop_retry_discard_detail_head",
		LoopSeq:   3120,
		LoopType:  "fixer",
		Branch:    "feature/discard-detail-head",
		NowISO:    nowISO,
		Dirty:     true,
	})

	detailOnly := fmt.Sprintf(`{"detail":{"headRefName":%q,"state":"OPEN"},"pause":{"reason":"dirty_worktree"}}`, "feature/discard-detail-head")
	if err := services.Repositories.Runs.Upsert(context.Background(), storage.RunRecord{
		ID: "run_" + fixture.LoopID, LoopID: fixture.LoopID, Status: "failed", CheckpointJSON: &detailOnly,
		StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Runs.Upsert(detail-only) error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/loops/3120/retry", strings.NewReader(`{"mode":"auto","discardWorktreeChanges":true}`))
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	body := parseJSONMap(t, recorder.Body.Bytes())
	data := body["data"].(map[string]any)
	discard := data["worktreeDiscard"].(map[string]any)
	assertEqual(t, discard["discarded"], true)
	assertEqual(t, discard["worktreePath"], fixture.WorktreePath)
}

func TestParseCheckpointWorktreeFallsBackToWorkBranchAndDetail(t *testing.T) {
	t.Parallel()
	workOnly := `{"work":{"branch":"feature/from-work"}}`
	ref := parseCheckpointWorktree(&workOnly)
	if ref == nil || ref.Branch != "feature/from-work" || ref.Path != "" {
		t.Fatalf("work-only = %#v, want branch feature/from-work", ref)
	}
	detailOnly := `{"detail":{"headRefName":"feature/from-detail"}}`
	ref = parseCheckpointWorktree(&detailOnly)
	if ref == nil || ref.Branch != "feature/from-detail" {
		t.Fatalf("detail-only = %#v, want branch feature/from-detail", ref)
	}
	worktreeWins := `{"worktree":{"branch":"feature/worktree","path":"/tmp/wt"},"work":{"branch":"feature/work"}}`
	ref = parseCheckpointWorktree(&worktreeWins)
	if ref == nil || ref.Branch != "feature/worktree" || ref.Path != "/tmp/wt" {
		t.Fatalf("worktree preference = %#v", ref)
	}
	// push-existing with empty work.branch derives pr-<PRNumber>, matching
	// worker runPrepareWorktreeStep before checkpoint.worktree is saved.
	pushExisting := `{"work":{"executionMode":"push-existing","prNumber":42}}`
	ref = parseCheckpointWorktree(&pushExisting)
	if ref == nil || ref.Branch != "pr-42" {
		t.Fatalf("push-existing empty branch = %#v, want branch pr-42", ref)
	}
	// Explicit work.branch wins over pr-N derivation.
	pushExistingNamed := `{"work":{"branch":"feature/named","executionMode":"push-existing","prNumber":42}}`
	ref = parseCheckpointWorktree(&pushExistingNamed)
	if ref == nil || ref.Branch != "feature/named" {
		t.Fatalf("push-existing named branch = %#v, want feature/named", ref)
	}
	empty := `{"work":{},"detail":{}}`
	if got := parseCheckpointWorktree(&empty); got != nil {
		t.Fatalf("empty hints = %#v, want nil", got)
	}
}

func TestHandlerLoopRetryDiscardWorktreeChangesResolvesPushExistingPRBranch(t *testing.T) {
	// push-existing dirty prepare creates worktree under pr-<N> but leaves
	// work.branch empty and omits checkpoint.worktree.
	rt, cfg := startTestRuntime(t)
	h := NewHandler(Context{Config: cfg, Runtime: rt})
	services := rt.Services()
	nowISO := "2026-04-11T12:00:00.000Z"

	fixture := seedManagedWorktreeFixture(t, services.Repositories, managedWorktreeSeed{
		ProjectID: "project_retry_discard_push_existing",
		LoopID:    "loop_retry_discard_push_existing",
		LoopSeq:   3121,
		LoopType:  "worker",
		Branch:    "pr-77",
		NowISO:    nowISO,
		Dirty:     true,
	})

	pushExistingOnly := `{"work":{"executionMode":"push-existing","prNumber":77}}`
	if err := services.Repositories.Runs.Upsert(context.Background(), storage.RunRecord{
		ID: "run_" + fixture.LoopID, LoopID: fixture.LoopID, Status: "failed", CheckpointJSON: &pushExistingOnly,
		StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Runs.Upsert(push-existing) error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/loops/3121/retry", strings.NewReader(`{"mode":"auto","discardWorktreeChanges":true}`))
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	body := parseJSONMap(t, recorder.Body.Bytes())
	data := body["data"].(map[string]any)
	discard := data["worktreeDiscard"].(map[string]any)
	assertEqual(t, discard["discarded"], true)
	assertEqual(t, discard["reason"], "discarded")
	assertEqual(t, discard["worktreePath"], fixture.WorktreePath)
	if _, err := os.Stat(filepath.Join(fixture.WorktreePath, "dirty.txt")); !os.IsNotExist(err) {
		t.Fatalf("dirty.txt still present after push-existing discard: %v", err)
	}
}

func TestHandlerLoopRetryDiscardWorktreeChangesAlreadyClean(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	h := NewHandler(Context{Config: cfg, Runtime: rt})
	services := rt.Services()
	nowISO := "2026-04-11T12:00:00.000Z"

	fixture := seedManagedWorktreeFixture(t, services.Repositories, managedWorktreeSeed{
		ProjectID: "project_retry_discard_clean",
		LoopID:    "loop_retry_discard_clean",
		LoopSeq:   3109,
		LoopType:  "worker",
		Branch:    "feature/discard-clean",
		NowISO:    nowISO,
		Dirty:     false,
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/loops/3109/retry", strings.NewReader(`{"mode":"auto","discardWorktreeChanges":true}`))
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	data := parseJSONMap(t, recorder.Body.Bytes())["data"].(map[string]any)
	discard := data["worktreeDiscard"].(map[string]any)
	assertEqual(t, discard["discarded"], false)
	assertEqual(t, discard["noOp"], true)
	assertEqual(t, discard["reason"], "already_clean")
	assertEqual(t, discard["worktreePath"], fixture.WorktreePath)

	loop, err := services.Repositories.Loops.GetByID(context.Background(), fixture.LoopID)
	if err != nil || loop == nil || loop.Status != "queued" {
		t.Fatalf("loop after retry = %#v, %v, want queued", loop, err)
	}
}

func TestHandlerLoopRetryDiscardWorktreeChangesPlannerNoOp(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	h := NewHandler(Context{Config: cfg, Runtime: rt})
	services := rt.Services()
	nowISO := "2026-04-11T12:00:00.000Z"
	projectID := "project_retry_discard_planner"
	loopID := "loop_retry_discard_planner"
	targetID := projectID

	if err := services.Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Planner", RepoPath: t.TempDir(), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := services.Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 3110, ProjectID: projectID, Type: "planner", TargetType: "project", TargetID: &targetID, Status: "paused", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	lastErrorKind := "manual_intervention"
	if err := services.Repositories.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: "queue_retry_discard_planner", ProjectID: &projectID, LoopID: &loopID, Type: "planner", TargetType: "project", TargetID: targetID, DedupeKey: "planner:retry_discard", Priority: storage.QueuePriorityPlanner, Status: "failed", AvailableAt: nowISO, Attempts: 1, MaxAttempts: 3, LastErrorKind: &lastErrorKind, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/loops/3110/retry", strings.NewReader(`{"mode":"auto","discardWorktreeChanges":true}`))
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	data := parseJSONMap(t, recorder.Body.Bytes())["data"].(map[string]any)
	discard := data["worktreeDiscard"].(map[string]any)
	assertEqual(t, discard["noOp"], true)
	assertEqual(t, discard["reason"], "planner_no_worktree")
	assertEqual(t, discard["discarded"], false)

	loop, err := services.Repositories.Loops.GetByID(context.Background(), loopID)
	if err != nil || loop == nil || loop.Status != "queued" {
		t.Fatalf("loop after retry = %#v, %v, want queued", loop, err)
	}
}

func TestHandlerLoopRetryDiscardWorktreeChangesRejectsActiveRun(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	h := NewHandler(Context{Config: cfg, Runtime: rt})
	services := rt.Services()
	nowISO := "2026-04-11T12:00:00.000Z"
	projectID := "project_retry_discard_active_run"
	loopID := "loop_retry_discard_active_run"
	targetID := projectID

	if err := services.Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Looper", RepoPath: t.TempDir(), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := services.Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 3111, ProjectID: projectID, Type: "fixer", TargetType: "project", TargetID: &targetID, Status: "running", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	if err := services.Repositories.Runs.Upsert(context.Background(), storage.RunRecord{ID: "run_active_discard", LoopID: loopID, Status: "running", StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/loops/3111/retry", strings.NewReader(`{"mode":"auto","discardWorktreeChanges":true}`))
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "while a run is active") {
		t.Fatalf("body = %s, want active run rejection", recorder.Body.String())
	}
}

func TestHandlerLoopRetryDiscardWorktreeChangesRejectsActiveQueue(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	h := NewHandler(Context{Config: cfg, Runtime: rt})
	services := rt.Services()
	nowISO := "2026-04-11T12:00:00.000Z"
	projectID := "project_retry_discard_active_queue"
	loopID := "loop_retry_discard_active_queue"
	targetID := projectID

	if err := services.Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Looper", RepoPath: t.TempDir(), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := services.Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 3112, ProjectID: projectID, Type: "worker", TargetType: "project", TargetID: &targetID, Status: "queued", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	if err := services.Repositories.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: "queue_active_discard", ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: targetID, DedupeKey: "worker:active_discard", Priority: storage.QueuePriorityWorker, Status: "queued", AvailableAt: nowISO, Attempts: 0, MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/loops/3112/retry", strings.NewReader(`{"mode":"auto","discardWorktreeChanges":true}`))
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "while queue item") {
		t.Fatalf("body = %s, want active queue rejection", recorder.Body.String())
	}
}

func TestHandlerLoopRetryDiscardWorktreeChangesRejectsNonManagedPath(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	h := NewHandler(Context{Config: cfg, Runtime: rt})
	services := rt.Services()
	nowISO := "2026-04-11T12:00:00.000Z"
	projectID := "project_retry_discard_nonmanaged"
	loopID := "loop_retry_discard_nonmanaged"
	targetID := projectID
	repoPath := t.TempDir()
	outsidePath := filepath.Join(t.TempDir(), "outside-worktree")
	if err := os.MkdirAll(outsidePath, 0o755); err != nil {
		t.Fatalf("MkdirAll(outside) error = %v", err)
	}
	worktreeRoot := filepath.Join(t.TempDir(), "managed-worktrees")
	metadata, _ := json.Marshal(map[string]any{"worktreeRoot": worktreeRoot})
	metadataJSON := string(metadata)

	if err := services.Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Looper", RepoPath: repoPath, MetadataJSON: &metadataJSON, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := services.Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 3113, ProjectID: projectID, Type: "reviewer", TargetType: "project", TargetID: &targetID, Status: "paused", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	checkpoint := fmt.Sprintf(`{"worktree":{"path":%q,"branch":"feature/outside"}}`, outsidePath)
	if err := services.Repositories.Runs.Upsert(context.Background(), storage.RunRecord{ID: "run_nonmanaged", LoopID: loopID, Status: "failed", CheckpointJSON: &checkpoint, StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}
	lastErrorKind := "manual_intervention"
	if err := services.Repositories.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: "queue_nonmanaged", ProjectID: &projectID, LoopID: &loopID, Type: "reviewer", TargetType: "project", TargetID: targetID, DedupeKey: "reviewer:nonmanaged", Priority: storage.QueuePriorityReviewer, Status: "manual_intervention", AvailableAt: nowISO, Attempts: 1, MaxAttempts: 3, LastErrorKind: &lastErrorKind, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/loops/3113/retry", strings.NewReader(`{"mode":"auto","discardWorktreeChanges":true}`))
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "not a Looper-managed worktree") && !strings.Contains(recorder.Body.String(), "unsafe worktree path") {
		t.Fatalf("body = %s, want non-managed path rejection", recorder.Body.String())
	}

	loop, err := services.Repositories.Loops.GetByID(context.Background(), loopID)
	if err != nil || loop == nil || loop.Status != "paused" {
		t.Fatalf("loop after failed discard = %#v, %v, want paused", loop, err)
	}
}

func TestHandlerLoopRetryDiscardWorktreeChangesRejectsPrimaryRepoPath(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	h := NewHandler(Context{Config: cfg, Runtime: rt})
	services := rt.Services()
	nowISO := "2026-04-11T12:00:00.000Z"
	projectID := "project_retry_discard_repo_path"
	loopID := "loop_retry_discard_repo_path"
	targetID := projectID
	repoPath := t.TempDir()
	worktreeRoot := filepath.Join(t.TempDir(), "worktrees")
	metadata, _ := json.Marshal(map[string]any{"worktreeRoot": worktreeRoot})
	metadataJSON := string(metadata)

	if err := services.Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Looper", RepoPath: repoPath, MetadataJSON: &metadataJSON, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := services.Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 3114, ProjectID: projectID, Type: "fixer", TargetType: "project", TargetID: &targetID, Status: "paused", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	checkpoint := fmt.Sprintf(`{"worktree":{"path":%q,"branch":"main"}}`, repoPath)
	if err := services.Repositories.Runs.Upsert(context.Background(), storage.RunRecord{ID: "run_repo_path", LoopID: loopID, Status: "failed", CheckpointJSON: &checkpoint, StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}
	lastErrorKind := "manual_intervention"
	if err := services.Repositories.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: "queue_repo_path", ProjectID: &projectID, LoopID: &loopID, Type: "fixer", TargetType: "project", TargetID: targetID, DedupeKey: "fixer:repo_path", Priority: storage.QueuePriorityFixer, Status: "manual_intervention", AvailableAt: nowISO, Attempts: 1, MaxAttempts: 3, LastErrorKind: &lastErrorKind, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/loops/3114/retry", strings.NewReader(`{"mode":"auto","discardWorktreeChanges":true}`))
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "must not equal project repo path") && !strings.Contains(recorder.Body.String(), "unsafe worktree path") {
		t.Fatalf("body = %s, want primary repo path rejection", recorder.Body.String())
	}
}

func TestHandlerLoopRetryWithoutDiscardDoesNotReportDiscard(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	h := NewHandler(Context{Config: cfg, Runtime: rt})
	services := rt.Services()
	nowISO := "2026-04-11T12:00:00.000Z"
	projectID := "project_retry_no_discard"
	loopID := "loop_retry_no_discard"
	targetID := projectID

	if err := services.Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Looper", RepoPath: t.TempDir(), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := services.Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 3115, ProjectID: projectID, Type: "worker", TargetType: "project", TargetID: &targetID, Status: "paused", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	lastErrorKind := "manual_intervention"
	if err := services.Repositories.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: "queue_retry_no_discard", ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: targetID, DedupeKey: "worker:no_discard", Priority: storage.QueuePriorityWorker, Status: "failed", AvailableAt: nowISO, Attempts: 2, MaxAttempts: 3, LastErrorKind: &lastErrorKind, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/loops/3115/retry", strings.NewReader(`{"mode":"auto","resetAttempts":true}`))
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	data := parseJSONMap(t, recorder.Body.Bytes())["data"].(map[string]any)
	assertEqual(t, data["discardWorktreeChanges"], false)
	if _, ok := data["worktreeDiscard"]; ok {
		t.Fatalf("worktreeDiscard present without flag: %#v", data["worktreeDiscard"])
	}
}

func TestHandlerLoopRetryDiscardPreservesDirtyWorktreeWhenAgentNotConfigured(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	cfg.Agent.Vendor = nil
	h := NewHandler(Context{Config: cfg, Runtime: rt})
	services := rt.Services()
	nowISO := "2026-04-11T12:00:00.000Z"

	fixture := seedManagedWorktreeFixture(t, services.Repositories, managedWorktreeSeed{
		ProjectID: "project_retry_discard_no_agent",
		LoopID:    "loop_retry_discard_no_agent",
		LoopSeq:   3116,
		LoopType:  "fixer",
		Branch:    "feature/discard-no-agent",
		NowISO:    nowISO,
		Dirty:     true,
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/loops/3116/retry", strings.NewReader(`{"mode":"auto","discardWorktreeChanges":true}`))
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "without config.agent.vendor") {
		t.Fatalf("body = %s, want agent not configured rejection", recorder.Body.String())
	}

	// Destructive discard must not run when a later retry precondition fails.
	if got := readTestFile(t, filepath.Join(fixture.WorktreePath, "dirty.txt")); got != "untracked\n" {
		t.Fatalf("dirty.txt after failed discard retry = %q, want preserved untracked content", got)
	}
	if got := readTestFile(t, filepath.Join(fixture.WorktreePath, "README.md")); got != "dirty tracked\n" {
		t.Fatalf("README.md after failed discard retry = %q, want preserved dirty tracked content", got)
	}
	loop, err := services.Repositories.Loops.GetByID(context.Background(), fixture.LoopID)
	if err != nil || loop == nil || loop.Status != "paused" {
		t.Fatalf("loop after failed discard retry = %#v, %v, want paused", loop, err)
	}
}

func TestHandlerLoopRetryDiscardPreservesDirtyWorktreeOnUniqueLoopConflict(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	h := NewHandler(Context{Config: cfg, Runtime: rt})
	services := rt.Services()
	nowISO := "2026-04-11T12:00:00.000Z"
	projectID := "project_retry_discard_unique"

	fixture := seedManagedWorktreeFixture(t, services.Repositories, managedWorktreeSeed{
		ProjectID: projectID,
		LoopID:    "loop_retry_discard_unique",
		LoopSeq:   3117,
		LoopType:  "fixer",
		Branch:    "feature/discard-unique",
		NowISO:    nowISO,
		Dirty:     true,
	})

	// Another active fixer on the same PR target must block retry before discard.
	repo := "acme/looper"
	prNumber := int64(42)
	prTarget := "pr:acme/looper:42"
	if err := services.Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{
		ID: "loop_retry_discard_unique_active", Seq: 3118, ProjectID: projectID,
		Type: "fixer", TargetType: "pull_request", TargetID: &prTarget, Repo: &repo, PRNumber: &prNumber,
		Status: "queued", CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert(conflict) error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/loops/3117/retry", strings.NewReader(`{"mode":"auto","discardWorktreeChanges":true}`))
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "active loop already exists") {
		t.Fatalf("body = %s, want unique loop conflict", recorder.Body.String())
	}
	if got := readTestFile(t, filepath.Join(fixture.WorktreePath, "dirty.txt")); got != "untracked\n" {
		t.Fatalf("dirty.txt after unique conflict = %q, want preserved", got)
	}
	if got := readTestFile(t, filepath.Join(fixture.WorktreePath, "README.md")); got != "dirty tracked\n" {
		t.Fatalf("README.md after unique conflict = %q, want preserved", got)
	}
}

// TestHandlerLoopStartSharesRetryLockWithDiscard ensures /start requeue takes
// the same per-loop mutex as discard+retry, so start cannot enqueue between
// discard preflight and git reset (wiping the worktree for start-created work).
func TestHandlerLoopStartSharesRetryLockWithDiscard(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	h := NewHandler(Context{Config: cfg, Runtime: rt})
	services := rt.Services()
	nowISO := "2026-04-11T12:00:00.000Z"

	fixture := seedManagedWorktreeFixture(t, services.Repositories, managedWorktreeSeed{
		ProjectID: "project_retry_discard_start_lock",
		LoopID:    "loop_retry_discard_start_lock",
		LoopSeq:   3122,
		LoopType:  "fixer",
		Branch:    "feature/discard-start-lock",
		NowISO:    nowISO,
		Dirty:     true,
	})

	// Hold the shared retry lock as if discard+retry is between preflight and reset.
	unlock := h.lockLoopRetry(fixture.LoopID)

	started := make(chan struct{})
	finished := make(chan int, 1)
	go func() {
		close(started)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/loops/3122/start", nil)
		recorder := httptest.NewRecorder()
		h.ServeHTTP(recorder, req)
		finished <- recorder.Code
	}()

	<-started
	select {
	case code := <-finished:
		unlock()
		t.Fatalf("start completed while retry/discard lock held: status=%d", code)
	case <-time.After(150 * time.Millisecond):
		// Still blocked on the shared lock — expected.
	}

	unlock()

	select {
	case code := <-finished:
		if code != http.StatusOK {
			t.Fatalf("start status after lock release = %d, want 200", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("start did not complete after retry/discard lock release")
	}

	// Start requeued from manual_intervention; dirty worktree must still exist
	// because no discard ran (only start held the lock after release).
	if got := readTestFile(t, filepath.Join(fixture.WorktreePath, "dirty.txt")); got != "untracked\n" {
		t.Fatalf("dirty.txt after blocked start = %q, want preserved", got)
	}
	active, err := services.Repositories.Queue.FindActiveByLoopID(context.Background(), fixture.LoopID)
	if err != nil {
		t.Fatalf("FindActiveByLoopID() error = %v", err)
	}
	if active == nil || active.Status != "queued" || active.ID == fixture.FailedQueueID {
		t.Fatalf("active queue after start = %#v, want new queued replacement", active)
	}
}

// TestHandlerLoopRetryDiscardConflictsAfterStartSerializes verifies that when
// start requeues first under the shared lock, a following discard+retry refuses
// with conflict and does not wipe the worktree (the failure the race caused).
func TestHandlerLoopRetryDiscardConflictsAfterStartSerializes(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	h := NewHandler(Context{Config: cfg, Runtime: rt})
	services := rt.Services()
	nowISO := "2026-04-11T12:00:00.000Z"

	fixture := seedManagedWorktreeFixture(t, services.Repositories, managedWorktreeSeed{
		ProjectID: "project_retry_discard_after_start",
		LoopID:    "loop_retry_discard_after_start",
		LoopSeq:   3123,
		LoopType:  "fixer",
		Branch:    "feature/discard-after-start",
		NowISO:    nowISO,
		Dirty:     true,
	})

	var wg sync.WaitGroup
	wg.Add(2)
	startCode := make(chan int, 1)
	retryCode := make(chan int, 1)
	retryBody := make(chan string, 1)

	go func() {
		defer wg.Done()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/loops/3123/start", nil)
		recorder := httptest.NewRecorder()
		h.ServeHTTP(recorder, req)
		startCode <- recorder.Code
	}()
	go func() {
		defer wg.Done()
		// Small delay so start is more likely to acquire the lock first; both
		// orders are correct under serialization, but this path asserts the
		// conflict+preserve-dirty outcome when start wins.
		time.Sleep(20 * time.Millisecond)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/loops/3123/retry", strings.NewReader(`{"mode":"auto","discardWorktreeChanges":true}`))
		recorder := httptest.NewRecorder()
		h.ServeHTTP(recorder, req)
		retryCode <- recorder.Code
		retryBody <- recorder.Body.String()
	}()
	wg.Wait()

	gotStart := <-startCode
	gotRetry := <-retryCode
	body := <-retryBody
	if gotStart != http.StatusOK {
		t.Fatalf("start status = %d, want 200", gotStart)
	}

	// Under shared lock, either order is valid:
	// - start first → retry 409, dirty preserved
	// - retry first → retry 200 (discarded), start 200 with active queue already present
	switch gotRetry {
	case http.StatusConflict:
		if !strings.Contains(body, "while queue item") && !strings.Contains(body, "while a run is active") {
			t.Fatalf("retry body = %s, want active queue/run conflict", body)
		}
		if got := readTestFile(t, filepath.Join(fixture.WorktreePath, "dirty.txt")); got != "untracked\n" {
			t.Fatalf("dirty.txt after conflicted discard retry = %q, want preserved", got)
		}
	case http.StatusOK:
		if _, err := os.Stat(filepath.Join(fixture.WorktreePath, "dirty.txt")); !os.IsNotExist(err) {
			t.Fatalf("dirty.txt still present after successful discard retry: %v", err)
		}
	default:
		t.Fatalf("retry status = %d, want 200 or 409; body=%s", gotRetry, body)
	}

	active, err := services.Repositories.Queue.FindActiveByLoopID(context.Background(), fixture.LoopID)
	if err != nil {
		t.Fatalf("FindActiveByLoopID() error = %v", err)
	}
	if active == nil || active.Status != "queued" {
		t.Fatalf("active queue after serialized start/retry = %#v, want queued", active)
	}
}

type managedWorktreeSeed struct {
	ProjectID string
	LoopID    string
	LoopSeq   int64
	LoopType  string
	Branch    string
	NowISO    string
	Dirty     bool
}

type managedWorktreeFixture struct {
	LoopID        string
	FailedQueueID string
	WorktreePath  string
	RepoPath      string
	WorktreeRoot  string
}

func seedManagedWorktreeFixture(t *testing.T, repos *storage.Repositories, seed managedWorktreeSeed) managedWorktreeFixture {
	t.Helper()
	root := t.TempDir()
	repoPath := filepath.Join(root, "repo")
	worktreeRoot := filepath.Join(root, "worktrees")
	remotePath := filepath.Join(root, "remote.git")
	if err := os.MkdirAll(repoPath, 0o755); err != nil {
		t.Fatalf("MkdirAll(repo) error = %v", err)
	}
	if err := os.MkdirAll(worktreeRoot, 0o755); err != nil {
		t.Fatalf("MkdirAll(worktreeRoot) error = %v", err)
	}
	if err := os.MkdirAll(remotePath, 0o755); err != nil {
		t.Fatalf("MkdirAll(remote) error = %v", err)
	}

	runGitTest(t, repoPath, "init", "-b", "main")
	runGitTest(t, remotePath, "init", "--bare")
	runGitTest(t, repoPath, "config", "user.email", "looper@example.com")
	runGitTest(t, repoPath, "config", "user.name", "Looper Test")
	runGitTest(t, repoPath, "remote", "add", "origin", remotePath)
	if err := os.WriteFile(filepath.Join(repoPath, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(README) error = %v", err)
	}
	runGitTest(t, repoPath, "add", "README.md")
	runGitTest(t, repoPath, "commit", "-m", "init")
	runGitTest(t, repoPath, "push", "-u", "origin", "main")
	runGitTest(t, repoPath, "checkout", "-b", seed.Branch)
	runGitTest(t, repoPath, "push", "-u", "origin", seed.Branch)
	runGitTest(t, repoPath, "checkout", "main")

	// Project must exist before CreateWorktree can store the worktree row.
	metadata, _ := json.Marshal(map[string]any{"worktreeRoot": worktreeRoot})
	metadataJSON := string(metadata)
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID: seed.ProjectID, Name: "Looper", RepoPath: repoPath, MetadataJSON: &metadataJSON, CreatedAt: seed.NowISO, UpdatedAt: seed.NowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	gateway := gitinfra.New(gitinfra.Options{GitPath: "git", Repos: repos})
	worktree, err := gateway.CreateWorktree(context.Background(), gitinfra.CreateWorktreeInput{
		ProjectID:    seed.ProjectID,
		RepoPath:     repoPath,
		WorktreeRoot: worktreeRoot,
		Branch:       seed.Branch,
		BaseBranch:   "main",
		PRNumber:     42,
	})
	if err != nil {
		t.Fatalf("CreateWorktree() error = %v", err)
	}
	if seed.Dirty {
		if err := os.WriteFile(filepath.Join(worktree.WorktreePath, "README.md"), []byte("dirty tracked\n"), 0o644); err != nil {
			t.Fatalf("WriteFile(dirty tracked) error = %v", err)
		}
		if err := os.WriteFile(filepath.Join(worktree.WorktreePath, "dirty.txt"), []byte("untracked\n"), 0o644); err != nil {
			t.Fatalf("WriteFile(dirty untracked) error = %v", err)
		}
	}

	targetID := seed.ProjectID
	repo := "acme/looper"
	prNumber := int64(42)
	loop := storage.LoopRecord{
		ID: seed.LoopID, Seq: seed.LoopSeq, ProjectID: seed.ProjectID, Type: seed.LoopType,
		TargetType: "project", TargetID: &targetID, Status: "paused", CreatedAt: seed.NowISO, UpdatedAt: seed.NowISO,
	}
	if seed.LoopType == "fixer" || seed.LoopType == "reviewer" {
		loop.TargetType = "pull_request"
		prTarget := fmt.Sprintf("pr:%s:%d", repo, prNumber)
		loop.TargetID = &prTarget
		loop.Repo = &repo
		loop.PRNumber = &prNumber
	}
	if err := repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	checkpoint := fmt.Sprintf(`{"worktree":{"id":%q,"path":%q,"branch":%q}}`, worktree.ID, worktree.WorktreePath, seed.Branch)
	if err := repos.Runs.Upsert(context.Background(), storage.RunRecord{
		ID: "run_" + seed.LoopID, LoopID: seed.LoopID, Status: "failed", CheckpointJSON: &checkpoint,
		StartedAt: seed.NowISO, CreatedAt: seed.NowISO, UpdatedAt: seed.NowISO,
	}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}

	failedQueueID := "queue_" + seed.LoopID
	lastErrorKind := "manual_intervention"
	lastError := "dirty worktree"
	queue := storage.QueueItemRecord{
		ID: failedQueueID, ProjectID: &seed.ProjectID, LoopID: &seed.LoopID, Type: seed.LoopType,
		TargetType: loop.TargetType, TargetID: *loop.TargetID, DedupeKey: seed.LoopType + ":" + seed.LoopID,
		Priority: storage.QueuePriorityWorker, Status: "manual_intervention", AvailableAt: seed.NowISO,
		Attempts: 2, MaxAttempts: 3, LastError: &lastError, LastErrorKind: &lastErrorKind,
		CreatedAt: seed.NowISO, UpdatedAt: seed.NowISO,
	}
	if seed.LoopType == "fixer" {
		queue.Priority = storage.QueuePriorityFixer
		queue.Repo = &repo
		queue.PRNumber = &prNumber
	}
	if seed.LoopType == "reviewer" {
		queue.Priority = storage.QueuePriorityReviewer
		queue.Repo = &repo
		queue.PRNumber = &prNumber
	}
	if err := repos.Queue.Upsert(context.Background(), queue); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	return managedWorktreeFixture{
		LoopID:        seed.LoopID,
		FailedQueueID: failedQueueID,
		WorktreePath:  worktree.WorktreePath,
		RepoPath:      repoPath,
		WorktreeRoot:  worktreeRoot,
	}
}

func runGitTest(t *testing.T, cwd string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s failed: %v\n%s", args, cwd, err, out)
	}
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", path, err)
	}
	return string(raw)
}
