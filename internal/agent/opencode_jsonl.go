package agent

import (
	"encoding/json"
	"regexp"
	"strings"
)

// openCodeToolEntry is one tool invocation from opencode's `run --format json`
// stream. opencode exposes many tools (bash, read, edit, grep, …), so Tool holds
// the tool name and Summary a short human description (the tool's title, or a key
// input arg like a bash command / file path); Status tracks its lifecycle and
// Output carries the tool result (used only when reconstructing the completion
// text, never rendered in the live feed).
type openCodeToolEntry struct {
	ID      string // part.callID — stable across a tool call's state updates
	Tool    string
	Summary string
	Status  string // "running" | "done" | "error"
	Output  string
}

// openCodeActivity is one rendered line in the live progress feed, keyed so a
// later event that updates the same tool call / text part replaces it in place
// instead of appending a duplicate.
type openCodeActivity struct {
	key  string
	line string
}

// openCodeJSONLTranslator consumes opencode `run --format json` JSONL events (one
// per line) and accumulates: the native session id (for resume), tool invocations
// and text snippets (for the live feed), and the assistant text (for looper's
// completion marker).
//
// opencode's envelope is uniform — every line is
//
//	{ "type": <event>, "sessionID": "ses_…", "part": { … } }
//
// and the human-relevant payload lives under `part` (`part.type`, `part.text`,
// `part.tool`, `part.state`). Observed event types (opencode 1.17, verified
// against real `run --format json` output):
//
//	step_start  → part.type=="step-start"  (step boundary; no user text)
//	text        → part.type=="text", part.text == assistant text
//	tool_use    → part.type=="tool", part.tool, part.callID, part.state.status
//	step_finish → part.type=="step-finish" (step boundary; token usage)
//
// Unlike codex (which emits a distinct thread.started), opencode stamps the SAME
// session id on EVERY event, so the id is available from the very first line.
//
// Best-effort by design: a non-JSON, partial, or unknown line is ignored so a
// chatty stream can never break the run.
type openCodeJSONLTranslator struct {
	sessionID string
	// tools + toolIndex: tool calls in first-seen order, deduped by callID so a
	// later state update (pending → running → completed) overwrites the same entry.
	tools     []openCodeToolEntry
	toolIndex map[string]int
	// textFrags + textIndex: assistant text parts in first-seen order, deduped by
	// part.id so an incrementally-updated part isn't double-counted (opencode resends
	// the full part text on each update, so the latest value wins).
	textFrags []string
	textIndex map[string]int
	// activity + activityIndex: the mixed tool/text feed for the live progress card,
	// keyed so an updated event replaces its line in place.
	activity      []openCodeActivity
	activityIndex map[string]int
}

func newOpenCodeJSONLTranslator() *openCodeJSONLTranslator {
	return &openCodeJSONLTranslator{
		toolIndex:     map[string]int{},
		textIndex:     map[string]int{},
		activityIndex: map[string]int{},
	}
}

// ingestLine parses one JSONL line and folds it into the accumulated state.
func (t *openCodeJSONLTranslator) ingestLine(line string) {
	line = strings.TrimSpace(line)
	if line == "" || line[0] != '{' {
		return
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(line), &raw); err != nil {
		return
	}
	// The session id is stamped at the top level of every event; capture the first
	// one so a live run can persist it before completion.
	if t.sessionID == "" {
		if id := jsonlStr(raw, "sessionID", "sessionId"); id != "" {
			t.sessionID = id
		}
	}
	part := jsonlRec(raw["part"])
	if part == nil {
		return
	}
	// Belt-and-suspenders: the id is repeated inside `part` too.
	if t.sessionID == "" {
		if id := jsonlStr(part, "sessionID", "sessionId"); id != "" {
			t.sessionID = id
		}
	}
	switch jsonlStr(part, "type") {
	case "text":
		t.ingestText(part)
	case "tool":
		t.ingestTool(part)
	}
}

// ingestText records an assistant text part, deduped by part.id (opencode resends
// the full text as a part streams/updates, so the latest value supersedes).
func (t *openCodeJSONLTranslator) ingestText(part map[string]any) {
	txt, _ := part["text"].(string)
	if strings.TrimSpace(txt) == "" {
		return
	}
	id := jsonlStr(part, "id")
	if id != "" {
		if idx, ok := t.textIndex[id]; ok {
			t.textFrags[idx] = txt
		} else {
			t.textIndex[id] = len(t.textFrags)
			t.textFrags = append(t.textFrags, txt)
		}
	} else {
		t.textFrags = append(t.textFrags, txt)
	}
	// Feed the live card a short snippet (first meaningful, non-marker line).
	t.upsertActivity(activityKey("text", id), openCodeTextSnippet(txt))
}

