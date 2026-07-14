package runtime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/agent"
	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/disclosure"
	"github.com/nexu-io/looper/internal/forge"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/reviewer"
)

func TestFindForgejoNativeReviewMarkerEnforcesOutcomeSpecificEvents(t *testing.T) {
	t.Parallel()

	markerBody := func(outcome string) string {
		return "review body\n<!-- looper:review id=reviewer:loop:head head=head outcome=" + outcome + " -->"
	}
	reviews := []forge.PullRequestReview{
		{ID: 1, State: "COMMENTED", Body: markerBody("clean"), User: forge.Identity{Login: "reviewer"}},
		{ID: 2, State: "COMMENTED", Body: markerBody("blocking"), User: forge.Identity{Login: "reviewer"}},
		{ID: 3, State: "APPROVED", Body: markerBody("clean"), User: forge.Identity{Login: "reviewer"}},
		{ID: 4, State: "CHANGES_REQUESTED", Body: markerBody("blocking"), User: forge.Identity{Login: "reviewer"}},
		{ID: 5, State: "COMMENTED", Body: markerBody("non_blocking"), User: forge.Identity{Login: "reviewer"}},
	}
	// Policy-derived allowed set always includes COMMENT for non-blocking outcomes.
	allowed := []reviewer.ReviewEvent{reviewer.ReviewEventComment, reviewer.ReviewEventApprove, reviewer.ReviewEventRequestChanges}

	clean := findForgejoNativeReviewMarker(reviews, reviewer.VerifyReviewMarkerInput{
		Marker: "looper:review id=reviewer:loop:head head=head outcome=clean", AllowedReviewEvents: allowed, AuthorLogin: "reviewer",
	})
	if !clean.Found || clean.Event != reviewer.ReviewEventApprove || clean.Outcome != "clean" {
		t.Fatalf("clean marker = %#v, want APPROVED outcome=clean (not COMMENTED)", clean)
	}

	blocking := findForgejoNativeReviewMarker(reviews, reviewer.VerifyReviewMarkerInput{
		Marker: "looper:review id=reviewer:loop:head head=head outcome=blocking", AllowedReviewEvents: allowed, AuthorLogin: "reviewer",
	})
	if !blocking.Found || blocking.Event != reviewer.ReviewEventRequestChanges || blocking.Outcome != "blocking" {
		t.Fatalf("blocking marker = %#v, want CHANGES_REQUESTED outcome=blocking (not COMMENTED)", blocking)
	}

	nonBlocking := findForgejoNativeReviewMarker(reviews, reviewer.VerifyReviewMarkerInput{
		Marker: "looper:review id=reviewer:loop:head head=head outcome=non_blocking", AllowedReviewEvents: allowed, AuthorLogin: "reviewer",
	})
	if !nonBlocking.Found || nonBlocking.Event != reviewer.ReviewEventComment || nonBlocking.Outcome != "non_blocking" {
		t.Fatalf("non_blocking marker = %#v, want COMMENTED outcome=non_blocking", nonBlocking)
	}

	// Self-approval fallback: clean on COMMENT is accepted only when AllowCleanComment is set.
	cleanCommentOnly := []forge.PullRequestReview{{ID: 10, State: "COMMENTED", Body: markerBody("clean"), User: forge.Identity{Login: "reviewer"}}}
	rejected := findForgejoNativeReviewMarker(cleanCommentOnly, reviewer.VerifyReviewMarkerInput{
		Marker: "looper:review id=reviewer:loop:head head=head outcome=clean", AllowedReviewEvents: allowed, AuthorLogin: "reviewer",
	})
	if rejected.Found {
		t.Fatalf("clean COMMENT without AllowCleanComment = %#v, want not found", rejected)
	}
	accepted := findForgejoNativeReviewMarker(cleanCommentOnly, reviewer.VerifyReviewMarkerInput{
		Marker: "looper:review id=reviewer:loop:head head=head outcome=clean", AllowedReviewEvents: allowed, AuthorLogin: "reviewer", AllowCleanComment: true,
	})
	if !accepted.Found || accepted.Event != reviewer.ReviewEventComment {
		t.Fatalf("clean COMMENT with AllowCleanComment = %#v, want found COMMENT", accepted)
	}
}

