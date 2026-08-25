package fixer

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/storage"
)

const testLooperLogin = "looper-bot"

func TestThreadWithheldFromFixerTrustedDisposition(t *testing.T) {
	t.Parallel()
	thread := ReviewThread{
		ID: "t1",
		Comments: []ReviewThreadComment{
			{ID: "c1", Author: testLooperLogin, Body: "Please fix <!-- looper:stamp v=1 -->"},
			{ID: "c2", Author: "alice", AuthorAssociation: "OWNER", Body: "/looper wontfix out of scope"},
		},
	}
	if !ThreadWithheldFromFixer(thread, "alice", testLooperLogin) {
		t.Fatal("trusted disposition should withhold")
	}
	// reject_wontfix restores the existing thread as actionable.
	thread.Comments = append(thread.Comments, ReviewThreadComment{
		ID: "c3", Author: testLooperLogin,
		Body: "<!-- looper:thread-resolution thread=t1 head=h feedback=f decision=reject_wontfix -->",
	})
	if ThreadWithheldFromFixer(thread, "alice", testLooperLogin) {
		t.Fatal("reject_wontfix should restore actionable Fixer item")
	}
}

func TestThreadWithheldFromFixerAcceptWontfixStaysWithheldUntilResolved(t *testing.T) {
	t.Parallel()
	thread := ReviewThread{
		ID: "t1",
		Comments: []ReviewThreadComment{
			{ID: "c1", Author: testLooperLogin, Body: "Please fix <!-- looper:stamp v=1 -->"},
			{ID: "c2", Author: "alice", AuthorAssociation: "OWNER", Body: "/looper wontfix out of scope"},
			{ID: "c3", Author: testLooperLogin, Body: "accepted <!-- looper:thread-resolution thread=t1 head=h feedback=f decision=accept_wontfix -->"},
		},
	}
	// accept reply succeeded but resolve failed → still unresolved → withheld.
	if !ThreadWithheldFromFixer(thread, "alice", testLooperLogin) {
		t.Fatal("accept_wontfix on unresolved thread must stay withheld")
	}
	thread.IsResolved = true
	if ThreadWithheldFromFixer(thread, "alice", testLooperLogin) {
		t.Fatal("resolved accept_wontfix thread must not withhold")
	}
}

func TestThreadWithheldFromFixerValidatedDecline(t *testing.T) {
	t.Parallel()
	thread := ReviewThread{
		ID: "t1",
		Comments: []ReviewThreadComment{
			{ID: "c1", Author: testLooperLogin, Body: "Please fix <!-- looper:stamp v=1 -->"},
			{ID: "c2", Author: testLooperLogin, Body: "nope <!-- looper-fixer-reply-declined thread:t1 fingerprint:x -->"},
		},
	}
	if !ThreadWithheldFromFixer(thread, "alice", testLooperLogin) {
		t.Fatal("validated fixer decline should withhold further edits")
	}
}

func TestThreadWithheldFromFixerIgnoresUntrusted(t *testing.T) {
	t.Parallel()
	thread := ReviewThread{
		ID: "t1",
		Comments: []ReviewThreadComment{
			{ID: "c1", Author: testLooperLogin, Body: "Please fix <!-- looper:stamp v=1 -->"},
			{ID: "c2", Author: "rando", AuthorAssociation: "NONE", Body: "/looper wontfix no"},
		},
	}
	if ThreadWithheldFromFixer(thread, "alice", testLooperLogin) {
		t.Fatal("untrusted wontfix must not withhold")
	}
}

