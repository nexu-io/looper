package reviewer

import (
	"testing"

	"github.com/nexu-io/looper/internal/config"
)

func TestParseTrustedDispositionDirectiveCanonicalAndAliases(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		body   string
		wantOK bool
		kind   string
		reason string
	}{
		{name: "canonical", body: "/looper wontfix out of scope for this PR", wantOK: true, kind: DispositionWontfix, reason: "out of scope for this PR"},
		{name: "canonical reconsider", body: "/looper reconsider still needed", wantOK: true, kind: DispositionReconsider, reason: "still needed"},
		{name: "alias plain", body: "wontfix", wantOK: true, kind: DispositionWontfix},
		{name: "alias ascii apostrophe", body: "won't fix", wantOK: true, kind: DispositionWontfix},
		{name: "alias unicode apostrophe", body: "won\u2019t fix", wantOK: true, kind: DispositionWontfix},
		{name: "alias with reason", body: "won't fix: covered elsewhere", wantOK: true, kind: DispositionWontfix, reason: "covered elsewhere"},
		{name: "incidental prose", body: "I think we should not wontfix this yet", wantOK: false},
		{name: "quoted only", body: "> wontfix\n\nlooking into it", wantOK: false},
		{name: "empty", body: "   ", wantOK: false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := ParseTrustedDispositionDirective(tc.body)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (got %#v)", ok, tc.wantOK, got)
			}
			if !tc.wantOK {
				return
			}
			if got.Kind != tc.kind || got.Reason != tc.reason {
				t.Fatalf("got %#v, want kind=%q reason=%q", got, tc.kind, tc.reason)
			}
		})
	}
}

func TestIsTrustedDispositionAuthority(t *testing.T) {
	t.Parallel()
	if !IsTrustedDispositionAuthority("alice", "alice", "") {
		t.Fatal("PR author should be authority")
	}
	if !IsTrustedDispositionAuthority("bob", "alice", "MEMBER") {
		t.Fatal("MEMBER should be authority")
	}
	if !IsTrustedDispositionAuthority("bob", "alice", "OWNER") {
		t.Fatal("OWNER should be authority")
	}
	if !IsTrustedDispositionAuthority("bob", "alice", "COLLABORATOR") {
		t.Fatal("COLLABORATOR should be authority")
	}
	if IsTrustedDispositionAuthority("outsider", "alice", "NONE") {
		t.Fatal("external user must not be authority")
	}
	if IsTrustedDispositionAuthority("helper[bot]", "alice", "NONE") {
		t.Fatal("bot must not be authority")
	}
	if IsTrustedDispositionAuthority("helper", "alice", "BOT") {
		t.Fatal("BOT association must not be authority")
	}
}

func TestLatestTrustedDispositionLooperAuthoredOnly(t *testing.T) {
	t.Parallel()
	nonLooper := ReviewThread{
		ID: "t1",
		Comments: []ReviewThreadComment{
			{ID: "c1", Author: "reviewer", Body: "Please fix this"},
			{ID: "c2", Author: "alice", AuthorAssociation: "OWNER", Body: "/looper wontfix no"},
		},
	}
	if _, _, ok := LatestTrustedDisposition(nonLooper, "alice"); ok {
		t.Fatal("non-Looper thread must not yield disposition")
	}

	looper := ReviewThread{
		ID: "t1",
		Comments: []ReviewThreadComment{
			{ID: "c1", Author: "looper-bot", Body: "Please fix <!-- looper:stamp v=1 -->"},
			{ID: "c2", Author: "outsider", AuthorAssociation: "NONE", Body: "/looper wontfix no"},
			{ID: "c3", Author: "alice", AuthorAssociation: "OWNER", Body: "/looper wontfix real reason"},
		},
	}
	got, comment, ok := LatestTrustedDisposition(looper, "alice")
	if !ok || got.Kind != DispositionWontfix || got.Reason != "real reason" || comment.ID != "c3" {
		t.Fatalf("got (%#v, %#v, %v)", got, comment, ok)
	}
}

func TestHasUnauditedValidatedFixerDecline(t *testing.T) {
	t.Parallel()
	thread := ReviewThread{
		ID: "t1",
		Comments: []ReviewThreadComment{
			{ID: "c1", Author: "looper-bot", Body: "Please fix <!-- looper:stamp v=1 -->"},
			{ID: "c2", Author: "looper-bot", Body: "declined <!-- looper-fixer-reply-declined thread:t1 fingerprint:abc -->"},
		},
	}
	if !HasUnauditedValidatedFixerDecline(thread) {
		t.Fatal("expected unaudited fixer decline")
	}
	thread.Comments = append(thread.Comments, ReviewThreadComment{
		ID: "c3", Author: "looper-bot",
		Body: "<!-- looper:thread-resolution thread=t1 head=h feedback=f decision=accept_wontfix -->",
	})
	if HasUnauditedValidatedFixerDecline(thread) {
		t.Fatal("audited decline should not be unaudited")
	}
}

