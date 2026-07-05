package loops

import "testing"

func strptr(s string) *string { return &s }

func TestHITLAskRoundTripPreservesOtherMetadata(t *testing.T) {
	base := strptr(`{"issueTitle":"Fix login","loop":{"status":"running"}}`)

	updated, err := WriteHITLAsk(base, HITLAsk{
		Question:  "Which direction?",
		Options:   []string{"continue", "redirect"},
		SessionID: "sess-1",
		Vendor:    "codex",
		Status:    "awaiting",
		AskedAt:   "2026-04-11T12:00:00.000Z",
	})
	if err != nil {
		t.Fatalf("WriteHITLAsk() error = %v", err)
	}

	ask, ok := ReadHITLAsk(&updated)
	if !ok {
		t.Fatal("ReadHITLAsk() ok = false, want true")
	}
	if ask.Question != "Which direction?" || len(ask.Options) != 2 || ask.SessionID != "sess-1" || ask.Vendor != "codex" || ask.Status != "awaiting" {
		t.Fatalf("ask round-trip mismatch: %#v", ask)
	}

	// Other metadata keys must be preserved.
	meta := parseMetadataObject(&updated)
	if meta["issueTitle"] != "Fix login" {
		t.Fatalf("issueTitle not preserved: %#v", meta["issueTitle"])
	}
	if _, ok := meta["loop"]; !ok {
		t.Fatal("loop metadata not preserved")
	}
}

func TestReadHITLAskAbsent(t *testing.T) {
	if _, ok := ReadHITLAsk(strptr(`{"issueTitle":"x"}`)); ok {
		t.Fatal("ReadHITLAsk() ok = true for metadata without hitl, want false")
	}
	if _, ok := ReadHITLAsk(nil); ok {
		t.Fatal("ReadHITLAsk(nil) ok = true, want false")
	}
}

func TestClearHITLAskRemovesOnlyHITL(t *testing.T) {
	updated, err := WriteHITLAsk(strptr(`{"issueTitle":"x"}`), HITLAsk{Question: "q", Answer: "a", Status: "answered"})
	if err != nil {
		t.Fatalf("WriteHITLAsk() error = %v", err)
	}
	cleared, err := ClearHITLAsk(&updated)
	if err != nil {
		t.Fatalf("ClearHITLAsk() error = %v", err)
	}
	if _, ok := ReadHITLAsk(&cleared); ok {
		t.Fatal("hitl still present after ClearHITLAsk")
	}
	if parseMetadataObject(&cleared)["issueTitle"] != "x" {
		t.Fatal("issueTitle removed by ClearHITLAsk")
	}
}
