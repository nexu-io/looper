package fixer

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/forge"
	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
)

func nativeReply(fixItemID, action string) replyExplanationEntry {
	return replyExplanationEntry{FixItemID: fixItemID, Action: action, Explanation: "Reviewed the Forgejo native comment."}
}

func TestRunResolveCommentsStepForgejoNativeRequiresActualPush(t *testing.T) {
	t.Parallel()
	github := &fakeGitHubGateway{viewResponses: []PullRequestDetail{{Number: 42, State: "OPEN", HeadSHA: "new-head"}}, nativeCommentBatches: [][]NativeReviewComment{{{ProviderCommentID: 101, ObservedFingerprint: NativeReviewCommentFingerprint(101, "u1"), ResolverPresent: true, Author: "alice"}}}}
	runner := New(Options{GitHub: github})
	checkpoint := fixerCheckpoint{Detail: &checkpointDetail{State: "OPEN"}, FixItems: []FixItem{{Type: "comment", Source: NativeReviewCommentSource, ID: "101", ThreadID: "101", ProviderCommentID: 101, ObservedFingerprint: NativeReviewCommentFingerprint(101, "u1"), ResolverPresent: true}}, Validation: &ValidationResult{Passed: true, HeadSHA: "new-head"}, Push: &checkpointPush{Pushed: false}, Repair: &checkpointRepair{ReplyExplanations: []replyExplanationEntry{nativeReply("101", "fixed")}}}
	_, err := runner.runResolveCommentsStep(context.Background(), stepInput{Project: storage.ProjectRecord{RepoPath: t.TempDir()}, Repo: "acme/looper", PRNumber: 42, Checkpoint: checkpoint})
	if err == nil || !strings.Contains(err.Error(), "actual push") {
		t.Fatalf("runResolveCommentsStep() error = %v, want actual push requirement", err)
	}
}

func TestRunResolveCommentsStepForgejoNativeNoopDoesNotRequirePush(t *testing.T) {
	t.Parallel()
	github := &fakeGitHubGateway{viewResponses: []PullRequestDetail{{Number: 42, State: "OPEN", HeadSHA: "new-head"}}, nativeCommentBatches: [][]NativeReviewComment{{
		{ProviderCommentID: 101, ObservedFingerprint: NativeReviewCommentFingerprint(101, "u1"), ResolverPresent: true, Author: "alice"},
		{ProviderCommentID: 102, ObservedFingerprint: NativeReviewCommentFingerprint(102, "u2"), ResolverPresent: true, Author: "bob"},
	}}}
	runner := New(Options{GitHub: github})
	checkpoint := fixerCheckpoint{Detail: &checkpointDetail{State: "OPEN"}, FixItems: []FixItem{{Type: "comment", Source: NativeReviewCommentSource, ID: "101", ThreadID: "101", ProviderCommentID: 101, ObservedFingerprint: NativeReviewCommentFingerprint(101, "u1"), ResolverPresent: true}, {Type: "comment", Source: NativeReviewCommentSource, ID: "102", ThreadID: "102", ProviderCommentID: 102, ObservedFingerprint: NativeReviewCommentFingerprint(102, "u2"), ResolverPresent: true}}, Validation: &ValidationResult{Passed: true, HeadSHA: "new-head"}, Push: &checkpointPush{Pushed: false}, Repair: &checkpointRepair{ReplyExplanations: []replyExplanationEntry{nativeReply("101", "declined"), nativeReply("102", "deferred")}}}
	updated, err := runner.runResolveCommentsStep(context.Background(), stepInput{Project: storage.ProjectRecord{RepoPath: t.TempDir()}, Repo: "acme/looper", PRNumber: 42, Checkpoint: checkpoint})
	if err != nil {
		t.Fatalf("runResolveCommentsStep() error = %v", err)
	}
	if len(github.resolveNativeCalls) != 0 {
		t.Fatalf("resolveNativeCalls = %#v, want no native resolve calls", github.resolveNativeCalls)
	}
	statuses := map[string]string{}
	for _, item := range updated.ResolvedComments.Items {
		statuses[item.FixItemID] = item.Status
	}
	if statuses["101"] != "skipped_noop" || statuses["102"] != "skipped_noop" {
		t.Fatalf("ResolvedComments = %#v, want skipped_noop for non-fixed native comments", updated.ResolvedComments)
	}
}

