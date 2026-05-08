package cliapp

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/storage"
)

func TestRunStatsCommandOutputsJSONAndHuman(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		args   []string
		assert func(*testing.T, string)
	}{
		{name: "json", args: []string{"run", "stats", "--since", "1000d", "--json"}, assert: func(t *testing.T, output string) {
			var decoded struct {
				Since string `json:"since"`
				Roles map[string]struct {
					Success     int64 `json:"success"`
					Failure     int64 `json:"failure"`
					Skipped     int64 `json:"skipped"`
					Interrupted int64 `json:"interrupted"`
					Requeued    int64 `json:"requeued"`
					Retried     int64 `json:"retried"`
					Outcomes    struct {
						Approved         int64 `json:"approved"`
						Commented        int64 `json:"commented"`
						RequestedChanges int64 `json:"requested_changes"`
					} `json:"outcomes"`
					AgentExecutions struct {
						Success int64            `json:"success"`
						Failure int64            `json:"failure"`
						Status  map[string]int64 `json:"status"`
					} `json:"agentExecutions"`
				} `json:"roles"`
			}
			if err := json.Unmarshal([]byte(output), &decoded); err != nil {
				t.Fatalf("json.Unmarshal() error = %v\noutput=%q", err, output)
			}
			reviewer := decoded.Roles["reviewer"]
			if decoded.Since != "1000d" || reviewer.Success != 1 || reviewer.Failure != 1 || reviewer.Skipped != 1 || reviewer.Interrupted != 1 || reviewer.Requeued != 1 {
				t.Fatalf("reviewer stats = %#v since=%q, want success/failure/skipped/interrupted/requeued", reviewer, decoded.Since)
			}
			if reviewer.Outcomes.Approved != 1 || reviewer.Outcomes.RequestedChanges != 1 {
				t.Fatalf("reviewer outcomes = %#v, want approved=1 requested_changes=1", reviewer.Outcomes)
			}
			fixer := decoded.Roles["fixer"]
			if fixer.Success != 1 || fixer.Failure != 1 || fixer.Retried != 1 || fixer.AgentExecutions.Success != 1 || fixer.AgentExecutions.Failure != 1 || fixer.AgentExecutions.Status["completed"] != 1 {
				t.Fatalf("fixer stats = %#v, want run and agent breakdowns", fixer)
			}
			worker := decoded.Roles["worker"]
			if worker.Retried != 14 {
				t.Fatalf("worker retried = %d, want queued, running, and terminal queue attempts counted", worker.Retried)
			}
		}},
		{name: "human", args: []string{"run", "stats", "--since", "1000d", "--role", "reviewer"}, assert: func(t *testing.T, output string) {
			for _, needle := range []string{"Recent run stats", "reviewer", "success", "skipped", "outcomes.approved"} {
				if !strings.Contains(output, needle) {
					t.Fatalf("run stats output missing %q\n%s", needle, output)
				}
			}
			if strings.Contains(output, "\nfixer\n") {
				t.Fatalf("run stats reviewer output included fixer section\n%s", output)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			configPath := writeRunStatsCommandFixture(t)
			stdout := &bytes.Buffer{}
			stderr := &bytes.Buffer{}
			app := New(Deps{Stdout: stdout, Stderr: stderr})
			args := append(append([]string{}, tc.args...), "--config", configPath)
			if exitCode := app.Run(context.Background(), args); exitCode != 0 {
				t.Fatalf("Run(%v) exit code = %d, want 0; stderr=%q", args, exitCode, stderr.String())
			}
			tc.assert(t, stdout.String())
		})
	}
}

