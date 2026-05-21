package cliapp

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/storage"
)

func TestQueueStatsCommandOutputsJSONAndHuman(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		args   []string
		assert func(*testing.T, string)
	}{
		{name: "json", args: []string{"queue", "stats", "--json"}, assert: func(t *testing.T, output string) {
			var decoded map[string]any
			if err := json.Unmarshal([]byte(output), &decoded); err != nil {
				t.Fatalf("json.Unmarshal() error = %v\noutput=%q", err, output)
			}
			if decoded["eligibleQueued"] != float64(1) || decoded["staleQueued"] != float64(1) {
				t.Fatalf("queue stats json = %#v, want eligibleQueued=1 staleQueued=1", decoded)
			}
		}},
		{name: "human", args: []string{"queue", "stats"}, assert: func(t *testing.T, output string) {
			for _, needle := range []string{"totalQueued", "eligibleQueued", "staleQueued"} {
				if !strings.Contains(output, needle) {
					t.Fatalf("queue stats output missing %q\n%s", needle, output)
				}
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			configPath, _ := writeQueueCommandFixture(t)
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

func TestQueueCleanupCommandOutputsJSONAndHuman(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		args   []string
		assert func(*testing.T, string)
	}{
		{name: "json", args: []string{"queue", "cleanup", "--stale", "--json"}, assert: func(t *testing.T, output string) {
			var decoded map[string]any
			if err := json.Unmarshal([]byte(output), &decoded); err != nil {
				t.Fatalf("json.Unmarshal() error = %v\noutput=%q", err, output)
			}
			if decoded["cleaned"] != float64(1) || decoded["stale"] != true {
				t.Fatalf("queue cleanup json = %#v, want cleaned=1 stale=true", decoded)
			}
		}},
		{name: "human", args: []string{"queue", "cleanup", "--stale"}, assert: func(t *testing.T, output string) {
			if !strings.Contains(output, "Cleaned 1 stale queued item(s).") {
				t.Fatalf("queue cleanup output = %q, want cleanup summary", output)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			configPath, repos := writeQueueCommandFixture(t)
			stdout := &bytes.Buffer{}
			stderr := &bytes.Buffer{}
			app := New(Deps{Stdout: stdout, Stderr: stderr})
			args := append(append([]string{}, tc.args...), "--config", configPath)
			if exitCode := app.Run(context.Background(), args); exitCode != 0 {
				t.Fatalf("Run(%v) exit code = %d, want 0; stderr=%q", args, exitCode, stderr.String())
			}
			tc.assert(t, stdout.String())
			item, err := repos.Queue.GetByID(context.Background(), "qi_stale")
			if err != nil {
				t.Fatalf("Queue.GetByID() error = %v", err)
			}
			if item == nil || item.Status != "cancelled" {
				t.Fatalf("Queue.GetByID(qi_stale) = %#v, want cancelled", item)
			}
		})
	}
}

func TestQueueListCommandListsQueuedItemsAndFiltersEligible(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name        string
		args        []string
		wantContain []string
		wantOmit    []string
	}{
		{name: "default lists queued", args: []string{"queue", "list"}, wantContain: []string{"qi_eligible", "qi_stale"}},
		{name: "eligible filters blocked", args: []string{"queue", "list", "--eligible"}, wantContain: []string{"qi_eligible"}, wantOmit: []string{"qi_stale"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			configPath, _ := writeQueueCommandFixture(t)
			stdout := &bytes.Buffer{}
			stderr := &bytes.Buffer{}
			app := New(Deps{Stdout: stdout, Stderr: stderr})
			args := append(append([]string{}, tc.args...), "--config", configPath)
			if exitCode := app.Run(context.Background(), args); exitCode != 0 {
				t.Fatalf("Run(%v) exit code = %d, want 0; stderr=%q", args, exitCode, stderr.String())
			}
			output := stdout.String()
			for _, needle := range tc.wantContain {
				if !strings.Contains(output, needle) {
					t.Fatalf("queue list output missing %q\n%s", needle, output)
				}
			}
			for _, needle := range tc.wantOmit {
				if strings.Contains(output, needle) {
					t.Fatalf("queue list output includes %q\n%s", needle, output)
				}
			}
		})
	}
}

func TestQueueFailedCommandListsFailedItems(t *testing.T) {
	t.Parallel()

	configPath, _ := writeQueueCommandFixture(t)
	exitCode, stdout, stderr := runApp(t, "queue", "failed", "--type", "reviewer", "--json", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run([queue failed]) exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
	var decoded struct {
		Count int `json:"count"`
		Items []struct {
			QueueItem struct {
				ID     string `json:"id"`
				Status string `json:"status"`
			} `json:"queueItem"`
			Diagnosis struct {
				FailureClass string `json:"failureClass"`
				Retryable    *bool  `json:"retryable"`
			} `json:"diagnosis"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(stdout), &decoded); err != nil {
		t.Fatalf("json.Unmarshal() error = %v\noutput=%q", err, stdout)
	}
	if decoded.Count != 1 || len(decoded.Items) != 1 {
		t.Fatalf("queue failed output = %#v, want one item", decoded)
	}
	item := decoded.Items[0]
	if item.QueueItem.ID != "qi_failed" || item.QueueItem.Status != "failed" || item.Diagnosis.FailureClass != "github_transient" || item.Diagnosis.Retryable == nil || !*item.Diagnosis.Retryable {
		t.Fatalf("queue failed item = %#v, want retryable github_transient failed item", item)
	}
}

func writeQueueCommandFixture(t *testing.T) (string, *storage.Repositories) {
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
	projectID := "project_queue_cli"
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Looper", RepoPath: "/tmp/looper", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: "loop_eligible", Seq: 1, ProjectID: projectID, Type: "worker", TargetType: "project", Status: "running", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Loops.Upsert(loop_eligible) error = %v", err)
	}
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: "loop_stale", Seq: 2, ProjectID: projectID, Type: "worker", TargetType: "project", Status: "terminated", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Loops.Upsert(loop_stale) error = %v", err)
	}
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: "loop_failed", Seq: 3, ProjectID: projectID, Type: "reviewer", TargetType: "pull_request", Status: "failed", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Loops.Upsert(loop_failed) error = %v", err)
	}
	if err := repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: "qi_eligible", ProjectID: &projectID, LoopID: stringPtr("loop_eligible"), Type: "worker", TargetType: "project", TargetID: projectID, DedupeKey: "eligible", Priority: 1, Status: "queued", AvailableAt: now, MaxAttempts: 3, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Queue.Upsert(qi_eligible) error = %v", err)
	}
	if err := repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: "qi_stale", ProjectID: &projectID, LoopID: stringPtr("loop_stale"), Type: "worker", TargetType: "project", TargetID: projectID, DedupeKey: "stale", Priority: 1, Status: "queued", AvailableAt: now, MaxAttempts: 3, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Queue.Upsert(qi_stale) error = %v", err)
	}
	lastError := `Command exited with code 1: Post "https://api.github.com/graphql": EOF`
	lastErrorKind := "retryable_transient"
	prNumber := int64(42)
	if err := repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: "qi_failed", ProjectID: &projectID, LoopID: stringPtr("loop_failed"), Type: "reviewer", TargetType: "pull_request", TargetID: "pr:acme/looper:42", Repo: stringPtr("acme/looper"), PRNumber: &prNumber, DedupeKey: "failed", Priority: 1, Status: "failed", AvailableAt: now, Attempts: 2, MaxAttempts: 3, LastError: &lastError, LastErrorKind: &lastErrorKind, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Queue.Upsert(qi_failed) error = %v", err)
	}
	configPath := filepath.Join(root, "config.json")
	raw, err := json.Marshal(map[string]any{"storage": map[string]any{"dbPath": dbPath}})
	if err != nil {
		t.Fatalf("json.Marshal(config) error = %v", err)
	}
	if err := os.WriteFile(configPath, raw, 0o644); err != nil {
		t.Fatalf("WriteFile(config) error = %v", err)
	}
	return configPath, repos
}
