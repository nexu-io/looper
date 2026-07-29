package agent

import (
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/config"
)

// realistic opencode `run --format json` lines (captured from opencode 1.17). Every
// event carries a top-level "sessionID"; the assistant text (and looper's
// completion marker) lives inside a `text` event's part.text.
const (
	ocStepStart = `{"type":"step_start","timestamp":1783579255472,"sessionID":"ses_0ba646c0cffeG6qqXj177xcRqC","part":{"id":"prt_f459ba2a","messageID":"msg_f459b97e","sessionID":"ses_0ba646c0cffeG6qqXj177xcRqC","type":"step-start"}}`
	ocToolBash  = `{"type":"tool_use","timestamp":1783579281874,"sessionID":"ses_0ba646c0cffeG6qqXj177xcRqC","part":{"type":"tool","tool":"bash","callID":"call_Wmf","state":{"status":"completed","input":{"command":"echo hi"},"output":"hi\n","title":"Prints hi","metadata":{"exit":0,"output":"hi\n"}},"id":"prt_bash","sessionID":"ses_0ba646c0cffeG6qqXj177xcRqC","messageID":"msg_f459bf5b"}}`
	ocText      = `{"type":"text","timestamp":1783579255837,"sessionID":"ses_0ba646c0cffeG6qqXj177xcRqC","part":{"id":"prt_f459ba38","messageID":"msg_f459b97e","sessionID":"ses_0ba646c0cffeG6qqXj177xcRqC","type":"text","text":"Added the license.\n__LOOPER_RESULT__={\"summary\":\"x\"}"}}`
	ocStepDone  = `{"type":"step_finish","timestamp":1783579256382,"sessionID":"ses_0ba646c0cffeG6qqXj177xcRqC","part":{"id":"prt_fin","type":"step-finish","tokens":{"total":7322}}}`
)

func TestExtractOpenCodeSessionID(t *testing.T) {
	if got := extractOpenCodeSessionID(ocStepStart); got != "ses_0ba646c0cffeG6qqXj177xcRqC" {
		t.Fatalf("extractOpenCodeSessionID(real line) = %q; want the ses_ id", got)
	}
	// No sessionID present → empty (never a spurious match).
	if got := extractOpenCodeSessionID(`{"type":"text","part":{"type":"text","text":"hello"}}`); got != "" {
		t.Fatalf("extractOpenCodeSessionID(no id) = %q; want empty", got)
	}
	if got := extractOpenCodeSessionID("not json at all\n"); got != "" {
		t.Fatalf("extractOpenCodeSessionID(garbage) = %q; want empty", got)
	}
	// Scans across the args in order; picks the first blob that has an id.
	if got := extractOpenCodeSessionID("", ocText); got != "ses_0ba646c0cffeG6qqXj177xcRqC" {
		t.Fatalf("extractOpenCodeSessionID(second arg) = %q; want the ses_ id", got)
	}
}

// TestOpenCodeJSONLCompletionExtraction proves the core token-cost fix: from a real
// step_start + text (containing the completion marker) stream, the translator
// recovers BOTH the native session id AND the completion summary — the two things
// looper needs for native resume + a parsed result.
func TestOpenCodeJSONLCompletionExtraction(t *testing.T) {
	tr := newOpenCodeJSONLTranslator()
	tr.ingestAll(strings.Join([]string{ocStepStart, ocToolBash, ocText, ocStepDone}, "\n"))

	if tr.sessionID != "ses_0ba646c0cffeG6qqXj177xcRqC" {
		t.Fatalf("sessionID = %q; want the ses_ id", tr.sessionID)
	}
	completion := parseCompletion(tr.combinedText(), "")
	if completion.ParseStatus != "parsed" {
		t.Fatalf("ParseStatus = %q; want parsed (combinedText = %q)", completion.ParseStatus, tr.combinedText())
	}
	if completion.Summary != "x" {
		t.Fatalf("Summary = %q; want x", completion.Summary)
	}
}