func TestThreadWithheldFromFixerIgnoresSpoofedMarkers(t *testing.T) {
	t.Parallel()
	// Spoofed root stamp from untrusted author is not Looper-authored.
	spoofedRoot := ReviewThread{
		ID: "t1",
		Comments: []ReviewThreadComment{
			{ID: "c1", Author: "evil", Body: "Please fix <!-- looper:stamp v=1 -->"},
			{ID: "c2", Author: "alice", AuthorAssociation: "OWNER", Body: "/looper wontfix"},
		},
	}
	if ThreadWithheldFromFixer(spoofedRoot, "alice", testLooperLogin) {
		t.Fatal("spoofed stamp root must not withhold")
	}
	// Spoofed decline from untrusted author is not a validated decline.
	spoofedDecline := ReviewThread{
		ID: "t1",
		Comments: []ReviewThreadComment{
			{ID: "c1", Author: testLooperLogin, Body: "Please fix <!-- looper:stamp v=1 -->"},
			{ID: "c2", Author: "evil", Body: "nope <!-- looper-fixer-reply-declined thread:t1 fingerprint:x -->"},
		},
	}
	if ThreadWithheldFromFixer(spoofedDecline, "alice", testLooperLogin) {
		t.Fatal("spoofed decline must not withhold")
	}
	// Spoofed accept audit must not keep thread withheld / count as audit.
	spoofedAudit := ReviewThread{
		ID: "t1",
		Comments: []ReviewThreadComment{
			{ID: "c1", Author: testLooperLogin, Body: "Please fix <!-- looper:stamp v=1 -->"},
			{ID: "c2", Author: "alice", AuthorAssociation: "OWNER", Body: "/looper wontfix"},
			{ID: "c3", Author: "evil", Body: "<!-- looper:thread-resolution thread=t1 head=h feedback=f decision=accept_wontfix -->"},
		},
	}
	if !ThreadWithheldFromFixer(spoofedAudit, "alice", testLooperLogin) {
		t.Fatal("spoofed audit must not clear trusted disposition withhold")
	}
	// Substring needle without exact HTML marker is not a validated decline.
	needleOnly := ReviewThread{
		ID: "t1",
		Comments: []ReviewThreadComment{
			{ID: "c1", Author: testLooperLogin, Body: "Please fix <!-- looper:stamp v=1 -->"},
			{ID: "c2", Author: testLooperLogin, Body: "mentioning looper-fixer-reply-declined without marker"},
		},
	}
	if ThreadWithheldFromFixer(needleOnly, "alice", testLooperLogin) {
		t.Fatal("substring decline needle must not count as validated decline")
	}
}

func TestSuppressWithheldDispositionFixItems(t *testing.T) {
	t.Parallel()
	threads := []ReviewThread{{
		ID: "t1",
		Comments: []ReviewThreadComment{
			{ID: "c1", Author: testLooperLogin, Body: "Please fix <!-- looper:stamp v=1 -->"},
			{ID: "c2", Author: "alice", AuthorAssociation: "MEMBER", Body: "wontfix"},
		},
	}}
	items := []FixItem{
		{Type: "comment", ID: "c1", ThreadID: "t1"},
		{Type: "comment", ID: "c3", ThreadID: "t2"},
		{Type: "check", Name: "ci"},
	}
	got := SuppressWithheldDispositionFixItems(items, threads, "alice", testLooperLogin)
	if len(got) != 2 {
		t.Fatalf("got %#v, want t2 + check", got)
	}
	if got[0].ThreadID != "t2" || got[1].Type != "check" {
		t.Fatalf("got %#v", got)
	}
}

func TestAdmitWithheldDispositionSkipsQueuedItemAfterNewDisposition(t *testing.T) {
	t.Parallel()
	github := &fakeGitHubGateway{
		currentUser: testLooperLogin,
		viewResponses: []PullRequestDetail{{
			Number: 42, State: "OPEN", HeadSHA: "h1", Author: "alice",
		}},
		threads: []ReviewThread{{
			ID: "t1",
			Comments: []ReviewThreadComment{
				{ID: "c1", Author: testLooperLogin, Body: "Please fix <!-- looper:stamp v=1 -->"},
				{ID: "c2", Author: "alice", AuthorAssociation: "OWNER", Body: "/looper wontfix after queue"},
			},
		}},
	}
	runner := New(Options{GitHub: github})
	items := []FixItem{{Type: "comment", ID: "c1", ThreadID: "t1", Author: "reviewer", Summary: "fix me"}}
	// collect-fixes admission path
	got, err := runner.admitWithheldDispositionFixItems(context.Background(), stepInput{
		Project:  storage.ProjectRecord{ID: "p1", RepoPath: t.TempDir()},
		Repo:     "acme/looper",
		PRNumber: 42,
	}, items)
	if err != nil {
		t.Fatalf("admit error = %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %#v, want all comment items withheld (no agent edits)", got)
	}
}

