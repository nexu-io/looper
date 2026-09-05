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

func TestThreadFeedbackFingerprintIgnoresResolvedThreadCommentEdits(t *testing.T) {
	t.Parallel()
	resolved := looperThread("t1", true, looperRoot("c1", "Please fix nil check"))
	resolvedFP := ThreadFeedbackFingerprint([]ReviewThread{resolved})

	edited := looperThread("t1", true,
		ReviewThreadComment{ID: "c1", Author: "looper-bot", Body: "Please fix nil check carefully <!-- looper:stamp v=1 -->", CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T01:00:00Z", CommitOID: "old-head"},
	)
	if ThreadFeedbackFingerprint([]ReviewThread{edited}) != resolvedFP {
		t.Fatal("edits on a resolved thread must not change fingerprint")
	}

	replied := looperThread("t1", true,
		looperRoot("c1", "Please fix nil check"),
		ReviewThreadComment{ID: "c2", Author: "alice", AuthorAssociation: "MEMBER", Body: "drive-by note", CreatedAt: "2026-01-01T02:00:00Z", UpdatedAt: "2026-01-01T02:00:00Z"},
	)
	if ThreadFeedbackFingerprint([]ReviewThread{replied}) != resolvedFP {
		t.Fatal("new comments on a resolved thread must not change fingerprint")
	}

	unresolved := looperThread("t1", false, looperRoot("c1", "Please fix nil check"))
	if ThreadFeedbackFingerprint([]ReviewThread{unresolved}) == resolvedFP {
		t.Fatal("reopen must still change fingerprint")
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
	decision, ok := resumeDispositionDecisionFromRemoteAudit(withAudit, "abc", "looper-bot", "", nil, nil)
	if !ok || decision.Decision != "accept_wontfix" {
		t.Fatalf("got %#v ok=%v, want accept resume", decision, ok)
	}
	if _, ok := resumeDispositionDecisionFromRemoteAudit(base, "abc", "looper-bot", "", nil, nil); ok {
		t.Fatal("missing audit must not resume")
	}
	if !hasUnresolvedAcceptWontfixAudit(withAudit, "abc", "looper-bot") {
		t.Fatal("unresolved accept audit must be admitted for resolve resume")
	}
	if hasUnresolvedAcceptWontfixAudit(base, "abc", "looper-bot") {
		t.Fatal("missing audit must not look like unresolved accept")
	}
	resolved := withAudit
	resolved.IsResolved = true
	if hasUnresolvedAcceptWontfixAudit(resolved, "abc", "looper-bot") {
		t.Fatal("resolved accept must not stay a candidate")
	}
}

func TestUnresolvedAcceptAuditIsReadmittedAfterHeadChange(t *testing.T) {
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
	if ThreadHasChangedDispositionSignalForLogin(withAudit, "alice", "looper-bot") {
		t.Fatal("H1 accept must keep the human directive audited")
	}
	if _, ok := resumeDispositionDecisionFromRemoteAudit(withAudit, "def", "looper-bot", "", nil, nil); ok {
		t.Fatal("H1 accept must not resume as current-head authority")
	}
	if !hasUnresolvedAcceptWontfixAudit(withAudit, "def", "looper-bot") {
		t.Fatal("unresolved H1 accept must be re-admitted on H2")
	}
	if latestValidatedAuditDecision(withAudit, "looper-bot") != "accept_wontfix" {
		t.Fatal("latest complete audit must remain accept_wontfix")
	}
}

func TestResumeRejectDoesNotSwallowPostRejectDecline(t *testing.T) {
	t.Parallel()
	base := looperThread("t1", false,
		looperRoot("c1", "Please fix"),
		ReviewThreadComment{ID: "c2", Author: "alice", AuthorAssociation: "OWNER", Body: "/looper wontfix no", CreatedAt: "t2", UpdatedAt: "t2"},
	)
	rejectBody := threadResolutionMarker("t1", "abc", coordinationExcludedThreadFeedbackFingerprint(base, "looper-bot"), "reject_wontfix")
	afterReject := looperThread("t1", false,
		looperRoot("c1", "Please fix"),
		ReviewThreadComment{ID: "c2", Author: "alice", AuthorAssociation: "OWNER", Body: "/looper wontfix no", CreatedAt: "t2", UpdatedAt: "t2"},
		ReviewThreadComment{ID: "c3", Author: "looper-bot", Body: rejectBody, CreatedAt: "t3", UpdatedAt: "t3"},
	)
	decision, ok := resumeDispositionDecisionFromRemoteAudit(afterReject, "abc", "looper-bot", "", nil, nil)
	if !ok || decision.Decision != "reject_wontfix" {
		t.Fatalf("got %#v ok=%v, want reject resume before second decline", decision, ok)
	}
	second := looperThread("t1", false,
		looperRoot("c1", "Please fix"),
		ReviewThreadComment{ID: "c2", Author: "alice", AuthorAssociation: "OWNER", Body: "/looper wontfix no", CreatedAt: "t2", UpdatedAt: "t2"},
		ReviewThreadComment{ID: "c3", Author: "looper-bot", Body: rejectBody, CreatedAt: "t3", UpdatedAt: "t3"},
		ReviewThreadComment{ID: "c4", Author: "looper-bot", Body: "<!-- looper-fixer-reply-declined thread:t1 fingerprint:x attempt:post-reject -->", CreatedAt: "t4", UpdatedAt: "t4"},
	)
	if _, ok := resumeDispositionDecisionFromRemoteAudit(second, "abc", "looper-bot", "", nil, nil); ok {
		t.Fatal("post-reject decline must not resume reject_wontfix")
	}
	if !ForceNeedsHumanAfterSecondDecline(second, "abc", "looper-bot") {
		t.Fatal("post-reject decline must force needs_human")
	}
}

func TestResumeAcceptWontfixDoesNotReuseReopenedThread(t *testing.T) {
	t.Parallel()
	base := looperThread("t1", false,
		looperRoot("c1", "Please fix"),
		ReviewThreadComment{ID: "c2", Author: "alice", AuthorAssociation: "OWNER", Body: "/looper wontfix x", CreatedAt: "t2", UpdatedAt: "t2"},
	)
	openFP := ThreadFeedbackFingerprintForLogin([]ReviewThread{base}, "looper-bot")
	if openFP == "" {
		t.Fatal("expected open fingerprint")
	}
	accepted := looperThread("t1", false,
		looperRoot("c1", "Please fix"),
		ReviewThreadComment{ID: "c2", Author: "alice", AuthorAssociation: "OWNER", Body: "/looper wontfix x", CreatedAt: "t2", UpdatedAt: "t2"},
		ReviewThreadComment{ID: "c3", Author: "looper-bot", Body: threadResolutionMarker("t1", "abc", openFP, "accept_wontfix")},
	)
	if ThreadFeedbackFingerprintForLogin([]ReviewThread{accepted}, "looper-bot") != openFP {
		t.Fatal("accept audit must be excluded from the open fingerprint")
	}
	resolved := accepted
	resolved.IsResolved = true
	resolvedFP := ThreadFeedbackFingerprintForLogin([]ReviewThread{resolved}, "looper-bot")
	if resolvedFP == openFP {
		t.Fatal("resolved fingerprint must differ from open pre-resolve base")
	}
	reopened := accepted
	reopened.IsResolved = false
	if ThreadFeedbackFingerprintForLogin([]ReviewThread{reopened}, "looper-bot") != openFP {
		t.Fatal("reopened accepted thread fingerprint must equal pre-resolve base")
	}
	resolvedSignal := ReviewSignalFingerprint("abc", resolvedFP)
	openSignal := ReviewSignalFingerprint("abc", openFP)
	if _, ok := resumeDispositionDecisionFromRemoteAudit(reopened, "abc", "looper-bot", resolvedSignal, []ReviewThread{reopened}, nil); ok {
		t.Fatal("reopen must not resume accept_wontfix")
	}
	if !hasUnresolvedAcceptWontfixAudit(reopened, "abc", "looper-bot") {
		t.Fatal("reopened accept must stay a classifier candidate")
	}
	decision, ok := resumeDispositionDecisionFromRemoteAudit(accepted, "abc", "looper-bot", openSignal, []ReviewThread{accepted}, nil)
	if !ok || decision.Decision != "accept_wontfix" {
		t.Fatalf("crash-before-resolve got %#v ok=%v, want accept resume", decision, ok)
	}
	stale, ok := resumeDispositionDecisionFromRemoteAudit(accepted, "abc", "looper-bot", "stale-signal", []ReviewThread{accepted}, nil)
	if !ok || stale.Decision != "accept_wontfix" {
		t.Fatalf("stale last got %#v ok=%v, want accept resume", stale, ok)
	}
	same := ComputeReviewSignalFingerprintForLogin("abc", []ReviewThread{accepted}, "looper-bot")
	lastEq, ok := resumeDispositionDecisionFromRemoteAudit(accepted, "abc", "looper-bot", same, []ReviewThread{accepted}, nil)
	if !ok || lastEq.Decision != "accept_wontfix" {
		t.Fatalf("last==current got %#v ok=%v, want accept resume", lastEq, ok)
	}
}

func TestResumeAcceptWontfixDoesNotReuseTwoReopenedThreads(t *testing.T) {
	t.Parallel()
	accepted := func(id string, resolved bool) ReviewThread {
		base := looperThread(id, false,
			looperRoot("c1-"+id, "Please fix "+id),
			ReviewThreadComment{ID: "c2-" + id, Author: "alice", AuthorAssociation: "OWNER", Body: "/looper wontfix x", CreatedAt: "t2", UpdatedAt: "t2"},
		)
		fp := ThreadFeedbackFingerprintForLogin([]ReviewThread{base}, "looper-bot")
		return looperThread(id, resolved,
			looperRoot("c1-"+id, "Please fix "+id),
			ReviewThreadComment{ID: "c2-" + id, Author: "alice", AuthorAssociation: "OWNER", Body: "/looper wontfix x", CreatedAt: "t2", UpdatedAt: "t2"},
			ReviewThreadComment{ID: "c3-" + id, Author: "looper-bot", Body: threadResolutionMarker(id, "abc", fp, "accept_wontfix")},
		)
	}
	t1res := accepted("t1", true)
	t2res := accepted("t2", true)
	resolvedSignal := ComputeReviewSignalFingerprintForLogin("abc", []ReviewThread{t1res, t2res}, "looper-bot")
	t1open := t1res
	t1open.IsResolved = false
	t2open := t2res
	t2open.IsResolved = false
	live := []ReviewThread{t1open, t2open}
	if _, ok := resumeDispositionDecisionFromRemoteAudit(t1open, "abc", "looper-bot", resolvedSignal, live, nil); ok {
		t.Fatal("reopened t1 must not resume accept_wontfix")
	}
	if _, ok := resumeDispositionDecisionFromRemoteAudit(t2open, "abc", "looper-bot", resolvedSignal, live, nil); ok {
		t.Fatal("reopened t2 must not resume accept_wontfix")
	}
}

func TestResumeAcceptWontfixSkipsReopenWhenSiblingCommentsChange(t *testing.T) {
	t.Parallel()
	accepted := func(id string, resolved bool) ReviewThread {
		base := looperThread(id, false,
			looperRoot("c1-"+id, "Please fix "+id),
			ReviewThreadComment{ID: "c2-" + id, Author: "alice", AuthorAssociation: "OWNER", Body: "/looper wontfix x", CreatedAt: "t2", UpdatedAt: "t2"},
		)
		fp := ThreadFeedbackFingerprintForLogin([]ReviewThread{base}, "looper-bot")
		return looperThread(id, resolved,
			looperRoot("c1-"+id, "Please fix "+id),
			ReviewThreadComment{ID: "c2-" + id, Author: "alice", AuthorAssociation: "OWNER", Body: "/looper wontfix x", CreatedAt: "t2", UpdatedAt: "t2"},
			ReviewThreadComment{ID: "c3-" + id, Author: "looper-bot", Body: threadResolutionMarker(id, "abc", fp, "accept_wontfix")},
		)
	}
	t1res := accepted("t1", true)
	t2res := accepted("t2", true)
	t3old := looperThread("t3", false, looperRoot("c1-t3", "Please look at this"))
	lastSignal := ComputeReviewSignalFingerprintForLogin("abc", []ReviewThread{t1res, t2res, t3old}, "looper-bot")
	t1open := t1res
	t1open.IsResolved = false
	t3new := looperThread("t3", false, looperRoot("c1-t3", "Please look at this carefully"))
	live := []ReviewThread{t1open, t2res, t3new}
	lastResolved := []string{"t1", "t2"}
	if _, ok := resumeDispositionDecisionFromRemoteAudit(t1open, "abc", "looper-bot", lastSignal, live, lastResolved); ok {
		t.Fatal("reopened t1 must not resume when lastResolvedIDs contains it")
	}
	crash := accepted("t1", false)
	if decision, ok := resumeDispositionDecisionFromRemoteAudit(crash, "abc", "looper-bot", lastSignal, []ReviewThread{crash, t2res, t3new}, []string{"t2"}); !ok || decision.Decision != "accept_wontfix" {
		t.Fatalf("crash-before-resolve got %#v ok=%v, want accept resume", decision, ok)
	}
}

func TestCanonicalFingerprintIncludesThirdPartyFixerDecline(t *testing.T) {
	t.Parallel()
	codexRoot := ReviewThreadComment{ID: "c1", Author: "codex", Body: "This is a bug", CreatedAt: "t1", UpdatedAt: "t1"}
	withoutDecline := ReviewThread{ID: "t1", Comments: []ReviewThreadComment{codexRoot}}
	withDecline := ReviewThread{ID: "t1", Comments: []ReviewThreadComment{
		codexRoot,
		{ID: "c2", Author: "looper-bot", Body: "declined <!-- looper-fixer-reply-declined thread:t1 fingerprint:abc -->", CreatedAt: "t2", UpdatedAt: "t2"},
	}}
	if ThreadHasChangedDispositionSignalForLogin(withoutDecline, "alice", "looper-bot") {
		t.Fatal("Codex thread without decline must not be a disposition signal")
	}
	if !ThreadHasChangedDispositionSignalForLogin(withDecline, "alice", "looper-bot") {
		t.Fatal("Codex thread with Looper decline must be a changed disposition signal")
	}
	if ThreadFeedbackFingerprintForLogin([]ReviewThread{withDecline}, "looper-bot") == ThreadFeedbackFingerprintForLogin([]ReviewThread{withoutDecline}, "looper-bot") {
		t.Fatal("unresolved Codex decline must change the review signal fingerprint")
	}
	humanWontfix := ReviewThread{ID: "t1", Comments: []ReviewThreadComment{
		{ID: "c1", Author: "alice", AuthorAssociation: "OWNER", Body: "Please fix", CreatedAt: "t1", UpdatedAt: "t1"},
		{ID: "c2", Author: "alice", AuthorAssociation: "OWNER", Body: "/looper wontfix out of scope", CreatedAt: "t2", UpdatedAt: "t2"},
	}}
	if ThreadHasChangedDispositionSignalForLogin(humanWontfix, "alice", "looper-bot") {
		t.Fatal("trusted /looper wontfix must still require a Looper-authored root")
	}
}
