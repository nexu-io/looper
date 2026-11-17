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

func TestCollabTraceReadsHistoryOfflineWithoutExportingPrompts(t *testing.T) {
	configPath := writeLoopDiagnosticsFixture(t, "")
	stateDir := filepath.Dir(writeLoopDiagnosticsFixture(t, ""))
	db, err := storage.OpenSQLiteDB(context.Background(), filepath.Join(stateDir, "looper.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repos := storage.NewRepositories(db)
	loopID, runID := "loop_failed", "run_reviewer_failed"
	for _, kind := range []string{"agent.invoked", "task.received"} {
		if err := repos.Events.Append(context.Background(), storage.EventLogRecord{ID: kind, EventType: kind, LoopID: &loopID, RunID: &runID,
			PayloadJSON: `{"args":["private-prompt"],"summary":"private-summary","step":"execute","status":"failed"}`, CreatedAt: "2026-04-11T00:00:00.000Z"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, selector := range []string{"7", loopID, runID} {
		code, out, stderr := runApp(t, "collab", "trace", selector, "--state-dir", stateDir, "--config", configPath, "--json")
		if code != 0 || strings.Contains(out, "private-prompt") || strings.Contains(out, "private-summary") {
			t.Fatalf("unsafe or failed trace: %d %s %s", code, out, stderr)
		}
		var report map[string]any
		if json.Unmarshal([]byte(out), &report) != nil || report["traceId"] != loopID || report["coverage"] != "tracking_enabled" || report["modelUsage"] != nil {
			t.Fatalf("bad trace: %s", out)
		}
	}
	missing := filepath.Join(t.TempDir(), "absent")
	if code, _, _ := runApp(t, "collab", "trace", "1", "--state-dir", missing, "--config", configPath); code == 0 {
		t.Fatal("missing trace database accepted")
	}
	if _, err := os.Stat(filepath.Join(missing, "looper.sqlite")); !os.IsNotExist(err) {
		t.Fatal("trace read created a database")
	}
}
