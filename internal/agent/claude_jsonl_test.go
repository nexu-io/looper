package agent

import (
	"strings"
	"testing"
)

// claudeSession is the session id used across the fixtures (a UUID, as claude emits).
const claudeSession = "792fc227-62ff-4af9-8b3a-3d1e9a0b1c2d"

func TestClaudeJSONLTranslator(t *testing.T) {
	tr := newClaudeJSONLTranslator()
	lines := []string{
		`{"type":"system","subtype":"init","session_id":"` + claudeSession + `","model":"claude-opus-4-8"}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Let me look at the repo."}]},"session_id":"` + claudeSession + `"}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"cat README.md"}}]},"session_id":"` + claudeSession + `"}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"# Repo"}]},"session_id":"` + claudeSession + `"}`,
		`not json — should be ignored`,
		`{"type":"system","subtype":"hook_started","session_id":"` + claudeSession + `"}`,
		`{"type":"result","subtype":"success","result":"Added the LICENSE file.\n__LOOPER_RESULT__={\"summary\":\"added LICENSE\"}","is_error":false,"session_id":"` + claudeSession + `","usage":{"input_tokens":10}}`,
	}
	for _, l := range lines {
		tr.ingestLine(l)
	}

	// session id captured from the very first (init) event.
	if tr.sessionID != claudeSession {
		t.Fatalf("sessionID = %q, want %q", tr.sessionID, claudeSession)
	}
	if tr.isError {
		t.Fatalf("isError = true, want false")
	}
	// result text captured (the final agent message, where the marker lives).
	if !strings.Contains(tr.resultText, "__LOOPER_RESULT__=") {
		t.Fatalf("resultText missing marker: %q", tr.resultText)
	}
	// combinedText carries the result LAST so parseCompletion (bottom-up) finds the marker.
	comp := parseCompletion(tr.combinedText(), "")
	if comp.ParseStatus != "parsed" || comp.Summary != "added LICENSE" {
		t.Fatalf("parseCompletion(combinedText) = %+v, want parsed 'added LICENSE'", comp)
	}
	// tool_use block captured for the live feed.
	if len(tr.tools) != 1 || tr.tools[0].Name != "Bash" || tr.tools[0].Summary != "cat README.md" {
		t.Fatalf("tools = %+v, want one Bash 'cat README.md'", tr.tools)
	}
	// assistantText returns the verbatim final reply (no marker hunting mixing).
	if tr.assistantText() != tr.resultText {
		t.Fatalf("assistantText = %q, want the result text", tr.assistantText())
	}
	// live feed has the text snippet + the tool line, in order.
	feed := tr.recentActivityLines(10)
	if len(feed) != 2 {
		t.Fatalf("recentActivityLines = %v, want 2 (text snippet + tool line)", feed)
	}
	if !strings.HasPrefix(feed[0], "💬 ") || !strings.Contains(feed[0], "Let me look at the repo.") {
		t.Fatalf("feed[0] = %q, want a text snippet", feed[0])
	}
	if !strings.HasPrefix(feed[1], "🔧 ") || !strings.Contains(feed[1], "Bash") || !strings.Contains(feed[1], "cat README.md") {
		t.Fatalf("feed[1] = %q, want a Bash tool line", feed[1])
	}
}

func TestClaudeJSONLTranslatorIsError(t *testing.T) {
	tr := newClaudeJSONLTranslator()
	tr.ingestAll(strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"` + claudeSession + `"}`,
		`{"type":"result","subtype":"error_max_turns","result":"hit the turn limit","is_error":true,"session_id":"` + claudeSession + `"}`,
	}, "\n"))

	if tr.sessionID != claudeSession {
		t.Fatalf("sessionID = %q, want %q", tr.sessionID, claudeSession)
	}
	if !tr.isError {
		t.Fatalf("isError = false, want true")
	}
	if tr.errMessage != "hit the turn limit" {
		t.Fatalf("errMessage = %q, want the failure text", tr.errMessage)
	}
}

func TestExtractClaudeSessionID(t *testing.T) {
	blob := strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"` + claudeSession + `","model":"claude-opus-4-8"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"hi"}]},"session_id":"` + claudeSession + `"}`,
	}, "\n")
	if got := extractClaudeSessionID(blob); got != claudeSession {
		t.Fatalf("extractClaudeSessionID = %q, want %q", got, claudeSession)
	}
	// A plain-text (non-json) blob has no session id — regex must not false-match.
	if got := extractClaudeSessionID("just some plain text output, no json here"); got != "" {
		t.Fatalf("extractClaudeSessionID(plain) = %q, want empty", got)
	}
}

func TestResolveClaudeArgsStreamJSONFlag(t *testing.T) {
	base := ExecutorConfig{Vendor: "claude-code"}
	// off by default → no stream-json, base flags intact.
	got := resolveClaudeArgs(base, nil, "do it")
	if containsArg(got, "--output-format") || containsArg(got, "--verbose") {
		t.Fatalf("stream-json flags should be absent by default: %v", got)
	}
	if !containsArg(got, "--print") || !containsArg(got, "--dangerously-skip-permissions") {
		t.Fatalf("base flags missing: %v", got)
	}

	// on → stream-json + verbose added, --print prompt preserved.
	on := base
	on.ClaudeJSONEvents = true
	got = resolveClaudeArgs(on, nil, "do it")
	if !containsArg(got, "--output-format") || !containsArg(got, "stream-json") || !containsArg(got, "--verbose") {
		t.Fatalf("expected --output-format stream-json --verbose: %v", got)
	}
	if !containsArg(got, "--print") || !containsArg(got, "do it") {
		t.Fatalf("--print prompt dropped: %v", got)
	}

	// resume path must ALSO get stream-json so a resumed run's stdout is JSONL too
	// (else claudeJSONMode would parse plain text and lose the completion marker).
	resumeGot := resolveNativeResumeArgs(on, "", nil, "sess-1", "continue")
	if !containsArg(resumeGot, "--resume") || !containsArg(resumeGot, "--output-format") || !containsArg(resumeGot, "--verbose") {
		t.Fatalf("resume args missing stream-json: %v", resumeGot)
	}
	// resume off → plain text resume, unchanged.
	resumeOff := resolveNativeResumeArgs(base, "", nil, "sess-1", "continue")
	if containsArg(resumeOff, "--output-format") {
		t.Fatalf("resume args should stay plain text when flag off: %v", resumeOff)
	}
}
