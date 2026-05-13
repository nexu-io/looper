package reviewer

import (
	"context"
	"testing"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/domain"
	"github.com/nexu-io/looper/internal/storage"
)

func TestRepairDetectsStaleLocalPublishedHeadDryRun(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	loopID := seedReviewerRepairLoop(t, fixture, string(domain.LoopStatusCompleted), stalePublishedRepairMetadata())
	repairer := newTestRepairer(fixture, &fakeGitHubGateway{currentLogin: "octocat", reviewRequests: []string{"octocat"}, viewHeadSHA: "abc123"})

	result, err := repairer.Repair(context.Background(), RepairInput{Repo: "acme/looper", PRNumber: 42})
	if err != nil {
		t.Fatalf("Repair() error = %v", err)
	}
	if result.Applied {
		t.Fatalf("Repair() applied in dry-run mode")
	}
	if !repairHasDiagnosis(result, "stale_local_published_head") {
		t.Fatalf("Repair() diagnoses = %#v, want stale_local_published_head", result.Diagnoses)
	}
	if !repairHasAction(result, "clear_local_published_head") {
		t.Fatalf("Repair() actions = %#v, want clear_local_published_head", result.Actions)
	}
	if got, want := result.Local.CleanPolicy, "COMMENT"; got != want {
		t.Fatalf("result.Local.CleanPolicy = %q, want stale snapshot %q", got, want)
	}

	loop, err := fixture.repos.Loops.GetByID(context.Background(), loopID)
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	meta := parseJSONObject(loop.MetadataJSON)
	if got, _ := stringFromAny(meta["lastPublishedHeadSha"]); got != "abc123" {
		t.Fatalf("dry-run metadata lastPublishedHeadSha = %q, want abc123", got)
	}
}

func TestRepairApplyClearsStalePublishedHeadAndResnapshotsPolicy(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	loopID := seedReviewerRepairLoop(t, fixture, string(domain.LoopStatusCompleted), stalePublishedRepairMetadata())
	repairer := newTestRepairer(fixture, &fakeGitHubGateway{currentLogin: "octocat", reviewRequests: []string{"octocat"}, viewHeadSHA: "abc123"})

	result, err := repairer.Repair(context.Background(), RepairInput{Repo: "acme/looper", PRNumber: 42, Apply: true})
	if err != nil {
		t.Fatalf("Repair(apply) error = %v", err)
	}
	if !result.Applied || result.AppliedChanges != 1 {
		t.Fatalf("Repair(apply) applied=%v changes=%d, want one applied change", result.Applied, result.AppliedChanges)
	}

	loop, err := fixture.repos.Loops.GetByID(context.Background(), loopID)
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if got, want := loop.Status, string(domain.LoopStatusWaiting); got != want {
		t.Fatalf("loop.Status = %q, want %q", got, want)
	}
	meta := parseJSONObject(loop.MetadataJSON)
	for _, key := range []string{"lastPublishedHeadSha", "lastReviewEvent", "lastOutputFingerprint"} {
		if _, ok := meta[key]; ok {
			t.Fatalf("metadata still has %s after repair: %#v", key, meta)
		}
	}
	reviewEvents, _ := meta["reviewEvents"].(map[string]any)
	if got, _ := stringFromAny(reviewEvents["clean"]); got != "APPROVE" {
		t.Fatalf("metadata.reviewEvents.clean = %q, want APPROVE", got)
	}
	loopMeta, _ := meta["loop"].(map[string]any)
	if _, ok := loopMeta["lastReviewedHeadSha"]; ok {
		t.Fatalf("metadata.loop still has lastReviewedHeadSha after repair: %#v", loopMeta)
	}
	if got, _ := stringFromAny(loopMeta["status"]); got != string(domain.LoopStatusWaiting) {
		t.Fatalf("metadata.loop.status = %q, want waiting", got)
	}

	events, err := fixture.repos.Events.ListByEntity(context.Background(), "pull_request", "acme/looper#42")
	if err != nil {
		t.Fatalf("Events.ListByEntity() error = %v", err)
	}
	if len(events) != 1 || events[0].EventType != "reviewer.loop.repair" {
		t.Fatalf("repair events = %#v, want one reviewer.loop.repair", events)
	}
}

func TestRepairDoesNotClearPublishedHeadWhenCurrentHeadWasReviewed(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	seedReviewerRepairLoop(t, fixture, string(domain.LoopStatusCompleted), stalePublishedRepairMetadata())
	repairer := newTestRepairer(fixture, &fakeGitHubGateway{
		currentLogin:   "octocat",
		reviewRequests: []string{"octocat"},
		viewHeadSHA:    "abc123",
		reviews: []map[string]any{
			{"author": map[string]any{"login": "octocat"}, "state": "COMMENTED", "commit": map[string]any{"oid": "abc123"}},
		},
	})

	result, err := repairer.Repair(context.Background(), RepairInput{Repo: "acme/looper", PRNumber: 42})
	if err != nil {
		t.Fatalf("Repair() error = %v", err)
	}
	if repairHasAction(result, "clear_local_published_head") {
		t.Fatalf("Repair() actions = %#v, did not expect clear_local_published_head", result.Actions)
	}
}