// ingestTool records a tool invocation, deduped by callID (or part.id) so lifecycle
// updates for the same call collapse into one entry, preserving the tool name and
// summary from the earlier event when a later update drops them.
func (t *openCodeJSONLTranslator) ingestTool(part map[string]any) {
	state := jsonlRec(part["state"])
	entry := openCodeToolEntry{
		ID:      jsonlStr(part, "callID", "id"),
		Tool:    jsonlStr(part, "tool"),
		Status:  openCodeToolStatus(state),
		Summary: openCodeToolSummary(jsonlStr(part, "tool"), state),
		Output:  openCodeToolOutput(state),
	}
	if entry.ID == "" {
		t.tools = append(t.tools, entry)
	} else if idx, ok := t.toolIndex[entry.ID]; ok {
		if entry.Tool == "" {
			entry.Tool = t.tools[idx].Tool
		}
		if strings.TrimSpace(entry.Summary) == "" {
			entry.Summary = t.tools[idx].Summary
		}
		if strings.TrimSpace(entry.Output) == "" {
			entry.Output = t.tools[idx].Output
		}
		t.tools[idx] = entry
	} else {
		t.toolIndex[entry.ID] = len(t.tools)
		t.tools = append(t.tools, entry)
	}
	t.upsertActivity(activityKey("tool", entry.ID), openCodeToolLine(entry))
}

// upsertActivity inserts or updates a feed line by key so an updated event
// replaces its line in place; empty lines are dropped.
func (t *openCodeJSONLTranslator) upsertActivity(key, line string) {
	if strings.TrimSpace(line) == "" {
		return
	}
	if key == "" {
		t.activity = append(t.activity, openCodeActivity{line: line})
		return
	}
	if idx, ok := t.activityIndex[key]; ok {
		t.activity[idx].line = line
		return
	}
	t.activityIndex[key] = len(t.activity)
	t.activity = append(t.activity, openCodeActivity{key: key, line: line})
}

