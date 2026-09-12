package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPromptReviewThreadRepliesUsesPromptFixItemIDs(t *testing.T) {
	items := parsePromptFixItems("Fix items:\n- {\"type\":\"comment\",\"id\":\"comment-abc\",\"threadId\":\"thread-xyz\"}\n- {\"type\":\"check\",\"id\":\"check-1\"}")
	replies := buildReviewThreadReplies(items, "done", false, nil)
	if len(replies) != 1 {
		t.Fatalf("len(replies) = %d, want 1", len(replies))
	}
	if replies[0]["fixItemId"] != "comment-abc" || replies[0]["threadId"] != "thread-xyz" || replies[0]["explanation"] != "done" {
		t.Fatalf("replies = %#v, want prompt-derived review-thread reply", replies)
	}
}

func TestBotFixerObservesThreadThroughHostCapability(t *testing.T) {
	cli := filepath.Join(t.TempDir(), "looper")
	script := "#!/bin/sh\n[ \"$*\" = 'host thread 42 thread-xyz' ] || exit 9\nprintf '%s' '{\"id\":\"thread-xyz\",\"comments\":[{\"id\":\"comment-abc\",\"updatedAt\":\"2026-09-12T00:00:00Z\"}]}'\n"
	if err := os.WriteFile(cli, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOOPER_HOST_CLI", cli)
	t.Setenv(envFakeAgentGHPath, "/must-not-call-personal-gh")
	t.Setenv(envLooperPrompt, "PR seed:\n{\"pr_number\":42}\n")
	hashes := fetchObservedThreadHashes([]promptFixItem{{Type: "comment", ID: "comment-abc", ThreadID: "thread-xyz"}})
	want := hashObservedThreadComments([]ghThreadComment{{ID: "comment-abc", UpdatedAt: "2026-09-12T00:00:00Z"}})
	if hashes["thread-xyz"] != want {
		t.Fatalf("observed fingerprint = %q, want %q", hashes["thread-xyz"], want)
	}
}

func TestPromptReviewThreadRepliesIncludesObservedHashWhenRequested(t *testing.T) {
	items := parsePromptFixItems("Fix items:\n- {\"type\":\"comment\",\"id\":\"comment-abc\",\"threadId\":\"thread-xyz\"}\n- {\"type\":\"comment\",\"id\":\"comment-def\",\"threadId\":\"thread-xyz\"}")
	replies := buildReviewThreadReplies(items[:1], "done", false, map[string]string{"thread-xyz": "thread-hash"})
	if len(replies) != 1 {
		t.Fatalf("len(replies) = %d, want 1", len(replies))
	}
	if got, want := replies[0]["threadCommentsObserved"], "thread-hash"; got != want {
		t.Fatalf("threadCommentsObserved = %#v, want %q", got, want)
	}
}

func TestPromptReviewThreadRepliesFallsBackWhenRequested(t *testing.T) {
	t.Setenv(envLooperPrompt, "")
	replies := promptReviewThreadReplies("done", true, false)
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

func TestPromptReviewThreadRepliesPreservesForgejoReviewerSummaryIDs(t *testing.T) {
	t.Setenv(envLooperPrompt, "Fix items:\n- {\"type\":\"comment\",\"id\":\"R-001\",\"threadId\":\"R-001\"}")
	replies := promptReviewThreadReplies("done", false, false)
	if len(replies) != 1 {
		t.Fatalf("len(replies) = %d, want 1", len(replies))
	}
	if replies[0]["fixItemId"] != "R-001" || replies[0]["threadId"] != "R-001" {
		t.Fatalf("replies = %#v, want forgejo reviewer summary ids preserved", replies)
	}
}
