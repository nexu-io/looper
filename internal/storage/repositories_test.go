package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"sync"
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
	nativeSessionID := "codex-session-1"
	nativeResumeMode := "native_resume"
	nativeResumeStatus := "pending"
	pid := int64(12345)
	if err := repos.AgentExecutions.Upsert(ctx, AgentExecutionRecord{
		ID:                 "agent_1",
		RunID:              strPtr("run_1"),
		LoopID:             strPtr("loop_1"),
		ProjectID:          strPtr("project_1"),
		Vendor:             "codex",
		Status:             "running",
		PID:                &pid,
		CommandJSON:        &agentCommand,
		CWD:                &agentCWD,
		HeartbeatCount:     2,
		OutputJSON:         &agentOutput,
		NativeSessionID:    &nativeSessionID,
		NativeResumeMode:   &nativeResumeMode,
		NativeResumeStatus: &nativeResumeStatus,
		StartedAt:          now,
		LastHeartbeatAt:    &now,
		CreatedAt:          now,
		UpdatedAt:          now,
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
	if agentExecution == nil || agentExecution.ID != "agent_1" || agentExecution.HeartbeatCount != 2 || agentExecution.NativeSessionID == nil || *agentExecution.NativeSessionID != nativeSessionID {
		t.Fatalf("AgentExecutions.GetByID() = %#v, want agent_1 with heartbeat_count=2 and native session", agentExecution)
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

func TestRepositoriesStatusAggregates(t *testing.T) {
	t.Parallel()

	coordinator := openMigratedCoordinatorForRepositories(t)
	ctx := context.Background()
	repos := NewRepositories(coordinator.DB())
	now := "2026-04-11T12:00:00.000Z"
	if err := repos.Projects.Upsert(ctx, ProjectRecord{ID: "project_1", Name: "Looper", RepoPath: "/tmp/looper", Archived: false, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	for _, loop := range []LoopRecord{
		{ID: "loop_reviewer_running", Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Status: "running", CreatedAt: now, UpdatedAt: now},
		{ID: "loop_reviewer_waiting", Seq: 2, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Status: "waiting", CreatedAt: now, UpdatedAt: now},
		{ID: "loop_worker_failed", Seq: 3, ProjectID: "project_1", Type: "worker", TargetType: "project", Status: "failed", CreatedAt: now, UpdatedAt: now},
	} {
		if err := repos.Loops.Upsert(ctx, loop); err != nil {
			t.Fatalf("Loops.Upsert(%s) error = %v", loop.ID, err)
		}
	}

	largeCheckpoint := make([]byte, 64*1024)
	for i := range largeCheckpoint {
		largeCheckpoint[i] = 'x'
	}
	checkpointJSON := string(largeCheckpoint)
	for _, run := range []RunRecord{
		{ID: "run_running", LoopID: "loop_reviewer_running", Status: "running", CheckpointJSON: &checkpointJSON, StartedAt: now, CreatedAt: now, UpdatedAt: now},
		{ID: "run_failed", LoopID: "loop_worker_failed", Status: "failed", CheckpointJSON: &checkpointJSON, StartedAt: now, CreatedAt: now, UpdatedAt: now},
		{ID: "run_completed", LoopID: "loop_reviewer_waiting", Status: "completed", CheckpointJSON: &checkpointJSON, StartedAt: now, CreatedAt: now, UpdatedAt: now},
	} {
		if err := repos.Runs.Upsert(ctx, run); err != nil {
			t.Fatalf("Runs.Upsert(%s) error = %v", run.ID, err)
		}
	}

	for _, item := range []QueueItemRecord{
		{ID: "queue_queued", Type: "reviewer", TargetType: "pull_request", TargetID: "pr:1", DedupeKey: "reviewer:1", Priority: 1, Status: "queued", AvailableAt: now, MaxAttempts: 3, CreatedAt: now, UpdatedAt: now},
		{ID: "queue_running", Type: "worker", TargetType: "project", TargetID: "project:1", DedupeKey: "worker:1", Priority: 1, Status: "running", AvailableAt: now, MaxAttempts: 3, CreatedAt: now, UpdatedAt: now},
		{ID: "queue_failed", Type: "fixer", TargetType: "project", TargetID: "project:2", DedupeKey: "fixer:2", Priority: 1, Status: "failed", AvailableAt: now, MaxAttempts: 3, CreatedAt: now, UpdatedAt: now},
	} {
		if err := repos.Queue.Upsert(ctx, item); err != nil {
			t.Fatalf("Queue.Upsert(%s) error = %v", item.ID, err)
		}
	}

	loopCounts, err := repos.Loops.CountByTypeAndStatus(ctx)
	if err != nil {
		t.Fatalf("Loops.CountByTypeAndStatus() error = %v", err)
	}
	if got := loopCounts["reviewer"]["running"]; got != 1 {
		t.Fatalf("reviewer/running loop count = %d, want 1", got)
	}
	if got := loopCounts["reviewer"]["waiting"]; got != 1 {
		t.Fatalf("reviewer/waiting loop count = %d, want 1", got)
	}
	if got := loopCounts["worker"]["failed"]; got != 1 {
		t.Fatalf("worker/failed loop count = %d, want 1", got)
	}

	runCounts, err := repos.Runs.CountByStatus(ctx)
	if err != nil {
		t.Fatalf("Runs.CountByStatus() error = %v", err)
	}
	if got := runCounts["running"]; got != 1 {
		t.Fatalf("running run count = %d, want 1", got)
	}
	if got := runCounts["failed"]; got != 1 {
		t.Fatalf("failed run count = %d, want 1", got)
	}
	if got := runCounts["completed"]; got != 1 {
		t.Fatalf("completed run count = %d, want 1", got)
	}

	queueCounts, err := repos.Queue.CountByAllStatuses(ctx)
	if err != nil {
		t.Fatalf("Queue.CountByAllStatuses() error = %v", err)
	}
	if got := queueCounts["queued"]; got != 1 {
		t.Fatalf("queued queue count = %d, want 1", got)
	}
	if got := queueCounts["running"]; got != 1 {
		t.Fatalf("running queue count = %d, want 1", got)
	}
	if got := queueCounts["failed"]; got != 1 {
		t.Fatalf("failed queue count = %d, want 1", got)
	}
}

func TestRepositoriesRoundTripForSweeperCasesAndProposals(t *testing.T) {
	t.Parallel()

	coordinator := openMigratedCoordinatorForRepositories(t)
	ctx := context.Background()
	repos := NewRepositories(coordinator.DB())

	now := "2026-04-11T12:00:00.000Z"
	updated := "2026-04-11T12:05:00.000Z"
	projectID := "project_sweeper"
	if err := repos.Projects.Upsert(ctx, ProjectRecord{
		ID:        projectID,
		Name:      "Sweeper Project",
		RepoPath:  "/tmp/sweeper-project",
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	repo := "acme/looper"
	category := "stale"
	confidence := int64(87)
	warningCommentID := int64(101)
	warningMarkerUUID := "sweeper_marker_1"
	lastProposalID := "proposal_2"
	fingerprintJSON := `{"hash":"abc"}`
	lastHumanActivityAt := "2026-04-10T10:00:00.000Z"
	warnedAt := "2026-04-12T09:00:00.000Z"
	closeDueAt := "2026-04-19T09:00:00.000Z"
	terminalOutcome := "closed"
	terminalAt := "2026-04-20T09:00:00.000Z"

	if err := repos.SweeperCases.Upsert(ctx, SweeperCaseRecord{
		ID:                     "case_1",
		ProjectID:              projectID,
		Repo:                   repo,
		TargetType:             "pull_request",
		TargetNumber:           42,
		Status:                 "pending",
		CurrentPhase:           "warn",
		CurrentCategory:        &category,
		CurrentConfidenceScore: &confidence,
		WarningCommentID:       &warningCommentID,
		WarningMarkerUUID:      &warningMarkerUUID,
		LastProposalID:         &lastProposalID,
		LastFingerprintJSON:    &fingerprintJSON,
		LastHumanActivityAt:    &lastHumanActivityAt,
		WarnedAt:               &warnedAt,
		CloseDueAt:             &closeDueAt,
		TerminalOutcome:        &terminalOutcome,
		TerminalAt:             &terminalAt,
		CreatedAt:              now,
		UpdatedAt:              now,
	}); err != nil {
		t.Fatalf("SweeperCases.Upsert(case_1 initial) error = %v", err)
	}

	if err := repos.SweeperCases.Upsert(ctx, SweeperCaseRecord{
		ID:           "case_2",
		ProjectID:    projectID,
		Repo:         repo,
		TargetType:   "issue",
		TargetNumber: 99,
		Status:       "terminal",
		CurrentPhase: "terminal",
		CreatedAt:    now,
		UpdatedAt:    "2026-04-11T12:01:00.000Z",
	}); err != nil {
		t.Fatalf("SweeperCases.Upsert(case_2) error = %v", err)
	}

	if err := repos.SweeperCases.Upsert(ctx, SweeperCaseRecord{
		ID:           "case_3",
		ProjectID:    projectID,
		Repo:         repo,
		TargetType:   "pull_request",
		TargetNumber: 7,
		Status:       "quarantined",
		CurrentPhase: "prefilter",
		CreatedAt:    now,
		UpdatedAt:    "2026-04-11T12:02:00.000Z",
	}); err != nil {
		t.Fatalf("SweeperCases.Upsert(case_3) error = %v", err)
	}

	if err := repos.SweeperCases.Upsert(ctx, SweeperCaseRecord{
		ID:           "case_4",
		ProjectID:    projectID,
		Repo:         repo,
		TargetType:   "issue",
		TargetNumber: 8,
		Status:       "pending",
		CurrentPhase: "warn",
		CreatedAt:    now,
		UpdatedAt:    "2026-04-11T12:02:30.000Z",
	}); err != nil {
		t.Fatalf("SweeperCases.Upsert(case_4) error = %v", err)
	}

	updatedConfidence := int64(91)
	if err := repos.SweeperCases.Upsert(ctx, SweeperCaseRecord{
		ID:                     "case_1",
		ProjectID:              projectID,
		Repo:                   repo,
		TargetType:             "pull_request",
		TargetNumber:           42,
		Status:                 "terminal",
		CurrentPhase:           "terminal",
		CurrentCategory:        &category,
		CurrentConfidenceScore: &updatedConfidence,
		WarningCommentID:       &warningCommentID,
		WarningMarkerUUID:      &warningMarkerUUID,
		LastProposalID:         &lastProposalID,
		LastFingerprintJSON:    &fingerprintJSON,
		LastHumanActivityAt:    &lastHumanActivityAt,
		WarnedAt:               &warnedAt,
		CloseDueAt:             &closeDueAt,
		TerminalOutcome:        &terminalOutcome,
		TerminalAt:             &terminalAt,
		CreatedAt:              now,
		UpdatedAt:              updated,
	}); err != nil {
		t.Fatalf("SweeperCases.Upsert(case_1 update) error = %v", err)
	}

	caseByID, err := repos.SweeperCases.GetByID(ctx, "case_1")
	if err != nil {
		t.Fatalf("SweeperCases.GetByID() error = %v", err)
	}
	if caseByID == nil || caseByID.Status != "terminal" || caseByID.CurrentPhase != "terminal" || caseByID.CurrentConfidenceScore == nil || *caseByID.CurrentConfidenceScore != updatedConfidence {
		t.Fatalf("SweeperCases.GetByID() = %#v, want updated terminal case", caseByID)
	}

	caseByTarget, err := repos.SweeperCases.GetByProjectRepoTarget(ctx, projectID, repo, "pull_request", 42)
	if err != nil {
		t.Fatalf("SweeperCases.GetByProjectRepoTarget() error = %v", err)
	}
	if caseByTarget == nil || caseByTarget.ID != "case_1" {
		t.Fatalf("SweeperCases.GetByProjectRepoTarget() = %#v, want case_1", caseByTarget)
	}

	phaseCases, err := repos.SweeperCases.ListByProjectRepoPhase(ctx, projectID, repo, "terminal")
	if err != nil {
		t.Fatalf("SweeperCases.ListByProjectRepoPhase() error = %v", err)
	}
	if len(phaseCases) != 2 || phaseCases[0].ID != "case_1" || phaseCases[1].ID != "case_2" {
		t.Fatalf("SweeperCases.ListByProjectRepoPhase() = %#v, want [case_1 case_2]", phaseCases)
	}

	statusCases, err := repos.SweeperCases.ListByProjectRepoStatus(ctx, projectID, repo, "quarantined")
	if err != nil {
		t.Fatalf("SweeperCases.ListByProjectRepoStatus() error = %v", err)
	}
	if len(statusCases) != 1 || statusCases[0].ID != "case_3" {
		t.Fatalf("SweeperCases.ListByProjectRepoStatus() = %#v, want [case_3]", statusCases)
	}

	caseList, err := repos.SweeperCases.ListByProjectRepo(ctx, projectID, repo, 2)
	if err != nil {
		t.Fatalf("SweeperCases.ListByProjectRepo() error = %v", err)
	}
	if len(caseList) != 2 || caseList[0].ID != "case_1" || caseList[1].ID != "case_4" {
		t.Fatalf("SweeperCases.ListByProjectRepo() = %#v, want [case_1 case_4]", caseList)
	}

	summary1 := "proposal one"
	rationale1 := "first rationale"
	marker1 := "marker_1"
	validation1 := "passed"
	applyStatus1 := "pending"
	if err := repos.SweeperProposals.Insert(ctx, SweeperProposalRecord{
		ID:               "proposal_1",
		CaseID:           "case_1",
		ProjectID:        projectID,
		Repo:             repo,
		TargetType:       "pull_request",
		TargetNumber:     42,
		SchemaVersion:    1,
		ProposerKind:     "phase1a",
		FactBundleJSON:   `{"number":42}`,
		FingerprintJSON:  `{"hash":"one"}`,
		ProposalJSON:     `{"decision":"warn"}`,
		Decision:         "warn",
		Category:         "stale",
		ConfidenceScore:  80,
		Summary:          &summary1,
		Rationale:        &rationale1,
		MarkerUUID:       &marker1,
		ValidationStatus: &validation1,
		ApplyStatus:      &applyStatus1,
		CreatedAt:        now,
	}); err != nil {
		t.Fatalf("SweeperProposals.Insert(proposal_1) error = %v", err)
	}

	if err := repos.SweeperProposals.Insert(ctx, SweeperProposalRecord{
		ID:               "proposal_3",
		CaseID:           "case_4",
		ProjectID:        projectID,
		Repo:             repo,
		TargetType:       "issue",
		TargetNumber:     8,
		SchemaVersion:    1,
		ProposerKind:     "phase1a",
		FactBundleJSON:   `{"number":8}`,
		FingerprintJSON:  `{"hash":"three"}`,
		ProposalJSON:     `{"decision":"warn"}`,
		Decision:         "warn",
		Category:         "stale",
		ConfidenceScore:  75,
		ValidationStatus: &validation1,
		ApplyStatus:      &applyStatus1,
		CreatedAt:        "2026-04-11T12:00:30.000Z",
	}); err != nil {
		t.Fatalf("SweeperProposals.Insert(proposal_3) error = %v", err)
	}

	summary2 := "proposal two"
	created2 := "2026-04-11T12:03:00.000Z"
	if err := repos.SweeperProposals.Insert(ctx, SweeperProposalRecord{
		ID:              "proposal_2",
		CaseID:          "case_1",
		ProjectID:       projectID,
		Repo:            repo,
		TargetType:      "pull_request",
		TargetNumber:    42,
		SchemaVersion:   1,
		ProposerKind:    "phase1a",
		FactBundleJSON:  `{"number":42,"latest":true}`,
		FingerprintJSON: `{"hash":"two"}`,
		ProposalJSON:    `{"decision":"close"}`,
		Decision:        "close",
		Category:        "abandoned_pr",
		ConfidenceScore: 95,
		Summary:         &summary2,
		CreatedAt:       created2,
	}); err != nil {
		t.Fatalf("SweeperProposals.Insert(proposal_2) error = %v", err)
	}

	proposalByID, err := repos.SweeperProposals.GetByID(ctx, "proposal_1")
	if err != nil {
		t.Fatalf("SweeperProposals.GetByID() error = %v", err)
	}
	if proposalByID == nil || proposalByID.Decision != "warn" || proposalByID.ApplyStatus == nil || *proposalByID.ApplyStatus != "pending" {
		t.Fatalf("SweeperProposals.GetByID() = %#v, want proposal_1 with pending apply status", proposalByID)
	}

	latestProposal, err := repos.SweeperProposals.GetLatestByCaseID(ctx, "case_1")
	if err != nil {
		t.Fatalf("SweeperProposals.GetLatestByCaseID() error = %v", err)
	}
	if latestProposal == nil || latestProposal.ID != "proposal_2" {
		t.Fatalf("SweeperProposals.GetLatestByCaseID() = %#v, want proposal_2", latestProposal)
	}

	proposalList, err := repos.SweeperProposals.ListByCaseID(ctx, "case_1")
	if err != nil {
		t.Fatalf("SweeperProposals.ListByCaseID() error = %v", err)
	}
	if len(proposalList) != 2 || proposalList[0].ID != "proposal_2" || proposalList[1].ID != "proposal_1" {
		t.Fatalf("SweeperProposals.ListByCaseID() = %#v, want [proposal_2 proposal_1]", proposalList)
	}

	proposalRepoList, err := repos.SweeperProposals.ListByProjectRepo(ctx, projectID, repo, 2)
	if err != nil {
		t.Fatalf("SweeperProposals.ListByProjectRepo() error = %v", err)
	}
	if len(proposalRepoList) != 2 || proposalRepoList[0].ID != "proposal_2" || proposalRepoList[1].ID != "proposal_3" {
		t.Fatalf("SweeperProposals.ListByProjectRepo() = %#v, want [proposal_2 proposal_3]", proposalRepoList)
	}

	applySummary := "applied successfully"
	applyError := ""
	appliedAt := "2026-04-11T12:04:00.000Z"
	if err := repos.SweeperProposals.UpdateApplyReceipt(ctx, "proposal_2", "applied", &applySummary, &applyError, &appliedAt); err != nil {
		t.Fatalf("SweeperProposals.UpdateApplyReceipt() error = %v", err)
	}

	updatedProposal, err := repos.SweeperProposals.GetByID(ctx, "proposal_2")
	if err != nil {
		t.Fatalf("SweeperProposals.GetByID(updated) error = %v", err)
	}
	if updatedProposal == nil || updatedProposal.ApplyStatus == nil || *updatedProposal.ApplyStatus != "applied" || updatedProposal.ApplySummary == nil || *updatedProposal.ApplySummary != applySummary || updatedProposal.AppliedAt == nil || *updatedProposal.AppliedAt != appliedAt {
		t.Fatalf("SweeperProposals.GetByID(updated) = %#v, want applied receipt fields", updatedProposal)
	}

	countAppliedWarn, err := repos.SweeperProposals.CountAppliedByRepoAndDecisionSince(ctx, projectID, repo, "close", "2026-04-11T00:00:00.000Z")
	if err != nil {
		t.Fatalf("SweeperProposals.CountAppliedByRepoAndDecisionSince() error = %v", err)
	}
	if countAppliedWarn != 0 {
		t.Fatalf("SweeperProposals.CountAppliedByRepoAndDecisionSince(close) = %d, want 0 because apply status is non-canonical", countAppliedWarn)
	}
	if err := repos.SweeperProposals.UpdateApplyReceipt(ctx, "proposal_2", "completed_closed", &applySummary, &applyError, &appliedAt); err != nil {
		t.Fatalf("SweeperProposals.UpdateApplyReceipt(completed_closed) error = %v", err)
	}
	countAppliedClose, err := repos.SweeperProposals.CountAppliedByRepoAndDecisionSince(ctx, projectID, repo, "close", "2026-04-11T00:00:00.000Z")
	if err != nil {
		t.Fatalf("SweeperProposals.CountAppliedByRepoAndDecisionSince(close) error = %v", err)
	}
	if countAppliedClose != 1 {
		t.Fatalf("SweeperProposals.CountAppliedByRepoAndDecisionSince(close) = %d, want 1", countAppliedClose)
	}
	inflightWarn, err := repos.SweeperProposals.CountInflightByRepoAndDecision(ctx, projectID, repo, "warn")
	if err != nil {
		t.Fatalf("SweeperProposals.CountInflightByRepoAndDecision(warn) error = %v", err)
	}
	if inflightWarn != 1 {
		t.Fatalf("SweeperProposals.CountInflightByRepoAndDecision(warn) = %d, want 1", inflightWarn)
	}
	if err := repos.SweeperProposals.UpdateApplyReceipt(ctx, "proposal_3", "failed_retryable", &applySummary, &applyError, nil); err != nil {
		t.Fatalf("SweeperProposals.UpdateApplyReceipt(failed_retryable) error = %v", err)
	}
	inflightWarn, err = repos.SweeperProposals.CountInflightByRepoAndDecision(ctx, projectID, repo, "warn")
	if err != nil {
		t.Fatalf("SweeperProposals.CountInflightByRepoAndDecision(warn after failed_retryable) error = %v", err)
	}
	if inflightWarn != 0 {
		t.Fatalf("SweeperProposals.CountInflightByRepoAndDecision(warn after failed_retryable) = %d, want 0", inflightWarn)
	}
}

func TestRunsGetLatestByLoopIDBreaksStartedAtTiesByCreatedAt(t *testing.T) {
	t.Parallel()

	coordinator := openMigratedCoordinatorForRepositories(t)
	ctx := context.Background()
	repos := NewRepositories(coordinator.DB())

	now := "2026-04-11T12:00:00.000Z"
	if err := repos.Projects.Upsert(ctx, ProjectRecord{
		ID:        "project_latest_run",
		Name:      "Looper",
		RepoPath:  "/tmp/looper",
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := repos.Loops.Upsert(ctx, LoopRecord{
		ID:         "loop_latest_run",
		Seq:        1,
		ProjectID:  "project_latest_run",
		Type:       "reviewer",
		TargetType: "project",
		Status:     "idle",
		CreatedAt:  now,
		UpdatedAt:  now,
	}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	startedAt := "2026-04-11T12:00:01.000Z"
	olderCreatedAt := "2026-04-11T12:00:02.000Z"
	newerCreatedAt := "2026-04-11T12:00:03.000Z"
	for _, run := range []RunRecord{
		{ID: "z_run_older", LoopID: "loop_latest_run", Status: "completed", StartedAt: startedAt, CreatedAt: olderCreatedAt, UpdatedAt: olderCreatedAt},
		{ID: "a_run_newer", LoopID: "loop_latest_run", Status: "running", StartedAt: startedAt, CreatedAt: newerCreatedAt, UpdatedAt: newerCreatedAt},
	} {
		if err := repos.Runs.Upsert(ctx, run); err != nil {
			t.Fatalf("Runs.Upsert(%s) error = %v", run.ID, err)
		}
	}

	latestRun, err := repos.Runs.GetLatestByLoopID(ctx, "loop_latest_run")
	if err != nil {
		t.Fatalf("Runs.GetLatestByLoopID() error = %v", err)
	}
	if latestRun == nil || latestRun.ID != "a_run_newer" {
		t.Fatalf("Runs.GetLatestByLoopID() = %#v, want a_run_newer", latestRun)
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
	for index, loopID := range []string{"loop_1", "loop_2", "loop_3"} {
		if err := repos.Loops.Upsert(ctx, LoopRecord{ID: loopID, Seq: int64(index + 1), ProjectID: "project_1", Type: "reviewer", TargetType: "project", Status: "running", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("Loops.Upsert(%s) error = %v", loopID, err)
		}
	}

	for _, run := range []RunRecord{
		{ID: "run_1", LoopID: "loop_1", Status: "running", StartedAt: "2026-04-11T12:00:00.000Z", CreatedAt: now, UpdatedAt: now},
		{ID: "run_3", LoopID: "loop_3", Status: "running", StartedAt: "2026-04-11T12:00:00.000Z", CreatedAt: now, UpdatedAt: now},
		{ID: "run_2", LoopID: "loop_2", Status: "running", StartedAt: "2026-04-11T12:01:00.000Z", CreatedAt: now, UpdatedAt: now},
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

func TestRunsEnforceOneRunningRunPerLoop(t *testing.T) {
	t.Parallel()

	coordinator := openMigratedCoordinatorForRepositories(t)
	ctx := context.Background()
	repos := NewRepositories(coordinator.DB())
	now := "2026-04-11T12:00:00.000Z"
	if err := repos.Projects.Upsert(ctx, ProjectRecord{ID: "project_running_unique", Name: "Looper", RepoPath: "/tmp/looper", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := repos.Loops.Upsert(ctx, LoopRecord{ID: "loop_running_unique", ProjectID: "project_running_unique", Type: "fixer", TargetType: "pull_request", Status: "running", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	if err := repos.Runs.Upsert(ctx, RunRecord{ID: "run_running_unique_1", LoopID: "loop_running_unique", Status: "running", StartedAt: now, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("first Runs.Upsert() error = %v", err)
	}
	if hasRunning, err := repos.Runs.HasRunningByLoopID(ctx, "loop_running_unique"); err != nil || !hasRunning {
		t.Fatalf("HasRunningByLoopID() = %v, %v; want true, nil", hasRunning, err)
	}
	if err := repos.Runs.Upsert(ctx, RunRecord{ID: "run_running_unique_2", LoopID: "loop_running_unique", Status: "running", StartedAt: now, CreatedAt: now, UpdatedAt: now}); err == nil {
		t.Fatal("second running Runs.Upsert() error = nil, want unique constraint failure")
	}
	if err := repos.Runs.Upsert(ctx, RunRecord{ID: "run_running_unique_2", LoopID: "loop_running_unique", Status: "success", StartedAt: now, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("terminal Runs.Upsert() error = %v", err)
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

func TestQueueRequeueFailedByIDRequiresMatchingLoopAndNoActiveQueue(t *testing.T) {
	t.Parallel()

	coordinator := openMigratedCoordinatorForRepositories(t)
	ctx := context.Background()
	repos := NewRepositories(coordinator.DB())

	now := "2026-04-11T12:00:00.000Z"
	if err := repos.Projects.Upsert(ctx, ProjectRecord{ID: "project_requeue", Name: "Looper", RepoPath: "/tmp/looper", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	for seq, loopID := range []string{"loop_a", "loop_b"} {
		if err := repos.Loops.Upsert(ctx, LoopRecord{ID: loopID, Seq: int64(seq + 1), ProjectID: "project_requeue", Type: "reviewer", TargetType: "pull_request", Status: "failed", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("Loops.Upsert(%s) error = %v", loopID, err)
		}
	}
	loopA := "loop_a"
	loopB := "loop_b"
	lastError := "PR head changed before publish"
	if err := repos.Queue.Upsert(ctx, QueueItemRecord{ID: "failed_a", LoopID: &loopA, Type: "reviewer", TargetType: "pull_request", TargetID: "pr:a", DedupeKey: "reviewer:a", Priority: QueuePriorityReviewer, Status: "failed", AvailableAt: now, Attempts: 3, MaxAttempts: 3, LastError: &lastError, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Queue.Upsert(failed_a) error = %v", err)
	}
	if err := repos.Queue.Upsert(ctx, QueueItemRecord{ID: "active_b", LoopID: &loopB, Type: "reviewer", TargetType: "pull_request", TargetID: "pr:b", DedupeKey: "reviewer:b", Priority: QueuePriorityReviewer, Status: "queued", AvailableAt: now, Attempts: 0, MaxAttempts: 3, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Queue.Upsert(active_b) error = %v", err)
	}

	affected, err := repos.Queue.RequeueFailedByID(ctx, loopB, "failed_a", now)
	if err != nil {
		t.Fatalf("RequeueFailedByID(wrong loop) error = %v", err)
	}
	if affected != 0 {
		t.Fatalf("RequeueFailedByID(wrong loop) affected = %d, want 0", affected)
	}
	item, _ := repos.Queue.GetByID(ctx, "failed_a")
	if item == nil || item.Status != "failed" {
		t.Fatalf("failed_a status = %#v, want still failed", item)
	}

	affected, err = repos.Queue.RequeueFailedByID(ctx, loopA, "failed_a", now)
	if err != nil {
		t.Fatalf("RequeueFailedByID(correct loop) error = %v", err)
	}
	if affected != 1 {
		t.Fatalf("RequeueFailedByID(correct loop) affected = %d, want 1", affected)
	}
	item, _ = repos.Queue.GetByID(ctx, "failed_a")
	if item == nil || item.Status != "queued" || item.LastError != nil || item.Attempts != 0 {
		t.Fatalf("failed_a = %#v, want requeued with cleared failure metadata", item)
	}
}

func TestQueueRequeueFailedByIDWithAttemptsPreservesAttemptBudget(t *testing.T) {
	t.Parallel()

	coordinator := openMigratedCoordinatorForRepositories(t)
	ctx := context.Background()
	repos := NewRepositories(coordinator.DB())

	now := "2026-04-11T12:00:00.000Z"
	if err := repos.Projects.Upsert(ctx, ProjectRecord{ID: "project_requeue_attempts", Name: "Looper", RepoPath: "/tmp/looper", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	loopID := "loop_requeue_attempts"
	if err := repos.Loops.Upsert(ctx, LoopRecord{ID: loopID, Seq: 1, ProjectID: "project_requeue_attempts", Type: "reviewer", TargetType: "pull_request", Status: "failed", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	lastError := "reviewer agent timed out"
	lastErrorKind := "retryable_transient"
	if err := repos.Queue.Upsert(ctx, QueueItemRecord{ID: "failed_attempts", LoopID: &loopID, Type: "reviewer", TargetType: "pull_request", TargetID: "pr:a", DedupeKey: "reviewer:attempts", Priority: QueuePriorityReviewer, Status: "failed", AvailableAt: now, Attempts: 3, MaxAttempts: 5, LastError: &lastError, LastErrorKind: &lastErrorKind, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	affected, err := repos.Queue.RequeueFailedByIDWithAttempts(ctx, loopID, "failed_attempts", now, 3)
	if err != nil {
		t.Fatalf("RequeueFailedByIDWithAttempts() error = %v", err)
	}
	if affected != 1 {
		t.Fatalf("RequeueFailedByIDWithAttempts() affected = %d, want 1", affected)
	}
	item, _ := repos.Queue.GetByID(ctx, "failed_attempts")
	if item == nil || item.Status != "queued" || item.Attempts != 3 || item.LastError != nil || item.LastErrorKind != nil {
		t.Fatalf("failed_attempts = %#v, want queued with preserved attempts and cleared failure metadata", item)
	}
}

func TestQueueCreateOrGetActiveByDedupeAllowsNewActiveAfterInactiveHistory(t *testing.T) {
	t.Parallel()

	coordinator := openMigratedCoordinatorForRepositories(t)
	ctx := context.Background()
	repos := NewRepositories(coordinator.DB())

	now := "2026-04-11T12:00:00.000Z"
	projectID := "project_history"
	loopID := "loop_history"
	repoName := "acme/looper"
	prNumber := int64(42)
	dedupeKey := "reviewer:project_history:loop_history:acme/looper:42"
	if err := repos.Projects.Upsert(ctx, ProjectRecord{ID: projectID, Name: "Looper", RepoPath: "/tmp/looper", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := repos.Loops.Upsert(ctx, LoopRecord{ID: loopID, Seq: 1, ProjectID: projectID, Type: "reviewer", TargetType: "pull_request", Status: "failed", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	for _, item := range []QueueItemRecord{
		{ID: "queue_completed", ProjectID: &projectID, LoopID: &loopID, Type: "reviewer", TargetType: "pull_request", TargetID: "pr:42", Repo: &repoName, PRNumber: &prNumber, DedupeKey: dedupeKey, Priority: QueuePriorityReviewer, Status: "completed", AvailableAt: now, Attempts: 1, MaxAttempts: 3, FinishedAt: &now, CreatedAt: now, UpdatedAt: now},
		{ID: "queue_failed", ProjectID: &projectID, LoopID: &loopID, Type: "reviewer", TargetType: "pull_request", TargetID: "pr:42", Repo: &repoName, PRNumber: &prNumber, DedupeKey: dedupeKey, Priority: QueuePriorityReviewer, Status: "failed", AvailableAt: now, Attempts: 3, MaxAttempts: 3, FinishedAt: &now, CreatedAt: now, UpdatedAt: now},
		{ID: "queue_cancelled", ProjectID: &projectID, LoopID: &loopID, Type: "reviewer", TargetType: "pull_request", TargetID: "pr:42", Repo: &repoName, PRNumber: &prNumber, DedupeKey: dedupeKey, Priority: QueuePriorityReviewer, Status: "cancelled", AvailableAt: now, Attempts: 0, MaxAttempts: 3, FinishedAt: &now, CreatedAt: now, UpdatedAt: now},
	} {
		if err := repos.Queue.Upsert(ctx, item); err != nil {
			t.Fatalf("Queue.Upsert(%s) error = %v", item.ID, err)
		}
	}

	created, didCreate, err := repos.Queue.CreateOrGetActiveByDedupe(ctx, QueueItemRecord{ID: "queue_new", ProjectID: &projectID, LoopID: &loopID, Type: "reviewer", TargetType: "pull_request", TargetID: "pr:42", Repo: &repoName, PRNumber: &prNumber, DedupeKey: dedupeKey, Priority: QueuePriorityReviewer, Status: "queued", AvailableAt: now, Attempts: 0, MaxAttempts: 3, CreatedAt: now, UpdatedAt: now})
	if err != nil {
		t.Fatalf("Queue.CreateOrGetActiveByDedupe() error = %v", err)
	}
	if !didCreate || created.ID != "queue_new" {
		t.Fatalf("Queue.CreateOrGetActiveByDedupe() = (%#v, %v), want newly created queue_new", created, didCreate)
	}

	active, err := repos.Queue.FindActiveByDedupe(ctx, dedupeKey)
	if err != nil {
		t.Fatalf("Queue.FindActiveByDedupe() error = %v", err)
	}
	if active == nil || active.ID != "queue_new" {
		t.Fatalf("Queue.FindActiveByDedupe() = %#v, want queue_new", active)
	}
}

func TestQueueCreateOrGetActiveByDedupeKeepsOneActiveItemUnderConcurrency(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		dedupeKey string
		queueType string
		priority  int64
	}{
		{name: "reviewer", dedupeKey: "reviewer:project_1:loop_1:acme/looper:42", queueType: "reviewer", priority: QueuePriorityReviewer},
		{name: "fixer", dedupeKey: "fixer:project_1:loop_1:acme/looper:42:head-1:hash-1", queueType: "fixer", priority: QueuePriorityFixer},
	} {
		t.Run(tc.name, func(t *testing.T) {
			coordinator := openMigratedCoordinatorForRepositories(t)
			ctx := context.Background()
			repos := NewRepositories(coordinator.DB())

			now := "2026-04-11T12:00:00.000Z"
			projectID := "project_1"
			loopID := "loop_1"
			repoName := "acme/looper"
			prNumber := int64(42)
			if err := repos.Projects.Upsert(ctx, ProjectRecord{ID: projectID, Name: "Looper", RepoPath: "/tmp/looper", CreatedAt: now, UpdatedAt: now}); err != nil {
				t.Fatalf("Projects.Upsert() error = %v", err)
			}
			if err := repos.Loops.Upsert(ctx, LoopRecord{ID: loopID, Seq: 1, ProjectID: projectID, Type: tc.queueType, TargetType: "pull_request", Status: "queued", CreatedAt: now, UpdatedAt: now}); err != nil {
				t.Fatalf("Loops.Upsert() error = %v", err)
			}

			const attempts = 8
			var wg sync.WaitGroup
			start := make(chan struct{})
			results := make(chan QueueItemRecord, attempts)
			errs := make(chan error, attempts)
			createdCount := make(chan bool, attempts)
			for i := 0; i < attempts; i++ {
				wg.Add(1)
				go func(index int) {
					defer wg.Done()
					<-start
					item, didCreate, err := repos.Queue.CreateOrGetActiveByDedupe(ctx, QueueItemRecord{ID: fmt.Sprintf("queue_%d", index), ProjectID: &projectID, LoopID: &loopID, Type: tc.queueType, TargetType: "pull_request", TargetID: "pr:42", Repo: &repoName, PRNumber: &prNumber, DedupeKey: tc.dedupeKey, Priority: tc.priority, Status: "queued", AvailableAt: now, Attempts: 0, MaxAttempts: 3, CreatedAt: now, UpdatedAt: now})
					if err != nil {
						errs <- err
						return
					}
					createdCount <- didCreate
					results <- item
				}(i)
			}
			close(start)
			wg.Wait()
			close(errs)
			close(results)
			close(createdCount)

			for err := range errs {
				if err != nil {
					t.Fatalf("Queue.CreateOrGetActiveByDedupe() error = %v", err)
				}
			}
			winnerID := ""
			for item := range results {
				if winnerID == "" {
					winnerID = item.ID
					continue
				}
				if item.ID != winnerID {
					t.Fatalf("concurrent item ID = %q, want %q", item.ID, winnerID)
				}
			}
			creates := 0
			for didCreate := range createdCount {
				if didCreate {
					creates++
				}
			}
			if creates != 1 {
				t.Fatalf("created count = %d, want 1", creates)
			}

			items, err := repos.Queue.List(ctx)
			if err != nil {
				t.Fatalf("Queue.List() error = %v", err)
			}
			if len(items) != 1 {
				t.Fatalf("len(Queue.List()) = %d, want 1", len(items))
			}
			if items[0].DedupeKey != tc.dedupeKey || items[0].Status != "queued" {
				t.Fatalf("Queue.List()[0] = %#v, want queued item for %q", items[0], tc.dedupeKey)
			}
		})
	}
}

func TestQueueUpsertActiveByDedupeOrGetExistingReturnsActiveReviewerOrFixer(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		queueType string
		priority  int64
	}{
		{name: "reviewer", queueType: "reviewer", priority: QueuePriorityReviewer},
		{name: "fixer", queueType: "fixer", priority: QueuePriorityFixer},
	} {
		t.Run(tc.name, func(t *testing.T) {
			coordinator := openMigratedCoordinatorForRepositories(t)
			ctx := context.Background()
			repos := NewRepositories(coordinator.DB())

			now := "2026-04-11T12:00:00.000Z"
			projectID := "project_1"
			loopID := "loop_1"
			repoName := "acme/looper"
			prNumber := int64(42)
			dedupeKey := fmt.Sprintf("%s:project_1:loop_1:acme/looper:42", tc.queueType)
			if tc.queueType == "fixer" {
				dedupeKey = "fixer:project_1:loop_1:acme/looper:42:head-1:hash-1"
			}
			if err := repos.Projects.Upsert(ctx, ProjectRecord{ID: projectID, Name: "Looper", RepoPath: "/tmp/looper", CreatedAt: now, UpdatedAt: now}); err != nil {
				t.Fatalf("Projects.Upsert() error = %v", err)
			}
			if err := repos.Loops.Upsert(ctx, LoopRecord{ID: loopID, Seq: 1, ProjectID: projectID, Type: tc.queueType, TargetType: "pull_request", Status: "queued", CreatedAt: now, UpdatedAt: now}); err != nil {
				t.Fatalf("Loops.Upsert() error = %v", err)
			}
			existing := QueueItemRecord{ID: "queue_existing", ProjectID: &projectID, LoopID: &loopID, Type: tc.queueType, TargetType: "pull_request", TargetID: "pr:42", Repo: &repoName, PRNumber: &prNumber, DedupeKey: dedupeKey, Priority: tc.priority, Status: "queued", AvailableAt: now, Attempts: 0, MaxAttempts: 3, CreatedAt: now, UpdatedAt: now}
			if err := repos.Queue.Upsert(ctx, existing); err != nil {
				t.Fatalf("Queue.Upsert(existing) error = %v", err)
			}

			persisted, didPersist, err := repos.Queue.UpsertActiveByDedupeOrGetExisting(ctx, QueueItemRecord{ID: "queue_racing", ProjectID: &projectID, LoopID: &loopID, Type: tc.queueType, TargetType: "pull_request", TargetID: "pr:42", Repo: &repoName, PRNumber: &prNumber, DedupeKey: dedupeKey, Priority: tc.priority, Status: "queued", AvailableAt: now, Attempts: 0, MaxAttempts: 3, CreatedAt: now, UpdatedAt: now})
			if err != nil {
				t.Fatalf("Queue.UpsertActiveByDedupeOrGetExisting() error = %v", err)
			}
			if didPersist {
				t.Fatal("Queue.UpsertActiveByDedupeOrGetExisting() persisted = true, want false")
			}
			if persisted.ID != existing.ID {
				t.Fatalf("Queue.UpsertActiveByDedupeOrGetExisting() = %#v, want existing queue item", persisted)
			}
		})
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
	if err := repos.Queue.Fail(ctx, QueueFailInput{ID: "qi_fail", Attempts: 4, FinishedAt: finished, ErrorMessage: &failReason, ErrorKind: "manual_intervention", UpdatedAt: finished}); err != nil {
		t.Fatalf("Queue.Fail() error = %v", err)
	}
	gotFailed, err := repos.Queue.GetByID(ctx, "qi_fail")
	if err != nil {
		t.Fatalf("Queue.GetByID(qi_fail) error = %v", err)
	}
	if gotFailed == nil || gotFailed.Status != "manual_intervention" || gotFailed.LastError == nil || *gotFailed.LastError != failReason || gotFailed.Attempts != 4 {
		t.Fatalf("Queue.GetByID(qi_fail) after fail = %#v, want manual_intervention", gotFailed)
	}
}

func TestTargetedListRepositories(t *testing.T) {
	t.Parallel()

	coordinator := openMigratedCoordinatorForRepositories(t)
	ctx := context.Background()
	repos := NewRepositories(coordinator.DB())

	now := "2026-04-11T12:00:00.000Z"
	later := "2026-04-11T12:10:00.000Z"
	projectID := "project_targeted"
	loopQueued := "loop_queued"
	loopManual := "loop_manual"
	loopDone := "loop_done"
	if err := repos.Projects.Upsert(ctx, ProjectRecord{ID: projectID, Name: "Looper", RepoPath: "/tmp/looper", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	for _, loop := range []LoopRecord{
		{ID: loopQueued, Seq: 1, ProjectID: projectID, Type: "worker", TargetType: "project", Status: "queued", CreatedAt: now, UpdatedAt: now},
		{ID: loopManual, Seq: 2, ProjectID: projectID, Type: "worker", TargetType: "project", Status: "failed", CreatedAt: now, UpdatedAt: later},
		{ID: loopDone, Seq: 3, ProjectID: projectID, Type: "worker", TargetType: "project", Status: "completed", CreatedAt: now, UpdatedAt: later},
	} {
		if err := repos.Loops.Upsert(ctx, loop); err != nil {
			t.Fatalf("Loops.Upsert(%s) error = %v", loop.ID, err)
		}
	}
	manualKind := "manual_intervention"
	checkpoint := `{"resumePolicy":"manual_intervention"}`
	for _, item := range []QueueItemRecord{
		{ID: "queue_queued", ProjectID: &projectID, LoopID: &loopQueued, Type: "worker", TargetType: "project", TargetID: projectID, DedupeKey: "queued", Priority: QueuePriorityWorker, Status: "queued", AvailableAt: now, Attempts: 0, MaxAttempts: 3, CreatedAt: now, UpdatedAt: now},
		{ID: "queue_manual", ProjectID: &projectID, LoopID: &loopManual, Type: "worker", TargetType: "project", TargetID: projectID, DedupeKey: "manual", Priority: QueuePriorityWorker, Status: "manual_intervention", AvailableAt: now, Attempts: 1, MaxAttempts: 3, LastErrorKind: &manualKind, CreatedAt: later, UpdatedAt: later},
		{ID: "queue_done_manual_old", ProjectID: &projectID, LoopID: &loopDone, Type: "worker", TargetType: "project", TargetID: projectID, DedupeKey: "done_manual_old", Priority: QueuePriorityWorker, Status: "manual_intervention", AvailableAt: now, Attempts: 1, MaxAttempts: 3, LastErrorKind: &manualKind, CreatedAt: now, UpdatedAt: now},
		{ID: "queue_done", ProjectID: &projectID, LoopID: &loopDone, Type: "worker", TargetType: "project", TargetID: projectID, DedupeKey: "done", Priority: QueuePriorityWorker, Status: "completed", AvailableAt: now, Attempts: 1, MaxAttempts: 3, CreatedAt: later, UpdatedAt: later},
	} {
		if err := repos.Queue.Upsert(ctx, item); err != nil {
			t.Fatalf("Queue.Upsert(%s) error = %v", item.ID, err)
		}
	}
	for _, run := range []RunRecord{
		{ID: "run_manual_old", LoopID: loopManual, Status: "running", StartedAt: now, CreatedAt: now, UpdatedAt: now},
		{ID: "run_manual_latest", LoopID: loopManual, Status: "failed", CheckpointJSON: &checkpoint, StartedAt: later, CreatedAt: later, UpdatedAt: later},
		{ID: "run_done", LoopID: loopDone, Status: "completed", StartedAt: later, CreatedAt: later, UpdatedAt: later},
	} {
		if err := repos.Runs.Upsert(ctx, run); err != nil {
			t.Fatalf("Runs.Upsert(%s) error = %v", run.ID, err)
		}
	}

	loopsByStatus, err := repos.Loops.ListByStatuses(ctx, []string{"queued", "running"})
	if err != nil {
		t.Fatalf("Loops.ListByStatuses() error = %v", err)
	}
	if len(loopsByStatus) != 1 || loopsByStatus[0].ID != loopQueued {
		t.Fatalf("Loops.ListByStatuses() = %#v, want queued loop only", loopsByStatus)
	}

	queueByStatus, err := repos.Queue.ListByStatuses(ctx, []string{"queued", "manual_intervention"})
	if err != nil {
		t.Fatalf("Queue.ListByStatuses() error = %v", err)
	}
	if len(queueByStatus) != 3 {
		t.Fatalf("len(Queue.ListByStatuses()) = %d, want 3", len(queueByStatus))
	}

	latestQueueByStatus, err := repos.Queue.ListLatestByLoopStatuses(ctx, []string{"queued", "manual_intervention"})
	if err != nil {
		t.Fatalf("Queue.ListLatestByLoopStatuses() error = %v", err)
	}
	if len(latestQueueByStatus) != 2 {
		t.Fatalf("len(Queue.ListLatestByLoopStatuses()) = %d, want 2", len(latestQueueByStatus))
	}
	latestQueueIDs := []string{latestQueueByStatus[0].ID, latestQueueByStatus[1].ID}
	sort.Strings(latestQueueIDs)
	if want := []string{"queue_manual", "queue_queued"}; !reflect.DeepEqual(latestQueueIDs, want) {
		t.Fatalf("Queue.ListLatestByLoopStatuses() ids = %#v, want %#v", latestQueueIDs, want)
	}

	latestRuns, err := repos.Runs.ListLatestByLoopIDs(ctx, []string{loopManual, loopDone})
	if err != nil {
		t.Fatalf("Runs.ListLatestByLoopIDs() error = %v", err)
	}
	gotRunIDs := []string{latestRuns[0].ID, latestRuns[1].ID}
	sort.Strings(gotRunIDs)
	if want := []string{"run_done", "run_manual_latest"}; !reflect.DeepEqual(gotRunIDs, want) {
		t.Fatalf("Runs.ListLatestByLoopIDs() ids = %#v, want %#v", gotRunIDs, want)
	}

}

func TestRunsListLatestByLoopStatusesAndResumePolicySkipsMalformedCheckpointJSON(t *testing.T) {
	t.Parallel()

	coordinator := openMigratedCoordinatorForRepositories(t)
	ctx := context.Background()
	repos := NewRepositories(coordinator.DB())

	now := "2026-04-11T12:00:00.000Z"
	later := "2026-04-11T12:10:00.000Z"
	projectID := "project_resume_policy"
	checkpoint := `{"resumePolicy":"manual_intervention"}`
	malformedCheckpoint := `{"resumePolicy":`

	if err := repos.Projects.Upsert(ctx, ProjectRecord{ID: projectID, Name: "Looper", RepoPath: "/tmp/looper", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	for _, loop := range []LoopRecord{
		{ID: "loop_valid", Seq: 1, ProjectID: projectID, Type: "worker", TargetType: "project", Status: "failed", CreatedAt: now, UpdatedAt: now},
		{ID: "loop_malformed", Seq: 2, ProjectID: projectID, Type: "worker", TargetType: "project", Status: "failed", CreatedAt: now, UpdatedAt: now},
	} {
		if err := repos.Loops.Upsert(ctx, loop); err != nil {
			t.Fatalf("Loops.Upsert(%s) error = %v", loop.ID, err)
		}
	}
	for _, run := range []RunRecord{
		{ID: "run_valid", LoopID: "loop_valid", Status: "failed", CheckpointJSON: &checkpoint, StartedAt: later, CreatedAt: later, UpdatedAt: later},
		{ID: "run_malformed", LoopID: "loop_malformed", Status: "failed", CheckpointJSON: &malformedCheckpoint, StartedAt: later, CreatedAt: later, UpdatedAt: later},
	} {
		if err := repos.Runs.Upsert(ctx, run); err != nil {
			t.Fatalf("Runs.Upsert(%s) error = %v", run.ID, err)
		}
	}

	runs, err := repos.Runs.ListLatestByLoopStatusesAndResumePolicy(ctx, []string{"failed"}, "manual_intervention")
	if err != nil {
		t.Fatalf("Runs.ListLatestByLoopStatusesAndResumePolicy() error = %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("len(Runs.ListLatestByLoopStatusesAndResumePolicy()) = %d, want 1", len(runs))
	}
	if runs[0].ID != "run_valid" {
		t.Fatalf("Runs.ListLatestByLoopStatusesAndResumePolicy()[0].ID = %q, want run_valid", runs[0].ID)
	}
}

func TestRepositoriesListByIDHelpersChunkLargeInput(t *testing.T) {
	t.Parallel()

	coordinator := openMigratedCoordinatorForRepositories(t)
	ctx := context.Background()
	repos := NewRepositories(coordinator.DB())
	now := "2026-04-11T12:00:00.000Z"
	if err := repos.Projects.Upsert(ctx, ProjectRecord{ID: "project_chunk", Name: "Looper", RepoPath: "/tmp/looper", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	loopIDs := make([]string, 0, sqliteMaxVariables+5)
	for i := 0; i < sqliteMaxVariables+5; i++ {
		loopID := fmt.Sprintf("loop_chunk_%04d", i)
		loopIDs = append(loopIDs, loopID)
		if err := repos.Loops.Upsert(ctx, LoopRecord{ID: loopID, Seq: int64(i + 1), ProjectID: "project_chunk", Type: "worker", TargetType: "project", Status: "paused", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("Loops.Upsert(%s) error = %v", loopID, err)
		}
		runID := fmt.Sprintf("run_chunk_%04d", i)
		if err := repos.Runs.Upsert(ctx, RunRecord{ID: runID, LoopID: loopID, Status: "failed", StartedAt: now, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("Runs.Upsert(%s) error = %v", runID, err)
		}
	}

	loopsList, err := repos.Loops.ListByIDs(ctx, loopIDs)
	if err != nil {
		t.Fatalf("Loops.ListByIDs() error = %v", err)
	}
	if got := len(loopsList); got != len(loopIDs) {
		t.Fatalf("len(Loops.ListByIDs()) = %d, want %d", got, len(loopIDs))
	}

	latestRuns, err := repos.Runs.ListLatestByLoopIDs(ctx, loopIDs)
	if err != nil {
		t.Fatalf("Runs.ListLatestByLoopIDs() error = %v", err)
	}
	if got := len(latestRuns); got != len(loopIDs) {
		t.Fatalf("len(Runs.ListLatestByLoopIDs()) = %d, want %d", got, len(loopIDs))
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

func TestQueueClaimNextOfTypeSkipsTerminatedAndStoppedLoops(t *testing.T) {
	ctx := context.Background()
	coordinator := openMigratedCoordinatorForRepositories(t)
	repos := NewRepositories(coordinator.DB())
	now := "2026-04-11T12:00:00.000Z"
	projectID := "project_queue_terminal"
	if err := repos.Projects.Upsert(ctx, ProjectRecord{ID: projectID, Name: "Queue Terminal", RepoPath: "/tmp/repo", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	for index, status := range []string{"terminated", "stopped"} {
		loopID := "loop_" + status
		if err := repos.Loops.Upsert(ctx, LoopRecord{ID: loopID, Seq: int64(index + 1), ProjectID: projectID, Type: "reviewer", TargetType: "pull_request", Status: status, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("Loops.Upsert(%s) error = %v", status, err)
		}
		if err := repos.Queue.Upsert(ctx, QueueItemRecord{ID: "queue_" + status, ProjectID: &projectID, LoopID: &loopID, Type: "reviewer", TargetType: "pull_request", TargetID: "pr:" + status, DedupeKey: "d_" + status, Priority: 1, Status: "queued", AvailableAt: now, Attempts: 0, MaxAttempts: 3, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("Queue.Upsert(%s) error = %v", status, err)
		}
	}

	claimed, err := repos.Queue.ClaimNextOfType(ctx, now, "worker", "reviewer")
	if err != nil {
		t.Fatalf("Queue.ClaimNextOfType() error = %v", err)
	}
	if claimed != nil {
		t.Fatalf("Queue.ClaimNextOfType() = %#v, want nil", claimed)
	}
}

func TestQueueStatsAndCleanupStaleQueued(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	coordinator := openMigratedCoordinatorForRepositories(t)
	repos := NewRepositories(coordinator.DB())
	now := "2026-04-11T12:00:00.000Z"
	projectID := "project_queue_stats"
	if err := repos.Projects.Upsert(ctx, ProjectRecord{ID: projectID, Name: "Looper", RepoPath: "/tmp/looper", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	for i, loop := range []LoopRecord{
		{ID: "loop_eligible", Seq: 1, ProjectID: projectID, Type: "worker", TargetType: "project", Status: "running", CreatedAt: now, UpdatedAt: now},
		{ID: "loop_future", Seq: 2, ProjectID: projectID, Type: "worker", TargetType: "project", Status: "running", CreatedAt: now, UpdatedAt: now},
		{ID: "loop_terminal", Seq: 3, ProjectID: projectID, Type: "worker", TargetType: "project", Status: "completed", CreatedAt: now, UpdatedAt: now},
		{ID: "loop_lock_wait", Seq: 4, ProjectID: projectID, Type: "worker", TargetType: "project", Status: "running", CreatedAt: now, UpdatedAt: now},
		{ID: "loop_lock_running", Seq: 5, ProjectID: projectID, Type: "worker", TargetType: "project", Status: "running", CreatedAt: now, UpdatedAt: now},
		{ID: "loop_reviewer", Seq: 6, ProjectID: projectID, Type: "reviewer", TargetType: "pull_request", Status: "running", CreatedAt: now, UpdatedAt: now},
		{ID: "loop_fixer", Seq: 7, ProjectID: projectID, Type: "fixer", TargetType: "pull_request", Status: "running", CreatedAt: now, UpdatedAt: now},
		{ID: "loop_stale", Seq: 8, ProjectID: projectID, Type: "worker", TargetType: "project", Status: "terminated", CreatedAt: now, UpdatedAt: now},
	} {
		loop.Seq = int64(i + 1)
		if err := repos.Loops.Upsert(ctx, loop); err != nil {
			t.Fatalf("Loops.Upsert(%s) error = %v", loop.ID, err)
		}
	}
	lockKey := "repo:acme/looper"
	repo := "acme/looper"
	prNumber := int64(42)
	for _, item := range []QueueItemRecord{
		{ID: "qi_eligible", ProjectID: &projectID, LoopID: strPtr("loop_eligible"), Type: "worker", TargetType: "project", TargetID: "project_queue_stats", DedupeKey: "eligible", Priority: 1, Status: "queued", AvailableAt: now, MaxAttempts: 3, CreatedAt: now, UpdatedAt: now},
		{ID: "qi_future", ProjectID: &projectID, LoopID: strPtr("loop_future"), Type: "worker", TargetType: "project", TargetID: "project_queue_stats", DedupeKey: "future", Priority: 1, Status: "queued", AvailableAt: "2026-04-11T12:10:00.000Z", MaxAttempts: 3, CreatedAt: now, UpdatedAt: now},
		{ID: "qi_terminal", ProjectID: &projectID, LoopID: strPtr("loop_terminal"), Type: "worker", TargetType: "project", TargetID: "project_queue_stats", DedupeKey: "terminal", Priority: 1, Status: "queued", AvailableAt: now, MaxAttempts: 3, CreatedAt: now, UpdatedAt: now},
		{ID: "qi_lock_running", ProjectID: &projectID, LoopID: strPtr("loop_lock_running"), Type: "worker", TargetType: "project", TargetID: "project_queue_stats", DedupeKey: "lock-running", Priority: 1, Status: "running", AvailableAt: now, LockKey: &lockKey, MaxAttempts: 3, CreatedAt: now, UpdatedAt: now},
		{ID: "qi_lock_wait", ProjectID: &projectID, LoopID: strPtr("loop_lock_wait"), Type: "worker", TargetType: "project", TargetID: "project_queue_stats", DedupeKey: "lock-wait", Priority: 1, Status: "queued", AvailableAt: now, LockKey: &lockKey, MaxAttempts: 3, CreatedAt: now, UpdatedAt: now},
		{ID: "qi_reviewer", ProjectID: &projectID, LoopID: strPtr("loop_reviewer"), Type: "reviewer", TargetType: "pull_request", TargetID: "acme/looper#42", Repo: &repo, PRNumber: &prNumber, DedupeKey: "reviewer", Priority: 1, Status: "queued", AvailableAt: now, MaxAttempts: 3, CreatedAt: now, UpdatedAt: now},
		{ID: "qi_fixer", ProjectID: &projectID, LoopID: strPtr("loop_fixer"), Type: "fixer", TargetType: "pull_request", TargetID: "acme/looper#42", Repo: &repo, PRNumber: &prNumber, DedupeKey: "fixer", Priority: 1, Status: "queued", AvailableAt: now, MaxAttempts: 3, CreatedAt: now, UpdatedAt: now},
		{ID: "qi_stale", ProjectID: &projectID, LoopID: strPtr("loop_stale"), Type: "worker", TargetType: "project", TargetID: "project_queue_stats", DedupeKey: "stale", Priority: 1, Status: "queued", AvailableAt: now, MaxAttempts: 3, CreatedAt: now, UpdatedAt: now},
	} {
		if err := repos.Queue.Upsert(ctx, item); err != nil {
			t.Fatalf("Queue.Upsert(%s) error = %v", item.ID, err)
		}
	}

	stats, err := repos.Queue.Stats(ctx, now)
	if err != nil {
		t.Fatalf("Queue.Stats() error = %v", err)
	}
	if stats.TotalQueued != 7 || stats.EligibleQueued != 2 || stats.BlockedByTerminalOrPausedLoop != 2 || stats.BlockedByLockKey != 1 || stats.BlockedByReviewerFixerDependency != 1 || stats.ScheduledForFuture != 1 || stats.StaleQueued != 2 {
		t.Fatalf("Queue.Stats() = %#v, want total=7 eligible=2 terminal=2 lock=1 dependency=1 future=1 stale=2", stats)
	}

	cleaned, err := repos.Queue.CleanupStaleQueued(ctx, now, "stale queue item attached to terminal loop")
	if err != nil {
		t.Fatalf("Queue.CleanupStaleQueued() error = %v", err)
	}
	if cleaned != 2 {
		t.Fatalf("Queue.CleanupStaleQueued() = %d, want 2", cleaned)
	}
	for _, id := range []string{"qi_terminal", "qi_stale"} {
		item, err := repos.Queue.GetByID(ctx, id)
		if err != nil {
			t.Fatalf("Queue.GetByID(%s) error = %v", id, err)
		}
		if item == nil || item.Status != "cancelled" || item.LastErrorKind == nil || *item.LastErrorKind != "non_retryable" {
			t.Fatalf("Queue.GetByID(%s) after cleanup = %#v, want cancelled non_retryable", id, item)
		}
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
