package cliapp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/storage"
)

func TestCollabTaskLogsReadOwnDatabaseWithoutRunningHost(t *testing.T) {
	t.Parallel()
	configPath := writeLoopDiagnosticsFixture(t, "")
	stateDir := filepath.Dir(writeLoopDiagnosticsFixture(t, ""))
	db, err := storage.OpenSQLiteDB(context.Background(), filepath.Join(stateDir, "looper.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	logDir := filepath.Join(stateDir, "logs")
	if err := os.MkdirAll(logDir, 0700); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(logDir, "stdout.log")
	if err := os.WriteFile(logPath, []byte("first line\nlast line\n"), 0600); err != nil {
		t.Fatal(err)
	}
	output, err := json.Marshal(map[string]string{"stdout": "stale inline output", "stdoutLogPath": logPath, "stderr": "task diagnostic"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE agent_executions SET output_json = ? WHERE id = ?", string(output), "agent_failed"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, selector, want, absent string
		flags                        []string
	}{
		{name: "sequence", selector: "7", want: "last line", absent: "stale inline output"},
		{name: "loop ID", selector: "loop_failed", want: "first line"},
		{name: "run ID", selector: "run_reviewer_failed", want: "first line"},
		{name: "tail", selector: "7", flags: []string{"--tail", "1"}, want: "last line", absent: "first line"},
		{name: "stderr", selector: "7", flags: []string{"--stderr"}, want: "task diagnostic", absent: "last line"},
		{name: "full", selector: "7", flags: []string{"--full"}, want: "first line"},
		{name: "JSON", selector: "7", flags: []string{"--json"}, want: `"executionId": "agent_failed"`},
		{name: "pre-agent failure", selector: "8", want: "fatal: worktree is locked"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"collab", "logs", tc.selector, "--config", configPath, "--state-dir", stateDir}, tc.flags...)
			code, stdout, stderr := runApp(t, args...)
			if code != 0 || !strings.Contains(stdout, tc.want) || tc.absent != "" && strings.Contains(stdout, tc.absent) {
				t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout, stderr)
			}
		})
	}
	for _, flags := range [][]string{{"999"}, {"7", "--tail", "0"}, {"--tail", "2"}} {
		args := append([]string{"collab", "logs", "--config", configPath, "--state-dir", stateDir}, flags...)
		if code, _, _ := runApp(t, args...); code == 0 {
			t.Fatalf("accepted invalid logs arguments: %v", flags)
		}
	}
	newer := "2026-04-12T12:00:00.000Z"
	if err := storage.NewRepositories(db).Runs.Upsert(context.Background(), storage.RunRecord{ID: "run_newer", LoopID: "loop_failed", Status: "running", StartedAt: newer, CreatedAt: newer, UpdatedAt: newer}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ selector, want string }{{"7", "No agent output"}, {"run_reviewer_failed", "last line"}} {
		code, stdout, stderr := runApp(t, "collab", "logs", tc.selector, "--config", configPath, "--state-dir", stateDir)
		if code != 0 || !strings.Contains(stdout, tc.want) {
			t.Fatalf("historical selection %s: exit=%d stdout=%q stderr=%q", tc.selector, code, stdout, stderr)
		}
	}
	if err := os.WriteFile(filepath.Join(stateDir, "collab.log"), []byte("host diagnostic\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if code, stdout, stderr := runApp(t, "collab", "logs", "--config", configPath, "--state-dir", stateDir); code != 0 || stdout != "host diagnostic\n" {
		t.Fatalf("host logs: exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestCollabTaskLogsDoNotCreateMissingState(t *testing.T) {
	t.Parallel()
	configPath := writeLoopDiagnosticsFixture(t, "")
	stateDir := filepath.Join(t.TempDir(), "never-started")
	if code, _, _ := runApp(t, "collab", "logs", "7", "--config", configPath, "--state-dir", stateDir); code == 0 {
		t.Fatal("accepted missing collab database")
	}
	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		t.Fatalf("log inspection created state: %v", err)
	}
}
