package cliapp

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/nexu-io/looper/internal/agent"
	"github.com/nexu-io/looper/internal/storage"
)

var collabTraceToken = regexp.MustCompile(`^[a-zA-Z0-9_.:/-]{1,255}$`)
var collabFeishuID = regexp.MustCompile(`^(om_|omt_|oc_|ou_)[a-zA-Z0-9_-]{1,128}$`)
var collabFeishuPosition = regexp.MustCompile(`^[0-9]{1,20}$`)

func collabExportPayload(payload map[string]any, event storage.EventLogRecord) map[string]any {
	// Legacy payloads are untrusted. Even allowlisted fields must have their
	// expected shape before being copied into a file intended for sharing.
	for key, value := range payload {
		switch typed := value.(type) {
		case string:
			if !collabTraceToken.MatchString(typed) || strings.Contains(typed, "://") || strings.HasPrefix(typed, "/") {
				delete(payload, key)
			}
		case []any:
			delete(payload, key)
			if key == "messageIds" {
				ids := []string{}
				for _, id := range typed {
					if value, ok := id.(string); ok && collabFeishuID.MatchString(value) {
						ids = append(ids, value)
					}
				}
				payload[key] = ids
			}
		case float64, bool:
		default:
			delete(payload, key)
		}
	}
	switch event.EventType {
	case "repository.reported":
		var raw any
		_ = json.Unmarshal([]byte(event.PayloadJSON), &raw)
		reports := agent.ParseRepositoryReports([]any{raw})
		if len(reports) == 1 {
			report := reports[0]
			payload["repo"], payload["prUrl"] = report.Repo, report.PRURL
		}
	case "human.collaboration":
		var raw struct {
			Human struct {
				ParticipantID string `json:"participantId"`
				MessageID     string `json:"messageId"`
				MessageURL    string `json:"messageUrl"`
			} `json:"human"`
		}
		if json.Unmarshal([]byte(event.PayloadJSON), &raw) == nil && collabFeishuID.MatchString(raw.Human.MessageID) {
			payload["messageId"] = raw.Human.MessageID
			if collabFeishuID.MatchString(raw.Human.ParticipantID) {
				payload["participantId"] = raw.Human.ParticipantID
			}
			if link := collabFeishuMessageURL(raw.Human.MessageURL); link != "" {
				payload["messageUrl"] = link
			}
		}
	}
	return payload
}

func collabFeishuMessageURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host != "applink.feishu.cn" || u.User != nil || u.Path != "/client/chat/open" {
		return ""
	}
	query := u.Query()
	chat, position := query.Get("openChatId"), query.Get("position")
	if !strings.HasPrefix(chat, "oc_") || !collabFeishuID.MatchString(chat) || !collabFeishuPosition.MatchString(position) {
		return ""
	}
	link := url.Values{"openChatId": {chat}, "position": {position}}
	return "https://applink.feishu.cn/client/chat/open?" + link.Encode()
}

func collabErrorKind(status string, detail *string) string {
	switch status {
	case "interrupted":
		return "interrupted"
	case "timeout", "killed", "cancelled":
		return status
	}
	if detail != nil {
		switch *detail {
		case "Host restarted without a terminal callback; outcome unknown":
			return "interrupted"
		case "context deadline exceeded":
			return "timeout"
		case "context canceled":
			return "cancelled"
		case "":
		default:
			return "error_recorded"
		}
	}
	if status == "failed" {
		return "failed"
	}
	return ""
}

