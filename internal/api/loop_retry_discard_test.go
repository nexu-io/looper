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

	"github.com/nexu-io/looper/internal/domain"
	gitinfra "github.com/nexu-io/looper/internal/infra/git"
	looperdruntime "github.com/nexu-io/looper/internal/runtime"
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

func TestHandlerLoopRetryDiscardWorktreeChangesDirtyPlanner(t *testing.T) {
	// Planner prepare-worktree checkpoints a managed worktree; discard must clear
	// it the same way as fixer/reviewer/worker rather than no-opping.
	rt, cfg := startTestRuntime(t)
	h := NewHandler(Context{Config: cfg, Runtime: rt})
	services := rt.Services()
	nowISO := "2026-04-11T12:00:00.000Z"

	fixture := seedManagedWorktreeFixture(t, services.Repositories, managedWorktreeSeed{
		ProjectID: "project_retry_discard_planner",
		LoopID:    "loop_retry_discard_planner",
		LoopSeq:   3110,
		LoopType:  "planner",
		Branch:    "looper/planner/42-plan-this",
		NowISO:    nowISO,
		Dirty:     true,
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/loops/3110/retry", strings.NewReader(`{"mode":"auto","discardWorktreeChanges":true}`))
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	data := parseJSONMap(t, recorder.Body.Bytes())["data"].(map[string]any)
	assertEqual(t, data["discardWorktreeChanges"], true)
	discard := data["worktreeDiscard"].(map[string]any)
	assertEqual(t, discard["discarded"], true)
	assertEqual(t, discard["noOp"], false)
	assertEqual(t, discard["worktreePath"], fixture.WorktreePath)
	assertEqual(t, discard["reason"], "discarded")

	if _, err := os.Stat(filepath.Join(fixture.WorktreePath, "dirty.txt")); !os.IsNotExist(err) {
		t.Fatalf("dirty.txt still present after planner discard: %v", err)
	}
	if got := readTestFile(t, filepath.Join(fixture.WorktreePath, "README.md")); got != "hello\n" {
		t.Fatalf("README.md after planner discard = %q, want restored contents", got)
	}
	clean, err := gitinfra.New(gitinfra.Options{GitPath: "git"}).WorktreeClean(context.Background(), fixture.WorktreePath)
	if err != nil || !clean {
		t.Fatalf("planner worktree clean after discard = %v, %v", clean, err)
	}

	loop, err := services.Repositories.Loops.GetByID(context.Background(), fixture.LoopID)
	if err != nil || loop == nil || loop.Status != "queued" {
		t.Fatalf("loop after retry = %#v, %v, want queued", loop, err)
	}
}

func TestHandlerLoopRetryDiscardWorktreeChangesPlannerNoWorktree(t *testing.T) {
	// Planner without a resolvable worktree still retries as a no-op discard.
	rt, cfg := startTestRuntime(t)
	h := NewHandler(Context{Config: cfg, Runtime: rt})
	services := rt.Services()
	nowISO := "2026-04-11T12:00:00.000Z"
	projectID := "project_retry_discard_planner_none"
	loopID := "loop_retry_discard_planner_none"
	targetID := projectID

	if err := services.Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Planner", RepoPath: t.TempDir(), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := services.Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 3136, ProjectID: projectID, Type: "planner", TargetType: "project", TargetID: &targetID, Status: "paused", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	lastErrorKind := "manual_intervention"
	if err := services.Repositories.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: "queue_retry_discard_planner_none", ProjectID: &projectID, LoopID: &loopID, Type: "planner", TargetType: "project", TargetID: targetID, DedupeKey: "planner:retry_discard_none", Priority: storage.QueuePriorityPlanner, Status: "failed", AvailableAt: nowISO, Attempts: 1, MaxAttempts: 3, LastErrorKind: &lastErrorKind, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/loops/3136/retry", strings.NewReader(`{"mode":"auto","discardWorktreeChanges":true}`))
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	data := parseJSONMap(t, recorder.Body.Bytes())["data"].(map[string]any)
	discard := data["worktreeDiscard"].(map[string]any)
	assertEqual(t, discard["noOp"], true)
	assertEqual(t, discard["reason"], "no_worktree")
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
	h := NewHandler(Context{Config: cfg, Runtime: runtimeWithConfig(rt, cfg)})
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

func TestHandlerLoopRetryAllowsStickySnapshotWhenAgentNotConfigured(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	cfg.Agent.Vendor = nil
	h := NewHandler(Context{Config: cfg, Runtime: runtimeWithConfig(rt, cfg)})
	services := rt.Services()
	nowISO := "2026-04-11T12:00:00.000Z"

	fixture := seedManagedWorktreeFixture(t, services.Repositories, managedWorktreeSeed{
		ProjectID: "project_retry_sticky_snapshot",
		LoopID:    "loop_retry_sticky_snapshot",
		LoopSeq:   3142,
		LoopType:  "fixer",
		Branch:    "feature/retry-sticky-snapshot",
		NowISO:    nowISO,
		Dirty:     false,
	})

	// Predecessor failed run carries frozen agent identity for sticky retry.
	// agent_snapshot_json is insert-only, so seed a later failed run with the snapshot.
	snapshot := `{"vendor":"codex","model":"frozen-model","profileId":"fixer-profile"}`
	laterISO := "2026-04-11T12:01:00.000Z"
	if err := services.Repositories.Runs.Upsert(context.Background(), storage.RunRecord{
		ID: "run_" + fixture.LoopID + "_snap", LoopID: fixture.LoopID, Status: "failed",
		StartedAt: laterISO, CreatedAt: laterISO, UpdatedAt: laterISO, AgentSnapshotJSON: &snapshot,
	}); err != nil {
		t.Fatalf("Runs.Upsert(snapshot) error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/loops/3142/retry", strings.NewReader(`{"mode":"auto","resetAttempts":true}`))
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 sticky retry with predecessor snapshot; body=%s", recorder.Code, recorder.Body.String())
	}
	loop, err := services.Repositories.Loops.GetByID(context.Background(), fixture.LoopID)
	if err != nil || loop == nil || loop.Status != "queued" {
		t.Fatalf("loop after sticky snapshot retry = %#v, %v, want queued", loop, err)
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

// TestHandlerLoopRetryDiscardRejectsActiveSiblingPRLoop ensures discard+retry
// refuses when a different loop type already holds a worktree-owning status on
// the same PR. Same-type uniqueness alone would allow a failed fixer discard to
// git reset/clean under a queued/running/waiting/failed/interrupted/
// human_takeover reviewer or worker that shares the managed PR worktree.
// waiting/failed/interrupted are outside IsConflictingActiveLoopStatus but
// still pin the checkout (worktree cleanup protects them).
func TestHandlerLoopRetryDiscardRejectsActiveSiblingPRLoop(t *testing.T) {
	cases := []struct {
		name          string
		siblingType   string
		siblingStatus string
	}{
		{name: "queued_reviewer", siblingType: "reviewer", siblingStatus: "queued"},
		{name: "running_worker", siblingType: "worker", siblingStatus: "running"},
		{name: "waiting_reviewer", siblingType: "reviewer", siblingStatus: "waiting"},
		{name: "failed_reviewer", siblingType: "reviewer", siblingStatus: "failed"},
		{name: "interrupted_worker", siblingType: "worker", siblingStatus: "interrupted"},
		{name: "human_takeover_reviewer", siblingType: "reviewer", siblingStatus: "human_takeover"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt, cfg := startTestRuntime(t)
			h := NewHandler(Context{Config: cfg, Runtime: rt})
			services := rt.Services()
			nowISO := "2026-04-11T12:00:00.000Z"
			projectID := "project_retry_discard_sibling_" + tc.name

			fixture := seedManagedWorktreeFixture(t, services.Repositories, managedWorktreeSeed{
				ProjectID: projectID,
				LoopID:    "loop_retry_discard_sibling_" + tc.name,
				LoopSeq:   3140,
				LoopType:  "fixer",
				Branch:    "feature/discard-sibling-" + tc.name,
				NowISO:    nowISO,
				Dirty:     true,
			})

			repo := "acme/looper"
			prNumber := int64(42)
			prTarget := "pr:acme/looper:42"
			if err := services.Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{
				ID: "loop_sibling_" + tc.name, Seq: 3141, ProjectID: projectID,
				Type: tc.siblingType, TargetType: "pull_request", TargetID: &prTarget, Repo: &repo, PRNumber: &prNumber,
				Status: tc.siblingStatus, CreatedAt: nowISO, UpdatedAt: nowISO,
			}); err != nil {
				t.Fatalf("Loops.Upsert(sibling) error = %v", err)
			}

			req := httptest.NewRequest(http.MethodPost, "/api/v1/loops/"+fixture.LoopID+"/retry", strings.NewReader(`{"mode":"auto","discardWorktreeChanges":true}`))
			recorder := httptest.NewRecorder()
			h.ServeHTTP(recorder, req)
			if recorder.Code != http.StatusConflict {
				t.Fatalf("status = %d, want 409; body=%s", recorder.Code, recorder.Body.String())
			}
			if !strings.Contains(recorder.Body.String(), "shares the same PR worktree") {
				t.Fatalf("body = %s, want sibling PR worktree conflict", recorder.Body.String())
			}
			if got := readTestFile(t, filepath.Join(fixture.WorktreePath, "dirty.txt")); got != "untracked\n" {
				t.Fatalf("dirty.txt after sibling conflict = %q, want preserved", got)
			}
			if got := readTestFile(t, filepath.Join(fixture.WorktreePath, "README.md")); got != "dirty tracked\n" {
				t.Fatalf("README.md after sibling conflict = %q, want preserved", got)
			}
			loop, err := services.Repositories.Loops.GetByID(context.Background(), fixture.LoopID)
			if err != nil || loop == nil || loop.Status != "paused" {
				t.Fatalf("loop after sibling reject = %#v, %v, want paused", loop, err)
			}
		})
	}
}

// TestHandlerLoopRetryDiscardAllowsCompletedSiblingPRLoop documents that a
// truly terminal sibling (completed/stopped/terminated) on the same PR does
// not pin the managed worktree and therefore does not block discard.
func TestHandlerLoopRetryDiscardAllowsCompletedSiblingPRLoop(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	h := NewHandler(Context{Config: cfg, Runtime: rt})
	services := rt.Services()
	nowISO := "2026-04-11T12:00:00.000Z"
	projectID := "project_retry_discard_completed_sibling"

	fixture := seedManagedWorktreeFixture(t, services.Repositories, managedWorktreeSeed{
		ProjectID: projectID,
		LoopID:    "loop_retry_discard_completed_sibling",
		LoopSeq:   3142,
		LoopType:  "fixer",
		Branch:    "feature/discard-completed-sibling",
		NowISO:    nowISO,
		Dirty:     true,
	})

	repo := "acme/looper"
	prNumber := int64(42)
	prTarget := "pr:acme/looper:42"
	if err := services.Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{
		ID: "loop_completed_sibling_reviewer", Seq: 3143, ProjectID: projectID,
		Type: "reviewer", TargetType: "pull_request", TargetID: &prTarget, Repo: &repo, PRNumber: &prNumber,
		Status: "completed", CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert(completed sibling) error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/loops/"+fixture.LoopID+"/retry", strings.NewReader(`{"mode":"auto","discardWorktreeChanges":true}`))
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	if _, err := os.Stat(filepath.Join(fixture.WorktreePath, "dirty.txt")); !os.IsNotExist(err) {
		t.Fatalf("dirty.txt still present after discard with completed sibling: %v", err)
	}
}
func TestHandlerWorkersCreateReuseSharesRetryLockWithDiscard(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	h := NewHandler(Context{Config: cfg, Runtime: rt})
	services := rt.Services()
	nowISO := "2026-04-11T12:00:00.000Z"

	baseBranch := "main"
	metadata := `{"repo":"acme/looper"}`
	if err := services.Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID: "project_retry_discard_worker_reuse", Name: "Looper", RepoPath: "/tmp/repos/looper",
		BaseBranch: &baseBranch, MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	loopID := "loop_retry_discard_worker_reuse"
	targetID := "issue:acme/looper:88"
	repo := "acme/looper"
	workerMeta := `{"worker":{"title":"Paused issue worker","repo":"acme/looper","baseBranch":"main","issueNumber":88}}`
	if err := services.Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{
		ID: loopID, Seq: 3124, ProjectID: "project_retry_discard_worker_reuse", Type: "worker",
		TargetType: "issue", TargetID: &targetID, Repo: &repo, Status: "paused",
		MetadataJSON: &workerMeta, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	// Hold the shared retry lock as if discard+retry is between preflight and reset.
	unlock := h.lockLoopRetry(loopID)

	started := make(chan struct{})
	finished := make(chan int, 1)
	go func() {
		close(started)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/workers", strings.NewReader(
			`{"projectId":"project_retry_discard_worker_reuse","repo":"acme/looper","issueNumber":88,"baseBranch":"main"}`,
		))
		req.Header.Set("content-type", "application/json")
		recorder := httptest.NewRecorder()
		h.ServeHTTP(recorder, req)
		finished <- recorder.Code
	}()

	<-started
	select {
	case code := <-finished:
		unlock()
		t.Fatalf("worker reuse completed while retry/discard lock held: status=%d", code)
	case <-time.After(150 * time.Millisecond):
		// Still blocked on the shared lock — expected.
	}

	unlock()

	select {
	case code := <-finished:
		if code != http.StatusOK {
			t.Fatalf("worker reuse status after lock release = %d, want 200", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("worker reuse did not complete after retry/discard lock release")
	}

	loop, err := services.Repositories.Loops.GetByID(context.Background(), loopID)
	if err != nil || loop == nil || loop.Status != "queued" {
		t.Fatalf("loop after reuse = %#v, %v, want queued", loop, err)
	}
	active, err := services.Repositories.Queue.FindActiveByLoopID(context.Background(), loopID)
	if err != nil {
		t.Fatalf("FindActiveByLoopID() error = %v", err)
	}
	if active == nil || active.Status != "queued" {
		t.Fatalf("active queue after worker reuse = %#v, want queued", active)
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

// TestHandlerLoopRetryDiscardResolvesByPRNotSiblingBranch ensures that when the
// sole (project, branch) worktree row points at a sibling PR's CreateWorktree
// path (last writer won the unique branch index), discard+retry for PR 42 does
// not reset/clean that sibling checkout.
func TestHandlerLoopRetryDiscardResolvesByPRNotSiblingBranch(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	h := NewHandler(Context{Config: cfg, Runtime: rt})
	services := rt.Services()
	nowISO := "2026-04-11T12:00:00.000Z"
	projectID := "project_retry_discard_pr_disambig"
	branch := "feature/shared-head"

	// Primary fixture creates ...-pr-42 for branch, then we retarget the row to a
	// sibling PR-99 path (simulating another PR with the same head branch name
	// overwriting the unique project+branch worktree record).
	fixture := seedManagedWorktreeFixture(t, services.Repositories, managedWorktreeSeed{
		ProjectID: projectID,
		LoopID:    "loop_retry_discard_pr_disambig",
		LoopSeq:   3125,
		LoopType:  "fixer",
		Branch:    branch,
		NowISO:    nowISO,
		Dirty:     true,
	})

	siblingPath := filepath.Join(fixture.WorktreeRoot, fmt.Sprintf("looper-fix-%s-pr-99", projectID))
	if err := os.MkdirAll(siblingPath, 0o755); err != nil {
		t.Fatalf("MkdirAll(sibling) error = %v", err)
	}
	// Real git worktree so DiscardWorktreeChanges would succeed if wrongly selected.
	runGitTest(t, fixture.RepoPath, "worktree", "add", "--force", siblingPath, branch)
	if err := os.WriteFile(filepath.Join(siblingPath, "sibling-dirty.txt"), []byte("keep me\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(sibling dirty) error = %v", err)
	}
	existing, err := services.Repositories.Worktrees.GetByBranch(context.Background(), projectID, branch)
	if err != nil || existing == nil {
		t.Fatalf("GetByBranch() = %#v, %v", existing, err)
	}
	existing.WorktreePath = siblingPath
	existing.UpdatedAt = nowISO
	if err := services.Repositories.Worktrees.Upsert(context.Background(), *existing); err != nil {
		t.Fatalf("Worktrees.Upsert(retarget sibling) error = %v", err)
	}

	// Branch-only checkpoint (dirty prepare): no worktree path/id.
	detailOnly := fmt.Sprintf(`{"detail":{"headRefName":%q,"state":"OPEN","prNumber":42},"pause":{"reason":"dirty_worktree"}}`, branch)
	if err := services.Repositories.Runs.Upsert(context.Background(), storage.RunRecord{
		ID: "run_" + fixture.LoopID, LoopID: fixture.LoopID, Status: "failed", CheckpointJSON: &detailOnly,
		StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Runs.Upsert(detail-only) error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/loops/3125/retry", strings.NewReader(`{"mode":"auto","discardWorktreeChanges":true}`))
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	data := parseJSONMap(t, recorder.Body.Bytes())["data"].(map[string]any)
	discard := data["worktreeDiscard"].(map[string]any)
	// Must not discard the sibling PR-99 tree; no resolvable PR-42 worktree → no-op.
	assertEqual(t, discard["discarded"], false)
	assertEqual(t, discard["noOp"], true)

	if got := readTestFile(t, filepath.Join(siblingPath, "sibling-dirty.txt")); got != "keep me\n" {
		t.Fatalf("sibling worktree was discarded: sibling-dirty.txt = %q", got)
	}
	// Original PR-42 checkout dirt should also remain (we did not resolve it).
	if got := readTestFile(t, filepath.Join(fixture.WorktreePath, "dirty.txt")); got != "untracked\n" {
		t.Fatalf("pr-42 dirty.txt = %q, want preserved (unresolved worktree)", got)
	}
}

// TestHandlerLoopRetryDiscardRejectsAwaitingHuman ensures discard+retry refuses
// awaiting_human loops so runtime HITL poll requeue cannot race after preflight
// and leave a wiped worktree when the retry TX conflicts.
func TestHandlerLoopRetryDiscardRejectsAwaitingHuman(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	h := NewHandler(Context{Config: cfg, Runtime: rt})
	services := rt.Services()
	nowISO := "2026-04-11T12:00:00.000Z"

	fixture := seedManagedWorktreeFixture(t, services.Repositories, managedWorktreeSeed{
		ProjectID: "project_retry_discard_awaiting_human",
		LoopID:    "loop_retry_discard_awaiting_human",
		LoopSeq:   3126,
		LoopType:  "fixer",
		Branch:    "feature/discard-awaiting-human",
		NowISO:    nowISO,
		Dirty:     true,
	})
	loop, err := services.Repositories.Loops.GetByID(context.Background(), fixture.LoopID)
	if err != nil || loop == nil {
		t.Fatalf("GetByID() = %#v, %v", loop, err)
	}
	loop.Status = "awaiting_human"
	if err := services.Repositories.Loops.Upsert(context.Background(), *loop); err != nil {
		t.Fatalf("Loops.Upsert(awaiting_human) error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/loops/3126/retry", strings.NewReader(`{"mode":"auto","discardWorktreeChanges":true}`))
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "awaiting_human") {
		t.Fatalf("body = %s, want awaiting_human rejection", recorder.Body.String())
	}
	if got := readTestFile(t, filepath.Join(fixture.WorktreePath, "dirty.txt")); got != "untracked\n" {
		t.Fatalf("dirty.txt after awaiting_human reject = %q, want preserved", got)
	}
}

// TestHandlerLoopRetryDiscardRejectsHumanTakeover ensures discard+retry refuses
// human_takeover loops so interactive edits pinned by takeover are not wiped
// via direct /retry (mirroring /handback which also rejects discard).
func TestHandlerLoopRetryDiscardRejectsHumanTakeover(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	h := NewHandler(Context{Config: cfg, Runtime: rt})
	services := rt.Services()
	nowISO := "2026-04-11T12:00:00.000Z"

	fixture := seedManagedWorktreeFixture(t, services.Repositories, managedWorktreeSeed{
		ProjectID: "project_retry_discard_human_takeover",
		LoopID:    "loop_retry_discard_human_takeover",
		LoopSeq:   3135,
		LoopType:  "fixer",
		Branch:    "feature/discard-human-takeover",
		NowISO:    nowISO,
		Dirty:     true,
	})
	loop, err := services.Repositories.Loops.GetByID(context.Background(), fixture.LoopID)
	if err != nil || loop == nil {
		t.Fatalf("GetByID() = %#v, %v", loop, err)
	}
	loop.Status = "human_takeover"
	if err := services.Repositories.Loops.Upsert(context.Background(), *loop); err != nil {
		t.Fatalf("Loops.Upsert(human_takeover) error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/loops/3135/retry", strings.NewReader(`{"mode":"auto","discardWorktreeChanges":true}`))
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "human_takeover") {
		t.Fatalf("body = %s, want human_takeover rejection", recorder.Body.String())
	}
	if got := readTestFile(t, filepath.Join(fixture.WorktreePath, "dirty.txt")); got != "untracked\n" {
		t.Fatalf("dirty.txt after human_takeover reject = %q, want preserved", got)
	}
	loopAfter, err := services.Repositories.Loops.GetByID(context.Background(), fixture.LoopID)
	if err != nil || loopAfter == nil || loopAfter.Status != "human_takeover" {
		t.Fatalf("loop after rejected discard retry = %#v, %v, want human_takeover", loopAfter, err)
	}
}

// TestHandlerHandbackRejectsDiscardWorktreeChanges ensures /handback never honors
// discardWorktreeChanges: the worktree may hold the human's interactive edits.
func TestHandlerHandbackRejectsDiscardWorktreeChanges(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	h := NewHandler(Context{Config: cfg, Runtime: rt})
	services := rt.Services()
	nowISO := "2026-04-11T12:00:00.000Z"

	fixture := seedManagedWorktreeFixture(t, services.Repositories, managedWorktreeSeed{
		ProjectID: "project_handback_reject_discard",
		LoopID:    "loop_handback_reject_discard",
		LoopSeq:   3132,
		LoopType:  "fixer",
		Branch:    "feature/handback-reject-discard",
		NowISO:    nowISO,
		Dirty:     true,
	})
	loop, err := services.Repositories.Loops.GetByID(context.Background(), fixture.LoopID)
	if err != nil || loop == nil {
		t.Fatalf("GetByID() = %#v, %v", loop, err)
	}
	loop.Status = "human_takeover"
	if err := services.Repositories.Loops.Upsert(context.Background(), *loop); err != nil {
		t.Fatalf("Loops.Upsert(human_takeover) error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/loops/3132/handback", strings.NewReader(`{"mode":"auto","discardWorktreeChanges":true}`))
	req.Header.Set("content-type", "application/json")
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "discardWorktreeChanges is not allowed on handback") {
		t.Fatalf("body = %s, want handback discard rejection", recorder.Body.String())
	}
	if got := readTestFile(t, filepath.Join(fixture.WorktreePath, "dirty.txt")); got != "untracked\n" {
		t.Fatalf("dirty.txt after handback discard reject = %q, want preserved", got)
	}
	loopAfter, err := services.Repositories.Loops.GetByID(context.Background(), fixture.LoopID)
	if err != nil || loopAfter == nil || loopAfter.Status != "human_takeover" {
		t.Fatalf("loop after rejected handback = %#v, %v, want human_takeover", loopAfter, err)
	}
}

// TestHandlerLoopRetryDiscardRejectsUntaggedPRWorktree ensures branch-only lookup
// for a PR-scoped loop does not discard an untagged worktree row that cannot prove
// pr-<N> ownership (shared head branch across PRs).
func TestHandlerLoopRetryDiscardRejectsUntaggedPRWorktree(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	h := NewHandler(Context{Config: cfg, Runtime: rt})
	services := rt.Services()
	nowISO := "2026-04-11T12:00:00.000Z"
	projectID := "project_retry_discard_untagged_pr"
	branch := "feature/shared-untagged"

	fixture := seedManagedWorktreeFixture(t, services.Repositories, managedWorktreeSeed{
		ProjectID: projectID,
		LoopID:    "loop_retry_discard_untagged_pr",
		LoopSeq:   3133,
		LoopType:  "fixer",
		Branch:    branch,
		NowISO:    nowISO,
		Dirty:     true,
	})

	// Retarget the stored row to an untagged path (RestoreWorktree-style adopt by
	// branch under worktree root with no pr-<N> marker).
	untaggedPath := filepath.Join(fixture.WorktreeRoot, "adopted-shared-head")
	if err := os.MkdirAll(untaggedPath, 0o755); err != nil {
		t.Fatalf("MkdirAll(untagged) error = %v", err)
	}
	runGitTest(t, fixture.RepoPath, "worktree", "add", "--force", untaggedPath, branch)
	if err := os.WriteFile(filepath.Join(untaggedPath, "untagged-dirty.txt"), []byte("keep me\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(untagged dirty) error = %v", err)
	}
	existing, err := services.Repositories.Worktrees.GetByBranch(context.Background(), projectID, branch)
	if err != nil || existing == nil {
		t.Fatalf("GetByBranch() = %#v, %v", existing, err)
	}
	existing.WorktreePath = untaggedPath
	existing.UpdatedAt = nowISO
	if err := services.Repositories.Worktrees.Upsert(context.Background(), *existing); err != nil {
		t.Fatalf("Worktrees.Upsert(untagged path) error = %v", err)
	}

	// Branch-only checkpoint (dirty prepare): no worktree path/id, PR from detail.
	detailOnly := fmt.Sprintf(`{"detail":{"headRefName":%q,"state":"OPEN","prNumber":42},"pause":{"reason":"dirty_worktree"}}`, branch)
	if err := services.Repositories.Runs.Upsert(context.Background(), storage.RunRecord{
		ID: "run_" + fixture.LoopID, LoopID: fixture.LoopID, Status: "failed", CheckpointJSON: &detailOnly,
		StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Runs.Upsert(detail-only) error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/loops/3133/retry", strings.NewReader(`{"mode":"auto","discardWorktreeChanges":true}`))
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	data := parseJSONMap(t, recorder.Body.Bytes())["data"].(map[string]any)
	discard := data["worktreeDiscard"].(map[string]any)
	assertEqual(t, discard["discarded"], false)
	assertEqual(t, discard["noOp"], true)

	if got := readTestFile(t, filepath.Join(untaggedPath, "untagged-dirty.txt")); got != "keep me\n" {
		t.Fatalf("untagged worktree was discarded: untagged-dirty.txt = %q", got)
	}
}

// TestHandlerLockLoopRetryIsProcessWideRequeueGuard ensures API lockLoopRetry
// is the same mutex runtime free-text enqueue takes (LockLoopRequeue), so
// discard+retry serializes against inbox requeues rather than relying only on
// a non-atomic pre-git recheck.
func TestHandlerLockLoopRetryIsProcessWideRequeueGuard(t *testing.T) {
	h := NewHandler(Context{})
	const loopID = "loop_shared_requeue_guard"

	unlock := h.lockLoopRetry(loopID)
	started := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		close(started)
		// Runtime enqueue takes this exact process-wide guard.
		unlockRuntime := looperdruntime.LockLoopRequeue(loopID)
		unlockRuntime()
		close(finished)
	}()
	<-started
	select {
	case <-finished:
		unlock()
		t.Fatal("LockLoopRequeue completed while Handler.lockLoopRetry held — locks are not shared")
	case <-time.After(150 * time.Millisecond):
	}
	unlock()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("LockLoopRequeue did not complete after Handler.lockLoopRetry release")
	}
}

// TestHandlerLoopRetryDiscardRechecksBeforeGitReset ensures a runtime-style
// free-text message requeue (paused → queued + active queue) injected after the
// first preflight and before git reset causes conflict without wiping the tree.
func TestHandlerLoopRetryDiscardRechecksBeforeGitReset(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	h := NewHandler(Context{Config: cfg, Runtime: rt})
	services := rt.Services()
	nowISO := "2026-04-11T12:00:00.000Z"

	fixture := seedManagedWorktreeFixture(t, services.Repositories, managedWorktreeSeed{
		ProjectID: "project_retry_discard_recheck",
		LoopID:    "loop_retry_discard_recheck",
		LoopSeq:   3134,
		LoopType:  "fixer",
		Branch:    "feature/discard-recheck",
		NowISO:    nowISO,
		Dirty:     true,
	})

	// Simulate Feishu/GitHub enqueueHumanMessageToLoop: mark the loop queued and
	// create an active queue item after the first discard preflight passes.
	// Fixture rows are manual_intervention (not cancelled), so requeue-by-cancelled
	// is a no-op — promote the latest row the same way a message-driven rearm would.
	h.discardBeforeGitHook = func(loopID string) {
		if loopID != fixture.LoopID {
			return
		}
		loop, err := services.Repositories.Loops.GetByID(context.Background(), loopID)
		if err != nil || loop == nil {
			t.Errorf("hook GetByID() = %#v, %v", loop, err)
			return
		}
		loop.Status = "queued"
		loop.NextRunAt = &nowISO
		loop.UpdatedAt = nowISO
		if err := services.Repositories.Loops.Upsert(context.Background(), *loop); err != nil {
			t.Errorf("hook Loops.Upsert() error = %v", err)
			return
		}
		latest, qerr := services.Repositories.Queue.GetLatestByLoopID(context.Background(), loopID)
		if qerr != nil || latest == nil {
			t.Errorf("hook GetLatestByLoopID() = %#v, %v", latest, qerr)
			return
		}
		latest.Status = "queued"
		latest.AvailableAt = nowISO
		latest.UpdatedAt = nowISO
		if uerr := services.Repositories.Queue.Upsert(context.Background(), *latest); uerr != nil {
			t.Errorf("hook Queue.Upsert(active) error = %v", uerr)
		}
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/loops/3134/retry", strings.NewReader(`{"mode":"auto","discardWorktreeChanges":true}`))
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "while queue item") {
		t.Fatalf("body = %s, want active queue conflict from pre-git recheck", recorder.Body.String())
	}
	if got := readTestFile(t, filepath.Join(fixture.WorktreePath, "dirty.txt")); got != "untracked\n" {
		t.Fatalf("dirty.txt after recheck conflict = %q, want preserved", got)
	}
}

// TestWorktreeBelongsToPRRejectsUntagged ensures PR-scoped ownership requires an
// explicit pr-<N> path or bare pr-<N> branch — untagged paths do not qualify.
func TestWorktreeBelongsToPRRejectsUntagged(t *testing.T) {
	if worktreeBelongsToPR(storage.WorktreeRecord{
		WorktreePath: "/tmp/worktrees/adopted-feature-foo",
		Branch:       "feature/foo",
	}, 42) {
		t.Fatal("untagged path must not belong to PR 42")
	}
	if !worktreeBelongsToPR(storage.WorktreeRecord{
		WorktreePath: "/tmp/worktrees/looper-fix-proj-pr-42",
		Branch:       "feature/foo",
	}, 42) {
		t.Fatal("pr-42 path must belong to PR 42")
	}
	if worktreeBelongsToPR(storage.WorktreeRecord{
		WorktreePath: "/tmp/worktrees/looper-fix-proj-pr-99",
		Branch:       "feature/foo",
	}, 42) {
		t.Fatal("pr-99 path must not belong to PR 42")
	}
	if !worktreeBelongsToPR(storage.WorktreeRecord{
		WorktreePath: "/tmp/worktrees/some-dir",
		Branch:       "pr-42",
	}, 42) {
		t.Fatal("bare pr-42 branch must belong to PR 42")
	}
}

// TestHandlerLoopCreateSharesTargetLockWithDiscard ensures POST /loops for the
// same PR target takes the shared target mutex held by discard+retry, so create
// cannot enqueue between preflight and git reset.
func TestHandlerLoopCreateSharesTargetLockWithDiscard(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	h := NewHandler(Context{Config: cfg, Runtime: rt})
	services := rt.Services()
	nowISO := "2026-04-11T12:00:00.000Z"
	projectID := "project_retry_discard_target_lock"

	fixture := seedManagedWorktreeFixture(t, services.Repositories, managedWorktreeSeed{
		ProjectID: projectID,
		LoopID:    "loop_retry_discard_target_lock",
		LoopSeq:   3127,
		LoopType:  "fixer",
		Branch:    "feature/discard-target-lock",
		NowISO:    nowISO,
		Dirty:     true,
	})

	// Align project metadata with create-loop validation (repo + no hold labels).
	project, err := services.Repositories.Projects.GetByID(context.Background(), projectID)
	if err != nil || project == nil {
		t.Fatalf("Projects.GetByID() = %#v, %v", project, err)
	}
	meta, _ := json.Marshal(map[string]any{"worktreeRoot": fixture.WorktreeRoot, "repo": "acme/looper"})
	metaJSON := string(meta)
	project.MetadataJSON = &metaJSON
	if err := services.Repositories.Projects.Upsert(context.Background(), *project); err != nil {
		t.Fatalf("Projects.Upsert(repo meta) error = %v", err)
	}

	// Hold the same-target lock as discard+retry does after loading the loop.
	target := mustLoopTargetFromFixture(t, services.Repositories, fixture.LoopID)
	unlockTarget := h.lockLoopTarget(projectID, domain.LoopTypeFixer, target)

	started := make(chan struct{})
	finished := make(chan struct {
		code int
		body string
	}, 1)
	go func() {
		close(started)
		// Creating another fixer for the same PR target should block on the target lock.
		// Use a different PR number so uniqueness does not 409 after the lock is released;
		// we only need the create path to take lockLoopTargetForStatus for fixer+PR targets.
		// Actually same target is required to share the key — expect 200 or conflict after.
		req := httptest.NewRequest(http.MethodPost, "/api/v1/loops", strings.NewReader(
			`{"projectId":"project_retry_discard_target_lock","type":"fixer","targetType":"pull_request","repo":"acme/looper","prNumber":42,"force":true}`,
		))
		req.Header.Set("content-type", "application/json")
		recorder := httptest.NewRecorder()
		h.ServeHTTP(recorder, req)
		finished <- struct {
			code int
			body string
		}{recorder.Code, recorder.Body.String()}
	}()

	<-started
	select {
	case got := <-finished:
		unlockTarget()
		t.Fatalf("loop create completed while target lock held: status=%d body=%s", got.code, got.body)
	case <-time.After(150 * time.Millisecond):
		// Still blocked — expected.
	}

	unlockTarget()

	select {
	case got := <-finished:
		// After release, create may 200 (failed loop is not uniqueness-conflicting)
		// or 409 for other reasons; we only require it was serialized behind the lock.
		if got.code != http.StatusOK && got.code != http.StatusConflict {
			t.Fatalf("loop create status after lock release = %d, want 200/409; body=%s", got.code, got.body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("loop create did not complete after target lock release")
	}

	// Dirty worktree must still be present: only the lock was held, no discard ran.
	if got := readTestFile(t, filepath.Join(fixture.WorktreePath, "dirty.txt")); got != "untracked\n" {
		t.Fatalf("dirty.txt after blocked create = %q, want preserved", got)
	}
}

// TestHandlerLoopRetrySharesTargetLockAcrossLoops ensures a regular (non-discard)
// retry for a different failed loop on the same PR target takes the shared target
// mutex, so it cannot create an active queue item between another request's
// discard preflight and git reset.
func TestHandlerLoopRetrySharesTargetLockAcrossLoops(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	h := NewHandler(Context{Config: cfg, Runtime: rt})
	services := rt.Services()
	nowISO := "2026-04-11T12:00:00.000Z"
	projectID := "project_retry_target_lock_across"

	// Loop A owns the shared PR target key; hold its target lock as discard would.
	// Both loops stay failed (non-conflicting) so B's requeue is only gated by the lock.
	fixtureA := seedManagedWorktreeFixture(t, services.Repositories, managedWorktreeSeed{
		ProjectID: projectID,
		LoopID:    "loop_retry_target_lock_a",
		LoopSeq:   3128,
		LoopType:  "fixer",
		Branch:    "feature/discard-target-lock-a",
		NowISO:    nowISO,
		Dirty:     true,
	})
	markLoopFailed(t, services.Repositories, fixtureA.LoopID, nowISO)
	// Loop B: second failed fixer for the same PR (non-conflicting while failed).
	seedSecondFailedFixerSamePR(t, services.Repositories, projectID, "loop_retry_target_lock_b", 3129, nowISO)

	target := mustLoopTargetFromFixture(t, services.Repositories, fixtureA.LoopID)
	unlockTarget := h.lockLoopTarget(projectID, domain.LoopTypeFixer, target)

	started := make(chan struct{})
	finished := make(chan int, 1)
	go func() {
		close(started)
		// Regular retry (no discard) of the *other* loop must wait on target lock.
		req := httptest.NewRequest(http.MethodPost, "/api/v1/loops/3129/retry", strings.NewReader(`{"mode":"auto","resetAttempts":true}`))
		req.Header.Set("content-type", "application/json")
		recorder := httptest.NewRecorder()
		h.ServeHTTP(recorder, req)
		finished <- recorder.Code
	}()

	<-started
	select {
	case code := <-finished:
		unlockTarget()
		t.Fatalf("regular retry completed while same-target lock held: status=%d", code)
	case <-time.After(150 * time.Millisecond):
		// Still blocked — expected.
	}

	unlockTarget()

	select {
	case code := <-finished:
		if code != http.StatusOK {
			t.Fatalf("regular retry status after target lock release = %d, want 200", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("regular retry did not complete after target lock release")
	}

	// Dirty worktree for loop A must remain: only the lock was held, no discard.
	if got := readTestFile(t, filepath.Join(fixtureA.WorktreePath, "dirty.txt")); got != "untracked\n" {
		t.Fatalf("dirty.txt after blocked regular retry = %q, want preserved", got)
	}
}

// TestHandlerLoopStartSharesTargetLockAcrossLoops ensures /start for a different
// failed loop on the same PR target takes the shared target mutex with discard.
func TestHandlerLoopStartSharesTargetLockAcrossLoops(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	h := NewHandler(Context{Config: cfg, Runtime: rt})
	services := rt.Services()
	nowISO := "2026-04-11T12:00:00.000Z"
	projectID := "project_start_target_lock_across"

	fixtureA := seedManagedWorktreeFixture(t, services.Repositories, managedWorktreeSeed{
		ProjectID: projectID,
		LoopID:    "loop_start_target_lock_a",
		LoopSeq:   3130,
		LoopType:  "fixer",
		Branch:    "feature/start-target-lock-a",
		NowISO:    nowISO,
		Dirty:     true,
	})
	markLoopFailed(t, services.Repositories, fixtureA.LoopID, nowISO)
	seedSecondFailedFixerSamePR(t, services.Repositories, projectID, "loop_start_target_lock_b", 3131, nowISO)

	target := mustLoopTargetFromFixture(t, services.Repositories, fixtureA.LoopID)
	unlockTarget := h.lockLoopTarget(projectID, domain.LoopTypeFixer, target)

	started := make(chan struct{})
	finished := make(chan int, 1)
	go func() {
		close(started)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/loops/3131/start", nil)
		recorder := httptest.NewRecorder()
		h.ServeHTTP(recorder, req)
		finished <- recorder.Code
	}()

	<-started
	select {
	case code := <-finished:
		unlockTarget()
		t.Fatalf("start completed while same-target lock held: status=%d", code)
	case <-time.After(150 * time.Millisecond):
		// Still blocked — expected.
	}

	unlockTarget()

	select {
	case code := <-finished:
		if code != http.StatusOK {
			t.Fatalf("start status after target lock release = %d, want 200", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("start did not complete after target lock release")
	}

	if got := readTestFile(t, filepath.Join(fixtureA.WorktreePath, "dirty.txt")); got != "untracked\n" {
		t.Fatalf("dirty.txt after blocked start = %q, want preserved", got)
	}
}

// TestHandlerLoopTargetLockSharedAcrossPRLoopTypes ensures fixer and worker for
// the same PR share one target mutex so discard+retry cannot race another type
// on the shared looper-fix-<project>-pr-N worktree.
func TestHandlerLoopTargetLockSharedAcrossPRLoopTypes(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	h := NewHandler(Context{Config: cfg, Runtime: rt})
	services := rt.Services()
	nowISO := "2026-04-11T12:00:00.000Z"
	projectID := "project_retry_pr_type_lock"

	fixture := seedManagedWorktreeFixture(t, services.Repositories, managedWorktreeSeed{
		ProjectID: projectID,
		LoopID:    "loop_retry_pr_type_lock_fixer",
		LoopSeq:   3140,
		LoopType:  "fixer",
		Branch:    "feature/pr-type-lock",
		NowISO:    nowISO,
		Dirty:     true,
	})
	markLoopFailed(t, services.Repositories, fixture.LoopID, nowISO)

	// Failed worker on the same PR (different type — uniqueness allows this).
	repo := "acme/looper"
	prNumber := int64(42)
	prTarget := "pr:acme/looper:42"
	workerID := "loop_retry_pr_type_lock_worker"
	if err := services.Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{
		ID: workerID, Seq: 3141, ProjectID: projectID, Type: "worker",
		TargetType: "pull_request", TargetID: &prTarget, Repo: &repo, PRNumber: &prNumber,
		Status: "failed", CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert(worker) error = %v", err)
	}
	lastErrorKind := "manual_intervention"
	lastError := "prior failure"
	if err := services.Repositories.Queue.Upsert(context.Background(), storage.QueueItemRecord{
		ID: "queue_" + workerID, ProjectID: &projectID, LoopID: &workerID, Type: "worker",
		TargetType: "pull_request", TargetID: prTarget, Repo: &repo, PRNumber: &prNumber,
		DedupeKey: "worker:" + projectID + ":" + repo + ":42", Priority: storage.QueuePriorityWorker,
		Status: "failed", AvailableAt: nowISO, Attempts: 1, MaxAttempts: 3,
		LastError: &lastError, LastErrorKind: &lastErrorKind,
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Queue.Upsert(worker) error = %v", err)
	}

	// Hold fixer PR target lock as discard would; worker retry must block.
	target := mustLoopTargetFromFixture(t, services.Repositories, fixture.LoopID)
	unlockTarget := h.lockLoopTarget(projectID, domain.LoopTypeFixer, target)

	started := make(chan struct{})
	finished := make(chan int, 1)
	go func() {
		close(started)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/loops/3141/retry", strings.NewReader(`{"mode":"auto","resetAttempts":true}`))
		req.Header.Set("content-type", "application/json")
		recorder := httptest.NewRecorder()
		h.ServeHTTP(recorder, req)
		finished <- recorder.Code
	}()

	<-started
	select {
	case code := <-finished:
		unlockTarget()
		t.Fatalf("worker retry completed while fixer PR target lock held: status=%d", code)
	case <-time.After(150 * time.Millisecond):
		// Still blocked — expected shared PR worktree key.
	}

	unlockTarget()

	select {
	case code := <-finished:
		if code != http.StatusOK {
			t.Fatalf("worker retry status after PR target lock release = %d, want 200", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("worker retry did not complete after PR target lock release")
	}
}

// TestHandlerLockLoopTargetIsProcessWideWithRuntime ensures API lockLoopTarget
// and runtime LockLoopTarget share the same mutex entry for a PR target.
func TestHandlerLockLoopTargetIsProcessWideWithRuntime(t *testing.T) {
	h := NewHandler(Context{})
	projectID := "project_target_lock_process_wide"
	target := domain.LoopTarget{
		TargetType: domain.LoopTargetTypePullRequest,
		Repo:       "acme/looper",
		PRNumber:   42,
	}
	unlock := h.lockLoopTarget(projectID, domain.LoopTypeFixer, target)

	started := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		close(started)
		// Worker type, same PR — must wait on the shared PR key.
		key := looperdruntime.LoopTargetGuardKey(projectID, string(domain.LoopTypeWorker), string(domain.LoopTargetTypePullRequest), loopTargetKeyCompat(target))
		unlockRuntime := looperdruntime.LockLoopTarget(key)
		unlockRuntime()
		close(finished)
	}()

	<-started
	select {
	case <-finished:
		unlock()
		t.Fatal("runtime LockLoopTarget completed while Handler.lockLoopTarget held — PR keys not shared across types")
	case <-time.After(150 * time.Millisecond):
	}
	unlock()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("runtime LockLoopTarget did not complete after Handler.lockLoopTarget release")
	}
}

func markLoopFailed(t *testing.T, repos *storage.Repositories, loopID, nowISO string) {
	t.Helper()
	loop, err := repos.Loops.GetByID(context.Background(), loopID)
	if err != nil || loop == nil {
		t.Fatalf("Loops.GetByID(%s) = %#v, %v", loopID, loop, err)
	}
	loop.Status = "failed"
	loop.UpdatedAt = nowISO
	if err := repos.Loops.Upsert(context.Background(), *loop); err != nil {
		t.Fatalf("Loops.Upsert(failed %s) error = %v", loopID, err)
	}
}

// seedSecondFailedFixerSamePR inserts another failed fixer loop for PR #42 so
// two non-conflicting failed loops share one target key (fixture hardcodes 42).
func seedSecondFailedFixerSamePR(t *testing.T, repos *storage.Repositories, projectID, loopID string, seq int64, nowISO string) {
	t.Helper()
	repo := "acme/looper"
	prNumber := int64(42)
	prTarget := fmt.Sprintf("pr:%s:%d", repo, prNumber)
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{
		ID: loopID, Seq: seq, ProjectID: projectID, Type: "fixer",
		TargetType: "pull_request", TargetID: &prTarget, Repo: &repo, PRNumber: &prNumber,
		Status: "failed", CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert(%s) error = %v", loopID, err)
	}
	failedQueueID := "queue_" + loopID
	lastErrorKind := "manual_intervention"
	lastError := "prior failure"
	if err := repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{
		ID: failedQueueID, ProjectID: &projectID, LoopID: &loopID, Type: "fixer",
		TargetType: "pull_request", TargetID: prTarget, Repo: &repo, PRNumber: &prNumber,
		DedupeKey: "fixer:" + loopID, Priority: storage.QueuePriorityFixer,
		Status: "failed", AvailableAt: nowISO, Attempts: 1, MaxAttempts: 3,
		LastError: &lastError, LastErrorKind: &lastErrorKind,
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Queue.Upsert(%s) error = %v", failedQueueID, err)
	}
}

func mustLoopTargetFromFixture(t *testing.T, repos *storage.Repositories, loopID string) domain.LoopTarget {
	t.Helper()
	loop, err := repos.Loops.GetByID(context.Background(), loopID)
	if err != nil || loop == nil {
		t.Fatalf("GetByID(%s) = %#v, %v", loopID, loop, err)
	}
	target, err := loopTargetFromRecordCompat(*loop)
	if err != nil {
		t.Fatalf("loopTargetFromRecordCompat() error = %v", err)
	}
	return target
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
	if seed.LoopType == "planner" {
		queue.Priority = storage.QueuePriorityPlanner
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