func TestRunResolveCommentsStepForgejoNativeMissingDecisionRetriesDiscovery(t *testing.T) {
	t.Parallel()
	github := &fakeGitHubGateway{viewResponses: []PullRequestDetail{{Number: 42, State: "OPEN", HeadSHA: "new-head"}}, nativeCommentBatches: [][]NativeReviewComment{{
		{ProviderCommentID: 101, ObservedFingerprint: NativeReviewCommentFingerprint(101, "u1"), ResolverPresent: true, Author: "alice"},
		{ProviderCommentID: 102, ObservedFingerprint: NativeReviewCommentFingerprint(102, "u2"), ResolverPresent: true, Author: "bob"},
	}}}
	runner := New(Options{GitHub: github})
	checkpoint := fixerCheckpoint{Detail: &checkpointDetail{State: "OPEN"}, FixItems: []FixItem{{Type: "comment", Source: NativeReviewCommentSource, ID: "101", ThreadID: "101", ProviderCommentID: 101, ObservedFingerprint: NativeReviewCommentFingerprint(101, "u1"), ResolverPresent: true}}, Validation: &ValidationResult{Passed: true, HeadSHA: "new-head"}, Push: &checkpointPush{Pushed: true}, Repair: &checkpointRepair{ReplyExplanations: []replyExplanationEntry{nativeReply("101", "fixed")}}}
	updated, err := runner.runResolveCommentsStep(context.Background(), stepInput{Project: storage.ProjectRecord{RepoPath: t.TempDir()}, Loop: storage.LoopRecord{}, Repo: "acme/looper", PRNumber: 42, Checkpoint: checkpoint})
	if err == nil || !strings.Contains(err.Error(), "omitted or invalidated thread decisions") {
		t.Fatalf("runResolveCommentsStep() error = %v, want missing-decision retry", err)
	}
	statuses := map[string]string{}
	for _, item := range updated.ResolvedComments.Items {
		statuses[item.FixItemID] = item.Status
	}
	if statuses["101"] != "resolved" || statuses["102"] != "skipped_missing_agent_decision" {
		t.Fatalf("ResolvedComments = %#v, want resolved original and missing-decision new native comment", updated.ResolvedComments)
	}
	if updated.ResumePolicy != loops.ResumePolicyRestartFromDiscover {
		t.Fatalf("updated.ResumePolicy = %q, want restart_from_discover", updated.ResumePolicy)
	}
}