func TestAdmitWithheldDispositionListFailureFailClosed(t *testing.T) {
	t.Parallel()
	github := &fakeGitHubGateway{
		currentUser:    testLooperLogin,
		listThreadsErr: errors.New("github timeout"),
		viewResponses:  []PullRequestDetail{{Number: 42, State: "OPEN", Author: "alice"}},
	}
	runner := New(Options{GitHub: github})
	items := []FixItem{{Type: "comment", ID: "c1", ThreadID: "t1"}}
	got, err := runner.admitWithheldDispositionFixItems(context.Background(), stepInput{
		Project:  storage.ProjectRecord{ID: "p1", RepoPath: t.TempDir()},
		Repo:     "acme/looper",
		PRNumber: 42,
	}, items)
	if err == nil {
		t.Fatal("want retryable fail-closed error on list failure")
	}
	var loopErr *loopError
	if !errors.As(err, &loopErr) || loopErr.kind != FailureRetryableTransient {
		t.Fatalf("err = %#v, want retryable_transient loopError", err)
	}
	if got != nil {
		t.Fatalf("got items %#v, want nil on fail-closed", got)
	}
}

func TestAdmitWithheldDispositionEmptyLoginFailClosed(t *testing.T) {
	t.Parallel()
	// Integration tokens can yield ("", nil) from GetCurrentUserLogin; empty
	// identity must not fail-open into SuppressWithheldDispositionFixItems.
	github := &fakeGitHubGateway{
		currentUserEmpty: true,
		viewResponses:    []PullRequestDetail{{Number: 42, State: "OPEN", Author: "alice"}},
		threads: []ReviewThread{{
			ID: "t1",
			Comments: []ReviewThreadComment{
				{ID: "c1", Author: testLooperLogin, Body: "Please fix <!-- looper:stamp v=1 -->"},
				{ID: "c2", Author: "alice", AuthorAssociation: "OWNER", Body: "wontfix"},
			},
		}},
	}
	runner := New(Options{GitHub: github})
	items := []FixItem{{Type: "comment", ID: "c1", ThreadID: "t1", Author: "reviewer", Summary: "fix me"}}
	got, err := runner.admitWithheldDispositionFixItems(context.Background(), stepInput{
		Project:  storage.ProjectRecord{ID: "p1", RepoPath: t.TempDir()},
		Repo:     "acme/looper",
		PRNumber: 42,
	}, items)
	if err == nil {
		t.Fatal("want retryable fail-closed error on empty disposition identity")
	}
	var loopErr *loopError
	if !errors.As(err, &loopErr) || loopErr.kind != FailureRetryableTransient {
		t.Fatalf("err = %#v, want retryable_transient loopError", err)
	}
	if !strings.Contains(loopErr.message, "fixer disposition identity is empty") {
		t.Fatalf("err message = %q, want empty identity", loopErr.message)
	}
	if got != nil {
		t.Fatalf("got items %#v, want nil (not treated as actionable)", got)
	}
	if len(github.listThreadsCalls) != 0 {
		t.Fatalf("listThreadsCalls = %d, want 0 (reject empty login before withhold scan)", len(github.listThreadsCalls))
	}
}

func TestDispositionLooperLoginRejectsEmpty(t *testing.T) {
	t.Parallel()
	runner := New(Options{GitHub: &fakeGitHubGateway{currentUserEmpty: true}})
	login, err := runner.dispositionLooperLogin(context.Background(), t.TempDir())
	if err == nil {
		t.Fatal("want error for empty login")
	}
	var loopErr *loopError
	if !errors.As(err, &loopErr) || loopErr.kind != FailureRetryableTransient {
		t.Fatalf("err = %#v, want retryable_transient loopError", err)
	}
	if login != "" {
		t.Fatalf("login = %q, want empty", login)
	}
	if !strings.Contains(loopErr.message, "fixer disposition identity is empty") {
		t.Fatalf("message = %q", loopErr.message)
	}
}

