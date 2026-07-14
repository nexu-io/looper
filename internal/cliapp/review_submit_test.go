package cliapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/diffanchor"
	"github.com/nexu-io/looper/internal/disclosure"
	"github.com/nexu-io/looper/internal/domain"
	"github.com/nexu-io/looper/internal/forge"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/infra/shell"
	"github.com/nexu-io/looper/internal/outboundguard"
	"github.com/nexu-io/looper/internal/storage"
	"github.com/spf13/cobra"
)

var commentOnlyReviewPolicy = config.ReviewerReviewEventsConfig{Clean: config.ReviewerReviewEventComment, Blocking: config.ReviewerReviewEventComment}
var decisionReviewPolicy = config.ReviewerReviewEventsConfig{Clean: config.ReviewerReviewEventApprove, Blocking: config.ReviewerReviewEventRequestChanges}

func TestCanSubmitWithoutAnchorValidationOnlyAllowsLargeDiffTopLevelReviews(t *testing.T) {
	t.Parallel()

	if !canSubmitWithoutAnchorValidation(githubinfra.ErrDiffTooLarge, nil) {
		t.Fatalf("canSubmitWithoutAnchorValidation() = false, want true for large diff top-level review")
	}
	if !canSubmitWithoutAnchorValidation(githubinfra.ErrLocalCaptureTruncated, nil) {
		t.Fatalf("canSubmitWithoutAnchorValidation() = false, want true for local capture truncation top-level review")
	}
	if canSubmitWithoutAnchorValidation(githubinfra.ErrDiffTooLarge, []reviewSubmitComment{{Body: "inline", Path: "app.go", Line: 10, Side: "RIGHT"}}) {
		t.Fatalf("canSubmitWithoutAnchorValidation() = true, want false when inline comments need validation")
	}
	if canSubmitWithoutAnchorValidation(githubinfra.ErrLocalCaptureTruncated, []reviewSubmitComment{{Body: "inline", Path: "app.go", Line: 10, Side: "RIGHT"}}) {
		t.Fatalf("canSubmitWithoutAnchorValidation(local truncation with comments) = true, want false")
	}
	if canSubmitWithoutAnchorValidation(githubinfra.ErrAnchorValidationUnavailable, []reviewSubmitComment{{Body: "inline", Path: "app.go", Line: 10, Side: "RIGHT"}}) {
		t.Fatalf("canSubmitWithoutAnchorValidation(unavailable with comments) = true, want false fail-closed")
	}
	if canSubmitWithoutAnchorValidation(errors.New("network failed"), nil) {
		t.Fatalf("canSubmitWithoutAnchorValidation() = true, want false for generic diff errors")
	}
}

func TestValidateExpectedBaseCommit(t *testing.T) {
	t.Parallel()
	if err := validateExpectedBaseCommit("abc123", "ABC123"); err != nil {
		t.Fatalf("validateExpectedBaseCommit() error = %v", err)
	}
	if err := validateExpectedBaseCommit("", "abc123"); err != nil {
		t.Fatalf("validateExpectedBaseCommit(empty expected) error = %v, want nil", err)
	}
	if err := validateExpectedBaseCommit("abc123", "def456"); err == nil || !strings.Contains(err.Error(), "expected base commit") {
		t.Fatalf("validateExpectedBaseCommit(mismatch) error = %v, want base drift failure", err)
	}
}

func TestForgejoReviewSubmitGatewayMapsOversizedDiffToDiffTooLarge(t *testing.T) {
	t.Parallel()

	// 1 MiB + 1 matches Forgejo client's response body cap.
	oversized := strings.Repeat("d", (1<<20)+1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/repos/acme/looper/pulls/42.diff" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(oversized))
	}))
	defer server.Close()

	client, err := forge.NewForgejoClient(forge.RepositoryRef{ProviderID: "forgejo", Kind: forge.ProviderKindForgejo, BaseURL: server.URL, Repo: "acme/looper"}, "token")
	if err != nil {
		t.Fatalf("NewForgejoClient() error = %v", err)
	}
	gateway := forgejoReviewSubmitGateway{client: client, stamper: disclosure.FromConfig(config.Config{})}
	_, err = gateway.GetPullRequestDiff(context.Background(), githubinfra.GetPullRequestDiffInput{Repo: "acme/looper", PRNumber: 42})
	if !errors.Is(err, githubinfra.ErrDiffTooLarge) {
		t.Fatalf("GetPullRequestDiff() error = %v, want ErrDiffTooLarge", err)
	}
	if !canSubmitWithoutAnchorValidation(err, nil) {
		t.Fatalf("canSubmitWithoutAnchorValidation() = false for Forgejo oversized top-level review")
	}
	if canSubmitWithoutAnchorValidation(err, []reviewSubmitComment{{Body: "inline", Path: "app.go", Line: 10, Side: "RIGHT"}}) {
		t.Fatalf("canSubmitWithoutAnchorValidation() = true, want false when inline comments need anchors")
	}
}