func TestRunResolveCommentsStepForgejoNativeResolvesFixedOnlyAndSkipsStates(t *testing.T) {
	t.Parallel()
	github := &fakeGitHubGateway{viewResponses: []PullRequestDetail{{Number: 42, State: "OPEN", HeadSHA: "new-head"}}, nativeCommentBatches: [][]NativeReviewComment{{
		{ProviderCommentID: 101, ObservedFingerprint: NativeReviewCommentFingerprint(101, "u1"), ResolverPresent: true, Author: "alice"},
		{ProviderCommentID: 102, ObservedFingerprint: NativeReviewCommentFingerprint(102, "u2"), ResolverPresent: true, IsResolved: true, Author: "bob"},
		{ProviderCommentID: 103, ObservedFingerprint: NativeReviewCommentFingerprint(103, "u3"), ResolverPresent: true, Author: "cara"},
		{ProviderCommentID: 104, ObservedFingerprint: NativeReviewCommentFingerprint(104, "u4-new"), ResolverPresent: true, Author: "dana"},
	}}}
	runner := New(Options{GitHub: github})
	checkpoint := fixerCheckpoint{Detail: &checkpointDetail{State: "OPEN"}, FixItems: []FixItem{{Type: "comment", Source: NativeReviewCommentSource, ID: "101", ThreadID: "101", ProviderCommentID: 101, ObservedFingerprint: NativeReviewCommentFingerprint(101, "u1"), ResolverPresent: true}, {Type: "comment", Source: NativeReviewCommentSource, ID: "102", ThreadID: "102", ProviderCommentID: 102, ObservedFingerprint: NativeReviewCommentFingerprint(102, "u2"), ResolverPresent: true}, {Type: "comment", Source: NativeReviewCommentSource, ID: "103", ThreadID: "103", ProviderCommentID: 103, ObservedFingerprint: NativeReviewCommentFingerprint(103, "u3"), ResolverPresent: true}, {Type: "comment", Source: NativeReviewCommentSource, ID: "104", ThreadID: "104", ProviderCommentID: 104, ObservedFingerprint: NativeReviewCommentFingerprint(104, "u4"), ResolverPresent: true}}, Validation: &ValidationResult{Passed: true, HeadSHA: "new-head"}, Push: &checkpointPush{Pushed: true}, Repair: &checkpointRepair{ReplyExplanations: []replyExplanationEntry{nativeReply("101", "fixed"), nativeReply("102", "fixed"), nativeReply("103", "declined"), nativeReply("104", "fixed")}}}
	updated, err := runner.runResolveCommentsStep(context.Background(), stepInput{Project: storage.ProjectRecord{RepoPath: t.TempDir()}, Loop: storage.LoopRecord{}, Repo: "acme/looper", PRNumber: 42, Checkpoint: checkpoint})
	if err == nil || !strings.Contains(err.Error(), "review thread content changed") {
		t.Fatalf("runResolveCommentsStep() error = %v, want native thread drift retry", err)
	}
	if len(github.resolveNativeCalls) != 1 || github.resolveNativeCalls[0].ProviderCommentID != 101 {
		t.Fatalf("resolveNativeCalls = %#v, want only fixed matching unresolved comment", github.resolveNativeCalls)
	}
	statuses := map[string]string{}
	for _, item := range updated.ResolvedComments.Items {
		statuses[item.FixItemID] = item.Status
	}
	if statuses["102"] != "already_resolved" || statuses["103"] != "skipped_noop" || statuses["104"] != "skipped_thread_drift" {
		t.Fatalf("ResolvedComments = %#v", updated.ResolvedComments)
	}
	if _, ok := statuses["101"]; !ok || statuses["101"] != "resolved" {
		t.Fatalf("ResolvedComments = %#v", updated.ResolvedComments)
	}
	if updated.ResumePolicy != loops.ResumePolicyRestartFromDiscover {
		t.Fatalf("updated.ResumePolicy = %q, want restart_from_discover", updated.ResumePolicy)
	}
}

func TestRunResolveCommentsStepForgejoNativeSkipsDeleted(t *testing.T) {
	t.Parallel()
	github := &fakeGitHubGateway{viewResponses: []PullRequestDetail{{Number: 42, State: "OPEN", HeadSHA: "new-head"}}}
	runner := New(Options{GitHub: github})
	checkpoint := fixerCheckpoint{Detail: &checkpointDetail{State: "OPEN"}, FixItems: []FixItem{{Type: "comment", Source: NativeReviewCommentSource, ID: "101", ThreadID: "101", ProviderCommentID: 101, ObservedFingerprint: NativeReviewCommentFingerprint(101, "u1"), ResolverPresent: true}}, Validation: &ValidationResult{Passed: true, HeadSHA: "new-head"}, Push: &checkpointPush{Pushed: true}, Repair: &checkpointRepair{ReplyExplanations: []replyExplanationEntry{nativeReply("101", "fixed")}}}
	updated, err := runner.runResolveCommentsStep(context.Background(), stepInput{Project: storage.ProjectRecord{RepoPath: t.TempDir()}, Repo: "acme/looper", PRNumber: 42, Checkpoint: checkpoint})
	if err != nil {
		t.Fatalf("runResolveCommentsStep() error = %v", err)
	}
	if got := updated.ResolvedComments.Items[0].Status; got != "deleted" {
		t.Fatalf("status = %q, want deleted", got)
	}
}