func TestAdmitWithheldDispositionListUsesAllPages(t *testing.T) {
	t.Parallel()
	github := &fakeGitHubGateway{
		currentUser: testLooperLogin,
		viewResponses: []PullRequestDetail{{
			Number: 42, State: "OPEN", HeadSHA: "h1", Author: "alice",
		}},
		threads: []ReviewThread{{
			ID: "t1",
			Comments: []ReviewThreadComment{
				{ID: "c1", Author: testLooperLogin, Body: "Please fix <!-- looper:stamp v=1 -->"},
			},
		}},
	}
	runner := New(Options{GitHub: github})
	items := []FixItem{{Type: "comment", ID: "c1", ThreadID: "t1", Author: "reviewer", Summary: "fix me"}}
	if _, err := runner.admitWithheldDispositionFixItems(context.Background(), stepInput{
		Project:  storage.ProjectRecord{ID: "p1", RepoPath: t.TempDir()},
		Repo:     "acme/looper",
		PRNumber: 42,
	}, items); err != nil {
		t.Fatalf("admit error = %v", err)
	}
	if len(github.listThreadsCalls) != 1 {
		t.Fatalf("listThreadsCalls = %d, want 1", len(github.listThreadsCalls))
	}
	got := github.listThreadsCalls[0]
	if !got.AllPages {
		t.Fatalf("ListReviewThreads AllPages = false, want true (authority withhold read must paginate fully)")
	}
	if got.Limit != 0 {
		t.Fatalf("ListReviewThreads Limit = %d, want 0 when AllPages is set", got.Limit)
	}
	if got.Repo != "acme/looper" || got.PRNumber != 42 {
		t.Fatalf("ListReviewThreads input = %#v", got)
	}
}

func TestCollectFixesAdmissionWithholdsBeforeWorktree(t *testing.T) {
	t.Parallel()
	github := &fakeGitHubGateway{
		currentUser: testLooperLogin,
		viewResponses: []PullRequestDetail{{
			Number: 42, State: "OPEN", HeadSHA: "h1", HeadRefName: "feature/x", BaseRefName: "main", Author: "alice",
			Comments: []map[string]any{
				{"id": "c1", "threadId": "t1", "body": "please fix", "author": "reviewer"},
			},
		}},
		threads: []ReviewThread{{
			ID: "t1",
			Comments: []ReviewThreadComment{
				{ID: "c1", Author: testLooperLogin, Body: "Please fix <!-- looper:stamp v=1 -->"},
				{ID: "c2", Author: "alice", AuthorAssociation: "OWNER", Body: "wontfix"},
			},
		}},
	}
	runner := New(Options{GitHub: github})
	checkpoint := fixerCheckpoint{
		Detail: &checkpointDetail{
			State: "OPEN", HeadSHA: "h1", HeadRefName: "feature/x", BaseRefName: "main",
			Comments: []map[string]any{
				{"id": "c1", "threadId": "t1", "body": "please fix", "author": "reviewer"},
			},
		},
	}
	updated, err := runner.runCollectFixesStep(context.Background(), stepInput{
		Project:    storage.ProjectRecord{ID: "p1", RepoPath: t.TempDir()},
		Repo:       "acme/looper",
		PRNumber:   42,
		Checkpoint: checkpoint,
	})
	if err != nil {
		t.Fatalf("runCollectFixesStep() error = %v", err)
	}
	if updated.SkipReason == "" {
		t.Fatal("want skip when all remaining comment items are withheld")
	}
	if len(updated.FixItems) != 0 {
		t.Fatalf("FixItems = %#v, want empty (no agent/worktree mutation)", updated.FixItems)
	}
}