func TestForgejoReviewSubmitGatewayReusesMatchingNativeReviewMarker(t *testing.T) {
	t.Parallel()
	marker := "<!-- looper:review id=reviewer:loop:head head=head outcome=blocking -->"
	reviews := []map[string]any{}
	publishCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/swagger.v1.json":
			_, _ = w.Write([]byte(`{"paths":{"/repos/{owner}/{repo}/pulls/{index}/reviews":{"get":{},"post":{}},"/repos/{owner}/{repo}/pulls/{index}/reviews/{id}/comments":{"get":{}}}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/user":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 7, "login": "reviewer"})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/acme/looper/pulls/42/reviews":
			_ = json.NewEncoder(w).Encode(reviews)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/comments"):
			_ = json.NewEncoder(w).Encode([]any{})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/repos/acme/looper/pulls/42/reviews":
			publishCalls++
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode review payload: %v", err)
			}
			review := map[string]any{"id": 9, "state": "REQUEST_CHANGES", "body": payload["body"], "commit_id": "head", "user": map[string]any{"login": "reviewer"}}
			reviews = append(reviews, review)
			_ = json.NewEncoder(w).Encode(review)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	client, err := forge.NewForgejoClient(forge.RepositoryRef{ProviderID: "forgejo", Kind: forge.ProviderKindForgejo, BaseURL: server.URL, Repo: "acme/looper"}, "token")
	if err != nil {
		t.Fatalf("NewForgejoClient() error = %v", err)
	}
	gateway := forgejoReviewSubmitGateway{client: client, stamper: disclosure.FromConfig(config.Config{})}
	input := githubinfra.SubmitReviewInput{PRNumber: 42, Event: "REQUEST_CHANGES", Body: "Blocking issue\n\n" + marker, CommitID: "head"}
	if err := gateway.SubmitReview(context.Background(), input); err != nil {
		t.Fatalf("first SubmitReview() error = %v", err)
	}
	if err := gateway.SubmitReview(context.Background(), input); err != nil {
		t.Fatalf("retry SubmitReview() error = %v", err)
	}
	if publishCalls != 1 {
		t.Fatalf("publish calls = %d, want one native review", publishCalls)
	}
}

func TestForgejoReviewSubmitGatewayDoesNotReuseOtherAuthorsMatchingMarker(t *testing.T) {
	t.Parallel()
	marker := "<!-- looper:review id=reviewer:loop:head head=head outcome=blocking -->"
	reviews := []map[string]any{
		{"id": 8, "state": "REQUEST_CHANGES", "body": "Blocking issue\n\n" + marker, "commit_id": "head", "user": map[string]any{"login": "other-bot"}},
	}
	publishCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/swagger.v1.json":
			_, _ = w.Write([]byte(`{"paths":{"/repos/{owner}/{repo}/pulls/{index}/reviews":{"get":{},"post":{}},"/repos/{owner}/{repo}/pulls/{index}/reviews/{id}/comments":{"get":{}}}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/user":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 7, "login": "reviewer"})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/acme/looper/pulls/42/reviews":
			_ = json.NewEncoder(w).Encode(reviews)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/comments"):
			_ = json.NewEncoder(w).Encode([]any{})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/repos/acme/looper/pulls/42/reviews":
			publishCalls++
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode review payload: %v", err)
			}
			review := map[string]any{"id": 9, "state": "REQUEST_CHANGES", "body": payload["body"], "commit_id": "head", "user": map[string]any{"login": "reviewer"}}
			reviews = append(reviews, review)
			_ = json.NewEncoder(w).Encode(review)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	client, err := forge.NewForgejoClient(forge.RepositoryRef{ProviderID: "forgejo", Kind: forge.ProviderKindForgejo, BaseURL: server.URL, Repo: "acme/looper"}, "token")
	if err != nil {
		t.Fatalf("NewForgejoClient() error = %v", err)
	}
	gateway := forgejoReviewSubmitGateway{client: client, stamper: disclosure.FromConfig(config.Config{})}
	input := githubinfra.SubmitReviewInput{PRNumber: 42, Event: "REQUEST_CHANGES", Body: "Blocking issue\n\n" + marker, CommitID: "head"}
	if err := gateway.SubmitReview(context.Background(), input); err != nil {
		t.Fatalf("SubmitReview() error = %v", err)
	}
	if publishCalls != 1 {
		t.Fatalf("publish calls = %d, want one native review for the current author", publishCalls)
	}
}

func TestForgejoReviewSubmitGatewayNormalizesInvalidAnchorsBeforePublish(t *testing.T) {
	t.Parallel()
	var published map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/swagger.v1.json":
			_, _ = w.Write([]byte(`{"paths":{"/repos/{owner}/{repo}/pulls/{index}/reviews":{"get":{},"post":{}},"/repos/{owner}/{repo}/pulls/{index}/reviews/{id}/comments":{"get":{}}}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/user":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 7, "login": "reviewer"})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/acme/looper/pulls/42/reviews":
			_ = json.NewEncoder(w).Encode([]any{})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/repos/acme/looper/pulls/42/reviews":
			if err := json.NewDecoder(r.Body).Decode(&published); err != nil {
				t.Fatalf("decode review payload: %v", err)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 9, "state": "COMMENT", "body": published["body"], "commit_id": "head", "user": map[string]any{"login": "reviewer"}})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	client, err := forge.NewForgejoClient(forge.RepositoryRef{ProviderID: "forgejo", Kind: forge.ProviderKindForgejo, BaseURL: server.URL, Repo: "acme/looper"}, "token")
	if err != nil {
		t.Fatalf("NewForgejoClient() error = %v", err)
	}
	diff := "diff --git a/app.go b/app.go\n@@ -1,1 +1,1 @@\n-old\n+new\n"
	anchors := diffanchor.Parse(diff)
	gateway := forgejoReviewSubmitGateway{client: client, stamper: disclosure.FromConfig(config.Config{})}
	err = gateway.SubmitReview(context.Background(), githubinfra.SubmitReviewInput{
		PRNumber: 42,
		Event:    "COMMENT",
		Body:     "Needs work\n<!-- looper:review id=reviewer:loop:head head=head outcome=non_blocking -->",
		CommitID: "head",
		Comments: []githubinfra.ReviewComment{
			{Body: "Valid inline", Path: "app.go", Line: 1, Side: "RIGHT"},
			{Body: "Invalid inline", Path: "missing.go", Line: 99, Side: "RIGHT"},
		},
		Anchors: &anchors,
	})
	if err != nil {
		t.Fatalf("SubmitReview() error = %v", err)
	}
	if published == nil {
		t.Fatal("expected Forgejo review payload")
	}
	comments, _ := published["comments"].([]any)
	if len(comments) != 1 {
		t.Fatalf("published comments = %#v, want only the valid anchor", published["comments"])
	}
	valid, _ := comments[0].(map[string]any)
	if path, _ := valid["path"].(string); path != "app.go" {
		t.Fatalf("valid comment path = %#v, want app.go", valid)
	}
	body, _ := published["body"].(string)
	if !strings.Contains(body, "Invalid inline") || !strings.Contains(body, "missing.go") {
		t.Fatalf("body = %q, want downgraded invalid anchor preserved at top level", body)
	}
}

func TestValidateExpectedHeadCommit(t *testing.T) {
	t.Parallel()

	if err := validateExpectedHeadCommit("abc123", "ABC123"); err != nil {
		t.Fatalf("validateExpectedHeadCommit() error = %v", err)
	}
	if err := validateExpectedHeadCommit("", "abc123"); err == nil || !strings.Contains(err.Error(), "requires --commit-id") {
		t.Fatalf("validateExpectedHeadCommit(empty) error = %v, want commit-id requirement", err)
	}
	if err := validateExpectedHeadCommit("abc123", "def456"); err == nil || !strings.Contains(err.Error(), "expected head commit abc123 but PR head is def456") {
		t.Fatalf("validateExpectedHeadCommit(stale) error = %v, want stale head failure", err)
	}
}

func TestTrustedManualReviewerRunRequiresMatchingManualLoop(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	coordinator, err := storage.OpenSQLiteCoordinator(context.Background(), filepath.Join(root, "looper.sqlite"), storage.SQLiteCoordinatorOptions{Migrations: storage.EmbeddedMigrations, BackupDir: filepath.Join(root, "backups")})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	if _, err := coordinator.MigrationRunner().RunPending(context.Background()); err != nil {
		t.Fatalf("MigrationRunner.RunPending() error = %v", err)
	}
	repos := storage.NewRepositories(coordinator.DB())
	now := "2026-04-11T12:00:00.000Z"
	repo := "acme/looper"
	prNumber := int64(42)
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "project_1", Name: "Project", RepoPath: "/tmp/project", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Projects.Upsert(project_1) error = %v", err)
	}
	manualMetadata := `{"manual":true}`
	autoMetadata := `{"manual":false}`
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: "loop_manual", Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: "running", MetadataJSON: &manualMetadata, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Loops.Upsert(loop_manual) error = %v", err)
	}
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: "loop_auto", Seq: 2, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: "running", MetadataJSON: &autoMetadata, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Loops.Upsert(loop_auto) error = %v", err)
	}
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: "loop_old_manual", Seq: 3, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: "completed", MetadataJSON: &manualMetadata, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Loops.Upsert(loop_old_manual) error = %v", err)
	}
	if err := repos.Runs.Upsert(context.Background(), storage.RunRecord{ID: "run_manual", LoopID: "loop_manual", Status: "running", StartedAt: now, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Runs.Upsert(run_manual) error = %v", err)
	}
	if err := repos.Runs.Upsert(context.Background(), storage.RunRecord{ID: "run_auto", LoopID: "loop_auto", Status: "running", StartedAt: now, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Runs.Upsert(run_auto) error = %v", err)
	}
	if err := repos.Runs.Upsert(context.Background(), storage.RunRecord{ID: "run_old_manual", LoopID: "loop_old_manual", Status: "success", StartedAt: now, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Runs.Upsert(run_old_manual) error = %v", err)
	}

	trusted, err := trustedManualReviewerRun(context.Background(), repos, repo, prNumber, "run_manual")
	if err != nil || !trusted {
		t.Fatalf("trustedManualReviewerRun(manual) = %v, %v; want true, nil", trusted, err)
	}
	trusted, err = trustedManualReviewerRun(context.Background(), repos, repo, prNumber, "run_auto")
	if err != nil || trusted {
		t.Fatalf("trustedManualReviewerRun(auto) = %v, %v; want false, nil", trusted, err)
	}
	trusted, err = trustedManualReviewerRun(context.Background(), repos, repo, prNumber, "run_old_manual")
	if err != nil || trusted {
		t.Fatalf("trustedManualReviewerRun(old manual) = %v, %v; want false, nil", trusted, err)
	}
	trusted, err = trustedManualReviewerRun(context.Background(), repos, repo, 99, "run_manual")
	if err != nil || trusted {
		t.Fatalf("trustedManualReviewerRun(wrong PR) = %v, %v; want false, nil", trusted, err)
	}

	later := "2026-04-11T12:01:00.000Z"
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: "loop_newer_auto", Seq: 4, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: "running", MetadataJSON: &autoMetadata, CreatedAt: later, UpdatedAt: later}); err != nil {
		t.Fatalf("Loops.Upsert(loop_newer_auto) error = %v", err)
	}
	if err := repos.Runs.Upsert(context.Background(), storage.RunRecord{ID: "run_newer_auto", LoopID: "loop_newer_auto", Status: "running", StartedAt: later, CreatedAt: later, UpdatedAt: later}); err != nil {
		t.Fatalf("Runs.Upsert(run_newer_auto) error = %v", err)
	}
	trusted, err = trustedManualReviewerRun(context.Background(), repos, repo, prNumber, "run_manual")
	if err != nil || trusted {
		t.Fatalf("trustedManualReviewerRun(stale current manual) = %v, %v; want false, nil", trusted, err)
	}
}

func TestValidateForgejoReviewRequestManualRequiresCallerProof(t *testing.T) {
	t.Parallel()

	// When bypass is false, requireReviewRequest=true, and no trigger labels match,
	// the gateway must check requested reviewers (and fail when none match).
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/swagger.v1.json":
			_, _ = w.Write([]byte(`{"paths":{"/repos/{owner}/{repo}/pulls/{index}/requested_reviewers":{"post":{}}}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/user":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 7, "login": "reviewer-bot"})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/acme/looper/pulls/42":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": 42, "state": "open", "user": map[string]any{"login": "alice"},
				"requested_reviewers": []any{},
			})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)

	client, err := forge.NewForgejoClient(forge.RepositoryRef{ProviderID: "forgejo", Kind: forge.ProviderKindForgejo, BaseURL: server.URL, Repo: "acme/looper"}, "token")
	if err != nil {
		t.Fatalf("NewForgejoClient() error = %v", err)
	}
	gateway := forgejoReviewSubmitGateway{
		client:               client,
		stamper:              disclosure.FromConfig(config.Config{}),
		requireReviewRequest: true,
		labels:               []string{"looper:review"},
		labelMode:            config.LabelModeAll,
	}

	// Agent-controlled bypass=true without proven metadata would previously skip
	// this check. Callers must pass bypass only after trusted run/loop proof
	// (manual loop or follow-up on a new head).
	if err := gateway.validateReviewRequest(context.Background(), 42, nil, false); err == nil || !strings.Contains(err.Error(), "review request removed before publish") {
		t.Fatalf("validateReviewRequest(auto) = %v, want review request removed", err)
	}
	if err := gateway.validateReviewRequest(context.Background(), 42, nil, true); err != nil {
		t.Fatalf("validateReviewRequest(proven bypass) error = %v, want nil", err)
	}
}

func TestReviewSubmitFollowUpHasNewHead(t *testing.T) {
	t.Parallel()

	followUpMeta := `{"followUpdates":true,"lastPublishedHeadSha":"old-head","loop":{"enabled":true}}`
	if !reviewSubmitFollowUpHasNewHead(&followUpMeta, "new-head") {
		t.Fatal("reviewSubmitFollowUpHasNewHead(follow-up new head) = false, want true")
	}
	if reviewSubmitFollowUpHasNewHead(&followUpMeta, "old-head") {
		t.Fatal("reviewSubmitFollowUpHasNewHead(same head) = true, want false")
	}
	disabledFollow := `{"followUpdates":false,"lastPublishedHeadSha":"old-head","loop":{"enabled":true}}`
	if reviewSubmitFollowUpHasNewHead(&disabledFollow, "new-head") {
		t.Fatal("reviewSubmitFollowUpHasNewHead(followUpdates false) = true, want false")
	}
	loopDisabled := `{"followUpdates":true,"lastPublishedHeadSha":"old-head","loop":{"enabled":false}}`
	if reviewSubmitFollowUpHasNewHead(&loopDisabled, "new-head") {
		t.Fatal("reviewSubmitFollowUpHasNewHead(loop.enabled false) = true, want false")
	}
	noPublished := `{"followUpdates":true,"loop":{"enabled":true}}`
	if reviewSubmitFollowUpHasNewHead(&noPublished, "new-head") {
		t.Fatal("reviewSubmitFollowUpHasNewHead(no lastPublishedHeadSha) = true, want false")
	}
}

func TestTrustedFollowUpNewHeadReviewRequestBypass(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	coordinator, err := storage.OpenSQLiteCoordinator(context.Background(), filepath.Join(root, "looper.sqlite"), storage.SQLiteCoordinatorOptions{Migrations: storage.EmbeddedMigrations, BackupDir: filepath.Join(root, "backups")})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	if _, err := coordinator.MigrationRunner().RunPending(context.Background()); err != nil {
		t.Fatalf("MigrationRunner.RunPending() error = %v", err)
	}
	repos := storage.NewRepositories(coordinator.DB())
	now := "2026-04-11T12:00:00.000Z"
	repo := "acme/looper"
	prNumber := int64(42)
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "project_1", Name: "Project", RepoPath: "/tmp/project", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Projects.Upsert(project_1) error = %v", err)
	}
	followUpMeta := `{"followUpdates":true,"lastPublishedHeadSha":"old-head","loop":{"enabled":true}}`
	sameHeadMeta := `{"followUpdates":true,"lastPublishedHeadSha":"new-head","loop":{"enabled":true}}`
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: "loop_followup", Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: "running", MetadataJSON: &followUpMeta, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Loops.Upsert(loop_followup) error = %v", err)
	}
	if err := repos.Runs.Upsert(context.Background(), storage.RunRecord{ID: "run_followup", LoopID: "loop_followup", Status: "running", StartedAt: now, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Runs.Upsert(run_followup) error = %v", err)
	}

	// Automatic follow-up prompts do not pass --reviewer-run-id; resolve via current run.
	bypass, err := trustedCurrentFollowUpNewHeadReviewerBypass(context.Background(), repos, repo, prNumber, "new-head")
	if err != nil || !bypass {
		t.Fatalf("trustedCurrentFollowUpNewHeadReviewerBypass(new head) = %v, %v; want true, nil", bypass, err)
	}
	bypass, err = trustedCurrentFollowUpNewHeadReviewerBypass(context.Background(), repos, repo, prNumber, "old-head")
	if err != nil || bypass {
		t.Fatalf("trustedCurrentFollowUpNewHeadReviewerBypass(same head) = %v, %v; want false, nil", bypass, err)
	}

	// Explicit run-id path must also honor follow-up new-head.
	bypass, err = trustedFollowUpNewHeadReviewerRun(context.Background(), repos, repo, prNumber, "run_followup", "new-head")
	if err != nil || !bypass {
		t.Fatalf("trustedFollowUpNewHeadReviewerRun(new head) = %v, %v; want true, nil", bypass, err)
	}
	bypass, err = trustedFollowUpNewHeadReviewerRun(context.Background(), repos, repo, prNumber, "run_followup", "old-head")
	if err != nil || bypass {
		t.Fatalf("trustedFollowUpNewHeadReviewerRun(same head) = %v, %v; want false, nil", bypass, err)
	}
	bypass, err = trustedFollowUpNewHeadReviewerRun(context.Background(), repos, repo, prNumber, "missing_run", "new-head")
	if err != nil || bypass {
		t.Fatalf("trustedFollowUpNewHeadReviewerRun(missing run) = %v, %v; want false, nil", bypass, err)
	}

	// Same published head must not bypass even when followUpdates is enabled.
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: "loop_followup", Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: "running", MetadataJSON: &sameHeadMeta, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Loops.Upsert(same head) error = %v", err)
	}
	bypass, err = trustedCurrentFollowUpNewHeadReviewerBypass(context.Background(), repos, repo, prNumber, "new-head")
	if err != nil || bypass {
		t.Fatalf("trustedCurrentFollowUpNewHeadReviewerBypass(same published head) = %v, %v; want false, nil", bypass, err)
	}
}