func TestRunResolveCommentsStepForgejoNativeClassifiesHTTPAndTimeoutErrors(t *testing.T) {
	t.Parallel()
	for name, errValue := range map[string]struct {
		err  error
		kind QueueFailureKind
		text string
	}{
		"503 retry":     {err: &forge.ForgejoHTTPError{StatusCode: 503, Method: "POST", Path: "/resolve", Message: "down"}, kind: FailureRetryableAfterResume, text: "will retry"},
		"timeout retry": {err: errors.New("request timed out"), kind: FailureRetryableAfterResume, text: "will retry"},
	} {
		t.Run(name, func(t *testing.T) {
			github := &fakeGitHubGateway{viewResponses: []PullRequestDetail{{Number: 42, State: "OPEN", HeadSHA: "new-head"}}, nativeCommentBatches: [][]NativeReviewComment{{{ProviderCommentID: 101, ObservedFingerprint: NativeReviewCommentFingerprint(101, "u1"), ResolverPresent: true, Author: "alice"}}}, resolveNativeErr: errValue.err}
			runner := New(Options{GitHub: github})
			checkpoint := fixerCheckpoint{Detail: &checkpointDetail{State: "OPEN"}, FixItems: []FixItem{{Type: "comment", Source: NativeReviewCommentSource, ID: "101", ThreadID: "101", ProviderCommentID: 101, ObservedFingerprint: NativeReviewCommentFingerprint(101, "u1"), ResolverPresent: true}}, Validation: &ValidationResult{Passed: true, HeadSHA: "new-head"}, Push: &checkpointPush{Pushed: true}, Repair: &checkpointRepair{ReplyExplanations: []replyExplanationEntry{nativeReply("101", "fixed")}}}
			_, err := runner.runResolveCommentsStep(context.Background(), stepInput{Project: storage.ProjectRecord{RepoPath: t.TempDir()}, Repo: "acme/looper", PRNumber: 42, Checkpoint: checkpoint})
			var loopErr *loopError
			if !errors.As(err, &loopErr) || loopErr.kind != errValue.kind || !strings.Contains(loopErr.Error(), errValue.text) {
				t.Fatalf("error = %#v, want kind=%s text containing %q", err, errValue.kind, errValue.text)
			}
		})
	}
}

func TestRunResolveCommentsStepForgejoNativeUnsupportedResolveRequiresManualIntervention(t *testing.T) {
	t.Parallel()
	for _, errValue := range []error{
		&forge.ForgejoHTTPError{StatusCode: 404, Method: "POST", Path: "/resolve", Message: "missing"},
		&forge.ForgejoHTTPError{StatusCode: 405, Method: "POST", Path: "/resolve", Message: "method"},
	} {
		github := &fakeGitHubGateway{viewResponses: []PullRequestDetail{{Number: 42, State: "OPEN", HeadSHA: "new-head"}}, nativeCommentBatches: [][]NativeReviewComment{{{ProviderCommentID: 101, ObservedFingerprint: NativeReviewCommentFingerprint(101, "u1"), ResolverPresent: true, Author: "alice"}}}, resolveNativeErr: errValue}
		runner := New(Options{GitHub: github})
		checkpoint := fixerCheckpoint{Detail: &checkpointDetail{State: "OPEN"}, FixItems: []FixItem{{Type: "comment", Source: NativeReviewCommentSource, ID: "101", ThreadID: "101", ProviderCommentID: 101, ObservedFingerprint: NativeReviewCommentFingerprint(101, "u1"), ResolverPresent: true}}, Validation: &ValidationResult{Passed: true, HeadSHA: "new-head"}, Push: &checkpointPush{Pushed: true}, Repair: &checkpointRepair{ReplyExplanations: []replyExplanationEntry{nativeReply("101", "fixed")}}}
		updated, err := runner.runResolveCommentsStep(context.Background(), stepInput{Project: storage.ProjectRecord{RepoPath: t.TempDir()}, Repo: "acme/looper", PRNumber: 42, Checkpoint: checkpoint})
		var loopErr *loopError
		if !errors.As(err, &loopErr) || loopErr.kind != FailureManualIntervention || !strings.Contains(loopErr.Error(), "requires manual intervention") {
			t.Fatalf("runResolveCommentsStep() error = %#v, want manual intervention", err)
		}
		if updated.ResolvedComments == nil || len(updated.ResolvedComments.Items) != 1 || updated.ResolvedComments.Items[0].Status != "unsupported_remote_resolution" {
			t.Fatalf("resolved comments = %#v, want unsupported_remote_resolution", updated.ResolvedComments)
		}
	}
}