func TestReviewerForgejoAdapterNativeDiscoveryContextPublishAndRetry(t *testing.T) {
	t.Setenv("FORGEJO_TOKEN", "secret")
	marker := "<!-- looper:review id=reviewer:loop:head-42 head=head-42 outcome=blocking -->"
	reviews := []map[string]any{{"id": 8, "state": "APPROVED", "body": "prior", "commit_id": "old-head", "user": map[string]any{"login": "human"}}}
	publishCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/swagger.v1.json":
			_, _ = w.Write([]byte(`{"paths":{"/repos/{owner}/{repo}/pulls/{index}/requested_reviewers":{"post":{}},"/repos/{owner}/{repo}/pulls/{index}/reviews":{"get":{},"post":{}},"/repos/{owner}/{repo}/pulls/{index}/reviews/{id}/comments":{"get":{}}}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/user":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 7, "login": "reviewer"})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/acme/looper/pulls":
			_ = json.NewEncoder(w).Encode([]map[string]any{{"number": 42, "title": "Review me", "state": "open", "head": map[string]any{"ref": "feature", "sha": "head-42"}, "base": map[string]any{"ref": "main", "sha": "base"}, "user": map[string]any{"login": "alice"}, "requested_reviewers": []map[string]any{{"id": 7, "login": "reviewer"}}}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/acme/looper/pulls/42":
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 42, "title": "Review me", "state": "open", "head": map[string]any{"ref": "feature", "sha": "head-42"}, "base": map[string]any{"ref": "main", "sha": "base"}, "user": map[string]any{"login": "alice"}, "requested_reviewers": []map[string]any{{"id": 7, "login": "reviewer"}}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/acme/looper/pulls/42/reviews":
			_ = json.NewEncoder(w).Encode(reviews)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/reviews/") && strings.HasSuffix(r.URL.Path, "/comments"):
			_ = json.NewEncoder(w).Encode([]any{})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/repos/acme/looper/pulls/42/reviews":
			publishCalls++
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode review: %v", err)
			}
			review := map[string]any{"id": 9, "state": payload["event"], "body": payload["body"], "commit_id": payload["commit_id"], "user": map[string]any{"login": "reviewer"}}
			reviews = append(reviews, review)
			_ = json.NewEncoder(w).Encode(review)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/acme/looper/pulls/42.diff":
			_, _ = w.Write([]byte("diff --git a/a.go b/a.go\n@@ -1 +1 @@\n-old\n+new\n"))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/acme/looper/issues/42/comments":
			_ = json.NewEncoder(w).Encode([]any{})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	repoPath := filepath.Join(t.TempDir(), "repo")
	cfg := config.Config{
		Roles:     config.RoleConfigs{Reviewer: config.ReviewerRoleConfig{Behavior: config.ReviewerConfig{PublishMode: config.ReviewerPublishModeSingleReview}}},
		Providers: []config.ProviderConfig{{ID: "forgejo-main", Kind: config.ProviderKindForgejo, BaseURL: server.URL, TokenEnv: stringPtr("FORGEJO_TOKEN")}},
		Projects:  []config.ProjectRefConfig{{ID: "project_1", Provider: "forgejo-main", Repo: "acme/looper", RepoPath: repoPath}},
	}
	adapter := reviewerGitHubAdapter{stamper: disclosure.FromConfig(cfg), config: &cfg}
	ctx := context.Background()
	prs, err := adapter.ListReviewRequestedPullRequests(ctx, reviewer.ListReviewRequestedPullRequestsInput{Repo: "acme/looper", Reviewer: "reviewer", CWD: repoPath})
	if err != nil || len(prs) != 1 || len(prs[0].ReviewRequests) != 1 {
		t.Fatalf("native discovery = %#v, %v", prs, err)
	}
	detail, err := adapter.ViewPullRequest(ctx, reviewer.ViewPullRequestInput{Repo: "acme/looper", PRNumber: 42, CWD: repoPath})
	if err != nil || detail.ReviewDecision != "APPROVED" || len(detail.Reviews) != 1 {
		t.Fatalf("native context = %#v, %v", detail, err)
	}
	verify := reviewer.VerifyReviewMarkerInput{Repo: "acme/looper", PRNumber: 42, Marker: "looper:review id=reviewer:loop:head-42 head=head-42", AllowedReviewEvents: []reviewer.ReviewEvent{reviewer.ReviewEventRequestChanges}, AuthorLogin: "reviewer", CWD: repoPath}
	found, err := adapter.FindReviewMarker(ctx, verify)
	if err != nil || found.Found {
		t.Fatalf("marker before publish = %#v, %v", found, err)
	}
	if err := adapter.SubmitReview(ctx, githubinfra.SubmitReviewInput{Repo: "acme/looper", PRNumber: 42, Event: "REQUEST_CHANGES", Body: "Blocking issue\n\n" + marker, CommitID: "head-42", CWD: repoPath}); err != nil {
		t.Fatalf("SubmitReview() error = %v", err)
	}
	found, err = adapter.FindReviewMarker(ctx, verify)
	if err != nil || !found.Found || found.Event != reviewer.ReviewEventRequestChanges {
		t.Fatalf("marker after publish = %#v, %v", found, err)
	}
	// The runner's retry contract checks the marker first; a retry therefore
	// reuses the native review instead of publishing a duplicate.
	if !found.Found {
		_ = adapter.SubmitReview(ctx, githubinfra.SubmitReviewInput{Repo: "acme/looper", PRNumber: 42, Event: "REQUEST_CHANGES", Body: marker, CommitID: "head-42", CWD: repoPath})
	}
	if publishCalls != 1 {
		t.Fatalf("publish calls = %d, want 1", publishCalls)
	}
}

func TestListReviewRequestedPullRequestsSummaryCommentToleratesMissingNativeReviews(t *testing.T) {
	// Instances may advertise requested reviewers without native review history.
	// summary_comment mode must still discover and enqueue those PRs.
	t.Setenv("FORGEJO_TOKEN", "secret")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/swagger.v1.json":
			// Advertise review-request capability only; omit native reviews GET/POST.
			_, _ = w.Write([]byte(`{"paths":{"/repos/{owner}/{repo}/pulls/{index}/requested_reviewers":{"post":{}}}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/acme/looper/pulls":
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"number": 42, "title": "Review me", "state": "open",
				"head":                map[string]any{"ref": "feature", "sha": "head-42"},
				"base":                map[string]any{"ref": "main", "sha": "base"},
				"user":                map[string]any{"login": "alice"},
				"requested_reviewers": []map[string]any{{"id": 7, "login": "reviewer"}},
			}})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	repoPath := filepath.Join(t.TempDir(), "repo")
	cfg := config.Config{
		Roles:     config.RoleConfigs{Reviewer: config.ReviewerRoleConfig{Behavior: config.ReviewerConfig{PublishMode: config.ReviewerPublishModeSummaryComment}}},
		Providers: []config.ProviderConfig{{ID: "forgejo-main", Kind: config.ProviderKindForgejo, BaseURL: server.URL, TokenEnv: stringPtr("FORGEJO_TOKEN")}},
		Projects:  []config.ProjectRefConfig{{ID: "project_1", Provider: "forgejo-main", Repo: "acme/looper", RepoPath: repoPath}},
	}
	adapter := reviewerGitHubAdapter{stamper: disclosure.FromConfig(cfg), config: &cfg}

	prs, err := adapter.ListReviewRequestedPullRequests(context.Background(), reviewer.ListReviewRequestedPullRequestsInput{Repo: "acme/looper", Reviewer: "reviewer", CWD: repoPath})
	if err != nil {
		t.Fatalf("ListReviewRequestedPullRequests() error = %v, want summary_comment compatibility fallback", err)
	}
	if len(prs) != 1 || prs[0].Number != 42 || len(prs[0].ReviewRequests) != 1 {
		t.Fatalf("ListReviewRequestedPullRequests() = %#v, want discovered PR with review request", prs)
	}
	if len(prs[0].Reviews) != 0 || prs[0].ReviewDecision != "" {
		t.Fatalf("review context = reviews=%#v decision=%q, want empty under summary_comment fallback", prs[0].Reviews, prs[0].ReviewDecision)
	}
}