func TestValidateReviewSubmitEventAcceptsRequestChanges(t *testing.T) {
	t.Parallel()

	if event, err := validateReviewSubmitEvent("comment"); err != nil || event != "COMMENT" {
		t.Fatalf("validateReviewSubmitEvent(comment) = %q, %v; want COMMENT, nil", event, err)
	}
	if event, err := validateReviewSubmitEvent("APPROVE"); err != nil || event != "APPROVE" {
		t.Fatalf("validateReviewSubmitEvent(APPROVE) = %q, %v; want APPROVE, nil", event, err)
	}
	if event, err := validateReviewSubmitEvent("REQUEST_CHANGES"); err != nil || event != "REQUEST_CHANGES" {
		t.Fatalf("validateReviewSubmitEvent(REQUEST_CHANGES) = %q, %v; want REQUEST_CHANGES, nil", event, err)
	}
}

func TestValidateReviewSubmitBodyRequiresSingleMatchingMarker(t *testing.T) {
	t.Parallel()
	body := "Review body\n<!-- looper:review id=abc head=def outcome=actionable -->"
	if err := validateReviewSubmitBody(body, nil, "def", "COMMENT", commentOnlyReviewPolicy, "octocat"); err != nil {
		t.Fatalf("validateReviewSubmitBody() error = %v", err)
	}
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{name: "missing", body: "Review body", want: "exactly one"},
		{name: "multiple", body: body + "\n<!-- looper:review id=abc head=def outcome=actionable -->", want: "exactly one"},
		{name: "malformed", body: "<!-- looper:review id=abc head=def -->", want: "exactly one"},
		{name: "stale", body: body, want: "does not match --commit-id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			commitID := "def"
			if tc.name == "stale" {
				commitID = "new"
			}
			err := validateReviewSubmitBody(tc.body, nil, commitID, "COMMENT", commentOnlyReviewPolicy, "octocat")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("validateReviewSubmitBody() error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestValidateReviewSubmitBodyRejectsApproveActionableMismatch(t *testing.T) {
	t.Parallel()
	body := "<!-- looper:review id=abc head=def outcome=actionable -->"
	if err := validateReviewSubmitBody(body, nil, "def", "APPROVE", decisionReviewPolicy, "octocat"); err == nil || !strings.Contains(err.Error(), "does not match APPROVE") {
		t.Fatalf("validateReviewSubmitBody(APPROVE actionable) error = %v, want mismatch", err)
	}
}

func TestValidateReviewSubmitBodyAllowsRequestChangesOnlyForBlocking(t *testing.T) {
	t.Parallel()
	body := "<!-- looper:review id=abc head=def outcome=blocking -->"
	if err := validateReviewSubmitBody(body, []reviewSubmitComment{{Body: "blocking", Path: "main.go", Line: 10, Side: "RIGHT"}}, "def", "REQUEST_CHANGES", decisionReviewPolicy, "octocat"); err != nil {
		t.Fatalf("validateReviewSubmitBody(REQUEST_CHANGES blocking) error = %v", err)
	}
	nonBlocking := "<!-- looper:review id=abc head=def outcome=non_blocking -->"
	if err := validateReviewSubmitBody(nonBlocking, nil, "def", "REQUEST_CHANGES", decisionReviewPolicy, "octocat"); err == nil || !strings.Contains(err.Error(), "does not match REQUEST_CHANGES") {
		t.Fatalf("validateReviewSubmitBody(REQUEST_CHANGES non_blocking) error = %v, want mismatch", err)
	}
}

func TestValidateReviewSubmitBodyRejectsCleanApproveWithInlineComments(t *testing.T) {
	t.Parallel()
	body := "<!-- looper:review id=abc head=def outcome=clean -->"
	err := validateReviewSubmitBody(body, []reviewSubmitComment{{Body: "inline", Path: "main.go", Line: 10, Side: "RIGHT"}}, "def", "APPROVE", decisionReviewPolicy, "octocat")
	if err == nil || !strings.Contains(err.Error(), "without inline comments") {
		t.Fatalf("validateReviewSubmitBody(APPROVE with comments) error = %v, want inline rejection", err)
	}
}

func TestValidateReviewSubmitBodyRequiresHumanCleanApproveBody(t *testing.T) {
	t.Parallel()
	marker := "<!-- looper:review id=abc head=def outcome=clean -->"
	stamp := "<!-- looper:stamp v=1 -->\n<sub>Generated by Looper 0.0.0-dev · runner=reviewer · agent=opencode</sub>"
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{name: "marker only", body: marker, want: "must start with an @mention"},
		{name: "disclosure only", body: marker + "\n\n" + stamp, want: "must start with an @mention"},
		{name: "wrong author", body: "@someone Thanks for the thoughtful update with clear safe changes and encouraging maintainable implementation.\n\n" + marker, want: "must start with an @mention"},
		{name: "too terse", body: "@octocat Nice work.\n\n" + marker, want: "short human summary"},
		{name: "hidden html filler", body: "@octocat <!-- these hidden filler words should not count toward the human summary requirement -->\n\n" + marker, want: "short human summary"},
		{name: "reference definition filler", body: "@octocat\n\n[hidden]:https://example.com\n  these hidden filler words should not count toward the human summary requirement\n\n" + marker, want: "short human summary"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateReviewSubmitBody(tc.body, nil, "def", "APPROVE", decisionReviewPolicy, "octocat")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("validateReviewSubmitBody() error = %v, want %q", err, tc.want)
			}
		})
	}

	body := strings.Join([]string{
		"@octocat Thanks for the thoughtful update — the changes are clear and well scoped.",
		"Summary: this keeps the approval flow safe while preserving the intended reviewer behavior.",
		"Nice work tightening this up; this should be easier to maintain going forward.",
		marker,
		stamp,
	}, "\n\n")
	if err := validateReviewSubmitBody(body, nil, "def", "APPROVE", decisionReviewPolicy, "OctoCat"); err != nil {
		t.Fatalf("validateReviewSubmitBody(APPROVE human body) error = %v", err)
	}
}