// recentActivityLines renders the last n feed lines (mixed tool calls + text
// snippets) for the live progress card, in event order.
func (t *openCodeJSONLTranslator) recentActivityLines(n int) []string {
	if n <= 0 || len(t.activity) == 0 {
		return nil
	}
	start := len(t.activity) - n
	if start < 0 {
		start = 0
	}
	out := make([]string, 0, len(t.activity)-start)
	for _, a := range t.activity[start:] {
		if strings.TrimSpace(a.line) != "" {
			out = append(out, a.line)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// combinedText returns all human-readable text opencode produced — tool outputs
// plus the assistant text, assistant text last — so the caller can find looper's
// completion marker wherever the agent emitted it (its final message, or echoed by
// a tool). The marker lives inside a `text` event's part.text in json mode, so the
// raw line-prefix parse over stdout misses it; scanning this reconstructed text
// recovers it. Assistant text is appended last so parseCompletion (which scans
// from the end) sees the agent's own final line before any tool echo.
func (t *openCodeJSONLTranslator) combinedText() string {
	parts := make([]string, 0, len(t.tools)+len(t.textFrags))
	for _, e := range t.tools {
		if strings.TrimSpace(e.Output) != "" {
			parts = append(parts, e.Output)
		}
	}
	parts = append(parts, t.textFrags...)
	return strings.Join(parts, "\n")
}

// assistantText returns ONLY the assistant's text parts (in order), without any
// tool output. Used when the caller needs the agent's own answer verbatim (e.g. an
// LLM-as-a-function call whose whole reply is a JSON object), not the marker-hunting
// combinedText which mixes in tool echoes.
func (t *openCodeJSONLTranslator) assistantText() string {
	return strings.Join(t.textFrags, "\n")
}

// ingestAll folds an entire JSONL blob (the full stdout) line by line.
func (t *openCodeJSONLTranslator) ingestAll(blob string) {
	for _, line := range strings.Split(blob, "\n") {
		t.ingestLine(line)
	}
}

// OpenCodeAssistantText extracts the assistant's reply from an opencode `--format
// json` stdout blob. In json mode the reply lives inside `text` events, so the raw
// blob is not itself the answer — an LLM-as-a-function caller (triage classifier,
// etc.) that json.Unmarshals stdout would fail. Returns (text, true) when the blob
// is opencode JSONL, ("", false) otherwise so callers fall back to raw stdout for
// vendors that print their answer plainly (codex).
func OpenCodeAssistantText(stdout string) (string, bool) {
	if !openCodeSessionIDRe.MatchString(stdout) {
		return "", false
	}
	tr := newOpenCodeJSONLTranslator()
	tr.ingestAll(stdout)
	text := strings.TrimSpace(tr.assistantText())
	if text == "" {
		return "", false
	}
	return text, true
}

// openCodeSessionIDRe matches the session id opencode stamps on every JSONL event,
// e.g. "sessionID":"ses_0ba646c0cffeG6qqXj177xcRqC". The id is mixed-case
// alphanumeric with a "ses_" prefix.
var openCodeSessionIDRe = regexp.MustCompile(`"sessionID"\s*:\s*"(ses_[A-Za-z0-9]+)"`)

// extractOpenCodeSessionID scans an opencode `--format json` stdout blob for the
// FIRST session id, so a live run can persist its native session id BEFORE
// completion (native resume / mid-run takeover need it). Because opencode stamps
// the same id on every event, the first match is the run's session. Returns ""
// when no id is present (e.g. before the first event, or a non-json run). Regex-
// based rather than a full JSON parse so it's cheap to call on each output chunk.
func extractOpenCodeSessionID(outputs ...string) string {
	for _, output := range outputs {
		if m := openCodeSessionIDRe.FindStringSubmatch(output); m != nil {
			return m[1]
		}
	}
	return ""
}

// openCodeActivityTail renders the last n feed lines from an opencode JSONL blob
// for the live progress card.
func openCodeActivityTail(stdout string, n int) []string {
	tr := newOpenCodeJSONLTranslator()
	tr.ingestAll(stdout)
	return tr.recentActivityLines(n)
}

// openCodeToolStatus maps opencode's part.state.status to the feed's lifecycle. A
// completed tool with a non-zero shell exit is surfaced as an error (mirrors codex,
// where a non-zero exit_code flips the icon to ❌).
func openCodeToolStatus(state map[string]any) string {
	switch jsonlStr(state, "status") {
	case "completed":
		if meta := jsonlRec(state["metadata"]); meta != nil {
			if exit, ok := jsonlNum(meta, "exit"); ok && exit != 0 {
				return "error"
			}
		}
		return "done"
	case "error":
		return "error"
	default:
		return "running"
	}
}

// openCodeToolSummary picks the most human line for a tool call: the tool's title
// if present, else a salient input arg (a bash command, a file path, a search
// pattern/query, a url), else the bare tool name.
func openCodeToolSummary(tool string, state map[string]any) string {
	if title := jsonlStr(state, "title"); title != "" {
		return title
	}
	if input := jsonlRec(state["input"]); input != nil {
		if v := jsonlStr(input, "command", "filePath", "file_path", "path", "pattern", "query", "url", "description"); v != "" {
			return v
		}
	}
	return tool
}

// openCodeToolOutput pulls the tool result text (used only for completion-marker
// reconstruction, never for the live feed) from state.output or state.metadata.output.
func openCodeToolOutput(state map[string]any) string {
	if out := jsonlStr(state, "output"); out != "" {
		return out
	}
	if meta := jsonlRec(state["metadata"]); meta != nil {
		if out := jsonlStr(meta, "output"); out != "" {
			return out
		}
	}
	return ""
}

// openCodeToolLine renders one tool call for the live feed, bridge-style:
// "✅ <tool> <summary>" (done) / "❌ …" (error) / "⏳ …" (running). The summary is
// one-lined and the whole label rune-capped (rune- not byte-capped so a CJK tool
// title / description isn't sliced mid-character in the Feishu card).
func openCodeToolLine(e openCodeToolEntry) string {
	label := strings.Join(strings.Fields(e.Summary), " ")
	if label == "" {
		label = e.Tool
	}
	if label == "" {
		return ""
	}
	if e.Tool != "" && e.Tool != label && !strings.HasPrefix(label, e.Tool+" ") {
		label = e.Tool + " " + label
	}
	label = capRunes(label, 90)
	icon := "⏳"
	switch e.Status {
	case "done":
		icon = "✅"
	case "error":
		icon = "❌"
	}
	return icon + " " + label
}

// openCodeTextSnippet renders a short "💬 …" feed line from an assistant text part:
// the first meaningful line that isn't the completion marker, rune-capped. Returns
// "" when the text is only the marker / punctuation (nothing worth showing).
func openCodeTextSnippet(text string) string {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, CompletionMarkerPrefix) {
			continue
		}
		if !meaningfulProgressLine(line) {
			continue
		}
		return "💬 " + capRunes(line, 90)
	}
	return ""
}

// capRunes truncates s to at most n runes, appending an ellipsis when it cut, so a
// multibyte label isn't sliced mid-character.
func capRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}

// activityKey namespaces a feed entry by kind+id so a tool call and a text part
// that happen to share an id can't collide.
func activityKey(kind, id string) string {
	if id == "" {
		return ""
	}
	return kind + ":" + id
}
