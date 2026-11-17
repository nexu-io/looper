package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	looperdapi "github.com/nexu-io/looper/internal/api"
	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/loops"
	looperdruntime "github.com/nexu-io/looper/internal/runtime"
	"github.com/nexu-io/looper/internal/storage"
)

func TestGroupedReviewerTakeoverAndHandbackUseMainSession(t *testing.T) {
	for _, tc := range []struct {
		groupStatus string
		hasMain     bool
	}{{"running", true}, {"completed", true}, {"running", false}} {
		t.Run(fmt.Sprintf("%s_main_%t", tc.groupStatus, tc.hasMain), func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			cfg, err := config.DefaultConfig(root)
			if err != nil {
				t.Fatal(err)
			}
			cfg.Storage.DBPath = filepath.Join(root, "looper.sqlite")
			cfg.Storage.BackupDir = stringPtr(filepath.Join(root, "backups"))
			cfg.Daemon.LogDir = filepath.Join(root, "logs")
			vendor := config.AgentVendorCodex
			cfg.Agent.Vendor = &vendor
			now := time.Date(2026, time.April, 21, 12, 0, 0, 0, time.UTC)
			rt := looperdruntime.New(looperdruntime.Options{Config: cfg, Now: func() time.Time { return now }, RunSchedulerTick: func(context.Context, looperdruntime.Services) error { return nil }})
			if err := rt.Start(ctx); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { rt.Stop("test cleanup") })
			for _, ready := range []func(context.Context) error{rt.CompleteStartup, rt.WaitForDeferredReviewerRecovery, rt.WaitForHumanAttentionRecoveryNotify} {
				if err := ready(ctx); err != nil {
					t.Fatal(err)
				}
			}
			services := rt.Services()
			repos := services.Repositories
			stamp := now.Format("2006-01-02T15:04:05.000Z")
			if err := repos.Projects.Upsert(ctx, storage.ProjectRecord{ID: "project_1", Name: "Looper", RepoPath: root, CreatedAt: stamp, UpdatedAt: stamp}); err != nil {
				t.Fatal(err)
			}
			insertStopAllTestLoop(t, ctx, repos, now, stopAllLoopFixture{loopID: "review", seq: 1, loopType: "reviewer", loopStatus: "running", runID: "run", runStatus: "running"})
			loop, err := repos.Loops.GetByID(ctx, "review")
			if err != nil || loop == nil {
				t.Fatalf("loop: %v, %v", loop, err)
			}
			prNumber := int64(42)
			loop.TargetType, loop.TargetID, loop.Repo, loop.PRNumber = "pull_request", stringPtr("pr:acme/looper:42"), stringPtr("acme/looper"), &prNumber
			if err := repos.Loops.Upsert(ctx, *loop); err != nil {
				t.Fatal(err)
			}
			for i, phase := range []string{"review", "review-group"} {
				if phase == "review" && !tc.hasMain {
					continue
				}
				execution := storage.AgentExecutionRecord{ID: phase, ProjectID: &loop.ProjectID, LoopID: &loop.ID, RunID: stringPtr("run"), Vendor: "codex", Status: "completed", NativeSessionID: stringPtr(phase + "-session"), CWD: &root, MetadataJSON: stringPtr(fmt.Sprintf(`{"metadata":{"phase":%q}}`, phase)), StartedAt: now.Add(time.Duration(i) * time.Second).Format("2006-01-02T15:04:05.000Z"), CreatedAt: stamp, UpdatedAt: stamp}
				if phase == "review-group" {
					execution.Status = tc.groupStatus
				}
				if err := repos.AgentExecutions.Upsert(ctx, execution); err != nil {
					t.Fatal(err)
				}
			}
			active := &fakeActiveExecution{}
			if tc.groupStatus == "running" {
				unregister := services.ActiveExecutions.Register(loop.ID, "run", "review-group", active)
				defer unregister()
			}
			result, err := takeoverLoop(ctx, services, loop.ID, "human takeover", func() time.Time { return now }, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			wantSession := ""
			if tc.hasMain {
				wantSession = "review-session"
			}
			if result.SessionID != wantSession {
				t.Errorf("takeover session=%q, want %q", result.SessionID, wantSession)
			}
			if tc.groupStatus == "running" && !active.killed {
				t.Error("takeover did not stop the active group")
			}
			// The stopped scheduler run drains before the human hands control back.
			run, err := repos.Runs.GetByID(ctx, "run")
			if err != nil || run == nil {
				t.Fatalf("stopped run: %v, %v", run, err)
			}
			run.Status, run.EndedAt = "failed", &stamp
			if err := repos.Runs.Upsert(ctx, *run); err != nil {
				t.Fatal(err)
			}
			handler := looperdapi.NewHandler(looperdapi.Context{Config: cfg, Runtime: rt})
			recorder := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/v1/loops/1/handback", strings.NewReader(`{"mode":"auto"}`))
			req.RemoteAddr = "127.0.0.1:12345"
			handler.ServeHTTP(recorder, req)
			if recorder.Code != http.StatusOK {
				t.Fatalf("handback: status=%d, body=%s", recorder.Code, recorder.Body.String())
			}
			loop, err = repos.Loops.GetByID(ctx, loop.ID)
			if err != nil || loop == nil {
				t.Fatalf("handed-back loop: %v, %v", loop, err)
			}
			resume, present := loops.ReadTakeoverResume(loop.MetadataJSON)
			if resume.SessionID != wantSession || present != tc.hasMain || loop.Status != "queued" {
				t.Errorf("handback resume=%+v, present=%t, status=%s; want main session %q", resume, present, loop.Status, wantSession)
			}
		})
	}
}
