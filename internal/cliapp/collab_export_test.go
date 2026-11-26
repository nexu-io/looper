package cliapp

import (
	"archive/zip"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/storage"
)

func TestCollabExportBundlesOnlyTheSelectedPlaneTask(t *testing.T) {
	ctx := context.Background()
	configPath := writeLoopDiagnosticsFixture(t, "")
	dir := filepath.Dir(configPath)
	db, err := storage.OpenSQLiteDB(ctx, filepath.Join(dir, "looper.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repos := storage.NewRepositories(db)
	const taskURL = "https://plane.example/team/browse/OPEND-123"
	const now = "2026-11-18T12:00:00.000Z"
	seed := func(id, link, status string, seq int64) {
		t.Helper()
		metadata := json.RawMessage(`{"worker":{"issueUrl":"` + link + `","strictDispatchId":"` +
			strings.TrimPrefix(id, "loop_plane_") + `","issueNumber":123,"prompt":"PRIVATE-PROMPT"},"bridge":{}}`)
		if err := repos.Loops.Upsert(ctx, storage.LoopRecord{ID: id, Seq: seq, ProjectID: "project_loop_diagnostics",
			Type: "worker", TargetType: "project", TargetID: stringPtr("project:local"), Status: status,
			MetadataJSON: stringPtr(string(metadata)), CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	seed("loop_plane_first", taskURL, "failed", 10)
	seed("loop_plane_second", taskURL+"/?token=PRIVATE-URL-QUERY", "completed", 11)
	seed("loop_plane_other", "https://plane.example/team/browse/OTHER-123", "completed", 12)
	seed("loop_manual", taskURL, "completed", 13)
	for _, result := range []struct{ loop, run, status, at string }{
		{"loop_plane_first", "run_first", "interrupted", "2026-11-18T12:00:00.000Z"},
		{"loop_plane_second", "run_retry", "failed", "2026-11-18T12:01:00.000Z"},
		{"loop_plane_second", "run_resumed", "success", "2026-11-18T12:02:00.000Z"},
	} {
		var detail *string
		if result.status != "success" {
			detail = stringPtr("PRIVATE-RAW-ERROR")
		}
		if err := repos.Runs.Upsert(ctx, storage.RunRecord{ID: result.run, LoopID: result.loop, Status: result.status,
			CurrentStep: stringPtr("execute"), ErrorMessage: detail, StartedAt: result.at,
			EndedAt: stringPtr(result.at), CreatedAt: result.at, UpdatedAt: result.at}); err != nil {
			t.Fatal(err)
		}
		if err := repos.AgentExecutions.Upsert(ctx, storage.AgentExecutionRecord{ID: "agent_" + result.run,
			LoopID: &result.loop, RunID: &result.run, Vendor: "codex", Status: result.status,
			OutputJSON: stringPtr(`{"stderrLogPath":"/home/PRIVATE-LOG-PATH","stderr":"PRIVATE-STDERR"}`),
			StartedAt:  now, EndedAt: stringPtr(now), CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	first := "loop_plane_first"
	if err := repos.Events.Append(ctx, storage.EventLogRecord{ID: "first-sync", EventType: "diagnostic.plane.sync.failed",
		LoopID: &first, PayloadJSON: `{"operation":"transition","errorKind":"plane_api_error","httpStatus":409}`, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	loop, run := "loop_plane_second", "run_resumed"
	for _, event := range []struct{ kind, body string }{
		{"task.received", `{"source":"collab","prompt":"PRIVATE-PROMPT"}`},
		{"agent.invoked", `{"step":{"token":"PRIVATE-NESTED"},"args":["PRIVATE-ARG"]}`},
		{"human.collaboration", `{"human":{"messageId":"om_reply","participantId":"ou_member","body":"PRIVATE-REPLY","messageUrl":"https://applink.feishu.cn/client/chat/open?openChatId=oc_team&position=42&token=PRIVATE-CHAT-QUERY"}}`},
		{"diagnostic.plane.sync.failed", `{"operation":"context","errorKind":"plane_api_error","httpStatus":503,"detail":"PRIVATE-RESPONSE"}`},
		{"diagnostic.plane.context.synced", `{"count":2}`},
		{"plane.state.synced", `{"state":"completed"}`},
	} {
		if err := repos.Events.Append(ctx, storage.EventLogRecord{ID: event.kind, EventType: event.kind,
			LoopID: &loop, RunID: &run, PayloadJSON: event.body, CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	// A task export must keep more than the analytics API's 5,000 events.
	if _, err := db.ExecContext(ctx, `WITH RECURSIVE ids(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM ids WHERE n<5001)
		INSERT INTO event_logs(id,event_type,loop_id,payload_json,created_at)
		SELECT 'probe:'||n,'test.observed',?,'{}',? FROM ids`, loop, now); err != nil {
		t.Fatal(err)
	}
	runExport := func(ref, path string) (int, string, string) {
		t.Helper()
		return runApp(t, "collab", "export", ref, "--output", path, "--state-dir", dir, "--config", configPath, "--json")
	}
	output := filepath.Join(dir, "output.zip")
	if code, stdout, stderr := runExport("opend-123", output); code != 0 || !strings.Contains(stdout, `"localDispatchCount": 2`) {
		t.Fatalf("export: %d %s %s", code, stdout, stderr)
	}
	archive := readCollabExport(t, output)
	if info, err := os.Stat(output); err != nil || info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("export permissions: %v %v", info, err)
	}
	var trace struct {
		TaskURL    string
		Dispatches []struct {
			DispatchID string
			Attempts   []json.RawMessage
			Events     []json.RawMessage
		}
	}
	if err := json.Unmarshal([]byte(archive["trace.json"]), &trace); err != nil {
		t.Fatal(err)
	}
	if trace.TaskURL != taskURL || len(trace.Dispatches) != 2 || trace.Dispatches[0].DispatchID != "first" ||
		len(trace.Dispatches[0].Attempts) != 1 || trace.Dispatches[1].DispatchID != "second" ||
		len(trace.Dispatches[1].Attempts) != 2 || len(trace.Dispatches[1].Events) != 5007 {
		t.Fatal("unexpected task scope or incomplete dispatch history")
	}
	if !strings.Contains(archive["trace.json"], "om_reply") ||
		!strings.Contains(archive["trace.json"], "client/chat/open?openChatId=oc_team") ||
		!strings.Contains(archive["diagnostics.json"], `"httpStatus": 503`) ||
		!strings.Contains(archive["summary.md"], "plane.state.synced at") ||
		!strings.Contains(archive["summary.md"], "Latest run: run_resumed (success)") ||
		!strings.Contains(archive["summary.md"], "no later confirmation in this trace: transition") ||
		strings.Contains(archive["summary.md"], "no later confirmation in this trace: context") {
		t.Fatal("missing trace, error, or success confirmation")
	}
	for name, contents := range archive {
		if strings.Contains(contents, "PRIVATE-") || strings.Contains(contents, "loop_manual") || strings.Contains(contents, "loop_plane_other") {
			t.Fatalf("%s included unrelated or private data", name)
		}
	}
	if code, _, _ := runExport("OPEND-123", output); code == 0 || readCollabExport(t, output)["summary.md"] != archive["summary.md"] {
		t.Fatal("export overwrote an existing archive")
	}
	unknown := filepath.Join(dir, "unknown.zip")
	if code, _, stderr := runExport("OPEND-124", unknown); code == 0 || !strings.Contains(stderr, "no matching Plane dispatch") {
		t.Fatalf("missing task: %d %s", code, stderr)
	}
	seed("loop_plane_ambiguous", "https://plane.example/other-team/browse/OPEND-123", "running", 14)
	if code, _, stderr := runExport("OPEND-123", unknown); code == 0 || !strings.Contains(stderr, "multiple Plane task URLs") {
		t.Fatalf("ambiguous task: %d %s", code, stderr)
	}
	if _, err := os.Stat(unknown); !os.IsNotExist(err) {
		t.Fatalf("failed export created a file: %v", err)
	}
	if code, out, stderr := runExport(taskURL, filepath.Join(dir, "scoped.zip")); code != 0 || !strings.Contains(out, `"localDispatchCount": 2`) {
		t.Fatalf("URL scope: %d %s %s", code, out, stderr)
	}
	issueID := "44444444-4444-4444-8444-444444444444"
	browseURL := "https://plane.example/team/browse/issues/" + issueID
	projectURL := "https://plane.example/team/projects/55555555-5555-4555-8555-555555555555/issues/" + issueID
	seed("loop_plane_uuid_first", browseURL, "interrupted", 15)
	seed("loop_plane_uuid_second", projectURL, "completed", 16)
	seed("loop_plane_uuid_other", "https://plane.example/other-team/browse/issues/"+issueID, "running", 17)
	for name, ref := range map[string]string{"browse": browseURL, "project": projectURL} {
		outpath := filepath.Join(dir, "legacy-"+name+".zip")
		code, out, stderr := runExport(ref, outpath)
		if code != 0 || !strings.Contains(out, `"localDispatchCount": 2`) || !strings.Contains(out, `"taskId": "`+issueID+`"`) {
			t.Fatalf("UUID URL scope: %d %s %s", code, out, stderr)
		}
		if strings.Contains(readCollabExport(t, outpath)["trace.json"], "loop_plane_uuid_other") {
			t.Fatal("UUID export crossed workspaces")
		}
	}
}

func readCollabExport(t *testing.T, path string) map[string]string {
	t.Helper()
	archive, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	result := map[string]string{}
	for _, entry := range archive.File {
		reader, err := entry.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(reader)
		reader.Close()
		if err != nil {
			t.Fatal(err)
		}
		result[entry.Name] = string(data)
	}
	if len(result) != 3 {
		t.Fatalf("unexpected number of archive entries: %d", len(result))
	}
	return result
}
