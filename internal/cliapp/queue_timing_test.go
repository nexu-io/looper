package cliapp

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestPSQueueTiming(t *testing.T) {
	now := time.Now()
	enqueued := now.Add(-8*time.Minute - 30*time.Second).Format(time.RFC3339Nano)
	future := now.Add(2 * time.Minute).Format(time.RFC3339Nano)
	due := now.Add(-time.Minute).Format(time.RFC3339Nano)
	for _, tc := range []struct {
		name, status, display, started, available, failure, age, reason string
	}{
		{"delayed", "queued", "queued", enqueued, future, "", "8m", "eligible in"},
		{"due", "queued", "queued", enqueued, due, "", "8m", "waiting for scheduler"},
		{"retry", "queued", "backing_off", enqueued, future, "network timeout", "8m", "eligible in"},
		{"legacy future", "queued", "queued", future, "", "", "-", "eligible in"},
		{"legacy due", "queued", "queued", due, "", "", "-", "waiting for scheduler"},
		{"missing queue", "queued", "queued", "", "", "", "-", "-"},
		{"running", "running", "running", enqueued, "", "", "8m", "-"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			item := map[string]any{"seq": 5015, "type": "fixer", "status": tc.status, "displayStatus": tc.display, "target": map[string]string{"label": "acme/repo#76"}}
			for key, value := range map[string]string{"startedAt": tc.started, "availableAt": tc.available, "lastFailureReason": tc.failure} {
				if value != "" {
					item[key] = value
				}
			}
			payload, err := json.Marshal(map[string]any{"items": []any{item}})
			if err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			if err := writeHumanActiveRuns(&out, payload); err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSpace(out.String()), "\n")
			fields := strings.Fields(lines[len(lines)-1])
			if len(fields) < 9 || fields[7] != tc.age {
				t.Fatalf("unexpected queue timing:\n%s", out.String())
			}
			reason := strings.Join(fields[8:], " ")
			if tc.reason == "eligible in" {
				if !strings.HasPrefix(reason, "eligible in ") {
					t.Fatalf("missing countdown: %s", reason)
				}
				parts := strings.SplitN(strings.TrimPrefix(reason, "eligible in "), "; ", 2)
				remaining, err := time.ParseDuration(parts[0])
				if err != nil || remaining <= 0 || remaining > 2*time.Minute {
					t.Fatalf("invalid countdown: %s", reason)
				}
				if tc.failure != "" && (len(parts) != 2 || parts[1] != tc.failure) {
					t.Fatalf("missing retry reason: %s", reason)
				}
			} else if reason != tc.reason {
				t.Fatalf("reason = %q, want %q", reason, tc.reason)
			}
		})
	}
}

func TestFormatQueueWaitBoundary(t *testing.T) {
	now := time.Date(2026, 9, 11, 15, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		at, want string
	}{
		{now.Add(time.Nanosecond).Format(time.RFC3339Nano), "eligible in 1s"},
		{now.Add(time.Second).Format(time.RFC3339Nano), "eligible in 1s"},
		{now.Format(time.RFC3339Nano), "waiting for scheduler"},
		{"invalid", ""},
	} {
		if got := formatQueueWait(&tc.at, now); got != tc.want {
			t.Errorf("formatQueueWait(%q) = %q, want %q", tc.at, got, tc.want)
		}
	}
}
