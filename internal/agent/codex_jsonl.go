package agent

import (
	"encoding/json"
	"strings"
)

// codexToolEntry is one command_execution tool call from codex's `--json` stream.
// In `codex exec` the only tool is command execution, so Command carries the bash
// line and Status tracks its lifecycle.
type codexToolEntry struct {
	ID      string
	Command string
	Status  string // "running" | "done" | "error"
	Output  string
}

// codexJSONLTranslator consumes codex `--json` JSONL events (one per line) and
// accumulates: the command executions (for a live tool-call feed), the native
// thread/session id (for resume), the final agent message (the result), and
// terminal state. The event shapes are ported from lark-coding-agent-bridge's
// codex translator, which is validated against real codex output:
//
//	thread.started   → { thread_id }
//	item.started     → { item: { type:"command_execution", id, command } }
//	item.completed   → { item: { type:"command_execution", id, output, exit_code } }
//	                   { item: { type:"agent_message", text } }
//	agent_message    → { message | text }
//	turn.completed   → terminal (+ usage)
//	turn.failed|error→ error
//
// Best-effort by design: a non-JSON, partial, or unknown line is ignored so a
// chatty stream can never break the run.
type codexJSONLTranslator struct {
	threadID   string
	tools      []codexToolEntry
	toolIndex  map[string]int
	finalText  string
	terminal   bool
	errMessage string
}

func newCodexJSONLTranslator() *codexJSONLTranslator {
	return &codexJSONLTranslator{toolIndex: map[string]int{}}
}

// ingestLine parses one JSONL line and folds it into the accumulated state.
func (t *codexJSONLTranslator) ingestLine(line string) {
	line = strings.TrimSpace(line)
	if line == "" || line[0] != '{' {
		return
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(line), &raw); err != nil {
		return
	}
	typ, _ := raw["type"].(string)
	switch typ {
	case "thread.started":
		if id := jsonlStr(raw, "thread_id", "threadId"); id != "" {
			t.threadID = id
		}
	case "item.started":
		item := jsonlRec(raw["item"])
		if item != nil && jsonlStr(item, "type") == "command_execution" {
			if id := jsonlStr(item, "id"); id != "" {
				t.upsertTool(codexToolEntry{ID: id, Command: jsonlStr(item, "command"), Status: "running"})
			}
		}
	case "item.completed":
		item := jsonlRec(raw["item"])
		if item == nil {
			return
		}
		switch jsonlStr(item, "type") {
		case "command_execution":
			status := "done"
			if exit, ok := jsonlNum(item, "exit_code"); ok && exit != 0 {
				status = "error"
			}
			t.upsertTool(codexToolEntry{
				ID:      jsonlStr(item, "id"),
				Command: jsonlStr(item, "command"),
				Status:  status,
				Output:  jsonlStr(item, "output", "aggregated_output", "stdout"),
			})
		case "agent_message":
			if txt := jsonlStr(item, "text", "message"); txt != "" {
				t.finalText = txt
			}
		}
	case "agent_message":
		if txt := jsonlStr(raw, "message", "text"); txt != "" {
			t.finalText = txt
		}
	case "turn.completed":
		t.terminal = true
	case "turn.failed":
		t.terminal = true
		t.errMessage = jsonlErr(raw)
	case "error":
		t.errMessage = jsonlErr(raw)
	}
}

// upsertTool inserts or updates a tool entry by id, preserving the command when a
// later event (the completion) omits it.
func (t *codexJSONLTranslator) upsertTool(e codexToolEntry) {
	if e.ID == "" {
		t.tools = append(t.tools, e)
		return
	}
	if idx, ok := t.toolIndex[e.ID]; ok {
		if strings.TrimSpace(e.Command) == "" {
			e.Command = t.tools[idx].Command
		}
		t.tools[idx] = e
		return
	}
	t.toolIndex[e.ID] = len(t.tools)
	t.tools = append(t.tools, e)
}