func TestValidateReviewSubmitEventAllowedRejectsApproveWhenDisabled(t *testing.T) {
	t.Parallel()
	if err := validateReviewSubmitEventAllowed("APPROVE", commentOnlyReviewPolicy); err == nil || !strings.Contains(err.Error(), "roles.reviewer.behavior.reviewEvents.clean=APPROVE") {
		t.Fatalf("validateReviewSubmitEventAllowed(APPROVE,commentOnly) error = %v, want policy rejection", err)
	}
	if err := validateReviewSubmitEventAllowed("APPROVE", decisionReviewPolicy); err != nil {
		t.Fatalf("validateReviewSubmitEventAllowed(APPROVE,decision) error = %v", err)
	}
	if err := validateReviewSubmitEventAllowed("REQUEST_CHANGES", commentOnlyReviewPolicy); err == nil || !strings.Contains(err.Error(), "roles.reviewer.behavior.reviewEvents.blocking=REQUEST_CHANGES") {
		t.Fatalf("validateReviewSubmitEventAllowed(REQUEST_CHANGES,commentOnly) error = %v, want policy rejection", err)
	}
	if err := validateReviewSubmitEventAllowed("REQUEST_CHANGES", decisionReviewPolicy); err != nil {
		t.Fatalf("validateReviewSubmitEventAllowed(REQUEST_CHANGES,decision) error = %v", err)
	}
	if err := validateReviewSubmitEventAllowed("COMMENT", commentOnlyReviewPolicy); err != nil {
		t.Fatalf("validateReviewSubmitEventAllowed(COMMENT,commentOnly) error = %v", err)
	}
}