func TestRepairApplyReactivatesFailedRequestedLoop(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	loopID := seedReviewerRepairLoop(t, fixture, string(domain.LoopStatusFailed), mustMarshalJSON(map[string]any{
		"loop": map[string]any{"status": "failed", "consecutiveFailures": 3, "lastFailure": "gh api EOF"},
	}))
	queueID := seedReviewerRepairFailedQueue(t, fixture, loopID)
	repairer := newTestRepairer(fixture, &fakeGitHubGateway{currentLogin: "octocat", reviewRequests: []string{"octocat"}, viewHeadSHA: "abc123"})

	result, err := repairer.Repair(context.Background(), RepairInput{Repo: "acme/looper", PRNumber: 42, Apply: true})
	if err != nil {
		t.Fatalf("Repair(apply) error = %v", err)
	}
	if !result.Applied || !repairHasAction(result, "reactivate_failed_loop") {
		t.Fatalf("Repair(apply) result = %#v, want reactivate_failed_loop applied", result)
	}
	loop, err := fixture.repos.Loops.GetByID(context.Background(), loopID)
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if got, want := loop.Status, string(domain.LoopStatusWaiting); got != want {
		t.Fatalf("loop.Status = %q, want %q", got, want)
	}
	meta := parseJSONObject(loop.MetadataJSON)
	loopMeta, _ := meta["loop"].(map[string]any)
	if got := loopMeta["consecutiveFailures"]; got != float64(0) {
		t.Fatalf("metadata.loop.consecutiveFailures = %#v, want 0", got)
	}
	if _, ok := loopMeta["lastFailure"]; ok {
		t.Fatalf("metadata.loop.lastFailure still present after repair: %#v", loopMeta)
	}
	queue, err := fixture.repos.Queue.GetByID(context.Background(), queueID)
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	if got, want := queue.Status, "cancelled"; got != want {
		t.Fatalf("queue.Status = %q, want %q", got, want)
	}
}

func newTestRepairer(fixture *runnerFixture, github RepairGitHub) *Repairer {
	return NewRepairer(RepairOptions{
		DB:     fixture.coordinator.DB(),
		Repos:  fixture.repos,
		GitHub: github,
		Now:    fixture.now,
		ReviewEvents: config.ReviewerReviewEventsConfig{
			Clean:    config.ReviewerReviewEventApprove,
			Blocking: config.ReviewerReviewEventComment,
		},
	})
}

func seedReviewerRepairLoop(t *testing.T, fixture *runnerFixture, status string, metadata string) string {
	t.Helper()
	ctx := context.Background()
	repo := "acme/looper"
	prNumber := int64(42)
	targetID := "pr:acme/looper:42"
	loopID := "loop_reviewer_repair"
	nowISO := fixture.nowISO()
	if err := fixture.repos.Loops.Upsert(ctx, storage.LoopRecord{ID: loopID, Seq: 42, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", TargetID: &targetID, Repo: &repo, PRNumber: &prNumber, Status: status, MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	return loopID
}

func seedReviewerRepairFailedQueue(t *testing.T, fixture *runnerFixture, loopID string) string {
	t.Helper()
	ctx := context.Background()
	repo := "acme/looper"
	prNumber := int64(42)
	targetID := "pr:acme/looper:42"
	queueID := "queue_reviewer_repair_failed"
	nowISO := fixture.nowISO()
	if err := fixture.repos.Queue.Upsert(ctx, storage.QueueItemRecord{ID: queueID, ProjectID: stringPtr("project_1"), LoopID: &loopID, Type: "reviewer", TargetType: "pull_request", TargetID: targetID, Repo: &repo, PRNumber: &prNumber, DedupeKey: buildReviewerDedupeKey("project_1", loopID, repo, prNumber), Priority: storage.QueuePriorityReviewer, Status: "failed", AvailableAt: nowISO, Attempts: 3, MaxAttempts: 3, FinishedAt: &nowISO, LastError: stringPtr("gh api EOF"), LastErrorKind: stringPtr("non_retryable"), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}
	return queueID
}

func stalePublishedRepairMetadata() string {
	return mustMarshalJSON(map[string]any{
		"lastPublishedHeadSha":  "abc123",
		"lastPublishedAt":       "2026-05-13T06:00:00.000Z",
		"lastReviewEvent":       "COMMENT",
		"lastOutputFingerprint": "fingerprint-1",
		"reviewEvents":          map[string]any{"clean": "COMMENT", "blocking": "COMMENT"},
		"loop":                  map[string]any{"status": "completed", "lastReviewedHeadSha": "abc123", "lastOutputFingerprint": "fingerprint-1"},
	})
}

func repairHasDiagnosis(result RepairResult, code string) bool {
	for _, diagnosis := range result.Diagnoses {
		if diagnosis.Code == code {
			return true
		}
	}
	return false
}

func repairHasAction(result RepairResult, code string) bool {
	for _, action := range result.Actions {
		if action.Code == code {
			return true
		}
	}
	return false
}
