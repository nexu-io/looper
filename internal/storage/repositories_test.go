package storage

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestRepositoriesRoundTripForProjectsLoopsRunsAndRuntimeMetadata(t *testing.T) {
	t.Parallel()

	coordinator := openMigratedCoordinatorForRepositories(t)
	ctx := context.Background()
	repos := NewRepositories(coordinator.DB())

	now := "2026-04-11T12:00:00.000Z"
	mainBranch := "main"
	baseBranch := "main"
	projectMeta := `{"tier":"mvp"}`
	if err := repos.Projects.Upsert(ctx, ProjectRecord{
		ID:           "project_1",
		Name:         "Looper",
		RepoPath:     "/tmp/looper",
		BaseBranch:   &baseBranch,
		Archived:     false,
		MetadataJSON: &projectMeta,
		CreatedAt:    now,
		UpdatedAt:    now,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	targetID := "pr:42"
	repo := "acme/looper"
	prNumber := int64(42)
	config := `{"priority":"normal"}`
	if err := repos.Loops.Upsert(ctx, LoopRecord{
		ID:         "loop_1",
		Seq:        1,
		ProjectID:  "project_1",
		Type:       "reviewer",
		TargetType: "pull_request",
		TargetID:   &targetID,
		Repo:       &repo,
		PRNumber:   &prNumber,
		Status:     "idle",
		ConfigJSON: &config,
		CreatedAt:  now,
		UpdatedAt:  now,
	}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	running := "running"
	if err := repos.Runs.Upsert(ctx, RunRecord{
		ID:              "run_1",
		LoopID:          "loop_1",
		Status:          running,
		StartedAt:       now,
		LastHeartbeatAt: &now,
		CreatedAt:       now,
		UpdatedAt:       now,
	}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}

	agentCommand := `{"command":"codex","args":["exec","prompt"]}`
	agentCWD := "/tmp/looper"
	agentOutput := `{"stdout":"ok","stderr":""}`
	pid := int64(12345)
	if err := repos.AgentExecutions.Upsert(ctx, AgentExecutionRecord{
		ID:              "agent_1",
		RunID:           strPtr("run_1"),
		LoopID:          strPtr("loop_1"),
		ProjectID:       strPtr("project_1"),
		Vendor:          "codex",
		Status:          "running",
		PID:             &pid,
		CommandJSON:     &agentCommand,
		CWD:             &agentCWD,
		HeartbeatCount:  2,
		OutputJSON:      &agentOutput,
		StartedAt:       now,
		LastHeartbeatAt: &now,
		CreatedAt:       now,
		UpdatedAt:       now,
	}); err != nil {
		t.Fatalf("AgentExecutions.Upsert() error = %v", err)
	}

	headSHA := "abc123"
	if err := repos.PullRequestSnapshots.Upsert(ctx, PullRequestSnapshotRecord{
		ID:         "snapshot_1",
		ProjectID:  "project_1",
		Repo:       repo,
		PRNumber:   prNumber,
		HeadSHA:    headSHA,
		CapturedAt: now,
		CreatedAt:  now,
	}); err != nil {
		t.Fatalf("PullRequestSnapshots.Upsert() error = %v", err)
	}

	entityType := "loop"
	entityID := "loop_1"
	correlationID := "corr_1"
	causationID := "cause_1"
	actorType := "agent"
	actorID := "reviewer_1"
	actorDisplayName := "Reviewer"
	payloadJSON := `{"status":"idle"}`
	projectID := "project_1"
	runID := "run_1"
	loopID := "loop_1"
	if err := repos.Events.Append(ctx, EventLogRecord{
		ID:               "event_1",
		EventType:        "loop.created",
		ProjectID:        &projectID,
		LoopID:           &loopID,
		RunID:            &runID,
		EntityType:       &entityType,
		EntityID:         &entityID,
		CorrelationID:    &correlationID,
		CausationID:      &causationID,
		ActorType:        &actorType,
		ActorID:          &actorID,
		ActorDisplayName: &actorDisplayName,
		PayloadJSON:      payloadJSON,
		CreatedAt:        now,
	}); err != nil {
		t.Fatalf("Events.Append() error = %v", err)
	}

	notificationSubtitle := "runtime"
	notificationDedupe := "looperd.started:system:looperd"
	notificationPayload := `{"title":"Looper"}`
	if err := repos.Notifications.Upsert(ctx, NotificationRecord{
		ID:          "notification_1",
		ProjectID:   &projectID,
		LoopID:      &loopID,
		RunID:       &runID,
		EntityType:  strPtr("notification"),
		EntityID:    strPtr("looperd.started"),
		Channel:     "in_app",
		Level:       "success",
		Title:       "Looper",
		Subtitle:    &notificationSubtitle,
		Body:        "Started",
		Status:      "success",
		DedupeKey:   &notificationDedupe,
		PayloadJSON: &notificationPayload,
		SentAt:      &now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatalf("Notifications.Upsert() error = %v", err)
	}

	lockReason := "reviewer"
	acquired, err := repos.Locks.Acquire(ctx, LockRecord{
		Key:       "pr:acme/looper:42",
		Owner:     "reviewer-loop",
		Reason:    &lockReason,
		ExpiresAt: "2026-04-11T12:05:00.000Z",
		CreatedAt: now,
		UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("Locks.Acquire() error = %v", err)
	}
	if !acquired {
		t.Fatal("Locks.Acquire() = false, want true")
	}

	worktreePath := "/tmp/looper-worktrees/feature-loop-1"
	headWorktreeSHA := "def456"
	if err := repos.Worktrees.Upsert(ctx, WorktreeRecord{
		ID:           "wt_1",
		ProjectID:    "project_1",
		RepoPath:     "/tmp/looper",
		WorktreePath: worktreePath,
		Branch:       "feature/loop-1",
		BaseBranch:   &mainBranch,
		Status:       "active",
		HeadSHA:      &headWorktreeSHA,
		CreatedAt:    now,
		UpdatedAt:    now,
	}); err != nil {
		t.Fatalf("Worktrees.Upsert() error = %v", err)
	}

	loopBySeq, err := repos.Loops.GetBySeq(ctx, 1)
	if err != nil {
		t.Fatalf("Loops.GetBySeq() error = %v", err)
	}
	if loopBySeq == nil || loopBySeq.ID != "loop_1" {
		t.Fatalf("Loops.GetBySeq() = %#v, want loop_1", loopBySeq)
	}

	latestRun, err := repos.Runs.GetLatestByLoopID(ctx, "loop_1")
	if err != nil {
		t.Fatalf("Runs.GetLatestByLoopID() error = %v", err)
	}
	if latestRun == nil || latestRun.ID != "run_1" {
		t.Fatalf("Runs.GetLatestByLoopID() = %#v, want run_1", latestRun)
	}

	agentExecution, err := repos.AgentExecutions.GetByID(ctx, "agent_1")
	if err != nil {
		t.Fatalf("AgentExecutions.GetByID() error = %v", err)
	}
	if agentExecution == nil || agentExecution.ID != "agent_1" || agentExecution.HeartbeatCount != 2 {
		t.Fatalf("AgentExecutions.GetByID() = %#v, want agent_1 with heartbeat_count=2", agentExecution)
	}

	latestExecution, err := repos.AgentExecutions.GetLatestByRunID(ctx, "run_1")
	if err != nil {
		t.Fatalf("AgentExecutions.GetLatestByRunID() error = %v", err)
	}
	if latestExecution == nil || latestExecution.ID != "agent_1" {
		t.Fatalf("AgentExecutions.GetLatestByRunID() = %#v, want agent_1", latestExecution)
	}

	activeExecutions, err := repos.AgentExecutions.ListActive(ctx)
	if err != nil {
		t.Fatalf("AgentExecutions.ListActive() error = %v", err)
	}
	if len(activeExecutions) != 1 || activeExecutions[0].ID != "agent_1" {
		t.Fatalf("AgentExecutions.ListActive() = %#v, want [agent_1]", activeExecutions)
	}

	snapshot, err := repos.PullRequestSnapshots.GetLatest(ctx, repo, prNumber)
	if err != nil {
		t.Fatalf("PullRequestSnapshots.GetLatest() error = %v", err)
	}
	if snapshot == nil || snapshot.HeadSHA != headSHA {
		t.Fatalf("PullRequestSnapshots.GetLatest() = %#v, want headSha %q", snapshot, headSHA)
	}

	projectSnapshot, err := repos.PullRequestSnapshots.GetLatestByProject(ctx, "project_1", repo, prNumber)
	if err != nil {
		t.Fatalf("PullRequestSnapshots.GetLatestByProject() error = %v", err)
	}
	if projectSnapshot == nil || projectSnapshot.HeadSHA != headSHA {
		t.Fatalf("PullRequestSnapshots.GetLatestByProject() = %#v, want headSha %q", projectSnapshot, headSHA)
	}

	events, err := repos.Events.ListByEntity(ctx, "loop", "loop_1")
	if err != nil {
		t.Fatalf("Events.ListByEntity() error = %v", err)
	}
	if len(events) != 1 || events[0].ActorID == nil || *events[0].ActorID != "reviewer_1" {
		t.Fatalf("Events.ListByEntity() = %#v, want actorId reviewer_1", events)
	}

	lock, err := repos.Locks.Get(ctx, "pr:acme/looper:42")
	if err != nil {
		t.Fatalf("Locks.Get() error = %v", err)
	}
	if lock == nil || lock.Owner != "reviewer-loop" {
		t.Fatalf("Locks.Get() = %#v, want owner reviewer-loop", lock)
	}

	notifications, err := repos.Notifications.List(ctx, 10)
	if err != nil {
		t.Fatalf("Notifications.List() error = %v", err)
	}
	if len(notifications) != 1 || notifications[0].DedupeKey == nil || *notifications[0].DedupeKey != notificationDedupe {
		t.Fatalf("Notifications.List() = %#v, want notification with dedupe key %q", notifications, notificationDedupe)
	}

	latestNotification, err := repos.Notifications.GetLatestByDedupe(ctx, "in_app", notificationDedupe)
	if err != nil {
		t.Fatalf("Notifications.GetLatestByDedupe() error = %v", err)
	}
	if latestNotification == nil || latestNotification.ID != "notification_1" {
		t.Fatalf("Notifications.GetLatestByDedupe() = %#v, want notification_1", latestNotification)
	}

	worktree, err := repos.Worktrees.GetByBranch(ctx, "project_1", "feature/loop-1")
	if err != nil {
		t.Fatalf("Worktrees.GetByBranch() error = %v", err)
	}
	if worktree == nil || worktree.WorktreePath != worktreePath {
		t.Fatalf("Worktrees.GetByBranch() = %#v, want %q", worktree, worktreePath)
	}
	if worktree.BaseBranch == nil || *worktree.BaseBranch != mainBranch {
		t.Fatalf("Worktrees.GetByBranch().BaseBranch = %#v, want %q", worktree.BaseBranch, mainBranch)
	}
}

func TestLoopsAllocateSeqSeedsFromMaxWhenCounterMissing(t *testing.T) {
	t.Parallel()

	coordinator := openMigratedCoordinatorForRepositories(t)
	ctx := context.Background()
	repos := NewRepositories(coordinator.DB())

	now := "2026-04-11T12:00:00.000Z"
	if err := repos.Projects.Upsert(ctx, ProjectRecord{
		ID:        "project_1",
		Name:      "Looper",
		RepoPath:  "/tmp/looper",
		Archived:  false,
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	if err := repos.Loops.Upsert(ctx, LoopRecord{ID: "loop_3", Seq: 3, ProjectID: "project_1", Type: "worker", TargetType: "project", Status: "running", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Loops.Upsert(loop_3) error = %v", err)
	}
	if err := repos.Loops.Upsert(ctx, LoopRecord{ID: "loop_7", Seq: 7, ProjectID: "project_1", Type: "worker", TargetType: "project", Status: "running", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Loops.Upsert(loop_7) error = %v", err)
	}

	if _, err := coordinator.DB().ExecContext(ctx, `DELETE FROM counters WHERE name = 'loop_seq'`); err != nil {
		t.Fatalf("DELETE counters error = %v", err)
	}

	seq1, err := repos.Loops.AllocateSeq(ctx)
	if err != nil {
		t.Fatalf("Loops.AllocateSeq() first error = %v", err)
	}
	if seq1 != 8 {
		t.Fatalf("Loops.AllocateSeq() first = %d, want 8", seq1)
	}

	seq2, err := repos.Loops.AllocateSeq(ctx)
	if err != nil {
		t.Fatalf("Loops.AllocateSeq() second error = %v", err)
	}
	if seq2 != 9 {
		t.Fatalf("Loops.AllocateSeq() second = %d, want 9", seq2)
	}
}

func TestRepositoriesRollbackTransactionalWrites(t *testing.T) {
	t.Parallel()

	coordinator := openMigratedCoordinatorForRepositories(t)
	ctx := context.Background()
	repos := NewRepositories(coordinator.DB())

	now := "2026-04-11T12:00:00.000Z"
	if err := repos.Projects.Upsert(ctx, ProjectRecord{ID: "project_1", Name: "Looper", RepoPath: "/tmp/looper", Archived: false, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	wantErr := errors.New("abort transaction")
	err := coordinator.WithTransaction(ctx, func(tx *sql.Tx) error {
		txRepos := NewRepositories(tx)
		if upsertErr := txRepos.Loops.Upsert(ctx, LoopRecord{ID: "loop_rollback", Seq: 1, ProjectID: "project_1", Type: "worker", TargetType: "project", Status: "queued", CreatedAt: now, UpdatedAt: now}); upsertErr != nil {
			return upsertErr
		}
		entityType := "loop"
		entityID := "loop_rollback"
		projectID := "project_1"
		if appendErr := txRepos.Events.Append(ctx, EventLogRecord{ID: "event_loop_rollback", EventType: "loop.created", ProjectID: &projectID, EntityType: &entityType, EntityID: &entityID, PayloadJSON: `{}`, CreatedAt: now}); appendErr != nil {
			return appendErr
		}

		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("WithTransaction() error = %v, want %v", err, wantErr)
	}

	got, err := repos.Loops.GetByID(ctx, "loop_rollback")
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if got != nil {
		t.Fatalf("Loops.GetByID(loop_rollback) = %#v, want nil", got)
	}

	events, err := repos.Events.ListByEntity(ctx, "loop", "loop_rollback")
	if err != nil {
		t.Fatalf("Events.ListByEntity() error = %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("Events.ListByEntity(loop_rollback) = %#v, want none", events)
	}
}

func TestLocksAcquireRequiresExpiryBeforeReplacement(t *testing.T) {
	t.Parallel()

	coordinator := openMigratedCoordinatorForRepositories(t)
	ctx := context.Background()
	repos := NewRepositories(coordinator.DB())
	repos.Locks.SetNow(func() time.Time {
		return time.Date(2026, time.April, 11, 12, 0, 0, 0, time.UTC)
	})

	now := "2026-04-11T12:00:00.000Z"
	reason := "initial"
	acquired, err := repos.Locks.Acquire(ctx, LockRecord{
		Key:       "task:123",
		Owner:     "worker-a",
		Reason:    &reason,
		ExpiresAt: "2026-04-11T12:05:00.000Z",
		CreatedAt: now,
		UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("Locks.Acquire(initial) error = %v", err)
	}
	if !acquired {
		t.Fatal("Locks.Acquire(initial) = false, want true")
	}

	replacementReason := "takeover"
	acquired, err = repos.Locks.Acquire(ctx, LockRecord{
		Key:       "task:123",
		Owner:     "worker-b",
		Reason:    &replacementReason,
		ExpiresAt: "2026-04-11T12:20:00.000Z",
		CreatedAt: "2026-04-11T12:01:00.000Z",
		UpdatedAt: "2026-04-11T12:01:00.000Z",
	})
	if err != nil {
		t.Fatalf("Locks.Acquire(replacement blocked) error = %v", err)
	}
	if acquired {
		t.Fatal("Locks.Acquire(replacement blocked) = true, want false")
	}

	repos.Locks.SetNow(func() time.Time {
		return time.Date(2026, time.April, 11, 12, 10, 0, 0, time.UTC)
	})
	acquired, err = repos.Locks.Acquire(ctx, LockRecord{
		Key:       "task:123",
		Owner:     "worker-b",
		Reason:    &replacementReason,
		ExpiresAt: "2026-04-11T12:20:00.000Z",
		CreatedAt: "2026-04-11T12:10:00.000Z",
		UpdatedAt: "2026-04-11T12:10:00.000Z",
	})
	if err != nil {
		t.Fatalf("Locks.Acquire(replacement after expiry) error = %v", err)
	}
	if !acquired {
		t.Fatal("Locks.Acquire(replacement after expiry) = false, want true")
	}

	lock, err := repos.Locks.Get(ctx, "task:123")
	if err != nil {
		t.Fatalf("Locks.Get() error = %v", err)
	}
	if lock == nil || lock.Owner != "worker-b" {
		t.Fatalf("Locks.Get() = %#v, want owner worker-b", lock)
	}
}

func TestEventsListOrdersAndDefaults(t *testing.T) {
	t.Parallel()

	coordinator := openMigratedCoordinatorForRepositories(t)
	ctx := context.Background()
	repos := NewRepositories(coordinator.DB())

	entityType := "run"
	entityID := "run_1"
	for _, event := range []EventLogRecord{
		{ID: "event_1", EventType: "worker.started", EntityType: &entityType, EntityID: &entityID, PayloadJSON: `{"step":1}`, CreatedAt: "2026-04-11T12:00:00.000Z"},
		{ID: "event_2", EventType: "worker.progress", EntityType: &entityType, EntityID: &entityID, PayloadJSON: `{"step":2}`, CreatedAt: "2026-04-11T12:01:00.000Z"},
		{ID: "event_3", EventType: "worker.completed", EntityType: &entityType, EntityID: &entityID, PayloadJSON: `{"step":3}`, CreatedAt: "2026-04-11T12:02:00.000Z"},
	} {
		if err := repos.Events.Append(ctx, event); err != nil {
			t.Fatalf("Events.Append(%s) error = %v", event.ID, err)
		}
	}

	listed, err := repos.Events.List(ctx, 2)
	if err != nil {
		t.Fatalf("Events.List() error = %v", err)
	}
	if len(listed) != 2 {
		t.Fatalf("len(Events.List()) = %d, want 2", len(listed))
	}
	if listed[0].ID != "event_3" || listed[1].ID != "event_2" {
		t.Fatalf("Events.List() order = %#v, want [event_3 event_2]", listed)
	}

	allByDefault, err := repos.Events.List(ctx, 0)
	if err != nil {
		t.Fatalf("Events.List(default) error = %v", err)
	}
	if len(allByDefault) != 3 {
		t.Fatalf("len(Events.List(default)) = %d, want 3", len(allByDefault))
	}

	byEntity, err := repos.Events.ListByEntity(ctx, "run", "run_1")
	if err != nil {
		t.Fatalf("Events.ListByEntity() error = %v", err)
	}
	if len(byEntity) != 3 {
		t.Fatalf("len(Events.ListByEntity()) = %d, want 3", len(byEntity))
	}
	if byEntity[0].ID != "event_1" || byEntity[1].ID != "event_2" || byEntity[2].ID != "event_3" {
		t.Fatalf("Events.ListByEntity() order = %#v, want [event_1 event_2 event_3]", byEntity)
	}
}

func TestRunsListByStatusOrdersByStartedAtThenIDDesc(t *testing.T) {
	t.Parallel()

	coordinator := openMigratedCoordinatorForRepositories(t)
	ctx := context.Background()
	repos := NewRepositories(coordinator.DB())

	now := "2026-04-11T12:00:00.000Z"
	if err := repos.Projects.Upsert(ctx, ProjectRecord{ID: "project_1", Name: "Looper", RepoPath: "/tmp/looper", Archived: false, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := repos.Loops.Upsert(ctx, LoopRecord{ID: "loop_1", Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "project", Status: "running", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	for _, run := range []RunRecord{
		{ID: "run_1", LoopID: "loop_1", Status: "running", StartedAt: "2026-04-11T12:00:00.000Z", CreatedAt: now, UpdatedAt: now},
		{ID: "run_3", LoopID: "loop_1", Status: "running", StartedAt: "2026-04-11T12:00:00.000Z", CreatedAt: now, UpdatedAt: now},
		{ID: "run_2", LoopID: "loop_1", Status: "running", StartedAt: "2026-04-11T12:01:00.000Z", CreatedAt: now, UpdatedAt: now},
	} {
		if err := repos.Runs.Upsert(ctx, run); err != nil {
			t.Fatalf("Runs.Upsert(%s) error = %v", run.ID, err)
		}
	}

	running, err := repos.Runs.ListByStatus(ctx, "running")
	if err != nil {
		t.Fatalf("Runs.ListByStatus() error = %v", err)
	}
	if len(running) != 3 {
		t.Fatalf("len(Runs.ListByStatus()) = %d, want 3", len(running))
	}

	got := []string{running[0].ID, running[1].ID, running[2].ID}
	want := []string{"run_2", "run_3", "run_1"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Runs.ListByStatus() order = %v, want %v", got, want)
		}
	}
}

func TestQueueRoundTripBasics(t *testing.T) {
	t.Parallel()

	coordinator := openMigratedCoordinatorForRepositories(t)
	ctx := context.Background()
	repos := NewRepositories(coordinator.DB())

	now := "2026-04-11T12:00:00.000Z"
	if err := repos.Projects.Upsert(ctx, ProjectRecord{ID: "project_q", Name: "Looper", RepoPath: "/tmp/looper", Archived: false, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := repos.Loops.Upsert(ctx, LoopRecord{ID: "loop_q", Seq: 1, ProjectID: "project_q", Type: "reviewer", TargetType: "pull_request", Status: "running", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	repo := "acme/looper"
	prNumber := int64(7)
	payload := `{"kind":"review"}`
	projectID := "project_q"
	loopID := "loop_q"
	if err := repos.Queue.Upsert(ctx, QueueItemRecord{
		ID:          "qi_1",
		ProjectID:   &projectID,
		LoopID:      &loopID,
		Type:        "reviewer",
		TargetType:  "pull_request",
		TargetID:    "pr:7",
		Repo:        &repo,
		PRNumber:    &prNumber,
		DedupeKey:   "reviewer:acme/looper:7",
		Priority:    10,
		Status:      "queued",
		AvailableAt: now,
		Attempts:    0,
		MaxAttempts: 3,
		PayloadJSON: &payload,
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	gotByID, err := repos.Queue.GetByID(ctx, "qi_1")
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	if gotByID == nil || gotByID.Type != "reviewer" || gotByID.TargetID != "pr:7" {
		t.Fatalf("Queue.GetByID() = %#v, want reviewer/pr:7", gotByID)
	}

	activeByDedupe, err := repos.Queue.FindActiveByDedupe(ctx, "reviewer:acme/looper:7")
	if err != nil {
		t.Fatalf("Queue.FindActiveByDedupe() error = %v", err)
	}
	if activeByDedupe == nil || activeByDedupe.ID != "qi_1" {
		t.Fatalf("Queue.FindActiveByDedupe() = %#v, want qi_1", activeByDedupe)
	}

	scheduled, err := repos.Queue.ListScheduled(ctx, now, 10)
	if err != nil {
		t.Fatalf("Queue.ListScheduled() error = %v", err)
	}
	if len(scheduled) != 1 || scheduled[0].ID != "qi_1" {
		t.Fatalf("Queue.ListScheduled() = %#v, want [qi_1]", scheduled)
	}

	all, err := repos.Queue.List(ctx)
	if err != nil {
		t.Fatalf("Queue.List() error = %v", err)
	}
	if len(all) != 1 || all[0].ID != "qi_1" {
		t.Fatalf("Queue.List() = %#v, want [qi_1]", all)
	}
}

func TestQueueClaimOrderingAndBlockers(t *testing.T) {
	t.Parallel()

	coordinator := openMigratedCoordinatorForRepositories(t)
	ctx := context.Background()
	repos := NewRepositories(coordinator.DB())

	now := "2026-04-11T12:00:00.000Z"
	if err := repos.Projects.Upsert(ctx, ProjectRecord{ID: "project_sched", Name: "Looper", RepoPath: "/tmp/looper", Archived: false, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := repos.Loops.Upsert(ctx, LoopRecord{ID: "loop_active", Seq: 1, ProjectID: "project_sched", Type: "worker", TargetType: "project", Status: "running", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Loops.Upsert(loop_active) error = %v", err)
	}
	if err := repos.Loops.Upsert(ctx, LoopRecord{ID: "loop_paused", Seq: 2, ProjectID: "project_sched", Type: "worker", TargetType: "project", Status: "paused", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Loops.Upsert(loop_paused) error = %v", err)
	}

	repo := "acme/looper"
	pr := int64(42)
	loopActive := "loop_active"
	loopPaused := "loop_paused"
	queueItems := []QueueItemRecord{
		{ID: "lock_running", LoopID: &loopActive, Type: "reviewer", TargetType: "pull_request", TargetID: "pr:lock-running", DedupeKey: "d_lock_running", Priority: 1, Status: "running", AvailableAt: "2026-04-11T11:50:00.000Z", Attempts: 1, MaxAttempts: 3, LockKey: strPtr("repo:acme/looper"), CreatedAt: "2026-04-11T11:40:00.000Z", UpdatedAt: now},
		{ID: "lock_blocked", LoopID: &loopActive, Type: "reviewer", TargetType: "pull_request", TargetID: "pr:lock-blocked", DedupeKey: "d_lock_blocked", Priority: 1, Status: "queued", AvailableAt: now, Attempts: 0, MaxAttempts: 3, LockKey: strPtr("repo:acme/looper"), CreatedAt: "2026-04-11T11:41:00.000Z", UpdatedAt: now},
		{ID: "reviewer_blocker", LoopID: &loopActive, Type: "reviewer", TargetType: "pull_request", TargetID: "pr:42", Repo: &repo, PRNumber: &pr, DedupeKey: "d_reviewer", Priority: 2, Status: "queued", AvailableAt: now, Attempts: 0, MaxAttempts: 3, CreatedAt: "2026-04-11T11:42:00.000Z", UpdatedAt: now},
		{ID: "fixer_blocked", LoopID: &loopActive, Type: "fixer", TargetType: "pull_request", TargetID: "pr:42", Repo: &repo, PRNumber: &pr, DedupeKey: "d_fixer_blocked", Priority: 1, Status: "queued", AvailableAt: now, Attempts: 0, MaxAttempts: 3, CreatedAt: "2026-04-11T11:43:00.000Z", UpdatedAt: now},
		{ID: "paused_excluded", LoopID: &loopPaused, Type: "reviewer", TargetType: "pull_request", TargetID: "pr:paused", DedupeKey: "d_paused", Priority: 1, Status: "queued", AvailableAt: now, Attempts: 0, MaxAttempts: 3, CreatedAt: "2026-04-11T11:44:00.000Z", UpdatedAt: now},
		{ID: "eligible_1", LoopID: &loopActive, Type: "planner", TargetType: "project", TargetID: "project_sched", DedupeKey: "d_eligible_1", Priority: 1, Status: "queued", AvailableAt: now, Attempts: 0, MaxAttempts: 3, CreatedAt: "2026-04-11T11:45:00.000Z", UpdatedAt: now},
		{ID: "eligible_2", LoopID: &loopActive, Type: "planner", TargetType: "project", TargetID: "project_sched", DedupeKey: "d_eligible_2", Priority: 3, Status: "queued", AvailableAt: now, Attempts: 0, MaxAttempts: 3, CreatedAt: "2026-04-11T11:46:00.000Z", UpdatedAt: now},
	}

	for _, item := range queueItems {
		if err := repos.Queue.Upsert(ctx, item); err != nil {
			t.Fatalf("Queue.Upsert(%s) error = %v", item.ID, err)
		}
	}

	scheduled, err := repos.Queue.ListScheduled(ctx, now, 10)
	if err != nil {
		t.Fatalf("Queue.ListScheduled() error = %v", err)
	}
	if len(scheduled) != 3 {
		t.Fatalf("len(Queue.ListScheduled()) = %d, want 3", len(scheduled))
	}
	if scheduled[0].ID != "eligible_1" || scheduled[1].ID != "reviewer_blocker" || scheduled[2].ID != "eligible_2" {
		t.Fatalf("Queue.ListScheduled() order = %#v, want eligible_1/reviewer_blocker/eligible_2", []string{scheduled[0].ID, scheduled[1].ID, scheduled[2].ID})
	}

	claimed, err := repos.Queue.ClaimNext(ctx, now, "worker-a")
	if err != nil {
		t.Fatalf("Queue.ClaimNext() error = %v", err)
	}
	if claimed == nil || claimed.ID != "eligible_1" || claimed.Status != "running" {
		t.Fatalf("Queue.ClaimNext() = %#v, want eligible_1 running", claimed)
	}
	if claimed.ClaimedBy == nil || *claimed.ClaimedBy != "worker-a" || claimed.ClaimedAt == nil || *claimed.ClaimedAt != now {
		t.Fatalf("claimed metadata = %#v, want claimed_by worker-a and claimed_at %q", claimed, now)
	}

	claimedType, err := repos.Queue.ClaimNextOfType(ctx, now, "worker-b", "planner")
	if err != nil {
		t.Fatalf("Queue.ClaimNextOfType() error = %v", err)
	}
	if claimedType == nil || claimedType.ID != "eligible_2" {
		t.Fatalf("Queue.ClaimNextOfType() = %#v, want eligible_2", claimedType)
	}
	if claimedType.ClaimedBy == nil || *claimedType.ClaimedBy != "worker-b" || claimedType.ClaimedAt == nil || *claimedType.ClaimedAt != now {
		t.Fatalf("claimedType metadata = %#v, want claimed_by worker-b and claimed_at %q", claimedType, now)
	}
}

func TestQueueRetryFailCompleteTransitions(t *testing.T) {
	t.Parallel()

	coordinator := openMigratedCoordinatorForRepositories(t)
	ctx := context.Background()
	repos := NewRepositories(coordinator.DB())

	now := "2026-04-11T12:00:00.000Z"
	if err := repos.Projects.Upsert(ctx, ProjectRecord{ID: "project_tr", Name: "Looper", RepoPath: "/tmp/looper", Archived: false, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := repos.Loops.Upsert(ctx, LoopRecord{ID: "loop_tr", Seq: 1, ProjectID: "project_tr", Type: "worker", TargetType: "project", Status: "running", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	loopID := "loop_tr"
	claimedBy := "worker-a"
	claimedAt := "2026-04-11T12:00:10.000Z"
	if err := repos.Queue.Upsert(ctx, QueueItemRecord{
		ID:          "qi_retry",
		LoopID:      &loopID,
		Type:        "planner",
		TargetType:  "project",
		TargetID:    "project_tr",
		DedupeKey:   "d_retry",
		Priority:    1,
		Status:      "running",
		AvailableAt: now,
		Attempts:    1,
		MaxAttempts: 3,
		ClaimedBy:   &claimedBy,
		ClaimedAt:   &claimedAt,
		StartedAt:   &claimedAt,
		CreatedAt:   now,
		UpdatedAt:   claimedAt,
	}); err != nil {
		t.Fatalf("Queue.Upsert(qi_retry) error = %v", err)
	}

	retryAt := "2026-04-11T12:05:00.000Z"
	errMsg := "temporary error"
	if err := repos.Queue.MarkRetry(ctx, QueueMarkRetryInput{ID: "qi_retry", AvailableAt: retryAt, Attempts: 2, ErrorMessage: &errMsg, ErrorKind: "retryable_transient", UpdatedAt: retryAt}); err != nil {
		t.Fatalf("Queue.MarkRetry() error = %v", err)
	}
	gotRetry, err := repos.Queue.GetByID(ctx, "qi_retry")
	if err != nil {
		t.Fatalf("Queue.GetByID(qi_retry) error = %v", err)
	}
	if gotRetry == nil || gotRetry.Status != "queued" || gotRetry.Attempts != 2 || gotRetry.ClaimedBy != nil || gotRetry.ClaimedAt != nil {
		t.Fatalf("Queue.GetByID(qi_retry) after markRetry = %#v, want queued attempts=2 unclaimed", gotRetry)
	}

	finished := "2026-04-11T12:06:00.000Z"
	if err := repos.Queue.Complete(ctx, "qi_retry", finished); err != nil {
		t.Fatalf("Queue.Complete() error = %v", err)
	}
	gotCompleted, err := repos.Queue.GetByID(ctx, "qi_retry")
	if err != nil {
		t.Fatalf("Queue.GetByID(qi_retry) completed error = %v", err)
	}
	if gotCompleted == nil || gotCompleted.Status != "completed" || gotCompleted.FinishedAt == nil || *gotCompleted.FinishedAt != finished {
		t.Fatalf("Queue.GetByID(qi_retry) after complete = %#v, want completed with finished_at", gotCompleted)
	}

	if err := repos.Queue.Upsert(ctx, QueueItemRecord{
		ID:          "qi_fail",
		LoopID:      &loopID,
		Type:        "planner",
		TargetType:  "project",
		TargetID:    "project_tr",
		DedupeKey:   "d_fail",
		Priority:    1,
		Status:      "running",
		AvailableAt: now,
		Attempts:    3,
		MaxAttempts: 3,
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatalf("Queue.Upsert(qi_fail) error = %v", err)
	}

	failReason := "needs human"
	if err := repos.Queue.Fail(ctx, QueueFailInput{ID: "qi_fail", FinishedAt: finished, ErrorMessage: &failReason, ErrorKind: "manual_intervention", UpdatedAt: finished}); err != nil {
		t.Fatalf("Queue.Fail() error = %v", err)
	}
	gotFailed, err := repos.Queue.GetByID(ctx, "qi_fail")
	if err != nil {
		t.Fatalf("Queue.GetByID(qi_fail) error = %v", err)
	}
	if gotFailed == nil || gotFailed.Status != "manual_intervention" || gotFailed.LastError == nil || *gotFailed.LastError != failReason {
		t.Fatalf("Queue.GetByID(qi_fail) after fail = %#v, want manual_intervention", gotFailed)
	}
}

func TestQueueRecoveryHelpersRequeueAndCancelByLoop(t *testing.T) {
	t.Parallel()

	coordinator := openMigratedCoordinatorForRepositories(t)
	ctx := context.Background()
	repos := NewRepositories(coordinator.DB())

	now := "2026-04-11T12:00:00.000Z"
	if err := repos.Projects.Upsert(ctx, ProjectRecord{ID: "project_rec", Name: "Looper", RepoPath: "/tmp/looper", Archived: false, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := repos.Loops.Upsert(ctx, LoopRecord{ID: "loop_rec", Seq: 1, ProjectID: "project_rec", Type: "worker", TargetType: "project", Status: "running", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	loopID := "loop_rec"
	runningAt := "2026-04-11T12:01:00.000Z"
	if err := repos.Queue.Upsert(ctx, QueueItemRecord{ID: "qi_run_1", LoopID: &loopID, Type: "planner", TargetType: "project", TargetID: "project_rec", DedupeKey: "d_run_1", Priority: 1, Status: "running", AvailableAt: now, Attempts: 1, MaxAttempts: 3, ClaimedBy: strPtr("worker-a"), ClaimedAt: &runningAt, StartedAt: &runningAt, CreatedAt: now, UpdatedAt: runningAt}); err != nil {
		t.Fatalf("Queue.Upsert(qi_run_1) error = %v", err)
	}
	if err := repos.Queue.Upsert(ctx, QueueItemRecord{ID: "qi_run_2", LoopID: &loopID, Type: "planner", TargetType: "project", TargetID: "project_rec", DedupeKey: "d_run_2", Priority: 2, Status: "running", AvailableAt: now, Attempts: 1, MaxAttempts: 3, ClaimedBy: strPtr("worker-b"), ClaimedAt: &runningAt, StartedAt: &runningAt, CreatedAt: now, UpdatedAt: runningAt}); err != nil {
		t.Fatalf("Queue.Upsert(qi_run_2) error = %v", err)
	}
	if err := repos.Queue.Upsert(ctx, QueueItemRecord{ID: "qi_queued", LoopID: &loopID, Type: "planner", TargetType: "project", TargetID: "project_rec", DedupeKey: "d_queued", Priority: 2, Status: "queued", AvailableAt: now, Attempts: 0, MaxAttempts: 3, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Queue.Upsert(qi_queued) error = %v", err)
	}

	requeuedAt := "2026-04-11T12:10:00.000Z"
	requeuedCount, err := repos.Queue.RequeueRunningByLoop(ctx, "loop_rec", requeuedAt)
	if err != nil {
		t.Fatalf("Queue.RequeueRunningByLoop() error = %v", err)
	}
	if requeuedCount != 2 {
		t.Fatalf("Queue.RequeueRunningByLoop() = %d, want 2", requeuedCount)
	}

	for _, id := range []string{"qi_run_1", "qi_run_2"} {
		got, getErr := repos.Queue.GetByID(ctx, id)
		if getErr != nil {
			t.Fatalf("Queue.GetByID(%s) error = %v", id, getErr)
		}
		if got == nil || got.Status != "queued" || got.ClaimedBy != nil || got.StartedAt != nil || got.AvailableAt != requeuedAt {
			t.Fatalf("Queue.GetByID(%s) after requeue = %#v, want queued/cleared/requeued time", id, got)
		}
	}

	reason := "loop paused"
	cancelledCount, err := repos.Queue.CancelByLoop(ctx, "loop_rec", requeuedAt, &reason)
	if err != nil {
		t.Fatalf("Queue.CancelByLoop() error = %v", err)
	}
	if cancelledCount != 3 {
		t.Fatalf("Queue.CancelByLoop() = %d, want 3", cancelledCount)
	}

	for _, id := range []string{"qi_run_1", "qi_run_2", "qi_queued"} {
		got, getErr := repos.Queue.GetByID(ctx, id)
		if getErr != nil {
			t.Fatalf("Queue.GetByID(%s) error = %v", id, getErr)
		}
		if got == nil || got.Status != "cancelled" || got.LastError == nil || *got.LastError != reason {
			t.Fatalf("Queue.GetByID(%s) after cancel = %#v, want cancelled with reason", id, got)
		}
	}

	revivedAt := "2026-04-11T12:11:00.000Z"
	revivedCount, err := repos.Queue.RequeueLatestCancelledByLoop(ctx, "loop_rec", revivedAt)
	if err != nil {
		t.Fatalf("Queue.RequeueLatestCancelledByLoop() error = %v", err)
	}
	if revivedCount != 1 {
		t.Fatalf("Queue.RequeueLatestCancelledByLoop() = %d, want 1", revivedCount)
	}

	revivedID := ""
	for _, id := range []string{"qi_run_1", "qi_run_2", "qi_queued"} {
		got, getErr := repos.Queue.GetByID(ctx, id)
		if getErr != nil {
			t.Fatalf("Queue.GetByID(%s) after revive error = %v", id, getErr)
		}
		if got != nil && got.Status == "queued" {
			revivedID = id
			if got.FinishedAt != nil || got.LastError != nil || got.AvailableAt != revivedAt {
				t.Fatalf("Queue.GetByID(%s) after revive = %#v, want queued with cleared terminal state", id, got)
			}
		}
	}
	if revivedID == "" {
		t.Fatal("expected one cancelled queue item to be requeued")
	}
}

func strPtr(value string) *string {
	return &value
}

func openMigratedCoordinatorForRepositories(t *testing.T) *SQLiteCoordinator {
	t.Helper()

	root := t.TempDir()
	coordinator, err := OpenSQLiteCoordinator(context.Background(), filepath.Join(root, "looper.sqlite"), SQLiteCoordinatorOptions{
		Migrations: EmbeddedMigrations,
		BackupDir:  filepath.Join(root, "backups"),
	})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	t.Cleanup(func() {
		if closeErr := coordinator.Close(); closeErr != nil {
			t.Fatalf("coordinator.Close() error = %v", closeErr)
		}
	})

	if _, err := coordinator.MigrationRunner().RunPending(context.Background()); err != nil {
		t.Fatalf("MigrationRunner.RunPending() error = %v", err)
	}

	return coordinator
}