func TestRunStatsCommandEmptyStateAndDurationValidation(t *testing.T) {
	t.Parallel()

	configPath := writeEmptyRunStatsCommandFixture(t)
	exitCode, stdout, stderr := runApp(t, "run", "stats", "--since", "1h", "--config", configPath)
	if exitCode != 0 || stderr != "" {
		t.Fatalf("Run([run stats empty]) = (%d, %q), want (0, empty)", exitCode, stderr)
	}
	if !strings.Contains(stdout, "No matching recent actions found") {
		t.Fatalf("empty run stats output = %q, want empty state", stdout)
	}

	exitCode, _, stderr = runApp(t, "run", "stats", "--since", "bogus", "--config", configPath)
	if exitCode == 0 || !strings.Contains(stderr, "invalid --since duration") {
		t.Fatalf("Run([run stats --since bogus]) = (%d, %q), want duration error", exitCode, stderr)
	}

	exitCode, _, stderr = runApp(t, "run", "stats", "unexpected", "--config", configPath)
	if exitCode == 0 || !strings.Contains(stderr, "unknown command") && !strings.Contains(stderr, "accepts 0 arg") {
		t.Fatalf("Run([run stats unexpected]) = (%d, %q), want no-args error", exitCode, stderr)
	}
}

func TestRunStatsCommandFiltersLoop(t *testing.T) {
	t.Parallel()

	configPath := writeRunStatsCommandFixture(t)
	exitCode, stdout, stderr := runApp(t, "run", "stats", "--since", "1000d", "--loop", "loop_reviewer", "--json", "--config", configPath)
	if exitCode != 0 || stderr != "" {
		t.Fatalf("Run([run stats --loop]) = (%d, %q), want (0, empty)", exitCode, stderr)
	}
	var decoded struct {
		Roles map[string]struct {
			Success int64 `json:"success"`
		} `json:"roles"`
	}
	if err := json.Unmarshal([]byte(stdout), &decoded); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if decoded.Roles["reviewer"].Success != 1 || decoded.Roles["fixer"].Success != 0 {
		t.Fatalf("loop-filtered roles = %#v, want reviewer only counts", decoded.Roles)
	}
}

