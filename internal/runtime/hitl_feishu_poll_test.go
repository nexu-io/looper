package runtime

import (
	"context"
	"testing"
)

func TestPollFeishuHITLInboxOnce(t *testing.T) {
	// feishu_threads: root om_root_1 -> loop-a (this looper); om_other -> "" (another looper).
	rootToLoop := map[string]string{"om_root_1": "loop-a"}
	seqToLoop := map[int64]string{71: "loop-seq71"}
	var answers, messages []string
	deps := feishuHITLPollDeps{
		loopByRoot: func(_ contextType, root string) string { return rootToLoop[root] },
		loopBySeq:  func(_ contextType, seq int64) string { return seqToLoop[seq] },
		deliverAnswer: func(_ contextType, loopID, answer string) error {
			answers = append(answers, loopID+"="+answer)
			return nil
		},
		enqueueMessage: func(_ contextType, loopID, text string) error {
			messages = append(messages, loopID+"="+text)
			return nil
		},
	}
	events := []feishuInboxEvent{
		{ID: 10, Kind: "message", RootID: "om_root_1", Text: "用 A,改 resize handle"}, // typed -> enqueue
		{ID: 11, Kind: "message", RootID: "om_other", Text: "not ours"},             // another looper -> skip
		{ID: 12, Kind: "message", RootID: "om_root_1", Text: "   "},                 // empty -> skip
		mustCardAction(15, "71", "redis"),                                           // button -> deliver by seq
	}
	n, maxID := pollFeishuHITLInboxOnce(context.Background(), events, deps)
	if n != 2 {
		t.Fatalf("handled = %d, want 2", n)
	}
	if maxID != 15 {
		t.Fatalf("maxID = %d, want 15", maxID)
	}
	// A typed reply is queued (conversational), a button click is a decision.
	if len(messages) != 1 || messages[0] != "loop-a=用 A,改 resize handle" {
		t.Fatalf("enqueued messages = %v, want the typed reply", messages)
	}
	if len(answers) != 1 || answers[0] != "loop-seq71=redis" {
		t.Fatalf("delivered answers = %v, want the button click", answers)
	}
}

func TestPollFeishuHITLInboxOnceAcksFinishedTaskInsteadOfQueuing(t *testing.T) {
	// om_root_done -> loop-done (a finished task); om_root_live -> loop-live (still running).
	rootToLoop := map[string]string{"om_root_done": "loop-done", "om_root_live": "loop-live"}
	done := map[string]bool{"loop-done": true}
	var enqueued, acked []string
	deps := feishuHITLPollDeps{
		loopByRoot:     func(_ contextType, root string) string { return rootToLoop[root] },
		enqueueMessage: func(_ contextType, loopID, text string) error { enqueued = append(enqueued, loopID); return nil },
		loopDone:       func(_ contextType, loopID string) bool { return done[loopID] },
		notifyClosed:   func(_ contextType, loopID string) error { acked = append(acked, loopID); return nil },
	}
	events := []feishuInboxEvent{
		{ID: 20, Kind: "message", RootID: "om_root_done", Text: "还能再改改吗?"}, // finished -> ack once, don't queue
		{ID: 21, Kind: "message", RootID: "om_root_live", Text: "用 redis"}, // live -> queue as normal
	}
	pollFeishuHITLInboxOnce(context.Background(), events, deps)
	if len(acked) != 1 || acked[0] != "loop-done" {
		t.Fatalf("acked = %v, want [loop-done]", acked)
	}
	if len(enqueued) != 1 || enqueued[0] != "loop-live" {
		t.Fatalf("enqueued = %v, want only the live loop [loop-live]", enqueued)
	}
}

func TestLoopFinishedForFollowup(t *testing.T) {
	for _, s := range []string{"completed", "done", "merged", "DONE"} {
		if !loopFinishedForFollowup(s) {
			t.Fatalf("loopFinishedForFollowup(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"running", "failed", "abandoned", "awaiting_human", ""} {
		if loopFinishedForFollowup(s) {
			t.Fatalf("loopFinishedForFollowup(%q) = true, want false", s)
		}
	}
}

func mustCardAction(id int64, seq, answer string) feishuInboxEvent {
	e := feishuInboxEvent{ID: id, Kind: "card_action"}
	e.Value.LoopSeq = seq
	e.Value.Answer = answer
	return e
}
