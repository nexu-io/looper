package cliapp

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/storage"
	"github.com/spf13/cobra"
)

func (r *commandRuntime) collabTrace(cmd *cobra.Command, args []string) error {
	_, dir, err := r.collabConfig(cmd)
	if err != nil {
		return err
	}
	db, err := openCollabReadOnly(dir)
	if err != nil {
		return err
	}
	defer db.Close()
	result, err := collabTraceReport(cmd.Context(), storage.NewRepositories(db), strings.TrimSpace(args[0]))
	if err != nil {
		return err
	}
	if getBoolFlag(cmd, "json") {
		return writeJSON(cmd.OutOrStdout(), result)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "#%d %s · %s · %s\n", result["seq"], result["status"], result["traceId"], result["coverage"])
	for _, event := range result["events"].([]map[string]any) {
		payload, _ := json.Marshal(event["payload"])
		fmt.Fprintf(cmd.OutOrStdout(), "%s  %s  %s\n", event["at"], event["type"], payload)
	}
	return nil
}

func collabTraceReport(ctx context.Context, repos *storage.Repositories, selector string) (map[string]any, error) {
	resolved, err := resolveLoopSelector(ctx, repos, selector)
	if err != nil {
		return nil, err
	}
	records, err := repos.Events.ListByLoop(ctx, resolved.Loop.ID)
	if err != nil {
		return nil, err
	}
	runs, err := repos.Runs.ListByLoop(ctx, resolved.Loop.ID)
	if err != nil {
		return nil, err
	}
	events := []map[string]any{}
	coverage := "partial_legacy"
	for _, record := range records {
		if record.EventType == "task.received" {
			coverage = "tracking_enabled"
		}
		if resolved.SelectorKind == "runId" && record.RunID != nil && *record.RunID != resolved.Run.ID {
			continue
		}
		events = append(events, map[string]any{"eventId": record.ID, "type": record.EventType, "at": record.CreatedAt,
			"runId": record.RunID, "entityId": record.EntityID, "actor": record.ActorID, "payload": tracePayload(record.PayloadJSON)})
	}
	attempts := []map[string]any{}
	for _, run := range runs {
		if resolved.SelectorKind == "runId" && run.ID != resolved.Run.ID {
			continue
		}
		agents, err := repos.AgentExecutions.ListByRun(ctx, run.ID)
		if err != nil {
			return nil, err
		}
		executions := []map[string]any{}
		for _, a := range agents {
			var output map[string]json.RawMessage
			if a.OutputJSON != nil {
				_ = json.Unmarshal([]byte(*a.OutputJSON), &output)
			}
			executions = append(executions, map[string]any{"executionId": a.ID, "vendor": a.Vendor, "status": a.Status,
				"startedAt": a.StartedAt, "endedAt": a.EndedAt, "durationMs": traceDuration(a.StartedAt, a.EndedAt),
				"stdoutLogPath": output["stdoutLogPath"], "stderrLogPath": output["stderrLogPath"]})
		}
		attempts = append(attempts, map[string]any{"runId": run.ID, "status": run.Status, "currentStep": run.CurrentStep,
			"startedAt": run.StartedAt, "endedAt": run.EndedAt, "durationMs": traceDuration(run.StartedAt, run.EndedAt), "agents": executions})
	}
	var metadata struct {
		Worker struct {
			StrictDispatchID string `json:"strictDispatchId"`
			IssueNumber      int64  `json:"issueNumber"`
			ExecutionMode    string `json:"executionMode"`
		} `json:"worker"`
		Bridge json.RawMessage `json:"bridge"`
	}
	if resolved.Loop.MetadataJSON != nil {
		_ = json.Unmarshal([]byte(*resolved.Loop.MetadataJSON), &metadata)
	}
	if len(metadata.Bridge) > 0 {
		metadata.Worker.ExecutionMode = "feishu_bridge"
	}
	result := map[string]any{"schemaVersion": 1, "traceId": resolved.Loop.ID, "seq": resolved.Loop.Seq,
		"status": resolved.Loop.Status, "createdAt": resolved.Loop.CreatedAt, "projectId": resolved.Loop.ProjectID, "dispatchId": metadata.Worker.StrictDispatchID, "issueNumber": metadata.Worker.IssueNumber,
		"executionMode": metadata.Worker.ExecutionMode, "coverage": coverage, "attempts": attempts, "events": events,
		"modelUsage": nil, "costUSD": nil, "completionMeaning": "execution status; not human acceptance, merge or release"}
	return result, nil
}

// The legacy agent.invoked event contains argv and prompt. Never export it here.
func tracePayload(raw string) map[string]any {
	var source map[string]any
	_ = json.Unmarshal([]byte(raw), &source)
	result := map[string]any{}
	for _, key := range strings.Fields("source workspace planeProjectId dispatchId issueId issueNumber nodeId routingRepo executionMode step startStep resumed status from to durationMs resumePolicy state waitKind transport askedAt questionMessageId messageIds messageId elapsedRuntimeSeconds timeoutType parseStatus repo role branch baseSha prUrl validationStatus count reference") {
		if value, ok := source[key]; ok {
			result[key] = value
		}
	}
	return result
}

func traceDuration(start string, end *string) *int64 {
	if end == nil {
		return nil
	}
	a, e1 := time.Parse(time.RFC3339Nano, start)
	b, e2 := time.Parse(time.RFC3339Nano, *end)
	if e1 != nil || e2 != nil || b.Before(a) {
		return nil
	}
	ms := b.Sub(a).Milliseconds()
	return &ms
}

func openCollabReadOnly(dir string) (*sql.DB, error) {
	dsn := url.URL{Scheme: "file", Path: filepath.Join(dir, "looper.sqlite"), RawQuery: "mode=ro&_busy_timeout=5000"}
	db, err := sql.Open(storage.DriverName, dsn.String())
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}
