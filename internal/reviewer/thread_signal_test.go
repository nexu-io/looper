package reviewer

import (
	"strings"
	"testing"
)

func looperThread(id string, resolved bool, comments ...ReviewThreadComment) ReviewThread {
	return ReviewThread{ID: id, IsResolved: resolved, Comments: comments}
}

func looperRoot(id, body string) ReviewThreadComment {
	if !strings.Contains(body, "looper:") {
		body = body + " <!-- looper:stamp v=1 -->"
	}
	return ReviewThreadComment{ID: id, Author: "looper-bot", Body: body, CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z", CommitOID: "old-head"}
}

func TestThreadFeedbackFingerprintEditReplyDeleteResolveReopen(t *testing.T) {
	t.Parallel()
	base := looperThread("t1", false,
		looperRoot("c1", "Please fix nil check"),
	)
	baseFP := ThreadFeedbackFingerprint([]ReviewThread{base})

	// Edit body → fingerprint changes.
	edited := looperThread("t1", false,
		ReviewThreadComment{ID: "c1", Author: "looper-bot", Body: "Please fix nil check carefully <!-- looper:stamp v=1 -->", CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T01:00:00Z", CommitOID: "old-head"},
	)
	if ThreadFeedbackFingerprint([]ReviewThread{edited}) == baseFP {
		t.Fatal("edit should change fingerprint")
	}

	// Reply → fingerprint changes.
	replied := looperThread("t1", false,
		looperRoot("c1", "Please fix nil check"),
		ReviewThreadComment{ID: "c2", Author: "alice", AuthorAssociation: "MEMBER", Body: "/looper wontfix out of scope", CreatedAt: "2026-01-01T02:00:00Z", UpdatedAt: "2026-01-01T02:00:00Z"},
	)
	replyFP := ThreadFeedbackFingerprint([]ReviewThread{replied})
	if replyFP == baseFP {
		t.Fatal("reply should change fingerprint")
	}

	// Delete reply (back to base comments) → fingerprint returns to base.
	if ThreadFeedbackFingerprint([]ReviewThread{base}) != baseFP {
		t.Fatal("delete reply should restore base fingerprint")
	}

	// Resolve → fingerprint changes (isResolved included).
	resolved := looperThread("t1", true, looperRoot("c1", "Please fix nil check"))
	if ThreadFeedbackFingerprint([]ReviewThread{resolved}) == baseFP {
		t.Fatal("resolve should change fingerprint")
	}

	// Reopen → back to base.
	reopened := looperThread("t1", false, looperRoot("c1", "Please fix nil check"))
	if ThreadFeedbackFingerprint([]ReviewThread{reopened}) != baseFP {
		t.Fatal("reopen should restore base fingerprint")
	}
}

func TestThreadFeedbackFingerprintExcludesAuditReplies(t *testing.T) {
	t.Parallel()
	withoutAudit := looperThread("t1", false,
		looperRoot("c1", "Please fix"),
		ReviewThreadComment{ID: "c2", Author: "alice", AuthorAssociation: "OWNER", Body: "/looper wontfix nope", CreatedAt: "t2", UpdatedAt: "t2"},
	)
	withAudit := looperThread("t1", false,
		looperRoot("c1", "Please fix"),
		ReviewThreadComment{ID: "c2", Author: "alice", AuthorAssociation: "OWNER", Body: "/looper wontfix nope", CreatedAt: "t2", UpdatedAt: "t2"},
		ReviewThreadComment{ID: "c3", Author: "looper-bot", Body: "accepted <!-- looper:thread-resolution thread=t1 head=abc feedback=fp decision=accept_wontfix -->", CreatedAt: "t3", UpdatedAt: "t3"},
	)
	if ThreadFeedbackFingerprint([]ReviewThread{withoutAudit}) != ThreadFeedbackFingerprint([]ReviewThread{withAudit}) {
		t.Fatal("audit replies must be excluded from fingerprint")
	}
}

func TestThreadFeedbackFingerprintKeepsQuotedForeignAuditMarkers(t *testing.T) {
	t.Parallel()
	withoutQuote := looperThread("t1", false,
		looperRoot("c1", "Please fix"),
		ReviewThreadComment{ID: "c2", Author: "alice", AuthorAssociation: "OWNER", Body: "/looper wontfix nope", CreatedAt: "t2", UpdatedAt: "t2"},
	)
	withQuote := looperThread("t1", false,
		looperRoot("c1", "Please fix"),
		ReviewThreadComment{ID: "c2", Author: "alice", AuthorAssociation: "OWNER", Body: "/looper wontfix nope", CreatedAt: "t2", UpdatedAt: "t2"},
		ReviewThreadComment{ID: "c3", Author: "looper-bot", Body: "quoting <!-- looper:thread-resolution thread=other head=abc decision=accept_wontfix -->", CreatedAt: "t3", UpdatedAt: "t3"},
	)
	if ThreadFeedbackFingerprint([]ReviewThread{withoutQuote}) == ThreadFeedbackFingerprint([]ReviewThread{withQuote}) {
		t.Fatal("quoted foreign-thread audit markers must remain in fingerprint")
	}
}

func TestThreadFeedbackFingerprintStableOrder(t *testing.T) {
	t.Parallel()
	a := []ReviewThread{
		looperThread("t2", false, looperRoot("c2", "second")),
		looperThread("t1", false, looperRoot("c1", "first")),
	}
	b := []ReviewThread{
		looperThread("t1", false, looperRoot("c1", "first")),
		looperThread("t2", false, looperRoot("c2", "second")),
	}
	if ThreadFeedbackFingerprint(a) != ThreadFeedbackFingerprint(b) {
		t.Fatal("thread order must be canonicalized")
	}
}

func TestReviewSignalFingerprintIncludesHead(t *testing.T) {
	t.Parallel()
	threads := []ReviewThread{looperThread("t1", false, looperRoot("c1", "x"))}
	fb := ThreadFeedbackFingerprint(threads)
	if ReviewSignalFingerprint("head-a", fb) == ReviewSignalFingerprint("head-b", fb) {
		t.Fatal("head must affect review signal")
	}
	if ComputeReviewSignalFingerprint("head-a", threads) != ReviewSignalFingerprint("head-a", fb) {
		t.Fatal("ComputeReviewSignalFingerprint mismatch")
	}
}

func TestThreadResolutionMarkerExtended(t *testing.T) {
	t.Parallel()
	got := threadResolutionMarker("t1", "abc", "fb123", "accept_wontfix")
	if !strings.Contains(got, "feedback=fb123") || !strings.Contains(got, "decision=accept_wontfix") {
		t.Fatalf("marker = %q", got)
	}
	legacy := threadResolutionMarker("t1", "abc", "", "objectively_fixed")
	if strings.Contains(legacy, "feedback=") {
		t.Fatalf("legacy marker should omit feedback: %q", legacy)
	}
}

func TestHasThreadResolutionAuditForSignalIgnoresLegacyMarkers(t *testing.T) {
	t.Parallel()
	thread := looperThread("t1", false,
		looperRoot("c1", "Please fix"),
		ReviewThreadComment{ID: "c2", Author: "looper-bot", Body: "<!-- looper:thread-resolution thread=t1 head=abc decision=accept_wontfix -->"},
	)
	if hasThreadResolutionAuditForSignal(thread, "t1", "abc", "fb-new", "accept_wontfix") {
		t.Fatal("legacy marker without feedback must not suppress new signal")
	}
	thread.Comments = append(thread.Comments, ReviewThreadComment{
		ID: "c3", Author: "looper-bot",
		Body: "<!-- looper:thread-resolution thread=t1 head=abc feedback=fb-new decision=accept_wontfix -->",
	})
	if !hasThreadResolutionAuditForSignal(thread, "t1", "abc", "fb-new", "accept_wontfix") {
		t.Fatal("extended marker should match")
	}
}

func TestResumeDispositionDecisionFromRemoteAudit(t *testing.T) {
	t.Parallel()
	base := looperThread("t1", false,
		looperRoot("c1", "Please fix"),
		ReviewThreadComment{ID: "c2", Author: "alice", AuthorAssociation: "OWNER", Body: "/looper wontfix x", CreatedAt: "t2", UpdatedAt: "t2"},
	)
	fp := ThreadFeedbackFingerprintForLogin([]ReviewThread{base}, "looper-bot")
	if fp == "" {
		t.Fatal("expected feedback fingerprint")
	}
	withAudit := looperThread("t1", false,
		looperRoot("c1", "Please fix"),
		ReviewThreadComment{ID: "c2", Author: "alice", AuthorAssociation: "OWNER", Body: "/looper wontfix x", CreatedAt: "t2", UpdatedAt: "t2"},
		ReviewThreadComment{ID: "c3", Author: "looper-bot", Body: threadResolutionMarker("t1", "abc", fp, "accept_wontfix")},
	)
	decision, ok := resumeDispositionDecisionFromRemoteAudit(withAudit, "abc", "looper-bot")
	if !ok || decision.Decision != "accept_wontfix" {
		t.Fatalf("got %#v ok=%v, want accept resume", decision, ok)
	}
	if _, ok := resumeDispositionDecisionFromRemoteAudit(base, "abc", "looper-bot"); ok {
		t.Fatal("missing audit must not resume")
	}
}