func TestValidateReviewSubmitPolicyRejectsInvalidOverrides(t *testing.T) {
	t.Parallel()
	if err := validateReviewSubmitPolicy(config.ReviewerReviewEventsConfig{Clean: config.ReviewerReviewEventRequestChanges, Blocking: config.ReviewerReviewEventComment}); err == nil || !strings.Contains(err.Error(), "COMMENT or APPROVE") {
		t.Fatalf("validateReviewSubmitPolicy(invalid clean) error = %v, want clean rejection", err)
	}
	if err := validateReviewSubmitPolicy(config.ReviewerReviewEventsConfig{Clean: config.ReviewerReviewEventComment, Blocking: config.ReviewerReviewEventApprove}); err == nil || !strings.Contains(err.Error(), "COMMENT or REQUEST_CHANGES") {
		t.Fatalf("validateReviewSubmitPolicy(invalid blocking) error = %v, want blocking rejection", err)
	}
}

func TestEffectiveReviewSubmitPolicyHonorsDecisionOverrides(t *testing.T) {
	t.Parallel()

	policy, err := effectiveReviewSubmitPolicy(commentOnlyReviewPolicy, "APPROVE", "REQUEST_CHANGES")
	if err != nil {
		t.Fatalf("effectiveReviewSubmitPolicy(decision overrides) error = %v", err)
	}
	if policy != decisionReviewPolicy {
		t.Fatalf("effectiveReviewSubmitPolicy(decision overrides) = %+v, want %+v", policy, decisionReviewPolicy)
	}
}

func TestEffectiveReviewSubmitPolicyAllowsBaseAndNarrowingOverrides(t *testing.T) {
	t.Parallel()

	policy, err := effectiveReviewSubmitPolicy(decisionReviewPolicy, "COMMENT", "COMMENT")
	if err != nil {
		t.Fatalf("effectiveReviewSubmitPolicy(narrow to comment) error = %v", err)
	}
	if policy.Clean != config.ReviewerReviewEventComment || policy.Blocking != config.ReviewerReviewEventComment {
		t.Fatalf("effectiveReviewSubmitPolicy(narrow to comment) = %+v, want both COMMENT", policy)
	}

	policy, err = effectiveReviewSubmitPolicy(decisionReviewPolicy, "APPROVE", "REQUEST_CHANGES")
	if err != nil {
		t.Fatalf("effectiveReviewSubmitPolicy(base decisions) error = %v", err)
	}
	if policy != decisionReviewPolicy {
		t.Fatalf("effectiveReviewSubmitPolicy(base decisions) = %+v, want %+v", policy, decisionReviewPolicy)
	}
}

func TestEffectiveReviewSubmitEventDowngradesSelfAuthoredApproval(t *testing.T) {
	t.Parallel()

	runner := &reviewSubmitFakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		args := strings.Join(options.Args, " ")
		if args != "api user --jq .login" {
			t.Fatalf("unexpected gh args: %q", args)
		}
		return shell.Result{Stdout: "Reviewer\n"}, nil
	}
	gh := githubinfra.New(githubinfra.Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	cmd := &cobra.Command{Use: "test"}
	stderr := &bytes.Buffer{}
	cmd.SetErr(stderr)

	got, err := (&commandRuntime{}).effectiveReviewSubmitEvent(cmd, gh, "acme/looper", 42, "APPROVE", "reviewer", "")
	if err != nil {
		t.Fatalf("effectiveReviewSubmitEvent() error = %v", err)
	}
	if got != "COMMENT" {
		t.Fatalf("effectiveReviewSubmitEvent() = %q, want COMMENT", got)
	}
	if log := stderr.String(); !strings.Contains(log, "downgrading APPROVE review to COMMENT") || !strings.Contains(log, "GitHub does not allow self-approval") {
		t.Fatalf("stderr = %q, want self-approval downgrade log", log)
	}
}