func TestReviewerTrustedReviewEnvIsScopedToReviewer(t *testing.T) {
	t.Parallel()

	if got := reviewerTrustedReviewEnv(""); got != nil {
		t.Fatalf("reviewerTrustedReviewEnv(\"\") = %#v, want nil", got)
	}
	if got := reviewerTrustedReviewEnv("   "); got != nil {
		t.Fatalf("reviewerTrustedReviewEnv(whitespace) = %#v, want nil", got)
	}

	sock := "/tmp/looper-trusted-review.sock"
	got := reviewerTrustedReviewEnv("  " + sock + "  ")
	if len(got) != 1 || got[forge.TrustedReviewSockEnv] != sock {
		t.Fatalf("reviewerTrustedReviewEnv(%q) = %#v, want only %s=%q", sock, got, forge.TrustedReviewSockEnv, sock)
	}
}

func TestReviewerAllowedPRRef(t *testing.T) {
	t.Parallel()
	if got := reviewerAllowedPRRef(nil); got != "" {
		t.Fatalf("nil metadata = %q, want empty", got)
	}
	if got := reviewerAllowedPRRef(map[string]any{"repo": "acme/looper", "prNumber": int64(42)}); got != "acme/looper#42" {
		t.Fatalf("int64 pr = %q, want acme/looper#42", got)
	}
	if got := reviewerAllowedPRRef(map[string]any{"repo": "acme/looper", "prNumber": float64(7)}); got != "acme/looper#7" {
		t.Fatalf("float64 pr = %q, want acme/looper#7", got)
	}
	if got := reviewerAllowedPRRef(map[string]any{"repo": "acme/looper", "prNumber": 0}); got != "" {
		t.Fatalf("zero pr = %q, want empty", got)
	}
	if got := reviewerAllowedPRRef(map[string]any{"prNumber": int64(1)}); got != "" {
		t.Fatalf("missing repo = %q, want empty", got)
	}
}

