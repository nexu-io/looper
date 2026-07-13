package storage

import (
	"context"
	"testing"
	"time"
)

func TestForgeLockKeysAreProjectScoped(t *testing.T) {
	t.Parallel()

	firstIssue := IssueLockKey("github", "Acme/App", 42)
	secondIssue := IssueLockKey("forgejo", "acme/app", 42)
	if firstIssue == secondIssue {
		t.Fatalf("issue lock keys collide: %q", firstIssue)
	}
	firstPR := PullRequestLockKey("forgejo-one", "acme/app", 7)
	secondPR := PullRequestLockKey("forgejo-two", "ACME/APP", 7)
	if firstPR == secondPR {
		t.Fatalf("pull request lock keys collide: %q", firstPR)
	}
	if got := IssueLockKey(" github ", " Acme/App ", 42); got != firstIssue {
		t.Fatalf("IssueLockKey() normalization = %q, want %q", got, firstIssue)
	}
}

func TestForgeLockAcquirePreservesLegacyTransitionExclusion(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	coordinator := openMigratedCoordinatorForRepositories(t)
	repos := NewRepositories(coordinator.DB())
	now := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	repos.Locks.SetNow(func() time.Time { return now })
	record := func(key, owner string) LockRecord {
		return LockRecord{Key: key, Owner: owner, ExpiresAt: now.Add(time.Minute).Format(time.RFC3339Nano), CreatedAt: now.Format(time.RFC3339Nano), UpdatedAt: now.Format(time.RFC3339Nano)}
	}
	legacy := "pr:Acme/App:42"
	github := PullRequestLockKey("github", "acme/app", 42)
	forgejo := PullRequestLockKey("forgejo", "acme/app", 42)

	if acquired, err := repos.Locks.Acquire(ctx, record(legacy, "legacy")); err != nil || !acquired {
		t.Fatalf("Acquire(legacy) = %v, %v", acquired, err)
	}
	if acquired, err := repos.Locks.Acquire(ctx, record(github, "new")); err != nil || acquired {
		t.Fatalf("Acquire(scoped after legacy) = %v, %v; want blocked", acquired, err)
	}
	if err := repos.Locks.Release(ctx, legacy); err != nil {
		t.Fatalf("Release(legacy) error = %v", err)
	}
	if acquired, err := repos.Locks.Acquire(ctx, record(github, "new")); err != nil || !acquired {
		t.Fatalf("Acquire(scoped) = %v, %v", acquired, err)
	}
	if acquired, err := repos.Locks.Acquire(ctx, record(legacy, "old")); err != nil || acquired {
		t.Fatalf("Acquire(legacy after scoped) = %v, %v; want blocked", acquired, err)
	}
	if acquired, err := repos.Locks.Acquire(ctx, record(forgejo, "other-provider")); err != nil || !acquired {
		t.Fatalf("Acquire(other scoped project) = %v, %v; want independent lock", acquired, err)
	}
}

func TestForgeLockTransitionSupportsColonProjectID(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	coordinator := openMigratedCoordinatorForRepositories(t)
	repos := NewRepositories(coordinator.DB())
	now := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	repos.Locks.SetNow(func() time.Time { return now })
	legacy := "issue:acme/app:42"
	scoped := IssueLockKey("team:forgejo", "acme/app", 42)
	legacyKey, suffix, kind, isScoped, ok := lockTransitionAlias(scoped)
	if !ok || !isScoped || legacyKey != legacy || suffix != ":acme/app:42" || kind != "issue" {
		t.Fatalf("lockTransitionAlias(%q) = %q, %q, %q, %v, %v", scoped, legacyKey, suffix, kind, isScoped, ok)
	}
	record := func(key string) LockRecord {
		return LockRecord{Key: key, Owner: key, ExpiresAt: now.Add(time.Minute).Format(time.RFC3339Nano), CreatedAt: now.Format(time.RFC3339Nano), UpdatedAt: now.Format(time.RFC3339Nano)}
	}
	if acquired, err := repos.Locks.Acquire(ctx, record(legacy)); err != nil || !acquired {
		t.Fatalf("Acquire(legacy) = %v, %v", acquired, err)
	}
	if acquired, err := repos.Locks.Acquire(ctx, record(scoped)); err != nil || acquired {
		t.Fatalf("Acquire(colon-scoped after legacy) = %v, %v; want blocked", acquired, err)
	}
}