func TestEffectiveReviewSubmitEventKeepsApprovalForDifferentAuthor(t *testing.T) {
	t.Parallel()

	runner := &reviewSubmitFakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		args := strings.Join(options.Args, " ")
		if args != "api user --jq .login" {
			t.Fatalf("unexpected gh args: %q", args)
		}
		return shell.Result{Stdout: "reviewer\n"}, nil
	}
	gh := githubinfra.New(githubinfra.Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	cmd := &cobra.Command{Use: "test"}
	stderr := &bytes.Buffer{}
	cmd.SetErr(stderr)

	got, err := (&commandRuntime{}).effectiveReviewSubmitEvent(cmd, gh, "acme/looper", 42, "APPROVE", "octocat", "")
	if err != nil {
		t.Fatalf("effectiveReviewSubmitEvent() error = %v", err)
	}
	if got != "APPROVE" {
		t.Fatalf("effectiveReviewSubmitEvent() = %q, want APPROVE", got)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestEffectiveReviewSubmitEventDoesNotFetchUserForComment(t *testing.T) {
	t.Parallel()

	runner := &reviewSubmitFakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		t.Fatalf("unexpected gh args: %q", strings.Join(options.Args, " "))
		return shell.Result{}, nil
	}
	gh := githubinfra.New(githubinfra.Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	cmd := &cobra.Command{Use: "test"}

	got, err := (&commandRuntime{}).effectiveReviewSubmitEvent(cmd, gh, "acme/looper", 42, "COMMENT", "reviewer", "")
	if err != nil {
		t.Fatalf("effectiveReviewSubmitEvent() error = %v", err)
	}
	if got != "COMMENT" {
		t.Fatalf("effectiveReviewSubmitEvent() = %q, want COMMENT", got)
	}
}

func TestWrapReviewSubmitErrorSurfacesContentSafetyRecoveryGuidance(t *testing.T) {
	t.Parallel()
	cmd := &cobra.Command{}
	stderr := &bytes.Buffer{}
	cmd.SetErr(stderr)
	secretPath := "SERVICE_TOKEN=secret-value"
	payload := reviewSubmitPayload{
		Body:     "Please address the findings.",
		Comments: []reviewSubmitComment{{Body: "Null check missing", Path: secretPath, Line: 1, Side: "RIGHT"}},
	}
	err := wrapReviewSubmitError(cmd, "acme/looper", 42, "COMMENT", "abc123", payload, "submit validated PR review", outboundguard.Validate(outboundguard.Field{Name: "inline review comment 1 path", Text: secretPath}))
	if err == nil {
		t.Fatal("wrapReviewSubmitError() error = nil, want content safety rejection")
	}
	msg := err.Error()
	for _, want := range []string{
		"blocked by content safety gate",
		"outbound content safety gate rejected inline review comment 1 path",
		"credential-shaped",
		outboundguard.RecoveryGuidance,
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error = %q, want substring %q", msg, want)
		}
	}
	if strings.Contains(msg, secretPath) {
		t.Fatalf("error %q echoed rejected path", msg)
	}
	if strings.Contains(stderr.String(), secretPath) {
		t.Fatalf("stderr diagnostic echoed rejected path: %s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "github_review_submit_validation_failed") {
		t.Fatalf("stderr = %q, want validation diagnostic", stderr.String())
	}
	if !strings.Contains(stderr.String(), `"path_present":true`) {
		t.Fatalf("stderr = %q, want path_present without raw path", stderr.String())
	}
}

func TestWriteReviewSubmitDiagnosticWritesStructuredJSON(t *testing.T) {
	t.Parallel()
	stderr := &bytes.Buffer{}
	payload := reviewSubmitPayload{
		Body:     "Please revisit this.\n<!-- looper:review id=abc head=def outcome=blocking -->",
		Comments: []reviewSubmitComment{{Body: "anchor", Path: "app.go", Line: 7, Side: "RIGHT", StartLine: 5, StartSide: "RIGHT"}},
	}
	writeReviewSubmitDiagnostic(stderr, "github_review_submit_validation_failed", reviewSubmitDiagnosticFields{
		Repo:     "acme/looper",
		PRNumber: 42,
		Event:    "REQUEST_CHANGES",
		CommitID: "def",
		Payload:  payload,
		Error:    "review marker outcome=clean does not match REQUEST_CHANGES event",
	})
	var entry map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(stderr.Bytes()), &entry); err != nil {
		t.Fatalf("decode stderr JSON: %v\n%s", err, stderr.String())
	}
	if entry["event"] != "github_review_submit_validation_failed" {
		t.Fatalf("event = %#v, want validation_failed", entry["event"])
	}
	if entry["repo"] != "acme/looper" || entry["pr_number"] != float64(42) || entry["commit_id"] != "def" {
		t.Fatalf("entry = %#v, want repo/pr/commit", entry)
	}
	payloadEntry, _ := entry["payload"].(map[string]any)
	marker, _ := payloadEntry["body_marker"].(map[string]any)
	if marker["id"] != "abc" || marker["head"] != "def" || marker["outcome"] != "blocking" {
		t.Fatalf("body marker = %#v, want marker fields", marker)
	}
	comments, _ := payloadEntry["comments"].([]any)
	if len(comments) != 1 {
		t.Fatalf("comments = %#v, want one comment", comments)
	}
	comment, _ := comments[0].(map[string]any)
	if comment["path"] != "app.go" || comment["line"] != float64(7) || comment["side"] != "RIGHT" || comment["start_line"] != float64(5) || comment["start_side"] != "RIGHT" {
		t.Fatalf("comment = %#v, want anchor summary", comment)
	}
	if entry["error"] == "" {
		t.Fatalf("entry = %#v, want error field", entry)
	}
}

func TestValidateReviewerReviewSubmitHoldRejectsHeldAutomaticReviewerFlow(t *testing.T) {
	t.Parallel()
	runtime := &commandRuntime{}
	cmd := &cobra.Command{}
	err := runtime.validateReviewerReviewSubmitHold(cmd, config.Config{}, "acme/looper", 42, false, "", []string{domain.HoldLabelReviewer})
	if err == nil || !strings.Contains(err.Error(), "currently held") {
		t.Fatalf("validateReviewerReviewSubmitHold() error = %v, want held automatic reviewer rejection", err)
	}
	if err := runtime.validateReviewerReviewSubmitHold(cmd, config.Config{}, "acme/looper", 42, false, "", nil); err != nil {
		t.Fatalf("validateReviewerReviewSubmitHold(unheld) error = %v", err)
	}
}

func TestValidateLatestReviewerReviewSubmitHoldRefreshesLabels(t *testing.T) {
	t.Parallel()

	runtime := &commandRuntime{}
	cmd := &cobra.Command{}
	gh := &reviewSubmitFakePRViewer{detail: githubinfra.PullRequestDetail{Number: 42, Labels: []string{domain.HoldLabelReviewer}}}

	labels, err := runtime.validateLatestReviewerReviewSubmitHold(cmd, gh, config.Config{}, "acme/looper", 42, false, "", "/repo")
	if err == nil || !strings.Contains(err.Error(), "currently held") {
		t.Fatalf("validateLatestReviewerReviewSubmitHold() error = %v, want held rejection", err)
	}
	if labels != nil {
		t.Fatalf("labels = %#v, want nil on hold rejection", labels)
	}
	if len(gh.calls) != 1 {
		t.Fatalf("ViewPullRequest calls = %#v, want one refresh", gh.calls)
	}
	if gh.calls[0].Repo != "acme/looper" || gh.calls[0].PRNumber != 42 || gh.calls[0].CWD != "/repo" {
		t.Fatalf("ViewPullRequest call = %#v, want requested PR and cwd", gh.calls[0])
	}

	gh = &reviewSubmitFakePRViewer{detail: githubinfra.PullRequestDetail{Number: 42, Labels: []string{"ready-for-review"}}}
	labels, err = runtime.validateLatestReviewerReviewSubmitHold(cmd, gh, config.Config{}, "acme/looper", 42, false, "", "/repo")
	if err != nil {
		t.Fatalf("validateLatestReviewerReviewSubmitHold(unheld) error = %v", err)
	}
	if len(labels) != 1 || labels[0] != "ready-for-review" {
		t.Fatalf("labels = %#v, want refreshed PR labels for publish authority", labels)
	}
}

func TestValidateLatestReviewerReviewSubmitPublicationRejectsHeadAndBaseDrift(t *testing.T) {
	t.Parallel()

	runtime := &commandRuntime{}
	cmd := &cobra.Command{}
	base := strings.Repeat("b", 40)
	head := strings.Repeat("c", 40)
	gh := &reviewSubmitFakePRViewer{detail: githubinfra.PullRequestDetail{
		Number: 42, HeadSHA: strings.Repeat("d", 40), BaseSHA: base, Labels: nil,
	}}

	_, err := runtime.validateLatestReviewerReviewSubmitPublication(cmd, gh, config.Config{}, "acme/looper", 42, head, base, false, "", "/repo")
	if err == nil || !strings.Contains(err.Error(), "expected head commit") {
		t.Fatalf("publication validation error = %v, want head drift rejection", err)
	}

	gh = &reviewSubmitFakePRViewer{detail: githubinfra.PullRequestDetail{
		Number: 42, HeadSHA: head, BaseSHA: strings.Repeat("e", 40), Labels: nil,
	}}
	_, err = runtime.validateLatestReviewerReviewSubmitPublication(cmd, gh, config.Config{}, "acme/looper", 42, head, base, false, "", "/repo")
	if err == nil || !strings.Contains(err.Error(), "expected base commit") {
		t.Fatalf("publication validation error = %v, want base drift rejection", err)
	}

	gh = &reviewSubmitFakePRViewer{detail: githubinfra.PullRequestDetail{
		Number: 42, HeadSHA: head, BaseSHA: base, Labels: []string{"ready-for-review"},
	}}
	labels, err := runtime.validateLatestReviewerReviewSubmitPublication(cmd, gh, config.Config{}, "acme/looper", 42, head, base, false, "", "/repo")
	if err != nil {
		t.Fatalf("publication validation error = %v, want nil when head/base match", err)
	}
	if len(labels) != 1 || labels[0] != "ready-for-review" {
		t.Fatalf("labels = %#v, want refreshed PR labels for publish authority", labels)
	}
}

// Pre-gate validation (malformed marker / APPROVE-with-comments) never reaches
// SubmitReview's content guard, so diagnostics must redact paths — a path may
// itself be secret-shaped (SERVICE_TOKEN=...).
func TestPreGateValidationDiagnosticRedactsSecretShapedPaths(t *testing.T) {
	t.Parallel()
	stderr := &bytes.Buffer{}
	secretPath := "SERVICE_TOKEN=secret-value"
	payload := reviewSubmitPayload{
		Body:     "missing marker",
		Comments: []reviewSubmitComment{{Body: "note", Path: secretPath, Line: 1, Side: "RIGHT"}},
	}
	writeReviewSubmitDiagnostic(stderr, "github_review_submit_validation_failed", reviewSubmitDiagnosticFields{
		Repo: "acme/looper", PRNumber: 42, Event: "APPROVE", CommitID: "abc123", Payload: payload,
		Error: "APPROVE reviews require clean outcome without inline comments", RedactPaths: true,
	})
	out := stderr.String()
	if strings.Contains(out, secretPath) {
		t.Fatalf("stderr diagnostic echoed secret-shaped path: %s", out)
	}
	if !strings.Contains(out, `"path_present":true`) {
		t.Fatalf("stderr = %q, want path_present without raw path", out)
	}
}

type reviewSubmitFakeGHRunner struct {
	t       *testing.T
	respond func(options shell.Options) (shell.Result, error)
}

func (f *reviewSubmitFakeGHRunner) run(_ context.Context, options shell.Options) (shell.Result, error) {
	f.t.Helper()
	if f.respond == nil {
		f.t.Fatalf("fake GH runner missing responder for args: %q", strings.Join(options.Args, " "))
	}
	return f.respond(options)
}

type reviewSubmitFakePRViewer struct {
	detail githubinfra.PullRequestDetail
	calls  []githubinfra.ViewPullRequestInput
	err    error
}

func (f *reviewSubmitFakePRViewer) ViewPullRequest(_ context.Context, input githubinfra.ViewPullRequestInput) (githubinfra.PullRequestDetail, error) {
	f.calls = append(f.calls, input)
	return f.detail, f.err
}

func TestReviewSubmitProjectForRepoPrefersCWDMatchAmongDuplicates(t *testing.T) {
	t.Parallel()

	githubRepo := filepath.Join(t.TempDir(), "github-checkout")
	forgejoRepo := filepath.Join(t.TempDir(), "forgejo-checkout")
	forgejoWorktreeRoot := filepath.Join(t.TempDir(), "forgejo-worktrees")
	forgejoWorktree := filepath.Join(forgejoWorktreeRoot, "reviewer-wt")
	for _, path := range []string{githubRepo, forgejoRepo, forgejoWorktree} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
	}

	cfg := config.Config{
		Projects: []config.ProjectRefConfig{
			{ID: "github-acme", Name: "GitHub", Repo: "acme/looper", RepoPath: githubRepo, Provider: "github"},
			{ID: "forgejo-acme", Name: "Forgejo", Repo: "acme/looper", RepoPath: forgejoRepo, Provider: "forgejo", WorktreeRoot: &forgejoWorktreeRoot},
		},
	}

	matched, err := reviewSubmitProjectForRepo(cfg, "acme/looper", forgejoRepo)
	if err != nil {
		t.Fatalf("reviewSubmitProjectForRepo(repo path) error = %v", err)
	}
	if matched == nil || matched.ID != "forgejo-acme" {
		t.Fatalf("reviewSubmitProjectForRepo(repo path) = %#v, want forgejo-acme", matched)
	}

	matched, err = reviewSubmitProjectForRepo(cfg, "acme/looper", forgejoWorktree)
	if err != nil {
		t.Fatalf("reviewSubmitProjectForRepo(worktree) error = %v", err)
	}
	if matched == nil || matched.ID != "forgejo-acme" {
		t.Fatalf("reviewSubmitProjectForRepo(worktree) = %#v, want forgejo-acme", matched)
	}

	matched, err = reviewSubmitProjectForRepo(cfg, "acme/looper", githubRepo)
	if err != nil {
		t.Fatalf("reviewSubmitProjectForRepo(github path) error = %v", err)
	}
	if matched == nil || matched.ID != "github-acme" {
		t.Fatalf("reviewSubmitProjectForRepo(github path) = %#v, want github-acme", matched)
	}
}

