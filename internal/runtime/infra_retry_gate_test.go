package runtime

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	loopcondition "github.com/nexu-io/looper/internal/loops/condition"
	"github.com/nexu-io/looper/internal/storage"
)

func TestInfraRetryBudgetUsesQueueWallClock(t *testing.T) {
	now := time.Date(2026, time.July, 15, 12, 0, 0, 0, time.UTC)
	started := formatJavaScriptISOString(now.Add(-time.Hour))
	item := storage.QueueItemRecord{CreatedAt: formatJavaScriptISOString(now.Add(-2 * time.Hour)), StartedAt: &started}
	cfg := &config.Config{Scheduler: config.SchedulerConfig{InfraRetryBudgetSeconds: 3600}}
	if !infraRetryBudgetExceeded(item, cfg, now) {
		t.Fatal("infraRetryBudgetExceeded() = false at wall-clock budget")
	}
	if infraRetryBudgetExceeded(item, cfg, now.Add(-time.Millisecond)) {
		t.Fatal("infraRetryBudgetExceeded() = true before wall-clock budget")
	}
}

func TestRecoverableInfraCheapGateDelaysMissingAgentWithoutSpendingAttempt(t *testing.T) {
	repositories := newEnqueueTestRepos(t)
	ctx := context.Background()
	now := time.Date(2026, time.July, 15, 12, 0, 0, 0, time.UTC)
	item, cfg := seedRecoverableInfraQueue(t, repositories, now, now.Add(-time.Minute), filepath.Join(t.TempDir(), "missing-agent"))
	handled, err := gateRecoverableInfraRetry(ctx, item, defaultSchedulerTickInput{Config: cfg, Repos: repositories, Now: func() time.Time { return now }})
	if err != nil || !handled {
		t.Fatalf("gateRecoverableInfraRetry() = %v, %v; want handled", handled, err)
	}
	queued, err := repositories.Queue.GetByID(ctx, item.ID)
	if err != nil || queued == nil {
		t.Fatalf("Queue.GetByID() = %#v, %v", queued, err)
	}
	if queued.Status != "queued" || queued.Attempts != item.Attempts || queued.AvailableAt != "2026-07-15T12:00:30.000Z" {
		t.Fatalf("queue after cheap gate = %#v", queued)
	}
}

func TestRecoverableInfraCheapGateAllowsRetryAfterAgentReturns(t *testing.T) {
	repositories := newEnqueueTestRepos(t)
	now := time.Date(2026, time.July, 15, 12, 0, 0, 0, time.UTC)
	agentPath := filepath.Join(t.TempDir(), "agent")
	if err := os.WriteFile(agentPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write agent: %v", err)
	}
	item, cfg := seedRecoverableInfraQueue(t, repositories, now, now.Add(-time.Minute), agentPath)
	handled, err := gateRecoverableInfraRetry(context.Background(), item, defaultSchedulerTickInput{Config: cfg, Repos: repositories, Now: func() time.Time { return now }})
	if err != nil || handled {
		t.Fatalf("gateRecoverableInfraRetry() = %v, %v; want runner allowed", handled, err)
	}
}

func TestRecoverableInfraBudgetExhaustionBlocksLoopAndRaisesHealthSignal(t *testing.T) {
	repositories := newEnqueueTestRepos(t)
	ctx := context.Background()
	now := time.Date(2026, time.July, 15, 12, 0, 0, 0, time.UTC)
	item, cfg := seedRecoverableInfraQueue(t, repositories, now, now.Add(-2*time.Hour), filepath.Join(t.TempDir(), "missing-agent"))
	handled, err := gateRecoverableInfraRetry(ctx, item, defaultSchedulerTickInput{Config: cfg, Repos: repositories, Now: func() time.Time { return now }})
	if err != nil || !handled {
		t.Fatalf("gateRecoverableInfraRetry() = %v, %v; want blocked", handled, err)
	}
	queue, err := repositories.Queue.GetByID(ctx, item.ID)
	if err != nil || queue == nil || queue.Status != "manual_intervention" || queue.LastErrorKind == nil || *queue.LastErrorKind != "recoverable_infra" {
		t.Fatalf("queue after budget exhaustion = %#v, %v", queue, err)
	}
	loop, err := repositories.Loops.GetByID(ctx, *item.LoopID)
	if err != nil || loop == nil || loop.Status != "paused" {
		t.Fatalf("loop after budget exhaustion = %#v, %v", loop, err)
	}
	condition, ok := loopcondition.Read(loop.MetadataJSON)
	if !ok || condition.Kind != loopcondition.InfraRecovered || condition.Fingerprint == "" {
		t.Fatalf("blocked condition = %#v, %v", condition, ok)
	}
	blocked, err := repositories.Queue.CountBlockedInfra(ctx)
	if err != nil || blocked != 1 {
		t.Fatalf("CountBlockedInfra() = %d, %v; want 1", blocked, err)
	}
}

func seedRecoverableInfraQueue(t *testing.T, repositories *storage.Repositories, now, started time.Time, agentPath string) (storage.QueueItemRecord, *config.Config) {
	t.Helper()
	ctx := context.Background()
	nowISO := formatJavaScriptISOString(now)
	startedISO := formatJavaScriptISOString(started)
	projectID := "project_1"
	loopID := "loop_infra"
	targetID := "project:project_1"
	if err := repositories.Projects.Upsert(ctx, storage.ProjectRecord{ID: projectID, Name: "Project", RepoPath: t.TempDir(), CreatedAt: startedISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := repositories.Loops.Upsert(ctx, storage.LoopRecord{ID: loopID, Seq: 1, ProjectID: projectID, Type: "worker", TargetType: "project", TargetID: &targetID, Status: "running", CreatedAt: startedISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	errorKind := "recoverable_infra"
	errorMessage := "start agent command: fork/exec " + agentPath + ": no such file or directory"
	item := storage.QueueItemRecord{ID: "queue_infra", ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: targetID, DedupeKey: "worker:loop_infra", Priority: storage.QueuePriorityWorker, Status: "running", AvailableAt: startedISO, Attempts: 3, MaxAttempts: -1, StartedAt: &startedISO, LastError: &errorMessage, LastErrorKind: &errorKind, CreatedAt: startedISO, UpdatedAt: nowISO}
	if err := repositories.Queue.Upsert(ctx, item); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}
	vendor := config.AgentVendorCodex
	cfg := &config.Config{Agent: config.AgentConfig{Vendor: &vendor, Params: map[string]any{"command": agentPath}}, Scheduler: config.SchedulerConfig{RetryBaseDelayMS: 5000, InfraRetryBudgetSeconds: 3600}}
	return item, cfg
}
