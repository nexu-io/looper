package cliapp

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
)

func collabMemoryOverview(ctx context.Context, db *sql.DB) (map[string]any, error) {
	// Only metadata lives in this view; published bodies remain under Plane access checks.
	rows, err := db.QueryContext(ctx, `SELECT event_type, COALESCE(loop_id,''), created_at, payload_json FROM event_logs WHERE event_type LIKE 'memory.%' ORDER BY created_at DESC LIMIT 500`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := []map[string]any{}
	readTasks, adoptionTasks := map[string]bool{}, map[string]bool{}
	for rows.Next() {
		var kind, task, at, payload string
		if err := rows.Scan(&kind, &task, &at, &payload); err != nil {
			return nil, err
		}
		var attrs map[string]any
		_ = json.Unmarshal([]byte(payload), &attrs)
		if kind == "memory.read_completed" {
			readTasks[task] = true
		}
		if kind == "memory.feedback" && attrs["result"] == "adopted" {
			adoptionTasks[task] = true
		}
		events = append(events, map[string]any{"type": kind, "taskId": task, "at": at, "memoryId": attrs["memoryId"], "revision": attrs["revision"], "indexes": attrs["indexes"], "result": attrs["result"]})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	var pending, failed int
	var last sql.NullString
	err = db.QueryRowContext(ctx, `SELECT count(*),COALESCE(sum(error<>''),0),max(acknowledged_at) FROM memory_operations WHERE acknowledged=0`).Scan(&pending, &failed, &last)
	if err != nil && strings.Contains(err.Error(), "no such table") {
		return map[string]any{"enabled": false, "events": events}, nil
	}
	if err != nil {
		return nil, err
	}
	_ = db.QueryRowContext(ctx, `SELECT max(acknowledged_at) FROM memory_operations`).Scan(&last)
	return map[string]any{"enabled": true, "scope": "本机数据", "events": events, "sampleLimit": 500, "bodyReadTasks": len(readTasks), "adoptionTasks": len(adoptionTasks), "pendingUploads": pending, "uploadErrors": failed, "lastUploadAt": last.String, "costUSD": nil}, nil
}
