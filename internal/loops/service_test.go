package loops

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/powerformer/looper/internal/domain"
	"github.com/powerformer/looper/internal/storage"
)

func TestServiceCreateAndPauseResumeLoop(t *testing.T) {
	t.Parallel()

	coordinator := openCoordinator(t)
	ctx := context.Background()
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 17, 12, 34, 56, 0, time.UTC)
	seedProject(t, repos, now)
	service := &Service{DB: coordinator.DB(), Repos: repos, Now: func() time.Time { return now }}

	loop, err := service.Create(ctx, CreateInput{
		ProjectID: "project_1",
		Type:      domain.LoopTypeReviewer,
		Target:    domain.LoopTarget{TargetType: domain.LoopTargetTypePullRequest, Repo: "acme/looper", PRNumber: 42},
		Status:    domain.LoopStatusQueued,
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if loop.Seq != 1 {
		t.Fatalf("Create().Seq = %d, want 1", loop.Seq)
	}

	reason := "pause for test"
	paused, err := service.Pause(ctx, loop.ID, &reason)
	if err != nil {
		t.Fatalf("Pause() error = %v", err)
	}
	if paused.Loop.Status != string(domain.LoopStatusPaused) {
		t.Fatalf("Pause().Loop.Status = %q, want paused", paused.Loop.Status)
	}

	resumed, err := service.Resume(ctx, loop.ID)
	if err != nil {
		t.Fatalf("Resume() error = %v", err)
	}
	if resumed.Status != string(domain.LoopStatusQueued) || resumed.NextRunAt == nil {
		t.Fatalf("Resume() = %#v, want queued with next run", resumed)
	}
}

func TestServiceCreateRejectsConflictingActiveLoop(t *testing.T) {
	t.Parallel()

	coordinator := openCoordinator(t)
	ctx := context.Background()
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 17, 12, 34, 56, 0, time.UTC)
	seedProject(t, repos, now)
	service := &Service{DB: coordinator.DB(), Repos: repos, Now: func() time.Time { return now }}

	_, err := service.Create(ctx, CreateInput{
		ProjectID: "project_1",
		Type:      domain.LoopTypeReviewer,
		Target:    domain.LoopTarget{TargetType: domain.LoopTargetTypePullRequest, Repo: "acme/looper", PRNumber: 42},
		Status:    domain.LoopStatusRunning,
	})
	if err != nil {
		t.Fatalf("first Create() error = %v", err)
	}

	_, err = service.Create(ctx, CreateInput{
		ProjectID: "project_1",
		Type:      domain.LoopTypeReviewer,
		Target:    domain.LoopTarget{TargetType: domain.LoopTargetTypePullRequest, Repo: "acme/looper", PRNumber: 42},
		Status:    domain.LoopStatusQueued,
	})
	if err == nil {
		t.Fatal("second Create() error = nil, want conflict")
	}
}

func TestServiceCreateAllowsWaitingReviewerLoopRerun(t *testing.T) {
	t.Parallel()

	coordinator := openCoordinator(t)
	ctx := context.Background()
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 17, 12, 34, 56, 0, time.UTC)
	seedProject(t, repos, now)
	service := &Service{DB: coordinator.DB(), Repos: repos, Now: func() time.Time { return now }}
	target := domain.LoopTarget{TargetType: domain.LoopTargetTypePullRequest, Repo: "acme/looper", PRNumber: 42}

	_, err := service.Create(ctx, CreateInput{ProjectID: "project_1", Type: domain.LoopTypeReviewer, Target: target, Status: domain.LoopStatusWaiting})
	if err != nil {
		t.Fatalf("waiting Create() error = %v", err)
	}
	if _, err := service.Create(ctx, CreateInput{ProjectID: "project_1", Type: domain.LoopTypeReviewer, Target: target, Status: domain.LoopStatusQueued}); err != nil {
		t.Fatalf("queued Create() with waiting existing loop error = %v, want allowed", err)
	}
}

func TestServicePauseWaitingLoopCancelsQueuedWork(t *testing.T) {
	t.Parallel()

	coordinator := openCoordinator(t)
	ctx := context.Background()
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 17, 12, 34, 56, 0, time.UTC)
	nowISO := now.UTC().Format("2006-01-02T15:04:05.000Z")
	seedProject(t, repos, now)
	service := &Service{DB: coordinator.DB(), Repos: repos, Now: func() time.Time { return now }}
	repo := "acme/looper"
	prNumber := int64(42)
	loop, err := service.Create(ctx, CreateInput{ProjectID: "project_1", Type: domain.LoopTypeReviewer, Target: domain.LoopTarget{TargetType: domain.LoopTargetTypePullRequest, Repo: repo, PRNumber: prNumber}, Status: domain.LoopStatusWaiting})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	projectID := "project_1"
	queue := storage.QueueItemRecord{ID: "queue_waiting", ProjectID: &projectID, LoopID: &loop.ID, Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, DedupeKey: "reviewer:waiting", Priority: storage.QueuePriorityReviewer, Status: "queued", AvailableAt: nowISO, Attempts: 0, MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := repos.Queue.Upsert(ctx, queue); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	reason := "stop waiting loop"
	paused, err := service.Pause(ctx, loop.ID, &reason)
	if err != nil {
		t.Fatalf("Pause(waiting) error = %v", err)
	}
	if paused.Loop.Status != string(domain.LoopStatusPaused) || paused.Loop.NextRunAt != nil {
		t.Fatalf("Pause(waiting).Loop = %#v, want paused without next run", paused.Loop)
	}
	if paused.CancelledQueueItems != 1 {
		t.Fatalf("CancelledQueueItems = %d, want 1", paused.CancelledQueueItems)
	}
	cancelled, err := repos.Queue.GetByID(ctx, queue.ID)
	if err != nil || cancelled == nil || cancelled.Status != "cancelled" {
		t.Fatalf("Queue.GetByID() = (%#v, %v), want cancelled", cancelled, err)
	}
}

func TestTargetFromRecordNormalizesRepeatedProjectPrefix(t *testing.T) {
	t.Parallel()

	targetID := "project:project:project_1"
	target, err := targetFromRecord(storage.LoopRecord{ID: "loop_1", TargetType: string(domain.LoopTargetTypeProject), TargetID: &targetID})
	if err != nil {
		t.Fatalf("targetFromRecord() error = %v", err)
	}
	if target.ProjectID != "project_1" {
		t.Fatalf("targetFromRecord().ProjectID = %q, want project_1", target.ProjectID)
	}
}

func openCoordinator(t *testing.T) *storage.SQLiteCoordinator {
	t.Helper()
	coordinator, err := storage.OpenSQLiteCoordinator(context.Background(), filepath.Join(t.TempDir(), "loops.sqlite"), storage.SQLiteCoordinatorOptions{BackupDir: t.TempDir()})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	if _, err := coordinator.MigrationRunner().RunPending(context.Background()); err != nil {
		t.Fatalf("RunPending() error = %v", err)
	}
	return coordinator
}

func seedProject(t *testing.T, repos *storage.Repositories, now time.Time) {
	t.Helper()
	nowISO := now.UTC().Format("2006-01-02T15:04:05.000Z")
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "project_1", Name: "Looper", RepoPath: "/tmp/looper", Archived: false, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
}