// recentToolLines renders the last n command executions bridge-style, one per
// line: "✅ <command>" (done) / "❌ <command>" (non-zero exit) / "⏳ <command>"
// (still running). Commands are one-lined and length-capped.
func (t *codexJSONLTranslator) recentToolLines(n int) []string {
	if n <= 0 || len(t.tools) == 0 {
		return nil
	}
	start := len(t.tools) - n
	if start < 0 {
		start = 0
	}
	out := make([]string, 0, len(t.tools)-start)
	for _, e := range t.tools[start:] {
		cmd := strings.Join(strings.Fields(cleanShellWrapper(e.Command)), " ")
		if cmd == "" {
			continue
		}
		if len(cmd) > 90 {
			cmd = cmd[:90] + "…"
		}
		icon := "⏳"
		switch e.Status {
		case "done":
			icon = "✅"
		case "error":
			icon = "❌"
		}
		out = append(out, icon+" "+cmd)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// combinedText returns all human-readable text codex produced — command outputs
// plus the final agent message — so the caller can find looper's completion
// marker wherever the agent chose to emit it (in its message, or echoed by a
// command). This is the text the text-mode result parser would have seen.
func (t *codexJSONLTranslator) combinedText() string {
	parts := make([]string, 0, len(t.tools)+1)
	for _, e := range t.tools {
		if strings.TrimSpace(e.Output) != "" {
			parts = append(parts, e.Output)
		}
	}
	if strings.TrimSpace(t.finalText) != "" {
		parts = append(parts, t.finalText)
	}
	return strings.Join(parts, "\n")
}

// ingestAll folds an entire JSONL blob (the full stdout) line by line.
func (t *codexJSONLTranslator) ingestAll(blob string) {
	for _, line := range strings.Split(blob, "\n") {
		t.ingestLine(line)
	}
}

// extractCodexThreadID scans a codex --json stdout blob for the session id from
// the thread.started event, so a live run can persist its session id BEFORE
// completion (needed for mid-run human takeover). Returns the first id found and
// stops early — thread.started is the stream's first event — so callers can gate
// on an empty session id and avoid rescanning once it's known.
func extractCodexThreadID(stdout string) string {
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] != '{' || !strings.Contains(line, "thread") {
			continue
		}
		var raw map[string]any
		if json.Unmarshal([]byte(line), &raw) != nil {
			continue
		}
		if typ, _ := raw["type"].(string); typ == "thread.started" {
			if id := jsonlStr(raw, "thread_id", "threadId"); id != "" {
				return id
			}
		}
	}
	return ""
}

// cleanShellWrapper unwraps a leading shell invocation ("/bin/zsh -lc '<cmd>'",
// "bash -c \"<cmd>\"") to the inner command, so the tool feed reads as the actual
// command rather than the shell plumbing around it.
func cleanShellWrapper(cmd string) string {
	cmd = strings.TrimSpace(cmd)
	for _, marker := range []string{" -lc '", " -c '", " -lc \"", " -c \""} {
		idx := strings.Index(cmd, marker)
		if idx < 0 || idx > 24 {
			continue
		}
		inner := strings.TrimSpace(cmd[idx+len(marker):])
		inner = strings.TrimRight(inner, "'\"")
		if inner != "" {
			return inner
		}
	}
	return cmd
}

// --- small typed accessors over decoded JSON ---

func jsonlRec(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return nil
}

func jsonlStr(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

func jsonlNum(m map[string]any, key string) (float64, bool) {
	if n, ok := m[key].(float64); ok {
		return n, true
	}
	return 0, false
}

func jsonlErr(raw map[string]any) string {
	if s := jsonlStr(raw, "message"); s != "" {
		return s
	}
	if nested := jsonlRec(raw["error"]); nested != nil {
		if s := jsonlStr(nested, "message"); s != "" {
			return s
		}
	}
	if s := jsonlStr(raw, "error"); s != "" {
		return s
	}
	return ""
}