func TestParseNativeRepairResultsAcceptsExplicitActionsAndDefaultsMissingToDeferred(t *testing.T) {
	t.Parallel()
	fixItems := []FixItem{
		{Type: "comment", ID: "c1", ThreadID: "101", Source: NativeReviewCommentSource, ProviderCommentID: 101, ObservedFingerprint: NativeReviewCommentFingerprint(101, "updated-1")},
		{Type: "comment", ID: "c2", ThreadID: "102", Source: NativeReviewCommentSource, ProviderCommentID: 102, ObservedFingerprint: NativeReviewCommentFingerprint(102, "updated-2")},
		{Type: "comment", ID: "c3", ThreadID: "103", Source: NativeReviewCommentSource, ProviderCommentID: 103, ObservedFingerprint: NativeReviewCommentFingerprint(103, "updated-3")},
		{Type: "comment", ID: "c4", ThreadID: "104", Source: NativeReviewCommentSource, ProviderCommentID: 104, ObservedFingerprint: NativeReviewCommentFingerprint(104, "updated-4")},
	}
	stdout := `__LOOPER_RESULT__={"repair_results":[` +
		`{"source":"forgejo_review_comment","providerCommentId":101,"action":"fixed","explanation":"Renamed the helper.","observedFingerprint":"` + NativeReviewCommentFingerprint(101, "updated-1") + `"},` +
		`{"source":"forgejo_review_comment","providerCommentId":102,"action":"declined","explanation":"Out of scope.","observedFingerprint":"` + NativeReviewCommentFingerprint(102, "updated-2") + `"},` +
		`{"source":"forgejo_review_comment","providerCommentId":103,"action":"deferred","explanation":"Need product input.","observedFingerprint":"` + NativeReviewCommentFingerprint(103, "updated-3") + `"}` +
		`]}`
	got := parseNativeRepairResults(stdout, "", fixItems)
	if len(got) != 4 {
		t.Fatalf("len(parseNativeRepairResults()) = %d, want 4", len(got))
	}
	if got[0].Action != "fixed" || got[1].Action != "declined" || got[2].Action != "deferred" {
		t.Fatalf("parseNativeRepairResults() actions = %#v", got)
	}
	if got[3].Action != "deferred" || got[3].FixItemID != "c4" || got[3].Explanation == "" {
		t.Fatalf("parseNativeRepairResults() missing result = %#v, want deferred fallback", got[3])
	}
}

