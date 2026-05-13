package main

import (
	"os"
	"testing"
)

func TestPromptReviewThreadRepliesUsesPromptFixItemIDs(t *testing.T) {
	t.Setenv(envLooperPrompt, "Fix items:\n- {\"type\":\"comment\",\"id\":\"comment-abc\",\"threadId\":\"thread-xyz\"}\n- {\"type\":\"check\",\"id\":\"check-1\"}")
	replies := promptReviewThreadReplies("done", false)
	if len(replies) != 1 {
		t.Fatalf("len(replies) = %d, want 1", len(replies))
	}
	if replies[0]["fixItemId"] != "comment-abc" || replies[0]["threadId"] != "thread-xyz" || replies[0]["explanation"] != "done" {
		t.Fatalf("replies = %#v, want prompt-derived review-thread reply", replies)
	}
}

func TestPromptReviewThreadRepliesFallsBackWhenRequested(t *testing.T) {
	t.Setenv(envLooperPrompt, "")
	replies := promptReviewThreadReplies("done", true)
	if len(replies) != 1 {
		t.Fatalf("len(replies) = %d, want 1", len(replies))
	}
	if replies[0]["fixItemId"] != "comment-1" || replies[0]["threadId"] != "thread-1" {
		t.Fatalf("replies = %#v, want default fallback ids", replies)
	}
	if _, ok := os.LookupEnv(envLooperPrompt); !ok {
		t.Fatal("LOOPER_PROMPT env should remain set by test")
	}
}
