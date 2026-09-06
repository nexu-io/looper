package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/forge"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/reviewer/automerge"
)

func TestForgejoAutoMergeAdapterPreservesPolicyAndReviewedHead(t *testing.T) {
	t.Setenv("FORGEJO_MERGE_TOKEN", "test")
	protected, statusChecks := true, true
	contexts := []string{"unit"}
	mergeStatus, mergeCalls := http.StatusOK, 0
	var mergeBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/swagger.v1.json":
			_, _ = w.Write([]byte(`{"paths":{"/repos/{owner}/{repo}/pulls/{index}/merge":{"post":{}}}}`))
		case "/api/v1/repos/core/looper":
			_, _ = w.Write([]byte(`{"allow_merge_commits":true,"allow_rebase":false,"allow_squash_merge":true}`))
		case "/api/v1/repos/core/looper/branches/release/next":
			if r.URL.EscapedPath() != "/api/v1/repos/core/looper/branches/release%2Fnext" {
				t.Errorf("branch path = %s", r.URL.EscapedPath())
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"protected": protected, "enable_status_check": statusChecks, "status_check_contexts": contexts})
		case "/api/v1/repos/core/looper/pulls/42/merge":
			if r.Method != http.MethodPost {
				t.Errorf("merge method = %s", r.Method)
			}
			mergeCalls++
			if err := json.NewDecoder(r.Body).Decode(&mergeBody); err != nil {
				t.Error(err)
			}
			w.WriteHeader(mergeStatus)
		case "/api/v1/repos/core/looper/issues/1":
			_, _ = w.Write([]byte(`{"number":1,"state":"open","body":"Acceptance criteria","html_url":"https://code.example/core/looper/issues/1"}`))
		case "/api/v1/repos/core/looper/issues/2":
			_, _ = w.Write([]byte(`{"number":2,"state":"open","pull_request":{"url":"api-url"}}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	cfg := config.Config{Providers: []config.ProviderConfig{{ID: "forge", Kind: config.ProviderKindForgejo, BaseURL: server.URL, TokenEnv: stringPtr("FORGEJO_MERGE_TOKEN")}}, Projects: []config.ProjectRefConfig{{ID: "p", Provider: "forge", Repo: "core/looper", RepoPath: t.TempDir()}}}
	adapter := reviewerGitHubAdapter{config: &cfg} // No GitHub gateway: every operation must route to Forgejo.
	ctx := context.Background()
	settings, err := adapter.GetRepositorySettings(ctx, githubinfra.RepositorySettingsInput{Repo: "core/looper"})
	if err != nil || !settings.AllowAutoMerge || !settings.AllowSquashMerge || !settings.AllowMergeCommit || settings.AllowRebaseMerge {
		t.Fatalf("settings = %#v, %v", settings, err)
	}
	policy := config.ReviewerAutoMergeConfig{Enabled: true, Scope: config.ReviewerAutoMergeScopeLooperOnly, Strategy: config.ReviewerAutoMergeStrategySquash, RequireBranchProtection: true}
	pr := automerge.PRSnapshot{Labels: []string{"looper:ready"}, HasTrackedIssueLink: true}
	for _, tc := range []struct {
		name              string
		protected, checks bool
		contexts          []string
		want              automerge.RefusalReason
	}{
		{"protected with required check", true, true, []string{"unit"}, ""},
		{"unprotected", false, true, []string{"unit"}, automerge.RefusalReasonNoBranchProtection},
		{"checks disabled", true, false, []string{"unit"}, automerge.RefusalReasonNoBranchProtection},
		{"no required contexts", true, true, nil, automerge.RefusalReasonNoBranchProtection},
	} {
		t.Run(tc.name, func(t *testing.T) {
			protected, statusChecks, contexts = tc.protected, tc.checks, tc.contexts
			branch, err := adapter.GetBranchProtection(ctx, githubinfra.BranchProtectionInput{Repo: "core/looper", Branch: "release/next"})
			if err != nil {
				t.Fatal(err)
			}
			decision := automerge.Decide(pr, policy, automerge.BranchProtectionSnapshot{Exists: branch.Enabled, HasRequiredChecks: branch.HasRequiredChecks}, automerge.RepoSettingsSnapshot{AllowSquashMerge: settings.AllowSquashMerge, AllowAutoMerge: settings.AllowAutoMerge})
			if decision.Reason != tc.want {
				t.Fatalf("decision = %#v, want reason %s", decision, tc.want)
			}
		})
	}
	for _, strategy := range []config.ReviewerAutoMergeStrategy{config.ReviewerAutoMergeStrategyMerge, config.ReviewerAutoMergeStrategySquash, config.ReviewerAutoMergeStrategyRebase} {
		if err := adapter.EnableAutoMerge(ctx, githubinfra.EnableAutoMergeInput{Repo: "core/looper", PRNumber: 42, Strategy: strategy, HeadSHA: "reviewed-head"}); err != nil {
			t.Fatal(err)
		}
		if len(mergeBody) != 5 || mergeBody["Do"] != string(strategy) || mergeBody["head_commit_id"] != "reviewed-head" || mergeBody["merge_when_checks_succeed"] != false || mergeBody["force_merge"] != false || mergeBody["delete_branch_after_merge"] != false {
			t.Fatalf("unsafe merge payload = %#v", mergeBody)
		}
	}
	before := mergeCalls
	if err := adapter.EnableAutoMerge(ctx, githubinfra.EnableAutoMergeInput{Repo: "core/looper", PRNumber: 42, Strategy: config.ReviewerAutoMergeStrategySquash}); err == nil || mergeCalls != before {
		t.Fatalf("missing reviewed head err = %v, calls = %d", err, mergeCalls)
	}
	for _, code := range []int{http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusInternalServerError} {
		mergeStatus = code
		err := adapter.EnableAutoMerge(ctx, githubinfra.EnableAutoMergeInput{Repo: "core/looper", PRNumber: 42, Strategy: config.ReviewerAutoMergeStrategySquash, HeadSHA: "reviewed-head"})
		var httpErr *forge.ForgejoHTTPError
		if !errors.As(err, &httpErr) || httpErr.StatusCode != code {
			t.Fatalf("merge status %d err = %v", code, err)
		}
	}
	for _, number := range []int64{1, 2} {
		issue, err := adapter.ViewIssue(ctx, githubinfra.ViewIssueInput{Repo: "core/looper", IssueNumber: number})
		if err != nil || issue.IsPullRequest != (number == 2) {
			t.Fatalf("issue %d = %#v, %v", number, issue, err)
		}
	}
}

func TestForgejoMergeAdapterDoesNotScheduleAndRejectsChangedHead(t *testing.T) {
	t.Setenv("FORGEJO_IMMEDIATE_MERGE_TOKEN", "test")
	head, checksReady, scheduled, merged := "reviewed-head", false, 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/swagger.v1.json":
			_, _ = w.Write([]byte(`{"paths":{"/repos/{owner}/{repo}/pulls/{index}/merge":{"post":{}}}}`))
		case "/api/v1/repos/core/looper/pulls/42":
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 42, "state": "open", "head": map[string]any{"sha": head}})
		case "/api/v1/repos/core/looper/pulls/42/merge":
			var payload struct {
				Head     string `json:"head_commit_id"`
				Schedule bool   `json:"merge_when_checks_succeed"`
				Force    bool   `json:"force_merge"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			if payload.Force {
				t.Error("merge request bypasses server protection")
			}
			// Forgejo v16.0.3 pull.go schedules before passing HeadCommitID to
			// Merge; services/automerge later merges with an empty expected head.
			if payload.Schedule {
				scheduled++
				w.WriteHeader(http.StatusCreated)
				return
			}
			if !checksReady {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			if payload.Head != head {
				w.WriteHeader(http.StatusConflict)
				return
			}
			merged++
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	cfg := config.Config{Providers: []config.ProviderConfig{{ID: "fj", Kind: config.ProviderKindForgejo, BaseURL: server.URL, TokenEnv: stringPtr("FORGEJO_IMMEDIATE_MERGE_TOKEN")}}, Projects: []config.ProjectRefConfig{{ID: "p", Provider: "fj", Repo: "core/looper", RepoPath: t.TempDir()}}}
	adapter := reviewerGitHubAdapter{config: &cfg}
	input := githubinfra.EnableAutoMergeInput{Repo: "core/looper", PRNumber: 42, Strategy: config.ReviewerAutoMergeStrategySquash, HeadSHA: head}
	requireStatus := func(want int) {
		t.Helper()
		err := adapter.EnableAutoMerge(context.Background(), input)
		var httpErr *forge.ForgejoHTTPError
		if !errors.As(err, &httpErr) || httpErr.StatusCode != want {
			t.Fatalf("merge = %v, want HTTP %d", err, want)
		}
	}
	requireStatus(http.StatusMethodNotAllowed)
	// A later push must never inherit the earlier merge decision, including
	// when the caller inspected the old head immediately before the request.
	head, checksReady = "unreviewed-new-head", true
	requireStatus(http.StatusConflict)
	if scheduled != 0 || merged != 0 {
		t.Fatalf("old decision caused remote action: queued=%d merged=%d", scheduled, merged)
	}
	// A separately authorized new-head decision can use the same transport.
	input.HeadSHA = head
	if err := adapter.EnableAutoMerge(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	if scheduled != 0 || merged != 1 {
		t.Fatalf("new-head merge: queued=%d merged=%d", scheduled, merged)
	}
}