func TestQueueSchedulerTreatsLegacyAndScopedLockKeysAsEquivalent(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	coordinator := openMigratedCoordinatorForRepositories(t)
	repos := NewRepositories(coordinator.DB())
	now := "2026-07-13T12:00:00.000Z"
	projectID := "github"
	if err := repos.Projects.Upsert(ctx, ProjectRecord{ID: projectID, Name: projectID, RepoPath: "/tmp/github", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	for index, loopID := range []string{"legacy", "scoped"} {
		if err := repos.Loops.Upsert(ctx, LoopRecord{ID: loopID, Seq: int64(index + 1), ProjectID: projectID, Type: "reviewer", TargetType: "pull_request", Status: "running", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("Loops.Upsert(%s) error = %v", loopID, err)
		}
	}
	repo := "acme/app"
	prNumber := int64(42)
	legacyLoop, scopedLoop := "legacy", "scoped"
	legacyKey := "pr:acme/app:42"
	scopedKey := PullRequestLockKey(projectID, repo, prNumber)
	for _, item := range []QueueItemRecord{
		{ID: "legacy_running", ProjectID: &projectID, LoopID: &legacyLoop, Type: "reviewer", TargetType: "pull_request", TargetID: legacyKey, Repo: &repo, PRNumber: &prNumber, DedupeKey: "legacy", Priority: 1, Status: "running", AvailableAt: now, LockKey: &legacyKey, MaxAttempts: 3, CreatedAt: now, UpdatedAt: now},
		{ID: "scoped_queued", ProjectID: &projectID, LoopID: &scopedLoop, Type: "reviewer", TargetType: "pull_request", TargetID: legacyKey, Repo: &repo, PRNumber: &prNumber, DedupeKey: "scoped", Priority: 1, Status: "queued", AvailableAt: now, LockKey: &scopedKey, MaxAttempts: 3, CreatedAt: now, UpdatedAt: now},
	} {
		if err := repos.Queue.Upsert(ctx, item); err != nil {
			t.Fatalf("Queue.Upsert(%s) error = %v", item.ID, err)
		}
	}
	scheduled, err := repos.Queue.ListScheduled(ctx, now, 10)
	if err != nil {
		t.Fatalf("Queue.ListScheduled() error = %v", err)
	}
	if len(scheduled) != 0 {
		t.Fatalf("Queue.ListScheduled() = %#v, want scoped item blocked by legacy running item", scheduled)
	}
	stats, err := repos.Queue.Stats(ctx, now)
	if err != nil {
		t.Fatalf("Queue.Stats() error = %v", err)
	}
	if stats.BlockedByLockKey != 1 {
		t.Fatalf("BlockedByLockKey = %d, want 1", stats.BlockedByLockKey)
	}
}

func TestReviewerFixerDependencyIsProjectScoped(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	coordinator := openMigratedCoordinatorForRepositories(t)
	repos := NewRepositories(coordinator.DB())
	now := "2026-07-13T12:00:00.000Z"
	for index, projectID := range []string{"github", "forgejo"} {
		if err := repos.Projects.Upsert(ctx, ProjectRecord{ID: projectID, Name: projectID, RepoPath: "/tmp/" + projectID, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("Projects.Upsert(%s) error = %v", projectID, err)
		}
		loopID := "loop_" + projectID
		if err := repos.Loops.Upsert(ctx, LoopRecord{ID: loopID, Seq: int64(index + 1), ProjectID: projectID, Type: "worker", TargetType: "pull_request", Status: "running", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("Loops.Upsert(%s) error = %v", loopID, err)
		}
	}
	repo := "acme/app"
	prNumber := int64(42)
	github, forgejo := "github", "forgejo"
	githubLoop, forgejoLoop := "loop_github", "loop_forgejo"
	items := []QueueItemRecord{
		{ID: "github_reviewer", ProjectID: &github, LoopID: &githubLoop, Type: "reviewer", TargetType: "pull_request", TargetID: "pr:acme/app:42", Repo: &repo, PRNumber: &prNumber, DedupeKey: "reviewer:github", Priority: 1, Status: "queued", AvailableAt: now, MaxAttempts: 3, CreatedAt: now, UpdatedAt: now},
		{ID: "forgejo_fixer", ProjectID: &forgejo, LoopID: &forgejoLoop, Type: "fixer", TargetType: "pull_request", TargetID: "pr:acme/app:42", Repo: &repo, PRNumber: &prNumber, DedupeKey: "fixer:forgejo", Priority: 2, Status: "queued", AvailableAt: now, MaxAttempts: 3, CreatedAt: now, UpdatedAt: now},
	}
	for _, item := range items {
		if err := repos.Queue.Upsert(ctx, item); err != nil {
			t.Fatalf("Queue.Upsert(%s) error = %v", item.ID, err)
		}
	}
	scheduled, err := repos.Queue.ListScheduled(ctx, now, 10)
	if err != nil {
		t.Fatalf("Queue.ListScheduled() error = %v", err)
	}
	if len(scheduled) != 2 {
		t.Fatalf("Queue.ListScheduled() = %#v, want both cross-project items eligible", scheduled)
	}
	stats, err := repos.Queue.Stats(ctx, now)
	if err != nil {
		t.Fatalf("Queue.Stats() error = %v", err)
	}
	if stats.BlockedByReviewerFixerDependency != 0 {
		t.Fatalf("BlockedByReviewerFixerDependency = %d, want 0", stats.BlockedByReviewerFixerDependency)
	}
}