func collabDiagnostics(report map[string]any) []map[string]any {
	errors := []map[string]any{}
	for _, attempt := range report["attempts"].([]map[string]any) {
		if attempt["errorKind"] != "" {
			errors = append(errors, map[string]any{"dispatchId": report["dispatchId"], "runId": attempt["runId"],
				"at": attempt["endedAt"], "step": attempt["currentStep"], "errorKind": attempt["errorKind"]})
		}
		for _, execution := range attempt["agents"].([]map[string]any) {
			if execution["errorKind"] != "" {
				errors = append(errors, map[string]any{"dispatchId": report["dispatchId"], "runId": attempt["runId"],
					"executionId": execution["executionId"], "at": execution["endedAt"], "errorKind": execution["errorKind"]})
			}
		}
	}
	for _, event := range report["events"].([]map[string]any) {
		switch event["type"] {
		case "diagnostic.plane.sync.failed", "diagnostic.agent.conclusion.failed":
			errors = append(errors, map[string]any{"dispatchId": report["dispatchId"], "runId": event["runId"],
				"eventId": event["eventId"], "at": event["at"], "type": event["type"], "details": event["payload"]})
		}
	}
	return errors
}

func collabExportSummary(key, taskURL, capturedAt string, reports []map[string]any) string {
	var summary strings.Builder
	fmt.Fprintf(&summary, "# %s — Looper collaboration diagnostics\n\n- Task: %s\n- Captured at: %s\n- Local dispatches: %d\n- Coverage: %s\n",
		key, taskURL, capturedAt, len(reports), collabExportCoverage)
	for _, report := range reports {
		attempts := report["attempts"].([]map[string]any)
		events := report["events"].([]map[string]any)
		fmt.Fprintf(&summary, "\n## Dispatch %s · local task #%d\n\n- Current execution status: %s\n- Local runs: %d\n- Recorded events: %d (%s)\n",
			report["dispatchId"], report["seq"], report["status"], len(attempts), len(events), report["coverage"])
		confirmed := "No confirmation recorded"
		var contextFailure, stateFailure, conclusionFailure string
		for _, event := range events {
			payload := event["payload"].(map[string]any)
			switch event["type"] {
			case "diagnostic.plane.sync.failed":
				operation, _ := payload["operation"].(string)
				kind, _ := payload["errorKind"].(string)
				if kind == "" {
					kind = "unknown"
				}
				detail := fmt.Sprintf("%s at %s (%s)", operation, event["at"], kind)
				if code := payload["httpStatus"]; code != nil {
					detail += fmt.Sprintf(", HTTP %v", code)
				}
				switch operation {
				case "context":
					contextFailure = detail
				case "transition":
					stateFailure = detail
				}
			case "diagnostic.plane.context.synced":
				contextFailure = ""
			case "plane.state.synced":
				stateFailure = ""
			case "agent.completed":
				if payload["status"] != "success" && payload["status"] != "completed" {
					continue
				}
				conclusionFailure = ""
			case "diagnostic.agent.conclusion.failed":
				conclusionFailure, _ = payload["errorKind"].(string)
				continue
			case "task.received", "plane.dispatch.received", "run.started", "step.completed", "human.requested", "human.answered", "human.collaboration", "feishu.receipt":
			default:
				continue
			}
			if event["type"] != "diagnostic.plane.sync.failed" {
				label := event["type"]
				if label == "human.collaboration" {
					label = "Feishu message received"
				}
				confirmed = fmt.Sprintf("%s at %s", label, event["at"])
				if status, ok := payload["state"].(string); ok {
					confirmed += " (" + status + ")"
				} else if step, ok := payload["step"].(string); ok {
					confirmed += " (" + step + ")"
				}
			}
		}
		fmt.Fprintf(&summary, "- Last confirmed step: %s\n", confirmed)
		if len(attempts) > 0 {
			latest := attempts[0]
			fmt.Fprintf(&summary, "- Latest run: %s (%s)\n", latest["runId"], latest["status"])
			if step, ok := latest["currentStep"].(*string); ok && step != nil {
				fmt.Fprintf(&summary, "- Latest run step: %s\n", *step)
			}
		}
		if conclusionFailure != "" {
			fmt.Fprintf(&summary, "- Last Agent conclusion error: %s\n", conclusionFailure)
		}
		for _, failure := range []string{contextFailure, stateFailure} {
			if failure != "" {
				fmt.Fprintf(&summary, "- Plane writeback failed with no later confirmation in this trace: %s\n", failure)
			}
		}
	}
	return summary.String()
}
