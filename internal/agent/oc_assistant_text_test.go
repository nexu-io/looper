package agent

import "testing"

func TestOpenCodeAssistantTextExtractsReply(t *testing.T) {
	jsonl := `{"type":"step_start","sessionID":"ses_abc","part":{}}
{"type":"text","sessionID":"ses_abc","part":{"id":"p1","type":"text","text":"{\"disposition\":\"valid\"}"}}`
	got, ok := OpenCodeAssistantText(jsonl)
	if !ok || got != `{"disposition":"valid"}` {
		t.Fatalf("OpenCodeAssistantText() = %q, %v", got, ok)
	}
	if _, ok := OpenCodeAssistantText(`{"disposition":"valid"}`); ok {
		t.Fatal("plain (non-JSONL) stdout must return ok=false so codex falls through")
	}
}
