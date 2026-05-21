package cliapp

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gitinfra "github.com/nexu-io/looper/internal/infra/git"
	"github.com/nexu-io/looper/internal/storage"
)

func TestWorktreeCleanupDefaultsToDryRun(t *testing.T) {
	t.Parallel()

	fixture := newWorktreeCleanupFixture(t)
	record := fixture.createWorktree(t, "looper/test-dry-run")

	exitCode, stdout, stderr := fixture.run("worktree", "cleanup")
	if exitCode != 0 {
		t.Fatalf("Run(worktree cleanup) exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
	if !strings.Contains(stdout, "Worktree cleanup (dry-run)") || !strings.Contains(stdout, "clean\tterminal_clean") {
		t.Fatalf("stdout = %q, want dry-run clean candidate", stdout)
	}
	requirePathExists(t, record.WorktreePath)
}

func TestWorktreeCleanupDryRunFlagMatchesDefault(t *testing.T) {
	t.Parallel()

	fixture := newWorktreeCleanupFixture(t)
	record := fixture.createWorktree(t, "looper/test-explicit-dry-run")

	exitCode, stdout, stderr := fixture.run("worktree", "cleanup", "--dry-run")
	if exitCode != 0 {
		t.Fatalf("Run(worktree cleanup --dry-run) exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
	if !strings.Contains(stdout, "Worktree cleanup (dry-run)") || !strings.Contains(stdout, record.WorktreePath) {
		t.Fatalf("stdout = %q, want explicit dry-run output", stdout)
	}
	requirePathExists(t, record.WorktreePath)
}

func TestWorktreeCleanupConfirmDeletesTerminalCleanWorktree(t *testing.T) {
	t.Parallel()

	fixture := newWorktreeCleanupFixture(t)
	record := fixture.createWorktree(t, "looper/test-confirm")

	exitCode, stdout, stderr := fixture.run("worktree", "cleanup", "--confirm")
	if exitCode != 0 {
		t.Fatalf("Run(worktree cleanup --confirm) exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
	if !strings.Contains(stdout, "Worktree cleanup (confirmed): inspected=1 eligible=1 cleaned=1 skipped=0 errors=0") {
		t.Fatalf("stdout = %q, want confirmed cleanup summary", stdout)
	}
	if _, err := os.Stat(record.WorktreePath); !os.IsNotExist(err) {
		t.Fatalf("worktree path exists after confirmed cleanup: %v", err)
	}
	updated, err := fixture.repos.Worktrees.GetByID(context.Background(), record.ID)
	if err != nil {
		t.Fatalf("Worktrees.GetByID() error = %v", err)
	}
	if updated == nil || updated.Status != "cleaned" || updated.CleanedAt == nil {
		t.Fatalf("updated worktree = %#v, want cleaned status", updated)
	}
}

func TestWorktreeCleanupJSONOutputAndDirtySkip(t *testing.T) {
	t.Parallel()

	fixture := newWorktreeCleanupFixture(t)
	record := fixture.createWorktree(t, "looper/test-json")
	if err := os.WriteFile(filepath.Join(record.WorktreePath, "dirty.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatalf("write dirty file: %v", err)
	}

	exitCode, stdout, stderr := fixture.run("--json", "worktree", "cleanup")
	if exitCode != 0 {
		t.Fatalf("Run(--json worktree cleanup) exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
	var decoded struct {
		DryRun  bool `json:"dryRun"`
		Summary struct {
			Inspected int `json:"inspected"`
			Skipped   int `json:"skipped"`
		} `json:"summary"`
		Candidates []struct {
			Action string `json:"action"`
			Reason string `json:"reason"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal([]byte(stdout), &decoded); err != nil {
		t.Fatalf("decode JSON stdout: %v\n%s", err, stdout)
	}
	if !decoded.DryRun || decoded.Summary.Inspected != 1 || decoded.Summary.Skipped != 1 || len(decoded.Candidates) != 1 {
		t.Fatalf("decoded = %#v, want dry-run skipped candidate", decoded)
	}
	if decoded.Candidates[0].Action != "skip" || decoded.Candidates[0].Reason != "dirty_worktree" {
		t.Fatalf("candidate = %#v, want dirty skip", decoded.Candidates[0])
	}
}

func TestWorktreeCleanupValidationRejectsConflictingFlags(t *testing.T) {
	t.Parallel()

	fixture := newWorktreeCleanupFixture(t)
	exitCode, _, stderr := fixture.run("worktree", "cleanup", "--dry-run", "--confirm")
	if exitCode == 0 {
		t.Fatalf("Run(worktree cleanup --dry-run --confirm) exit code = 0, want failure")
	}
	if !strings.Contains(stderr, "--confirm and --dry-run cannot be used together") {
		t.Fatalf("stderr = %q, want conflicting flag validation", stderr)
	}
}

func TestWorktreeCleanupSkipsActiveLoopWorktree(t *testing.T) {
	t.Parallel()

	fixture := newWorktreeCleanupFixture(t)
	record := fixture.createWorktree(t, "looper/test-active")
	now := "2026-05-20T00:00:00.000Z"
	checkpoint := `{"worktree":{"id":` + jsonString(record.ID) + `,"path":` + jsonString(record.WorktreePath) + `}}`
	if err := fixture.repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: "loop_active", Seq: 1, ProjectID: "project_1", Type: "worker", TargetType: "project", Status: "running", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	if err := fixture.repos.Runs.Upsert(context.Background(), storage.RunRecord{ID: "run_active", LoopID: "loop_active", Status: "running", CheckpointJSON: &checkpoint, StartedAt: now, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}

	exitCode, stdout, stderr := fixture.run("worktree", "cleanup", "--confirm")
	if exitCode != 0 {
		t.Fatalf("Run(worktree cleanup --confirm) exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
	if !strings.Contains(stdout, "skip\treferenced by running run") {
		t.Fatalf("stdout = %q, want active loop skip", stdout)
	}
	requirePathExists(t, record.WorktreePath)
}

type worktreeCleanupFixture struct {
	configPath string
	repoPath   string
	root       string
	repos      *storage.Repositories
}

func newWorktreeCleanupFixture(t *testing.T) worktreeCleanupFixture {
	t.Helper()
	repoPath := initSampleGitRepo(t)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "looper.sqlite")
	root := filepath.Join(dir, "worktrees")
	configPath := filepath.Join(dir, "config.json")
	baseBranch := "main"
	cfg := map[string]any{
		"storage": map[string]any{"dbPath": dbPath},
		"tools":   map[string]any{"gitPath": "git"},
		"daemon": map[string]any{
			"worktreeCleanup": map[string]any{
				"retentionDays":  0,
				"maxPerTick":     10,
				"includeOrphans": true,
			},
		},
		"projects": []map[string]any{{
			"id":           "project_1",
			"name":         "Test Project",
			"repoPath":     repoPath,
			"baseBranch":   baseBranch,
			"worktreeRoot": root,
		}},
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if err := os.WriteFile(configPath, raw, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	coordinator, err := storage.OpenSQLiteCoordinator(context.Background(), dbPath, storage.SQLiteCoordinatorOptions{Migrations: storage.EmbeddedMigrations})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	if _, err := coordinator.MigrationRunner().RunPending(context.Background()); err != nil {
		t.Fatalf("MigrationRunner.RunPending() error = %v", err)
	}
	repos := storage.NewRepositories(coordinator.DB())
	now := "2026-05-20T00:00:00.000Z"
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "project_1", Name: "Test Project", RepoPath: repoPath, BaseBranch: &baseBranch, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	return worktreeCleanupFixture{configPath: configPath, repoPath: repoPath, root: root, repos: repos}
}

func (f worktreeCleanupFixture) createWorktree(t *testing.T, branch string) storage.WorktreeRecord {
	t.Helper()
	gateway := gitinfra.New(gitinfra.Options{GitPath: "git", Repos: f.repos})
	record, err := gateway.CreateWorktree(context.Background(), gitinfra.CreateWorktreeInput{ProjectID: "project_1", RepoPath: f.repoPath, WorktreeRoot: f.root, Branch: branch, BaseBranch: "main", ProtectedBranches: []string{"main"}})
	if err != nil {
		t.Fatalf("CreateWorktree() error = %v", err)
	}
	return record
}

func (f worktreeCleanupFixture) run(args ...string) (int, string, string) {
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := New(Deps{Stdout: stdout, Stderr: stderr})
	fullArgs := append([]string{}, args...)
	fullArgs = append(fullArgs, "--config", f.configPath)
	exitCode := app.Run(context.Background(), fullArgs)
	return exitCode, stdout.String(), stderr.String()
}

func jsonString(value string) string {
	payload, _ := json.Marshal(value)
	return string(payload)
}

func requirePathExists(tb testing.TB, path string) {
	tb.Helper()
	if _, err := os.Stat(path); err != nil {
		tb.Fatalf("expected path %s to exist: %v", path, err)
	}
}
