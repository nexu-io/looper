package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/infra/planedoc"
	"github.com/nexu-io/looper/internal/infra/shell"
	"github.com/nexu-io/looper/internal/storage"
)

func TestLoopSpecApprovalState(t *testing.T) {
	cases := []struct {
		name           string
		meta           *string
		wantAwaiting   bool
		wantDispatched bool
		wantIssueURL   string
	}{
		{"nil meta", nil, false, false, ""},
		{"awaiting, not dispatched", strPtr(`{"awaitingSpecApproval":true,"issueUrl":"https://plane.x/w/projects/p/issues/wi-1"}`), true, false, "https://plane.x/w/projects/p/issues/wi-1"},
		{"awaiting + dispatched", strPtr(`{"awaitingSpecApproval":true,"specApprovedDispatched":true,"issueUrl":"u"}`), true, true, "u"},
		{"not awaiting", strPtr(`{"issueUrl":"u"}`), false, false, "u"},
		{"issueURL alt casing", strPtr(`{"awaitingSpecApproval":true,"issueURL":"u2"}`), true, false, "u2"},
	}
	for _, tc := range cases {
		state := loopSpecApprovalState(tc.meta)
		if state.Awaiting != tc.wantAwaiting || state.Dispatched != tc.wantDispatched || state.IssueURL != tc.wantIssueURL {
			t.Fatalf("%s: got (%v,%v,%q); want (%v,%v,%q)", tc.name, state.Awaiting, state.Dispatched, state.IssueURL, tc.wantAwaiting, tc.wantDispatched, tc.wantIssueURL)
		}
	}
}

func TestEligibleSpecApprovalCommentsRequireOwnerAndCurrentRevision(t *testing.T) {
	comments := []planedoc.PageComment{
		{ID: "old-owner", Actor: "owner", CreatedAt: "2026-07-17T09:59:59Z", CommentStripped: "approve"},
		{ID: "wrong-role", Actor: "product", CreatedAt: "2026-07-17T10:00:01Z", CommentStripped: "approve"},
		{ID: "request", Actor: "looper", CreatedAt: "2026-07-17T10:00:00Z", CommentStripped: "please approve"},
		{ID: "current-owner", Actor: "owner", CreatedAt: "2026-07-17T10:00:02Z", CommentStripped: "approve"},
	}
	got := eligibleSpecApprovalComments(comments, "owner", "2026-07-17T10:00:00Z", "request")
	if len(got) != 1 || got[0].ID != "current-owner" {
		t.Fatalf("eligible comments = %#v, want current owner comment only", got)
	}
	if got := eligibleSpecApprovalComments(comments, "", "2026-07-17T10:00:00Z", "request"); len(got) != 0 {
		t.Fatalf("missing owner identity must fail closed: %#v", got)
	}
	if got := eligibleSpecApprovalComments(comments, "owner", "bad-time", "request"); len(got) != 0 {
		t.Fatalf("invalid revision boundary must fail closed: %#v", got)
	}
}

func TestPlannerIssueURLFromCheckpoint(t *testing.T) {
	checkpoint := `{"issue":{"url":" https://plane.example/work-item/123 "}}`
	if got := plannerIssueURLFromCheckpoint(&checkpoint); got != "https://plane.example/work-item/123" {
		t.Fatalf("plannerIssueURLFromCheckpoint() = %q", got)
	}
	for _, invalid := range []*string{nil, strPtr(""), strPtr("{"), strPtr(`{"issue":null}`)} {
		if got := plannerIssueURLFromCheckpoint(invalid); got != "" {
			t.Fatalf("invalid checkpoint returned %q", got)
		}
	}
}

