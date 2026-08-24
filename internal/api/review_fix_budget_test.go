package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
)

func TestHandlerLoopStartDelegatesNoHITLBudgetHoldToPairedContinue(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	cfg.HITL.Enabled = false
	cfg.Roles.Reviewer.Behavior.Loop.MaxPublishesPerPR = 3
	cfg.Roles.Fixer.Behavior.Loop.MaxPushesPerPR = 3
	h := NewHandler(Context{Config: cfg, Runtime: rt})
	services := rt.Services()
	nowISO := "2026-04-11T12:00:00.000Z"
	projectID := "project_budget_unpause"
	repo := "acme/looper"
	pr := int64(42)
	target := "pr:acme/looper:42"
	if err := services.Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID: projectID, Name: "Looper", RepoPath: t.TempDir(), CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	reviewerMeta := `{"loop":{"iterationCount":3}}`
	reviewer := storage.LoopRecord{
		ID: "loop_budget_unpause_rev", Seq: 4201, ProjectID: projectID, Type: "reviewer",
		TargetType: "pull_request", TargetID: &target, Repo: &repo, PRNumber: &pr,
		Status: "running", MetadataJSON: &reviewerMeta, CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	fixerMeta := `{"reviewFixBudget":{"pushCount":1}}`
	fixer := storage.LoopRecord{
		ID: "loop_budget_unpause_fix", Seq: 4202, ProjectID: projectID, Type: "fixer",
		TargetType: "pull_request", TargetID: &target, Repo: &repo, PRNumber: &pr,
		Status: "queued", MetadataJSON: &fixerMeta, CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	if err := services.Repositories.Loops.Upsert(context.Background(), reviewer); err != nil {
		t.Fatalf("Upsert reviewer: %v", err)
	}
	if err := services.Repositories.Loops.Upsert(context.Background(), fixer); err != nil {
		t.Fatalf("Upsert fixer: %v", err)
	}
	parked, err := loops.ParkReviewFixBudget(context.Background(), services.Repositories, loops.ParkReviewFixBudgetInput{
		Exhausted: reviewer, Role: "reviewer", Repo: repo, PRNumber: pr, Count: 3, Cap: 3, NowISO: nowISO, HITLEnabled: false,
	})
	if err != nil {
		t.Fatalf("ParkReviewFixBudget: %v", err)
	}
	if parked.Status != "paused" {
		t.Fatalf("parked status = %q, want paused", parked.Status)
	}

	// Unpause from sibling side.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/loops/"+fixer.ID+"/start", nil)
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("start status = %d body=%s", recorder.Code, recorder.Body.String())
	}

	reviewerAfter, err := services.Repositories.Loops.GetByID(context.Background(), reviewer.ID)
	if err != nil || reviewerAfter == nil || reviewerAfter.Status != "queued" || loops.ReviewerPublishCount(reviewerAfter.MetadataJSON) != 0 {
		t.Fatalf("reviewer after unpause = (%#v, %v), want queued with reset meter", reviewerAfter, err)
	}
	fixerAfter, err := services.Repositories.Loops.GetByID(context.Background(), fixer.ID)
	if err != nil || fixerAfter == nil || fixerAfter.Status != "queued" || loops.FixerPushCount(fixerAfter.MetadataJSON) != 1 {
		t.Fatalf("fixer after unpause = (%#v, %v), want queued with preserved unused budget", fixerAfter, err)
	}
	if loops.IsReviewFixBudgetHold(*reviewerAfter) || loops.IsReviewFixBudgetHold(*fixerAfter) {
		t.Fatal("pair still budget-held after unpause")
	}
}

func TestHandlerActiveStopDelegatesBudgetHoldToPairedStop(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	cfg.HITL.Enabled = false
	h := NewHandler(Context{
		Config:  cfg,
		Runtime: rt,
		StopLoop: func(context.Context, string, string) (any, error) {
			t.Fatal("generic StopLoop must not run for budget-held pair")
			return nil, nil
		},
	})
	services := rt.Services()
	nowISO := "2026-04-11T12:00:00.000Z"
	projectID := "project_budget_stop"
	repo := "acme/looper"
	pr := int64(43)
	target := "pr:acme/looper:43"
	if err := services.Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID: projectID, Name: "Looper", RepoPath: t.TempDir(), CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	reviewerMeta := `{"loop":{"iterationCount":3}}`
	reviewer := storage.LoopRecord{
		ID: "loop_budget_stop_rev", Seq: 4301, ProjectID: projectID, Type: "reviewer",
		TargetType: "pull_request", TargetID: &target, Repo: &repo, PRNumber: &pr,
		Status: "running", MetadataJSON: &reviewerMeta, CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	fixer := storage.LoopRecord{
		ID: "loop_budget_stop_fix", Seq: 4302, ProjectID: projectID, Type: "fixer",
		TargetType: "pull_request", TargetID: &target, Repo: &repo, PRNumber: &pr,
		Status: "queued", CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	if err := services.Repositories.Loops.Upsert(context.Background(), reviewer); err != nil {
		t.Fatalf("Upsert reviewer: %v", err)
	}
	if err := services.Repositories.Loops.Upsert(context.Background(), fixer); err != nil {
		t.Fatalf("Upsert fixer: %v", err)
	}
	if _, err := loops.ParkReviewFixBudget(context.Background(), services.Repositories, loops.ParkReviewFixBudgetInput{
		Exhausted: reviewer, Role: "reviewer", Repo: repo, PRNumber: pr, Count: 3, Cap: 3, NowISO: nowISO, HITLEnabled: false,
	}); err != nil {
		t.Fatalf("ParkReviewFixBudget: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/runs/active/"+fixer.ID+"/stop", nil)
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("stop status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var envelope struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode: %v body=%s", err, recorder.Body.String())
	}
	if envelope.Data["outcome"] != "review_fix_budget_stop" {
		t.Fatalf("outcome = %#v, want review_fix_budget_stop", envelope.Data)
	}
	reviewerAfter, _ := services.Repositories.Loops.GetByID(context.Background(), reviewer.ID)
	fixerAfter, _ := services.Repositories.Loops.GetByID(context.Background(), fixer.ID)
	if reviewerAfter == nil || reviewerAfter.Status != "terminated" || fixerAfter == nil || fixerAfter.Status != "terminated" {
		t.Fatalf("pair after stop = reviewer %#v fixer %#v, want both terminated", reviewerAfter, fixerAfter)
	}
}

func TestHandlerRetryRejectsReviewFixBudgetHold(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	cfg.HITL.Enabled = false
	h := NewHandler(Context{Config: cfg, Runtime: rt})
	services := rt.Services()
	nowISO := "2026-04-11T12:00:00.000Z"
	projectID := "project_budget_retry"
	repo := "acme/looper"
	pr := int64(44)
	target := "pr:acme/looper:44"
	if err := services.Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID: projectID, Name: "Looper", RepoPath: t.TempDir(), CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	reviewerMeta := `{"loop":{"iterationCount":3}}`
	reviewer := storage.LoopRecord{
		ID: "loop_budget_retry_rev", Seq: 4401, ProjectID: projectID, Type: "reviewer",
		TargetType: "pull_request", TargetID: &target, Repo: &repo, PRNumber: &pr,
		Status: "running", MetadataJSON: &reviewerMeta, CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	fixer := storage.LoopRecord{
		ID: "loop_budget_retry_fix", Seq: 4402, ProjectID: projectID, Type: "fixer",
		TargetType: "pull_request", TargetID: &target, Repo: &repo, PRNumber: &pr,
		Status: "queued", CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	if err := services.Repositories.Loops.Upsert(context.Background(), reviewer); err != nil {
		t.Fatalf("Upsert reviewer: %v", err)
	}
	if err := services.Repositories.Loops.Upsert(context.Background(), fixer); err != nil {
		t.Fatalf("Upsert fixer: %v", err)
	}
	if _, err := loops.ParkReviewFixBudget(context.Background(), services.Repositories, loops.ParkReviewFixBudgetInput{
		Exhausted: reviewer, Role: "reviewer", Repo: repo, PRNumber: pr, Count: 3, Cap: 3, NowISO: nowISO, HITLEnabled: false,
	}); err != nil {
		t.Fatalf("ParkReviewFixBudget: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/loops/4401/retry", strings.NewReader(`{"mode":"auto","resetAttempts":true}`))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("retry status = %d body=%s, want 400", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "looper unpause") && !strings.Contains(recorder.Body.String(), "budget") {
		t.Fatalf("retry body = %s, want budget hold guidance", recorder.Body.String())
	}
	// Counters and hold must be unchanged.
	after, _ := services.Repositories.Loops.GetByID(context.Background(), reviewer.ID)
	if after == nil || !loops.IsReviewFixBudgetHold(*after) || loops.ReviewerPublishCount(after.MetadataJSON) != 3 {
		t.Fatalf("after rejected retry = %#v, want still held with count 3", after)
	}
}