func TestReviewSubmitProjectForRepoStillAmbiguousWithoutCWDMatch(t *testing.T) {
	t.Parallel()

	cfg := config.Config{
		Projects: []config.ProjectRefConfig{
			{ID: "github-acme", Name: "GitHub", Repo: "acme/looper", RepoPath: filepath.Join(t.TempDir(), "github"), Provider: "github"},
			{ID: "forgejo-acme", Name: "Forgejo", Repo: "acme/looper", RepoPath: filepath.Join(t.TempDir(), "forgejo"), Provider: "forgejo"},
		},
	}
	matched, err := reviewSubmitProjectForRepo(cfg, "acme/looper", filepath.Join(t.TempDir(), "unrelated"))
	if err == nil || !strings.Contains(err.Error(), "matches multiple configured projects") {
		t.Fatalf("reviewSubmitProjectForRepo() error = %v, matched = %#v; want multiple-project error", err, matched)
	}
}

func TestReviewSubmitGatewayForConfigUsesCWDMatchedForgejoProject(t *testing.T) {
	// t.Setenv is incompatible with t.Parallel.
	tokenEnv := "LOOPER_TEST_FORGEJO_REVIEW_SUBMIT_TOKEN"
	t.Setenv(tokenEnv, "test-token")
	forgejoRepo := filepath.Join(t.TempDir(), "forgejo-checkout")
	if err := os.MkdirAll(forgejoRepo, 0o755); err != nil {
		t.Fatalf("mkdir forgejo checkout: %v", err)
	}
	cfg := config.Config{
		Providers: []config.ProviderConfig{{
			ID:       "forgejo",
			Kind:     config.ProviderKindForgejo,
			BaseURL:  "https://forgejo.example.test",
			TokenEnv: &tokenEnv,
		}},
		Projects: []config.ProjectRefConfig{
			{ID: "github-acme", Name: "GitHub", Repo: "acme/looper", RepoPath: filepath.Join(t.TempDir(), "github"), Provider: "github"},
			{ID: "forgejo-acme", Name: "Forgejo", Repo: "acme/looper", RepoPath: forgejoRepo, Provider: "forgejo"},
		},
	}
	gateway, err := reviewSubmitGatewayForConfig(cfg, "acme/looper", forgejoRepo, nil)
	if err != nil {
		t.Fatalf("reviewSubmitGatewayForConfig() error = %v", err)
	}
	if _, ok := gateway.(forgejoReviewSubmitGateway); !ok {
		t.Fatalf("gateway type = %T, want forgejoReviewSubmitGateway", gateway)
	}
}

