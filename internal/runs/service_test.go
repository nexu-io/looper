package runs

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/domain"
	loopsvc "github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
)

func TestServiceStartRecordAndCompleteRun(t *testing.T) {
	t.Parallel()

	coordinator := openCoordinator(t)
	ctx := context.Background()
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 17, 12, 34, 56, 0, time.UTC)
	seedProjectAndLoop(t, repos, now)
	loopService := &loopsvc.Service{DB: coordinator.DB(), Repos: repos, Now: func() time.Time { return now }}
	service := &Service{DB: coordinator.DB(), Repos: repos, Loops: loopService, Now: func() time.Time { return now }}

	step := "review"
	run, err := service.StartRun(ctx, StartInput{LoopID: "loop_1", CurrentStep: &step})
	if err != nil {
		t.Fatalf("StartRun() error = %v", err)
	}
	if run.Status != string(domain.RunStatusRunning) {
		t.Fatalf("StartRun().Status = %q, want running", run.Status)
	}

	completedStep := "review"
	run, err = service.RecordStep(ctx, RecordStepInput{RunID: run.ID, LoopType: domain.LoopTypeReviewer, LastCompletedStep: &completedStep, EventType: "loop.step.completed", EventPayload: map[string]any{"step": completedStep}})
	if err != nil {
		t.Fatalf("RecordStep() error = %v", err)
	}
	if run.LastCompletedStep == nil || *run.LastCompletedStep != completedStep {
		t.Fatalf("RecordStep().LastCompletedStep = %v, want %q", run.LastCompletedStep, completedStep)
	}

	summary := "Published review"
	run, err = service.Complete(ctx, run.ID, CompleteInput{Status: domain.RunStatusSuccess, Summary: &summary})
	if err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if run.Status != string(domain.RunStatusSuccess) || run.EndedAt == nil {
		t.Fatalf("Complete() = %#v, want success with endedAt", run)
	}

	loop, err := repos.Loops.GetByID(ctx, "loop_1")
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if loop == nil || loop.Status != string(domain.LoopStatusRunning) || loop.LastRunAt == nil {
		t.Fatalf("loop after StartRun() = %#v, want running with lastRunAt", loop)
	}
}

func TestServiceCompleteRejectsInvalidTransition(t *testing.T) {
	t.Parallel()

	coordinator := openCoordinator(t)
	ctx := context.Background()
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 17, 12, 34, 56, 0, time.UTC)
	seedProjectAndLoop(t, repos, now)
	loopService := &loopsvc.Service{DB: coordinator.DB(), Repos: repos, Now: func() time.Time { return now }}
	service := &Service{DB: coordinator.DB(), Repos: repos, Loops: loopService, Now: func() time.Time { return now }}

	step := "review"
	run, err := service.StartRun(ctx, StartInput{LoopID: "loop_1", CurrentStep: &step})
	if err != nil {
		t.Fatalf("StartRun() error = %v", err)
	}
	if _, err := service.Complete(ctx, run.ID, CompleteInput{Status: domain.RunStatusRunning}); err == nil {
		t.Fatal("Complete(running) error = nil, want invalid transition")
	}
}

func openCoordinator(t *testing.T) *storage.SQLiteCoordinator {
	t.Helper()
	coordinator, err := storage.OpenSQLiteCoordinator(context.Background(), filepath.Join(t.TempDir(), "runs.sqlite"), storage.SQLiteCoordinatorOptions{BackupDir: t.TempDir()})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	if _, err := coordinator.MigrationRunner().RunPending(context.Background()); err != nil {
		t.Fatalf("RunPending() error = %v", err)
	}
	return coordinator
}

func seedProjectAndLoop(t *testing.T, repos *storage.Repositories, now time.Time) {
	t.Helper()
	nowISO := now.UTC().Format("2006-01-02T15:04:05.000Z")
	repo := "acme/looper"
	prNumber := int64(42)
	targetID := "pr:acme/looper:42"
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "project_1", Name: "Looper", RepoPath: "/tmp/looper", Archived: false, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: "loop_1", Seq: 1, ProjectID: "project_1", Type: string(domain.LoopTypeReviewer), TargetType: string(domain.LoopTargetTypePullRequest), TargetID: &targetID, Repo: &repo, PRNumber: &prNumber, Status: string(domain.LoopStatusQueued), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
}