func TestStalePreRejectDeclineReplay(t *testing.T) {
	t.Parallel()
	reject := ReviewThreadComment{
		ID: "c3", Author: testLooperLogin,
		Body:      "<!-- looper:thread-resolution thread=t1 head=h feedback=f decision=reject_wontfix --> <!-- looper:stamp v=1 -->",
		CreatedAt: "2026-08-25T08:10:00Z",
		UpdatedAt: "2026-08-25T08:10:00Z",
	}
	thread := ReviewThread{ID: "t1", Comments: []ReviewThreadComment{
		{ID: "c1", Author: testLooperLogin, Body: "Please fix <!-- looper:stamp v=1 -->"},
		{ID: "c2", Author: testLooperLogin, Body: "declined <!-- looper-fixer-reply-declined thread:t1 fingerprint:x -->"},
		reject,
	}}
	if !stalePreRejectDeclineReplay(thread, testLooperLogin, "2026-08-25T08:00:00Z") {
		t.Fatal("repair before reject must be treated as stale replay")
	}
	if stalePreRejectDeclineReplay(thread, testLooperLogin, "2026-08-25T08:20:00Z") {
		t.Fatal("repair after reject is a fresh decision, not replay")
	}
	if !stalePreRejectDeclineReplay(thread, testLooperLogin, "") {
		t.Fatal("missing repair time must fail closed as stale replay")
	}
	untimed := thread
	untimed.Comments = append([]ReviewThreadComment{}, thread.Comments...)
	untimed.Comments[2].CreatedAt = ""
	untimed.Comments[2].UpdatedAt = ""
	if !stalePreRejectDeclineReplay(untimed, testLooperLogin, "2026-08-25T08:20:00Z") {
		t.Fatal("missing reject time must fail closed as stale replay")
	}
	thread.Comments = append(thread.Comments, ReviewThreadComment{
		ID: "c4", Author: testLooperLogin,
		Body: "second decline <!-- looper-fixer-reply-declined thread:t1 fingerprint:x attempt:post-reject -->",
	})
	if stalePreRejectDeclineReplay(thread, testLooperLogin, "2026-08-25T08:00:00Z") {
		t.Fatal("existing post-reject decline is idempotent, not stale replay")
	}
}

func TestReplyToDeclinedAfterRejectPostsNewMarker(t *testing.T) {
	t.Parallel()
	fp := "deadbeef"
	baseMarker := fixerDeclinedReplyMarker("t1", fp)
	postMarker := fixerDeclinedReplyMarkerPostReject("t1", fp)
	github := &fakeGitHubGateway{
		currentUser: testLooperLogin,
		threads: []ReviewThread{{
			ID: "t1",
			Comments: []ReviewThreadComment{
				{ID: "c1", Author: testLooperLogin, Body: "Please fix <!-- looper:stamp v=1 -->"},
				{ID: "c2", Author: testLooperLogin, Body: "first decline " + baseMarker},
				{ID: "c3", Author: testLooperLogin, Body: "<!-- looper:thread-resolution thread=t1 head=h feedback=f decision=reject_wontfix -->"},
			},
		}},
	}
	runner := New(Options{GitHub: github})
	item := FixItem{Type: "comment", ID: "c1", ThreadID: "t1", Author: "alice"}
	state, replyErr := runner.replyToDeclinedComment(context.Background(), stepInput{
		Project: storage.ProjectRecord{RepoPath: t.TempDir()},
		Repo:    "acme/looper",
	}, item, fp, "Still out of scope after reject.", nil)
	if replyErr != "" || state != "sent" {
		t.Fatalf("reply state=%q err=%q, want sent", state, replyErr)
	}
	if len(github.replyCalls) != 1 {
		t.Fatalf("reply calls = %d, want 1 new post-reject decline", len(github.replyCalls))
	}
	if !strings.Contains(github.replyCalls[0].Body, postMarker) {
		t.Fatalf("reply body = %q, want post-reject marker %q", github.replyCalls[0].Body, postMarker)
	}
	// Second call is idempotent (same post-reject marker already present).
	state2, replyErr2 := runner.replyToDeclinedComment(context.Background(), stepInput{
		Project: storage.ProjectRecord{RepoPath: t.TempDir()},
		Repo:    "acme/looper",
	}, item, fp, "Still out of scope after reject.", nil)
	if replyErr2 != "" || state2 != "sent" {
		t.Fatalf("second reply state=%q err=%q", state2, replyErr2)
	}
	if len(github.replyCalls) != 1 {
		t.Fatalf("reply calls after idempotent retry = %d, want still 1", len(github.replyCalls))
	}
}