func TestReviewSubmitProjectForRepoResolvesAPIManagedForgejoFromSQLite(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	repoPath := filepath.Join(root, "repo")
	if err := os.MkdirAll(repoPath, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	dbPath := filepath.Join(root, "looper.sqlite")
	coordinator, err := storage.OpenSQLiteCoordinator(context.Background(), dbPath, storage.SQLiteCoordinatorOptions{Migrations: storage.EmbeddedMigrations, BackupDir: filepath.Join(root, "backups")})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	if _, err := coordinator.MigrationRunner().RunPending(context.Background()); err != nil {
		t.Fatalf("RunPending() error = %v", err)
	}

	now := "2026-07-14T00:00:00.000Z"
	metadata := `{"provider":"forgejo","repo":"acme/looper","source":"api"}`
	if err := storage.NewRepositories(coordinator.DB()).Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID: "api-forgejo", Name: "API Forgejo", RepoPath: repoPath, MetadataJSON: &metadata, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	tokenEnv := "LOOPER_TEST_FORGEJO_API_PROJECT_TOKEN"
	cfg := config.Config{
		Storage: config.StorageConfig{DBPath: dbPath},
		Providers: []config.ProviderConfig{{
			ID: "forgejo", Kind: config.ProviderKindForgejo, BaseURL: "https://forgejo.example.test", TokenEnv: &tokenEnv,
		}},
		// No file-config projects: binding lives only in SQLite.
	}
	matched, err := reviewSubmitProjectForRepo(cfg, "acme/looper", repoPath)
	if err != nil {
		t.Fatalf("reviewSubmitProjectForRepo() error = %v", err)
	}
	if matched == nil || matched.ID != "api-forgejo" || matched.Provider != "forgejo" {
		t.Fatalf("reviewSubmitProjectForRepo() = %#v, want api-forgejo forgejo binding", matched)
	}
}

func TestReviewSubmitProjectForRepoCombinesFileAndStorageCandidates(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	githubRepo := filepath.Join(root, "github-checkout")
	forgejoRepo := filepath.Join(root, "forgejo-checkout")
	for _, path := range []string{githubRepo, forgejoRepo} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
	}
	dbPath := filepath.Join(root, "looper.sqlite")
	coordinator, err := storage.OpenSQLiteCoordinator(context.Background(), dbPath, storage.SQLiteCoordinatorOptions{Migrations: storage.EmbeddedMigrations, BackupDir: filepath.Join(root, "backups")})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	if _, err := coordinator.MigrationRunner().RunPending(context.Background()); err != nil {
		t.Fatalf("RunPending() error = %v", err)
	}

	now := "2026-07-14T00:00:00.000Z"
	metadata := `{"provider":"forgejo","repo":"acme/looper","source":"api"}`
	if err := storage.NewRepositories(coordinator.DB()).Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID: "api-forgejo", Name: "API Forgejo", RepoPath: forgejoRepo, MetadataJSON: &metadata, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	mixedTokenEnv := "LOOPER_TEST_MIXED_PROJECT_TOKEN"
	cfg := config.Config{
		Storage: config.StorageConfig{DBPath: dbPath},
		Providers: []config.ProviderConfig{{
			ID: "forgejo", Kind: config.ProviderKindForgejo, BaseURL: "https://forgejo.example.test", TokenEnv: &mixedTokenEnv,
		}},
		// Single file-config match for the same owner/repo must not hide the storage project.
		Projects: []config.ProjectRefConfig{
			{ID: "github-acme", Name: "GitHub", Repo: "acme/looper", RepoPath: githubRepo, Provider: "github"},
		},
	}

	matched, err := reviewSubmitProjectForRepo(cfg, "acme/looper", forgejoRepo)
	if err != nil {
		t.Fatalf("reviewSubmitProjectForRepo(forgejo cwd) error = %v", err)
	}
	if matched == nil || matched.ID != "api-forgejo" || matched.Provider != "forgejo" {
		t.Fatalf("reviewSubmitProjectForRepo(forgejo cwd) = %#v, want api-forgejo", matched)
	}

	matched, err = reviewSubmitProjectForRepo(cfg, "acme/looper", githubRepo)
	if err != nil {
		t.Fatalf("reviewSubmitProjectForRepo(github cwd) error = %v", err)
	}
	if matched == nil || matched.ID != "github-acme" {
		t.Fatalf("reviewSubmitProjectForRepo(github cwd) = %#v, want github-acme", matched)
	}
}

func TestReviewSubmitGatewayUsesStorageProjectRoles(t *testing.T) {
	// t.Setenv is incompatible with t.Parallel.
	tokenEnv := "LOOPER_TEST_FORGEJO_STORAGE_ROLES_TOKEN"
	t.Setenv(tokenEnv, "test-token")

	root := t.TempDir()
	repoPath := filepath.Join(root, "repo")
	if err := os.MkdirAll(repoPath, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	dbPath := filepath.Join(root, "looper.sqlite")
	coordinator, err := storage.OpenSQLiteCoordinator(context.Background(), dbPath, storage.SQLiteCoordinatorOptions{Migrations: storage.EmbeddedMigrations, BackupDir: filepath.Join(root, "backups")})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	if _, err := coordinator.MigrationRunner().RunPending(context.Background()); err != nil {
		t.Fatalf("RunPending() error = %v", err)
	}

	now := "2026-07-14T00:00:00.000Z"
	// Project-specific policy: do not require a review request before publish.
	metadata := `{"provider":"forgejo","repo":"acme/looper","source":"api","roles":{"reviewer":{"discovery":{"triggers":{"requireReviewRequest":false,"labels":["looper:review"]}}}}}`
	if err := storage.NewRepositories(coordinator.DB()).Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID: "api-forgejo", Name: "API Forgejo", RepoPath: repoPath, MetadataJSON: &metadata, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	cfg := config.Config{
		Storage: config.StorageConfig{DBPath: dbPath},
		Providers: []config.ProviderConfig{{
			ID: "forgejo", Kind: config.ProviderKindForgejo, BaseURL: "https://forgejo.example.test", TokenEnv: &tokenEnv,
		}},
		// Global policy still requires review requests; storage project must win.
		Roles: config.RoleConfigs{
			Reviewer: config.ReviewerRoleConfig{
				Discovery: config.ReviewerRoleDiscoveryConfig{
					Triggers: config.ReviewerRoleTriggersConfig{RequireReviewRequest: true},
				},
			},
		},
	}

	gateway, err := reviewSubmitGatewayForConfig(cfg, "acme/looper", repoPath, nil)
	if err != nil {
		t.Fatalf("reviewSubmitGatewayForConfig() error = %v", err)
	}
	forgeGateway, ok := gateway.(forgejoReviewSubmitGateway)
	if !ok {
		t.Fatalf("gateway type = %T, want forgejoReviewSubmitGateway", gateway)
	}
	if forgeGateway.requireReviewRequest {
		t.Fatalf("requireReviewRequest = true, want false from storage project roles")
	}
	if got := strings.Join(forgeGateway.labels, ","); got != "looper:review" {
		t.Fatalf("labels = %q, want looper:review from storage project roles", got)
	}
}