func writeRunStatsCommandFixture(t *testing.T) string {
	t.Helper()
	configPath, repos := writeEmptyRunStatsCommandFixtureWithRepos(t)
	now := "2026-04-11T12:00:00.000Z"
	old := "2020-04-11T12:00:00.000Z"
	projectID := "project_run_stats_cli"
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Looper", RepoPath: "/tmp/looper", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	loops := []storage.LoopRecord{
		{ID: "loop_reviewer", Seq: 1, ProjectID: projectID, Type: "reviewer", TargetType: "pull_request", Status: "completed", CreatedAt: now, UpdatedAt: now},
		{ID: "loop_fixer", Seq: 2, ProjectID: projectID, Type: "fixer", TargetType: "pull_request", Status: "completed", CreatedAt: now, UpdatedAt: now},
		{ID: "loop_worker", Seq: 3, ProjectID: projectID, Type: "worker", TargetType: "project", Status: "completed", CreatedAt: now, UpdatedAt: now},
	}
	for _, loop := range loops {
		if err := repos.Loops.Upsert(context.Background(), loop); err != nil {
			t.Fatalf("Loops.Upsert(%s) error = %v", loop.ID, err)
		}
	}

	skippedCheckpoint := `{"skipReason":"draft pull request","skipKind":"draft"}`
	runs := []storage.RunRecord{
		{ID: "run_reviewer_success", LoopID: "loop_reviewer", Status: "success", StartedAt: now, CreatedAt: now, UpdatedAt: now},
		{ID: "run_reviewer_failed", LoopID: "loop_reviewer", Status: "failed", StartedAt: now, CreatedAt: now, UpdatedAt: now},
		{ID: "run_reviewer_skipped", LoopID: "loop_reviewer", Status: "success", CheckpointJSON: &skippedCheckpoint, StartedAt: now, CreatedAt: now, UpdatedAt: now},
		{ID: "run_reviewer_interrupted", LoopID: "loop_reviewer", Status: "interrupted", StartedAt: now, CreatedAt: now, UpdatedAt: now},
		{ID: "run_fixer_success", LoopID: "loop_fixer", Status: "success", StartedAt: now, CreatedAt: now, UpdatedAt: now},
		{ID: "run_fixer_failed", LoopID: "loop_fixer", Status: "failed", StartedAt: now, CreatedAt: now, UpdatedAt: now},
		{ID: "run_old", LoopID: "loop_fixer", Status: "success", StartedAt: old, CreatedAt: old, UpdatedAt: old},
	}
	for _, run := range runs {
		if err := repos.Runs.Upsert(context.Background(), run); err != nil {
			t.Fatalf("Runs.Upsert(%s) error = %v", run.ID, err)
		}
	}

	if err := repos.AgentExecutions.Upsert(context.Background(), storage.AgentExecutionRecord{ID: "agent_fixer_completed", LoopID: stringPtr("loop_fixer"), RunID: stringPtr("run_fixer_success"), Vendor: "opencode", Status: "completed", StartedAt: now, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("AgentExecutions.Upsert(completed) error = %v", err)
	}
	if err := repos.AgentExecutions.Upsert(context.Background(), storage.AgentExecutionRecord{ID: "agent_fixer_failed", LoopID: stringPtr("loop_fixer"), RunID: stringPtr("run_fixer_failed"), Vendor: "opencode", Status: "failed", StartedAt: now, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("AgentExecutions.Upsert(failed) error = %v", err)
	}
	if err := repos.Events.Append(context.Background(), storage.EventLogRecord{ID: "event_requeued", EventType: "looperd.recovery.loop_requeued", LoopID: stringPtr("loop_reviewer"), PayloadJSON: `{}`, CreatedAt: now}); err != nil {
		t.Fatalf("Events.Append(requeued) error = %v", err)
	}
	if err := repos.Events.Append(context.Background(), storage.EventLogRecord{ID: "event_review_approved", EventType: "pr.review.posted", LoopID: stringPtr("loop_reviewer"), RunID: stringPtr("run_reviewer_success"), PayloadJSON: `{"event":"APPROVE"}`, CreatedAt: now}); err != nil {
		t.Fatalf("Events.Append(review approved) error = %v", err)
	}
	if err := repos.Events.Append(context.Background(), storage.EventLogRecord{ID: "event_review_requested_changes", EventType: "pr.review.posted", LoopID: stringPtr("loop_reviewer"), RunID: stringPtr("run_reviewer_failed"), PayloadJSON: `{"event":"REQUEST_CHANGES"}`, CreatedAt: now}); err != nil {
		t.Fatalf("Events.Append(review requested changes) error = %v", err)
	}
	if err := repos.Events.Append(context.Background(), storage.EventLogRecord{ID: "event_retryable_failure", EventType: "fixer.push.retryable", LoopID: stringPtr("loop_fixer"), PayloadJSON: `{}`, CreatedAt: now}); err != nil {
		t.Fatalf("Events.Append(retry) error = %v", err)
	}
	retryableKind := "retryable_transient"
	if err := repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: "queue_fixer_retry", LoopID: stringPtr("loop_fixer"), Type: "fixer", TargetType: "pull_request", TargetID: "239", DedupeKey: "queue_fixer_retry", Priority: 1, Status: "queued", AvailableAt: now, Attempts: 1, MaxAttempts: 5, LastErrorKind: &retryableKind, CreatedAt: old, UpdatedAt: now}); err != nil {
		t.Fatalf("Queue.Upsert(retry) error = %v", err)
	}
	if err := repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: "queue_worker_retry", LoopID: stringPtr("loop_worker"), Type: "worker", TargetType: "project", TargetID: "project_run_stats_cli", DedupeKey: "queue_worker_retry", Priority: 1, Status: "queued", AvailableAt: now, Attempts: 2, MaxAttempts: 5, LastErrorKind: &retryableKind, CreatedAt: old, UpdatedAt: now}); err != nil {
		t.Fatalf("Queue.Upsert(worker retry) error = %v", err)
	}
	if err := repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: "queue_worker_claimed_after_retry", LoopID: stringPtr("loop_worker"), Type: "worker", TargetType: "project", TargetID: "project_run_stats_cli", DedupeKey: "queue_worker_claimed_after_retry", Priority: 1, Status: "running", AvailableAt: now, Attempts: 4, MaxAttempts: 5, LastErrorKind: &retryableKind, CreatedAt: old, UpdatedAt: now}); err != nil {
		t.Fatalf("Queue.Upsert(worker claimed after retry) error = %v", err)
	}
	if err := repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: "queue_worker_completed_after_retry", LoopID: stringPtr("loop_worker"), Type: "worker", TargetType: "project", TargetID: "project_run_stats_cli", DedupeKey: "queue_worker_completed_after_retry", Priority: 1, Status: "completed", AvailableAt: now, Attempts: 3, MaxAttempts: 5, LastErrorKind: &retryableKind, CreatedAt: old, UpdatedAt: now}); err != nil {
		t.Fatalf("Queue.Upsert(worker completed after retry) error = %v", err)
	}
	if err := repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: "queue_worker_failed_after_retry", LoopID: stringPtr("loop_worker"), Type: "worker", TargetType: "project", TargetID: "project_run_stats_cli", DedupeKey: "queue_worker_failed_after_retry", Priority: 1, Status: "failed", AvailableAt: now, Attempts: 2, MaxAttempts: 5, LastErrorKind: &retryableKind, CreatedAt: old, UpdatedAt: now}); err != nil {
		t.Fatalf("Queue.Upsert(worker failed after retry) error = %v", err)
	}
	nonRetryableKind := "non_retryable"
	if err := repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: "queue_worker_non_retryable_after_retry", LoopID: stringPtr("loop_worker"), Type: "worker", TargetType: "project", TargetID: "project_run_stats_cli", DedupeKey: "queue_worker_non_retryable_after_retry", Priority: 1, Status: "failed", AvailableAt: now, Attempts: 1, MaxAttempts: 5, LastErrorKind: &nonRetryableKind, CreatedAt: old, UpdatedAt: now}); err != nil {
		t.Fatalf("Queue.Upsert(worker non-retryable after retry) error = %v", err)
	}
	if err := repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: "queue_worker_cancelled_after_retry", LoopID: stringPtr("loop_worker"), Type: "worker", TargetType: "project", TargetID: "project_run_stats_cli", DedupeKey: "queue_worker_cancelled_after_retry", Priority: 1, Status: "cancelled", AvailableAt: now, Attempts: 1, MaxAttempts: 5, LastErrorKind: &retryableKind, CreatedAt: old, UpdatedAt: now}); err != nil {
		t.Fatalf("Queue.Upsert(worker cancelled after retry) error = %v", err)
	}
	if err := repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: "queue_worker_manual_intervention_after_retry", LoopID: stringPtr("loop_worker"), Type: "worker", TargetType: "project", TargetID: "project_run_stats_cli", DedupeKey: "queue_worker_manual_intervention_after_retry", Priority: 1, Status: "manual_intervention", AvailableAt: now, Attempts: 1, MaxAttempts: 5, LastErrorKind: &retryableKind, CreatedAt: old, UpdatedAt: now}); err != nil {
		t.Fatalf("Queue.Upsert(worker manual intervention after retry) error = %v", err)
	}
	return configPath
}