func TestReplyToDeclinedBeforeRejectIsIdempotent(t *testing.T) {
	t.Parallel()
	fp := "cafebabe"
	baseMarker := fixerDeclinedReplyMarker("t1", fp)
	github := &fakeGitHubGateway{
		currentUser: testLooperLogin,
		threads: []ReviewThread{{
			ID: "t1",
			Comments: []ReviewThreadComment{
				{ID: "c1", Author: testLooperLogin, Body: "Please fix <!-- looper:stamp v=1 -->"},
				{ID: "c2", Author: testLooperLogin, Body: "first decline " + baseMarker},
			},
		}},
	}
	runner := New(Options{GitHub: github})
	item := FixItem{Type: "comment", ID: "c1", ThreadID: "t1", Author: "alice"}
	state, replyErr := runner.replyToDeclinedComment(context.Background(), stepInput{
		Project: storage.ProjectRecord{RepoPath: t.TempDir()},
		Repo:    "acme/looper",
	}, item, fp, "Out of scope.", nil)
	if replyErr != "" || state != "sent" {
		t.Fatalf("reply state=%q err=%q, want sent (idempotent)", state, replyErr)
	}
	if len(github.replyCalls) != 0 {
		t.Fatalf("reply calls = %d, want 0 (same-fingerprint decline remains idempotent before reject)", len(github.replyCalls))
	}
}

func TestRejectWontfixRestoresExistingFixerItem(t *testing.T) {
	t.Parallel()
	threads := []ReviewThread{{
		ID: "t1",
		Comments: []ReviewThreadComment{
			{ID: "c1", Author: testLooperLogin, Body: "Please fix <!-- looper:stamp v=1 -->"},
			{ID: "c2", Author: testLooperLogin, Body: "declined <!-- looper-fixer-reply-declined thread:t1 fingerprint:x -->"},
			{ID: "c3", Author: testLooperLogin, Body: "<!-- looper:thread-resolution thread=t1 head=h feedback=f decision=reject_wontfix -->"},
		},
	}}
	items := []FixItem{{Type: "comment", ID: "c1", ThreadID: "t1"}}
	got := SuppressWithheldDispositionFixItems(items, threads, "alice", testLooperLogin)
	if len(got) != 1 || got[0].ThreadID != "t1" {
		t.Fatalf("got %#v, want existing t1 item restored after reject_wontfix", got)
	}
}

