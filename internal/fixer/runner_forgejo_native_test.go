package fixer

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/forge"
	"github.com/nexu-io/looper/internal/storage"
)

func TestBuildFixerPromptUsesForgejoSeedAndFetchContract(t *testing.T) {
	t.Parallel()

	detail := &checkpointDetail{
		State:       "OPEN",
		HeadSHA:     "abc123",
		BaseRefName: "main",
		HeadRefName: "feature/fix",
		IssueComments: []map[string]any{{
			"url": "https://code.forgejo.example/acme/looper/pulls/42#issuecomment-202",
		}},
	}
	prompt, _ := buildFixerPrompt("project_1", customInstructionConfig(nil), "acme/looper", 42, detail, []FixItem{{ID: "fix-1", URL: "https://code.forgejo.example/acme/looper/pulls/42/files#diff-1"}}, false, config.DefaultDisclosureConfig(), "opencode", "openai/gpt-5.5")
	for _, want := range []string{
		"\"url\": \"https://code.forgejo.example/acme/looper/pulls/42\"",
		"Agent-side Forgejo fetch contract",
		"GET /api/v1/repos/{owner}/{repo}/pulls/{number}",
		"GET /api/v1/repos/{owner}/{repo}/pulls/{number}.diff",
		"GET /api/v1/repos/{owner}/{repo}/pulls/{number}/reviews/{review_id}/comments",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
	for _, unwanted := range []string{
		"gh pr view <pr-url>",
		"gh pr diff <pr-url>",
		"gh pr checks <pr-url>",
	} {
		if strings.Contains(prompt, unwanted) {
			t.Fatalf("prompt contains GitHub-specific instruction %q:\n%s", unwanted, prompt)
		}
	}
}

func TestBuildFixerPromptAddsForgejoNativeCommentRepairResultsInstruction(t *testing.T) {
	t.Parallel()

	detail := &checkpointDetail{State: "OPEN", HeadSHA: "abc123", BaseRefName: "main", HeadRefName: "feature/fix"}
	prompt, _ := buildFixerPrompt("project_1", customInstructionConfig(nil), "acme/looper", 42, detail, []FixItem{{Type: "comment", ID: "c1", ThreadID: "101", Source: NativeReviewCommentSource, ProviderCommentID: 101, ObservedFingerprint: NativeReviewCommentFingerprint(101, "updated-1"), Summary: "rename helper", Body: "Please rename this helper.", Path: "internal/fixer/runner.go", DiffHunk: "@@ -1,2 +1,2 @@"}}, false, config.DefaultDisclosureConfig(), "opencode", "openai/gpt-5.5")
	for _, want := range []string{
		"Forgejo native review comment fix item",
		"`repair_results`",
		"`providerCommentId`",
		"`observedFingerprint`",
		"individual review comment is fixed, declined, or deferred",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
	if !strings.Contains(prompt, `"providerCommentId":101`) || !strings.Contains(prompt, NativeReviewCommentFingerprint(101, "updated-1")) {
		t.Fatalf("prompt missing native item identifiers/fingerprint:\n%s", prompt)
	}
	for _, unwanted := range []string{"review_thread_replies", "threadId", "threadCommentsObserved"} {
		if strings.Contains(prompt, unwanted) {
			t.Fatalf("prompt unexpectedly contains %q:\n%s", unwanted, prompt)
		}
	}
}

func TestBuildFixerPromptGitHubRegressionRemainsUnchangedForReviewThreadReplies(t *testing.T) {
	t.Parallel()

	detail := &checkpointDetail{State: "OPEN", HeadSHA: "abc123", BaseRefName: "main", HeadRefName: "feature/fix"}
	prompt, _ := buildFixerPrompt("project_1", customInstructionConfig(nil), "acme/looper", 42, detail, []FixItem{{Type: "comment", ID: "c1", ThreadID: "thread-1", Summary: "repair disclosure"}}, false, config.DefaultDisclosureConfig(), "opencode", "openai/gpt-5.5")
	if strings.Contains(prompt, "`repair_results`") {
		t.Fatalf("prompt unexpectedly contains Forgejo native contract:\n%s", prompt)
	}
	if !strings.Contains(prompt, "review_thread_replies") || !strings.Contains(prompt, `  - "threadId": the exact "threadId" of the same fix item`) {
		t.Fatalf("prompt missing GitHub review thread contract:\n%s", prompt)
	}
}

func TestCollectFixItemsFromForgejoNativeReviewCommentPreservesNativeFields(t *testing.T) {
	t.Parallel()

	items := collectFixItems(PullRequestDetail{Comments: []map[string]any{{
		"id":                  "101",
		"databaseId":          int64(101),
		"threadId":            "101",
		"threadFingerprint":   NativeReviewCommentFingerprint(101, "updated-1"),
		"observedFingerprint": NativeReviewCommentFingerprint(101, "updated-1"),
		"source":              NativeReviewCommentSource,
		"body":                "Please rename this helper.",
		"url":                 "https://forgejo.test/acme/looper/pulls/42#discussion_r101",
		"path":                "internal/fixer/runner.go",
		"diffHunk":            "@@ -1,2 +1,2 @@",
	}}})

	if len(items) != 1 {
		t.Fatalf("len(items) = %d, want 1", len(items))
	}
	if got := items[0]; got.Source != NativeReviewCommentSource || got.ID != NativeReviewCommentFixItemID(101) || got.ThreadID != NativeReviewCommentThreadID(101) || got.ProviderCommentID != 101 || got.ObservedFingerprint != NativeReviewCommentFingerprint(101, "updated-1") || got.Body != "Please rename this helper." || got.DiffHunk != "@@ -1,2 +1,2 @@" || got.URL == "" {
		t.Fatalf("item = %#v, want forgejo native review comment fields", got)
	}
}

func TestCollectFixItemsPreservesGitHubCommentBehaviorWithExtendedModel(t *testing.T) {
	t.Parallel()

	items := collectFixItems(PullRequestDetail{Comments: []map[string]any{{"id": "c1", "threadId": "t1", "threadFingerprint": "legacy:t1:c1", "body": "please fix", "url": "https://github.test/thread"}}})
	if len(items) != 1 {
		t.Fatalf("len(items) = %d, want 1", len(items))
	}
	if got := items[0]; got.ID != "c1" || got.ThreadID != "t1" || got.ThreadFingerprint != "legacy:t1:c1" || got.ProviderCommentID != 0 || got.Source != "" || got.Body != "" {
		t.Fatalf("item = %#v, want GitHub thread fields preserved", got)
	}
}

func TestCollectFixItemsAllowsForgejoNativeAndSummaryItemsToCoexist(t *testing.T) {
	t.Parallel()

	summary := forge.NewReviewerSummary(3, []forge.ReviewItem{{ReviewItemID: "101", Status: forge.ReviewItemStatusOpen, Title: "Fix parsing", Body: "Parser must fail fast.", LastSeenRoundID: 3}})
	marker, err := forge.RenderReviewerSummary(summary)
	if err != nil {
		t.Fatalf("RenderReviewerSummary() error = %v", err)
	}

	items, err := collectFixItemsFromCheckpointForStep(fixerCheckpoint{Detail: &checkpointDetail{
		Comments: []map[string]any{{
			"id":                  NativeReviewCommentFixItemID(101),
			"databaseId":          int64(101),
			"threadId":            "101",
			"threadFingerprint":   NativeReviewCommentFingerprint(101, "updated-1"),
			"observedFingerprint": NativeReviewCommentFingerprint(101, "updated-1"),
			"source":              NativeReviewCommentSource,
			"body":                "Please rename this helper.",
			"url":                 "https://forgejo.test/acme/looper/pulls/42#discussion_r101",
			"path":                "internal/fixer/runner.go",
			"diffHunk":            "@@ -1,2 +1,2 @@",
		}},
		IssueComments: []map[string]any{{"id": int64(101), "body": "visible\n" + marker, "url": "https://forgejo.test/comment/101"}},
	}})
	if err != nil {
		t.Fatalf("collectFixItemsFromCheckpointForStep() error = %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("items = %#v, want native + summary items", items)
	}
	if items[0].Source != NativeReviewCommentSource || items[1].Source != "forgejo-reviewer-summary" {
		t.Fatalf("items = %#v, want native item before reviewer summary item", items)
	}
	if items[0].ID != NativeReviewCommentFixItemID(101) || items[1].ID != "101" {
		t.Fatalf("items = %#v, want source-scoped native ID and unchanged summary ID", items)
	}
	if items[0].ThreadID != NativeReviewCommentThreadID(101) || items[1].ThreadID != "101" {
		t.Fatalf("items = %#v, want source-scoped native thread ID and unchanged summary thread ID", items)
	}
}

func TestRunCollectFixesStepIncludesManualForgejoNativeCommentsAndFiltersLooperAuthored(t *testing.T) {
	t.Parallel()
	github := &fakeGitHubGateway{currentUser: "looper", nativeComments: []NativeReviewComment{{ProviderCommentID: 101, Body: "fix this", Author: "alice", ObservedFingerprint: NativeReviewCommentFingerprint(101, "u1"), ResolverPresent: true}, {ProviderCommentID: 102, Body: "my own comment", Author: "looper", ObservedFingerprint: NativeReviewCommentFingerprint(102, "u2"), ResolverPresent: true}}}
	runner := New(Options{GitHub: github})
	checkpoint, err := runner.runCollectFixesStep(context.Background(), stepInput{Project: storage.ProjectRecord{ID: "project_1", RepoPath: t.TempDir()}, Loop: storage.LoopRecord{MetadataJSON: stringPtr(`{"manual":true}`)}, Repo: "acme/looper", PRNumber: 42, Checkpoint: fixerCheckpoint{Detail: &checkpointDetail{State: "OPEN"}}})
	if err != nil {
		t.Fatalf("runCollectFixesStep() error = %v", err)
	}
	if len(checkpoint.FixItems) != 1 || checkpoint.FixItems[0].ProviderCommentID != 101 {
		t.Fatalf("FixItems = %#v, want only non-Looper native comment", checkpoint.FixItems)
	}
}

func TestRunCollectFixesStepFailsUnsupportedWhenNativeResolverMissing(t *testing.T) {
	t.Parallel()
	github := &fakeGitHubGateway{currentUser: "looper", nativeComments: []NativeReviewComment{{ProviderCommentID: 101, Body: "fix this", Author: "alice", ObservedFingerprint: NativeReviewCommentFingerprint(101, "u1"), ResolverPresent: false}}}
	runner := New(Options{GitHub: github})
	_, err := runner.runCollectFixesStep(context.Background(), stepInput{Project: storage.ProjectRecord{ID: "project_1", RepoPath: t.TempDir()}, Loop: storage.LoopRecord{MetadataJSON: stringPtr(`{"manual":true}`)}, Repo: "acme/looper", PRNumber: 42, Checkpoint: fixerCheckpoint{Detail: &checkpointDetail{State: "OPEN"}}})
	if err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("runCollectFixesStep() error = %v, want unsupported manual intervention", err)
	}
}

func TestRunCollectFixesStepUsesStepContextForManualForgejoNativeComments(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	github := &fakeGitHubGateway{currentUser: "looper"}
	runner := New(Options{GitHub: github})
	_, err := runner.runCollectFixesStep(ctx, stepInput{Project: storage.ProjectRecord{ID: "project_1", RepoPath: t.TempDir()}, Loop: storage.LoopRecord{MetadataJSON: stringPtr(`{"manual":true}`)}, Repo: "acme/looper", PRNumber: 42, Checkpoint: fixerCheckpoint{Detail: &checkpointDetail{State: "OPEN"}}})
	if err != nil {
		t.Fatalf("runCollectFixesStep() error = %v", err)
	}
	if !errors.Is(github.listNativeContextErr, context.Canceled) {
		t.Fatalf("ListNativeReviewComments context error = %v, want context.Canceled", github.listNativeContextErr)
	}
}

func TestRunCollectFixesStepClassifiesUnsupportedNativeDiscovery(t *testing.T) {
	t.Parallel()
	for _, status := range []int{404, 405} {
		github := &fakeGitHubGateway{
			currentUser:   "looper",
			listNativeErr: &forge.ForgejoHTTPError{StatusCode: status, Method: "GET", Path: "/pulls/42/reviews", Message: "missing endpoint"},
		}
		runner := New(Options{GitHub: github})
		_, err := runner.runCollectFixesStep(context.Background(), stepInput{Project: storage.ProjectRecord{ID: "project_1", RepoPath: t.TempDir()}, Loop: storage.LoopRecord{MetadataJSON: stringPtr(`{"manual":true}`)}, Repo: "acme/looper", PRNumber: 42, Checkpoint: fixerCheckpoint{Detail: &checkpointDetail{State: "OPEN"}}})
		var loopErr *loopError
		if !errors.As(err, &loopErr) || loopErr.kind != FailureManualIntervention || !strings.Contains(loopErr.Error(), "discovery is unsupported") {
			t.Fatalf("runCollectFixesStep() error = %#v, want unsupported discovery manual intervention for status %d", err, status)
		}
	}
}