func TestParseNativeRepairResultsMissingObservedFingerprintFallsBackToDeferred(t *testing.T) {
	t.Parallel()
	fixItems := []FixItem{{Type: "comment", ID: "c1", ThreadID: "101", Source: NativeReviewCommentSource, ProviderCommentID: 101, ObservedFingerprint: NativeReviewCommentFingerprint(101, "updated-1")}}
	stdout := `__LOOPER_RESULT__={"repair_results":[{"source":"forgejo_review_comment","providerCommentId":101,"action":"fixed","explanation":"Renamed the helper."}]}`
	got := parseNativeRepairResults(stdout, "", fixItems)
	if len(got) != 1 || got[0].Action != "deferred" || got[0].FixItemID != "c1" {
		t.Fatalf("parseNativeRepairResults() = %#v, want deferred fallback", got)
	}
}

func TestParseNativeRepairResultsBlankExplanationFallsBackToDeferred(t *testing.T) {
	t.Parallel()
	fixItems := []FixItem{{Type: "comment", ID: "c1", ThreadID: "101", Source: NativeReviewCommentSource, ProviderCommentID: 101, ObservedFingerprint: NativeReviewCommentFingerprint(101, "updated-1")}}
	stdout := `__LOOPER_RESULT__={"repair_results":[{"source":"forgejo_review_comment","providerCommentId":101,"action":"fixed","explanation":"   ","observedFingerprint":"` + NativeReviewCommentFingerprint(101, "updated-1") + `"}]}`
	got := parseNativeRepairResults(stdout, "", fixItems)
	if len(got) != 1 || got[0].Action != "deferred" || got[0].FixItemID != "c1" {
		t.Fatalf("parseNativeRepairResults() = %#v, want deferred fallback", got)
	}
}

func TestParseNativeRepairResultsIgnoresUnknownExplicitEntriesAndFallsBackToDeferred(t *testing.T) {
	t.Parallel()
	fixItems := []FixItem{{Type: "comment", ID: "c1", ThreadID: "101", Source: NativeReviewCommentSource, ProviderCommentID: 101, ObservedFingerprint: NativeReviewCommentFingerprint(101, "updated-1")}}
	stdout := `__LOOPER_RESULT__={"repair_results":[` +
		`{"source":"github_review_comment","providerCommentId":101,"action":"fixed","explanation":"wrong source","observedFingerprint":"` + NativeReviewCommentFingerprint(101, "updated-1") + `"},` +
		`{"source":"forgejo_review_comment","providerCommentId":999,"action":"fixed","explanation":"unknown provider","observedFingerprint":"ignored"}` +
		`]}`
	got := parseNativeRepairResults(stdout, "", fixItems)
	if len(got) != 1 || got[0].Action != "deferred" || got[0].FixItemID != "c1" {
		t.Fatalf("parseNativeRepairResults() = %#v, want deferred fallback for known item", got)
	}
}

func TestParseNativeRepairResultsKeepsFirstValidDuplicate(t *testing.T) {
	t.Parallel()
	fixItems := []FixItem{{Type: "comment", ID: "c1", ThreadID: "101", Source: NativeReviewCommentSource, ProviderCommentID: 101, ObservedFingerprint: NativeReviewCommentFingerprint(101, "updated-1")}}
	stdout := `__LOOPER_RESULT__={"repair_results":[` +
		`{"source":"forgejo_review_comment","providerCommentId":101,"action":"fixed","explanation":"   ","observedFingerprint":"` + NativeReviewCommentFingerprint(101, "updated-1") + `"},` +
		`{"source":"forgejo_review_comment","providerCommentId":101,"action":"declined","explanation":"First valid result.","observedFingerprint":"` + NativeReviewCommentFingerprint(101, "updated-1") + `"},` +
		`{"source":"forgejo_review_comment","providerCommentId":101,"action":"fixed","explanation":"Later duplicate should be ignored.","observedFingerprint":"` + NativeReviewCommentFingerprint(101, "updated-1") + `"}` +
		`]}`
	got := parseNativeRepairResults(stdout, "", fixItems)
	if len(got) != 1 || got[0].Action != "declined" || got[0].Explanation != "First valid result." {
		t.Fatalf("parseNativeRepairResults() = %#v, want first valid duplicate kept", got)
	}
}