func TestReviewerAllowedReviewPolicy(t *testing.T) {
	t.Parallel()
	if got := reviewerAllowedReviewPolicy(nil); got.Clean != "" || got.Blocking != "" {
		t.Fatalf("nil metadata = %#v, want empty", got)
	}
	got := reviewerAllowedReviewPolicy(map[string]any{
		"cleanReviewEvent":    " APPROVE ",
		"blockingReviewEvent": "REQUEST_CHANGES",
	})
	if got.Clean != "APPROVE" || got.Blocking != "REQUEST_CHANGES" {
		t.Fatalf("reviewerAllowedReviewPolicy() = %#v, want APPROVE/REQUEST_CHANGES", got)
	}
}

func TestReviewerAllowsTrustedReviewProxy(t *testing.T) {
	t.Parallel()
	if reviewerAllowsTrustedReviewProxy(nil, "demo", nil) {
		t.Fatal("nil metadata allowed, want false")
	}
	if reviewerAllowsTrustedReviewProxy(nil, "demo", map[string]any{"phase": "thread_resolution"}) {
		t.Fatal("thread_resolution allowed, want false")
	}
	if reviewerAllowsTrustedReviewProxy(nil, "demo", map[string]any{"phase": ""}) {
		t.Fatal("empty phase allowed, want false")
	}
	// Without a Forgejo project config, review/publish phases must not mint a socket
	// (mixed installs must keep GitHub on the normal review-submit path).
	if reviewerAllowsTrustedReviewProxy(nil, "demo", map[string]any{"phase": "review"}) {
		t.Fatal("nil config review phase allowed, want false")
	}
	if reviewerAllowsTrustedReviewProxy(nil, "demo", map[string]any{"phase": "publish"}) {
		t.Fatal("nil config publish phase allowed, want false")
	}

	// summary_comment mode must never mint a review-submit socket even in review phase.
	summaryCfg := &config.Config{
		Providers: []config.ProviderConfig{{ID: "fj", Kind: config.ProviderKindForgejo, BaseURL: "https://forgejo.example.test", TokenEnv: stringPtr("FORGEJO_TOKEN")}},
		Projects:  []config.ProjectRefConfig{{ID: "forgejo-demo", Name: "Forgejo", Provider: "fj", Repo: "owner/repo", RepoPath: "/tmp/repo"}},
		Roles:     config.RoleConfigs{Reviewer: config.ReviewerRoleConfig{Behavior: config.ReviewerConfig{PublishMode: config.ReviewerPublishModeSummaryComment}}},
	}
	if reviewerAllowsTrustedReviewProxy(summaryCfg, "forgejo-demo", map[string]any{"phase": "review"}) {
		t.Fatal("summary_comment project allowed socket, want false")
	}
	// Native single_review Forgejo projects still allow the socket.
	nativeCfg := &config.Config{
		Providers: []config.ProviderConfig{{ID: "fj", Kind: config.ProviderKindForgejo, BaseURL: "https://forgejo.example.test", TokenEnv: stringPtr("FORGEJO_TOKEN")}},
		Projects:  []config.ProjectRefConfig{{ID: "forgejo-native", Name: "Forgejo", Provider: "fj", Repo: "owner/repo", RepoPath: "/tmp/repo"}},
		Roles:     config.RoleConfigs{Reviewer: config.ReviewerRoleConfig{Behavior: config.ReviewerConfig{PublishMode: config.ReviewerPublishModeSingleReview}}},
	}
	if !reviewerAllowsTrustedReviewProxy(nativeCfg, "forgejo-native", map[string]any{"phase": "review"}) {
		t.Fatal("single_review Forgejo project not allowed, want true")
	}
	if !reviewerAllowsTrustedReviewProxy(nativeCfg, "forgejo-native", map[string]any{"phase": "publish"}) {
		t.Fatal("publish phase on native Forgejo not allowed, want true")
	}
	if !reviewerAllowsTrustedReviewProxy(nativeCfg, "forgejo-native", map[string]any{"phase": "REVIEW"}) {
		t.Fatal("REVIEW phase on native Forgejo not allowed, want true")
	}
	// GitHub projects in a mixed install must not receive the Forgejo review proxy.
	githubCfg := &config.Config{
		Providers: []config.ProviderConfig{
			{ID: "gh", Kind: config.ProviderKindGitHub},
			{ID: "fj", Kind: config.ProviderKindForgejo, BaseURL: "https://forgejo.example.test", TokenEnv: stringPtr("FORGEJO_TOKEN")},
		},
		Projects: []config.ProjectRefConfig{
			{ID: "github-demo", Name: "GitHub", Provider: "gh", Repo: "owner/repo", RepoPath: "/tmp/github"},
			{ID: "forgejo-native", Name: "Forgejo", Provider: "fj", Repo: "owner/fj-repo", RepoPath: "/tmp/forgejo"},
		},
		Roles: config.RoleConfigs{Reviewer: config.ReviewerRoleConfig{Behavior: config.ReviewerConfig{PublishMode: config.ReviewerPublishModeSingleReview}}},
	}
	if reviewerAllowsTrustedReviewProxy(githubCfg, "github-demo", map[string]any{"phase": "review"}) {
		t.Fatal("GitHub project allowed socket, want false")
	}
	if !reviewerAllowsTrustedReviewProxy(githubCfg, "forgejo-native", map[string]any{"phase": "review"}) {
		t.Fatal("Forgejo project in mixed install not allowed, want true")
	}
}

