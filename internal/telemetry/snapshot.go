// Package telemetry exports explicit task metadata, never agent inputs or logs.
package telemetry

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/nexu-io/looper/internal/storage"
)

var attributeKeys = strings.Fields("source step startStep resumed status from to durationMs resumePolicy state waitKind transport askedAt questionMessageId messageIds messageId elapsedRuntimeSeconds timeoutType parseStatus repo role branch baseSha prUrl validationStatus count reference dispatchId issueNumber executionMode")

func snapshot(ctx context.Context, repos *storage.Repositories, loop storage.LoopRecord) (map[string]any, error) {
	var metadata struct {
		Worker struct {
			Title      string `json:"title"`
			DispatchID string `json:"strictDispatchId"`
		} `json:"worker"`
	}
	if loop.MetadataJSON != nil {
		_ = json.Unmarshal([]byte(*loop.MetadataJSON), &metadata)
	}
	if metadata.Worker.DispatchID == "" {
		return nil, nil
	}
	runs, err := repos.Runs.ListByLoop(ctx, loop.ID)
	if err != nil {
		return nil, err
	}
	attempts := []map[string]any{}
	for _, run := range runs {
		executions, err := repos.AgentExecutions.ListByRun(ctx, run.ID)
		if err != nil {
			return nil, err
		}
		agents := []map[string]any{}
		for _, a := range executions {
			agents = append(agents, map[string]any{"id": a.ID, "vendor": a.Vendor, "status": a.Status, "startedAt": a.StartedAt, "endedAt": a.EndedAt})
		}
		attempts = append(attempts, map[string]any{"id": run.ID, "status": run.Status, "startedAt": run.StartedAt, "endedAt": run.EndedAt, "agents": agents})
	}
	records, err := repos.Events.ListByLoop(ctx, loop.ID)
	if err != nil {
		return nil, err
	}
	events := []map[string]any{}
	coverage := "partial_legacy"
	for _, record := range records {
		// Diagnostic events are reserved for the local support bundle.
		if strings.HasPrefix(record.EventType, "diagnostic.") {
			continue
		}
		if record.EventType == "task.received" {
			coverage = "tracking_enabled"
		}
		var source map[string]json.RawMessage
		if err := json.Unmarshal([]byte(record.PayloadJSON), &source); err != nil {
			return nil, fmt.Errorf("invalid event metadata %s", record.ID)
		}
		attrs := map[string]json.RawMessage{}
		for _, key := range attributeKeys {
			value, ok := source[key]
			if !ok {
				continue
			}
			if key == "repo" || key == "prUrl" {
				var link string
				if json.Unmarshal(value, &link) != nil {
					continue
				}
				u, err := url.Parse(link)
				if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
					continue
				}
			}
			attrs[key] = value
		}
		event := map[string]any{"id": record.ID, "type": record.EventType, "at": record.CreatedAt, "runId": record.RunID, "attributes": attrs}
		if record.EventType == "human.collaboration" {
			// The host captures this exact metadata shape from the transport.
			var human struct {
				Source        string `json:"source"`
				ParticipantID string `json:"participantId"`
				Kind          string `json:"kind"`
				MessageID     string `json:"messageId"`
				MessageURL    string `json:"messageUrl"`
			}
			if err := json.Unmarshal(source["human"], &human); err != nil {
				return nil, fmt.Errorf("invalid collaboration metadata %s", record.ID)
			}
			event["human"] = human
		}
		events = append(events, event)
	}
	return map[string]any{"id": loop.ID, "sequence": loop.Seq, "title": metadata.Worker.Title, "type": loop.Type, "status": loop.Status,
		"createdAt": loop.CreatedAt, "updatedAt": loop.UpdatedAt, "dispatchId": metadata.Worker.DispatchID, "coverage": coverage, "runs": attempts, "events": events}, nil
}
