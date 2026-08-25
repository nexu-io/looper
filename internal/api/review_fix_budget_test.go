package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/domain"
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

func TestHandlerBudgetContinueWaitsForSameTargetLock(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	cfg.HITL.Enabled = false
	cfg.Roles.Reviewer.Behavior.Loop.MaxPublishesPerPR = 3
	cfg.Roles.Fixer.Behavior.Loop.MaxPushesPerPR = 3
	h := NewHandler(Context{Config: cfg, Runtime: rt})
	services := rt.Services()
	nowISO := "2026-04-11T12:00:00.000Z"
	projectID := "project_budget_unpause_lock"
	repo := "acme/looper"
	pr := int64(45)
	targetID := "pr:acme/looper:45"
	if err := services.Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID: projectID, Name: "Looper", RepoPath: t.TempDir(), CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	reviewerMeta := `{"loop":{"iterationCount":3}}`
	reviewer := storage.LoopRecord{
		ID: "loop_budget_lock_rev", Seq: 4501, ProjectID: projectID, Type: "reviewer",
		TargetType: "pull_request", TargetID: &targetID, Repo: &repo, PRNumber: &pr,
		Status: "running", MetadataJSON: &reviewerMeta, CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	fixerMeta := `{"reviewFixBudget":{"pushCount":1}}`
	fixer := storage.LoopRecord{
		ID: "loop_budget_lock_fix", Seq: 4502, ProjectID: projectID, Type: "fixer",
		TargetType: "pull_request", TargetID: &targetID, Repo: &repo, PRNumber: &pr,
		Status: "queued", MetadataJSON: &fixerMeta, CreatedAt: nowISO, UpdatedAt: nowISO,
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

	target, err := loopTargetFromRecordCompat(fixer)
	if err != nil {
		t.Fatalf("loopTargetFromRecordCompat: %v", err)
	}
	unlockTarget := h.lockLoopTarget(projectID, domain.LoopTypeFixer, target)

	started := make(chan struct{})
	finished := make(chan int, 1)
	go func() {
		close(started)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/loops/"+fixer.ID+"/start", nil)
		recorder := httptest.NewRecorder()
		h.ServeHTTP(recorder, req)
		finished <- recorder.Code
	}()
	<-started
	select {
	case code := <-finished:
		unlockTarget()
		t.Fatalf("budget continue completed while target lock held: status=%d", code)
	case <-time.After(150 * time.Millisecond):
	}

	reviewerHeld, _ := services.Repositories.Loops.GetByID(context.Background(), reviewer.ID)
	fixerHeld, _ := services.Repositories.Loops.GetByID(context.Background(), fixer.ID)
	if reviewerHeld == nil || !loops.IsReviewFixBudgetHold(*reviewerHeld) || fixerHeld == nil || !loops.IsReviewFixBudgetHold(*fixerHeld) {
		unlockTarget()
		t.Fatalf("pair released while target lock held: reviewer %#v fixer %#v", reviewerHeld, fixerHeld)
	}

	unlockTarget()
	select {
	case code := <-finished:
		if code != http.StatusOK {
			t.Fatalf("start status after lock release = %d, want 200", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("budget continue did not complete after target lock release")
	}
	reviewerAfter, _ := services.Repositories.Loops.GetByID(context.Background(), reviewer.ID)
	fixerAfter, _ := services.Repositories.Loops.GetByID(context.Background(), fixer.ID)
	if reviewerAfter == nil || reviewerAfter.Status != "queued" || fixerAfter == nil || fixerAfter.Status != "queued" {
		t.Fatalf("pair after locked continue = reviewer %#v fixer %#v, want queued", reviewerAfter, fixerAfter)
	}
}

func TestHandlerActiveStopDelegatesBudgetHoldToPairedStop(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	cfg.HITL.Enabled = false
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
	if len(drained) != 2 {
		t.Fatalf("StopLoop calls = %v, want both pair members before terminate", drained)
	}
	seen := map[string]bool{drained[0]: true, drained[1]: true}
	if !seen[reviewer.ID] || !seen[fixer.ID] {
		t.Fatalf("drained = %v, want %s and %s", drained, reviewer.ID, fixer.ID)
	}
	if services.ActiveExecutions != nil && (!services.ActiveExecutions.LoopStopActive(reviewer.ID) || !services.ActiveExecutions.LoopStopActive(fixer.ID)) {
		t.Fatal("pair stop gates not closed after drain")
	}
}

func TestHandlerActiveStopDrainsScopeHeldSibling(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	cfg.HITL.Enabled = false
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
	projectID := "project_scope_stop"
	repo := "acme/looper"
	pr := int64(46)
	target := "pr:acme/looper:46"
	if err := services.Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID: projectID, Name: "Looper", RepoPath: t.TempDir(), CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	reviewer := storage.LoopRecord{
		ID: "loop_scope_stop_rev", Seq: 4601, ProjectID: projectID, Type: "reviewer",
		TargetType: "pull_request", TargetID: &target, Repo: &repo, PRNumber: &pr,
		Status: "running", CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	fixer := storage.LoopRecord{
		ID: "loop_scope_stop_fix", Seq: 4602, ProjectID: projectID, Type: "fixer",
		TargetType: "pull_request", TargetID: &target, Repo: &repo, PRNumber: &pr,
		Status: "running", CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	if err := services.Repositories.Loops.Upsert(context.Background(), reviewer); err != nil {
		t.Fatalf("Upsert reviewer: %v", err)
	}
	if err := services.Repositories.Loops.Upsert(context.Background(), fixer); err != nil {
		t.Fatalf("Upsert fixer: %v", err)
	}
	if _, err := loops.ParkReviewScopeHuman(context.Background(), services.Repositories, loops.ParkReviewScopeHumanInput{
		Held: reviewer, Role: "reviewer", Repo: repo, PRNumber: pr, NowISO: nowISO, HITLEnabled: false,
		Question: "Clarify AGENTS.md rule X before unpause",
	}); err != nil {
		t.Fatalf("ParkReviewScopeHuman: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/runs/active/"+reviewer.ID+"/stop", nil)
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
	if envelope.Data["outcome"] != "review_scope_human_stop" {
		t.Fatalf("outcome = %#v, want review_scope_human_stop", envelope.Data)
	}
	reviewerAfter, _ := services.Repositories.Loops.GetByID(context.Background(), reviewer.ID)
	fixerAfter, _ := services.Repositories.Loops.GetByID(context.Background(), fixer.ID)
	if reviewerAfter == nil || reviewerAfter.Status != "terminated" || fixerAfter == nil || fixerAfter.Status != "terminated" {
		t.Fatalf("pair after stop = reviewer %#v fixer %#v, want both terminated", reviewerAfter, fixerAfter)
	}
	if len(drained) != 2 {
		t.Fatalf("StopLoop calls = %v, want both scope-held pair members before terminate", drained)
	}
	seen := map[string]bool{drained[0]: true, drained[1]: true}
	if !seen[reviewer.ID] || !seen[fixer.ID] {
		t.Fatalf("drained = %v, want %s and %s", drained, reviewer.ID, fixer.ID)
	}
}

func TestHandlerNoHITLBudgetContinueNotifiesPromotedScopeHold(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	cfg.HITL.Enabled = false
	cfg.Roles.Reviewer.Behavior.Loop.MaxPublishesPerPR = 3
	cfg.Roles.Fixer.Behavior.Loop.MaxPushesPerPR = 3
	var notified []string
	h := NewHandler(Context{
		Config:  cfg,
		Runtime: rt,
		NotifyHumanAttention: func(_ context.Context, loopID string) {
			notified = append(notified, loopID)
		},
	})
	services := rt.Services()
	nowISO := "2026-04-11T12:00:00.000Z"
	projectID := "project_budget_promote_notify"
	repo := "acme/looper"
	pr := int64(47)
	target := "pr:acme/looper:47"
	if err := services.Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID: projectID, Name: "Looper", RepoPath: t.TempDir(), CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	reviewerMeta := `{"loop":{"iterationCount":3}}`
	reviewer := storage.LoopRecord{
		ID: "loop_budget_promote_notify_rev", Seq: 4701, ProjectID: projectID, Type: "reviewer",
		TargetType: "pull_request", TargetID: &target, Repo: &repo, PRNumber: &pr,
		Status: "running", MetadataJSON: &reviewerMeta, CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	fixer := storage.LoopRecord{
		ID: "loop_budget_promote_notify_fix", Seq: 4702, ProjectID: projectID, Type: "fixer",
		TargetType: "pull_request", TargetID: &target, Repo: &repo, PRNumber: &pr,
		Status: "queued", CreatedAt: nowISO, UpdatedAt: nowISO,
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
	encoded, err := loops.PersistPendingReviewScopeHumanEvidence(parked.MetadataJSON, "Clarify AGENTS.md rule X before unpause", "PR non-goals exclude API expansion", false)
	if err != nil {
		t.Fatalf("PersistPendingReviewScopeHumanEvidence: %v", err)
	}
	parked.MetadataJSON = &encoded
	if err := services.Repositories.Loops.Upsert(context.Background(), parked); err != nil {
		t.Fatalf("Loops.Upsert pending: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/loops/"+fixer.ID+"/start", nil)
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("start status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	reviewerAfter, err := services.Repositories.Loops.GetByID(context.Background(), reviewer.ID)
	if err != nil || reviewerAfter == nil || !loops.IsReviewScopeHumanHold(*reviewerAfter) || loops.IsReviewFixBudgetHold(*reviewerAfter) {
		t.Fatalf("reviewer after promote = (%#v, %v), want no-HITL scope hold", reviewerAfter, err)
	}
	if reviewerAfter.Status != "paused" {
		t.Fatalf("reviewer status = %q, want paused", reviewerAfter.Status)
	}
	seen := map[string]bool{}
	for _, id := range notified {
		seen[id] = true
	}
	if !seen[reviewer.ID] {
		t.Fatalf("NotifyHumanAttention = %v, want promoted scope hold %s", notified, reviewer.ID)
	}
}

func TestHandlerNoHITLBudgetContinueNotifiesPrimaryWhenSiblingListFails(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	cfg.HITL.Enabled = false
	cfg.Roles.Reviewer.Behavior.Loop.MaxPublishesPerPR = 3
	cfg.Roles.Fixer.Behavior.Loop.MaxPushesPerPR = 3
	var notified []string
	services := rt.Services()
	services.Repositories = storage.NewRepositories(errorInjectingQuerier{
		db: services.Coordinator.DB(),
		queryError: func(query string) error {
			if strings.Contains(query, "SELECT * FROM loops") && !strings.Contains(query, "WHERE") {
				return errors.New("database is locked")
			}
			return nil
		},
	})
	h := NewHandler(Context{
		Config:  cfg,
		Runtime: fixedRuntimeState{services: services},
		NotifyHumanAttention: func(_ context.Context, loopID string) {
			notified = append(notified, loopID)
		},
	})
	nowISO := "2026-04-11T12:00:00.000Z"
	projectID := "project_budget_promote_list_fail"
	repo := "acme/looper"
	pr := int64(48)
	target := "pr:acme/looper:48"
	if err := rt.Services().Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID: projectID, Name: "Looper", RepoPath: t.TempDir(), CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	reviewerMeta := `{"loop":{"iterationCount":3}}`
	reviewer := storage.LoopRecord{
		ID: "loop_budget_promote_list_fail_rev", Seq: 4801, ProjectID: projectID, Type: "reviewer",
		TargetType: "pull_request", TargetID: &target, Repo: &repo, PRNumber: &pr,
		Status: "running", MetadataJSON: &reviewerMeta, CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	fixer := storage.LoopRecord{
		ID: "loop_budget_promote_list_fail_fix", Seq: 4802, ProjectID: projectID, Type: "fixer",
		TargetType: "pull_request", TargetID: &target, Repo: &repo, PRNumber: &pr,
		Status: "queued", CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	if err := rt.Services().Repositories.Loops.Upsert(context.Background(), reviewer); err != nil {
		t.Fatalf("Upsert reviewer: %v", err)
	}
	if err := rt.Services().Repositories.Loops.Upsert(context.Background(), fixer); err != nil {
		t.Fatalf("Upsert fixer: %v", err)
	}
	parked, err := loops.ParkReviewFixBudget(context.Background(), rt.Services().Repositories, loops.ParkReviewFixBudgetInput{
		Exhausted: reviewer, Role: "reviewer", Repo: repo, PRNumber: pr, Count: 3, Cap: 3, NowISO: nowISO, HITLEnabled: false,
	})
	if err != nil {
		t.Fatalf("ParkReviewFixBudget: %v", err)
	}
	encoded, err := loops.PersistPendingReviewScopeHumanEvidence(parked.MetadataJSON, "Clarify AGENTS.md rule X before unpause", "PR non-goals exclude API expansion", false)
	if err != nil {
		t.Fatalf("PersistPendingReviewScopeHumanEvidence: %v", err)
	}
	parked.MetadataJSON = &encoded
	if err := rt.Services().Repositories.Loops.Upsert(context.Background(), parked); err != nil {
		t.Fatalf("Loops.Upsert pending: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/loops/"+fixer.ID+"/start", nil)
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("start status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	reviewerAfter, err := rt.Services().Repositories.Loops.GetByID(context.Background(), reviewer.ID)
	if err != nil || reviewerAfter == nil || !loops.IsReviewScopeHumanHold(*reviewerAfter) || loops.IsReviewFixBudgetHold(*reviewerAfter) {
		t.Fatalf("reviewer after promote = (%#v, %v), want no-HITL scope hold", reviewerAfter, err)
	}
	fixerAfter, err := rt.Services().Repositories.Loops.GetByID(context.Background(), fixer.ID)
	if err != nil || fixerAfter == nil || !loops.IsSiblingReviewScopeHumanPause(fixerAfter.MetadataJSON) {
		t.Fatalf("fixer after promote = (%#v, %v), want sibling-only pause", fixerAfter, err)
	}
	if len(notified) != 1 || notified[0] != reviewer.ID {
		t.Fatalf("NotifyHumanAttention = %v, want primary scope hold %s after sibling List failure", notified, reviewer.ID)
	}
}

func TestHandlerActiveStopDoesNotTerminateHistoricalSibling(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	cfg.HITL.Enabled = false
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
			if rec.Status == "terminated" || rec.Status == "stopped" || rec.Status == "completed" {
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
	projectID := "project_budget_stop_hist"
	repo := "acme/looper"
	pr := int64(44)
	target := "pr:acme/looper:44"
	if err := services.Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID: projectID, Name: "Looper", RepoPath: t.TempDir(), CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	reviewerMeta := `{"manual":true,"followUpdates":true,"loop":{"iterationCount":3}}`
	reviewer := storage.LoopRecord{
		ID: "loop_budget_stop_hist_rev", Seq: 4401, ProjectID: projectID, Type: "reviewer",
		TargetType: "pull_request", TargetID: &target, Repo: &repo, PRNumber: &pr,
		Status: "running", MetadataJSON: &reviewerMeta, CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	fixerMeta := `{"manual":true,"followUpdates":true,"reviewFixBudget":{"pushCount":1}}`
	fixer := storage.LoopRecord{
		ID: "loop_budget_stop_hist_fix", Seq: 4402, ProjectID: projectID, Type: "fixer",
		TargetType: "pull_request", TargetID: &target, Repo: &repo, PRNumber: &pr,
		Status: "queued", MetadataJSON: &fixerMeta, CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	historicalMeta := `{"manual":true,"followUpdates":true,"reviewFixBudget":{"pushCount":3,"exhaustedBy":"fixer"}}`
	historical := storage.LoopRecord{
		ID: "loop_budget_stop_hist_old", Seq: 4400, ProjectID: projectID, Type: "fixer",
		TargetType: "pull_request", TargetID: &target, Repo: &repo, PRNumber: &pr,
		Status: "completed", MetadataJSON: &historicalMeta, CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	if err := services.Repositories.Loops.Upsert(context.Background(), reviewer); err != nil {
		t.Fatalf("Upsert reviewer: %v", err)
	}
	if err := services.Repositories.Loops.Upsert(context.Background(), fixer); err != nil {
		t.Fatalf("Upsert fixer: %v", err)
	}
	if err := services.Repositories.Loops.Upsert(context.Background(), historical); err != nil {
		t.Fatalf("Upsert historical fixer: %v", err)
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

	reviewerAfter, _ := services.Repositories.Loops.GetByID(context.Background(), reviewer.ID)
	fixerAfter, _ := services.Repositories.Loops.GetByID(context.Background(), fixer.ID)
	histAfter, _ := services.Repositories.Loops.GetByID(context.Background(), historical.ID)
	if reviewerAfter == nil || reviewerAfter.Status != "terminated" || fixerAfter == nil || fixerAfter.Status != "terminated" {
		t.Fatalf("held pair after stop = reviewer %#v fixer %#v, want both terminated", reviewerAfter, fixerAfter)
	}
	if histAfter == nil || histAfter.Status != "completed" || loops.FixerPushCount(histAfter.MetadataJSON) != 3 {
		t.Fatalf("historical fixer = %#v, want completed with original pushCount 3", histAfter)
	}
	if histAfter.MetadataJSON == nil || *histAfter.MetadataJSON != historicalMeta {
		t.Fatalf("historical metadata = %v, want unchanged", histAfter.MetadataJSON)
	}
	if len(drained) != 2 {
		t.Fatalf("StopLoop calls = %v, want only held pair members", drained)
	}
	seen := map[string]bool{drained[0]: true, drained[1]: true}
	if !seen[reviewer.ID] || !seen[fixer.ID] || seen[historical.ID] {
		t.Fatalf("drained = %v, want %s and %s only", drained, reviewer.ID, fixer.ID)
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