func TestAdmitWithheldDispositionSkipsNonCommentWithoutAuthorityReads(t *testing.T) {
	t.Parallel()
	github := &fakeGitHubGateway{
		currentUserEmpty: true,
		listThreadsErr:   errors.New("should not list threads"),
		authorErr:        errors.New("should not look up author"),
	}
	runner := New(Options{GitHub: github})
	items := []FixItem{
		{Type: "check", ID: "ci-1", Summary: "CI failed"},
		{Type: "conflict", ID: "merge-1", Summary: "merge conflict"},
	}
	got, err := runner.admitWithheldDispositionFixItems(context.Background(), stepInput{
		Project:  storage.ProjectRecord{ID: "p1", RepoPath: t.TempDir()},
		Repo:     "acme/looper",
		PRNumber: 42,
	}, items)
	if err != nil {
		t.Fatalf("admit error = %v, want skip authority reads for non-comment work", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %#v, want original check/conflict items", got)
	}
	if len(github.listThreadsCalls) != 0 {
		t.Fatalf("listThreadsCalls = %d, want 0", len(github.listThreadsCalls))
	}
}

func TestEditedDirectiveAfterRejectIsWithheld(t *testing.T) {
	t.Parallel()
	thread := ReviewThread{
		ID: "t1",
		Comments: []ReviewThreadComment{
			{ID: "c1", Author: testLooperLogin, Body: "Please fix <!-- looper:stamp v=1 -->"},
			{ID: "c2", Author: "alice", AuthorAssociation: "OWNER", Body: "/looper wontfix still out of scope", CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T13:00:00Z"},
			{ID: "c3", Author: testLooperLogin, Body: "<!-- looper:thread-resolution thread=t1 head=h feedback=f decision=reject_wontfix -->", CreatedAt: "2026-01-01T12:00:00Z", UpdatedAt: "2026-01-01T12:00:00Z"},
		},
	}
	if !ThreadWithheldFromFixer(thread, "alice", testLooperLogin) {
		t.Fatal("in-place edit after reject_wontfix must withhold Fixer edits")
	}
	items := []FixItem{{Type: "comment", ID: "c1", ThreadID: "t1"}}
	got := SuppressWithheldDispositionFixItems(items, []ReviewThread{thread}, "alice", testLooperLogin)
	if len(got) != 0 {
		t.Fatalf("got %#v, want edited directive withheld", got)
	}
}

func TestDeclineReplyCheckpointCrashKeepsStableMarker(t *testing.T) {
	t.Parallel()
	// Crash window: AddReviewThreadReply succeeds, resolve-comments checkpoint
	// is not persisted, retry rebuilds the decision fingerprint from the live
	// thread that now contains the posted decline.
	preFingerprint := "c1@2026-01-01T00:00:00Z"
	item := FixItem{Type: "comment", ID: "c1", ThreadID: "t1", Author: "alice", ThreadFingerprint: preFingerprint}
	fp := buildDeclinedThreadFingerprint(item, "abc123")
	marker := fixerDeclinedReplyMarker("t1", fp)
	github := &fakeGitHubGateway{
		currentUser: testLooperLogin,
		threads: []ReviewThread{{
			ID: "t1",
			Comments: []ReviewThreadComment{
				{ID: "c1", Author: testLooperLogin, Body: "Please fix <!-- looper:stamp v=1 -->", UpdatedAt: "2026-01-01T00:00:00Z"},
			},
		}},
	}
	runner := New(Options{GitHub: github})
	input := stepInput{Project: storage.ProjectRecord{RepoPath: t.TempDir()}, Repo: "acme/looper"}
	state, replyErr := runner.replyToDeclinedComment(context.Background(), input, item, fp, "Out of scope.", nil)
	if replyErr != "" || state != "sent" {
		t.Fatalf("first reply state=%q err=%q, want sent", state, replyErr)
	}
	if len(github.replyCalls) != 1 || !strings.Contains(github.replyCalls[0].Body, marker) {
		t.Fatalf("first reply = %#v, want marker %q", github.replyCalls, marker)
	}
	github.threads[0].Comments = append(github.threads[0].Comments, ReviewThreadComment{
		ID: "c-decline", Author: testLooperLogin, Body: github.replyCalls[0].Body, UpdatedAt: "2026-01-01T00:02:00Z",
	})
	// Retry collect-fixes rebuilds ThreadFingerprint from live nodes after the
	// gateway excludes looper-fixer-reply-declined comments.
	retryItem := item
	retryItem.ThreadFingerprint = preFingerprint
	retryFP := buildDeclinedThreadFingerprint(retryItem, "abc123")
	if retryFP != fp {
		t.Fatalf("retry fingerprint %q != original %q", retryFP, fp)
	}
	state2, replyErr2 := runner.replyToDeclinedComment(context.Background(), input, retryItem, retryFP, "Out of scope.", nil)
	if replyErr2 != "" || state2 != "sent" {
		t.Fatalf("retry state=%q err=%q, want idempotent sent", state2, replyErr2)
	}
	if len(github.replyCalls) != 1 {
		t.Fatalf("retry posted %d replies, want 1 (stable marker hit the existing decline)", len(github.replyCalls))
	}
}
