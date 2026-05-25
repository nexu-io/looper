package cliapp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/storage"
	pkgapi "github.com/nexu-io/looper/pkg/api"
)

func TestLoopInspectAcceptsRunIDAndClassifiesFailure(t *testing.T) {
	t.Parallel()

	configPath := writeLoopDiagnosticsFixture(t, "")
	exitCode, stdout, stderr := runApp(t, "loop", "inspect", "run_reviewer_failed", "--json", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run([loop inspect]) exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
	var decoded struct {
		SelectorKind string `json:"selectorKind"`
		Loop         struct {
			Seq    int64  `json:"seq"`
			Status string `json:"status"`
			Target struct {
				Label string `json:"label"`
			} `json:"target"`
		} `json:"loop"`
		Run struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"run"`
		LatestQueueItem struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"latestQueueItem"`
		Diagnosis struct {
			FailureClass      string `json:"failureClass"`
			Retryable         *bool  `json:"retryable"`
			RecommendedAction string `json:"recommendedAction"`
		} `json:"diagnosis"`
	}
	if err := json.Unmarshal([]byte(stdout), &decoded); err != nil {
		t.Fatalf("json.Unmarshal() error = %v\noutput=%q", err, stdout)
	}
	if decoded.SelectorKind != "runId" || decoded.Loop.Seq != 7 || decoded.Loop.Target.Label != "acme/looper#42" || decoded.Run.ID != "run_reviewer_failed" {
		t.Fatalf("inspect output = %#v, want run selector resolved to loop #7", decoded)
	}
	if decoded.LatestQueueItem.ID != "queue_failed" || decoded.LatestQueueItem.Status != "failed" {
		t.Fatalf("latest queue item = %#v, want failed queue", decoded.LatestQueueItem)
	}
	if decoded.Diagnosis.FailureClass != "github_transient" || decoded.Diagnosis.Retryable == nil || !*decoded.Diagnosis.Retryable || decoded.Diagnosis.RecommendedAction == "" {
		t.Fatalf("diagnosis = %#v, want retryable github_transient with action", decoded.Diagnosis)
	}
}

func TestLoopFailuresListsFailedLoops(t *testing.T) {
	t.Parallel()

	configPath := writeLoopDiagnosticsFixture(t, "")
	exitCode, stdout, stderr := runApp(t, "loop", "failures", "--type", "reviewer", "--json", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run([loop failures]) exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
	var decoded struct {
		Count int `json:"count"`
		Items []struct {
			Loop struct {
				Seq int64 `json:"seq"`
			} `json:"loop"`
			Diagnosis struct {
				FailureClass string `json:"failureClass"`
			} `json:"diagnosis"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(stdout), &decoded); err != nil {
		t.Fatalf("json.Unmarshal() error = %v\noutput=%q", err, stdout)
	}
	if decoded.Count != 1 || len(decoded.Items) != 1 || decoded.Items[0].Loop.Seq != 7 || decoded.Items[0].Diagnosis.FailureClass != "github_transient" {
		t.Fatalf("loop failures output = %#v, want one failed reviewer loop", decoded)
	}
}

func TestLogsAcceptsRunIDFromPSOutput(t *testing.T) {
	t.Parallel()

	var requestedPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestedPath = r.URL.Path
		writeEnvelope(t, w, pkgapi.Success("req_logs", map[string]any{
			"seq":        7,
			"loopId":     "loop_failed",
			"loopType":   "reviewer",
			"loopStatus": "failed",
			"run":        map[string]any{"runId": "run_reviewer_failed", "status": "failed", "currentStep": "snapshot"},
			"agent":      map[string]any{"executionId": "agent_failed", "vendor": "claude-code", "status": "failed", "stdout": "review output", "stderr": ""},
		}))
	}))
	defer server.Close()

	configPath := writeLoopDiagnosticsFixture(t, server.URL)
	exitCode, stdout, stderr := runApp(t, "logs", "run_reviewer_failed", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run([logs run_reviewer_failed]) exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
	if requestedPath != "/api/v1/runs/run_reviewer_failed/logs" {
		t.Fatalf("requested path = %q, want run-scoped logs path", requestedPath)
	}
	for _, want := range []string{"Loop #7 · reviewer · failed", "Run run_reviewer_failed · step: snapshot", "review output"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout = %q, want to contain %q", stdout, want)
		}
	}
}

func TestLogsRejectsRunIDFollow(t *testing.T) {
	t.Parallel()

	configPath := writeLoopDiagnosticsFixture(t, "")
	exitCode, _, stderr := runApp(t, "logs", "run_reviewer_failed", "--follow", "--config", configPath)
	if exitCode == 0 {
		t.Fatal("Run([logs run_reviewer_failed --follow]) exit code = 0, want non-zero")
	}
	if !strings.Contains(stderr, "run-scoped logs cannot be followed") {
		t.Fatalf("stderr = %q, want run-scoped follow error", stderr)
	}
}

func writeLoopDiagnosticsFixture(t *testing.T, serverURL string) string {
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

	repos := storage.NewRepositories(coordinator.DB())
	now := "2026-04-11T12:00:00.000Z"
	projectID := "project_loop_diagnostics"
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Looper", RepoPath: "/tmp/looper", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	prNumber := int64(42)
	targetID := "pr:acme/looper:42"
	metadata := `{"followUpdates":true,"lastPublishedAt":"2026-04-11T11:00:00.000Z","lastReviewSummary":"previous review","loop":{"status":"active","lastStatus":"failed","consecutiveFailures":2,"failureCount":2,"lastFailure":"Command exited with code 1: Post \"https://api.github.com/graphql\": EOF"}}`
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: "loop_failed", Seq: 7, ProjectID: projectID, Type: "reviewer", TargetType: "pull_request", TargetID: &targetID, Repo: stringPtr("acme/looper"), PRNumber: &prNumber, Status: "failed", MetadataJSON: &metadata, LastRunAt: &now, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	errorMessage := `Command exited with code 1: Post "https://api.github.com/graphql": EOF`
	if err := repos.Runs.Upsert(context.Background(), storage.RunRecord{ID: "run_reviewer_failed", LoopID: "loop_failed", Status: "failed", CurrentStep: stringPtr("snapshot"), ErrorMessage: &errorMessage, StartedAt: now, LastHeartbeatAt: &now, EndedAt: &now, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}
	lastErrorKind := "retryable_transient"
	if err := repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: "queue_failed", ProjectID: &projectID, LoopID: stringPtr("loop_failed"), Type: "reviewer", TargetType: "pull_request", TargetID: targetID, Repo: stringPtr("acme/looper"), PRNumber: &prNumber, DedupeKey: "reviewer:failed", Priority: 1, Status: "failed", AvailableAt: now, Attempts: 2, MaxAttempts: 5, LastError: &errorMessage, LastErrorKind: &lastErrorKind, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}
	pid := int64(1234)
	if err := repos.AgentExecutions.Upsert(context.Background(), storage.AgentExecutionRecord{ID: "agent_failed", ProjectID: &projectID, LoopID: stringPtr("loop_failed"), RunID: stringPtr("run_reviewer_failed"), Vendor: "claude-code", Status: "failed", PID: &pid, ErrorMessage: &errorMessage, StartedAt: now, EndedAt: &now, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("AgentExecutions.Upsert() error = %v", err)
	}

	configPayload := map[string]any{"storage": map[string]any{"dbPath": dbPath}}
	if serverURL != "" {
		configPayload["server"] = map[string]any{"baseUrl": serverURL, "authMode": "none"}
	}
	configPath := filepath.Join(root, "config.json")
	raw, err := json.Marshal(configPayload)
	if err != nil {
		t.Fatalf("json.Marshal(config) error = %v", err)
	}
	if err := os.WriteFile(configPath, raw, 0o644); err != nil {
		t.Fatalf("WriteFile(config) error = %v", err)
	}
	return configPath
}
