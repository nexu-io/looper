package agent

import (
	"encoding/json"
	"regexp"
	"strings"
)

// claudeToolEntry is one tool_use block from claude-code's `--output-format
// stream-json` assistant events. claude exposes many tools (Bash, Read, Edit,
// Grep, …), so Name holds the tool name and Summary a short human line (a salient
// input arg like a command / file path). Used only for the live progress feed.
type claudeToolEntry struct {
	ID      string
	Name    string
	Summary string
}

// claudeActivity is one rendered line in the live progress feed, keyed so a repeated
// tool_use block replaces it in place instead of appending a duplicate.
type claudeActivity struct {
	key  string
	line string
}

// claudeJSONLTranslator consumes claude-code `--print --output-format stream-json
// --verbose` JSONL events (one per line) and accumulates: the native session id
// (for resume), the final result text + error flag (for looper's completion marker),
// and tool_use calls + assistant text (for the live feed).
//
// claude stamps `session_id` on EVERY event, so the id is available from the very
// first line. Observed event shapes (claude 2.1.207, verified against real
// `claude --print --output-format stream-json --verbose` output):
//
//	{"type":"system","subtype":"init","session_id":…,"model":…}          → init
//	{"type":"assistant","message":{"content":[…]},"session_id":…}        → turn / tool calls
//	{"type":"result","subtype":"success","result":"<final text>",
//	  "is_error":false,"session_id":…,"usage":…}                         → final result
//	{"type":"system","subtype":"hook_started"|"hook_response",…}         → ignored
//
// The assistant message.content is the Anthropic Messages content array — a mix of
// {"type":"text","text":…} and {"type":"tool_use","id":…,"name":…,"input":{…}}
// blocks.
//
// Best-effort by design: a non-JSON, partial, or unknown line is ignored so a
// chatty stream can never break the run.
type claudeJSONLTranslator struct {
	sessionID  string
	resultText string
	isError    bool
	errMessage string
	// textFrags: assistant text blocks in first-seen order (completion + snippets).
	textFrags []string
	// tools + toolIndex: tool_use calls in first-seen order, deduped by block id.
	tools     []claudeToolEntry
	toolIndex map[string]int
	// activity + activityIndex: the tool/text feed for the live progress card, keyed
	// so a repeated block replaces its line in place.
	activity      []claudeActivity
	activityIndex map[string]int
}

func newClaudeJSONLTranslator() *claudeJSONLTranslator {
	return &claudeJSONLTranslator{
		toolIndex:     map[string]int{},
		activityIndex: map[string]int{},
	}
}

// ingestLine parses one JSONL line and folds it into the accumulated state.
func (t *claudeJSONLTranslator) ingestLine(line string) {
	line = strings.TrimSpace(line)
	if line == "" || line[0] != '{' {
		return
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(line), &raw); err != nil {
		return
	}
	// The session id is stamped at the top level of every event; capture the first so
	// a live run can persist it before completion (native resume / takeover need it).
	if t.sessionID == "" {
		if id := jsonlStr(raw, "session_id", "sessionId"); id != "" {
			t.sessionID = id
		}
	}
	switch jsonlStr(raw, "type") {
	case "assistant":
		t.ingestAssistant(jsonlRec(raw["message"]))
	case "result":
		t.ingestResult(raw)
	}
}

// ingestAssistant folds an assistant event's message.content blocks: text blocks
// feed the completion text + live snippets; tool_use blocks feed the tool call feed.
func (t *claudeJSONLTranslator) ingestAssistant(message map[string]any) {
	if message == nil {
		return
	}
	content, ok := message["content"].([]any)
	if !ok {
		return
	}
	for _, block := range content {
		rec := jsonlRec(block)
		if rec == nil {
			continue
		}
		switch jsonlStr(rec, "type") {
		case "text":
			if txt := jsonlStr(rec, "text"); txt != "" {
				t.textFrags = append(t.textFrags, txt)
				t.upsertActivity("", claudeTextSnippet(txt))
			}
		case "tool_use":
			t.ingestToolUse(rec)
		}
	}
}

// ingestToolUse records a tool_use block, deduped by its block id so a re-emitted
// block collapses into one entry.
func (t *claudeJSONLTranslator) ingestToolUse(rec map[string]any) {
	entry := claudeToolEntry{
		ID:      jsonlStr(rec, "id"),
		Name:    jsonlStr(rec, "name"),
		Summary: claudeToolSummary(jsonlRec(rec["input"])),
	}
	if entry.Name == "" && entry.Summary == "" {
		return
	}
	if entry.ID == "" {
		t.tools = append(t.tools, entry)
	} else if idx, ok := t.toolIndex[entry.ID]; ok {
		if entry.Name == "" {
			entry.Name = t.tools[idx].Name
		}
		if strings.TrimSpace(entry.Summary) == "" {
			entry.Summary = t.tools[idx].Summary
		}
		t.tools[idx] = entry
	} else {
		t.toolIndex[entry.ID] = len(t.tools)
		t.tools = append(t.tools, entry)
	}
	t.upsertActivity(activityKey("tool", entry.ID), claudeToolLine(entry))
}

