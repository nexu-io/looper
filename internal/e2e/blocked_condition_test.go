package e2e

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/e2e/harness"
	"github.com/nexu-io/looper/internal/loops"
	loopcondition "github.com/nexu-io/looper/internal/loops/condition"
	loopengine "github.com/nexu-io/looper/internal/loops/engine"
	"github.com/nexu-io/looper/internal/storage"
)

func TestBlockedConditionAnsweredWhileDaemonDownResumesOnBoot(t *testing.T) {
	bins := harness.MustBinaries(t)
	home := harness.NewTempHome(t)
	repo := harness.CreateSeededRepo(t, "git")
	port := harness.MustFreePort(t)
	fakeAgent := harness.NewFakeAgent(t, bins)
	fakeGH := harness.NewFakeGH(t, bins, harness.GHSchema{JSONFieldAllowlist: map[string][]string{}})
	cfg := configWithFakeTools(t, bins, home, repo, fakeGH, fakeAgent, port)
	cfg.Scheduler.PollIntervalSeconds = 10
	harness.WriteConfig(t, home.ConfigPath, cfg, nil)

	coordinator, err := storage.OpenSQLiteCoordinator(context.Background(), home.DBPath, storage.SQLiteCoordinatorOptions{BackupDir: home.BackupDir})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	if _, err := coordinator.MigrationRunner().RunPending(context.Background()); err != nil {
		t.Fatalf("RunPending() error = %v", err)
	}
	repositories := storage.NewRepositories(coordinator.DB())
	nowISO := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	projectID := "project_1"
	loopID := "loop_answered_while_down"
	targetID := "project:project_1"
	if err := repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Looper E2E", RepoPath: repo.Path, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	metadata, err := loops.WriteHITLAsk(nil, loops.HITLAsk{Question: "Which option?", Answer: "A", Status: "answered", AskedAt: nowISO, AnsweredAt: nowISO})
	if err != nil {
		t.Fatalf("WriteHITLAsk() error = %v", err)
	}
	metadata, err = loopcondition.Set(&metadata, loopcondition.Record{Kind: loopcondition.HumanAnswered, Since: nowISO})
	if err != nil {
		t.Fatalf("condition.Set() error = %v", err)
	}
	if err := repositories.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 1, ProjectID: projectID, Type: "worker", TargetType: "project", TargetID: &targetID, Status: "awaiting_human", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	cancelReason := "daemon stopped while awaiting answer"
	if err := repositories.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: "queue_answered_while_down", ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: targetID, DedupeKey: "worker:loop_answered_while_down", Priority: storage.QueuePriorityWorker, Status: "cancelled", AvailableAt: nowISO, Attempts: 0, MaxAttempts: -1, LastError: &cancelReason, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}
	if err := coordinator.Close(); err != nil {
		t.Fatalf("close seed database: %v", err)
	}

	proc := harness.StartLooperd(t, bins, home, home.ConfigPath, fakeGH.EnvMap(), cfg.Server.Host, cfg.Server.Port)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := proc.WaitForReady(ctx); err != nil {
		t.Fatalf("wait for ready: %v", err)
	}
	_, liveRepositories := openRepos(t, home.DBPath)
	deadline := time.Now().Add(900 * time.Millisecond)
	for time.Now().Before(deadline) {
		loop, err := liveRepositories.Loops.GetByID(context.Background(), loopID)
		if err == nil && loop != nil && loop.Status == "queued" {
			if _, ok := loopcondition.Read(loop.MetadataJSON); ok {
				t.Fatal("blocked condition remained after boot reconciliation")
			}
			if state, ok := loopengine.Read(loop.MetadataJSON); !ok || state.Phase != loopengine.PhaseRunning {
				t.Fatalf("lifecycle state after resume = %#v, present=%v", state, ok)
			}
			active, queueErr := liveRepositories.Queue.FindActiveByLoopID(context.Background(), loopID)
			if queueErr != nil || active == nil || active.Status != "queued" {
				t.Fatalf("active queue = %#v, %v", active, queueErr)
			}
			proc.Stop(context.Background())
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	proc.Stop(context.Background())
	t.Fatalf("loop %s did not leave human_answered hold on boot (db=%s)", loopID, filepath.Clean(home.DBPath))
}