func writeEmptyRunStatsCommandFixture(t *testing.T) string {
	t.Helper()
	configPath, _ := writeEmptyRunStatsCommandFixtureWithRepos(t)
	return configPath
}

func writeEmptyRunStatsCommandFixtureWithRepos(t *testing.T) (string, *storage.Repositories) {
	t.Helper()
	root := t.TempDir()
	dbPath := filepath.Join(root, "looper.sqlite")
	coordinator, err := storage.OpenSQLiteCoordinator(context.Background(), dbPath, storage.SQLiteCoordinatorOptions{Migrations: storage.EmbeddedMigrations, BackupDir: filepath.Join(root, "backups")})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	if _, err := coordinator.MigrationRunner().RunPending(context.Background()); err != nil {
		t.Fatalf("RunPending() error = %v", err)
	}
	configPath := filepath.Join(root, "config.json")
	raw, err := json.Marshal(map[string]any{"storage": map[string]any{"dbPath": dbPath}})
	if err != nil {
		t.Fatalf("json.Marshal(config) error = %v", err)
	}
	if err := os.WriteFile(configPath, raw, 0o644); err != nil {
		t.Fatalf("WriteFile(config) error = %v", err)
	}
	return configPath, storage.NewRepositories(coordinator.DB())
}

func TestParseRunStatsDurationSupportsDays(t *testing.T) {
	t.Parallel()
	duration, err := parseRunStatsDuration("7d")
	if err != nil {
		t.Fatalf("parseRunStatsDuration(7d) error = %v", err)
	}
	if duration != 7*24*time.Hour {
		t.Fatalf("parseRunStatsDuration(7d) = %v, want 168h", duration)
	}
}