func TestReviewerAgentExecutorAdapterInjectsTrustedReviewSock(t *testing.T) {
	workDir := t.TempDir()
	scriptDir := t.TempDir()
	outputPath := filepath.Join(scriptDir, "child.env")
	scriptPath := filepath.Join(scriptDir, "dump-env")
	// Dump only whether a non-empty sock path was injected; the path is ephemeral.
	script := "#!/bin/sh\nif [ -n \"$LOOPER_TRUSTED_REVIEW_SOCK\" ]; then printf 'sock=set\\n' > \"" + outputPath + "\"; else printf 'sock=\\n' > \"" + outputPath + "\"; fi\nprintf '__LOOPER_RESULT__={\"summary\":\"done\"}\\n'\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile(scriptPath) error = %v", err)
	}
	// realLooper only needs to exist; this test asserts sock injection, not proxy exec.
	realLooper := filepath.Join(scriptDir, "real-looper")
	if err := os.WriteFile(realLooper, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(realLooper) error = %v", err)
	}

	// Shared executor config deliberately omits the sock — only the reviewer
	// adapter may inject LOOPER_TRUSTED_REVIEW_SOCK for review-submit capability.
	executor := agent.New(agent.ExecutorOptions{
		Config: agent.ExecutorConfig{
			Vendor: config.AgentVendor("custom"),
			Params: map[string]any{"command": scriptPath},
			Env:    map[string]string{"SHARED": "1"},
		},
	})
	nativeCfg := &config.Config{
		Providers: []config.ProviderConfig{{ID: "fj", Kind: config.ProviderKindForgejo, BaseURL: "https://forgejo.example.test", TokenEnv: stringPtr("FORGEJO_TOKEN")}},
		Projects:  []config.ProjectRefConfig{{ID: "forgejo-native", Name: "Forgejo", Provider: "fj", Repo: "acme/looper", RepoPath: workDir}},
		Roles:     config.RoleConfigs{Reviewer: config.ReviewerRoleConfig{Behavior: config.ReviewerConfig{PublishMode: config.ReviewerPublishModeSingleReview}}},
	}
	adapter := reviewerAgentExecutorAdapter{
		executor:   executor,
		realLooper: realLooper,
		trustedEnv: map[string]string{"FORGEJO_TOKEN": "test-token"},
		config:     nativeCfg,
	}
	execHandle, err := adapter.Start(context.Background(), reviewer.AgentRunInput{
		ExecutionID:      "reviewer_trusted_sock",
		ProjectID:        "forgejo-native",
		WorkingDirectory: workDir,
		Prompt:           "review",
		Timeout:          5 * time.Second,
		Metadata: map[string]any{
			"phase":               "review",
			"repo":                "acme/looper",
			"prNumber":            int64(42),
			"cleanReviewEvent":    "APPROVE",
			"blockingReviewEvent": "REQUEST_CHANGES",
		},
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	result, err := execHandle.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if result.Status != "completed" {
		t.Fatalf("result.Status = %q stderr=%q, want completed", result.Status, result.Stderr)
	}

	data, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("ReadFile(outputPath) error = %v", err)
	}
	if string(data) != "sock=set\n" {
		t.Fatalf("child env dump = %q, want sock=set", string(data))
	}

	// Tea-backed Forgejo (empty trustedEnv) must still mint the socket for
	// PR/CWD/policy/config binding even though there are no token env vars.
	teaAdapter := reviewerAgentExecutorAdapter{
		executor:   executor,
		realLooper: realLooper,
		trustedEnv: nil,
		config:     nativeCfg,
	}
	teaHandle, err := teaAdapter.Start(context.Background(), reviewer.AgentRunInput{
		ExecutionID:      "reviewer_tea_trusted_sock",
		ProjectID:        "forgejo-native",
		WorkingDirectory: workDir,
		Prompt:           "review",
		Timeout:          5 * time.Second,
		Metadata: map[string]any{
			"phase":               "review",
			"repo":                "acme/looper",
			"prNumber":            int64(42),
			"cleanReviewEvent":    "APPROVE",
			"blockingReviewEvent": "REQUEST_CHANGES",
		},
	})
	if err != nil {
		t.Fatalf("Start(tea) error = %v", err)
	}
	if _, err := teaHandle.Wait(context.Background()); err != nil {
		t.Fatalf("Wait(tea) error = %v", err)
	}
	data, err = os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("ReadFile after tea run error = %v", err)
	}
	if string(data) != "sock=set\n" {
		t.Fatalf("tea child env dump = %q, want sock=set", string(data))
	}

	// GitHub projects must not receive the Forgejo review proxy socket.
	githubCfg := &config.Config{
		Providers: []config.ProviderConfig{{ID: "gh", Kind: config.ProviderKindGitHub}},
		Projects:  []config.ProjectRefConfig{{ID: "github-demo", Name: "GitHub", Provider: "gh", Repo: "acme/looper", RepoPath: workDir}},
	}
	githubAdapter := reviewerAgentExecutorAdapter{
		executor:   executor,
		realLooper: realLooper,
		trustedEnv: map[string]string{"FORGEJO_TOKEN": "test-token"},
		config:     githubCfg,
	}
	githubHandle, err := githubAdapter.Start(context.Background(), reviewer.AgentRunInput{
		ExecutionID:      "reviewer_github_no_sock",
		ProjectID:        "github-demo",
		WorkingDirectory: workDir,
		Prompt:           "review",
		Timeout:          5 * time.Second,
		Metadata: map[string]any{
			"phase":               "review",
			"repo":                "acme/looper",
			"prNumber":            int64(42),
			"cleanReviewEvent":    "APPROVE",
			"blockingReviewEvent": "REQUEST_CHANGES",
		},
	})
	if err != nil {
		t.Fatalf("Start(github) error = %v", err)
	}
	if _, err := githubHandle.Wait(context.Background()); err != nil {
		t.Fatalf("Wait(github) error = %v", err)
	}
	data, err = os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("ReadFile after github run error = %v", err)
	}
	if string(data) != "sock=\n" {
		t.Fatalf("github child env dump = %q, want empty sock", string(data))
	}

	// Thread-resolution classifiers must not receive review-publish capability.
	threadHandle, err := adapter.Start(context.Background(), reviewer.AgentRunInput{
		ExecutionID:      "reviewer_thread_resolution",
		ProjectID:        "forgejo-native",
		WorkingDirectory: workDir,
		Prompt:           "classify",
		Timeout:          5 * time.Second,
		Metadata:         map[string]any{"phase": "thread_resolution", "repo": "acme/looper", "prNumber": int64(42)},
	})
	if err != nil {
		t.Fatalf("Start(thread_resolution) error = %v", err)
	}
	if _, err := threadHandle.Wait(context.Background()); err != nil {
		t.Fatalf("Wait(thread_resolution) error = %v", err)
	}
	data, err = os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("ReadFile after thread_resolution run error = %v", err)
	}
	if string(data) != "sock=\n" {
		t.Fatalf("thread_resolution child env dump = %q, want empty sock", string(data))
	}

	// Without daemon-selected PR metadata, no review-publish capability is injected.
	noPRHandle, err := adapter.Start(context.Background(), reviewer.AgentRunInput{
		ExecutionID:      "reviewer_no_pr_meta",
		ProjectID:        "forgejo-native",
		WorkingDirectory: workDir,
		Prompt:           "review",
		Timeout:          5 * time.Second,
		Metadata:         map[string]any{"phase": "review"},
	})
	if err != nil {
		t.Fatalf("Start(no PR) error = %v", err)
	}
	if _, err := noPRHandle.Wait(context.Background()); err != nil {
		t.Fatalf("Wait(no PR) error = %v", err)
	}
	data, err = os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("ReadFile after no-PR run error = %v", err)
	}
	if string(data) != "sock=\n" {
		t.Fatalf("no-PR child env dump = %q, want empty sock", string(data))
	}

	// summary_comment projects must not mint a review-submit socket even with full PR metadata.
	summaryCfg := &config.Config{
		Providers: []config.ProviderConfig{{ID: "fj", Kind: config.ProviderKindForgejo, BaseURL: "https://forgejo.example.test", TokenEnv: stringPtr("FORGEJO_TOKEN")}},
		Projects:  []config.ProjectRefConfig{{ID: "summary-project", Name: "Forgejo", Provider: "fj", Repo: "acme/looper", RepoPath: workDir}},
		Roles:     config.RoleConfigs{Reviewer: config.ReviewerRoleConfig{Behavior: config.ReviewerConfig{PublishMode: config.ReviewerPublishModeSummaryComment}}},
	}
	summaryAdapter := reviewerAgentExecutorAdapter{
		executor:   executor,
		realLooper: realLooper,
		trustedEnv: map[string]string{"FORGEJO_TOKEN": "test-token"},
		config:     summaryCfg,
	}
	summaryHandle, err := summaryAdapter.Start(context.Background(), reviewer.AgentRunInput{
		ExecutionID:      "reviewer_summary_comment",
		ProjectID:        "summary-project",
		WorkingDirectory: workDir,
		Prompt:           "review",
		Timeout:          5 * time.Second,
		Metadata: map[string]any{
			"phase":    "review",
			"repo":     "acme/looper",
			"prNumber": int64(42),
		},
	})
	if err != nil {
		t.Fatalf("Start(summary_comment) error = %v", err)
	}
	if _, err := summaryHandle.Wait(context.Background()); err != nil {
		t.Fatalf("Wait(summary_comment) error = %v", err)
	}
	data, err = os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("ReadFile after summary_comment run error = %v", err)
	}
	if string(data) != "sock=\n" {
		t.Fatalf("summary_comment child env dump = %q, want empty sock", string(data))
	}

	// Planner/worker/fixer path: shared executor without adapter injection must
	// not expose the review-submit socket.
	plainExec, err := executor.Start(context.Background(), agent.RunInput{
		ExecutionID:      "non_reviewer",
		WorkingDirectory: workDir,
		Prompt:           "work",
		Timeout:          5 * time.Second,
	})
	if err != nil {
		t.Fatalf("plain Start() error = %v", err)
	}
	plainResult, err := plainExec.Wait(context.Background())
	if err != nil {
		t.Fatalf("plain Wait() error = %v", err)
	}
	if plainResult.Status != "completed" {
		t.Fatalf("plain result.Status = %q, want completed", plainResult.Status)
	}
	data, err = os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("ReadFile(outputPath) after plain run error = %v", err)
	}
	if string(data) != "sock=\n" {
		t.Fatalf("non-reviewer child env dump = %q, want empty sock", string(data))
	}
}