func TestOpenCodeJSONLIgnoresGarbageAndDedupesText(t *testing.T) {
	tr := newOpenCodeJSONLTranslator()
	// A malformed line, an unknown event, and the SAME text part streamed twice
	// (opencode resends the full part text on update) must not break parsing or
	// duplicate the text.
	updated := strings.Replace(ocText, "Added the license.", "Added the LICENSE file.", 1)
	tr.ingestAll(strings.Join([]string{
		"}{ not json",
		`{"type":"unknown.event","sessionID":"ses_0ba646c0cffeG6qqXj177xcRqC"}`,
		ocText,
		updated,
	}, "\n"))

	if tr.sessionID != "ses_0ba646c0cffeG6qqXj177xcRqC" {
		t.Fatalf("sessionID = %q; want the ses_ id even with garbage lines", tr.sessionID)
	}
	if len(tr.textFrags) != 1 {
		t.Fatalf("textFrags = %v; want a single deduped fragment", tr.textFrags)
	}
	if !strings.Contains(tr.textFrags[0], "Added the LICENSE file.") {
		t.Fatalf("textFrags[0] = %q; want the latest text to win", tr.textFrags[0])
	}
}

func TestOpenCodeActivityTail(t *testing.T) {
	tail := openCodeActivityTail(strings.Join([]string{ocStepStart, ocToolBash, ocText}, "\n"), 5)
	if len(tail) != 2 {
		t.Fatalf("tail = %v; want a tool line + a text snippet", tail)
	}
	if tail[0] != "✅ bash Prints hi" {
		t.Fatalf("tool line = %q; want ✅ bash Prints hi", tail[0])
	}
	// The text snippet shows the first meaningful line, never the completion marker.
	if tail[1] != "💬 Added the license." {
		t.Fatalf("text line = %q; want 💬 Added the license.", tail[1])
	}
}

func TestOpenCodeToolStatusNonZeroExitIsError(t *testing.T) {
	// A tool that "completed" with a non-zero shell exit surfaces as an error.
	failing := `{"type":"tool_use","sessionID":"ses_x","part":{"type":"tool","tool":"bash","callID":"c1","state":{"status":"completed","input":{"command":"false"},"metadata":{"exit":1}}}}`
	tr := newOpenCodeJSONLTranslator()
	tr.ingestLine(failing)
	if len(tr.tools) != 1 || tr.tools[0].Status != "error" {
		t.Fatalf("tools = %+v; want a single error-status tool", tr.tools)
	}
	lines := tr.recentActivityLines(5)
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "❌ ") {
		t.Fatalf("activity = %v; want a ❌ line", lines)
	}
}

func TestResolveOpenCodeArgsAlwaysAddsFormatJSON(t *testing.T) {
	model := "gpt-5"
	// Fresh start.
	_, args := ResolveSpawn(ExecutorConfig{Vendor: config.AgentVendorOpenCode, Model: &model}, "/tmp/wt", "hi")
	if !hasAnyFlag(args, []string{"--format"}) || !containsArg(args, "json") {
		t.Fatalf("fresh opencode args missing --format json: %v", args)
	}
	if args[len(args)-1] != "hi" {
		t.Fatalf("prompt must stay last: %v", args)
	}
	// Native resume.
	_, resumeArgs := ResolveSpawnWithNativeResume(ExecutorConfig{Vendor: config.AgentVendorOpenCode, Model: &model}, "/tmp/wt", "hi", "ses_123", true)
	if !hasAnyFlag(resumeArgs, []string{"--format"}) || !containsArg(resumeArgs, "--session") {
		t.Fatalf("resume opencode args missing --format json or --session: %v", resumeArgs)
	}
	// A caller-supplied --format is not duplicated.
	_, override := ResolveSpawn(ExecutorConfig{Vendor: config.AgentVendorOpenCode, Model: &model, Params: map[string]any{"args": []any{"run", "--format", "json"}}}, "/tmp/wt", "hi")
	count := 0
	for _, a := range override {
		if a == "--format" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("--format duplicated: %v", override)
	}
}