func TestForceNeedsHumanUnchangedInputOnly(t *testing.T) {
	t.Parallel()
	baseComments := []ReviewThreadComment{
		{ID: "c1", Author: "looper-bot", Body: "Please fix <!-- looper:stamp v=1 -->"},
		{ID: "c2", Author: "alice", AuthorAssociation: "OWNER", Body: "/looper wontfix no", CreatedAt: "t2", UpdatedAt: "t2"},
	}
	base := ReviewThread{ID: "t1", Comments: append([]ReviewThreadComment{}, baseComments...)}
	// Marker feedback must come from the real rejection builder (coordination-excluded FP).
	runner := &Runner{}
	rejectBody := runner.buildThreadResolutionReplyWithFeedback(
		"t1", "abc",
		coordinationExcludedThreadFeedbackFingerprint(base, "looper-bot"),
		threadResolutionAgentDecision{Decision: "reject_wontfix", Evidence: "still needed", Confidence: "high"},
		config.ReviewerThreadResolutionConfig{},
	)

	// Unchanged human text + post-reject decline → true
	unchanged := ReviewThread{ID: "t1", Comments: append(append([]ReviewThreadComment{}, baseComments...),
		ReviewThreadComment{ID: "c3", Author: "looper-bot", Body: rejectBody, CreatedAt: "t3", UpdatedAt: "t3"},
		ReviewThreadComment{ID: "c4", Author: "looper-bot", Body: "<!-- looper-fixer-reply-declined thread:t1 fingerprint:x -->", CreatedAt: "t4", UpdatedAt: "t4"},
	)}
	if !ForceNeedsHumanAfterSecondDecline(unchanged, "abc", "looper-bot") {
		t.Fatal("unchanged human + decline after reject must force needs_human")
	}

	// Human edits /looper wontfix after reject, then decline → false
	edited := ReviewThread{ID: "t1", Comments: []ReviewThreadComment{
		{ID: "c1", Author: "looper-bot", Body: "Please fix <!-- looper:stamp v=1 -->"},
		{ID: "c2", Author: "alice", AuthorAssociation: "OWNER", Body: "/looper wontfix EDITED reason", CreatedAt: "t2", UpdatedAt: "t5"},
		{ID: "c3", Author: "looper-bot", Body: rejectBody, CreatedAt: "t3", UpdatedAt: "t3"},
		{ID: "c4", Author: "looper-bot", Body: "<!-- looper-fixer-reply-declined thread:t1 fingerprint:x -->", CreatedAt: "t4", UpdatedAt: "t4"},
	}}
	if ForceNeedsHumanAfterSecondDecline(edited, "abc", "looper-bot") {
		t.Fatal("changed human directive must not force needs_human")
	}

	// Head change → false
	if ForceNeedsHumanAfterSecondDecline(unchanged, "other-head", "looper-bot") {
		t.Fatal("head change must not force needs_human")
	}
}

func TestLatestTrustedDispositionTreatsInPlaceEditAsNewInput(t *testing.T) {
	t.Parallel()
	audit := "<!-- looper:thread-resolution thread=t1 head=h feedback=f decision=reject_wontfix -->"
	thread := ReviewThread{
		ID: "t1",
		Comments: []ReviewThreadComment{
			{ID: "c1", Author: "looper-bot", Body: "Please fix <!-- looper:stamp v=1 -->"},
			{ID: "c2", Author: "alice", AuthorAssociation: "OWNER", Body: "/looper reconsider still required", CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T13:00:00Z"},
			{ID: "c3", Author: "looper-bot", Body: audit, CreatedAt: "2026-01-01T12:00:00Z", UpdatedAt: "2026-01-01T12:00:00Z"},
		},
	}
	got, comment, ok := LatestTrustedDispositionForLogin(thread, "alice", "looper-bot")
	if !ok || got.Kind != DispositionReconsider || comment.ID != "c2" {
		t.Fatalf("edited directive after audit = (%#v, %#v, %v)", got, comment, ok)
	}
	if !HasUnauditedTrustedDispositionForLogin(thread, "alice", "looper-bot") {
		t.Fatal("edited directive must remain unaudited input")
	}
	if !ThreadHasChangedDispositionSignalForLogin(thread, "alice", "looper-bot") {
		t.Fatal("signal must notice the in-place edit")
	}
}