// ingestResult folds the terminal `result` event: its `result` field is the final
// agent message (where looper's completion marker lives), and is_error flags a
// failed turn.
func (t *claudeJSONLTranslator) ingestResult(raw map[string]any) {
	if txt := jsonlStr(raw, "result"); txt != "" {
		t.resultText = txt
	}
	if b, ok := raw["is_error"].(bool); ok {
		t.isError = b
	}
	if t.isError {
		if msg := jsonlStr(raw, "result", "error"); msg != "" {
			t.errMessage = msg
		}
	}
}

// upsertActivity inserts or updates a feed line by key so a repeated block replaces
// its line in place; empty lines and (for keyless entries) plain appends are handled.
func (t *claudeJSONLTranslator) upsertActivity(key, line string) {
	if strings.TrimSpace(line) == "" {
		return
	}
	if key == "" {
		t.activity = append(t.activity, claudeActivity{line: line})
		return
	}
	if idx, ok := t.activityIndex[key]; ok {
		t.activity[idx].line = line
		return
	}
	t.activityIndex[key] = len(t.activity)
	t.activity = append(t.activity, claudeActivity{key: key, line: line})
}

// recentActivityLines renders the last n feed lines (mixed tool calls + text
// snippets) for the live progress card, in event order.
func (t *claudeJSONLTranslator) recentActivityLines(n int) []string {
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

// combinedText returns the human-readable text claude produced, with the final
// result LAST so parseCompletion (which scans bottom-up) sees the agent's final
// message — where the completion marker lives — before any earlier text. stdout is
// JSONL in this mode, so a raw line-prefix scan misses the marker; this
// reconstructs the text to scan.
func (t *claudeJSONLTranslator) combinedText() string {
	parts := make([]string, 0, len(t.textFrags)+1)
	parts = append(parts, t.textFrags...)
	if strings.TrimSpace(t.resultText) != "" {
		parts = append(parts, t.resultText)
	}
	return strings.Join(parts, "\n")
}

// assistantText returns ONLY the agent's own answer — the terminal `result` text
// when present, else the joined assistant text blocks — without marker-hunting.
// Used when a caller needs the reply verbatim (e.g. an LLM-as-a-function call).
func (t *claudeJSONLTranslator) assistantText() string {
	if strings.TrimSpace(t.resultText) != "" {
		return t.resultText
	}
	return strings.Join(t.textFrags, "\n")
}

// ingestAll folds an entire JSONL blob (the full stdout) line by line.
func (t *claudeJSONLTranslator) ingestAll(blob string) {
	for _, line := range strings.Split(blob, "\n") {
		t.ingestLine(line)
	}
}

// claudeSessionIDRe matches the session id claude stamps on every JSONL event, e.g.
// "session_id":"792fc227-62ff-4af9-8b3a-...". The id is a UUID (hex + dashes).
var claudeSessionIDRe = regexp.MustCompile(`"session_id"\s*:\s*"([0-9a-fA-F][0-9a-fA-F-]{7,})"`)

// extractClaudeSessionID scans a claude stream-json stdout blob for the FIRST
// session id, so a live run can persist its native session id BEFORE completion
// (native resume / mid-run takeover need it). Because claude stamps the same id on
// every event, the first match is the run's session. Regex-based rather than a full
// JSON parse so it's cheap to call on each output chunk.
func extractClaudeSessionID(outputs ...string) string {
	for _, output := range outputs {
		if m := claudeSessionIDRe.FindStringSubmatch(output); m != nil {
			return m[1]
		}
	}
	return ""
}

// claudeActivityTail renders the last n feed lines from a claude stream-json blob
// for the live progress card.
func claudeActivityTail(stdout string, n int) []string {
	tr := newClaudeJSONLTranslator()
	tr.ingestAll(stdout)
	return tr.recentActivityLines(n)
}

// claudeToolSummary picks the most human line for a tool_use call from its input: a
// salient arg (a bash command, a file path, a search pattern/query, a url, a
// description), else "" (the tool name alone will be shown).
func claudeToolSummary(input map[string]any) string {
	if input == nil {
		return ""
	}
	return jsonlStr(input, "command", "file_path", "path", "pattern", "query", "url", "description", "prompt")
}

// claudeToolLine renders one tool_use call for the live feed: "🔧 <name> <summary>".
// The summary is one-lined and the whole label rune-capped so a CJK description
// isn't sliced mid-character in the Feishu card. claude's assistant event carries no
// per-tool completion status, so this is a neutral call line (no ✅/❌).
func claudeToolLine(e claudeToolEntry) string {
	label := strings.Join(strings.Fields(e.Summary), " ")
	if label == "" {
		label = e.Name
	}
	if label == "" {
		return ""
	}
	if e.Name != "" && e.Name != label && !strings.HasPrefix(label, e.Name+" ") {
		label = e.Name + " " + label
	}
	return "🔧 " + capRunes(label, 90)
}

// claudeTextSnippet renders a short "💬 …" feed line from an assistant text block:
// the first meaningful line that isn't the completion marker, rune-capped. Returns
// "" when the text is only the marker / punctuation (nothing worth showing).
func claudeTextSnippet(text string) string {
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