func TestInvalidateSpecApprovalFailsClosedWithDurableReason(t *testing.T) {
	repos := newEnqueueTestRepos(t)
	ctx := context.Background()
	nowISO := "2026-07-17T12:00:00.000Z"
	metadata := `{"awaitingSpecApproval":true,"specApprovalRevision":3,"unrelated":"kept"}`
	if err := repos.Projects.Upsert(ctx, storage.ProjectRecord{ID: "project", Name: "P", RepoPath: t.TempDir(), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatal(err)
	}
	if err := repos.Loops.Upsert(ctx, storage.LoopRecord{ID: "loop", Seq: 1, ProjectID: "project", Type: "planner", TargetType: "issue", Status: "completed", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatal(err)
	}
	if err := invalidateSpecApproval(ctx, repos, "loop", nowISO, "page changed"); err != nil {
		t.Fatal(err)
	}
	got, err := repos.Loops.GetByID(ctx, "loop")
	if err != nil || got == nil {
		t.Fatalf("loop = %#v, err=%v", got, err)
	}
	if got.Status != "failed" {
		t.Fatalf("status = %q, want failed", got.Status)
	}
	var meta map[string]any
	if got.MetadataJSON == nil || json.Unmarshal([]byte(*got.MetadataJSON), &meta) != nil {
		t.Fatalf("metadata = %v", got.MetadataJSON)
	}
	if awaiting, _ := meta["awaitingSpecApproval"].(bool); awaiting {
		t.Fatal("invalidated revision must no longer await approval")
	}
	if invalidated, _ := meta["specApprovalInvalidated"].(bool); !invalidated || meta["specApprovalInvalidReason"] != "page changed" || meta["nodeHPhase"] != "approval_invalidated" || meta["unrelated"] != "kept" {
		t.Fatalf("metadata = %#v", meta)
	}
}

func TestSpecApprovalRetriesSameApprovedCommentAfterPartialSideEffectFailure(t *testing.T) {
	repos := newEnqueueTestRepos(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	projectID, loopID, workItemID := "project", "loop-approval", "wi-1"
	specContent := "<h1>已评审技术方案</h1>"
	contentSum := sha256.Sum256([]byte(specContent))
	metadataBytes, _ := json.Marshal(map[string]any{
		"awaitingSpecApproval":         true,
		"specApprovedDispatched":       false,
		"issueUrl":                     "https://plane.example/workspace/projects/project/issues/" + workItemID,
		"specApprovalRevision":         2,
		"specApprovalContentHash":      fmt.Sprintf("%x", contentSum[:]),
		"specApprovalRequestCommentID": "request",
		"specApprovalRequestedAt":      "2026-07-17T11:59:00Z",
	})
	metadata := string(metadataBytes)
	if err := repos.Projects.Upsert(ctx, storage.ProjectRecord{ID: projectID, Name: "P", RepoPath: t.TempDir(), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatal(err)
	}
	if err := repos.Loops.Upsert(ctx, storage.LoopRecord{ID: loopID, Seq: 1, ProjectID: projectID, Type: "planner", TargetType: "issue", Status: "completed", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatal(err)
	}

	labelApplied, labelUpdates, stateAttempts, auditPosts := false, 0, 0, 0
	pageComments := []planedoc.PageComment{{ID: "owner-approve", Actor: "owner", CreatedAt: "2026-07-17T11:59:30Z", CommentHTML: "<p>approve</p>", CommentStripped: "approve"}}
	gateway := planedoc.New(planedoc.Options{Workspace: "workspace", Run: func(_ context.Context, options shell.Options) (shell.Result, error) {
		joined := strings.Join(options.Args, "\x00")
		switch {
		case strings.Contains(joined, "api\x00link\x00list"):
			return shell.Result{Stdout: `{"results":[{"id":"link","title":"looper:tech-spec","url":"https://plane.example/workspace/projects/project/pages/page-1"}]}`}, nil
		case strings.Contains(joined, "api\x00page\x00get") && strings.Contains(joined, "--content"):
			return shell.Result{Stdout: specContent}, nil
		case strings.Contains(joined, "pages/page-1/comments/") && strings.Contains(joined, "--method\x00GET"):
			encoded, _ := json.Marshal(map[string]any{"results": pageComments})
			return shell.Result{Stdout: string(encoded)}, nil
		case strings.Contains(joined, "api\x00label\x00list"):
			return shell.Result{Stdout: `{"results":[{"id":"worker-ready-id","name":"looper:worker-ready"}]}`}, nil
		case strings.Contains(joined, "api\x00work-item\x00get"):
			labels := []string{}
			if labelApplied {
				labels = []string{"worker-ready-id"}
			}
			encoded, _ := json.Marshal(map[string]any{"labels": labels})
			return shell.Result{Stdout: string(encoded)}, nil
		case strings.Contains(joined, "api\x00work-item\x00update"):
			labelApplied = true
			labelUpdates++
			return shell.Result{Stdout: `{}`}, nil
		case strings.Contains(joined, "/states/") && strings.Contains(joined, "--method\x00GET"):
			return shell.Result{Stdout: `{"results":[{"id":"in-progress-id","name":"In Progress"}]}`}, nil
		case strings.Contains(joined, "/issues/"+workItemID+"/") && strings.Contains(joined, "--method\x00PATCH"):
			stateAttempts++
			if stateAttempts == 1 {
				return shell.Result{}, errors.New("transient Plane state failure")
			}
			return shell.Result{Stdout: `{}`}, nil
		case strings.Contains(joined, "pages/page-1/comments/") && strings.Contains(joined, "--method\x00POST"):
			auditPosts++
			created := planedoc.PageComment{ID: "audit", Actor: "looper", CreatedAt: "2026-07-17T12:00:00Z", CommentHTML: "<!-- looper:spec-approved -->[looper]"}
			pageComments = append(pageComments, created)
			encoded, _ := json.Marshal(created)
			return shell.Result{Stdout: string(encoded)}, nil
		default:
			return shell.Result{}, nil
		}
	}})
	cfg := config.Config{Projects: []config.ProjectRefConfig{{ID: projectID, RepoPath: t.TempDir(), Owner: &config.FeishuActorConfig{PlaneID: "owner"}}}}
	judgeCalls := 0
	runtime := &Runtime{
		config:          cfg,
		now:             func() time.Time { return now },
		services:        Services{Repositories: repos},
		planeDocFactory: func(*config.Config, string) (*planedoc.Gateway, string, bool) { return gateway, projectID, true },
		specApprovalJudge: func(context.Context, []planedoc.PageComment, string) (specApprovalVerdict, error) {
			judgeCalls++
			return specApprovalVerdict{Approved: true, Reason: "owner approved"}, nil
		},
	}

	runtime.reconcileSpecApproval(ctx)
	afterFailure, _ := repos.Loops.GetByID(ctx, loopID)
	if afterFailure == nil || loopSpecApprovalState(afterFailure.MetadataJSON).Dispatched {
		t.Fatalf("first transient failure must remain undispatched: %#v", afterFailure)
	}
	runtime.reconcileSpecApproval(ctx)
	runtime.reconcileSpecApproval(ctx)
	afterSuccess, _ := repos.Loops.GetByID(ctx, loopID)
	if afterSuccess == nil || !loopSpecApprovalState(afterSuccess.MetadataJSON).Dispatched {
		t.Fatalf("same approved comment must retry to dispatch: %#v", afterSuccess)
	}
	if judgeCalls != 2 || labelUpdates != 1 || stateAttempts != 2 || auditPosts != 1 {
		t.Fatalf("side effects not exactly-once/retry-safe: judge=%d label=%d state=%d audit=%d", judgeCalls, labelUpdates, stateAttempts, auditPosts)
	}
}

func TestSpecApprovalInvalidatesPageEditedWhileJudgeRuns(t *testing.T) {
	repos := newEnqueueTestRepos(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	projectID, loopID, workItemID := "project", "loop-race", "wi-race"
	reviewedContent := "<h1>通过 REVIEW 的方案</h1>"
	changedContent := reviewedContent + "<p>审批判断期间被改写</p>"
	contentSum := sha256.Sum256([]byte(reviewedContent))
	metadataBytes, _ := json.Marshal(map[string]any{
		"awaitingSpecApproval":         true,
		"issueUrl":                     "https://plane.example/workspace/projects/project/issues/" + workItemID,
		"specApprovalRevision":         1,
		"specApprovalContentHash":      fmt.Sprintf("%x", contentSum[:]),
		"specApprovalRequestCommentID": "request",
		"specApprovalRequestedAt":      "2026-07-17T11:59:00Z",
	})
	metadata := string(metadataBytes)
	if err := repos.Projects.Upsert(ctx, storage.ProjectRecord{ID: projectID, Name: "P", RepoPath: t.TempDir(), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatal(err)
	}
	if err := repos.Loops.Upsert(ctx, storage.LoopRecord{ID: loopID, Seq: 1, ProjectID: projectID, Type: "planner", TargetType: "issue", Status: "completed", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatal(err)
	}

	pageReads, labelWrites, invalidationPosts := 0, 0, 0
	comments := []planedoc.PageComment{{ID: "owner-approve", Actor: "owner", CreatedAt: "2026-07-17T11:59:30Z", CommentHTML: "<p>approve</p>", CommentStripped: "approve"}}
	gateway := planedoc.New(planedoc.Options{Workspace: "workspace", Run: func(_ context.Context, options shell.Options) (shell.Result, error) {
		joined := strings.Join(options.Args, "\x00")
		switch {
		case strings.Contains(joined, "api\x00link\x00list"):
			return shell.Result{Stdout: `{"results":[{"id":"link","title":"looper:tech-spec","url":"https://plane.example/workspace/projects/project/pages/page-1"}]}`}, nil
		case strings.Contains(joined, "api\x00page\x00get") && strings.Contains(joined, "--content"):
			pageReads++
			if pageReads == 1 {
				return shell.Result{Stdout: reviewedContent}, nil
			}
			return shell.Result{Stdout: changedContent}, nil
		case strings.Contains(joined, "pages/page-1/comments/") && strings.Contains(joined, "--method\x00GET"):
			encoded, _ := json.Marshal(map[string]any{"results": comments})
			return shell.Result{Stdout: string(encoded)}, nil
		case strings.Contains(joined, "pages/page-1/comments/") && strings.Contains(joined, "--method\x00POST"):
			invalidationPosts++
			return shell.Result{Stdout: `{"id":"invalidated","created_at":"2026-07-17T12:00:00Z"}`}, nil
		case strings.Contains(joined, "api\x00work-item\x00update"):
			labelWrites++
			return shell.Result{Stdout: `{}`}, nil
		default:
			return shell.Result{}, nil
		}
	}})
	runtime := &Runtime{
		config:   config.Config{Projects: []config.ProjectRefConfig{{ID: projectID, RepoPath: t.TempDir(), Owner: &config.FeishuActorConfig{PlaneID: "owner"}}}},
		now:      func() time.Time { return now },
		services: Services{Repositories: repos},
		planeDocFactory: func(*config.Config, string) (*planedoc.Gateway, string, bool) {
			return gateway, projectID, true
		},
		specApprovalJudge: func(context.Context, []planedoc.PageComment, string) (specApprovalVerdict, error) {
			return specApprovalVerdict{Approved: true}, nil
		},
	}

	runtime.reconcileSpecApproval(ctx)
	got, _ := repos.Loops.GetByID(ctx, loopID)
	if got == nil || got.Status != "failed" {
		t.Fatalf("loop must fail closed after concurrent page edit: %#v", got)
	}
	var gotMeta map[string]any
	if got.MetadataJSON == nil || json.Unmarshal([]byte(*got.MetadataJSON), &gotMeta) != nil || gotMeta["specApprovalInvalidated"] != true {
		t.Fatalf("invalidated metadata = %#v", got)
	}
	if pageReads != 2 || labelWrites != 0 || invalidationPosts != 1 {
		t.Fatalf("pageReads=%d labelWrites=%d invalidationPosts=%d", pageReads, labelWrites, invalidationPosts)
	}
}
